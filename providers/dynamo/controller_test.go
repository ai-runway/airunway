package dynamo

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func newMDForController(name, ns string) *airunwayv1alpha1.ModelDeployment {
	return &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       types.UID("test-uid"),
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

func setDGDGVK(u *unstructured.Unstructured) {
	u.SetAPIVersion("nvidia.com/v1alpha1")
	u.SetKind("DynamoGraphDeployment")
}

func assertCondition(t *testing.T, conditions []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	cond := meta.FindStatusCondition(conditions, condType)
	if cond == nil {
		t.Errorf("expected condition %s to be set", condType)
		return
	}
	if cond.Status != status {
		t.Errorf("condition %s: expected status %s, got %s", condType, status, cond.Status)
	}
	if cond.Reason != reason {
		t.Errorf("condition %s: expected reason %q, got %q", condType, reason, cond.Reason)
	}
}

func TestValidateCompatibility(t *testing.T) {
	r := &DynamoProviderReconciler{}

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
			name: "sglang with GPU is compatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeSGLang},
					Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}},
				},
			},
			wantErr: false,
		},
		{
			name: "trtllm with GPU is compatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeTRTLLM},
					Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}},
				},
			},
			wantErr: false,
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
			errMsg:  "Dynamo does not support llamacpp engine",
		},
		{
			name: "no GPU is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
				},
			},
			wantErr: true,
			errMsg:  "Dynamo requires GPU (set resources.gpu.count > 0)",
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
		{
			name: "disaggregated without GPU is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Serving: &airunwayv1alpha1.ServingSpec{
						Mode: airunwayv1alpha1.ServingModeDisaggregated,
					},
					Scaling: &airunwayv1alpha1.ScalingSpec{},
				},
			},
			wantErr: true,
			errMsg:  "Dynamo requires GPU (set resources.gpu.count > 0)",
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
	r := &DynamoProviderReconciler{}
	md := &airunwayv1alpha1.ModelDeployment{}

	r.setCondition(md, "TestCondition", "True", "TestReason", "test message")
	if len(md.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(md.Status.Conditions))
	}

	r.setCondition(md, "TestCondition", "False", "Updated", "updated")
	if len(md.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(md.Status.Conditions))
	}
	if string(md.Status.Conditions[0].Status) != "False" {
		t.Errorf("expected False after update")
	}
}

func TestNewDynamoProviderReconciler(t *testing.T) {
	r := NewDynamoProviderReconciler(nil, nil, "")
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
	if ProviderName != "dynamo" {
		t.Errorf("expected provider name 'dynamo', got %s", ProviderName)
	}
	if FinalizerName != "airunway.ai/dynamo-provider" {
		t.Errorf("expected finalizer 'airunway.ai/dynamo-provider', got %s", FinalizerName)
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
	r := NewDynamoProviderReconciler(c, scheme, "")

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
	r := NewDynamoProviderReconciler(c, scheme, "")

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
	r := NewDynamoProviderReconciler(c, scheme, "")

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
	r := NewDynamoProviderReconciler(c, scheme, "")

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
	r := NewDynamoProviderReconciler(c, scheme, "")

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
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeLlamaCpp
	controllerutil.AddFinalizer(md, FinalizerName)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

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
	r := NewDynamoProviderReconciler(c, scheme, "")

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
	r := NewDynamoProviderReconciler(c, scheme, "")

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != RequeueInterval {
		t.Errorf("expected requeue after %v, got %v", RequeueInterval, result.RequeueAfter)
	}

	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, dgd)
	if err != nil {
		t.Fatalf("expected DynamoGraphDeployment to be created: %v", err)
	}
}

