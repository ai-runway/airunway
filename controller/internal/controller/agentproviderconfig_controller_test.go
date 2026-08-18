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
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/types"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// stubDiscovery implements just enough of discovery.DiscoveryInterface for the
// readiness reconciler: only ServerGroups is called. Embedding the interface
// (left nil) satisfies the rest of the method set.
type stubDiscovery struct {
	discovery.DiscoveryInterface
	groups []string
}

// ServerGroups accepts either a bare group ("kagent.dev") or "group/version"
// ("kagent.dev/v1alpha2"), so a test can describe a cluster that serves a group
// at one version but not another.
func (s *stubDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	byGroup := map[string][]metav1.GroupVersionForDiscovery{}
	var order []string
	for _, g := range s.groups {
		name, version, hasVersion := strings.Cut(g, "/")
		if _, seen := byGroup[name]; !seen {
			order = append(order, name)
		}
		if hasVersion {
			byGroup[name] = append(byGroup[name], metav1.GroupVersionForDiscovery{
				GroupVersion: name + "/" + version,
				Version:      version,
			})
		} else if _, seen := byGroup[name]; !seen {
			byGroup[name] = nil
		}
	}
	list := &metav1.APIGroupList{}
	for _, name := range order {
		list.Groups = append(list.Groups, metav1.APIGroup{Name: name, Versions: byGroup[name]})
	}
	return list, nil
}

