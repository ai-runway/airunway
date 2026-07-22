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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
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

// verifyOwnedOrAbsent guards a server-side apply against silently adopting an
// unrelated, same-named object. It looks up any existing object matching obj's
// kind/name/namespace and returns an error unless that object is already
// controlled by owner (or does not exist). Providers must call this before
// force-applying so an AgentDeployment cannot overwrite a Deployment, Service,
// Job, ConfigMap, or upstream framework CR it does not own.
func verifyOwnedOrAbsent(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner metav1.Object, obj client.Object) error {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		// Unregistered (e.g. upstream CRD) types resolve their GVK from the
		// object itself rather than the scheme.
		gvk = obj.GetObjectKind().GroupVersionKind()
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	key := k8stypes.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	if err := c.Get(ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get existing %s %s for ownership check: %w", gvk.Kind, key, err)
	}
	if !metav1.IsControlledBy(existing, owner) {
		return fmt.Errorf("refusing to adopt %s %s: it is not owned by AgentDeployment %s", gvk.Kind, key, owner.GetName())
	}
	return nil
}

// deleteOwnedObject deletes obj (addressed by its already-set name/namespace and
// kind) only when it is controlled by owner; a missing or unowned object is a
// no-op. Providers use it to tear down the resources they rendered when a
// binding is revoked, without ever touching an unrelated same-named object.
func deleteOwnedObject(ctx context.Context, c client.Client, owner metav1.Object, obj client.Object) error {
	key := k8stypes.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get owned object %s for cleanup: %w", key, err)
	}
	if !metav1.IsControlledBy(obj, owner) {
		return nil
	}
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete owned object %s: %w", key, err)
	}
	return nil
}

// cleanupOwnedCRs tears down the given upstream custom resources a CRD-backed
// provider rendered for ad when a model binding is revoked, so the previously
// rendered agent stops running instead of continuing to serve with a stale
// endpoint. Each object is deleted only when it is actually controlled by ad.
//
// The managed no-auth Secret is intentionally left in place: it holds only a
// keyless placeholder token (not a real credential), deleting it would require
// re-granting the Secret read access these providers deliberately drop, and it
// is garbage-collected via its owner reference when the AgentDeployment itself
// is deleted.
func cleanupOwnedCRs(ctx context.Context, c client.Client, ad *airunwayv1alpha1.AgentDeployment, objs ...client.Object) error {
	for _, obj := range objs {
		if err := deleteOwnedObject(ctx, c, ad, obj); err != nil {
			return err
		}
	}
	return nil
}

// unstructuredRef builds a minimal object handle (kind + name + namespace)
// suitable for a Get/Delete of an upstream custom resource.
func unstructuredRef(gvk schema.GroupVersionKind, name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

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
	generation := u.GetGeneration()
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, raw := range conds {
		cm, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if cm["type"] != "Ready" {
			continue
		}
		if cm["status"] != "True" {
			return false
		}
		// Guard against a stale Ready=True left over from a previous
		// generation: right after the provider reapplies a changed CR, the
		// operator may not have re-reconciled yet. When it records an
		// observedGeneration, require it to have caught up before we promote
		// AgentDeployment to Running.
		if observed, present := conditionObservedGeneration(cm); present && observed < generation {
			return false
		}
		return true
	}
	return false
}

// conditionObservedGeneration extracts a status condition's observedGeneration.
// The unstructured decoder yields JSON numbers as int64/float64, so both are
// handled. Returns false when the operator does not record it.
func conditionObservedGeneration(cm map[string]interface{}) (int64, bool) {
	switch n := cm["observedGeneration"].(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
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
