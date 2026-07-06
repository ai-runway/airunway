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

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

// applyProviderOwnedStatus writes ONLY the provider-owned AgentDeployment
// status fields (phase, runtime, replicas, and the ProviderReady condition)
// via server-side apply under the given field owner. Core-owned fields
// (framework, modelBindings, and the core conditions) are omitted, so the API
// server leaves the core controller's writes intact.
//
// Both framework providers (crd and container) share this so the SSA
// field-ownership contract is implemented in exactly one place.
func applyProviderOwnedStatus(
	ctx context.Context,
	c client.Client,
	ad *airunwayv1alpha1.AgentDeployment,
	fieldOwner string,
	phase airunwayv1alpha1.AgentPhase,
	rt *airunwayv1alpha1.AgentRuntimeStatus,
	replicas *airunwayv1alpha1.AgentReplicaStatus,
	providerReady metav1.ConditionStatus,
	reason, message string,
) error {
	apply := &airunwayv1alpha1.AgentDeployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: airunwayv1alpha1.GroupVersion.String(),
			Kind:       "AgentDeployment",
		},
		ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace},
		Status: airunwayv1alpha1.AgentDeploymentStatus{
			Phase:    phase,
			Runtime:  rt,
			Replicas: replicas,
			Conditions: []metav1.Condition{{
				Type:               airunwayv1alpha1.AgentConditionTypeProviderReady,
				Status:             providerReady,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: providerReadyTransition(ad, providerReady),
				ObservedGeneration: ad.Generation,
			}},
		},
	}

	return c.Status().Patch(ctx, apply, client.Apply,
		client.FieldOwner(fieldOwner),
		client.ForceOwnership,
	)
}

// providerReadyTransition preserves the existing ProviderReady
// LastTransitionTime when the status is unchanged, so repeated reconciles do
// not churn the timestamp (SSA re-applies the whole condition entry).
func providerReadyTransition(ad *airunwayv1alpha1.AgentDeployment, status metav1.ConditionStatus) metav1.Time {
	if existing := meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady); existing != nil && existing.Status == status {
		return existing.LastTransitionTime
	}
	return metav1.Now()
}
