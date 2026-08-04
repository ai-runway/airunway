/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package dynamo

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// Regression tests for upstream schema compatibility.
//
// Every write to the upstream resource must carry fieldValidation=Strict, so the API
// server rejects fields the installed CRD does not declare instead of silently pruning
// them. Without it the shim can render a field the cluster drops without error, leaving
// a workload that reports healthy but cannot serve.
//
// These assert the option reaches the client. They deliberately do NOT assert the API
// server's behaviour — the fake client has no structural schema and cannot prune, which
// is precisely why no pre-existing test caught this bug. Proving the rejection itself needs a
// real API server — there is no envtest harness for the providers yet, so that coverage is a
// documented follow-up rather than something these tests provide.

func TestUpstreamCreateUsesStrictFieldValidation(t *testing.T) {
	scheme := newScheme()

	var got string
	var called bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			called = true
			o := &client.CreateOptions{}
			for _, opt := range opts {
				opt.ApplyToCreate(o)
			}
			got = o.FieldValidation
			return cl.Create(ctx, obj, opts...)
		},
	}).Build()

	r := NewDynamoProviderReconciler(c, scheme, "")

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = types.UID("test-uid")

	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	dgd.SetName("test")
	dgd.SetNamespace("default")
	dgd.Object["spec"] = map[string]interface{}{"backendFramework": "vllm"}

	if err := r.createOrUpdateResource(context.Background(), dgd, md); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("Create was never called — test is not exercising the create path")
	}
	if got != metav1.FieldValidationStrict {
		t.Errorf("create fieldValidation = %q, want %q (unknown fields must be rejected, not pruned)",
			got, metav1.FieldValidationStrict)
	}
}

func TestUpstreamUpdateUsesStrictFieldValidation(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setDGDGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       "test",
		UID:        types.UID("test-uid"),
	}})
	// Differs from desired, so createOrUpdateResource takes the update path.
	existing.Object["spec"] = map[string]interface{}{"backendFramework": "sglang"}

	var got string
	var called bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				called = true
				o := &client.UpdateOptions{}
				for _, opt := range opts {
					opt.ApplyToUpdate(o)
				}
				got = o.FieldValidation
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()

	r := NewDynamoProviderReconciler(c, scheme, "")

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = types.UID("test-uid")

	desired := &unstructured.Unstructured{}
	setDGDGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")
	desired.SetOwnerReferences(existing.GetOwnerReferences())
	desired.Object["spec"] = map[string]interface{}{"backendFramework": "vllm"}

	if err := r.createOrUpdateResource(context.Background(), desired, md); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("Update was never called — test is not exercising the update path")
	}
	if got != metav1.FieldValidationStrict {
		t.Errorf("update fieldValidation = %q, want %q (unknown fields must be rejected, not pruned)",
			got, metav1.FieldValidationStrict)
	}
}

// TestRenderedObjectHasNoUnknownRootFields guards the interaction between the
// provider.overrides escape hatch and strict field validation.
//
// parseOverrides decodes routerMode/frontend/epp into DynamoOverrides to shape how this
// transformer renders. applyOverrides then deep-merges the *raw* override map into the
// object, so before this change those already-consumed keys also landed at the
// object root as siblings of apiVersion/spec — fields no DynamoGraphDeployment CRD
// declares at any version.
//
// The API server pruned them silently, so it looked like it worked. Under
// fieldValidation=Strict an unknown root field is a hard rejection, which would have
// broken the override example documented in docs/controller-architecture.md on a
// perfectly up-to-date cluster.
func TestRenderedObjectHasNoUnknownRootFields(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.Spec.Model = airunwayv1alpha1.ModelSpec{
		ID:     "Qwen/Qwen3-0.6B",
		Source: airunwayv1alpha1.ModelSourceHuggingFace,
	}
	md.Spec.Engine = airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM}
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
	// Only keys admission actually permits. The webhook rejects "replicas" and "resources"
	// RECURSIVELY (modeldeployment_webhook.go sizingOverrideKeys), so frontend.replicas and
	// epp.replicas cannot reach a cluster and must not be used as fixtures here.
	// "RouterMode" is the interesting spelling: encoding/json matches field names
	// case-insensitively, so parseOverrides consumes it into DynamoOverrides just as it does
	// the documented lowercase form, and a case-sensitive strip would leak it to the root.
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Name: "dynamo",
		Overrides: &runtime.RawExtension{
			Raw: []byte(`{"routerMode":"kv","RouterMode":"round-robin","epp":{"image":"custom:v1"}}`),
		},
	}

	results, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	allowed := map[string]bool{"apiVersion": true, "kind": true, "metadata": true, "spec": true}
	var unknown []string
	for k := range results[0].Object {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("rendered DynamoGraphDeployment has root fields the CRD does not declare: %v\n"+
			"strict field validation will reject the write; keys consumed by parseOverrides "+
			"must be stripped before applyOverrides merges the raw map", unknown)
	}
}

