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

package dynamo

import (
	"fmt"
	"strings"

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

// DynamoState represents the state of a DynamoGraphDeployment
type DynamoState string

const (
	// DynamoStateInitializing indicates the deployment is initializing.
	DynamoStateInitializing DynamoState = "initializing"
	// DynamoStateDeploying indicates the deployment is in progress
	DynamoStateDeploying DynamoState = "deploying"
	// DynamoStateSuccessful indicates the deployment is successful
	DynamoStateSuccessful DynamoState = "successful"
	// DynamoStateFailed indicates the deployment has failed
	DynamoStateFailed DynamoState = "failed"
	// DynamoStatePending indicates the deployment is pending
	DynamoStatePending DynamoState = "pending"
)

// StatusTranslator handles translating DynamoGraphDeployment status to ModelDeployment status
type StatusTranslator struct{}

// NewStatusTranslator creates a new status translator
func NewStatusTranslator() *StatusTranslator {
	return &StatusTranslator{}
}

// TranslateStatus converts DynamoGraphDeployment status to ModelDeployment status fields
func (t *StatusTranslator) TranslateStatus(upstream *unstructured.Unstructured) (*ProviderStatusResult, error) {
	if upstream == nil {
		return nil, fmt.Errorf("upstream resource is nil")
	}

	result := &ProviderStatusResult{
		ResourceName: upstream.GetName(),
		ResourceKind: upstream.GetKind(),
		Phase:        airunwayv1alpha1.DeploymentPhasePending,
	}
	if result.ResourceKind == "" {
		result.ResourceKind = DynamoGraphDeploymentKind
	}

	// Get status object
	status, found, err := unstructured.NestedMap(upstream.Object, "status")
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}
	if !found {
		return result, nil
	}
	if upstream.GetKind() == DynamoGraphDeploymentRequestKind {
		return t.translateDGDRStatus(result, status), nil
	}

	// Extract state field
	state, found, err := unstructured.NestedString(status, "state")
	if err != nil {
		return nil, fmt.Errorf("failed to get state: %w", err)
	}
	if found {
		result.Phase = t.mapStateToPhase(DynamoState(state))
	}

	// Extract message field
	message, found, err := unstructured.NestedString(status, "message")
	if err == nil && found {
		result.Message = message
	}

	// Extract replica information if available
	result.Replicas = t.extractReplicas(status)

	// Stale or partially available component status must not advertise readiness.
	if result.Phase == airunwayv1alpha1.DeploymentPhaseRunning {
		observed, hasObserved, _ := unstructured.NestedInt64(status, "observedGeneration")
		if upstream.GetDeletionTimestamp() != nil || (hasObserved && observed < upstream.GetGeneration()) || (result.Replicas != nil && (result.Replicas.Ready < result.Replicas.Desired || result.Replicas.Available < result.Replicas.Desired)) {
			result.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
		}
		conditions, _, _ := unstructured.NestedSlice(status, "conditions")
		for _, raw := range conditions {
			condition, _ := raw.(map[string]any)
			if condition["type"] == "Ready" && condition["status"] != "True" {
				result.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
				if msg, ok := condition["message"].(string); ok {
					result.Message = msg
				}
			}
		}
	}
	// Extract endpoint information if available
	result.Endpoint = t.extractEndpoint(upstream, status)

	return result, nil
}

func (t *StatusTranslator) translateDGDRStatus(
	result *ProviderStatusResult,
	status map[string]any,
) *ProviderStatusResult {
	phase, _, _ := unstructured.NestedString(status, "phase")
	switch phase {
	case "Deployed":
		result.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
	case "Profiling", "Ready", "Deploying":
		result.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
	case "Failed":
		result.Phase = airunwayv1alpha1.DeploymentPhaseFailed
	default:
		result.Phase = airunwayv1alpha1.DeploymentPhasePending
	}

	if profilingPhase, found, _ := unstructured.NestedString(status, "profilingPhase"); found && profilingPhase != "" {
		result.Message = fmt.Sprintf("Dynamo profiling: %s", profilingPhase)
	}
	if conditions, found, _ := unstructured.NestedSlice(status, "conditions"); found {
		for _, rawCondition := range conditions {
			condition, ok := rawCondition.(map[string]any)
			if !ok || condition["type"] != "Succeeded" {
				continue
			}
			if message, ok := condition["message"].(string); ok && message != "" {
				result.Message = message
			}
			break
		}
	}

	desired, desiredFound, _ := unstructured.NestedInt64(status, "deploymentInfo", "replicas")
	available, availableFound, _ := unstructured.NestedInt64(status, "deploymentInfo", "availableReplicas")
	if desiredFound || availableFound {
		result.Replicas = &airunwayv1alpha1.ReplicaStatus{
			Desired:   int32(desired),
			Ready:     int32(available),
			Available: int32(available),
		}
	}
	return result
}

