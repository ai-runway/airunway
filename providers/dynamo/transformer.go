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

package dynamo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// DynamoAPIGroup is the API group for Dynamo CRDs
	DynamoAPIGroup = "nvidia.com"
	// DynamoAPIVersion is the current API version for Dynamo CRDs
	DynamoAPIVersion = "v1alpha1"
	// DynamoGraphDeploymentKind is the kind for DynamoGraphDeployment
	DynamoGraphDeploymentKind = "DynamoGraphDeployment"
	// DynamoGraphDeploymentRequestAPIVersion is the intent API served by Dynamo.
	DynamoGraphDeploymentRequestAPIVersion = "v1beta1"
	// DynamoGraphDeploymentRequestKind is the kind for intent-based deployments.
	DynamoGraphDeploymentRequestKind = "DynamoGraphDeploymentRequest"

	// DeploymentModeIntent delegates topology selection to Dynamo's profiler.
	DeploymentModeIntent = "intent"
	// DeploymentModeManual preserves the direct DGD compatibility path.
	DeploymentModeManual = "manual"

	// Default component settings
	DefaultEppReplicas = 1

	// The KV cache block size advertised to the Dynamo
	// EPP via DYN_KV_CACHE_BLOCK_SIZE. It MUST match the worker's vLLM --block-size
	// (currently the vLLM default of 16). If not default, then pass --block-size
	// on the worker args.
	DefaultKVCacheBlockSize = "16"

	// Component types
	ComponentTypeWorker        = "worker"
	ComponentTypeEpp           = "epp"
	defaultModelCacheMountPath = "/model-cache"

	// Sub-component types for disaggregated mode
	SubComponentTypePrefill = "prefill"
	SubComponentTypeDecode  = "decode"

	// VLLMKVTransferConfig is the --kv-transfer-config value required by
	// newer vLLM for NIXL-based disaggregated serving (replaces --connector).
	VLLMKVTransferConfig = `{"kv_connector":"NixlConnector","kv_role":"kv_both"}`
)

// Default upstream runtime image tags, derived from DynamoVersion.
// Declared as vars (not consts) so a build-time ldflags override of
// DynamoVersion in config.go flows through automatically.
var (
	defaultVLLMRuntimeImage   = "nvcr.io/nvidia/ai-dynamo/vllm-runtime:" + DynamoVersion
	defaultSGLangRuntimeImage = "nvcr.io/nvidia/ai-dynamo/sglang-runtime:" + DynamoVersion
	defaultTRTLLMRuntimeImage = "nvcr.io/nvidia/ai-dynamo/tensorrtllm-runtime:" + DynamoVersion
	defaultFrontendImage      = "nvcr.io/nvidia/ai-dynamo/dynamo-frontend:" + DynamoVersion
)

// DynamoOverrides contains Dynamo-specific override configuration
type DynamoOverrides struct {
	// DeploymentMode selects the intent DGDR or direct manual DGD path.
	DeploymentMode string `json:"deploymentMode,omitempty"`

	// SearchStrategy controls DGDR profiling depth.
	SearchStrategy string `json:"searchStrategy,omitempty"`

	// AutoApply controls whether DGDR deploys its selected configuration.
	AutoApply *bool `json:"autoApply,omitempty"`

	// PlannerImage overrides the image used by the DGDR profiling job.
	PlannerImage string `json:"plannerImage,omitempty"`

	// RouterMode is the request routing strategy: kv, round-robin, none
	RouterMode string `json:"routerMode,omitempty"`

	// Frontend contains frontend/router component configuration
	Frontend *FrontendOverrides `json:"frontend,omitempty"`

	// Epp contains EPP component configuration
	Epp *EPPOverrides `json:"epp,omitempty"`

	// hasDirectDGDOverrides records root-key presence so intent mode rejects even
	// empty manual-only override objects instead of silently discarding them.
	hasDirectDGDOverrides bool

	// dgdrSpec is the mode-dependent opaque spec override. In manual mode the
	// same root remains the existing raw-DGD escape hatch.
	dgdrSpec map[string]any
}

// FrontendOverrides contains frontend component configuration
type FrontendOverrides struct {
	Replicas  *int32             `json:"replicas,omitempty"`
	Resources *ResourceOverrides `json:"resources,omitempty"`
}

// EPPOverrides contains EPP component configuration
type EPPOverrides struct {
	Replicas *int32 `json:"replicas,omitempty"`
	Image    string `json:"image,omitempty"`
}

// ResourceOverrides contains resource overrides
type ResourceOverrides struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// dynamoOverridesWire is the strict decoding boundary for keys the transformer
// consumes itself. Spec remains opaque because it is passed through to the
// upstream DynamoGraphDeployment and validated against the installed CRD.
type dynamoOverridesWire struct {
	DeploymentMode string             `json:"deploymentMode,omitempty"`
	SearchStrategy string             `json:"searchStrategy,omitempty"`
	AutoApply      *bool              `json:"autoApply,omitempty"`
	PlannerImage   string             `json:"plannerImage,omitempty"`
	RouterMode     string             `json:"routerMode,omitempty"`
	Frontend       *FrontendOverrides `json:"frontend,omitempty"`
	Epp            *EPPOverrides      `json:"epp,omitempty"`
	Spec           json.RawMessage    `json:"spec,omitempty"`
}

// Transformer handles transformation of ModelDeployment to DynamoGraphDeployment
type Transformer struct{}

// NewTransformer creates a new Dynamo transformer
func NewTransformer() *Transformer {
	return &Transformer{}
}

// Transform converts a ModelDeployment to a DynamoGraphDeployment
func (t *Transformer) Transform(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
) ([]*unstructured.Unstructured, error) {
	// Parse overrides if present
	overrides, err := t.parseOverrides(md)
	if err != nil {
		return nil, fmt.Errorf("failed to parse provider overrides: %w", err)
	}

	deploymentMode, err := t.resolveDeploymentMode(md, overrides)
	if err != nil {
		return nil, err
	}
	if err := validateDeploymentModeTransition(md, deploymentMode); err != nil {
		return nil, err
	}
	if deploymentMode == DeploymentModeIntent {
		// Intent mode lets Dynamo profile the cluster and choose the serving
		// topology instead of Airunway fixing one up front.
		return t.transformDGDR(md, overrides)
	}

	// Create the DynamoGraphDeployment
	dgd := &unstructured.Unstructured{}
	dgd.SetAPIVersion(fmt.Sprintf("%s/%s", DynamoAPIGroup, DynamoAPIVersion))
	dgd.SetKind(DynamoGraphDeploymentKind)
	dgd.SetName(md.Name)
	dgd.SetNamespace(md.Namespace)

	// Set OwnerReference to the parent ModelDeployment for proper ownership tracking
	dgd.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         airunwayv1alpha1.GroupVersion.String(),
			Kind:               "ModelDeployment",
			Name:               md.Name,
			UID:                md.UID,
			Controller:         boolPtr(true),
			BlockOwnerDeletion: boolPtr(true),
		},
	})

	// Add labels
	labels := map[string]string{
		airunwayv1alpha1.LabelManagedBy:       "airunway",
		airunwayv1alpha1.LabelModelDeployment: md.Name,
		"airunway.ai/model-id":                sanitizeLabelValue(md.Spec.Model.ID),
		"airunway.ai/engine-type":             string(md.ResolvedEngineType()),
	}
	dgd.SetLabels(labels)

	// Build the spec
	spec := map[string]interface{}{
		"backendFramework": t.mapEngineType(md.ResolvedEngineType()),
	}

	services, err := t.buildServices(md, overrides)
	if err != nil {
		return nil, fmt.Errorf("failed to build services: %w", err)
	}
	spec["services"] = services

	// Add PVCs if storage is configured
	if md.Spec.Model.Storage != nil && len(md.Spec.Model.Storage.Volumes) > 0 {
		spec["pvcs"] = t.buildPVCs(md)
	}

	if err := unstructured.SetNestedField(dgd.Object, spec, "spec"); err != nil {
		return nil, fmt.Errorf("failed to set spec: %w", err)
	}

	// Apply escape hatch overrides last so they can override any field
	if err := applyOverrides(dgd, md); err != nil {
		return nil, fmt.Errorf("failed to apply provider overrides: %w", err)
	}

	return []*unstructured.Unstructured{dgd}, nil
}

