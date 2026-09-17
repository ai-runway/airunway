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
	"maps"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	// APIReader is the uncached reader used at ownership and deletion
	// boundaries, including the managed no-auth Secret.
	//
	// It must not be the manager's cached client. A cached typed Get on a Secret
	// starts a cluster-wide Secret informer, whose initial list/watch this
	// provider's RBAC deliberately does not grant — so the first keyless agent
	// would fail to provision its placeholder Secret with a Forbidden. Core
	// carries an APIReader for exactly the same reason.
	APIReader client.Reader
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
	Runtime string          `json:"runtime,omitempty"`
	Tools   []kagentToolRef `json:"tools,omitempty"`
}

// kagentToolRef is the supported subset of kagent's declarative ToolProvider.
// Airunway advertises MCP for the kagent backend, so users must be able to bind
// an existing ToolServer or RemoteMCPServer through AgentDeployment config
// instead of patching the rendered child Agent out of band.
type kagentToolRef struct {
	Type      string                  `json:"type"`
	MCPServer *kagentMCPServerToolRef `json:"mcpServer,omitempty"`
}

type kagentMCPServerToolRef struct {
	APIGroup        string   `json:"apiGroup,omitempty"`
	Kind            string   `json:"kind,omitempty"`
	Name            string   `json:"name"`
	Namespace       string   `json:"namespace,omitempty"`
	ToolNames       []string `json:"toolNames,omitempty"`
	RequireApproval []string `json:"requireApproval,omitempty"`
}

// +kubebuilder:rbac:groups=kagent.dev,resources=agents;modelconfigs,verbs=get;list;watch;create;update;patch;delete
// Credential resolution is the core controller's job; providers never read a
// credential they did not create. The `get` here is an existence-and-ownership
// check before writing the managed no-auth Secret — without it, an unforced
// server-side apply silently ADOPTS a same-named Secret a user already owns
// (SSA only conflicts on fields another manager owns AND this apply changes;
// adding a key, labels and an ownerReference is all "added"), which then
// garbage-collects their Secret when the AgentDeployment is deleted.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;patch

