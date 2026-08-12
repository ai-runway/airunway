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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
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
	authRef := &airunwayv1alpha1.SecretKeyRef{Name: "research-api-auth", Key: "token"}
	dep := renderAgentDeployment(ad, renderInputs{
		cfg: containerConfig{Image: "ghcr.io/x/crewai:poc"}, binding: binding,
		configMapName: "research-config", authSecretRef: authRef,
		accessTokenHash: "access-hash", writableRoot: false, securityOverrides: nil,
	})

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
	// The image is author-chosen, so the pod must not carry the namespace's
	// default ServiceAccount token: that would let someone who can create an
	// AgentDeployment, but not a Pod, act as that ServiceAccount.
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be explicitly false — an author-chosen image must not inherit the default ServiceAccount token")
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
	var apiKeyFromSecret, accessTokenFromSecret bool
	for _, e := range c.Env {
		env[e.Name] = e.Value
		if e.Name == "OPENAI_API_KEY" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			apiKeyFromSecret = true
			if e.ValueFrom.SecretKeyRef.Name != "openai-api-key" {
				t.Errorf("OPENAI_API_KEY secret = %q", e.ValueFrom.SecretKeyRef.Name)
			}
		}
		if e.Name == agentAccessTokenEnv && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			accessTokenFromSecret = true
			if e.ValueFrom.SecretKeyRef.Name != authRef.Name || e.ValueFrom.SecretKeyRef.Key != authRef.Key {
				t.Errorf("%s secret ref = %+v, want %+v", agentAccessTokenEnv, e.ValueFrom.SecretKeyRef, authRef)
			}
		}
	}
	if env["OPENAI_BASE_URL"] != "https://api.openai.com/v1" {
		t.Errorf("OPENAI_BASE_URL = %q", env["OPENAI_BASE_URL"])
	}
	if env["AIRUNWAY_AGENT_CONFIG"] != agentConfigMountPath {
		t.Errorf("AIRUNWAY_AGENT_CONFIG = %q, want %q", env["AIRUNWAY_AGENT_CONFIG"], agentConfigMountPath)
	}
	if env["AIRUNWAY_AGENT_MODE"] != "server" {
		t.Errorf("AIRUNWAY_AGENT_MODE = %q, want server", env["AIRUNWAY_AGENT_MODE"])
	}
	if env["AIRUNWAY_AGENT_PORT"] != "8080" {
		t.Errorf("AIRUNWAY_AGENT_PORT = %q, want 8080", env["AIRUNWAY_AGENT_PORT"])
	}
	if c.StartupProbe == nil || c.StartupProbe.TCPSocket == nil || c.StartupProbe.TCPSocket.Port.IntValue() != 8080 {
		t.Errorf("startup probe = %+v, want TCP :8080", c.StartupProbe)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil || c.ReadinessProbe.TCPSocket.Port.IntValue() != 8080 {
		t.Errorf("readiness probe = %+v, want TCP :8080", c.ReadinessProbe)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.TCPSocket == nil || c.LivenessProbe.TCPSocket.Port.IntValue() != 8080 {
		t.Errorf("liveness probe = %+v, want TCP :8080", c.LivenessProbe)
	}
	if !apiKeyFromSecret {
		t.Error("OPENAI_API_KEY must be sourced from the binding secret")
	}
	if !accessTokenFromSecret {
		t.Errorf("%s must be sourced from the provider-managed access Secret", agentAccessTokenEnv)
	}
	if got := dep.Spec.Template.Annotations[agentAccessChecksumAnnotation]; got != "access-hash" {
		t.Errorf("access token checksum = %q, want access-hash", got)
	}
}

func TestRenderAgentDeployment_WritableRootForFramework(t *testing.T) {
	ad := containerAD("openclaw", containerConfig{Image: "img:1"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	// writableRoot is a provider-owned decision passed by the reconciler, not a
	// user-facing spec.config field.
	dep := renderAgentDeployment(ad, renderInputs{cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "openclaw-config", writableRoot: true, securityOverrides: nil})

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
	dep := renderAgentDeployment(ad, renderInputs{cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "keyless-config", writableRoot: false, securityOverrides: nil})

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
	if apiKey.Value != agentprovider.KeylessCredentialValue {
		t.Fatalf("OPENAI_API_KEY = %q, want %q", apiKey.Value, agentprovider.KeylessCredentialValue)
	}
}

func TestModelBindingEnv_MapsFamilyByAPIType(t *testing.T) {
	cases := []struct {
		name        string
		apiType     airunwayv1alpha1.ExternalAPIType
		wantBaseKey string
		wantModel   string
		wantKeyName string
	}{
		{"openai", airunwayv1alpha1.ExternalAPITypeOpenAI, "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENAI_API_KEY"},
		{"custom", airunwayv1alpha1.ExternalAPITypeCustom, "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENAI_API_KEY"},
		{"unset-deploymentRef", "", "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENAI_API_KEY"},
		{"anthropic", airunwayv1alpha1.ExternalAPITypeAnthropic, "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_API_KEY"},
		{"azureOpenAI", airunwayv1alpha1.ExternalAPITypeAzureOpenAI, "AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_MODEL", "AZURE_OPENAI_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binding := airunwayv1alpha1.ModelBindingStatus{
				APIType:        tc.apiType,
				BaseURL:        "https://endpoint/v1",
				ModelName:      "some-model",
				CredentialsRef: &airunwayv1alpha1.SecretKeyRef{Name: "creds", Key: "api-key"},
			}
			env := map[string]corev1.EnvVar{}
			for _, e := range modelBindingEnv(binding) {
				env[e.Name] = e
			}
			if env[tc.wantBaseKey].Value != "https://endpoint/v1" {
				t.Errorf("%s = %q, want endpoint URL", tc.wantBaseKey, env[tc.wantBaseKey].Value)
			}
			if env[tc.wantModel].Value != "some-model" {
				t.Errorf("%s = %q, want model", tc.wantModel, env[tc.wantModel].Value)
			}
			keyVar, ok := env[tc.wantKeyName]
			if !ok || keyVar.ValueFrom == nil || keyVar.ValueFrom.SecretKeyRef == nil {
				t.Errorf("%s must be sourced from the binding secret, got %+v", tc.wantKeyName, keyVar)
			}
			// The OpenAI family must NOT leak in for non-OpenAI types.
			if tc.wantBaseKey != "OPENAI_BASE_URL" {
				if _, leaked := env["OPENAI_BASE_URL"]; leaked {
					t.Errorf("OPENAI_BASE_URL must not be set for APIType %q", tc.apiType)
				}
			}
		})
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

	dep := renderAgentDeployment(ad, renderInputs{cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "override-config", writableRoot: false, securityOverrides: overrides})

	// Overrides that do not weaken the posture are applied as given.
	podSC := dep.Spec.Template.Spec.SecurityContext
	if podSC == nil || podSC.RunAsUser == nil || *podSC.RunAsUser != runAsUser {
		t.Fatalf("pod runAsUser override not applied: %+v", podSC)
	}
	if podSC.RunAsGroup == nil || *podSC.RunAsGroup != runAsGroup {
		t.Fatalf("pod runAsGroup override not applied: %+v", podSC)
	}
	if podSC.FSGroup == nil || *podSC.FSGroup != fsGroup {
		t.Fatalf("pod fsGroup override not applied: %+v", podSC)
	}
	if podSC.SeccompProfile == nil || podSC.SeccompProfile.Type != corev1.SeccompProfileTypeLocalhost {
		t.Fatalf("pod seccomp override not applied: %+v", podSC.SeccompProfile)
	}
	containerSC := dep.Spec.Template.Spec.Containers[0].SecurityContext
	if containerSC == nil || containerSC.RunAsUser == nil || *containerSC.RunAsUser != runAsUser {
		t.Fatalf("container runAsUser override not applied: %+v", containerSC)
	}

	// Overrides that WOULD weaken it are clamped by the render path, not merged.
	// The webhook rejects each of these too, but ENABLE_WEBHOOKS=false is a
	// supported mode, so the floor cannot depend on admission having run.
	// (readOnly=false, allowPrivilegeEscalation=true and a drop list omitting
	// ALL are all requested above.)
	if containerSC.ReadOnlyRootFilesystem == nil || *containerSC.ReadOnlyRootFilesystem == readOnly {
		t.Errorf("readOnlyRootFilesystem must be clamped back to true, got %v", containerSC.ReadOnlyRootFilesystem)
	}
	if containerSC.AllowPrivilegeEscalation == nil || *containerSC.AllowPrivilegeEscalation == allowPrivilegeEscalation {
		t.Errorf("allowPrivilegeEscalation must be clamped back to false, got %v", containerSC.AllowPrivilegeEscalation)
	}
	// The extra drop survives; ALL is added rather than replacing it.
	var hasAll, hasNetRaw bool
	for _, d := range containerSC.Capabilities.Drop {
		switch d {
		case "ALL":
			hasAll = true
		case "NET_RAW":
			hasNetRaw = true
		}
	}
	if !hasAll {
		t.Errorf("capabilities.drop must include ALL, got %v", containerSC.Capabilities.Drop)
	}
	if !hasNetRaw {
		t.Errorf("an additional drop must be preserved, got %v", containerSC.Capabilities.Drop)
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
	dep := renderAgentDeployment(ad, renderInputs{cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "obs-config", writableRoot: false, securityOverrides: nil})
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

	dep := renderAgentDeployment(ad, renderInputs{cfg: cfg, binding: binding, configMapName: "smoke-config", writableRoot: false, securityOverrides: nil})
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
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil || c.ReadinessProbe.TCPSocket.Port.IntValue() != 9000 {
		t.Errorf("readiness probe = %+v, want TCP :9000", c.ReadinessProbe)
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
	cfg, err := parseContainerConfig(raw)
	if err != nil {
		t.Fatalf("parseContainerConfig: %v", err)
	}
	if cfg.Image != "img:2" {
		t.Errorf("parsed = %+v", cfg)
	}
	if cfg.Port != 8000 {
		t.Errorf("port = %d, want 8000", cfg.Port)
	}
	if len(cfg.Command) != 1 || cfg.Command[0] != "/bin/serve" {
		t.Errorf("command = %v", cfg.Command)
	}
	if got, err := parseContainerConfig(nil); err != nil || got.Image != "" {
		t.Errorf("nil config should be empty, got %+v", got)
	}
	for _, port := range []int32{-1, 70000} {
		raw := &runtime.RawExtension{Raw: []byte(fmt.Sprintf(`{"image":"img:2","port":%d}`, port))}
		if _, err := parseContainerConfig(raw); err == nil {
			t.Errorf("port %d should be rejected, got no error", port)
		}
	}
}

func TestRenderAgentJob(t *testing.T) {
	ad := containerAD("swarm", containerConfig{Image: "img:1"}, nil)
	ad.Spec.Lifecycle = airunwayv1alpha1.AgentLifecycleJob
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	job, err := renderAgentJob(ad, renderInputs{cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "swarm-config", writableRoot: false, securityOverrides: nil})
	if err != nil {
		t.Fatalf("renderAgentJob: %v", err)
	}

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
	env := map[string]string{}
	for _, value := range c.Env {
		env[value.Name] = value.Value
	}
	if env["AIRUNWAY_AGENT_MODE"] != "job" {
		t.Errorf("AIRUNWAY_AGENT_MODE = %q, want job", env["AIRUNWAY_AGENT_MODE"])
	}
	if c.StartupProbe != nil || c.ReadinessProbe != nil || c.LivenessProbe != nil {
		t.Error("one-shot jobs must not receive server probes")
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
		r := &AgentDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), CredentialAdmissionActive: true}
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

		By("creating an independently scoped access token for the outward endpoint")
		accessSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run-api-auth", Namespace: "default"}, accessSecret)).To(Succeed())
		Expect(accessSecret.OwnerReferences).To(HaveLen(1))
		Expect(accessSecret.OwnerReferences[0].Name).To(Equal("c-run"))
		Expect(accessSecret.Immutable).NotTo(BeNil())
		Expect(*accessSecret.Immutable).To(BeTrue())
		accessToken := accessSecret.Data[agentAccessTokenKey]
		Expect(accessToken).To(HaveLen(43), "32 random bytes should be encoded as unpadded base64url")
		accessDigest := sha256.Sum256(accessToken)

		By("creating the Deployment with the BYO image and injected bindings")
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run", Namespace: "default"}, dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/crewai:poc"))
		Expect(dep.OwnerReferences).To(HaveLen(1))
		Expect(dep.OwnerReferences[0].Name).To(Equal("c-run"))
		Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(agentAccessChecksumAnnotation, fmt.Sprintf("%x", accessDigest)))
		var accessEnv *corev1.EnvVar
		for i := range dep.Spec.Template.Spec.Containers[0].Env {
			if dep.Spec.Template.Spec.Containers[0].Env[i].Name == agentAccessTokenEnv {
				accessEnv = &dep.Spec.Template.Spec.Containers[0].Env[i]
				break
			}
		}
		Expect(accessEnv).NotTo(BeNil())
		Expect(accessEnv.ValueFrom.SecretKeyRef.Name).To(Equal("c-run-api-auth"))
		Expect(accessEnv.ValueFrom.SecretKeyRef.Key).To(Equal(agentAccessTokenKey))

		By("creating the Service")
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run", Namespace: "default"}, svc)).To(Succeed())

		By("reporting Deploying + ProviderReady=False while no replicas are available")
		ad := getAgent("c-run")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(prCond(ad).Status).To(Equal(metav1.ConditionFalse))
		// Core-owned fields survive the provider write.
		Expect(ad.Status.ModelBinding).NotTo(BeNil())
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).To(Equal(&airunwayv1alpha1.SecretKeyRef{
			Name: "c-run-api-auth", Key: agentAccessTokenKey,
		}))

		By("staying Deploying while the Deployment status still describes the previous generation")
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		dep.Status.UpdatedReplicas = 0
		dep.Status.ObservedGeneration = dep.Generation - 1
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		reconcileContainer("c-run")
		ad = getAgent("c-run")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(prCond(ad).Status).To(Equal(metav1.ConditionFalse))

		By("flipping to Running + ProviderReady=True once the current generation has rolled out")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run", Namespace: "default"}, dep)).To(Succeed())
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		dep.Status.UpdatedReplicas = 1
		dep.Status.ObservedGeneration = dep.Generation
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

	It("refuses to adopt a same-named user access Secret", func() {
		makeContainerProvider("crewai-auth-conflict", "")
		makeContainerAgent("c-auth-conflict", "crewai-auth-conflict", "ghcr.io/x/crewai:poc")
		reconcileCore("c-auth-conflict")

		foreign := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-auth-conflict-api-auth", Namespace: "default"},
			Data:       map[string][]byte{agentAccessTokenKey: []byte("user-owned-token-must-survive")},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: "c-auth-conflict", Namespace: "default",
		}})
		Expect(err).To(MatchError(ContainSubstring("refusing to adopt agent access Secret")))

		current := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: foreign.Name, Namespace: foreign.Namespace}, current)).To(Succeed())
		Expect(current.Data[agentAccessTokenKey]).To(Equal([]byte("user-owned-token-must-survive")))
		Expect(current.OwnerReferences).To(BeEmpty())

		out := getAgent("c-auth-conflict")
		Expect(prCond(out).Reason).To(Equal("IngressCredentialProvisionFailed"))
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-auth-conflict", Namespace: "default"}, dep)).NotTo(Succeed())
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
		for i := range job.Spec.Template.Spec.Containers[0].Env {
			Expect(job.Spec.Template.Spec.Containers[0].Env[i].Name).NotTo(Equal(agentAccessTokenEnv))
		}
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job", Namespace: "default"}, dep)).NotTo(Succeed())
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job", Namespace: "default"}, svc)).NotTo(Succeed())
		accessSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job-api-auth", Namespace: "default"}, accessSecret)).NotTo(Succeed())

		By("reporting Deploying while the Job has not started")
		ad2 := getAgent("c-job")
		Expect(ad2.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(prCond(ad2).Reason).To(Equal("JobPending"))
		Expect(ad2.Status.Runtime.AuthSecretRef).To(BeNil())

		By("flipping to Running once the Job reports an active pod")
		job.Status.Active = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer("c-job")
		ad2 = getAgent("c-job")
		Expect(ad2.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning))
		Expect(prCond(ad2).Status).To(Equal(metav1.ConditionTrue))

		By("flipping to Completed once the Job succeeds")
		// Reconcile server-side-applied the Job after the first status update,
		// which can advance resourceVersion while converting the create manager's
		// managed-fields entry to Apply. Refresh before the next status write.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job", Namespace: "default"}, job)).To(Succeed())
		job.Status.Active = 0
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer("c-job")
		ad2 = getAgent("c-job")
		Expect(ad2.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
		Expect(prCond(ad2).Reason).To(Equal("JobCompleted"))
	})
})