// TestReconcileRequeuesOnStrictRejection covers the recovery path for the failure mode
// strict validation introduces.
//
// A schema rejection is terminal from the controller's point of view: the remedy is an
// out-of-band upstream upgrade. Nothing in the watch set would re-trigger this reconcile
// afterwards — the provider-config watch fires only on Spec/Ready changes, and no upstream
// object exists to watch because the write failed. Without an explicit requeue the
// deployment would sit in Failed until controller-runtime's ~10h resync, so an operator
// who fixed their cluster would see nothing change.
func TestReconcileRequeuesOnStrictRejection(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)

	// Seed the Running-era status. Without this the clearing below is a no-op the
	// assertions cannot observe, and deleting it from the controller would not fail
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1}

	rejection := apierrors.NewBadRequest(
		`DynamoGraphDeployment in version "v1alpha1" cannot be handled as a DynamoGraphDeployment: ` +
			`strict decoding error: unknown field "spec.services.VllmWorker.frontendSidecar"`)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentKind {
					return rejection
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()

	r := NewDynamoProviderReconciler(c, scheme, "")

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error, expected the rejection to be reported in status: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Error("expected a requeue after a strict-validation rejection; without one the " +
			"deployment stays Failed until the default resync even after the upstream is upgraded")
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	reasons := map[string]string{}
	statuses := map[string]metav1.ConditionStatus{}
	for _, cond := range updated.Status.Conditions {
		reasons[cond.Type] = cond.Reason
		statuses[cond.Type] = cond.Status
	}

	// ResourceCreated carries the distinguishing reason so an upstream schema mismatch is
	// greppable rather than buried in free text under a generic CreateFailed.
	if got := reasons[airunwayv1alpha1.ConditionTypeResourceCreated]; got != "IncompatibleUpstream" {
		t.Errorf("ResourceCreated reason = %q, want %q", got, "IncompatibleUpstream")
	}

	// docs/providers.md promises the offending field is named in status.message; hold it.
	if !strings.Contains(updated.Status.Message, "frontendSidecar") {
		t.Errorf("status.message does not name the offending field: %q", updated.Status.Message)
	}

	// Ready must be forced False. The failure this guards is precisely a deployment that reports healthy
	// while unable to serve, so leaving the primary health condition stale would reproduce
	// the original sin in a new place.
	if got := statuses[airunwayv1alpha1.ConditionTypeReady]; got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want %q", got, metav1.ConditionFalse)
	}
	// docs/providers.md promises BOTH conditions carry this reason.
	if got := reasons[airunwayv1alpha1.ConditionTypeReady]; got != "IncompatibleUpstream" {
		t.Errorf("Ready reason = %q, want IncompatibleUpstream", got)
	}

	// Stale endpoint/replicas must be cleared. Reporting Failed alongside a live endpoint
	// and "1/1 ready" is the same contradiction.
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil — a Failed deployment must not still advertise an endpoint", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("phase = %q, want %q", updated.Status.Phase, airunwayv1alpha1.DeploymentPhaseFailed)
	}

	// ProviderCompatible is deliberately NOT touched here: it is set True earlier in the
	// same reconcile, so flipping it would rewrite LastTransitionTime on every 30s requeue
	// and the condition would never settle.
	if got := statuses[airunwayv1alpha1.ConditionTypeProviderCompatible]; got != metav1.ConditionTrue {
		t.Errorf("ProviderCompatible = %q, want %q — flipping it here causes permanent status churn",
			got, metav1.ConditionTrue)
	}
}

