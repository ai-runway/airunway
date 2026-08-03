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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
)

const (
	// AgentCoreFieldOwner is the server-side apply field manager for the
	// core controller. Core and each framework provider use distinct field
	// owners so the API server itself prevents cross-writes to the shared
	// AgentDeployment status (see AgentDeploymentStatus ownership contract).
	AgentCoreFieldOwner = "airunway-agents-core"

	// agentRequeueInterval is how long the core controller waits before
	// re-checking a not-yet-satisfiable dependency (framework not ready, a
	// referenced ModelDeployment without an endpoint yet).
	agentRequeueInterval = 15 * time.Second

	// agentCredentialRefreshInterval bounds how long a revoked credential can
	// go unnoticed. Credential Secrets are read uncached and deliberately not
	// watched — a Secret informer would need cluster-wide list/watch and would
	// hold every Secret in the cluster in memory — so Secret-backed bindings
	// are re-validated on a slow timer instead.
	agentCredentialRefreshInterval = 5 * time.Minute

	// The by-framework index key now lives on the public provider contract as
	// agentprovider.FrameworkIndexKey, since every provider needs it too.

	// agentDeploymentModelRefIndexKey indexes AgentDeployments by
	// namespace/name deploymentRef target so ModelDeployment changes can
	// requeue only affected agents.
	agentDeploymentModelRefIndexKey = "spec.model.deploymentRef"
)

// AgentDeploymentReconciler reconciles the core, framework-neutral concerns
// of an AgentDeployment: it validates the requested framework against its
// registered AgentProviderConfig and resolves spec.model into a stable
// status.modelBinding contract the framework provider consumes.
//
// It deliberately does NOT render agent workloads — that is the framework
// provider's job. Core owns framework/modelBinding and the ModelBound,
// FrameworkReady, and aggregate Ready conditions; providers own phase,
// runtime, replicas, and ProviderReady.
type AgentDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader is an uncached reader used for credential Secret lookups.
	//
	// The manager's default client serves typed reads from a cache, and the
	// cache starts an informer on first use — so reading a Secret through it
	// would require cluster-wide list/watch on Secrets and would hold every
	// Secret in the cluster in the controller's memory. This controller only
	// ever reads one Secret by name, so it reads straight from the API server
	// and the RBAC below stays limited to `get`.
	//
	// Falls back to the cached client when unset so unit tests that construct
	// the reconciler directly keep working.
	APIReader client.Reader

	// CredentialAdmissionActive reports whether the validating webhook — the only
	// place the requesting user is authorized against a referenced Secret — is
	// running. When false the reconciler refuses credential-bearing bindings
	// rather than resolving a Secret nobody checked the requester could read.
	//
	// Defaults to false so a caller that forgets to set it fails closed.
	CredentialAdmissionActive bool
}

