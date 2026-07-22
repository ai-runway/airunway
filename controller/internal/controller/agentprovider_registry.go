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
)

// AgentProviderReconciler is the minimal contract for an agent provider shim.
// In-tree and out-of-tree providers can both satisfy this by implementing
// SetupWithManager.
type AgentProviderReconciler interface {
	SetupWithManager(mgr manager.Manager) error
}

// AgentProviderRegistration wires one provider shim into the manager.
type AgentProviderRegistration struct {
	// Name is used only for setup error context.
	Name string
	// New builds the provider reconciler using the manager's client/scheme.
	New func(client.Client, *runtime.Scheme) AgentProviderReconciler
}

// RegisterAgentProviders installs all provider shims in one place.
func RegisterAgentProviders(mgr manager.Manager, regs ...AgentProviderRegistration) error {
	for i := range regs {
		reg := regs[i]
		reconciler := reg.New(mgr.GetClient(), mgr.GetScheme())
		if err := reconciler.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("unable to create controller %q: %w", reg.Name, err)
		}
	}
	return nil
}
