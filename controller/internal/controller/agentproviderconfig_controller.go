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
	"maps"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
)

const (
	// AgentProviderReadinessFieldOwner is the SSA field manager for the
	// provider-readiness reconciler, distinct from every other owner so it
	// only ever writes status.ready and the Ready condition.
	AgentProviderReadinessFieldOwner = "airunway-agents-provider-readiness"

	// agentProviderHeartbeatInterval is how often provider readiness is
	// re-evaluated so a provider that loses its operator (CRD uninstalled)
	// flips back to not-ready, and a stale heartbeat is detectable.
	agentProviderHeartbeatInterval = 60 * time.Second

	// agentProviderHeartbeatTimeout allows several 30-second reporter cycles
	// before declaring a process absent, while still bounding stale readiness.
	agentProviderHeartbeatTimeout = 2 * time.Minute

	// agentProviderHeartbeatBootstrapGrace prevents an already-evaluated
	// framework from visibly flapping not-ready when the core readiness
	// reconciler wins the startup race against its provider reporter. The
	// window is deliberately bounded: after one full heartbeat timeout, a
	// still-missing reporter is a liveness failure and must make the framework
	// not-ready rather than retaining a stale verdict indefinitely.
	agentProviderHeartbeatBootstrapGrace = agentProviderHeartbeatTimeout

	// agentProviderReadyCondition is the condition type mirroring status.ready.
	agentProviderReadyCondition = "Ready"
)

// AgentProviderConfigReconciler keeps AgentProviderConfig.status.ready current
// so provisioning never depends on a human hand-patching provider readiness.
// The actual provider process owns lastHeartbeat through its reporter.
//
// Readiness is data-driven from the provider's declared capabilities:
//   - every backend first requires a fresh heartbeat from its renderer process;
//   - container backends then need no external dependency;
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

	// The bootstrap deadline is initialized lazily by the first reconciliation,
	// not SetupWithManager. That keeps the full grace window available after
	// leader election or a slow manager startup. once makes concurrent first
	// reconciliations safe. now is injectable for deterministic unit tests.
	heartbeatBootstrapOnce     sync.Once
	heartbeatBootstrapDeadline time.Time
	now                        func() time.Time
}

// +kubebuilder:rbac:groups=airunway.ai,resources=agentproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=airunway.ai,resources=agentproviderconfigs/status,verbs=get;update;patch

// Reconcile evaluates and publishes readiness for one AgentProviderConfig.
func (r *AgentProviderConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	defer func() {
		result, err = agentprovider.ResolveStatusWriteConflict(result, err)
	}()

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
	now := r.currentTime()
	r.startHeartbeatBootstrap(now)
	if apc.Status.LastHeartbeat == nil {
		return r.heartbeatUnavailableDuringBootstrap(apc, now,
			"ProviderHeartbeatMissing", "The framework provider has not reported a heartbeat")
	}
	if now.Sub(apc.Status.LastHeartbeat.Time) > agentProviderHeartbeatTimeout {
		return r.heartbeatUnavailableDuringBootstrap(apc, now,
			"ProviderHeartbeatStale",
			fmt.Sprintf("The framework provider's last heartbeat is older than %s", agentProviderHeartbeatTimeout))
	}

	// Catalog validity is deliberately NOT a readiness gate.
	//
	// An earlier version failed readiness on a malformed catalog annotation,
	// because the container provider then returned an error from every
	// reconcile. That was the wrong end to fix. readiness=false sets
	// FrameworkReady=False on every agent, which stops core resolving their
	// bindings, which makes the providers tear the workloads down — so one typo
	// in marketplace UI metadata destroyed every running agent on the framework,
	// including agents that set spec.config.image and never read the catalog at
	// all. CRD backends were hit hardest: kagent and Orka never consume the
	// catalog when rendering, so nothing about their agents was actually broken.
	//
	// The provider now defers the parse failure instead (see
	// resolveContainerProvider), so only agents that genuinely need a catalog
	// image fail, and they fail with the parse error in their own status. That
	// removes the inconsistency this gate existed to close, from the side that
	// does not take healthy agents down with it.
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

func (r *AgentProviderConfigReconciler) currentTime() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *AgentProviderConfigReconciler) startHeartbeatBootstrap(now time.Time) {
	r.heartbeatBootstrapOnce.Do(func() {
		if r.heartbeatBootstrapDeadline.IsZero() {
			r.heartbeatBootstrapDeadline = now.Add(agentProviderHeartbeatBootstrapGrace)
		}
	})
}