// +kubebuilder:rbac:groups=airunway.ai,resources=agentdeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=agentdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=agentdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=airunway.ai,resources=agentproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// secretReader returns the uncached reader for Secret lookups, falling back to
// the cached client when APIReader was not wired.
func (r *AgentDeploymentReconciler) secretReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// Reconcile resolves framework and model bindings for an AgentDeployment.
func (r *AgentDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ad airunwayv1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &ad); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Nothing to resolve while the object is being deleted; Kubernetes
	// garbage collection tears down provider-rendered resources via owner
	// references.
	if !ad.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	logger.Info("Reconciling AgentDeployment", "name", ad.Name, "namespace", ad.Namespace)

	// conds starts from the CURRENT status so meta.SetStatusCondition
	// preserves LastTransitionTime on unchanged conditions and so the
	// provider-owned ProviderReady condition is visible when aggregating
	// Ready. Only core-owned condition types are applied back (see below).
	conds := ad.Status.Conditions

	result := ctrl.Result{}
	framework, frameworkReady := r.resolveFramework(ctx, &ad, &conds)

	var binding *airunwayv1alpha1.ModelBindingStatus
	modelBound := false
	if frameworkReady {
		var requeue bool
		binding, modelBound, requeue = r.resolveModelBinding(ctx, &ad, framework.provider, &conds)
		if requeue {
			result.RequeueAfter = agentRequeueInterval
			// Retryable failure (endpoint not published yet, Secret lookup
			// errored, gateway has no address). Hold the last good binding
			// rather than clearing it — see retainBinding.
			binding = retainBinding(binding, &ad)
		}
		if modelBound && bindingNeedsRefresh(&ad) {
			// Credential Secrets are not watched (that would require a
			// cluster-wide Secret informer), so a revoked credential is only
			// noticed on the next pass. Bound the staleness.
			result.RequeueAfter = agentCredentialRefreshInterval
		}
	} else {
		// Cannot validate binding modes without the provider's capabilities.
		setAgentCondition(&conds, airunwayv1alpha1.AgentConditionTypeModelBound, metav1.ConditionFalse,
			ad.Generation, "FrameworkNotReady", "Waiting for the framework provider before resolving model bindings")
		result.RequeueAfter = agentRequeueInterval
		binding = retainBinding(nil, &ad)
	}

	// Aggregate readiness. Ready requires the two core preconditions plus the
	// provider-owned ProviderReady, so a fresh AgentDeployment is never Ready
	// until the framework provider has rendered and reported a healthy workload.
	providerReady := providerReadyForGeneration(conds, ad.Generation)
	switch {
	case frameworkReady && modelBound && providerReady:
		setAgentCondition(&conds, airunwayv1alpha1.AgentConditionTypeReady, metav1.ConditionTrue,
			ad.Generation, "AgentReady", "Framework resolved, model bindings resolved, and provider reports ready")
	case frameworkReady && modelBound:
		setAgentCondition(&conds, airunwayv1alpha1.AgentConditionTypeReady, metav1.ConditionFalse,
			ad.Generation, "WaitingForProvider", "Core resolution complete; waiting for the framework provider to report ready")
	default:
		setAgentCondition(&conds, airunwayv1alpha1.AgentConditionTypeReady, metav1.ConditionFalse,
			ad.Generation, "ResolutionIncomplete", "Framework or model binding resolution is incomplete")
	}

	if err := r.applyCoreStatus(ctx, &ad, framework.status, binding, conds); err != nil {
		logger.Error(err, "Failed to apply core status", "name", ad.Name)
		return ctrl.Result{}, err
	}

	return result, nil
}

// providerReadyForGeneration reports whether the provider has declared itself
// ready FOR THE CURRENT SPEC.
//
// Aggregate readiness must not inherit a stale verdict. Immediately after a
// user edits the spec, the provider's previous ProviderReady=True is still
// present but describes the old generation — publishing Ready=True from it
// tells the user their change is live before the provider has even seen it.
//
// ObservedGeneration 0 means the condition predates generation tracking; accept
// it rather than stalling agents written by an older controller.
func providerReadyForGeneration(conds []metav1.Condition, generation int64) bool {
	cond := meta.FindStatusCondition(conds, airunwayv1alpha1.AgentConditionTypeProviderReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return false
	}
	return cond.ObservedGeneration == 0 || cond.ObservedGeneration >= generation
}

// bindingHoldWindow bounds how long a published binding survives a failure it
// cannot re-verify.
//
// Holding is right for a blip and wrong forever. The two revocations users
// actually perform — deleting the bound ModelDeployment, deleting the
// credential Secret — both surface as retryable NotFound, so an unbounded hold
// makes them permanently indistinguishable from a momentary failure: the agent
// keeps running against an endpoint that no longer exists, or a credential that
// was revoked. After this window the binding is cleared and providers tear
// down, which is what "revoked" is supposed to mean.
//
// Sized well above a rollout or a brief API-server disruption, and well below
// the point where a stale endpoint stops being a surprise.
const bindingHoldWindow = 10 * time.Minute

