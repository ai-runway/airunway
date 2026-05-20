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
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"github.com/kaito-project/airunway/controller/internal/gateway"
)

// ModelDeploymentReconciler reconciles a ModelDeployment object
type ModelDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// EnableProviderSelector controls whether the controller runs provider selection
	EnableProviderSelector bool

	// GatewayDetector checks for Gateway API CRD availability and resolves gateway config
	GatewayDetector *gateway.Detector

	// ProviderResolver looks up gateway capabilities from InferenceProviderConfig CRs.
	// When nil, the reconciler treats all providers as having no gateway capabilities.
	ProviderResolver gateway.ProviderCapabilityResolver
}

// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=airunway.ai,resources=inferenceproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=inference.networking.k8s.io,resources=inferencepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=create;get;update
// +kubebuilder:rbac:groups=inference.networking.x-k8s.io,resources=inferenceobjectives;inferencemodelrewrites,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.istio.io,resources=destinationrules,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles the reconciliation loop for ModelDeployment resources.
//
// The core controller is intentionally minimal - it does NOT create provider resources.
// Instead, it:
// 1. Validates the ModelDeployment spec
// 2. Runs provider selection (if enabled and spec.provider.name is empty)
// 3. Updates status conditions
//
// Provider controllers (out-of-tree) watch for ModelDeployments where status.provider.name
// matches their name and handle the actual resource creation.
func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the ModelDeployment
	var md airunwayv1alpha1.ModelDeployment
	if err := r.Get(ctx, req.NamespacedName, &md); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// MD was deleted — check if the namespace should be removed from
			// the Gateway's allowedRoutes.
			r.cleanupGatewayAllowedRoutesForNamespace(ctx, req.Namespace)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Save a deep copy as the patch base so we only send changed status fields.
	// This avoids clobbering status fields set by out-of-tree provider controllers.
	base := md.DeepCopy()

	logger.Info("Reconciling ModelDeployment", "name", md.Name, "namespace", md.Namespace)

	// If the ModelDeployment is being deleted, clean up gateway resources and return.
	// This catches foreground deletion or any other finalizer holding the MD open.
	if !md.DeletionTimestamp.IsZero() {
		if err := r.cleanupGatewayResources(ctx, &md); err != nil {
			logger.Error(err, "Failed to clean up gateway resources on deletion")
		}
		return ctrl.Result{}, nil
	}

	// Check for pause annotation
	if md.Annotations != nil && md.Annotations["airunway.ai/reconcile-paused"] == "true" {
		logger.Info("Reconciliation paused", "name", md.Name)
		return ctrl.Result{}, nil
	}

	// Update observed generation
	if md.Status.ObservedGeneration != md.Generation {
		md.Status.ObservedGeneration = md.Generation
	}

	// Initialize status if needed
	if md.Status.Phase == "" {
		md.Status.Phase = airunwayv1alpha1.DeploymentPhasePending
	}

	// Step 1: Select engine if needed (before validation, since validation needs engine type)
	if r.EnableProviderSelector {
		if err := r.selectEngine(ctx, &md); err != nil {
			logger.Error(err, "Engine selection failed", "name", md.Name)
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeEngineSelected, metav1.ConditionFalse, "SelectionFailed", err.Error())
			md.Status.Message = fmt.Sprintf("Engine selection failed: %s", err.Error())
			return ctrl.Result{}, r.Status().Patch(ctx, &md, client.MergeFrom(base))
		}
	}

	// Step 2: Inject resolved engine into in-memory spec for CEL evaluation.
	// This is NOT persisted — only status is patched. It ensures provider selection
	// CEL rules (e.g., "spec.engine.type == 'vllm'") see the resolved engine type.
	if md.Spec.Engine.Type == "" && md.Status.Engine != nil {
		md.Spec.Engine.Type = md.Status.Engine.Type
	}

	// Step 4: Validate the spec (uses resolved engine type)
	if err := r.validateSpec(ctx, &md); err != nil {
		logger.Error(err, "Validation failed", "name", md.Name)
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeValidated, metav1.ConditionFalse, "ValidationFailed", err.Error())
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = fmt.Sprintf("Validation failed: %s", err.Error())
		return ctrl.Result{}, r.Status().Patch(ctx, &md, client.MergeFrom(base))
	}
	r.setCondition(&md, airunwayv1alpha1.ConditionTypeValidated, metav1.ConditionTrue, "ValidationPassed", "Schema validation passed")

	// Step 5: Run provider selection if needed
	if r.EnableProviderSelector {
		if err := r.selectProvider(ctx, &md); err != nil {
			logger.Error(err, "Provider selection failed", "name", md.Name)
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderSelected, metav1.ConditionFalse, "SelectionFailed", err.Error())
			md.Status.Message = fmt.Sprintf("Provider selection failed: %s", err.Error())
			return ctrl.Result{}, r.Status().Patch(ctx, &md, client.MergeFrom(base))
		}
	}

	// Step 6: Update status
	// If no provider is selected yet, stay in Pending
	if md.Status.Provider == nil || md.Status.Provider.Name == "" {
		if md.Spec.Provider != nil && md.Spec.Provider.Name != "" {
			// User explicitly specified a provider
			md.Status.Provider = &airunwayv1alpha1.ProviderStatus{
				Name:           md.Spec.Provider.Name,
				SelectedReason: "explicit provider selection",
			}
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderSelected, metav1.ConditionTrue, "ExplicitSelection", "Provider explicitly specified in spec")
		} else if !r.EnableProviderSelector {
			// No provider specified and selector disabled
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderSelected, metav1.ConditionFalse, "NoProvider", "No provider specified and provider-selector not enabled")
			md.Status.Message = "No provider specified and provider-selector not enabled"
		}
	}

	// The core controller does NOT create provider resources.
	// Provider controllers watch for ModelDeployments where status.provider.name matches
	// their name and handle the actual resource creation.
	//
	// The core controller's job is done after validation and provider selection.
	// Provider controllers will update:
	// - status.phase (Deploying, Running, Failed)
	// - status.provider.resourceName
	// - status.provider.resourceKind
	// - status.replicas
	// - status.endpoint
	// - ProviderCompatible, ResourceCreated, Ready conditions

	// Step 7: Reconcile gateway resources (InferencePool + HTTPRoute) when deployment is running
	if md.Status.Phase == airunwayv1alpha1.DeploymentPhaseRunning {
		if md.Spec.Gateway != nil && md.Spec.Gateway.Enabled != nil && !*md.Spec.Gateway.Enabled {
			// Gateway explicitly disabled — clean up any existing resources
			if err := r.cleanupGatewayResources(ctx, &md); err != nil {
				logger.Error(err, "Failed to clean up gateway resources")
			}
		} else {
			if err := r.reconcileGateway(ctx, &md); err != nil {
				logger.Error(err, "Gateway reconciliation failed", "name", md.Name)
				// If the error suggests CRDs were removed, refresh the detection cache
				if isNoMatchError(err) && r.GatewayDetector != nil {
					logger.Info("Gateway CRDs may have been removed, refreshing detection cache")
					r.GatewayDetector.Refresh()
				} else if apierrors.IsNotFound(err) {
					// Return an error to trigger exponential backoff retries.
					return ctrl.Result{}, err
				}
				// Non-fatal: don't block overall reconciliation
			}
		}
	}
	// Kubernetes garbage collection will handle cleanup when the ModelDeployment is deleted.

	logger.Info("Reconciliation complete", "name", md.Name, "phase", md.Status.Phase, "provider", md.Status.Provider)

	return ctrl.Result{}, r.Status().Patch(ctx, &md, client.MergeFrom(base))
}

