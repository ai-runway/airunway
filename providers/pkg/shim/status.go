package shim

import (
	"fmt"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DeploymentStatusResult contains the status fields extracted from an upstream Deployment.
type DeploymentStatusResult struct {
	Phase        airunwayv1alpha1.DeploymentPhase
	Message      string
	Replicas     *airunwayv1alpha1.ReplicaStatus
	Endpoint     *airunwayv1alpha1.EndpointStatus
	ResourceName string
	ResourceKind string
}

const (
	conditionAvailable   = "Available"
	conditionProgressing = "Progressing"
)

// DeploymentStatusTranslator translates Kubernetes Deployment status to ModelDeployment status.
type DeploymentStatusTranslator struct {
	port int32
}

// NewDeploymentStatusTranslator creates a Deployment status translator.
func NewDeploymentStatusTranslator(port int32) *DeploymentStatusTranslator {
	return &DeploymentStatusTranslator{port: port}
}

// TranslateStatus converts a Kubernetes Deployment status to ModelDeployment status fields.
func (t *DeploymentStatusTranslator) TranslateStatus(
	upstream *unstructured.Unstructured,
) (*DeploymentStatusResult, error) {
	if upstream == nil {
		return nil, fmt.Errorf("upstream resource is nil")
	}

	result := &DeploymentStatusResult{
		ResourceName: upstream.GetName(),
		ResourceKind: "Deployment",
		Phase:        airunwayv1alpha1.DeploymentPhasePending,
	}

	conditions, found, err := unstructured.NestedSlice(upstream.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("failed to get status conditions: %w", err)
	}
	if !found || len(conditions) == 0 {
		result.Replicas = t.extractReplicas(upstream)
		result.Endpoint = t.extractEndpoint(upstream)
		return result, nil
	}

	condMap := t.parseConditions(conditions)
	result.Replicas = t.extractReplicas(upstream)

	result.Phase, result.Message = t.mapConditionsToPhase(condMap, result.Replicas)
	result.Endpoint = t.extractEndpoint(upstream)

	return result, nil
}

type conditionInfo struct {
	Status  string
	Message string
	Reason  string
}

func (t *DeploymentStatusTranslator) parseConditions(conditions []any) map[string]conditionInfo {
	condMap := make(map[string]conditionInfo)
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		if condType == "" {
			continue
		}
		condMap[condType] = conditionInfo{
			Status:  stringVal(cond, "status"),
			Message: stringVal(cond, "message"),
			Reason:  stringVal(cond, "reason"),
		}
	}
	return condMap
}

func (t *DeploymentStatusTranslator) mapConditionsToPhase(
	condMap map[string]conditionInfo,
	replicas *airunwayv1alpha1.ReplicaStatus,
) (airunwayv1alpha1.DeploymentPhase, string) {
	avail, hasAvail := condMap[conditionAvailable]
	prog, hasProg := condMap[conditionProgressing]

	if hasAvail && avail.Status == "True" && replicas != nil && replicas.Desired > 0 &&
		replicas.Ready >= replicas.Desired && replicas.Available >= replicas.Desired {
		return airunwayv1alpha1.DeploymentPhaseRunning, ""
	}

	if hasProg && prog.Status == "False" && prog.Reason == "ProgressDeadlineExceeded" {
		msg := prog.Message
		if msg == "" {
			msg = "deployment timed out waiting for rollout"
		}
		return airunwayv1alpha1.DeploymentPhaseFailed, msg
	}

	if hasProg && prog.Status == "True" {
		return airunwayv1alpha1.DeploymentPhaseDeploying, ""
	}

	if hasAvail && avail.Status == "False" && avail.Message != "" {
		return airunwayv1alpha1.DeploymentPhaseFailed, avail.Message
	}

	return airunwayv1alpha1.DeploymentPhasePending, ""
}

func (t *DeploymentStatusTranslator) extractReplicas(
	upstream *unstructured.Unstructured,
) *airunwayv1alpha1.ReplicaStatus {
	replicas := &airunwayv1alpha1.ReplicaStatus{}

	if desired, found, _ := unstructured.NestedInt64(upstream.Object, "spec", "replicas"); found {
		replicas.Desired = int32(desired)
	}
	if ready, found, _ := unstructured.NestedInt64(upstream.Object, "status", "readyReplicas"); found {
		replicas.Ready = int32(ready)
	}
	if available, found, _ := unstructured.NestedInt64(upstream.Object, "status", "availableReplicas"); found {
		replicas.Available = int32(available)
	}

	return replicas
}

func (t *DeploymentStatusTranslator) extractEndpoint(
	upstream *unstructured.Unstructured,
) *airunwayv1alpha1.EndpointStatus {
	return &airunwayv1alpha1.EndpointStatus{
		Service: upstream.GetName(),
		Port:    t.port,
	}
}

func stringVal(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
