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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
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
// custom resources, consuming the core-resolved status.modelBindings. It owns
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

	// Consume the core-resolved bindings. Do not render until core reports the
	// bindings are resolved, so we never build a ModelConfig from a half-
	// resolved endpoint.
	if !meta.IsStatusConditionTrue(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeModelBound) ||
		len(ad.Status.ModelBindings) == 0 {
		return ctrl.Result{}, r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhasePending, nil, nil,
			metav1.ConditionFalse, "WaitingForBindings", "Waiting for the core controller to resolve model bindings")
	}

	binding := ad.Status.ModelBindings[0]
	cfg := parseKagentConfig(ad.Spec.Config)

	modelConfig := renderKagentModelConfig(&ad, binding)
	agent := renderKagentAgent(&ad, cfg, modelConfig.GetName())

	for _, obj := range []*unstructured.Unstructured{modelConfig, agent} {
		if err := controllerutil.SetControllerReference(&ad, obj, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set owner reference on %s: %w", obj.GetKind(), err)
		}
		if err := r.Patch(ctx, obj, client.Apply, client.FieldOwner(KagentFieldOwner), client.ForceOwnership); err != nil {
			logger.Error(err, "Failed to apply kagent resource", "kind", obj.GetKind(), "name", obj.GetName())
			return ctrl.Result{}, r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
				metav1.ConditionFalse, "RenderFailed", err.Error())
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
	if upstreamCRReady(ctx, r.Client, kagentAgentGVK, agent.GetName(), agent.GetNamespace()) {
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
// spec.config. A nil or unparseable config yields an empty config (the agent
// still renders, just without a system prompt).
func parseKagentConfig(raw *runtime.RawExtension) kagentConfig {
	var cfg kagentConfig
	if raw == nil || len(raw.Raw) == 0 {
		return cfg
	}
	_ = json.Unmarshal(raw.Raw, &cfg)
	return cfg
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
	// Default: treat everything as an OpenAI-compatible endpoint driven by the
	// resolved base URL. externalAPI type only refines this.
	provider = "OpenAI"
	block = map[string]interface{}{}
	if binding.BaseURL != "" {
		block["openAI"] = map[string]interface{}{"baseUrl": binding.BaseURL}
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

	runtime := cfg.Runtime
	if runtime == "" {
		runtime = "python"
	}
	declarative := map[string]interface{}{
		"modelConfig": modelConfigName,
		"runtime":     runtime,
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
	return applyProviderOwnedStatus(ctx, r.Client, ad, KagentFieldOwner, phase, rt, replicas, providerReady, reason, message)
}

// SetupWithManager wires the kagent provider. It watches AgentDeployment and
// re-reconciles when core updates status (e.g. bindings become resolved).
func (r *KagentProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.AgentDeployment{}).
		Named("agent-provider-kagent").
		Complete(r)
}
