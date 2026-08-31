package kaito

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	testUID                       = "test-uid"
	testPrivateAccessMode         = "private"
	testLinuxOS                   = "linux"
	testModelDeploymentAPIVersion = "airunway.ai/v1alpha1"
	testModelDeploymentKind       = "ModelDeployment"
	testCreateFailedReason        = "CreateFailed"
	testResourceConflictReason    = "ResourceConflict"
	testPreservedAnnotationValue  = "preserve-me"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(s)
	_ = clientgoscheme.AddToScheme(s)
	return s
}

// newSchemeWithWorkspace returns a scheme that additionally has the kaito.sh/Workspace
// GVK registered so the probe's REST-mapper check passes in fake-client tests.
func newSchemeWithWorkspace() *runtime.Scheme {
	s := newScheme()
	gvk := schema.GroupVersionKind{Group: "kaito.sh", Version: "v1beta1", Kind: "Workspace"}
	s.AddKnownTypeWithName(gvk, &metav1.PartialObjectMetadata{})
	gvkList := schema.GroupVersionKind{Group: "kaito.sh", Version: "v1beta1", Kind: "WorkspaceList"}
	s.AddKnownTypeWithName(gvkList, &metav1.PartialObjectMetadataList{})
	metav1.AddToGroupVersion(s, schema.GroupVersion{Group: "kaito.sh", Version: "v1beta1"})
	return s
}

// newReadyKaitoDeployment returns an appsv1.Deployment that satisfies the upstream
// health probe (label app.kubernetes.io/name=workspace, ReadyReplicas=1).
func newReadyKaitoDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaito-workspace",
			Namespace: "kaito-workspace",
			Labels:    map[string]string{kaitoDeploymentSelectorKey: kaitoDeploymentSelectorValue},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
}

func newMDForController(name, ns string) *airunwayv1alpha1.ModelDeployment {
	return &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model:  airunwayv1alpha1.ModelSpec{ID: "test-model", Source: airunwayv1alpha1.ModelSourceHuggingFace},
			Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
		},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Provider: &airunwayv1alpha1.ProviderStatus{Name: ProviderName},
		},
	}
}

func TestValidateCompatibility(t *testing.T) {
	r := &KaitoProviderReconciler{}

	tests := []struct {
		name    string
		md      *airunwayv1alpha1.ModelDeployment
		wantErr bool
		errMsg  string
	}{
		{
			name: "vllm is compatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
				},
			},
			wantErr: false,
		},
		{
			name: "llamacpp with image is compatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeLlamaCpp},
					Image:  "my-image:latest",
				},
			},
			wantErr: false,
		},
		{
			name: "llamacpp without image is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeLlamaCpp},
				},
			},
			wantErr: true,
			errMsg:  "llamacpp engine requires spec.image to be set",
		},
		{
			name: "sglang is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeSGLang},
				},
			},
			wantErr: true,
			errMsg:  "KAITO does not support sglang engine",
		},
		{
			name: "trtllm is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeTRTLLM},
				},
			},
			wantErr: true,
			errMsg:  "KAITO does not support trtllm engine",
		},
		{
			name: "disaggregated mode is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Serving: &airunwayv1alpha1.ServingSpec{
						Mode: airunwayv1alpha1.ServingModeDisaggregated,
					},
				},
			},
			wantErr: true,
			errMsg:  "KAITO does not support disaggregated serving mode",
		},
		{
			name: "aggregated mode is compatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Serving: &airunwayv1alpha1.ServingSpec{
						Mode: airunwayv1alpha1.ServingModeAggregated,
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.validateCompatibility(tt.md)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if err.Error() != tt.errMsg {
					t.Errorf("expected error %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestSetCondition(t *testing.T) {
	r := &KaitoProviderReconciler{}
	md := &airunwayv1alpha1.ModelDeployment{}

	r.setCondition(md, "TestCondition", "True", "TestReason", "test message")

	if len(md.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(md.Status.Conditions))
	}
	cond := md.Status.Conditions[0]
	if cond.Type != "TestCondition" {
		t.Errorf("expected type TestCondition, got %s", cond.Type)
	}
	if string(cond.Status) != "True" {
		t.Errorf("expected status True, got %s", cond.Status)
	}
	if cond.Reason != "TestReason" {
		t.Errorf("expected reason TestReason, got %s", cond.Reason)
	}
	if cond.Message != "test message" {
		t.Errorf("expected message 'test message', got %s", cond.Message)
	}

	// Update the same condition
	r.setCondition(md, "TestCondition", "False", "UpdatedReason", "updated message")
	if len(md.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(md.Status.Conditions))
	}
	if string(md.Status.Conditions[0].Status) != "False" {
		t.Errorf("expected updated status False, got %s", md.Status.Conditions[0].Status)
	}
}

func TestNewKaitoProviderReconciler(t *testing.T) {
	r := NewKaitoProviderReconciler(nil, nil, nil, record.NewFakeRecorder(10))
	if r == nil {
		t.Fatal("expected non-nil reconciler")
	}
	if r.Transformer == nil {
		t.Error("expected non-nil transformer")
	}
	if r.StatusTranslator == nil {
		t.Error("expected non-nil status translator")
	}
}

func TestControllerConstants(t *testing.T) {
	if ProviderName != "kaito" {
		t.Errorf("expected provider name 'kaito', got %s", ProviderName)
	}
	if FinalizerName != "airunway.ai/kaito-provider" {
		t.Errorf("expected finalizer name 'airunway.ai/kaito-provider', got %s", FinalizerName)
	}
	if FieldManager != "kaito-provider" {
		t.Errorf("expected stable field manager 'kaito-provider', got %s", FieldManager)
	}
}

func TestReconcileNotFound(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "missing", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue for not-found")
	}
}

func TestReconcileWrongProvider(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.Status.Provider.Name = "other-provider"

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue for wrong provider")
	}
}

func TestReconcilePaused(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.Annotations = map[string]string{"airunway.ai/reconcile-paused": "true"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue when paused")
	}
}

func TestReconcileAddsFinalizer(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("should requeue after adding finalizer")
	}

	// Verify finalizer was added
	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get updated MD: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&updated, FinalizerName) {
		t.Error("expected finalizer to be added")
	}
}

func TestReconcileIncompatibleEngine(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang
	controllerutil.AddFinalizer(md, FinalizerName)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated airunwayv1alpha1.ModelDeployment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase, got %s", updated.Status.Phase)
	}
}

func TestReconcileTransformFailure(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	// Use an engine type that passes validateCompatibility but fails in Transform
	md.Spec.Engine.Type = airunwayv1alpha1.EngineType("unsupported-engine")
	controllerutil.AddFinalizer(md, FinalizerName)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	deploy := newReadyKaitoDeployment()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(deploy).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated airunwayv1alpha1.ModelDeployment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase, got %s", updated.Status.Phase)
	}
}

func TestReconcileNilProvider(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.Status.Provider = nil

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue for nil provider")
	}
}

func TestReconcileSuccessfulCreate(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = testUID
	controllerutil.AddFinalizer(md, FinalizerName)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		WithReturnManagedFields().
		Build()
	deploy := newReadyKaitoDeployment()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(deploy).Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != RequeueInterval {
		var updated airunwayv1alpha1.ModelDeployment
		_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
		t.Errorf("expected requeue after %v, got %v; status=%#v", RequeueInterval, result.RequeueAfter, updated.Status)
	}

	// Verify Workspace was created
	ws := &unstructured.Unstructured{}
	setWorkspaceGVK(ws)
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, ws)
	if err != nil {
		t.Fatalf("expected Workspace to be created: %v", err)
	}
}

func TestReconcileAlreadyRunning(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = testUID
	controllerutil.AddFinalizer(md, FinalizerName)

	// Create an upstream workspace that matches what the transformer would produce
	// so createOrUpdateResource does NOT update it (preserving status)
	ws := &unstructured.Unstructured{}
	setWorkspaceGVK(ws)
	ws.SetName("test")
	ws.SetNamespace("default")
	ws.SetOwnerReferences([]metav1.OwnerReference{
		{UID: testUID, APIVersion: testModelDeploymentAPIVersion, Kind: testModelDeploymentKind, Name: "test"},
	})
	ws.Object["resource"] = map[string]interface{}{
		"count": int64(1),
		"labelSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{
				"kubernetes.io/os": testLinuxOS,
			},
		},
	}
	ws.Object["inference"] = map[string]interface{}{
		"preset": map[string]interface{}{
			"name": "test-model",
		},
	}
	ws.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{
				"type":   "WorkspaceSucceeded",
				"status": "True",
			},
		},
	}

	deploy := newReadyKaitoDeployment()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(deploy).Build()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, ws).
		WithStatusSubresource(md).
		WithReturnManagedFields().
		Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != RequeueInterval {
		t.Errorf("expected requeue after %v, got %v", RequeueInterval, result.RequeueAfter)
	}

	var updated airunwayv1alpha1.ModelDeployment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", updated.Status.Phase)
	}
}

// TestReconcileRunningUpdatesMessage reproduces issue #289: once the Workspace
// is ready the phase flips to Running, but the status message must no longer
// claim it is "waiting for pods to be ready".
func TestReconcileRunningUpdatesMessage(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = testUID
	controllerutil.AddFinalizer(md, FinalizerName)
	// Simulate a prior reconcile loop that left the deploying-phase message.
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
	md.Status.Message = "Workspace created, waiting for pods to be ready"

	ws := &unstructured.Unstructured{}
	setWorkspaceGVK(ws)
	ws.SetName("test")
	ws.SetNamespace("default")
	ws.SetOwnerReferences([]metav1.OwnerReference{
		{UID: testUID, APIVersion: testModelDeploymentAPIVersion, Kind: testModelDeploymentKind, Name: "test"},
	})
	ws.Object["resource"] = map[string]interface{}{
		"count": int64(1),
		"labelSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{
				"kubernetes.io/os": testLinuxOS,
			},
		},
	}
	ws.Object["inference"] = map[string]interface{}{
		"preset": map[string]interface{}{
			"name": "test-model",
		},
	}
	ws.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{
				"type":   "WorkspaceSucceeded",
				"status": "True",
			},
		},
	}

	deploy := newReadyKaitoDeployment()
	directC := probeClientBuilderWithWorkspace(t).WithObjects(deploy).Build()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, ws).
		WithStatusSubresource(md).
		WithReturnManagedFields().
		Build()
	r := NewKaitoProviderReconciler(c, scheme, directC, record.NewFakeRecorder(10))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated airunwayv1alpha1.ModelDeployment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Fatalf("expected Running phase, got %s", updated.Status.Phase)
	}
	if strings.Contains(updated.Status.Message, "waiting for pods") {
		t.Errorf("status message still claims waiting for pods while Running: %q", updated.Status.Message)
	}
	if updated.Status.Message == "" {
		t.Errorf("expected a non-empty status message in Running phase")
	}
}

func TestReconcileHandleDeletion(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No upstream resource exists, so finalizer should be removed
	var updated airunwayv1alpha1.ModelDeployment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	if controllerutil.ContainsFinalizer(&updated, FinalizerName) {
		t.Error("expected finalizer to be removed after deletion with no upstream resource")
	}
	_ = result
}

