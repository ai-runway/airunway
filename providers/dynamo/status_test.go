package dynamo

import (
	"context"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newDGDWithStatus(status map[string]interface{}) *unstructured.Unstructured {
	obj := map[string]interface{}{
		"apiVersion": "nvidia.com/v1alpha1",
		"kind":       "DynamoGraphDeployment",
		"metadata": map[string]interface{}{
			"name":      "test-dgd",
			"namespace": "default",
		},
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func newDGDRWithStatus(status map[string]any) *unstructured.Unstructured {
	dgdr := &unstructured.Unstructured{}
	dgdr.SetAPIVersion("nvidia.com/v1beta1")
	dgdr.SetKind(DynamoGraphDeploymentRequestKind)
	dgdr.SetName("test-dgdr")
	dgdr.SetNamespace("default")
	if status != nil {
		dgdr.Object["status"] = status
	}
	return dgdr
}

func TestTranslateDGDRPhases(t *testing.T) {
	translator := NewStatusTranslator()
	tests := []struct {
		name      string
		status    map[string]any
		autoApply *bool
		phase     airunwayv1alpha1.DeploymentPhase
		message   string
	}{
		{
			name:    "no status",
			phase:   airunwayv1alpha1.DeploymentPhasePending,
			message: "Dynamo deployment request is pending",
		},
		{
			name:    "pending",
			status:  map[string]any{"phase": "Pending"},
			phase:   airunwayv1alpha1.DeploymentPhasePending,
			message: "Dynamo deployment request is pending",
		},
		{
			name:    "profiling",
			status:  map[string]any{"phase": "Profiling", "profilingPhase": "benchmarking"},
			phase:   airunwayv1alpha1.DeploymentPhaseDeploying,
			message: "Dynamo is profiling the deployment (benchmarking)",
		},
		{
			name:    "profiling without detail",
			status:  map[string]any{"phase": "Profiling"},
			phase:   airunwayv1alpha1.DeploymentPhaseDeploying,
			message: "Dynamo is profiling the deployment",
		},
		{
			name:      "ready without auto apply",
			status:    map[string]any{"phase": "Ready"},
			autoApply: boolTestPtr(false),
			phase:     airunwayv1alpha1.DeploymentPhaseDeploying,
			message:   "Dynamo deployment plan is ready; autoApply is false",
		},
		{
			name:    "ready",
			status:  map[string]any{"phase": "Ready"},
			phase:   airunwayv1alpha1.DeploymentPhaseDeploying,
			message: "Dynamo deployment plan is ready and waiting to be applied",
		},
		{
			name:    "deploying",
			status:  map[string]any{"phase": "Deploying"},
			phase:   airunwayv1alpha1.DeploymentPhaseDeploying,
			message: "Dynamo is creating the generated deployment",
		},
		{
			name:    "deployed",
			status:  map[string]any{"phase": "Deployed"},
			phase:   airunwayv1alpha1.DeploymentPhaseRunning,
			message: "Dynamo deployment is running",
		},
		{
			name:    "unknown",
			status:  map[string]any{"phase": "Surprising"},
			phase:   airunwayv1alpha1.DeploymentPhasePending,
			message: `Dynamo deployment request has unknown phase "Surprising"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dgdr := newDGDRWithStatus(test.status)
			if test.autoApply != nil {
				dgdr.Object["spec"] = map[string]any{"autoApply": *test.autoApply}
			}
			result, err := translator.TranslateStatus(dgdr)
			if err != nil {
				t.Fatalf("TranslateStatus() error = %v", err)
			}
			if result.Phase != test.phase || result.Message != test.message {
				t.Errorf(
					"TranslateStatus() = (%s, %q), want (%s, %q)",
					result.Phase,
					result.Message,
					test.phase,
					test.message,
				)
			}
		})
	}
}

func TestTranslateDGDRDetails(t *testing.T) {
	translator := NewStatusTranslator()
	dgdr := newDGDRWithStatus(map[string]any{
		"phase":   "Deployed",
		"dgdName": "generated-dgd",
		"deploymentInfo": map[string]any{
			"replicas":          int64(4),
			"availableReplicas": int64(3),
		},
	})

	result, err := translator.TranslateStatus(dgdr)
	if err != nil {
		t.Fatalf("TranslateStatus() error = %v", err)
	}
	if result.ResourceKind != DynamoGraphDeploymentRequestKind {
		t.Errorf("ResourceKind = %q", result.ResourceKind)
	}
	if result.Replicas == nil || result.Replicas.Desired != 4 ||
		result.Replicas.Ready != 3 || result.Replicas.Available != 3 {
		t.Errorf("Replicas = %#v", result.Replicas)
	}
	if result.Endpoint == nil || result.Endpoint.Service != "generated-dgd-frontend" ||
		result.Endpoint.Port != 8000 {
		t.Errorf("Endpoint = %#v", result.Endpoint)
	}
	if !translator.IsReady(dgdr) {
		t.Error("expected deployed DGDR to be ready")
	}

	dgdr.Object["status"] = map[string]any{"phase": "Deploying"}
	if translator.IsReady(dgdr) {
		t.Error("expected deploying DGDR not to be ready")
	}
	result, err = translator.TranslateStatus(dgdr)
	if err != nil {
		t.Fatalf("TranslateStatus() error = %v", err)
	}
	if result.Endpoint != nil || result.Replicas != nil {
		t.Errorf("unexpected incomplete details: %#v", result)
	}
}

func TestTranslateDGDRFailures(t *testing.T) {
	translator := NewStatusTranslator()
	tests := []struct {
		name    string
		status  map[string]any
		message string
	}{
		{
			name:    "top-level message",
			status:  map[string]any{"phase": "Failed", "message": "profiling failed"},
			message: "profiling failed",
		},
		{
			name: "latest condition",
			status: map[string]any{
				"phase": "Failed",
				"conditions": []any{
					map[string]any{"message": "old failure"},
					map[string]any{"message": "latest failure"},
				},
			},
			message: "latest failure",
		},
		{
			name:    "fallback",
			status:  map[string]any{"phase": "Failed"},
			message: "Dynamo deployment request failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := translator.TranslateStatus(newDGDRWithStatus(test.status))
			if err != nil {
				t.Fatalf("TranslateStatus() error = %v", err)
			}
			if result.Phase != airunwayv1alpha1.DeploymentPhaseFailed ||
				result.Message != test.message {
				t.Errorf("TranslateStatus() = %#v, want message %q", result, test.message)
			}
		})
	}
}

func boolTestPtr(value bool) *bool {
	return &value
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

func TestTranslateStatusNoStatus(t *testing.T) {
	st := NewStatusTranslator()
	dgd := newDGDWithStatus(nil)

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("expected Pending phase, got %s", result.Phase)
	}
	if result.ResourceName != "test-dgd" {
		t.Errorf("expected resource name test-dgd, got %s", result.ResourceName)
	}
	if result.ResourceKind != DynamoGraphDeploymentKind {
		t.Errorf("expected resource kind %s, got %s", DynamoGraphDeploymentKind, result.ResourceKind)
	}
}

func TestTranslateStatusSuccessful(t *testing.T) {
	st := NewStatusTranslator()
	dgd := newDGDWithStatus(map[string]interface{}{
		"state": "successful",
	})

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", result.Phase)
	}
}

func TestTranslateStatusDeploying(t *testing.T) {
	st := NewStatusTranslator()
	dgd := newDGDWithStatus(map[string]interface{}{
		"state": "deploying",
	})

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying phase, got %s", result.Phase)
	}
}

func TestTranslateStatusInitializing(t *testing.T) {
	st := NewStatusTranslator()
	dgd := newDGDWithStatus(map[string]interface{}{
		"state": "initializing",
	})

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying phase, got %s", result.Phase)
	}
}

func TestTranslateStatusFailed(t *testing.T) {
	st := NewStatusTranslator()
	dgd := newDGDWithStatus(map[string]interface{}{
		"state":   "failed",
		"message": "OOM error",
	})

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase, got %s", result.Phase)
	}
	if result.Message != "OOM error" {
		t.Errorf("expected message 'OOM error', got %s", result.Message)
	}
}

func TestTranslateStatusPending(t *testing.T) {
	st := NewStatusTranslator()
	dgd := newDGDWithStatus(map[string]interface{}{
		"state": "pending",
	})

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("expected Pending phase, got %s", result.Phase)
	}
}

func TestTranslateStatusUnknownState(t *testing.T) {
	st := NewStatusTranslator()
	dgd := newDGDWithStatus(map[string]interface{}{
		"state": "unknown",
	})

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("expected Pending phase for unknown state, got %s", result.Phase)
	}
}

func TestTranslateStatusWithServicesReplicas(t *testing.T) {
	st := NewStatusTranslator()
	dgd := newDGDWithStatus(map[string]interface{}{
		"state": "successful",
		"services": map[string]interface{}{
			"worker1": map[string]interface{}{
				"replicas":          int64(2),
				"readyReplicas":     int64(2),
				"availableReplicas": int64(2),
			},
			"worker2": map[string]interface{}{
				"replicas":          int64(1),
				"readyReplicas":     int64(1),
				"availableReplicas": int64(1),
			},
		},
	})

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Replicas.Desired != 3 {
		t.Errorf("expected desired 3, got %d", result.Replicas.Desired)
	}
	if result.Replicas.Ready != 3 {
		t.Errorf("expected ready 3, got %d", result.Replicas.Ready)
	}
}

func TestTranslateStatusWithDirectReplicas(t *testing.T) {
	st := NewStatusTranslator()
	dgd := newDGDWithStatus(map[string]interface{}{
		"state":             "deploying",
		"desiredReplicas":   int64(4),
		"readyReplicas":     int64(2),
		"availableReplicas": int64(3),
	})

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Replicas.Desired != 4 {
		t.Errorf("expected desired 4, got %d", result.Replicas.Desired)
	}
	if result.Replicas.Ready != 2 {
		t.Errorf("expected ready 2, got %d", result.Replicas.Ready)
	}
	if result.Replicas.Available != 3 {
		t.Errorf("expected available 3, got %d", result.Replicas.Available)
	}
}

func TestTranslateStatusWithEndpoint(t *testing.T) {
	st := NewStatusTranslator()
	dgd := newDGDWithStatus(map[string]interface{}{
		"state": "successful",
		"endpoint": map[string]interface{}{
			"service": "custom-svc",
			"port":    int64(9000),
		},
	})

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Endpoint.Service != "custom-svc" {
		t.Errorf("expected service 'custom-svc', got %s", result.Endpoint.Service)
	}
	if result.Endpoint.Port != 9000 {
		t.Errorf("expected port 9000, got %d", result.Endpoint.Port)
	}
}

func TestTranslateStatusNilEndpointWithoutFrontend(t *testing.T) {
	st := NewStatusTranslator()
	resources, err := NewTransformer().Transform(context.Background(), newTestMD("test-dgd", "default"))
	if err != nil {
		t.Fatalf("unexpected transformer error: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 transformed resource, got %d", len(resources))
	}

	dgd := resources[0]
	dgd.Object["status"] = map[string]interface{}{
		"state": "successful",
	}

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Endpoint != nil {
		t.Fatalf("expected nil endpoint without standalone Frontend service, got %#v", result.Endpoint)
	}
}

func TestTranslateStatusWithInferredFrontendEndpoint(t *testing.T) {
	st := NewStatusTranslator()
	md := newTestMD("test-dgd", "default")
	// the Transformer creates Frontend whenever spec.gateway.enabled is explicitly false.
	gatewayEnabled := false
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{Enabled: &gatewayEnabled}
	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected transformer error: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 transformed resource, got %d", len(resources))
	}

	dgd := resources[0]
	dgd.Object["status"] = map[string]interface{}{
		"state": "successful",
	}

	result, err := st.TranslateStatus(dgd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Endpoint == nil {
		t.Fatal("expected inferred endpoint when standalone Frontend service is configured")
	}
	if result.Endpoint.Service != "test-dgd-frontend" {
		t.Errorf("expected inferred service 'test-dgd-frontend', got %s", result.Endpoint.Service)
	}
	if result.Endpoint.Port != 8000 {
		t.Errorf("expected default port 8000, got %d", result.Endpoint.Port)
	}
}

func TestIsReady(t *testing.T) {
	st := NewStatusTranslator()

	if st.IsReady(nil) {
		t.Error("expected not ready for nil")
	}

	// No status
	dgd := newDGDWithStatus(nil)
	if st.IsReady(dgd) {
		t.Error("expected not ready with no status")
	}

	// Successful
	dgd = newDGDWithStatus(map[string]interface{}{"state": "successful"})
	if !st.IsReady(dgd) {
		t.Error("expected ready when successful")
	}

	// Failed
	dgd = newDGDWithStatus(map[string]interface{}{"state": "failed"})
	if st.IsReady(dgd) {
		t.Error("expected not ready when failed")
	}
}

func TestGetErrorMessage(t *testing.T) {
	st := NewStatusTranslator()

	// Nil
	if msg := st.GetErrorMessage(nil); msg != "resource not found" {
		t.Errorf("expected 'resource not found', got %s", msg)
	}

	// With message
	dgd := newDGDWithStatus(map[string]interface{}{
		"state":   "failed",
		"message": "OOM killed",
	})
	if msg := st.GetErrorMessage(dgd); msg != "OOM killed" {
		t.Errorf("expected 'OOM killed', got %s", msg)
	}

	// With error field
	dgd = newDGDWithStatus(map[string]interface{}{
		"state": "failed",
		"error": "pod crash",
	})
	if msg := st.GetErrorMessage(dgd); msg != "pod crash" {
		t.Errorf("expected 'pod crash', got %s", msg)
	}

	// With conditions
	dgd = newDGDWithStatus(map[string]interface{}{
		"state": "failed",
		"conditions": []interface{}{
			map[string]interface{}{
				"type":    "Ready",
				"status":  "False",
				"message": "condition error",
			},
		},
	})
	if msg := st.GetErrorMessage(dgd); msg != "condition error" {
		t.Errorf("expected 'condition error', got %s", msg)
	}

	// Fallback
	dgd = newDGDWithStatus(map[string]interface{}{
		"state": "failed",
	})
	if msg := st.GetErrorMessage(dgd); msg != "deployment failed" {
		t.Errorf("expected 'deployment failed', got %s", msg)
	}
}

func TestMapStateToPhase(t *testing.T) {
	st := NewStatusTranslator()

	tests := []struct {
		state    DynamoState
		expected airunwayv1alpha1.DeploymentPhase
	}{
		{DynamoStateSuccessful, airunwayv1alpha1.DeploymentPhaseRunning},
		{DynamoStateInitializing, airunwayv1alpha1.DeploymentPhaseDeploying},
		{DynamoStateDeploying, airunwayv1alpha1.DeploymentPhaseDeploying},
		{DynamoStateFailed, airunwayv1alpha1.DeploymentPhaseFailed},
		{DynamoStatePending, airunwayv1alpha1.DeploymentPhasePending},
		{DynamoState("unknown"), airunwayv1alpha1.DeploymentPhasePending},
	}

	for _, tt := range tests {
		result := st.mapStateToPhase(tt.state)
		if result != tt.expected {
			t.Errorf("mapStateToPhase(%s) = %s, expected %s", tt.state, result, tt.expected)
		}
	}
}
