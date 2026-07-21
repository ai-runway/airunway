package vllm

import (
	"context"
	"strings"
	"testing"
	"time"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
			Model:  airunwayv1alpha1.ModelSpec{ID: "test-model", Source: airunwayv1alpha1.ModelSourceHuggingFace},
			Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Resources: &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{Count: 1},
			},
		},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Provider: &airunwayv1alpha1.ProviderStatus{Name: ProviderName},
		},
	}
}

func TestValidateCompatibility(t *testing.T) {
	r := &VLLMProviderReconciler{}

	tests := []struct {
		name    string
		md      *airunwayv1alpha1.ModelDeployment
		wantErr bool
	}{
		{
			name: "vllm with GPU is compatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 1},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "sglang is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeSGLang},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 1},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "trtllm is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeTRTLLM},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 1},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "no GPU resources is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Resources: nil,
				},
			},
			wantErr: true,
		},
		{
			name: "zero GPU count is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 0},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "disaggregated without prefill is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:  airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Serving: &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Decode: &airunwayv1alpha1.ComponentScalingSpec{
							Replicas: 1,
							GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "disaggregated without decode is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:  airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Serving: &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{
							Replicas: 2,
							GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "disaggregated with both prefill and decode is rejected (aggregated-only)",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:  airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Serving: &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{
							Replicas: 2,
							GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
						},
						Decode: &airunwayv1alpha1.ComponentScalingSpec{
							Replicas: 1,
							GPU:      &airunwayv1alpha1.GPUSpec{Count: 4},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "disaggregated without GPU on prefill is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:  airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Serving: &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{
							Replicas: 2,
						},
						Decode: &airunwayv1alpha1.ComponentScalingSpec{
							Replicas: 1,
							GPU:      &airunwayv1alpha1.GPUSpec{Count: 4},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "disaggregated without GPU on decode is incompatible",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:  airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Serving: &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{
							Replicas: 2,
							GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
						},
						Decode: &airunwayv1alpha1.ComponentScalingSpec{
							Replicas: 1,
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "disaggregated without top-level resources is rejected (aggregated-only)",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Engine:  airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Serving: &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{
							Replicas: 4,
							GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
						},
						Decode: &airunwayv1alpha1.ComponentScalingSpec{
							Replicas: 1,
							GPU:      &airunwayv1alpha1.GPUSpec{Count: 4},
						},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.validateCompatibility(tt.md)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateCompatibility() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestReconcileIgnoresOtherProviders(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test-model", "default")
	md.Status.Provider.Name = "some-other-provider"

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		Build()

	r := NewVLLMProviderReconciler(c, scheme)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-model"},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should return empty result (no requeue) since provider doesn't match
	if result.Requeue || result.RequeueAfter != 0 {
		t.Error("expected no requeue for non-matching provider")
	}
}

func TestReconcileIgnoresNoProvider(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test-model", "default")
	md.Status.Provider = nil

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		Build()

	r := NewVLLMProviderReconciler(c, scheme)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-model"},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Error("expected no requeue when no provider assigned")
	}
}

func TestReconcileHappyPathCreatesDeploymentAndService(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test-model", "default")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md).
		WithStatusSubresource(md).
		Build()

	r := NewVLLMProviderReconciler(c, scheme)
	r.ImageResolver = successfulFakeResolver(fakeResolvedImage(DefaultVLLMImage, "sha256:default"))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-model"}}
	// First reconcile adds the finalizer and requeues; the second creates resources.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected reconcile error (finalizer pass): %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected reconcile error (apply pass): %v", err)
	}

	// The finalizer must be added so cleanup runs on delete.
	var got airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-model"}, &got); err != nil {
		t.Fatalf("failed to get ModelDeployment: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, FinalizerName) {
		t.Errorf("expected finalizer %s to be added", FinalizerName)
	}

	// The Deployment and Service must be created and owned by the MD.
	deploy := &unstructured.Unstructured{}
	deploy.SetGroupVersionKind(deploymentGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-model"}, deploy); err != nil {
		t.Fatalf("expected Deployment to be created: %v", err)
	}
	svc := &unstructured.Unstructured{}
	svc.SetGroupVersionKind(serviceGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-model"}, svc); err != nil {
		t.Fatalf("expected Service to be created: %v", err)
	}
}

func TestReconcileOwnershipConflictNamesOwner(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test-model", "default")

	// Pre-create a Deployment owned by a DIFFERENT controller.
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(deploymentGVK)
	foreign.SetNamespace("default")
	foreign.SetName("test-model")
	foreign.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "airunway.ai/v1alpha1",
		Kind:       "ModelDeployment",
		Name:       "someone-else",
		UID:        types.UID("foreign-uid"),
	}})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, foreign).
		WithStatusSubresource(md).
		Build()

	r := NewVLLMProviderReconciler(c, scheme)
	r.ImageResolver = successfulFakeResolver(fakeResolvedImage(DefaultVLLMImage, "sha256:default"))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-model"}}
	// First reconcile adds the finalizer and requeues; the conflict surfaces on the apply pass.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected reconcile error (finalizer pass): %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected reconcile error (apply pass): %v", err)
	}

	// The conflict is recorded on status (phase Failed + ResourceConflict condition),
	// and the message must name the actual owner to aid debugging.
	var got airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("failed to get ModelDeployment: %v", err)
	}
	if got.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Fatalf("expected phase Failed on ownership conflict, got %q", got.Status.Phase)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeResourceCreated)
	if cond == nil || cond.Reason != "ResourceConflict" {
		t.Fatalf("expected ResourceCreated=False/ResourceConflict, got %#v", cond)
	}
	if !strings.Contains(cond.Message, "someone-else") || !strings.Contains(cond.Message, "foreign-uid") {
		t.Errorf("expected conflict message to name the owner, got %q", cond.Message)
	}

	// The foreign resource must be left untouched.
	deploy := &unstructured.Unstructured{}
	deploy.SetGroupVersionKind(deploymentGVK)
	if err := c.Get(context.Background(), req.NamespacedName, deploy); err != nil {
		t.Fatalf("failed to get foreign Deployment: %v", err)
	}
	owners := deploy.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Name != "someone-else" {
		t.Errorf("expected foreign Deployment to keep its owner, got %+v", owners)
	}
}