// retainBinding keeps the previously resolved status.modelBinding across a
// RETRYABLE resolution failure, for at most bindingHoldWindow.
//
// status.modelBinding is core-owned and json:omitempty, so applying a nil
// binding makes the API server DELETE the field. Every provider treats a
// missing binding as "revoked" and tears down the workloads it rendered. That
// turns a momentary blip — a discovery error flipping framework readiness, a
// bound ModelDeployment briefly republishing its endpoint, a transient Secret
// lookup error — into a full teardown and re-render of every agent on that
// framework.
//
// So a retryable failure holds the last good binding and reports the reason on
// the ModelBound condition instead. Only TERMINAL invalidity (unsupported
// binding mode, cross-namespace reference, no recognised mode) clears the
// binding, because in those cases the agent genuinely must stop.
func retainBinding(resolved *airunwayv1alpha1.ModelBindingStatus, ad *airunwayv1alpha1.AgentDeployment) *airunwayv1alpha1.ModelBindingStatus {
	if resolved != nil {
		return resolved
	}
	if ad.Status.ModelBinding == nil {
		return nil
	}
	if bindingHoldExpired(ad) {
		// The failure has persisted long enough to be a revocation rather than
		// a blip. Clearing the binding is what makes providers stop the agent.
		return nil
	}
	return ad.Status.ModelBinding
}

// bindingHoldExpired reports whether ModelBound has been False for longer than
// bindingHoldWindow.
//
// The condition's LastTransitionTime is the clock: meta.SetStatusCondition only
// moves it when Status actually changes, so it marks when re-verification first
// started failing rather than when we last looked.
func bindingHoldExpired(ad *airunwayv1alpha1.AgentDeployment) bool {
	cond := meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeModelBound)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.LastTransitionTime.IsZero() {
		return false
	}
	return time.Since(cond.LastTransitionTime.Time) > bindingHoldWindow
}

// bindingNeedsRefresh reports whether a resolved binding depends on something
// this controller does not watch, and so needs periodic re-validation.
//
// ModelDeployment IS watched, so deploymentRef re-resolves on change. The other
// two inputs are not:
//
//   - a credential Secret — watching Secrets would need a cluster-wide informer
//     with list/watch on every Secret, which is exactly the RBAC and memory cost
//     the uncached read exists to avoid;
//   - a Gateway's published status address, which can change under a resolved
//     gatewayEndpoint binding and leave the agent pointed at a dead address.
//
// A slow timer is the cheap correct answer for both.
func bindingNeedsRefresh(ad *airunwayv1alpha1.AgentDeployment) bool {
	if ad.Spec.Model.GatewayEndpoint != nil {
		return true
	}
	return ad.Spec.Model.ExternalAPI != nil && ad.Spec.Model.ExternalAPI.CredentialsRef != nil
}

// frameworkNotReadyDetail turns an unready AgentProviderConfig into a reason
// and message an operator can act on from the AgentDeployment alone.
//
// It prefers the provider's own Ready condition, which already carries the
// specific cause (OperatorNotInstalled, CapabilitiesMissing, DiscoveryFailed)
// and, where the provider config supplies one, the install instructions. It
// falls back to a generic message when the provider has published nothing yet.
func frameworkNotReadyDetail(apc *airunwayv1alpha1.AgentProviderConfig, name string) (reason, message string) {
	cond := meta.FindStatusCondition(apc.Status.Conditions, agentProviderReadyCondition)
	if cond == nil || cond.Reason == "" {
		return "FrameworkNotReady",
			fmt.Sprintf("Framework provider %q is registered but not reporting ready", name)
	}

	message = fmt.Sprintf("Framework provider %q is not ready: %s", name, cond.Message)
	if install := apc.InstallInstructions(); install != "" && !strings.Contains(cond.Message, install) {
		message = fmt.Sprintf("%s. Install instructions: %s", message, install)
	}
	return cond.Reason, message
}

// resolvedFramework carries the outcome of framework resolution.
type resolvedFramework struct {
	// provider is the resolved AgentProviderConfig; nil when unresolved.
	provider *airunwayv1alpha1.AgentProviderConfig
	// status is the core-owned status.framework value to publish; nil when unresolved.
	status *airunwayv1alpha1.AgentFrameworkStatus
}

