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

// Package agentprovider is the contract for writing an AI Runway agent
// framework provider, in-tree or out-of-tree.
//
// # The division of labour
//
// The core controller resolves an AgentDeployment's framework and model
// binding, publishes the result as status.modelBinding, and owns the
// FrameworkReady, ModelBound and Ready conditions. It never renders a
// workload.
//
// A provider watches AgentDeployments for its framework, consumes the
// core-resolved binding, renders whatever its backend actually speaks
// (framework-native custom resources, or plain Deployments and Services), and
// owns status.phase, status.runtime, status.replicas and the ProviderReady
// condition. It never resolves a model.
//
// That split is enforced by the API server through server-side apply field
// ownership, not by convention — which is why the status helper here is the
// only supported way to write provider status.
//
// # Writing a provider
//
// A provider is an ordinary controller-runtime reconciler. The typical shape:
//
//	func (r *MyProvider) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
//	    var ad airunwayv1alpha1.AgentDeployment
//	    if err := r.Get(ctx, req.NamespacedName, &ad); err != nil {
//	        return ctrl.Result{}, client.IgnoreNotFound(err)
//	    }
//	    if ad.Spec.Framework.Name != "myframework" || !ad.DeletionTimestamp.IsZero() {
//	        return ctrl.Result{}, nil
//	    }
//
//	    switch agentprovider.ClassifyBinding(&ad) {
//	    case agentprovider.BindingUnavailable:
//	        // tear down what we rendered, then report Pending
//	    case agentprovider.BindingStale:
//	        // hold: leave the running workload and our status alone
//	        return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
//	    }
//
//	    // render from *ad.Status.ModelBinding, then:
//	    return ctrl.Result{}, agentprovider.ApplyOwnedStatus(ctx, r.Client, &ad, MyFieldOwner, ...)
//	}
//
// Every helper here is safe to call from an out-of-tree module; the package
// deliberately depends only on the public API types and controller-runtime.
package agentprovider

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
	// FrameworkIndexKey indexes AgentDeployments by spec.framework.name, so a
	// provider-config change requeues only the agents it can affect. Register
	// it with EnsureFrameworkIndex and query it with AgentsForFramework.
	FrameworkIndexKey = "spec.framework.name"

	// KeylessCredentialKey is the data key of the managed no-auth Secret, and
	// the key a resolved binding's CredentialsRef points at for keyless
	// in-cluster endpoints.
	KeylessCredentialKey = "token"

	// KeylessCredentialValue is the placeholder token written into that Secret.
	// In-cluster model endpoints need no credential, but many framework SDKs
	// refuse to start without an API-key variable present.
	KeylessCredentialValue = "not-required"

	// keylessCredentialSecretSuffix is appended to the AgentDeployment name to
	// form the managed no-auth Secret name.
	keylessCredentialSecretSuffix = "-model-noauth"

	// MaxLabelValueLength is the Kubernetes limit on a label value. An
	// AgentDeployment name may be a DNS subdomain of up to 253 characters, so
	// names cannot be reused as label values unmodified.
	MaxLabelValueLength = 63

	// MaxResourceNameLength is the Kubernetes limit on an object name.
	MaxResourceNameLength = 253

	// MaxDNSLabelNameLength is the limit on names validated as an RFC 1035 DNS
	// label rather than a subdomain — Services, most notably. Stricter than
	// MaxResourceNameLength.
	MaxDNSLabelNameLength = 63

	// shortHashLength is how many hex characters of a SHA-256 digest are used
	// to keep truncated names and labels distinct.
	shortHashLength = 8
)

// -----------------------------------------------------------------------------
// Binding contract
// -----------------------------------------------------------------------------

// BindingState classifies what a provider should do with the core-resolved
// model binding. Obtain it from ClassifyBinding.
type BindingState int

const (
	// BindingUnavailable means core published no binding at all: either it has
	// not resolved one yet, or it cleared a previously valid one because the
	// request became terminally invalid. Anything already rendered must be torn
	// down so the agent stops serving.
	BindingUnavailable BindingState = iota

	// BindingStale means a binding is published but core could not re-verify it
	// on its latest pass — a framework-readiness blip, a ModelDeployment
	// briefly without an endpoint, a transient Secret lookup error. HOLD: do
	// not re-render against possibly-changed inputs, and do not tear down a
	// healthy agent over a momentary failure.
	BindingStale

	// BindingReady means the published binding is current and verified, and the
	// provider should render from it.
	BindingReady
)

