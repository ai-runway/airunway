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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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

	// maxLabelValueLength is the Kubernetes limit on a label value. An
	// AgentDeployment name may be a DNS subdomain of up to 253 characters, so
	// names cannot be used as label values unmodified.
	maxLabelValueLength = 63
	// maxResourceNameLength is the Kubernetes limit on an object name.
	maxResourceNameLength = 253
	// maxDNSLabelNameLength is the limit on names validated as an RFC 1035 DNS
	// label rather than a subdomain — Services, most notably. It is stricter
	// than maxResourceNameLength.
	maxDNSLabelNameLength = 63
	// shortHashLength is how many hex characters of a SHA-256 digest are used
	// to keep truncated names/labels distinct.
	shortHashLength = 8
)

// shortHash returns the first shortHashLength hex characters of the SHA-256 of
// s. It only ever disambiguates truncated names, never carries meaning.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:shortHashLength]
}

// boundedLabelValue returns v unchanged when it already fits in a label value,
// and otherwise truncates it and appends a short hash of the full value so two
// distinct long names do not collapse onto the same label. Leaving this
// unbounded makes every rendered workload for a long-named AgentDeployment fail
// admission with "must be no more than 63 bytes".
func boundedLabelValue(v string) string {
	if len(v) <= maxLabelValueLength {
		return v
	}
	h := shortHash(v)
	return v[:maxLabelValueLength-len(h)-1] + "-" + h
}

// boundedResourceName joins base and suffix into an object name that fits the
// 253-character limit, truncating base and inserting a short hash when needed.
func boundedResourceName(base, suffix string) string {
	if len(base)+len(suffix) <= maxResourceNameLength {
		return base + suffix
	}
	h := shortHash(base)
	keep := maxResourceNameLength - len(suffix) - len(h) - 1
	return base[:keep] + "-" + h + suffix
}

// boundedDNSLabelName bounds a name to the 63-character RFC 1035 DNS label
// limit that Services are validated against.
func boundedDNSLabelName(base string) string {
	if len(base) <= maxDNSLabelNameLength {
		return base
	}
	h := shortHash(base)
	return base[:maxDNSLabelNameLength-len(h)-1] + "-" + h
}

// hashJSON returns a stable short digest of v's JSON encoding. It backs the
// pod-template and config checksums that force a rollout when rendered content
// changes.
func hashJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("hash object: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16], nil
}

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

// bindingState classifies what a provider should do with the core-resolved
// model binding.
type bindingState int

const (
	// bindingUnavailable means core published no binding at all: either it has
	// not resolved one yet, or it cleared a previously valid one because the
	// request became terminally invalid. Anything already rendered must be torn
	// down so the agent stops serving.
	bindingUnavailable bindingState = iota

	// bindingStale means a binding is published but core could not re-verify it
	// on its latest pass — a framework-readiness blip, a ModelDeployment
	// briefly without an endpoint, a transient Secret lookup error. The right
	// move is to HOLD: do not re-render against possibly-changed inputs, and do
	// not tear down a healthy agent over a momentary failure.
	bindingStale

	// bindingReady means the published binding is current and verified.
	bindingReady
)

// classifyBinding distinguishes "core has no binding for this agent" from
// "core has a binding it could not re-verify right now".
//
// Gating teardown on the ModelBound CONDITION alone conflates the two, which is
// how a single failed discovery call could delete every running agent workload
// on a framework. The binding itself is the contract; the condition is only a
// freshness signal about it.
func classifyBinding(ad *airunwayv1alpha1.AgentDeployment) bindingState {
	if ad.Status.ModelBinding == nil {
		return bindingUnavailable
	}
	if !meta.IsStatusConditionTrue(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeModelBound) {
		return bindingStale
	}
	return bindingReady
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
//
// This deliberately does NOT force ownership: conditions are a listType=map
// keyed by type, so core and each provider own disjoint entries and a conflict
// means two writers really are fighting over the same field. Forcing would
// silently steal the field and make the overlap last-writer-wins, which is
// exactly the thrash this ownership split exists to prevent. A conflict
// surfaces as a reconcile error instead.
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

	return c.Status().Patch(ctx, apply, client.Apply, client.FieldOwner(fieldOwner))
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
	return boundedResourceName(agentName, keylessCredentialSecretSuffix)
}

