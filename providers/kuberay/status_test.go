package kuberay

import (
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newRayServiceWithStatus(conditions []interface{}, appStatuses map[string]interface{}) *unstructured.Unstructured {
	obj := map[string]interface{}{
		"apiVersion": "ray.io/v1",
		"kind":       "RayService",
		"metadata": map[string]interface{}{
			"name":      "test-rs",
			"namespace": "default",
		},
		"status": map[string]interface{}{},
	}
	if conditions != nil {
		obj["status"].(map[string]interface{})["conditions"] = conditions
	}
	if appStatuses != nil {
		obj["status"].(map[string]interface{})["activeServiceStatus"] = map[string]interface{}{
			"applicationStatuses": appStatuses,
		}
	}
	return &unstructured.Unstructured{Object: obj}
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
	rs := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "ray.io/v1",
			"kind":       "RayService",
			"metadata":   map[string]interface{}{"name": "test-rs"},
		},
	}

	result, err := st.TranslateStatus(rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("expected Pending phase, got %s", result.Phase)
	}
	if result.ResourceKind != RayServiceKind {
		t.Errorf("expected resource kind %s, got %s", RayServiceKind, result.ResourceKind)
	}
}

func TestTranslateStatusConditionReady(t *testing.T) {
	st := NewStatusTranslator()
	rs := newRayServiceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "RayServiceReady",
			"status": "True",
		},
	}, nil)

	result, err := st.TranslateStatus(rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", result.Phase)
	}
}

func TestTranslateStatusConditionNotReady(t *testing.T) {
	st := NewStatusTranslator()
	rs := newRayServiceWithStatus([]interface{}{
		map[string]interface{}{
			"type":    "RayServiceReady",
			"status":  "False",
			"message": "waiting for pods",
		},
	}, nil)

	result, err := st.TranslateStatus(rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying phase, got %s", result.Phase)
	}
	if result.Message != "waiting for pods" {
		t.Errorf("expected message 'waiting for pods', got %s", result.Message)
	}
}

func TestTranslateStatusAppRunning(t *testing.T) {
	st := NewStatusTranslator()
	rs := newRayServiceWithStatus(nil, map[string]interface{}{
		"llm": map[string]interface{}{
			"status":  "RUNNING",
			"message": "",
		},
	})

	result, err := st.TranslateStatus(rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", result.Phase)
	}
}

func TestTranslateStatusAppFailed(t *testing.T) {
	st := NewStatusTranslator()
	rs := newRayServiceWithStatus(nil, map[string]interface{}{
		"llm": map[string]interface{}{
			"status":  "DEPLOY_FAILED",
			"message": "pod crash",
		},
	})

	result, err := st.TranslateStatus(rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase, got %s", result.Phase)
	}
	if result.Message != "pod crash" {
		t.Errorf("expected message 'pod crash', got %s", result.Message)
	}
}

func TestTranslateStatusAppDeploying(t *testing.T) {
	st := NewStatusTranslator()
	rs := newRayServiceWithStatus(nil, map[string]interface{}{
		"llm": map[string]interface{}{
			"status": "DEPLOYING",
		},
	})

	result, err := st.TranslateStatus(rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying phase, got %s", result.Phase)
	}
}

