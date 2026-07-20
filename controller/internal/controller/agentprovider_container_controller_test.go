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
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

// --- Pure render-function unit tests (no cluster) --------------------------

func containerAD(name string, cfg containerConfig, extra map[string]any) *airunwayv1alpha1.AgentDeployment {
	merged := map[string]any{}
	if cfg.Image != "" {
		merged["image"] = cfg.Image
	}
	for k, v := range extra {
		merged[k] = v
	}
	raw, _ := json.Marshal(merged)

	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = name
	ad.Namespace = "default"
	ad.Spec.Framework.Name = "crewai"
	ad.Spec.Config = &runtime.RawExtension{Raw: raw}
	return ad
}

func TestRenderAgentConfigMap(t *testing.T) {
	ad := containerAD("research", containerConfig{Image: "img:1"}, map[string]any{"systemPrompt": "be brief"})
	cm := renderAgentConfigMap(ad)

	if cm.Name != "research-config" {
		t.Errorf("name = %q, want research-config", cm.Name)
	}
	payload, ok := cm.Data[agentConfigFileName]
	if !ok {
		t.Fatalf("configmap missing %q key", agentConfigFileName)
	}
	// The full spec.config is mounted verbatim so the BYO image reads its
	// framework config from the pinned path.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("agent.json not valid JSON: %v", err)
	}
	if parsed["systemPrompt"] != "be brief" {
		t.Errorf("agent.json missing systemPrompt passthrough: %v", parsed)
	}
}

