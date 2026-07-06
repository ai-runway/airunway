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
	// OrkaFrameworkName is the framework this provider reconciles.
	OrkaFrameworkName = "orka"

	// OrkaFieldOwner is this provider's server-side apply field manager.
	OrkaFieldOwner = "airunway-agents-orka"

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
}

// orkaAgentConfig is the Orka-specific spec.config contract.
type orkaAgentConfig struct {
	SystemPrompt string `json:"systemPrompt,omitempty"`
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=providers;agents,verbs=get;list;watch;create;update;patch;delete

// Reconcile renders the Orka-native resources for an Orka AgentDeployment.
func (r *OrkaProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ad airunwayv1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &ad); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if ad.Spec.Framework.Name != OrkaFrameworkName || !ad.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if !meta.IsStatusConditionTrue(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeModelBound) ||
		len(ad.Status.ModelBindings) == 0 {
		return ctrl.Result{}, r.status(ctx, &ad, airunwayv1alpha1.AgentPhasePending, nil,
			metav1.ConditionFalse, "WaitingForBindings", "Waiting for the core controller to resolve model bindings")
	}

	binding := ad.Status.ModelBindings[0]
	var cfg orkaAgentConfig
	if ad.Spec.Config != nil && len(ad.Spec.Config.Raw) > 0 {
		_ = json.Unmarshal(ad.Spec.Config.Raw, &cfg)
	}

	provider := renderOrkaProvider(&ad, binding)
	agent := renderOrkaAgent(&ad, cfg, binding, provider.GetName())

	for _, obj := range []*unstructured.Unstructured{provider, agent} {
		if err := controllerutil.SetControllerReference(&ad, obj, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set owner reference on %s: %w", obj.GetKind(), err)
		}
		if err := r.Patch(ctx, obj, client.Apply, client.FieldOwner(OrkaFieldOwner), client.ForceOwnership); err != nil {
			logger.Error(err, "Failed to apply Orka resource", "kind", obj.GetKind(), "name", obj.GetName())
			return ctrl.Result{}, r.status(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil,
				metav1.ConditionFalse, "RenderFailed", err.Error())
		}
	}

	rt := &airunwayv1alpha1.AgentRuntimeStatus{
		WorkloadRef: &airunwayv1alpha1.RuntimeWorkloadRef{
			APIVersion: orkaAPIVersion, Kind: "Agent", Name: agent.GetName(), Namespace: agent.GetNamespace(),
		},
	}

	if upstreamCRReady(ctx, r.Client, orkaAgentGVK, agent.GetName(), agent.GetNamespace()) {
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
	spec := map[string]interface{}{
		"type": orkaProviderType(ad, binding),
	}
	if binding.BaseURL != "" {
		spec["baseURL"] = binding.BaseURL
	}
	if binding.ModelName != "" {
		spec["defaultModel"] = binding.ModelName
	}
	if binding.CredentialsRef != nil {
		spec["secretRef"] = map[string]interface{}{
			"name": binding.CredentialsRef.Name,
			"key":  binding.CredentialsRef.Key,
		}
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
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
		for i := range ad.Spec.Models {
			m := &ad.Spec.Models[i]
			if m.Name == binding.Name && m.ExternalAPI != nil {
				switch m.ExternalAPI.Type {
				case airunwayv1alpha1.ExternalAPITypeAnthropic:
					return "anthropic"
				case airunwayv1alpha1.ExternalAPITypeAzureOpenAI:
					return "azure-openai"
				}
			}
		}
	}
	return "openai"
}

// renderOrkaAgent builds an Orka Agent CR referencing the rendered Provider and
// carrying the mapped system prompt (Orka's systemPrompt.inline).
func renderOrkaAgent(ad *airunwayv1alpha1.AgentDeployment, cfg orkaAgentConfig, binding airunwayv1alpha1.ModelBindingStatus, providerName string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"providerRef": map[string]interface{}{"name": providerName},
	}
	if binding.ModelName != "" {
		spec["model"] = map[string]interface{}{"name": binding.ModelName}
	}
	if cfg.SystemPrompt != "" {
		spec["systemPrompt"] = map[string]interface{}{"inline": cfg.SystemPrompt}
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
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
	return applyProviderOwnedStatus(ctx, r.Client, ad, OrkaFieldOwner, phase, rt, nil, providerReady, reason, message)
}

// SetupWithManager wires the Orka provider.
func (r *OrkaProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.AgentDeployment{}).
		Named("agent-provider-orka").
		Complete(r)
}