// TestReconcileDeletionWithMissingUpstreamCRDRemovesFinalizer reproduces
// https://github.com/ai-runway/airunway/issues/239 — when the KAITO
// upstream CRDs are not installed, fetching the Workspace returns
// meta.NoKindMatchError (not IsNotFound). The reconciler must still complete
// finalizer removal so the ModelDeployment can be deleted.
func TestReconcileDeletionWithMissingUpstreamCRDRemovesFinalizer(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	interceptorFuncs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == WorkspaceKind {
				return &apimeta.NoKindMatchError{
					GroupKind:        schema.GroupKind{Group: KaitoAPIGroup, Kind: WorkspaceKind},
					SearchedVersions: []string{KaitoAPIVersion},
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		WithInterceptorFuncs(interceptorFuncs).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("expected deletion to finish without requeue, got %#v", result)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); !apierrors.IsNotFound(err) {
		t.Fatalf("expected ModelDeployment to be deleted after finalizer removal, got %v", err)
	}
}

func TestReconcileDeletionWithUpstreamUnavailableDeleteRemovesFinalizer(t *testing.T) {
	tests := []struct {
		name      string
		deleteErr error
	}{
		{
			name: "workspace deleted between get and delete",
			deleteErr: apierrors.NewNotFound(
				schema.GroupResource{Group: KaitoAPIGroup, Resource: "workspaces"},
				"test",
			),
		},
		{
			name: "workspace CRD removed between get and delete",
			deleteErr: &apimeta.NoKindMatchError{
				GroupKind:        schema.GroupKind{Group: KaitoAPIGroup, Kind: WorkspaceKind},
				SearchedVersions: []string{KaitoAPIVersion},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newScheme()
			md := newMDForController("test", "default")
			md.UID = testUID
			controllerutil.AddFinalizer(md, FinalizerName)
			now := metav1.Now()
			md.DeletionTimestamp = &now

			ws := &unstructured.Unstructured{}
			setWorkspaceGVK(ws)
			ws.SetName("test")
			ws.SetNamespace("default")
			ws.SetOwnerReferences([]metav1.OwnerReference{
				{UID: testUID, APIVersion: testModelDeploymentAPIVersion, Kind: testModelDeploymentKind, Name: "test"},
			})

			interceptorFuncs := interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == WorkspaceKind {
						return tt.deleteErr
					}
					return c.Delete(ctx, obj, opts...)
				},
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(md, ws).
				WithStatusSubresource(md).
				WithInterceptorFuncs(interceptorFuncs).
				Build()
			r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Requeue || result.RequeueAfter != 0 {
				t.Fatalf("expected deletion to finish without requeue, got %#v", result)
			}

			var updated airunwayv1alpha1.ModelDeployment
			if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); !apierrors.IsNotFound(err) {
				t.Fatalf("expected ModelDeployment to be deleted after finalizer removal, got %v", err)
			}
		})
	}
}

// TestReconcileDeletionTransientGetErrorBeforeTimeout confirms that an
// unexpected (non-NoMatch / non-NotFound) error fetching the upstream
// resource requeues instead of returning a hard error, so subsequent
// reconciles can still observe the timeout fallback.
func TestReconcileDeletionTransientGetErrorBeforeTimeout(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	interceptorFuncs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == WorkspaceKind {
				return errors.New("transient API server failure")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		WithInterceptorFuncs(interceptorFuncs).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatalf("expected requeue while waiting for timeout, got %#v", result)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("expected ModelDeployment to still exist before timeout, got %v", err)
	}
	if !controllerutil.ContainsFinalizer(&updated, FinalizerName) {
		t.Error("expected finalizer to still be present before timeout")
	}
}

// TestReconcileDeletionTransientGetErrorAfterTimeout confirms the documented
// 5-minute force-remove fallback eventually fires when the upstream Get
// continues to fail with an unexpected error.
func TestReconcileDeletionTransientGetErrorAfterTimeout(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	old := metav1.NewTime(time.Now().Add(-(FinalizerTimeout + time.Minute)))
	md.DeletionTimestamp = &old

	interceptorFuncs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == WorkspaceKind {
				return errors.New("persistent API server failure")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		WithInterceptorFuncs(interceptorFuncs).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); !apierrors.IsNotFound(err) {
		t.Fatalf("expected ModelDeployment to be deleted after finalizer timeout, got %v", err)
	}
}

func TestReconcileDeletionNoFinalizer(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	// The handleDeletion path checks for finalizer and returns early if not present.
	// We test this by creating a MD with deletionTimestamp AND a dummy finalizer
	// (so fake client accepts it), but NOT our finalizer.
	now := metav1.Now()
	md.DeletionTimestamp = &now
	md.Finalizers = []string{"other-finalizer"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("should not requeue when our finalizer is not present on deletion")
	}
}

func TestReconcileDeletionWithUpstreamResource(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = testUID
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	// Create upstream workspace
	ws := &unstructured.Unstructured{}
	setWorkspaceGVK(ws)
	ws.SetName("test")
	ws.SetNamespace("default")
	ws.SetOwnerReferences([]metav1.OwnerReference{
		{UID: testUID, APIVersion: testModelDeploymentAPIVersion, Kind: testModelDeploymentKind, Name: "test"},
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, ws).WithStatusSubresource(md).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should requeue waiting for deletion
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s, got %v", result.RequeueAfter)
	}
}

func TestCreateOrUpdateResourceCreatesAtomicallyWithStableFieldManager(t *testing.T) {
	scheme := newScheme()
	controllerApplyCalls := 0
	preservedApplyCalls := 0
	createCalls := 0
	var gotOptions client.ApplyOptions
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				createCalls++
				annotations := obj.GetAnnotations()
				if _, hasCurrent := annotations[lastAppliedWorkspaceAnnotation]; hasCurrent {
					t.Fatalf("expected Create not to duplicate the current and previous applied configurations, got %v", annotations)
				}
				if _, hasPrevious := annotations[migrationPreviousFieldsAnnotation]; !hasPrevious {
					t.Fatalf("expected Create to retain the pending applied configuration, got %v", annotations)
				}
				return c.Create(ctx, obj, opts...)
			},
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				options := (&client.ApplyOptions{}).ApplyOptions(opts)
				content := obj.(interface{ UnstructuredContent() map[string]any }).UnstructuredContent()
				annotations, _, err := unstructured.NestedStringMap(content, "metadata", "annotations")
				if err != nil {
					return err
				}
				_, hasCurrent := annotations[lastAppliedWorkspaceAnnotation]
				_, hasPrevious := annotations[migrationPreviousFieldsAnnotation]
				if hasCurrent && hasPrevious {
					t.Fatalf("expected Apply not to duplicate the current and previous applied configurations, got %v", annotations)
				}
				switch options.FieldManager {
				case FieldManager:
					controllerApplyCalls++
					gotOptions = *options
				case preservedFieldsManager:
					preservedApplyCalls++
				}
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(testPrivateAccessMode),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("createOrUpdateResource: %v", err)
	}
	if createCalls != 1 || controllerApplyCalls != 2 || preservedApplyCalls != 3 {
		t.Fatalf(
			"expected one Create, migration/final rendered Applies, and preservation seed/release/finish Applies; "+
				"got create=%d rendered=%d preserved=%d",
			createCalls,
			controllerApplyCalls,
			preservedApplyCalls,
		)
	}
	if gotOptions.FieldManager != FieldManager {
		t.Fatalf("expected field manager %q, got %q", FieldManager, gotOptions.FieldManager)
	}
	if gotOptions.Force != nil && *gotOptions.Force {
		t.Fatal("expected initial Apply not to force ownership")
	}

	created := getWorkspaceForTest(t, c)
	if err := verifyOwnerReference(created, newSSADeploymentForTest().UID); err != nil {
		t.Fatalf("expected created Workspace ownership: %v", err)
	}
	if !hasApplyManagedFields(created) {
		t.Fatalf("expected %q managedFields entry, got %v", FieldManager, created.GetManagedFields())
	}
	migrationManagers, err := updateManagersOwningLastApplied(created)
	if err != nil {
		t.Fatalf("inspect migrated managedFields: %v", err)
	}
	if len(migrationManagers) != 0 {
		t.Fatalf("expected Create Update ownership to be removed, got %v", migrationManagers)
	}
}

