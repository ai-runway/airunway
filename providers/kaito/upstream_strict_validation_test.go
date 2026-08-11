/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package kaito

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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// Regression test for upstream schema compatibility.
//
// Every write to the upstream Workspace must carry fieldValidation=Strict, so the API
// server rejects fields the installed KAITO CRD does not declare instead of silently
// pruning them. Without it the shim can render a field the cluster drops without any
// error, leaving a workload that reports healthy but cannot serve.
//
// This asserts the option reaches the client. It deliberately does NOT assert the API
// server's behaviour — the fake client has no structural schema and cannot prune, which
// is exactly why no pre-existing test caught this bug.
//
// KAITO writes via Create on first reconcile and a merge Patch thereafter (rather than a
// full replace), so both paths are covered.

func kaitoTestOwner() []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       "test",
		UID:        types.UID("test-uid"),
	}}
}

func kaitoTestMD() *airunwayv1alpha1.ModelDeployment {
	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = types.UID("test-uid")
	return md
}

// kaitoDesiredWorkspace builds a Workspace with the shim-managed subtrees. Note KAITO
// puts `resource` and `inference` at the object root, not under `spec`.
func kaitoDesiredWorkspace(instanceType string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	setWorkspaceGVK(u)
	u.SetName("test")
	u.SetNamespace("default")
	u.SetOwnerReferences(kaitoTestOwner())
	u.Object["resource"] = map[string]interface{}{
		"count":        int64(1),
		"instanceType": instanceType,
	}
	u.Object["inference"] = map[string]interface{}{
		"preset": map[string]interface{}{"name": "phi-3-mini-4k-instruct"},
	}
	return u
}

func TestUpstreamCreateUsesStrictFieldValidation(t *testing.T) {
	scheme := newSchemeWithWorkspace()

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
			return nil
		},
	}).Build()

	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	if err := r.createOrUpdateResource(context.Background(), kaitoDesiredWorkspace("Standard_NC6s_v3"), kaitoTestMD()); err != nil {
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

func TestUpstreamPatchUsesStrictFieldValidation(t *testing.T) {
	scheme := newSchemeWithWorkspace()

	// Existing workspace differs from desired in a shim-managed field, so
	// createOrUpdateResource takes the merge-patch path rather than no-oping.
	existing := kaitoDesiredWorkspace("Standard_NC12s_v3")

	var got string
	var called bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
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

	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	if err := r.createOrUpdateResource(context.Background(), kaitoDesiredWorkspace("Standard_NC6s_v3"), kaitoTestMD()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("Patch was never called — test is not exercising the update path")
	}
	if got != metav1.FieldValidationStrict {
		t.Errorf("patch fieldValidation = %q, want %q (unknown fields must be rejected, not pruned)",
			got, metav1.FieldValidationStrict)
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
			name: "observed: custom resource create against an older CRD (400)",
			err:  apierrors.NewBadRequest(`strict decoding error: unknown field "spec.services.VllmWorker.frontendSidecar"`),
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
	// KAITO probes upstream health through a SEPARATE direct client before it writes. That
	// probe needs the workspace GVK resolvable by the RESTMapper and a ready KAITO
	// controller Deployment, or it refuses first — and since the refusal path ALSO returns
	// a RequeueAfter, asserting only on that would pass vacuously.
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
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					return rejection
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()

	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

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
	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

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

// TestReconcileTransientWriteFailurePreservesLastKnownStatus covers a retryable patch
// failure after the controller has observed an owned upstream resource. The ambiguous write
// does not prove that existing workload stopped serving, so last-known status is retained.
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

	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	existing := resources[0].DeepCopy()
	if err := unstructured.SetNestedField(existing.Object, int64(2), "resource", "count"); err != nil {
		t.Fatalf("mutate existing resource: %v", err)
	}
	if err := unstructured.SetNestedField(existing.Object, "Ready", "status", "state"); err != nil {
		t.Fatalf("mark existing Workspace ready: %v", err)
	}

	var patchCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					patchCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}
	if res.RequeueAfter != RequeueInterval {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, RequeueInterval)
	}
	if !patchCalled {
		t.Fatal("Patch was never called; test did not exercise the existing-resource path")
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

// TestReconcileTransientWriteFailureOnUnreadyWorkspaceFailsClosed proves that an active,
// owned Workspace must also be serving before stale ModelDeployment status can be retained.
func TestReconcileTransientWriteFailureOnUnreadyWorkspaceFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedKaitoStaleServingStatus(md)

	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	existing := resources[0].DeepCopy()
	if err := unstructured.SetNestedField(existing.Object, int64(2), "resource", "count"); err != nil {
		t.Fatalf("mutate existing resource: %v", err)
	}
	if err := unstructured.SetNestedField(existing.Object, "NotReady", "status", "state"); err != nil {
		t.Fatalf("mark existing Workspace unready: %v", err)
	}

	var patchCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					patchCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the failure should be reported in status: %v", err)
	}
	if !patchCalled {
		t.Fatal("Patch was never called; test did not exercise the active unready-Workspace path")
	}
	assertKaitoPromptRetryFailedClosed(t, c, res)
}

