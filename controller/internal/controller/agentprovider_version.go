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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
)

const agentProviderReporterHeartbeatInterval = 30 * time.Second

// AgentProviderVersionReconciler publishes one framework provider's build
// version into its AgentProviderConfig.status.version, mirroring how the
// inference provider shims report ProviderVersion. Without it the Version
// printer column and every AgentDeployment's status.framework.providerVersion
// stay empty, so an operator cannot tell which build is serving a framework.
//
// It writes status.version and a periodic status.lastHeartbeat under separate
// field owners. The heartbeat proves that the process which actually renders
// this framework is alive; the central readiness reconciler owns only ready and
// the Ready condition.
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

// AgentProviderHeartbeatFieldOwner builds the SSA field manager for a
// provider's liveness heartbeat.
func AgentProviderHeartbeatFieldOwner(name string) string {
	return "airunway-agents-" + name + "-heartbeat"
}

// serves reports whether this reporter publishes a version for the given
// provider config.
//
// Every selector that is set must match. Checking them independently — returning
// on the first one — meant an AgentProviderConfig named "kagent" but declaring
// the container backend was served by two reporters at once: the kagent one
// because the name matched, and the container one because the backend did. Both
// then applied a different status.version under a different field owner, so the
// second lost an SSA conflict on every reconcile, forever.
//
// This is the same collision the provider reconcilers already guard against with
// FrameworkUsesBackend; the version reporters simply never got the equivalent
// gate.
func (r *AgentProviderVersionReconciler) serves(apc *airunwayv1alpha1.AgentProviderConfig) bool {
	if r.Framework == "" && r.Backend == "" {
		return false
	}
	if r.Framework != "" && apc.Name != r.Framework {
		return false
	}
	if r.Backend != "" {
		if apc.Spec.Capabilities == nil || apc.Spec.Capabilities.Backend != r.Backend {
			return false
		}
	}
	return true
}

// +kubebuilder:rbac:groups=airunway.ai,resources=agentproviderconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=airunway.ai,resources=agentproviderconfigs/status,verbs=get;update;patch

// Reconcile publishes the provider version for its framework.
func (r *AgentProviderVersionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	defer func() {
		result, err = agentprovider.ResolveStatusWriteConflict(result, err)
	}()

	var apc airunwayv1alpha1.AgentProviderConfig
	if err := r.Get(ctx, req.NamespacedName, &apc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !apc.DeletionTimestamp.IsZero() || !r.serves(&apc) {
		return ctrl.Result{}, nil
	}

	now := metav1.Now()
	if err := r.publishHeartbeat(ctx, &apc, now); err != nil {
		return ctrl.Result{}, err
	}

	if apc.Status.Version != r.Version {
		if err := r.publishVersion(ctx, &apc); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: agentProviderReporterHeartbeatInterval}, nil
}

func (r *AgentProviderVersionReconciler) publishHeartbeat(
	ctx context.Context,
	apc *airunwayv1alpha1.AgentProviderConfig,
	now metav1.Time,
) error {
	heartbeat := &airunwayv1alpha1.AgentProviderConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: airunwayv1alpha1.GroupVersion.String(),
			Kind:       "AgentProviderConfig",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            apc.Name,
			UID:             apc.UID,
			ResourceVersion: apc.ResourceVersion,
		},
		Status: airunwayv1alpha1.AgentProviderConfigStatus{LastHeartbeat: &now},
	}
	// Force only this dedicated heartbeat apply. Older combined-controller
	// builds owned lastHeartbeat from the readiness manager; taking it here is
	// the intentional one-time ownership migration to the real provider process.
	// The generated CRD type has no ApplyConfiguration implementation yet.
	if err := r.Status().Patch(ctx, heartbeat, client.Apply, //nolint:staticcheck
		client.FieldOwner(AgentProviderHeartbeatFieldOwner(r.Name)),
		client.ForceOwnership,
	); err != nil {
		return fmt.Errorf("publish provider heartbeat for %q: %w", apc.Name, agentprovider.StatusWriteError(err))
	}
	apc.ResourceVersion = heartbeat.ResourceVersion
	apc.Status.LastHeartbeat = &now
	return nil
}

func (r *AgentProviderVersionReconciler) publishVersion(ctx context.Context, apc *airunwayv1alpha1.AgentProviderConfig) error {
	metadata := map[string]any{
		"name":            apc.Name,
		"uid":             string(apc.UID),
		"resourceVersion": apc.ResourceVersion,
	}
	fieldOwner := AgentProviderVersionFieldOwner(r.Name)
	if r.Version == "" {
		// An omitted typed status.version cannot express relinquishing an
		// already-owned field. Apply an otherwise empty status under the same
		// manager so SSA prunes the old version without claiming any condition.
		release := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": airunwayv1alpha1.GroupVersion.String(),
			"kind":       "AgentProviderConfig",
			"metadata":   metadata,
			"status": map[string]any{
				"conditions": []any{},
			},
		}}
		if err := r.Status().Patch(ctx, release, client.Apply, //nolint:staticcheck
			client.FieldOwner(fieldOwner)); err != nil {
			return fmt.Errorf("release provider version for %q: %w", apc.Name, agentprovider.StatusWriteError(err))
		}
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
		Status: airunwayv1alpha1.AgentProviderConfigStatus{Version: r.Version},
	}
	if err := r.Status().Patch(ctx, apply, client.Apply, //nolint:staticcheck
		client.FieldOwner(fieldOwner)); err != nil {
		return fmt.Errorf("publish provider version for %q: %w", apc.Name, agentprovider.StatusWriteError(err))
	}
	return nil
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
			ctrlbuilder.WithPredicates(predicate.And(
				predicate.NewPredicateFuncs(func(obj client.Object) bool {
					apc, ok := obj.(*airunwayv1alpha1.AgentProviderConfig)
					return ok && r.serves(apc)
				}),
				// Heartbeats and version writes are status-only. RequeueAfter is the
				// pacer; feeding those writes straight back into this controller
				// would create the same hot loop the readiness predicate prevents.
				predicate.GenerationChangedPredicate{},
			)),
		).
		Named("agent-provider-version-" + r.Name).
		Complete(r)
}
