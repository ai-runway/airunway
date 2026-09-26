package dynamo

import (
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// alphaSpecToBeta converts the alpha subset rendered by Runway, including
// representable legacy overrides. Unsupported alpha-only semantics are errors,
// not annotations which merely preserve data without affecting beta workloads.
func alphaSpecToBeta(src map[string]any) (map[string]any, error) {
	if err := renderingKeys(src, "spec", "backendFramework", "annotations", "labels", "priorityClassName", "restart", "topologyConstraint", "experimental", "services", "envs", "pvcs"); err != nil {
		return nil, err
	}
	dst := map[string]any{}
	for k, v := range src {
		if k != "services" && k != "envs" && k != "pvcs" {
			dst[k] = v
		}
	}
	if v, ok := src["envs"]; ok {
		dst["env"] = v
	}
	// Native beta PVC references are ordinary Pod volumes. Runway creates its
	// PVCs separately. Operator-created alpha PVCs have no equivalent here.
	pvcs := map[string]bool{}
	if raw, ok := src["pvcs"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("spec.pvcs must be an array")
		}
		for _, v := range list {
			pvc, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid spec.pvcs entry")
			}
			if err := renderingKeys(pvc, "spec.pvcs[]", "name", "create"); err != nil {
				return nil, err
			}
			name, ok := pvc["name"].(string)
			if !ok || name == "" {
				return nil, fmt.Errorf("PVC name is required")
			}
			if create, exists := pvc["create"]; !exists || create != false {
				return nil, fmt.Errorf("PVC %q must be pre-created (create: false) for beta rendering", name)
			}
			pvcs[name] = true
		}
	}
	services, ok := src["services"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("spec.services must be an object")
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	components := make([]any, 0, len(names))
	for _, name := range names {
		service, ok := services[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("service %q must be an object", name)
		}
		component, err := alphaComponentToBeta(name, service, pvcs)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
		components = append(components, component)
	}
	dst["components"] = components
	return dst, nil
}

