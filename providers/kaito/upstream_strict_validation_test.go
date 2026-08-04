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
	"strings"
	"testing"

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
				if obj.GetObjectKind().GroupVersionKind().Kind == WorkspaceKind {
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