// Reconcile renders the kagent-native resources for a kagent AgentDeployment.
//
//nolint:dupl // CRD providers share lifecycle invariants while rendering different upstream resources.
func (r *KagentProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	defer func() {
		result, err = agentprovider.ResolveStatusWriteConflict(result, err)
	}()

	logger := log.FromContext(ctx)

	var ad airunwayv1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &ad); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ad.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if ad.Spec.Framework.Name != KagentFrameworkName {
		// A pre-validation object may still change framework while this provider
		// owns its status. Standing aside immediately would leave the old Agent and
		// ModelConfig running and keep the successor blocked on our SSA ownership.
		if ad.Status.ProviderOwner == KagentFieldOwner {
			return r.cleanupAndReleaseForFrameworkHandoff(ctx, &ad)
		}
		// A crash can occur after the deterministic children are created but before
		// providerOwner is persisted. Exact controller ownership is sufficient
		// evidence to clean those children without claiming or releasing status.
		pending, err := r.cleanupRenderedResources(ctx, &ad)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
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
		pending, err := agentprovider.CleanupOwnedAndWait(ctx, r.Client, r.objectReader(), &ad,
			agentprovider.UnstructuredRef(kagentModelConfigGVK, ad.Name+"-model", ad.Namespace),
			agentprovider.UnstructuredRef(kagentAgentGVK, ad.Name, ad.Namespace),
		)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.applyProviderStatus(ctx, &ad,
				airunwayv1alpha1.AgentPhaseDeploying, nil, nil, metav1.ConditionFalse,
				"ProviderHandoffCleanup", "Removing kagent resources before handing the agent to its new provider")
		}
		// Release the provider-owned status rather than leaving it stale, and
		// with it the SSA ownership — otherwise this agent keeps reporting
		// Running with a workloadRef to something deleted, and a successor
		// provider deadlocks on a conflict against a manager that will never
		// write again.
		return ctrl.Result{}, agentprovider.ReleaseOwnedStatus(ctx, r.Client, &ad, KagentFieldOwner)
	}
	if agentprovider.ProviderHandoffPending(&ad, KagentFieldOwner) {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Consume the core-resolved binding. Never build a ModelConfig from a
	// half-resolved endpoint.
	switch agentprovider.ClassifyBinding(&ad) {
	case agentprovider.BindingUnavailable:
		// No binding at all: tear down the rendered Agent/ModelConfig so it
		// stops using a stale endpoint before reporting Pending.
		pending, err := agentprovider.CleanupOwnedAndWait(ctx, r.Client, r.objectReader(), &ad,
			agentprovider.UnstructuredRef(kagentModelConfigGVK, ad.Name+"-model", ad.Namespace),
			agentprovider.UnstructuredRef(kagentAgentGVK, ad.Name, ad.Namespace),
		)
		if err != nil {
			statusErr := r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
				metav1.ConditionFalse, "BindingCleanupFailed", err.Error())
			if statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.applyProviderStatus(ctx, &ad,
				airunwayv1alpha1.AgentPhaseDeploying, nil, nil, metav1.ConditionFalse,
				"BindingCleanup", "Stopping kagent resources after the model binding was removed")
		}
		return ctrl.Result{}, r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhasePending, nil, nil,
			metav1.ConditionFalse, "WaitingForBindings", "Waiting for the core controller to resolve model bindings")
	case agentprovider.BindingStale:
		// Binding published but not re-verified this pass — hold rather than
		// deleting a healthy agent over a transient failure.
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	binding := *ad.Status.ModelBinding
	binding, err = agentprovider.EnsureBindingCredentials(ctx, r.Client, r.objectReader(), r.Scheme, &ad, binding, KagentFieldOwner)
	if err != nil {
		return r.failClosedForCredentialProvision(ctx, &ad, err)
	}
	cfg, err := parseKagentConfig(ad.Spec.Config)
	if err != nil {
		pending, cleanupErr := agentprovider.CleanupOwnedAndWait(ctx, r.Client, r.objectReader(), &ad,
			agentprovider.UnstructuredRef(kagentModelConfigGVK, ad.Name+"-model", ad.Namespace),
			agentprovider.UnstructuredRef(kagentAgentGVK, ad.Name, ad.Namespace),
		)
		if cleanupErr != nil {
			statusErr := r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
				metav1.ConditionFalse, "InvalidConfigCleanupFailed", cleanupErr.Error())
			if statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, cleanupErr
		}
		if pending {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.applyProviderStatus(ctx, &ad,
				airunwayv1alpha1.AgentPhaseFailed, nil, nil, metav1.ConditionFalse,
				"InvalidConfigCleanup", "Stopping kagent resources after spec.config became invalid")
		}
		return ctrl.Result{}, r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
			metav1.ConditionFalse, "InvalidConfig", err.Error())
	}

	modelConfig := renderKagentModelConfig(&ad, binding)
	agent := renderKagentAgent(&ad, cfg, modelConfig.GetName())

	// Check the complete rendered topology before creating either object. In
	// particular, do not publish a credential-bearing ModelConfig that a foreign
	// same-named Agent may already reference. ApplyOwned remains the write boundary
	// for each object after this diagnostic preflight.
	rendered := []*unstructured.Unstructured{agent, modelConfig}
	for _, obj := range rendered {
		if err := agentprovider.VerifyOwnedOrAbsent(ctx, r.objectReader(), r.Scheme, &ad, obj); err != nil {
			err = r.cleanupAfterRenderedWriteFailure(ctx, &ad, obj, err, false)
			statusErr := r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
				metav1.ConditionFalse, "OwnershipConflict", err.Error())
			if statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
	}

	// Establish ownership of the non-credential Agent before publishing the
	// ModelConfig. ApplyOwned's create/resourceVersion guards make a concurrent
	// foreign Agent fail before any credential reference is written.
	for i, obj := range rendered {
		if err := agentprovider.ApplyOwned(ctx, r.Client, r.objectReader(), r.Scheme, &ad, obj, KagentFieldOwner, true); err != nil {
			err = r.cleanupAfterRenderedWriteFailure(ctx, &ad, obj, err, i > 0)
			statusErr := r.applyProviderStatus(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
				metav1.ConditionFalse, "OwnershipConflict", err.Error())
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
	ready, err := agentprovider.UpstreamObjectReady(agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ready {
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
	switch cfg.Runtime {
	case "", "python", "go":
	default:
		return kagentConfig{}, fmt.Errorf("spec.config.runtime %q is invalid: must be \"python\" or \"go\"", cfg.Runtime)
	}
	for i, tool := range cfg.Tools {
		if tool.Type != "McpServer" {
			return kagentConfig{}, fmt.Errorf("spec.config.tools[%d].type %q is invalid: only \"McpServer\" is supported", i, tool.Type)
		}
		if tool.MCPServer == nil {
			return kagentConfig{}, fmt.Errorf("spec.config.tools[%d].mcpServer is required", i)
		}
		if tool.MCPServer.Name == "" {
			return kagentConfig{}, fmt.Errorf("spec.config.tools[%d].mcpServer.name is required", i)
		}
	}
	return cfg, nil
}

// renderKagentModelConfig builds a kagent ModelConfig from a resolved binding.
// It maps the airunway externalAPI type onto kagent's provider enum and points
// the provider at the resolved base URL (works for OpenAI, Azure OpenAI, and
// any in-cluster OpenAI-compatible endpoint from deploymentRef/gateway).
func renderKagentModelConfig(ad *airunwayv1alpha1.AgentDeployment, binding airunwayv1alpha1.ModelBindingStatus) *unstructured.Unstructured {
	provider, providerBlock := kagentProviderFor(binding)

	spec := map[string]any{
		"provider": provider,
		"model":    binding.ModelName,
	}
	if len(providerBlock) > 0 {
		// e.g. "openAI": {"baseUrl": "..."} or "azureOpenAI": {...}
		maps.Copy(spec, providerBlock)
	}
	if binding.CredentialsRef != nil {
		// kagent v1alpha2 renamed this field from v1alpha1's apiKeySecretRef to
		// apiKeySecret; a CEL rule also requires apiKeySecret + apiKeySecretKey
		// to be set together.
		spec["apiKeySecret"] = binding.CredentialsRef.Name
		spec["apiKeySecretKey"] = binding.CredentialsRef.Key
	}

	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetGroupVersionKind(kagentModelConfigGVK)
	obj.SetName(ad.Name + "-model")
	obj.SetNamespace(ad.Namespace)
	return obj
}

// kagentProviderFor maps a resolved binding to kagent's provider enum value
// and the matching provider-specific config block. Base URL is carried on the
// provider block so any OpenAI-compatible endpoint (including in-cluster
// models) works.
func kagentProviderFor(binding airunwayv1alpha1.ModelBindingStatus) (provider string, block map[string]any) {
	switch binding.APIType {
	case airunwayv1alpha1.ExternalAPITypeAzureOpenAI:
		provider = "AzureOpenAI"
		block = map[string]any{
			"azureOpenAI": map[string]any{
				"azureEndpoint":   binding.BaseURL,
				"azureDeployment": binding.ModelName,
				"apiVersion":      "2024-02-01",
			},
		}
	case airunwayv1alpha1.ExternalAPITypeAnthropic:
		provider = "Anthropic"
		block = map[string]any{
			"anthropic": map[string]any{
				"baseUrl": binding.BaseURL,
			},
		}
	default:
		provider = "OpenAI"
		block = map[string]any{}
		if binding.BaseURL != "" {
			block["openAI"] = map[string]any{"baseUrl": binding.BaseURL}
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
	declarative := map[string]any{
		"modelConfig": modelConfigName,
		"runtime":     adkRuntime,
	}
	if cfg.SystemPrompt != "" {
		declarative["systemMessage"] = cfg.SystemPrompt
	}
	if len(cfg.Tools) > 0 {
		declarative["tools"] = renderKagentTools(cfg.Tools)
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
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

func renderKagentTools(tools []kagentToolRef) []any {
	rendered := make([]any, 0, len(tools))
	for _, tool := range tools {
		mcpServer := map[string]any{
			"name": tool.MCPServer.Name,
		}
		if tool.MCPServer.APIGroup != "" {
			mcpServer["apiGroup"] = tool.MCPServer.APIGroup
		}
		if tool.MCPServer.Kind != "" {
			mcpServer["kind"] = tool.MCPServer.Kind
		}
		if tool.MCPServer.Namespace != "" {
			mcpServer["namespace"] = tool.MCPServer.Namespace
		}
		if len(tool.MCPServer.ToolNames) > 0 {
			mcpServer["toolNames"] = stringSliceToInterfaces(tool.MCPServer.ToolNames)
		}
		if len(tool.MCPServer.RequireApproval) > 0 {
			mcpServer["requireApproval"] = stringSliceToInterfaces(tool.MCPServer.RequireApproval)
		}
		rendered = append(rendered, map[string]any{
			"type":      tool.Type,
			"mcpServer": mcpServer,
		})
	}
	return rendered
}

func stringSliceToInterfaces(values []string) []any {
	rendered := make([]any, len(values))
	for i, value := range values {
		rendered[i] = value
	}
	return rendered
}

// applyProviderStatus writes the provider-owned status via the shared SSA
// helper under the kagent field owner.
//
//nolint:dupl,unparam // The shared provider-status shape keeps call sites consistent; kagent has no replica status.
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
		For(&airunwayv1alpha1.AgentDeployment{},
			ctrlbuilder.WithPredicates(agentprovider.ProviderAgentDeploymentRelevantChange())).
		Watches(
			&airunwayv1alpha1.AgentProviderConfig{},
			handler.EnqueueRequestsFromMapFunc(r.mapProviderConfigToAgentDeployments),
			ctrlbuilder.WithPredicates(agentprovider.ProviderConfigRelevantChange()),
		).
		Named("agent-provider-kagent").
		Complete(r)
}

// objectReader returns the uncached reader for ownership and deletion checks,
// falling back to the cached client for directly-constructed tests.
func (r *KagentProviderReconciler) objectReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

//nolint:dupl // The ordered resource list is provider-specific and deliberately explicit.
func (r *KagentProviderReconciler) cleanupRenderedResources(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (bool, error) {
	return agentprovider.CleanupOwnedAndWait(ctx, r.Client, r.objectReader(), ad,
		agentprovider.UnstructuredRef(kagentModelConfigGVK, ad.Name+"-model", ad.Namespace),
		agentprovider.UnstructuredRef(kagentAgentGVK, ad.Name, ad.Namespace),
	)
}

func (r *KagentProviderReconciler) cleanupAndReleaseForFrameworkHandoff(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (ctrl.Result, error) {
	pending, err := r.cleanupRenderedResources(ctx, ad)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.applyProviderStatus(ctx, ad,
			airunwayv1alpha1.AgentPhaseDeploying, nil, nil, metav1.ConditionFalse,
			"ProviderHandoffCleanup", "Removing kagent resources after the agent framework changed")
	}
	return ctrl.Result{}, agentprovider.ReleaseOwnedStatus(ctx, r.Client, ad, KagentFieldOwner)
}

func (r *KagentProviderReconciler) failClosedForCredentialProvision(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	cause error,
) (ctrl.Result, error) {
	pending, cleanupErr := r.cleanupRenderedResources(ctx, ad)
	if cleanupErr != nil {
		statusErr := r.applyProviderStatus(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
			metav1.ConditionFalse, "CredentialCleanupFailed", cleanupErr.Error())
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, fmt.Errorf("%w; stop kagent resources after credential failure: %v", cause, cleanupErr)
	}
	if pending {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.applyProviderStatus(ctx, ad,
			airunwayv1alpha1.AgentPhaseFailed, nil, nil, metav1.ConditionFalse,
			"CredentialCleanup", "Stopping kagent resources after keyless credential provisioning failed")
	}
	statusErr := r.applyProviderStatus(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
		metav1.ConditionFalse, "CredentialProvisionFailed", cause.Error())
	if statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, cause
}

//nolint:dupl // CRD providers apply the same fail-closed ownership proof to distinct topologies.
func (r *KagentProviderReconciler) cleanupAfterRenderedWriteFailure(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	failed *unstructured.Unstructured,
	cause error,
	cleanupDefinitiveFailure bool,
) error {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(failed.GroupVersionKind())
	readErr := r.objectReader().Get(ctx, client.ObjectKeyFromObject(failed), live)
	foreign := readErr == nil && !agentprovider.IsControlledByAgentDeployment(live, ad)
	definitivelyIncomplete := cleanupDefinitiveFailure && definitiveResourceWriteFailure(cause)
	if !foreign && !definitivelyIncomplete {
		return cause
	}
	if _, err := r.cleanupRenderedResources(ctx, ad); err != nil {
		return fmt.Errorf("%w; clean up incomplete kagent topology after rendered resource write failure: %v", cause, err)
	}
	return cause
}