func validateDeploymentModeTransition(md *airunwayv1alpha1.ModelDeployment, deploymentMode string) error {
	if md.Status.Provider == nil || md.Status.Provider.ResourceKind == "" {
		return nil
	}

	desiredKind := DynamoGraphDeploymentKind
	if deploymentMode == DeploymentModeIntent {
		desiredKind = DynamoGraphDeploymentRequestKind
	}
	if md.Status.Provider.ResourceKind == desiredKind {
		return nil
	}

	return fmt.Errorf(
		"cannot change Dynamo deployment mode from %s to %s; delete and recreate the ModelDeployment",
		md.Status.Provider.ResourceKind,
		desiredKind,
	)
}

// resolveDeploymentMode uses direct DGD rendering unless intent mode is explicit.
func (t *Transformer) resolveDeploymentMode(
	md *airunwayv1alpha1.ModelDeployment,
	overrides *DynamoOverrides,
) (string, error) {
	// Mocker is a test-only module in the planner image and cannot use DGDR's real
	// profiling flow, so it must continue through the direct DGD renderer.
	if isMockerMode(md) {
		return DeploymentModeManual, nil
	}

	switch overrides.DeploymentMode {
	case DeploymentModeIntent:
		return DeploymentModeIntent, nil
	case DeploymentModeManual:
		if overrides.SearchStrategy != "" || overrides.AutoApply != nil || overrides.PlannerImage != "" {
			return "", fmt.Errorf(
				"dynamo searchStrategy, autoApply, and plannerImage overrides require deploymentMode %q",
				DeploymentModeIntent,
			)
		}
		return DeploymentModeManual, nil
	case "":
		if overrides.SearchStrategy != "" || overrides.AutoApply != nil || overrides.PlannerImage != "" {
			return "", fmt.Errorf(
				"dynamo searchStrategy, autoApply, and plannerImage overrides require deploymentMode %q",
				DeploymentModeIntent,
			)
		}
		return DeploymentModeManual, nil
	default:
		return "", fmt.Errorf(
			"unsupported Dynamo deploymentMode %q: must be %q or %q",
			overrides.DeploymentMode, DeploymentModeIntent, DeploymentModeManual,
		)
	}
}

// transformDGDR creates the minimal intent document needed for Dynamo to profile
// the model and automatically apply its selected DynamoGraphDeployment.
//
//nolint:gocognit,gocyclo // DGDR intent assembly merges several optional upstream schema sections.
func (t *Transformer) transformDGDR(
	md *airunwayv1alpha1.ModelDeployment,
	overrides *DynamoOverrides,
) ([]*unstructured.Unstructured, error) {
	if overrides.hasDirectDGDOverrides {
		return nil, fmt.Errorf(
			"dynamo direct-DGD overrides require provider.overrides.deploymentMode %q",
			DeploymentModeManual,
		)
	}
	// Dynamo injects this fixed Secret name into both the profiler and generated
	// serving pods. Manual mode remains available when a custom Secret name is needed.
	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" &&
		md.Spec.Secrets.HuggingFaceToken != HuggingFaceTokenSecretName {
		return nil, fmt.Errorf(
			"dynamo intent mode requires spec.secrets.huggingFaceToken to be %q; "+
				"use deploymentMode %q for custom Secret names",
			HuggingFaceTokenSecretName, DeploymentModeManual,
		)
	}

	dgdr := &unstructured.Unstructured{}
	dgdr.SetAPIVersion(fmt.Sprintf("%s/%s", DynamoAPIGroup, DynamoGraphDeploymentRequestAPIVersion))
	dgdr.SetKind(DynamoGraphDeploymentRequestKind)
	dgdr.SetName(md.Name)
	dgdr.SetNamespace(md.Namespace)
	dgdr.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         airunwayv1alpha1.GroupVersion.String(),
			Kind:               "ModelDeployment",
			Name:               md.Name,
			UID:                md.UID,
			Controller:         boolPtr(true),
			BlockOwnerDeletion: boolPtr(true),
		},
	})

	labels := map[string]string{
		airunwayv1alpha1.LabelManagedBy:       "airunway",
		airunwayv1alpha1.LabelModelDeployment: md.Name,
		"airunway.ai/model-id":                sanitizeLabelValue(md.Spec.Model.ID),
		"airunway.ai/engine-type":             string(md.ResolvedEngineType()),
	}
	dgdr.SetLabels(labels)
	// Record the ModelDeployment generation used to create this immutable intent;
	// comparing this marker avoids diffing webhook-defaulted DGDR fields later.
	dgdr.SetAnnotations(map[string]string{
		"airunway.ai/model-deployment-generation": strconv.FormatInt(md.Generation, 10),
	})

	backend := string(md.ResolvedEngineType())
	switch md.ResolvedEngineType() {
	case airunwayv1alpha1.EngineTypeVLLM, airunwayv1alpha1.EngineTypeSGLang, airunwayv1alpha1.EngineTypeTRTLLM:
	default:
		// The live DGDR API defaults to auto when Airunway has not selected a
		// concrete backend supported by the profiler.
		backend = "auto"
	}

	generatedDGDLabels := make(map[string]any, len(labels))
	for key, value := range labels {
		generatedDGDLabels[key] = value
	}
	spec := map[string]any{
		"model":          md.Spec.Model.ID,
		"backend":        backend,
		"searchStrategy": "rapid",
		"autoApply":      true,
		"overrides": map[string]any{
			// The installed v1beta1 DGDR API requires its embedded DGD override
			// to use v1alpha1, which is also the cluster's served DGD version.
			"dgd": map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", DynamoAPIGroup, DynamoAPIVersion),
				"kind":       DynamoGraphDeploymentKind,
				"metadata": map[string]any{
					// Pinning the generated DGD name preserves the expected
					// <ModelDeployment-name>-frontend Service name.
					"name":   md.Name,
					"labels": generatedDGDLabels,
				},
			},
		},
	}
	if overrides.dgdrSpec != nil {
		spec = deepMerge(spec, overrides.dgdrSpec)
	}
	if overrides.SearchStrategy != "" {
		spec["searchStrategy"] = overrides.SearchStrategy
	}
	if overrides.AutoApply != nil {
		spec["autoApply"] = *overrides.AutoApply
	}
	if overrides.PlannerImage != "" {
		spec["image"] = overrides.PlannerImage
	}

	// Airunway fields remain authoritative when an equivalent field exists.
	spec["model"] = md.Spec.Model.ID
	spec["backend"] = backend
	if md.Spec.Resources != nil && md.Spec.Resources.GPU != nil && md.Spec.Resources.GPU.Count > 0 {
		hardware, _ := spec["hardware"].(map[string]any)
		if hardware == nil {
			hardware = map[string]any{}
		}
		hardware["totalGpus"] = int64(md.Spec.Resources.GPU.Count)
		spec["hardware"] = hardware
	}
	if md.Spec.Model.Storage != nil {
		for _, volume := range md.Spec.Model.Storage.Volumes {
			if volume.Purpose != airunwayv1alpha1.VolumePurposeModelCache {
				continue
			}
			mountPath := volume.MountPath
			if mountPath == "" {
				mountPath = defaultModelCacheMountPath
			}
			modelCache, _ := spec["modelCache"].(map[string]any)
			if modelCache == nil {
				modelCache = map[string]any{}
			}
			modelCache["pvcName"] = volume.ResolvedClaimName(md.Name)
			modelCache["pvcMountPath"] = mountPath
			spec["modelCache"] = modelCache
			break
		}
	}

	// Preserve user DGD customization while forcing stable linkage metadata.
	dgdOverride, _, _ := unstructured.NestedMap(spec, "overrides", "dgd")
	if dgdOverride == nil {
		dgdOverride = map[string]any{}
	}
	metadata, _ := dgdOverride["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["name"] = md.Name
	userLabels, _ := metadata["labels"].(map[string]any)
	if userLabels == nil {
		userLabels = map[string]any{}
	}
	maps.Copy(userLabels, generatedDGDLabels)
	metadata["labels"] = userLabels
	dgdOverride["metadata"] = metadata
	if err := unstructured.SetNestedMap(spec, dgdOverride, "overrides", "dgd"); err != nil {
		return nil, fmt.Errorf("failed to set generated DGD override: %w", err)
	}
	dgdr.Object["spec"] = spec

	return []*unstructured.Unstructured{dgdr}, nil
}

