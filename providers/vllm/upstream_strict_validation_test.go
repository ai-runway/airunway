/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package vllm

import (
	"context"
	"fmt"
	"io"
	"strings"
	"syscall"
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

	r := NewVLLMProviderReconciler(c, newScheme())

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

func TestIsRetryableUpstreamWriteErrorTransportEOF(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "wrapped resource write EOF",
			err:  wrapResourceWriteError(io.EOF, true),
		},
		{
			name: "wrapped resource write unexpected EOF",
			err:  wrapResourceWriteError(io.ErrUnexpectedEOF, true),
		},
		{
			name: "wrapped resource write broken pipe",
			err:  wrapResourceWriteError(syscall.EPIPE, true),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !isRetryableUpstreamWriteError(tc.err) {
				t.Errorf("isRetryableUpstreamWriteError(%v) = false, want true", tc.err)
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

	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the rejection should be reported in status: %v", err)
	}
	if res.RequeueAfter != ExternalRecoveryInterval {
		t.Errorf("RequeueAfter = %s, want external recovery interval %s", res.RequeueAfter, ExternalRecoveryInterval)
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
	r := NewVLLMProviderReconciler(c, scheme)

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

// TestReconcileTransientWriteFailurePreservesLastKnownStatus covers retryable API failures.
// A failed update to an observed existing resource does not prove the workload stopped
// serving, so the provider records the failed write while retaining last-known status.
func TestReconcileTransientWriteFailurePreservesLastKnownStatus(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
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

	deployment := ownedDeploymentForMD(md)
	seedRunningDeploymentStatus(t, deployment)
	service := ownedServiceForMD(md)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, deployment, service).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}
	if res.RequeueAfter != RequeueInterval {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, RequeueInterval)
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

// TestReconcileTransientApplyFailureWithUnreadyDeploymentFailsClosed proves that existence,
// ownership, and a non-terminating object are not enough to retain stale serving health. The
// pre-write Deployment snapshot has a stale Available=True condition but zero current replica
// counts, so a retryable apply failure must use the prompt retry cadence while clearing the
// stale Ready/endpoint/replica status.
func TestReconcileTransientApplyFailureWithUnreadyDeploymentFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedStaleServingStatus(md)

	deployment := ownedDeploymentForMD(md)
	seedUnreadyDeploymentStatus(t, deployment)
	service := ownedServiceForMD(md)
	deploymentPatched := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, deployment, service).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
					deploymentPatched = true
					return apierrors.NewInternalError(fmt.Errorf("simulated transient Deployment apply failure"))
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the apply failure should be reported in status: %v", err)
	}
	if !deploymentPatched {
		t.Fatal("Deployment Patch was never called")
	}
	assertPromptRetryFailedClosed(t, c, res, types.NamespacedName{Name: "test", Namespace: "default"})
}

// TestReconcileTransientLaterServiceFailureAfterSuccessfulDeploymentApplyFailsClosed proves
// that the pre-write serving snapshot becomes stale as soon as an earlier resource write
// succeeds. Even when every required resource was initially ready, a later ambiguous Service
// failure cannot retain Ready because the Deployment apply may already have started a rollout.
func TestReconcileTransientLaterServiceFailureAfterSuccessfulDeploymentApplyFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedStaleServingStatus(md)

	deployment := ownedDeploymentForMD(md)
	seedRunningDeploymentStatus(t, deployment)
	service := ownedServiceForMD(md)
	var patchOrder []string
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, deployment, service).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				kind := obj.GetObjectKind().GroupVersionKind().Kind
				switch kind {
				case "Deployment":
					patchOrder = append(patchOrder, kind)
					return nil
				case "Service":
					patchOrder = append(patchOrder, kind)
					return apierrors.NewInternalError(fmt.Errorf("simulated transient Service apply failure"))
				default:
					return cl.Patch(ctx, obj, patch, opts...)
				}
			},
		}).Build()
	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the later apply failure should be reported in status: %v", err)
	}
	if len(patchOrder) != 2 || patchOrder[0] != "Deployment" || patchOrder[1] != "Service" {
		t.Fatalf("patch order = %v, want [Deployment Service]", patchOrder)
	}
	assertPromptRetryFailedClosed(t, c, res, types.NamespacedName{Name: "test", Namespace: "default"})
}