func TestRenderAgentDeployment_SecurityAndEnv(t *testing.T) {
	ad := containerAD("research", containerConfig{Image: "ghcr.io/x/crewai:poc"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{
		BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
		CredentialsRef: &airunwayv1alpha1.SecretKeyRef{Name: "openai-api-key", Key: "api-key"},
	}
	dep := renderAgentDeployment(ad, containerConfig{Image: "ghcr.io/x/crewai:poc"}, binding, "research-config", false, nil)

	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "ghcr.io/x/crewai:poc" {
		t.Errorf("image = %q", c.Image)
	}

	// Provider-owned hardening (design §7): runAsNonRoot + seccomp at pod level.
	pod := dep.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("pod securityContext.runAsNonRoot must be true (provider-owned hardening)")
	}
	if pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("pod seccompProfile must be RuntimeDefault")
	}
	// Container: drop ALL caps, no privilege escalation, read-only root by default.
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must default to true")
	}
	if len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities must drop ALL, got %v", c.SecurityContext.Capabilities.Drop)
	}

	// Model binding injected as OpenAI-compatible env.
	env := map[string]string{}
	var apiKeyFromSecret bool
	for _, e := range c.Env {
		env[e.Name] = e.Value
		if e.Name == "OPENAI_API_KEY" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			apiKeyFromSecret = true
			if e.ValueFrom.SecretKeyRef.Name != "openai-api-key" {
				t.Errorf("OPENAI_API_KEY secret = %q", e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	if env["OPENAI_BASE_URL"] != "https://api.openai.com/v1" {
		t.Errorf("OPENAI_BASE_URL = %q", env["OPENAI_BASE_URL"])
	}
	if env["AIRUNWAY_AGENT_CONFIG"] != agentConfigMountPath {
		t.Errorf("AIRUNWAY_AGENT_CONFIG = %q, want %q", env["AIRUNWAY_AGENT_CONFIG"], agentConfigMountPath)
	}
	if !apiKeyFromSecret {
		t.Error("OPENAI_API_KEY must be sourced from the binding secret")
	}
}

func TestRenderAgentDeployment_WritableRootForFramework(t *testing.T) {
	ad := containerAD("openclaw", containerConfig{Image: "img:1"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	// writableRoot is a provider-owned decision passed by the reconciler, not a
	// user-facing spec.config field.
	dep := renderAgentDeployment(ad, containerConfig{Image: "img:1"}, binding, "openclaw-config", true, nil)

	roFS := dep.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem
	if roFS == nil || *roFS {
		t.Error("readOnlyRootFilesystem must be false when the framework declares a writable root need")
	}

	// A writable /tmp scratch mount is always provided regardless of root FS.
	var hasTmp bool
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/tmp" {
			hasTmp = true
		}
	}
	if !hasTmp {
		t.Error("a writable /tmp mount must always be present")
	}
}

func TestRenderAgentDeployment_KeylessBindingInjectsLiteralAPIKey(t *testing.T) {
	ad := containerAD("keyless", containerConfig{Image: "img:1"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{
		BaseURL:   "http://demo-model.default.svc.cluster.local:80/v1",
		ModelName: "llama-3.2-1b-instruct",
	}
	dep := renderAgentDeployment(ad, containerConfig{Image: "img:1"}, binding, "keyless-config", false, nil)

	var apiKey *corev1.EnvVar
	for i := range dep.Spec.Template.Spec.Containers[0].Env {
		if dep.Spec.Template.Spec.Containers[0].Env[i].Name == "OPENAI_API_KEY" {
			apiKey = &dep.Spec.Template.Spec.Containers[0].Env[i]
			break
		}
	}
	if apiKey == nil {
		t.Fatal("OPENAI_API_KEY env var was not rendered")
	}
	if apiKey.ValueFrom != nil {
		t.Fatalf("OPENAI_API_KEY should be a literal for keyless bindings, got ValueFrom=%+v", apiKey.ValueFrom)
	}
	if apiKey.Value != keylessCredentialValue {
		t.Fatalf("OPENAI_API_KEY = %q, want %q", apiKey.Value, keylessCredentialValue)
	}
}

func TestRenderAgentDeployment_AppliesSecurityOverrides(t *testing.T) {
	ad := containerAD("override", containerConfig{Image: "img:1"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{
		BaseURL:   "http://demo-model.default.svc.cluster.local:80/v1",
		ModelName: "llama-3.2-1b-instruct",
	}

	runAsUser := int64(2000)
	runAsGroup := int64(2001)
	fsGroup := int64(2002)
	readOnly := false
	allowPrivilegeEscalation := true
	localhostProfile := "profiles/default.json"
	overrides := &containerSecurityOverrides{
		PodSecurityContext: &corev1.PodSecurityContext{
			RunAsUser:  &runAsUser,
			RunAsGroup: &runAsGroup,
			FSGroup:    &fsGroup,
			SeccompProfile: &corev1.SeccompProfile{
				Type:             corev1.SeccompProfileTypeLocalhost,
				LocalhostProfile: &localhostProfile,
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                &runAsUser,
			AllowPrivilegeEscalation: &allowPrivilegeEscalation,
			ReadOnlyRootFilesystem:   &readOnly,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"NET_RAW"},
			},
		},
	}

	dep := renderAgentDeployment(ad, containerConfig{Image: "img:1"}, binding, "override-config", false, overrides)
	podSC := dep.Spec.Template.Spec.SecurityContext
	if podSC == nil || podSC.RunAsUser == nil || *podSC.RunAsUser != runAsUser {
		t.Fatalf("pod runAsUser override not applied: %+v", podSC)
	}
	if podSC.SeccompProfile == nil || podSC.SeccompProfile.Type != corev1.SeccompProfileTypeLocalhost {
		t.Fatalf("pod seccomp override not applied: %+v", podSC.SeccompProfile)
	}
	containerSC := dep.Spec.Template.Spec.Containers[0].SecurityContext
	if containerSC == nil || containerSC.ReadOnlyRootFilesystem == nil || *containerSC.ReadOnlyRootFilesystem != readOnly {
		t.Fatalf("container readOnlyRootFilesystem override not applied: %+v", containerSC)
	}
	if containerSC.AllowPrivilegeEscalation == nil || *containerSC.AllowPrivilegeEscalation != allowPrivilegeEscalation {
		t.Fatalf("container allowPrivilegeEscalation override not applied: %+v", containerSC)
	}
	if containerSC.Capabilities == nil || len(containerSC.Capabilities.Drop) != 1 || containerSC.Capabilities.Drop[0] != "NET_RAW" {
		t.Fatalf("container capabilities.drop override not applied: %+v", containerSC.Capabilities)
	}
}

func TestParseContainerSecurityOverrides_MergesSections(t *testing.T) {
	raw := []byte(`{
		"workload": {
			"podSecurityContext": {
				"runAsUser": 1000
			}
		},
		"container": {
			"securityContext": {
				"readOnlyRootFilesystem": false,
				"allowPrivilegeEscalation": true
			}
		}
	}`)
	ad := &airunwayv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"},
		Spec: airunwayv1alpha1.AgentDeploymentSpec{
			Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "crewai"},
			Model: airunwayv1alpha1.ModelBinding{
				ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
					Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
					BaseURL:   "https://api.openai.com/v1",
					ModelName: "gpt-4o-mini",
				},
			},
			Provider: &airunwayv1alpha1.AgentProviderSpec{
				Overrides: &runtime.RawExtension{Raw: raw},
			},
		},
	}

	overrides, err := parseContainerSecurityOverrides(ad)
	if err != nil {
		t.Fatalf("parseContainerSecurityOverrides returned error: %v", err)
	}
	if overrides == nil || overrides.PodSecurityContext == nil || overrides.PodSecurityContext.RunAsUser == nil || *overrides.PodSecurityContext.RunAsUser != 1000 {
		t.Fatalf("expected merged pod runAsUser override, got %+v", overrides)
	}
	if overrides.SecurityContext == nil || overrides.SecurityContext.ReadOnlyRootFilesystem == nil || *overrides.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("expected readOnlyRootFilesystem=false override, got %+v", overrides.SecurityContext)
	}
	if overrides.SecurityContext.AllowPrivilegeEscalation == nil || !*overrides.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("expected allowPrivilegeEscalation=true override, got %+v", overrides.SecurityContext)
	}
}

