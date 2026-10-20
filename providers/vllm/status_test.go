package vllm

import (
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newTestDeployment(name, namespace string) *unstructured.Unstructured {
	d := &unstructured.Unstructured{}
	d.SetAPIVersion("apps/v1")
	d.SetKind("Deployment")
	d.SetName(name)
	d.SetNamespace(namespace)
	d.SetGeneration(1)
	_ = unstructured.SetNestedField(d.Object, int64(1), "status", "observedGeneration")
	return d
}

func setDeploymentConditions(d *unstructured.Unstructured, conditions []map[string]any) {
	condSlice := make([]any, len(conditions))
	for i, c := range conditions {
		condSlice[i] = c
	}
	_ = unstructured.SetNestedSlice(d.Object, condSlice, "status", "conditions")
}

func setDeploymentReplicas(d *unstructured.Unstructured, desired, ready, available int64) {
	_ = unstructured.SetNestedField(d.Object, desired, "spec", "replicas")
	_ = unstructured.SetNestedField(d.Object, desired, "status", "replicas")
	_ = unstructured.SetNestedField(d.Object, desired, "status", "updatedReplicas")
	_ = unstructured.SetNestedField(d.Object, ready, "status", "readyReplicas")
	_ = unstructured.SetNestedField(d.Object, available, "status", "availableReplicas")
}

func TestTranslateStatusNilUpstream(t *testing.T) {
	st := NewStatusTranslator()
	_, err := st.TranslateStatus(nil)
	if err == nil {
		t.Fatal("expected error for nil upstream")
	}
}

func TestTranslateStatusNoConditions(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("expected Pending phase, got %s", result.Phase)
	}
}

func TestTranslateStatusAvailableTrue(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")
	setDeploymentReplicas(d, 2, 2, 2)
	setDeploymentConditions(d, []map[string]any{
		{"type": "Available", "status": "True"},
		{"type": "Progressing", "status": "True"},
	})

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", result.Phase)
	}
	if result.Replicas == nil || result.Replicas.Desired != 2 {
		t.Errorf("expected 2 desired replicas, got %v", result.Replicas)
	}
	if result.Replicas.Ready != 2 {
		t.Errorf("expected 2 ready replicas, got %v", result.Replicas.Ready)
	}
}

func TestTranslateStatusAvailableTrueRequiresCurrentReplicas(t *testing.T) {
	tests := []struct {
		name        string
		desired     int64
		ready       int64
		available   int64
		setReplicas bool
	}{
		{name: "ready count is stale", desired: 1, ready: 0, available: 1, setReplicas: true},
		{name: "available count is stale", desired: 1, ready: 1, available: 0, setReplicas: true},
		{name: "both counts are stale", desired: 1, ready: 0, available: 0, setReplicas: true},
		{name: "counts are below desired", desired: 2, ready: 1, available: 1, setReplicas: true},
		{name: "scaled to zero is not serving", desired: 0, ready: 0, available: 0, setReplicas: true},
		{name: "replica fields are missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := NewStatusTranslator()
			d := newTestDeployment("test", "default")
			if tt.setReplicas {
				setDeploymentReplicas(d, tt.desired, tt.ready, tt.available)
			}
			setDeploymentConditions(d, []map[string]any{
				{"type": "Available", "status": "True"},
				{"type": "Progressing", "status": "True"},
			})

			result, err := st.TranslateStatus(d)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
				t.Errorf("expected Deploying phase, got %s", result.Phase)
			}
		})
	}
}