var _ = Describe("AgentProviderConfig readiness controller", func() {
	ctx := context.Background()

	reconcileWith := func(name string, served ...string) {
		r := &AgentProviderConfigReconciler{
			Client:    k8sClient,
			Discovery: &stubDiscovery{groups: served},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}

	create := func(name string, caps airunwayv1alpha1.AgentProviderCapabilities) {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       airunwayv1alpha1.AgentProviderConfigSpec{Capabilities: &caps},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
		reporter := &AgentProviderVersionReconciler{
			Client: k8sClient, Name: "test-" + name, Version: "test",
			Backend: caps.Backend,
		}
		result, err := reporter.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(agentProviderReporterHeartbeatInterval))
	}

	get := func(name string) *airunwayv1alpha1.AgentProviderConfig {
		out := &airunwayv1alpha1.AgentProviderConfig{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, out)).To(Succeed())
		return out
	}

	It("marks a container backend ready without any operator", func() {
		create("cap-container", airunwayv1alpha1.AgentProviderCapabilities{
			Backend: airunwayv1alpha1.AgentProviderBackendContainer,
		})
		reconcileWith("cap-container")

		apc := get("cap-container")
		Expect(apc.Status.Ready).NotTo(BeNil())
		Expect(*apc.Status.Ready).To(BeTrue())
		Expect(apc.Status.LastHeartbeat).NotTo(BeNil(), "the provider reporter must publish the liveness heartbeat")
	})

	It("keeps a framework not-ready until its provider process reports a heartbeat", func() {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cap-no-provider"},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
				Backend: airunwayv1alpha1.AgentProviderBackendContainer,
			}},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })

		reconcileWith("cap-no-provider")
		apc = get("cap-no-provider")
		Expect(*apc.Status.Ready).To(BeFalse())
		cond := meta.FindStatusCondition(apc.Status.Conditions, agentProviderReadyCondition)
		Expect(cond.Reason).To(Equal("ProviderHeartbeatMissing"))
	})

	It("marks a framework not-ready when its provider heartbeat is stale", func() {
		create("cap-stale-provider", airunwayv1alpha1.AgentProviderCapabilities{
			Backend: airunwayv1alpha1.AgentProviderBackendContainer,
		})
		apc := get("cap-stale-provider")
		stale := metav1.NewTime(time.Now().Add(-agentProviderHeartbeatTimeout - time.Minute))
		apc.Status.LastHeartbeat = &stale
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())

		reconcileWith("cap-stale-provider")
		apc = get("cap-stale-provider")
		Expect(*apc.Status.Ready).To(BeFalse())
		cond := meta.FindStatusCondition(apc.Status.Conditions, agentProviderReadyCondition)
		Expect(cond.Reason).To(Equal("ProviderHeartbeatStale"))
	})

	It("retains an existing ready verdict while a missing heartbeat bootstraps", func() {
		now := time.Date(2026, time.August, 12, 1, 2, 3, 0, time.UTC)
		previousReady := true
		r := &AgentProviderConfigReconciler{now: func() time.Time { return now }}
		apc := &airunwayv1alpha1.AgentProviderConfig{
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
				Backend: airunwayv1alpha1.AgentProviderBackendContainer,
			}},
			Status: airunwayv1alpha1.AgentProviderConfigStatus{Ready: &previousReady},
		}

		ready, reason, _ := r.evaluate(apc)
		Expect(ready).To(BeTrue())
		Expect(reason).To(Equal("ProviderHeartbeatBootstrapRetaining"))
	})

	It("retains a stale upgrade heartbeat only for the bounded bootstrap window", func() {
		now := time.Date(2026, time.August, 12, 1, 2, 3, 0, time.UTC)
		previousReady := true
		stale := metav1.NewTime(now.Add(-agentProviderHeartbeatTimeout - time.Minute))
		r := &AgentProviderConfigReconciler{now: func() time.Time { return now }}
		apc := &airunwayv1alpha1.AgentProviderConfig{
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
				Backend: airunwayv1alpha1.AgentProviderBackendContainer,
			}},
			Status: airunwayv1alpha1.AgentProviderConfigStatus{
				Ready:         &previousReady,
				LastHeartbeat: &stale,
			},
		}

		ready, reason, _ := r.evaluate(apc)
		Expect(ready).To(BeTrue())
		Expect(reason).To(Equal("ProviderHeartbeatBootstrapRetaining"))

		now = now.Add(agentProviderHeartbeatBootstrapGrace + time.Second)
		ready, reason, _ = r.evaluate(apc)
		Expect(ready).To(BeFalse())
		Expect(reason).To(Equal("ProviderHeartbeatStale"))
	})

	It("publishes a heartbeat even when the provider version is empty", func() {
		name := "cap-versionless-provider"
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
				Backend: airunwayv1alpha1.AgentProviderBackendContainer,
			}},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })

		reporter := &AgentProviderVersionReconciler{
			Client: k8sClient, Name: "test-versionless", Backend: airunwayv1alpha1.AgentProviderBackendContainer,
		}
		result, err := reporter.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(agentProviderReporterHeartbeatInterval))

		apc = get(name)
		Expect(apc.Status.LastHeartbeat).NotTo(BeNil())
		Expect(apc.Status.Version).To(BeEmpty())
	})

	It("releases a previously reported version when the reporter becomes versionless", func() {
		name := "cap-version-release"
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
				Backend: airunwayv1alpha1.AgentProviderBackendContainer,
			}},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })

		reporter := &AgentProviderVersionReconciler{
			Client: k8sClient, Name: "test-version-release", Version: "v1",
			Backend: airunwayv1alpha1.AgentProviderBackendContainer,
		}
		request := reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
		_, err := reporter.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(get(name).Status.Version).To(Equal("v1"))

		reporter.Version = ""
		_, err = reporter.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(get(name).Status.Version).To(BeEmpty())
	})

	It("rejects stale reporter writes after a provider config is replaced", func() {
		name := "cap-reporter-replacement"
		original := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
				Backend: airunwayv1alpha1.AgentProviderBackendContainer,
			}},
		}
		Expect(k8sClient.Create(ctx, original)).To(Succeed())
		stale := original.DeepCopy()
		Expect(k8sClient.Delete(ctx, original)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &airunwayv1alpha1.AgentProviderConfig{}))
		}).Should(BeTrue())

		replacement := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
				Backend: airunwayv1alpha1.AgentProviderBackendContainer,
			}},
		}
		Expect(k8sClient.Create(ctx, replacement)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, replacement) })

		reporter := &AgentProviderVersionReconciler{
			Client: k8sClient, Name: "test-replacement", Version: "stale",
			Backend: airunwayv1alpha1.AgentProviderBackendContainer,
		}
		Expect(reporter.publishHeartbeat(ctx, stale, metav1.Now())).To(HaveOccurred())
		Expect(reporter.publishVersion(ctx, stale)).To(HaveOccurred())

		current := get(name)
		Expect(current.UID).NotTo(Equal(stale.UID))
		Expect(current.Status.LastHeartbeat).To(BeNil())
		Expect(current.Status.Version).To(BeEmpty())
	})

	It("rejects a stale readiness verdict after a provider config is replaced", func() {
		name := "cap-readiness-replacement"
		original := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
				Backend: airunwayv1alpha1.AgentProviderBackendContainer,
			}},
		}
		Expect(k8sClient.Create(ctx, original)).To(Succeed())
		stale := original.DeepCopy()
		Expect(k8sClient.Delete(ctx, original)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &airunwayv1alpha1.AgentProviderConfig{}))
		}).Should(BeTrue())

		replacement := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
				Backend: airunwayv1alpha1.AgentProviderBackendContainer,
			}},
		}
		Expect(k8sClient.Create(ctx, replacement)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, replacement) })

		r := &AgentProviderConfigReconciler{Client: k8sClient}
		Expect(r.applyReadiness(ctx, stale, true, "Stale", "stale verdict")).To(HaveOccurred())

		current := get(name)
		Expect(current.UID).NotTo(Equal(stale.UID))
		Expect(current.Status.Ready).To(BeNil())
		Expect(current.Status.Conditions).To(BeEmpty())
	})

	It("marks a crd backend ready only when its operator API group is served", func() {
		create("cap-crd-present", airunwayv1alpha1.AgentProviderCapabilities{
			Backend:          airunwayv1alpha1.AgentProviderBackendCRD,
			OperatorAPIGroup: "kagent.dev",
		})
		reconcileWith("cap-crd-present", "kagent.dev", "core.orka.ai")

		apc := get("cap-crd-present")
		Expect(apc.Status.Ready).NotTo(BeNil())
		Expect(*apc.Status.Ready).To(BeTrue())
	})

	It("holds a crd backend not-ready when the operator is absent", func() {
		create("cap-crd-absent", airunwayv1alpha1.AgentProviderCapabilities{
			Backend:          airunwayv1alpha1.AgentProviderBackendCRD,
			OperatorAPIGroup: "kagent.dev",
		})
		reconcileWith("cap-crd-absent") // no groups served

		apc := get("cap-crd-absent")
		Expect(apc.Status.Ready).NotTo(BeNil())
		Expect(*apc.Status.Ready).To(BeFalse())
		cond := meta.FindStatusCondition(apc.Status.Conditions, agentProviderReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("OperatorNotInstalled"))
	})

	It("holds a crd backend not-ready when the group is served but not at the pinned version", func() {
		// The kagent renderer emits kagent.dev/v1alpha2. A cluster running only
		// v1alpha1 satisfies a group-only check, so the framework would report
		// Ready, agents would bind, and every render would then fail on a kind
		// the cluster does not serve — a permanent per-agent error loop instead
		// of one clear signal on the framework.
		create("cap-crd-skew", airunwayv1alpha1.AgentProviderCapabilities{
			Backend:          airunwayv1alpha1.AgentProviderBackendCRD,
			OperatorAPIGroup: "kagent.dev/v1alpha2",
		})
		reconcileWith("cap-crd-skew", "kagent.dev/v1alpha1")

		apc := get("cap-crd-skew")
		Expect(apc.Status.Ready).NotTo(BeNil())
		Expect(*apc.Status.Ready).To(BeFalse(), "an operator serving only v1alpha1 must not satisfy a v1alpha2 renderer")
		cond := meta.FindStatusCondition(apc.Status.Conditions, agentProviderReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("OperatorNotInstalled"))
		Expect(cond.Message).To(ContainSubstring("kagent.dev/v1alpha2"))
	})

	It("marks a crd backend ready when the pinned version is served", func() {
		create("cap-crd-pinned", airunwayv1alpha1.AgentProviderCapabilities{
			Backend:          airunwayv1alpha1.AgentProviderBackendCRD,
			OperatorAPIGroup: "kagent.dev/v1alpha2",
		})
		reconcileWith("cap-crd-pinned", "kagent.dev/v1alpha1", "kagent.dev/v1alpha2")

		apc := get("cap-crd-pinned")
		Expect(apc.Status.Ready).NotTo(BeNil())
		Expect(*apc.Status.Ready).To(BeTrue())
	})

	It("still accepts a bare group, so existing configs keep working", func() {
		create("cap-crd-bare", airunwayv1alpha1.AgentProviderCapabilities{
			Backend:          airunwayv1alpha1.AgentProviderBackendCRD,
			OperatorAPIGroup: "core.orka.ai",
		})
		reconcileWith("cap-crd-bare", "core.orka.ai")

		apc := get("cap-crd-bare")
		Expect(apc.Status.Ready).NotTo(BeNil())
		Expect(*apc.Status.Ready).To(BeTrue(), "a group-only value must behave exactly as before")
	})

	It("holds not-ready when requiresOperator is true but operatorAPIGroup is missing", func() {
		requireTrue := true
		create("cap-crd-misconfigured", airunwayv1alpha1.AgentProviderCapabilities{
			Backend:          airunwayv1alpha1.AgentProviderBackendCRD,
			RequiresOperator: &requireTrue,
		})
		reconcileWith("cap-crd-misconfigured")

		apc := get("cap-crd-misconfigured")
		Expect(apc.Status.Ready).NotTo(BeNil())
		Expect(*apc.Status.Ready).To(BeFalse())
		cond := meta.FindStatusCondition(apc.Status.Conditions, agentProviderReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("OperatorAPIGroupMissing"))
	})

	It("includes install instructions in OperatorNotInstalled when annotated", func() {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cap-crd-install-hint",
				Annotations: map[string]string{
					airunwayv1alpha1.AgentProviderInstallInstructionsAnnotation: "Run: kubectl apply -f https://example.com/install.yaml",
				},
			},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:          airunwayv1alpha1.AgentProviderBackendCRD,
					OperatorAPIGroup: "kagent.dev",
				},
			},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
		reporter := &AgentProviderVersionReconciler{
			Client: k8sClient, Name: "test-install-hint", Version: "test",
			Backend: airunwayv1alpha1.AgentProviderBackendCRD,
		}
		_, err := reporter.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: apc.Name}})
		Expect(err).NotTo(HaveOccurred())

		reconcileWith("cap-crd-install-hint") // no groups served
		out := get("cap-crd-install-hint")
		cond := meta.FindStatusCondition(out.Status.Conditions, agentProviderReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("OperatorNotInstalled"))
		Expect(strings.Contains(cond.Message, "Install instructions")).To(BeTrue())
		Expect(strings.Contains(cond.Message, "kubectl apply -f https://example.com/install.yaml")).To(BeTrue())
	})
})

