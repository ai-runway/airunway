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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// ptrBool is a local helper for the *bool status fields.
func ptrBool(b bool) *bool { return &b }

var _ = Describe("AgentDeployment core controller", func() {
	ctx := context.Background()

	// createReadyProvider registers a cluster-scoped AgentProviderConfig with
	// the given backend + supported binding modes and marks it ready. It
	// registers cleanup and returns the created name.
	createReadyProvider := func(name string, backend airunwayv1alpha1.AgentProviderBackend, modes ...airunwayv1alpha1.ModelBindingMode) {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           backend,
					ModelBindingModes: modes,
				},
			},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, apc)
		})

		// Mark ready via the status subresource.
		apc.Status.Ready = ptrBool(true)
		apc.Status.Version = "v0.0.0-test"
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())
	}

	// newAgent builds an AgentDeployment with a single externalAPI binding.
	newAgent := func(name, framework string) *airunwayv1alpha1.AgentDeployment {
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: framework},
				Model: airunwayv1alpha1.ModelBinding{
					ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
						Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
						BaseURL:   "https://api.openai.com/v1",
						ModelName: "gpt-4o-mini",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, ad)
		})
		return ad
	}

	reconcileOnce := func(name string) {
		r := &AgentDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	get := func(name string) *airunwayv1alpha1.AgentDeployment {
		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, out)).To(Succeed())
		return out
	}

	condition := func(ad *airunwayv1alpha1.AgentDeployment, t string) *metav1.Condition {
		return meta.FindStatusCondition(ad.Status.Conditions, t)
	}

	Context("when the framework and binding resolve cleanly", func() {
		It("sets FrameworkReady + ModelBound and publishes the resolved binding", func() {
			createReadyProvider("kagent-happy", airunwayv1alpha1.AgentProviderBackendCRD,
				airunwayv1alpha1.ModelBindingModeExternalAPI)
			newAgent("agent-happy", "kagent-happy")

			reconcileOnce("agent-happy")
			ad := get("agent-happy")

			fr := condition(ad, airunwayv1alpha1.AgentConditionTypeFrameworkReady)
			Expect(fr).NotTo(BeNil())
			Expect(fr.Status).To(Equal(metav1.ConditionTrue))

			mb := condition(ad, airunwayv1alpha1.AgentConditionTypeModelBound)
			Expect(mb).NotTo(BeNil())
			Expect(mb.Status).To(Equal(metav1.ConditionTrue))

			Expect(ad.Status.Framework).NotTo(BeNil())
			Expect(ad.Status.Framework.Name).To(Equal("kagent-happy"))
			Expect(ad.Status.Framework.ProviderVersion).To(Equal("v0.0.0-test"))

			Expect(ad.Status.ModelBinding).NotTo(BeNil())
			Expect(ad.Status.ModelBinding.BindingMode).To(Equal(airunwayv1alpha1.ModelBindingModeExternalAPI))
			Expect(ad.Status.ModelBinding.BaseURL).To(Equal("https://api.openai.com/v1"))
			Expect(ad.Status.ModelBinding.ModelName).To(Equal("gpt-4o-mini"))

			// Not Ready yet: the provider has not reported ProviderReady.
			ready := condition(ad, airunwayv1alpha1.AgentConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("WaitingForProvider"))
		})

		It("holds ModelBound false when externalAPI.credentialsRef Secret is missing", func() {
			createReadyProvider("kagent-missing-secret", airunwayv1alpha1.AgentProviderBackendCRD,
				airunwayv1alpha1.ModelBindingModeExternalAPI)
			ad := newAgent("agent-missing-secret", "kagent-missing-secret")
			ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
				Name: "does-not-exist",
				Key:  "token",
			}
			Expect(k8sClient.Update(ctx, ad)).To(Succeed())

			reconcileOnce("agent-missing-secret")
			out := get("agent-missing-secret")

			mb := condition(out, airunwayv1alpha1.AgentConditionTypeModelBound)
			Expect(mb).NotTo(BeNil())
			Expect(mb.Status).To(Equal(metav1.ConditionFalse))
			Expect(mb.Reason).To(Equal("CredentialSecretNotFound"))
			Expect(out.Status.ModelBinding).To(BeNil())
		})

		It("resolves externalAPI.credentialsRef when the Secret key exists", func() {
			createReadyProvider("kagent-with-secret", airunwayv1alpha1.AgentProviderBackendCRD,
				airunwayv1alpha1.ModelBindingModeExternalAPI)
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "openai-creds", Namespace: "default"},
				Data:       map[string][]byte{"token": []byte("dummy")},
			}
			Expect(k8sClient.Create(ctx, sec)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, sec) })

			ad := newAgent("agent-with-secret", "kagent-with-secret")
			ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
				Name: "openai-creds",
				Key:  "token",
			}
			Expect(k8sClient.Update(ctx, ad)).To(Succeed())

			reconcileOnce("agent-with-secret")
			out := get("agent-with-secret")

			mb := condition(out, airunwayv1alpha1.AgentConditionTypeModelBound)
			Expect(mb).NotTo(BeNil())
			Expect(mb.Status).To(Equal(metav1.ConditionTrue))
			Expect(out.Status.ModelBinding).NotTo(BeNil())
			Expect(out.Status.ModelBinding.CredentialsRef).NotTo(BeNil())
			Expect(out.Status.ModelBinding.CredentialsRef.Name).To(Equal("openai-creds"))
			Expect(out.Status.ModelBinding.CredentialsRef.Key).To(Equal("token"))
		})
	})

	Context("when the binding is a deploymentRef to an in-cluster model", func() {
		It("resolves the in-cluster endpoint for a keyless deploymentRef binding", func() {
			createReadyProvider("kagent-depref", airunwayv1alpha1.AgentProviderBackendCRD,
				airunwayv1alpha1.ModelBindingModeDeploymentRef)

			// An in-cluster ModelDeployment that has published a serving
			// endpoint but carries no credential (KAITO llama.cpp is keyless).
			md := &airunwayv1alpha1.ModelDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-model", Namespace: "default"},
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{
						Source:     airunwayv1alpha1.ModelSourceCustom,
						ServedName: "llama-3.2-1b-instruct",
					},
				},
			}
			Expect(k8sClient.Create(ctx, md)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, md) })
			md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "demo-model", Port: 80}
			Expect(k8sClient.Status().Update(ctx, md)).To(Succeed())

			ad := &airunwayv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "agent-depref", Namespace: "default"},
				Spec: airunwayv1alpha1.AgentDeploymentSpec{
					Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "kagent-depref"},
					Model: airunwayv1alpha1.ModelBinding{
						DeploymentRef: &airunwayv1alpha1.ModelDeploymentBinding{Name: "demo-model"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, ad)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })

			reconcileOnce("agent-depref")
			out := get("agent-depref")

			mb := condition(out, airunwayv1alpha1.AgentConditionTypeModelBound)
			Expect(mb).NotTo(BeNil())
			Expect(mb.Status).To(Equal(metav1.ConditionTrue))

			Expect(out.Status.ModelBinding).NotTo(BeNil())
			b := *out.Status.ModelBinding
			Expect(b.BindingMode).To(Equal(airunwayv1alpha1.ModelBindingModeDeploymentRef))
			Expect(b.BaseURL).To(Equal("http://demo-model.default.svc.cluster.local:80/v1"))
			Expect(b.ModelName).To(Equal("llama-3.2-1b-instruct"))
			Expect(b.ObservedResourceUID).To(Equal(string(md.UID)))

			// Core leaves keyless credentials empty; provider backends handle the
			// final credential materialization/injection.
			Expect(b.CredentialsRef).To(BeNil())
		})

		It("prefers the gateway status endpoint and gateway model name when gateway routing is configured", func() {
			createReadyProvider("kagent-depref-gw", airunwayv1alpha1.AgentProviderBackendCRD,
				airunwayv1alpha1.ModelBindingModeDeploymentRef)

			md := &airunwayv1alpha1.ModelDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-model-gw", Namespace: "default"},
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{
						Source:     airunwayv1alpha1.ModelSourceCustom,
						ServedName: "llama-3.2-1b-instruct",
					},
				},
			}
			Expect(k8sClient.Create(ctx, md)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, md) })
			md.Status.Endpoint = &airunwayv1alpha1.EndpointStatus{Service: "demo-model-gw", Port: 80}
			md.Status.Gateway = &airunwayv1alpha1.GatewayStatus{
				Endpoint:         "10.0.0.42",
				GatewayName:      "inference-gateway",
				GatewayNamespace: "gateway-system",
				ModelName:        "llama-3.2-1b-gateway",
			}
			Expect(k8sClient.Status().Update(ctx, md)).To(Succeed())

			ad := &airunwayv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "agent-depref-gw", Namespace: "default"},
				Spec: airunwayv1alpha1.AgentDeploymentSpec{
					Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "kagent-depref-gw"},
					Model: airunwayv1alpha1.ModelBinding{
						DeploymentRef: &airunwayv1alpha1.ModelDeploymentBinding{Name: "demo-model-gw"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, ad)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })

			reconcileOnce("agent-depref-gw")
			out := get("agent-depref-gw")

			Expect(out.Status.ModelBinding).NotTo(BeNil())
			b := *out.Status.ModelBinding
			Expect(b.BaseURL).To(Equal("http://10.0.0.42/v1"))
			Expect(b.ModelName).To(Equal("llama-3.2-1b-gateway"))
			Expect(b.CredentialsRef).To(BeNil())
		})

		It("maps ModelDeployment changes to referencing AgentDeployments", func() {
			createReadyProvider("kagent-map", airunwayv1alpha1.AgentProviderBackendCRD,
				airunwayv1alpha1.ModelBindingModeDeploymentRef)

			md := &airunwayv1alpha1.ModelDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "map-model", Namespace: "default"},
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{Source: airunwayv1alpha1.ModelSourceCustom},
				},
			}
			Expect(k8sClient.Create(ctx, md)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, md) })

			ref := &airunwayv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "agent-map-ref", Namespace: "default"},
				Spec: airunwayv1alpha1.AgentDeploymentSpec{
					Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "kagent-map"},
					Model: airunwayv1alpha1.ModelBinding{
						DeploymentRef: &airunwayv1alpha1.ModelDeploymentBinding{Name: "map-model"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, ref)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ref) })

			other := &airunwayv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "agent-map-other", Namespace: "default"},
				Spec: airunwayv1alpha1.AgentDeploymentSpec{
					Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "kagent-map"},
					Model: airunwayv1alpha1.ModelBinding{
						ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
							Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
							BaseURL:   "https://api.openai.com/v1",
							ModelName: "gpt-4o-mini",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, other) })

			r := &AgentDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			reqs := r.mapModelDeploymentToAgentDeployments(ctx, md)
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{
				Name:      "agent-map-ref",
				Namespace: "default",
			}))
		})

		It("rejects a cross-namespace deploymentRef until AgentReferenceGrant exists", func() {
			createReadyProvider("kagent-xns", airunwayv1alpha1.AgentProviderBackendCRD,
				airunwayv1alpha1.ModelBindingModeDeploymentRef)

			ad := &airunwayv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "agent-xns", Namespace: "default"},
				Spec: airunwayv1alpha1.AgentDeploymentSpec{
					Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "kagent-xns"},
					Model: airunwayv1alpha1.ModelBinding{
						DeploymentRef: &airunwayv1alpha1.ModelDeploymentBinding{
							Name:      "some-model",
							Namespace: "other-namespace",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, ad)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })

			reconcileOnce("agent-xns")
			out := get("agent-xns")

			mb := condition(out, airunwayv1alpha1.AgentConditionTypeModelBound)
			Expect(mb).NotTo(BeNil())
			Expect(mb.Status).To(Equal(metav1.ConditionFalse))
			Expect(mb.Reason).To(Equal("CrossNamespaceRefNotAllowed"))
			Expect(out.Status.ModelBinding).To(BeNil())
		})
	})

	Context("when the framework is not registered", func() {
		It("refuses with FrameworkNotRegistered", func() {
			newAgent("agent-noframework", "does-not-exist")
			reconcileOnce("agent-noframework")
			ad := get("agent-noframework")

			fr := condition(ad, airunwayv1alpha1.AgentConditionTypeFrameworkReady)
			Expect(fr).NotTo(BeNil())
			Expect(fr.Status).To(Equal(metav1.ConditionFalse))
			Expect(fr.Reason).To(Equal("FrameworkNotRegistered"))
		})
	})

	Context("when the framework is registered but not ready", func() {
		It("holds with FrameworkNotReady", func() {
			apc := &airunwayv1alpha1.AgentProviderConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "kagent-notready"},
				Spec: airunwayv1alpha1.AgentProviderConfigSpec{
					Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
						Backend:           airunwayv1alpha1.AgentProviderBackendCRD,
						ModelBindingModes: []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI},
					},
				},
			}
			Expect(k8sClient.Create(ctx, apc)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
			// Intentionally leave status.ready nil.

			newAgent("agent-notready", "kagent-notready")
			reconcileOnce("agent-notready")
			ad := get("agent-notready")

			fr := condition(ad, airunwayv1alpha1.AgentConditionTypeFrameworkReady)
			Expect(fr).NotTo(BeNil())
			Expect(fr.Status).To(Equal(metav1.ConditionFalse))
			Expect(fr.Reason).To(Equal("FrameworkNotReady"))
		})
	})

	Context("when the binding mode is not supported by the framework", func() {
		It("refuses with UnsupportedBindingMode", func() {
			// Provider supports only deploymentRef, but the agent uses externalAPI.
			createReadyProvider("kagent-nomode", airunwayv1alpha1.AgentProviderBackendCRD,
				airunwayv1alpha1.ModelBindingModeDeploymentRef)
			newAgent("agent-badmode", "kagent-nomode")

			reconcileOnce("agent-badmode")
			ad := get("agent-badmode")

			mb := condition(ad, airunwayv1alpha1.AgentConditionTypeModelBound)
			Expect(mb).NotTo(BeNil())
			Expect(mb.Status).To(Equal(metav1.ConditionFalse))
			Expect(mb.Reason).To(Equal("UnsupportedBindingMode"))
		})
	})

	Context("server-side apply field ownership (issue #264 anti-clobber)", func() {
		It("preserves provider-owned status across a core reconcile and aggregates Ready", func() {
			createReadyProvider("kagent-ssa", airunwayv1alpha1.AgentProviderBackendCRD,
				airunwayv1alpha1.ModelBindingModeExternalAPI)
			newAgent("agent-ssa", "kagent-ssa")

			// Simulate the framework provider writing its own status fields
			// (phase, runtime, replicas, ProviderReady) under a DISTINCT field
			// owner, exactly as the out-of-tree provider controller would.
			providerWrite := &airunwayv1alpha1.AgentDeployment{
				TypeMeta: metav1.TypeMeta{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "AgentDeployment",
				},
				ObjectMeta: metav1.ObjectMeta{Name: "agent-ssa", Namespace: "default"},
				Status: airunwayv1alpha1.AgentDeploymentStatus{
					Phase: airunwayv1alpha1.AgentPhaseRunning,
					Runtime: &airunwayv1alpha1.AgentRuntimeStatus{
						Address: "http://agent-ssa.default.svc.cluster.local",
					},
					Replicas: &airunwayv1alpha1.AgentReplicaStatus{Desired: 1, Ready: 1, Available: 1},
					Conditions: []metav1.Condition{{
						Type:               airunwayv1alpha1.AgentConditionTypeProviderReady,
						Status:             metav1.ConditionTrue,
						Reason:             "WorkloadReady",
						Message:            "provider reports ready",
						LastTransitionTime: metav1.Now(),
					}},
				},
			}
			Expect(k8sClient.Status().Patch(ctx, providerWrite, client.Apply,
				client.FieldOwner("airunway-agents-kagent"),
				client.ForceOwnership,
			)).To(Succeed())

			// Now the core controller reconciles.
			reconcileOnce("agent-ssa")
			ad := get("agent-ssa")

			// Provider-owned fields MUST survive the core write.
			Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning), "core clobbered provider phase")
			Expect(ad.Status.Runtime).NotTo(BeNil(), "core clobbered provider runtime")
			Expect(ad.Status.Runtime.Address).To(Equal("http://agent-ssa.default.svc.cluster.local"))
			Expect(ad.Status.Replicas).NotTo(BeNil(), "core clobbered provider replicas")
			pr := condition(ad, airunwayv1alpha1.AgentConditionTypeProviderReady)
			Expect(pr).NotTo(BeNil(), "core clobbered provider condition")
			Expect(pr.Status).To(Equal(metav1.ConditionTrue))

			// Core-owned fields MUST be set.
			Expect(ad.Status.Framework).NotTo(BeNil())
			Expect(ad.Status.ModelBinding).NotTo(BeNil())
			Expect(condition(ad, airunwayv1alpha1.AgentConditionTypeFrameworkReady).Status).To(Equal(metav1.ConditionTrue))
			Expect(condition(ad, airunwayv1alpha1.AgentConditionTypeModelBound).Status).To(Equal(metav1.ConditionTrue))

			// With framework + model + ProviderReady all true, Ready aggregates True.
			ready := condition(ad, airunwayv1alpha1.AgentConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue), fmt.Sprintf("expected Ready=True, got %+v", ready))
		})
	})
})