// parseOverrides parses the provider.overrides field into DynamoOverrides
func (t *Transformer) parseOverrides(md *airunwayv1alpha1.ModelDeployment) (*DynamoOverrides, error) {
	if md.Spec.Provider == nil || md.Spec.Provider.Overrides == nil {
		return &DynamoOverrides{}, nil
	}

	// Validate root keys separately before strict typed decoding. Besides preserving
	// a deterministic error that names every unsupported root, this keeps exact-root
	// "spec" semantics: encoding/json would otherwise match "Spec" case-insensitively.
	var overrideRoots map[string]interface{}
	if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &overrideRoots); err != nil {
		return nil, fmt.Errorf("failed to unmarshal overrides: %w", err)
	}
	if err := validateDynamoOverrideRootKeys(overrideRoots); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(md.Spec.Provider.Overrides.Raw))
	decoder.DisallowUnknownFields()
	var wire dynamoOverridesWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("failed to unmarshal overrides: %w", err)
	}

	var dgdrSpec map[string]any
	if len(wire.Spec) > 0 {
		if err := json.Unmarshal(wire.Spec, &dgdrSpec); err != nil {
			return nil, fmt.Errorf("failed to unmarshal spec override: %w", err)
		}
	}

	return &DynamoOverrides{
		DeploymentMode:        wire.DeploymentMode,
		SearchStrategy:        wire.SearchStrategy,
		AutoApply:             wire.AutoApply,
		PlannerImage:          wire.PlannerImage,
		RouterMode:            wire.RouterMode,
		Frontend:              wire.Frontend,
		Epp:                   wire.Epp,
		hasDirectDGDOverrides: hasDirectDGDOverrideRoots(overrideRoots),
		dgdrSpec:              dgdrSpec,
	}, nil
}

// hasDirectDGDOverrideRoots distinguishes legacy DGD customization from the
// deploymentMode control key before intent rendering can discard any values.
func hasDirectDGDOverrideRoots(overrideRoots map[string]any) bool {
	for key := range overrideRoots {
		switch strings.ToLower(key) {
		case "routermode", "frontend", "epp":
			return true
		}
	}
	return false
}

// mapEngineType maps AI Runway engine types to Dynamo backend framework names
func (t *Transformer) mapEngineType(engineType airunwayv1alpha1.EngineType) string {
	switch engineType {
	case airunwayv1alpha1.EngineTypeVLLM:
		return "vllm"
	case airunwayv1alpha1.EngineTypeSGLang:
		return "sglang"
	case airunwayv1alpha1.EngineTypeTRTLLM:
		return "trtllm"
	default:
		return string(engineType)
	}
}

// buildServices creates the services map for DynamoGraphDeployment
func (t *Transformer) buildServices(md *airunwayv1alpha1.ModelDeployment, overrides *DynamoOverrides) (map[string]interface{}, error) {
	services := map[string]interface{}{}

	// Determine serving mode
	servingMode := airunwayv1alpha1.ServingModeAggregated
	if md.Spec.Serving != nil && md.Spec.Serving.Mode != "" {
		servingMode = md.Spec.Serving.Mode
	}

	// Get the image to use
	image := t.getImage(md)

	gatewayEnabled := md.Spec.Gateway == nil || md.Spec.Gateway.Enabled == nil || *md.Spec.Gateway.Enabled

	// Mocker mode always uses the standalone Frontend path. It exercises the
	// Dynamo Frontend → mocker worker slice without EPP/GAIE complexity, so the
	// gateway/EPP branch is never taken regardless of spec.gateway.enabled.
	if isMockerMode(md) {
		gatewayEnabled = false
	}

	if gatewayEnabled {
		// GAIE path: Gateway → EPP → worker frontendSidecar. No standalone
		// Frontend — each worker's sidecar handles requests locally.
		services["Epp"] = t.buildEPP(overrides, servingMode)
	} else {
		// Non-GAIE path: standalone Frontend service handles routing.
		// No EPP or frontendSidecars needed.
		services["Frontend"] = t.buildFrontendService(md, overrides)
	}

	if servingMode == airunwayv1alpha1.ServingModeDisaggregated {
		if md.Spec.Scaling == nil {
			return nil, fmt.Errorf("spec.scaling is required for disaggregated serving mode")
		}
		if md.Spec.Scaling.Prefill == nil {
			return nil, fmt.Errorf("spec.scaling.prefill is required for disaggregated serving mode")
		}
		if md.Spec.Scaling.Decode == nil {
			return nil, fmt.Errorf("spec.scaling.decode is required for disaggregated serving mode")
		}
		// Disaggregated mode: separate prefill and decode workers
		prefillWorker, err := t.buildPrefillWorker(md, image, gatewayEnabled)
		if err != nil {
			return nil, fmt.Errorf("failed to build prefill worker: %w", err)
		}
		services["VllmPrefillWorker"] = prefillWorker
		decodeWorker, err := t.buildDecodeWorker(md, image, gatewayEnabled)
		if err != nil {
			return nil, fmt.Errorf("failed to build decode worker: %w", err)
		}
		services["VllmDecodeWorker"] = decodeWorker
	} else {
		// Aggregated mode: single worker
		aggregatedWorker, err := t.buildAggregatedWorker(md, image, gatewayEnabled)
		if err != nil {
			return nil, fmt.Errorf("failed to build aggregated worker: %w", err)
		}
		services["VllmWorker"] = aggregatedWorker
	}

	return services, nil
}

