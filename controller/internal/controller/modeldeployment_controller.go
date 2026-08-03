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
	"sync"
	"time"

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

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/internal/gateway"
	airmetrics "github.com/ai-runway/airunway/controller/internal/metrics"
	"github.com/ai-runway/airunway/controller/internal/validation"
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

	// phaseCache tracks the last observed phase per ModelDeployment for detecting transitions.
	phaseCacheMu sync.RWMutex
	phaseCache   map[k8stypes.NamespacedName]phaseEntry
}

// phaseEntry holds per-deployment metrics state that the K8s API cannot
// provide: previous phase, wall-clock timestamps, one-shot guards, and
// aggregate gauge data. Volatile on restart; transitions are skipped
// until each deployment reconciles again.
type phaseEntry struct {
	// Phase is the last observed deployment phase.
	Phase airunwayv1alpha1.DeploymentPhase
	// Provider is the provider name for this deployment, used to aggregate metrics.
	Provider string
	// Replicas holds the last observed replica counts (desired, ready, available).
	Replicas [3]int32
	// DeployingTimestamp records when the Deploying phase was first observed.
	// Used to compute provision duration (Deploying→Running wall-clock time).
	DeployingTimestamp time.Time
	// RunningMetricsRecorded tracks whether one-time Running metrics (lead time,
	// provision duration) have already been recorded for this deployment lifecycle.
	RunningMetricsRecorded bool
	// MetricsInitialized tracks whether DORA metric label combinations have been
	// pre-initialized for this deployment's provider. This ensures Prometheus sees
	// zero-valued counters/histograms before any real observations, so that
	// increase() correctly reports the first event.
	MetricsInitialized bool
}

// celEnvOnce lazily initializes a shared CEL environment for evaluating selection rules.
// The environment is safe to share across goroutines since it only declares the "spec" variable.
var (
	celEnvOnce sync.Once
	celEnvInst *cel.Env
	celEnvErr  error
)

func getCELEnv() (*cel.Env, error) {
	celEnvOnce.Do(func() {
		celEnvInst, celEnvErr = cel.NewEnv(
			cel.Variable("spec", cel.DynType),
		)
	})
	return celEnvInst, celEnvErr
}

const (
	ExplicitProviderSelectionReason = "explicit provider selection"
)

// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=airunway.ai,resources=inferenceproviderconfigs,verbs=get;list;watch;update;patch
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
func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	reconcileStart := time.Now()
	logger := log.FromContext(ctx)

	var md airunwayv1alpha1.ModelDeployment

	// Record reconciliation duration when a provider is known.
	defer func() {
		if md.Status.Provider != nil {
			airmetrics.ReconciliationDurationSeconds.WithLabelValues(md.Status.Provider.Name).Observe(time.Since(reconcileStart).Seconds())
		}
	}()

	// Fetch the ModelDeployment
	if err := r.Get(ctx, req.NamespacedName, &md); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// MD was deleted — clean up phase cache, gauges, and gateway routes.
			r.cleanupMetrics(req.NamespacedName)
			r.cleanupGatewayAllowedRoutesForNamespace(ctx, req.Namespace)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Capture previous phase entry for transition detection.
	r.phaseCacheMu.RLock()
	previousEntry := r.phaseCache[req.NamespacedName]
	r.phaseCacheMu.RUnlock()

	// Record metrics when reconciliation returns without error. This includes
	// successful status patches and early-return paths (deletion, pause) where
	// the in-memory state still reflects the API. On error, we skip metrics
	// because the retry will re-reconcile from the old state.
	defer func() {
		if retErr == nil {
			r.recordMetrics(&md, previousEntry)
		}
	}()

	// Save a deep copy as the patch base so we only send changed status fields.
	// This avoids clobbering status fields set by out-of-tree provider controllers.
	base := md.DeepCopy()

	logger.Info("Reconciling ModelDeployment", "name", md.Name, "namespace", md.Namespace)

	// If the ModelDeployment is being deleted, clean up gateway resources and return.
	// This catches foreground deletion or any other finalizer holding the MD open.
	if !md.DeletionTimestamp.IsZero() {
		if err := r.cleanupGatewayResources(ctx, &md); err != nil {
			logger.Error(err, "Failed to clean up gateway resources on deletion")
			r.recordReconcileError(&md, "gateway")
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

	// Step 1: List all InferenceProviderConfigs once for use across validation and selection.
	// This is loaded regardless of EnableProviderSelector because validateSpec needs
	// provider capabilities to determine whether an engine supports CPU-only inference.
	var providerConfigs []airunwayv1alpha1.InferenceProviderConfig
	{
		var providerConfigList airunwayv1alpha1.InferenceProviderConfigList
		if err := r.List(ctx, &providerConfigList); err != nil {
			// If InferenceProviderConfig CRD is not installed, proceed with an empty list.
			// This allows the controller to run without any providers registered.
			if !isNoMatchError(err) {
				logger.Error(err, "Failed to list provider configs")
				return ctrl.Result{}, err
			}
		} else {
			providerConfigs = providerConfigList.Items
		}
	}

	// Step 2: Resolve the serving mode once for downstream use. Unlike engine,
	// serving mode is always derivable from spec (defaulting to Aggregated) so
	// it can be resolved before engine selection.
	resolvedServingMode := md.ResolvedServingMode()

	// Step 3: Defensively reject conflicting image overrides before selection logic.
	// The validating webhook normally prevents this, but objects can bypass
	// admission in tests, during upgrades, or when webhooks are disabled.
	if err := md.Spec.ValidateImageFields(); err != nil {
		logger.Error(err, "Image field validation failed", "name", md.Name)
		r.setImageFieldConflictStatus(&md, err)
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeValidated, metav1.ConditionFalse, "ValidationFailed", err.Error())
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = fmt.Sprintf("Validation failed: %s", err.Error())
		return ctrl.Result{}, r.Status().Patch(ctx, &md, client.MergeFrom(base))
	}

	// Step 4: Select engine if needed (before validation, since validation needs engine type)
	if r.EnableProviderSelector {
		if err := r.selectEngine(ctx, &md, providerConfigs, resolvedServingMode); err != nil {
			logger.Error(err, "Engine selection failed", "name", md.Name)
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeEngineSelected, metav1.ConditionFalse, "SelectionFailed", err.Error())
			md.Status.Message = fmt.Sprintf("Engine selection failed: %s", err.Error())
			r.recordReconcileError(&md, "engine_selection")
			return ctrl.Result{}, r.Status().Patch(ctx, &md, client.MergeFrom(base))
		}
	}

	// Step 5: Resolve the engine type once for downstream use (validation, CEL
	// evaluation, provider selection). We pass it through explicitly rather than
	// mutating md.Spec to avoid any risk of corrupting the shared informer
	// cache's backing data.
	resolvedEngineType := md.ResolvedEngineType()

	// Step 6: Validate the spec (uses resolved engine type and serving mode)
	if err := r.validateSpec(ctx, &md, providerConfigs, resolvedEngineType, resolvedServingMode); err != nil {
		logger.Error(err, "Validation failed", "name", md.Name)
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeValidated, metav1.ConditionFalse, "ValidationFailed", err.Error())
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = fmt.Sprintf("Validation failed: %s", err.Error())
		r.recordReconcileError(&md, "validation")
		return ctrl.Result{}, r.Status().Patch(ctx, &md, client.MergeFrom(base))
	}
	r.setCondition(&md, airunwayv1alpha1.ConditionTypeValidated, metav1.ConditionTrue, "ValidationPassed", "Schema validation passed")

	// Validation passed, so the engine recorded in status is provider-compatible.
	// Flip EngineSelected=True now (selectEngine deliberately defers this).
	if md.Status.Engine != nil && md.Status.Engine.Type != "" {
		if md.Spec.Engine.Type != "" {
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeEngineSelected, metav1.ConditionTrue, "ExplicitSelection", "Engine explicitly specified in spec")
		} else {
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeEngineSelected, metav1.ConditionTrue, "AutoSelected", fmt.Sprintf("Engine %s auto-selected from provider capabilities", md.Status.Engine.Type))
		}
	}

	// Step 7: Reject an unsupported provider switch. Once a provider is recorded
	// in status, re-pointing spec.provider.name to a different provider is not yet
	// supported: the previously-selected provider's resources and finalizer would
	// be orphaned because provider controllers only clean up on ModelDeployment
	// deletion, not on deselection (tracked in
	// https://github.com/ai-runway/airunway/issues/325). Fail explicitly here
	// rather than silently keeping the old provider, so the conflict is visible.
	if md.Spec.Provider != nil && md.Spec.Provider.Name != "" &&
		md.Status.Provider != nil && md.Status.Provider.Name != "" &&
		md.Spec.Provider.Name != md.Status.Provider.Name {
		msg := fmt.Sprintf(
			"changing spec.provider.name from %q to %q after a provider has been selected is not supported; delete and recreate the ModelDeployment to use a different provider",
			md.Status.Provider.Name, md.Spec.Provider.Name,
		)
		logger.Info("Rejected unsupported provider change", "name", md.Name, "from", md.Status.Provider.Name, "to", md.Spec.Provider.Name)
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderSelected, metav1.ConditionFalse, "ProviderChangeNotSupported", msg)
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = msg
		r.recordReconcileError(&md, "provider_change")
		return ctrl.Result{}, r.Status().Patch(ctx, &md, client.MergeFrom(base))
	}

	// Step 8: Run provider selection if needed
	if r.EnableProviderSelector {
		if err := r.selectProvider(ctx, &md, providerConfigs, resolvedEngineType, resolvedServingMode); err != nil {
			logger.Error(err, "Provider selection failed", "name", md.Name)
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderSelected, metav1.ConditionFalse, "SelectionFailed", err.Error())
			md.Status.Message = fmt.Sprintf("Provider selection failed: %s", err.Error())
			r.recordReconcileError(&md, "provider_selection")
			return ctrl.Result{}, r.Status().Patch(ctx, &md, client.MergeFrom(base))
		}
	}

	// Step 9: Update status
	// If no provider is selected yet, stay in Pending
	if md.Status.Provider == nil || md.Status.Provider.Name == "" {
		if md.Spec.Provider != nil && md.Spec.Provider.Name != "" {
			// User explicitly specified a provider
			md.Status.Provider = &airunwayv1alpha1.ProviderStatus{
				Name:           md.Spec.Provider.Name,
				SelectedReason: ExplicitProviderSelectionReason,
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

	// Step 10: Reconcile gateway resources (InferencePool + HTTPRoute) when deployment is running
	if md.Status.Phase == airunwayv1alpha1.DeploymentPhaseRunning {
		if md.Spec.Gateway != nil && md.Spec.Gateway.Enabled != nil && !*md.Spec.Gateway.Enabled {
			// Gateway explicitly disabled — clean up any existing resources
			if err := r.cleanupGatewayResources(ctx, &md); err != nil {
				logger.Error(err, "Failed to clean up gateway resources")
				r.recordReconcileError(&md, "gateway")
			}
		} else {
			if err := r.reconcileGateway(ctx, &md); err != nil {
				logger.Error(err, "Gateway reconciliation failed", "name", md.Name)
				r.recordReconcileError(&md, "gateway")
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
func (r *ModelDeploymentReconciler) validateSpec(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, providerConfigs []airunwayv1alpha1.InferenceProviderConfig, engineType airunwayv1alpha1.EngineType, servingMode airunwayv1alpha1.ServingMode) error {
	spec := &md.Spec

	if err := spec.ValidateImageFields(); err != nil {
		r.setImageFieldConflictStatus(md, err)
		return err
	}

	// Validate model.id is required for huggingface source
	if spec.Model.Source == airunwayv1alpha1.ModelSourceHuggingFace || spec.Model.Source == "" {
		if spec.Model.ID == "" {
			return fmt.Errorf("model.id is required when source is huggingface")
		}
	}

	if engineType == "" {
		return fmt.Errorf("engine.type must be specified or auto-selected from provider capabilities")
	}

	// Mocker mode escape hatch: a ModelDeployment annotated with
	// airunway.ai/dynamo-test-backend=mocker targeting the dynamo provider runs
	// the GPU-less python3 -m dynamo.mocker backend, so the GPU compatibility and
	// disaggregated gpu.count checks below must not reject it. This mirrors the
	// admission webhook (see modeldeployment_webhook.go) so the two cannot drift.
	// Mocker is vLLM-only.
	isDynamoMocker := isDynamoMockerMode(md)
	if isDynamoMocker && engineType != airunwayv1alpha1.EngineTypeVLLM {
		return fmt.Errorf("the dynamo mocker test backend only supports the vllm engine")
	}

	// Validate provider/engine/serving-mode/GPU-CPU compatibility via the
	// shared helper so the webhook and reconciler cannot drift.
	gpuCount := int32(0)
	if spec.Resources != nil && spec.Resources.GPU != nil {
		gpuCount = spec.Resources.GPU.Count
	}
	providerName := ""
	var namedConfig *airunwayv1alpha1.InferenceProviderConfig
	if spec.Provider != nil {
		providerName = spec.Provider.Name
		for i := range providerConfigs {
			if providerConfigs[i].Name == providerName {
				namedConfig = &providerConfigs[i]
				break
			}
		}
	}
	if !isDynamoMocker {
		if ces := validation.CheckProviderCompatibility(
			providerName,
			namedConfig,
			providerConfigs,
			engineType,
			servingMode,
			gpuCount,
		); len(ces) > 0 {
			// Return the first error to preserve the reconciler's existing
			// single-error contract.
			return fmt.Errorf("%s", ces[0].Message)
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

		// The GPU-less mocker backend waives the per-component gpu.count
		// requirement, but the prefill/decode blocks themselves are still
		// required (above) so the dynamo transformer can build both workers.
		if !isDynamoMocker {
			// Prefill must have GPU
			if spec.Scaling.Prefill.GPU == nil || spec.Scaling.Prefill.GPU.Count == 0 {
				return fmt.Errorf("disaggregated mode requires scaling.prefill.gpu.count > 0")
			}

			// Decode must have GPU
			if spec.Scaling.Decode.GPU == nil || spec.Scaling.Decode.GPU.Count == 0 {
				return fmt.Errorf("disaggregated mode requires scaling.decode.gpu.count > 0")
			}
		}
	}

	return nil
}

// engineSupportsCPU was inlined into validation.CheckProviderCompatibility.

// selectEngine auto-selects the engine type from provider capabilities if not specified
func (r *ModelDeploymentReconciler) selectEngine(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, providerConfigs []airunwayv1alpha1.InferenceProviderConfig, servingMode airunwayv1alpha1.ServingMode) error {
	logger := log.FromContext(ctx)

	// If engine type is explicitly specified, just record it in status.
	// The EngineSelected=True condition is intentionally NOT set here — it is
	// only flipped True after downstream provider-compatibility validation
	// passes (see Reconcile), so the condition reflects "selected AND usable"
	// rather than "selected but possibly incompatible".
	if md.Spec.Engine.Type != "" {
		md.Status.Engine = &airunwayv1alpha1.EngineStatus{
			Type:           md.Spec.Engine.Type,
			SelectedReason: "explicit engine selection",
		}
		return nil
	}

	// Skip if engine already auto-selected
	if md.Status.Engine != nil && md.Status.Engine.Type != "" {
		return nil
	}

	if len(providerConfigs) == 0 {
		return fmt.Errorf("no providers registered (InferenceProviderConfig resources not found)")
	}

	// Collect supported engines from ready providers, filtering by per-engine compatibility
	// Determine deployment characteristics
	hasGPU := false
	if md.Spec.Resources != nil && md.Spec.Resources.GPU != nil && md.Spec.Resources.GPU.Count > 0 {
		hasGPU = true
	}
	if md.Spec.Serving != nil && md.Spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		hasGPU = true
	}

	availableEngines := make(map[airunwayv1alpha1.EngineType]string)    // engine -> provider name
	advertisedEngines := make(map[string][]airunwayv1alpha1.EngineType) // provider name -> engines advertised

	for _, pc := range providerConfigs {
		if !pc.Status.Ready || pc.Spec.Capabilities == nil {
			continue
		}

		caps := pc.Spec.Capabilities
		advertisedEngines[pc.Name] = caps.EngineNames()

		for _, engineCap := range caps.Engines {
			// Filter by GPU/CPU compatibility at the engine level
			if hasGPU && !engineCap.GPUSupport {
				continue
			}
			if !hasGPU && !engineCap.CPUSupport {
				continue
			}

			// Filter by serving mode compatibility at the engine level
			if !engineCap.SupportsServingMode(servingMode) {
				continue
			}

			if _, exists := availableEngines[engineCap.Name]; !exists {
				availableEngines[engineCap.Name] = pc.Name
			}
		}
	}

	if len(availableEngines) == 0 {
		logger.Info("No engines available after filtering",
			"hasGPU", hasGPU,
			"servingMode", servingMode,
			"advertisedByProvider", advertisedEngines)
		return fmt.Errorf("no engines available from registered providers (hasGPU=%v, servingMode=%s, advertised=%v)", hasGPU, servingMode, advertisedEngines)
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
			// EngineSelected=True is set in Reconcile after provider-compatibility
			// validation passes; see comment on the explicit-selection branch above.
			return nil
		}
	}

	// Unreachable in practice: enginePreference enumerates every EngineType
	// constant and availableEngines is keyed by that same set, so the loop
	// above always returns when len(availableEngines) > 0. Surface a clear
	// error if a future EngineType is added without updating enginePreference.
	return fmt.Errorf("no engine in preference list matches available engines %v (enginePreference may be missing a newly added EngineType)", availableEngines)
}

// selectProvider runs the provider selection algorithm
func (r *ModelDeploymentReconciler) selectProvider(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, providerConfigs []airunwayv1alpha1.InferenceProviderConfig, resolvedEngineType airunwayv1alpha1.EngineType, resolvedServingMode airunwayv1alpha1.ServingMode) error {
	logger := log.FromContext(ctx)

	// Skip if provider is already selected (either in spec or status)
	if md.Spec.Provider != nil && md.Spec.Provider.Name != "" {
		return nil // User explicitly specified provider
	}
	if md.Status.Provider != nil && md.Status.Provider.Name != "" {
		return nil // Provider already selected
	}

	if len(providerConfigs) == 0 {
		return fmt.Errorf("no providers registered (InferenceProviderConfig resources not found)")
	}

	// Filter to ready providers
	var readyProviders []airunwayv1alpha1.InferenceProviderConfig
	for _, pc := range providerConfigs {
		if pc.Status.Ready {
			readyProviders = append(readyProviders, pc)
		}
	}

	if len(readyProviders) == 0 {
		return fmt.Errorf("no healthy providers available")
	}

	// Run selection algorithm
	selectedProvider, reason, err := r.runSelectionAlgorithm(md, readyProviders, resolvedEngineType, resolvedServingMode)
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
func (r *ModelDeploymentReconciler) runSelectionAlgorithm(md *airunwayv1alpha1.ModelDeployment, providers []airunwayv1alpha1.InferenceProviderConfig, engineType airunwayv1alpha1.EngineType, servingMode airunwayv1alpha1.ServingMode) (string, string, error) {
	spec := &md.Spec

	// Determine GPU requirements
	hasGPU := false
	if spec.Resources != nil && spec.Resources.GPU != nil && spec.Resources.GPU.Count > 0 {
		hasGPU = true
	}
	if spec.Serving != nil && spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		hasGPU = true
	}

	// Convert spec to map for CEL evaluation.
	specMap, err := specToMap(spec)
	if err != nil {
		return "", "", fmt.Errorf("failed to convert spec for CEL evaluation: %w", err)
	}

	// Overlay the resolved engine type so CEL rules like `spec.engine.type == 'vllm'`
	// see the auto-selected engine even though md.Spec was never mutated.
	if engineType != "" {
		engineMap, _ := specMap["engine"].(map[string]any)
		if engineMap == nil {
			engineMap = map[string]any{}
			specMap["engine"] = engineMap
		}
		if t, _ := engineMap["type"].(string); t == "" {
			engineMap["type"] = string(engineType)
		}
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

		// Check engine support and get per-engine capabilities
		engineCap := caps.GetEngineCapability(engineType)
		if engineCap == nil {
			continue
		}

		// Check GPU/CPU support for this specific engine
		if hasGPU && !engineCap.GPUSupport {
			continue
		}
		if !hasGPU && !engineCap.CPUSupport {
			continue
		}

		// Check serving mode support for this specific engine
		if !engineCap.SupportsServingMode(servingMode) {
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

func (r *ModelDeploymentReconciler) setImageFieldConflictStatus(md *airunwayv1alpha1.ModelDeployment, err error) {
	message := err.Error()
	r.setCondition(md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionFalse, "ConflictingImageFields", message)
	md.Status.Image = &airunwayv1alpha1.ImageStatus{
		Requested: md.Spec.ImageOverride(),
		Message:   message,
	}
}

// setCondition updates a condition on the ModelDeployment.
//
// LastTransitionTime is passed as metav1.Now() here, but
// meta.SetStatusCondition only adopts that timestamp when the condition's
// Status actually changes; on no-op updates (same Status) it preserves the
// previously stored LastTransitionTime. So this helper does not clobber the
// transition timestamp on repeated reconciles of an unchanged status.
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

// recordMetrics updates all Prometheus metrics for the current ModelDeployment state.
func (r *ModelDeploymentReconciler) recordMetrics(md *airunwayv1alpha1.ModelDeployment, previous phaseEntry) {
	// Lazy-init the phase cache so recordMetrics is safe even if SetupWithManager was not called
	// (e.g. in unit tests that invoke Reconcile directly).
	r.phaseCacheMu.Lock()
	if r.phaseCache == nil {
		r.phaseCache = make(map[k8stypes.NamespacedName]phaseEntry)
	}
	r.phaseCacheMu.Unlock()

	providerName := ""
	if md.Status.Provider != nil {
		providerName = md.Status.Provider.Name
	}
	currentPhase := md.Status.Phase
	key := k8stypes.NamespacedName{Name: md.Name, Namespace: md.Namespace}

	// Known phases used to zero-initialize all label combinations for this provider.
	phases := []string{"Pending", "Deploying", "Running", "Failed", "Terminating"}

	// Build the updated phase entry. Start from the previous entry to preserve timestamps.
	entry := previous

	// Update replica counts and provider in the entry for aggregate computation
	if md.Status.Replicas != nil {
		entry.Replicas = [3]int32{md.Status.Replicas.Desired, md.Status.Replicas.Ready, md.Status.Replicas.Available}
	} else {
		entry.Replicas = [3]int32{}
	}
	entry.Provider = providerName

	// Zero-initialize all known label combinations so that increase() and
	// rate() work correctly from the first scrape.
	if providerName != "" && !previous.MetricsInitialized {
		airmetrics.ReadyDurationSeconds.WithLabelValues(providerName)
		airmetrics.ProvisionDurationSeconds.WithLabelValues(providerName)
		airmetrics.ReconciliationDurationSeconds.WithLabelValues(providerName)
		for _, errType := range []string{"validation", "engine_selection", "provider_selection", "gateway"} {
			airmetrics.ReconciliationErrorsTotal.WithLabelValues(providerName, errType)
		}
		for _, reason := range []string{"manual", "auto"} {
			airmetrics.ProviderSelection.WithLabelValues(providerName, reason)
		}
		for _, from := range phases {
			for _, to := range phases {
				if from != to {
					airmetrics.PhaseTransitionsTotal.WithLabelValues(providerName, from, to)
				}
			}
		}
		entry.MetricsInitialized = true
	}

	// Record provider selection counter.
	// When the previous provider is empty and a new provider is assigned, it indicates a selection event occured,
	// either auto or manual. We use the presence of the ExplicitProviderSelectionReason reason to distinguish between them.
	if previous.Provider == "" && providerName != "" {
		reason := "auto"
		if md.Status.Provider != nil && md.Status.Provider.SelectedReason == ExplicitProviderSelectionReason {
			reason = "manual"
		}
		airmetrics.ProviderSelection.WithLabelValues(providerName, reason).Inc()
	}

	// Record phase transition counter.
	// When previous.Phase is empty (first reconciliation or after controller restart),
	// we skip recording a transition to avoid a spurious "" -> X event.
	if previous.Phase != "" && currentPhase != previous.Phase {
		// Skip recording transitions that involve deployments without providers.
		if providerName != "" {
			airmetrics.PhaseTransitionsTotal.WithLabelValues(
				providerName, string(previous.Phase), string(currentPhase),
			).Inc()
		}
	}

	// Track when the Deploying phase starts. This gives us a reliable wall-clock
	// anchor for provision duration, immune to condition-timestamp flapping
	// (e.g. ResourceCreated being toggled by conflict retries).
	if currentPhase == airunwayv1alpha1.DeploymentPhaseDeploying && previous.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		entry.DeployingTimestamp = time.Now()
	}

	// Record one-time Running metrics only when we observe an actual phase
	// transition into Running. This avoids duplicate/inflated lead-time samples
	// after controller restarts, where the in-memory cache is empty and the
	// previous phase is unknown.
	transitionedToRunning := currentPhase == airunwayv1alpha1.DeploymentPhaseRunning &&
		previous.Phase != "" &&
		previous.Phase != airunwayv1alpha1.DeploymentPhaseRunning
	if transitionedToRunning && !previous.RunningMetricsRecorded {
		// Skip recording if provider is not known.
		if providerName != "" {
			// Lead time: wall-clock time from CR creation to first observed transition
			// into Running.
			leadTime := time.Since(md.CreationTimestamp.Time).Seconds()
			airmetrics.ReadyDurationSeconds.WithLabelValues(providerName).Observe(leadTime)

			// Provision duration: wall-clock time from Deploying to Running.
			// Only recorded when we observed the Deploying phase start (i.e. the
			// controller was running when the deployment first entered Deploying).
			if !entry.DeployingTimestamp.IsZero() {
				provisionDuration := time.Since(entry.DeployingTimestamp).Seconds()
				airmetrics.ProvisionDurationSeconds.WithLabelValues(providerName).Observe(provisionDuration)
			}

			entry.RunningMetricsRecorded = true
		}
	}

	// Reset RunningMetricsRecorded when leaving Running (allows re-recording if
	// deployment cycles back through Deploying→Running, e.g. after a rollback).
	if currentPhase != airunwayv1alpha1.DeploymentPhaseRunning {
		entry.RunningMetricsRecorded = false
	}

	// Update the phase cache and apply gauge deltas (decrement old, increment new).
	entry.Phase = currentPhase
	r.phaseCacheMu.Lock()
	decrementPhaseEntryGauges(previous)
	incrementPhaseEntryGauges(entry)
	r.phaseCache[key] = entry
	r.phaseCacheMu.Unlock()
}

// decrementPhaseEntryGauges subtracts a phaseEntry's contributions from the aggregate gauges.
func decrementPhaseEntryGauges(e phaseEntry) {
	replicaStates := []string{"desired", "ready", "available"}
	if e.Phase != "" {
		airmetrics.DeploymentStatus.WithLabelValues(e.Provider, string(e.Phase)).Dec()
	}
	for i, s := range replicaStates {
		airmetrics.DeploymentReplicas.WithLabelValues(e.Provider, s).Sub(float64(e.Replicas[i]))
	}
}

// incrementPhaseEntryGauges adds a phaseEntry's contributions to the aggregate gauges.
func incrementPhaseEntryGauges(e phaseEntry) {
	replicaStates := []string{"desired", "ready", "available"}
	if e.Phase != "" {
		airmetrics.DeploymentStatus.WithLabelValues(e.Provider, string(e.Phase)).Inc()
	}
	for i, s := range replicaStates {
		airmetrics.DeploymentReplicas.WithLabelValues(e.Provider, s).Add(float64(e.Replicas[i]))
	}
}

// cleanupMetrics decrements aggregate gauges and removes the phase cache entry for a deleted ModelDeployment.
func (r *ModelDeploymentReconciler) cleanupMetrics(key k8stypes.NamespacedName) {
	r.phaseCacheMu.Lock()
	if old, ok := r.phaseCache[key]; ok {
		decrementPhaseEntryGauges(old)
		delete(r.phaseCache, key)
	}
	r.phaseCacheMu.Unlock()
}

// recordReconcileError records a reconciliation error metric.
// Skipped when no provider is assigned to avoid empty-label series.
func (r *ModelDeploymentReconciler) recordReconcileError(md *airunwayv1alpha1.ModelDeployment, errorType string) {
	if md.Status.Provider == nil {
		return
	}
	airmetrics.ReconciliationErrorsTotal.WithLabelValues(md.Status.Provider.Name, errorType).Inc()
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
	r.phaseCache = make(map[k8stypes.NamespacedName]phaseEntry)

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
	env, err := getCELEnv()
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
