package kaito

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
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
	controllerutil.AddFinalizer(md, FinalizerName)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
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
		t.Errorf("expected requeue after %v, got %v", RequeueInterval, result.RequeueAfter)
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
	md.UID = "test-uid"
	controllerutil.AddFinalizer(md, FinalizerName)

	// Create an upstream workspace that matches what the transformer would produce
	// so createOrUpdateResource does NOT update it (preserving status)
	ws := &unstructured.Unstructured{}
	setWorkspaceGVK(ws)
	ws.SetName("test")
	ws.SetNamespace("default")
	ws.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	ws.Object["resource"] = map[string]interface{}{
		"count": int64(1),
		"labelSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{
				"kubernetes.io/os": "linux",
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
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, ws).WithStatusSubresource(md).Build()
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
// https://github.com/kaito-project/airunway/issues/239 — when the KAITO
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
			md.UID = "test-uid"
			controllerutil.AddFinalizer(md, FinalizerName)
			now := metav1.Now()
			md.DeletionTimestamp = &now

			ws := &unstructured.Unstructured{}
			setWorkspaceGVK(ws)
			ws.SetName("test")
			ws.SetNamespace("default")
			ws.SetOwnerReferences([]metav1.OwnerReference{
				{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
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
	md.UID = "test-uid"
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	// Create upstream workspace
	ws := &unstructured.Unstructured{}
	setWorkspaceGVK(ws)
	ws.SetName("test")
	ws.SetNamespace("default")
	ws.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
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

func TestManagedFieldsMatch(t *testing.T) {
	tests := []struct {
		name        string
		desired     map[string]interface{}
		existing    map[string]interface{}
		lastApplied map[string]interface{}
		path        []string
		want        bool
	}{
		{
			name:        "exact scalar fields match",
			desired:     map[string]interface{}{"count": int64(1)},
			existing:    map[string]interface{}{"count": int64(1)},
			lastApplied: map[string]interface{}{"count": int64(1)},
			path:        []string{"resource"},
			want:        true,
		},
		{
			name:        "changed desired field does not match",
			desired:     map[string]interface{}{"count": int64(2)},
			existing:    map[string]interface{}{"count": int64(1)},
			lastApplied: map[string]interface{}{"count": int64(1)},
			path:        []string{"resource"},
			want:        false,
		},
		{
			name: "unmanaged nested operator default is ignored",
			desired: map[string]interface{}{
				"preset": map[string]interface{}{"name": "test"},
			},
			existing: map[string]interface{}{
				"preset": map[string]interface{}{"name": "test", "accessMode": "public"},
			},
			lastApplied: map[string]interface{}{
				"preset": map[string]interface{}{"name": "test"},
			},
			path: []string{"inference"},
			want: true,
		},
		{
			name: "deleted managed nested field does not match",
			desired: map[string]interface{}{
				"preset": map[string]interface{}{"name": "test"},
			},
			existing: map[string]interface{}{
				"preset": map[string]interface{}{"name": "test", "accessMode": "private"},
			},
			lastApplied: map[string]interface{}{
				"preset": map[string]interface{}{"name": "test", "accessMode": "private"},
			},
			path: []string{"inference"},
			want: false,
		},
		{
			name:        "matching slices match",
			desired:     map[string]interface{}{"presetOptions": []interface{}{"a", "b"}},
			existing:    map[string]interface{}{"presetOptions": []interface{}{"a", "b"}},
			lastApplied: map[string]interface{}{"presetOptions": []interface{}{"a", "b"}},
			path:        []string{"inference", "preset"},
			want:        true,
		},
		{
			name:        "changed slices do not match",
			desired:     map[string]interface{}{"presetOptions": []interface{}{"a", "b"}},
			existing:    map[string]interface{}{"presetOptions": []interface{}{"b", "a"}},
			lastApplied: map[string]interface{}{"presetOptions": []interface{}{"a", "b"}},
			path:        []string{"inference", "preset"},
			want:        false,
		},
		{
			name: "legacy BYO label selector extras are treated as managed",
			desired: map[string]interface{}{
				"labelSelector": map[string]interface{}{
					"matchLabels": map[string]interface{}{"kubernetes.io/os": "linux"},
				},
			},
			existing: map[string]interface{}{
				"labelSelector": map[string]interface{}{
					"matchLabels": map[string]interface{}{"kubernetes.io/os": "linux", "airunway.ai/old": "true"},
				},
			},
			path: []string{"resource"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := managedFieldsMatch(tt.desired, tt.existing, tt.lastApplied, tt.path...); got != tt.want {
				t.Fatalf("managedFieldsMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagedStringMapMatches(t *testing.T) {
	tests := []struct {
		name        string
		desired     map[string]string
		existing    map[string]string
		lastApplied map[string]string
		want        bool
	}{
		{
			name:        "desired labels present",
			desired:     map[string]string{"app": "test"},
			existing:    map[string]string{"app": "test", "operator.example.com/defaulted": "true"},
			lastApplied: map[string]string{"app": "test"},
			want:        true,
		},
		{
			name:        "desired label missing",
			desired:     map[string]string{"app": "test"},
			existing:    map[string]string{"app": "other"},
			lastApplied: map[string]string{"app": "test"},
			want:        false,
		},
		{
			name:        "deleted managed label remains",
			desired:     map[string]string{"app": "test"},
			existing:    map[string]string{"app": "test", "airunway.ai/old": "true"},
			lastApplied: map[string]string{"app": "test", "airunway.ai/old": "true"},
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := managedStringMapMatches(tt.desired, tt.existing, tt.lastApplied); got != tt.want {
				t.Fatalf("managedStringMapMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCreateOrUpdateResourceNew(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	ws := &unstructured.Unstructured{}
	setWorkspaceGVK(ws)
	ws.SetName("test")
	ws.SetNamespace("default")
	ws.Object["resource"] = map[string]interface{}{"count": int64(1)}

	err := r.createOrUpdateResource(context.Background(), ws, md)
	if err != nil {
		t.Fatalf("unexpected error creating resource: %v", err)
	}

	// Verify it was created
	existing := &unstructured.Unstructured{}
	setWorkspaceGVK(existing)
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, existing)
	if err != nil {
		t.Fatalf("expected resource to exist: %v", err)
	}
}

func TestCreateOrUpdateResourceUpdate(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setWorkspaceGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	existing.Object["resource"] = map[string]interface{}{"count": int64(1)}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	// Update with different resource
	updated := &unstructured.Unstructured{}
	setWorkspaceGVK(updated)
	updated.SetName("test")
	updated.SetNamespace("default")
	updated.Object["resource"] = map[string]interface{}{"count": int64(3)}

	err := r.createOrUpdateResource(context.Background(), updated, md)
	if err != nil {
		t.Fatalf("unexpected error updating resource: %v", err)
	}
}

func TestCreateOrUpdateResourceNoChange(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setWorkspaceGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	existing.Object["resource"] = map[string]interface{}{"count": int64(1)}
	existing.Object["inference"] = map[string]interface{}{"preset": map[string]interface{}{"name": "test"}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	// Same resource
	same := &unstructured.Unstructured{}
	setWorkspaceGVK(same)
	same.SetName("test")
	same.SetNamespace("default")
	same.Object["resource"] = map[string]interface{}{"count": int64(1)}
	same.Object["inference"] = map[string]interface{}{"preset": map[string]interface{}{"name": "test"}}

	err := r.createOrUpdateResource(context.Background(), same, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateOrUpdateResourceBackfillsLastAppliedForLegacyWorkspace(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setWorkspaceGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetAnnotations(map[string]string{"operator.example.com/defaulted": "true"})
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	existing.Object["resource"] = map[string]interface{}{"count": int64(1)}
	existing.Object["inference"] = map[string]interface{}{
		"preset": map[string]interface{}{
			"name":          "test",
			"accessMode":    "private",
			"presetOptions": []interface{}{"operator-default"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	desired := &unstructured.Unstructured{}
	setWorkspaceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")
	desired.Object["resource"] = map[string]interface{}{"count": int64(1)}
	desired.Object["inference"] = map[string]interface{}{
		"preset": map[string]interface{}{
			"name":       "test",
			"accessMode": "private",
		},
	}

	if err := r.createOrUpdateResource(context.Background(), desired, md); err != nil {
		t.Fatalf("unexpected error backfilling annotation: %v", err)
	}

	backfilled := &unstructured.Unstructured{}
	setWorkspaceGVK(backfilled)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, backfilled); err != nil {
		t.Fatalf("expected resource to exist: %v", err)
	}
	if backfilled.GetAnnotations()[lastAppliedWorkspaceAnnotation] == "" {
		t.Fatal("expected legacy Workspace to receive last-applied annotation")
	}
	if backfilled.GetAnnotations()["operator.example.com/defaulted"] != "true" {
		t.Fatalf("expected annotation-only backfill to preserve other annotations, got %v", backfilled.GetAnnotations())
	}
	presetOptions, found, _ := unstructured.NestedSlice(backfilled.Object, "inference", "preset", "presetOptions")
	if !found || len(presetOptions) != 1 || presetOptions[0] != "operator-default" {
		t.Fatalf("expected annotation-only backfill to preserve operator defaults, got %v (found=%v)", presetOptions, found)
	}
	_, backfilledInference, _, _ := lastAppliedManagedFields(backfilled)
	preset, ok := backfilledInference["preset"].(map[string]interface{})
	if !ok || preset["accessMode"] != "private" {
		t.Fatalf("expected backfilled last-applied annotation to track managed accessMode, got %v", backfilledInference)
	}

	desiredWithoutOverride := &unstructured.Unstructured{}
	setWorkspaceGVK(desiredWithoutOverride)
	desiredWithoutOverride.SetName("test")
	desiredWithoutOverride.SetNamespace("default")
	desiredWithoutOverride.Object["resource"] = map[string]interface{}{"count": int64(1)}
	desiredWithoutOverride.Object["inference"] = map[string]interface{}{
		"preset": map[string]interface{}{"name": "test"},
	}

	if err := r.createOrUpdateResource(context.Background(), desiredWithoutOverride, md); err != nil {
		t.Fatalf("unexpected error removing managed override after backfill: %v", err)
	}

	updated := &unstructured.Unstructured{}
	setWorkspaceGVK(updated)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, updated); err != nil {
		t.Fatalf("expected resource to exist: %v", err)
	}
	if _, found, _ := unstructured.NestedString(updated.Object, "inference", "preset", "accessMode"); found {
		t.Fatalf("expected deleted managed accessMode override to be removed after backfill, got %v", updated.Object["inference"])
	}
	presetOptions, found, _ = unstructured.NestedSlice(updated.Object, "inference", "preset", "presetOptions")
	if !found || len(presetOptions) != 1 || presetOptions[0] != "operator-default" {
		t.Fatalf("expected deletion update to preserve operator defaults, got %v (found=%v)", presetOptions, found)
	}
}

func TestCreateOrUpdateResourceRemovesStaleManagedLabel(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setWorkspaceGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	existingResource := map[string]interface{}{
		"count": int64(1),
		"labelSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{
				"kubernetes.io/os": "linux",
				"stale":            "true",
			},
		},
	}
	existing.Object["resource"] = existingResource
	setLastAppliedForTest(t, existing, existingResource, nil)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	desired := &unstructured.Unstructured{}
	setWorkspaceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")
	desired.Object["resource"] = map[string]interface{}{
		"count": int64(1),
		"labelSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{
				"kubernetes.io/os": "linux",
			},
		},
	}

	if err := r.createOrUpdateResource(context.Background(), desired, md); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &unstructured.Unstructured{}
	setWorkspaceGVK(updated)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, updated); err != nil {
		t.Fatalf("expected resource to exist: %v", err)
	}
	matchLabels, found, _ := unstructured.NestedStringMap(updated.Object, "resource", "labelSelector", "matchLabels")
	if !found {
		t.Fatal("expected matchLabels")
	}
	if _, ok := matchLabels["stale"]; ok {
		t.Fatalf("expected stale managed label to be removed, got %v", matchLabels)
	}
	if matchLabels["kubernetes.io/os"] != "linux" {
		t.Fatalf("expected kubernetes.io/os label to remain, got %v", matchLabels)
	}
}

func TestCreateOrUpdateResourceAppliesMetadataOnlyChanges(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setWorkspaceGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetLabels(map[string]string{
		"operator.example.com/defaulted": "true",
		"airunway.example.com/revision":  "old",
	})
	existing.SetAnnotations(map[string]string{
		"operator.example.com/defaulted": "true",
		"airunway.example.com/revision":  "old",
	})
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	existingResource := map[string]interface{}{"count": int64(1)}
	existingInference := map[string]interface{}{"preset": map[string]interface{}{"name": "test"}}
	existing.Object["resource"] = existingResource
	existing.Object["inference"] = existingInference
	setLastAppliedForTest(t, existing, existingResource, existingInference)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	desired := &unstructured.Unstructured{}
	setWorkspaceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")
	desired.SetLabels(map[string]string{"airunway.example.com/revision": "new"})
	desired.SetAnnotations(map[string]string{"airunway.example.com/revision": "new"})
	desired.Object["resource"] = map[string]interface{}{"count": int64(1)}
	desired.Object["inference"] = map[string]interface{}{"preset": map[string]interface{}{"name": "test"}}

	if err := r.createOrUpdateResource(context.Background(), desired, md); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &unstructured.Unstructured{}
	setWorkspaceGVK(updated)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, updated); err != nil {
		t.Fatalf("expected resource to exist: %v", err)
	}

	labels := updated.GetLabels()
	if labels["airunway.example.com/revision"] != "new" {
		t.Fatalf("expected desired label to be updated, got %v", labels)
	}
	if labels["operator.example.com/defaulted"] != "true" {
		t.Fatalf("expected unrelated operator label to remain, got %v", labels)
	}

	annotations := updated.GetAnnotations()
	if annotations["airunway.example.com/revision"] != "new" {
		t.Fatalf("expected desired annotation to be updated, got %v", annotations)
	}
	if annotations["operator.example.com/defaulted"] != "true" {
		t.Fatalf("expected unrelated operator annotation to remain, got %v", annotations)
	}
	if annotations[lastAppliedWorkspaceAnnotation] == "" {
		t.Fatalf("expected last-applied annotation to remain, got %v", annotations)
	}
}

func TestCreateOrUpdateResourceRemovesDeletedManagedMetadata(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setWorkspaceGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetLabels(map[string]string{
		"operator.example.com/defaulted": "true",
		"airunway.example.com/keep":      "true",
		"airunway.example.com/stale":     "true",
	})
	existing.SetAnnotations(map[string]string{
		"operator.example.com/defaulted": "true",
		"airunway.example.com/keep":      "true",
		"airunway.example.com/stale":     "true",
	})
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	existingResource := map[string]interface{}{"count": int64(1)}
	existingInference := map[string]interface{}{"preset": map[string]interface{}{"name": "test"}}
	existing.Object["resource"] = existingResource
	existing.Object["inference"] = existingInference
	setLastAppliedForTestWithMetadata(
		t,
		existing,
		existingResource,
		existingInference,
		map[string]string{
			"airunway.example.com/keep":  "true",
			"airunway.example.com/stale": "true",
		},
		map[string]string{
			"airunway.example.com/keep":  "true",
			"airunway.example.com/stale": "true",
		},
	)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	desired := &unstructured.Unstructured{}
	setWorkspaceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")
	desired.SetLabels(map[string]string{"airunway.example.com/keep": "true"})
	desired.SetAnnotations(map[string]string{"airunway.example.com/keep": "true"})
	desired.Object["resource"] = map[string]interface{}{"count": int64(1)}
	desired.Object["inference"] = map[string]interface{}{"preset": map[string]interface{}{"name": "test"}}

	if err := r.createOrUpdateResource(context.Background(), desired, md); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &unstructured.Unstructured{}
	setWorkspaceGVK(updated)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, updated); err != nil {
		t.Fatalf("expected resource to exist: %v", err)
	}

	labels := updated.GetLabels()
	if _, ok := labels["airunway.example.com/stale"]; ok {
		t.Fatalf("expected stale managed label to be removed, got %v", labels)
	}
	if labels["airunway.example.com/keep"] != "true" || labels["operator.example.com/defaulted"] != "true" {
		t.Fatalf("expected desired and operator labels to remain, got %v", labels)
	}

	annotations := updated.GetAnnotations()
	if _, ok := annotations["airunway.example.com/stale"]; ok {
		t.Fatalf("expected stale managed annotation to be removed, got %v", annotations)
	}
	if annotations["airunway.example.com/keep"] != "true" || annotations["operator.example.com/defaulted"] != "true" {
		t.Fatalf("expected desired and operator annotations to remain, got %v", annotations)
	}
	if annotations[lastAppliedWorkspaceAnnotation] == "" {
		t.Fatalf("expected last-applied annotation to remain, got %v", annotations)
	}

	_, _, lastAppliedLabels, lastAppliedAnnotations := lastAppliedManagedFields(updated)
	if _, ok := lastAppliedLabels["airunway.example.com/stale"]; ok {
		t.Fatalf("expected stale label to be removed from last-applied metadata, got %v", lastAppliedLabels)
	}
	if _, ok := lastAppliedAnnotations["airunway.example.com/stale"]; ok {
		t.Fatalf("expected stale annotation to be removed from last-applied metadata, got %v", lastAppliedAnnotations)
	}
}

func TestCreateOrUpdateResourceIgnoresUnmanagedOperatorDefaults(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setWorkspaceGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	existingResource := map[string]interface{}{"count": int64(1)}
	existing.Object["resource"] = existingResource
	existing.Object["inference"] = map[string]interface{}{
		"preset": map[string]interface{}{
			"name":          "test",
			"accessMode":    "public",
			"presetOptions": []interface{}{"operator-default"},
		},
	}
	setLastAppliedForTest(t, existing, existingResource, map[string]interface{}{
		"preset": map[string]interface{}{"name": "test"},
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	desired := &unstructured.Unstructured{}
	setWorkspaceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")
	desired.Object["resource"] = map[string]interface{}{"count": int64(1)}
	desired.Object["inference"] = map[string]interface{}{
		"preset": map[string]interface{}{"name": "test"},
	}

	if err := r.createOrUpdateResource(context.Background(), desired, md); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	unchanged := &unstructured.Unstructured{}
	setWorkspaceGVK(unchanged)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, unchanged); err != nil {
		t.Fatalf("expected resource to exist: %v", err)
	}
	accessMode, found, _ := unstructured.NestedString(unchanged.Object, "inference", "preset", "accessMode")
	if !found || accessMode != "public" {
		t.Fatalf("expected unmanaged operator default accessMode to remain, got %q (found=%v)", accessMode, found)
	}
	presetOptions, found, _ := unstructured.NestedSlice(unchanged.Object, "inference", "preset", "presetOptions")
	if !found || len(presetOptions) != 1 || presetOptions[0] != "operator-default" {
		t.Fatalf("expected unmanaged operator default presetOptions to remain, got %v (found=%v)", presetOptions, found)
	}
}

func TestCreateOrUpdateResourceRemovesDeletedManagedInferenceOverride(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setWorkspaceGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	existing.SetAnnotations(map[string]string{"operator.example.com/defaulted": "true"})
	existingResource := map[string]interface{}{"count": int64(1)}
	existingInference := map[string]interface{}{
		"preset": map[string]interface{}{
			"name":          "test",
			"accessMode":    "private",
			"presetOptions": []interface{}{"operator-default"},
		},
	}
	lastAppliedInference := map[string]interface{}{
		"preset": map[string]interface{}{
			"name":       "test",
			"accessMode": "private",
		},
	}
	existing.Object["resource"] = existingResource
	existing.Object["inference"] = existingInference
	setLastAppliedForTest(t, existing, existingResource, lastAppliedInference)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewKaitoProviderReconciler(c, scheme, c, record.NewFakeRecorder(10))

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	desired := &unstructured.Unstructured{}
	setWorkspaceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")
	desired.Object["resource"] = map[string]interface{}{"count": int64(1)}
	desired.Object["inference"] = map[string]interface{}{
		"preset": map[string]interface{}{"name": "test"},
	}

	if err := r.createOrUpdateResource(context.Background(), desired, md); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &unstructured.Unstructured{}
	setWorkspaceGVK(updated)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, updated); err != nil {
		t.Fatalf("expected resource to exist: %v", err)
	}
	if _, found, _ := unstructured.NestedString(updated.Object, "inference", "preset", "accessMode"); found {
		t.Fatalf("expected deleted managed accessMode override to be removed, got %v", updated.Object["inference"])
	}
	presetOptions, found, _ := unstructured.NestedSlice(updated.Object, "inference", "preset", "presetOptions")
	if !found || len(presetOptions) != 1 || presetOptions[0] != "operator-default" {
		t.Fatalf("expected unmanaged operator default presetOptions to remain, got %v (found=%v)", presetOptions, found)
	}
	if updated.GetAnnotations()["operator.example.com/defaulted"] != "true" {
		t.Fatalf("expected update to preserve operator annotations, got %v", updated.GetAnnotations())
	}
}

func TestSetLastAppliedManagedFieldsCopiesAnnotations(t *testing.T) {
	ws := &unstructured.Unstructured{}
	ws.SetName("test")
	original := map[string]string{
		"operator.example.com/defaulted": "true",
	}
	ws.SetAnnotations(original)

	if err := setLastAppliedManagedFields(ws); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := original[lastAppliedWorkspaceAnnotation]; ok {
		t.Fatalf("expected setLastAppliedManagedFields not to mutate caller annotation map, got %v", original)
	}
	if ws.GetAnnotations()[lastAppliedWorkspaceAnnotation] == "" {
		t.Fatalf("expected Workspace to receive last-applied annotation, got %v", ws.GetAnnotations())
	}

	original["operator.example.com/defaulted"] = "mutated"
	original["operator.example.com/new"] = "true"
	if ws.GetAnnotations()["operator.example.com/defaulted"] != "true" {
		t.Fatalf("expected Workspace annotations to be isolated from caller map mutations, got %v", ws.GetAnnotations())
	}
	if _, ok := ws.GetAnnotations()["operator.example.com/new"]; ok {
		t.Fatalf("expected Workspace annotations not to alias caller map, got %v", ws.GetAnnotations())
	}
}

func TestManagedFieldsMatchSliceBehavior(t *testing.T) {
	desired := map[string]interface{}{
		"preset": map[string]interface{}{
			"name":          "test",
			"presetOptions": []interface{}{"desired"},
		},
	}
	existingSame := map[string]interface{}{
		"preset": map[string]interface{}{
			"name":          "test",
			"presetOptions": []interface{}{"desired"},
		},
	}
	if !managedFieldsMatch(desired, existingSame, desired, "inference") {
		t.Fatal("expected identical managed slice fields to match")
	}

	existingWithExtraSliceItem := map[string]interface{}{
		"preset": map[string]interface{}{
			"name":          "test",
			"presetOptions": []interface{}{"desired", "operator-default"},
		},
	}
	if managedFieldsMatch(desired, existingWithExtraSliceItem, desired, "inference") {
		t.Fatal("expected managed slice values to require exact semantic equality")
	}

	desiredWithoutSlice := map[string]interface{}{
		"preset": map[string]interface{}{
			"name": "test",
		},
	}
	lastAppliedWithoutSlice := map[string]interface{}{
		"preset": map[string]interface{}{
			"name": "test",
		},
	}
	existingWithUnmanagedDefaultedSlice := map[string]interface{}{
		"preset": map[string]interface{}{
			"name":          "test",
			"presetOptions": []interface{}{"operator-default"},
		},
	}
	if !managedFieldsMatch(desiredWithoutSlice, existingWithUnmanagedDefaultedSlice, lastAppliedWithoutSlice, "inference") {
		t.Fatal("expected unmanaged operator-defaulted slice field to be ignored")
	}

	lastAppliedWithSlice := map[string]interface{}{
		"preset": map[string]interface{}{
			"name":          "test",
			"presetOptions": []interface{}{"old-managed"},
		},
	}
	existingWithDeletedManagedSlice := map[string]interface{}{
		"preset": map[string]interface{}{
			"name":          "test",
			"presetOptions": []interface{}{"old-managed"},
		},
	}
	if managedFieldsMatch(desiredWithoutSlice, existingWithDeletedManagedSlice, lastAppliedWithSlice, "inference") {
		t.Fatal("expected a previously managed slice field to require deletion when removed from desired state")
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
