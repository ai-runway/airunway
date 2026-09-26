package dynamo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"sort"
	"strings"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	dynamoBetaVersion         = "v1beta1"
	manualInputHashAnnotation = "airunway.ai/dynamo-input-hash"
	runtimeVersionAnnotation  = "airunway.ai/dynamo-runtime-version"
)

func resourceReference(u *unstructured.Unstructured) *api.ProviderResourceReference {
	return &api.ProviderResourceReference{APIVersion: u.GetAPIVersion(), Kind: u.GetKind(), Name: u.GetName(), Namespace: u.GetNamespace(), UID: string(u.GetUID())}
}

func referenceResource(ref *api.ProviderResourceReference) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(ref.APIVersion)
	u.SetKind(ref.Kind)
	u.SetName(ref.Name)
	u.SetNamespace(ref.Namespace)
	return u
}

// DGD versions are alternate representations of one Kubernetes object, not separate workloads.
func (r *DynamoProviderReconciler) findDGD(ctx context.Context, namespace, name string, ref *api.ProviderResourceReference) (*unstructured.Unstructured, error) {
	versions := []string{DynamoAPIVersion, dynamoBetaVersion}
	if ref != nil && ref.Name == name && ref.Namespace == namespace {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != DynamoAPIGroup || ref.Kind != DynamoGraphDeploymentKind {
			return nil, fmt.Errorf("invalid Dynamo workload reference")
		}
		versions = []string{gv.Version, DynamoAPIVersion, dynamoBetaVersion}
	}
	served := false
	seen := map[string]bool{}
	for _, version := range versions {
		if seen[version] {
			continue
		}
		seen[version] = true
		u := newDynamoResource(version, DynamoGraphDeploymentKind, name, namespace)
		if err := r.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
			if upstreamResourceUnavailable(err) {
				if !meta.IsNoMatchError(err) {
					served = true
				}
				continue
			}
			return nil, err
		}
		if ref != nil && ref.UID != "" && ref.UID != string(u.GetUID()) {
			return nil, &resourceConflictError{namespace: namespace, name: name}
		}
		return u, nil
	}
	if !served && ref != nil {
		return nil, fmt.Errorf("no served API for recorded Dynamo workload %s", ref.Name)
	}
	return nil, nil
}

func supportedVersion(mapper meta.RESTMapper, kind, version string) (bool, error) {
	_, err := mapper.RESTMapping(schema.GroupKind{Group: DynamoAPIGroup, Kind: kind}, version)
	if meta.IsNoMatchError(err) {
		return false, nil
	}
	return err == nil, err
}

func (r *DynamoProviderReconciler) renderResources(ctx context.Context, md *api.ModelDeployment) ([]*unstructured.Unstructured, error) {
	version := DynamoAPIVersion
	runtimeVersion := ""
	if ok, err := supportedVersion(r.RESTMapper(), DynamoGraphDeploymentKind, dynamoBetaVersion); err != nil {
		return nil, err
	} else if ok {
		version = dynamoBetaVersion
	}
	if dynamointent.Enabled(md) {
		request, err := r.findRequest(ctx, md)
		if err != nil {
			return nil, err
		}
		if request != nil {
			hash, err := dynamointent.Fingerprint(md)
			if err != nil {
				return nil, err
			}
			if request.GetAnnotations()[dynamointent.HashAnnotation] == hash && request.GetAnnotations()[dynamointent.AttemptAnnotation] == md.Annotations[dynamointent.AttemptAnnotation] {
				return []*unstructured.Unstructured{request.DeepCopy()}, nil
			}
			// Legacy requests inherit the operator's image rather than setting one.
			// Comparing/updating their common DGDR fields needs no runtime guess.
			if typed, err := dynamointent.Parse(md); err != nil {
				return nil, err
			} else if typed == nil {
				return r.Transformer.Transform(ctx, md)
			}
			image, _, _ := unstructured.NestedString(request.Object, "spec", "image")
			runtimeVersion = semanticImageTag(image)
		}
	}
	var existing *unstructured.Unstructured
	if !dynamointent.Enabled(md) {
		var ref *api.ProviderResourceReference
		if md.Status.Provider != nil && md.Status.Provider.WorkloadRef != nil && md.Status.Provider.RequestRef == nil {
			ref = md.Status.Provider.WorkloadRef
		}
		name := md.Name
		if ref != nil {
			name = ref.Name
		}
		var err error
		existing, err = r.findDGD(ctx, md.Namespace, name, ref)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if err := verifyDynamoOwnership(existing, md.UID); err != nil {
				return nil, err
			}
			version = existing.GroupVersionKind().Version
			runtimeVersion = existingRuntimeVersion(existing)
			if existing.GetAnnotations()[manualInputHashAnnotation] == manualFingerprint(md) {
				return []*unstructured.Unstructured{existing.DeepCopy()}, nil
			}
		}
	}
	if runtimeVersion == "" {
		var err error
		runtimeVersion, err = r.discoverRuntimeVersion(ctx, md.Namespace)
		if err != nil {
			return nil, &runtimeDiscoveryError{err}
		}
	}
	// Namespace-restricted 1.1 operators may share CRDs with a newer cluster-wide
	// operator. Select an API it can render without inferring the version from it.
	if parsed, err := utilversion.ParseSemantic(runtimeVersion); err == nil && existing == nil && parsed.LessThan(utilversion.MustParseSemantic("1.5.0")) {
		version = DynamoAPIVersion
	}
	resources, err := r.Transformer.TransformForVersion(ctx, md, version, runtimeVersion)
	if err != nil {
		return nil, err
	}
	for _, resource := range resources {
		if resource.GetKind() != DynamoGraphDeploymentKind {
			continue
		}
		annotations := resource.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[runtimeVersionAnnotation] = runtimeVersion
		resource.SetAnnotations(annotations)
	}
	if existing != nil && len(resources) > 0 {
		resources[0].SetName(existing.GetName())
		preserveRuntimeImages(md, existing, resources[0])
	}
	return resources, nil
}

