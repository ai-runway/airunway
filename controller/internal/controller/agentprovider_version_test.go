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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

func apcNamed(name string, backend airunwayv1alpha1.AgentProviderBackend) *airunwayv1alpha1.AgentProviderConfig {
	return &airunwayv1alpha1.AgentProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: airunwayv1alpha1.AgentProviderConfigSpec{
			Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{Backend: backend},
		},
	}
}

// TestVersionReporterSelectorsAreConjunctive is the regression test for two
// reporters claiming the same AgentProviderConfig.
//
// The selectors used to be evaluated independently — first match wins — so an
// AgentProviderConfig named "kagent" declaring the container backend was served
// by the kagent reporter (name matched) *and* the container reporter (backend
// matched). Both then applied a different status.version under a different SSA
// field owner, so the loser conflicted on every reconcile, permanently.
func TestVersionReporterSelectorsAreConjunctive(t *testing.T) {
	kagent := &AgentProviderVersionReconciler{
		Name: "agent-kagent", Framework: KagentFrameworkName,
		Backend: airunwayv1alpha1.AgentProviderBackendCRD,
	}
	container := &AgentProviderVersionReconciler{
		Name: "agent-container", Backend: airunwayv1alpha1.AgentProviderBackendContainer,
	}

	// The collision case: right name, wrong backend for the framework reporter.
	collide := apcNamed(KagentFrameworkName, airunwayv1alpha1.AgentProviderBackendContainer)
	if kagent.serves(collide) {
		t.Error("the kagent reporter must not claim a config named kagent that declares the container backend")
	}
	if !container.serves(collide) {
		t.Error("the container reporter should still claim it — it is container-backed")
	}

	// Exactly one reporter must serve any given config.
	for _, apc := range []*airunwayv1alpha1.AgentProviderConfig{
		apcNamed(KagentFrameworkName, airunwayv1alpha1.AgentProviderBackendCRD),
		apcNamed(OrkaFrameworkName, airunwayv1alpha1.AgentProviderBackendCRD),
		apcNamed("crewai", airunwayv1alpha1.AgentProviderBackendContainer),
		collide,
	} {
		n := 0
		for _, r := range []*AgentProviderVersionReconciler{
			kagent,
			{Name: "agent-orka", Framework: OrkaFrameworkName, Backend: airunwayv1alpha1.AgentProviderBackendCRD},
			container,
		} {
			if r.serves(apc) {
				n++
			}
		}
		if n > 1 {
			t.Errorf("%s/%s is claimed by %d reporters; they would fight over status.version",
				apc.Name, apc.Spec.Capabilities.Backend, n)
		}
	}
}

func TestVersionReporterIgnoresUnrelatedConfigs(t *testing.T) {
	kagent := &AgentProviderVersionReconciler{
		Name: "agent-kagent", Framework: KagentFrameworkName,
		Backend: airunwayv1alpha1.AgentProviderBackendCRD,
	}
	if kagent.serves(apcNamed("orka", airunwayv1alpha1.AgentProviderBackendCRD)) {
		t.Error("a framework reporter must not claim another framework's config")
	}

	// A config with no capabilities cannot be matched on backend at all.
	bare := &airunwayv1alpha1.AgentProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: "crewai"}}
	container := &AgentProviderVersionReconciler{
		Name: "agent-container", Backend: airunwayv1alpha1.AgentProviderBackendContainer,
	}
	if container.serves(bare) {
		t.Error("a config with no capabilities has no backend to match")
	}

	// A reporter with neither selector set serves nothing, rather than everything.
	none := &AgentProviderVersionReconciler{Name: "misconfigured"}
	if none.serves(apcNamed("crewai", airunwayv1alpha1.AgentProviderBackendContainer)) {
		t.Error("a reporter with no selector must serve nothing")
	}
}
