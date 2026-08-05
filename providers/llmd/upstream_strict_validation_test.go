/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package llmd

import (
	"context"
	"fmt"
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

// Regression test for upstream schema compatibility.
//
// Every write to the upstream resource must carry fieldValidation=Strict, so the API
// server rejects fields the installed upstream does not declare instead of silently
// pruning them. Without it the shim can render a field the cluster drops without any
// error, leaving a workload that reports healthy but cannot serve.
//
// This asserts the option reaches the client. It deliberately does NOT assert the API
// server's behaviour — the fake client has no structural schema and cannot prune, which
// is exactly why no pre-existing test caught this bug.

func TestUpstreamApplyUsesStrictFieldValidation(t *testing.T) {
	var got string
	var called bool
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			called = true
			o := &client.PatchOptions{}
			for _, opt := range opts {
				opt.ApplyToPatch(o)
			}
			got = o.FieldValidation
			return nil
		},
	}).Build()

	r := NewLLMDProviderReconciler(c, newScheme())

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = types.UID("test-uid")

	desired := &unstructured.Unstructured{}
	desired.SetAPIVersion("apps/v1")
	desired.SetKind("Deployment")
	desired.SetName("test")
	desired.SetNamespace("default")
	desired.Object["spec"] = map[string]interface{}{"replicas": int64(1)}

	if err := r.createOrUpdateResource(context.Background(), desired, md); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("Patch was never called")
	}
	if got != metav1.FieldValidationStrict {
		t.Errorf("apply fieldValidation = %q, want %q", got, metav1.FieldValidationStrict)
	}
}

