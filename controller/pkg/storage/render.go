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

package storage

import airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"

const (
	DefaultModelCacheMountPath       = "/model-cache"
	DefaultCompilationCacheMountPath = "/compilation-cache"
)

// VolumeMountPath resolves the admission-defaulted mount path while retaining
// safe fallbacks for tests or clusters where the mutating webhook is disabled.
func VolumeMountPath(volume airunwayv1alpha1.StorageVolume) string {
	if volume.MountPath != "" {
		return volume.MountPath
	}
	switch volume.Purpose {
	case airunwayv1alpha1.VolumePurposeModelCache:
		return DefaultModelCacheMountPath
	case airunwayv1alpha1.VolumePurposeCompilationCache:
		return DefaultCompilationCacheMountPath
	default:
		return ""
	}
}

// PodVolumes renders Kubernetes pod volumes for every configured claim.
func PodVolumes(md *airunwayv1alpha1.ModelDeployment) []any {
	if md.Spec.Model.Storage == nil {
		return nil
	}

	volumes := make([]any, 0, len(md.Spec.Model.Storage.Volumes))
	for _, volume := range md.Spec.Model.Storage.Volumes {
		volumes = append(volumes, map[string]any{
			"name": volume.Name,
			"persistentVolumeClaim": map[string]any{
				"claimName": volume.ResolvedClaimName(md.Name),
			},
		})
	}
	return volumes
}

// ContainerVolumeMounts renders Kubernetes container mounts for every volume
// whose path can be resolved. Admission requires custom volumes to set a path.
func ContainerVolumeMounts(md *airunwayv1alpha1.ModelDeployment) []any {
	if md.Spec.Model.Storage == nil {
		return nil
	}

	mounts := make([]any, 0, len(md.Spec.Model.Storage.Volumes))
	for _, volume := range md.Spec.Model.Storage.Volumes {
		mountPath := VolumeMountPath(volume)
		if mountPath == "" {
			continue
		}
		mount := map[string]any{
			"name":      volume.Name,
			"mountPath": mountPath,
		}
		if volume.ReadOnly {
			mount["readOnly"] = true
		}
		mounts = append(mounts, mount)
	}
	return mounts
}

// ModelCacheMountPath returns the configured model-cache path, if present.
func ModelCacheMountPath(md *airunwayv1alpha1.ModelDeployment) (string, bool) {
	if md.Spec.Model.Storage == nil {
		return "", false
	}
	for _, volume := range md.Spec.Model.Storage.Volumes {
		if volume.Purpose == airunwayv1alpha1.VolumePurposeModelCache {
			return VolumeMountPath(volume), true
		}
	}
	return "", false
}

// HasEnvVar reports whether the user explicitly configured an environment variable.
func HasEnvVar(md *airunwayv1alpha1.ModelDeployment, name string) bool {
	for _, env := range md.Spec.Env {
		if env.Name == name {
			return true
		}
	}
	return false
}

// AppendModelCacheEnv appends HF_HOME for a model-cache volume unless the user
// already provided it. Providers keep the model identifier as the engine model
// argument; HuggingFace resolves it from this persistent cache directory.
func AppendModelCacheEnv(md *airunwayv1alpha1.ModelDeployment, env []any) []any {
	path, found := ModelCacheMountPath(md)
	if !found || path == "" || HasEnvVar(md, "HF_HOME") || renderedEnvHasName(env, "HF_HOME") {
		return env
	}
	return append(env, map[string]any{
		"name":  "HF_HOME",
		"value": path,
	})
}

// renderedEnvHasName reports whether a provider has already rendered an
// environment variable. Some provider builders call AppendModelCacheEnv at
// more than one layer, so checking only ModelDeployment.spec.env is not enough
// to guarantee a valid, duplicate-free container environment.
func renderedEnvHasName(env []any, name string) bool {
	for _, item := range env {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if renderedName, ok := entry["name"].(string); ok && renderedName == name {
			return true
		}
	}
	return false
}