func TestCreateOrUpdateResourceStripsUntrustedMigrationAnnotations(t *testing.T) {
	tests := []struct {
		name              string
		migrationManagers string
		previousFields    string
	}{
		{
			name:              "malformed internal state",
			migrationManagers: "foo",
			previousFields:    "{",
		},
		{
			name:              "valid-looking internal state",
			migrationManagers: `["attacker"]`,
			previousFields:    `{"annotations":{"attacker":"true"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newScheme()
			applyCalls := 0
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithReturnManagedFields().
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: interceptApplyPatch(func(
						ctx context.Context,
						c client.WithWatch,
						obj runtime.ApplyConfiguration,
						opts ...client.ApplyOption,
					) error {
						applyCalls++
						return c.Apply(ctx, obj, opts...)
					}),
				}).
				Build()
			r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

			newInjectedDesired := func() *unstructured.Unstructured {
				desired := newSSAWorkspaceForTest(testPrivateAccessMode)
				desired.SetAnnotations(map[string]string{
					"example.com/keep":                "true",
					migrationManagersAnnotation:       tt.migrationManagers,
					migrationPreviousFieldsAnnotation: tt.previousFields,
				})
				return desired
			}

			if err := r.createOrUpdateResource(
				context.Background(),
				newInjectedDesired(),
				newSSADeploymentForTest(),
			); err != nil {
				t.Fatalf("initial createOrUpdateResource: %v", err)
			}
			workspace := getWorkspaceForTest(t, c)
			if workspace.GetAnnotations()["example.com/keep"] != "true" {
				t.Fatalf("expected ordinary annotation to be applied, got %v", workspace.GetAnnotations())
			}
			for _, key := range []string{migrationManagersAnnotation, migrationPreviousFieldsAnnotation} {
				if _, found := workspace.GetAnnotations()[key]; found {
					t.Fatalf("expected injected migration annotation %q to be absent, got %v", key, workspace.GetAnnotations())
				}
			}

			applyCallsAfterCreate := applyCalls
			if err := r.createOrUpdateResource(
				context.Background(),
				newInjectedDesired(),
				newSSADeploymentForTest(),
			); err != nil {
				t.Fatalf("stable createOrUpdateResource: %v", err)
			}
			if applyCalls != applyCallsAfterCreate {
				t.Fatalf(
					"expected stable reconcile not to apply injected state, calls before=%d after=%d",
					applyCallsAfterCreate,
					applyCalls,
				)
			}
		})
	}
}

func TestCreateOrUpdateResourceRecoversInterruptedCreateMigration(t *testing.T) {
	scheme := newScheme()
	wantErr := errors.New("managedFields migration interrupted")
	failMigration := true
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if patch.Type() == types.JSONPatchType && failMigration {
					failMigration = false
					return wantErr
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	desired := newSSAWorkspaceForTest("")

	err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest())
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected interrupted Create ownership migration, got %v", err)
	}
	if _, found := getWorkspaceForTest(t, c).GetAnnotations()[migrationManagersAnnotation]; !found {
		t.Fatal("expected Create migration marker to survive interruption")
	}
	if err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
		t.Fatalf("retry Create ownership migration: %v", err)
	}
	updated := getWorkspaceForTest(t, c)
	if _, found := updated.GetAnnotations()[migrationManagersAnnotation]; found {
		t.Fatalf("expected recovered Create migration marker to be removed, got %v", updated.GetAnnotations())
	}
	if !hasApplyManagedFields(updated) {
		t.Fatalf("expected recovered Create to retain stable Apply ownership, got %v", updated.GetManagedFields())
	}
}

func TestCreateOrUpdateResourceCreateRaceDoesNotAdoptForeignWorkspace(t *testing.T) {
	scheme := newScheme()
	applyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				foreign := obj.(*unstructured.Unstructured).DeepCopy()
				foreign.SetOwnerReferences([]metav1.OwnerReference{{UID: "other-uid"}})
				annotations := foreign.GetAnnotations()
				delete(annotations, migrationManagersAnnotation)
				foreign.SetAnnotations(annotations)
				if err := c.Create(ctx, foreign, opts...); err != nil {
					t.Fatalf("create competing Workspace: %v", err)
				}
				return c.Create(ctx, obj, opts...)
			},
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				applyCalls++
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	err := r.createOrUpdateResource(context.Background(), newSSAWorkspaceForTest(""), newSSADeploymentForTest())
	if !isResourceConflict(err) {
		t.Fatalf("expected surfaced creation conflict, got %v", err)
	}
	if applyCalls != 0 {
		t.Fatalf("expected create collision not to apply, got %d calls", applyCalls)
	}
	foreign := getWorkspaceForTest(t, c)
	if err := verifyOwnerReference(foreign, newSSADeploymentForTest().UID); !isResourceConflict(err) {
		t.Fatalf("expected foreign owner reference to remain unchanged, got %v", foreign.GetOwnerReferences())
	}
}

func TestCreateOrUpdateResourceCreateRaceContinuesForSameOwner(t *testing.T) {
	scheme := newScheme()
	applyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				winner := obj.(*unstructured.Unstructured).DeepCopy()
				if err := c.Create(ctx, winner, opts...); err != nil {
					t.Fatalf("create same-owner competing Workspace: %v", err)
				}
				return c.Create(ctx, obj, opts...)
			},
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				applyCalls++
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("expected same-owner create race to reconcile successfully, got %v", err)
	}
	if applyCalls == 0 {
		t.Fatal("expected same-owner create race winner to be adopted with Apply")
	}
	workspace := getWorkspaceForTest(t, c)
	if err := verifyOwnerReference(workspace, newSSADeploymentForTest().UID); err != nil {
		t.Fatalf("expected same-owner race winner to retain ownership: %v", err)
	}
	if !hasApplyManagedFields(workspace) {
		t.Fatalf("expected same-owner race winner to have stable Apply ownership, got %v", workspace.GetManagedFields())
	}
}

func TestCreateOrUpdateResourceApplyRejectsWorkspaceReplacement(t *testing.T) {
	scheme := newScheme()
	replaceOnApply := false
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				options := (&client.ApplyOptions{}).ApplyOptions(opts)
				if replaceOnApply && options.FieldManager == FieldManager {
					replaceOnApply = false
					current := getWorkspaceForTest(t, c)
					if err := c.Delete(ctx, current); err != nil {
						t.Fatalf("delete verified Workspace before Apply: %v", err)
					}
					replacement := newSSAWorkspaceForTest("")
					replacement.SetOwnerReferences([]metav1.OwnerReference{{UID: "replacement-owner-uid"}})
					replacement.SetResourceVersion("")
					if err := c.Create(ctx, replacement, client.FieldOwner("replacement-manager")); err != nil {
						t.Fatalf("create replacement Workspace: %v", err)
					}
				}
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}

	replaceOnApply = true
	err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(testPrivateAccessMode),
		newSSADeploymentForTest(),
	)
	if !apierrors.IsConflict(err) || isResourceConflict(err) {
		t.Fatalf("expected optimistic conflict for replaced Workspace, got %v", err)
	}
	replacement := getWorkspaceForTest(t, c)
	if replacement.GetOwnerReferences()[0].UID != "replacement-owner-uid" {
		t.Fatalf("expected replacement owner to remain unchanged, got %v", replacement.GetOwnerReferences())
	}
	if _, found, err := unstructured.NestedString(
		replacement.Object,
		"inference",
		"preset",
		"accessMode",
	); err != nil || found {
		t.Fatalf("expected stale Apply not to mutate replacement, found=%v err=%v", found, err)
	}
}

func TestCreateOrUpdateResourceStableDesiredIsNoOp(t *testing.T) {
	scheme := newScheme()
	applyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				options := (&client.ApplyOptions{}).ApplyOptions(opts)
				if options.FieldManager == FieldManager {
					applyCalls++
				}
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("adopt Workspace with SSA: %v", err)
	}
	before := getWorkspaceForTest(t, c)
	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("stable createOrUpdateResource: %v", err)
	}
	after := getWorkspaceForTest(t, c)

	if applyCalls != 2 {
		t.Fatalf("expected unchanged reconcile not to call Apply, got %d controller applies", applyCalls)
	}
	if after.GetResourceVersion() != before.GetResourceVersion() {
		t.Fatalf("expected unchanged resourceVersion %q, got %q", before.GetResourceVersion(), after.GetResourceVersion())
	}
}

func TestCreateOrUpdateResourceDefaultedFieldsDoNotChurn(t *testing.T) {
	scheme := newScheme()
	controllerApplyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				options := (&client.ApplyOptions{}).ApplyOptions(opts)
				if options.FieldManager == FieldManager {
					controllerApplyCalls++
				}
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("adopt Workspace with SSA: %v", err)
	}
	defaulted := workspaceApplyForTest()
	defaulted.Object["inference"] = map[string]any{
		"preset": map[string]interface{}{
			"accessMode": "public",
			"presetOptions": map[string]any{
				"modelAccessSecret": "operator-default",
			},
		},
	}
	applyAsManagerForTest(t, c, defaulted, "kaito-defaults", false)
	before := getWorkspaceForTest(t, c)

	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("stable reconcile: %v", err)
	}
	after := getWorkspaceForTest(t, c)
	if controllerApplyCalls != 2 {
		t.Fatalf("expected defaulted fields not to trigger another controller apply, got %d", controllerApplyCalls)
	}
	if after.GetResourceVersion() != before.GetResourceVersion() {
		t.Fatalf(
			"expected no write after defaults, resourceVersion changed from %q to %q",
			before.GetResourceVersion(),
			after.GetResourceVersion(),
		)
	}
	assertKaitoDefaultsForTest(t, after)
	accessMode, found, err := unstructured.NestedString(after.Object, "inference", "preset", "accessMode")
	if err != nil || !found || accessMode != "public" {
		t.Fatalf("expected defaulted accessMode to survive, got %q found=%v err=%v", accessMode, found, err)
	}
}

func TestDesiredSubsetMatchesTreatsLabelSelectorAtomically(t *testing.T) {
	desired := map[string]any{
		"matchLabels": map[string]any{"kubernetes.io/os": testLinuxOS},
	}
	live := map[string]any{
		"matchLabels": map[string]any{
			"kubernetes.io/os":          testLinuxOS,
			"topology.example.com/pool": "blue",
		},
	}
	if desiredSubsetMatches(desired, live, "resource", "labelSelector") {
		t.Fatal("expected extra selector key to be drift because KAITO declares LabelSelector atomic")
	}
	if !desiredSubsetMatches(desired, runtime.DeepCopyJSON(desired), "resource", "labelSelector") {
		t.Fatal("expected identical atomic selector to match")
	}
}

func TestCreateOrUpdateResourceExtraOwnerReferenceDoesNotChurn(t *testing.T) {
	scheme := newScheme()
	initialClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		Build()
	initialReconciler := NewKaitoProviderReconciler(initialClient, scheme, initialClient, record.NewFakeRecorder(10))
	desired := newSSAWorkspaceForTest("")
	if err := initialReconciler.createOrUpdateResource(
		context.Background(),
		desired.DeepCopy(),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}

	// The field-managed fake client models unstructured ownerReferences as an
	// atomic list, while Kubernetes metadata defines it as associative by UID.
	// Seed the valid live shape with the controller's actual managedFields to
	// exercise the no-op comparator without imposing the fake CRD's list model.
	live := getWorkspaceForTest(t, initialClient)
	ownerReferences := append(live.GetOwnerReferences(), metav1.OwnerReference{
		APIVersion: "example.com/v1",
		Kind:       "ExternalOwner",
		Name:       "external",
		UID:        "external-owner-uid",
	})
	live.SetOwnerReferences(ownerReferences)
	managedFields := live.GetManagedFields()
	foundStableManager := false
	for index := range managedFields {
		if managedFields[index].Manager != FieldManager ||
			managedFields[index].Operation != metav1.ManagedFieldsOperationApply {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(managedFields[index].FieldsV1.Raw, &fields); err != nil {
			t.Fatalf("decode stable managedFields: %v", err)
		}
		if err := unstructured.SetNestedMap(fields, map[string]any{
			fmt.Sprintf(`k:{"uid":%q}`, testUID): map[string]any{},
		}, "f:metadata", "f:ownerReferences"); err != nil {
			t.Fatalf("set atomic owner-reference managedFields: %v", err)
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("encode stable managedFields: %v", err)
		}
		managedFields[index].FieldsV1.Raw = raw
		foundStableManager = true
	}
	if !foundStableManager {
		t.Fatal("expected stable Apply managedFields entry")
	}
	live.SetManagedFields(managedFields)
	controllerApplyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(live).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				controllerApplyCalls++
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	before := getWorkspaceForTest(t, c)

	if err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
		t.Fatalf("stable reconcile: %v", err)
	}
	after := getWorkspaceForTest(t, c)
	if controllerApplyCalls != 0 {
		t.Fatalf("expected no controller Apply for extra owner reference, got %d", controllerApplyCalls)
	}
	if after.GetResourceVersion() != before.GetResourceVersion() {
		t.Fatalf("expected stable resourceVersion %q, got %q", before.GetResourceVersion(), after.GetResourceVersion())
	}
	if len(after.GetOwnerReferences()) != 2 {
		t.Fatalf("expected external owner reference to survive, got %v", after.GetOwnerReferences())
	}
}

func TestCreateOrUpdateResourceDoesNotMigrateUnrelatedUpdateManagerAfterSSA(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithReturnManagedFields().Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	desired := newSSAWorkspaceForTest("")
	if err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}

	external := getWorkspaceForTest(t, c)
	annotations := external.GetAnnotations()
	annotations["external.example.com/value"] = testPreservedAnnotationValue
	external.SetAnnotations(annotations)
	if err := c.Update(context.Background(), external, client.FieldOwner("external-update-manager")); err != nil {
		t.Fatalf("update Workspace as external manager: %v", err)
	}
	fields, err := managedFieldsForManager(
		getWorkspaceForTest(t, c),
		"external-update-manager",
		metav1.ManagedFieldsOperationUpdate,
	)
	if err != nil || fields == nil {
		t.Fatalf("expected external Update manager before reconcile, fields=%v err=%v", fields, err)
	}

	if err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
		t.Fatalf("stable reconcile: %v", err)
	}
	after := getWorkspaceForTest(t, c)
	fields, err = managedFieldsForManager(after, "external-update-manager", metav1.ManagedFieldsOperationUpdate)
	if err != nil || fields == nil {
		t.Fatalf("expected unrelated Update manager ownership to survive, fields=%v err=%v", fields, err)
	}
	if got := after.GetAnnotations()["external.example.com/value"]; got != testPreservedAnnotationValue {
		t.Fatalf("expected unrelated field to survive, got %q", got)
	}
}

func TestCreateOrUpdateResourceDoesNotMigrateUnrelatedPreAnnotationIdentityManager(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest(testPrivateAccessMode)
	existing.SetLabels(map[string]string{
		"airunway.ai/managed-by":       "airunway",
		"airunway.ai/model-deployment": "test",
	})
	existing.SetAnnotations(map[string]string{"external.example.com/value": "external-value"})
	existing.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:    "legacy-kaito-provider",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:ownerReferences":{"k:{\"uid\":\"test-uid\"}":{}}},` +
					`"f:inference":{"f:preset":{"f:accessMode":{}}}}`,
			)},
		},
		{
			Manager:    "external-label-manager",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:annotations":{"f:external.example.com/value":{}},` +
					`"f:labels":{"f:airunway.ai/managed-by":{},` +
					`"f:airunway.ai/model-deployment":{}}}}`,
			)},
		},
	})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).WithReturnManagedFields().Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(map[string]string{
		"airunway.ai/managed-by":       "airunway",
		"airunway.ai/model-deployment": "test",
	})

	if err := r.createOrUpdateResource(context.Background(), desired, newSSADeploymentForTest()); err != nil {
		t.Fatalf("adopt Workspace with unrelated identity-label manager: %v", err)
	}
	after := getWorkspaceForTest(t, c)
	fields, err := managedFieldsForManager(
		after,
		"external-label-manager",
		metav1.ManagedFieldsOperationUpdate,
	)
	if err != nil || fields == nil {
		t.Fatalf("expected unrelated identity-label manager to survive, fields=%v err=%v", fields, err)
	}
	if got := after.GetAnnotations()["external.example.com/value"]; got != "external-value" {
		t.Fatalf("expected unrelated annotation to survive, got %q", got)
	}
}

