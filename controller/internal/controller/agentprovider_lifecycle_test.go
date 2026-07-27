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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

func TestBoundedLabelValue(t *testing.T) {
	short := "my-agent"
	if got := boundedLabelValue(short); got != short {
		t.Errorf("short name should pass through unchanged, got %q", got)
	}

	long := strings.Repeat("a", 80)
	got := boundedLabelValue(long)
	if len(got) > maxLabelValueLength {
		t.Errorf("bounded label = %d bytes, want <= %d", len(got), maxLabelValueLength)
	}

	// Two distinct long names that share a prefix must not collapse together,
	// otherwise their workloads would share a selector.
	other := strings.Repeat("a", 79) + "b"
	if boundedLabelValue(other) == got {
		t.Errorf("distinct long names collided on %q", got)
	}
}

func TestBoundedResourceName(t *testing.T) {
	if got := boundedResourceName("agent", "-config"); got != "agent-config" {
		t.Errorf("short name = %q, want agent-config", got)
	}
	long := strings.Repeat("a", 250)
	got := boundedResourceName(long, "-config")
	if len(got) > maxResourceNameLength {
		t.Errorf("bounded name = %d bytes, want <= %d", len(got), maxResourceNameLength)
	}
	if !strings.HasSuffix(got, "-config") {
		t.Errorf("bounded name lost its suffix: %q", got)
	}
}