func alphaComponentToBeta(name string, src map[string]any, pvcs map[string]bool) (map[string]any, error) {
	if err := renderingKeys(src, "service", "componentType", "subComponentType", "serviceName", "replicas", "minAvailable", "globalDynamoNamespace", "runtimeVersionOverride", "modelRef", "topologyConstraint", "eppConfig", "resources", "extraPodSpec", "extraPodMetadata", "envs", "envFromSecret", "volumeMounts", "sharedMemory", "frontendSidecar", "livenessProbe", "readinessProbe", "scalingAdapter", "multinode"); err != nil {
		return nil, err
	}
	dst := map[string]any{"name": name}
	for _, key := range []string{"replicas", "minAvailable", "globalDynamoNamespace", "runtimeVersionOverride", "modelRef", "topologyConstraint", "eppConfig", "multinode"} {
		if v, ok := src[key]; ok {
			dst[key] = v
		}
	}
	if v, ok := src["serviceName"]; ok && v != name {
		return nil, fmt.Errorf("serviceName must match its services-map key")
	}
	typ, ok := src["componentType"].(string)
	if !ok || typ == "" {
		return nil, fmt.Errorf("componentType is required")
	}
	if sub, ok := src["subComponentType"]; ok && sub != "" {
		if typ != "worker" || sub != "prefill" && sub != "decode" {
			return nil, fmt.Errorf("unsupported subComponentType %v", sub)
		}
		typ = sub.(string)
	}
	dst["type"] = typ
	if raw, ok := src["sharedMemory"]; ok {
		shared, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("sharedMemory must be an object")
		}
		if err := renderingKeys(shared, "sharedMemory", "size", "disabled"); err != nil {
			return nil, err
		}
		if shared["disabled"] == true {
			dst["sharedMemorySize"] = "0"
		} else if size, ok := shared["size"]; ok {
			dst["sharedMemorySize"] = size
		}
	}
	if raw, ok := src["scalingAdapter"]; ok {
		adapter, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("scalingAdapter must be an object")
		}
		if adapter["enabled"] != true {
			return nil, fmt.Errorf("disabled scalingAdapter has no beta equivalent; omit it")
		}
		adapter = copyRenderingMap(adapter)
		delete(adapter, "enabled")
		dst["scalingAdapter"] = adapter
	}
	pod := map[string]any{}
	main := map[string]any{"name": "main"}
	if raw, ok := src["resources"]; ok {
		r, err := alphaResourcesToBeta(raw)
		if err != nil {
			return nil, err
		}
		main["resources"] = r
	}
	for _, key := range []string{"livenessProbe", "readinessProbe"} {
		if v, ok := src[key]; ok {
			main[key] = v
		}
	}
	if env, ok := src["envs"]; ok {
		main["env"] = env
	}
	if secret, ok := src["envFromSecret"]; ok {
		main["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": secret}}}
	}
	if raw, ok := src["extraPodSpec"]; ok {
		extra, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("extraPodSpec must be an object")
		}
		for k, v := range extra {
			if k != "mainContainer" {
				pod[k] = v
			}
		}
		if raw, exists := extra["mainContainer"]; exists {
			override, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("mainContainer must be an object")
			}
			if n, ok := override["name"]; ok && n != "main" && n != "" {
				return nil, fmt.Errorf("mainContainer.name must be main")
			}
			// Kubernetes strategic merge preserves named env entries and resource maps.
			merged, err := mergePodMaps(map[string]any{"spec": map[string]any{"containers": []any{main}}}, map[string]any{"spec": map[string]any{"containers": []any{deepMerge(copyRenderingMap(override), map[string]any{"name": "main"})}}})
			if err != nil {
				return nil, err
			}
			main = merged["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		}
	}
	containers := []any{main}
	if raw, exists := pod["containers"]; exists {
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("extraPodSpec.containers must be an array")
		}
		for _, v := range list {
			c, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid sidecar container")
			}
			if c["name"] == "main" {
				return nil, fmt.Errorf("configure main through extraPodSpec.mainContainer, not containers")
			}
			containers = append(containers, c)
		}
	}
	if raw, exists := src["volumeMounts"]; exists {
		mounts, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("volumeMounts must be an array")
		}
		for _, raw := range mounts {
			mount, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid volumeMount")
			}
			if err := renderingKeys(mount, "volumeMount", "name", "mountPoint", "readOnly", "useAsCompilationCache"); err != nil {
				return nil, err
			}
			volume, ok := mount["name"].(string)
			if !ok || !pvcs[volume] {
				return nil, fmt.Errorf("volumeMount %v must reference a declared pre-created PVC", mount["name"])
			}
			native := map[string]any{"name": volume, "mountPath": mount["mountPoint"]}
			if ro, ok := mount["readOnly"]; ok {
				native["readOnly"] = ro
			}
			current, _ := main["volumeMounts"].([]any)
			main["volumeMounts"] = append(current, native)
			volumes, _ := pod["volumes"].([]any)
			found := false
			for _, v := range volumes {
				if m, ok := v.(map[string]any); ok && m["name"] == volume {
					found = true
					claim, ok := m["persistentVolumeClaim"].(map[string]any)
					if !ok || claim["claimName"] != volume {
						return nil, fmt.Errorf("PVC volume %q conflicts with extraPodSpec.volumes", volume)
					}
				}
			}
			if !found {
				pod["volumes"] = append(volumes, map[string]any{"name": volume, "persistentVolumeClaim": map[string]any{"claimName": volume}})
			}
			if mount["useAsCompilationCache"] == true {
				if _, exists := dst["compilationCache"]; exists {
					return nil, fmt.Errorf("beta supports only one compilation cache")
				}
				dst["compilationCache"] = map[string]any{"pvcName": volume, "mountPath": mount["mountPoint"]}
			}
		}
	}
	if raw, exists := src["frontendSidecar"]; exists {
		side, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("frontendSidecar must be an object")
		}
		if err := renderingKeys(side, "frontendSidecar", "image", "args", "envs", "envFromSecret"); err != nil {
			return nil, err
		}
		c := map[string]any{"name": "sidecar-frontend"}
		for _, key := range []string{"image", "args"} {
			if v, ok := side[key]; ok {
				c[key] = v
			}
		}
		if v, ok := side["envs"]; ok {
			c["env"] = v
		}
		if v, ok := side["envFromSecret"]; ok {
			c["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": v}}}
		}
		for _, v := range containers {
			if v.(map[string]any)["name"] == "sidecar-frontend" {
				return nil, fmt.Errorf("sidecar-frontend container name is reserved")
			}
		}
		containers = append(containers, c)
		dst["frontendSidecar"] = "sidecar-frontend"
	}
	pod["containers"] = containers
	template := map[string]any{"spec": pod}
	if v, ok := src["extraPodMetadata"]; ok {
		template["metadata"] = v
	}
	dst["podTemplate"] = template
	return dst, nil
}

