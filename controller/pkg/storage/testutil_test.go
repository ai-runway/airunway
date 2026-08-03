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
	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = airunwayv1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

// pvcSize is a helper to create a resource.Quantity pointer for testing.
func pvcSize(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func stringPtr(s string) *string { return &s }

func newDownloadMD(name, ns string) *airunwayv1alpha1.ModelDeployment {
	size := pvcSize("100Gi")
	return &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID:     "meta-llama/Llama-2-7b-chat-hf",
				Source: airunwayv1alpha1.ModelSourceHuggingFace,
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:       "model-cache",
							MountPath:  "/model-cache",
							Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
							Size:       size,
							AccessMode: corev1.ReadWriteMany,
						},
					},
				},
			},
		},
	}
}
