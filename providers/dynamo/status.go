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

// DynamoGraphDeploymentRequestPhase represents the lifecycle reported by DGDR.
type DynamoGraphDeploymentRequestPhase string

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

	// DGDR phases are title-cased by the v1beta1 API.
	DynamoGraphDeploymentRequestPhasePending   DynamoGraphDeploymentRequestPhase = "Pending"
	DynamoGraphDeploymentRequestPhaseProfiling DynamoGraphDeploymentRequestPhase = "Profiling"
	DynamoGraphDeploymentRequestPhaseReady     DynamoGraphDeploymentRequestPhase = "Ready"
	DynamoGraphDeploymentRequestPhaseDeploying DynamoGraphDeploymentRequestPhase = "Deploying"
	DynamoGraphDeploymentRequestPhaseDeployed  DynamoGraphDeploymentRequestPhase = "Deployed"
	DynamoGraphDeploymentRequestPhaseFailed    DynamoGraphDeploymentRequestPhase = "Failed"
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
	if upstream.GetKind() == DynamoGraphDeploymentRequestKind {
		// DGDR has a distinct phase and deploymentInfo schema, so translate it
		// before entering the legacy DGD state-based path.
		return t.translateDGDRStatus(upstream)
	}

	result := &ProviderStatusResult{
		ResourceName: upstream.GetName(),
		ResourceKind: upstream.GetKind(),
		Phase:        airunwayv1alpha1.DeploymentPhasePending,
	}
	if result.ResourceKind == "" {
		// Keep direct unit callers that omit TypeMeta compatible with the DGD path.
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

// translateDGDRStatus maps the installed v1beta1 request lifecycle to the
// provider-neutral ModelDeployment status consumed by AI Runway.
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
		return result, nil
	}

	phase, phaseFound, err := unstructured.NestedString(status, "phase")
	if err != nil {
		return nil, fmt.Errorf("failed to get DGDR phase: %w", err)
	}
	if phaseFound {
		result.Phase = t.mapDGDRPhaseToPhase(DynamoGraphDeploymentRequestPhase(phase))
	}
	if phase == string(DynamoGraphDeploymentRequestPhaseReady) {
		if autoApply, found, _ := unstructured.NestedBool(upstream.Object, "spec", "autoApply"); found && !autoApply {
			result.Message = "DGDR plan is ready; autoApply is false"
		}
	}

	// DGDR reports useful diagnostics through conditions rather than a top-level
	// status.message field, so surface the first non-empty condition message.
	if conditions, found, _ := unstructured.NestedSlice(status, "conditions"); found {
		for _, item := range conditions {
			condition, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if message, ok := condition["message"].(string); ok && message != "" {
				result.Message = message
				if conditionStatus, _ := condition["status"].(string); conditionStatus == "False" {
					break
				}
			}
		}
	}

	if deploymentInfo, found, _ := unstructured.NestedMap(status, "deploymentInfo"); found {
		replicas := &airunwayv1alpha1.ReplicaStatus{}
		if desired, found, _ := unstructured.NestedInt64(deploymentInfo, "replicas"); found {
			replicas.Desired = int32(desired)
		}
		if available, found, _ := unstructured.NestedInt64(deploymentInfo, "availableReplicas"); found {
			replicas.Ready = int32(available)
			replicas.Available = int32(available)
		}
		result.Replicas = replicas
	}

	if result.Phase == airunwayv1alpha1.DeploymentPhaseRunning {
		dgdName, found, _ := unstructured.NestedString(status, "dgdName")
		if !found || dgdName == "" {
			dgdName = upstream.GetName()
		}
		result.Endpoint = &airunwayv1alpha1.EndpointStatus{
			Service: fmt.Sprintf("%s-frontend", dgdName),
			Port:    8000,
		}
	}

	return result, nil
}

// mapDGDRPhaseToPhase collapses profiling-specific states into AI Runway's
// existing deployment phase vocabulary.
func (t *StatusTranslator) mapDGDRPhaseToPhase(phase DynamoGraphDeploymentRequestPhase) airunwayv1alpha1.DeploymentPhase {
	switch phase {
	case DynamoGraphDeploymentRequestPhaseDeployed:
		return airunwayv1alpha1.DeploymentPhaseRunning
	case DynamoGraphDeploymentRequestPhaseFailed:
		return airunwayv1alpha1.DeploymentPhaseFailed
	case DynamoGraphDeploymentRequestPhaseProfiling,
		DynamoGraphDeploymentRequestPhaseReady,
		DynamoGraphDeploymentRequestPhaseDeploying:
		return airunwayv1alpha1.DeploymentPhaseDeploying
	case DynamoGraphDeploymentRequestPhasePending:
		return airunwayv1alpha1.DeploymentPhasePending
	default:
		return airunwayv1alpha1.DeploymentPhasePending
	}
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
	if upstream.GetKind() == DynamoGraphDeploymentRequestKind {
		// A DGDR is ready only after autoApply has produced a healthy DGD.
		phase, found, err := unstructured.NestedString(upstream.Object, "status", "phase")
		return err == nil && found && DynamoGraphDeploymentRequestPhase(phase) == DynamoGraphDeploymentRequestPhaseDeployed
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
