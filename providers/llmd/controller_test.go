package llmd

import (
	"context"
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	r := &LLMDProviderReconciler{}

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
			name: "disaggregated with both prefill and decode is compatible",
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
			wantErr: false,
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
			name: "disaggregated without top-level resources is compatible",
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
			wantErr: false,
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

	r := NewLLMDProviderReconciler(c, scheme)
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

	r := NewLLMDProviderReconciler(c, scheme)
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

// TestSyncStatusRunningUpdatesMessage reproduces issue #289: once the upstream
// Deployment is Available the phase flips to Running, but the status message
// must no longer claim it is "waiting for pods to be ready".
func TestSyncStatusRunningUpdatesMessage(t *testing.T) {
	scheme := newScheme()

	deploy := &unstructured.Unstructured{}
	deploy.SetAPIVersion("apps/v1")
	deploy.SetKind("Deployment")
	deploy.SetName("test")
	deploy.SetNamespace("default")
	deploy.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{"type": "Available", "status": "True"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	r := NewLLMDProviderReconciler(c, scheme)

	md := &airunwayv1alpha1.ModelDeployment{}
	md.Status.Message = "Deployments created, waiting for pods to be ready"

	desired := &unstructured.Unstructured{}
	desired.SetAPIVersion("apps/v1")
	desired.SetKind("Deployment")
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
