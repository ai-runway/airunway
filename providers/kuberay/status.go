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

package kuberay

import (
	"fmt"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
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
	// defaultRayServicePort is the default service port for RayService
	defaultRayServicePort int32 = 8000

	// RayService condition types
	conditionRayServiceReady = "RayServiceReady"
)

// RayServiceStatus represents application status values
type RayServiceAppStatus string

const (
	AppStatusRunning      RayServiceAppStatus = "RUNNING"
	AppStatusDeploying    RayServiceAppStatus = "DEPLOYING"
	AppStatusDeployFailed RayServiceAppStatus = "DEPLOY_FAILED"
	AppStatusNotStarted   RayServiceAppStatus = "NOT_STARTED"
)

// StatusTranslator handles translating RayService status to ModelDeployment status
type StatusTranslator struct{}

// NewStatusTranslator creates a new status translator
func NewStatusTranslator() *StatusTranslator {
	return &StatusTranslator{}
}

// TranslateStatus converts RayService status to ModelDeployment status fields
func (t *StatusTranslator) TranslateStatus(upstream *unstructured.Unstructured) (*ProviderStatusResult, error) {
	if upstream == nil {
		return nil, fmt.Errorf("upstream resource is nil")
	}

	result := &ProviderStatusResult{
		ResourceName: upstream.GetName(),
		ResourceKind: RayServiceKind,
		Phase:        airunwayv1alpha1.DeploymentPhasePending,
	}

	// Check conditions first
	conditions, condFound, err := unstructured.NestedSlice(upstream.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("failed to get status conditions: %w", err)
	}
	if condFound && len(conditions) > 0 {
		condMap := t.parseConditions(conditions)
		if phase, message, found := t.mapConditionsToPhase(condMap); found {
			result.Phase = phase
			result.Message = message
		}
	}

	// Check applicationStatuses for more detailed status
	appStatuses, appFound, _ := unstructured.NestedMap(upstream.Object, "status", "activeServiceStatus", "applicationStatuses")
	if appFound && len(appStatuses) > 0 {
		phase, message := t.mapAppStatusesToPhase(appStatuses)
		if phase != "" {
			result.Phase = phase
			if message != "" {
				result.Message = message
			}
		}
	}

	// Extract replica information
	result.Replicas = t.extractReplicas(upstream)

	// Extract endpoint information
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

// mapConditionsToPhase determines the deployment phase from RayService conditions
func (t *StatusTranslator) mapConditionsToPhase(condMap map[string]conditionInfo) (airunwayv1alpha1.DeploymentPhase, string, bool) {
	// RayServiceReady=True → Running
	if rs, ok := condMap[conditionRayServiceReady]; ok {
		if rs.Status == "True" {
			return airunwayv1alpha1.DeploymentPhaseRunning, "", true
		}
		if rs.Status == "False" {
			return airunwayv1alpha1.DeploymentPhaseDeploying, rs.Message, true
		}
	}

	return "", "", false
}

// mapAppStatusesToPhase determines phase from application statuses
func (t *StatusTranslator) mapAppStatusesToPhase(appStatuses map[string]interface{}) (airunwayv1alpha1.DeploymentPhase, string) {
	allRunning := true
	anyFailed := false
	var failMessage string

	for _, appStatus := range appStatuses {
		app, ok := appStatus.(map[string]interface{})
		if !ok {
			continue
		}
		status, _ := app["status"].(string)
		message, _ := app["message"].(string)

		switch RayServiceAppStatus(status) {
		case AppStatusRunning:
			// OK
		case AppStatusDeployFailed:
			anyFailed = true
			allRunning = false
			failMessage = message
		default:
			allRunning = false
		}
	}

	if anyFailed {
		return airunwayv1alpha1.DeploymentPhaseFailed, failMessage
	}
	if allRunning && len(appStatuses) > 0 {
		return airunwayv1alpha1.DeploymentPhaseRunning, ""
	}
	if len(appStatuses) > 0 {
		return airunwayv1alpha1.DeploymentPhaseDeploying, ""
	}
	return "", ""
}

// extractReplicas extracts replica information from the RayService
func (t *StatusTranslator) extractReplicas(upstream *unstructured.Unstructured) *airunwayv1alpha1.ReplicaStatus {
	replicas := &airunwayv1alpha1.ReplicaStatus{}

	// Try to get desired replicas from spec workerGroupSpecs
	workerGroups, found, _ := unstructured.NestedSlice(upstream.Object, "spec", "rayClusterConfig", "workerGroupSpecs")
	if found {
		var totalDesired int32
		for _, wg := range workerGroups {
			if group, ok := wg.(map[string]interface{}); ok {
				if r, ok := group["replicas"].(int64); ok {
					totalDesired += int32(r)
				}
			}
		}
		replicas.Desired = totalDesired
	}

	// If service is running, assume replicas are ready
	appStatuses, appFound, _ := unstructured.NestedMap(upstream.Object, "status", "activeServiceStatus", "applicationStatuses")
	if appFound {
		allRunning := true
		for _, appStatus := range appStatuses {
			if app, ok := appStatus.(map[string]interface{}); ok {
				status, _ := app["status"].(string)
				if RayServiceAppStatus(status) != AppStatusRunning {
					allRunning = false
					break
				}
			}
		}
		if allRunning && len(appStatuses) > 0 {
			replicas.Ready = replicas.Desired
			replicas.Available = replicas.Desired
		}
	}

	return replicas
}

// extractEndpoint extracts service endpoint information for the RayService
func (t *StatusTranslator) extractEndpoint(upstream *unstructured.Unstructured) *airunwayv1alpha1.EndpointStatus {
	return &airunwayv1alpha1.EndpointStatus{
		// RayService creates a service with the name <rayservice-name>-serve-svc
		Service: fmt.Sprintf("%s-serve-svc", upstream.GetName()),
		Port:    defaultRayServicePort,
	}
}

// IsReady checks if the RayService is ready
func (t *StatusTranslator) IsReady(upstream *unstructured.Unstructured) bool {
	if upstream == nil {
		return false
	}

	// Check conditions
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
		if condType == conditionRayServiceReady {
			status, _ := cond["status"].(string)
			return status == "True"
		}
	}

	// Fallback: check application statuses
	appStatuses, found, _ := unstructured.NestedMap(upstream.Object, "status", "activeServiceStatus", "applicationStatuses")
	if !found {
		return false
	}

	for _, appStatus := range appStatuses {
		if app, ok := appStatus.(map[string]interface{}); ok {
			status, _ := app["status"].(string)
			if RayServiceAppStatus(status) != AppStatusRunning {
				return false
			}
		}
	}

	return len(appStatuses) > 0
}

// GetErrorMessage extracts error messages from a failed RayService
func (t *StatusTranslator) GetErrorMessage(upstream *unstructured.Unstructured) string {
	if upstream == nil {
		return "resource not found"
	}

	// Check application statuses for errors
	appStatuses, found, _ := unstructured.NestedMap(upstream.Object, "status", "activeServiceStatus", "applicationStatuses")
	if found {
		for _, appStatus := range appStatuses {
			if app, ok := appStatus.(map[string]interface{}); ok {
				status, _ := app["status"].(string)
				if RayServiceAppStatus(status) == AppStatusDeployFailed {
					if message, ok := app["message"].(string); ok && message != "" {
						return message
					}
				}
			}
		}
	}

	// Check conditions
	conditions, found, _ := unstructured.NestedSlice(upstream.Object, "status", "conditions")
	if found {
		for _, c := range conditions {
			if condition, ok := c.(map[string]interface{}); ok {
				status, _ := condition["status"].(string)
				if status == "False" {
					if message, ok := condition["message"].(string); ok && message != "" {
						return message
					}
				}
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