func TestDeploymentRolledOut(t *testing.T) {
	cases := []struct {
		name    string
		dep     appsv1.Deployment
		desired int32
		want    bool
	}{
		{
			name: "stale status from the previous generation is not ready",
			dep: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1, Replicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1,
				},
			},
			desired: 1,
			want:    false,
		},
		{
			name: "observed but not yet rolled out is not ready",
			dep: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2, Replicas: 1, AvailableReplicas: 1, UpdatedReplicas: 0,
				},
			},
			desired: 1,
			want:    false,
		},
		{
			// The default RollingUpdate at replicas=1 is maxSurge=1 /
			// maxUnavailable=0, so mid-rollout there are two pods and the only
			// AVAILABLE one is still the old generation's. Reporting ready here
			// would mark a broken new image (ImagePullBackOff) as Running
			// permanently, since the old pod is never scaled down.
			name: "surged mid-rollout with only the old pod available is not ready",
			dep: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 3, Replicas: 2, AvailableReplicas: 1, UpdatedReplicas: 1,
				},
			},
			desired: 1,
			want:    false,
		},
		{
			name: "current generation fully available with old replicas drained is ready",
			dep: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2, Replicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1,
				},
			},
			desired: 1,
			want:    true,
		},
		{
			name: "zero desired replicas is never ready",
			dep: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status:     appsv1.DeploymentStatus{ObservedGeneration: 1},
			},
			desired: 0,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deploymentRolledOut(&tc.dep, tc.desired); got != tc.want {
				t.Errorf("deploymentRolledOut = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseContainerConfigRejectsMalformed(t *testing.T) {
	raw := &runtime.RawExtension{Raw: []byte(`{"image": 42}`)}
	if _, err := parseContainerConfig(raw); err == nil {
		t.Fatal("expected a malformed spec.config to be reported, got nil error")
	}
}

func TestParseKagentConfigRejectsMalformed(t *testing.T) {
	raw := &runtime.RawExtension{Raw: []byte(`{"systemPrompt": ["not", "a", "string"]}`)}
	if _, err := parseKagentConfig(raw); err == nil {
		t.Fatal("expected a malformed spec.config to be reported, got nil error")
	}
}

// --- envtest specs ----------------------------------------------------------

var _ = Describe("Container provider workload lifecycle", func() {
	ctx := context.Background()

	provider := func(name string) {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
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

	agent := func(name, framework string, cfg map[string]any, lifecycle airunwayv1alpha1.AgentLifecycle) {
		raw, _ := json.Marshal(cfg)
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: framework},
				Lifecycle: lifecycle,
				Config:    &runtime.RawExtension{Raw: raw},
				Model: airunwayv1alpha1.ModelBinding{
					ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
						Type:    airunwayv1alpha1.ExternalAPITypeOpenAI,
						BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })
	}

	core := func(name string) {
		r := &AgentDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	container := func(name string) error {
		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		return err
	}
	setConfig := func(name string, cfg map[string]any) {
		ad := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, ad)).To(Succeed())
		raw, _ := json.Marshal(cfg)
		ad.Spec.Config = &runtime.RawExtension{Raw: raw}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
	}

	It("recreates the Job when its immutable pod template changes", func() {
		provider("job-fw")
		agent("job-agent", "job-fw", map[string]any{"image": "ghcr.io/x/agent:v1"}, airunwayv1alpha1.AgentLifecycleJob)

		core("job-agent")
		Expect(container("job-agent")).To(Succeed())

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "job-agent", Namespace: "default"}, job)).To(Succeed())
		Expect(job.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/agent:v1"))
		firstHash := job.Annotations[agentTemplateHashAnnotation]
		Expect(firstHash).NotTo(BeEmpty())

		By("bumping the image, which a plain apply could not do (spec.template is immutable)")
		setConfig("job-agent", map[string]any{"image": "ghcr.io/x/agent:v2"})

		// First pass deletes the old Job and reports the recreate.
		Expect(container("job-agent")).To(Succeed())
		ad := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "job-agent", Namespace: "default"}, ad)).To(Succeed())
		Expect(ad.Status.Phase).NotTo(Equal(airunwayv1alpha1.AgentPhaseFailed))

		// envtest has no Job controller to finalize the delete, so clear any
		// lingering object before the recreate pass.
		stale := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "job-agent", Namespace: "default"}, stale); err == nil {
			stale.Finalizers = nil
			_ = k8sClient.Update(ctx, stale)
			_ = k8sClient.Delete(ctx, stale)
		}

		By("creating the replacement Job with the new template on the following pass")
		Eventually(func(g Gomega) {
			g.Expect(container("job-agent")).To(Succeed())
			out := &batchv1.Job{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "job-agent", Namespace: "default"}, out)).To(Succeed())
			g.Expect(out.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/agent:v2"))
			g.Expect(out.Annotations[agentTemplateHashAnnotation]).NotTo(Equal(firstHash))
		}).Should(Succeed())

		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "job-agent", Namespace: "default"}, out)).To(Succeed())
		Expect(out.Status.Phase).NotTo(Equal(airunwayv1alpha1.AgentPhaseFailed))
	})

	It("rolls the Deployment when only the mounted config changes", func() {
		provider("cm-fw")
		agent("cm-agent", "cm-fw", map[string]any{"image": "ghcr.io/x/agent:v1", "systemPrompt": "old"}, "")

		core("cm-agent")
		Expect(container("cm-agent")).To(Succeed())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cm-agent", Namespace: "default"}, dep)).To(Succeed())
		firstChecksum := dep.Spec.Template.Annotations[agentConfigChecksumAnnotation]
		Expect(firstChecksum).NotTo(BeEmpty())

		By("changing only a config field the provider does not otherwise render")
		setConfig("cm-agent", map[string]any{"image": "ghcr.io/x/agent:v1", "systemPrompt": "new"})
		Expect(container("cm-agent")).To(Succeed())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cm-agent-config", Namespace: "default"}, cm)).To(Succeed())
		Expect(cm.Data[agentConfigFileName]).To(ContainSubstring("new"))

		updated := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cm-agent", Namespace: "default"}, updated)).To(Succeed())
		Expect(updated.Spec.Template.Annotations[agentConfigChecksumAnnotation]).NotTo(Equal(firstChecksum),
			"config-only change must alter the pod template so the workload rolls")
		Expect(updated.Generation).To(BeNumerically(">", dep.Generation))
	})

	It("renders workloads for an AgentDeployment whose name exceeds the label limit", func() {
		longName := strings.Repeat("a", 80)
		provider("long-fw")
		agent(longName, "long-fw", map[string]any{"image": "ghcr.io/x/agent:v1"}, "")

		core(longName)
		Expect(container(longName)).To(Succeed())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: longName, Namespace: "default"}, dep)).To(Succeed())
		for key, value := range dep.Spec.Template.Labels {
			Expect(len(value)).To(BeNumerically("<=", maxLabelValueLength), "label %q exceeds the limit", key)
		}

		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: longName, Namespace: "default"}, out)).To(Succeed())
		Expect(out.Status.Phase).NotTo(Equal(airunwayv1alpha1.AgentPhaseFailed))
	})

	It("keeps running workloads when framework readiness blips", func() {
		provider("blip-fw")
		agent("blip-agent", "blip-fw", map[string]any{"image": "ghcr.io/x/agent:v1"}, "")

		core("blip-agent")
		Expect(container("blip-agent")).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "blip-agent", Namespace: "default"}, &appsv1.Deployment{})).To(Succeed())

		By("flipping the framework to not-ready, as a discovery error would")
		apc := &airunwayv1alpha1.AgentProviderConfig{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "blip-fw"}, apc)).To(Succeed())
		apc.Status.Ready = ptrBool(false)
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())

		core("blip-agent")

		By("retaining the resolved binding rather than clearing it")
		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "blip-agent", Namespace: "default"}, out)).To(Succeed())
		Expect(out.Status.ModelBinding).NotTo(BeNil(),
			"a retryable framework-readiness failure must not clear status.modelBinding; clearing it makes every provider tear down its workloads")

		By("leaving the rendered workload running")
		Expect(container("blip-agent")).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "blip-agent", Namespace: "default"}, &appsv1.Deployment{})).To(Succeed())
	})

	It("clears the binding and tears down on terminal invalidity", func() {
		// A cross-namespace deploymentRef is rejected outright, not retried, so
		// the binding really must be cleared and the workload stopped.
		provider("terminal-fw")
		raw, _ := json.Marshal(map[string]any{"image": "ghcr.io/x/agent:v1"})
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "terminal-agent", Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "terminal-fw"},
				Config:    &runtime.RawExtension{Raw: raw},
				Model: airunwayv1alpha1.ModelBinding{
					ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
						Type:    airunwayv1alpha1.ExternalAPITypeOpenAI,
						BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })

		core("terminal-agent")
		Expect(container("terminal-agent")).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "terminal-agent", Namespace: "default"}, &appsv1.Deployment{})).To(Succeed())

		By("switching to a cross-namespace reference, which is terminal")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "terminal-agent", Namespace: "default"}, ad)).To(Succeed())
		ad.Spec.Model = airunwayv1alpha1.ModelBinding{
			DeploymentRef: &airunwayv1alpha1.ModelDeploymentBinding{Name: "somewhere", Namespace: "other-ns"},
		}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		core("terminal-agent")

		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "terminal-agent", Namespace: "default"}, out)).To(Succeed())
		Expect(out.Status.ModelBinding).To(BeNil(), "terminal invalidity must clear the binding")

		By("tearing down the workload")
		Expect(container("terminal-agent")).To(Succeed())
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "terminal-agent", Namespace: "default"}, &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected the Deployment to be deleted, got %v", err)
	})

	It("does not delete an unrelated same-named workload on a lifecycle switch", func() {
		provider("obsolete-fw")
		agent("obsolete-agent", "obsolete-fw", map[string]any{"image": "ghcr.io/x/agent:v1"}, "")

		// A Job with the agent's name that this AgentDeployment does not own.
		foreign := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "obsolete-agent", Namespace: "default"},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers:    []corev1.Container{{Name: "x", Image: "busybox"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

		core("obsolete-agent")

		// Reconciling the Deployment lifecycle must not be blocked by, nor
		// delete, the unrelated Job.
		Expect(container("obsolete-agent")).To(Succeed())

		survived := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "obsolete-agent", Namespace: "default"}, survived)).To(Succeed())
		Expect(survived.UID).To(Equal(foreign.UID), "unrelated Job must not be deleted")

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "obsolete-agent", Namespace: "default"}, dep)).To(Succeed())
	})
})