// TestReconcileTransientCreateFailureFailsClosed covers an ambiguous write failure after the
// controller successfully observed that the primary Deployment was absent. There is no
// workload whose health can be preserved, so stale Running status must be cleared even though
// the 5xx gets the prompt transient retry cadence.
func TestReconcileTransientCreateFailureFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedStaleServingStatus(md)

	deploymentPatched := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
					deploymentPatched = true
					return apierrors.NewInternalError(fmt.Errorf("simulated transient create failure"))
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the create failure should be reported in status: %v", err)
	}
	if !deploymentPatched {
		t.Fatal("Deployment Patch was never called")
	}
	assertPromptRetryFailedClosed(t, c, res, types.NamespacedName{Name: "test", Namespace: "default"})
}

// TestReconcileTransientMissingServiceFailureFailsClosed covers a partially present resource
// set: the primary Deployment exists, but its required Service does not. A successful
// Deployment update cannot justify retaining Ready when creation of the missing child fails.
func TestReconcileTransientMissingServiceFailureFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedStaleServingStatus(md)

	deployment := ownedDeploymentForMD(md)
	seedRunningDeploymentStatus(t, deployment)
	deploymentPatched := false
	servicePatched := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, deployment).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				switch obj.GetObjectKind().GroupVersionKind().Kind {
				case "Deployment":
					deploymentPatched = true
					return nil
				case "Service":
					servicePatched = true
					return apierrors.NewInternalError(fmt.Errorf("simulated transient Service create failure"))
				default:
					return cl.Patch(ctx, obj, patch, opts...)
				}
			},
		}).Build()
	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the create failure should be reported in status: %v", err)
	}
	if !deploymentPatched || !servicePatched {
		t.Fatalf("patch calls: Deployment=%v Service=%v, want both", deploymentPatched, servicePatched)
	}
	assertPromptRetryFailedClosed(t, c, res, types.NamespacedName{Name: "test", Namespace: "default"})
}

// TestReconcileTransientExistingServiceAfterMissingDeploymentFailsClosed reverses the partial
// resource set above: the required primary Deployment was absent, while its Service already
// existed. Even though the Deployment apply succeeds and the later transient failure is an
// update to that Service, the reconcile cannot preserve stale serving health after observing
// the primary workload missing earlier in the same resource pass.
func TestReconcileTransientExistingServiceAfterMissingDeploymentFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedStaleServingStatus(md)

	service := ownedServiceForMD(md)
	deploymentPatched := false
	servicePatched := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, service).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				switch obj.GetObjectKind().GroupVersionKind().Kind {
				case "Deployment":
					deploymentPatched = true
					return nil
				case "Service":
					servicePatched = true
					return apierrors.NewInternalError(fmt.Errorf("simulated transient Service update failure"))
				default:
					return cl.Patch(ctx, obj, patch, opts...)
				}
			},
		}).Build()
	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the update failure should be reported in status: %v", err)
	}
	if !deploymentPatched || !servicePatched {
		t.Fatalf("patch calls: Deployment=%v Service=%v, want both", deploymentPatched, servicePatched)
	}
	assertPromptRetryFailedClosed(t, c, res, types.NamespacedName{Name: "test", Namespace: "default"})
}

// TestReconcileTransientEarlierFailureWithForeignRequiredResourceFailsClosed proves the
// pre-write resource snapshot validates ownership across the complete set. The Deployment
// update fails before reconciliation reaches the foreign Service; mere presence of that later
// object must not make stale Ready=True safe to preserve.
func TestReconcileTransientEarlierFailureWithForeignRequiredResourceFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedStaleServingStatus(md)

	deployment := ownedDeploymentForMD(md)
	seedRunningDeploymentStatus(t, deployment)
	service := ownedServiceForMD(md)
	service.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       "someone-else",
		UID:        types.UID("foreign-uid"),
	}})
	deploymentPatched := false
	servicePatched := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, deployment, service).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				switch obj.GetObjectKind().GroupVersionKind().Kind {
				case "Deployment":
					deploymentPatched = true
					return apierrors.NewInternalError(fmt.Errorf("simulated transient Deployment update failure"))
				case "Service":
					servicePatched = true
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the update failure should be reported in status: %v", err)
	}
	if !deploymentPatched {
		t.Fatal("Deployment Patch was never called")
	}
	if servicePatched {
		t.Fatal("Service Patch was called despite the earlier Deployment failure")
	}
	assertPromptRetryFailedClosed(t, c, res, types.NamespacedName{Name: "test", Namespace: "default"})
}