// ClassifyBinding distinguishes "core has no binding for this agent" from
// "core has a binding it could not re-verify right now".
//
// Providers MUST branch on this rather than on the ModelBound condition alone.
// Gating teardown on the condition conflates the two states, which is how a
// single failed discovery call could delete every running agent workload on a
// framework. The binding is the contract; the condition is only a freshness
// signal about it.
func ClassifyBinding(ad *airunwayv1alpha1.AgentDeployment) BindingState {
	if ad.Status.ModelBinding == nil {
		return BindingUnavailable
	}
	cond := meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeModelBound)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return BindingStale
	}
	// A ModelBound=True carried over from an earlier generation says nothing
	// about the CURRENT spec. Immediately after a user edits spec.model, the
	// old condition is still True and the old binding still published, so a
	// provider that trusted the condition alone would render the new
	// generation against the previous endpoint or credential. Treat it as
	// stale until core has re-verified — hold, do not tear down.
	//
	// ObservedGeneration 0 means the condition predates generation tracking;
	// accept it rather than stalling agents written by an older controller.
	if cond.ObservedGeneration != 0 && cond.ObservedGeneration < ad.Generation {
		return BindingStale
	}
	return BindingReady
}

// -----------------------------------------------------------------------------
// Status ownership
// -----------------------------------------------------------------------------

// ApplyOwnedStatus writes ONLY the provider-owned AgentDeployment status
// fields — phase, runtime, replicas and the ProviderReady condition — via
// server-side apply under fieldOwner. Core-owned fields (framework,
// modelBinding, and the core conditions) are omitted, so the API server leaves
// the core controller's writes intact.
//
// fieldOwner must be unique to your provider; see FieldOwner.
//
// This deliberately does NOT force ownership. Conditions are a listType=map
// keyed by type, so core and each provider own disjoint entries, and a conflict
// therefore means two writers really are fighting over one field. Forcing would
// silently steal it and make the overlap last-writer-wins — exactly the thrash
// this ownership split exists to prevent. A conflict surfaces as a reconcile
// error instead.
func ApplyOwnedStatus(
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

// FieldOwner builds the server-side apply field manager for a provider. Each
// provider MUST use a distinct owner so the API server can keep their writes
// apart; passing the framework name gives that for free.
func FieldOwner(framework string) string {
	return "airunway-agents-" + framework
}

// providerReadyTransition preserves the existing ProviderReady
// LastTransitionTime when the status is unchanged, so repeated reconciles do
// not churn the timestamp — SSA re-applies the whole condition entry.
func providerReadyTransition(ad *airunwayv1alpha1.AgentDeployment, status metav1.ConditionStatus) metav1.Time {
	if existing := meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady); existing != nil && existing.Status == status {
		return existing.LastTransitionTime
	}
	return metav1.Now()
}

// -----------------------------------------------------------------------------
// Ownership guards
// -----------------------------------------------------------------------------

// VerifyOwnedOrAbsent guards a server-side apply against silently adopting an
// unrelated, same-named object. It looks up any existing object matching obj's
// kind, name and namespace, and returns an error unless that object is already
// controlled by owner (or does not exist).
//
// Call this before any force-apply, so an AgentDeployment cannot overwrite a
// Deployment, Service, Job, ConfigMap or upstream custom resource it does not
// own.
func VerifyOwnedOrAbsent(ctx context.Context, c client.Reader, scheme *runtime.Scheme, owner metav1.Object, obj client.Object) error {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		// Unregistered types (an upstream CRD the provider renders as
		// unstructured) resolve their GVK from the object itself.
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

// DeleteOwned deletes obj — addressed by its already-set kind, name and
// namespace — only when it is controlled by owner. A missing or unowned object
// is a no-op, never an error, so a provider can call this unconditionally
// during teardown without risking an unrelated same-named object.
func DeleteOwned(ctx context.Context, c client.Client, owner metav1.Object, obj client.Object) error {
	key := k8stypes.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		// The kind itself is not served — the framework's operator is not
		// installed. Nothing of this kind can exist, so there is nothing to
		// clean up. Treating it as an error instead makes the provider retry
		// forever and leak the raw discovery message onto ProviderReady.
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return nil
		}
		return fmt.Errorf("get owned object %s for cleanup: %w", key, err)
	}
	if !metav1.IsControlledBy(obj, owner) {
		return nil
	}
	// Propagation must be explicit. batch/v1 Job is the one kind here whose
	// server-side default is OrphanDependents, kept for backwards compatibility:
	// deleting it without a policy strips the ownerReferences from its pods and
	// removes only the Job. The pods keep running, and having lost their owner
	// they are no longer collected when the AgentDeployment itself goes away.
	//
	// That turns teardown into a leak in exactly the case it exists for: when a
	// credential is revoked, this helper is what is supposed to stop the agent
	// still holding it. Deployment, Service and ConfigMap already default to
	// Background, so stating it costs nothing and closes the Job case.
	policy := metav1.DeletePropagationBackground
	if err := c.Delete(ctx, obj, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete owned object %s: %w", key, err)
	}
	return nil
}