// buildFrontendService creates the standalone frontend service for non-GAIE
// deployments (gateway disabled). The Frontend handles request routing when
// there is no InferencePool/EPP path.
func (t *Transformer) buildFrontendService(md *airunwayv1alpha1.ModelDeployment, overrides *DynamoOverrides) map[string]interface{} {
	replicas := int64(1)
	if overrides.Frontend != nil && overrides.Frontend.Replicas != nil {
		replicas = int64(*overrides.Frontend.Replicas)
	}

	routerMode := "round-robin"
	if overrides.RouterMode != "" {
		routerMode = overrides.RouterMode
	}

	cpu := "2"
	memory := "4Gi"
	// Mocker mode targets GPU-less CI nodes that are often small. The Frontend
	// is a lightweight router, so default to modest requests (overridable) to
	// leave headroom for the mocker worker on the same node.
	if isMockerMode(md) {
		cpu = MockerWorkerCPU
		memory = MockerWorkerMemory
	}
	if overrides.Frontend != nil && overrides.Frontend.Resources != nil {
		if overrides.Frontend.Resources.CPU != "" {
			cpu = overrides.Frontend.Resources.CPU
		}
		if overrides.Frontend.Resources.Memory != "" {
			memory = overrides.Frontend.Resources.Memory
		}
	}

	frontend := map[string]interface{}{
		"componentType": "frontend",
		"replicas":      replicas,
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{
				"cpu":    cpu,
				"memory": memory,
			},
		},
		"extraPodSpec": map[string]interface{}{
			"mainContainer": map[string]interface{}{
				"image": t.getImage(md),
				"env": []interface{}{
					map[string]interface{}{
						"name":  "DYN_ROUTER_MODE",
						"value": routerMode,
					},
				},
			},
		},
	}

	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" {
		frontend["envFromSecret"] = md.Spec.Secrets.HuggingFaceToken
	}

	return frontend
}

// buildEPP creates the EPP service configuration.
//
// The plugin set and env vars are mode-aware. Both modes set DYN_KV_CACHE_BLOCK_SIZE,
// DYN_MODEL_NAME, and DYN_ENFORCE_DISAGG on the EPP container. The Dynamo KV scorer
// FFI reads these at init time and create_routers fails ("code 5") without them on
// Dynamo runtime 1.0.2+. The shape mirrors the canonical examples in
// ai-dynamo/dynamo/examples/backends/vllm/deploy/gaie/{agg,disagg}.yaml.
func (t *Transformer) buildEPP(overrides *DynamoOverrides, servingMode airunwayv1alpha1.ServingMode) map[string]interface{} {
	// Determine replicas
	replicas := int64(DefaultEppReplicas)
	if overrides.Epp != nil && overrides.Epp.Replicas != nil {
		replicas = int64(*overrides.Epp.Replicas)
	}

	// EPP image defaults to the frontend runtime image (per Dynamo docs, the
	// frontend image can be used for the EPP) but can be overridden if you
	// choose to build the EPP image.
	eppImage := defaultFrontendImage
	if overrides.Epp != nil && overrides.Epp.Image != "" {
		eppImage = overrides.Epp.Image
	}

	isDisagg := servingMode == airunwayv1alpha1.ServingModeDisaggregated

	// In aggregated mode, set decode_fallback=true to avoid "create_routers failed" errors.
	// In disaggregated mode, set it to false to catch missing prefill workers.
	decodeFallback := "true"
	// DYN_ENFORCE_DISAGG is a no-op on 1.0.2 / 1.1.1 (the binding doesn't read
	// it) but is the equivalent knob on Dynamo main with inverted semantics.
	// We set both so this code is forward-compatible with a future runtime bump.
	enforceDisagg := "false"
	if isDisagg {
		decodeFallback = "false"
		enforceDisagg = "true"
	}

	env := []interface{}{
		map[string]interface{}{
			"name":  "DYN_DECODE_FALLBACK",
			"value": decodeFallback,
		},
		map[string]interface{}{
			"name":  "DYN_ENFORCE_DISAGG",
			"value": enforceDisagg,
		},
	}

	plugins, schedulingProfiles := t.buildEPPPluginsAndProfiles(isDisagg)

	epp := map[string]interface{}{
		"componentType": ComponentTypeEpp,
		"replicas":      replicas,
		"extraPodSpec": map[string]interface{}{
			"mainContainer": map[string]interface{}{
				"image": eppImage,
				"env":   env,
			},
		},
		"eppConfig": map[string]interface{}{
			"config": map[string]interface{}{
				"plugins":            plugins,
				"schedulingProfiles": schedulingProfiles,
			},
		},
	}

	return epp
}

// buildEPPPluginsAndProfiles returns the EPP plugin list and scheduling profiles
// matching the canonical Dynamo examples for the requested serving mode.
//
// Aggregated mode emits: disagg-profile-handler, decode-filter (allowsNoLabel=true),
// picker, dyn-decode-scorer, and a single "decode" scheduling profile.
//
// Disaggregated mode additionally emits prefill-filter (allowsNoLabel=false),
// dyn-prefill-scorer, and a separate "prefill" scheduling profile so the
// disagg-profile-handler can dispatch prefill and decode requests independently.
func (t *Transformer) buildEPPPluginsAndProfiles(isDisagg bool) ([]interface{}, []interface{}) {
	decodeFilter := map[string]interface{}{
		"name": "decode-filter",
		"type": "label-filter",
		"parameters": map[string]interface{}{
			"label": "nvidia.com/dynamo-sub-component-type",
			"validValues": []interface{}{
				"decode",
			},
			"allowsNoLabel": !isDisagg,
		},
	}

	picker := map[string]interface{}{
		"name": "picker",
		"type": "max-score-picker",
	}

	dynDecode := map[string]interface{}{
		"name": "dyn-decode",
		"type": "dyn-decode-scorer",
	}

	disaggHandler := map[string]interface{}{
		"type": "disagg-profile-handler",
	}

	decodeProfile := map[string]interface{}{
		"name": "decode",
		"plugins": []interface{}{
			map[string]interface{}{"pluginRef": "decode-filter", "weight": int64(1)},
			map[string]interface{}{"pluginRef": "dyn-decode", "weight": int64(1)},
			map[string]interface{}{"pluginRef": "picker", "weight": int64(1)},
		},
	}

	if !isDisagg {
		return []interface{}{
				disaggHandler,
				decodeFilter,
				picker,
				dynDecode,
			}, []interface{}{
				decodeProfile,
			}
	}

	prefillFilter := map[string]interface{}{
		"name": "prefill-filter",
		"type": "label-filter",
		"parameters": map[string]interface{}{
			"label": "nvidia.com/dynamo-sub-component-type",
			"validValues": []interface{}{
				"prefill",
			},
			"allowsNoLabel": false,
		},
	}

	dynPrefill := map[string]interface{}{
		"name": "dyn-prefill",
		"type": "dyn-prefill-scorer",
	}

	prefillProfile := map[string]interface{}{
		"name": "prefill",
		"plugins": []interface{}{
			map[string]interface{}{"pluginRef": "prefill-filter", "weight": int64(1)},
			map[string]interface{}{"pluginRef": "dyn-prefill", "weight": int64(1)},
			map[string]interface{}{"pluginRef": "picker", "weight": int64(1)},
		},
	}

	return []interface{}{
			disaggHandler,
			prefillFilter,
			decodeFilter,
			picker,
			dynPrefill,
			dynDecode,
		}, []interface{}{
			prefillProfile,
			decodeProfile,
		}
}

