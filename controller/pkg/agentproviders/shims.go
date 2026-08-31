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
func NewKagentReconciler(c client.Client, apiReader client.Reader, scheme *runtime.Scheme) Reconciler {
	return &internalcontroller.KagentProviderReconciler{
		Client:    c,
		APIReader: apiReader,
		Scheme:    scheme,
	}
}

// NewContainerReconciler returns the container agent-provider reconciler. The
// optional API reader keeps the original (client, scheme) call source-compatible;
// production callers should pass manager.GetAPIReader() as the third argument.
func NewContainerReconciler(c client.Client, scheme *runtime.Scheme, apiReaders ...client.Reader) Reconciler {
	var apiReader client.Reader
	if len(apiReaders) > 0 {
		apiReader = apiReaders[0]
	}
	return &internalcontroller.ContainerProviderReconciler{
		Client:    c,
		APIReader: apiReader,
		Scheme:    scheme,
	}
}

// NewOrkaReconciler returns the orka agent-provider reconciler.
func NewOrkaReconciler(c client.Client, apiReader client.Reader, scheme *runtime.Scheme) Reconciler {
	return &internalcontroller.OrkaProviderReconciler{
		Client:    c,
		APIReader: apiReader,
		Scheme:    scheme,
	}
}

// NewFrameworkVersionReporter returns a reconciler that publishes a shim's
// build version into the named framework's AgentProviderConfig.status.version,
// matching the reported-version contract the inference provider shims follow.
//
// backend is required, not inferred. Selecting on the framework name alone means
// a config named "kagent" that declares the container backend is claimed by both
// this reporter and the generic container reporter, and the two then apply
// different versions to status.version under different SSA field owners — so one
// of them loses a conflict on every reconcile, forever. Naming both narrows this
// reporter to the framework/backend pair it actually serves.
func NewFrameworkVersionReporter(
	c client.Client,
	name, framework string,
	backend airunwayv1alpha1.AgentProviderBackend,
	version string,
) Reconciler {
	return &internalcontroller.AgentProviderVersionReconciler{
		Client:    c,
		Name:      name,
		Framework: framework,
		Backend:   backend,
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
