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
	"io"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

// TestUpstreamUpdateDoesNotIgnoreDesiredUnknownEmptyFields guards the update
// half of strict validation. The API server may add empty defaults to the object it
// returns, but empty values explicitly present in the desired spec must not be
// normalized away: an installed CRD that does not declare them still needs to see a
// strict update and reject them.
func TestUpstreamUpdateDoesNotIgnoreDesiredUnknownEmptyFields(t *testing.T) {
	tests := []struct {
		name           string
		overrideRaw    string
		existingFields map[string]interface{}
		desiredFields  map[string]interface{}
	}{
		{
			name:          "empty string",
			overrideRaw:   `{"spec":{"futureField":""}}`,
			desiredFields: map[string]interface{}{"futureField": ""},
		},
		{
			name:          "empty object",
			overrideRaw:   `{"spec":{"futureField":{}}}`,
			desiredFields: map[string]interface{}{"futureField": map[string]interface{}{}},
		},
		{
			name:        "nested empty object",
			overrideRaw: `{"spec":{"services":{"VllmWorker":{"futureField":{}}}}}`,
			existingFields: map[string]interface{}{
				"services": map[string]interface{}{
					"VllmWorker": map[string]interface{}{},
				},
			},
			desiredFields: map[string]interface{}{
				"services": map[string]interface{}{
					"VllmWorker": map[string]interface{}{
						"futureField": map[string]interface{}{},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			existingSpec := map[string]interface{}{
				"backendFramework": "vllm",
				"serverDefault":    "",
			}
			for key, value := range tt.existingFields {
				existingSpec[key] = value
			}
			existing.Object["spec"] = existingSpec

			var called bool
			var got string
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(_ context.Context, _ client.WithWatch, _ client.Object, opts ...client.UpdateOption) error {
						called = true
						o := &client.UpdateOptions{}
						for _, opt := range opts {
							opt.ApplyToUpdate(o)
						}
						got = o.FieldValidation
						return fmt.Errorf(`strict decoding error: unknown field "spec.futureField"`)
					},
				}).Build()

			r := NewDynamoProviderReconciler(c, scheme, "")
			md := &airunwayv1alpha1.ModelDeployment{ObjectMeta: metav1.ObjectMeta{
				Name: "test", Namespace: "default", UID: types.UID("test-uid"),
			}}
			md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
				Name: ProviderName,
				Overrides: &runtime.RawExtension{
					Raw: []byte(tt.overrideRaw),
				},
			}
			desired := &unstructured.Unstructured{}
			setDGDGVK(desired)
			desired.SetName("test")
			desired.SetNamespace("default")
			desired.SetOwnerReferences(existing.GetOwnerReferences())
			desiredSpec := map[string]interface{}{
				"backendFramework": "vllm",
			}
			for key, value := range tt.desiredFields {
				desiredSpec[key] = value
			}
			desired.Object["spec"] = desiredSpec

			err := r.createOrUpdateResource(context.Background(), desired, md)
			if err == nil {
				t.Fatal("expected strict unknown-field rejection")
			}
			if !called {
				t.Fatal("Update was not called; desired empty unknown field was normalized away")
			}
			if got != metav1.FieldValidationStrict {
				t.Errorf("update fieldValidation = %q, want %q", got, metav1.FieldValidationStrict)
			}
			if !isUpstreamSchemaRejection(err) {
				t.Errorf("error was not classified as an upstream schema rejection: %v", err)
			}
		})
	}
}