// resolveFramework looks up the AgentProviderConfig named by
// spec.framework.name, verifies it is registered and ready, and sets the
// FrameworkReady condition. AgentProviderConfig is cluster-scoped.
func (r *AgentDeploymentReconciler) resolveFramework(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	conds *[]metav1.Condition,
) (resolvedFramework, bool) {
	name := ad.Spec.Framework.Name

	var apc airunwayv1alpha1.AgentProviderConfig
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: name}, &apc); err != nil {
		if apierrors.IsNotFound(err) {
			setAgentCondition(conds, airunwayv1alpha1.AgentConditionTypeFrameworkReady, metav1.ConditionFalse,
				ad.Generation, "FrameworkNotRegistered",
				fmt.Sprintf("No AgentProviderConfig named %q is registered in the cluster", name))
			return resolvedFramework{}, false
		}
		setAgentCondition(conds, airunwayv1alpha1.AgentConditionTypeFrameworkReady, metav1.ConditionFalse,
			ad.Generation, "FrameworkLookupFailed", err.Error())
		return resolvedFramework{}, false
	}

	if apc.Status.Ready == nil || !*apc.Status.Ready {
		// Carry the provider's own reason and install hint onto the agent.
		// Without this the agent says only "registered but not reporting
		// ready", and the operator has no way to know the actual cause is
		// "the framework's operator isn't installed" — let alone how to
		// install it — because that lives on a cluster-scoped object they may
		// not think to look at, or have access to read.
		reason, message := frameworkNotReadyDetail(&apc, name)
		setAgentCondition(conds, airunwayv1alpha1.AgentConditionTypeFrameworkReady, metav1.ConditionFalse,
			ad.Generation, reason, message)
		// Still publish the resolved framework name so the deployer can see
		// which provider it is waiting on.
		return resolvedFramework{
			provider: &apc,
			status:   &airunwayv1alpha1.AgentFrameworkStatus{Name: name, ProviderVersion: apc.Status.Version},
		}, false
	}

	setAgentCondition(conds, airunwayv1alpha1.AgentConditionTypeFrameworkReady, metav1.ConditionTrue,
		ad.Generation, "FrameworkResolved", fmt.Sprintf("Framework provider %q is registered and ready", name))
	return resolvedFramework{
		provider: &apc,
		status:   &airunwayv1alpha1.AgentFrameworkStatus{Name: name, ProviderVersion: apc.Status.Version},
	}, true
}

// resolveModelBinding resolves spec.model into a ModelBindingStatus, validating
// the binding mode against the provider's declared capabilities. It returns the
// resolved binding, whether the binding resolved (modelBound), and whether the
// caller should requeue for a dependency that is not yet satisfiable.
func (r *AgentDeploymentReconciler) resolveModelBinding(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	provider *airunwayv1alpha1.AgentProviderConfig,
	conds *[]metav1.Condition,
) (binding *airunwayv1alpha1.ModelBindingStatus, modelBound bool, requeue bool) {
	caps := provider.Spec.Capabilities
	m := &ad.Spec.Model
	mode := bindingMode(m)

	if !caps.HasBindingMode(mode) {
		setAgentCondition(conds, airunwayv1alpha1.AgentConditionTypeModelBound, metav1.ConditionFalse,
			ad.Generation, "UnsupportedBindingMode",
			fmt.Sprintf("spec.model uses mode %q which framework %q does not support", mode, provider.Name))
		return nil, false, false
	}

	st, ok, rq, reason, msg := r.resolveOneBinding(ctx, ad, m, mode)
	if !ok {
		setAgentCondition(conds, airunwayv1alpha1.AgentConditionTypeModelBound, metav1.ConditionFalse,
			ad.Generation, reason, msg)
		return nil, false, rq
	}

	setAgentCondition(conds, airunwayv1alpha1.AgentConditionTypeModelBound, metav1.ConditionTrue,
		ad.Generation, "ModelBound", "Resolved model binding")
	return &st, true, false
}

// resolveOneBinding resolves a single ModelBinding into its status form. On
// failure it returns ok=false with a condition reason/message and whether to
// requeue (for a dependency that may become satisfiable later).
func (r *AgentDeploymentReconciler) resolveOneBinding(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	m *airunwayv1alpha1.ModelBinding,
	mode airunwayv1alpha1.ModelBindingMode,
) (st airunwayv1alpha1.ModelBindingStatus, ok, requeue bool, reason, msg string) {
	st = airunwayv1alpha1.ModelBindingStatus{BindingMode: mode}

	switch mode {
	case airunwayv1alpha1.ModelBindingModeExternalAPI:
		return r.resolveExternalAPI(ctx, ad, m, st)

	case airunwayv1alpha1.ModelBindingModeDeploymentRef:
		return r.resolveDeploymentRef(ctx, ad, m, st)

	case airunwayv1alpha1.ModelBindingModeGatewayEndpoint:
		return r.resolveGatewayEndpointBinding(ctx, ad, m, st)

	default:
		return st, false, false, "UnknownBindingMode", "spec.model has no recognised binding mode"
	}
}