// TestSecurityFloorSurvivesHostileOverrides is the regression test for the
// webhook-off hole. The validating webhook rejects each of these values, but
// ENABLE_WEBHOOKS=false is a supported mode and resources admitted before the
// webhook existed are never re-validated — so the render path has to hold the
// floor on its own. Every field here is one the merge would otherwise let an
// AgentDeployment author win, because overrides are merged after the defaults.
func TestSecurityFloorSurvivesHostileOverrides(t *testing.T) {
	hostile := &containerSecurityOverrides{
		PodSecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr.To(false),
			RunAsUser:      ptr.To[int64](0),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(false),
			RunAsUser:                ptr.To[int64](0),
			AllowPrivilegeEscalation: ptr.To(true),
			ReadOnlyRootFilesystem:   ptr.To(false),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			Capabilities:             &corev1.Capabilities{Drop: nil},
		},
	}

	pod := &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](defaultAgentRunAsUser)}
	ctr := &corev1.SecurityContext{RunAsNonRoot: ptr.To(true), ReadOnlyRootFilesystem: ptr.To(true)}

	applyContainerSecurityOverrides(pod, ctr, hostile, false /* writableRoot */)

	if pod.RunAsNonRoot == nil || !*pod.RunAsNonRoot {
		t.Error("pod runAsNonRoot must stay true")
	}
	if pod.RunAsUser == nil || *pod.RunAsUser == 0 {
		t.Errorf("pod runAsUser must not be root, got %v", pod.RunAsUser)
	}
	if pod.SeccompProfile == nil || pod.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccompProfile must not be Unconfined, got %v", pod.SeccompProfile)
	}
	if ctr.RunAsNonRoot == nil || !*ctr.RunAsNonRoot {
		t.Error("container runAsNonRoot must stay true")
	}
	if ctr.RunAsUser == nil || *ctr.RunAsUser == 0 {
		t.Errorf("container runAsUser must not be root, got %v", ctr.RunAsUser)
	}
	if ctr.AllowPrivilegeEscalation == nil || *ctr.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must stay false")
	}
	if ctr.ReadOnlyRootFilesystem == nil || !*ctr.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must stay true when the provider did not declare a writable root")
	}
	if ctr.SeccompProfile == nil || ctr.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("container seccompProfile must not be Unconfined, got %v", ctr.SeccompProfile)
	}
	if ctr.Capabilities == nil || len(ctr.Capabilities.Drop) != 1 || ctr.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities must still drop ALL, got %v", ctr.Capabilities)
	}
}

