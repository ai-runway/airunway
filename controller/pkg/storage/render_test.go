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

import (
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodStorageRendering(t *testing.T) {
	md := newDownloadMD("demo", "models")
	md.Spec.Model.Storage.Volumes = append(md.Spec.Model.Storage.Volumes,
		airunwayv1alpha1.StorageVolume{
			Name:      "shared-data",
			ClaimName: "existing-data",
			MountPath: "/data",
			Purpose:   airunwayv1alpha1.VolumePurposeCustom,
			ReadOnly:  true,
		},
	)

	volumes := PodVolumes(md)
	if len(volumes) != 2 {
		t.Fatalf("expected two volumes, got %d", len(volumes))
	}
	managed := volumes[0].(map[string]any)
	managedPVC := managed["persistentVolumeClaim"].(map[string]any)
	if managedPVC["claimName"] != "demo-model-cache" {
		t.Fatalf("expected generated managed claim name, got %v", managedPVC["claimName"])
	}
	existing := volumes[1].(map[string]any)
	existingPVC := existing["persistentVolumeClaim"].(map[string]any)
	if existingPVC["claimName"] != "existing-data" {
		t.Fatalf("expected existing claim name, got %v", existingPVC["claimName"])
	}

	mounts := ContainerVolumeMounts(md)
	if len(mounts) != 2 {
		t.Fatalf("expected two mounts, got %d", len(mounts))
	}
	if mounts[0].(map[string]any)["mountPath"] != DefaultModelCacheMountPath {
		t.Fatalf("expected model-cache fallback path, got %v", mounts[0])
	}
	if mounts[1].(map[string]any)["readOnly"] != true {
		t.Fatalf("expected existing claim to render readOnly=true, got %v", mounts[1])
	}
}

func TestAppendModelCacheEnvHonorsUserOverride(t *testing.T) {
	md := newDownloadMD("demo", "models")
	env := AppendModelCacheEnv(md, nil)
	if len(env) != 1 || env[0].(map[string]any)["value"] != DefaultModelCacheMountPath {
		t.Fatalf("expected default HF_HOME, got %v", env)
	}

	md.Spec.Env = []corev1.EnvVar{{Name: "HF_HOME", Value: "/custom-cache"}}
	env = AppendModelCacheEnv(md, []any{map[string]any{"name": "HF_HOME", "value": "/custom-cache"}})
	if len(env) != 1 {
		t.Fatalf("expected no duplicate HF_HOME, got %v", env)
	}
}

func TestWorkloadReadyUsesCurrentGeneration(t *testing.T) {
	md := newDownloadMD("demo", "models")
	md.Generation = 2
	md.Status.Conditions = []metav1.Condition{
		{Type: airunwayv1alpha1.ConditionTypeStorageReady, Status: metav1.ConditionTrue, ObservedGeneration: 2},
		{Type: airunwayv1alpha1.ConditionTypeModelDownloaded, Status: metav1.ConditionTrue, ObservedGeneration: 1},
	}
	if WorkloadReady(md) {
		t.Fatal("expected a stale download condition to block the workload")
	}

	md.Status.Conditions[1].ObservedGeneration = 2
	if !WorkloadReady(md) {
		t.Fatal("expected current storage and download conditions to admit the workload")
	}

	md.Spec.Model.Storage = nil
	if !WorkloadReady(md) {
		t.Fatal("expected omitted storage to remain backward compatible")
	}
}