// TestReconcileTransientEarlierFailureWithTerminatingRequiredResourceFailsClosed proves an
// object already being deleted is not counted as a usable member of the required resource
// set. A retryable Deployment update error occurs first, but stale serving status must still
// be cleared because the later Service has a deletion timestamp.
func TestReconcileTransientEarlierFailureWithTerminatingRequiredResourceFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedStaleServingStatus(md)

	deployment := ownedDeploymentForMD(md)
	seedRunningDeploymentStatus(t, deployment)
	service := ownedServiceForMD(md)
	service.SetFinalizers([]string{"test.airunway.ai/hold"})
	deletingAt := metav1.Now()
	service.SetDeletionTimestamp(&deletingAt)
	deploymentPatched := false
	servicePatched := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, deployment, service).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				switch obj.GetObjectKind().GroupVersionKind().Kind {
				case "Deployment":
					deploymentPatched = true
					return apierrors.NewInternalError(fmt.Errorf("simulated transient Deployment update failure"))
				case "Service":
					servicePatched = true
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the update failure should be reported in status: %v", err)
	}
	if !deploymentPatched {
		t.Fatal("Deployment Patch was never called")
	}
	if servicePatched {
		t.Fatal("Service Patch was called despite the earlier Deployment failure")
	}
	assertPromptRetryFailedClosed(t, c, res, types.NamespacedName{Name: "test", Namespace: "default"})
}

func ownedDeploymentForMD(md *airunwayv1alpha1.ModelDeployment) *unstructured.Unstructured {
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(deploymentGVK)
	deployment.SetName(md.Name)
	deployment.SetNamespace(md.Namespace)
	deployment.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       md.Name,
		UID:        md.UID,
	}})
	return deployment
}

func seedRunningDeploymentStatus(t *testing.T, deployment *unstructured.Unstructured) {
	t.Helper()
	seedDeploymentStatus(t, deployment, "True", int64(1), int64(1))
}

func seedUnreadyDeploymentStatus(t *testing.T, deployment *unstructured.Unstructured) {
	t.Helper()
	seedDeploymentStatus(t, deployment, "True", int64(0), int64(0))
	if err := unstructured.SetNestedSlice(deployment.Object, []interface{}{
		map[string]interface{}{"type": conditionAvailable, "status": "True"},
		map[string]interface{}{"type": conditionProgressing, "status": "True"},
	}, "status", "conditions"); err != nil {
		t.Fatalf("set unready Deployment conditions: %v", err)
	}
}

func seedDeploymentStatus(t *testing.T, deployment *unstructured.Unstructured, available string, readyReplicas, availableReplicas int64) {
	t.Helper()
	if err := unstructured.SetNestedField(deployment.Object, int64(1), "spec", "replicas"); err != nil {
		t.Fatalf("set Deployment desired replicas: %v", err)
	}
	if err := unstructured.SetNestedField(deployment.Object, readyReplicas, "status", "readyReplicas"); err != nil {
		t.Fatalf("set Deployment ready replicas: %v", err)
	}
	if err := unstructured.SetNestedField(deployment.Object, availableReplicas, "status", "availableReplicas"); err != nil {
		t.Fatalf("set Deployment available replicas: %v", err)
	}
	if err := unstructured.SetNestedSlice(deployment.Object, []interface{}{
		map[string]interface{}{"type": conditionAvailable, "status": available},
	}, "status", "conditions"); err != nil {
		t.Fatalf("set Deployment conditions: %v", err)
	}
}

func ownedServiceForMD(md *airunwayv1alpha1.ModelDeployment) *unstructured.Unstructured {
	service := &unstructured.Unstructured{}
	service.SetGroupVersionKind(serviceGVK)
	service.SetName(md.Name)
	service.SetNamespace(md.Namespace)
	service.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       md.Name,
		UID:        md.UID,
	}})
	return service
}

func seedStaleServingStatus(md *airunwayv1alpha1.ModelDeployment) {
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:   airunwayv1alpha1.ConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: "DeploymentReady",
	})
}

