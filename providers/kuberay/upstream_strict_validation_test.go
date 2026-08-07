/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package kuberay

import (
	"context"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func ownedRayServiceNeedingUpdate(t *testing.T, md *airunwayv1alpha1.ModelDeployment) *unstructured.Unstructured {
	t.Helper()

	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform existing RayService: %v", err)
	}
	existing := resources[0].DeepCopy()
	if err := unstructured.SetNestedField(existing.Object, "stale", "spec", "serveConfigV2"); err != nil {
		t.Fatalf("make existing RayService differ from desired: %v", err)
	}
	return existing
}

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

func TestUpstreamWritesUseStrictFieldValidation(t *testing.T) {
	owner := []metav1.OwnerReference{{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       "test",
		UID:        types.UID("test-uid"),
	}}

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = types.UID("test-uid")

	desired := func() *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		setRayServiceGVK(u)
		u.SetName("test")
		u.SetNamespace("default")
		u.SetOwnerReferences(owner)
		u.Object["spec"] = map[string]interface{}{"serveConfigV2": "desired"}
		return u
	}

	t.Run("create", func(t *testing.T) {
		var got string
		var called bool
		c := fake.NewClientBuilder().WithScheme(newScheme()).WithInterceptorFuncs(interceptor.Funcs{
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

		r := NewKubeRayProviderReconciler(c, newScheme())
		if err := r.createOrUpdateResource(context.Background(), desired(), md); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !called {
			t.Fatal("Create was never called")
		}
		if got != metav1.FieldValidationStrict {
			t.Errorf("create fieldValidation = %q, want %q", got, metav1.FieldValidationStrict)
		}
	})

	t.Run("update", func(t *testing.T) {
		existing := &unstructured.Unstructured{}
		setRayServiceGVK(existing)
		existing.SetName("test")
		existing.SetNamespace("default")
		existing.SetOwnerReferences(owner)
		existing.Object["spec"] = map[string]interface{}{"serveConfigV2": "stale"}

		var got string
		var called bool
		c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(existing).
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

		r := NewKubeRayProviderReconciler(c, newScheme())
		if err := r.createOrUpdateResource(context.Background(), desired(), md); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !called {
			t.Fatal("Update was never called")
		}
		if got != metav1.FieldValidationStrict {
			t.Errorf("update fieldValidation = %q, want %q", got, metav1.FieldValidationStrict)
		}
	})
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
			name: "observed: custom resource create against an older CRD (400)",
			err:  apierrors.NewBadRequest(`strict decoding error: unknown field "spec.services.VllmWorker.frontendSidecar"`),
			want: true,
		},
		{
			// kuberay itself never uses server-side apply; kept so the shared matcher
			// behaves identically across providers if the write path ever changes.
			name: "server-side apply on a built-in type (500) — not a path kuberay takes",
			err:  fmt.Errorf("failed to create typed patch object (default/x; apps/v1, Kind=Deployment): .spec.bogus: field not declared in schema"),
			want: true,
		},
		{
			name: "not ours: CEL or type validation failure the user must fix",
			err:  apierrors.NewInvalid(schema.GroupKind{Kind: "X"}, "x", nil),
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

	rejection := apierrors.NewBadRequest(`strict decoding error: unknown field "spec.someNewerField"`)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					return rejection
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()

	r := NewKubeRayProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the rejection should be reported in status: %v", err)
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

	// docs/providers.md promises this provider names the offending field in status.message.
	if !strings.Contains(updated.Status.Message, "someNewerField") {
		t.Errorf("status.message does not name the offending field: %q", updated.Status.Message)
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

// NOTE: there is deliberately no TestReconcileTransformFailureClearsStaleStatus here.
//
// The other four providers can be driven into the Transform-error branch with an unsupported
// provider.overrides root key. KubeRay does not read spec.provider.overrides at all, and
// buildRayClusterConfig/buildSpec have no error returns reachable from a ModelDeployment
// spec — so that branch is defensive only, and any test would have to fake a failure the
// transformer cannot actually produce. Recorded here so its absence reads as a decision
// rather than an oversight.

// TestReconcileTransientUpdateFailurePreservesLastKnownStatus covers retryable API failures
// after the provider observed an owned RayService. A failed update does not prove that the
// existing workload stopped serving, so its last-known operational status is retained.
func TestReconcileTransientUpdateFailurePreservesLastKnownStatus(t *testing.T) {
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
	existing := ownedRayServiceNeedingUpdate(t, md)
	if err := unstructured.SetNestedSlice(existing.Object, []interface{}{
		map[string]interface{}{"type": conditionRayServiceReady, "status": "True"},
	}, "status", "conditions"); err != nil {
		t.Fatalf("mark existing RayService ready: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

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

// TestReconcileTransientUpdateFailureOnUnreadyResourceFailsClosed proves that an active,
// owned RayService must also be serving before stale ModelDeployment status can be retained.
func TestReconcileTransientUpdateFailureOnUnreadyResourceFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedKubeRayStaleServingStatus(md)

	existing := ownedRayServiceNeedingUpdate(t, md)
	if err := unstructured.SetNestedSlice(existing.Object, []interface{}{
		map[string]interface{}{"type": conditionRayServiceReady, "status": "False"},
	}, "status", "conditions"); err != nil {
		t.Fatalf("mark existing RayService unready: %v", err)
	}

	var updateCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					updateCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}
	if !updateCalled {
		t.Fatal("RayService Update was never called; test did not exercise the active unready-resource path")
	}
	assertKubeRayPromptRetryFailedClosed(t, c, res, types.NamespacedName{Name: "test", Namespace: "default"})
}

// TestReconcileTransientUpdateFailureOnTerminatingResourceFailsClosed proves that presence
// alone is not enough to preserve status. A RayService with a deletion timestamp is no longer
// a verified serving workload, so an ambiguous update failure must clear stale Ready state and
// retry normally even when the object is still owned by this ModelDeployment.
func TestReconcileTransientUpdateFailureOnTerminatingResourceFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedKubeRayStaleServingStatus(md)

	existing := ownedRayServiceNeedingUpdate(t, md)
	if err := unstructured.SetNestedSlice(existing.Object, []interface{}{
		map[string]interface{}{"type": conditionRayServiceReady, "status": "True"},
	}, "status", "conditions"); err != nil {
		t.Fatalf("mark existing RayService ready: %v", err)
	}
	existing.SetFinalizers([]string{"test.airunway.ai/hold"})
	deletingAt := metav1.Now()
	existing.SetDeletionTimestamp(&deletingAt)
	updateCalled := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					updateCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated transient update failure"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the update failure should be reported in status: %v", err)
	}
	if !updateCalled {
		t.Fatal("RayService Update was never called")
	}
	assertKubeRayPromptRetryFailedClosed(t, c, res, types.NamespacedName{Name: "test", Namespace: "default"})
}

// TestReconcileTransientGetFailureFailsClosed proves an API read failure cannot preserve
// stale status: the provider never verified that an active, owned RayService exists. The read
// error is retryable, but status must fail closed until a later successful observation.
func TestReconcileTransientGetFailureFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedKubeRayStaleServingStatus(md)

	rayServiceGetCalled := false
	writeCalled := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					rayServiceGetCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated transient get failure"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					writeCalled = true
				}
				return cl.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					writeCalled = true
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the read failure should be reported in status: %v", err)
	}
	if !rayServiceGetCalled {
		t.Fatal("RayService Get was never called")
	}
	if writeCalled {
		t.Fatal("RayService write was attempted after its Get failed")
	}
	assertKubeRayPromptRetryFailedClosed(t, c, res, types.NamespacedName{Name: "test", Namespace: "default"})
}

