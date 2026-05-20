package kaito

import (
	"testing"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newWorkspaceWithStatus(conditions []interface{}) *unstructured.Unstructured {
	ws := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kaito.sh/v1beta1",
			"kind":       "Workspace",
			"metadata": map[string]interface{}{
				"name":      "test-ws",
				"namespace": "default",
			},
			"status": map[string]interface{}{
				"conditions": conditions,
			},
		},
	}
	return ws
}

func newWorkspaceWithResourceCount(count int64, conditions []interface{}) *unstructured.Unstructured {
	ws := newWorkspaceWithStatus(conditions)
	ws.Object["resource"] = map[string]interface{}{
		"count": count,
	}
	return ws
}

func TestNewStatusTranslator(t *testing.T) {
	st := NewStatusTranslator()
	if st == nil {
		t.Fatal("expected non-nil status translator")
	}
}

func TestTranslateStatusNil(t *testing.T) {
	st := NewStatusTranslator()
	_, err := st.TranslateStatus(nil)
	if err == nil {
		t.Fatal("expected error for nil upstream")
	}
}

func TestTranslateStatusNoConditions(t *testing.T) {
	st := NewStatusTranslator()
	ws := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kaito.sh/v1beta1",
			"kind":       "Workspace",
			"metadata": map[string]interface{}{
				"name": "test-ws",
			},
		},
	}

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("expected Pending phase, got %s", result.Phase)
	}
	if result.ResourceName != "test-ws" {
		t.Errorf("expected resource name test-ws, got %s", result.ResourceName)
	}
	if result.ResourceKind != WorkspaceKind {
		t.Errorf("expected resource kind %s, got %s", WorkspaceKind, result.ResourceKind)
	}
}

func TestTranslateStatusWorkspaceSucceeded(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithResourceCount(2, []interface{}{
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "True",
		},
	})

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", result.Phase)
	}
	if result.Replicas.Desired != 2 {
		t.Errorf("expected desired replicas 2, got %d", result.Replicas.Desired)
	}
	if result.Replicas.Ready != 2 {
		t.Errorf("expected ready replicas 2, got %d", result.Replicas.Ready)
	}
	if result.Replicas.Available != 2 {
		t.Errorf("expected available replicas 2, got %d", result.Replicas.Available)
	}
}

func TestTranslateStatusWorkspaceFailed(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":    "WorkspaceSucceeded",
			"status":  "False",
			"message": "insufficient resources",
		},
	})

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase, got %s", result.Phase)
	}
	if result.Message != "insufficient resources" {
		t.Errorf("expected message 'insufficient resources', got %s", result.Message)
	}
}

func TestTranslateStatusResourceReadyOnly(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "ResourceReady",
			"status": "True",
		},
	})

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying phase, got %s", result.Phase)
	}
}

func TestTranslateStatusResourceReadyAndInferenceReady(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "ResourceReady",
			"status": "True",
		},
		map[string]interface{}{
			"type":   "InferenceReady",
			"status": "True",
		},
	})

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Without WorkspaceSucceeded, should be Pending (falls through)
	if result.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("expected Pending phase (no WorkspaceSucceeded), got %s", result.Phase)
	}
}

func TestTranslateStatusEndpoint(t *testing.T) {
	st := NewStatusTranslator()
	// Need at least one condition to reach endpoint extraction
	ws := newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "True",
		},
	})

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Endpoint == nil {
		t.Fatal("expected non-nil endpoint")
	}
	if result.Endpoint.Service != "test-ws" {
		t.Errorf("expected service name test-ws, got %s", result.Endpoint.Service)
	}
	if result.Endpoint.Port != defaultKAITOPort {
		t.Errorf("expected port %d, got %d", defaultKAITOPort, result.Endpoint.Port)
	}
}