// heartbeatUnavailableDuringBootstrap retains an existing readiness verdict
// while provider reporters perform their first reconciliation after a core
// controller restart. It never makes a new framework ready: a config without a
// prior verdict still fails closed. Retention ends after a bounded grace window
// so a genuinely dead provider is detected and normal binding-hold cleanup can
// proceed.
func (r *AgentProviderConfigReconciler) heartbeatUnavailableDuringBootstrap(
	apc *airunwayv1alpha1.AgentProviderConfig,
	now time.Time,
	reason, msg string,
) (ready bool, resultReason, resultMsg string) {
	if apc.Status.Ready != nil && now.Before(r.heartbeatBootstrapDeadline) {
		return *apc.Status.Ready, "ProviderHeartbeatBootstrapRetaining",
			fmt.Sprintf("%s; retaining previous readiness during provider reporter bootstrap", msg)
	}
	return false, reason, msg
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

// applyReadiness writes status.ready and the Ready condition via server-side
// apply under the readiness field owner, leaving provider-owned status fields
// (version and lastHeartbeat) untouched.
func (r *AgentProviderConfigReconciler) applyReadiness(
	ctx context.Context,
	apc *airunwayv1alpha1.AgentProviderConfig,
	ready bool,
	reason, msg string,
) error {
	condStatus := metav1.ConditionFalse
	if ready {
		condStatus = metav1.ConditionTrue
	}
	if providerConfigReadinessCurrent(apc, ready, condStatus, reason, msg) {
		return nil
	}

	apply := &airunwayv1alpha1.AgentProviderConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: airunwayv1alpha1.GroupVersion.String(),
			Kind:       "AgentProviderConfig",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            apc.Name,
			UID:             apc.UID,
			ResourceVersion: apc.ResourceVersion,
		},
		Status: airunwayv1alpha1.AgentProviderConfigStatus{
			Ready: &ready,
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

	return agentprovider.StatusWriteError(r.Status().Patch(ctx, apply, client.Apply,
		client.FieldOwner(AgentProviderReadinessFieldOwner),
		client.ForceOwnership,
	))
}

func providerConfigReadinessCurrent(
	apc *airunwayv1alpha1.AgentProviderConfig,
	ready bool,
	conditionStatus metav1.ConditionStatus,
	reason, message string,
) bool {
	if apc.Status.Ready == nil || *apc.Status.Ready != ready {
		return false
	}
	condition := meta.FindStatusCondition(apc.Status.Conditions, agentProviderReadyCondition)
	return condition != nil &&
		condition.Status == conditionStatus &&
		condition.Reason == reason &&
		condition.Message == message &&
		condition.ObservedGeneration == apc.Generation
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
		For(&airunwayv1alpha1.AgentProviderConfig{},
			ctrlbuilder.WithPredicates(providerConfigReadinessTrigger())).
		Named("agent-provider-config-readiness").
		Complete(r)
}

// providerConfigReadinessTrigger drops routine status-only writes. It retains
// the one status transition that matters immediately: a provider heartbeat
// becoming fresh after being missing or stale. Routine 30-second heartbeats are
// then ignored and the 60-second RequeueAfter remains the readiness pacer.
//
// Filtering here removes the question entirely, and matches what the container
// provider already does for this same type via ProviderConfigRelevantChange.
//
// Kept: creates, deletes, generation changes, annotation changes, and heartbeat
// recovery. Dropped: routine heartbeat/version/readiness status writes.
func providerConfigReadinessTrigger() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldAPC, okOld := e.ObjectOld.(*airunwayv1alpha1.AgentProviderConfig)
			newAPC, okNew := e.ObjectNew.(*airunwayv1alpha1.AgentProviderConfig)
			if !okOld || !okNew {
				return true
			}
			if oldAPC.Generation != newAPC.Generation {
				return true
			}
			now := time.Now()
			oldHeartbeatFresh := oldAPC.Status.LastHeartbeat != nil && now.Sub(oldAPC.Status.LastHeartbeat.Time) <= agentProviderHeartbeatTimeout
			newHeartbeatFresh := newAPC.Status.LastHeartbeat != nil && now.Sub(newAPC.Status.LastHeartbeat.Time) <= agentProviderHeartbeatTimeout
			if !oldHeartbeatFresh && newHeartbeatFresh {
				return true
			}
			return !maps.Equal(oldAPC.Annotations, newAPC.Annotations)
		},
	}
}