// TestReconcileTerminatingWorkspaceTransientPatchFailsClosed distinguishes an active owned
// Workspace from one already being deleted. Even though the object was observed and the patch
// failure is retryable, a terminating Workspace cannot support stale Ready=True serving status.
func TestReconcileTerminatingWorkspaceTransientPatchFailsClosed(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedKaitoStaleServingStatus(md)

	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	existing := resources[0].DeepCopy()
	if err := unstructured.SetNestedField(existing.Object, int64(2), "resource", "count"); err != nil {
		t.Fatalf("mutate existing resource: %v", err)
	}
	if err := unstructured.SetNestedField(existing.Object, "Ready", "status", "state"); err != nil {
		t.Fatalf("mark existing Workspace ready: %v", err)
	}
	deletionTimestamp := metav1.Now()
	existing.SetFinalizers([]string{"test.airunway.ai/hold"})
	existing.SetDeletionTimestamp(&deletionTimestamp)

	var patchCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					patchCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated transient Workspace patch failure"))
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the patch failure should be reported in status: %v", err)
	}
	if !patchCalled {
		t.Fatal("Patch was never called; test did not exercise the terminating existing-resource path")
	}
	assertKaitoPromptRetryFailedClosed(t, c, res)
}

// TestReconcileTransientWorkspaceGetFailureFailsClosed covers a retryable read failure before
// the controller has verified whether the Workspace exists, is owned, or is active. Without a
// trustworthy observation there is no basis for preserving stale serving status.
func TestReconcileTransientWorkspaceGetFailureFailsClosed(t *testing.T) {
	scheme := newSchemeWithWorkspace()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
	controllerutil.AddFinalizer(md, FinalizerName)
	seedKaitoStaleServingStatus(md)

	var getCalled, writeCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					getCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated transient Workspace get failure"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					writeCalled = true
				}
				return cl.Create(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					writeCalled = true
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; the get failure should be reported in status: %v", err)
	}
	if !getCalled {
		t.Fatal("Workspace Get was never called; test did not exercise the unverified-observation path")
	}
	if writeCalled {
		t.Fatal("Workspace write was called after its Get failed")
	}
	assertKaitoPromptRetryFailedClosed(t, c, res)
}

func seedKaitoStaleServingStatus(md *airunwayv1alpha1.ModelDeployment) {
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "stale-svc", Port: 8000}
	md.Status.Replicas = &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1}
	md.Status.Conditions = append(md.Status.Conditions, metav1.Condition{
		Type:    airunwayv1alpha1.ConditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "DeploymentReady",
		Message: "last observed workload is ready",
	})
}

