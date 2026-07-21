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

package llmd

import (
	"fmt"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ProviderStatusResult contains the status fields extracted from an upstream Deployment.
type ProviderStatusResult struct {
	Phase        airunwayv1alpha1.DeploymentPhase
	Message      string
	Replicas     *airunwayv1alpha1.ReplicaStatus
	Endpoint     *airunwayv1alpha1.EndpointStatus
	ResourceName string
	ResourceKind string
}

// Kubernetes Deployment condition types
const (
	conditionAvailable   = "Available"
	conditionProgressing = "Progressing"
)

// StatusTranslator handles translating Kubernetes Deployment status to ModelDeployment status
type StatusTranslator struct{}

// NewStatusTranslator creates a new status translator
func NewStatusTranslator() *StatusTranslator {
	return &StatusTranslator{}
}

// TranslateStatus converts a Kubernetes Deployment status to ModelDeployment status fields.
// The upstream resource must be an apps/v1 Deployment.
func (t *StatusTranslator) TranslateStatus(upstream *unstructured.Unstructured) (*ProviderStatusResult, error) {
	if upstream == nil {
		return nil, fmt.Errorf("upstream resource is nil")
	}

	result := &ProviderStatusResult{
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

	result.Phase, result.Message = t.mapConditionsToPhase(condMap)
	result.Replicas = t.extractReplicas(upstream)
	result.Endpoint = t.extractEndpoint(upstream)

	return result, nil
}

// conditionInfo holds parsed Deployment condition fields.
type conditionInfo struct {
	Status  string
	Message string
	Reason  string
}

// parseConditions converts the unstructured conditions slice to a map keyed by type.
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

// mapConditionsToPhase maps Kubernetes Deployment conditions to a ModelDeployment phase.
//
// Mapping logic:
//   - Available=True → Running
//   - Available=False AND Progressing=True → Deploying
//   - Progressing=False (DeadlineExceeded) OR Available=False with reason → Failed
//   - else → Pending
func (t *StatusTranslator) mapConditionsToPhase(condMap map[string]conditionInfo) (airunwayv1alpha1.DeploymentPhase, string) {
	avail, hasAvail := condMap[conditionAvailable]
	prog, hasProg := condMap[conditionProgressing]

	// Available = True means all desired replicas are up
	if hasAvail && avail.Status == "True" {
		return airunwayv1alpha1.DeploymentPhaseRunning, ""
	}

	// Progressing=False with DeadlineExceeded is a hard failure
	if hasProg && prog.Status == "False" && prog.Reason == "ProgressDeadlineExceeded" {
		msg := prog.Message
		if msg == "" {
			msg = "deployment timed out waiting for rollout"
		}
		return airunwayv1alpha1.DeploymentPhaseFailed, msg
	}

	// Progressing=True and Available=False → still rolling out
	if hasProg && prog.Status == "True" {
		return airunwayv1alpha1.DeploymentPhaseDeploying, ""
	}

	// Available=False with an explicit failure message
	if hasAvail && avail.Status == "False" && avail.Message != "" {
		return airunwayv1alpha1.DeploymentPhaseFailed, avail.Message
	}

	return airunwayv1alpha1.DeploymentPhasePending, ""
}

// extractReplicas extracts replica counts from Deployment status.
func (t *StatusTranslator) extractReplicas(upstream *unstructured.Unstructured) *airunwayv1alpha1.ReplicaStatus {
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

// extractEndpoint returns the Service endpoint for this Deployment.
// The Service name matches the Deployment name by convention.
func (t *StatusTranslator) extractEndpoint(upstream *unstructured.Unstructured) *airunwayv1alpha1.EndpointStatus {
	// For disaggregated decode deployments the name ends in "-decode";
	// the service name matches the deployment name.
	return &airunwayv1alpha1.EndpointStatus{
		Service: upstream.GetName(),
		Port:    int32(DefaultVLLMPort),
	}
}

// stringVal safely extracts a string value from a map.
func stringVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}