// The floor must not override what the provider legitimately declared, or a
// framework that genuinely needs a writable root could never run.
func TestSecurityFloorHonoursProviderWritableRoot(t *testing.T) {
	pod := &corev1.PodSecurityContext{}
	ctr := &corev1.SecurityContext{}
	applyContainerSecurityOverrides(pod, ctr, nil, true /* writableRoot */)
	if ctr.ReadOnlyRootFilesystem == nil || *ctr.ReadOnlyRootFilesystem {
		t.Error("a provider declaring writableRootFilesystem must get a writable root")
	}
	// Everything else is still clamped.
	if ctr.AllowPrivilegeEscalation == nil || *ctr.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false even with a writable root")
	}
}

// A localhost seccomp profile is a cluster-admin artefact, not something an
// agent author can forge, so it must be preserved rather than flattened.
func TestSecurityFloorPreservesLocalhostSeccomp(t *testing.T) {
	pod := &corev1.PodSecurityContext{}
	ctr := &corev1.SecurityContext{}
	overrides := &containerSecurityOverrides{
		SecurityContext: &corev1.SecurityContext{
			SeccompProfile: &corev1.SeccompProfile{
				Type:             corev1.SeccompProfileTypeLocalhost,
				LocalhostProfile: ptr.To("operator/agent.json"),
			},
		},
	}
	applyContainerSecurityOverrides(pod, ctr, overrides, false)
	if ctr.SeccompProfile == nil || ctr.SeccompProfile.Type != corev1.SeccompProfileTypeLocalhost {
		t.Errorf("localhost seccomp profile must be preserved, got %v", ctr.SeccompProfile)
	}
}