func TestTranslateStatusEndpointLlamaCpp(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "True",
		},
	})
	// Set inference.template to simulate a llamacpp workspace
	ws.Object["inference"] = map[string]interface{}{
		"template": map[string]interface{}{
			"name": "llamacpp",
		},
	}

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Endpoint == nil {
		t.Fatal("expected non-nil endpoint")
	}
	if result.Endpoint.Service != "test-ws" {
		t.Errorf("expected service name test-ws, got %s", result.Endpoint.Service)
	}
	if result.Endpoint.Port != defaultKAITOPort {
		t.Errorf("expected service port %d for llamacpp template, got %d", defaultKAITOPort, result.Endpoint.Port)
	}
}

func TestIsReady(t *testing.T) {
	st := NewStatusTranslator()

	// Nil upstream
	if st.IsReady(nil) {
		t.Error("expected not ready for nil upstream")
	}

	// No conditions
	ws := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "test"},
		},
	}
	if st.IsReady(ws) {
		t.Error("expected not ready with no conditions")
	}

	// WorkspaceSucceeded=True
	ws = newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "True",
		},
	})
	if !st.IsReady(ws) {
		t.Error("expected ready when WorkspaceSucceeded=True")
	}

	// WorkspaceSucceeded=False
	ws = newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "False",
		},
	})
	if st.IsReady(ws) {
		t.Error("expected not ready when WorkspaceSucceeded=False")
	}

	// No WorkspaceSucceeded condition
	ws = newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "ResourceReady",
			"status": "True",
		},
	})
	if st.IsReady(ws) {
		t.Error("expected not ready without WorkspaceSucceeded")
	}

	// Invalid condition entries should be skipped
	ws = newWorkspaceWithStatus([]interface{}{
		"not-a-map",
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "True",
		},
	})
	if !st.IsReady(ws) {
		t.Error("expected ready even with invalid entries before valid one")
	}
}

func TestGetErrorMessage(t *testing.T) {
	st := NewStatusTranslator()

	// Nil upstream
	msg := st.GetErrorMessage(nil)
	if msg != "resource not found" {
		t.Errorf("expected 'resource not found', got %s", msg)
	}

	// No conditions
	ws := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "test"},
		},
	}
	msg = st.GetErrorMessage(ws)
	if msg != "deployment failed" {
		t.Errorf("expected 'deployment failed', got %s", msg)
	}

	// WorkspaceSucceeded=False with message
	ws = newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":    "WorkspaceSucceeded",
			"status":  "False",
			"message": "node allocation failed",
		},
	})
	msg = st.GetErrorMessage(ws)
	if msg != "node allocation failed" {
		t.Errorf("expected 'node allocation failed', got %s", msg)
	}

	// WorkspaceSucceeded=False without message, other condition with message
	ws = newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "False",
		},
		map[string]interface{}{
			"type":    "ResourceReady",
			"status":  "False",
			"message": "resource creation failed",
		},
	})
	msg = st.GetErrorMessage(ws)
	if msg != "resource creation failed" {
		t.Errorf("expected 'resource creation failed', got %s", msg)
	}

	// All conditions True → fallback
	ws = newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "True",
		},
	})
	msg = st.GetErrorMessage(ws)
	if msg != "deployment failed" {
		t.Errorf("expected 'deployment failed' fallback, got %s", msg)
	}

	// Invalid condition entries in both loops
	ws = newWorkspaceWithStatus([]interface{}{
		"not-a-map",
		map[string]interface{}{
			"type":    "WorkspaceSucceeded",
			"status":  "False",
			"message": "from-invalid-mix",
		},
	})
	msg = st.GetErrorMessage(ws)
	if msg != "from-invalid-mix" {
		t.Errorf("expected 'from-invalid-mix', got %s", msg)
	}

	// WorkspaceSucceeded=False without message, no fallback conditions with messages
	ws = newWorkspaceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "False",
		},
	})
	msg = st.GetErrorMessage(ws)
	if msg != "deployment failed" {
		t.Errorf("expected 'deployment failed', got %s", msg)
	}
}

