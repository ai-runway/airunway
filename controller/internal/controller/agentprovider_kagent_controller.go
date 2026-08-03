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
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
)

const (
	// KagentFrameworkName is the AgentProviderConfig / spec.framework.name
	// this provider reconciles.
	KagentFrameworkName = "kagent"

	// KagentFieldOwner is this provider's server-side apply field manager.
	// It is distinct from AgentCoreFieldOwner so the API server prevents the
	// provider from clobbering core-owned status fields and vice versa.
	KagentFieldOwner = "airunway-agents-kagent"

	// kagentAPIVersion is the kagent CRD group/version this provider renders
	// against. kagent v1alpha2 restructured Agent into type + declarative{};
	// v1alpha1 panics the kagent controller, so v1alpha2 is required.
	kagentAPIVersion = "kagent.dev/v1alpha2"
)

// kagentAgentGVK / kagentModelConfigGVK are the unstructured GVKs this
// provider renders. Rendering as unstructured avoids a compile-time
// dependency on kagent's Go types, matching how the inference providers
// handle upstream CRDs.
var (
	kagentAgentGVK = schema.GroupVersionKind{
		Group: "kagent.dev", Version: "v1alpha2", Kind: "Agent",
	}
	kagentModelConfigGVK = schema.GroupVersionKind{
		Group: "kagent.dev", Version: "v1alpha2", Kind: "ModelConfig",
	}
)

// KagentProviderReconciler renders an AgentDeployment whose framework is
// "kagent" (a crd-backend framework) into kagent-native Agent + ModelConfig
// custom resources, consuming the core-resolved status.modelBinding. It owns
// the provider half of the AgentDeployment status (phase, runtime, replicas,
// ProviderReady).
type KagentProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// kagentConfig is the framework-specific spec.config contract for kagent.
// The core controller keeps spec.config opaque (RawExtension); each provider
// parses the shape it understands. This is the PoC's pinned kagent contract.
type kagentConfig struct {
	SystemPrompt string `json:"systemPrompt,omitempty"`
	Description  string `json:"description,omitempty"`
	// Runtime selects the kagent ADK runtime ("python" or "go"). Defaults to
	// "python": it is kagent's full-featured default and its image is the one
	// the project publishes reliably (the "go" golang-adk image has had its
	// pinned digests disappear from cr.kagent.dev). Override to "go" only when
	// the faster-startup Go ADK is required and its image is known-good.
	Runtime string `json:"runtime,omitempty"`
}

// +kubebuilder:rbac:groups=kagent.dev,resources=agents;modelconfigs,verbs=get;list;watch;create;update;patch;delete
// Framework providers WRITE their managed no-auth Secret but never READ any
// Secret — no `get`. Credential resolution is the core controller's job.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;update;patch