func (r *AgentDeploymentReconciler) resolveExternalAPI(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	m *airunwayv1alpha1.ModelBinding,
	st airunwayv1alpha1.ModelBindingStatus,
) (airunwayv1alpha1.ModelBindingStatus, bool, bool, string, string) {
	ext := m.ExternalAPI
	st.APIType = ext.Type
	st.BaseURL = ext.BaseURL
	st.ModelName = ext.ModelName
	st.CredentialsRef = ext.CredentialsRef
	if ext.CredentialsRef == nil {
		return st, true, false, "", ""
	}

	// The admission webhook is the only place the *requesting user* is checked
	// against the Secret they referenced. With webhooks disabled that check does
	// not run at all, so resolving the credential here would reinstate the
	// escalation the webhook exists to prevent: anyone able to create an
	// AgentDeployment could name any Secret in the namespace and have it injected
	// into an image of their choosing.
	//
	// Keyless bindings are unaffected — only credential-bearing ones are refused,
	// so a webhook-less development cluster still runs everything else.
	if !r.CredentialAdmissionActive {
		return st, false, false, "CredentialAuthorizationUnavailable",
			"credential-bearing bindings are refused because the validating webhook is disabled, " +
				"and it is the only place the requesting user is authorized against the referenced Secret; " +
				"enable the webhook, or use a binding with no credentialsRef"
	}

	var sec corev1.Secret
	ref := ext.CredentialsRef
	key := k8stypes.NamespacedName{Name: ref.Name, Namespace: ad.Namespace}
	if err := r.secretReader().Get(ctx, key, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return st, false, true, "CredentialSecretNotFound",
				fmt.Sprintf("spec.model.externalAPI.credentialsRef references Secret %s/%s which does not exist", ad.Namespace, ref.Name)
		}
		return st, false, true, "CredentialSecretLookupFailed", err.Error()
	}
	if _, ok := sec.Data[ref.Key]; !ok {
		return st, false, true, "CredentialKeyNotFound",
			fmt.Sprintf("spec.model.externalAPI.credentialsRef points to missing key %q in Secret %s/%s", ref.Key, ad.Namespace, ref.Name)
	}
	return st, true, false, "", ""
}

func (r *AgentDeploymentReconciler) resolveGatewayEndpointBinding(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	m *airunwayv1alpha1.ModelBinding,
	st airunwayv1alpha1.ModelBindingStatus,
) (airunwayv1alpha1.ModelBindingStatus, bool, bool, string, string) {
	gw := m.GatewayEndpoint
	ns := gw.GatewayRef.Namespace
	if ns == "" {
		ns = ad.Namespace
	}

	// Same guard as deploymentRef above, for the same reason: the controller
	// reads the Gateway with cluster-wide access, so without this an
	// AgentDeployment in one namespace could name a Gateway in another and have
	// its address resolved and published into status on their behalf. Leaving
	// the check on only two of the three binding modes made the third the
	// obvious way around it.
	if ns != ad.Namespace {
		return st, false, false, "CrossNamespaceRefNotAllowed",
			fmt.Sprintf("spec.model.gatewayEndpoint references Gateway %s/%s in another namespace; cross-namespace references require an AgentReferenceGrant (not yet supported)", ns, gw.GatewayRef.Name)
	}

	st.ModelName = gw.ModelName
	var gateway unstructured.Unstructured
	gateway.SetAPIVersion("gateway.networking.k8s.io/v1")
	gateway.SetKind("Gateway")
	key := k8stypes.NamespacedName{Name: gw.GatewayRef.Name, Namespace: ns}
	if err := r.Get(ctx, key, &gateway); err != nil {
		if apierrors.IsNotFound(err) {
			return st, false, true, "GatewayNotFound",
				fmt.Sprintf("spec.model.gatewayEndpoint references Gateway %s/%s which does not exist", ns, gw.GatewayRef.Name)
		}
		if meta.IsNoMatchError(err) {
			return st, false, true, "GatewayAPIUnavailable",
				"gateway.networking.k8s.io/v1 is not available in this cluster; install Gateway API CRDs to use spec.model.gatewayEndpoint"
		}
		return st, false, true, "GatewayLookupFailed", err.Error()
	}

	baseURL := normalizeOpenAIBaseURL(gatewayStatusAddress(&gateway))
	if baseURL == "" {
		return st, false, true, "GatewayNotReady",
			fmt.Sprintf("spec.model.gatewayEndpoint target Gateway %s/%s has no published status address", ns, gw.GatewayRef.Name)
	}
	st.BaseURL = baseURL
	return st, true, false, "", ""
}