// buildAggregatedWorker creates the worker service for aggregated mode.
//
// Phase 2 TODO (Dynamo EPP integration): pass --block-size matching
// DefaultKVCacheBlockSize so the worker's vLLM cache geometry agrees with the
// EPP's DYN_KV_CACHE_BLOCK_SIZE. Today we rely on the vLLM default (16) lining
// up with the EPP env, which is fragile. See ai-dynamo/dynamo agg.yaml example.
func (t *Transformer) buildAggregatedWorker(md *airunwayv1alpha1.ModelDeployment, image string, gatewayEnabled bool) (map[string]interface{}, error) {
	// Get replicas
	replicas := int64(1)
	if md.Spec.Scaling != nil && md.Spec.Scaling.Replicas > 0 {
		replicas = int64(md.Spec.Scaling.Replicas)
	}

	// Build resource limits
	resources := t.buildResourceLimits(md.Spec.Resources)

	// Build engine arguments
	args, err := t.buildEngineArgs(md)
	if err != nil {
		return nil, err
	}

	command := t.engineCommand(md.ResolvedEngineType())

	// Mocker mode: swap the real engine for python3 -m dynamo.mocker and replace
	// the GPU resources with small CPU/memory requests+limits (no GPU) so the
	// worker schedules on CPU-only nodes. Equal requests and limits make the pod
	// Guaranteed QoS — the point is simply to avoid BestEffort, which is evicted
	// first under memory pressure and rejected by request-mandating LimitRanges.
	if isMockerMode(md) {
		command = mockerCommand()
		args = buildMockerArgs(md)
		resources = mockerWorkerResources()
	}

	worker := map[string]interface{}{
		"componentType": ComponentTypeWorker,
		"replicas":      replicas,
		"resources":     resources,
		"extraPodSpec": map[string]interface{}{
			"mainContainer": map[string]interface{}{
				"image":   image,
				"command": toInterfaceSlice(command),
				"args":    toInterfaceSlice(args),
			},
		},
	}

	if gatewayEnabled {
		worker["frontendSidecar"] = t.buildFrontendSidecar(md, false)
	}

	// Add secret reference if specified
	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" {
		worker["envFromSecret"] = md.Spec.Secrets.HuggingFaceToken
	}

	// Add node selector and tolerations
	t.addSchedulingConfig(worker, md)

	// Add storage configuration (PVC volume mounts and HF_HOME)
	t.addStorageConfig(worker, md)
	t.maybeInjectVLLMSideChannelHost(worker, md)

	return worker, nil
}

// buildFrontendSidecar returns the frontendSidecar config for a worker service.
// The Dynamo operator (v1.1.0+) injects a frontend container on each worker pod
// so the InferencePool can route directly to workers on port 8000.
//
// Aggregated mode uses "--router-mode direct" — the sidecar forwards to the
// colocated engine without internal routing since the EPP handles pod selection.
//
// Disaggregated mode omits --router-mode so the sidecar uses the Dynamo default,
// allowing the prefill router to coordinate worker selection.
func (t *Transformer) buildFrontendSidecar(md *airunwayv1alpha1.ModelDeployment, disagg bool) map[string]interface{} {
	args := []interface{}{"-m", "dynamo.frontend"}
	if !disagg {
		args = append(args, "--router-mode", "direct")
	}
	sidecar := map[string]interface{}{
		"image": defaultVLLMRuntimeImage,
		"args":  args,
	}
	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" {
		sidecar["envFromSecret"] = md.Spec.Secrets.HuggingFaceToken
	}
	return sidecar
}

// buildPrefillWorker creates the prefill worker for disaggregated mode.
func (t *Transformer) buildPrefillWorker(md *airunwayv1alpha1.ModelDeployment, image string, gatewayEnabled bool) (map[string]interface{}, error) {
	prefillSpec := md.Spec.Scaling.Prefill

	// Build resource limits and requests from component spec
	limits := map[string]interface{}{}
	requests := map[string]interface{}{}

	if prefillSpec.GPU != nil && prefillSpec.GPU.Count > 0 {
		gpuCount := fmt.Sprintf("%d", prefillSpec.GPU.Count)
		limits["gpu"] = gpuCount
		requests["gpu"] = gpuCount
	}
	if prefillSpec.Memory != "" {
		limits["memory"] = prefillSpec.Memory
	}

	resources := map[string]interface{}{
		"limits":   limits,
		"requests": requests,
	}

	// Dynamo 1.0.x uses an explicit disaggregation mode for worker roles.
	args, err := t.buildEngineArgs(md)
	if err != nil {
		return nil, err
	}
	args = append(args, "--disaggregation-mode", SubComponentTypePrefill)
	if md.ResolvedEngineType() == airunwayv1alpha1.EngineTypeVLLM {
		args = append(args, "--kv-transfer-config", VLLMKVTransferConfig)
	}

	command := t.engineCommand(md.ResolvedEngineType())

	// Mocker mode: swap the real engine for python3 -m dynamo.mocker and replace
	// the GPU resources with small CPU/memory requests+limits (no GPU). The mocker
	// keeps --disaggregation-mode but does NOT use --kv-transfer-config (that NIXL
	// flag is real-vLLM-only).
	if isMockerMode(md) {
		command = mockerCommand()
		args = append(buildMockerArgs(md), "--disaggregation-mode", SubComponentTypePrefill)
		resources = mockerWorkerResources()
	}

	worker := map[string]interface{}{
		"componentType":    ComponentTypeWorker,
		"subComponentType": SubComponentTypePrefill,
		"replicas":         int64(prefillSpec.Replicas),
		"resources":        resources,
		"extraPodSpec": map[string]interface{}{
			"mainContainer": map[string]interface{}{
				"image":   image,
				"command": toInterfaceSlice(command),
				"args":    toInterfaceSlice(args),
			},
		},
	}

	if gatewayEnabled {
		worker["frontendSidecar"] = t.buildFrontendSidecar(md, true)
	}

	// Add secret reference if specified
	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" {
		worker["envFromSecret"] = md.Spec.Secrets.HuggingFaceToken
	}

	// Add node selector and tolerations
	t.addSchedulingConfig(worker, md)

	// Add storage configuration (PVC volume mounts and HF_HOME)
	t.addStorageConfig(worker, md)
	t.maybeInjectVLLMSideChannelHost(worker, md)

	return worker, nil
}