func TestLegacyUpdateManagersPreferLastAppliedOwner(t *testing.T) {
	live := newSSAWorkspaceForTest("")
	live.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:    "legacy-kaito-provider",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:annotations":{"f:airunway.ai/kaito-last-applied":{}}}}`,
			)},
		},
		{
			Manager:    "external-owner-manager",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:ownerReferences":{}},"f:resource":{"f:count":{}}}`,
			)},
		},
	})
	identityManagers, err := updateManagersOwningAnyField(live, [][]string{{"f:metadata", "f:ownerReferences"}})
	if err != nil {
		t.Fatalf("inspect owner-reference managers: %v", err)
	}
	if _, found := identityManagers["external-owner-manager"]; !found {
		t.Fatalf("expected external identity-field manager in test setup, got %v", identityManagers)
	}
	managers, err := legacyUpdateManagers(live, newSSADeploymentForTest().UID)
	if err != nil {
		t.Fatalf("discover legacy manager: %v", err)
	}
	if _, found := managers["legacy-kaito-provider"]; !found {
		t.Fatalf("expected exact last-applied owner, got %v", managers)
	}
	if _, found := managers["external-owner-manager"]; found {
		t.Fatalf("expected identity-field fallback not to absorb unrelated manager, got %v", managers)
	}
}

func TestLegacyUpdateManagersRejectSoleIdentityLabelOwnerWithoutVerifiedOwnerReference(t *testing.T) {
	live := newSSAWorkspaceForTest("")
	live.SetLabels(map[string]string{
		"airunway.ai/managed-by":       "airunway",
		"airunway.ai/model-deployment": "test",
	})
	live.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:    "legacy-kaito-provider",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:ownerReferences":{"k:{\"uid\":\"test-uid\"}":{}}},"f:resource":{"f:count":{}}}`,
			)},
		},
		{
			Manager:    "external-label-manager",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:labels":{"f:airunway.ai/managed-by":{},` +
					`"f:airunway.ai/model-deployment":{}}},"f:resource":{"f:count":{}}}`,
			)},
		},
	})

	managers, err := legacyUpdateManagers(live, newSSADeploymentForTest().UID)
	if err != nil {
		t.Fatalf("discover legacy manager: %v", err)
	}
	if len(managers) != 0 {
		t.Fatalf("expected sole label owner without verified owner reference to be rejected, got %v", managers)
	}
}

func TestLegacyUpdateManagersRequireEveryPresentIdentityLabel(t *testing.T) {
	live := newSSAWorkspaceForTest("")
	live.SetLabels(map[string]string{
		"airunway.ai/managed-by":       "airunway",
		"airunway.ai/model-deployment": "test",
	})
	live.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:    "legacy-kaito-provider",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:labels":{"f:airunway.ai/managed-by":{}},"f:ownerReferences":{"k:{\"uid\":\"test-uid\"}":{}}}}`,
			)},
		},
		{
			Manager:    "external-label-manager",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:labels":{"f:airunway.ai/model-deployment":{}}}}`,
			)},
		},
	})

	managers, err := legacyUpdateManagers(live, newSSADeploymentForTest().UID)
	if err != nil {
		t.Fatalf("discover legacy manager: %v", err)
	}
	if len(managers) != 0 {
		t.Fatalf("expected split identity-label ownership to be rejected, got %v", managers)
	}
}

func TestLegacyUpdateManagersMatchAllIdentityLabelsAndVerifiedOwnerReference(t *testing.T) {
	live := newSSAWorkspaceForTest("")
	live.SetLabels(map[string]string{
		"airunway.ai/managed-by":       "airunway",
		"airunway.ai/model-deployment": "test",
	})
	live.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:    "legacy-kaito-provider",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:labels":{"f:airunway.ai/managed-by":{},` +
					`"f:airunway.ai/model-deployment":{}},` +
					`"f:ownerReferences":{"k:{\"uid\":\"test-uid\"}":{}}}}`,
			)},
		},
		{
			Manager:    "external-manager",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1:   &metav1.FieldsV1{Raw: []byte(`{"f:resource":{"f:count":{}}}`)},
		},
	})

	managers, err := legacyUpdateManagers(live, newSSADeploymentForTest().UID)
	if err != nil {
		t.Fatalf("discover legacy manager: %v", err)
	}
	if len(managers) != 1 {
		t.Fatalf("expected exactly one fully verified identity manager, got %v", managers)
	}
	if _, found := managers["legacy-kaito-provider"]; !found {
		t.Fatalf("expected fully verified legacy manager, got %v", managers)
	}
}