func TestTranslateStatusEndpoint(t *testing.T) {
	st := NewStatusTranslator()
	rs := newRayServiceWithStatus(nil, map[string]interface{}{
		"llm": map[string]interface{}{"status": "RUNNING"},
	})

	result, err := st.TranslateStatus(rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Endpoint.Service != "test-rs-serve-svc" {
		t.Errorf("expected service 'test-rs-serve-svc', got %s", result.Endpoint.Service)
	}
	if result.Endpoint.Port != defaultRayServicePort {
		t.Errorf("expected port %d, got %d", defaultRayServicePort, result.Endpoint.Port)
	}
}

func TestTranslateStatusReplicas(t *testing.T) {
	st := NewStatusTranslator()
	rs := newRayServiceWithStatus(nil, map[string]interface{}{
		"llm": map[string]interface{}{"status": "RUNNING"},
	})
	// Add worker group specs
	rs.Object["spec"] = map[string]interface{}{
		"rayClusterConfig": map[string]interface{}{
			"workerGroupSpecs": []interface{}{
				map[string]interface{}{
					"replicas": int64(3),
				},
			},
		},
	}

	result, err := st.TranslateStatus(rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Replicas.Desired != 3 {
		t.Errorf("expected desired 3, got %d", result.Replicas.Desired)
	}
	if result.Replicas.Ready != 3 {
		t.Errorf("expected ready 3 (all running), got %d", result.Replicas.Ready)
	}
}

func TestIsReady(t *testing.T) {
	st := NewStatusTranslator()

	if st.IsReady(nil) {
		t.Error("expected not ready for nil")
	}

	// No status
	rs := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "test"},
		},
	}
	if st.IsReady(rs) {
		t.Error("expected not ready with no status")
	}

	// Condition ready
	rs = newRayServiceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "RayServiceReady",
			"status": "True",
		},
	}, nil)
	if !st.IsReady(rs) {
		t.Error("expected ready when condition is True")
	}

	// Condition not ready, but app running
	rs = newRayServiceWithStatus([]interface{}{
		map[string]interface{}{
			"type":   "SomeOther",
			"status": "True",
		},
	}, map[string]interface{}{
		"llm": map[string]interface{}{"status": "RUNNING"},
	})
	if !st.IsReady(rs) {
		t.Error("expected ready when app is RUNNING")
	}

	// App not running
	rs = newRayServiceWithStatus(nil, map[string]interface{}{
		"llm": map[string]interface{}{"status": "DEPLOYING"},
	})
	if st.IsReady(rs) {
		t.Error("expected not ready when app is DEPLOYING")
	}

	// No app statuses
	rs = newRayServiceWithStatus(nil, nil)
	if st.IsReady(rs) {
		t.Error("expected not ready with no app statuses")
	}
}

func TestGetErrorMessage(t *testing.T) {
	st := NewStatusTranslator()

	// Nil
	if msg := st.GetErrorMessage(nil); msg != "resource not found" {
		t.Errorf("expected 'resource not found', got %s", msg)
	}

	// App failed
	rs := newRayServiceWithStatus(nil, map[string]interface{}{
		"llm": map[string]interface{}{
			"status":  "DEPLOY_FAILED",
			"message": "image pull error",
		},
	})
	if msg := st.GetErrorMessage(rs); msg != "image pull error" {
		t.Errorf("expected 'image pull error', got %s", msg)
	}

	// Condition failed
	rs = newRayServiceWithStatus([]interface{}{
		map[string]interface{}{
			"type":    "SomeCondition",
			"status":  "False",
			"message": "condition error",
		},
	}, nil)
	if msg := st.GetErrorMessage(rs); msg != "condition error" {
		t.Errorf("expected 'condition error', got %s", msg)
	}

	// Fallback
	rs = newRayServiceWithStatus(nil, nil)
	if msg := st.GetErrorMessage(rs); msg != "deployment failed" {
		t.Errorf("expected 'deployment failed', got %s", msg)
	}
}

func TestParseConditionsInvalid(t *testing.T) {
	st := NewStatusTranslator()

	conditions := []interface{}{
		"not-a-map",
		map[string]interface{}{},
		map[string]interface{}{"type": "Valid", "status": "True"},
	}

	condMap := st.parseConditions(conditions)
	if len(condMap) != 1 {
		t.Errorf("expected 1 valid condition, got %d", len(condMap))
	}
}

func TestMapAppStatusesEmptyAndMixed(t *testing.T) {
	st := NewStatusTranslator()

	// Empty
	phase, _ := st.mapAppStatusesToPhase(map[string]interface{}{})
	if phase != "" {
		t.Errorf("expected empty phase for empty statuses, got %s", phase)
	}

	// Mixed: one running, one deploying
	phase, _ = st.mapAppStatusesToPhase(map[string]interface{}{
		"app1": map[string]interface{}{"status": "RUNNING"},
		"app2": map[string]interface{}{"status": "DEPLOYING"},
	})
	if phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying for mixed statuses, got %s", phase)
	}
}

func TestStringVal(t *testing.T) {
	m := map[string]interface{}{
		"key1": "value1",
		"key2": 42,
	}
	if stringVal(m, "key1") != "value1" {
		t.Error("expected 'value1'")
	}
	if stringVal(m, "key2") != "" {
		t.Error("expected empty for non-string")
	}
	if stringVal(m, "missing") != "" {
		t.Error("expected empty for missing key")
	}
}

func TestMapConditionsToPhaseNoMatch(t *testing.T) {
	st := NewStatusTranslator()

	condMap := map[string]conditionInfo{
		"Other": {Status: "True"},
	}
	_, _, found := st.mapConditionsToPhase(condMap)
	if found {
		t.Error("expected found=false when no RayServiceReady condition")
	}
}
