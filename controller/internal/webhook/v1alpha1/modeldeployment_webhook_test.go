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

package v1alpha1

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// flakyReader is a client.Reader stub used to simulate transient API errors
// (timeouts, connection refused) from the InferenceProviderConfig lookup in
// the validating webhook. Get returns the configured error; List is unused.
type flakyReader struct {
	err error
}

func (r *flakyReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return r.err
}

func (r *flakyReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return r.err
}

var _ = Describe("ModelDeployment Webhook", func() {
	var (
		obj       *airunwayv1alpha1.ModelDeployment
		oldObj    *airunwayv1alpha1.ModelDeployment
		validator ModelDeploymentCustomValidator
		defaulter ModelDeploymentCustomDefaulter
	)

	stringPtr := func(s string) *string { return &s }

	BeforeEach(func() {
		obj = &airunwayv1alpha1.ModelDeployment{}
		oldObj = &airunwayv1alpha1.ModelDeployment{}

		dynamo := &airunwayv1alpha1.InferenceProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamo"},
			Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.ProviderCapabilities{
					Engines: []airunwayv1alpha1.EngineCapability{
						{Name: airunwayv1alpha1.EngineTypeVLLM, GPUSupport: true,
							ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated, airunwayv1alpha1.ServingModeDisaggregated}},
						{Name: airunwayv1alpha1.EngineTypeTRTLLM, GPUSupport: true,
							ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated}},
					},
				},
			},
		}
		kaito := &airunwayv1alpha1.InferenceProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "kaito"},
			Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.ProviderCapabilities{
					Engines: []airunwayv1alpha1.EngineCapability{
						{Name: airunwayv1alpha1.EngineTypeVLLM, GPUSupport: true,
							ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated}},
						{Name: airunwayv1alpha1.EngineTypeLlamaCpp, GPUSupport: true, CPUSupport: true,
							ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated}},
					},
				},
			},
		}

		s := runtime.NewScheme()
		Expect(airunwayv1alpha1.AddToScheme(s)).To(Succeed())
		fakeReader := fake.NewClientBuilder().WithScheme(s).WithObjects(dynamo, kaito).Build()

		validator = ModelDeploymentCustomValidator{Reader: fakeReader}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		defaulter = ModelDeploymentCustomDefaulter{}
		Expect(defaulter).NotTo(BeNil(), "Expected defaulter to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	AfterEach(func() {
	})

	Context("When creating ModelDeployment under Defaulting Webhook", func() {
		It("Should default mountPath for modelCache purpose", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "my-pvc",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
					},
				},
			}
			err := defaulter.Default(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Model.Storage.Volumes[0].MountPath).To(Equal("/model-cache"))
		})

		It("Should default mountPath for compilationCache purpose", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "compile-data",
						ClaimName: "compile-pvc",
						Purpose:   airunwayv1alpha1.VolumePurposeCompilationCache,
					},
				},
			}
			err := defaulter.Default(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Model.Storage.Volumes[0].MountPath).To(Equal("/compilation-cache"))
		})

		It("Should default purpose to custom when not specified", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "extra-data",
						ClaimName: "extra-pvc",
						MountPath: "/data",
					},
				},
			}
			err := defaulter.Default(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Model.Storage.Volumes[0].Purpose).To(Equal(airunwayv1alpha1.VolumePurposeCustom))
		})

		It("Should not override explicitly set mountPath", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "my-pvc",
						MountPath: "/custom-path",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
					},
				},
			}
			err := defaulter.Default(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Model.Storage.Volumes[0].MountPath).To(Equal("/custom-path"))
		})

		It("Should default claimName when size is set and claimName is empty", func() {
			obj.Name = "my-deployment"
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:    "model-data",
						Purpose: airunwayv1alpha1.VolumePurposeModelCache,
						Size:    &size,
					},
				},
			}
			err := defaulter.Default(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Model.Storage.Volumes[0].ClaimName).To(Equal("my-deployment-model-data"))
		})

		It("Should default accessMode to ReadWriteMany when size is set", func() {
			obj.Name = "my-deployment"
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:    "model-data",
						Purpose: airunwayv1alpha1.VolumePurposeModelCache,
						Size:    &size,
					},
				},
			}
			err := defaulter.Default(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Model.Storage.Volumes[0].AccessMode).To(Equal(corev1.ReadWriteMany))
		})

		It("Should not override explicitly set claimName when size is set", func() {
			obj.Name = "my-deployment"
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "custom-pvc-name",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						Size:      &size,
					},
				},
			}
			err := defaulter.Default(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Model.Storage.Volumes[0].ClaimName).To(Equal("custom-pvc-name"))
		})

		It("Should not set accessMode defaults when size is not set", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "existing-pvc",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
					},
				},
			}
			err := defaulter.Default(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Model.Storage.Volumes[0].AccessMode).To(BeEmpty())
		})

		It("Should default GPU to 1 in aggregated mode when engine is explicit and no custom image is set", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			err := defaulter.Default(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Resources).NotTo(BeNil())
			Expect(obj.Spec.Resources.GPU).NotTo(BeNil())
			Expect(obj.Spec.Resources.GPU.Count).To(Equal(int32(1)))
			Expect(obj.Spec.Resources.GPU.Type).To(Equal("nvidia.com/gpu"))
		})

		It("Should not default GPU when a custom image is set", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Image = "my.registry.example.com/custom-runtime:latest"
			err := defaulter.Default(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Resources).To(BeNil())
		})
	})

	Context("When creating or updating ModelDeployment under Validating Webhook", func() {
		It("Should reject names containing dots", func() {
			obj.Name = "qwen3-0.6b"
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must not contain dots"))
		})

		It("Should admit names without dots", func() {
			obj.Name = "qwen3-0-6b"
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should allow updates on existing resources with dots in name", func() {
			obj.Name = "qwen3-0.6b"
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			oldObj.Name = "qwen3-0.6b"
			oldObj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			warnings, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should reject conflicting spec.image and spec.engine.image on create", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Image = "legacy:v1"
			obj.Spec.Engine.Image = "engine:v2"

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.image"))
			Expect(err.Error()).To(ContainSubstring("spec.engine.image"))
		})

		It("Should reject conflicting spec.image and spec.engine.image on update", func() {
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Engine.Image = "engine:v1"

			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Image = "legacy:v1"
			obj.Spec.Engine.Image = "engine:v2"

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.image"))
			Expect(err.Error()).To(ContainSubstring("spec.engine.image"))
		})

		It("Should reject changing an explicitly-set provider.name", func() {
			oldObj.Spec.Model.ID = "Qwen/Qwen2.5-0.5B-Instruct"
			oldObj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}

			obj.Spec.Model.ID = "Qwen/Qwen2.5-0.5B-Instruct"
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "vllm"}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("provider.name is immutable"))
		})

		It("Should reject overriding an auto-selected provider recorded only in status", func() {
			// Auto-selection writes the chosen provider to status, never to the
			// user's spec. Overriding it via spec.provider.name must be rejected
			// at admission, not admitted and then failed asynchronously by the
			// controller.
			oldObj.Spec.Model.ID = "Qwen/Qwen2.5-0.5B-Instruct"
			oldObj.Spec.Provider = nil // user never set a provider
			oldObj.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: "dynamo"}

			obj.Spec.Model.ID = "Qwen/Qwen2.5-0.5B-Instruct"
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "vllm"}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("provider.name is immutable"))
		})

		It("Should admit an update that does not change the effective provider", func() {
			// A plain re-apply (spec.provider.name still empty) must not be
			// rejected just because status records an auto-selected provider —
			// otherwise every reconcile-driven update on an auto-selected MD breaks.
			oldObj.Spec.Model.ID = "Qwen/Qwen2.5-0.5B-Instruct"
			oldObj.Spec.Provider = nil
			oldObj.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: "dynamo"}

			obj.Spec.Model.ID = "Qwen/Qwen2.5-0.5B-Instruct"
			obj.Spec.Provider = nil

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit matching spec.image and spec.engine.image", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Image = "same:v1"
			obj.Spec.Engine.Image = "same:v1"

			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should reject a flag set in both engine.args and engine.extraArgs on create", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Engine.Args = map[string]string{"tensor-parallel-size": "4"}
			obj.Spec.Engine.ExtraArgs = []string{"--tensor-parallel-size=2"}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tensor-parallel-size"))
			Expect(err.Error()).To(ContainSubstring("spec.engine.args"))
			Expect(err.Error()).To(ContainSubstring("spec.engine.extraArgs"))
		})

		It("Should reject a flag set in both engine.args and engine.extraArgs on update", func() {
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"

			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Engine.Args = map[string]string{"tensor-parallel-size": "4"}
			obj.Spec.Engine.ExtraArgs = []string{"--tensor-parallel-size", "2"}

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tensor-parallel-size"))
		})

		It("Should admit disjoint engine.args and engine.extraArgs", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Engine.Args = map[string]string{"gpu-memory-utilization": "0.9"}
			obj.Spec.Engine.ExtraArgs = []string{"--enable-chunked-prefill", "--max-num-seqs=64"}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit a single modelCache volume", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "my-pvc",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
					},
				},
			}
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should admit modelCache + compilationCache together", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "model-pvc",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
					},
					{
						Name:      "compile-data",
						ClaimName: "compile-pvc",
						MountPath: "/compilation-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeCompilationCache,
					},
				},
			}
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should admit custom volume with explicit mountPath", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "extra-data",
						ClaimName: "extra-pvc",
						MountPath: "/data/extra",
						Purpose:   airunwayv1alpha1.VolumePurposeCustom,
					},
				},
			}
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should reject duplicate volume names", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "vol",
						ClaimName: "pvc-a",
						MountPath: "/mount-a",
						Purpose:   airunwayv1alpha1.VolumePurposeCustom,
					},
					{
						Name:      "vol",
						ClaimName: "pvc-b",
						MountPath: "/mount-b",
						Purpose:   airunwayv1alpha1.VolumePurposeCustom,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate volume name"))
		})

		It("Should reject duplicate mountPaths", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "vol-a",
						ClaimName: "pvc-a",
						MountPath: "/same-path",
						Purpose:   airunwayv1alpha1.VolumePurposeCustom,
					},
					{
						Name:      "vol-b",
						ClaimName: "pvc-b",
						MountPath: "/same-path",
						Purpose:   airunwayv1alpha1.VolumePurposeCustom,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate mount path"))
		})

		It("Should reject duplicate claimNames", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "vol-a",
						ClaimName: "same-pvc",
						MountPath: "/mount-a",
						Purpose:   airunwayv1alpha1.VolumePurposeCustom,
					},
					{
						Name:      "vol-b",
						ClaimName: "same-pvc",
						MountPath: "/mount-b",
						Purpose:   airunwayv1alpha1.VolumePurposeCustom,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate claim name"))
		})

		It("Should reject relative mountPath", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "vol",
						ClaimName: "pvc",
						MountPath: "relative/path",
						Purpose:   airunwayv1alpha1.VolumePurposeCustom,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must be an absolute path"))
		})

		It("Should reject custom purpose without explicit mountPath", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "vol",
						ClaimName: "pvc",
						Purpose:   airunwayv1alpha1.VolumePurposeCustom,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("mountPath is required when purpose is custom"))
		})

		It("Should reject two modelCache volumes", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "cache-a",
						ClaimName: "pvc-a",
						MountPath: "/model-cache-a",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
					},
					{
						Name:      "cache-b",
						ClaimName: "pvc-b",
						MountPath: "/model-cache-b",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("at most one volume with purpose=modelCache"))
		})

		It("Should reject system path overlap", func() {
			systemPaths := []string{"/dev", "/proc", "/sys", "/etc", "/var/run"}
			for _, sysPath := range systemPaths {
				obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
				obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:      "vol",
							ClaimName: "pvc",
							MountPath: sysPath,
							Purpose:   airunwayv1alpha1.VolumePurposeCustom,
						},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred(), "Expected error for system path %s", sysPath)
				Expect(err.Error()).To(ContainSubstring("system path"), "Expected system path error for %s", sysPath)
			}
		})

		It("Should reject system path sub-directory", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "vol",
						ClaimName: "pvc",
						MountPath: "/proc/something",
						Purpose:   airunwayv1alpha1.VolumePurposeCustom,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("system path"))
		})

		It("Should warn on readOnly compilationCache", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "compile-data",
						ClaimName: "compile-pvc",
						MountPath: "/compilation-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeCompilationCache,
						ReadOnly:  true,
					},
				},
			}
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(HaveLen(1))
			Expect(warnings[0]).To(ContainSubstring("compilationCache"))
			Expect(warnings[0]).To(ContainSubstring("readOnly"))
		})

		It("Should warn on readOnly modelCache with huggingface source", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Source = airunwayv1alpha1.ModelSourceHuggingFace
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "my-pvc",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						ReadOnly:  true,
					},
				},
			}
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(HaveLen(1))
			Expect(warnings[0]).To(ContainSubstring("modelCache"))
			Expect(warnings[0]).To(ContainSubstring("readOnly"))
			Expect(warnings[0]).To(ContainSubstring("download will be skipped"))
		})

		It("Should admit volume with size and auto-generated claimName", func() {
			obj.Name = "my-deployment"
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "my-deployment-model-data",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						Size:      &size,
					},
				},
			}
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should admit volume with size and explicit storageClassName and claimName", func() {
			obj.Name = "my-deployment"
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("200Gi")
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:             "model-data",
						ClaimName:        "my-deployment-model-data",
						MountPath:        "/model-cache",
						Purpose:          airunwayv1alpha1.VolumePurposeModelCache,
						Size:             &size,
						StorageClassName: stringPtr("fast-ssd"),
					},
				},
			}
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should reject claimName that doesn't match auto-generated pattern when size is set", func() {
			obj.Name = "my-deployment"
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "arbitrary-pvc",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						Size:      &size,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("claimName must not be set when size is set"))
		})

		It("Should reject volume without size and without claimName", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("claimName is required when size is not set"))
		})

		It("Should reject size with readOnly true", func() {
			obj.Name = "my-deployment"
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "my-deployment-model-data",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						Size:      &size,
						ReadOnly:  true,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("readOnly must not be true when size is set"))
		})

		It("Should reject accessMode set without size", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "existing-pvc",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("accessMode is only applicable when size is set"))
		})

		It("Should reject unsupported accessMode value", func() {
			obj.Name = "my-deployment"
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.PersistentVolumeAccessMode("banana"),
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Unsupported value"))
		})

		It("Should reject storageClassName set without size", func() {
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:             "model-data",
						ClaimName:        "existing-pvc",
						MountPath:        "/model-cache",
						Purpose:          airunwayv1alpha1.VolumePurposeModelCache,
						StorageClassName: stringPtr("fast-ssd"),
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("storageClassName is only applicable when size is set"))
		})

		It("Should admit volume with size and empty storageClassName", func() {
			obj.Name = "my-deployment"
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:             "model-data",
						ClaimName:        "my-deployment-model-data",
						MountPath:        "/model-cache",
						Purpose:          airunwayv1alpha1.VolumePurposeModelCache,
						Size:             &size,
						StorageClassName: stringPtr(""),
					},
				},
			}
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should reject size change on managed volume", func() {
			oldSize := resource.MustParse("100Gi")
			newSize := resource.MustParse("200Gi")
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &oldSize,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &newSize,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("managed storage volume is immutable"))
		})

		It("Should reject storageClassName change on managed volume", func() {
			size := resource.MustParse("100Gi")
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:             "model-data",
						ClaimName:        "my-deployment-model-data",
						MountPath:        "/model-cache",
						Purpose:          airunwayv1alpha1.VolumePurposeModelCache,
						Size:             &size,
						AccessMode:       corev1.ReadWriteMany,
						StorageClassName: stringPtr("fast-ssd"),
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:             "model-data",
						ClaimName:        "my-deployment-model-data",
						MountPath:        "/model-cache",
						Purpose:          airunwayv1alpha1.VolumePurposeModelCache,
						Size:             &size,
						AccessMode:       corev1.ReadWriteMany,
						StorageClassName: stringPtr("slow-hdd"),
					},
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("managed storage volume is immutable"))
		})

		It("Should reject accessMode change on managed volume", func() {
			size := resource.MustParse("100Gi")
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteOnce,
					},
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("managed storage volume is immutable"))
		})

		It("Should reject removing size from managed volume", func() {
			oldSize := resource.MustParse("100Gi")
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &oldSize,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "my-deployment-model-data",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						// Size is nil - removing size from a managed volume
					},
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("managed storage volume is immutable"))
		})

		It("Should reject mountPath change on managed volume", func() {
			size := resource.MustParse("100Gi")
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/new-mount-path",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("managed storage volume is immutable"))
		})

		It("Should reject readOnly change on managed volume", func() {
			size := resource.MustParse("100Gi")
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
						ReadOnly:   true,
					},
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("managed storage volume is immutable"))
		})

		It("Should reject purpose change on managed volume", func() {
			size := resource.MustParse("100Gi")
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeCompilationCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("managed storage volume is immutable"))
		})

		It("Should allow PVC field changes on unmanaged volumes", func() {
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "existing-pvc",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						// Size is nil - unmanaged volume (pre-existing PVC reference)
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "different-pvc",
						MountPath: "/new-mount-path",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						// Size is still nil - no immutability constraints
					},
				},
			}
			warnings, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should allow adding new managed volume", func() {
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			// Old spec has no storage
			obj.Name = "my-deployment"
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			warnings, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should reject removing managed volume from list", func() {
			size := resource.MustParse("100Gi")
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot be removed"))
			Expect(err.Error()).To(ContainSubstring("model-data"))
		})

		It("Should reject nullifying storage with managed volumes", func() {
			size := resource.MustParse("100Gi")
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			// Storage is nil
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot be removed"))
		})

		It("Should reject removing one of two managed volumes", func() {
			size := resource.MustParse("100Gi")
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
					{
						Name:       "compile-data",
						ClaimName:  "my-deployment-compile-data",
						MountPath:  "/compilation-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeCompilationCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:       "model-data",
						ClaimName:  "my-deployment-model-data",
						MountPath:  "/model-cache",
						Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						Size:       &size,
						AccessMode: corev1.ReadWriteMany,
					},
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot be removed"))
			Expect(err.Error()).To(ContainSubstring("compile-data"))
		})

		It("Should allow removing unmanaged volume", func() {
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "existing-pvc",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						// Size is nil - unmanaged volume
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{},
			}
			warnings, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should allow nullifying storage with only unmanaged volumes", func() {
			oldObj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			oldObj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      "model-data",
						ClaimName: "existing-pvc",
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						// Size is nil - unmanaged volume
					},
				},
			}
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			// Storage is nil
			warnings, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should reject managed volume when derived PVC claim name exceeds 253 chars", func() {
			// 240-char MD name + 1 dash + 20-char volume name = 261 > 253
			obj.Name = strings.Repeat("a", 240)
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			volName := strings.Repeat("v", 20)
			claimName := obj.Name + "-" + volName
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      volName,
						ClaimName: claimName,
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						Size:      &size,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("253-character"))
		})

		It("Should admit managed volume when derived PVC claim name is within limit", func() {
			// 200-char MD name + 1 dash + 5-char volume name = 206 <= 253
			obj.Name = strings.Repeat("a", 200)
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			volName := "cache"
			claimName := obj.Name + "-" + volName
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      volName,
						ClaimName: claimName,
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						Size:      &size,
					},
				},
			}
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should reject when download job name exceeds 253 chars", func() {
			// 250-char MD name + "-model-download" (15 chars) = 265 > 253
			obj.Name = strings.Repeat("a", 250)
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			size := resource.MustParse("100Gi")
			volName := "mc"
			claimName := obj.Name + "-" + volName
			obj.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
				Volumes: []airunwayv1alpha1.StorageVolume{
					{
						Name:      volName,
						ClaimName: claimName,
						MountPath: "/model-cache",
						Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
						Size:      &size,
					},
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("download Job name"))
			Expect(err.Error()).To(ContainSubstring("253-character"))
		})

		It("Should not validate download job name when no managed modelCache volume exists", func() {
			// 250-char MD name would trigger download job name validation
			// but only if a managed modelCache volume exists
			obj.Name = strings.Repeat("a", 250)
			obj.Spec.Model.ID = "meta-llama/Llama-2-7b-chat-hf"
			// No storage at all
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})
	})

	Context("When validating provider compatibility", func() {
		It("Should reject trtllm + disaggregated on dynamo", func() {
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeTRTLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Serving = &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated}
			obj.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
				Prefill: &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
				Decode:  &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not support disaggregated mode for engine trtllm"))
		})

		It("Should reject engine not supported by provider", func() {
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "kaito"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not support engine sglang"))
		})

		It("Should accept vllm + disaggregated on dynamo", func() {
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Serving = &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated}
			obj.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
				Prefill: &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
				Decode:  &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should skip check when no provider specified", func() {
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should reject when provider not found in cluster", func() {
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "nonexistent"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("InferenceProviderConfig"))
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		It("Should skip check when InferenceProviderConfig CRD is not installed", func() {
			// Build a reader whose scheme does NOT register InferenceProviderConfig,
			// so Get returns a NoKindMatchError (mid-bootstrap cluster).
			emptyScheme := runtime.NewScheme()
			noCRDReader := fake.NewClientBuilder().WithScheme(emptyScheme).Build()
			noCRDValidator := ModelDeploymentCustomValidator{Reader: noCRDReader}

			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
			_, err := noCRDValidator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should skip provider lookup when Reader is nil", func() {
			// A validator constructed without a Reader (e.g. in unit tests that
			// don't wire up the manager) must not panic and must not run the
			// provider/engine compatibility lookup — the guard at
			// modeldeployment_webhook.go ensures the Get is short-circuited.
			nilReaderValidator := ModelDeploymentCustomValidator{Reader: nil}

			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}

			warnings, err := nilReaderValidator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("Should surface a warning when provider lookup fails transiently", func() {
			flakyReader := &flakyReader{err: errors.New("etcdserver: request timed out")}
			flakyValidator := ModelDeploymentCustomValidator{Reader: flakyReader}

			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
			warnings, err := flakyValidator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).NotTo(BeEmpty())
			joined := strings.Join(warnings, "|")
			Expect(joined).To(ContainSubstring("dynamo"))
			Expect(joined).To(ContainSubstring("controller will re-validate"))
		})

		It("Should reject admission when provider lookup is Forbidden (webhook RBAC misconfigured)", func() {
			// A Forbidden response from the apiserver means the webhook
			// ServiceAccount cannot read InferenceProviderConfig. Silently
			// skipping the compatibility check in that case would disable
			// admission-time enforcement cluster-wide, so the webhook must
			// fail admission with an InternalError instead.
			forbiddenReader := &flakyReader{err: apierrors.NewForbidden(
				schema.GroupResource{Group: airunwayv1alpha1.GroupVersion.Group, Resource: "inferenceproviderconfigs"},
				"dynamo",
				errors.New("user cannot get resource"),
			)}
			forbiddenValidator := ModelDeploymentCustomValidator{Reader: forbiddenReader}

			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
			_, err := forbiddenValidator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot verify provider"))
			Expect(err.Error()).To(ContainSubstring("dynamo"))
		})

		It("Should reject admission when provider lookup is Unauthorized", func() {
			unauthReader := &flakyReader{err: apierrors.NewUnauthorized("token expired")}
			unauthValidator := ModelDeploymentCustomValidator{Reader: unauthReader}

			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}}
			_, err := unauthValidator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot verify provider"))
		})

		It("Should reject vllm with gpu.count=0 on dynamo (no CPU support)", func() {
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 0}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("requires GPU"))
		})

		It("Should reject vllm with no resources on dynamo (no CPU support)", func() {
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("requires GPU"))
		})

		It("Should accept llamacpp with no GPU on kaito (has CPU support)", func() {
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeLlamaCpp
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "kaito"}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should accept CPU-only vllm on dynamo when mocker annotation is set", func() {
			obj.Annotations = map[string]string{"airunway.ai/dynamo-test-backend": "mocker"}
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 0}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should still reject CPU-only vllm on dynamo when mocker annotation has a different value", func() {
			obj.Annotations = map[string]string{"airunway.ai/dynamo-test-backend": "real"}
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 0}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("requires GPU"))
		})

		It("Should accept CPU-only disaggregated vllm on dynamo when mocker annotation is set", func() {
			obj.Annotations = map[string]string{"airunway.ai/dynamo-test-backend": "mocker"}
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Serving = &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated}
			// No GPU counts on prefill/decode: the GPU-less mocker waives the
			// disaggregated gpu.count requirement, but the blocks are still required.
			obj.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
				Prefill: &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
				Decode:  &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should still reject CPU-only disaggregated vllm on dynamo without the mocker annotation", func() {
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Serving = &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated}
			obj.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
				Prefill: &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
				Decode:  &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("gpu.count > 0"))
		})

		It("Should reject a non-vllm engine on dynamo even with the mocker annotation", func() {
			obj.Annotations = map[string]string{"airunway.ai/dynamo-test-backend": "mocker"}
			obj.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang
			obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "dynamo"}
			obj.Spec.Resources = &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 0}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("only supports the vllm engine"))
		})
	})
})