// isNoMatchError checks if an error indicates that a CRD/resource type is not registered.
func isNoMatchError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "no matches for kind") ||
		strings.Contains(errStr, "the server could not find the requested resource") ||
		strings.Contains(errStr, "no kind is registered for the type")
}

// validateSpec performs validation on the ModelDeployment spec
func (r *ModelDeploymentReconciler) validateSpec(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) error {
	spec := &md.Spec

	// Validate model.id is required for huggingface source
	if spec.Model.Source == airunwayv1alpha1.ModelSourceHuggingFace || spec.Model.Source == "" {
		if spec.Model.ID == "" {
			return fmt.Errorf("model.id is required when source is huggingface")
		}
	}

	// Resolve engine type (from spec or auto-selected in status)
	engineType := md.ResolvedEngineType()
	if engineType == "" {
		return fmt.Errorf("engine.type must be specified or auto-selected from provider capabilities")
	}

	// Validate GPU requirements for certain engines
	gpuCount := int32(0)
	if spec.Resources != nil && spec.Resources.GPU != nil {
		gpuCount = spec.Resources.GPU.Count
	}

	switch engineType {
	case airunwayv1alpha1.EngineTypeVLLM, airunwayv1alpha1.EngineTypeSGLang, airunwayv1alpha1.EngineTypeTRTLLM:
		// These engines require GPU (unless in disaggregated mode with component-level GPUs)
		servingMode := airunwayv1alpha1.ServingModeAggregated
		if spec.Serving != nil && spec.Serving.Mode != "" {
			servingMode = spec.Serving.Mode
		}

		if servingMode == airunwayv1alpha1.ServingModeAggregated && gpuCount == 0 {
			return fmt.Errorf("%s engine requires GPU (set resources.gpu.count > 0)", engineType)
		}
	}

	// Validate disaggregated mode configuration
	if spec.Serving != nil && spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		// Cannot specify resources.gpu in disaggregated mode
		if spec.Resources != nil && spec.Resources.GPU != nil && spec.Resources.GPU.Count > 0 {
			return fmt.Errorf("cannot specify both resources.gpu and scaling.prefill/decode in disaggregated mode")
		}

		// Must specify prefill and decode
		if spec.Scaling == nil || spec.Scaling.Prefill == nil || spec.Scaling.Decode == nil {
			return fmt.Errorf("disaggregated mode requires scaling.prefill and scaling.decode")
		}

		// Prefill must have GPU
		if spec.Scaling.Prefill.GPU == nil || spec.Scaling.Prefill.GPU.Count == 0 {
			return fmt.Errorf("disaggregated mode requires scaling.prefill.gpu.count > 0")
		}

		// Decode must have GPU
		if spec.Scaling.Decode.GPU == nil || spec.Scaling.Decode.GPU.Count == 0 {
			return fmt.Errorf("disaggregated mode requires scaling.decode.gpu.count > 0")
		}
	}

	return nil
}

