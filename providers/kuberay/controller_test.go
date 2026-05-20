package kuberay

import (
	"context"
	"testing"
	"time"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(s)
	return s
}

func newMDForController(name, ns string) *airunwayv1alpha1.ModelDeployment {
	return &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model:     airunwayv1alpha1.ModelSpec{ID: "test-model", Source: airunwayv1alpha1.ModelSourceHuggingFace},
			Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}},
		},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Provider: &airunwayv1alpha1.ProviderStatus{Name: ProviderName},
		},
	}
}

func setRayServiceGVK(u *unstructured.Unstructured) {
	u.SetAPIVersion("ray.io/v1")
	u.SetKind("RayService")
}

func TestValidateCompatibility(t *testing.T) {
	r := &KubeRayProviderReconciler{}

	tests := []struct {
		name    string
		md      *airunwayv1alpha1.ModelDeployment
		wantErr bool
		errMsg  string
	}{
		{
			name: "vllm with GPU is compatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}},
				},
			},
			wantErr: false,
		},
		{
			name: "sglang is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeSGLang},
					Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}},
				},
			},
			wantErr: true,
			errMsg:  "KubeRay only supports vllm engine, got sglang",
		},
		{
			name: "llamacpp is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeLlamaCpp},
					Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}},
				},
			},
			wantErr: true,
			errMsg:  "KubeRay only supports vllm engine, got llamacpp",
		},
		{
			name: "trtllm is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeTRTLLM},
					Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}},
				},
			},
			wantErr: true,
			errMsg:  "KubeRay only supports vllm engine, got trtllm",
		},
		{
			name: "no GPU is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
				},
			},
			wantErr: true,
			errMsg:  "KubeRay requires GPU (set resources.gpu.count > 0)",
		},
		{
			name: "disaggregated with prefill GPU is compatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Serving: &airunwayv1alpha1.ServingSpec{
						Mode: airunwayv1alpha1.ServingModeDisaggregated,
					},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{
							GPU: &airunwayv1alpha1.GPUSpec{Count: 2},
						},
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
	r := &KubeRayProviderReconciler{}
	md := &airunwayv1alpha1.ModelDeployment{}

	r.setCondition(md, "TestCondition", "True", "TestReason", "test message")
	if len(md.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(md.Status.Conditions))
	}

	r.setCondition(md, "TestCondition", "False", "Updated", "updated")
	if len(md.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(md.Status.Conditions))
	}
}

func TestNewKubeRayProviderReconciler(t *testing.T) {
	r := NewKubeRayProviderReconciler(nil, nil)
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
	if ProviderName != "kuberay" {
		t.Errorf("expected provider name 'kuberay', got %s", ProviderName)
	}
	if FinalizerName != "airunway.ai/kuberay-provider" {
		t.Errorf("expected finalizer 'airunway.ai/kuberay-provider', got %s", FinalizerName)
	}
}

func TestKubeRayProviderPredicateAllowsProviderConfigEvents(t *testing.T) {
	if !kubeRayProviderPredicate(&airunwayv1alpha1.InferenceProviderConfig{}) {
		t.Fatal("expected provider predicate to allow InferenceProviderConfig events")
	}
}

func TestMapProviderConfigToModelDeployments(t *testing.T) {
	scheme := newScheme()
	selected := newMDForController("selected", "default")
	pinned := newMDForController("pinned", "default")
	pinned.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: ProviderName}
	pinned.Status.Provider = nil
	other := newMDForController("other", "default")
	other.Status.Provider.Name = "other"

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(selected, pinned, other).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	requests := r.mapProviderConfigToModelDeployments(context.Background(), &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: ProviderName},
	})

	got := make(map[string]struct{}, len(requests))
	for _, request := range requests {
		got[request.Namespace+"/"+request.Name] = struct{}{}
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 requests, got %d: %#v", len(got), got)
	}
	if _, ok := got["default/selected"]; !ok {
		t.Fatalf("expected selected deployment to be requeued, got %#v", got)
	}
	if _, ok := got["default/pinned"]; !ok {
		t.Fatalf("expected pinned deployment to be requeued, got %#v", got)
	}
	if _, ok := got["default/other"]; ok {
		t.Fatalf("did not expect unrelated deployment to be requeued, got %#v", got)
	}
}

func TestReconcileNotFound(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

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
	md.Status.Provider.Name = "other"

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

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
	r := NewKubeRayProviderReconciler(c, scheme)

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
	r := NewKubeRayProviderReconciler(c, scheme)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("should requeue after adding finalizer")
	}

	var updated airunwayv1alpha1.ModelDeployment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
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
	r := NewKubeRayProviderReconciler(c, scheme)

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
	r := NewKubeRayProviderReconciler(c, scheme)

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
	r := NewKubeRayProviderReconciler(c, scheme)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != RequeueInterval {
		t.Errorf("expected requeue after %v, got %v", RequeueInterval, result.RequeueAfter)
	}

	rs := &unstructured.Unstructured{}
	setRayServiceGVK(rs)
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, rs)
	if err != nil {
		t.Fatalf("expected RayService to be created: %v", err)
	}
}