// TestIsUpstreamSchemaRejection pins the matcher to error strings observed against a live
// cluster. The API server's response class varies by write path — custom-resource create is
// 400, merge patch is 422, and server-side apply on a built-in type is **500** (the error
// comes from the field manager, not from field validation) — which is why this matches on
// message rather than status code.
//
// The negative cases are the point: gating on IsInvalid would capture ordinary CEL and type
// violations and report them as an upstream version mismatch, sending an operator off to
// upgrade a cluster that is already fine.
func TestIsUpstreamSchemaRejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			// Unreachable for this provider: it writes only apps/v1 Deployment and v1 Service,
			// both through server-side apply, so a custom-resource strict-decoding error can
			// never arrive. Not matching it is deliberate — the phrase would otherwise be a
			// way for an ordinary validation error echoing a user value to be misread as a
			// version mismatch and retried forever.
			name: "not ours: custom-resource strict decoding (this provider writes no CRs)",
			err:  apierrors.NewBadRequest(`strict decoding error: unknown field "spec.services.VllmWorker.frontendSidecar"`),
			want: false,
		},
		{
			// The sibling wrapper. structuredmerge emits this one when the LIVE object
			// carries a field the current schema no longer declares.
			name: "server-side apply, live-object wrapper (500)",
			err:  fmt.Errorf("failed to create typed live object (default/x; apps/v1, Kind=Deployment): .spec.legacy: field not declared in schema"),
			want: true,
		},
		{
			name: "observed: server-side apply on a built-in type (500)",
			err:  fmt.Errorf("failed to create typed patch object (default/x; apps/v1, Kind=Deployment): .spec.bogus: field not declared in schema"),
			want: true,
		},
		{
			name: "not ours: CEL or type validation failure the user must fix",
			err:  apierrors.NewInvalid(schema.GroupKind{Kind: "X"}, "x", nil),
			want: false,
		},
		{
			// An Invalid status echoes the offending value back, so a user-supplied string can
			// carry any phrasing at all. This provider matches no bare phrase, so none of the
			// three cases below can be misread as an upstream mismatch and retried every 30s.
			name: "not ours: Invalid echoing a user value containing 'unknown field'",
			err: apierrors.NewInvalid(schema.GroupKind{Kind: "X"}, "x", field.ErrorList{
				field.Invalid(field.NewPath("spec", "engine", "extraArgs"),
					"--served-model-name=unknown field probe", "must be a valid flag"),
			}),
			want: false,
		},
		{
			name: "not ours: Invalid echoing a user value containing 'strict decoding error'",
			err: apierrors.NewInvalid(schema.GroupKind{Kind: "X"}, "x", field.ErrorList{
				field.Invalid(field.NewPath("spec", "engine", "extraArgs"),
					"--served-model-name=strict decoding error probe", "must be a valid flag"),
			}),
			want: false,
		},
		{
			// The adversarial case: both phrases inside one user-supplied value. A provider
			// that matches the custom-resource shape has to weigh this against the false
			// negative of a stricter match; this one writes no custom resources, so the
			// question does not arise and the answer is simply no.
			name: "not ours: Invalid echoing a value containing both phrases at once",
			err: apierrors.NewInvalid(schema.GroupKind{Kind: "X"}, "x", field.ErrorList{
				field.Invalid(field.NewPath("spec", "engine", "extraArgs"),
					"--served-model-name=strict decoding error unknown field", "must be a valid flag"),
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

// TestReconcileRequeuesOnStrictRejection covers the recovery path for the failure mode
// strict validation introduces. A schema rejection is terminal from the controller's point
// of view — the remedy is an out-of-band upstream upgrade — and nothing in the watch set
// re-triggers this reconcile afterwards, so without an explicit requeue the deployment
// would sit Failed until the ~10h resync even after the cluster is fixed.
func TestReconcileRequeuesOnStrictRejection(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)

	// Seed the Running-era status. Without this the clearing below is a no-op the
	// assertions cannot observe, and deleting it from the controller would not fail
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1}

	// The shape this provider can actually receive. It renders built-in types through
	// server-side apply, where the rejection comes from the field manager as a plain error
	// naming ONE arbitrarily-chosen unknown field — never the custom-resource
	// "strict decoding error" wording.
	rejection := fmt.Errorf("failed to create typed patch object (default/test; apps/v1, " +
		"Kind=Deployment): .spec.someNewerField: field not declared in schema")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				// Gated on Kind so this cannot pass for the wrong reason if a future
				// refactor adds an earlier Patch to the reconcile path.
				if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
					return rejection
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	r := NewLLMDProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the rejection should be reported in status: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Error("expected a requeue after a strict-validation rejection")
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
	if got := reasons[airunwayv1alpha1.ConditionTypeResourceCreated]; got != "IncompatibleUpstream" {
		t.Errorf("ResourceCreated reason = %q, want %q", got, "IncompatibleUpstream")
	}
	if got := statuses[airunwayv1alpha1.ConditionTypeReady]; got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False — #308 is about reporting healthy while broken", got)
	}
	// docs/providers.md promises BOTH conditions carry this reason.
	if got := reasons[airunwayv1alpha1.ConditionTypeReady]; got != "IncompatibleUpstream" {
		t.Errorf("Ready reason = %q, want IncompatibleUpstream", got)
	}
	// ProviderCompatible is deliberately NOT touched here: it is set True earlier in the same
	// reconcile, so flipping it would rewrite LastTransitionTime on every 30s requeue and the
	// condition would never settle — a permanent status-write loop.
	if got := statuses[airunwayv1alpha1.ConditionTypeProviderCompatible]; got != metav1.ConditionTrue {
		t.Errorf("ProviderCompatible = %q, want True — flipping it here causes permanent churn", got)
	}

	// The volatile field path must be normalised out of anything stored. SSA names only one
	// of possibly several unknown fields and picks a different one per call, so storing it
	// raw would rewrite status every reconcile and re-enqueue the object each time.
	if strings.Contains(updated.Status.Message, "someNewerField") {
		t.Errorf("status.message retains the volatile field path: %q", updated.Status.Message)
	}
	if !strings.Contains(updated.Status.Message, "controller logs") {
		t.Errorf("status.message is not the normalised form: %q", updated.Status.Message)
	}

	// Condition messages are status too. meta.SetStatusCondition propagates a changed
	// Message unconditionally, so a volatile field path here mutates the object on every
	// requeue and re-enqueues it — the same loop, with status.message looking stable.
	for _, cond := range updated.Status.Conditions {
		if cond.Type != airunwayv1alpha1.ConditionTypeReady &&
			cond.Type != airunwayv1alpha1.ConditionTypeResourceCreated {
			continue
		}
		if strings.Contains(cond.Message, "someNewerField") {
			t.Errorf("condition %s retains the volatile field path: %q", cond.Type, cond.Message)
		}
		if !strings.Contains(cond.Message, "controller logs") {
			t.Errorf("condition %s is not the normalised form: %q", cond.Type, cond.Message)
		}
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
	r := NewLLMDProviderReconciler(c, scheme)

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

// TestStatusSafeRejectionDetailIsStable guards against a hot reconcile loop.
//
// Server-side apply reports only the first unknown field it hits, and picks a different one
// per call when several are unknown — measured against a live API server: three unknown
// fields produced three different messages across twelve calls. Storing that raw would
// rewrite status.message every reconcile, and each status write re-enqueues the object
// because the ModelDeployment watch has no GenerationChangedPredicate.
func TestStatusSafeRejectionDetailIsStable(t *testing.T) {
	// The same rejection, as the API server might word it on successive calls.
	variants := []error{
		fmt.Errorf("failed to create typed patch object (default/x; apps/v1, Kind=Deployment): .spec.alphaUnknown: field not declared in schema"),
		fmt.Errorf("failed to create typed patch object (default/x; apps/v1, Kind=Deployment): .spec.midUnknown: field not declared in schema"),
		fmt.Errorf("failed to create typed patch object (default/x; apps/v1, Kind=Deployment): .spec.zebraUnknown: field not declared in schema"),
	}
	first := statusSafeRejectionDetail(variants[0])
	for _, v := range variants[1:] {
		if got := statusSafeRejectionDetail(v); got != first {
			t.Errorf("detail differs between equivalent rejections:\n  %q\n  %q\n"+
				"an unstable status.message re-enqueues the object on every write", first, got)
		}
	}

	// The live-object wrapper must normalise too, or the volatile detail leaks through.
	liveVariants := []error{
		fmt.Errorf("failed to create typed live object (default/x; apps/v1, Kind=Deployment): .spec.alpha: field not declared in schema"),
		fmt.Errorf("failed to create typed live object (default/x; apps/v1, Kind=Deployment): .spec.zeta: field not declared in schema"),
	}
	if a, b := statusSafeRejectionDetail(liveVariants[0]), statusSafeRejectionDetail(liveVariants[1]); a != b {
		t.Errorf("live-object wrapper detail is not normalised:\n  %q\n  %q", a, b)
	}

	// Custom-resource strict decoding lists every unknown field in sorted order, so those
	// messages are already stable and must pass through with their detail intact.
	crErr := fmt.Errorf(`strict decoding error: unknown field "spec.a", unknown field "spec.b"`)
	if got := statusSafeRejectionDetail(crErr); got != crErr.Error() {
		t.Errorf("strict-decoding detail should pass through unchanged, got %q", got)
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
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := NewLLMDProviderReconciler(c, scheme)

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

// TestUnsupportedOverrideErrorIsDeterministic guards against a status-write loop.
//
// Go randomises map iteration, so returning on the first unsupported key yields a different
// message per call for the same spec. That message lands in status.message, and because the
// ModelDeployment watch has no GenerationChangedPredicate, a changing message means every
// reconcile writes status, which re-enqueues the object.
func TestUnsupportedOverrideErrorIsDeterministic(t *testing.T) {
	newMD := func() *airunwayv1alpha1.ModelDeployment {
		md := newMDForController("test", "default")
		md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
			Name:      md.Status.Provider.Name,
			Overrides: &runtime.RawExtension{Raw: []byte(`{"zeta":{},"alpha":{},"mu":{}}`)},
		}
		return md
	}
	var first string
	for i := 0; i < 100; i++ {
		_, err := NewTransformer().Transform(context.Background(), newMD())
		if err == nil {
			t.Fatal("expected an error for unsupported override keys")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("override rejection message is nondeterministic:\n  %q\n  %q", first, err.Error())
		}
	}
	for _, want := range []string{"alpha", "mu", "zeta"} {
		if !strings.Contains(first, want) {
			t.Errorf("error %q does not name offending key %q", first, want)
		}
	}
}

// TestOverrideRootKeyComparisonIsCaseSensitive pins a deliberate choice: the accepted root
// key is matched case-sensitively, so "Spec" is rejected rather than merged. encoding/json
// would not decode it into anything meaningful, and merging it would push a key no upstream
// schema declares into the rendered object.
func TestOverrideRootKeyComparisonIsCaseSensitive(t *testing.T) {
	md := newMDForController("test", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Name:      md.Status.Provider.Name,
		Overrides: &runtime.RawExtension{Raw: []byte(`{"Spec":{"x":1}}`)},
	}
	if _, err := NewTransformer().Transform(context.Background(), md); err == nil {
		t.Error("a capitalised root key was accepted; it must be rejected, not merged")
	}
}

// TestReconcileOwnershipConflictUsesItsOwnReason covers the ownership-collision branch.
//
// A Deployment of the right name already exists but belongs to someone else. That is terminal
// — unlike a transient API 409 — so it is reported under its own reason rather than the
// generic CreateFailed.
func TestReconcileOwnershipConflictUsesItsOwnReason(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)

	foreign := &unstructured.Unstructured{}
	foreign.SetAPIVersion("apps/v1")
	foreign.SetKind("Deployment")
	foreign.SetName("test")
	foreign.SetNamespace("default")
	foreign.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       "someone-else",
		UID:        types.UID("a-different-uid"),
	}})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, foreign).WithStatusSubresource(md).Build()
	r := NewLLMDProviderReconciler(c, scheme)

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