// buildDecodeWorker creates the decode worker for disaggregated mode.
func (t *Transformer) buildDecodeWorker(md *airunwayv1alpha1.ModelDeployment, image string, gatewayEnabled bool) (map[string]interface{}, error) {
	decodeSpec := md.Spec.Scaling.Decode

	// Build resource limits and requests from component spec
	limits := map[string]interface{}{}
	requests := map[string]interface{}{}

	if decodeSpec.GPU != nil && decodeSpec.GPU.Count > 0 {
		gpuCount := fmt.Sprintf("%d", decodeSpec.GPU.Count)
		limits["gpu"] = gpuCount
		requests["gpu"] = gpuCount
	}
	if decodeSpec.Memory != "" {
		limits["memory"] = decodeSpec.Memory
	}

	resources := map[string]interface{}{
		"limits":   limits,
		"requests": requests,
	}

	// Dynamo 1.0.x uses an explicit disaggregation mode for worker roles.
	args, err := t.buildEngineArgs(md)
	if err != nil {
		return nil, err
	}
	args = append(args, "--disaggregation-mode", SubComponentTypeDecode)
	if md.ResolvedEngineType() == airunwayv1alpha1.EngineTypeVLLM {
		args = append(args, "--kv-transfer-config", VLLMKVTransferConfig)
	}

	command := t.engineCommand(md.ResolvedEngineType())

	// Mocker mode: swap the real engine for python3 -m dynamo.mocker and replace
	// the GPU resources with small CPU/memory requests+limits (no GPU). The mocker
	// keeps --disaggregation-mode but does NOT use --kv-transfer-config (that NIXL
	// flag is real-vLLM-only).
	if isMockerMode(md) {
		command = mockerCommand()
		args = append(buildMockerArgs(md), "--disaggregation-mode", SubComponentTypeDecode)
		resources = mockerWorkerResources()
	}

	worker := map[string]interface{}{
		"componentType":    ComponentTypeWorker,
		"subComponentType": SubComponentTypeDecode,
		"replicas":         int64(decodeSpec.Replicas),
		"resources":        resources,
		"extraPodSpec": map[string]interface{}{
			"mainContainer": map[string]interface{}{
				"image":   image,
				"command": toInterfaceSlice(command),
				"args":    toInterfaceSlice(args),
			},
		},
	}

	if gatewayEnabled {
		worker["frontendSidecar"] = t.buildFrontendSidecar(md, true)
	}

	// Add secret reference if specified
	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" {
		worker["envFromSecret"] = md.Spec.Secrets.HuggingFaceToken
	}

	// Add node selector and tolerations
	t.addSchedulingConfig(worker, md)

	// Add storage configuration (PVC volume mounts and HF_HOME)
	t.addStorageConfig(worker, md)
	t.maybeInjectVLLMSideChannelHost(worker, md)

	return worker, nil
}

// buildResourceLimits creates resource limits and requests from ResourceSpec
func (t *Transformer) buildResourceLimits(spec *airunwayv1alpha1.ResourceSpec) map[string]interface{} {
	limits := map[string]interface{}{}
	requests := map[string]interface{}{}

	if spec == nil {
		return map[string]interface{}{
			"limits":   limits,
			"requests": requests,
		}
	}

	if spec.GPU != nil && spec.GPU.Count > 0 {
		gpuCount := fmt.Sprintf("%d", spec.GPU.Count)
		limits["gpu"] = gpuCount
		requests["gpu"] = gpuCount
	}

	if spec.Memory != "" {
		limits["memory"] = spec.Memory
	}

	if spec.CPU != "" {
		limits["cpu"] = spec.CPU
	}

	return map[string]interface{}{
		"limits":   limits,
		"requests": requests,
	}
}

// buildEngineArgs constructs the engine command line arguments (without the engine runner command)
func (t *Transformer) buildEngineArgs(md *airunwayv1alpha1.ModelDeployment) ([]string, error) {
	var args []string

	// SGLang and TRT-LLM expect --model-path while vLLM continues to use --model.
	modelArg := "--model"
	switch md.ResolvedEngineType() {
	case airunwayv1alpha1.EngineTypeSGLang, airunwayv1alpha1.EngineTypeTRTLLM:
		modelArg = "--model-path"
	}
	args = append(args, modelArg, md.Spec.Model.ID)

	// Add served name if specified
	if md.Spec.Model.ServedName != "" {
		args = append(args, "--served-model-name", md.Spec.Model.ServedName)
	}

	// Add context length
	if md.Spec.Engine.ContextLength != nil {
		switch md.ResolvedEngineType() {
		case airunwayv1alpha1.EngineTypeVLLM:
			args = append(args, "--max-model-len", fmt.Sprintf("%d", *md.Spec.Engine.ContextLength))
		case airunwayv1alpha1.EngineTypeSGLang:
			args = append(args, "--context-length", fmt.Sprintf("%d", *md.Spec.Engine.ContextLength))
			// TensorRT-LLM context length is build-time, skip with warning logged elsewhere
		}
	}

	// Add trust remote code
	if md.Spec.Engine.TrustRemoteCode {
		switch md.ResolvedEngineType() {
		case airunwayv1alpha1.EngineTypeVLLM, airunwayv1alpha1.EngineTypeSGLang:
			args = append(args, "--trust-remote-code")
		}
	}

	// Add prefix caching
	if md.Spec.Engine.EnablePrefixCaching {
		switch md.ResolvedEngineType() {
		case airunwayv1alpha1.EngineTypeVLLM, airunwayv1alpha1.EngineTypeSGLang:
			args = append(args, "--enable-prefix-caching")
		}
	}

	// Add enforce eager
	if md.Spec.Engine.EnforceEager {
		switch md.ResolvedEngineType() {
		case airunwayv1alpha1.EngineTypeVLLM, airunwayv1alpha1.EngineTypeSGLang:
			args = append(args, "--enforce-eager")
		}
	}

	// Add custom engine args with key validation (sorted for deterministic output)
	keys := make([]string, 0, len(md.Spec.Engine.Args))
	for k := range md.Spec.Engine.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		// "connector" is consumed internally (e.g. NIXL side-channel host
		// injection) but must not be forwarded to vLLM which rejects the flag.
		if key == "connector" && md.ResolvedEngineType() == airunwayv1alpha1.EngineTypeVLLM {
			continue
		}
		if !isValidArgKey(key) {
			return nil, fmt.Errorf("invalid engine arg key %q: must contain only alphanumeric characters, hyphens, and underscores", key)
		}
		value := md.Spec.Engine.Args[key]
		if value != "" {
			args = append(args, fmt.Sprintf("--%s", key), value)
		} else {
			args = append(args, fmt.Sprintf("--%s", key))
		}
	}

	// Preserve raw token order after deterministic structured args. Disaggregated
	// workers append Dynamo-owned role and KV-transfer flags after this result.
	args = append(args, md.Spec.Engine.ExtraArgs...)

	return args, nil
}