// selectEngine auto-selects the engine type from provider capabilities if not specified
func (r *ModelDeploymentReconciler) selectEngine(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	// If engine type is explicitly specified, just record it in status
	if md.Spec.Engine.Type != "" {
		md.Status.Engine = &airunwayv1alpha1.EngineStatus{
			Type:           md.Spec.Engine.Type,
			SelectedReason: "explicit engine selection",
		}
		r.setCondition(md, airunwayv1alpha1.ConditionTypeEngineSelected, metav1.ConditionTrue, "ExplicitSelection", "Engine explicitly specified in spec")
		return nil
	}

	// Skip if engine already auto-selected
	if md.Status.Engine != nil && md.Status.Engine.Type != "" {
		return nil
	}

	// List all InferenceProviderConfigs
	var providerConfigs airunwayv1alpha1.InferenceProviderConfigList
	if err := r.List(ctx, &providerConfigs); err != nil {
		return fmt.Errorf("failed to list provider configs: %w", err)
	}

	if len(providerConfigs.Items) == 0 {
		return fmt.Errorf("no providers registered (InferenceProviderConfig resources not found)")
	}

	// Collect supported engines from ready providers, filtering by compatibility
	// GPU-requiring engines cannot run on CPU-only deployments
	gpuRequiringEngines := map[airunwayv1alpha1.EngineType]bool{
		airunwayv1alpha1.EngineTypeVLLM:   true,
		airunwayv1alpha1.EngineTypeSGLang: true,
		airunwayv1alpha1.EngineTypeTRTLLM: true,
	}

	// Determine deployment characteristics
	hasGPU := false
	if md.Spec.Resources != nil && md.Spec.Resources.GPU != nil && md.Spec.Resources.GPU.Count > 0 {
		hasGPU = true
	}
	if md.Spec.Serving != nil && md.Spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		hasGPU = true
	}

	servingMode := airunwayv1alpha1.ServingModeAggregated
	if md.Spec.Serving != nil && md.Spec.Serving.Mode != "" {
		servingMode = md.Spec.Serving.Mode
	}

	availableEngines := make(map[airunwayv1alpha1.EngineType]string) // engine -> provider name

	for _, pc := range providerConfigs.Items {
		if !pc.Status.Ready || pc.Spec.Capabilities == nil {
			continue
		}

		caps := pc.Spec.Capabilities

		// Filter by GPU/CPU compatibility
		if hasGPU && !caps.GPUSupport {
			continue
		}
		if !hasGPU && !caps.CPUSupport {
			continue
		}

		// Filter by serving mode compatibility
		servingModeSupported := false
		for _, sm := range caps.ServingModes {
			if sm == servingMode {
				servingModeSupported = true
				break
			}
		}
		if !servingModeSupported {
			continue
		}

		for _, engine := range caps.Engines {
			// Skip GPU-requiring engines for CPU-only deployments
			if !hasGPU && gpuRequiringEngines[engine] {
				continue
			}
			if _, exists := availableEngines[engine]; !exists {
				availableEngines[engine] = pc.Name
			}
		}
	}

	if len(availableEngines) == 0 {
		return fmt.Errorf("no engines available from registered providers")
	}

	// Select the highest-preference engine that is available
	enginePreference := []airunwayv1alpha1.EngineType{
		airunwayv1alpha1.EngineTypeVLLM,
		airunwayv1alpha1.EngineTypeSGLang,
		airunwayv1alpha1.EngineTypeTRTLLM,
		airunwayv1alpha1.EngineTypeLlamaCpp,
	}
	for _, engine := range enginePreference {
		if providerName, ok := availableEngines[engine]; ok {
			logger.Info("Engine auto-selected", "engine", engine, "fromProvider", providerName)
			md.Status.Engine = &airunwayv1alpha1.EngineStatus{
				Type:           engine,
				SelectedReason: fmt.Sprintf("auto-selected from provider %s capabilities", providerName),
			}
			r.setCondition(md, airunwayv1alpha1.ConditionTypeEngineSelected, metav1.ConditionTrue, "AutoSelected", fmt.Sprintf("Engine %s auto-selected from provider %s", engine, providerName))
			return nil
		}
	}

	// Fallback: pick any available engine (shouldn't happen if preference list is complete)
	for engine, providerName := range availableEngines {
		md.Status.Engine = &airunwayv1alpha1.EngineStatus{
			Type:           engine,
			SelectedReason: fmt.Sprintf("auto-selected from provider %s capabilities", providerName),
		}
		r.setCondition(md, airunwayv1alpha1.ConditionTypeEngineSelected, metav1.ConditionTrue, "AutoSelected", fmt.Sprintf("Engine %s auto-selected from provider %s", engine, providerName))
		return nil
	}

	return fmt.Errorf("no compatible engine found from available providers")
}