func TestParseConditionsInvalidEntries(t *testing.T) {
	st := NewStatusTranslator()

	conditions := []interface{}{
		"not-a-map",
		map[string]interface{}{}, // no type
		map[string]interface{}{
			"type":   "Valid",
			"status": "True",
		},
	}

	condMap := st.parseConditions(conditions)
	if len(condMap) != 1 {
		t.Errorf("expected 1 valid condition, got %d", len(condMap))
	}
	if _, ok := condMap["Valid"]; !ok {
		t.Error("expected 'Valid' condition in map")
	}
}

func TestStringVal(t *testing.T) {
	m := map[string]interface{}{
		"key1": "value1",
		"key2": 42,
	}

	if stringVal(m, "key1") != "value1" {
		t.Errorf("expected 'value1'")
	}
	if stringVal(m, "key2") != "" {
		t.Errorf("expected empty string for non-string value")
	}
	if stringVal(m, "key3") != "" {
		t.Errorf("expected empty string for missing key")
	}
}

// newWorkspaceWithState creates a Workspace with status.state set (KAITO 0.10.0+).
// Optionally includes conditions for backward-compat / replica tests.
func newWorkspaceWithState(state string, conditions []interface{}) *unstructured.Unstructured {
	ws := newWorkspaceWithStatus(conditions)
	if state != "" {
		statusMap := ws.Object["status"].(map[string]interface{})
		statusMap["state"] = state
	}
	return ws
}

func TestTranslateStatusWithStateReady(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithState("Ready", nil)

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", result.Phase)
	}
	if result.Message != "" {
		t.Errorf("expected empty message, got %q", result.Message)
	}
}

func TestTranslateStatusWithStateNotReady(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithState("NotReady", nil)

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying phase, got %s", result.Phase)
	}
}

func TestTranslateStatusWithStateFailed(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithState("Failed", nil)

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase, got %s", result.Phase)
	}
}

func TestTranslateStatusWithStatePending(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithState("Pending", nil)

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("expected Pending phase, got %s", result.Phase)
	}
}

func TestTranslateStatusWithStateRunning(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithState("Running", nil)

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying phase, got %s", result.Phase)
	}
	if result.Message != "fine-tuning in progress" {
		t.Errorf("expected message 'fine-tuning in progress', got %q", result.Message)
	}
}

func TestTranslateStatusWithStateSucceeded(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithState("Succeeded", nil)

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", result.Phase)
	}
}

func TestTranslateStatusWithStateUnknown(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithState("SomeUnknown", nil)

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("expected Pending phase, got %s", result.Phase)
	}
	if result.Message != "unknown state: SomeUnknown" {
		t.Errorf("expected message 'unknown state: SomeUnknown', got %q", result.Message)
	}
}

func TestTranslateStatusWithEmptyState(t *testing.T) {
	// Empty state with conditions should fall back to condition-based mapping
	st := NewStatusTranslator()
	ws := newWorkspaceWithState("", []interface{}{
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "True",
		},
	})

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase from condition fallback, got %s", result.Phase)
	}
}

func TestTranslateStatusStateWithReplicas(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithState("Ready", []interface{}{
		map[string]interface{}{
			"type":   "WorkspaceSucceeded",
			"status": "True",
		},
	})
	ws.Object["resource"] = map[string]interface{}{
		"count": int64(3),
	}

	result, err := st.TranslateStatus(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", result.Phase)
	}
	if result.Replicas == nil {
		t.Fatal("expected non-nil replicas")
	}
	if result.Replicas.Desired != 3 {
		t.Errorf("expected desired replicas 3, got %d", result.Replicas.Desired)
	}
	if result.Replicas.Ready != 3 {
		t.Errorf("expected ready replicas 3, got %d", result.Replicas.Ready)
	}
	if result.Replicas.Available != 3 {
		t.Errorf("expected available replicas 3, got %d", result.Replicas.Available)
	}
}

func TestExtractReplicasNoSpec(t *testing.T) {
	st := NewStatusTranslator()
	ws := newWorkspaceWithStatus([]interface{}{})

	condMap := map[string]conditionInfo{}
	replicas := st.extractReplicas(ws, condMap)
	if replicas.Desired != 0 {
		t.Errorf("expected desired 0, got %d", replicas.Desired)
	}
}