// Reconcile renders the kagent-native resources for a kagent AgentDeployment.
func (r *KagentProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ad airunwayv1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &ad); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only handle agents for this framework; ignore others. Garbage
	// collection via owner references handles deletion.
	if ad.Spec.Framework.Name != KagentFrameworkName || !ad.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Also require the framework to be registered with the crd backend. Without
	// this check a provider config named "kagent" but declaring the container
	// backend would be rendered by both this provider and the generic container
	// provider, each force-owning the same ProviderReady condition.
	crdBacked, err := agentprovider.FrameworkUsesBackend(ctx, r.Client, KagentFrameworkName, airunwayv1alpha1.AgentProviderBackendCRD)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !crdBacked {
		// The framework is gone, has no capabilities, or has been re-registered
		// with a different backend. Anything this provider already rendered for
		// the agent is now orphaned — nothing will reconcile it again, and if
		// the framework moved to the container backend that provider is already
		// rendering a second workload alongside it. Tear ours down before
		// standing aside, rather than leaving it serving unmanaged.
		//
		// Safe for agents this provider never rendered: DeleteOwned skips
		// objects that are absent, of an unserved kind, or not controlled by
		// this AgentDeployment.
		if err := agentprovider.CleanupOwned(ctx, r.Client, &ad,
			agentprovider.UnstructuredRef(kagentModelConfigGVK, ad.Name+"-model", ad.Namespace),
			agentprovider.UnstructuredRef(kagentAgentGVK, ad.Name, ad.Namespace),
		); err != nil {
			return ctrl.Result{}, err
		}
		// Deliberately no status write: ProviderReady now belongs to whichever
		// provider owns this framework, and two writers would fight over it.
		return ctrl.Result{}, nil
	}

	// Consume the core-resolved binding. Never build a ModelConfig from a
	// half-resolved endpoint.
	switch agentprovider.ClassifyBinding(&ad) {
	case agentprovider.BindingUnavailable:
		// No binding at all: tear down the rendered Agent/ModelConfig so it
		// stops using a stale endpoint before reporting Pending.
		if err := agentprovider.CleanupOwned(ctx, r.Client, &ad,
			agentprovider.UnstructuredRef(kagentModelConfigGVK, ad.Name+"-model", ad.Namespace),
			agentprovider.UnstructuredRef(kagentAgentGVK, ad.Name, ad.Namespace),
		); err != nil {
			statusErr := r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
				metav1.ConditionFalse, "BindingCleanupFailed", err.Error())
			if statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhasePending, nil, nil,
			metav1.ConditionFalse, "WaitingForBindings", "Waiting for the core controller to resolve model bindings")
	case agentprovider.BindingStale:
		// Binding published but not re-verified this pass — hold rather than
		// deleting a healthy agent over a transient failure.
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	binding := *ad.Status.ModelBinding
	binding, err = agentprovider.EnsureBindingCredentials(ctx, r.Client, r.Scheme, &ad, binding, KagentFieldOwner)
	if err != nil {
		statusErr := r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
			metav1.ConditionFalse, "CredentialProvisionFailed", err.Error())
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}
	cfg, err := parseKagentConfig(ad.Spec.Config)
	if err != nil {
		return ctrl.Result{}, r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
			metav1.ConditionFalse, "InvalidConfig", err.Error())
	}

	modelConfig := renderKagentModelConfig(&ad, binding)
	agent := renderKagentAgent(&ad, cfg, modelConfig.GetName())

	for _, obj := range []*unstructured.Unstructured{modelConfig, agent} {
		if err := agentprovider.VerifyOwnedOrAbsent(ctx, r.Client, r.Scheme, &ad, obj); err != nil {
			statusErr := r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
				metav1.ConditionFalse, "OwnershipConflict", err.Error())
			if statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
		if err := controllerutil.SetControllerReference(&ad, obj, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set owner reference on %s: %w", obj.GetKind(), err)
		}
		if err := r.Patch(ctx, obj, client.Apply, client.FieldOwner(KagentFieldOwner), client.ForceOwnership); err != nil {
			logger.Error(err, "Failed to apply kagent resource", "kind", obj.GetKind(), "name", obj.GetName())
			statusErr := r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
				metav1.ConditionFalse, "RenderFailed", err.Error())
			if statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
	}

	runtimeStatus := &airunwayv1alpha1.AgentRuntimeStatus{
		WorkloadRef: &airunwayv1alpha1.RuntimeWorkloadRef{
			APIVersion: kagentAPIVersion,
			Kind:       "Agent",
			Name:       agent.GetName(),
			Namespace:  agent.GetNamespace(),
		},
	}

	// Reflect the kagent Agent's own readiness back into ProviderReady, rather
	// than reporting ready the moment the CR is applied.
	if agentprovider.UpstreamCRReady(ctx, r.Client, kagentAgentGVK, agent.GetName(), agent.GetNamespace()) {
		logger.Info("kagent Agent is ready", "agent", agent.GetName())
		return ctrl.Result{RequeueAfter: 60 * time.Second}, r.applyProviderStatus(ctx, &ad,
			airunwayv1alpha1.AgentPhaseRunning, runtimeStatus, nil,
			metav1.ConditionTrue, "AgentReady", "kagent Agent reports ready")
	}

	logger.Info("Rendered kagent resources; awaiting kagent readiness", "agent", agent.GetName(), "modelConfig", modelConfig.GetName())
	return ctrl.Result{RequeueAfter: 15 * time.Second}, r.applyProviderStatus(ctx, &ad,
		airunwayv1alpha1.AgentPhaseDeploying, runtimeStatus, nil,
		metav1.ConditionFalse, "AwaitingKagent", "kagent Agent and ModelConfig applied; awaiting kagent readiness")
}

// parseKagentConfig extracts the kagent-specific fields from the opaque
// spec.config. A malformed config is reported rather than silently rendering an
// agent with no system prompt.
func parseKagentConfig(raw *runtime.RawExtension) (kagentConfig, error) {
	var cfg kagentConfig
	if raw == nil || len(raw.Raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw.Raw, &cfg); err != nil {
		return kagentConfig{}, fmt.Errorf("parse spec.config for the kagent backend: %w", err)
	}
	return cfg, nil
}