func (t *Transformer) resolvedServingMode(md *airunwayv1alpha1.ModelDeployment) airunwayv1alpha1.ServingMode {
	if md.Spec.Serving != nil && md.Spec.Serving.Mode != "" {
		return md.Spec.Serving.Mode
	}
	return airunwayv1alpha1.ServingModeAggregated
}

// engineCommand returns the command slice for the given engine type
func (t *Transformer) engineCommand(engineType airunwayv1alpha1.EngineType) []string {
	switch engineType {
	case airunwayv1alpha1.EngineTypeVLLM:
		return []string{"python3", "-m", "dynamo.vllm"}
	case airunwayv1alpha1.EngineTypeSGLang:
		return []string{"python3", "-m", "dynamo.sglang"}
	case airunwayv1alpha1.EngineTypeTRTLLM:
		return []string{"python3", "-m", "dynamo.trtllm"}
	default:
		return []string{"python3", "-m", fmt.Sprintf("dynamo.%s", engineType)}
	}
}

// isValidArgKey checks that an arg key contains only alphanumeric chars, hyphens, and underscores,
// and does not start with a hyphen.
func isValidArgKey(key string) bool {
	if len(key) == 0 {
		return false
	}
	// Must not start with a hyphen to prevent option injection
	if key[0] == '-' {
		return false
	}
	for _, r := range key {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// toInterfaceSlice converts a string slice to an interface slice for unstructured construction
func toInterfaceSlice(ss []string) []interface{} {
	result := make([]interface{}, len(ss))
	for i, s := range ss {
		result[i] = s
	}
	return result
}

// defaultImages contains the default container images for each engine type
var defaultImages = map[airunwayv1alpha1.EngineType]string{
	airunwayv1alpha1.EngineTypeVLLM:   defaultVLLMRuntimeImage,
	airunwayv1alpha1.EngineTypeSGLang: defaultSGLangRuntimeImage,
	airunwayv1alpha1.EngineTypeTRTLLM: defaultTRTLLMRuntimeImage,
}

// getImage returns the container image to use
func (t *Transformer) getImage(md *airunwayv1alpha1.ModelDeployment) string {
	// Mocker mode runs python3 -m dynamo.mocker, which lives only in the
	// dynamo-planner image. This is annotation-gated, test-only behavior, so the
	// planner image must win even over an explicit spec.image: a custom image
	// without the dynamo.mocker module would silently break the test backend.
	if isMockerMode(md) {
		return defaultMockerImage
	}

	// Use custom image if specified. spec.engine.image is preferred over the
	// legacy top-level spec.image field.
	if image := md.Spec.ImageOverride(); image != "" {
		return image
	}

	// Use default image for engine type
	if image, ok := defaultImages[md.ResolvedEngineType()]; ok && image != "" {
		return image
	}

	// Fallback to vLLM default
	return defaultVLLMRuntimeImage
}

// buildPVCs creates the pvcs list for DynamoGraphDeployment from StorageSpec volumes.
// Each entry maps to {name: claimName, create: false} since PVCs are either pre-existing
// or created by the controller separately.
func (t *Transformer) buildPVCs(md *airunwayv1alpha1.ModelDeployment) []interface{} {
	if md.Spec.Model.Storage == nil {
		return nil
	}
	var pvcs []interface{}
	for _, vol := range md.Spec.Model.Storage.Volumes {
		pvcs = append(pvcs, map[string]interface{}{
			"name":   vol.ResolvedClaimName(md.Name),
			"create": false,
		})
	}
	return pvcs
}

// buildVolumeMounts creates the volumeMounts list for a DGD worker service.
// Each volume maps to {name: claimName, mountPoint: mountPath} with optional
// useAsCompilationCache: true for compilationCache purpose.
func (t *Transformer) buildVolumeMounts(md *airunwayv1alpha1.ModelDeployment) []interface{} {
	if md.Spec.Model.Storage == nil {
		return nil
	}
	var mounts []interface{}
	for _, vol := range md.Spec.Model.Storage.Volumes {
		mount := map[string]interface{}{
			"name":       vol.ResolvedClaimName(md.Name),
			"mountPoint": vol.MountPath,
		}
		if vol.Purpose == airunwayv1alpha1.VolumePurposeCompilationCache {
			mount["useAsCompilationCache"] = true
		}
		if vol.ReadOnly {
			mount["readOnly"] = true
		}
		mounts = append(mounts, mount)
	}
	return mounts
}

// addStorageConfig adds volumeMounts and HF_HOME env var injection to a worker service map.
// This should be called for worker services (aggregated, prefill, decode) but NOT the frontend.
func (t *Transformer) addStorageConfig(worker map[string]interface{}, md *airunwayv1alpha1.ModelDeployment) {
	if md.Spec.Model.Storage == nil || len(md.Spec.Model.Storage.Volumes) == 0 {
		return
	}

	// Add volumeMounts to the service
	volumeMounts := t.buildVolumeMounts(md)
	if len(volumeMounts) > 0 {
		worker["volumeMounts"] = volumeMounts
	}

	// Auto-inject HF_HOME for modelCache volumes
	for _, vol := range md.Spec.Model.Storage.Volumes {
		if vol.Purpose == airunwayv1alpha1.VolumePurposeModelCache {
			// Check if user already set HF_HOME in spec.env
			if !hasEnvVar(md, "HF_HOME") {
				t.injectEnvVar(worker, "HF_HOME", vol.MountPath)
			}
			break
		}
	}
}

// hasEnvVar checks if the ModelDeployment has a specific environment variable set
func hasEnvVar(md *airunwayv1alpha1.ModelDeployment, name string) bool {
	for _, env := range md.Spec.Env {
		if env.Name == name {
			return true
		}
	}
	return false
}

// maybeInjectVLLMSideChannelHost ensures each NIXL-backed vLLM worker advertises its own pod IP.
// Injection is gated on disaggregated vLLM serving, which always uses NIXL for KV-cache transfer.
func (t *Transformer) maybeInjectVLLMSideChannelHost(service map[string]interface{}, md *airunwayv1alpha1.ModelDeployment) {
	if md.ResolvedEngineType() != airunwayv1alpha1.EngineTypeVLLM ||
		t.resolvedServingMode(md) != airunwayv1alpha1.ServingModeDisaggregated {
		return
	}

	t.injectEnvVarFromFieldRef(service, "VLLM_NIXL_SIDE_CHANNEL_HOST", "status.podIP")
}

// injectEnvVar adds an environment variable to the mainContainer's env list in extraPodSpec
func (t *Transformer) injectEnvVar(service map[string]interface{}, name, value string) {
	extraPodSpec, ok := service["extraPodSpec"].(map[string]interface{})
	if !ok {
		extraPodSpec = map[string]interface{}{}
		service["extraPodSpec"] = extraPodSpec
	}

	mainContainer, ok := extraPodSpec["mainContainer"].(map[string]interface{})
	if !ok {
		mainContainer = map[string]interface{}{}
		extraPodSpec["mainContainer"] = mainContainer
	}

	envList, _ := mainContainer["env"].([]interface{})
	envList = append(envList, map[string]interface{}{
		"name":  name,
		"value": value,
	})
	mainContainer["env"] = envList
}

// injectEnvVarFromFieldRef adds an environment variable sourced from a pod field.
func (t *Transformer) injectEnvVarFromFieldRef(service map[string]interface{}, name, fieldPath string) {
	extraPodSpec, ok := service["extraPodSpec"].(map[string]interface{})
	if !ok {
		extraPodSpec = map[string]interface{}{}
		service["extraPodSpec"] = extraPodSpec
	}

	mainContainer, ok := extraPodSpec["mainContainer"].(map[string]interface{})
	if !ok {
		mainContainer = map[string]interface{}{}
		extraPodSpec["mainContainer"] = mainContainer
	}

	envList, _ := mainContainer["env"].([]interface{})
	envList = append(envList, map[string]interface{}{
		"name": name,
		"valueFrom": map[string]interface{}{
			"fieldRef": map[string]interface{}{
				"fieldPath": fieldPath,
			},
		},
	})
	mainContainer["env"] = envList
}

// addSchedulingConfig adds node selector and tolerations to a service
func (t *Transformer) addSchedulingConfig(service map[string]interface{}, md *airunwayv1alpha1.ModelDeployment) {
	extraPodSpec, ok := service["extraPodSpec"].(map[string]interface{})
	if !ok {
		extraPodSpec = map[string]interface{}{}
		service["extraPodSpec"] = extraPodSpec
	}

	if len(md.Spec.NodeSelector) > 0 {
		ns := make(map[string]interface{}, len(md.Spec.NodeSelector))
		for k, v := range md.Spec.NodeSelector {
			ns[k] = v
		}
		extraPodSpec["nodeSelector"] = ns
	}

	if len(md.Spec.Tolerations) > 0 {
		tolerations := make([]interface{}, len(md.Spec.Tolerations))
		for i, t := range md.Spec.Tolerations {
			toleration := map[string]interface{}{
				"key":      t.Key,
				"operator": string(t.Operator),
			}
			if t.Value != "" {
				toleration["value"] = t.Value
			}
			if t.Effect != "" {
				toleration["effect"] = string(t.Effect)
			}
			if t.TolerationSeconds != nil {
				toleration["tolerationSeconds"] = *t.TolerationSeconds
			}
			tolerations[i] = toleration
		}
		extraPodSpec["tolerations"] = tolerations
	}
}

// sanitizeLabelValue ensures a value is valid for a Kubernetes label
func sanitizeLabelValue(value string) string {
	// Labels must be 63 chars or less, start and end with alphanumeric
	if len(value) > 63 {
		value = value[:63]
	}
	// Replace invalid characters with dashes
	value = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, value)
	// Trim leading/trailing dashes
	value = strings.Trim(value, "-_.")
	return value
}

// boolPtr returns a pointer to a bool
func boolPtr(b bool) *bool {
	return &b
}

// applyOverrides deep-merges spec.provider.overrides into the unstructured object.
// This is the escape hatch that lets users set arbitrary fields on the provider CRD.
func applyOverrides(obj *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) error {
	if md.Spec.Provider == nil || md.Spec.Provider.Overrides == nil {
		return nil
	}

	var overrides map[string]interface{}
	if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &overrides); err != nil {
		return fmt.Errorf("failed to unmarshal overrides: %w", err)
	}

	// Only "spec" may be merged into the object, plus the keys parseOverrides has already
	// consumed into DynamoOverrides (routerMode, frontend, epp), which are inputs to this
	// transformer rather than DynamoGraphDeployment fields. A DGD root declares
	// apiVersion/kind/metadata/spec/status, so anything else is a field no API server will
	// store.
	//
	// Unsupported keys are REJECTED rather than dropped. Silently ignoring them would
	// reproduce exactly the failure strict validation exists to surface: an override visible in the
	// ModelDeployment, absent from the rendered object, and a deployment reporting healthy.
	//
	// The consumed-key comparison is case-insensitive because encoding/json matches field
	// names that way — parseOverrides decodes {"RouterMode": ...} into the typed struct just
	// as happily as the documented spelling, so a case-sensitive check would let it through
	// to the root. The "spec" comparison is deliberately case-SENSITIVE: "Spec" is not a
	// field of any upstream CRD, so it must be rejected rather than merged.
	// Collected and sorted rather than returning on the first offender: Go randomises map
	// iteration, so returning early would produce a different message on each call for the
	// same spec. That message becomes status.message, and since the ModelDeployment watch
	// has no GenerationChangedPredicate, a changing message means every reconcile writes
	// status, which re-enqueues the object — an unbounded write loop for as long as the
	// bad spec exists.
	if err := validateDynamoOverrideRootKeys(overrides); err != nil {
		return err
	}

	for key := range overrides {
		if key == "spec" {
			continue
		}
		if consumedOverrideKeys[strings.ToLower(key)] {
			delete(overrides, key)
		}
	}

	obj.Object = deepMerge(obj.Object, overrides)
	return nil
}

