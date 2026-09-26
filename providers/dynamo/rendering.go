package dynamo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/version"
)

// TransformForVersion renders against the selected installed API and runtime.
// apiVersion may be qualified (nvidia.com/v1beta1) or just v1beta1. Discovery
// and preserving an existing deployment's runtime are the caller's concern.
func (t *Transformer) TransformForVersion(ctx context.Context, md *api.ModelDeployment, apiVersion, runtimeVersion string) ([]*unstructured.Unstructured, error) {
	apiVersion = strings.TrimPrefix(apiVersion, DynamoAPIGroup+"/")
	if apiVersion != "v1alpha1" && apiVersion != "v1beta1" {
		return nil, fmt.Errorf("unsupported Dynamo API version %q", apiVersion)
	}
	if runtimeVersion == "" {
		return nil, fmt.Errorf("Dynamo runtime version is required for versioned rendering")
	}
	parsed, err := version.ParseSemantic(runtimeVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid Dynamo runtime version: %w", err)
	}
	render := *t
	render.runtimeVersion = strings.TrimPrefix(runtimeVersion, "v")
	render.nativeBeta = apiVersion == "v1beta1"
	render.modernRuntime = !parsed.LessThan(version.MustParseSemantic("1.5.0"))
	overrides, err := render.parseOverrides(md)
	if err != nil {
		return nil, err
	}
	if overrides.DeploymentMode == DeploymentModeIntent {
		// DGDR is beta on both releases, independently of the DGD schema.
		return render.transformIntent(md, overrides)
	}
	if apiVersion == "v1beta1" && parsed.LessThan(version.MustParseSemantic("1.5.0")) {
		return nil, fmt.Errorf("native beta DGD rendering requires Dynamo runtime 1.5.0 or newer")
	}
	if apiVersion == "v1alpha1" {
		if raw, err := rawDGDSpec(md); err != nil {
			return nil, err
		} else if _, beta := raw["components"]; beta {
			return nil, fmt.Errorf("spec.components overrides require the v1beta1 DGD API; use spec.services on v1alpha1")
		}
		return render.transformAlpha(ctx, md)
	}
	raw, err := rawDGDSpec(md)
	if err != nil {
		return nil, err
	}
	_, nativeOverrides := raw["components"]
	if _, hasNativeEnv := raw["env"]; hasNativeEnv {
		nativeOverrides = true
	}
	if nativeOverrides {
		if _, legacy := raw["services"]; legacy {
			return nil, fmt.Errorf("cannot mix spec.services and spec.components overrides")
		}
		for _, key := range []string{"pvcs", "envs"} {
			if _, legacy := raw[key]; legacy {
				return nil, fmt.Errorf("cannot mix legacy spec.%s with native spec.components", key)
			}
		}
	}
	input := md
	if nativeOverrides {
		input = md.DeepCopy()
		var root map[string]json.RawMessage
		if err := json.Unmarshal(input.Spec.Provider.Overrides.Raw, &root); err != nil {
			return nil, err
		}
		delete(root, "spec")
		data, err := json.Marshal(root)
		if err != nil {
			return nil, err
		}
		input.Spec.Provider.Overrides = &runtime.RawExtension{Raw: data}
	}
	resources, err := render.transformAlpha(ctx, input)
	if err != nil {
		return nil, err
	}
	for _, resource := range resources {
		if resource.GetKind() != DynamoGraphDeploymentKind {
			return nil, fmt.Errorf("expected a direct DynamoGraphDeployment")
		}
		spec, _, _ := unstructured.NestedMap(resource.Object, "spec")
		spec, err = alphaSpecToBeta(spec)
		if err != nil {
			return nil, fmt.Errorf("cannot render legacy DGD inputs as beta: %w; use native spec.components overrides", err)
		}
		if nativeOverrides {
			spec, err = mergeBetaSpec(spec, raw)
			if err != nil {
				return nil, err
			}
		}
		if err := validateBetaRuntimeContracts(spec, render.runtimeVersion); err != nil {
			return nil, err
		}
		resource.Object["spec"] = spec
		resource.SetAPIVersion(DynamoAPIGroup + "/v1beta1")
	}
	return resources, nil
}

func (t *Transformer) runtimeImage(repository, fallback string) string {
	if t.runtimeVersion == "" {
		return fallback
	}
	return "nvcr.io/nvidia/ai-dynamo/" + repository + ":" + t.runtimeVersion
}

func rawDGDSpec(md *api.ModelDeployment) (map[string]any, error) {
	if md.Spec.Provider == nil || md.Spec.Provider.Overrides == nil {
		return nil, nil
	}
	var root map[string]any
	if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &root); err != nil {
		return nil, err
	}
	value, exists := root["spec"]
	if !exists {
		return nil, nil
	}
	spec, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("provider.overrides.spec must be an object")
	}
	return spec, nil
}