func assertKaitoPromptRetryFailedClosed(t *testing.T, c client.Client, res ctrl.Result) {
	t.Helper()
	if res.Requeue || res.RequeueAfter != RequeueInterval {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s", res.Requeue, res.RequeueAfter, RequeueInterval)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	var ready, created *metav1.Condition
	for i := range updated.Status.Conditions {
		condition := &updated.Status.Conditions[i]
		switch condition.Type {
		case airunwayv1alpha1.ConditionTypeReady:
			ready = condition
		case airunwayv1alpha1.ConditionTypeResourceCreated:
			created = condition
		}
	}
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v, want False", ready)
	}
	if created == nil || created.Status != metav1.ConditionFalse || created.Reason != "CreateFailed" {
		t.Errorf("ResourceCreated = %+v, want False reason CreateFailed", created)
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

// TestReconcileOwnedPatchConflictRetriesPromptly covers a resourceVersion race after the
// controller has read and ownership-checked an existing Workspace. A 409 from KAITO's
// managed-field merge patch should trigger a fresh read promptly, while preserving the
// last-known serving status.
func TestReconcileOwnedPatchConflictRetriesPromptly(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = types.UID("test-uid")
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
	if err := unstructured.SetNestedField(existing.Object, int64(2), "resource", "count"); err != nil {
		t.Fatalf("mutate existing resource: %v", err)
	}
	if err := unstructured.SetNestedField(existing.Object, "Ready", "status", "state"); err != nil {
		t.Fatalf("mark existing Workspace ready: %v", err)
	}

	var patchCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					patchCalled = true
					return apierrors.NewConflict(
						schema.GroupResource{Group: KaitoAPIGroup, Resource: "workspaces"},
						obj.GetName(), fmt.Errorf("the object has been modified"),
					)
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned an error; a conflict should requeue: %v", err)
	}
	if !patchCalled {
		t.Fatal("Patch was never called; test did not exercise the owned existing-resource path")
	}
	if res.Requeue || res.RequeueAfter != time.Second {
		t.Errorf("requeue = %v, RequeueAfter = %s; want false and %s after a patch conflict",
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
	var created, ready *metav1.Condition
	for i := range updated.Status.Conditions {
		condition := &updated.Status.Conditions[i]
		switch condition.Type {
		case airunwayv1alpha1.ConditionTypeResourceCreated:
			created = condition
		case airunwayv1alpha1.ConditionTypeReady:
			ready = condition
		}
	}
	if created == nil || created.Reason != "ResourceConflict" {
		t.Errorf("ResourceCreated = %+v, want reason ResourceConflict", created)
	}
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
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					createCalled = true
					return apierrors.NewInternalError(fmt.Errorf("simulated upstream outage"))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

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
	var readyStatus metav1.ConditionStatus
	for _, condition := range updated.Status.Conditions {
		if condition.Type == airunwayv1alpha1.ConditionTypeReady {
			readyStatus = condition.Status
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
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					return apierrors.NewNotFound(
						schema.GroupResource{Group: KaitoAPIGroup, Resource: "workspaces"},
						obj.GetName(),
					)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

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
	var readyStatus metav1.ConditionStatus
	var createdReason string
	for _, cond := range updated.Status.Conditions {
		if cond.Type == airunwayv1alpha1.ConditionTypeReady {
			readyStatus = cond.Status
		}
		if cond.Type == airunwayv1alpha1.ConditionTypeResourceCreated {
			createdReason = cond.Reason
		}
	}
	if readyStatus != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", readyStatus)
	}
	if createdReason != "CreateFailed" {
		t.Errorf("ResourceCreated reason = %q, want CreateFailed", createdReason)
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
		schema.GroupKind{Group: KaitoAPIGroup, Kind: WorkspaceKind},
		"test",
		field.ErrorList{field.Invalid(
			field.NewPath("resource", "count"),
			int64(0),
			"must be greater than zero",
		)},
	)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
					return rejection
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

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
// key is matched case-sensitively, so "Resource" is rejected rather than merged. KAITO has no
// "spec" root key at all — its accepted keys are "resource" and "inference". encoding/json
// would not decode it into anything meaningful, and merging it would push a key no upstream
// schema declares into the rendered object.
func TestOverrideRootKeyComparisonIsCaseSensitive(t *testing.T) {
	md := newMDForController("test", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Name:      md.Status.Provider.Name,
		Overrides: &runtime.RawExtension{Raw: []byte(`{"Resource":{"x":1}}`)},
	}
	if _, err := NewTransformer().Transform(context.Background(), md); err == nil {
		t.Error("a capitalised root key was accepted; it must be rejected, not merged")
	}
}

// TestOverrideRejectsTuningDeliberately pins a policy that is narrower than the upstream
// schema, so it reads as a decision rather than an oversight.
//
// The pinned KAITO Workspace CRD (v0.10.0) declares four root properties beyond metadata:
// resource, inference, tuning and status. This transformer accepts only the first two. That
// is intentional: a ModelDeployment describes an inference deployment, the status translator
// reads inference conditions, and nothing in the CRD prevents inference and tuning both
// being set — so a tuning override would yield a Workspace whose status this provider cannot
// interpret. Admitting `tuning` here would need a status-translation story first.
func TestOverrideRejectsTuningDeliberately(t *testing.T) {
	md := newMDForController("test", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Name: md.Status.Provider.Name,
		Overrides: &runtime.RawExtension{
			Raw: []byte(`{"inference":null,"tuning":{"input":{},"output":{}}}`),
		},
	}
	_, err := NewTransformer().Transform(context.Background(), md)
	if err == nil {
		t.Fatal("tuning override was accepted; this provider renders inference workloads only")
	}
	// The message must explain the policy, not imply the field is absent from the schema.
	if !strings.Contains(err.Error(), "tuning") {
		t.Errorf("error %q does not name the offending key", err.Error())
	}
	if !strings.Contains(err.Error(), "inference workloads") {
		t.Errorf("error %q does not state the inference-only policy that motivates it", err.Error())
	}
}

// TestReconcileOwnershipConflictUsesItsOwnReason covers the ownership-collision branch.
//
// An upstream object of the right name already exists but belongs to someone else. That is
// terminal — unlike a transient API 409 — so it is reported under its own reason rather than
// the generic CreateFailed.
func TestReconcileOwnershipConflictUsesItsOwnReason(t *testing.T) {
	scheme := newSchemeWithWorkspace()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)

	// Same name, different owner.
	foreign := &unstructured.Unstructured{}
	setWorkspaceGVK(foreign)
	foreign.SetName("test")
	foreign.SetNamespace("default")
	foreign.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       "someone-else",
		UID:        types.UID("a-different-uid"),
	}})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, foreign).WithStatusSubresource(md).Build()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

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

// TestWorkspaceRootKeysExcludesSpec pins the one allowlist entry that would otherwise have no
// negative test. A KAITO Workspace has no "spec" root key — that is the whole reason this
// provider's allowlist differs from every other provider's — so admitting one would push a
// field no Workspace declares into the rendered object.
func TestWorkspaceRootKeysExcludesSpec(t *testing.T) {
	if workspaceRootKeys["spec"] {
		t.Error(`workspaceRootKeys admits "spec"; a KAITO Workspace has no spec root key`)
	}
	for _, want := range []string{"resource", "inference"} {
		if !workspaceRootKeys[want] {
			t.Errorf("workspaceRootKeys is missing %q, which this provider renders", want)
		}
	}
}