func TestReconcileHandleDeletion(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

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

func TestReconcileDeletionNoFinalizer(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	now := metav1.Now()
	md.DeletionTimestamp = &now
	md.Finalizers = []string{"other-finalizer"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

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
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	dgd.SetName("test")
	dgd.SetNamespace("default")
	dgd.SetLabels(map[string]string{
		airunwayv1alpha1.LabelManagedBy: "airunway",
	})
	dgd.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test", UID: "test-uid"},
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, dgd).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

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

func TestReconcileDeletionWithMissingUpstreamCRDCleansUpManagedResources(t *testing.T) {
	scheme := newScheme()
	md := newMDWithStorage("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-cache",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "test",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "test",
					UID:        "test-uid",
				},
			},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-download",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "test",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "test",
					UID:        "test-uid",
				},
			},
		},
	}

	interceptorFuncs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == DynamoGraphDeploymentKind {
				return &meta.NoKindMatchError{
					GroupKind:        schema.GroupKind{Group: DynamoAPIGroup, Kind: DynamoGraphDeploymentKind},
					SearchedVersions: []string{DynamoAPIVersion},
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, pvc, job).
		WithStatusSubresource(md).
		WithInterceptorFuncs(interceptorFuncs).
		Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("expected cleanup to finish without requeue, got %#v", result)
	}

	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); !apierrors.IsNotFound(err) {
		t.Fatalf("expected ModelDeployment to be deleted after finalizer removal, got %v", err)
	}

	pvcCheck := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-model-cache", Namespace: "default"}, pvcCheck); err == nil {
		t.Fatal("expected managed PVC to be deleted when upstream CRD is missing")
	}

	jobCheck := &batchv1.Job{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-model-download", Namespace: "default"}, jobCheck); err == nil {
		t.Fatal("expected managed Job to be deleted when upstream CRD is missing")
	}
}

func TestCreateOrUpdateResourceNew(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"

	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	dgd.SetName("test")
	dgd.SetNamespace("default")
	dgd.Object["spec"] = map[string]interface{}{"backendFramework": "vllm"}

	err := r.createOrUpdateResource(context.Background(), dgd, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateOrUpdateResourceUpdate(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setDGDGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetLabels(map[string]string{
		airunwayv1alpha1.LabelManagedBy: "airunway",
	})
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test", UID: "test-uid"},
	})
	existing.Object["spec"] = map[string]interface{}{"backendFramework": "vllm"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	updated := &unstructured.Unstructured{}
	setDGDGVK(updated)
	updated.SetName("test")
	updated.SetNamespace("default")
	updated.Object["spec"] = map[string]interface{}{"backendFramework": "sglang"}

	err := r.createOrUpdateResource(context.Background(), updated, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateOrUpdateResourceNoChange(t *testing.T) {
	scheme := newScheme()

	existing := &unstructured.Unstructured{}
	setDGDGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetLabels(map[string]string{
		airunwayv1alpha1.LabelManagedBy: "airunway",
	})
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test", UID: "test-uid"},
	})
	existing.Object["spec"] = map[string]interface{}{"backendFramework": "vllm"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Name = "test"
	md.Namespace = "default"
	md.UID = "test-uid"

	same := &unstructured.Unstructured{}
	setDGDGVK(same)
	same.SetName("test")
	same.SetNamespace("default")
	same.Object["spec"] = map[string]interface{}{"backendFramework": "vllm"}

	err := r.createOrUpdateResource(context.Background(), same, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSyncStatusNotFound(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setDGDGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")

	err := r.syncStatus(context.Background(), md, desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSyncStatusRunning(t *testing.T) {
	scheme := newScheme()

	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	dgd.SetName("test")
	dgd.SetNamespace("default")
	dgd.Object["status"] = map[string]interface{}{"state": "successful"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dgd).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Status.Message = "DynamoGraphDeployment created, waiting for pods to be ready"
	desired := &unstructured.Unstructured{}
	setDGDGVK(desired)
	desired.SetName("test")
	desired.SetNamespace("default")

	err := r.syncStatus(context.Background(), md, desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", md.Status.Phase)
	}
	// Issue #289: a Running deployment must not keep the "waiting for pods" message.
	if strings.Contains(md.Status.Message, "waiting for pods") {
		t.Errorf("status message still claims waiting for pods while Running: %q", md.Status.Message)
	}
	if md.Status.Message == "" {
		t.Errorf("expected a non-empty status message in Running phase")
	}
}

func TestSyncStatusFailed(t *testing.T) {
	scheme := newScheme()

	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	dgd.SetName("test")
	dgd.SetNamespace("default")
	dgd.Object["status"] = map[string]interface{}{"state": "failed", "message": "oom"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dgd).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setDGDGVK(desired)
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

	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	dgd.SetName("test")
	dgd.SetNamespace("default")
	dgd.Object["status"] = map[string]interface{}{"state": "deploying"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dgd).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	md := &airunwayv1alpha1.ModelDeployment{}
	desired := &unstructured.Unstructured{}
	setDGDGVK(desired)
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

// --- 3-phase orchestration tests ---

func newMDWithStorage(name, ns string) *airunwayv1alpha1.ModelDeployment {
	size := resource.MustParse("100Gi")
	return &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID:     "meta-llama/Llama-2-7b-chat-hf",
				Source: airunwayv1alpha1.ModelSourceHuggingFace,
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:       "model-cache",
							MountPath:  "/model-cache",
							Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
							Size:       &size,
							AccessMode: corev1.ReadWriteMany,
						},
					},
				},
			},
			Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}},
		},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Provider: &airunwayv1alpha1.ProviderStatus{Name: ProviderName},
		},
	}
}