func assertPromptRetryFailedClosed(t *testing.T, c client.Client, res ctrl.Result, key types.NamespacedName) {
	t.Helper()
	if res.Requeue || res.RequeueAfter != RequeueInterval {
		t.Errorf("requeue = %+v, want RequeueAfter=%s", res, RequeueInterval)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	statuses := map[string]metav1.ConditionStatus{}
	reasons := map[string]string{}
	for _, cond := range updated.Status.Conditions {
		statuses[cond.Type] = cond.Status
		reasons[cond.Type] = cond.Reason
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
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("phase = %q, want %q", updated.Status.Phase, airunwayv1alpha1.DeploymentPhaseFailed)
	}
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
}

// TestReconcileNotFoundWriteFailureClearsStaleStatus covers a definite API-server 404.
// Unlike an ambiguous transport or server failure, NotFound proves the write did not leave
// the expected object at that name, so retaining Running/Ready and its endpoint would report
// a workload the controller knows is absent. It still retries promptly because recreation can
// succeed without a ModelDeployment change.
func TestReconcileNotFoundWriteFailureClearsStaleStatus(t *testing.T) {
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

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
					return apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName())
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}
	if res.Requeue || res.RequeueAfter != RequeueInterval {
		t.Errorf("requeue = %+v, want RequeueAfter=%s", res, RequeueInterval)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	var createdReason, readyReason string
	var readyStatus metav1.ConditionStatus
	for _, cond := range updated.Status.Conditions {
		switch cond.Type {
		case airunwayv1alpha1.ConditionTypeResourceCreated:
			createdReason = cond.Reason
		case airunwayv1alpha1.ConditionTypeReady:
			readyStatus = cond.Status
			readyReason = cond.Reason
		}
	}
	if createdReason != "CreateFailed" {
		t.Errorf("ResourceCreated reason = %q, want CreateFailed", createdReason)
	}
	if readyStatus != metav1.ConditionFalse || readyReason != "CreateFailed" {
		t.Errorf("Ready = %q reason %q, want False reason CreateFailed", readyStatus, readyReason)
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

// TestGenericFailureNormalisesVolatileSSADetail closes the one hole in this change's own
// invariant.
//
// A server-side apply TYPE mismatch carries the same "failed to create typed patch object"
// wrapper but NOT the unknown-field needle, so isUpstreamSchemaRejection returns false and it
// lands in the generic branch rather than the rejection branch. structured-merge-diff
// accumulates type errors without sorting them, so their concatenation order follows Go map
// iteration — just as volatile as the unknown-field case. The failure needs an external
// change, so it must fail closed and retry only at the slower recovery cadence.
func TestGenericFailureNormalisesVolatileSSADetail(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:   airunwayv1alpha1.ConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: "DeploymentReady",
	})

	// Kubernetes wraps deterministic managed-fields conversion failures as HTTP 500.
	// The status code must not make an invalid override look transient.
	typeMismatch := apierrors.NewInternalError(fmt.Errorf("failed to create typed patch object (default/test; apps/v1, " +
		"Kind=Deployment): .spec.replicas: expected numeric, got string"))

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
					return typeMismatch
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error: %v", err)
	}
	if res.Requeue || res.RequeueAfter != ExternalRecoveryInterval {
		t.Errorf("requeue = %+v, want RequeueAfter=%s for deterministic SSA validation failure", res, ExternalRecoveryInterval)
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
	var readyStatus metav1.ConditionStatus
	for _, cond := range updated.Status.Conditions {
		if cond.Type == airunwayv1alpha1.ConditionTypeReady {
			readyStatus = cond.Status
		}
	}
	if readyStatus != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", readyStatus)
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
}

// TestReconcileOwnershipConflictSetsReadyReason covers the ownership-collision branch's Ready
// condition specifically.
//
// vLLM already had ownership coverage in controller_test.go, but it asserts only
// ResourceCreated. The Ready condition carries the same reason, and an operator alerting on
// Ready.reason=ResourceConflict must be able to tell a collision needing manual intervention
// from a transient CreateFailed that will retry on its own.
func TestReconcileOwnershipConflictSetsReadyReason(t *testing.T) {
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
	r := NewVLLMProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error: %v", err)
	}
	if res.RequeueAfter != ExternalRecoveryInterval {
		t.Errorf("RequeueAfter = %s, want external recovery interval %s", res.RequeueAfter, ExternalRecoveryInterval)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	var readyReason string
	for _, cond := range updated.Status.Conditions {
		if cond.Type == airunwayv1alpha1.ConditionTypeReady {
			readyReason = cond.Reason
		}
	}
	if readyReason != "ResourceConflict" {
		t.Errorf("Ready reason = %q, want ResourceConflict", readyReason)
	}
}
