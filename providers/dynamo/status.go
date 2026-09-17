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
	// DGDR has a distinct uppercase phase model and status shape from direct DGD resources.
	if upstream.GetKind() == DynamoGraphDeploymentRequestKind {
		return t.translateDGDRStatus(upstream)
	}

	result := &ProviderStatusResult{
		ResourceName: upstream.GetName(),
		ResourceKind: DynamoGraphDeploymentKind,
		Phase:        airunwayv1alpha1.DeploymentPhasePending,
	}

	// Get status object
	status, found, err := unstructured.NestedMap(upstream.Object, "status")
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}
	if !found {
		return result, nil
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

	// Extract endpoint information if available
	result.Endpoint = t.extractEndpoint(upstream, status)

	return result, nil
}

// translateDGDRStatus maps Dynamo's profiling lifecycle onto ModelDeployment status.
func (t *StatusTranslator) translateDGDRStatus(upstream *unstructured.Unstructured) (*ProviderStatusResult, error) {
	result := &ProviderStatusResult{
		ResourceName: upstream.GetName(),
		ResourceKind: DynamoGraphDeploymentRequestKind,
		Phase:        airunwayv1alpha1.DeploymentPhasePending,
	}

	status, found, err := unstructured.NestedMap(upstream.Object, "status")
	if err != nil {
		return nil, fmt.Errorf("failed to get DGDR status: %w", err)
	}
	if !found {
		result.Message = "Dynamo deployment request is pending"
		return result, nil
	}

	phase, _, _ := unstructured.NestedString(status, "phase")
	result.Phase, result.Message = t.mapDGDRPhase(upstream, status, phase)
	result.Replicas = extractDGDRReplicas(status)
	result.Endpoint = extractDGDREndpoint(upstream, status, result.Phase)

	return result, nil
}

// mapDGDRPhase keeps lifecycle translation separate from status detail extraction.
func (t *StatusTranslator) mapDGDRPhase(
	upstream *unstructured.Unstructured,
	status map[string]any,
	phase string,
) (airunwayv1alpha1.DeploymentPhase, string) {
	switch phase {
	case "Pending", "":
		return airunwayv1alpha1.DeploymentPhasePending, "Dynamo deployment request is pending"
	case "Profiling":
		profilingPhase, _, _ := unstructured.NestedString(status, "profilingPhase")
		if profilingPhase != "" {
			return airunwayv1alpha1.DeploymentPhaseDeploying,
				fmt.Sprintf("Dynamo is profiling the deployment (%s)", profilingPhase)
		}
		return airunwayv1alpha1.DeploymentPhaseDeploying, "Dynamo is profiling the deployment"
	case "Ready":
		autoApply, found, _ := unstructured.NestedBool(upstream.Object, "spec", "autoApply")
		if found && !autoApply {
			return airunwayv1alpha1.DeploymentPhaseDeploying,
				"Dynamo deployment plan is ready; autoApply is false"
		}
		return airunwayv1alpha1.DeploymentPhaseDeploying,
			"Dynamo deployment plan is ready and waiting to be applied"
	case "Deploying":
		return airunwayv1alpha1.DeploymentPhaseDeploying,
			"Dynamo is creating the generated deployment"
	case "Deployed":
		return airunwayv1alpha1.DeploymentPhaseRunning, "Dynamo deployment is running"
	case "Failed":
		return airunwayv1alpha1.DeploymentPhaseFailed, dgdrFailureMessage(status)
	default:
		return airunwayv1alpha1.DeploymentPhasePending,
			fmt.Sprintf("Dynamo deployment request has unknown phase %q", phase)
	}
}

// extractDGDRReplicas reads Dynamo's generated-deployment readiness summary.
func extractDGDRReplicas(status map[string]any) *airunwayv1alpha1.ReplicaStatus {
	if deploymentInfo, found, _ := unstructured.NestedMap(status, "deploymentInfo"); found {
		replicas := &airunwayv1alpha1.ReplicaStatus{}
		if desired, ok := deploymentInfo["replicas"].(int64); ok {
			replicas.Desired = int32(desired)
		}
		if available, ok := deploymentInfo["availableReplicas"].(int64); ok {
			replicas.Ready = int32(available)
			replicas.Available = int32(available)
		}
		return replicas
	}
	return nil
}

// extractDGDREndpoint exposes the standalone frontend only after deployment succeeds.
func extractDGDREndpoint(
	upstream *unstructured.Unstructured,
	status map[string]any,
	phase airunwayv1alpha1.DeploymentPhase,
) *airunwayv1alpha1.EndpointStatus {
	if phase != airunwayv1alpha1.DeploymentPhaseRunning {
		return nil
	}
	dgdName, found, _ := unstructured.NestedString(status, "dgdName")
	if !found || dgdName == "" {
		dgdName = upstream.GetName()
	}
	return &airunwayv1alpha1.EndpointStatus{
		Service: fmt.Sprintf("%s-frontend", dgdName),
		Port:    8000,
	}
}

// dgdrFailureMessage extracts the most useful condition message without assuming a version-specific top-level field.
func dgdrFailureMessage(status map[string]any) string {
	if message, found, _ := unstructured.NestedString(status, "message"); found && message != "" {
		return message
	}
	conditions, found, _ := unstructured.NestedSlice(status, "conditions")
	if found {
		for i := len(conditions) - 1; i >= 0; i-- {
			condition, ok := conditions[i].(map[string]any)
			if !ok {
				continue
			}
			if message, ok := condition["message"].(string); ok && message != "" {
				return message
			}
		}
	}
	return "Dynamo deployment request failed"
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
	services, found, _ := unstructured.NestedMap(status, "services")
	if found {
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
		endpoint.Service = fmt.Sprintf("%s-frontend", upstream.GetName())
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
func hasFrontendService(upstream *unstructured.Unstructured) bool {
	services, found, _ := unstructured.NestedMap(upstream.Object, "spec", "services")
	if !found {
		return false
	}

	_, hasFrontend := services["Frontend"]
	return hasFrontend
}

// IsReady checks if the DynamoGraphDeployment is ready
func (t *StatusTranslator) IsReady(upstream *unstructured.Unstructured) bool {
	if upstream == nil {
		return false
	}
	// Reuse the kind-aware translation so callers do not need separate DGDR readiness logic.
	if upstream.GetKind() == DynamoGraphDeploymentRequestKind {
		result, err := t.TranslateStatus(upstream)
		return err == nil && result.Phase == airunwayv1alpha1.DeploymentPhaseRunning
	}

	state, found, err := unstructured.NestedString(upstream.Object, "status", "state")
	if err != nil || !found {
		return false
	}

	return DynamoState(state) == DynamoStateSuccessful
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