func TestReconcilePVCNotBound(t *testing.T) {
	scheme := newScheme()
	md := newMDWithStorage("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should requeue (PVC was just created, not yet Bound)
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("expected requeue after 10s, got %v", result.RequeueAfter)
	}

	// Verify PVC was created
	pvc := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-model-cache", Namespace: "default"}, pvc)
	if err != nil {
		t.Fatalf("expected PVC to be created: %v", err)
	}

	// Verify DGD was NOT created (should not proceed past PVC phase)
	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, dgd)
	if err == nil {
		t.Error("DGD should NOT be created before PVCs are bound")
	}

	// Verify conditions were set correctly
	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get updated MD: %v", err)
	}
	assertCondition(t, updated.Status.Conditions, airunwayv1alpha1.ConditionTypeStorageReady, metav1.ConditionFalse, "PVCsPending")
	// ModelDownloaded should NOT be set (download phase was never reached)
	if meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeModelDownloaded) != nil {
		t.Error("expected ModelDownloaded condition to NOT be set (download phase not reached)")
	}
}

func TestReconcileTerminatingPVCPrioritizesOwnedConsumerTeardown(t *testing.T) {
	scheme := newScheme()
	md := newMDWithStorage("terminating-dynamo", "dynamo-storage-recovery")
	controllerutil.AddFinalizer(md, FinalizerName)
	// The old workload still has to release its PVC even though the updated
	// desired spec is no longer Dynamo-compatible.
	md.Spec.Resources = nil
	modelCache := md.Spec.Model.Storage.Volumes[0]
	md.Spec.Model.Storage.Volumes = []airunwayv1alpha1.StorageVolume{{
		Name:      "missing-data",
		ClaimName: "missing-data-pvc",
		MountPath: "/data",
		Purpose:   airunwayv1alpha1.VolumePurposeCustom,
	}, modelCache}
	now := metav1.Now()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              md.Name + "-model-cache",
			Namespace:         md.Namespace,
			DeletionTimestamp: &now,
			Finalizers:        []string{"kubernetes.io/pvc-protection"},
			OwnerReferences:   []metav1.OwnerReference{{UID: md.UID}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	dgd.SetName(md.Name)
	dgd.SetNamespace(md.Namespace)
	dgd.SetOwnerReferences([]metav1.OwnerReference{{UID: md.UID}})
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      md.Name + "-model-download",
		Namespace: md.Namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: airunwayv1alpha1.GroupVersion.String(),
			Kind:       "ModelDeployment",
			Name:       md.Name,
			UID:        md.UID,
		}},
	}}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, pvc, dgd, job).
		WithStatusSubresource(md, pvc, &batchv1.Job{}).
		Build()
	r := NewDynamoProviderReconciler(c, scheme, testModelDownloaderImage)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: md.Name, Namespace: md.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("expected storage recovery requeue after 5s, got %v", result.RequeueAfter)
	}
	remaining := &unstructured.Unstructured{}
	setDGDGVK(remaining)
	err = c.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, remaining)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected owned DynamoGraphDeployment deletion, got %v", err)
	}
	remainingJob := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, remainingJob)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected owned downloader Job deletion, got %v", err)
	}
	updated := &airunwayv1alpha1.ModelDeployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, updated); err != nil {
		t.Fatalf("getting updated ModelDeployment: %v", err)
	}
	assertCondition(
		t,
		updated.Status.Conditions,
		airunwayv1alpha1.ConditionTypeStorageReady,
		metav1.ConditionFalse,
		"PVCsTerminating",
	)
	assertCondition(
		t,
		updated.Status.Conditions,
		airunwayv1alpha1.ConditionTypeReady,
		metav1.ConditionFalse,
		"StorageUnavailable",
	)
	if updated.Status.Endpoint != nil || updated.Status.Replicas != nil {
		t.Fatalf("expected stale serving status to be cleared, got endpoint=%+v replicas=%+v", updated.Status.Endpoint, updated.Status.Replicas)
	}
	if compatible := meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeProviderCompatible); compatible != nil {
		t.Fatalf("compatibility validation must wait for PVC consumer teardown, got %#v", compatible)
	}
}

