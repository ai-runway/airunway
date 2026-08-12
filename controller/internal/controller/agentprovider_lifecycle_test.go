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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
)

func TestBoundedLabelValue(t *testing.T) {
	short := "my-agent"
	if got := agentprovider.BoundedLabelValue(short); got != short {
		t.Errorf("short name should pass through unchanged, got %q", got)
	}

	long := strings.Repeat("a", 80)
	got := agentprovider.BoundedLabelValue(long)
	if len(got) > agentprovider.MaxLabelValueLength {
		t.Errorf("bounded label = %d bytes, want <= %d", len(got), agentprovider.MaxLabelValueLength)
	}

	// Two distinct long names that share a prefix must not collapse together,
	// otherwise their workloads would share a selector.
	other := strings.Repeat("a", 79) + "b"
	if agentprovider.BoundedLabelValue(other) == got {
		t.Errorf("distinct long names collided on %q", got)
	}
}

func TestBoundedResourceName(t *testing.T) {
	if got := agentprovider.BoundedResourceName("agent", "-config"); got != "agent-config" {
		t.Errorf("short name = %q, want agent-config", got)
	}
	long := strings.Repeat("a", 250)
	got := agentprovider.BoundedResourceName(long, "-config")
	if len(got) > agentprovider.MaxResourceNameLength {
		t.Errorf("bounded name = %d bytes, want <= %d", len(got), agentprovider.MaxResourceNameLength)
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
		r := &AgentDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), CredentialAdmissionActive: true}
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
		// The spec edit bumps .metadata.generation, so ModelBound is stale for
		// the new generation until core re-verifies it. Providers deliberately
		// hold in that window rather than render against the old binding.
		core("job-agent")

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
		core("cm-agent") // re-verify the binding for the bumped generation
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

	It("renders workloads for an AgentDeployment at the maximum permitted name length", func() {
		// The maximum the CEL rule now admits. Note this spec can no longer prove
		// anything about the bounding helpers: at 63 characters they pass their
		// input through byte-identically, and a longer name cannot be created
		// through the API any more. The helpers' long-input behaviour — which
		// still matters for objects admitted before the rule existed — is
		// covered by TestDerivedNamesStayBoundedForPreRuleNames below, which
		// calls them directly.
		longName := strings.Repeat("a", 63)
		provider("long-fw")
		agent(longName, "long-fw", map[string]any{"image": "ghcr.io/x/agent:v1"}, "")

		core(longName)
		Expect(container(longName)).To(Succeed())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: longName, Namespace: "default"}, dep)).To(Succeed())
		for key, value := range dep.Spec.Template.Labels {
			Expect(len(value)).To(BeNumerically("<=", agentprovider.MaxLabelValueLength), "label %q exceeds the limit", key)
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
		dep := &appsv1.Deployment{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "terminal-agent", Namespace: "default"}, dep)
		if err == nil {
			// envtest has no garbage-collector controller, so foreground deletion
			// remains in progress. A real cluster removes the object after its
			// dependants are gone; the important property here is that termination
			// started before the provider reports cleanup complete.
			Expect(dep.DeletionTimestamp.IsZero()).To(BeFalse(), "expected foreground deletion to be in progress")
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected the Deployment to be deleting or deleted, got %v", err)
		}
	})

	It("waits for the previous provider to release its handoff lock before rendering", func() {
		provider("handoff-fw")
		agent("handoff-agent", "handoff-fw", map[string]any{"image": "ghcr.io/x/agent:v1"}, "")
		core("handoff-agent")

		ad := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "handoff-agent", Namespace: "default"}, ad)).To(Succeed())
		Expect(agentprovider.ApplyOwnedStatus(ctx, k8sClient, ad, KagentFieldOwner,
			airunwayv1alpha1.AgentPhaseRunning, nil, nil,
			metav1.ConditionTrue, "AgentReady", "old provider still owns the workload")).To(Succeed())

		By("not rendering while another provider owns the workload lifecycle")
		Expect(container("handoff-agent")).To(Succeed())
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "handoff-agent", Namespace: "default"}, &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		By("rendering only after the old provider releases status ownership")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "handoff-agent", Namespace: "default"}, ad)).To(Succeed())
		Expect(agentprovider.ReleaseOwnedStatus(ctx, k8sClient, ad, KagentFieldOwner)).To(Succeed())
		Expect(container("handoff-agent")).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "handoff-agent", Namespace: "default"}, &appsv1.Deployment{})).To(Succeed())
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

