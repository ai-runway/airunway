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
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// AgentProviderReconciler is the minimal contract for an agent provider shim.
// In-tree and out-of-tree providers can both satisfy this by implementing
// SetupWithManager.
type AgentProviderReconciler interface {
	SetupWithManager(mgr manager.Manager) error
}

// AgentProviderRegistration describes one agent provider served in-process.
//
// This is the single seam where a framework provider is bound into the
// controller binary. It exists because the agent providers are still in-tree
// (unlike the inference providers in providers/*, which are separate modules
// with their own RBAC and never import controller internals). Everything a
// provider needs to declare lives here, so moving one out-of-tree means
// deleting its entry — not hunting through main().
type AgentProviderRegistration struct {
	// Name identifies the provider for setup errors, and is the reporter name
	// that seeds its status.version SSA field owner. It MUST match the
	// standalone shim's ProviderConfigName (e.g. "agent-kagent"), so the field
	// owner is identical whether the framework is served by the combined
	// controller or by its shim — otherwise the two become distinct owners of
	// status.version and the second writer deadlocks on a permanent conflict.
	Name string

	// New builds the provider reconciler using the manager's client/scheme.
	New func(client.Client, client.Reader, *runtime.Scheme) AgentProviderReconciler

	// Framework selects the AgentProviderConfig this provider serves by name.
	// Mutually exclusive with Backend.
	Framework string

	// Backend selects every AgentProviderConfig declaring this backend. The
	// generic container provider is framework-agnostic, so it selects by
	// backend rather than by a single name.
	Backend airunwayv1alpha1.AgentProviderBackend

	// Version is the build version reported to status.version. Empty disables
	// version reporting for this provider.
	Version string
}

// RegisterAgentProviders installs the in-process agent providers, wiring each
// one's reconciler and its version reporter together so the two can never drift
// apart.
func RegisterAgentProviders(mgr manager.Manager, regs ...AgentProviderRegistration) error {
	for i := range regs {
		reg := regs[i]
		reconciler := reg.New(mgr.GetClient(), mgr.GetAPIReader(), mgr.GetScheme())
		if err := reconciler.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("unable to create controller %q: %w", reg.Name, err)
		}
		if reg.Version == "" {
			continue
		}
		reporter := &AgentProviderVersionReconciler{
			Client:    mgr.GetClient(),
			Name:      reg.Name,
			Framework: reg.Framework,
			Backend:   reg.Backend,
			Version:   reg.Version,
		}
		if err := reporter.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("unable to create version reporter for %q: %w", reg.Name, err)
		}
	}
	return nil
}