func seedKubeRayStaleServingStatus(md *airunwayv1alpha1.ModelDeployment) {
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:   airunwayv1alpha1.ConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: "DeploymentReady",
	})
}

func assertKubeRayPromptRetryFailedClosed(t *testing.T, c client.Client, res ctrl.Result, key types.NamespacedName) {
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
		t.Errorf("Status.Phase = %q, want Failed", updated.Status.Phase)
	}
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
}

// TestReconcileTransientCreateFailureFailsClosed covers a retryable write failure after the
// provider observed that no RayService exists. There is no known serving workload to preserve,
// so status must fail closed while the provider retries promptly.
func TestReconcileTransientCreateFailureFailsClosed(t *testing.T) {
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
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the create failure should be reported in status: %v", err)
	}
	if res.Requeue || res.RequeueAfter != RequeueInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s after a transient create failure",
			res.Requeue, res.RequeueAfter, RequeueInterval)
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
		t.Errorf("Status.Phase = %q, want Failed", updated.Status.Phase)
	}
	if updated.Status.Endpoint != nil {
		t.Errorf("Status.Endpoint = %+v, want nil", updated.Status.Endpoint)
	}
	if updated.Status.Replicas != nil {
		t.Errorf("Status.Replicas = %+v, want nil", updated.Status.Replicas)
	}
}