func TestLegacyUpdateManagersMatchVerifiedOwnerReferenceWithoutIdentityLabels(t *testing.T) {
	live := newSSAWorkspaceForTest("")
	live.SetLabels(nil)
	live.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:    "legacy-kaito-provider",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:ownerReferences":{"k:{\"uid\":\"test-uid\"}":{}}}}`,
			)},
		},
		{
			Manager:    "external-owner-manager",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(
				`{"f:metadata":{"f:ownerReferences":{"k:{\"uid\":\"external-owner-uid\"}":{}}},"f:resource":{"f:count":{}}}`,
			)},
		},
	})
	managers, err := legacyUpdateManagers(live, newSSADeploymentForTest().UID)
	if err != nil {
		t.Fatalf("discover owner-reference legacy manager: %v", err)
	}
	if len(managers) != 1 {
		t.Fatalf("expected exactly one verified owner-reference manager, got %v", managers)
	}
	if _, found := managers["legacy-kaito-provider"]; !found {
		t.Fatalf("expected verified owner-reference manager, got %v", managers)
	}
}

func TestCreateOrUpdateResourceRemovedOwnedFieldIsCleared(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithReturnManagedFields().Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(testPrivateAccessMode),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(testPrivateAccessMode),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("adopt Workspace with SSA: %v", err)
	}
	defaulted := workspaceApplyForTest()
	defaulted.Object["inference"] = map[string]any{
		"preset": map[string]any{
			"presetOptions": map[string]any{"modelAccessSecret": "operator-default"},
		},
	}
	applyAsManagerForTest(t, c, defaulted, "kaito-defaults", false)

	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("remove accessMode: %v", err)
	}
	updated := getWorkspaceForTest(t, c)
	if _, found, err := unstructured.NestedString(
		updated.Object,
		"inference",
		"preset",
		"accessMode",
	); err != nil || found {
		t.Fatalf(
			"expected owned accessMode to be removed, found=%v err=%v object=%v",
			found,
			err,
			updated.Object["inference"],
		)
	}
	presetOptions, found, err := unstructured.NestedMap(updated.Object, "inference", "preset", "presetOptions")
	if err != nil || !found || presetOptions["modelAccessSecret"] != "operator-default" {
		t.Fatalf("expected separately owned default to survive, got %v found=%v err=%v", presetOptions, found, err)
	}
}

func TestCreateOrUpdateResourceAdoptsLegacyWorkspaceAndClearsStaleFields(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest(testPrivateAccessMode)
	existing.SetLabels(map[string]string{
		"airunway.example.com/stale":     "true",
		"operator.example.com/defaulted": "true",
	})
	existing.SetAnnotations(map[string]string{
		"airunway.example.com/stale":     "true",
		"operator.example.com/defaulted": "true",
	})
	existing.Object["resource"] = map[string]any{
		"count": int64(1),
		"labelSelector": map[string]any{
			"matchLabels": map[string]any{
				"kubernetes.io/os":          testLinuxOS,
				"topology.example.com/pool": "stale",
			},
		},
	}
	existing.Object["inference"] = map[string]any{
		"preset": map[string]any{
			"name":       "test-model",
			"accessMode": testPrivateAccessMode,
			"presetOptions": map[string]any{
				"modelAccessSecret": "operator-default",
			},
		},
	}
	setLastAppliedForTestWithMetadata(
		t,
		existing,
		map[string]any{
			"count": int64(1),
			"labelSelector": map[string]any{
				"matchLabels": map[string]any{"kubernetes.io/os": testLinuxOS},
			},
		},
		map[string]any{"preset": map[string]any{"name": "test-model", "accessMode": testPrivateAccessMode}},
		map[string]string{"airunway.example.com/stale": "true"},
		map[string]string{"airunway.example.com/stale": "true"},
	)

	controllerApplyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				options := (&client.ApplyOptions{}).ApplyOptions(opts)
				if options.FieldManager == FieldManager {
					controllerApplyCalls++
					if options.Force != nil && *options.Force {
						t.Fatal("expected legacy adoption not to force ownership")
					}
				}
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	if err := c.Create(context.Background(), existing, client.FieldOwner("legacy-kaito-provider")); err != nil {
		t.Fatalf("create legacy Workspace: %v", err)
	}
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	desired := newSSAWorkspaceForTest("")
	desired.Object["resource"] = map[string]any{
		"count": int64(1),
		"labelSelector": map[string]any{
			"matchLabels": map[string]any{"kubernetes.io/os": testLinuxOS},
		},
	}
	desired.SetLabels(map[string]string{"airunway.example.com/keep": "true"})
	desired.SetAnnotations(map[string]string{"airunway.example.com/keep": "true"})
	if err := r.createOrUpdateResource(context.Background(), desired, newSSADeploymentForTest()); err != nil {
		t.Fatalf("adopt legacy Workspace: %v", err)
	}
	if controllerApplyCalls != 2 {
		t.Fatalf("expected migration and final desired applies after legacy cleanup, got %d", controllerApplyCalls)
	}

	adopted := getWorkspaceForTest(t, c)
	if !hasApplyManagedFields(adopted) {
		t.Fatalf("expected stable apply manager after adoption, got %v", adopted.GetManagedFields())
	}
	migrationManagers, err := updateManagersOwningLastApplied(adopted)
	if err != nil {
		t.Fatalf("inspect adopted managedFields: %v", err)
	}
	if len(migrationManagers) != 0 {
		t.Fatalf("expected legacy Update ownership to be removed, got %v", migrationManagers)
	}
	if _, found, _ := unstructured.NestedString(adopted.Object, "inference", "preset", "accessMode"); found {
		t.Fatalf("expected legacy owned accessMode to be removed, got %v", adopted.Object["inference"])
	}
	if adopted.GetLabels()["airunway.example.com/stale"] != "" ||
		adopted.GetAnnotations()["airunway.example.com/stale"] != "" {
		t.Fatalf(
			"expected legacy owned metadata to be removed, labels=%v annotations=%v",
			adopted.GetLabels(),
			adopted.GetAnnotations(),
		)
	}
	if adopted.GetLabels()["operator.example.com/defaulted"] != "true" ||
		adopted.GetAnnotations()["operator.example.com/defaulted"] != "true" {
		t.Fatalf(
			"expected non-owned metadata to survive adoption, labels=%v annotations=%v",
			adopted.GetLabels(),
			adopted.GetAnnotations(),
		)
	}
	assertKaitoDefaultsForTest(t, adopted)
	if _, found, err := unstructured.NestedString(
		adopted.Object,
		"resource",
		"labelSelector",
		"matchLabels",
		"topology.example.com/pool",
	); err != nil || found {
		t.Fatalf("expected stale key in atomic selector to be removed, found=%v err=%v", found, err)
	}

	changed := desired.DeepCopy()
	changedResource, _, _ := unstructured.NestedMap(changed.Object, "resource")
	changedResource["count"] = int64(2)
	changed.Object["resource"] = changedResource
	if err := r.createOrUpdateResource(context.Background(), changed, newSSADeploymentForTest()); err != nil {
		t.Fatalf("update after legacy ownership migration: %v", err)
	}
	count, found, err := unstructured.NestedInt64(getWorkspaceForTest(t, c).Object, "resource", "count")
	if err != nil || !found || count != 2 {
		t.Fatalf("expected post-migration update to succeed, got count=%d found=%v err=%v", count, found, err)
	}
}

func TestCreateOrUpdateResourceMigratesLegacyFieldsAcrossAPIVersions(t *testing.T) {
	scheme := newScheme()
	seedClient := fake.NewClientBuilder().WithScheme(scheme).WithReturnManagedFields().Build()
	existing := newSSAWorkspaceForTest(testPrivateAccessMode)
	if err := setLastAppliedManagedFields(existing); err != nil {
		t.Fatalf("set legacy last-applied state: %v", err)
	}
	if err := seedClient.Create(context.Background(), existing, client.FieldOwner("legacy-kaito-provider")); err != nil {
		t.Fatalf("create legacy Workspace: %v", err)
	}
	live := getWorkspaceForTest(t, seedClient)
	managedFields := live.GetManagedFields()
	for index := range managedFields {
		if managedFields[index].Manager == "legacy-kaito-provider" &&
			managedFields[index].Operation == metav1.ManagedFieldsOperationUpdate {
			managedFields[index].APIVersion = "kaito.sh/v1alpha1"
		}
	}
	live.SetManagedFields(managedFields)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).WithReturnManagedFields().Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("adopt cross-version legacy Workspace: %v", err)
	}
	updated := getWorkspaceForTest(t, c)
	if _, found, err := unstructured.NestedString(
		updated.Object,
		"inference",
		"preset",
		"accessMode",
	); err != nil || found {
		t.Fatalf(
			"expected cross-version stale field to be removed, found=%v err=%v annotations=%v managedFields=%v",
			found,
			err,
			updated.GetAnnotations(),
			updated.GetManagedFields(),
		)
	}
	if managers, err := updateManagersOwningMigrationState(updated); err != nil || len(managers) != 0 {
		t.Fatalf("expected cross-version Update ownership to be removed, managers=%v err=%v", managers, err)
	}
	if _, found := updated.GetAnnotations()[migrationPreviousFieldsAnnotation]; found {
		t.Fatalf("expected cross-version migration fingerprint to be cleared, got %v", updated.GetAnnotations())
	}
}

func TestCreateOrUpdateResourceAdoptsPreAnnotationWorkspace(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest(testPrivateAccessMode)
	existing.SetAnnotations(nil)
	existing.SetLabels(map[string]string{
		"airunway.ai/managed-by":       "airunway",
		"airunway.ai/model-deployment": "test",
	})
	existing.Object["resource"] = map[string]any{
		"count": int64(2),
		"labelSelector": map[string]any{
			"matchLabels": map[string]any{
				"kubernetes.io/os": testLinuxOS,
				"airunway.ai/old":  "true",
			},
		},
	}
	existing.Object["inference"] = map[string]any{
		"preset": map[string]any{
			"name":       "test-model",
			"accessMode": "public",
			"presetOptions": map[string]any{
				"modelAccessSecret": "operator-default",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithReturnManagedFields().Build()
	if err := c.Create(context.Background(), existing, client.FieldOwner("pre-annotation-kaito-provider")); err != nil {
		t.Fatalf("create pre-annotation Workspace: %v", err)
	}
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	desired := newSSAWorkspaceForTest(testPrivateAccessMode)
	desired.Object["resource"] = map[string]any{
		"count": int64(1),
		"labelSelector": map[string]any{
			"matchLabels": map[string]any{"kubernetes.io/os": testLinuxOS},
		},
	}

	if err := r.createOrUpdateResource(context.Background(), desired, newSSADeploymentForTest()); err != nil {
		t.Fatalf("adopt pre-annotation Workspace: %v", err)
	}
	adopted := getWorkspaceForTest(t, c)
	if !hasApplyManagedFields(adopted) {
		t.Fatalf("expected stable apply ownership after adoption, got %v", adopted.GetManagedFields())
	}
	migrationManagers, err := legacyUpdateManagers(adopted, newSSADeploymentForTest().UID)
	if err != nil {
		t.Fatalf("inspect adopted managedFields: %v", err)
	}
	if len(migrationManagers) != 0 {
		t.Fatalf("expected pre-annotation controller ownership to be handed off, got %v", migrationManagers)
	}
	count, found, err := unstructured.NestedInt64(adopted.Object, "resource", "count")
	if err != nil || !found || count != 1 {
		t.Fatalf("expected desired count to replace pre-annotation value, got %d found=%v err=%v", count, found, err)
	}
	accessMode, found, err := unstructured.NestedString(adopted.Object, "inference", "preset", "accessMode")
	if err != nil || !found || accessMode != testPrivateAccessMode {
		t.Fatalf(
			"expected desired accessMode to replace pre-annotation value, got %q found=%v err=%v",
			accessMode,
			found,
			err,
		)
	}
	matchLabels, found, err := unstructured.NestedStringMap(adopted.Object, "resource", "labelSelector", "matchLabels")
	if err != nil || !found || len(matchLabels) != 1 || matchLabels["kubernetes.io/os"] != testLinuxOS {
		t.Fatalf("expected stale pre-annotation selector to be removed, got %v found=%v err=%v", matchLabels, found, err)
	}
	assertKaitoDefaultsForTest(t, adopted)

	desiredWithoutAccessMode := desired.DeepCopy()
	unstructured.RemoveNestedField(desiredWithoutAccessMode.Object, "inference", "preset", "accessMode")
	if err := r.createOrUpdateResource(
		context.Background(),
		desiredWithoutAccessMode,
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("remove field after pre-annotation adoption: %v", err)
	}
	updated := getWorkspaceForTest(t, c)
	if _, found, err := unstructured.NestedString(
		updated.Object,
		"inference",
		"preset",
		"accessMode",
	); err != nil || found {
		t.Fatalf("expected adopted accessMode to be removed, found=%v err=%v", found, err)
	}
	assertKaitoDefaultsForTest(t, updated)
}

func TestCreateOrUpdateResourceRecoversPreAnnotationOwnershipMigration(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest(testPrivateAccessMode)
	existing.SetAnnotations(nil)
	wantErr := errors.New("managedFields migration interrupted")
	failMigration := true
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if patch.Type() == types.JSONPatchType && failMigration {
					failMigration = false
					return wantErr
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	if err := c.Create(context.Background(), existing, client.FieldOwner("pre-annotation-kaito-provider")); err != nil {
		t.Fatalf("create pre-annotation Workspace: %v", err)
	}
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(testPrivateAccessMode),
		newSSADeploymentForTest(),
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected interrupted ownership migration, got %v", err)
	}
	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(testPrivateAccessMode),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("retry ownership migration: %v", err)
	}
	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("remove field after recovered migration: %v", err)
	}
	updated := getWorkspaceForTest(t, c)
	if _, found, err := unstructured.NestedString(
		updated.Object,
		"inference",
		"preset",
		"accessMode",
	); err != nil || found {
		t.Fatalf("expected recovered ownership to remove accessMode, found=%v err=%v", found, err)
	}
	if _, found := updated.GetAnnotations()[migrationManagersAnnotation]; found {
		t.Fatalf("expected temporary migration marker to be removed, got %v", updated.GetAnnotations())
	}
}

func TestCreateOrUpdateResourceRecoversUnchangedAnnotatedMigration(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest(testPrivateAccessMode)
	if err := setLastAppliedManagedFields(existing); err != nil {
		t.Fatalf("set annotated legacy Workspace state: %v", err)
	}
	wantErr := errors.New("managedFields migration interrupted")
	failMigration := true
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if patch.Type() == types.JSONPatchType && failMigration {
					failMigration = false
					return wantErr
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	if err := c.Create(context.Background(), existing, client.FieldOwner("legacy-kaito-provider")); err != nil {
		t.Fatalf("create annotated legacy Workspace: %v", err)
	}
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	desired := newSSAWorkspaceForTest(testPrivateAccessMode)

	err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest())
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected interrupted ownership migration, got %v", err)
	}
	pending, found, err := pendingMigrationManagers(getWorkspaceForTest(t, c))
	if err != nil || !found {
		t.Fatalf("expected persistent migration marker, pending=%v found=%v err=%v", pending, found, err)
	}
	if _, found := pending[FieldManager]; !found {
		t.Fatalf("expected migration marker to record %q, got %v", FieldManager, pending)
	}
	if err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
		t.Fatalf("retry annotated ownership migration: %v", err)
	}
	updated := getWorkspaceForTest(t, c)
	if _, found := updated.GetAnnotations()[migrationManagersAnnotation]; found {
		t.Fatalf("expected recovered migration marker to be removed, got %v", updated.GetAnnotations())
	}
	if managers, err := updateManagersOwningMigrationState(updated); err != nil || len(managers) != 0 {
		t.Fatalf("expected controller Update migration ownership to be removed, managers=%v err=%v", managers, err)
	}
}

func TestCreateOrUpdateResourceRecoversWhenOperatorReownsMigrationMarker(t *testing.T) {
	scheme := newScheme()
	controllerApplyCalls := 0
	failClear := false
	applyPatch := interceptApplyPatch(func(
		ctx context.Context,
		c client.WithWatch,
		obj runtime.ApplyConfiguration,
		opts ...client.ApplyOption,
	) error {
		options := (&client.ApplyOptions{}).ApplyOptions(opts)
		if options.FieldManager == FieldManager {
			controllerApplyCalls++
		}
		return c.Apply(ctx, obj, opts...)
	})
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if patch.Type() == types.MergePatchType && failClear {
					if _, markerPresent := obj.GetAnnotations()[migrationManagersAnnotation]; !markerPresent {
						failClear = false
						return apierrors.NewConflict(
							schema.GroupResource{Group: KaitoAPIGroup, Resource: "workspaces"},
							obj.GetName(),
							errors.New("simulated concurrent KAITO annotation update"),
						)
					}
				}
				return applyPatch(ctx, c, obj, patch, opts...)
			},
		}).
		Build()

	stable := newSSAWorkspaceForTest("")
	if err := setLastAppliedManagedFields(stable); err != nil {
		t.Fatalf("set stable applied state: %v", err)
	}
	if err := c.Apply(
		context.Background(),
		client.ApplyConfigurationFromUnstructured(stable),
		client.FieldOwner(FieldManager),
	); err != nil {
		t.Fatalf("seed stable applied Workspace: %v", err)
	}
	live := getWorkspaceForTest(t, c)
	base := live.DeepCopy()
	annotations := copyStringMap(live.GetAnnotations())
	delete(annotations, lastAppliedWorkspaceAnnotation)
	annotations[migrationManagersAnnotation] = `["kaito-provider"]`
	annotations["external.example.com/value"] = testPreservedAnnotationValue
	live.SetAnnotations(annotations)
	if err := c.Patch(
		context.Background(),
		live,
		client.MergeFrom(base),
		client.FieldOwner("kaito-workspace"),
	); err != nil {
		t.Fatalf("simulate KAITO annotation update: %v", err)
	}
	interrupted := getWorkspaceForTest(t, c)
	if fields, err := managedFieldsForManager(
		interrupted,
		preservedFieldsManager,
		metav1.ManagedFieldsOperationApply,
	); err != nil || fields != nil {
		t.Fatalf("expected missing preservation manager, fields=%v err=%v", fields, err)
	}

	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	desired := newSSAWorkspaceForTest("")
	failClear = true
	err := r.createOrUpdateResource(
		context.Background(),
		desired.DeepCopy(),
		newSSADeploymentForTest(),
	)
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected migration cleanup conflict to surface, got %v", err)
	}
	if _, found := getWorkspaceForTest(t, c).GetAnnotations()[migrationManagersAnnotation]; !found {
		t.Fatal("expected migration marker to remain after failed cleanup")
	}
	if err := r.createOrUpdateResource(
		context.Background(),
		desired.DeepCopy(),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("resume operator-owned migration marker: %v", err)
	}
	recovered := getWorkspaceForTest(t, c)
	if _, found := recovered.GetAnnotations()[migrationManagersAnnotation]; found {
		t.Fatalf("expected migration marker to be cleared, got %v", recovered.GetAnnotations())
	}
	if recovered.GetAnnotations()["external.example.com/value"] != testPreservedAnnotationValue {
		t.Fatalf("expected unrelated annotation to survive, got %v", recovered.GetAnnotations())
	}
	if recovered.GetAnnotations()[lastAppliedWorkspaceAnnotation] == "" {
		t.Fatalf("expected stable applied fingerprint, got %v", recovered.GetAnnotations())
	}
	applyCallsAfterRecovery := controllerApplyCalls
	if err := r.createOrUpdateResource(
		context.Background(),
		desired.DeepCopy(),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("stable reconcile after recovery: %v", err)
	}
	if controllerApplyCalls != applyCallsAfterRecovery {
		t.Fatalf(
			"expected stable reconcile after recovery to be a no-op, apply calls %d -> %d",
			applyCallsAfterRecovery,
			controllerApplyCalls,
		)
	}
}

func TestCreateOrUpdateResourceRecoveryRemovesFieldDroppedDuringMigration(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest(testPrivateAccessMode)
	if err := setLastAppliedManagedFields(existing); err != nil {
		t.Fatalf("set annotated legacy Workspace state: %v", err)
	}
	wantErr := errors.New("preservation release interrupted")
	failRelease := true
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				options := (&client.ApplyOptions{}).ApplyOptions(opts)
				workspace := obj.(interface{ UnstructuredContent() map[string]any })
				_, hasAccessMode, err := unstructured.NestedString(
					workspace.UnstructuredContent(),
					"inference",
					"preset",
					"accessMode",
				)
				if err != nil {
					return err
				}
				if failRelease && options.FieldManager == preservedFieldsManager && !hasAccessMode {
					failRelease = false
					return wantErr
				}
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	if err := c.Create(context.Background(), existing, client.FieldOwner("legacy-kaito-provider")); err != nil {
		t.Fatalf("create annotated legacy Workspace: %v", err)
	}
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	err := r.createOrUpdateResource(context.Background(), newSSAWorkspaceForTest(""), newSSADeploymentForTest())
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected interrupted preservation release, got %v", err)
	}
	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("resume migration with removed accessMode: %v", err)
	}
	updated := getWorkspaceForTest(t, c)
	if _, found, err := unstructured.NestedString(
		updated.Object,
		"inference",
		"preset",
		"accessMode",
	); err != nil || found {
		t.Fatalf("expected field removed during interrupted migration to be cleared, found=%v err=%v", found, err)
	}
	if _, found := updated.GetAnnotations()[migrationManagersAnnotation]; found {
		t.Fatalf("expected migration marker to be cleared, got %v", updated.GetAnnotations())
	}
	if _, found := updated.GetAnnotations()[migrationPreviousFieldsAnnotation]; found {
		t.Fatalf("expected original migration fingerprint to be cleared, got %v", updated.GetAnnotations())
	}
}

func TestCreateOrUpdateResourceRetainsMigrationStateUntilStaleCleanupSucceeds(t *testing.T) {
	scheme := newScheme()
	seedClient := fake.NewClientBuilder().WithScheme(scheme).WithReturnManagedFields().Build()
	existing := newSSAWorkspaceForTest(testPrivateAccessMode)
	if err := setLastAppliedManagedFields(existing); err != nil {
		t.Fatalf("set legacy last-applied state: %v", err)
	}
	if err := seedClient.Create(context.Background(), existing, client.FieldOwner("legacy-kaito-provider")); err != nil {
		t.Fatalf("create legacy Workspace: %v", err)
	}
	live := getWorkspaceForTest(t, seedClient)
	managedFields := live.GetManagedFields()
	for index := range managedFields {
		if managedFields[index].Manager == "legacy-kaito-provider" &&
			managedFields[index].Operation == metav1.ManagedFieldsOperationUpdate {
			managedFields[index].APIVersion = "kaito.sh/v1alpha1"
		}
	}
	live.SetManagedFields(managedFields)

	wantErr := errors.New("stale cleanup interrupted")
	releaseApplied := false
	failCleanup := true
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(live).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if patch.Type() == types.ApplyPatchType {
					options := &client.PatchOptions{}
					for _, opt := range opts {
						opt.ApplyToPatch(options)
					}
					if options.FieldManager != preservedFieldsManager {
						return c.Patch(ctx, obj, patch, opts...)
					}
					content := obj.(*unstructured.Unstructured).UnstructuredContent()
					annotations, _, err := unstructured.NestedStringMap(content, "metadata", "annotations")
					if err != nil {
						return err
					}
					_, hasManagers := annotations[migrationManagersAnnotation]
					_, hasPrevious := annotations[migrationPreviousFieldsAnnotation]
					_, hasStaleField, err := unstructured.NestedString(content, "inference", "preset", "accessMode")
					if err != nil {
						return err
					}
					if hasManagers && hasPrevious && !hasStaleField {
						releaseApplied = true
					}
				}
				if patch.Type() == types.MergePatchType && releaseApplied && failCleanup {
					failCleanup = false
					return wantErr
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	desired := newSSAWorkspaceForTest("")

	err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest())
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected interrupted stale cleanup, got %v", err)
	}
	interrupted := getWorkspaceForTest(t, c)
	if _, found := interrupted.GetAnnotations()[migrationManagersAnnotation]; !found {
		t.Fatalf("expected migration marker to survive failed cleanup, got %v", interrupted.GetAnnotations())
	}
	if _, found := interrupted.GetAnnotations()[migrationPreviousFieldsAnnotation]; !found {
		t.Fatalf("expected migration fingerprint to survive failed cleanup, got %v", interrupted.GetAnnotations())
	}

	if err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
		t.Fatalf("retry stale cleanup: %v", err)
	}
	updated := getWorkspaceForTest(t, c)
	if _, found, err := unstructured.NestedString(
		updated.Object,
		"inference",
		"preset",
		"accessMode",
	); err != nil || found {
		t.Fatalf("expected retry to remove stale field, found=%v err=%v", found, err)
	}
	if _, found := updated.GetAnnotations()[migrationManagersAnnotation]; found {
		t.Fatalf("expected migration marker to clear after cleanup, got %v", updated.GetAnnotations())
	}
	if _, found := updated.GetAnnotations()[migrationPreviousFieldsAnnotation]; found {
		t.Fatalf("expected migration fingerprint to clear after cleanup, got %v", updated.GetAnnotations())
	}
}

func TestCreateOrUpdateResourceHandsOffNewlyRenderedPreservedFieldBeforeRelease(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest("public")
	existing.SetAnnotations(nil)
	enforceHandoff := false
	stableClaimed := false
	applyManagers := []string{}
	wantAdmissionErr := errors.New("accessMode cannot be absent during ownership handoff")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				options := (&client.ApplyOptions{}).ApplyOptions(opts)
				if enforceHandoff {
					applyManagers = append(applyManagers, options.FieldManager)
					workspace := obj.(interface{ UnstructuredContent() map[string]any })
					accessMode, found, err := unstructured.NestedString(
						workspace.UnstructuredContent(),
						"inference",
						"preset",
						"accessMode",
					)
					if err != nil {
						return err
					}
					switch options.FieldManager {
					case FieldManager:
						if found && accessMode == testPrivateAccessMode {
							stableClaimed = true
						}
					case preservedFieldsManager:
						if !found && !stableClaimed {
							return wantAdmissionErr
						}
					}
				}
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	if err := c.Create(context.Background(), existing, client.FieldOwner("legacy-kaito-provider")); err != nil {
		t.Fatalf("create legacy Workspace: %v", err)
	}
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	if err := r.createOrUpdateResource(
		context.Background(),
		newSSAWorkspaceForTest(""),
		newSSADeploymentForTest(),
	); err != nil {
		t.Fatalf("adopt preserved accessMode: %v", err)
	}

	enforceHandoff = true
	desired := newSSAWorkspaceForTest(testPrivateAccessMode)
	if err := r.createOrUpdateResource(context.Background(), desired, newSSADeploymentForTest()); err != nil {
		t.Fatalf("hand off newly rendered accessMode: %v", err)
	}
	wantManagers := []string{preservedFieldsManager, FieldManager, preservedFieldsManager}
	if !slices.Equal(applyManagers, wantManagers) {
		t.Fatalf("expected preserved/stable/release handoff order %v, got %v", wantManagers, applyManagers)
	}
	updated := getWorkspaceForTest(t, c)
	accessMode, found, err := unstructured.NestedString(updated.Object, "inference", "preset", "accessMode")
	if err != nil || !found || accessMode != testPrivateAccessMode {
		t.Fatalf("expected handed-off accessMode private, got %q found=%v err=%v", accessMode, found, err)
	}
	overlaps, err := managerOwnsAnyDesired(updated, preservedFieldsManager, desired)
	if err != nil || overlaps {
		t.Fatalf("expected preservation manager to relinquish rendered fields, overlaps=%v err=%v", overlaps, err)
	}
}

func TestCreateOrUpdateResourceReappliesWhenRenderedOwnershipIsLost(t *testing.T) {
	scheme := newScheme()
	controllerApplyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				options := (&client.ApplyOptions{}).ApplyOptions(opts)
				if options.FieldManager == FieldManager {
					controllerApplyCalls++
				}
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	desired := newSSAWorkspaceForTest(testPrivateAccessMode)
	if err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}

	external := workspaceApplyForTest()
	external.Object["inference"] = map[string]any{
		"preset": map[string]any{"accessMode": "public"},
	}
	applyAsManagerForTest(t, c, external, "external-access-manager", true)
	external.Object["inference"] = map[string]any{
		"preset": map[string]any{"accessMode": testPrivateAccessMode},
	}
	applyAsManagerForTest(t, c, external, "external-access-manager", false)

	if err := r.createOrUpdateResource(context.Background(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
		t.Fatalf("re-establish rendered ownership: %v", err)
	}
	if controllerApplyCalls != 3 {
		t.Fatalf("expected ownership loss to trigger another controller Apply, got %d", controllerApplyCalls)
	}
	owned, err := applyManagerOwnsDesired(getWorkspaceForTest(t, c), desired)
	if err != nil || !owned {
		t.Fatalf("expected rendered ownership to be restored, owned=%v err=%v", owned, err)
	}
}

func TestCreateOrUpdateResourceDoesNotAdoptUnownedWorkspace(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest("")
	existing.SetOwnerReferences([]metav1.OwnerReference{{UID: "other-uid"}})
	applyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existing).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				applyCalls++
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	err := r.createOrUpdateResource(context.Background(), newSSAWorkspaceForTest(""), newSSADeploymentForTest())
	if !isResourceConflict(err) {
		t.Fatalf("expected resource ownership conflict, got %v", err)
	}
	if applyCalls != 0 {
		t.Fatalf("expected unowned Workspace not to be applied, got %d calls", applyCalls)
	}
}

func TestCreateOrUpdateResourceDoesNotOverwriteUnrelatedSSAOwnerDuringAdoption(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest("")
	if err := setLastAppliedManagedFields(existing); err != nil {
		t.Fatalf("set legacy last-applied state: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithReturnManagedFields().Build()
	if err := c.Create(context.Background(), existing, client.FieldOwner("legacy-kaito-provider")); err != nil {
		t.Fatalf("create legacy Workspace: %v", err)
	}
	external := workspaceApplyForTest()
	external.Object["resource"] = map[string]any{"count": int64(2)}
	applyAsManagerForTest(t, c, external, "external-count-manager", true)
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	err := r.createOrUpdateResource(context.Background(), newSSAWorkspaceForTest(""), newSSADeploymentForTest())
	if !apierrors.IsConflict(err) || !isResourceConflict(err) {
		t.Fatalf("expected unrelated SSA ownership conflict during adoption, got %v", err)
	}
	count, found, getErr := unstructured.NestedInt64(getWorkspaceForTest(t, c).Object, "resource", "count")
	if getErr != nil || !found || count != 2 {
		t.Fatalf("expected external value to remain untouched, count=%d found=%v err=%v", count, found, getErr)
	}
}

func TestCreateOrUpdateResourceSurfacesSSAConflict(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithReturnManagedFields().Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))
	desired := newSSAWorkspaceForTest("")

	if err := r.createOrUpdateResource(context.Background(), desired, newSSADeploymentForTest()); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	if err := r.createOrUpdateResource(context.Background(), desired, newSSADeploymentForTest()); err != nil {
		t.Fatalf("adopt Workspace with SSA: %v", err)
	}
	conflicting := workspaceApplyForTest()
	conflicting.Object["resource"] = map[string]any{"count": int64(2)}
	applyAsManagerForTest(t, c, conflicting, "other-manager", true)

	err := r.createOrUpdateResource(context.Background(), newSSAWorkspaceForTest(""), newSSADeploymentForTest())
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected SSA conflict, got %v", err)
	}
}

func TestCreateOrUpdateResourceSurfacesApplyError(t *testing.T) {
	scheme := newScheme()
	wantErr := errors.New("apply transport failed")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				context.Context,
				client.WithWatch,
				runtime.ApplyConfiguration,
				...client.ApplyOption,
			) error {
				return wantErr
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	err := r.createOrUpdateResource(context.Background(), newSSAWorkspaceForTest(""), newSSADeploymentForTest())
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped apply error, got %v", err)
	}
	if !strings.Contains(err.Error(), "server-side apply") {
		t.Fatalf("expected apply context in error, got %v", err)
	}
}

func TestCreateOrUpdateResourceSurfacesLegacyMigrationError(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest(testPrivateAccessMode)
	setLastAppliedForTest(
		t,
		existing,
		map[string]any{"count": int64(1)},
		map[string]any{"preset": map[string]any{"name": "test-model", "accessMode": testPrivateAccessMode}},
	)
	wantErr := errors.New("migration patch failed")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existing).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return wantErr
			},
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	err := r.createOrUpdateResource(context.Background(), newSSAWorkspaceForTest(""), newSSADeploymentForTest())
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped migration error, got %v", err)
	}
	if !strings.Contains(err.Error(), "mark legacy Workspace") {
		t.Fatalf("expected migration context in error, got %v", err)
	}
}

func TestCreateOrUpdateResourceRejectsMalformedLegacyAnnotation(t *testing.T) {
	scheme := newScheme()
	existing := newSSAWorkspaceForTest("")
	existing.SetAnnotations(map[string]string{lastAppliedWorkspaceAnnotation: "{"})
	applyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				ctx context.Context,
				c client.WithWatch,
				obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				applyCalls++
				return c.Apply(ctx, obj, opts...)
			}),
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	err := r.createOrUpdateResource(context.Background(), newSSAWorkspaceForTest(""), newSSADeploymentForTest())
	if err == nil || !strings.Contains(err.Error(), "last-applied annotation") {
		t.Fatalf("expected malformed annotation error, got %v", err)
	}
	if applyCalls != 0 {
		t.Fatalf("expected malformed migration state not to be applied, got %d calls", applyCalls)
	}
}

func TestCreateOrUpdateResourceSurfacesManagedFieldsMigrationError(t *testing.T) {
	scheme := newScheme()
	wantErr := errors.New("managedFields migration failed")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if patch.Type() == types.JSONPatchType {
					return wantErr
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	err := r.createOrUpdateResource(context.Background(), newSSAWorkspaceForTest(""), newSSADeploymentForTest())
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped managedFields migration error, got %v", err)
	}
	if !strings.Contains(err.Error(), "migrate Workspace") || !strings.Contains(err.Error(), "managedFields") {
		t.Fatalf("expected managedFields migration context, got %v", err)
	}
}

func TestReconcileSSAConflictWritesStatusOnceAndRetriesSlowly(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.APIVersion = testModelDeploymentAPIVersion
	md.Kind = testModelDeploymentKind
	md.UID = testUID
	controllerutil.AddFinalizer(md, FinalizerName)
	statusUpdates := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				ctx context.Context,
				c client.Client,
				subResourceName string,
				obj client.Object,
				opts ...client.SubResourceUpdateOption,
			) error {
				if subResourceName == "status" {
					statusUpdates++
				}
				return c.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()
	direct := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, direct, record.NewFakeRecorder(10))

	resources, err := r.Transformer.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if err := r.createOrUpdateResource(context.Background(), resources[0], md); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	if err := r.createOrUpdateResource(context.Background(), resources[0], md); err != nil {
		t.Fatalf("adopt Workspace with SSA: %v", err)
	}
	conflicting := workspaceApplyForTest()
	conflicting.Object["resource"] = map[string]any{"count": int64(2)}
	applyAsManagerForTest(t, c, conflicting, "other-manager", true)

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(md)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != ExternalRecoveryInterval {
		t.Fatalf("expected conflict to use the external-recovery retry, got %#v", result)
	}
	if statusUpdates != 1 {
		t.Fatalf("expected the conflict transition to write status once, got %d updates", statusUpdates)
	}
	var got airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(md), &got); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	if got.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Fatalf("expected failed phase, got %q", got.Status.Phase)
	}
	condition := apimeta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeResourceCreated)
	if condition == nil || condition.Reason != testResourceConflictReason || condition.Status != metav1.ConditionFalse {
		t.Fatalf("expected ResourceConflict condition, got %#v", condition)
	}

	result, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(md)})
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if result.RequeueAfter != ExternalRecoveryInterval {
		t.Fatalf("expected persistent conflict to retain the external-recovery retry, got %#v", result)
	}
	if statusUpdates != 1 {
		t.Fatalf("expected unchanged conflict not to write status again, got %d updates", statusUpdates)
	}
}

func TestReconcileRetriesOptimisticManagedFieldsConflict(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.APIVersion = testModelDeploymentAPIVersion
	md.Kind = testModelDeploymentKind
	md.UID = testUID
	controllerutil.AddFinalizer(md, FinalizerName)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if patch.Type() == types.JSONPatchType {
					return apierrors.NewConflict(
						schema.GroupResource{Group: KaitoAPIGroup, Resource: "workspaces"},
						obj.GetName(),
						errors.New("resourceVersion changed"),
					)
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	direct := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, direct, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(md)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("expected optimistic conflict to retry promptly, got %#v", result)
	}
	var got airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(md), &got); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	if got.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Fatalf("expected unverified create-migration conflict to fail closed, got %#v", got.Status)
	}
	condition := apimeta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeResourceCreated)
	if condition == nil || condition.Reason != testCreateFailedReason || condition.Status != metav1.ConditionFalse {
		t.Fatalf("expected surfaced optimistic conflict condition, got %#v", condition)
	}
}

func TestReconcileSurfacesApplyErrorInStatus(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.APIVersion = testModelDeploymentAPIVersion
	md.Kind = testModelDeploymentKind
	md.UID = testUID
	controllerutil.AddFinalizer(md, FinalizerName)
	wantErr := errors.New("apply transport failed")
	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	existing := resources[0]
	if err := setLastAppliedManagedFields(existing); err != nil {
		t.Fatalf("set last applied fields: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, existing).
		WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: interceptApplyPatch(func(
				context.Context,
				client.WithWatch,
				runtime.ApplyConfiguration,
				...client.ApplyOption,
			) error {
				return wantErr
			}),
		}).
		Build()
	direct := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, direct, record.NewFakeRecorder(10))

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(md)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != ExternalRecoveryInterval {
		t.Fatalf("expected apply error to be recorded with an external-recovery retry, got %#v", result)
	}
	var got airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(md), &got); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	if got.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed ||
		!strings.Contains(got.Status.Message, wantErr.Error()) {
		t.Fatalf("expected failed apply status, phase=%q message=%q", got.Status.Phase, got.Status.Message)
	}
	condition := apimeta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeResourceCreated)
	if condition == nil || condition.Reason != testCreateFailedReason || condition.Status != metav1.ConditionFalse {
		t.Fatalf("expected CreateFailed condition, got %#v", condition)
	}
}

func TestSetLastAppliedManagedFieldsCopiesAnnotations(t *testing.T) {
	ws := &unstructured.Unstructured{}
	ws.SetName("test")
	original := map[string]string{
		"operator.example.com/defaulted":  "true",
		lastAppliedWorkspaceAnnotation:    "untrusted-fingerprint",
		migrationManagersAnnotation:       "foo",
		migrationPreviousFieldsAnnotation: "{",
	}
	ws.SetAnnotations(original)

	if err := setLastAppliedManagedFields(ws); err != nil {
		t.Fatalf("setLastAppliedManagedFields: %v", err)
	}
	if original[lastAppliedWorkspaceAnnotation] != "untrusted-fingerprint" ||
		original[migrationManagersAnnotation] != "foo" ||
		original[migrationPreviousFieldsAnnotation] != "{" {
		t.Fatalf("expected caller annotations not to be mutated, got %v", original)
	}
	original["operator.example.com/defaulted"] = "mutated"
	if ws.GetAnnotations()["operator.example.com/defaulted"] != "true" {
		t.Fatalf("expected copied annotations, got %v", ws.GetAnnotations())
	}
	for _, key := range []string{migrationManagersAnnotation, migrationPreviousFieldsAnnotation} {
		if _, found := ws.GetAnnotations()[key]; found {
			t.Fatalf("expected reserved annotation %q to be removed from desired state, got %v", key, ws.GetAnnotations())
		}
	}
	_, _, _, annotations, err := lastAppliedManagedFields(ws)
	if err != nil {
		t.Fatalf("lastAppliedManagedFields: %v", err)
	}
	if !equality.Semantic.DeepEqual(annotations, map[string]string{"operator.example.com/defaulted": "true"}) {
		t.Fatalf("expected fingerprint to contain only ordinary annotations, got %v", annotations)
	}
}

func TestExtractManagedFieldsListKeepsOnlyOwnedSetValues(t *testing.T) {
	live := []any{"airunway.ai/owned", "external.example.com/other"}
	fields := map[string]any{
		`v:"airunway.ai/owned"`: map[string]any{},
	}

	extracted := extractManagedFieldsList(live, fields)
	if len(extracted) != 1 || extracted[0] != "airunway.ai/owned" {
		t.Fatalf("expected only the managed set value, got %v", extracted)
	}
}

func TestMergeManagedFieldValuesUnionsListsWithoutDuplicates(t *testing.T) {
	target := map[string]any{
		"metadata": map[string]any{
			"finalizers": []any{"shared", "first"},
		},
	}
	source := map[string]any{
		"metadata": map[string]any{
			"finalizers": []any{"shared", "second"},
		},
	}

	merged := mergeManagedFieldValues(target, source)
	finalizers, found, err := unstructured.NestedStringSlice(merged, "metadata", "finalizers")
	if err != nil || !found {
		t.Fatalf("get merged finalizers: found=%v err=%v", found, err)
	}
	if !slices.Equal(finalizers, []string{"shared", "first", "second"}) {
		t.Fatalf("expected a stable union of list values, got %v", finalizers)
	}
}

func TestExtractManagedFieldsListCopiesWholeOwnedKeyedItem(t *testing.T) {
	live := []any{map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "ExternalOwner",
		"name":       "external",
		"uid":        "external-owner-uid",
	}}
	for _, children := range []map[string]any{
		{},
		{".": map[string]any{}},
	} {
		fields := map[string]any{
			`k:{"uid":"external-owner-uid"}`: children,
		}
		extracted := extractManagedFieldsList(live, fields)
		if !slices.EqualFunc(extracted, live, func(left, right any) bool {
			return equality.Semantic.DeepEqual(left, right)
		}) {
			t.Fatalf("expected full keyed item for fields %v, got %v", children, extracted)
		}
	}
}

func TestManagedFieldsOwnOwnerReferencesTreatsAtomicItemsAsWhole(t *testing.T) {
	desired, found, err := unstructured.NestedSlice(newSSAWorkspaceForTest("").Object, "metadata", "ownerReferences")
	if err != nil || !found {
		t.Fatalf("get desired owner references: found=%v err=%v", found, err)
	}
	for name, children := range map[string]map[string]any{
		"empty item": {},
		"dot item":   {".": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			fields := map[string]any{
				fmt.Sprintf(`k:{"uid":%q}`, testUID): children,
			}
			if !managedFieldsOwnOwnerReferences(desired, fields) {
				t.Fatalf("expected atomic item fields %v to own the full owner reference", children)
			}
		})
	}
}

func TestPreservedFieldsHandoffPreservesNonRenderedOwnerReferences(t *testing.T) {
	desired := newSSAWorkspaceForTest("")
	live := desired.DeepCopy()
	ownerReferences := live.GetOwnerReferences()
	ownerReferences[0].Name = "legacy-name"
	ownerReferences = append(ownerReferences, metav1.OwnerReference{
		APIVersion: "example.com/v1",
		Kind:       "ExternalOwner",
		Name:       "external",
		UID:        "external-owner-uid",
	})
	live.SetOwnerReferences(ownerReferences)
	live.SetManagedFields([]metav1.ManagedFieldsEntry{{
		Manager:    preservedFieldsManager,
		Operation:  metav1.ManagedFieldsOperationApply,
		APIVersion: "kaito.sh/v1beta1",
		FieldsType: "FieldsV1",
		FieldsV1: &metav1.FieldsV1{Raw: []byte(
			`{"f:metadata":{"f:ownerReferences":{"k:{\"uid\":\"test-uid\"}":{},"k:{\"uid\":\"external-owner-uid\"}":{}}}}`,
		)},
	}})

	handoff, err := preservedFieldsHandoffConfiguration(live, desired)
	if err != nil {
		t.Fatalf("build ownership handoff: %v", err)
	}
	got := handoff.GetOwnerReferences()
	if len(got) != 2 {
		t.Fatalf("expected both rendered and external owner references, got %v", got)
	}
	namesByUID := map[types.UID]string{}
	for _, reference := range got {
		namesByUID[reference.UID] = reference.Name
	}
	if namesByUID[testUID] != "test" {
		t.Fatalf("expected rendered owner reference to use desired value, got %v", got)
	}
	if namesByUID["external-owner-uid"] != "external" {
		t.Fatalf("expected external owner reference to survive handoff, got %v", got)
	}
}

func TestPreservedFieldsHandoffRelinquishesAtomicSelectorOwnedByStableManager(t *testing.T) {
	live := newSSAWorkspaceForTest("")
	live.Object["resource"] = map[string]any{
		"count": int64(1),
		"labelSelector": map[string]any{
			"matchLabels": map[string]any{"kubernetes.io/os": testLinuxOS},
		},
	}
	desired := live.DeepCopy()
	desired.Object["resource"].(map[string]any)["labelSelector"] = map[string]any{
		"matchLabels": map[string]any{
			"kubernetes.io/os": testLinuxOS,
			"airunway.ai/new":  "true",
		},
	}
	selectorFields := []byte(`{"f:resource":{"f:labelSelector":{}}}`)
	live.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:    preservedFieldsManager,
			Operation:  metav1.ManagedFieldsOperationApply,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1:   &metav1.FieldsV1{Raw: selectorFields},
		},
		{
			Manager:    FieldManager,
			Operation:  metav1.ManagedFieldsOperationApply,
			APIVersion: "kaito.sh/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1:   &metav1.FieldsV1{Raw: selectorFields},
		},
	})

	handoff, err := preservedFieldsHandoffConfiguration(live, desired)
	if err != nil {
		t.Fatalf("build interrupted selector handoff: %v", err)
	}
	if _, found, err := unstructured.NestedMap(handoff.Object, "resource", "labelSelector"); err != nil || found {
		t.Fatalf(
			"expected preservation manager to relinquish the whole atomic selector, found=%v err=%v object=%v",
			found,
			err,
			handoff.Object,
		)
	}
}

func newSSADeploymentForTest() *airunwayv1alpha1.ModelDeployment {
	return &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
			UID:       testUID,
		},
	}
}

func newSSAWorkspaceForTest(accessMode string) *unstructured.Unstructured {
	workspace := workspaceApplyForTest()
	controller := true
	workspace.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: testModelDeploymentAPIVersion,
			Kind:       testModelDeploymentKind,
			Name:       "test",
			UID:        testUID,
			Controller: &controller,
		},
	})
	workspace.SetLabels(map[string]string{"airunway.ai/managed-by": "airunway"})
	workspace.Object["resource"] = map[string]any{"count": int64(1)}
	preset := map[string]any{"name": "test-model"}
	if accessMode != "" {
		preset["accessMode"] = accessMode
	}
	workspace.Object["inference"] = map[string]any{"preset": preset}
	return workspace
}

func workspaceApplyForTest() *unstructured.Unstructured {
	workspace := &unstructured.Unstructured{}
	setWorkspaceGVK(workspace)
	workspace.SetName("test")
	workspace.SetNamespace("default")
	return workspace
}

func getWorkspaceForTest(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	workspace := workspaceApplyForTest()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workspace), workspace); err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	return workspace
}

func interceptApplyPatch(
	callback func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error,
) func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
	return func(
		ctx context.Context,
		c client.WithWatch,
		obj client.Object,
		patch client.Patch,
		opts ...client.PatchOption,
	) error {
		if patch.Type() != types.ApplyPatchType {
			return c.Patch(ctx, obj, patch, opts...)
		}
		patchOptions := &client.PatchOptions{}
		for _, opt := range opts {
			opt.ApplyToPatch(patchOptions)
		}
		applyOptions := []client.ApplyOption{client.FieldOwner(patchOptions.FieldManager)}
		if patchOptions.Force != nil && *patchOptions.Force {
			applyOptions = append(applyOptions, client.ForceOwnership)
		}
		workspace, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return fmt.Errorf("expected unstructured apply patch, got %T", obj)
		}
		return callback(ctx, c, client.ApplyConfigurationFromUnstructured(workspace), applyOptions...)
	}
}

func applyAsManagerForTest(
	t *testing.T,
	c client.Client,
	workspace *unstructured.Unstructured,
	manager string,
	force bool,
) {
	t.Helper()
	options := []client.ApplyOption{client.FieldOwner(manager)}
	if force {
		options = append(options, client.ForceOwnership)
	}
	if err := c.Apply(
		context.Background(),
		client.ApplyConfigurationFromUnstructured(workspace.DeepCopy()),
		options...,
	); err != nil {
		t.Fatalf("apply Workspace as %q: %v", manager, err)
	}
}

func assertKaitoDefaultsForTest(t *testing.T, workspace *unstructured.Unstructured) {
	t.Helper()
	presetOptions, found, err := unstructured.NestedMap(workspace.Object, "inference", "preset", "presetOptions")
	if err != nil || !found || presetOptions["modelAccessSecret"] != "operator-default" {
		t.Fatalf("expected KAITO presetOptions default to survive, got %v found=%v err=%v", presetOptions, found, err)
	}
}

func TestSyncStatusNotFound(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setWorkspaceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")

	err := r.syncStatus(context.Background(), md, desired)
	if err != nil {
		t.Fatalf("unexpected error for not-found: %v", err)
	}
}

func TestSyncStatusRunning(t *testing.T) {
	scheme := newScheme()

	ws := &unstructured.Unstructured{}
	setWorkspaceGVK(ws)
	ws.SetName("test")
	ws.SetNamespace("default")
	ws.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{
				"type":   "WorkspaceSucceeded",
				"status": "True",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setWorkspaceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")

	err := r.syncStatus(context.Background(), md, desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", md.Status.Phase)
	}
}

func TestSyncStatusFailed(t *testing.T) {
	scheme := newScheme()

	ws := &unstructured.Unstructured{}
	setWorkspaceGVK(ws)
	ws.SetName("test")
	ws.SetNamespace("default")
	ws.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{
				"type":    "WorkspaceSucceeded",
				"status":  "False",
				"message": "failed",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setWorkspaceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")

	err := r.syncStatus(context.Background(), md, desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase, got %s", md.Status.Phase)
	}
}

func TestSyncStatusDeploying(t *testing.T) {
	scheme := newScheme()

	ws := &unstructured.Unstructured{}
	setWorkspaceGVK(ws)
	ws.SetName("test")
	ws.SetNamespace("default")
	ws.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{
				"type":   "ResourceReady",
				"status": "True",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setWorkspaceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")

	err := r.syncStatus(context.Background(), md, desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying phase, got %s", md.Status.Phase)
	}
}

func TestReconcile_InvalidSpecReportsCompatibilityBeforeProbe(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "default", Finalizers: []string{FinalizerName}},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model:   airunwayv1alpha1.ModelSpec{ID: "m", Source: airunwayv1alpha1.ModelSourceHuggingFace},
			Engine:  airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Serving: &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
		},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Provider: &airunwayv1alpha1.ProviderStatus{Name: ProviderName},
		},
	}
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(md).WithStatusSubresource(md).Build()
	rec := record.NewFakeRecorder(10)
	r := &KaitoProviderReconciler{
		Client:           c,
		Scheme:           s,
		Transformer:      NewTransformer(),
		StatusTranslator: NewStatusTranslator(),
		DirectClient:     c,
		Recorder:         rec,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "bad", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &airunwayv1alpha1.ModelDeployment{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "bad", Namespace: "default"}, got)

	// Must see IncompatibleConfiguration, NOT an upstream-health reason.
	if got.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Phase=Failed, got %q", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "disaggregated") {
		t.Errorf("expected message about disaggregated, got %q", got.Status.Message)
	}
}

func TestReconcile_UnhealthyProbeRefusesFast(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "mymd", Namespace: "default", Finalizers: []string{FinalizerName}},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model:  airunwayv1alpha1.ModelSpec{ID: "m", Source: airunwayv1alpha1.ModelSourceHuggingFace},
			Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Resources: &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{Count: 1},
			},
		},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Provider: &airunwayv1alpha1.ProviderStatus{Name: ProviderName},
		},
	}
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(md).WithStatusSubresource(md).Build()
	// directC: has Workspace CRD but no controller Deployment → UpstreamControllerMissing
	directC := probeClientBuilderWithWorkspace(t).Build()
	rec := record.NewFakeRecorder(10)
	r := &KaitoProviderReconciler{
		Client:           c,
		Scheme:           s,
		Transformer:      NewTransformer(),
		StatusTranslator: NewStatusTranslator(),
		DirectClient:     directC,
		Recorder:         rec,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "mymd", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != RequeueInterval {
		t.Errorf("expected RequeueAfter=%v, got %v", RequeueInterval, res.RequeueAfter)
	}

	got := &airunwayv1alpha1.ModelDeployment{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "mymd", Namespace: "default"}, got)

	// Phase must NOT be set to Failed (transient state).
	if got.Status.Phase == airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Phase to be left untouched, got Failed")
	}

	// Event must have been recorded.
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, ReasonUpstreamControllerMissing) {
			t.Errorf("expected event with %s, got %q", ReasonUpstreamControllerMissing, ev)
		}
	default:
		t.Error("expected a Warning event, none recorded")
	}
}

func setLastAppliedForTest(t *testing.T, obj *unstructured.Unstructured, resource, inference map[string]interface{}) {
	t.Helper()
	setLastAppliedForTestWithMetadata(t, obj, resource, inference, nil, nil)
}

func setLastAppliedForTestWithMetadata(t *testing.T, obj *unstructured.Unstructured, resource, inference map[string]interface{}, labels, annotations map[string]string) {
	t.Helper()

	lastApplied := &unstructured.Unstructured{Object: map[string]interface{}{}}
	lastApplied.SetLabels(labels)
	lastApplied.SetAnnotations(annotations)
	if resource != nil {
		lastApplied.Object["resource"] = resource
	}
	if inference != nil {
		lastApplied.Object["inference"] = inference
	}
	if err := setLastAppliedManagedFields(lastApplied); err != nil {
		t.Fatalf("failed to set last-applied annotation: %v", err)
	}

	objAnnotations := obj.GetAnnotations()
	if objAnnotations == nil {
		objAnnotations = map[string]string{}
	}
	objAnnotations[lastAppliedWorkspaceAnnotation] = lastApplied.GetAnnotations()[lastAppliedWorkspaceAnnotation]
	obj.SetAnnotations(objAnnotations)
}

func setWorkspaceGVK(u *unstructured.Unstructured) {
	u.SetAPIVersion("kaito.sh/v1beta1")
	u.SetKind("Workspace")
}