// mapStateToPhase converts Dynamo state to ModelDeployment phase
func (t *StatusTranslator) mapStateToPhase(state DynamoState) airunwayv1alpha1.DeploymentPhase {
	switch state {
	case DynamoStateSuccessful:
		return airunwayv1alpha1.DeploymentPhaseRunning
	case DynamoStateInitializing:
		return airunwayv1alpha1.DeploymentPhaseDeploying
	case DynamoStateDeploying:
		return airunwayv1alpha1.DeploymentPhaseDeploying
	case DynamoStateFailed:
		return airunwayv1alpha1.DeploymentPhaseFailed
	case DynamoStatePending:
		return airunwayv1alpha1.DeploymentPhasePending
	default:
		return airunwayv1alpha1.DeploymentPhasePending
	}
}

// extractReplicas extracts replica information from the status
func (t *StatusTranslator) extractReplicas(status map[string]interface{}) *airunwayv1alpha1.ReplicaStatus {
	replicas := &airunwayv1alpha1.ReplicaStatus{}

	// Try to get replica counts from various possible locations
	// Dynamo may report these in different ways depending on the version

	// Check for services status
	services := dynamoComponents(map[string]any{"status": status}, "status")
	if services != nil {
		var totalDesired, totalReady, totalAvailable int32
		for _, svcStatus := range services {
			if svc, ok := svcStatus.(map[string]interface{}); ok {
				if desired, ok := svc["replicas"].(int64); ok {
					totalDesired += int32(desired)
				}
				ready, hasReady := svc["readyReplicas"].(int64)
				available, hasAvailable := svc["availableReplicas"].(int64)
				if hasReady {
					totalReady += int32(ready)
				} else if hasAvailable {
					// VllmWorker (PodCliqueScalingGroup) reports availableReplicas but not readyReplicas
					totalReady += int32(available)
				}
				if hasAvailable {
					totalAvailable += int32(available)
				} else if hasReady {
					totalAvailable += int32(ready)
				}
			}
		}
		replicas.Desired = totalDesired
		replicas.Ready = totalReady
		replicas.Available = totalAvailable
	}

	// Check for direct replica fields
	if desired, found, _ := unstructured.NestedInt64(status, "desiredReplicas"); found {
		replicas.Desired = int32(desired)
	}
	if ready, found, _ := unstructured.NestedInt64(status, "readyReplicas"); found {
		replicas.Ready = int32(ready)
	}
	if available, found, _ := unstructured.NestedInt64(status, "availableReplicas"); found {
		replicas.Available = int32(available)
	}

	return replicas
}

// extractEndpoint extracts service endpoint information
func (t *StatusTranslator) extractEndpoint(upstream *unstructured.Unstructured, status map[string]interface{}) *airunwayv1alpha1.EndpointStatus {
	endpoint := &airunwayv1alpha1.EndpointStatus{}

	// Check for endpoint in status
	if serviceName, found, _ := unstructured.NestedString(status, "endpoint", "service"); found {
		endpoint.Service = serviceName
	} else {
		// Dynamo does not report an endpoint, so only infer one when its rendered
		// spec includes the standalone Frontend service.
		if !hasFrontendService(upstream) {
			return nil
		}
		// Default to deployment name + "-frontend"
		endpoint.Service = fmt.Sprintf("%s-%s", upstream.GetName(), strings.ToLower(frontendComponent(upstream)))
	}

	if port, found, _ := unstructured.NestedInt64(status, "endpoint", "port"); found {
		endpoint.Port = int32(port)
	} else {
		// Default Dynamo frontend port
		endpoint.Port = 8000
	}

	return endpoint
}

// hasFrontendService reads the spec to check if a Frontend service exists.
func frontendComponent(upstream *unstructured.Unstructured) string {
	for name, raw := range dynamoComponents(upstream.Object, "spec") {
		component, _ := raw.(map[string]any)
		kind := dynamoComponentType(component)
		if strings.EqualFold(kind, "frontend") || (kind == "" && name == "Frontend") {
			return name
		}
	}
	return ""
}
func hasFrontendService(upstream *unstructured.Unstructured) bool {
	return frontendComponent(upstream) != ""
}

// IsReady checks if the DynamoGraphDeployment is ready
func (t *StatusTranslator) IsReady(upstream *unstructured.Unstructured) bool {
	if upstream == nil {
		return false
	}

	result, err := t.TranslateStatus(upstream)
	return err == nil && result.Phase == airunwayv1alpha1.DeploymentPhaseRunning
}

// GetErrorMessage extracts error messages from a failed deployment
func (t *StatusTranslator) GetErrorMessage(upstream *unstructured.Unstructured) string {
	if upstream == nil {
		return "resource not found"
	}

	// Check for message in status
	if message, found, _ := unstructured.NestedString(upstream.Object, "status", "message"); found && message != "" {
		return message
	}

	// Check for error in status
	if errMsg, found, _ := unstructured.NestedString(upstream.Object, "status", "error"); found && errMsg != "" {
		return errMsg
	}

	// Check conditions for error details
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