// TestIsUpstreamSchemaRejection pins the matcher to error strings actually observed against
// a live cluster, not invented ones. The API server's response class varies by write path,
// which is why this matches on message rather than status code:
//
//	CR create/update      -> 400 BadRequest
//	CR merge patch        -> 422 Invalid
//	SSA on a built-in type-> 500, from the field manager rather than field validation
//
// The negative cases are the reason a status-class check was rejected: an IsInvalid gate
// would capture ordinary CEL and type violations and report them as an upstream version
// mismatch, sending an operator to upgrade a cluster that is already fine.
func TestIsUpstreamSchemaRejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "observed: CR create against an older CRD (400)",
			err: apierrors.NewBadRequest(`DynamoGraphDeployment in version "v1alpha1" cannot be handled as a ` +
				`DynamoGraphDeployment: strict decoding error: unknown field "spec.services.VllmWorker.frontendSidecar"`),
			want: true,
		},
		{
			name: "observed: server-side apply on a built-in type (500)",
			err: fmt.Errorf("failed to create typed patch object (default/x; apps/v1, Kind=Deployment): " +
				".spec.bogusUnknownField: field not declared in schema"),
			want: true,
		},
		{
			name: "not ours: CEL or type validation failure the user must fix",
			err:  apierrors.NewInvalid(schema.GroupKind{Group: "nvidia.com", Kind: "DynamoGraphDeployment"}, "x", nil),
			want: false,
		},
		{
			name: "not ours: conflict",
			err:  apierrors.NewConflict(schema.GroupResource{Resource: "dynamographdeployments"}, "x", fmt.Errorf("stale")),
			want: false,
		},
		{
			// The reason the needle is "strict decoding error" and not the bare
			// "unknown field": an Invalid status echoes the offending value back, so a
			// user-supplied string can contain the bare phrase. Classifying this as an
			// upstream mismatch would tell an operator to upgrade a cluster that is fine,
			// and retry every 30s forever.
			name: "not ours: Invalid echoing a user value that contains the bare phrase",
			err: apierrors.NewInvalid(schema.GroupKind{Kind: "X"}, "x", field.ErrorList{
				field.Invalid(field.NewPath("spec", "engine", "extraArgs"),
					"--served-model-name=unknown field probe", "must be a valid flag"),
			}),
			want: false,
		},
		{
			// The needle requires BOTH halves. An Invalid status echoes the offending value
			// back, so a user-supplied string containing only the prefix must not match.
			name: "not ours: 'strict decoding error' echoed without an unknown-field cause",
			err: apierrors.NewInvalid(schema.GroupKind{Kind: "X"}, "x", field.ErrorList{
				field.Invalid(field.NewPath("spec", "engine", "extraArgs"),
					"--served-model-name=strict decoding error probe", "must be a valid flag"),
			}),
			want: false,
		},
		{
			name: "not ours: typed-object wrapper without the schema diagnostic",
			err:  fmt.Errorf("failed to create typed patch object (default/x; apps/v1, Kind=Deployment): some other failure"),
			want: false,
		},
		{name: "nil", err: nil, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUpstreamSchemaRejection(tc.err); got != tc.want {
				t.Errorf("isUpstreamSchemaRejection() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUnsupportedOverrideKeyIsRejected covers the reject-rather-than-drop decision.
//
// Silently ignoring an override key would reproduce the failure strict validation exists to surface:
// the override is visible in the ModelDeployment, absent from the rendered object, and the
// deployment reports healthy. So an unsupported root key must be a loud error.
//
// "Spec" (capital S) is the interesting case — encoding/json would NOT decode it into
// DynamoOverrides, so it is neither consumed nor a valid root key and must be rejected.
func TestUnsupportedOverrideKeyIsRejected(t *testing.T) {
	for _, key := range []string{"Spec", "totallyUnknown", "services"} {
		t.Run(key, func(t *testing.T) {
			md := &airunwayv1alpha1.ModelDeployment{}
			md.Name = "test"
			md.Namespace = "default"
			md.Spec.Model = airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace}
			md.Spec.Engine = airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM}
			md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
			md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
				Name:      "dynamo",
				Overrides: &runtime.RawExtension{Raw: []byte(`{"` + key + `":{"x":1}}`)},
			}

			if _, err := NewTransformer().Transform(context.Background(), md); err == nil {
				t.Errorf("override root key %q was accepted; it must be rejected rather than "+
					"silently dropped, or the user gets a setting that does nothing", key)
			}
		})
	}
}

