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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

const (
	// keylessCredentialSecretSuffix is appended to AgentDeployment names for the
	// per-agent no-auth Secret used by CRD-backed frameworks.
	keylessCredentialSecretSuffix = "-model-noauth"
	// keylessCredentialKey is the Secret data key and credentialsRef.key value.
	keylessCredentialKey = "token"
	// keylessCredentialValue is the placeholder token literal for keyless
	// in-cluster model endpoints.
	keylessCredentialValue = "not-required"
)

// applyProviderOwnedStatus writes ONLY the provider-owned AgentDeployment
// status fields (phase, runtime, replicas, and the ProviderReady condition)
// via server-side apply under the given field owner. Core-owned fields
// (framework, modelBinding, and the core conditions) are omitted, so the API
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

// upstreamCRReady reports whether an already-applied upstream custom resource
// reports Ready=True in its status.conditions. It lets a crd-backend provider
// (kagent, Orka) reflect the framework operator's own readiness back into
// AgentDeployment's ProviderReady, rather than reporting ready the moment the
// CR is created. Returns false when the CR is missing or has no Ready=True
// condition yet.
func upstreamCRReady(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, name, namespace string) bool {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, k8stypes.NamespacedName{Name: name, Namespace: namespace}, u); err != nil {
		return false
	}
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, raw := range conds {
		cm, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if cm["type"] == "Ready" {
			return cm["status"] == "True"
		}
	}
	return false
}

// keylessCredentialSecretName returns the deterministic per-agent Secret name
// used for keyless in-cluster model credentials.
func keylessCredentialSecretName(agentName string) string {
	return agentName + keylessCredentialSecretSuffix
}

// ensureBindingCredentials guarantees a binding has CredentialsRef. When core
// resolves a keyless binding (credentialsRef=nil), CRD-backed providers need a
// Kubernetes Secret reference to satisfy upstream CRD schemas (kagent/orka).
// This helper creates/updates an owner-referenced no-auth Secret and returns
// the binding with CredentialsRef set.
func ensureBindingCredentials(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	ad *airunwayv1alpha1.AgentDeployment,
	binding airunwayv1alpha1.ModelBindingStatus,
	fieldOwner string,
) (airunwayv1alpha1.ModelBindingStatus, error) {
	if binding.CredentialsRef != nil {
		return binding, nil
	}
	if scheme == nil {
		return binding, fmt.Errorf("scheme is required to create keyless credential secret")
	}

	secretName := keylessCredentialSecretName(ad.Name)
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: ad.Namespace,
			Labels: map[string]string{
				"airunway.ai/agent":     ad.Name,
				"airunway.ai/framework": ad.Spec.Framework.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			keylessCredentialKey: []byte(keylessCredentialValue),
		},
	}
	if err := controllerutil.SetControllerReference(ad, secret, scheme); err != nil {
		return binding, fmt.Errorf("set owner reference on keyless credential secret: %w", err)
	}
	if err := c.Patch(ctx, secret, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
		return binding, fmt.Errorf("apply keyless credential secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}

	binding.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
		Name: secretName,
		Key:  keylessCredentialKey,
	}
	return binding, nil
}
