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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
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
}

// +kubebuilder:rbac:groups=airunway.ai,resources=agentdeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=agentdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=agentdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=airunway.ai,resources=agentproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments,verbs=get;list;watch

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
		}
	} else {
		// Cannot validate binding modes without the provider's capabilities.
		setAgentCondition(&conds, airunwayv1alpha1.AgentConditionTypeModelBound, metav1.ConditionFalse,
			ad.Generation, "FrameworkNotReady", "Waiting for the framework provider before resolving model bindings")
		result.RequeueAfter = agentRequeueInterval
	}

	// Aggregate readiness. Ready requires the two core preconditions plus the
	// provider-owned ProviderReady, so a fresh AgentDeployment is never Ready
	// until the framework provider has rendered and reported a healthy workload.
	providerReady := meta.IsStatusConditionTrue(conds, airunwayv1alpha1.AgentConditionTypeProviderReady)
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
		setAgentCondition(conds, airunwayv1alpha1.AgentConditionTypeFrameworkReady, metav1.ConditionFalse,
			ad.Generation, "FrameworkNotReady",
			fmt.Sprintf("Framework provider %q is registered but not reporting ready", name))
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
		ext := m.ExternalAPI
		st.APIType = ext.Type
		st.BaseURL = ext.BaseURL
		st.ModelName = ext.ModelName
		st.CredentialsRef = ext.CredentialsRef
		return st, true, false, "", ""

	case airunwayv1alpha1.ModelBindingModeDeploymentRef:
		return r.resolveDeploymentRef(ctx, ad, m, st)

	case airunwayv1alpha1.ModelBindingModeGatewayEndpoint:
		gw := m.GatewayEndpoint
		// Gateway endpoint resolution is intentionally minimal for the PoC:
		// the served model name is authoritative and the base URL is derived
		// from the referenced Gateway's in-cluster address. Full GAIE
		// endpoint discovery is a follow-up.
		st.ModelName = gw.ModelName
		st.BaseURL = gatewayBaseURL(gw, ad.Namespace)
		return st, true, false, "", ""

	default:
		return st, false, false, "UnknownBindingMode", "spec.model has no recognised binding mode"
	}
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

	return r.Status().Patch(ctx, apply, client.Apply,
		client.FieldOwner(AgentCoreFieldOwner),
		client.ForceOwnership,
	)
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
// model name from a ModelDeployment's status. It prefers the in-cluster Gateway
// service URL when gateway routing is configured (so deploymentRef follows the
// same OpenAI-native/BBR path as gatewayEndpoint bindings), then falls back to
// the gateway endpoint address, and finally to the model service endpoint.
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
		if gw.GatewayName != "" && gw.GatewayNamespace != "" {
			return fmt.Sprintf("http://%s.%s.svc.cluster.local/v1", gw.GatewayName, gw.GatewayNamespace), modelName
		}
		return normalizeOpenAIBaseURL(gw.Endpoint), modelName
	}
	if gw := md.Status.Gateway; gw != nil {
		if gw.ModelName != "" {
			modelName = gw.ModelName
		}
		if gw.GatewayName != "" && gw.GatewayNamespace != "" {
			return fmt.Sprintf("http://%s.%s.svc.cluster.local/v1", gw.GatewayName, gw.GatewayNamespace), modelName
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
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		if strings.HasSuffix(endpoint, "/v1") {
			return endpoint
		}
		return endpoint + "/v1"
	}
	return "http://" + endpoint + "/v1"
}

// gatewayBaseURL derives the in-cluster base URL for a Gateway reference.
func gatewayBaseURL(gw *airunwayv1alpha1.GatewayEndpointBinding, defaultNS string) string {
	ns := gw.GatewayRef.Namespace
	if ns == "" {
		ns = defaultNS
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local/v1", gw.GatewayRef.Name, ns)
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

	var list airunwayv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}

	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.Framework.Name == apc.Name {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: k8stypes.NamespacedName{
					Name:      list.Items[i].Name,
					Namespace: list.Items[i].Namespace,
				},
			})
		}
	}
	return reqs
}

// SetupWithManager wires the core AgentDeployment controller. It watches
// AgentProviderConfig so an AgentDeployment re-reconciles when its framework
// provider becomes ready.
func (r *AgentDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.AgentDeployment{}).
		Watches(
			&airunwayv1alpha1.AgentProviderConfig{},
			handler.EnqueueRequestsFromMapFunc(r.mapProviderConfigToAgentDeployments),
			ctrlbuilder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Named("agentdeployment").
		Complete(r)
}