func TestReconcileTerminatingPVCReportsConsumerTeardownFailure(t *testing.T) {
	tests := []struct {
		name          string
		consumerKind  string
		messagePrefix string
	}{
		{
			name:          "model downloader",
			consumerKind:  "Job",
			messagePrefix: "Failed to stop model downloader for terminating PVC",
		},
		{
			name:          "Dynamo workload",
			consumerKind:  DynamoGraphDeploymentKind,
			messagePrefix: "Failed to stop Dynamo workload for terminating PVC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newScheme()
			md := newMDWithStorage("terminating-dynamo", "dynamo-storage-recovery")
			controllerutil.AddFinalizer(md, FinalizerName)
			now := metav1.Now()
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              md.Name + "-model-cache",
					Namespace:         md.Namespace,
					DeletionTimestamp: &now,
					Finalizers:        []string{"kubernetes.io/pvc-protection"},
					OwnerReferences:   []metav1.OwnerReference{{UID: md.UID}},
				},
				Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
			}

			objects := []client.Object{md, pvc}
			switch tt.consumerKind {
			case "Job":
				objects = append(objects, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
					Name:      md.Name + "-model-download",
					Namespace: md.Namespace,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: airunwayv1alpha1.GroupVersion.String(),
						Kind:       "ModelDeployment",
						Name:       md.Name,
						UID:        md.UID,
					}},
				}})
			case DynamoGraphDeploymentKind:
				dgd := &unstructured.Unstructured{}
				setDGDGVK(dgd)
				dgd.SetName(md.Name)
				dgd.SetNamespace(md.Namespace)
				dgd.SetOwnerReferences([]metav1.OwnerReference{{UID: md.UID}})
				objects = append(objects, dgd)
			}

			interceptorFuncs := interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if tt.consumerKind == "Job" {
						if _, ok := obj.(*batchv1.Job); ok {
							return fmt.Errorf("simulated %s deletion failure", tt.name)
						}
					}
					if workload, ok := obj.(*unstructured.Unstructured); ok && workload.GetKind() == tt.consumerKind {
						return fmt.Errorf("simulated %s deletion failure", tt.name)
					}
					return c.Delete(ctx, obj, opts...)
				},
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				WithStatusSubresource(md, pvc, &batchv1.Job{}).
				WithInterceptorFuncs(interceptorFuncs).
				Build()
			r := NewDynamoProviderReconciler(c, scheme, testModelDownloaderImage)

			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: md.Name, Namespace: md.Namespace},
			})
			if err != nil {
				t.Fatalf("unexpected reconciliation error: %v", err)
			}
			if result != (ctrl.Result{}) {
				t.Fatalf("expected teardown failure to rely on a watched status update, got %+v", result)
			}

			updated := &airunwayv1alpha1.ModelDeployment{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, updated); err != nil {
				t.Fatalf("getting updated ModelDeployment: %v", err)
			}
			assertCondition(
				t,
				updated.Status.Conditions,
				airunwayv1alpha1.ConditionTypeStorageReady,
				metav1.ConditionFalse,
				"PVCConsumerTeardownFailed",
			)
			if updated.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
				t.Fatalf("expected Failed phase, got %s", updated.Status.Phase)
			}
			if !strings.Contains(updated.Status.Message, tt.messagePrefix) ||
				!strings.Contains(updated.Status.Message, "simulated "+tt.name+" deletion failure") {
				t.Fatalf("unexpected failure message: %q", updated.Status.Message)
			}
		})
	}
}

