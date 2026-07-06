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

func TestParseKagentConfig(t *testing.T) {
	raw := &runtime.RawExtension{Raw: []byte(`{"systemPrompt":"be concise","description":"sre agent"}`)}
	cfg := parseKagentConfig(raw)
	if cfg.SystemPrompt != "be concise" {
		t.Errorf("systemPrompt = %q, want %q", cfg.SystemPrompt, "be concise")
	}
	if cfg.Description != "sre agent" {
		t.Errorf("description = %q, want %q", cfg.Description, "sre agent")
	}

	// nil / empty config must not panic and yields an empty config.
	if got := parseKagentConfig(nil); got.SystemPrompt != "" {
		t.Errorf("nil config should be empty, got %+v", got)
	}
}

func TestRenderKagentAgent(t *testing.T) {
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = "k8s-sre"
	ad.Namespace = "agent-poc"

	cfg := kagentConfig{SystemPrompt: "You are an SRE."}
	agent := renderKagentAgent(ad, cfg, "k8s-sre-model")

	if agent.GetAPIVersion() != kagentAPIVersion || agent.GetKind() != "Agent" {
		t.Fatalf("GVK = %s/%s, want %s/Agent", agent.GetAPIVersion(), agent.GetKind(), kagentAPIVersion)
	}
	if agent.GetName() != "k8s-sre" || agent.GetNamespace() != "agent-poc" {
		t.Errorf("name/ns = %s/%s", agent.GetNamespace(), agent.GetName())
	}

	typ, _, _ := unstructured.NestedString(agent.Object, "spec", "type")
	if typ != "Declarative" {
		t.Errorf("spec.type = %q, want Declarative (v1alpha2 shape)", typ)
	}
	// The crux mapping: systemPrompt -> spec.declarative.systemMessage.
	sysMsg, _, _ := unstructured.NestedString(agent.Object, "spec", "declarative", "systemMessage")
	if sysMsg != "You are an SRE." {
		t.Errorf("systemMessage = %q, want the mapped systemPrompt", sysMsg)
	}
	mc, _, _ := unstructured.NestedString(agent.Object, "spec", "declarative", "modelConfig")
	if mc != "k8s-sre-model" {
		t.Errorf("declarative.modelConfig = %q, want k8s-sre-model", mc)
	}
}

func TestRenderKagentAgent_NoSystemPrompt(t *testing.T) {
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = "bare"
	ad.Namespace = "default"

	agent := renderKagentAgent(ad, kagentConfig{}, "bare-model")
	// systemMessage must be absent (not an empty string) when no prompt is set.
	if _, found, _ := unstructured.NestedString(agent.Object, "spec", "declarative", "systemMessage"); found {
		t.Error("systemMessage should be absent when no systemPrompt is configured")
	}
}

func TestRenderKagentModelConfig(t *testing.T) {
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = "k8s-sre"
	ad.Namespace = "agent-poc"

	binding := airunwayv1alpha1.ModelBindingStatus{
		Name:      "default",
		BaseURL:   "https://api.openai.com/v1",
		ModelName: "gpt-4o-mini",
		CredentialsRef: &airunwayv1alpha1.SecretKeyRef{
			Name: "openai-api-key",
			Key:  "api-key",
		},
	}
	mc := renderKagentModelConfig(ad, binding)

	if mc.GetKind() != "ModelConfig" {
		t.Fatalf("kind = %s, want ModelConfig", mc.GetKind())
	}
	if mc.GetName() != "k8s-sre-model" {
		t.Errorf("name = %q, want k8s-sre-model", mc.GetName())
	}
	provider, _, _ := unstructured.NestedString(mc.Object, "spec", "provider")
	if provider != "OpenAI" {
		t.Errorf("provider = %q, want OpenAI", provider)
	}
	model, _, _ := unstructured.NestedString(mc.Object, "spec", "model")
	if model != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini", model)
	}
	baseURL, _, _ := unstructured.NestedString(mc.Object, "spec", "openAI", "baseUrl")
	if baseURL != "https://api.openai.com/v1" {
		t.Errorf("openAI.baseUrl = %q, want the binding base URL", baseURL)
	}
	secretRef, _, _ := unstructured.NestedString(mc.Object, "spec", "apiKeySecretRef")
	if secretRef != "openai-api-key" {
		t.Errorf("apiKeySecretRef = %q, want openai-api-key", secretRef)
	}
}

func TestRenderKagentModelConfig_InClusterEndpoint(t *testing.T) {
	// deploymentRef/gateway bindings arrive as an in-cluster base URL with no
	// credentials; they must still render as an OpenAI-compatible ModelConfig.
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = "local"
	ad.Namespace = "default"

	binding := airunwayv1alpha1.ModelBindingStatus{
		Name:      "default",
		BaseURL:   "http://my-model.default.svc.cluster.local:80/v1",
		ModelName: "llama",
	}
	mc := renderKagentModelConfig(ad, binding)

	baseURL, _, _ := unstructured.NestedString(mc.Object, "spec", "openAI", "baseUrl")
	if baseURL != "http://my-model.default.svc.cluster.local:80/v1" {
		t.Errorf("openAI.baseUrl = %q", baseURL)
	}
	if _, found, _ := unstructured.NestedString(mc.Object, "spec", "apiKeySecretRef"); found {
		t.Error("apiKeySecretRef should be absent when the binding has no credentials")
	}
}

