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
	"strings"
	"testing"

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
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "live-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == RayServiceKind {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "ray.io", Resource: "rayservices"}, "test",
						fmt.Errorf("the object has been modified"))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()

	r := NewKubeRayProviderReconciler(c, scheme)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; a conflict should requeue: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Error("expected a requeue after a transient conflict")
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