const testModelDownloaderImage = "example.test/model-downloader:v1"

func TestReconcileDownloadNotComplete(t *testing.T) {
	scheme := newScheme()
	md := newMDWithStorage("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)

	// Pre-create a bound PVC so we pass Phase 1
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-cache",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "test",
					UID:        "test-uid",
				},
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, pvc).WithStatusSubresource(md, pvc).Build()
	r := NewDynamoProviderReconciler(c, scheme, testModelDownloaderImage)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should requeue (download Job was just created, not yet complete)
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected requeue after 15s, got %v", result.RequeueAfter)
	}

	// Verify download Job was created
	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-model-download", Namespace: "default"}, job)
	if err != nil {
		t.Fatalf("expected download Job to be created: %v", err)
	}

	// Verify DGD was NOT created
	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, dgd)
	if err == nil {
		t.Error("DGD should NOT be created before download completes")
	}

	// Verify conditions were set correctly
	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get updated MD: %v", err)
	}
	assertCondition(t, updated.Status.Conditions, airunwayv1alpha1.ConditionTypeStorageReady, metav1.ConditionTrue, "PVCsBound")
	assertCondition(t, updated.Status.Conditions, airunwayv1alpha1.ConditionTypeModelDownloaded, metav1.ConditionFalse, "DownloadInProgress")
}

func TestReconcileFullPipeline(t *testing.T) {
	scheme := newScheme()
	md := newMDWithStorage("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)

	// Pre-create bound PVC
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-cache",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "test",
					UID:        "test-uid",
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, pvc).
		WithStatusSubresource(md, pvc, &batchv1.Job{}).
		Build()
	r := NewDynamoProviderReconciler(c, scheme, testModelDownloaderImage)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("creating download Job: %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Fatalf("expected initial download requeue after 15s, got %v", result.RequeueAfter)
	}
	job := &batchv1.Job{}
	jobKey := types.NamespacedName{Name: "test-model-download", Namespace: "default"}
	if err := c.Get(context.Background(), jobKey, job); err != nil {
		t.Fatalf("getting generated download Job: %v", err)
	}
	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("completing generated download Job: %v", err)
	}

	result, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should complete and requeue for periodic sync
	if result.RequeueAfter != RequeueInterval {
		t.Errorf("expected requeue after %v, got %v", RequeueInterval, result.RequeueAfter)
	}

	// Verify DGD was created
	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, dgd)
	if err != nil {
		t.Fatalf("expected DGD to be created: %v", err)
	}

	// Verify conditions were set correctly
	var updated airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get updated MD: %v", err)
	}
	assertCondition(t, updated.Status.Conditions, airunwayv1alpha1.ConditionTypeStorageReady, metav1.ConditionTrue, "PVCsBound")
	assertCondition(t, updated.Status.Conditions, airunwayv1alpha1.ConditionTypeModelDownloaded, metav1.ConditionTrue, "DownloadComplete")
}