func TestReconcileHandleDeletion(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated airunwayv1alpha1.ModelDeployment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	if controllerutil.ContainsFinalizer(&updated, FinalizerName) {
		t.Error("expected finalizer to be removed")
	}
}

func TestReconcileDeletionWithUpstreamResource(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.UID = "test-uid"
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	rs := &unstructured.Unstructured{}
	setRayServiceGVK(rs)
	rs.SetName("test")
	rs.SetNamespace("default")
	rs.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, rs).WithStatusSubresource(md).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s, got %v", result.RequeueAfter)
	}
}

func TestReconcileDeletionWithMissingUpstreamCRDRemovesFinalizer(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	interceptorFuncs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == RayServiceKind {
				return &apimeta.NoKindMatchError{
					GroupKind:        schema.GroupKind{Group: RayAPIGroup, Kind: RayServiceKind},
					SearchedVersions: []string{RayAPIVersion},
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
	r := NewKubeRayProviderReconciler(c, scheme)

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

func TestReconcileDeletionNoFinalizer(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	now := metav1.Now()
	md.DeletionTimestamp = &now
	md.Finalizers = []string{"other-finalizer"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

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

func TestCreateOrUpdateResourceNew(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	rs := &unstructured.Unstructured{}
	setRayServiceGVK(rs)
	rs.SetName("test")
	rs.SetNamespace("default")
	rs.Object["spec"] = map[string]interface{}{"serveConfigV2": "test"}

	err := r.createOrUpdateResource(context.Background(), rs, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateOrUpdateResourceUpdate(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setRayServiceGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	existing.Object["spec"] = map[string]interface{}{"serveConfigV2": "old"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	updated := &unstructured.Unstructured{}
	setRayServiceGVK(updated)
	updated.SetName("test")
	updated.SetNamespace("default")
	updated.Object["spec"] = map[string]interface{}{"serveConfigV2": "new"}

	err := r.createOrUpdateResource(context.Background(), updated, md)
	if err != nil {
		t.Fatalf("unexpected error updating resource: %v", err)
	}
}

func TestCreateOrUpdateResourceNoChange(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setRayServiceGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{UID: "test-uid", APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test"},
	})
	existing.Object["spec"] = map[string]interface{}{"serveConfigV2": "same"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	same := &unstructured.Unstructured{}
	setRayServiceGVK(same)
	same.SetName("test")
	same.SetNamespace("default")
	same.Object["spec"] = map[string]interface{}{"serveConfigV2": "same"}

	err := r.createOrUpdateResource(context.Background(), same, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSyncStatusNotFound(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setRayServiceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")

	err := r.syncStatus(context.Background(), md, desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSyncStatusRunning(t *testing.T) {
	scheme := newScheme()

	rs := &unstructured.Unstructured{}
	setRayServiceGVK(rs)
	rs.SetName("test")
	rs.SetNamespace("default")
	rs.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{"type": "RayServiceReady", "status": "True"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setRayServiceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")

	err := r.syncStatus(context.Background(), md, desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running, got %s", md.Status.Phase)
	}
}

func TestSyncStatusFailed(t *testing.T) {
	scheme := newScheme()

	rs := &unstructured.Unstructured{}
	setRayServiceGVK(rs)
	rs.SetName("test")
	rs.SetNamespace("default")
	rs.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{"type": "RayServiceReady", "status": "False", "reason": "Failed"},
		},
		"activeServiceStatus": map[string]interface{}{
			"applicationStatuses": map[string]interface{}{
				"serve": map[string]interface{}{"status": "DEPLOY_FAILED", "message": "oom"},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setRayServiceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")

	err := r.syncStatus(context.Background(), md, desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed, got %s", md.Status.Phase)
	}
}

func TestSyncStatusDeploying(t *testing.T) {
	scheme := newScheme()

	rs := &unstructured.Unstructured{}
	setRayServiceGVK(rs)
	rs.SetName("test")
	rs.SetNamespace("default")
	rs.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{"type": "RayServiceReady", "status": "False", "reason": "WaitingForServeDeploymentReady"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).Build()
	r := NewKubeRayProviderReconciler(c, scheme)

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setRayServiceGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")

	err := r.syncStatus(context.Background(), md, desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying, got %s", md.Status.Phase)
	}
}
