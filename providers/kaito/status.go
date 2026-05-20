/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kaito

import (
	"fmt"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ProviderStatusResult contains the status fields extracted from an upstream resource.
// Defined locally to avoid importing the controller's internal providers package,
// keeping this provider self-contained for out-of-tree use.
type ProviderStatusResult struct {
	Phase        airunwayv1alpha1.DeploymentPhase
	Message      string
	Replicas     *airunwayv1alpha1.ReplicaStatus
	Endpoint     *airunwayv1alpha1.EndpointStatus
	ResourceName string
	ResourceKind string
}

const (
	// defaultKAITOPort is the default service port for KAITO preset deployments
	defaultKAITOPort int32 = 80

	// KAITO condition types
	conditionWorkspaceSucceeded = "WorkspaceSucceeded"
	conditionResourceReady      = "ResourceReady"
	conditionInferenceReady     = "InferenceReady"
)

// StatusTranslator handles translating KAITO Workspace status to ModelDeployment status
type StatusTranslator struct{}

// NewStatusTranslator creates a new status translator
func NewStatusTranslator() *StatusTranslator {
	return &StatusTranslator{}
}

// TranslateStatus converts KAITO Workspace status to ModelDeployment status fields
func (t *StatusTranslator) TranslateStatus(upstream *unstructured.Unstructured) (*ProviderStatusResult, error) {
	if upstream == nil {
		return nil, fmt.Errorf("upstream resource is nil")
	}

	result := &ProviderStatusResult{
		ResourceName: upstream.GetName(),
		ResourceKind: WorkspaceKind,
		Phase:        airunwayv1alpha1.DeploymentPhasePending,
	}

	// Try status.state first (KAITO 0.10.0+)
	state, stateFound, _ := unstructured.NestedString(upstream.Object, "status", "state")

	// Always parse conditions when present (needed for replicas regardless of phase source)
	var condMap map[string]conditionInfo
	conditions, condFound, err := unstructured.NestedSlice(upstream.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("failed to get status conditions: %w", err)
	}
	if condFound && len(conditions) > 0 {
		condMap = t.parseConditions(conditions)
	}

	// Determine phase: prefer state field, fall back to conditions
	if stateFound && state != "" {
		result.Phase, result.Message = t.mapStateToPhase(state)
	} else if condMap != nil {
		result.Phase, result.Message = t.mapConditionsToPhase(condMap)
	} else {
		// No state and no conditions — stay at default Pending
		return result, nil
	}

	// Always extract replicas and endpoint (independent of phase source)
	if condMap == nil {
		condMap = make(map[string]conditionInfo)
	}
	result.Replicas = t.extractReplicas(upstream, condMap)
	result.Endpoint = t.extractEndpoint(upstream)

	return result, nil
}

// conditionInfo holds parsed condition fields
type conditionInfo struct {
	Status  string
	Message string
	Reason  string
}

// parseConditions converts the unstructured conditions slice into a map keyed by condition type
func (t *StatusTranslator) parseConditions(conditions []interface{}) map[string]conditionInfo {
	condMap := make(map[string]conditionInfo)
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
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

// mapConditionsToPhase determines the deployment phase from KAITO conditions
func (t *StatusTranslator) mapConditionsToPhase(condMap map[string]conditionInfo) (airunwayv1alpha1.DeploymentPhase, string) {
	// WorkspaceSucceeded=True → Running
	if ws, ok := condMap[conditionWorkspaceSucceeded]; ok {
		if ws.Status == "True" {
			return airunwayv1alpha1.DeploymentPhaseRunning, ""
		}
		if ws.Status == "False" {
			return airunwayv1alpha1.DeploymentPhaseFailed, ws.Message
		}
	}

	// ResourceReady=True + InferenceReady not True → Deploying
	if rr, ok := condMap[conditionResourceReady]; ok && rr.Status == "True" {
		ir, irFound := condMap[conditionInferenceReady]
		if !irFound || ir.Status != "True" {
			return airunwayv1alpha1.DeploymentPhaseDeploying, ""
		}
	}

	return airunwayv1alpha1.DeploymentPhasePending, ""
}

// mapStateToPhase maps the KAITO 0.10.0+ status.state field to a ModelDeployment phase
func (t *StatusTranslator) mapStateToPhase(state string) (airunwayv1alpha1.DeploymentPhase, string) {
	switch state {
	case "Ready":
		return airunwayv1alpha1.DeploymentPhaseRunning, ""
	case "Succeeded":
		return airunwayv1alpha1.DeploymentPhaseRunning, ""
	case "NotReady":
		return airunwayv1alpha1.DeploymentPhaseDeploying, ""
	case "Running":
		return airunwayv1alpha1.DeploymentPhaseDeploying, "fine-tuning in progress"
	case "Failed":
		return airunwayv1alpha1.DeploymentPhaseFailed, ""
	case "Pending":
		return airunwayv1alpha1.DeploymentPhasePending, ""
	default:
		return airunwayv1alpha1.DeploymentPhasePending, fmt.Sprintf("unknown state: %s", state)
	}
}

// extractReplicas extracts replica information from the Workspace spec and conditions
func (t *StatusTranslator) extractReplicas(upstream *unstructured.Unstructured, condMap map[string]conditionInfo) *airunwayv1alpha1.ReplicaStatus {
	replicas := &airunwayv1alpha1.ReplicaStatus{}

	// Try resource.count for desired replicas (resource is at root level in KAITO Workspace)
	if count, found, _ := unstructured.NestedInt64(upstream.Object, "resource", "count"); found {
		replicas.Desired = int32(count)
	}

	// Mark all replicas ready when workspace is in a successful state.
	// Two sources: status.state (KAITO 0.10.0+) or WorkspaceSucceeded condition (older KAITO).
	state, _, _ := unstructured.NestedString(upstream.Object, "status", "state")
	stateReady := state == "Ready" || state == "Succeeded"
	condReady := func() bool {
		ws, ok := condMap[conditionWorkspaceSucceeded]
		return ok && ws.Status == "True"
	}()
	if stateReady || condReady {
		replicas.Ready = replicas.Desired
		replicas.Available = replicas.Desired
	}

	return replicas
}

// extractEndpoint extracts service endpoint information for the Workspace
func (t *StatusTranslator) extractEndpoint(upstream *unstructured.Unstructured) *airunwayv1alpha1.EndpointStatus {
	return &airunwayv1alpha1.EndpointStatus{
		// KAITO creates a service with the same name as the Workspace, always on port 80
		Service: upstream.GetName(),
		Port:    defaultKAITOPort,
	}
}

// IsReady checks if the KAITO Workspace is ready
func (t *StatusTranslator) IsReady(upstream *unstructured.Unstructured) bool {
	if upstream == nil {
		return false
	}

	conditions, found, err := unstructured.NestedSlice(upstream.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		if condType == conditionWorkspaceSucceeded {
			status, _ := cond["status"].(string)
			return status == "True"
		}
	}

	return false
}

// GetErrorMessage extracts error messages from a failed Workspace
func (t *StatusTranslator) GetErrorMessage(upstream *unstructured.Unstructured) string {
	if upstream == nil {
		return "resource not found"
	}

	conditions, found, _ := unstructured.NestedSlice(upstream.Object, "status", "conditions")
	if !found {
		return "deployment failed"
	}

	// Check WorkspaceSucceeded condition first for the most relevant error
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		if condType == conditionWorkspaceSucceeded {
			status, _ := cond["status"].(string)
			if status == "False" {
				if message, ok := cond["message"].(string); ok && message != "" {
					return message
				}
			}
		}
	}

	// Fall back to any condition with status=False
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		status, _ := cond["status"].(string)
		if status == "False" {
			if message, ok := cond["message"].(string); ok && message != "" {
				return message
			}
		}
	}

	return "deployment failed"
}

// stringVal safely extracts a string value from a map
func stringVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}