// TestDocumentedOverrideKeysStillAccepted is the other half: the documented keys, in both
// the documented spelling and the capitalisation encoding/json also accepts, must work.
func TestDocumentedOverrideKeysStillAccepted(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.Spec.Model = airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace}
	md.Spec.Engine = airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM}
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Name: "dynamo",
		Overrides: &runtime.RawExtension{
			Raw: []byte(`{"routerMode":"kv","epp":{"image":"custom:v1"},"spec":{"backendFramework":"sglang"}}`),
		},
	}

	results, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("documented override keys were rejected: %v", err)
	}
	// the spec override still lands
	got, _, _ := unstructured.NestedString(results[0].Object, "spec", "backendFramework")
	if got != "sglang" {
		t.Errorf("spec override did not apply: backendFramework = %q, want %q", got, "sglang")
	}
}

// TestUnsupportedOverrideErrorIsDeterministic guards against a status-write loop.
//
// Go randomises map iteration, so an implementation that returns on the first unsupported
// key produces a different message per call for the same spec. That message lands in
// status.message, and because the ModelDeployment watch has no GenerationChangedPredicate,
// a changing message means every reconcile writes status, which re-enqueues the object.
func TestUnsupportedOverrideErrorIsDeterministic(t *testing.T) {
	newMD := func() *airunwayv1alpha1.ModelDeployment {
		md := &airunwayv1alpha1.ModelDeployment{}
		md.Name = "test"
		md.Namespace = "default"
		md.Spec.Model = airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace}
		md.Spec.Engine = airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM}
		md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
		md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
			Name:      "dynamo",
			Overrides: &runtime.RawExtension{Raw: []byte(`{"alpha":{"a":1},"beta":{"b":2},"gamma":{"c":3}}`)},
		}
		return md
	}

	var first string
	for i := 0; i < 200; i++ {
		_, err := NewTransformer().Transform(context.Background(), newMD())
		if err == nil {
			t.Fatal("expected an error for unsupported override keys")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("override rejection message is nondeterministic across calls:\n  %q\n  %q\n"+
				"a changing status.message re-enqueues the object every reconcile", first, err.Error())
		}
	}
	// All offenders should be named, not just whichever the map yielded first.
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(first, want) {
			t.Errorf("error %q does not mention offending key %q", first, want)
		}
	}
}

// TestConsumedOverrideKeysMatchesStruct keeps the hand-written map in sync with
// DynamoOverrides. Drift in the struct-has-field-map-doesn't direction leaks the key to the
// object root; drift the other way silently drops a user's override — both are the failure
// class strict validation exists to surface.
func TestConsumedOverrideKeysMatchesStruct(t *testing.T) {
	typ := reflect.TypeOf(DynamoOverrides{})
	fromStruct := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		fromStruct[strings.ToLower(name)] = true
	}
	for k := range fromStruct {
		if !consumedOverrideKeys[k] {
			t.Errorf("DynamoOverrides has json key %q but consumedOverrideKeys does not — it will leak to the object root", k)
		}
	}
	for k := range consumedOverrideKeys {
		if !fromStruct[k] {
			t.Errorf("consumedOverrideKeys has %q but DynamoOverrides does not — that override would be silently dropped", k)
		}
	}
}