var _ = Describe("Container provider: catalog and backend-switch handling", func() {
	ctx := context.Background()

	malformedCatalogProvider := func(name string) {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Annotations: map[string]string{
					// Valid YAML, invalid catalog JSON — exactly the typo case.
					airunwayv1alpha1.AgentProviderCatalogAnnotation: `[{"name": "broken",`,
				},
			},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           airunwayv1alpha1.AgentProviderBackendContainer,
					ModelBindingModes: []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI},
				},
			},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
		apc.Status.Ready = ptrBool(true)
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())
	}

	agentOn := func(name, framework, image string) {
		cfg := map[string]any{"systemPrompt": "hi"}
		if image != "" {
			cfg["image"] = image
		}
		raw, _ := json.Marshal(cfg)
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

	core := func(name string) {
		r := &AgentDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), CredentialAdmissionActive: true}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	container := func(name string) error {
		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		return err
	}

	It("renders an agent with an explicit image despite a malformed catalog", func() {
		// A typo in marketplace UI metadata must not take down an agent that
		// never reads the catalog. This previously failed twice over: readiness
		// went false (tearing down every agent on the framework) and the
		// provider returned an error from every reconcile.
		malformedCatalogProvider("crewai-badcat")
		agentOn("c-explicit", "crewai-badcat", "ghcr.io/x/crewai:poc")

		core("c-explicit")
		Expect(container("c-explicit")).To(Succeed(), "a malformed catalog must not fail the reconcile")

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-explicit", Namespace: "default"}, dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/crewai:poc"))
	})

	It("fails only the agent that actually needed the catalog, naming the parse error", func() {
		malformedCatalogProvider("crewai-badcat2")
		agentOn("c-needs-catalog", "crewai-badcat2", "") // no explicit image

		core("c-needs-catalog")
		Expect(container("c-needs-catalog")).To(Succeed())

		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-needs-catalog", Namespace: "default"}, out)).To(Succeed())
		cond := meta.FindStatusCondition(out.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("MissingImage"))
		Expect(cond.Message).To(ContainSubstring("could not be parsed"),
			"the agent that needs the catalog should be told the catalog is broken")
	})

	It("tears down its workload when the framework registration goes away", func() {
		// spec.capabilities.backend is immutable, so a framework can only move
		// to another backend by being deleted and recreated. The leak window is
		// the delete: without cleanup here the Deployment/Service/ConfigMap keep
		// running unmanaged, and once the framework is recreated on a CRD
		// backend that provider renders a second workload beside them.
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "switch-fw"},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           airunwayv1alpha1.AgentProviderBackendContainer,
					ModelBindingModes: []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI},
				},
			},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
		apc.Status.Ready = ptrBool(true)
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())

		agentOn("c-switch", "switch-fw", "ghcr.io/x/crewai:poc")
		core("c-switch")
		Expect(container("c-switch")).To(Succeed())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-switch", Namespace: "default"}, dep)).To(Succeed())

		By("deleting the framework registration")
		Expect(k8sClient.Delete(ctx, apc)).To(Succeed())

		Expect(container("c-switch")).To(Succeed())
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "c-switch", Namespace: "default"}, dep)
		if err == nil {
			Expect(dep.DeletionTimestamp.IsZero()).To(BeFalse(),
				"the orphaned Deployment must be terminating, not left running unmanaged")
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}

		By("also starting foreground removal of the Service and ConfigMap")
		svc := &corev1.Service{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "c-switch", Namespace: "default"}, svc)
		if err == nil {
			Expect(svc.DeletionTimestamp.IsZero()).To(BeFalse())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
		cm := &corev1.ConfigMap{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "c-switch-config", Namespace: "default"}, cm)
		if err == nil {
			Expect(cm.DeletionTimestamp.IsZero()).To(BeFalse())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})
})