func TestHandleDeletionRemovesOwnedDeployment(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test-model", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	now := metav1.Now()
	md.DeletionTimestamp = &now

	owned := &unstructured.Unstructured{}
	owned.SetGroupVersionKind(deploymentGVK)
	owned.SetNamespace("default")
	owned.SetName("test-model")
	owned.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "airunway.ai/v1alpha1",
		Kind:       "ModelDeployment",
		Name:       md.Name,
		UID:        md.UID,
	}})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, owned).
		WithStatusSubresource(md).
		Build()

	r := NewVLLMProviderReconciler(c, scheme)
	r.ImageResolver = successfulFakeResolver(fakeResolvedImage(DefaultVLLMImage, "sha256:default"))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-model"},
	}); err != nil {
		t.Fatalf("unexpected reconcile error during deletion: %v", err)
	}

	// The owned Deployment should be deleted.
	deploy := &unstructured.Unstructured{}
	deploy.SetGroupVersionKind(deploymentGVK)
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-model"}, deploy)
	if err == nil {
		t.Errorf("expected owned Deployment to be deleted during finalization")
	}
}

// A Deployment stuck Terminating (its own finalizers/PDBs) never disappears and
// Delete returns nil, so the finalizer-timeout must fire on its own — otherwise
// the ModelDeployment requeues forever. Regression for that nesting bug.
func TestHandleDeletionRemovesFinalizerAfterTimeoutWhenDeploymentStuck(t *testing.T) {
	scheme := newScheme()
	md := newMDForController("test-model", "default")
	controllerutil.AddFinalizer(md, FinalizerName)
	// DeletionTimestamp older than FinalizerTimeout (5m).
	stuck := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	md.DeletionTimestamp = &stuck

	owned := &unstructured.Unstructured{}
	owned.SetGroupVersionKind(deploymentGVK)
	owned.SetNamespace("default")
	owned.SetName("test-model")
	owned.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "airunway.ai/v1alpha1",
		Kind:       "ModelDeployment",
		Name:       md.Name,
		UID:        md.UID,
	}})

	// Intercept Delete as a no-op so the Deployment stays present (simulating a
	// stuck-Terminating object whose Delete returns nil but never completes).
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, owned).
		WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return nil // pretend deletion was accepted but the object lingers
			},
		}).
		Build()

	r := NewVLLMProviderReconciler(c, scheme)
	r.ImageResolver = successfulFakeResolver(fakeResolvedImage(DefaultVLLMImage, "sha256:default"))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-model"},
	}); err != nil {
		t.Fatalf("unexpected reconcile error during deletion: %v", err)
	}

	// The finalizer must be removed so the ModelDeployment can be garbage-collected.
	var got airunwayv1alpha1.ModelDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-model"}, &got); err != nil {
		// If the object is gone (finalizer removed → GC'd), that also satisfies the intent.
		return
	}
	if controllerutil.ContainsFinalizer(&got, FinalizerName) {
		t.Errorf("expected finalizer to be removed after timeout, but it is still present")
	}
}

func TestRemoteImageResolverRejectsEmptyAndInvalidRefs(t *testing.T) {
	resolver := NewRemoteImageResolver()

	if _, err := resolver.Resolve(context.Background(), "   "); err == nil {
		t.Error("expected error for empty image reference")
	}
	if _, err := resolver.Resolve(context.Background(), "::not a ref::"); err == nil {
		t.Error("expected parse error for malformed image reference")
	}
}

func TestSyncStatusRunningUpdatesMessage(t *testing.T) {
	scheme := newScheme()

	deploy := &unstructured.Unstructured{}
	deploy.SetGroupVersionKind(deploymentGVK)
	deploy.SetName("test")
	deploy.SetNamespace("default")
	deploy.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{"type": "Available", "status": "True"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	r := NewVLLMProviderReconciler(c, scheme)

	md := &airunwayv1alpha1.ModelDeployment{}
	// Simulate a prior reconcile loop that left the deploying-phase message.
	md.Status.Message = "Deployments created, waiting for pods to be ready"

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(deploymentGVK)
	desired.SetName("test")
	desired.SetNamespace("default")

	if err := r.syncStatus(context.Background(), md, desired); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Fatalf("expected Running phase, got %s", md.Status.Phase)
	}
	if strings.Contains(md.Status.Message, "waiting for pods") {
		t.Errorf("status message still claims waiting for pods while Running: %q", md.Status.Message)
	}
	if md.Status.Message == "" {
		t.Errorf("expected a non-empty status message in Running phase")
	}
}
