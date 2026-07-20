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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

const (
	// AgentProviderReadinessFieldOwner is the SSA field manager for the
	// provider-readiness reconciler, distinct from every other owner so it
	// only ever writes status.ready / lastHeartbeat / the Ready condition.
	AgentProviderReadinessFieldOwner = "airunway-agents-provider-readiness"

	// agentProviderHeartbeatInterval is how often provider readiness is
	// re-evaluated so a provider that loses its operator (CRD uninstalled)
	// flips back to not-ready, and a stale heartbeat is detectable.
	agentProviderHeartbeatInterval = 60 * time.Second

	// agentProviderReadyCondition is the condition type mirroring status.ready.
	agentProviderReadyCondition = "Ready"
)

// AgentProviderConfigReconciler keeps AgentProviderConfig.status.ready and
// lastHeartbeat current so that provisioning an agent is fully airunway-driven
// and never depends on a human hand-patching provider readiness.
//
// Readiness is data-driven from the provider's declared capabilities:
//   - container backends are ready whenever this controller is running, because
//     the generic container renderer has no external dependency;
//   - crd backends are ready only once their declared operatorAPIGroup is served
//     in the cluster, so core never renders an agent before the framework
//     operator is installed. (Installing that operator stays an out-of-band /
//     UI-driven admin action; detecting it and flipping readiness is airunway's
//     job.)
type AgentProviderConfigReconciler struct {
	client.Client
	// Discovery is used to check whether a crd backend's operator API group
	// is served in the cluster.
	Discovery discovery.DiscoveryInterface
}

// +kubebuilder:rbac:groups=airunway.ai,resources=agentproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=airunway.ai,resources=agentproviderconfigs/status,verbs=get;update;patch

// Reconcile evaluates and publishes readiness for one AgentProviderConfig.
func (r *AgentProviderConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var apc airunwayv1alpha1.AgentProviderConfig
	if err := r.Get(ctx, req.NamespacedName, &apc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !apc.DeletionTimestamp.IsZero() || apc.Spec.Capabilities == nil {
		return ctrl.Result{}, nil
	}

	ready, reason, msg := r.evaluate(&apc)
	if err := r.applyReadiness(ctx, &apc, ready, reason, msg); err != nil {
		return ctrl.Result{}, err
	}
	// Re-check periodically so readiness tracks operator install/uninstall and
	// the heartbeat stays fresh.
	return ctrl.Result{RequeueAfter: agentProviderHeartbeatInterval}, nil
}

// evaluate derives readiness from the declared capabilities and optional
// provider metadata (such as install instructions annotations).
func (r *AgentProviderConfigReconciler) evaluate(apc *airunwayv1alpha1.AgentProviderConfig) (ready bool, reason, msg string) {
	caps := apc.Spec.Capabilities
	if caps.Backend == airunwayv1alpha1.AgentProviderBackendContainer {
		return true, "ProviderRunning", "Container rendering provider is available"
	}

	// crd backend: gate on the operator's API group being served, when known.
	group := caps.OperatorAPIGroup
	if group == "" {
		return true, "ProviderRunning", "Provider controller is running"
	}
	served, err := r.groupServed(group)
	if err != nil {
		return false, "DiscoveryFailed", fmt.Sprintf("could not query API group %q: %v", group, err)
	}
	if !served {
		msg := fmt.Sprintf("operator API group %q is not installed in the cluster", group)
		if install := apc.InstallInstructions(); install != "" {
			msg = fmt.Sprintf("%s. Install instructions: %s", msg, install)
		}
		return false, "OperatorNotInstalled",
			msg
	}
	return true, "OperatorInstalled", fmt.Sprintf("operator API group %q is present", group)
}

// groupServed reports whether an API group is registered in the cluster.
func (r *AgentProviderConfigReconciler) groupServed(group string) (bool, error) {
	groups, err := r.Discovery.ServerGroups()
	if err != nil {
		return false, err
	}
	for _, g := range groups.Groups {
		if g.Name == group {
			return true, nil
		}
	}
	return false, nil
}

// applyReadiness writes status.ready, lastHeartbeat, and the Ready condition via
// server-side apply under the readiness field owner, leaving other status
// fields (e.g. version) untouched.
func (r *AgentProviderConfigReconciler) applyReadiness(
	ctx context.Context,
	apc *airunwayv1alpha1.AgentProviderConfig,
	ready bool,
	reason, msg string,
) error {
	now := metav1.Now()
	condStatus := metav1.ConditionFalse
	if ready {
		condStatus = metav1.ConditionTrue
	}

	apply := &airunwayv1alpha1.AgentProviderConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: airunwayv1alpha1.GroupVersion.String(),
			Kind:       "AgentProviderConfig",
		},
		ObjectMeta: metav1.ObjectMeta{Name: apc.Name},
		Status: airunwayv1alpha1.AgentProviderConfigStatus{
			Ready:         &ready,
			LastHeartbeat: &now,
			Conditions: []metav1.Condition{{
				Type:               agentProviderReadyCondition,
				Status:             condStatus,
				Reason:             reason,
				Message:            msg,
				LastTransitionTime: providerConfigReadyTransition(apc, condStatus),
				ObservedGeneration: apc.Generation,
			}},
		},
	}

	return r.Status().Patch(ctx, apply, client.Apply,
		client.FieldOwner(AgentProviderReadinessFieldOwner),
		client.ForceOwnership,
	)
}

// providerConfigReadyTransition preserves the Ready condition's existing
// LastTransitionTime when the status is unchanged, so the 60s heartbeat does not
// churn the transition timestamp (only lastHeartbeat updates each tick).
func providerConfigReadyTransition(apc *airunwayv1alpha1.AgentProviderConfig, status metav1.ConditionStatus) metav1.Time {
	if existing := meta.FindStatusCondition(apc.Status.Conditions, agentProviderReadyCondition); existing != nil && existing.Status == status {
		return existing.LastTransitionTime
	}
	return metav1.Now()
}

// SetupWithManager wires the provider-readiness reconciler.
func (r *AgentProviderConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.AgentProviderConfig{}).
		Named("agent-provider-config-readiness").
		Complete(r)
}