// agentProviderConfigRelevantChange filters AgentProviderConfig watch events
// down to the changes that can alter how an AgentDeployment renders: a spec
// change, a readiness flip, or a catalog change. Without it the 60s readiness
// heartbeat (which only rewrites status.lastHeartbeat) requeues every
// AgentDeployment in the cluster once a minute, in every provider.
func agentProviderConfigRelevantChange() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldAPC, okOld := e.ObjectOld.(*airunwayv1alpha1.AgentProviderConfig)
			newAPC, okNew := e.ObjectNew.(*airunwayv1alpha1.AgentProviderConfig)
			if !okOld || !okNew {
				return true
			}
			if oldAPC.Generation != newAPC.Generation {
				return true
			}
			if !ptrBoolEqual(oldAPC.Status.Ready, newAPC.Status.Ready) {
				return true
			}
			// The core controller copies status.version into every agent's
			// status.framework.providerVersion, so a shim upgrade must fan out.
			// Without this, that field — whose whole purpose is spotting skew
			// between an agent and its provider — reports the OLD version until
			// an unrelated change or the ~10h resync, and stays empty forever if
			// the version write lands after the readiness flip on a fresh install.
			if oldAPC.Status.Version != newAPC.Status.Version {
				return true
			}
			return oldAPC.Annotations[airunwayv1alpha1.AgentProviderCatalogAnnotation] !=
				newAPC.Annotations[airunwayv1alpha1.AgentProviderCatalogAnnotation]
		},
	}
}

func ptrBoolEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// frameworkIndex guards one-time registration of the AgentDeployment-by-
// framework field index. Core and every provider need it, registering the same
// key twice on one manager is an error, and the standalone provider shims run
// without the core controller — so registration is shared and idempotent.
var frameworkIndex struct {
	once sync.Once
	err  error
}

// ensureFrameworkIndex registers the by-framework field index exactly once per
// process.
func ensureFrameworkIndex(mgr ctrl.Manager) error {
	frameworkIndex.once.Do(func() {
		frameworkIndex.err = mgr.GetFieldIndexer().IndexField(
			context.Background(),
			&airunwayv1alpha1.AgentDeployment{},
			agentDeploymentFrameworkIndexKey,
			func(raw client.Object) []string {
				ad, ok := raw.(*airunwayv1alpha1.AgentDeployment)
				if !ok || ad.Spec.Framework.Name == "" {
					return nil
				}
				return []string{ad.Spec.Framework.Name}
			},
		)
	})
	return frameworkIndex.err
}

// agentsForFramework lists the AgentDeployments bound to a framework, using the
// shared field index and falling back to a full list when the index is not
// registered (unit tests that bypass SetupWithManager).
func agentsForFramework(ctx context.Context, c client.Client, framework string) []airunwayv1alpha1.AgentDeployment {
	var list airunwayv1alpha1.AgentDeploymentList
	if err := c.List(ctx, &list, client.MatchingFields{agentDeploymentFrameworkIndexKey: framework}); err != nil {
		if err := c.List(ctx, &list); err != nil {
			// Returning nil here silently drops the event: a map function has
			// no error channel, so nothing retries. Log it, or a readiness flip
			// vanishes with no trace of why agents never reconciled.
			log.FromContext(ctx).Error(err, "listing AgentDeployments for framework failed; provider-config event dropped",
				"framework", framework)
			return nil
		}
	}
	out := make([]airunwayv1alpha1.AgentDeployment, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.Framework.Name == framework {
			out = append(out, list.Items[i])
		}
	}
	return out
}

// frameworkUsesCRDBackend reports whether the named framework is registered
// with the crd backend. The CRD-backed providers gate on this so that a
// framework registered as a container backend is rendered by exactly one
// provider; otherwise both would render resources and fight over the
// provider-owned ProviderReady condition.
func frameworkUsesCRDBackend(ctx context.Context, c client.Client, framework string) (bool, error) {
	var apc airunwayv1alpha1.AgentProviderConfig
	if err := c.Get(ctx, k8stypes.NamespacedName{Name: framework}, &apc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if apc.Spec.Capabilities == nil {
		return false, nil
	}
	return apc.Spec.Capabilities.Backend == airunwayv1alpha1.AgentProviderBackendCRD, nil
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
				"airunway.ai/agent":     boundedLabelValue(ad.Name),
				"airunway.ai/framework": boundedLabelValue(ad.Spec.Framework.Name),
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
	// The Secret name is derived from the agent name, so it can collide with a
	// pre-existing hand-managed Secret — and adopting one would overwrite its
	// data AND attach an owner reference that garbage-collects it with the
	// agent.
	//
	// Guard that with SSA conflict detection rather than a read-then-apply
	// ownership check: a non-forced apply already fails when another field
	// manager owns the same fields, which is the collision we care about, and
	// unlike a Get it needs no `secrets` read verb. Framework providers write
	// managed Secrets but deliberately never read Secrets, so their RBAC stays
	// at create/update/patch.
	if err := c.Patch(ctx, secret, client.Apply, client.FieldOwner(fieldOwner)); err != nil {
		return binding, fmt.Errorf("apply keyless credential secret %s/%s (a conflict here means a Secret of this name already exists and is managed by someone else): %w",
			secret.Namespace, secret.Name, err)
	}

	binding.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
		Name: secretName,
		Key:  keylessCredentialKey,
	}
	return binding, nil
}