func alphaResourcesToBeta(raw any) (map[string]any, error) {
	src, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resources must be an object")
	}
	if err := renderingKeys(src, "resources", "requests", "limits", "claims"); err != nil {
		return nil, err
	}
	dst := map[string]any{}
	if claims, ok := src["claims"]; ok {
		dst["claims"] = claims
	}
	for _, key := range []string{"limits", "requests"} {
		raw, exists := src[key]
		if !exists {
			continue
		}
		r, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resources.%s must be an object", key)
		}
		if err := renderingKeys(r, "resources."+key, "gpu", "gpuType", "cpu", "memory", "custom"); err != nil {
			return nil, err
		}
		native := map[string]any{}
		if custom, exists := r["custom"]; exists {
			m, ok := custom.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("resources.custom must be an object")
			}
			native = copyRenderingMap(m)
		}
		for _, k := range []string{"cpu", "memory"} {
			if v, ok := r[k]; ok {
				native[k] = v
			}
		}
		if v, ok := r["gpu"]; ok {
			name := "nvidia.com/gpu"
			if t, ok := r["gpuType"].(string); ok && t != "" {
				name = t
			}
			native[name] = v
		} else if _, ok := r["gpuType"]; ok {
			return nil, fmt.Errorf("gpuType requires gpu")
		}
		dst[key] = native
	}
	return dst, nil
}

func renderingKeys(m map[string]any, path string, keys ...string) error {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	unknown := []string{}
	for k := range m {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return fmt.Errorf("unsupported legacy fields at %s: %v", path, unknown)
	}
	return nil
}
func copyRenderingMap(src map[string]any) map[string]any {
	dst := map[string]any{}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func mergePodMaps(base, patch map[string]any) (map[string]any, error) {
	a, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	raw, err := strategicpatch.StrategicMergePatch(a, b, corev1.PodTemplateSpec{})
	if err != nil {
		return nil, fmt.Errorf("invalid pod template override: %w", err)
	}
	var out map[string]any
	err = json.Unmarshal(raw, &out)
	return out, err
}

// Native components are map-lists by name. A partial pod patch must not replace
// the generated worker command, resources, or sibling sidecar containers.
func mergeBetaSpec(base, patch map[string]any) (map[string]any, error) {
	merged := copyRenderingMap(base)
	for k, v := range patch {
		if k != "components" {
			if old, ok := merged[k].(map[string]any); ok {
				if p, ok := v.(map[string]any); ok {
					v = deepMerge(old, p)
				}
			}
			merged[k] = v
		}
	}
	raw, exists := patch["components"]
	if !exists {
		return merged, nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("spec.components must be an array")
	}
	current, _ := base["components"].([]any)
	if len(entries) == 0 {
		merged["components"] = entries
		return merged, nil
	}
	seen := map[string]bool{}
	for _, v := range entries {
		p, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("component override must be an object")
		}
		name, ok := p["name"].(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("component override requires name")
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate component override %q", name)
		}
		seen[name] = true
		found := false
		for i, v := range current {
			c := v.(map[string]any)
			if c["name"] != name {
				continue
			}
			found = true
			result := copyRenderingMap(c)
			for k, v := range p {
				if k == "podTemplate" {
					template, ok := v.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("podTemplate must be an object")
					}
					original, _ := c[k].(map[string]any)
					m, err := mergePodMaps(original, template)
					if err != nil {
						return nil, err
					}
					result[k] = m
				} else {
					result[k] = v
				}
			}
			current[i] = result
			break
		}
		if !found {
			current = append(current, p)
		}
	}
	merged["components"] = current
	return merged, nil
}