// ReleaseOwnedStatus relinquishes every provider-owned status field, for a
// provider that is standing aside because the framework is no longer its to
// serve.
//
// Simply returning without writing is not enough, and does two wrong things.
// First, the last status the provider wrote survives: an agent whose workload
// has just been torn down keeps reporting phase Running, a runtime.workloadRef
// pointing at a deleted Deployment, and replicas 1/1 ready.
//
// Second — and worse — server-side apply ownership is per-field and sticky. A
// provider that stops applying keeps owning phase, runtime, replicas and the
// ProviderReady condition indefinitely. Because ApplyOwnedStatus deliberately
// does not force, a successor provider taking the framework over then fails
// every reconcile with a conflict against a manager that will never write
// again. Standing aside silently would deadlock the handover it exists to
// enable.
//
// The apply carries phase alone. Everything else the provider owns — runtime,
// replicas and the ProviderReady condition — is absent, so SSA drops those
// fields and releases them.
//
// The shape is forced by two measured constraints. An entirely empty status is
// rejected with `status: Invalid value: "null"`, both as a typed
// AgentDeployment (converting the zero AgentDeploymentStatus drops the key) and
// as unstructured with a literal `status: {}`. And keeping the ProviderReady
// condition, as an earlier version did, deadlocks the handover for real — a
// successor's non-forced apply fails with:
//
//	Apply failed with 2 conflicts: conflicts with "airunway-agents-container"
//	- .status.conditions[type="ProviderReady"].message
//	- .status.conditions[type="ProviderReady"].reason
//
// Only reason and message conflict, because phase and the condition's status
// happened to match; that is luck, not design. Releasing the condition is what
// makes the takeover work. Phase stays owned, which is harmless: a successor
// writing the same Pending value co-owns it without conflict, and any later
// phase it writes is a value this manager no longer maintains.
//
// It is built unstructured because a typed apply cannot express "status with
// phase and no conditions" — the zero-valued condition slice is indistinguishable
// from one the caller wants removed.
func ReleaseOwnedStatus(ctx context.Context, c client.Client, ad *airunwayv1alpha1.AgentDeployment, fieldOwner string) error {
	apply := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": airunwayv1alpha1.GroupVersion.String(),
		"kind":       "AgentDeployment",
		"metadata": map[string]any{
			"name":      ad.Name,
			"namespace": ad.Namespace,
		},
		"status": map[string]any{
			"phase": string(airunwayv1alpha1.AgentPhasePending),
		},
	}}
	return c.Status().Patch(ctx, apply, client.Apply, client.FieldOwner(fieldOwner))
}

// CleanupOwned tears down the resources a provider rendered for ad, so a
// revoked binding actually stops the agent instead of leaving it serving a
// stale endpoint. Each object is deleted only when controlled by ad.
//
// A managed no-auth Secret from EnsureBindingCredentials should NOT be passed
// here: it holds only a placeholder token, deleting it would require the Secret
// read access providers deliberately drop, and it is garbage-collected via its
// owner reference when the AgentDeployment goes away.
func CleanupOwned(ctx context.Context, c client.Client, ad *airunwayv1alpha1.AgentDeployment, objs ...client.Object) error {
	for _, obj := range objs {
		if err := DeleteOwned(ctx, c, ad, obj); err != nil {
			return err
		}
	}
	return nil
}

