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

package vllm

import (
	"fmt"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
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
	conditionAvailable      = "Available"
	conditionProgressing    = "Progressing"
	conditionReplicaFailure = "ReplicaFailure"
	conditionTrue           = "True"
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
	result.Replicas = t.extractReplicas(upstream)

	rolloutComplete := t.isRolloutComplete(upstream, result.Replicas)
	result.Phase, result.Message = t.mapConditionsToPhase(condMap, rolloutComplete)
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
// Available=True means minimum availability, not that the requested revision
// has rolled out. Running additionally requires current-generation status,
// updated desired replicas, no old replicas, and ready and available counts.
func (t *StatusTranslator) mapConditionsToPhase(
	condMap map[string]conditionInfo,
	rolloutComplete bool,
) (airunwayv1alpha1.DeploymentPhase, string) {
	avail, hasAvail := condMap[conditionAvailable]
	prog, hasProg := condMap[conditionProgressing]

	// Available=True guarantees only minimum availability, and the available
	// replicas may still belong to an older ReplicaSet.
	if hasAvail && avail.Status == conditionTrue && rolloutComplete {
		return airunwayv1alpha1.DeploymentPhaseRunning, ""
	}

	// Progressing=False with DeadlineExceeded is a hard failure
	if hasProg && prog.Status == "False" && prog.Reason == "ProgressDeadlineExceeded" {
		msg := prog.Message
		if msg == "" {
			msg = "deployment timed out waiting for rollout"
		}
		if failureMessage, hasFailure := replicaFailureMessage(condMap); hasFailure && failureMessage != "" {
			msg += "; " + failureMessage
		}
		return airunwayv1alpha1.DeploymentPhaseFailed, msg
	}

	if message, hasFailure := replicaFailureMessage(condMap); hasFailure {
		return airunwayv1alpha1.DeploymentPhaseDeploying, message
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

// replicaFailureMessage surfaces replica creation failures that can block a rollout
// while old replicas remain available. hasFailure is true even if no details are provided.
func replicaFailureMessage(condMap map[string]conditionInfo) (message string, hasFailure bool) {
	failure, found := condMap[conditionReplicaFailure]
	if !found || failure.Status != conditionTrue {
		return "", false
	}
	if failure.Reason != "" && failure.Message != "" {
		return failure.Reason + ": " + failure.Message, true
	}
	if failure.Message != "" {
		return failure.Message, true
	}
	return failure.Reason, true
}

func (t *StatusTranslator) isRolloutComplete(
	upstream *unstructured.Unstructured,
	replicas *airunwayv1alpha1.ReplicaStatus,
) bool {
	if replicas == nil || replicas.Desired <= 0 || upstream.GetGeneration() <= 0 {
		return false
	}

	observedGeneration, hasObservedGeneration, _ := unstructured.NestedInt64(
		upstream.Object, "status", "observedGeneration",
	)
	totalReplicas, hasTotalReplicas, _ := unstructured.NestedInt64(upstream.Object, "status", "replicas")
	updatedReplicas, hasUpdatedReplicas, _ := unstructured.NestedInt64(upstream.Object, "status", "updatedReplicas")
	desired := int64(replicas.Desired)

	return hasObservedGeneration && observedGeneration >= upstream.GetGeneration() &&
		hasTotalReplicas && hasUpdatedReplicas && totalReplicas == updatedReplicas &&
		updatedReplicas == desired &&
		replicas.Ready >= replicas.Desired && replicas.Available >= replicas.Desired
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