func TestReconcileNoStorageSkipsPhases(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	md.Status.Conditions = []metav1.Condition{
		{Type: airunwayv1alpha1.ConditionTypeStorageReady, Status: metav1.ConditionTrue},
		{Type: airunwayv1alpha1.ConditionTypeModelDownloaded, Status: metav1.ConditionTrue},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should go straight to DGD creation (no PVC or download phases)
	if result.RequeueAfter != RequeueInterval {
		t.Errorf("expected requeue after %v, got %v", RequeueInterval, result.RequeueAfter)
	}

	// Verify DGD was created
	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, dgd)
	if err != nil {
		t.Fatalf("expected DGD to be created: %v", err)
	}
	updated := &airunwayv1alpha1.ModelDeployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, updated); err != nil {
		t.Fatalf("getting updated ModelDeployment: %v", err)
	}
	if meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeStorageReady) != nil {
		t.Fatal("expected stale StorageReady condition to be removed")
	}
	if meta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeModelDownloaded) != nil {
		t.Fatal("expected stale ModelDownloaded condition to be removed")
	}
}

func TestReconcileDeletionCleansUpResources(t *testing.T) {
	scheme := newScheme()
	md := newMDWithStorage("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	// Create managed PVC with OwnerReference matching the ModelDeployment UID
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-cache",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "test",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "test",
					UID:        "test-uid",
				},
			},
		},
	}

	// Create managed Job with OwnerReference matching the ModelDeployment UID
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-download",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "test",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "test",
					UID:        "test-uid",
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, pvc, job).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify managed PVC was deleted
	pvcCheck := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-model-cache", Namespace: "default"}, pvcCheck)
	if err == nil {
		t.Error("expected managed PVC to be deleted during cleanup")
	}

	// Verify managed Job was deleted
	jobCheck := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-model-download", Namespace: "default"}, jobCheck)
	if err == nil {
		t.Error("expected managed Job to be deleted during cleanup")
	}
}

func TestReconcileDeletionRetriesOnCleanupFailure(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	// Create a managed Job that will fail to delete
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-download",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "test",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "test",
					UID:        "test-uid",
				},
			},
		},
	}

	// Intercept Delete calls for Job resources to simulate API server failure
	interceptorFuncs := interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return fmt.Errorf("simulated API server error")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, job).
		WithStatusSubresource(md).
		WithInterceptorFuncs(interceptorFuncs).
		Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should requeue after 10s due to cleanup failure (not immediate requeue)
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("expected requeue after 10s, got %v", result.RequeueAfter)
	}

	// Verify the finalizer is still present (cleanup failure should prevent removal)
	var updated airunwayv1alpha1.ModelDeployment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	if !controllerutil.ContainsFinalizer(&updated, FinalizerName) {
		t.Error("expected finalizer to still be present after cleanup failure")
	}
}

