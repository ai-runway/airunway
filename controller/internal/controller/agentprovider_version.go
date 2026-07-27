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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// AgentProviderVersionReconciler publishes one framework provider's build
// version into its AgentProviderConfig.status.version, mirroring how the
// inference provider shims report ProviderVersion. Without it the Version
// printer column and every AgentDeployment's status.framework.providerVersion
// stay empty, so an operator cannot tell which build is serving a framework.
//
// It writes under its own field owner and touches only status.version, so it
// composes with the readiness reconciler (which owns ready/lastHeartbeat/the
// Ready condition) without either clobbering the other.
type AgentProviderVersionReconciler struct {
	client.Client

	// Name identifies this reporter; it seeds the controller name and the SSA
	// field owner, so each shim gets a distinct owner and two providers never
	// contend for the same field.
	Name string
	// Version is the reported build version, e.g. "agent-kagent-provider:v1.2.3".
	Version string

	// Framework, when set, restricts reporting to the AgentProviderConfig with
	// this name. Used by the framework-specific shims (kagent, orka).
	Framework string
	// Backend, when set, restricts reporting to every AgentProviderConfig
	// declaring this backend. The generic container shim serves any
	// container-backed framework, so it selects by backend rather than name.
	Backend airunwayv1alpha1.AgentProviderBackend
}

// AgentProviderVersionFieldOwner builds the SSA field manager for a version
// reporter.
func AgentProviderVersionFieldOwner(name string) string {
	return "airunway-agents-" + name + "-version"
}

// serves reports whether this reporter publishes a version for the given
// provider config.
func (r *AgentProviderVersionReconciler) serves(apc *airunwayv1alpha1.AgentProviderConfig) bool {
	if r.Framework != "" {
		return apc.Name == r.Framework
	}
	if r.Backend != "" {
		return apc.Spec.Capabilities != nil && apc.Spec.Capabilities.Backend == r.Backend
	}
	return false
}

// +kubebuilder:rbac:groups=airunway.ai,resources=agentproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=airunway.ai,resources=agentproviderconfigs/status,verbs=get;update;patch

// Reconcile publishes the provider version for its framework.
func (r *AgentProviderVersionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var apc airunwayv1alpha1.AgentProviderConfig
	if err := r.Get(ctx, req.NamespacedName, &apc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !apc.DeletionTimestamp.IsZero() || !r.serves(&apc) {
		return ctrl.Result{}, nil
	}
	if apc.Status.Version == r.Version {
		return ctrl.Result{}, nil
	}

	apply := &airunwayv1alpha1.AgentProviderConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: airunwayv1alpha1.GroupVersion.String(),
			Kind:       "AgentProviderConfig",
		},
		ObjectMeta: metav1.ObjectMeta{Name: apc.Name},
		Status:     airunwayv1alpha1.AgentProviderConfigStatus{Version: r.Version},
	}
	if err := r.Status().Patch(ctx, apply, client.Apply,
		client.FieldOwner(AgentProviderVersionFieldOwner(r.Name)),
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish provider version for %q: %w", apc.Name, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the version reporter.
func (r *AgentProviderVersionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Name == "" {
		return fmt.Errorf("AgentProviderVersionReconciler requires a Name")
	}
	if r.Framework == "" && r.Backend == "" {
		return fmt.Errorf("AgentProviderVersionReconciler %q requires a Framework or a Backend", r.Name)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.AgentProviderConfig{},
			ctrlbuilder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				apc, ok := obj.(*airunwayv1alpha1.AgentProviderConfig)
				return ok && r.serves(apc)
			})),
		).
		Named("agent-provider-version-" + r.Name).
		Complete(r)
}