// TestBindingHoldExpiry pins the bound on how long a published binding survives
// a failure it cannot re-verify.
//
// Found on a real cluster: deleting the bound ModelDeployment left the binding
// populated 11 minutes later, pointing at a Service that no longer existed,
// with the agent still advertised as ready. NotFound is classed retryable, so
// an unbounded hold made a genuine revocation indistinguishable from a blip.
func TestBindingHoldExpiry(t *testing.T) {
	withModelBound := func(status metav1.ConditionStatus, age time.Duration) *airunwayv1alpha1.AgentDeployment {
		return &airunwayv1alpha1.AgentDeployment{
			Status: airunwayv1alpha1.AgentDeploymentStatus{
				ModelBinding: &airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1"},
				Conditions: []metav1.Condition{{
					Type:               airunwayv1alpha1.AgentConditionTypeModelBound,
					Status:             status,
					Reason:             "ModelDeploymentNotFound",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-age)),
				}},
			},
		}
	}

	t.Run("a fresh failure holds the binding", func(t *testing.T) {
		ad := withModelBound(metav1.ConditionFalse, 30*time.Second)
		if got := retainBinding(nil, ad); got == nil {
			t.Fatal("a momentary failure must not clear the binding — that is the bug this whole path exists to prevent")
		}
	})

	t.Run("a sustained failure eventually clears it", func(t *testing.T) {
		ad := withModelBound(metav1.ConditionFalse, bindingHoldWindow+time.Minute)
		if got := retainBinding(nil, ad); got != nil {
			t.Fatal("after the hold window the binding must be cleared, so providers tear the agent down")
		}
	})

	t.Run("a polled credential failure includes the refresh blind spot in the ten minute budget", func(t *testing.T) {
		ad := withModelBound(metav1.ConditionFalse, periodicallyRefreshedBindingHoldWindow+time.Second)
		ad.Spec.Model.ExternalAPI = &airunwayv1alpha1.ExternalAPIBinding{
			CredentialsRef: &airunwayv1alpha1.SecretKeyRef{Name: "credential", Key: "token"},
		}
		if got := retainBinding(nil, ad); got != nil {
			t.Fatal("a periodically checked credential must clear early enough that polling plus hold stays within ten minutes")
		}
	})

	t.Run("a healthy binding is never cleared", func(t *testing.T) {
		ad := withModelBound(metav1.ConditionTrue, 24*time.Hour)
		if got := retainBinding(nil, ad); got == nil {
			t.Fatal("ModelBound=True must never expire")
		}
	})

	t.Run("a freshly resolved binding always wins", func(t *testing.T) {
		ad := withModelBound(metav1.ConditionFalse, 24*time.Hour)
		fresh := &airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://new/v1"}
		if got := retainBinding(fresh, ad); got != fresh {
			t.Fatal("a successful resolution must replace whatever was held")
		}
	})
}

// TestWorkloadNotReadyDetail covers the case a real cluster surfaced: in a
// namespace enforcing Pod Security Admission, rendered pods can be REJECTED
// rather than merely slow. The Deployment then sits at 0 replicas and the real
// cause lives on a ReplicaSet event nobody thinks to check, while the agent
// reports a bland "waiting to become available".
func TestWorkloadNotReadyDetail(t *testing.T) {
	t.Run("a plain slow rollout stays generic", func(t *testing.T) {
		reason, msg := workloadNotReadyDetail(&appsv1.Deployment{})
		if reason != "WorkloadNotReady" {
			t.Errorf("reason = %q, want WorkloadNotReady", reason)
		}
		if !strings.Contains(msg, "become available") {
			t.Errorf("unexpected message: %q", msg)
		}
	})

	t.Run("a rejection surfaces the real cause", func(t *testing.T) {
		dep := &appsv1.Deployment{Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentReplicaFailure,
				Status: corev1.ConditionTrue,
				Reason: "FailedCreate",
				Message: `pods "sre-bot-x" is forbidden: violates PodSecurity ` +
					`"restricted:latest": allowPrivilegeEscalation != false`,
			}},
		}}
		reason, msg := workloadNotReadyDetail(dep)
		if reason != "WorkloadRejected" {
			t.Errorf("reason = %q, want WorkloadRejected", reason)
		}
		for _, want := range []string{"FailedCreate", "PodSecurity", "restricted"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message should carry %q so the cause is diagnosable from the agent alone; got: %q", want, msg)
			}
		}
	})

	t.Run("a resolved ReplicaFailure is not reported", func(t *testing.T) {
		dep := &appsv1.Deployment{Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentReplicaFailure,
				Status: corev1.ConditionFalse,
			}},
		}}
		if reason, _ := workloadNotReadyDetail(dep); reason != "WorkloadNotReady" {
			t.Errorf("a cleared ReplicaFailure must not be reported as a rejection, got %q", reason)
		}
	})
}