// resolveDeploymentRef resolves an in-cluster ModelDeployment reference into a
// binding: the OpenAI-compatible base URL from the model's gateway/service
// endpoint, the served model name, and the ModelDeployment UID so providers
// re-render if the target is deleted and recreated.
func (r *AgentDeploymentReconciler) resolveDeploymentRef(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	m *airunwayv1alpha1.ModelBinding,
	st airunwayv1alpha1.ModelBindingStatus,
) (airunwayv1alpha1.ModelBindingStatus, bool, bool, string, string) {
	ref := m.DeploymentRef
	ns := ref.Namespace
	if ns == "" {
		ns = ad.Namespace
	}

	// A cross-namespace deploymentRef would let a namespaced AgentDeployment
	// consume a ModelDeployment in another namespace using the controller's
	// cluster-wide access. Until AgentReferenceGrant enforcement exists,
	// reject references whose resolved namespace differs from the
	// AgentDeployment's own namespace.
	if ns != ad.Namespace {
		return st, false, false, "CrossNamespaceRefNotAllowed",
			fmt.Sprintf("spec.model references ModelDeployment %s/%s in another namespace; cross-namespace references require an AgentReferenceGrant (not yet supported)", ns, ref.Name)
	}

	var md airunwayv1alpha1.ModelDeployment
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: ref.Name, Namespace: ns}, &md); err != nil {
		if apierrors.IsNotFound(err) {
			return st, false, true, "ModelDeploymentNotFound",
				fmt.Sprintf("spec.model references ModelDeployment %s/%s which does not exist", ns, ref.Name)
		}
		return st, false, true, "ModelDeploymentLookupFailed", err.Error()
	}

	st.ObservedResourceUID = string(md.UID)
	st.BaseURL, st.ModelName = modelDeploymentEndpoint(&md)
	if st.BaseURL == "" {
		return st, false, true, "ModelDeploymentNotReady",
			fmt.Sprintf("spec.model target ModelDeployment %s/%s has no resolved endpoint yet", ns, ref.Name)
	}

	// In-cluster model endpoints are keyless. Core leaves credentials empty and
	// each provider backend handles this explicitly: container injects a literal
	// OPENAI_API_KEY value, and CRD backends provision a managed no-auth Secret.
	return st, true, false, "", ""
}

// applyCoreStatus writes ONLY the core-owned status fields via server-side
// apply under the core field owner. Provider-owned fields (phase, runtime,
// replicas, ProviderReady) are omitted, so the API server leaves the
// provider's writes untouched. The shared conditions list is listType=map
// keyed by type, so SSA merges core and provider conditions per key.
func (r *AgentDeploymentReconciler) applyCoreStatus(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	framework *airunwayv1alpha1.AgentFrameworkStatus,
	binding *airunwayv1alpha1.ModelBindingStatus,
	conds []metav1.Condition,
) error {
	apply := &airunwayv1alpha1.AgentDeployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: airunwayv1alpha1.GroupVersion.String(),
			Kind:       "AgentDeployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ad.Name,
			Namespace: ad.Namespace,
		},
		Status: airunwayv1alpha1.AgentDeploymentStatus{
			Framework:          framework,
			ModelBinding:       binding,
			ObservedGeneration: ad.Generation,
			Conditions:         coreOwnedConditions(conds),
		},
	}

	// Not forced: core and each provider own disjoint status fields and
	// disjoint condition-map entries, so a conflict means a second writer is
	// genuinely claiming a core-owned field. Forcing would steal it back every
	// reconcile and turn the documented ownership split into last-writer-wins.
	return r.Status().Patch(ctx, apply, client.Apply, client.FieldOwner(AgentCoreFieldOwner))
}