func (t *Transformer) applyTypedIntent(spec map[string]any, md *api.ModelDeployment, intent *dynamointent.Spec) error {
	// Do not guess generated component names or a revision-specific HF snapshot
	// path. These settings require an explicit topology and remain manual-only.
	if len(md.Spec.Env) > 0 || len(md.Spec.NodeSelector) > 0 || len(md.Spec.Tolerations) > 0 {
		return fmt.Errorf("custom env, nodeSelector and tolerations require manual configuration")
	}
	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" && md.Spec.Secrets.HuggingFaceToken != "hf-token-secret" {
		return fmt.Errorf("automatic configuration uses the upstream hf-token-secret; custom secret names require manual configuration")
	}
	if md.Spec.Model.Storage != nil && len(md.Spec.Model.Storage.Volumes) > 0 {
		return fmt.Errorf("automatic configuration does not support storage volumes: the downloaded HF snapshot path is not known when rendering; use manual configuration or an explicit legacy modelCache.pvcModelPath")
	}
	if md.Spec.PodTemplate != nil && md.Spec.PodTemplate.Metadata != nil && (len(md.Spec.PodTemplate.Metadata.Labels) > 0 || len(md.Spec.PodTemplate.Metadata.Annotations) > 0) {
		return fmt.Errorf("custom pod metadata requires manual configuration")
	}
	// enablePrefixCaching is defaulted to true by the ModelDeployment CRD.
	// It is not a topology override and must not reject otherwise valid intent.
	if isMockerMode(md) {
		return fmt.Errorf("mocker settings require manual configuration")
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key, value := range fields {
		spec[key] = value
	}
	spec["searchStrategy"] = "rapid"
	spec["autoApply"] = true
	// Both releases derive the generated backend image from the profiler image.
	// Pin it to the chosen runtime rather than inheriting an operator default.
	if t.runtimeVersion != "" {
		spec["image"] = t.runtimeImage("dynamo-planner", "")
	}
	return nil
}

// flagValue follows the backend's last-value-wins CLI convention.
func flagValue(args []string, flag string) string {
	value := ""
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			value = args[i+1]
		}
		if strings.HasPrefix(arg, flag+"=") {
			value = strings.TrimPrefix(arg, flag+"=")
		}
	}
	return value
}
func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

// The CRD does not express Dynamo's webhook runtime checks. Keep the EPP
// contract explicit, including custom image tags whose runtime cannot be read
// from the tag. The caller-selected runtime is the default only for those tags;
// semver image pins and explicit per-component overrides stay authoritative.
func validateBetaRuntimeContracts(spec map[string]any, selectedRuntime string) error {
	components, _ := spec["components"].([]any)
	for _, raw := range components {
		c, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("component must be an object")
		}
		explicit, hasOverride := c["runtimeVersionOverride"]
		runtimeVersion := ""
		if hasOverride {
			var ok bool
			runtimeVersion, ok = explicit.(string)
			if !ok || runtimeVersion == "" {
				return fmt.Errorf("component %v runtimeVersionOverride must be a semantic version", c["name"])
			}
		} else {
			containers, _, err := unstructured.NestedSlice(c, "podTemplate", "spec", "containers")
			if err != nil {
				return err
			}
			for _, raw := range containers {
				container, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("component %v has an invalid container", c["name"])
				}
				if container["name"] != "main" {
					continue
				}
				image, _ := container["image"].(string)
				image = strings.SplitN(image, "@", 2)[0]
				tag := image[strings.LastIndex(image, ":")+1:]
				if _, err := version.ParseSemantic(tag); err == nil {
					runtimeVersion = strings.TrimPrefix(tag, "v")
				}
			}
			if runtimeVersion == "" {
				runtimeVersion = selectedRuntime
				c["runtimeVersionOverride"] = runtimeVersion
			}
		}
		v, err := version.ParseSemantic(runtimeVersion)
		if err != nil {
			return fmt.Errorf("component %v has invalid runtimeVersionOverride: %w", c["name"], err)
		}
		if c["type"] == "epp" {
			_, legacy := c["eppConfig"]
			modern := !v.LessThan(version.MustParseSemantic("1.5.0")) || v.Major() == 1 && v.Minor() == 5
			if legacy && modern {
				return fmt.Errorf("component %v: eppConfig requires a legacy Go EPP runtime image before 1.5; remove eppConfig for native Rust EPP", c["name"])
			}
			if !legacy && !modern {
				return fmt.Errorf("component %v: EPP runtime %s requires legacy eppConfig", c["name"], runtimeVersion)
			}
		}
	}
	return nil
}