// UnstructuredRef builds a minimal object handle — kind, name and namespace —
// suitable for a Get or Delete of an upstream custom resource.
func UnstructuredRef(gvk schema.GroupVersionKind, name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

// -----------------------------------------------------------------------------
// Upstream readiness
// -----------------------------------------------------------------------------

// UpstreamCRReady reports whether an already-applied upstream custom resource
// reports Ready=True. It lets a crd-backed provider reflect the framework
// operator's own readiness into ProviderReady, rather than claiming ready the
// moment the CR is created.
//
// Returns false when the CR is missing, has no Ready condition, or carries a
// Ready=True left over from a previous generation — checked via the condition's
// observedGeneration where the operator records one.
func UpstreamCRReady(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, name, namespace string) bool {
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
		if observed, present := conditionObservedGeneration(cm); present && observed < generation {
			return false
		}
		return true
	}
	return false
}

// conditionObservedGeneration extracts a status condition's observedGeneration.
// The unstructured decoder yields JSON numbers as int64 or float64, so both are
// handled. Reports false when the operator does not record one.
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

// -----------------------------------------------------------------------------
// Credentials
// -----------------------------------------------------------------------------

// KeylessCredentialSecretName returns the deterministic per-agent Secret name
// used for keyless in-cluster model credentials.
func KeylessCredentialSecretName(agentName string) string {
	return BoundedResourceName(agentName, keylessCredentialSecretSuffix)
}

// EnsureBindingCredentials guarantees a binding carries a CredentialsRef.
//
// Core resolves in-cluster endpoints as keyless (CredentialsRef nil), but
// crd-backed providers usually need a Secret reference to satisfy their
// upstream CRD schema. This creates or updates an owner-referenced Secret
// holding a placeholder token and returns the binding with CredentialsRef set.
// A binding that already has credentials is returned untouched.
//
// The Secret name derives from the agent name, so it can collide with one a
// user already manages. Adopting that Secret would inject a key into it and
// attach a controller owner reference, so deleting the AgentDeployment would
// garbage-collect someone else's Secret — and note the manager has no `delete`
// on Secrets, so an owner reference is a way to acquire deletion it was never
// granted.
//
// An earlier version relied on the unforced apply to prevent this, on the
// reasoning that SSA would report a conflict. **That is false**, and was
// demonstrated against a real API server: SSA raises a conflict only for fields
// another manager already owns *and* whose value the apply changes. Adding a new
// data key, new labels and a new ownerReferences entry to an unowned Secret is
// all "added", so the apply succeeds silently and the Secret is adopted.
//
// So ownership is checked explicitly before writing. This costs `get` on Secrets
// in the provider's RBAC, which design-doc §5 had hoped to avoid — but a
// component that may create a Secret and not read one cannot do create-if-absent
// safely, which is precisely the bug. Providers still never read a *credential*
// they did not create; the read here is an existence-and-ownership check.
func EnsureBindingCredentials(
	ctx context.Context,
	c client.Client,
	apiReader client.Reader,
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

	secretName := KeylessCredentialSecretName(ad.Name)
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: ad.Namespace,
			Labels: map[string]string{
				"airunway.ai/agent":     BoundedLabelValue(ad.Name),
				"airunway.ai/framework": BoundedLabelValue(ad.Spec.Framework.Name),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			KeylessCredentialKey: []byte(KeylessCredentialValue),
		},
	}
	if err := controllerutil.SetControllerReference(ad, secret, scheme); err != nil {
		return binding, fmt.Errorf("set owner reference on keyless credential secret: %w", err)
	}
	// Refuse rather than adopt. VerifyOwnedOrAbsent returns an error when a
	// Secret of this name exists and is not controlled by this AgentDeployment,
	// which surfaces as a provider-not-ready reason the operator can act on —
	// rename the agent, or remove the colliding Secret.
	if err := VerifyOwnedOrAbsent(ctx, apiReader, scheme, ad, secret.DeepCopy()); err != nil {
		return binding, err
	}
	if err := c.Patch(ctx, secret, client.Apply, client.FieldOwner(fieldOwner)); err != nil {
		return binding, fmt.Errorf("apply keyless credential secret %s/%s: %w",
			secret.Namespace, secret.Name, err)
	}

	binding.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
		Name: secretName,
		Key:  KeylessCredentialKey,
	}
	return binding, nil
}

// -----------------------------------------------------------------------------
// Naming
// -----------------------------------------------------------------------------