// coreOwnedConditions filters a condition list down to the types the core
// controller owns, so the SSA apply does not claim ownership of (or clobber)
// the provider-owned ProviderReady condition.
func coreOwnedConditions(conds []metav1.Condition) []metav1.Condition {
	owned := map[string]bool{
		airunwayv1alpha1.AgentConditionTypeFrameworkReady: true,
		airunwayv1alpha1.AgentConditionTypeModelBound:     true,
		airunwayv1alpha1.AgentConditionTypeReady:          true,
	}
	var out []metav1.Condition
	for _, c := range conds {
		if owned[c.Type] {
			out = append(out, c)
		}
	}
	return out
}

// bindingMode reports which binding mode a ModelBinding uses. The CRD's CEL
// validation guarantees exactly one is set; this mirrors that for the
// resolved status.
func bindingMode(m *airunwayv1alpha1.ModelBinding) airunwayv1alpha1.ModelBindingMode {
	switch {
	case m.DeploymentRef != nil:
		return airunwayv1alpha1.ModelBindingModeDeploymentRef
	case m.GatewayEndpoint != nil:
		return airunwayv1alpha1.ModelBindingModeGatewayEndpoint
	case m.ExternalAPI != nil:
		return airunwayv1alpha1.ModelBindingModeExternalAPI
	default:
		return ""
	}
}

// modelDeploymentEndpoint derives an OpenAI-compatible base URL and served
// model name from a ModelDeployment's status. It prefers the resolved gateway
// status endpoint when gateway routing is configured (so deploymentRef follows
// the same OpenAI-native/BBR path), then falls back to the model service
// endpoint.
// Returns an empty base URL when the ModelDeployment has not published any
// usable endpoint yet.
func modelDeploymentEndpoint(md *airunwayv1alpha1.ModelDeployment) (baseURL, modelName string) {
	modelName = md.Name
	if md.Spec.Model.ServedName != "" {
		modelName = md.Spec.Model.ServedName
	} else if md.Spec.Model.ID != "" {
		modelName = md.Spec.Model.ID
	}

	if gw := md.Status.Gateway; gw != nil && gw.Endpoint != "" {
		if gw.ModelName != "" {
			modelName = gw.ModelName
		}
		return normalizeOpenAIBaseURL(gw.Endpoint), modelName
	}
	if gw := md.Status.Gateway; gw != nil {
		if gw.ModelName != "" {
			modelName = gw.ModelName
		}
	}

	if ep := md.Status.Endpoint; ep != nil && ep.Service != "" {
		port := ep.Port
		if port == 0 {
			port = 80
		}
		return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/v1", ep.Service, md.Namespace, port), modelName
	}

	return "", modelName
}

// normalizeOpenAIBaseURL ensures the URL is HTTP(S) and includes the /v1
// OpenAI-compatible path expected by providers.
func normalizeOpenAIBaseURL(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	endpoint = strings.TrimRight(endpoint, "/")
	if endpoint == "" {
		return ""
	}
	for _, scheme := range []string{"http://", "https://"} {
		if !strings.HasPrefix(endpoint, scheme) {
			continue
		}
		endpoint = scheme + bracketBareIPv6(strings.TrimPrefix(endpoint, scheme))
		if strings.HasSuffix(endpoint, "/v1") {
			return endpoint
		}
		return endpoint + "/v1"
	}
	return "http://" + bracketBareIPv6(endpoint) + "/v1"
}

// bracketBareIPv6 wraps a bare IPv6 literal so it is a valid URL host.
//
// Gateway API publishes `IPAddress` status values unbracketed — "2001:db8::1",
// not "[2001:db8::1]" — but RFC 3986 requires the brackets. Without them the
// first colon reads as a port separator and "http://2001:db8::1/v1" is not a
// parseable URL, so every agent bound through that gateway gets an endpoint it
// cannot dial.
//
// A host:port has exactly one colon, so two or more means an IPv6 literal;
// anything already bracketed, or carrying a path, is left alone.
func bracketBareIPv6(host string) string {
	if host == "" || strings.HasPrefix(host, "[") || strings.Contains(host, "/") {
		return host
	}
	if strings.Count(host, ":") < 2 {
		return host
	}
	return "[" + host + "]"
}