func TestTranslateStatusAvailableTrueRequiresCurrentRollout(t *testing.T) {
	tests := []struct {
		name               string
		totalReplicas      int64
		updatedReplicas    int64
		observedGeneration int64
	}{
		{name: "new revision has no replicas", totalReplicas: 2, updatedReplicas: 0, observedGeneration: 4},
		{name: "new revision is only partially updated", totalReplicas: 2, updatedReplicas: 1, observedGeneration: 4},
		{name: "old replicas remain", totalReplicas: 3, updatedReplicas: 2, observedGeneration: 4},
		{name: "scale-down has excess updated replicas", totalReplicas: 3, updatedReplicas: 3, observedGeneration: 4},
		{name: "deployment controller has not observed current generation", totalReplicas: 2, updatedReplicas: 2, observedGeneration: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDeployment("test", "default")
			d.SetGeneration(4)
			setDeploymentReplicas(d, 2, tt.totalReplicas, tt.totalReplicas)
			_ = unstructured.SetNestedField(d.Object, tt.totalReplicas, "status", "replicas")
			_ = unstructured.SetNestedField(d.Object, tt.updatedReplicas, "status", "updatedReplicas")
			_ = unstructured.SetNestedField(d.Object, tt.observedGeneration, "status", "observedGeneration")
			setDeploymentConditions(d, []map[string]any{
				{"type": "Available", "status": "True"},
				{"type": "Progressing", "status": "True"},
			})

			result, err := NewStatusTranslator().TranslateStatus(d)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
				t.Errorf("expected Deploying phase, got %s", result.Phase)
			}
		})
	}
}

func TestTranslateStatusReplicaFailureIncludesCause(t *testing.T) {
	d := newTestDeployment("test", "default")
	setDeploymentReplicas(d, 2, 2, 2)
	_ = unstructured.SetNestedField(d.Object, int64(0), "status", "updatedReplicas")
	setDeploymentConditions(d, []map[string]any{
		{"type": "Available", "status": "True"},
		{"type": "Progressing", "status": "True"},
		{
			"type":    "ReplicaFailure",
			"status":  "True",
			"reason":  "FailedCreate",
			"message": "exceeded quota for GPU requests",
		},
	})

	result, err := NewStatusTranslator().TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Fatalf("expected Deploying phase, got %s", result.Phase)
	}
	if result.Message != "FailedCreate: exceeded quota for GPU requests" {
		t.Errorf("message = %q, want child ReplicaFailure reason and message", result.Message)
	}
}

func TestTranslateStatusProgressingDeadlineExceeded(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")
	setDeploymentConditions(d, []map[string]any{
		{
			"type":    "Progressing",
			"status":  "False",
			"reason":  "ProgressDeadlineExceeded",
			"message": "deployment timed out",
		},
	})

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase, got %s", result.Phase)
	}
	if result.Message == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestTranslateStatusProgressing(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")
	setDeploymentReplicas(d, 3, 1, 1)
	setDeploymentConditions(d, []map[string]any{
		{"type": "Available", "status": "False"},
		{"type": "Progressing", "status": "True"},
	})

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying phase, got %s", result.Phase)
	}
}

func TestTranslateStatusEndpoint(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("my-deployment", "default")

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Endpoint == nil {
		t.Fatal("expected endpoint")
	}
	if result.Endpoint.Service != "my-deployment" {
		t.Errorf("expected service name 'my-deployment', got %s", result.Endpoint.Service)
	}
	if result.Endpoint.Port != int32(DefaultVLLMPort) {
		t.Errorf("expected port %d, got %d", DefaultVLLMPort, result.Endpoint.Port)
	}
}

func TestTranslateStatusResourceName(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("my-deployment", "default")

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ResourceName != "my-deployment" {
		t.Errorf("expected resource name 'my-deployment', got %s", result.ResourceName)
	}
	if result.ResourceKind != "Deployment" {
		t.Errorf("expected resource kind 'Deployment', got %s", result.ResourceKind)
	}
}

func TestTranslateStatusAvailableFalseWithMessage(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")
	setDeploymentConditions(d, []map[string]any{
		{
			"type":    "Available",
			"status":  "False",
			"message": "insufficient replicas",
		},
	})

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase when Available=False with message, got %s", result.Phase)
	}
	if result.Message != "insufficient replicas" {
		t.Errorf("expected message 'insufficient replicas', got %s", result.Message)
	}
}
