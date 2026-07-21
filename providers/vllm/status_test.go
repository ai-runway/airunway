package vllm

import (
	"testing"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newTestDeployment(name, namespace string) *unstructured.Unstructured {
	d := &unstructured.Unstructured{}
	d.SetAPIVersion("apps/v1")
	d.SetKind("Deployment")
	d.SetName(name)
	d.SetNamespace(namespace)
	return d
}

func setDeploymentConditions(d *unstructured.Unstructured, conditions []map[string]interface{}) {
	condSlice := make([]interface{}, len(conditions))
	for i, c := range conditions {
		condSlice[i] = c
	}
	_ = unstructured.SetNestedSlice(d.Object, condSlice, "status", "conditions")
}

func setDeploymentReplicas(d *unstructured.Unstructured, desired, ready, available int64) {
	_ = unstructured.SetNestedField(d.Object, desired, "spec", "replicas")
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
	setDeploymentConditions(d, []map[string]interface{}{
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

func TestTranslateStatusProgressingDeadlineExceeded(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")
	setDeploymentConditions(d, []map[string]interface{}{
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
	setDeploymentConditions(d, []map[string]interface{}{
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
	setDeploymentConditions(d, []map[string]interface{}{
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