// A hardening override must survive the clamp. The webhook accepts
// readOnlyRootFilesystem: true, so pinning the value to the provider capability
// in both directions silently discarded it — the one direction the "can harden,
// never weaken" rule is supposed to permit.
func TestSecurityFloorAllowsHardeningAboveProviderDefault(t *testing.T) {
	overrides := &containerSecurityOverrides{
		SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true)},
	}
	pod := &corev1.PodSecurityContext{}
	ctr := &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(false)} // provider default for writableRoot

	applyContainerSecurityOverrides(pod, ctr, overrides, true /* writableRoot */)

	if ctr.ReadOnlyRootFilesystem == nil || !*ctr.ReadOnlyRootFilesystem {
		t.Error("an author hardening a writable-root framework must keep readOnlyRootFilesystem: true")
	}
}

// The other direction is still forced: a framework that never declared a
// writable root cannot have read-only turned off, webhook or no webhook.
func TestSecurityFloorStillForcesReadOnlyWhenNotDeclared(t *testing.T) {
	overrides := &containerSecurityOverrides{
		SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(false)},
	}
	pod := &corev1.PodSecurityContext{}
	ctr := &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true)}

	applyContainerSecurityOverrides(pod, ctr, overrides, false /* writableRoot */)

	if ctr.ReadOnlyRootFilesystem == nil || !*ctr.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must stay true when the provider did not declare a writable root")
	}
}