func gatewayStatusAddress(gw *unstructured.Unstructured) string {
	addresses, found, err := unstructured.NestedSlice(gw.Object, "status", "addresses")
	if err != nil || !found {
		return ""
	}
	for _, raw := range addresses {
		addr, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		value, ok := addr["value"].(string)
		if ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// setAgentCondition upserts a condition, preserving LastTransitionTime on
// unchanged status (meta.SetStatusCondition only adopts the new timestamp when
// Status changes), so repeated reconciles do not churn the status.
func setAgentCondition(conds *[]metav1.Condition, condType string, status metav1.ConditionStatus, generation int64, reason, message string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// mapProviderConfigToAgentDeployments enqueues AgentDeployments affected by a
// change to an AgentProviderConfig (e.g. it just became ready), scoped to the
// framework that changed.
func (r *AgentDeploymentReconciler) mapProviderConfigToAgentDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
	apc, ok := obj.(*airunwayv1alpha1.AgentProviderConfig)
	if !ok {
		return nil
	}

	agents := agentprovider.AgentsForFramework(ctx, r.Client, apc.Name)
	reqs := make([]reconcile.Request, 0, len(agents))
	for i := range agents {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: k8stypes.NamespacedName{
				Name:      agents[i].Name,
				Namespace: agents[i].Namespace,
			},
		})
	}
	return reqs
}

// mapModelDeploymentToAgentDeployments enqueues AgentDeployments bound to the
// changed ModelDeployment via spec.model.deploymentRef.
func (r *AgentDeploymentReconciler) mapModelDeploymentToAgentDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
	md, ok := obj.(*airunwayv1alpha1.ModelDeployment)
	if !ok {
		return nil
	}

	var list airunwayv1alpha1.AgentDeploymentList
	modelRef := md.Namespace + "/" + md.Name
	if err := r.List(ctx, &list, client.MatchingFields{agentDeploymentModelRefIndexKey: modelRef}); err != nil {
		if err := r.List(ctx, &list); err != nil {
			return nil
		}
	}

	var reqs []reconcile.Request
	for i := range list.Items {
		ref := list.Items[i].Spec.Model.DeploymentRef
		if ref == nil {
			continue
		}
		refNS := ref.Namespace
		if refNS == "" {
			refNS = list.Items[i].Namespace
		}
		if refNS != md.Namespace || ref.Name != md.Name {
			continue
		}
		reqs = append(reqs, reconcile.Request{
			NamespacedName: k8stypes.NamespacedName{
				Name:      list.Items[i].Name,
				Namespace: list.Items[i].Namespace,
			},
		})
	}
	return reqs
}

// SetupWithManager wires the core AgentDeployment controller. It watches
// AgentProviderConfig and ModelDeployment so an AgentDeployment re-reconciles
// when framework readiness or model binding inputs change.
func (r *AgentDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := agentprovider.EnsureFrameworkIndex(mgr); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &airunwayv1alpha1.AgentDeployment{}, agentDeploymentModelRefIndexKey, func(raw client.Object) []string {
		ad, ok := raw.(*airunwayv1alpha1.AgentDeployment)
		if !ok || ad.Spec.Model.DeploymentRef == nil {
			return nil
		}
		ns := ad.Spec.Model.DeploymentRef.Namespace
		if ns == "" {
			ns = ad.Namespace
		}
		if ad.Spec.Model.DeploymentRef.Name == "" || ns == "" {
			return nil
		}
		return []string{ns + "/" + ad.Spec.Model.DeploymentRef.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.AgentDeployment{}).
		Watches(
			&airunwayv1alpha1.AgentProviderConfig{},
			handler.EnqueueRequestsFromMapFunc(r.mapProviderConfigToAgentDeployments),
			ctrlbuilder.WithPredicates(agentprovider.ProviderConfigRelevantChange()),
		).
		Watches(
			&airunwayv1alpha1.ModelDeployment{},
			handler.EnqueueRequestsFromMapFunc(r.mapModelDeploymentToAgentDeployments),
			ctrlbuilder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Named("agentdeployment").
		Complete(r)
}
