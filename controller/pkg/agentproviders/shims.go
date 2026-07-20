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

	internalcontroller "github.com/kaito-project/airunway/controller/internal/controller"
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
