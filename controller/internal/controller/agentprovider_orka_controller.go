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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
)

const (
	// OrkaFrameworkName is the framework this provider reconciles.
	OrkaFrameworkName = "orka"

	// OrkaFieldOwner is this provider's server-side apply field manager.
	OrkaFieldOwner = "airunway-agents-orka"

	orkaDefaultMaxConcurrentChildren int32 = 5
	orkaDefaultMaxDepth              int32 = 3

	// orkaAPIVersion is the Orka CRD group/version this provider renders
	// against (github.com/orka-agents/orka).
	orkaAPIVersion = "core.orka.ai/v1alpha1"
)

// orkaProviderGVK / orkaAgentGVK are the Orka CRDs this provider renders. Orka
// models an LLM backend as a Provider CR (type + secretRef + baseURL) and a
// reusable Agent CR that references it — analogous to kagent's ModelConfig +
// Agent split.
var (
	orkaProviderGVK = schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "Provider"}
	orkaAgentGVK    = schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "Agent"}
)

// OrkaProviderReconciler renders an AgentDeployment whose framework is "orka"
// (a crd-backend, Kubernetes-native agent-swarm framework) into Orka-native
// Provider + Agent custom resources, consuming the core-resolved bindings.
type OrkaProviderReconciler struct {
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

// orkaAgentConfig is the Orka-specific spec.config contract.
type orkaAgentConfig struct {
	SystemPrompt string                  `json:"systemPrompt,omitempty"`
	Tools        []orkaToolReference     `json:"tools,omitempty"`
	Coordination *orkaCoordinationConfig `json:"coordination,omitempty"`
}

// orkaToolReference mirrors the supported fields in Orka v0.1.3's
// core.orka.ai/v1alpha1 ToolReference. The enabled pointer preserves omission
// so Orka can apply its default (true).
type orkaToolReference struct {
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// orkaCoordinationConfig mirrors Orka v0.1.3's coordination contract. Optional
// scalars use pointers so the renderer can distinguish explicit values from
// omission and substitute Orka's documented defaults canonically.
type orkaCoordinationConfig struct {
	Enabled               bool               `json:"enabled"`
	AllowedAgents         []orkaAllowedAgent `json:"allowedAgents,omitempty"`
	ApprovalRequiredTools []string           `json:"approvalRequiredTools,omitempty"`
	Autonomous            *bool              `json:"autonomous,omitempty"`
	MaxConcurrentChildren *int32             `json:"maxConcurrentChildren,omitempty"`
	MaxDepth              *int32             `json:"maxDepth,omitempty"`
	MaxIterations         *int32             `json:"maxIterations,omitempty"`
}

type orkaAllowedAgent struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=providers;agents,verbs=get;list;watch;create;update;patch;delete
// Credential resolution is the core controller's job; providers never read a
// credential they did not create. The `get` here is an existence-and-ownership
// check before writing the managed no-auth Secret — without it, an unforced
// server-side apply silently ADOPTS a same-named Secret a user already owns
// (SSA only conflicts on fields another manager owns AND this apply changes;
// adding a key, labels and an ownerReference is all "added"), which then
// garbage-collects their Secret when the AgentDeployment is deleted.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;patch

// Reconcile renders the Orka-native resources for an Orka AgentDeployment.
//
//nolint:gocyclo,dupl // Reconcile mirrors the audited CRD-provider lifecycle while retaining Orka-specific resources.
func (r *OrkaProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	defer func() {
		result, err = agentprovider.ResolveStatusWriteConflict(result, err)
	}()

	var ad airunwayv1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &ad); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ad.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if ad.Spec.Framework.Name != OrkaFrameworkName {
		// A pre-validation object may still change framework while this provider
		// owns its status. Standing aside immediately would leave the old Agent and
		// Provider running and keep the successor blocked on our SSA ownership.
		if ad.Status.ProviderOwner == OrkaFieldOwner {
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

	// Require the crd backend, so a framework registered as a container backend
	// is not rendered by this provider and the generic container provider at
	// the same time (both would force-own the same ProviderReady condition).
	crdBacked, err := agentprovider.FrameworkUsesBackend(ctx, r.Client, OrkaFrameworkName, airunwayv1alpha1.AgentProviderBackendCRD)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !crdBacked {
		// See the equivalent branch in the kagent provider: the framework is
		// gone or now uses a different backend, so anything rendered here is
		// orphaned and must be torn down rather than left running unmanaged.
		// DeleteOwned skips objects that are absent, of an unserved kind, or not
		// controlled by this AgentDeployment, so this is safe for agents this
		// provider never rendered.
		pending, err := agentprovider.CleanupOwnedAndWait(ctx, r.Client, r.objectReader(), &ad,
			agentprovider.UnstructuredRef(orkaProviderGVK, ad.Name+"-provider", ad.Namespace),
			agentprovider.UnstructuredRef(orkaAgentGVK, ad.Name, ad.Namespace),
		)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.status(ctx, &ad,
				airunwayv1alpha1.AgentPhaseDeploying, nil, metav1.ConditionFalse,
				"ProviderHandoffCleanup", "Removing Orka resources before handing the agent to its new provider")
		}
		// See the kagent provider: release the provider-owned status and its SSA
		// ownership so the status is not left stale and a successor provider can
		// take over.
		return ctrl.Result{}, agentprovider.ReleaseOwnedStatus(ctx, r.Client, &ad, OrkaFieldOwner)
	}
	if agentprovider.ProviderHandoffPending(&ad, OrkaFieldOwner) {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	switch agentprovider.ClassifyBinding(&ad) {
	case agentprovider.BindingUnavailable:
		// No binding at all: tear down the rendered Provider/Agent so it stops
		// using stale credentials before reporting Pending.
		pending, err := agentprovider.CleanupOwnedAndWait(ctx, r.Client, r.objectReader(), &ad,
			agentprovider.UnstructuredRef(orkaProviderGVK, ad.Name+"-provider", ad.Namespace),
			agentprovider.UnstructuredRef(orkaAgentGVK, ad.Name, ad.Namespace),
		)
		if err != nil {
			statusErr := r.status(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil,
				metav1.ConditionFalse, "BindingCleanupFailed", err.Error())
			if statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.status(ctx, &ad,
				airunwayv1alpha1.AgentPhaseDeploying, nil, metav1.ConditionFalse,
				"BindingCleanup", "Stopping Orka resources after the model binding was removed")
		}
		return ctrl.Result{}, r.status(ctx, &ad, airunwayv1alpha1.AgentPhasePending, nil,
			metav1.ConditionFalse, "WaitingForBindings", "Waiting for the core controller to resolve model bindings")
	case agentprovider.BindingStale:
		// Binding published but not re-verified this pass — hold rather than
		// deleting a healthy agent over a transient failure.
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	binding := *ad.Status.ModelBinding
	binding, err = agentprovider.EnsureBindingCredentials(ctx, r.Client, r.objectReader(), r.Scheme, &ad, binding, OrkaFieldOwner)
	if err != nil {
		return r.failClosedForCredentialProvision(ctx, &ad, err)
	}
	var cfg orkaAgentConfig
	if ad.Spec.Config != nil && len(ad.Spec.Config.Raw) > 0 {
		if err := json.Unmarshal(ad.Spec.Config.Raw, &cfg); err != nil {
			pending, cleanupErr := agentprovider.CleanupOwnedAndWait(ctx, r.Client, r.objectReader(), &ad,
				agentprovider.UnstructuredRef(orkaProviderGVK, ad.Name+"-provider", ad.Namespace),
				agentprovider.UnstructuredRef(orkaAgentGVK, ad.Name, ad.Namespace),
			)
			if cleanupErr != nil {
				statusErr := r.status(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil,
					metav1.ConditionFalse, "InvalidConfigCleanupFailed", cleanupErr.Error())
				if statusErr != nil {
					return ctrl.Result{}, statusErr
				}
				return ctrl.Result{}, cleanupErr
			}
			if pending {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, r.status(ctx, &ad,
					airunwayv1alpha1.AgentPhaseFailed, nil, metav1.ConditionFalse,
					"InvalidConfigCleanup", "Stopping Orka resources after spec.config became invalid")
			}
			return ctrl.Result{}, r.status(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil,
				metav1.ConditionFalse, "InvalidConfig",
				fmt.Sprintf("parse spec.config for the orka backend: %v", err))
		}
	}

	provider := renderOrkaProvider(&ad, binding)
	agent := renderOrkaAgent(&ad, cfg, binding, provider.GetName())

	// Verify both deterministic names before either write. The Provider embeds the
	// model credential reference, so discovering a foreign Agent after applying
	// the Provider would expose that reference in a resource we cannot complete.
	// Agent-first apply then preserves the same invariant if a foreign object wins
	// a race after preflight: ApplyOwned rejects the race, and a later Provider
	// conflict cleans up the already-applied Agent below.
	objects := []*unstructured.Unstructured{agent, provider}
	var failedObject *unstructured.Unstructured
	cleanupDefinitiveFailure := false
	for _, obj := range objects {
		if err = agentprovider.VerifyOwnedOrAbsent(ctx, r.objectReader(), r.Scheme, &ad, obj); err != nil {
			failedObject = obj
			break
		}
	}
	if err == nil {
		for i, obj := range objects {
			if err = agentprovider.ApplyOwned(ctx, r.Client, r.objectReader(), r.Scheme, &ad, obj, OrkaFieldOwner, true); err != nil {
				failedObject = obj
				cleanupDefinitiveFailure = i > 0
				break
			}
		}
	}
	if err != nil {
		err = r.cleanupAfterRenderedWriteFailure(ctx, &ad, failedObject, err, cleanupDefinitiveFailure)
		statusErr := r.status(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil,
			metav1.ConditionFalse, "OwnershipConflict", err.Error())
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	rt := &airunwayv1alpha1.AgentRuntimeStatus{
		WorkloadRef: &airunwayv1alpha1.RuntimeWorkloadRef{
			APIVersion: orkaAPIVersion, Kind: "Agent", Name: agent.GetName(), Namespace: agent.GetNamespace(),
		},
	}

	ready, err := agentprovider.UpstreamObjectReady(agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ready {
		return ctrl.Result{RequeueAfter: 60 * time.Second}, r.status(ctx, &ad,
			airunwayv1alpha1.AgentPhaseRunning, rt, metav1.ConditionTrue, "AgentReady", "Orka Agent reports ready")
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, r.status(ctx, &ad,
		airunwayv1alpha1.AgentPhaseDeploying, rt, metav1.ConditionFalse, "AwaitingOrka", "Orka Provider and Agent applied; awaiting readiness")
}

// renderOrkaProvider builds an Orka Provider CR from a resolved binding. Orka
// keeps the API key in a Kubernetes Secret referenced by name+key, and takes a
// baseURL override for OpenAI-compatible / proxy endpoints.
func renderOrkaProvider(ad *airunwayv1alpha1.AgentDeployment, binding airunwayv1alpha1.ModelBindingStatus) *unstructured.Unstructured {
	spec := map[string]any{
		"type": orkaProviderType(ad, binding),
	}
	if binding.BaseURL != "" {
		spec["baseURL"] = binding.BaseURL
	}
	if binding.ModelName != "" {
		spec["defaultModel"] = binding.ModelName
	}
	if orkaProviderType(ad, binding) == "azure-openai" {
		// Orka does not use defaultModel as Azure's deployment selector. Its
		// controller requires the provider-specific deploymentName field and marks
		// the Provider unready when it is absent.
		spec["azure"] = map[string]any{
			"deploymentName": binding.ModelName,
		}
	}
	// Orka's Provider CRD requires spec.secretRef (name + key). Reconcile
	// ensures keyless bindings have a managed no-auth Secret; this fallback keeps
	// render output valid when called directly in unit tests.
	secretName, secretKey := agentprovider.KeylessCredentialSecretName(ad.Name), agentprovider.KeylessCredentialKey
	if binding.CredentialsRef != nil {
		secretName, secretKey = binding.CredentialsRef.Name, binding.CredentialsRef.Key
	}
	spec["secretRef"] = map[string]any{
		"name": secretName,
		"key":  secretKey,
	}

	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetGroupVersionKind(orkaProviderGVK)
	obj.SetName(ad.Name + "-provider")
	obj.SetNamespace(ad.Namespace)
	return obj
}

// orkaProviderType maps the airunway external API type onto Orka's provider
// enum (anthropic|openai|azure-openai). Non-external bindings (in-cluster
// models) are OpenAI-compatible.
func orkaProviderType(ad *airunwayv1alpha1.AgentDeployment, binding airunwayv1alpha1.ModelBindingStatus) string {
	if binding.BindingMode == airunwayv1alpha1.ModelBindingModeExternalAPI {
		switch binding.APIType {
		case airunwayv1alpha1.ExternalAPITypeAnthropic:
			return "anthropic"
		case airunwayv1alpha1.ExternalAPITypeAzureOpenAI:
			return "azure-openai"
		}
		// Fallback for older status objects that may not yet carry apiType.
		if ad.Spec.Model.ExternalAPI != nil {
			switch ad.Spec.Model.ExternalAPI.Type {
			case airunwayv1alpha1.ExternalAPITypeAnthropic:
				return "anthropic"
			case airunwayv1alpha1.ExternalAPITypeAzureOpenAI:
				return "azure-openai"
			}
		}
	}
	return "openai"
}

// renderOrkaAgent builds an Orka Agent CR referencing the rendered Provider and
// carrying the mapped system prompt, tools, and coordination configuration.
func renderOrkaAgent(ad *airunwayv1alpha1.AgentDeployment, cfg orkaAgentConfig, binding airunwayv1alpha1.ModelBindingStatus, providerName string) *unstructured.Unstructured {
	spec := map[string]any{
		"providerRef":  map[string]any{"name": providerName},
		"systemPrompt": map[string]any{"inline": cfg.SystemPrompt},
	}
	if binding.ModelName != "" {
		spec["model"] = map[string]any{"name": binding.ModelName}
	}
	tools := make([]any, 0, len(cfg.Tools))
	for _, tool := range cfg.Tools {
		rendered := map[string]any{"name": tool.Name}
		if tool.Enabled != nil {
			rendered["enabled"] = *tool.Enabled
		}
		tools = append(tools, rendered)
	}
	spec["tools"] = tools

	coordination := map[string]any{
		"allowedAgents":         []any{},
		"approvalRequiredTools": []any{},
		"autonomous":            false,
		"enabled":               false,
		"maxConcurrentChildren": int64(orkaDefaultMaxConcurrentChildren),
		"maxDepth":              int64(orkaDefaultMaxDepth),
		"maxIterations":         int64(0),
	}
	if cfg.Coordination != nil {
		coordination["enabled"] = cfg.Coordination.Enabled
		allowedAgents := make([]any, 0, len(cfg.Coordination.AllowedAgents))
		for _, allowed := range cfg.Coordination.AllowedAgents {
			rendered := map[string]any{"name": allowed.Name}
			if allowed.Namespace != "" {
				rendered["namespace"] = allowed.Namespace
			}
			allowedAgents = append(allowedAgents, rendered)
		}
		coordination["allowedAgents"] = allowedAgents
		approvalRequiredTools := make([]any, 0, len(cfg.Coordination.ApprovalRequiredTools))
		for _, toolName := range cfg.Coordination.ApprovalRequiredTools {
			approvalRequiredTools = append(approvalRequiredTools, toolName)
		}
		coordination["approvalRequiredTools"] = approvalRequiredTools
		if cfg.Coordination.Autonomous != nil {
			coordination["autonomous"] = *cfg.Coordination.Autonomous
		}
		if cfg.Coordination.MaxConcurrentChildren != nil {
			coordination["maxConcurrentChildren"] = int64(*cfg.Coordination.MaxConcurrentChildren)
		}
		if cfg.Coordination.MaxDepth != nil {
			coordination["maxDepth"] = int64(*cfg.Coordination.MaxDepth)
		}
		if cfg.Coordination.MaxIterations != nil {
			coordination["maxIterations"] = int64(*cfg.Coordination.MaxIterations)
		}
	}
	spec["coordination"] = coordination

	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetGroupVersionKind(orkaAgentGVK)
	obj.SetName(ad.Name)
	obj.SetNamespace(ad.Namespace)
	return obj
}

// status writes provider-owned status via the shared SSA helper.
func (r *OrkaProviderReconciler) status(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	phase airunwayv1alpha1.AgentPhase,
	rt *airunwayv1alpha1.AgentRuntimeStatus,
	providerReady metav1.ConditionStatus,
	reason, message string,
) error {
	return agentprovider.ApplyOwnedStatus(ctx, r.Client, ad, OrkaFieldOwner, phase, rt, nil, providerReady, reason, message)
}

func (r *OrkaProviderReconciler) mapProviderConfigToAgentDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
	apc, ok := obj.(*airunwayv1alpha1.AgentProviderConfig)
	if !ok || apc.Name != OrkaFrameworkName {
		return nil
	}
	agents := agentprovider.AgentsForFramework(ctx, r.Client, OrkaFrameworkName)
	reqs := make([]reconcile.Request, 0, len(agents))
	for i := range agents {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&agents[i])})
	}
	return reqs
}

// SetupWithManager wires the Orka provider.
func (r *OrkaProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
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
		Named("agent-provider-orka").
		Complete(r)
}

// objectReader returns the uncached reader for ownership and deletion checks,
// falling back to the cached client for directly-constructed tests.
func (r *OrkaProviderReconciler) objectReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

//nolint:dupl // The ordered resource list is provider-specific and deliberately explicit.
func (r *OrkaProviderReconciler) cleanupRenderedResources(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (bool, error) {
	return agentprovider.CleanupOwnedAndWait(ctx, r.Client, r.objectReader(), ad,
		agentprovider.UnstructuredRef(orkaProviderGVK, ad.Name+"-provider", ad.Namespace),
		agentprovider.UnstructuredRef(orkaAgentGVK, ad.Name, ad.Namespace),
	)
}

func (r *OrkaProviderReconciler) cleanupAndReleaseForFrameworkHandoff(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (ctrl.Result, error) {
	pending, err := r.cleanupRenderedResources(ctx, ad)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseDeploying, nil, metav1.ConditionFalse,
			"ProviderHandoffCleanup", "Removing Orka resources after the agent framework changed")
	}
	return ctrl.Result{}, agentprovider.ReleaseOwnedStatus(ctx, r.Client, ad, OrkaFieldOwner)
}

func (r *OrkaProviderReconciler) failClosedForCredentialProvision(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	cause error,
) (ctrl.Result, error) {
	pending, cleanupErr := r.cleanupRenderedResources(ctx, ad)
	if cleanupErr != nil {
		statusErr := r.status(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, nil,
			metav1.ConditionFalse, "CredentialCleanupFailed", cleanupErr.Error())
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, fmt.Errorf("%w; stop Orka resources after credential failure: %v", cause, cleanupErr)
	}
	if pending {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseFailed, nil, metav1.ConditionFalse,
			"CredentialCleanup", "Stopping Orka resources after keyless credential provisioning failed")
	}
	statusErr := r.status(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, nil,
		metav1.ConditionFalse, "CredentialProvisionFailed", cause.Error())
	if statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, cause
}

//nolint:dupl // CRD providers apply the same fail-closed ownership proof to distinct topologies.
func (r *OrkaProviderReconciler) cleanupAfterRenderedWriteFailure(
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
		return fmt.Errorf("%w; clean up incomplete Orka topology after rendered resource write failure: %v", cause, err)
	}
	return cause
}