// TestReconcileNotFoundFailsClosedAndRetries distinguishes an explicit missing-resource
// response from an ambiguous transport failure. NotFound disproves the stale serving state,
// but a prompt retry can recreate the resource once the API race or dependency resolves.
func TestReconcileNotFoundFailsClosedAndRetries(t *testing.T) {
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

	notFound := apierrors.NewNotFound(
		schema.GroupResource{Group: RayAPIGroup, Resource: "rayservices"},
		"test",
	)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					return notFound
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; NotFound should be reported in status: %v", err)
	}
	if res.Requeue || res.RequeueAfter != RequeueInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s after NotFound",
			res.Requeue, res.RequeueAfter, RequeueInterval)
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
	if got := statuses[airunwayv1alpha1.ConditionTypeReady]; got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", got)
	}
	if got := reasons[airunwayv1alpha1.ConditionTypeResourceCreated]; got != "CreateFailed" {
		t.Errorf("ResourceCreated reason = %q, want CreateFailed", got)
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

// TestReconcileValidationFailureFailsClosedAndRetriesSlowly distinguishes deterministic
// admission failures from retryable transport errors and schema-version mismatches. It fails
// closed and polls slowly so an external admission-policy fix can recover without another event.
func TestReconcileValidationFailureFailsClosedAndRetriesSlowly(t *testing.T) {
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

	rejection := apierrors.NewInvalid(
		schema.GroupKind{Group: RayAPIGroup, Kind: RayServiceKind},
		"test",
		field.ErrorList{field.Invalid(
			field.NewPath("spec", "rayClusterConfig", "workerGroupSpecs").Index(0).Child("replicas"),
			int64(0),
			"must be greater than zero",
		)},
	)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					return rejection
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

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

// TestReconcileRequeuesOnConflictWithoutClearingStatus covers the 409 guard.
//
// The KubeRay operator writes RayService status frequently, so this provider's
// Get-then-Update window can lose a race. A 409 is transient and must NOT be treated as a
// failure: before this guard existed a lost race would clear the endpoint and replica counts
// and mark a still-serving workload Failed with no requeue. The other four providers already
// had this guard; kuberay did not.
func TestReconcileRequeuesOnConflictWithoutClearingStatus(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "live-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:   airunwayv1alpha1.ConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: "DeploymentReady",
	})
	existing := ownedRayServiceNeedingUpdate(t, md)
	if err := unstructured.SetNestedSlice(existing.Object, []interface{}{
		map[string]interface{}{"type": conditionRayServiceReady, "status": "True"},
	}, "status", "conditions"); err != nil {
		t.Fatalf("mark existing RayService ready: %v", err)
	}

	var updateCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					updateCalled = true
					return apierrors.NewConflict(
						schema.GroupResource{Group: "ray.io", Resource: "rayservices"}, "test",
						fmt.Errorf("the object has been modified"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()

	r := NewKubeRayProviderReconciler(c, scheme)

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
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s after a transient conflict",
			res.Requeue, res.RequeueAfter, time.Second)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.Phase == airunwayv1alpha1.DeploymentPhaseFailed {
		t.Error("a transient conflict must not mark the deployment Failed")
	}
	if updated.Status.Endpoint == nil {
		t.Error("a transient conflict must not clear the endpoint of a still-serving workload")
	}
	if updated.Status.Replicas == nil {
		t.Error("a transient conflict must not clear replica counts")
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == airunwayv1alpha1.ConditionTypeReady && cond.Status != metav1.ConditionTrue {
			t.Errorf("Ready = %q, want True after transient update conflict", cond.Status)
		}
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
	setRayServiceGVK(foreign)
	foreign.SetName("test")
	foreign.SetNamespace("default")
	foreign.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       "someone-else",
		UID:        types.UID("a-different-uid"),
	}})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, foreign).WithStatusSubresource(md).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

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