// selectProvider runs the provider selection algorithm
func (r *ModelDeploymentReconciler) selectProvider(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	// Skip if provider is already selected (either in spec or status)
	if md.Spec.Provider != nil && md.Spec.Provider.Name != "" {
		return nil // User explicitly specified provider
	}
	if md.Status.Provider != nil && md.Status.Provider.Name != "" {
		return nil // Provider already selected
	}

	// List all InferenceProviderConfigs
	var providerConfigs airunwayv1alpha1.InferenceProviderConfigList
	if err := r.List(ctx, &providerConfigs); err != nil {
		return fmt.Errorf("failed to list provider configs: %w", err)
	}

	if len(providerConfigs.Items) == 0 {
		return fmt.Errorf("no providers registered (InferenceProviderConfig resources not found)")
	}

	// Filter to ready providers
	var readyProviders []airunwayv1alpha1.InferenceProviderConfig
	for _, pc := range providerConfigs.Items {
		if pc.Status.Ready {
			readyProviders = append(readyProviders, pc)
		}
	}

	if len(readyProviders) == 0 {
		return fmt.Errorf("no healthy providers available")
	}

	// Run selection algorithm
	selectedProvider, reason, err := r.runSelectionAlgorithm(md, readyProviders)
	if err != nil {
		return fmt.Errorf("provider selection failed: %w", err)
	}
	if selectedProvider == "" {
		return fmt.Errorf("no compatible provider found for this configuration")
	}

	logger.Info("Provider selected", "provider", selectedProvider, "reason", reason)

	md.Status.Provider = &airunwayv1alpha1.ProviderStatus{
		Name:           selectedProvider,
		SelectedReason: reason,
	}
	r.setCondition(md, airunwayv1alpha1.ConditionTypeProviderSelected, metav1.ConditionTrue, "AutoSelected", fmt.Sprintf("Provider %s auto-selected", selectedProvider))

	return nil
}

