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

type orkaApplyWrite struct {
	verb string
	gvk  schema.GroupVersionKind
	name string
}

type recordingOrkaApplyClient struct {
	client.Client
	writes     []orkaApplyWrite
	rejectVerb string
	rejectGVK  schema.GroupVersionKind
	rejectErr  error
}

func (c *recordingOrkaApplyClient) record(verb string, obj client.Object) {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk != orkaAgentGVK && gvk != orkaProviderGVK {
		return
	}
	c.writes = append(c.writes, orkaApplyWrite{verb: verb, gvk: gvk, name: obj.GetName()})
}

func (c *recordingOrkaApplyClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	c.record("create", obj)
	if c.rejectVerb == "create" && obj.GetObjectKind().GroupVersionKind() == c.rejectGVK {
		return c.rejectErr
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *recordingOrkaApplyClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	c.record("patch", obj)
	if c.rejectVerb == "patch" && obj.GetObjectKind().GroupVersionKind() == c.rejectGVK {
		return c.rejectErr
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *recordingOrkaApplyClient) writesFor(gvk schema.GroupVersionKind) []orkaApplyWrite {
	var writes []orkaApplyWrite
	for _, write := range c.writes {
		if write.gvk == gvk {
			writes = append(writes, write)
		}
	}
	return writes
}

// --- Pure render-function unit tests (no cluster) --------------------------

func orkaAD(name string, ext *airunwayv1alpha1.ExternalAPIBinding) *airunwayv1alpha1.AgentDeployment {
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = name
	ad.Namespace = "default"
	ad.Spec.Framework.Name = OrkaFrameworkName
	ad.Spec.Model = airunwayv1alpha1.ModelBinding{ExternalAPI: ext}
	return ad
}

func TestRenderOrkaProvider(t *testing.T) {
	ad := orkaAD("swarm", &airunwayv1alpha1.ExternalAPIBinding{Type: airunwayv1alpha1.ExternalAPITypeOpenAI})
	binding := airunwayv1alpha1.ModelBindingStatus{
		BindingMode: airunwayv1alpha1.ModelBindingModeExternalAPI,
		BaseURL:     "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
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

func TestRenderOrkaProvider_KeylessUsesManagedSecretName(t *testing.T) {
	// A credential-free binding (keyless in-cluster model via deploymentRef)
	// must still render a valid Orka Provider: the CRD requires spec.secretRef,
	// so the renderer falls back to the deterministic managed no-auth Secret.
	ad := orkaAD("swarm", nil)
	binding := airunwayv1alpha1.ModelBindingStatus{
		BindingMode: airunwayv1alpha1.ModelBindingModeDeploymentRef,
		BaseURL:     "http://demo-llm.default.svc.cluster.local/v1", ModelName: "llama",
	}
	p := renderOrkaProvider(ad, binding)

	secretName, found, _ := unstructured.NestedString(p.Object, "spec", "secretRef", "name")
	expectedName := agentprovider.KeylessCredentialSecretName(ad.Name)
	if !found || secretName != expectedName {
		t.Errorf("secretRef.name = %q (found=%v), want managed secret %q", secretName, found, expectedName)
	}
	secretKey, _, _ := unstructured.NestedString(p.Object, "spec", "secretRef", "key")
	if secretKey != agentprovider.KeylessCredentialKey {
		t.Errorf("secretRef.key = %q, want %q", secretKey, agentprovider.KeylessCredentialKey)
	}
}

func TestRenderOrkaProvider_AzureType(t *testing.T) {
	ad := orkaAD("swarm", &airunwayv1alpha1.ExternalAPIBinding{Type: airunwayv1alpha1.ExternalAPITypeAzureOpenAI})
	binding := airunwayv1alpha1.ModelBindingStatus{
		BindingMode: airunwayv1alpha1.ModelBindingModeExternalAPI, ModelName: "gpt-4.1",
	}
	p := renderOrkaProvider(ad, binding)
	typ, _, _ := unstructured.NestedString(p.Object, "spec", "type")
	if typ != "azure-openai" {
		t.Errorf("type = %q, want azure-openai", typ)
	}
	deploymentName, found, err := unstructured.NestedString(p.Object, "spec", "azure", "deploymentName")
	if err != nil || !found || deploymentName != "gpt-4.1" {
		t.Errorf("azure.deploymentName = %q (found=%v, err=%v), want gpt-4.1", deploymentName, found, err)
	}
}

func TestRenderOrkaAgent(t *testing.T) {
	ad := orkaAD("swarm", &airunwayv1alpha1.ExternalAPIBinding{Type: airunwayv1alpha1.ExternalAPITypeOpenAI})
	binding := airunwayv1alpha1.ModelBindingStatus{ModelName: "gpt-4o-mini"}
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
				Model: airunwayv1alpha1.ModelBinding{
					ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
						Type: airunwayv1alpha1.ExternalAPITypeOpenAI, BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })
	}

	reconcileCore := func(name string) {
		r := newCredentialAuthorizedAgentDeploymentReconciler(k8sClient)
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
	assertRejectedProviderWriteCleansTopology := func(name, verb string) {
		makeOrkaProviderConfig()
		makeOrkaAgent(name)
		reconcileCore(name)
		if verb == "patch" {
			reconcileOrka(name)
		}

		rejection := "injected definitive Provider " + verb + " rejection"
		writeClient := &recordingOrkaApplyClient{
			Client:     k8sClient,
			rejectVerb: verb,
			rejectGVK:  orkaProviderGVK,
			rejectErr:  apierrors.NewBadRequest(rejection),
		}
		r := &OrkaProviderReconciler{Client: writeClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: name, Namespace: "default",
		}})
		Expect(err).To(MatchError(ContainSubstring(rejection)))

		finishUpstreamDelete(orkaProviderGVK, name+"-provider")
		finishUpstreamDelete(orkaAgentGVK, name)
	}
	assertForeignResourceBlocksSiblingWrite := func(
		name string,
		foreignGVK schema.GroupVersionKind,
		siblingGVK schema.GroupVersionKind,
	) {
		makeOrkaProviderConfig()
		makeOrkaAgent(name)
		reconcileCore(name)

		ad := getAgent(name)
		Expect(ad.Status.ModelBinding).NotTo(BeNil())
		binding := *ad.Status.ModelBinding
		provider := renderOrkaProvider(ad, binding)
		var foreign *unstructured.Unstructured
		switch foreignGVK {
		case orkaAgentGVK:
			foreign = renderOrkaAgent(ad, orkaAgentConfig{SystemPrompt: "foreign"}, binding, provider.GetName())
		case orkaProviderGVK:
			foreign = provider
		default:
			Fail("unsupported foreign Orka GVK")
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

		recordingClient := &recordingOrkaApplyClient{Client: k8sClient}
		r := &OrkaProviderReconciler{
			Client: recordingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: name, Namespace: "default",
		}})
		Expect(err).To(HaveOccurred())
		Expect(recordingClient.writesFor(siblingGVK)).To(BeEmpty())
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
		// Simulate what the real Orka operator writes on the Agent status. The
		// real Agent CRD requires status.activeTasks, so set it too (the stub
		// CRD did not enforce this; the real schema does).
		Expect(unstructured.SetNestedField(agent.Object, int64(0), "status", "activeTasks")).To(Succeed())
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

	It("stops the Agent after a definitive Provider create rejection", func() {
		assertRejectedProviderWriteCleansTopology("orka-provider-create-rejected", "create")
	})

	It("stops the Agent and stale Provider after a definitive Provider patch rejection", func() {
		assertRejectedProviderWriteCleansTopology("orka-provider-patch-rejected", "patch")
	})

	It("does not write the credential Provider when the Agent name is foreign", func() {
		assertForeignResourceBlocksSiblingWrite("orka-foreign-agent", orkaAgentGVK, orkaProviderGVK)
	})

	It("does not write the Agent when the Provider name is foreign", func() {
		assertForeignResourceBlocksSiblingWrite("orka-foreign-provider", orkaProviderGVK, orkaAgentGVK)
	})

	It("stops the sibling Agent when the Provider name is owned by another object", func() {
		makeOrkaProviderConfig()
		makeOrkaAgent("orka-ownership-conflict")
		reconcileCore("orka-ownership-conflict")
		reconcileOrka("orka-ownership-conflict")

		owned := getUpstream(orkaProviderGVK, "orka-ownership-conflict-provider")
		foreign := owned.DeepCopy()
		Expect(k8sClient.Delete(ctx, owned)).To(Succeed())
		finishUpstreamDelete(orkaProviderGVK, owned.GetName())
		foreign.SetResourceVersion("")
		foreign.SetUID("")
		foreign.SetGeneration(0)
		foreign.SetCreationTimestamp(metav1.Time{})
		foreign.SetDeletionTimestamp(nil)
		foreign.SetDeletionGracePeriodSeconds(nil)
		foreign.SetManagedFields(nil)
		ad := getAgent("orka-ownership-conflict")
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

		r := &OrkaProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: "orka-ownership-conflict", Namespace: "default",
		}})
		Expect(err).To(HaveOccurred())

		finishUpstreamDelete(orkaAgentGVK, "orka-ownership-conflict")
		preserved := getUpstream(orkaProviderGVK, foreign.GetName())
		Expect(preserved.GetUID()).To(Equal(foreignUID), "the foreign Provider must not be deleted or adopted")
		Expect(preserved.GetOwnerReferences()).To(Equal([]metav1.OwnerReference{forgedOwner}))
		condition := meta.FindStatusCondition(getAgent("orka-ownership-conflict").Status.Conditions,
			airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("OwnershipConflict"))
	})

	It("cleans up and releases status for a pre-validation framework handoff", func() {
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "orka-framework-handoff", Namespace: "default"},
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
		provider := renderOrkaProvider(ad, binding)
		agent := renderOrkaAgent(ad, orkaAgentConfig{SystemPrompt: "old provider"}, binding, provider.GetName())
		for _, obj := range []*unstructured.Unstructured{provider, agent} {
			Expect(agentprovider.ApplyOwned(ctx, k8sClient, k8sClient, k8sClient.Scheme(), ad,
				obj, OrkaFieldOwner, true)).To(Succeed())
		}
		Expect(agentprovider.ApplyOwnedStatus(ctx, k8sClient, ad, OrkaFieldOwner,
			airunwayv1alpha1.AgentPhaseRunning,
			&airunwayv1alpha1.AgentRuntimeStatus{WorkloadRef: &airunwayv1alpha1.RuntimeWorkloadRef{
				APIVersion: orkaAPIVersion, Kind: "Agent", Name: ad.Name, Namespace: ad.Namespace,
			}}, nil, metav1.ConditionTrue, "AgentReady", "old Orka provider is running")).To(Succeed())

		r := &OrkaProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: ad.Name, Namespace: ad.Namespace,
		}})
		Expect(err).NotTo(HaveOccurred())
		finishUpstreamDelete(orkaProviderGVK, provider.GetName())
		finishUpstreamDelete(orkaAgentGVK, agent.GetName())

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
			ObjectMeta: metav1.ObjectMeta{Name: "orka-framework-crash", Namespace: "default"},
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
		provider := renderOrkaProvider(ad, binding)
		agent := renderOrkaAgent(ad, orkaAgentConfig{SystemPrompt: "old provider"}, binding, provider.GetName())
		for _, obj := range []*unstructured.Unstructured{provider, agent} {
			Expect(agentprovider.ApplyOwned(ctx, k8sClient, k8sClient, k8sClient.Scheme(), ad,
				obj, OrkaFieldOwner, true)).To(Succeed())
		}
		Expect(getAgent(ad.Name).Status.ProviderOwner).To(BeEmpty())

		r := &OrkaProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: ad.Name, Namespace: ad.Namespace,
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
		Expect(getAgent(ad.Name).Status.ProviderOwner).To(BeEmpty())

		finishUpstreamDelete(orkaProviderGVK, provider.GetName())
		finishUpstreamDelete(orkaAgentGVK, agent.GetName())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: ad.Name, Namespace: ad.Namespace,
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(getAgent(ad.Name).Status.ProviderOwner).To(BeEmpty())
	})

	It("stops prior resources when the managed keyless Secret is replaced", func() {
		makeOrkaProviderConfig()
		makeOrkaAgent("orka-keyless-conflict")
		reconcileCore("orka-keyless-conflict")
		reconcileOrka("orka-keyless-conflict")

		secretKey := types.NamespacedName{
			Name: agentprovider.KeylessCredentialSecretName("orka-keyless-conflict"), Namespace: "default",
		}
		managed := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, managed)).To(Succeed())
		provider := getUpstream(orkaProviderGVK, "orka-keyless-conflict-provider")
		secretName, _, _ := unstructured.NestedString(provider.Object, "spec", "secretRef", "name")
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

		r := &OrkaProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: "orka-keyless-conflict", Namespace: "default",
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
		finishUpstreamDelete(orkaProviderGVK, "orka-keyless-conflict-provider")
		finishUpstreamDelete(orkaAgentGVK, "orka-keyless-conflict")

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: "orka-keyless-conflict", Namespace: "default",
		}})
		Expect(err).To(HaveOccurred())
		condition := meta.FindStatusCondition(getAgent("orka-keyless-conflict").Status.Conditions,
			airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("CredentialProvisionFailed"))
		preserved := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, preserved)).To(Succeed())
		Expect(preserved.UID).To(Equal(foreignUID))
		Expect(preserved.OwnerReferences).To(BeEmpty())
		Expect(preserved.Data).To(Equal(map[string][]byte{"password": []byte("foreign")}))
	})

	It("removes rendered resources when spec.config becomes invalid", func() {
		makeOrkaProviderConfig()
		makeOrkaAgent("orka-invalid-config")
		reconcileCore("orka-invalid-config")
		reconcileOrka("orka-invalid-config")

		ad := getAgent("orka-invalid-config")
		ad.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"systemPrompt":[]}`)}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		reconcileOrka(ad.Name)

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
		finishForegroundDelete(orkaProviderGVK, "orka-invalid-config-provider")
		finishForegroundDelete(orkaAgentGVK, "orka-invalid-config")

		reconcileOrka(ad.Name)
		out := getAgent(ad.Name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseFailed))
		condition := meta.FindStatusCondition(out.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("InvalidConfig"))
	})
})