// TestGenericFailureNormalisesVolatileSSADetail closes the one hole in this change's own
// invariant.
//
// A server-side apply TYPE mismatch carries the same "failed to create typed patch object"
// wrapper but NOT the unknown-field needle, so isUpstreamSchemaRejection returns false and it
// lands in the generic branch rather than the rejection branch. structured-merge-diff
// accumulates type errors without sorting them, so their concatenation order follows Go map
// iteration — just as volatile as the unknown-field case, and with a 30s requeue on top.
func TestGenericFailureNormalisesVolatileSSADetail(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)

	typeMismatch := fmt.Errorf("failed to create typed patch object (default/test; apps/v1, " +
		"Kind=Deployment): .spec.replicas: expected numeric, got string")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
					return typeMismatch
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	r := NewLLMDProviderReconciler(c, scheme)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	}); err != nil {
		t.Fatalf("reconcile returned an error: %v", err)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	// It must NOT reach the rejection branch...
	for _, cond := range updated.Status.Conditions {
		if cond.Type == airunwayv1alpha1.ConditionTypeResourceCreated && cond.Reason != "CreateFailed" {
			t.Errorf("ResourceCreated reason = %q, want CreateFailed — a type mismatch is not a schema rejection", cond.Reason)
		}
	}
	// ...and the volatile per-call detail must still be normalised out of stored status.
	if strings.Contains(updated.Status.Message, "expected numeric") {
		t.Errorf("status.message retains the volatile SSA detail: %q", updated.Status.Message)
	}
	// The normalised wording must stay cause-neutral. This branch is reached precisely when
	// the error is NOT an unknown-field rejection, so claiming "not declared in its schema"
	// would send an operator hunting for a field that exists and is merely mistyped.
	if strings.Contains(updated.Status.Message, "not declared") {
		t.Errorf("generic-failure message asserts a cause this branch cannot know: %q", updated.Status.Message)
	}
	if !strings.Contains(updated.Status.Message, "controller logs") {
		t.Errorf("generic-failure message does not point at the logs: %q", updated.Status.Message)
	}
	for _, cond := range updated.Status.Conditions {
		if strings.Contains(cond.Message, "expected numeric") {
			t.Errorf("condition %s retains the volatile SSA detail: %q", cond.Type, cond.Message)
		}
	}
}