// --- envtest reconcile specs -----------------------------------------------

var _ = Describe("Kagent crd provider", func() {
	ctx := context.Background()

	agentGVK := kagentAgentGVK

	makeReadyKagentProvider := func() {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: KagentFrameworkName},
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

	makeKagentAgent := func(name string) *airunwayv1alpha1.AgentDeployment {
		cfg, _ := json.Marshal(kagentConfig{SystemPrompt: "You are a Kubernetes SRE assistant."})
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: KagentFrameworkName},
				Config:    &runtime.RawExtension{Raw: cfg},
				Models: []airunwayv1alpha1.ModelBinding{{
					Name: "default",
					ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
						Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
						BaseURL:   "https://api.openai.com/v1",
						ModelName: "gpt-4o-mini",
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })
		return ad
	}

	reconcileCore := func(name string) {
		r := &AgentDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	reconcileKagent := func(name string) {
		r := &KagentProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	getAgent := func(name string) *airunwayv1alpha1.AgentDeployment {
		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, out)).To(Succeed())
		return out
	}

	It("waits for core bindings before rendering", func() {
		makeReadyKagentProvider()
		makeKagentAgent("kagent-waiting")

		// Provider runs BEFORE core has resolved bindings.
		reconcileKagent("kagent-waiting")
		ad := getAgent("kagent-waiting")

		pr := meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(pr).NotTo(BeNil())
		Expect(pr.Status).To(Equal(metav1.ConditionFalse))
		Expect(pr.Reason).To(Equal("WaitingForBindings"))

		// No kagent Agent should have been created yet.
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(agentGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "kagent-waiting", Namespace: "default"}, got)
		Expect(err).To(HaveOccurred())
	})

	It("renders the kagent Agent + ModelConfig once bindings are resolved", func() {
		makeReadyKagentProvider()
		makeKagentAgent("kagent-render")

		// Core resolves bindings first, then the provider renders.
		reconcileCore("kagent-render")
		reconcileKagent("kagent-render")

		By("creating a kagent Agent with the mapped system prompt and model config ref")
		agent := &unstructured.Unstructured{}
		agent.SetGroupVersionKind(agentGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "kagent-render", Namespace: "default"}, agent)).To(Succeed())

		sysMsg, _, _ := unstructured.NestedString(agent.Object, "spec", "declarative", "systemMessage")
		Expect(sysMsg).To(Equal("You are a Kubernetes SRE assistant."))
		mc, _, _ := unstructured.NestedString(agent.Object, "spec", "declarative", "modelConfig")
		Expect(mc).To(Equal("kagent-render-model"))

		// Owner reference for GC.
		owners := agent.GetOwnerReferences()
		Expect(owners).To(HaveLen(1))
		Expect(owners[0].Kind).To(Equal("AgentDeployment"))
		Expect(owners[0].Name).To(Equal("kagent-render"))

		By("creating a kagent ModelConfig pointed at the resolved base URL")
		modelConfig := &unstructured.Unstructured{}
		modelConfig.SetGroupVersionKind(kagentModelConfigGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "kagent-render-model", Namespace: "default"}, modelConfig)).To(Succeed())
		baseURL, _, _ := unstructured.NestedString(modelConfig.Object, "spec", "openAI", "baseUrl")
		Expect(baseURL).To(Equal("https://api.openai.com/v1"))

		By("reporting provider status without clobbering core status")
		ad := getAgent("kagent-render")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.WorkloadRef.Kind).To(Equal("Agent"))
		// Core-owned fields survive.
		Expect(ad.Status.ModelBindings).To(HaveLen(1))
		Expect(meta.IsStatusConditionTrue(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeModelBound)).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeFrameworkReady)).To(BeTrue())
	})

	It("reflects the kagent Agent's readiness into ProviderReady", func() {
		makeReadyKagentProvider()
		makeKagentAgent("kagent-ready")

		reconcileCore("kagent-ready")
		reconcileKagent("kagent-ready")

		// Before the kagent Agent reports Ready, the provider stays Deploying.
		ad := getAgent("kagent-ready")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady).Status).
			To(Equal(metav1.ConditionFalse))

		By("simulating the kagent operator marking the Agent Ready=True")
		agent := &unstructured.Unstructured{}
		agent.SetGroupVersionKind(agentGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "kagent-ready", Namespace: "default"}, agent)).To(Succeed())
		Expect(unstructured.SetNestedSlice(agent.Object, []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True", "reason": "AgentRunning", "message": "ok",
				"lastTransitionTime": metav1.Now().Format("2006-01-02T15:04:05Z07:00")},
		}, "status", "conditions")).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, agent)).To(Succeed())

		By("re-reconciling: ProviderReady flips True and phase becomes Running")
		reconcileKagent("kagent-ready")
		ad = getAgent("kagent-ready")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning))
		pr := meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(pr.Status).To(Equal(metav1.ConditionTrue))
		Expect(pr.Reason).To(Equal("AgentReady"))
	})
})