// runSelectionAlgorithm implements the provider selection algorithm
func (r *ModelDeploymentReconciler) runSelectionAlgorithm(md *airunwayv1alpha1.ModelDeployment, providers []airunwayv1alpha1.InferenceProviderConfig) (string, string, error) {
	spec := &md.Spec
	engineType := md.ResolvedEngineType()

	// Determine GPU requirements
	hasGPU := false
	if spec.Resources != nil && spec.Resources.GPU != nil && spec.Resources.GPU.Count > 0 {
		hasGPU = true
	}
	if spec.Serving != nil && spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		hasGPU = true
	}

	// Convert spec to map for CEL evaluation
	specMap, err := specToMap(spec)
	if err != nil {
		return "", "", fmt.Errorf("failed to convert spec for CEL evaluation: %w", err)
	}

	// Build candidate list with scores
	type candidate struct {
		name     string
		reason   string
		priority int32
	}
	var candidates []candidate

	for _, pc := range providers {
		caps := pc.Spec.Capabilities
		if caps == nil {
			continue
		}

		// Check engine support
		engineSupported := false
		for _, e := range caps.Engines {
			if e == engineType {
				engineSupported = true
				break
			}
		}
		if !engineSupported {
			continue
		}

		// Check GPU/CPU support
		if hasGPU && !caps.GPUSupport {
			continue
		}
		if !hasGPU && !caps.CPUSupport {
			continue
		}

		// Check serving mode support
		servingMode := airunwayv1alpha1.ServingModeAggregated
		if spec.Serving != nil && spec.Serving.Mode != "" {
			servingMode = spec.Serving.Mode
		}
		servingModeSupported := false
		for _, sm := range caps.ServingModes {
			if sm == servingMode {
				servingModeSupported = true
				break
			}
		}
		if !servingModeSupported {
			continue
		}

		// This provider is compatible
		// Evaluate CEL selection rules to calculate priority
		priority := int32(0)
		for _, rule := range pc.Spec.SelectionRules {
			matched, err := evaluateCEL(rule.Condition, specMap)
			if err != nil {
				continue // skip rules that fail to evaluate
			}
			if matched && rule.Priority > priority {
				priority = rule.Priority
			}
		}

		reason := fmt.Sprintf("matched capabilities: engine=%s, gpu=%v, mode=%s", engineType, hasGPU, servingMode)
		candidates = append(candidates, candidate{
			name:     pc.Name,
			reason:   reason,
			priority: priority,
		})
	}

	if len(candidates) == 0 {
		return "", "", nil
	}

	// Select highest priority candidate; use name as stable tiebreaker
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.priority > best.priority || (c.priority == best.priority && c.name < best.name) {
			best = c
		}
	}

	return best.name, best.reason, nil
}