// shortHash returns the leading hex characters of the SHA-256 of s. It only
// ever disambiguates truncated names; it never carries meaning.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:shortHashLength]
}

// BoundedLabelValue returns v unchanged when it already fits a label value, and
// otherwise truncates it and appends a short hash of the full value so two
// distinct long names cannot collapse onto the same label.
//
// Use this for every label derived from an AgentDeployment name. Unbounded, a
// legal long name makes every rendered workload fail admission with "must be no
// more than 63 bytes".
func BoundedLabelValue(v string) string {
	if len(v) <= MaxLabelValueLength {
		return v
	}
	h := shortHash(v)
	return v[:MaxLabelValueLength-len(h)-1] + "-" + h
}

// BoundedResourceName joins base and suffix into an object name within the
// 253-character limit, truncating base and inserting a short hash when needed.
func BoundedResourceName(base, suffix string) string {
	if len(base)+len(suffix) <= MaxResourceNameLength {
		return base + suffix
	}
	h := shortHash(base)
	keep := MaxResourceNameLength - len(suffix) - len(h) - 1
	return base[:keep] + "-" + h + suffix
}

// BoundedDNSLabelName bounds a name to the 63-character RFC 1035 DNS label
// limit that Services are validated against — stricter than the object-name
// limit, so a name legal for a Deployment can be illegal for its Service.
func BoundedDNSLabelName(base string) string {
	if len(base) <= MaxDNSLabelNameLength {
		return base
	}
	h := shortHash(base)
	return base[:MaxDNSLabelNameLength-len(h)-1] + "-" + h
}

// HashJSON returns a stable short digest of v's JSON encoding. Providers use it
// for pod-template and config checksums that force a rollout when rendered
// content changes but the object's name does not.
func HashJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("hash object: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16], nil
}

// -----------------------------------------------------------------------------
// Watches and framework gating
// -----------------------------------------------------------------------------

// ProviderConfigRelevantChange filters AgentProviderConfig watch events down to
// the changes that can alter how an AgentDeployment renders: a spec change, a
// readiness flip, a reported-version change, or a catalog change.
//
// Without it the 60-second readiness heartbeat — which only rewrites
// status.lastHeartbeat — requeues every AgentDeployment in the cluster once a
// minute, in every provider.
func ProviderConfigRelevantChange() predicate.Predicate {
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
			// Core copies status.version into every agent's
			// status.framework.providerVersion, so a shim upgrade must fan out.
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

// frameworkIndex guards one-time registration of the by-framework field index.
// Core and every provider need it, registering the same key twice on one
// manager is an error, and a standalone provider runs without core — so
// registration is shared and idempotent.
var frameworkIndex struct {
	once sync.Once
	err  error
}

// EnsureFrameworkIndex registers the AgentDeployment-by-framework field index
// exactly once per process. Call it from SetupWithManager before building the
// controller; calling it from several providers in one binary is safe.
func EnsureFrameworkIndex(mgr ctrl.Manager) error {
	frameworkIndex.once.Do(func() {
		frameworkIndex.err = mgr.GetFieldIndexer().IndexField(
			context.Background(),
			&airunwayv1alpha1.AgentDeployment{},
			FrameworkIndexKey,
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

// AgentsForFramework lists the AgentDeployments bound to a framework, using the
// index registered by EnsureFrameworkIndex and falling back to a full list when
// it is absent — which is the case in unit tests that bypass SetupWithManager.
func AgentsForFramework(ctx context.Context, c client.Client, framework string) []airunwayv1alpha1.AgentDeployment {
	var list airunwayv1alpha1.AgentDeploymentList
	if err := c.List(ctx, &list, client.MatchingFields{FrameworkIndexKey: framework}); err != nil {
		if err := c.List(ctx, &list); err != nil {
			// A map function has no error channel, so returning nil silently
			// drops the event and nothing retries. Log it, or a readiness flip
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

// FrameworkUsesBackend reports whether the named framework is registered with
// the given backend.
//
// A provider MUST gate on this as well as on the framework name. Without it, a
// framework registered as a container backend but named after a crd-backed
// provider would be rendered by both, and the two would fight over the
// provider-owned ProviderReady condition.
func FrameworkUsesBackend(ctx context.Context, c client.Client, framework string, backend airunwayv1alpha1.AgentProviderBackend) (bool, error) {
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
	return apc.Spec.Capabilities.Backend == backend, nil
}