// consumedOverrideKeys are the provider.overrides root keys parseOverrides decodes into
// DynamoOverrides. Lowercased, because encoding/json matches field names case-insensitively.
var consumedOverrideKeys = map[string]bool{
	"deploymentmode": true,
	"searchstrategy": true,
	"autoapply":      true,
	"plannerimage":   true,
	"routermode":     true,
	"frontend":       true,
	"epp":            true,
}

func validateDynamoOverrideRootKeys(overrides map[string]interface{}) error {
	// Block dangerous top-level keys to prevent privilege escalation.
	blockedKeys := []string{"apiVersion", "kind", "metadata", "status"}
	for _, key := range blockedKeys {
		if _, exists := overrides[key]; exists {
			return fmt.Errorf("overriding %q is not allowed", key)
		}
	}

	var unsupported []string
	for key := range overrides {
		if key == "spec" || consumedOverrideKeys[strings.ToLower(key)] {
			continue
		}
		unsupported = append(unsupported, key)
	}
	if len(unsupported) == 0 {
		return nil
	}

	sort.Strings(unsupported)
	return fmt.Errorf("unsupported provider.overrides key(s) %q: supported keys are \"spec\", "+
		"deploymentMode, searchStrategy, autoApply, plannerImage, routerMode, frontend, and epp", unsupported)
}

// deepMerge recursively merges src into dst. dst is modified in place and also
// returned for convenience. For maps, values are merged recursively. For all
// other types, src overwrites dst.
func deepMerge(dst, src map[string]interface{}) map[string]interface{} {
	for key, srcVal := range src {
		if dstVal, exists := dst[key]; exists {
			srcMap, srcOk := srcVal.(map[string]interface{})
			dstMap, dstOk := dstVal.(map[string]interface{})
			if srcOk && dstOk {
				dst[key] = deepMerge(dstMap, srcMap)
				continue
			}
		}
		dst[key] = srcVal
	}
	return dst
}