func TestReconcileDeletionWithDGDDelaysCleanup(t *testing.T) {
	scheme := newScheme()
	md := newMDWithStorage("test", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	// Create a DGD owned by this ModelDeployment
	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	dgd.SetName("test")
	dgd.SetNamespace("default")
	dgd.SetLabels(map[string]string{
		airunwayv1alpha1.LabelManagedBy: "airunway",
	})
	dgd.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "test", UID: "test-uid"},
	})

	// Create managed PVC with OwnerReference matching the ModelDeployment UID
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-cache",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "test",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "test",
					UID:        "test-uid",
				},
			},
		},
	}

	// Create managed Job with OwnerReference matching the ModelDeployment UID
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-download",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "test",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "test",
					UID:        "test-uid",
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, dgd, pvc, job).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")

	// --- First reconciliation: DGD exists, should delete DGD but NOT PVC/Job ---
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error on first reconcile: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected requeue after 5s on first reconcile, got %v", result.RequeueAfter)
	}

	// Verify DGD was deleted
	dgdCheck := &unstructured.Unstructured{}
	setDGDGVK(dgdCheck)
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, dgdCheck)
	if err == nil {
		t.Error("expected DGD to be deleted after first reconcile")
	}

	// Verify PVC still exists (cleanup deferred until DGD is gone)
	pvcCheck := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-model-cache", Namespace: "default"}, pvcCheck)
	if err != nil {
		t.Errorf("expected PVC to still exist after first reconcile: %v", err)
	}

	// Verify Job still exists (cleanup deferred until DGD is gone)
	jobCheck := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-model-download", Namespace: "default"}, jobCheck)
	if err != nil {
		t.Errorf("expected Job to still exist after first reconcile: %v", err)
	}

	// Verify finalizer still present
	var mdAfterFirst airunwayv1alpha1.ModelDeployment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &mdAfterFirst)
	if !controllerutil.ContainsFinalizer(&mdAfterFirst, FinalizerName) {
		t.Error("expected finalizer to still be present after first reconcile")
	}

	// --- Second reconciliation: DGD is gone, should clean up PVC/Job and remove finalizer ---
	result, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error on second reconcile: %v", err)
	}

	// Verify PVC was deleted
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-model-cache", Namespace: "default"}, pvcCheck)
	if err == nil {
		t.Error("expected PVC to be deleted after second reconcile")
	}

	// Verify Job was deleted
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-model-download", Namespace: "default"}, jobCheck)
	if err == nil {
		t.Error("expected Job to be deleted after second reconcile")
	}

	// Verify finalizer was removed
	var mdAfterSecond airunwayv1alpha1.ModelDeployment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &mdAfterSecond)
	if controllerutil.ContainsFinalizer(&mdAfterSecond, FinalizerName) {
		t.Error("expected finalizer to be removed after second reconcile")
	}
}

func TestVerifyDynamoOwnershipRejectsWrongUID(t *testing.T) {
	existing := &unstructured.Unstructured{}
	setDGDGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetLabels(map[string]string{
		airunwayv1alpha1.LabelManagedBy: "airunway",
	})
	existing.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: "airunway.ai/v1alpha1", Kind: "ModelDeployment", Name: "other-md", UID: "other-uid"},
	})

	err := verifyDynamoOwnership(existing, "test-uid")
	if err == nil {
		t.Fatal("expected error for wrong UID, got nil")
	}
	if !isResourceConflict(err) {
		t.Errorf("expected resourceConflictError, got %T: %v", err, err)
	}
}

func TestVerifyDynamoOwnershipRejectsNoOwnerRef(t *testing.T) {
	existing := &unstructured.Unstructured{}
	setDGDGVK(existing)
	existing.SetName("test")
	existing.SetNamespace("default")
	existing.SetLabels(map[string]string{
		airunwayv1alpha1.LabelManagedBy: "airunway",
	})
	// No OwnerReferences set — simulates a manually created resource with the label

	err := verifyDynamoOwnership(existing, "test-uid")
	if err == nil {
		t.Fatal("expected error for missing OwnerReference, got nil")
	}
	if !isResourceConflict(err) {
		t.Errorf("expected resourceConflictError, got %T: %v", err, err)
	}
}

// --- dynamoProviderPredicate tests ---

func TestDynamoProviderPredicatePassesPVC(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-cache",
			Namespace: "default",
		},
	}
	if !dynamoProviderPredicate(pvc) {
		t.Error("expected PVC to pass predicate (non-ModelDeployment objects should always pass)")
	}
}

func TestDynamoModelDeploymentPVCReferenceIndexValues(t *testing.T) {
	managedSize := resource.MustParse("5Gi")
	md := newMDForController("model", "default")
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{
		{Name: "explicit", ClaimName: "shared-cache"},
		{Name: "duplicate", ClaimName: "shared-cache"},
		{Name: "generated"},
		{Name: "managed", Size: &managedSize},
	}}

	got := dynamoModelDeploymentPVCReferenceIndexValues(md)
	if len(got) != 2 || got[0] != "shared-cache" || got[1] != "model-generated" {
		t.Fatalf("unexpected indexed PVC references: %#v", got)
	}
}

