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

package agentproviders

import (
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	internalcontroller "github.com/ai-runway/airunway/controller/internal/controller"
)

// Reconciler is the setup contract shared by agent provider shims.
type Reconciler interface {
	SetupWithManager(mgr manager.Manager) error
}

// NewKagentReconciler returns the kagent agent-provider reconciler.
func NewKagentReconciler(c client.Client, scheme *runtime.Scheme) Reconciler {
	return &internalcontroller.KagentProviderReconciler{
		Client: c,
		Scheme: scheme,
	}
}

// NewContainerReconciler returns the container agent-provider reconciler.
func NewContainerReconciler(c client.Client, scheme *runtime.Scheme) Reconciler {
	return &internalcontroller.ContainerProviderReconciler{
		Client: c,
		Scheme: scheme,
	}
}

// NewOrkaReconciler returns the orka agent-provider reconciler.
func NewOrkaReconciler(c client.Client, scheme *runtime.Scheme) Reconciler {
	return &internalcontroller.OrkaProviderReconciler{
		Client: c,
		Scheme: scheme,
	}
}

// NewFrameworkVersionReporter returns a reconciler that publishes a shim's
// build version into the named framework's AgentProviderConfig.status.version,
// matching the reported-version contract the inference provider shims follow.
func NewFrameworkVersionReporter(c client.Client, name, framework, version string) Reconciler {
	return &internalcontroller.AgentProviderVersionReconciler{
		Client:    c,
		Name:      name,
		Framework: framework,
		Version:   version,
	}
}

// NewContainerVersionReporter returns a reconciler that publishes the generic
// container shim's build version into every container-backed
// AgentProviderConfig. The container provider is framework-agnostic, so it
// selects by backend rather than by a single framework name.
func NewContainerVersionReporter(c client.Client, name, version string) Reconciler {
	return &internalcontroller.AgentProviderVersionReconciler{
		Client:  c,
		Name:    name,
		Backend: airunwayv1alpha1.AgentProviderBackendContainer,
		Version: version,
	}
}
