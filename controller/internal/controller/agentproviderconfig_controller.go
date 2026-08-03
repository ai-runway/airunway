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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
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
	if !apc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	// A config with no capabilities cannot be evaluated, but returning silently
	// leaves it with no status at all — every AgentDeployment on the framework
	// then reports "registered but not reporting ready" forever with nothing on
	// the provider config explaining why. Publish the reason instead.
	if apc.Spec.Capabilities == nil {
		return ctrl.Result{RequeueAfter: agentProviderHeartbeatInterval},
			r.applyReadiness(ctx, &apc, false, "CapabilitiesMissing",
				"spec.capabilities is not set, so this framework's backend and binding modes are unknown")
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

	// The catalog annotation is unstructured, so nothing rejects a malformed
	// one at admission. Validate it here: the container provider parses the
	// same annotation and fails EVERY reconcile when it cannot, so reporting
	// Ready=True would claim the framework accepts work while all of it fails.
	if _, err := apc.CatalogItems(); err != nil {
		return false, "InvalidCatalog",
			fmt.Sprintf("the %s annotation could not be parsed, so no agent on this framework can render: %v",
				airunwayv1alpha1.AgentProviderCatalogAnnotation, err)
	}

	if caps.Backend == airunwayv1alpha1.AgentProviderBackendContainer {
		return true, "ProviderRunning", "Container rendering provider is available"
	}

	// crd backend: gate on the operator's API group being served, when known.
	group := caps.OperatorAPIGroup
	requiresOperator := caps.RequiresOperator != nil && *caps.RequiresOperator
	if requiresOperator && group == "" {
		msg := "capabilities.requiresOperator is true but capabilities.operatorAPIGroup is empty"
		if install := apc.InstallInstructions(); install != "" {
			msg = fmt.Sprintf("%s. Install instructions: %s", msg, install)
		}
		return false, "OperatorAPIGroupMissing", msg
	}
	if group == "" {
		return true, "ProviderRunning", "Provider controller is running"
	}
	served, err := r.groupServed(group)
	if err != nil {
		// Discovery failing tells us nothing about whether the operator is
		// installed — it is an error reading, not a negative result. Flipping
		// ready=false here would ripple out to every AgentDeployment on this
		// framework, so a transient API-server blip must not be reported as
		// "the framework went away". Hold the previous verdict and retry.
		if prev := apc.Status.Ready; prev != nil {
			return *prev, "DiscoveryFailedRetaining",
				fmt.Sprintf("could not query API group %q: %v (retaining previous readiness)", group, err)
		}
		return false, "DiscoveryFailed", fmt.Sprintf("could not query API group %q: %v", group, err)
	}
	if !served {
		// Worded to cover both cases: the group is absent entirely, or it is
		// served but not at the version this framework pinned. "API %q is not
		// served" is true either way, and naming the exact group/version tells
		// the operator which one to install.
		msg := fmt.Sprintf("operator API %q is not served in the cluster", group)
		if install := apc.InstallInstructions(); install != "" {
			msg = fmt.Sprintf("%s. Install instructions: %s", msg, install)
		}
		return false, "OperatorNotInstalled",
			msg
	}
	return true, "OperatorInstalled", fmt.Sprintf("operator API %q is served", group)
}

// groupServed reports whether the cluster serves the operator API that a
// framework renders into.
//
// The value may be a bare group ("kagent.dev") or a group with a pinned version
// ("kagent.dev/v1alpha2"). A bare group only proves *some* version of the
// operator is installed, which is not the same question: the kagent renderer
// emits kagent.dev/v1alpha2, so a cluster serving only v1alpha1 satisfies a
// group-only check, the framework reports Ready, agents bind, and every render
// then fails on a kind the cluster does not serve. Pinning the version turns
// that permanent per-agent error loop into one OperatorNotInstalled on the
// framework, which is where the operator's absence actually belongs.
func (r *AgentProviderConfigReconciler) groupServed(groupVersion string) (bool, error) {
	group, version, pinned := strings.Cut(groupVersion, "/")

	groups, err := r.Discovery.ServerGroups()
	if err != nil {
		return false, err
	}
	for _, g := range groups.Groups {
		if g.Name != group {
			continue
		}
		if !pinned {
			return true, nil
		}
		for _, v := range g.Versions {
			if v.Version == version {
				return true, nil
			}
		}
		// The group is served but not at the pinned version — an operator is
		// installed, just not one this renderer can target.
		return false, nil
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