func TestMapPVCToDynamoModelDeployments(t *testing.T) {
	referenced := newMDForController("referenced", "default")
	referenced.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{
		Name: "cache", ClaimName: "shared-cache",
	}}}
	unrelated := newMDForController("unrelated", "default")
	unrelated.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{
		Name: "cache", ClaimName: "other-cache",
	}}}
	otherProvider := newMDForController("other-provider", "default")
	otherProvider.Status.Provider.Name = ProviderName + "-other"
	otherProvider.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{
		Name: "cache", ClaimName: "shared-cache",
	}}}
	otherNamespace := newMDForController("other-namespace", "other")
	otherNamespace.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{
		Name: "cache", ClaimName: "shared-cache",
	}}}

	scheme := newScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(referenced, unrelated, otherProvider, otherNamespace).
		WithIndex(
			&airunwayv1alpha1.ModelDeployment{},
			dynamoModelDeploymentPVCReferenceField,
			dynamoModelDeploymentPVCReferenceIndexValues,
		).
		Build()
	r := NewDynamoProviderReconciler(c, scheme, testModelDownloaderImage)

	requests := r.mapPVCToModelDeployments(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-cache", Namespace: "default"},
	})
	if len(requests) != 1 || requests[0].Name != referenced.Name || requests[0].Namespace != referenced.Namespace {
		t.Fatalf("unexpected Dynamo PVC watch requests: %#v", requests)
	}
}

func TestDynamoProviderPredicatePassesJob(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-download",
			Namespace: "default",
		},
	}
	if !dynamoProviderPredicate(job) {
		t.Error("expected Job to pass predicate (non-ModelDeployment objects should always pass)")
	}
}

func TestDynamoProviderPredicatePassesUnstructuredDGD(t *testing.T) {
	dgd := &unstructured.Unstructured{}
	setDGDGVK(dgd)
	dgd.SetName("test")
	dgd.SetNamespace("default")
	if !dynamoProviderPredicate(dgd) {
		t.Error("expected DynamoGraphDeployment (unstructured) to pass predicate")
	}
}

func TestDynamoProviderPredicatePassesDynamoProviderInStatus(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Provider: &airunwayv1alpha1.ProviderStatus{Name: ProviderName},
		},
	}
	if !dynamoProviderPredicate(md) {
		t.Error("expected ModelDeployment with status.provider.name=dynamo to pass predicate")
	}
}

func TestDynamoProviderPredicatePassesDynamoProviderInSpec(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Provider: &airunwayv1alpha1.ProviderSpec{Name: ProviderName},
		},
	}
	if !dynamoProviderPredicate(md) {
		t.Error("expected ModelDeployment with spec.provider.name=dynamo to pass predicate")
	}
}

func TestDynamoProviderPredicatePassesWithFinalizer(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Namespace:  "default",
			Finalizers: []string{FinalizerName},
		},
	}
	if !dynamoProviderPredicate(md) {
		t.Error("expected ModelDeployment with dynamo finalizer to pass predicate")
	}
}

func TestDynamoProviderPredicateRejectsOtherProvider(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Provider: &airunwayv1alpha1.ProviderStatus{Name: "kaito"},
		},
	}
	if dynamoProviderPredicate(md) {
		t.Error("expected ModelDeployment with status.provider.name=kaito to be rejected")
	}
}

func TestDynamoProviderPredicateRejectsNoProviderNoFinalizer(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}
	if dynamoProviderPredicate(md) {
		t.Error("expected ModelDeployment with no provider and no finalizer to be rejected")
	}
}

func TestDynamoProviderPredicateRejectsOtherProviderInSpecAndStatus(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Provider: &airunwayv1alpha1.ProviderSpec{Name: "kuberay"},
		},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Provider: &airunwayv1alpha1.ProviderStatus{Name: "kuberay"},
		},
	}
	if dynamoProviderPredicate(md) {
		t.Error("expected ModelDeployment with provider=kuberay in both spec and status to be rejected")
	}
}