// TestFrameworkNotReadyDetail covers the other half of the same problem: the
// AgentProviderConfig knows the operator is missing AND how to install it, but
// the agent used to say only "registered but not reporting ready".
func TestFrameworkNotReadyDetail(t *testing.T) {
	t.Run("carries the provider's reason and install hint", func(t *testing.T) {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "orka",
				Annotations: map[string]string{
					airunwayv1alpha1.AgentProviderInstallInstructionsAnnotation: "helm install orka ...",
				},
			},
			Status: airunwayv1alpha1.AgentProviderConfigStatus{
				Conditions: []metav1.Condition{{
					Type:    "Ready",
					Status:  metav1.ConditionFalse,
					Reason:  "OperatorNotInstalled",
					Message: `operator API group "core.orka.ai" is not installed in the cluster`,
				}},
			},
		}
		reason, msg := frameworkNotReadyDetail(apc, "orka")
		if reason != "OperatorNotInstalled" {
			t.Errorf("reason = %q, want the provider's own reason", reason)
		}
		for _, want := range []string{"core.orka.ai", "helm install orka"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message should carry %q so the user can act without reading a cluster-scoped object; got: %q", want, msg)
			}
		}
	})

	t.Run("falls back when the provider has published nothing", func(t *testing.T) {
		apc := &airunwayv1alpha1.AgentProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: "kagent"}}
		if reason, _ := frameworkNotReadyDetail(apc, "kagent"); reason != "FrameworkNotReady" {
			t.Errorf("reason = %q, want the generic fallback", reason)
		}
	})
}