// setCondition updates a condition on the ModelDeployment
func (r *ModelDeploymentReconciler) setCondition(md *airunwayv1alpha1.ModelDeployment, conditionType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: md.Generation,
	}
	meta.SetStatusCondition(&md.Status.Conditions, condition)
}

func providerConfigChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldConfig, okOld := e.ObjectOld.(*airunwayv1alpha1.InferenceProviderConfig)
			newConfig, okNew := e.ObjectNew.(*airunwayv1alpha1.InferenceProviderConfig)
			if !okOld || !okNew {
				return false
			}
			return oldConfig.Status.Ready != newConfig.Status.Ready ||
				!apiequality.Semantic.DeepEqual(oldConfig.Spec, newConfig.Spec)
		},
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	}
}

func modelDeploymentNeedsProviderSelection(md *airunwayv1alpha1.ModelDeployment) bool {
	if md.Spec.Provider != nil && md.Spec.Provider.Name != "" {
		return false
	}
	return md.Status.Provider == nil || md.Status.Provider.Name == ""
}

func providerConfigAffectsModelDeployment(md *airunwayv1alpha1.ModelDeployment, providerName string) bool {
	if md.Spec.Provider != nil && md.Spec.Provider.Name == providerName {
		return true
	}
	if md.Status.Provider != nil && md.Status.Provider.Name == providerName {
		return true
	}
	return modelDeploymentNeedsProviderSelection(md)
}

func (r *ModelDeploymentReconciler) mapProviderConfigToModelDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
	providerConfig, ok := obj.(*airunwayv1alpha1.InferenceProviderConfig)
	if !ok {
		return nil
	}

	var mdList airunwayv1alpha1.ModelDeploymentList
	if err := r.List(ctx, &mdList); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list ModelDeployments for provider config change", "provider", providerConfig.Name)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(mdList.Items))
	seen := make(map[k8stypes.NamespacedName]struct{}, len(mdList.Items))
	for i := range mdList.Items {
		md := &mdList.Items[i]
		if !providerConfigAffectsModelDeployment(md, providerConfig.Name) {
			continue
		}

		key := k8stypes.NamespacedName{Name: md.Name, Namespace: md.Namespace}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}

	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModelDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.ModelDeployment{}).
		Watches(
			&airunwayv1alpha1.InferenceProviderConfig{},
			handler.EnqueueRequestsFromMapFunc(r.mapProviderConfigToModelDeployments),
			ctrlbuilder.WithPredicates(providerConfigChangePredicate()),
		).
		Named("modeldeployment")

	// Watch InferencePool so the controller reconciles when one is created/deleted.
	// HTTPRoutes are not watched — they may be user-managed (BYO) and we don't
	// want deletion of an HTTPRoute to trigger a reconcile that recreates it.
	// Only add this watch if the gateway CRDs are actually installed.
	if r.GatewayDetector != nil && r.GatewayDetector.IsAvailable(context.Background()) {
		builder = builder.
			Owns(&inferencev1.InferencePool{})
	}

	return builder.Complete(r)
}

// specToMap converts a ModelDeploymentSpec to a map for CEL evaluation
func specToMap(spec *airunwayv1alpha1.ModelDeploymentSpec) (map[string]any, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal spec: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("failed to unmarshal spec: %w", err)
	}
	return m, nil
}

// evaluateCEL evaluates a CEL expression against the spec map
func evaluateCEL(expression string, specMap map[string]any) (bool, error) {
	env, err := cel.NewEnv(
		cel.Variable("spec", cel.DynType),
	)
	if err != nil {
		return false, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("failed to compile CEL expression %q: %w", expression, issues.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return false, fmt.Errorf("failed to create CEL program: %w", err)
	}

	out, _, err := prg.Eval(map[string]any{
		"spec": specMap,
	})
	if err != nil {
		return false, fmt.Errorf("failed to evaluate CEL expression: %w", err)
	}

	if out.Type() != types.BoolType {
		return false, fmt.Errorf("CEL expression did not return bool, got %s", out.Type())
	}

	return out.Value().(bool), nil
}