func TestUpstreamUpdateIgnoresDefaultsAndPersistedEmptyOverrides(t *testing.T) {
	tests := []struct {
		name         string
		overrideRaw  string
		existingSpec map[string]interface{}
		desiredSpec  map[string]interface{}
	}{
		{
			name: "server-added defaults",
			existingSpec: map[string]interface{}{
				"backendFramework": "vllm",
				"name":             "",
				"resources":        map[string]interface{}{},
			},
			desiredSpec: map[string]interface{}{"backendFramework": "vllm"},
		},
		{
			name:        "persisted empty string override",
			overrideRaw: `{"spec":{"futureField":""}}`,
			existingSpec: map[string]interface{}{
				"backendFramework": "vllm",
				"futureField":      "",
				"name":             "",
			},
			desiredSpec: map[string]interface{}{
				"backendFramework": "vllm",
				"futureField":      "",
			},
		},
		{
			name:        "persisted empty object override",
			overrideRaw: `{"spec":{"futureField":{}}}`,
			existingSpec: map[string]interface{}{
				"backendFramework": "vllm",
				"futureField":      map[string]interface{}{},
				"resources":        map[string]interface{}{},
			},
			desiredSpec: map[string]interface{}{
				"backendFramework": "vllm",
				"futureField":      map[string]interface{}{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			existing.Object["spec"] = tt.existingSpec

			var updateCalled bool
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
						updateCalled = true
						return nil
					},
				}).Build()

			md := &airunwayv1alpha1.ModelDeployment{ObjectMeta: metav1.ObjectMeta{
				Name: "test", Namespace: "default", UID: types.UID("test-uid"),
			}}
			if tt.overrideRaw != "" {
				md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
					Name:      ProviderName,
					Overrides: &runtime.RawExtension{Raw: []byte(tt.overrideRaw)},
				}
			}

			desired := &unstructured.Unstructured{}
			setDGDGVK(desired)
			desired.SetName("test")
			desired.SetNamespace("default")
			desired.SetOwnerReferences(existing.GetOwnerReferences())
			desired.Object["spec"] = tt.desiredSpec

			if err := NewDynamoProviderReconciler(c, scheme, "").createOrUpdateResource(context.Background(), desired, md); err != nil {
				t.Fatalf("createOrUpdateResource: %v", err)
			}
			if updateCalled {
				t.Fatal("Update was called even though defaults and explicit empty overrides were already satisfied")
			}
		})
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
	if res.Requeue || res.RequeueAfter != ExternalRecoveryInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s after a strict-validation rejection",
			res.Requeue, res.RequeueAfter, ExternalRecoveryInterval)
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
			// Even the complete diagnostic can occur inside an echoed value. The real
			// strict-decoding cause is terminal; this value is followed by its validation
			// reason and must not be classified as an upstream mismatch.
			name: "not ours: full strict diagnostic echoed inside a user value",
			err: apierrors.NewInvalid(schema.GroupKind{Kind: "X"}, "x", field.ErrorList{
				field.Invalid(field.NewPath("spec", "engine", "extraArgs"),
					`--served-model-name=strict decoding error: unknown field "spec.fake"`, "must be a valid flag"),
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

func TestIsRetryableUpstreamWriteErrorRecognizesWrappedEOF(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "EOF", err: io.EOF},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF},
		{name: "broken pipe", err: syscall.EPIPE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !isRetryableUpstreamWriteError(wrapResourceWriteError(tt.err, true)) {
				t.Errorf("wrapped %v was not classified as retryable", tt.err)
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
	testCases := []struct {
		name string
		raw  string
	}{
		{
			name: "documented spelling",
			raw:  `{"routerMode":"kv","epp":{"image":"custom:v1"},"spec":{"backendFramework":"sglang"}}`,
		},
		{
			name: "case-insensitive consumed keys",
			raw:  `{"RouterMode":"kv","EPP":{"Image":"custom:v1"},"spec":{"backendFramework":"sglang"}}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newTestMD("test", "default")
			md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
				Name:      "dynamo",
				Overrides: &runtime.RawExtension{Raw: []byte(tc.raw)},
			}

			results, err := NewTransformer().Transform(context.Background(), md)
			if err != nil {
				t.Fatalf("documented override keys were rejected: %v", err)
			}
			// The opaque upstream spec override still lands.
			got, _, _ := unstructured.NestedString(results[0].Object, "spec", "backendFramework")
			if got != "sglang" {
				t.Errorf("spec override did not apply: backendFramework = %q, want %q", got, "sglang")
			}
		})
	}
}

func TestConsumedOverrideUnknownFieldsAreRejected(t *testing.T) {
	testCases := []struct {
		name         string
		raw          string
		unknownField string
	}{
		{
			name:         "epp child",
			raw:          `{"epp":{"imag":"custom:v1"}}`,
			unknownField: "imag",
		},
		{
			name:         "frontend child",
			raw:          `{"frontend":{"bogus":true}}`,
			unknownField: "bogus",
		},
		{
			name:         "frontend resources child",
			raw:          `{"frontend":{"resources":{"cpus":"4"}}}`,
			unknownField: "cpus",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newTestMD("test", "default")
			md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
				Name:      "dynamo",
				Overrides: &runtime.RawExtension{Raw: []byte(tc.raw)},
			}

			_, err := NewTransformer().Transform(context.Background(), md)
			if err == nil {
				t.Fatalf("unknown consumed override field %q was silently accepted", tc.unknownField)
			}
			if want := fmt.Sprintf(`unknown field %q`, tc.unknownField); !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not identify %q", err, want)
			}
		})
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

// TestReconcileTransientWriteFailurePreservesLastKnownStatus covers a retryable update
// failure after the controller has observed an owned upstream resource. The ambiguous write
// does not prove that existing workload stopped serving, so last-known status is retained.
func TestReconcileTransientWriteFailurePreservesLastKnownStatus(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:    airunwayv1alpha1.ConditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "DeploymentReady",
		Message: "last observed workload is ready",
	})

	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	existing := resources[0].DeepCopy()
	if err := unstructured.SetNestedField(existing.Object, "sglang", "spec", "backendFramework"); err != nil {
		t.Fatalf("mutate existing spec: %v", err)
	}
	if err := unstructured.SetNestedField(existing.Object, string(DynamoStateSuccessful), "status", "state"); err != nil {
		t.Fatalf("mark existing resource ready: %v", err)
	}

	var updateCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentKind {
					updateCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}
	if res.RequeueAfter != RequeueInterval {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, RequeueInterval)
	}
	if !updateCalled {
		t.Fatal("Update was never called; test did not exercise the existing-resource path")
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	var createdReason string
	var createdStatus metav1.ConditionStatus
	var readyStatus metav1.ConditionStatus
	var readyReason string
	for _, cond := range updated.Status.Conditions {
		if cond.Type == airunwayv1alpha1.ConditionTypeResourceCreated {
			createdReason = cond.Reason
			createdStatus = cond.Status
		}
		if cond.Type == airunwayv1alpha1.ConditionTypeReady {
			readyStatus = cond.Status
			readyReason = cond.Reason
		}
	}
	if createdReason != "CreateFailed" {
		t.Errorf("ResourceCreated reason = %q, want CreateFailed", createdReason)
	}
	if createdStatus != metav1.ConditionFalse {
		t.Errorf("ResourceCreated = %q, want False", createdStatus)
	}
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("Status.Phase = %q, want Running", updated.Status.Phase)
	}
	if readyStatus != metav1.ConditionTrue || readyReason != "DeploymentReady" {
		t.Errorf("Ready = %q reason %q, want True reason DeploymentReady", readyStatus, readyReason)
	}
	if updated.Status.Endpoint == nil || updated.Status.Endpoint.Service != "stale-svc" || updated.Status.Endpoint.Port != 8000 {
		t.Errorf("Status.Endpoint = %+v, want stale-svc:8000", updated.Status.Endpoint)
	}
	if updated.Status.Replicas == nil || updated.Status.Replicas.Desired != 1 || updated.Status.Replicas.Ready != 1 || updated.Status.Replicas.Available != 1 {
		t.Errorf("Status.Replicas = %+v, want 1 desired/ready/available", updated.Status.Replicas)
	}
}

// TestReconcileTransientWriteFailureOnUnreadyResourceFailsClosed proves that ownership and
// an active object are not enough to retain stale serving status. The current upstream
// observation is explicitly non-serving, so an ambiguous write failure must fail closed.
func TestReconcileTransientWriteFailureOnUnreadyResourceFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:   airunwayv1alpha1.ConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: "DeploymentReady",
	})

	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	existing := resources[0].DeepCopy()
	if err := unstructured.SetNestedField(existing.Object, "sglang", "spec", "backendFramework"); err != nil {
		t.Fatalf("mutate existing spec: %v", err)
	}
	if err := unstructured.SetNestedField(existing.Object, string(DynamoStateDeploying), "status", "state"); err != nil {
		t.Fatalf("mark existing resource unready: %v", err)
	}

	var updateCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentKind {
					updateCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}
	if res.Requeue || res.RequeueAfter != RequeueInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s", res.Requeue, res.RequeueAfter, RequeueInterval)
	}
	if !updateCalled {
		t.Fatal("Update was never called; test did not exercise the active unready-resource path")
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("Status.Phase = %q, want Failed", updated.Status.Phase)
	}
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
	created := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeResourceCreated)
	if created == nil || created.Status != metav1.ConditionFalse {
		t.Errorf("ResourceCreated = %+v, want False", created)
	}
	ready := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v, want False", ready)
	}
}

// TestReconcileTransientUpdateFailureOnTerminatingResourceFailsClosed covers an owned
// upstream object that was observed but is already terminating. A retryable update failure
// cannot preserve serving status in that case because the last observation says the workload
// is going away, not that it remains safely available.
func TestReconcileTransientUpdateFailureOnTerminatingResourceFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:    airunwayv1alpha1.ConditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "DeploymentReady",
		Message: "last observed workload is ready",
	})

	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	existing := resources[0].DeepCopy()
	if err := unstructured.SetNestedField(existing.Object, "sglang", "spec", "backendFramework"); err != nil {
		t.Fatalf("mutate existing spec: %v", err)
	}
	if err := unstructured.SetNestedField(existing.Object, string(DynamoStateSuccessful), "status", "state"); err != nil {
		t.Fatalf("mark existing resource ready: %v", err)
	}
	deletingAt := metav1.Now()
	existing.SetDeletionTimestamp(&deletingAt)
	existing.SetFinalizers([]string{"test.airunway.ai/upstream-cleanup"})

	var updateCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentKind {
					updateCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}
	if res.Requeue || res.RequeueAfter != RequeueInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s after a transient update of a terminating resource",
			res.Requeue, res.RequeueAfter, RequeueInterval)
	}
	if !updateCalled {
		t.Fatal("Update was never called; test did not exercise the terminating existing-resource path")
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("Status.Phase = %q, want Failed", updated.Status.Phase)
	}
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
	created := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeResourceCreated)
	if created == nil || created.Status != metav1.ConditionFalse || created.Reason != "CreateFailed" {
		t.Errorf("ResourceCreated = %+v, want False reason CreateFailed", created)
	}
	ready := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "CreateFailed" {
		t.Errorf("Ready = %+v, want False reason CreateFailed", ready)
	}
}

// TestReconcileTransientUpstreamGetFailureFailsClosed covers a retryable read failure before
// the provider can determine whether the upstream object exists, is owned, or is active. With
// no verified observation, stale serving status is unsafe to preserve.
func TestReconcileTransientUpstreamGetFailureFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:    airunwayv1alpha1.ConditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "DeploymentReady",
		Message: "last observed workload is ready",
	})

	var upstreamGetCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentKind {
					upstreamGetCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream read outage"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}
	if res.Requeue || res.RequeueAfter != RequeueInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s after a transient upstream read failure",
			res.Requeue, res.RequeueAfter, RequeueInterval)
	}
	if !upstreamGetCalled {
		t.Fatal("upstream Get was never called; test did not exercise the unverified-observation path")
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("Status.Phase = %q, want Failed", updated.Status.Phase)
	}
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
	created := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeResourceCreated)
	if created == nil || created.Status != metav1.ConditionFalse || created.Reason != "CreateFailed" {
		t.Errorf("ResourceCreated = %+v, want False reason CreateFailed", created)
	}
	ready := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "CreateFailed" {
		t.Errorf("Ready = %+v, want False reason CreateFailed", ready)
	}
}

// TestReconcileOwnedUpdateConflictRetriesPromptly covers a resourceVersion race after the
// controller has read and ownership-checked an existing DynamoGraphDeployment. A 409 from
// that owned update is transient and should trigger a fresh read promptly, while preserving
// the last-known serving status.
func TestReconcileOwnedUpdateConflictRetriesPromptly(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "live-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:   airunwayv1alpha1.ConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: "DeploymentReady",
	})

	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	existing := resources[0].DeepCopy()
	if err := unstructured.SetNestedField(existing.Object, "sglang", "spec", "backendFramework"); err != nil {
		t.Fatalf("mutate existing spec: %v", err)
	}
	if err := unstructured.SetNestedField(existing.Object, string(DynamoStateSuccessful), "status", "state"); err != nil {
		t.Fatalf("mark existing resource ready: %v", err)
	}

	var updateCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentKind {
					updateCalled = true
					return apierrors.NewConflict(
						schema.GroupResource{Group: DynamoAPIGroup, Resource: "dynamographdeployments"},
						obj.GetName(), fmt.Errorf("the object has been modified"),
					)
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; a conflict should requeue: %v", err)
	}
	if !updateCalled {
		t.Fatal("Update was never called; test did not exercise the owned existing-resource path")
	}
	if res.Requeue || res.RequeueAfter != time.Second {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s after an update conflict",
			res.Requeue, res.RequeueAfter, time.Second)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("Status.Phase = %q, want Running", updated.Status.Phase)
	}
	if updated.Status.Endpoint == nil || updated.Status.Endpoint.Service != "live-svc" {
		t.Errorf("Status.Endpoint = %+v, want live-svc", updated.Status.Endpoint)
	}
	if updated.Status.Replicas == nil || updated.Status.Replicas.Ready != 1 {
		t.Errorf("Status.Replicas = %+v, want last-known ready count preserved", updated.Status.Replicas)
	}
	created := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeResourceCreated)
	if created == nil || created.Reason != "ResourceConflict" {
		t.Errorf("ResourceCreated = %+v, want reason ResourceConflict", created)
	}
	ready := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want last-known True condition preserved", ready)
	}
}

// TestReconcileTransientCreateFailureClearsStaleStatus covers the same retryable API error
// when the controller has just observed that no upstream resource exists. There is no known
// workload to preserve, so stale serving status must be cleared while retrying promptly.
func TestReconcileTransientCreateFailureClearsStaleStatus(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:    airunwayv1alpha1.ConditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "DeploymentReady",
		Message: "last observed workload is ready",
	})

	var createCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentKind {
					createCalled = true
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
	if res.Requeue || res.RequeueAfter != RequeueInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s after a transient create failure",
			res.Requeue, res.RequeueAfter, RequeueInterval)
	}
	if !createCalled {
		t.Fatal("Create was never called; test did not exercise the absent-resource path")
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("Status.Phase = %q, want Failed", updated.Status.Phase)
	}
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
	if condition := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeReady); condition == nil || condition.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v, want False", condition)
	}
}

// TestReconcileNotFoundWriteFailureClearsStaleStatus covers a definite API-server 404.
// Unlike an ambiguous 5xx or transport failure, a 404 proves this write did not update an
// existing workload, so stale serving status must be cleared while retrying promptly.
func TestReconcileNotFoundWriteFailureClearsStaleStatus(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:    airunwayv1alpha1.ConditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "DeploymentReady",
		Message: "last observed workload is ready",
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentKind {
					return apierrors.NewNotFound(
						schema.GroupResource{Group: DynamoAPIGroup, Resource: "dynamographdeployments"},
						obj.GetName(),
					)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the 404 should be reported in status: %v", err)
	}
	if res.Requeue || res.RequeueAfter != RequeueInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s after a 404",
			res.Requeue, res.RequeueAfter, RequeueInterval)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("Status.Phase = %q, want Failed", updated.Status.Phase)
	}
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
	if condition := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeReady); condition == nil || condition.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v, want False", condition)
	}
}

// TestReconcileValidationFailureIsTerminal distinguishes deterministic admission failures
// from retryable transport errors and schema-version mismatches. It fails closed and retries
// slowly so an out-of-band policy or CRD repair can recover it without a spec edit.
func TestReconcileValidationFailureIsTerminal(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:    airunwayv1alpha1.ConditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "DeploymentReady",
		Message: "last observed workload is ready",
	})

	rejection := apierrors.NewInvalid(
		schema.GroupKind{Group: DynamoAPIGroup, Kind: DynamoGraphDeploymentKind},
		"test",
		field.ErrorList{field.Invalid(
			field.NewPath("spec", "services", "VllmWorker", "replicas"),
			int64(0),
			"must be greater than zero",
		)},
	)
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
		t.Fatalf("reconcile returned an error; the validation failure should be reported in status: %v", err)
	}
	if res.Requeue || res.RequeueAfter != ExternalRecoveryInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s for a deterministic validation failure",
			res.Requeue, res.RequeueAfter, ExternalRecoveryInterval)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	statuses := map[string]metav1.ConditionStatus{}
	reasons := map[string]string{}
	for _, cond := range updated.Status.Conditions {
		statuses[cond.Type] = cond.Status
		reasons[cond.Type] = cond.Reason
	}
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("Status.Phase = %q, want Failed", updated.Status.Phase)
	}
	if got := statuses[airunwayv1alpha1.ConditionTypeReady]; got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", got)
	}
	if got := statuses[airunwayv1alpha1.ConditionTypeResourceCreated]; got != metav1.ConditionFalse {
		t.Errorf("ResourceCreated = %q, want False", got)
	}
	if got := reasons[airunwayv1alpha1.ConditionTypeResourceCreated]; got != "CreateFailed" {
		t.Errorf("ResourceCreated reason = %q, want CreateFailed", got)
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

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error: %v", err)
	}
	if res.Requeue || res.RequeueAfter != ExternalRecoveryInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s for an ownership conflict",
			res.Requeue, res.RequeueAfter, ExternalRecoveryInterval)
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