func existingRuntimeVersion(u *unstructured.Unstructured) string {
	if v := u.GetAnnotations()[runtimeVersionAnnotation]; v != "" {
		if _, err := utilversion.ParseSemantic(v); err == nil {
			return v
		}
	}
	images := []string{}
	collectImages(u.Object["spec"], &images)
	sort.Strings(images)
	// Backend runtime images are authoritative ahead of separate frontend/EPP images.
	for _, backendOnly := range []bool{true, false} {
		for _, image := range images {
			if !strings.HasPrefix(image, "nvcr.io/nvidia/ai-dynamo/") || backendOnly && !strings.Contains(image, "-runtime:") {
				continue
			}
			if tag := semanticImageTag(image); tag != "" {
				return tag
			}
		}
	}
	return ""
}

func collectImages(value any, images *[]string) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "image" {
				if image, ok := child.(string); ok {
					*images = append(*images, image)
				}
			} else {
				collectImages(child, images)
			}
		}
	case []any:
		for _, child := range v {
			collectImages(child, images)
		}
	}
}

func manualFingerprint(md *api.ModelDeployment) string {
	raw, _ := json.Marshal(struct {
		Spec   api.ModelDeploymentSpec
		Engine api.EngineType
	}{md.Spec, md.ResolvedEngineType()})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// preserveRuntimeImages retains observed defaults even after the provider's image
// defaults change. Explicit user images continue to be rendered normally.
func preserveRuntimeImages(md *api.ModelDeployment, existing, desired *unstructured.Unstructured) {
	if md.Spec.Image != "" || md.Spec.Engine.Image != "" {
		return
	}
	var overrides map[string]any
	if md.Spec.Provider != nil && md.Spec.Provider.Overrides != nil {
		_ = json.Unmarshal(md.Spec.Provider.Overrides.Raw, &overrides)
	}
	explicit := dynamoComponents(overrides, "spec")
	old := dynamoComponents(existing.Object, "spec")
	next := dynamoComponents(desired.Object, "spec")
	for name, component := range next {
		images := []string{}
		collectImages(explicit[name], &images)
		if len(images) > 0 {
			continue
		}
		preserveImageFields(old[name], component)
	}
	if components, found, _ := unstructured.NestedSlice(desired.Object, "spec", "components"); found {
		for i, raw := range components {
			component, _ := raw.(map[string]any)
			name, _ := component["name"].(string)
			if replacement, ok := next[name]; ok {
				components[i] = replacement
			}
		}
		_ = unstructured.SetNestedSlice(desired.Object, components, "spec", "components")
	} else {
		key := "services"
		if _, found, _ := unstructured.NestedMap(desired.Object, "spec", "components"); found {
			key = "components"
		}
		_ = unstructured.SetNestedMap(desired.Object, next, "spec", key)
	}
}

func preserveImageFields(existing, desired any) {
	switch next := desired.(type) {
	case map[string]any:
		old, ok := existing.(map[string]any)
		if !ok {
			return
		}
		for key, child := range next {
			if key == "image" {
				if image, ok := old[key].(string); ok {
					next[key] = image
				}
			} else {
				preserveImageFields(old[key], child)
			}
		}
	case []any:
		old, _ := existing.([]any)
		for _, raw := range next {
			component, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, ok := component["name"].(string)
			if !ok {
				continue
			}
			for _, prior := range old {
				p, ok := prior.(map[string]any)
				if ok && p["name"] == name {
					preserveImageFields(p, component)
					break
				}
			}
		}
	}
}

// Alpha specs and both status versions are maps. Native beta specs use a
// name-keyed list. Return a uniform view without changing the stored shape.
func dynamoComponents(object map[string]any, section string) map[string]any {
	if m, found, _ := unstructured.NestedMap(object, section, "components"); found {
		return m
	}
	if entries, found, _ := unstructured.NestedSlice(object, section, "components"); found {
		m := map[string]any{}
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, ok := entry["name"].(string)
			if ok {
				m[name] = entry
			}
		}
		return m
	}
	m, _, _ := unstructured.NestedMap(object, section, "services")
	return m
}