// TestReconcileTransformFailureClearsStaleStatus covers the OTHER failure branch: a spec the
// transformer refuses to render (here, an unsupported provider.overrides root key).
//
// It must get the same treatment as an upstream rejection — Ready forced False and the
// Running-era endpoint/replica counts dropped. Without that, a previously-Running deployment
// edited into something unrenderable reports Failed while still advertising a live endpoint
// and "1/1 ready", which is the contradiction this guards against.
func TestReconcileTransformFailureClearsStaleStatus(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Name:      md.Status.Provider.Name,
		Overrides: &runtime.RawExtension{Raw: []byte(`{"totallyBogusRootKey":{"a":1}}`)},
	}
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	}); err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	var createdReason, readyReason string
	var readyStatus metav1.ConditionStatus
	for _, cond := range updated.Status.Conditions {
		if cond.Type == airunwayv1alpha1.ConditionTypeResourceCreated {
			createdReason = cond.Reason
		}
		if cond.Type == airunwayv1alpha1.ConditionTypeReady {
			readyStatus = cond.Status
			readyReason = cond.Reason
		}
	}
	if createdReason != "TransformFailed" {
		t.Errorf("ResourceCreated reason = %q, want TransformFailed", createdReason)
	}
	if readyStatus != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", readyStatus)
	}
	if readyReason != "TransformFailed" {
		t.Errorf("Ready reason = %q, want TransformFailed", readyReason)
	}
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
}

// TestReconcileGenericFailureClearsStaleStatus covers the THIRD failure branch — an ordinary
// write failure that is neither a transform error nor a schema rejection.
//
// It is the most common of the three, and it must not be the one that still reports healthy.
// Without the clearing, a previously-Running deployment hitting a transient upstream error
// reports Failed while advertising a live endpoint and "1/1 ready" — the contradiction issue
// this guards against.
func TestReconcileGenericFailureClearsStaleStatus(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentKind {
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}
	// A generic write failure is usually transient, so it must retry. Without a requeue the
	// second pass writes identical status (a server-side no-op), nothing re-enqueues, and the
	// deployment sits Failed with no endpoint until the default resync.
	if res.RequeueAfter <= 0 {
		t.Error("expected a requeue after a transient write failure")
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	var createdReason string
	var readyStatus metav1.ConditionStatus
	for _, cond := range updated.Status.Conditions {
		if cond.Type == airunwayv1alpha1.ConditionTypeResourceCreated {
			createdReason = cond.Reason
		}
		if cond.Type == airunwayv1alpha1.ConditionTypeReady {
			readyStatus = cond.Status
		}
	}
	if createdReason != "CreateFailed" {
		t.Errorf("ResourceCreated reason = %q, want CreateFailed", createdReason)
	}
	if readyStatus != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", readyStatus)
	}
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
}

// TestReconcileOwnershipConflictUsesItsOwnReason covers the ownership-collision branch.
//
// An upstream object of the right name already exists but belongs to someone else. That is
// terminal — unlike a transient API 409 — so it is reported under its own reason rather than
// the generic CreateFailed.
func TestReconcileOwnershipConflictUsesItsOwnReason(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)

	// Same name, different owner.
	foreign := &unstructured.Unstructured{}
	setDGDGVK(foreign)
	foreign.SetName("test")
	foreign.SetNamespace("default")
	foreign.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       "someone-else",
		UID:        types.UID("a-different-uid"),
	}})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, foreign).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	}); err != nil {
		t.Fatalf("reconcile returned an error: %v", err)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	var reason, readyReason string
	for _, cond := range updated.Status.Conditions {
		if cond.Type == airunwayv1alpha1.ConditionTypeResourceCreated {
			reason = cond.Reason
		}
		if cond.Type == airunwayv1alpha1.ConditionTypeReady {
			readyReason = cond.Reason
		}
	}
	// The branch comment claims "Ready is set below unconditionally with this same reason".
	// Hold it: an operator alerting on Ready.reason=ResourceConflict needs to distinguish a
	// collision needing manual intervention from a transient CreateFailed that will retry.
	if readyReason != "ResourceConflict" {
		t.Errorf("Ready reason = %q, want ResourceConflict", readyReason)
	}
	if reason != "ResourceConflict" {
		t.Errorf("ResourceCreated reason = %q, want ResourceConflict — an ownership collision "+
			"must be distinguishable from a generic create failure", reason)
	}
}