// Gateway API publishes IPAddress status values unbracketed, so an IPv6 gateway
// yields "2001:db8::1". Without bracketing that becomes "http://2001:db8::1/v1",
// where the first colon reads as a port separator and the URL will not parse —
// every agent bound through that gateway gets an endpoint it cannot dial.
func TestNormalizeOpenAIBaseURLHandlesIPv6(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2001:db8::1", "http://[2001:db8::1]/v1"},
		{"::1", "http://[::1]/v1"},
		{"http://2001:db8::1", "http://[2001:db8::1]/v1"},
		{"[2001:db8::1]", "http://[2001:db8::1]/v1"}, // already bracketed
		{"http://[2001:db8::1]:8080", "http://[2001:db8::1]:8080/v1"},
		// IPv4 and hostnames must be untouched, including host:port, which has
		// exactly one colon and must not be mistaken for an IPv6 literal.
		{"1.2.3.4", "http://1.2.3.4/v1"},
		{"demo.default.svc.cluster.local:80", "http://demo.default.svc.cluster.local:80/v1"},
		{"https://api.openai.com/v1", "https://api.openai.com/v1"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeOpenAIBaseURL(c.in); got != c.want {
			t.Errorf("normalizeOpenAIBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

var _ = Describe("Provider status ownership on stand-aside", func() {
	ctx := context.Background()

	It("releases the provider-owned status and its SSA ownership", func() {
		// Needs a real API server: SSA field ownership is what is under test, and
		// the fake client does not implement it.
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "release-fw"},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           airunwayv1alpha1.AgentProviderBackendContainer,
					ModelBindingModes: []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI},
				},
			},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })

		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "release-agent", Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "release-fw"},
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

		By("a provider publishing a healthy status")
		Expect(agentprovider.ApplyOwnedStatus(ctx, k8sClient, ad, ContainerFieldOwner,
			airunwayv1alpha1.AgentPhaseRunning,
			&airunwayv1alpha1.AgentRuntimeStatus{Address: "http://release-agent.default.svc.cluster.local"},
			&airunwayv1alpha1.AgentReplicaStatus{Desired: 1, Ready: 1, Available: 1},
			metav1.ConditionTrue, "WorkloadReady", "ready")).To(Succeed())

		live := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "release-agent", Namespace: "default"}, live)).To(Succeed())
		Expect(live.Status.ProviderOwner).To(Equal(ContainerFieldOwner))
		Expect(live.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning))
		Expect(live.Status.Replicas).NotTo(BeNil())

		By("standing aside")
		Expect(agentprovider.ReleaseOwnedStatus(ctx, k8sClient, live, ContainerFieldOwner)).To(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "release-agent", Namespace: "default"}, live)).To(Succeed())
		Expect(live.Status.Phase).To(BeEmpty(),
			"phase must be released, not just changed — a retained phase deadlocks the successor's first transition")
		Expect(live.Status.ProviderOwner).To(BeEmpty(), "the successor's handoff lock must be released with provider status")
		Expect(live.Status.Runtime).To(BeNil(), "runtime.workloadRef pointed at a deleted workload")
		Expect(live.Status.Replicas).To(BeNil(), "nothing is running, so replicas must not be reported")

		Expect(meta.FindStatusCondition(live.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady)).
			To(BeNil(), "ProviderReady must be released, or a successor provider deadlocks on it")

		By("a successor provider claiming those fields with a non-forced apply")
		// This is the property that matters and that the previous version of this
		// spec did not cover: releasing ownership is what stops the handover
		// deadlocking. ApplyOwnedStatus deliberately does not force, so if the
		// previous manager still owned these fields this call would conflict on
		// every reconcile, forever.
		Expect(agentprovider.ApplyOwnedStatus(ctx, k8sClient, live, KagentFieldOwner,
			airunwayv1alpha1.AgentPhasePending, nil, nil,
			metav1.ConditionFalse, "WaitingForBindings", "taking over")).To(Succeed(),
			"the previous owner must have released these fields")

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "release-agent", Namespace: "default"}, live)).To(Succeed())
		Expect(live.Status.ProviderOwner).To(Equal(KagentFieldOwner))
		succ := meta.FindStatusCondition(live.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(succ).NotTo(BeNil())
		Expect(succ.Reason).To(Equal("WaitingForBindings"))

		By("the successor then transitioning to a DIFFERENT phase")
		// The assertion that was missing. Claiming Pending only works because it
		// happens to match the value the old manager wrote; the real handover is
		// the first transition away from it. If phase ownership was not released,
		// this is where the successor deadlocks against a manager that will never
		// write again.
		Expect(agentprovider.ApplyOwnedStatus(ctx, k8sClient, live, KagentFieldOwner,
			airunwayv1alpha1.AgentPhaseDeploying,
			&airunwayv1alpha1.AgentRuntimeStatus{Address: "http://taken-over"},
			&airunwayv1alpha1.AgentReplicaStatus{Desired: 1},
			metav1.ConditionFalse, "AwaitingKagent", "rendering")).To(Succeed(),
			"a successor must be able to move phase off the value the previous owner left")

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "release-agent", Namespace: "default"}, live)).To(Succeed())
		Expect(live.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
	})
})

// TestDerivedNamesStayBoundedForPreRuleNames covers what the envtest spec no
// longer can. The CRD's CEL rule caps metadata.name at 63, so an over-long name
// cannot be created through the API — but objects admitted before that rule
// existed keep their names, and every derived name must still be legal.
//
// These are called directly rather than through a reconcile precisely because
// the API server would now reject the input.
func TestDerivedNamesStayBoundedForPreRuleNames(t *testing.T) {
	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = strings.Repeat("a", 253) // the object-name limit a pre-rule cluster allowed
	ad.Namespace = "default"
	ad.Spec.Framework.Name = "crewai"

	if got := agentServiceName(ad); len(got) > agentprovider.MaxDNSLabelNameLength {
		t.Errorf("Service name = %d bytes, want <= %d (Services are RFC 1035 labels)",
			len(got), agentprovider.MaxDNSLabelNameLength)
	}
	if got := agentConfigMapName(ad); len(got) > agentprovider.MaxResourceNameLength {
		t.Errorf("ConfigMap name = %d bytes, want <= %d", len(got), agentprovider.MaxResourceNameLength)
	}
	for key, value := range agentLabels(ad) {
		if len(value) > agentprovider.MaxLabelValueLength {
			t.Errorf("label %q = %d bytes, want <= %d", key, len(value), agentprovider.MaxLabelValueLength)
		}
	}
	for key, value := range agentSelector(ad) {
		if len(value) > agentprovider.MaxLabelValueLength {
			t.Errorf("selector %q = %d bytes, want <= %d", key, len(value), agentprovider.MaxLabelValueLength)
		}
	}
}