// renderKagentModelConfig builds a kagent ModelConfig from a resolved binding.
// It maps the airunway externalAPI type onto kagent's provider enum and points
// the provider at the resolved base URL (works for OpenAI, Azure OpenAI, and
// any in-cluster OpenAI-compatible endpoint from deploymentRef/gateway).
func renderKagentModelConfig(ad *airunwayv1alpha1.AgentDeployment, binding airunwayv1alpha1.ModelBindingStatus) *unstructured.Unstructured {
	provider, providerBlock := kagentProviderFor(binding)

	spec := map[string]interface{}{
		"provider": provider,
		"model":    binding.ModelName,
	}
	if len(providerBlock) > 0 {
		// e.g. "openAI": {"baseUrl": "..."} or "azureOpenAI": {...}
		for k, v := range providerBlock {
			spec[k] = v
		}
	}
	if binding.CredentialsRef != nil {
		// kagent v1alpha2 renamed this field from v1alpha1's apiKeySecretRef to
		// apiKeySecret; a CEL rule also requires apiKeySecret + apiKeySecretKey
		// to be set together.
		spec["apiKeySecret"] = binding.CredentialsRef.Name
		spec["apiKeySecretKey"] = binding.CredentialsRef.Key
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	obj.SetGroupVersionKind(kagentModelConfigGVK)
	obj.SetName(ad.Name + "-model")
	obj.SetNamespace(ad.Namespace)
	return obj
}

// kagentProviderFor maps a resolved binding to kagent's provider enum value
// and the matching provider-specific config block. Base URL is carried on the
// provider block so any OpenAI-compatible endpoint (including in-cluster
// models) works.
func kagentProviderFor(binding airunwayv1alpha1.ModelBindingStatus) (provider string, block map[string]interface{}) {
	switch binding.APIType {
	case airunwayv1alpha1.ExternalAPITypeAzureOpenAI:
		provider = "AzureOpenAI"
		block = map[string]interface{}{
			"azureOpenAI": map[string]interface{}{
				"azureEndpoint": binding.BaseURL,
				"apiVersion":    "2024-02-01",
			},
		}
	case airunwayv1alpha1.ExternalAPITypeAnthropic:
		provider = "Anthropic"
		block = map[string]interface{}{
			"anthropic": map[string]interface{}{
				"baseUrl": binding.BaseURL,
			},
		}
	default:
		provider = "OpenAI"
		block = map[string]interface{}{}
		if binding.BaseURL != "" {
			block["openAI"] = map[string]interface{}{"baseUrl": binding.BaseURL}
		}
	}
	return provider, block
}

// renderKagentAgent builds a kagent v1alpha2 Agent (type=Declarative) that
// references the given ModelConfig and carries the mapped system prompt.
func renderKagentAgent(ad *airunwayv1alpha1.AgentDeployment, cfg kagentConfig, modelConfigName string) *unstructured.Unstructured {
	description := cfg.Description
	if description == "" {
		description = fmt.Sprintf("airunway agent %s", ad.Name)
	}

	adkRuntime := cfg.Runtime
	if adkRuntime == "" {
		adkRuntime = "python"
	}
	declarative := map[string]interface{}{
		"modelConfig": modelConfigName,
		"runtime":     adkRuntime,
	}
	if cfg.SystemPrompt != "" {
		declarative["systemMessage"] = cfg.SystemPrompt
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"type":        "Declarative",
			"description": description,
			"declarative": declarative,
		},
	}}
	obj.SetGroupVersionKind(kagentAgentGVK)
	obj.SetName(ad.Name)
	obj.SetNamespace(ad.Namespace)
	return obj
}

// applyProviderStatus writes the provider-owned status via the shared SSA
// helper under the kagent field owner.
func (r *KagentProviderReconciler) applyProviderStatus(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	phase airunwayv1alpha1.AgentPhase,
	rt *airunwayv1alpha1.AgentRuntimeStatus,
	replicas *airunwayv1alpha1.AgentReplicaStatus,
	providerReady metav1.ConditionStatus,
	reason, message string,
) error {
	return agentprovider.ApplyOwnedStatus(ctx, r.Client, ad, KagentFieldOwner, phase, rt, replicas, providerReady, reason, message)
}

func (r *KagentProviderReconciler) mapProviderConfigToAgentDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
	apc, ok := obj.(*airunwayv1alpha1.AgentProviderConfig)
	if !ok || apc.Name != KagentFrameworkName {
		return nil
	}
	agents := agentprovider.AgentsForFramework(ctx, r.Client, KagentFrameworkName)
	reqs := make([]reconcile.Request, 0, len(agents))
	for i := range agents {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&agents[i])})
	}
	return reqs
}

// SetupWithManager wires the kagent provider. It watches AgentDeployment and
// framework-provider config changes so existing agents are re-rendered when
// capabilities/defaults change.
func (r *KagentProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := agentprovider.EnsureFrameworkIndex(mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.AgentDeployment{}).
		Watches(
			&airunwayv1alpha1.AgentProviderConfig{},
			handler.EnqueueRequestsFromMapFunc(r.mapProviderConfigToAgentDeployments),
			ctrlbuilder.WithPredicates(agentprovider.ProviderConfigRelevantChange()),
		).
		Named("agent-provider-kagent").
		Complete(r)
}
