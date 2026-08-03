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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
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
		Expect(apc.Status.LastHeartbeat).NotTo(BeNil())
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

		reconcileWith("cap-crd-install-hint") // no groups served
		out := get("cap-crd-install-hint")
		cond := meta.FindStatusCondition(out.Status.Conditions, agentProviderReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("OperatorNotInstalled"))
		Expect(strings.Contains(cond.Message, "Install instructions")).To(BeTrue())
		Expect(strings.Contains(cond.Message, "kubectl apply -f https://example.com/install.yaml")).To(BeTrue())
	})
})