// TestProviderConfigReadinessTriggerDropsSelfCausedUpdates pins the predicate
// that stops this controller re-triggering on its own status writes.
//
// Provider heartbeat and readiness writes are status-only. Without the filter
// those patches return as update events and enqueue another reconcile, so the
// feedback loop rather than RequeueAfter paces the controller.
func TestProviderConfigReadinessTriggerDropsSelfCausedUpdates(t *testing.T) {
	base := func() *airunwayv1alpha1.AgentProviderConfig {
		return &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "fw",
				Generation:  3,
				Annotations: map[string]string{"airunway.ai/install-instructions": "helm install ..."},
			},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend: airunwayv1alpha1.AgentProviderBackendContainer,
				},
			},
		}
	}
	p := providerConfigReadinessTrigger()

	t.Run("its own heartbeat write is dropped", func(t *testing.T) {
		before, after := base(), base()
		now := metav1.Now()
		later := metav1.NewTime(now.Add(time.Second))
		before.Status.LastHeartbeat = &now
		after.Status.LastHeartbeat = &later
		after.Status.Ready = ptrBool(true)
		if p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
			t.Error("a status-only update must not enqueue another reconcile")
		}
	})

	t.Run("a provider heartbeat appearing triggers readiness immediately", func(t *testing.T) {
		before, after := base(), base()
		now := metav1.Now()
		after.Status.LastHeartbeat = &now
		if !p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
			t.Error("a missing-to-fresh provider heartbeat must trigger readiness")
		}
	})

	t.Run("a spec edit still triggers", func(t *testing.T) {
		before, after := base(), base()
		after.Generation = 4
		if !p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
			t.Error("a generation change must trigger")
		}
	})

	t.Run("an annotation edit still triggers", func(t *testing.T) {
		// Install instructions and the catalog feed readiness messages, and
		// annotation edits do not bump generation.
		before, after := base(), base()
		after.Annotations["airunway.ai/install-instructions"] = "helm install --set new=true"
		if !p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
			t.Error("an annotation change must trigger")
		}
	})

	t.Run("creates and deletes are untouched", func(t *testing.T) {
		if !p.Create(event.CreateEvent{Object: base()}) {
			t.Error("creates must still enqueue")
		}
		if !p.Delete(event.DeleteEvent{Object: base()}) {
			t.Error("deletes must still enqueue")
		}
	})
}
