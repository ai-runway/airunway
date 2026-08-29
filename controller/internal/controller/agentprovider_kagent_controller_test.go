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
	"fmt"
	"reflect"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
)

type recordingKagentResourceWriteClient struct {
	client.Client
	writes     map[schema.GroupVersionKind]int
	rejectVerb string
	rejectGVK  schema.GroupVersionKind
	rejectErr  error
}

func (c *recordingKagentResourceWriteClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	c.record(obj)
	if c.rejectVerb == "create" && obj.GetObjectKind().GroupVersionKind() == c.rejectGVK {
		return c.rejectErr
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *recordingKagentResourceWriteClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	c.record(obj)
	if c.rejectVerb == "patch" && obj.GetObjectKind().GroupVersionKind() == c.rejectGVK {
		return c.rejectErr
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *recordingKagentResourceWriteClient) record(obj client.Object) {
	if c.writes == nil {
		c.writes = make(map[schema.GroupVersionKind]int)
	}
	c.writes[obj.GetObjectKind().GroupVersionKind()]++
}

func (c *recordingKagentResourceWriteClient) writesFor(gvk schema.GroupVersionKind) int {
	return c.writes[gvk]
}

// --- Pure render-function unit tests (no cluster) --------------------------

func TestParseKagentConfig(t *testing.T) {
	raw := &runtime.RawExtension{Raw: []byte(`{
		"systemPrompt":"be concise",
		"description":"sre agent",
		"runtime":"go",
		"tools":[{
			"type":"McpServer",
			"mcpServer":{
				"apiGroup":"kagent.dev",
				"kind":"RemoteMCPServer",
				"name":"readonly-kubernetes",
				"toolNames":["k8s_get_resources"]
			}
		}]
	}`)}
	cfg, err := parseKagentConfig(raw)
	if err != nil {
		t.Fatalf("parseKagentConfig: %v", err)
	}
	if cfg.SystemPrompt != "be concise" {
		t.Errorf("systemPrompt = %q, want %q", cfg.SystemPrompt, "be concise")
	}
	if cfg.Description != "sre agent" {
		t.Errorf("description = %q, want %q", cfg.Description, "sre agent")
	}
	if cfg.Runtime != "go" {
		t.Errorf("runtime = %q, want go", cfg.Runtime)
	}
	if len(cfg.Tools) != 1 || cfg.Tools[0].MCPServer == nil || cfg.Tools[0].MCPServer.Name != "readonly-kubernetes" {
		t.Fatalf("tools = %#v, want the configured MCP server", cfg.Tools)
	}

	// nil / empty config must not panic and yields an empty config.
	if got, err := parseKagentConfig(nil); err != nil || got.SystemPrompt != "" {
		t.Errorf("nil config should be empty, got %+v", got)
	}
	for _, valid := range []string{"", "python", "go"} {
		raw := &runtime.RawExtension{Raw: []byte(fmt.Sprintf(`{"runtime":%q}`, valid))}
		if _, err := parseKagentConfig(raw); err != nil {
			t.Errorf("runtime %q rejected: %v", valid, err)
		}
	}
	if _, err := parseKagentConfig(&runtime.RawExtension{Raw: []byte(`{"runtime":"rust"}`)}); err == nil {
		t.Fatal("undocumented kagent runtime must be rejected")
	}
	for name, config := range map[string]string{
		"unsupported type": `{"tools":[{"type":"Agent","agent":{"name":"helper"}}]}`,
		"missing server":   `{"tools":[{"type":"McpServer"}]}`,
		"missing name":     `{"tools":[{"type":"McpServer","mcpServer":{}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseKagentConfig(&runtime.RawExtension{Raw: []byte(config)}); err == nil {
				t.Fatalf("config %s must be rejected", config)
			}
		})
	}
}

func TestRenderKagentAgent(t *testing.T) {
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = "k8s-sre"
	ad.Namespace = "agent-poc"

	cfg := kagentConfig{
		SystemPrompt: "You are an SRE.",
		Tools: []kagentToolRef{{
			Type: "McpServer",
			MCPServer: &kagentMCPServerToolRef{
				APIGroup:        "kagent.dev",
				Kind:            "RemoteMCPServer",
				Name:            "readonly-kubernetes",
				Namespace:       "agent-tools",
				ToolNames:       []string{"k8s_get_resources", "k8s_get_events"},
				RequireApproval: []string{"k8s_get_events"},
			},
		}},
	}
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
	tools, found, err := unstructured.NestedSlice(agent.Object, "spec", "declarative", "tools")
	if err != nil || !found || len(tools) != 1 {
		t.Fatalf("declarative.tools = %#v, found=%v, err=%v", tools, found, err)
	}
	mcpServer, found, err := unstructured.NestedMap(tools[0].(map[string]interface{}), "mcpServer")
	if err != nil || !found {
		t.Fatalf("declarative.tools[0].mcpServer missing: err=%v", err)
	}
	if mcpServer["name"] != "readonly-kubernetes" || mcpServer["namespace"] != "agent-tools" {
		t.Errorf("mcpServer = %#v, want configured name and namespace", mcpServer)
	}
	if got := mcpServer["toolNames"]; !reflect.DeepEqual(got, []interface{}{"k8s_get_resources", "k8s_get_events"}) {
		t.Errorf("mcpServer.toolNames = %#v", got)
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
	if _, found, _ := unstructured.NestedSlice(agent.Object, "spec", "declarative", "tools"); found {
		t.Error("tools should be absent when none are configured")
	}
}

func TestRenderKagentModelConfig(t *testing.T) {
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = "k8s-sre"
	ad.Namespace = "agent-poc"

	binding := airunwayv1alpha1.ModelBindingStatus{
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
	secretRef, _, _ := unstructured.NestedString(mc.Object, "spec", "apiKeySecret")
	if secretRef != "openai-api-key" {
		t.Errorf("apiKeySecret = %q, want openai-api-key", secretRef)
	}
}

func TestRenderKagentModelConfig_InClusterEndpoint(t *testing.T) {
	// deploymentRef/gateway bindings arrive as an in-cluster base URL with no
	// credentials; they must still render as an OpenAI-compatible ModelConfig.
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = "local"
	ad.Namespace = "default"

	binding := airunwayv1alpha1.ModelBindingStatus{
		BaseURL:   "http://my-model.default.svc.cluster.local:80/v1",
		ModelName: "llama",
	}
	mc := renderKagentModelConfig(ad, binding)

	baseURL, _, _ := unstructured.NestedString(mc.Object, "spec", "openAI", "baseUrl")
	if baseURL != "http://my-model.default.svc.cluster.local:80/v1" {
		t.Errorf("openAI.baseUrl = %q", baseURL)
	}
	if _, found, _ := unstructured.NestedString(mc.Object, "spec", "apiKeySecret"); found {
		t.Error("apiKeySecret should be absent when the binding has no credentials")
	}
}

func TestRenderKagentModelConfig_AzureOpenAIType(t *testing.T) {
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = "azure"
	ad.Namespace = "default"
	binding := airunwayv1alpha1.ModelBindingStatus{
		BindingMode: airunwayv1alpha1.ModelBindingModeExternalAPI,
		APIType:     airunwayv1alpha1.ExternalAPITypeAzureOpenAI,
		BaseURL:     "https://my-azure.openai.azure.com",
		ModelName:   "gpt-4.1",
	}
	mc := renderKagentModelConfig(ad, binding)

	provider, _, _ := unstructured.NestedString(mc.Object, "spec", "provider")
	if provider != "AzureOpenAI" {
		t.Fatalf("provider = %q, want AzureOpenAI", provider)
	}
	azureEndpoint, _, _ := unstructured.NestedString(mc.Object, "spec", "azureOpenAI", "azureEndpoint")
	if azureEndpoint != "https://my-azure.openai.azure.com" {
		t.Fatalf("azureOpenAI.azureEndpoint = %q", azureEndpoint)
	}
	azureDeployment, _, _ := unstructured.NestedString(mc.Object, "spec", "azureOpenAI", "azureDeployment")
	if azureDeployment != "gpt-4.1" {
		t.Fatalf("azureOpenAI.azureDeployment = %q, want gpt-4.1", azureDeployment)
	}
	apiVersion, _, _ := unstructured.NestedString(mc.Object, "spec", "azureOpenAI", "apiVersion")
	if apiVersion != "2024-02-01" {
		t.Fatalf("azureOpenAI.apiVersion = %q, want 2024-02-01", apiVersion)
	}
}

func TestRenderKagentModelConfig_AnthropicType(t *testing.T) {
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = "anthropic"
	ad.Namespace = "default"
	binding := airunwayv1alpha1.ModelBindingStatus{
		BindingMode: airunwayv1alpha1.ModelBindingModeExternalAPI,
		APIType:     airunwayv1alpha1.ExternalAPITypeAnthropic,
		BaseURL:     "https://api.anthropic.com",
		ModelName:   "claude-3-5-sonnet",
	}
	mc := renderKagentModelConfig(ad, binding)

	provider, _, _ := unstructured.NestedString(mc.Object, "spec", "provider")
	if provider != "Anthropic" {
		t.Fatalf("provider = %q, want Anthropic", provider)
	}
	baseURL, _, _ := unstructured.NestedString(mc.Object, "spec", "anthropic", "baseUrl")
	if baseURL != "https://api.anthropic.com" {
		t.Fatalf("anthropic.baseUrl = %q", baseURL)
	}
}

// --- envtest reconcile specs -----------------------------------------------

var _ = Describe("Kagent crd provider", func() {
	ctx := context.Background()

	agentGVK := kagentAgentGVK

	makeReadyKagentProvider := func(modes ...airunwayv1alpha1.ModelBindingMode) {
		if len(modes) == 0 {
			modes = []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI}
		}
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: KagentFrameworkName},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           airunwayv1alpha1.AgentProviderBackendCRD,
					ModelBindingModes: modes,
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
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })
		return ad
	}

	reconcileCore := func(name string) {
		r := newCredentialAuthorizedAgentDeploymentReconciler(k8sClient)
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
	getUpstream := func(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
		out := &unstructured.Unstructured{}
		out.SetGroupVersionKind(gvk)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, out)).To(Succeed())
		return out
	}
	finishUpstreamDelete := func(gvk schema.GroupVersionKind, name string) {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(gvk)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, live)
		if apierrors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(live.GetDeletionTimestamp().IsZero()).To(BeFalse())
		live.SetFinalizers(nil)
		if err := k8sClient.Update(ctx, live); err != nil {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
		Eventually(func() bool {
			probe := &unstructured.Unstructured{}
			probe.SetGroupVersionKind(gvk)
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: name, Namespace: "default"}, probe))
		}).Should(BeTrue())
	}
	assertRejectedModelConfigWriteCleansTopology := func(name, verb string) {
		makeReadyKagentProvider()
		makeKagentAgent(name)
		reconcileCore(name)
		if verb == "patch" {
			reconcileKagent(name)
		}

		rejection := "injected definitive ModelConfig " + verb + " rejection"
		writeClient := &recordingKagentResourceWriteClient{
			Client:     k8sClient,
			rejectVerb: verb,
			rejectGVK:  kagentModelConfigGVK,
			rejectErr:  apierrors.NewBadRequest(rejection),
		}
		r := &KagentProviderReconciler{Client: writeClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: name, Namespace: "default",
		}})
		Expect(err).To(MatchError(ContainSubstring(rejection)))

		finishUpstreamDelete(kagentModelConfigGVK, name+"-model")
		finishUpstreamDelete(kagentAgentGVK, name)
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
		Expect(ad.Status.ModelBinding).NotTo(BeNil())
		Expect(meta.IsStatusConditionTrue(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeModelBound)).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeFrameworkReady)).To(BeTrue())
	})

	It("creates a managed keyless Secret for deploymentRef bindings", func() {
		makeReadyKagentProvider(airunwayv1alpha1.ModelBindingModeDeploymentRef)

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
			ObjectMeta: metav1.ObjectMeta{Name: "kagent-keyless", Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: KagentFrameworkName},
				Model: airunwayv1alpha1.ModelBinding{
					DeploymentRef: &airunwayv1alpha1.ModelDeploymentBinding{Name: "demo-model"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })

		reconcileCore("kagent-keyless")
		reconcileKagent("kagent-keyless")

		secretName := agentprovider.KeylessCredentialSecretName("kagent-keyless")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, secret)).To(Succeed())
		Expect(secret.Data).To(HaveKey(agentprovider.KeylessCredentialKey))
		Expect(string(secret.Data[agentprovider.KeylessCredentialKey])).To(Equal(agentprovider.KeylessCredentialValue))
		Expect(secret.GetOwnerReferences()).To(HaveLen(1))
		Expect(secret.GetOwnerReferences()[0].Name).To(Equal("kagent-keyless"))

		modelConfig := &unstructured.Unstructured{}
		modelConfig.SetGroupVersionKind(kagentModelConfigGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "kagent-keyless-model", Namespace: "default"}, modelConfig)).To(Succeed())
		apiKeySecret, _, _ := unstructured.NestedString(modelConfig.Object, "spec", "apiKeySecret")
		Expect(apiKeySecret).To(Equal(secretName))
		apiKeySecretKey, _, _ := unstructured.NestedString(modelConfig.Object, "spec", "apiKeySecretKey")
		Expect(apiKeySecretKey).To(Equal(agentprovider.KeylessCredentialKey))
	})

	It("stops the Agent after a definitive ModelConfig create rejection", func() {
		assertRejectedModelConfigWriteCleansTopology("kagent-model-create-rejected", "create")
	})

	It("stops the Agent and stale ModelConfig after a definitive ModelConfig patch rejection", func() {
		assertRejectedModelConfigWriteCleansTopology("kagent-model-patch-rejected", "patch")
	})

	It("stops the sibling Agent when the ModelConfig name is owned by another object", func() {
		makeReadyKagentProvider()
		makeKagentAgent("kagent-ownership-conflict")
		reconcileCore("kagent-ownership-conflict")
		reconcileKagent("kagent-ownership-conflict")

		owned := getUpstream(kagentModelConfigGVK, "kagent-ownership-conflict-model")
		foreign := owned.DeepCopy()
		Expect(k8sClient.Delete(ctx, owned)).To(Succeed())
		finishUpstreamDelete(kagentModelConfigGVK, owned.GetName())
		foreign.SetResourceVersion("")
		foreign.SetUID("")
		foreign.SetGeneration(0)
		foreign.SetCreationTimestamp(metav1.Time{})
		foreign.SetDeletionTimestamp(nil)
		foreign.SetDeletionGracePeriodSeconds(nil)
		foreign.SetManagedFields(nil)
		ad := getAgent("kagent-ownership-conflict")
		controller, blockOwnerDeletion := true, true
		forgedOwner := metav1.OwnerReference{
			APIVersion:         airunwayv1alpha1.GroupVersion.String(),
			Kind:               "AgentDeployment",
			Name:               ad.Name + "-forged",
			UID:                ad.UID,
			Controller:         &controller,
			BlockOwnerDeletion: &blockOwnerDeletion,
		}
		foreign.SetOwnerReferences([]metav1.OwnerReference{forgedOwner})
		foreign.SetFinalizers(nil)
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })
		foreignUID := foreign.GetUID()

		recordingClient := &recordingKagentResourceWriteClient{Client: k8sClient}
		r := &KagentProviderReconciler{Client: recordingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: "kagent-ownership-conflict", Namespace: "default",
		}})
		Expect(err).To(HaveOccurred())
		Expect(recordingClient.writesFor(kagentAgentGVK)).To(BeZero(),
			"all rendered resources must be ownership-preflighted before the Agent is written")

		finishUpstreamDelete(kagentAgentGVK, "kagent-ownership-conflict")
		preserved := getUpstream(kagentModelConfigGVK, foreign.GetName())
		Expect(preserved.GetUID()).To(Equal(foreignUID), "the foreign ModelConfig must not be deleted or adopted")
		Expect(preserved.GetOwnerReferences()).To(Equal([]metav1.OwnerReference{forgedOwner}))
		condition := meta.FindStatusCondition(getAgent("kagent-ownership-conflict").Status.Conditions,
			airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("OwnershipConflict"))
	})

	It("does not publish the ModelConfig when the Agent name is owned by another object", func() {
		makeReadyKagentProvider()
		makeKagentAgent("kagent-agent-ownership-conflict")
		reconcileCore("kagent-agent-ownership-conflict")
		reconcileKagent("kagent-agent-ownership-conflict")

		owned := getUpstream(kagentAgentGVK, "kagent-agent-ownership-conflict")
		foreign := owned.DeepCopy()
		Expect(k8sClient.Delete(ctx, owned)).To(Succeed())
		finishUpstreamDelete(kagentAgentGVK, owned.GetName())
		foreign.SetResourceVersion("")
		foreign.SetUID("")
		foreign.SetGeneration(0)
		foreign.SetCreationTimestamp(metav1.Time{})
		foreign.SetDeletionTimestamp(nil)
		foreign.SetDeletionGracePeriodSeconds(nil)
		foreign.SetManagedFields(nil)
		ad := getAgent("kagent-agent-ownership-conflict")
		controller, blockOwnerDeletion := true, true
		forgedOwner := metav1.OwnerReference{
			APIVersion:         airunwayv1alpha1.GroupVersion.String(),
			Kind:               "AgentDeployment",
			Name:               ad.Name + "-forged",
			UID:                ad.UID,
			Controller:         &controller,
			BlockOwnerDeletion: &blockOwnerDeletion,
		}
		foreign.SetOwnerReferences([]metav1.OwnerReference{forgedOwner})
		foreign.SetFinalizers(nil)
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })
		foreignUID := foreign.GetUID()

		recordingClient := &recordingKagentResourceWriteClient{Client: k8sClient}
		r := &KagentProviderReconciler{Client: recordingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: "kagent-agent-ownership-conflict", Namespace: "default",
		}})
		Expect(err).To(HaveOccurred())
		Expect(recordingClient.writesFor(kagentModelConfigGVK)).To(BeZero(),
			"a foreign Agent must be detected before credential-bearing ModelConfig writes")

		finishUpstreamDelete(kagentModelConfigGVK, "kagent-agent-ownership-conflict-model")
		preserved := getUpstream(kagentAgentGVK, foreign.GetName())
		Expect(preserved.GetUID()).To(Equal(foreignUID), "the foreign Agent must not be deleted or adopted")
		Expect(preserved.GetOwnerReferences()).To(Equal([]metav1.OwnerReference{forgedOwner}))
		condition := meta.FindStatusCondition(getAgent("kagent-agent-ownership-conflict").Status.Conditions,
			airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("OwnershipConflict"))
	})

	It("cleans up and releases status for a pre-validation framework handoff", func() {
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "kagent-framework-handoff", Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "successor"},
				Model: airunwayv1alpha1.ModelBinding{ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
					Type: airunwayv1alpha1.ExternalAPITypeOpenAI, BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
				}},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })

		binding := airunwayv1alpha1.ModelBindingStatus{
			BindingMode: airunwayv1alpha1.ModelBindingModeExternalAPI,
			BaseURL:     "https://api.openai.com/v1",
			ModelName:   "gpt-4o-mini",
		}
		modelConfig := renderKagentModelConfig(ad, binding)
		agent := renderKagentAgent(ad, kagentConfig{SystemPrompt: "old provider"}, modelConfig.GetName())
		for _, obj := range []*unstructured.Unstructured{modelConfig, agent} {
			Expect(agentprovider.ApplyOwned(ctx, k8sClient, k8sClient, k8sClient.Scheme(), ad,
				obj, KagentFieldOwner, true)).To(Succeed())
		}
		Expect(agentprovider.ApplyOwnedStatus(ctx, k8sClient, ad, KagentFieldOwner,
			airunwayv1alpha1.AgentPhaseRunning,
			&airunwayv1alpha1.AgentRuntimeStatus{WorkloadRef: &airunwayv1alpha1.RuntimeWorkloadRef{
				APIVersion: kagentAPIVersion, Kind: "Agent", Name: ad.Name, Namespace: ad.Namespace,
			}}, nil, metav1.ConditionTrue, "AgentReady", "old kagent provider is running")).To(Succeed())

		r := &KagentProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: ad.Name, Namespace: ad.Namespace,
		}})
		Expect(err).NotTo(HaveOccurred())
		finishUpstreamDelete(kagentModelConfigGVK, modelConfig.GetName())
		finishUpstreamDelete(kagentAgentGVK, agent.GetName())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: ad.Name, Namespace: ad.Namespace,
		}})
		Expect(err).NotTo(HaveOccurred())
		out := getAgent(ad.Name)
		Expect(out.Status.ProviderOwner).To(BeEmpty())
		Expect(out.Status.Runtime).To(BeNil())
		Expect(meta.FindStatusCondition(out.Status.Conditions,
			airunwayv1alpha1.AgentConditionTypeProviderReady)).To(BeNil())
	})

	It("cleans exact-owned resources after a crash before provider status was written", func() {
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "kagent-framework-crash", Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "successor"},
				Model: airunwayv1alpha1.ModelBinding{ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
					Type: airunwayv1alpha1.ExternalAPITypeOpenAI, BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
				}},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })

		binding := airunwayv1alpha1.ModelBindingStatus{
			BindingMode: airunwayv1alpha1.ModelBindingModeExternalAPI,
			BaseURL:     "https://api.openai.com/v1",
			ModelName:   "gpt-4o-mini",
		}
		modelConfig := renderKagentModelConfig(ad, binding)
		agent := renderKagentAgent(ad, kagentConfig{SystemPrompt: "old provider"}, modelConfig.GetName())
		for _, obj := range []*unstructured.Unstructured{modelConfig, agent} {
			Expect(agentprovider.ApplyOwned(ctx, k8sClient, k8sClient, k8sClient.Scheme(), ad,
				obj, KagentFieldOwner, true)).To(Succeed())
		}
		Expect(getAgent(ad.Name).Status.ProviderOwner).To(BeEmpty())

		r := &KagentProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: ad.Name, Namespace: ad.Namespace,
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
		Expect(getAgent(ad.Name).Status.ProviderOwner).To(BeEmpty())

		finishUpstreamDelete(kagentModelConfigGVK, modelConfig.GetName())
		finishUpstreamDelete(kagentAgentGVK, agent.GetName())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: ad.Name, Namespace: ad.Namespace,
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(getAgent(ad.Name).Status.ProviderOwner).To(BeEmpty())
	})

	It("stops prior resources when the managed keyless Secret is replaced", func() {
		makeReadyKagentProvider()
		makeKagentAgent("kagent-keyless-conflict")
		reconcileCore("kagent-keyless-conflict")
		reconcileKagent("kagent-keyless-conflict")

		secretKey := types.NamespacedName{
			Name: agentprovider.KeylessCredentialSecretName("kagent-keyless-conflict"), Namespace: "default",
		}
		managed := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, managed)).To(Succeed())
		modelConfig := getUpstream(kagentModelConfigGVK, "kagent-keyless-conflict-model")
		secretName, _, _ := unstructured.NestedString(modelConfig.Object, "spec", "apiKeySecret")
		Expect(secretName).To(Equal(secretKey.Name))

		Expect(k8sClient.Delete(ctx, managed)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))
		}).Should(BeTrue())
		foreign := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"password": []byte("foreign")},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })
		foreignUID := foreign.UID

		r := &KagentProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: "kagent-keyless-conflict", Namespace: "default",
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
		finishUpstreamDelete(kagentModelConfigGVK, "kagent-keyless-conflict-model")
		finishUpstreamDelete(kagentAgentGVK, "kagent-keyless-conflict")

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: "kagent-keyless-conflict", Namespace: "default",
		}})
		Expect(err).To(HaveOccurred())
		condition := meta.FindStatusCondition(getAgent("kagent-keyless-conflict").Status.Conditions,
			airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("CredentialProvisionFailed"))
		preserved := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, preserved)).To(Succeed())
		Expect(preserved.UID).To(Equal(foreignUID))
		Expect(preserved.OwnerReferences).To(BeEmpty())
		Expect(preserved.Data).To(Equal(map[string][]byte{"password": []byte("foreign")}))
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

	It("removes rendered resources when spec.config becomes invalid", func() {
		makeReadyKagentProvider()
		makeKagentAgent("kagent-invalid-config")
		reconcileCore("kagent-invalid-config")
		reconcileKagent("kagent-invalid-config")

		ad := getAgent("kagent-invalid-config")
		ad.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"systemPrompt":[]}`)}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		reconcileKagent(ad.Name)

		finishForegroundDelete := func(gvk schema.GroupVersionKind, name string) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gvk)
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, obj)
			if apierrors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.GetDeletionTimestamp().IsZero()).To(BeFalse())
			obj.SetFinalizers(nil)
			if err := k8sClient.Update(ctx, obj); err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
			Eventually(func() bool {
				probe := &unstructured.Unstructured{}
				probe.SetGroupVersionKind(gvk)
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, probe))
			}).Should(BeTrue())
		}
		finishForegroundDelete(kagentModelConfigGVK, "kagent-invalid-config-model")
		finishForegroundDelete(kagentAgentGVK, "kagent-invalid-config")

		reconcileKagent(ad.Name)
		out := getAgent(ad.Name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseFailed))
		condition := meta.FindStatusCondition(out.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("InvalidConfig"))
	})
})
