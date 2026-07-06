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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

// --- Pure render-function unit tests (no cluster) --------------------------

func orkaAD(name string, ext *airunwayv1alpha1.ExternalAPIBinding) *airunwayv1alpha1.AgentDeployment {
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = name
	ad.Namespace = "default"
	ad.Spec.Framework.Name = OrkaFrameworkName
	ad.Spec.Models = []airunwayv1alpha1.ModelBinding{{Name: "default", ExternalAPI: ext}}
	return ad
}

func TestRenderOrkaProvider(t *testing.T) {
	ad := orkaAD("swarm", &airunwayv1alpha1.ExternalAPIBinding{Type: airunwayv1alpha1.ExternalAPITypeOpenAI})
	binding := airunwayv1alpha1.ModelBindingStatus{
		Name: "default", BindingMode: airunwayv1alpha1.ModelBindingModeExternalAPI,
		BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
		CredentialsRef: &airunwayv1alpha1.SecretKeyRef{Name: "openai-api-key", Key: "api-key"},
	}
	p := renderOrkaProvider(ad, binding)

	if p.GetAPIVersion() != orkaAPIVersion || p.GetKind() != "Provider" {
		t.Fatalf("GVK = %s/%s", p.GetAPIVersion(), p.GetKind())
	}
	if p.GetName() != "swarm-provider" {
		t.Errorf("name = %q", p.GetName())
	}
	typ, _, _ := unstructured.NestedString(p.Object, "spec", "type")
	if typ != "openai" {
		t.Errorf("type = %q, want openai", typ)
	}
	baseURL, _, _ := unstructured.NestedString(p.Object, "spec", "baseURL")
	if baseURL != "https://api.openai.com/v1" {
		t.Errorf("baseURL = %q", baseURL)
	}
	model, _, _ := unstructured.NestedString(p.Object, "spec", "defaultModel")
	if model != "gpt-4o-mini" {
		t.Errorf("defaultModel = %q", model)
	}
	secretName, _, _ := unstructured.NestedString(p.Object, "spec", "secretRef", "name")
	if secretName != "openai-api-key" {
		t.Errorf("secretRef.name = %q", secretName)
	}
}

func TestRenderOrkaProvider_AzureType(t *testing.T) {
	ad := orkaAD("swarm", &airunwayv1alpha1.ExternalAPIBinding{Type: airunwayv1alpha1.ExternalAPITypeAzureOpenAI})
	binding := airunwayv1alpha1.ModelBindingStatus{
		Name: "default", BindingMode: airunwayv1alpha1.ModelBindingModeExternalAPI, ModelName: "gpt-4.1",
	}
	p := renderOrkaProvider(ad, binding)
	typ, _, _ := unstructured.NestedString(p.Object, "spec", "type")
	if typ != "azure-openai" {
		t.Errorf("type = %q, want azure-openai", typ)
	}
}

func TestRenderOrkaAgent(t *testing.T) {
	ad := orkaAD("swarm", &airunwayv1alpha1.ExternalAPIBinding{Type: airunwayv1alpha1.ExternalAPITypeOpenAI})
	binding := airunwayv1alpha1.ModelBindingStatus{Name: "default", ModelName: "gpt-4o-mini"}
	agent := renderOrkaAgent(ad, orkaAgentConfig{SystemPrompt: "coordinate specialists"}, binding, "swarm-provider")

	if agent.GetKind() != "Agent" || agent.GetName() != "swarm" {
		t.Fatalf("kind/name = %s/%s", agent.GetKind(), agent.GetName())
	}
	pref, _, _ := unstructured.NestedString(agent.Object, "spec", "providerRef", "name")
	if pref != "swarm-provider" {
		t.Errorf("providerRef.name = %q", pref)
	}
	model, _, _ := unstructured.NestedString(agent.Object, "spec", "model", "name")
	if model != "gpt-4o-mini" {
		t.Errorf("model.name = %q", model)
	}
	// The crux mapping: systemPrompt -> spec.systemPrompt.inline.
	prompt, _, _ := unstructured.NestedString(agent.Object, "spec", "systemPrompt", "inline")
	if prompt != "coordinate specialists" {
		t.Errorf("systemPrompt.inline = %q", prompt)
	}
}

// --- envtest reconcile specs -----------------------------------------------

var _ = Describe("Orka crd provider", func() {
	ctx := context.Background()

	makeOrkaProviderConfig := func() {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: OrkaFrameworkName},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           airunwayv1alpha1.AgentProviderBackendCRD,
					ModelBindingModes: []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI},
				},
			},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
		apc.Status.Ready = ptrBool(true)
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())
	}

	makeOrkaAgent := func(name string) {
		cfg, _ := json.Marshal(orkaAgentConfig{SystemPrompt: "Decompose the task and coordinate specialists."})
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: OrkaFrameworkName},
				Config:    &runtime.RawExtension{Raw: cfg},
				Models: []airunwayv1alpha1.ModelBinding{{
					Name: "default",
					ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
						Type: airunwayv1alpha1.ExternalAPITypeOpenAI, BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
						CredentialsRef: &airunwayv1alpha1.SecretKeyRef{Name: "openai-api-key", Key: "api-key"},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })
	}

	reconcileCore := func(name string) {
		r := &AgentDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	reconcileOrka := func(name string) {
		r := &OrkaProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	getAgent := func(name string) *airunwayv1alpha1.AgentDeployment {
		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, out)).To(Succeed())
		return out
	}

	It("renders Orka Provider + Agent and reflects readiness", func() {
		makeOrkaProviderConfig()
		makeOrkaAgent("orka-render")

		reconcileCore("orka-render")
		reconcileOrka("orka-render")

		By("creating an Orka Provider from the resolved binding")
		provider := &unstructured.Unstructured{}
		provider.SetGroupVersionKind(orkaProviderGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "orka-render-provider", Namespace: "default"}, provider)).To(Succeed())
		typ, _, _ := unstructured.NestedString(provider.Object, "spec", "type")
		Expect(typ).To(Equal("openai"))

		By("creating an Orka Agent referencing the Provider with the mapped prompt")
		agent := &unstructured.Unstructured{}
		agent.SetGroupVersionKind(orkaAgentGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "orka-render", Namespace: "default"}, agent)).To(Succeed())
		pref, _, _ := unstructured.NestedString(agent.Object, "spec", "providerRef", "name")
		Expect(pref).To(Equal("orka-render-provider"))
		prompt, _, _ := unstructured.NestedString(agent.Object, "spec", "systemPrompt", "inline")
		Expect(prompt).To(Equal("Decompose the task and coordinate specialists."))
		Expect(agent.GetOwnerReferences()).To(HaveLen(1))

		By("staying Deploying until the Orka Agent reports Ready")
		ad := getAgent("orka-render")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))

		By("flipping to Running once the Orka Agent reports Ready=True")
		Expect(unstructured.SetNestedSlice(agent.Object, []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True", "reason": "Ready", "message": "ok",
				"lastTransitionTime": metav1.Now().Format("2006-01-02T15:04:05Z07:00")},
		}, "status", "conditions")).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, agent)).To(Succeed())

		reconcileOrka("orka-render")
		ad = getAgent("orka-render")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning))
		Expect(meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady).Status).
			To(Equal(metav1.ConditionTrue))
	})
})