func TestRenderAgentDeployment_ResourcesAndOTLP(t *testing.T) {
	ad := containerAD("obs", containerConfig{Image: "img:1"}, nil)
	ad.Spec.Resources = &airunwayv1alpha1.AgentResourceSpec{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	ad.Spec.Observability = &airunwayv1alpha1.AgentObservabilitySpec{
		OTLP: &airunwayv1alpha1.OTLPSpec{Endpoint: "http://collector:4318", Protocol: "http/protobuf"},
	}
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	dep := renderAgentDeployment(ad, containerConfig{Image: "img:1"}, binding, "obs-config", false, nil)
	c := dep.Spec.Template.Spec.Containers[0]

	if c.Resources.Requests.Cpu().String() != "250m" {
		t.Errorf("cpu request = %v, want 250m", c.Resources.Requests.Cpu())
	}
	if c.Resources.Limits.Memory().String() != "512Mi" {
		t.Errorf("memory limit = %v, want 512Mi", c.Resources.Limits.Memory())
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://collector:4318" {
		t.Errorf("OTEL endpoint = %q", env["OTEL_EXPORTER_OTLP_ENDPOINT"])
	}
	if env["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/protobuf" {
		t.Errorf("OTEL protocol = %q", env["OTEL_EXPORTER_OTLP_PROTOCOL"])
	}
}

func TestRenderAgentDeployment_CommandArgsPort(t *testing.T) {
	ad := containerAD("smoke", containerConfig{Image: "img:1"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	cfg := containerConfig{Image: "img:1", Command: []string{"python", "/serve.py"}, Args: []string{"--verbose"}, Port: 9000}

	dep := renderAgentDeployment(ad, cfg, binding, "smoke-config", false, nil)
	c := dep.Spec.Template.Spec.Containers[0]
	if len(c.Command) != 2 || c.Command[0] != "python" || c.Command[1] != "/serve.py" {
		t.Errorf("command = %v", c.Command)
	}
	if len(c.Args) != 1 || c.Args[0] != "--verbose" {
		t.Errorf("args = %v", c.Args)
	}
	if c.Ports[0].ContainerPort != 9000 {
		t.Errorf("containerPort = %d, want 9000", c.Ports[0].ContainerPort)
	}
	// The Service must target the overridden port too.
	svc := renderAgentService(ad, cfg)
	if svc.Spec.Ports[0].TargetPort.IntValue() != 9000 {
		t.Errorf("service targetPort = %v, want 9000", svc.Spec.Ports[0].TargetPort)
	}
}

func TestContainerPortDefault(t *testing.T) {
	if got := containerPort(containerConfig{}); got != agentContainerPort {
		t.Errorf("default port = %d, want %d", got, agentContainerPort)
	}
	if got := containerPort(containerConfig{Port: 8000}); got != 8000 {
		t.Errorf("override port = %d, want 8000", got)
	}
}

func TestParseContainerConfig(t *testing.T) {
	raw := &runtime.RawExtension{Raw: []byte(`{"image":"img:2","port":8000,"command":["/bin/serve"],"systemPrompt":"x"}`)}
	cfg := parseContainerConfig(raw)
	if cfg.Image != "img:2" {
		t.Errorf("parsed = %+v", cfg)
	}
	if cfg.Port != 8000 {
		t.Errorf("port = %d, want 8000", cfg.Port)
	}
	if len(cfg.Command) != 1 || cfg.Command[0] != "/bin/serve" {
		t.Errorf("command = %v", cfg.Command)
	}
	if got := parseContainerConfig(nil); got.Image != "" {
		t.Errorf("nil config should be empty, got %+v", got)
	}
}

func TestRenderAgentJob(t *testing.T) {
	ad := containerAD("swarm", containerConfig{Image: "img:1"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	job := renderAgentJob(ad, containerConfig{Image: "img:1"}, binding, "swarm-config", false, nil)

	if job.Kind != "Job" || job.APIVersion != "batch/v1" {
		t.Fatalf("GVK = %s/%s", job.APIVersion, job.Kind)
	}
	// Jobs require a non-Always restart policy.
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
	// Shares the hardened pod spec + image.
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "img:1" {
		t.Errorf("image = %q", c.Image)
	}
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Error("job pod must share the hardened security posture (drop ALL)")
	}
}

// --- envtest reconcile specs -----------------------------------------------

var _ = Describe("Container provider", func() {
	ctx := context.Background()

	makeContainerProvider := func(name string, catalogImage string) {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           airunwayv1alpha1.AgentProviderBackendContainer,
					ModelBindingModes: []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI},
				},
			},
		}
		if catalogImage != "" {
			catalog := []airunwayv1alpha1.AgentCatalogItem{
				{Name: name + "-recipe", Title: "Recipe", Image: catalogImage},
			}
			raw, err := json.Marshal(catalog)
			Expect(err).NotTo(HaveOccurred())
			apc.Annotations = map[string]string{
				airunwayv1alpha1.AgentProviderCatalogAnnotation: string(raw),
			}
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
		apc.Status.Ready = ptrBool(true)
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())
	}

	makeContainerAgent := func(name, framework, image string) {
		cfgMap := map[string]any{"systemPrompt": "You are a research assistant."}
		if image != "" {
			cfgMap["image"] = image
		}
		raw, _ := json.Marshal(cfgMap)
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: framework},
				Config:    &runtime.RawExtension{Raw: raw},
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
		r := &AgentDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	reconcileContainer := func(name string) {
		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	getAgent := func(name string) *airunwayv1alpha1.AgentDeployment {
		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, out)).To(Succeed())
		return out
	}
	prCond := func(ad *airunwayv1alpha1.AgentDeployment) *metav1.Condition {
		return meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady)
	}

	It("waits for core bindings before rendering", func() {
		makeContainerProvider("crewai-wait", "")
		makeContainerAgent("c-wait", "crewai-wait", "ghcr.io/x/crewai:poc")

		reconcileContainer("c-wait")
		ad := getAgent("c-wait")
		Expect(prCond(ad).Reason).To(Equal("WaitingForBindings"))

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-wait", Namespace: "default"}, dep)).NotTo(Succeed())
	})

	It("renders Deployment + Service + ConfigMap and tracks readiness", func() {
		makeContainerProvider("crewai-run", "")
		makeContainerAgent("c-run", "crewai-run", "ghcr.io/x/crewai:poc")

		reconcileCore("c-run")
		reconcileContainer("c-run")

		By("creating the ConfigMap with the mounted agent.json")
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run-config", Namespace: "default"}, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKey(agentConfigFileName))

		By("creating the Deployment with the BYO image and injected binding env")
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run", Namespace: "default"}, dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/crewai:poc"))
		Expect(dep.OwnerReferences).To(HaveLen(1))
		Expect(dep.OwnerReferences[0].Name).To(Equal("c-run"))

		By("creating the Service")
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run", Namespace: "default"}, svc)).To(Succeed())

		By("reporting Deploying + ProviderReady=False while no replicas are available")
		ad := getAgent("c-run")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(prCond(ad).Status).To(Equal(metav1.ConditionFalse))
		// Core-owned fields survive the provider write.
		Expect(ad.Status.ModelBinding).NotTo(BeNil())

		By("flipping to Running + ProviderReady=True once the Deployment reports available replicas")
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		reconcileContainer("c-run")
		ad = getAgent("c-run")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning))
		Expect(prCond(ad).Status).To(Equal(metav1.ConditionTrue))
		Expect(ad.Status.Replicas).NotTo(BeNil())
		Expect(ad.Status.Replicas.Available).To(Equal(int32(1)))
	})

	It("falls back to the framework catalog image when spec.config has none", func() {
		makeContainerProvider("crewai-catalog", "ghcr.io/x/from-catalog:poc")
		makeContainerAgent("c-catalog", "crewai-catalog", "") // no image in config

		reconcileCore("c-catalog")
		reconcileContainer("c-catalog")

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-catalog", Namespace: "default"}, dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/from-catalog:poc"))
	})

	It("fails with MissingImage when neither config nor catalog supplies an image", func() {
		makeContainerProvider("crewai-noimg", "")
		makeContainerAgent("c-noimg", "crewai-noimg", "")

		reconcileCore("c-noimg")
		reconcileContainer("c-noimg")

		ad := getAgent("c-noimg")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseFailed))
		Expect(prCond(ad).Reason).To(Equal("MissingImage"))
	})

	It("ignores agents whose framework is not container-backed", func() {
		// A crd-backend framework must be skipped by the container provider.
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "somecrd"},
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

		makeContainerAgent("c-notmine", "somecrd", "img:1")
		reconcileCore("c-notmine")
		reconcileContainer("c-notmine")

		// The container provider must not have created a Deployment.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-notmine", Namespace: "default"}, dep)).NotTo(Succeed())
	})

	It("renders a one-shot Job when spec.lifecycle is job", func() {
		makeContainerProvider("crewai-job", "")

		// Build an agent with lifecycle: job.
		cfgRaw, _ := json.Marshal(map[string]any{"image": "ghcr.io/x/task:poc", "systemPrompt": "do the task"})
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "c-job", Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "crewai-job"},
				Lifecycle: airunwayv1alpha1.AgentLifecycleJob,
				Config:    &runtime.RawExtension{Raw: cfgRaw},
				Model: airunwayv1alpha1.ModelBinding{
					ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
						Type: airunwayv1alpha1.ExternalAPITypeOpenAI, BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })

		reconcileCore("c-job")
		reconcileContainer("c-job")

		By("creating a Job (not a Deployment or Service)")
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job", Namespace: "default"}, job)).To(Succeed())
		Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(job.OwnerReferences).To(HaveLen(1))
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job", Namespace: "default"}, dep)).NotTo(Succeed())
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job", Namespace: "default"}, svc)).NotTo(Succeed())

		By("reporting Deploying while the Job has not started")
		ad2 := getAgent("c-job")
		Expect(ad2.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(prCond(ad2).Reason).To(Equal("JobPending"))

		By("flipping to Running once the Job reports an active pod")
		job.Status.Active = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer("c-job")
		ad2 = getAgent("c-job")
		Expect(ad2.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning))
		Expect(prCond(ad2).Status).To(Equal(metav1.ConditionTrue))
	})
})
