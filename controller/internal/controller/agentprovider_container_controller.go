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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sretry "k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
)

const (
	// ContainerFieldOwner is the container provider's server-side apply field
	// manager, distinct from core and other providers.
	ContainerFieldOwner = "airunway-agents-container"

	// agentConfigMountPath is the pinned BYO config contract: the container
	// provider mounts the agent's spec.config as agent.json here, and sets
	// AIRUNWAY_AGENT_CONFIG to the file path. Any container-backed framework
	// image (OpenClaw, CrewAI, LangGraph, Hermes) reads its config from here.
	agentConfigMountDir  = "/etc/airunway"
	agentConfigFileName  = "agent.json"
	agentConfigMountPath = agentConfigMountDir + "/" + agentConfigFileName

	// agentContainerPort is the port the BYO agent server listens on.
	agentContainerPort      = 8080
	agentContainerPortName  = "http"
	agentContainerName      = "agent"
	agentImageLatestTag     = "latest"
	agentJobCompletedReason = "JobCompleted"
	agentJobFailedReason    = "JobFailed"

	// agentConfigChecksumAnnotation carries a digest of the rendered agent.json
	// on the pod template. The ConfigMap is mounted by name, so without this a
	// config-only change updates the ConfigMap but leaves the pod template
	// byte-identical and no rollout happens — agents that read agent.json at
	// startup would keep serving the old config indefinitely.
	agentConfigChecksumAnnotation = "airunway.ai/config-checksum"

	// agentAccessChecksumAnnotation rolls a Deployment when its provider-managed
	// ingress token changes. Environment Secret refs are resolved only when a pod
	// starts, so without a template digest recreating a deleted token Secret
	// would make the Secret and the running proxy disagree indefinitely.
	agentAccessChecksumAnnotation   = "airunway.ai/access-token-checksum"
	agentAccessSecretSuffix         = "-api-auth"
	agentAccessTokenKey             = "token"
	agentAccessTokenEnv             = "AIRUNWAY_AGENT_API_KEY"
	agentAccessTokenBytes           = 32
	agentAccessNameHashBytes        = 16
	agentAccessCreateAttempts       = 5
	agentAccessSecretCreateTimeout  = 15 * time.Second
	agentAccessSecretAmbiguityGrace = 2 * time.Minute
	// Provider-owned ConfigMap annotations are crash-recovery state, not an
	// authorization boundary. AgentDeployment author roles must not grant child
	// ConfigMap writes; isolating mutually untrusted providers needs protected storage.
	agentAccessPendingAnnotation         = "airunway.ai/pending-access-secret"
	agentAccessCreateStartedAnnotation   = "airunway.ai/pending-access-secret-create-started"
	agentAccessCreateStartedAtAnnotation = "airunway.ai/pending-access-secret-create-started-at"

	// agentModelCredentialChecksumAnnotation rolls long-running pods when the
	// bound model Secret is updated or replaced. Secret-backed environment
	// variables are resolved only at pod start.
	agentModelCredentialChecksumAnnotation = "airunway.ai/model-credential-checksum"
	agentCredentialSecretIndexKey          = "status.secretRefs"

	// The provider gives every author-selected image its own empty ServiceAccount
	// rather than letting ServiceAccount admission attach the namespace default's
	// imagePullSecrets.
	agentServiceAccountSuffix = "-runtime"

	// A job execution is claimed before the Job is created and its terminal
	// outcome is recorded on the provider-owned ConfigMap. This is the durable
	// at-most-once ledger for one execution per AgentDeployment generation.
	agentJobGenerationAnnotation     = "airunway.ai/job-generation"
	agentJobOutcomeAnnotation        = "airunway.ai/job-outcome"
	agentJobClaimNonceAnnotation     = "airunway.ai/job-claim-nonce"
	agentJobOutcomeCompleted         = "completed"
	agentJobOutcomeFailed            = "failed"
	agentJobOutcomeLost              = "lost"
	agentJobOutcomeRetryable         = "retryable"
	agentJobClaimNonceBytes          = 16
	agentJobMigrationAmbiguousReason = "JobMigrationAmbiguous"

	// Catalog images may follow the running container provider version without
	// hard-coding a tag that has not been published yet.
	agentVersionPlaceholder             = "${AIRUNWAY_VERSION}"
	agentCatalogImageRevisionAnnotation = "airunway.ai/catalog-image-revision"

	// defaultAgentRunAsUser is the conventional distroless/nonroot UID. It is
	// both the numeric default the kubelet needs (see agentPodSpec) and the value
	// the security floor falls back to when an override asks for root.
	defaultAgentRunAsUser int64 = 65532

	// agentTemplateHashAnnotation records the accepted Job template for
	// diagnostics. It deliberately does not authorize same-generation
	// recreation: once a Job is accepted, the at-most-once ledger treats it as
	// that generation's execution even if provider-derived inputs later drift.
	agentTemplateHashAnnotation = "airunway.ai/template-hash"
)

var errAmbiguousLegacyJobGeneration = errors.New("ambiguous legacy agent Job generation")

// ContainerProviderReconciler renders any container-backend AgentDeployment
// (OpenClaw, CrewAI, LangGraph, Hermes, ...) into a Deployment + Service +
// ConfigMap. A single generic provider serves every container framework
// because the image is supplied per-deployment (spec.config.image) or by the
// framework's catalog annotation entry — the framework is data, not code.
type ContainerProviderReconciler struct {
	client.Client
	Scheme                    *runtime.Scheme
	APIReader                 client.Reader
	agentAccessNow            func() time.Time
	agentAccessCreateTimeout  time.Duration
	agentAccessAmbiguityGrace time.Duration
}

func (r *ContainerProviderReconciler) accessNow() time.Time {
	if r.agentAccessNow != nil {
		return r.agentAccessNow().UTC()
	}
	return time.Now().UTC()
}

func (r *ContainerProviderReconciler) accessCreateTimeout() time.Duration {
	if r.agentAccessCreateTimeout > 0 {
		return r.agentAccessCreateTimeout
	}
	return agentAccessSecretCreateTimeout
}

func (r *ContainerProviderReconciler) accessAmbiguityGrace() time.Duration {
	if r.agentAccessAmbiguityGrace > 0 {
		return r.agentAccessAmbiguityGrace
	}
	return agentAccessSecretAmbiguityGrace
}

func parseContainerSecurityOverrides(ad *airunwayv1alpha1.AgentDeployment) (*containerSecurityOverrides, error) {
	if ad.Spec.Provider == nil || ad.Spec.Provider.Overrides == nil || len(ad.Spec.Provider.Overrides.Raw) == 0 {
		return nil, nil
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(ad.Spec.Provider.Overrides.Raw, &root); err != nil {
		return nil, fmt.Errorf("parse spec.provider.overrides JSON: %w", err)
	}

	keys := []string{"workload", "container"}

	var merged containerSecurityOverrides
	var found bool
	for _, key := range keys {
		sectionRaw, ok := root[key]
		if !ok || len(sectionRaw) == 0 || string(sectionRaw) == "null" {
			continue
		}
		var section containerSecurityOverrides
		if err := json.Unmarshal(sectionRaw, &section); err != nil {
			return nil, fmt.Errorf("parse spec.provider.overrides.%s: %w", key, err)
		}
		merged.PodSecurityContext = mergePodSecurityContext(merged.PodSecurityContext, section.PodSecurityContext)
		merged.SecurityContext = mergeSecurityContext(merged.SecurityContext, section.SecurityContext)
		found = true
	}

	if !found {
		return nil, nil
	}
	return &merged, nil
}

func mergePodSecurityContext(dst, src *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	if src == nil {
		return dst
	}
	if dst == nil {
		dst = &corev1.PodSecurityContext{}
	}
	if src.RunAsUser != nil {
		dst.RunAsUser = src.RunAsUser
	}
	if src.RunAsGroup != nil {
		dst.RunAsGroup = src.RunAsGroup
	}
	if src.RunAsNonRoot != nil {
		dst.RunAsNonRoot = src.RunAsNonRoot
	}
	if src.FSGroup != nil {
		dst.FSGroup = src.FSGroup
	}
	if len(src.SupplementalGroups) > 0 {
		dst.SupplementalGroups = append([]int64(nil), src.SupplementalGroups...)
	}
	if src.FSGroupChangePolicy != nil {
		dst.FSGroupChangePolicy = src.FSGroupChangePolicy
	}
	if src.SeccompProfile != nil {
		dst.SeccompProfile = src.SeccompProfile.DeepCopy()
	}
	return dst
}

func mergeSecurityContext(dst, src *corev1.SecurityContext) *corev1.SecurityContext {
	if src == nil {
		return dst
	}
	if dst == nil {
		dst = &corev1.SecurityContext{}
	}
	if src.RunAsUser != nil {
		dst.RunAsUser = src.RunAsUser
	}
	if src.RunAsGroup != nil {
		dst.RunAsGroup = src.RunAsGroup
	}
	if src.RunAsNonRoot != nil {
		dst.RunAsNonRoot = src.RunAsNonRoot
	}
	if src.AllowPrivilegeEscalation != nil {
		dst.AllowPrivilegeEscalation = src.AllowPrivilegeEscalation
	}
	if src.ReadOnlyRootFilesystem != nil {
		dst.ReadOnlyRootFilesystem = src.ReadOnlyRootFilesystem
	}
	if src.Capabilities != nil {
		dst.Capabilities = &corev1.Capabilities{
			Drop: append([]corev1.Capability(nil), src.Capabilities.Drop...),
		}
	}
	if src.SeccompProfile != nil {
		dst.SeccompProfile = src.SeccompProfile.DeepCopy()
	}
	return dst
}

func applyContainerSecurityOverrides(
	podSecurity *corev1.PodSecurityContext,
	containerSecurity *corev1.SecurityContext,
	overrides *containerSecurityOverrides,
	writableRoot bool,
) {
	if overrides != nil {
		podMerged := mergePodSecurityContext(podSecurity, overrides.PodSecurityContext)
		containerMerged := mergeSecurityContext(containerSecurity, overrides.SecurityContext)
		*podSecurity = *podMerged
		*containerSecurity = *containerMerged
	}
	clampSecurityFloor(podSecurity, containerSecurity, writableRoot)
}

// clampSecurityFloor re-asserts the hardening the overrides are not allowed to
// weaken, after the merge that could have weakened it.
//
// The webhook rejects these values too, and that is the better error — it tells
// the author at apply time, in terms of the field they set. But the webhook is
// optional: `ENABLE_WEBHOOKS=false` is a supported mode, and a resource admitted
// before the webhook existed is never re-validated. Leaving the floor to
// admission alone means `runAsNonRoot: false`, `runAsUser: 0`,
// `allowPrivilegeEscalation: true`, `readOnlyRootFilesystem: false` or
// `seccompProfile: Unconfined` reach the rendered pod in those cases, because the
// override is merged *after* the hardened default and therefore wins.
//
// So the render path enforces it independently. Validation is for the error
// message; this is for the guarantee.
func clampSecurityFloor(
	podSecurity *corev1.PodSecurityContext,
	containerSecurity *corev1.SecurityContext,
	writableRoot bool,
) {
	// Root is never negotiable, at either level.
	if podSecurity != nil {
		podSecurity.RunAsNonRoot = ptr.To(true)
		if podSecurity.RunAsUser != nil && *podSecurity.RunAsUser == 0 {
			podSecurity.RunAsUser = ptr.To[int64](defaultAgentRunAsUser)
		}
		podSecurity.SeccompProfile = clampSeccomp(podSecurity.SeccompProfile)
	}
	if containerSecurity == nil {
		return
	}
	containerSecurity.RunAsNonRoot = ptr.To(true)
	if containerSecurity.RunAsUser != nil && *containerSecurity.RunAsUser == 0 {
		containerSecurity.RunAsUser = ptr.To[int64](defaultAgentRunAsUser)
	}
	containerSecurity.AllowPrivilegeEscalation = ptr.To(false)
	// Whether the root filesystem *may* be writable is provider-owned, declared
	// by AgentProviderConfig.spec.capabilities.writableRootFilesystem — so an
	// author cannot weaken what their framework declared.
	//
	// This is a floor, not a pin. A framework that did not declare a writable
	// root gets read-only forced on regardless of what the author asked for. A
	// framework that did gets it off by default, but an author may still turn it
	// back on: hardening is always allowed, and pinning here would have silently
	// discarded a `readOnlyRootFilesystem: true` override that the webhook
	// explicitly accepts.
	if !writableRoot {
		containerSecurity.ReadOnlyRootFilesystem = ptr.To(true)
	} else if containerSecurity.ReadOnlyRootFilesystem == nil {
		containerSecurity.ReadOnlyRootFilesystem = ptr.To(false)
	}
	containerSecurity.SeccompProfile = clampSeccomp(containerSecurity.SeccompProfile)
	containerSecurity.Capabilities = clampCapabilities(containerSecurity.Capabilities)
}

// clampCapabilities guarantees ALL is dropped and nothing is added back, while
// leaving any additional drops in place — matching what the webhook asks for
// (drop must *include* ALL) rather than flattening a legitimate
// ["ALL", "NET_RAW"] down to ["ALL"].
func clampCapabilities(c *corev1.Capabilities) *corev1.Capabilities {
	if c == nil {
		return &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
	}
	out := &corev1.Capabilities{Drop: append([]corev1.Capability(nil), c.Drop...)}
	if slices.Contains(out.Drop, "ALL") {
		return out
	}
	// Adding capabilities is never permitted, so c.Add is dropped entirely.
	return &corev1.Capabilities{Drop: append([]corev1.Capability{"ALL"}, out.Drop...)}
}

// clampSeccomp refuses Unconfined, which would opt the pod out of the syscall
// filter entirely.
//
// Localhost profiles are left alone, and this is the one hole in "an override
// can harden but never weaken". An agent author cannot *forge* a node-local
// profile — it has to exist under the kubelet's seccomp root, which needs node
// access or something like the Security Profiles Operator — but they can
// *reference* one, and a profile placed there for another workload may permit
// more than RuntimeDefault does. Blocking Localhost outright would also remove
// the legitimate case it exists for: clusters that ship hardened custom
// profiles. Closing this properly means making the allowed profiles
// provider-owned, the way writableRootFilesystem already is; recorded as a gap
// rather than guessed at here.
func clampSeccomp(p *corev1.SeccompProfile) *corev1.SeccompProfile {
	if p == nil || p.Type == corev1.SeccompProfileTypeUnconfined {
		return &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	return p
}

// containerConfig is the container-backend spec.config contract. The full
// spec.config is also mounted verbatim as agent.json; these are the fields the
// provider itself consumes to render the workload.
type containerConfig struct {
	// Image is the BYO agent container image. Required for the container
	// backend unless the framework's catalog supplies a default.
	Image string `json:"image,omitempty"`
	// Command overrides the image entrypoint. Useful for generic/dev images
	// (e.g. a smoke-test server) and for wrapping frameworks whose default
	// entrypoint is not an HTTP server.
	Command []string `json:"command,omitempty"`
	// Args overrides the image arguments.
	Args []string `json:"args,omitempty"`
	// Port overrides the container/serving port. Real framework images serve on
	// varied ports (e.g. LangGraph 8000, OpenClaw 18789); this lets the
	// Service target the right one. Defaults to 8080.
	Port int32 `json:"port,omitempty"`
}

// containerPort returns the configured serving port, defaulting to 8080.
func containerPort(cfg containerConfig) int32 {
	if cfg.Port > 0 {
		return cfg.Port
	}
	return agentContainerPort
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;configmaps,verbs=get;list;watch;create;update;patch;delete
// The container provider creates one owner-referenced ingress token Secret per
// long-running agent. `get` is the authoritative ownership check that prevents
// a same-named user Secret from being adopted. list/watch power a metadata-only
// informer so a referenced model credential update or deletion immediately
// requeues the affected agents without caching Secret data.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;delete

// Reconcile renders the container workload for a container-backed AgentDeployment.
//
//nolint:gocyclo // Reconcile is an explicit fail-closed state machine spanning lifecycle, credentials, and workloads.
func (r *ContainerProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	defer func() {
		result, err = agentprovider.ResolveStatusWriteConflict(result, err)
	}()

	var ad airunwayv1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &ad); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ad.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Only handle agents whose framework uses the container backend. Resolve
	// the provider-owned settings (backend, default image, security posture)
	// from the framework's AgentProviderConfig; ignore the agent otherwise.
	settings, err := r.resolveContainerProvider(ctx, &ad)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !settings.isContainer {
		// Either this framework was never container-backed, or it was and has
		// since been deleted or re-registered onto a CRD backend. In the second
		// case the Deployment/Job and Service rendered here are now orphaned —
		// nothing reconciles them, and the CRD provider may already be rendering a
		// second workload beside them. Tear ours down before standing aside.
		// Preserve a Job agent's ConfigMap ledger, however: deleting both it and
		// the Job would make the same generation executable again if the container
		// registration returns.
		//
		// For agents that were never ours this is a no-op: each delete does a
		// cached read first and skips objects that are absent or not controlled
		// by this AgentDeployment.
		if ad.Spec.Lifecycle == airunwayv1alpha1.AgentLifecycleJob {
			releasedClaim, err := r.preflightJobLedgerBeforeBindingCleanup(ctx, &ad)
			if err != nil {
				return ctrl.Result{}, err
			}
			if releasedClaim {
				return ctrl.Result{Requeue: true}, nil
			}
		}
		pending, err := r.cleanupOwnedWorkloadsForBinding(ctx, &ad)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.status(ctx, &ad,
				airunwayv1alpha1.AgentPhaseDeploying, nil, nil, metav1.ConditionFalse,
				"ProviderHandoffCleanup", "Removing the previous container workload before handing the agent to its new provider")
		}
		// See the kagent provider: release the provider-owned status and its SSA
		// ownership so the status is not left stale and a successor provider can
		// take over.
		return ctrl.Result{}, agentprovider.ReleaseOwnedStatus(ctx, r.Client, &ad, ContainerFieldOwner)
	}
	if agentprovider.ProviderHandoffPending(&ad, ContainerFieldOwner) {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Consume the core-resolved binding.
	switch agentprovider.ClassifyBinding(&ad) {
	case agentprovider.BindingUnavailable:
		// No binding: either not resolved yet, or core cleared it because the
		// request became terminally invalid (e.g. a cross-namespace reference).
		// Before tearing down a one-shot Job, migrate any pre-ledger execution
		// evidence onto the preserved ConfigMap. Otherwise deleting the Job and
		// replacing its provider status here can make the same generation look
		// unused when the binding later recovers.
		if ad.Spec.Lifecycle == airunwayv1alpha1.AgentLifecycleJob {
			releasedClaim, err := r.preflightJobLedgerBeforeBindingCleanup(ctx, &ad)
			if err != nil {
				// Keep both the Job and its Job-specific status intact until the
				// durable ledger write succeeds.
				return ctrl.Result{}, err
			}
			if releasedClaim {
				return ctrl.Result{Requeue: true}, nil
			}
		}
		// Tear down so the agent stops serving with stale endpoint/credentials.
		pending, err := r.cleanupOwnedWorkloadsForBinding(ctx, &ad)
		if err != nil {
			return r.failWithAccessCredentialStatus(ctx, &ad, currentAccessCredentialRef(&ad), "BindingCleanupFailed", err)
		}
		if pending {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.status(ctx, &ad,
				airunwayv1alpha1.AgentPhaseDeploying, nil, nil, metav1.ConditionFalse,
				"BindingCleanup", "Stopping the agent workload after its model binding was removed")
		}
		return ctrl.Result{}, r.status(ctx, &ad, airunwayv1alpha1.AgentPhasePending, nil, nil,
			metav1.ConditionFalse, "WaitingForBindings", "Waiting for the core controller to resolve model bindings")
	case agentprovider.BindingStale:
		// Core still publishes a binding but could not re-verify it this pass.
		// Hold verified workloads so a transient binding blip does not restart a
		// healthy agent. The provider-owned ingress credential is independent of
		// that model binding, however, and revocation must remain immediate while
		// the binding is stale: pods load the Secret-backed token only at startup.
		return r.reconcileStaleBinding(ctx, &ad)
	}

	cfg, err := parseContainerConfig(ad.Spec.Config)
	if err != nil {
		return r.terminalFailure(ctx, &ad, "InvalidConfig", err.Error())
	}
	catalogImageRevision := ""
	if cfg.Image == "" {
		cfg.Image = settings.image // fall back to the framework's unambiguous catalog image
		catalogImageRevision = settings.imageRevision
	}
	if cfg.Image == "" {
		msg := "No container image: set spec.config.image or a framework catalog image"
		if settings.imageErr != "" {
			msg = settings.imageErr
		}
		return r.terminalFailure(ctx, &ad, "MissingImage", msg)
	}
	securityOverrides, err := parseContainerSecurityOverrides(&ad)
	if err != nil {
		return r.terminalFailure(ctx, &ad, "InvalidProviderOverrides", err.Error())
	}

	binding := *ad.Status.ModelBinding

	// Complete the previous lifecycle's foreground deletion before touching the
	// shared ConfigMap or creating the replacement workload. Updating the
	// ConfigMap first could change the configuration mounted into the old pods
	// while they are still running.
	var obsolete []client.Object
	var retiringAccessRef *airunwayv1alpha1.SecretKeyRef
	cleanupMessage := "Removing the previous job before starting the long-running workload"
	if ad.Spec.Lifecycle == airunwayv1alpha1.AgentLifecycleJob {
		if ad.Status.Runtime != nil {
			retiringAccessRef = ad.Status.Runtime.AuthSecretRef
		}
		obsolete = []client.Object{
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ad.Name}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: agentServiceName(&ad)}},
		}
		cleanupMessage = "Removing the long-running workload before starting the agent job"
	} else {
		obsolete = []client.Object{&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: ad.Name}}}
	}
	deleting, err := r.deleteObsolete(ctx, &ad, obsolete...)
	if err != nil {
		return r.failWithAccessCredentialStatus(ctx, &ad, retiringAccessRef, "LifecycleCleanupFailed", err)
	}
	if deleting {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.status(ctx, &ad,
			airunwayv1alpha1.AgentPhaseDeploying, accessCredentialRuntime(retiringAccessRef), nil, metav1.ConditionFalse,
			"LifecycleSwitching", cleanupMessage)
	}
	if ad.Spec.Lifecycle == airunwayv1alpha1.AgentLifecycleJob {
		// The Deployment and Service are authoritatively gone, so no running pod
		// needs an ingress credential. Resolve any unpublished random Secret from
		// the ConfigMap journal before the Job path preserves that ledger without
		// ever consuming the reservation. Ambiguous creation or failed deletion
		// keeps the journal intact and blocks the lifecycle switch for a safe retry.
		if err := r.cleanupAgentAccessSecretReservation(ctx, &ad); err != nil {
			return r.failWithAccessCredentialStatus(ctx, &ad, retiringAccessRef, "LifecycleCleanupFailed", err)
		}
		// Delete the published credential before Job status replaces the only
		// persisted reference to its unguessable name.
		if err := r.deleteAgentAccessSecret(ctx, &ad, retiringAccessRef); err != nil {
			return r.failWithAccessCredentialStatus(ctx, &ad, retiringAccessRef, "LifecycleCleanupFailed", err)
		}
		if err := r.deleteAgentAccessSecret(ctx, &ad, legacyAccessCredentialRef(&ad)); err != nil {
			return r.failWithAccessCredentialStatus(ctx, &ad, retiringAccessRef, "LifecycleCleanupFailed",
				fmt.Errorf("delete legacy agent access Secret before starting Job: %w", err))
		}
		deleting, err = r.deletePreviousGenerationJob(ctx, &ad)
		if err != nil {
			return r.failJobLedgerMigration(ctx, &ad, "JobGenerationCleanupFailed", err)
		}
		if deleting {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.status(ctx, &ad,
				airunwayv1alpha1.AgentPhaseDeploying, nil, nil, metav1.ConditionFalse,
				"JobGenerationSwitching", "Removing the previous generation's agent job")
		}
	}

	// The ConfigMap (mounted agent.json) is shared by both lifecycles. Its
	// checksum rides on the pod template so a config-only edit rolls the
	// workload. Job execution state lives on this same owned object, so persist a
	// newly terminal outcome before ServiceAccount fail-closed cleanup can remove
	// the only Job carrying that evidence.
	configMap := renderAgentConfigMap(&ad)
	configChecksum, err := agentprovider.HashJSON(configMap.Data)
	if err != nil {
		return r.failWithStatus(ctx, &ad, "RenderFailed", err)
	}
	if err := r.applyOwned(ctx, &ad, configMap); err != nil {
		return r.failJobLedgerMigration(ctx, &ad, "RenderFailed", err)
	}
	var recordedJobOutcome string
	if ad.Spec.Lifecycle == airunwayv1alpha1.AgentLifecycleJob {
		outcome, releasedClaim, err := r.preflightJobLedger(ctx, &ad, configMap)
		if err != nil {
			return r.failJobLedgerMigration(ctx, &ad, "JobLedgerFailed", err)
		}
		if releasedClaim {
			return ctrl.Result{Requeue: true}, nil
		}
		recordedJobOutcome = outcome
	}
	if recordedJobOutcome != "" {
		return r.reportRecordedJobOutcome(ctx, &ad, recordedJobOutcome, nil)
	}

	serviceAccount, repaired, err := r.ensureAgentServiceAccount(ctx, &ad)
	if err != nil {
		return r.failClosedForServiceAccount(ctx, &ad, err)
	}
	if repaired {
		return r.failClosedForServiceAccount(ctx, &ad, fmt.Errorf(
			"removed unexpected credential-bearing fields from ServiceAccount %s/%s; retrying only after the previous workload is stopped",
			serviceAccount.Namespace, serviceAccount.Name))
	}

	modelCredentialChecksum, err := r.modelCredentialChecksum(ctx, ad.Namespace, binding)
	if err != nil {
		// A credential Secret can be replaced, lose its resolved key, or become unreadable after this
		// provider rendered the workload. Do not leave the old pod serving with
		// stale credentials while retrying the authoritative Secret read.
		if _, cleanupErr := r.cleanupOwnedWorkloadsForBinding(ctx, &ad); cleanupErr != nil {
			return r.failWithAccessCredentialStatus(ctx, &ad, currentAccessCredentialRef(&ad), "CredentialMetadataCleanupFailed", fmt.Errorf(
				"%v; tear down workload after credential metadata read failure: %w", err, cleanupErr))
		}
		return r.failWithStatus(ctx, &ad, "CredentialMetadataReadFailed", err)
	}

	var authSecretRef *airunwayv1alpha1.SecretKeyRef
	var accessTokenChecksum string
	if ad.Spec.Lifecycle != airunwayv1alpha1.AgentLifecycleJob {
		var created bool
		authSecretRef, accessTokenChecksum, created, err = r.ensureAgentAccessCredentials(ctx, &ad)
		if err != nil {
			return r.failWithAccessCredentialStatus(ctx, &ad, currentAccessCredentialRef(&ad), "IngressCredentialProvisionFailed", err)
		}
		if created {
			rt := &airunwayv1alpha1.AgentRuntimeStatus{AuthSecretRef: authSecretRef}
			if err := r.status(ctx, &ad, airunwayv1alpha1.AgentPhaseDeploying, rt, nil,
				metav1.ConditionFalse, "IngressCredentialProvisioned", "Agent ingress credential has been provisioned"); err != nil {
				statusErr := err
				if cleanupErr := r.stopWorkloadForAccessCredentialTransitionFailure(ctx, &ad); cleanupErr != nil {
					// Keep the reserved Secret intact when fail-closed teardown itself
					// fails. The next reconcile can recover this exact random name and
					// retry both publication and teardown without leaking another Secret.
					return ctrl.Result{}, fmt.Errorf("%w; stop workload after agent access credential publication failure: %v", statusErr, cleanupErr)
				}
				if deleteErr := r.deleteAgentAccessSecret(ctx, &ad, authSecretRef); deleteErr != nil {
					// The ConfigMap reservation was written before Secret creation.
					// Keep it when both publication and cleanup fail so the next
					// reconcile can recover this exact Secret instead of allocating
					// another unreachable random name.
					return ctrl.Result{}, fmt.Errorf("%w; delete unpublished agent access Secret: %v", statusErr, deleteErr)
				}
				if clearErr := r.clearAgentAccessSecretReservation(ctx, &ad, authSecretRef.Name); clearErr != nil {
					return ctrl.Result{}, fmt.Errorf("%w; clear deleted agent access Secret reservation: %v", statusErr, clearErr)
				}
				return ctrl.Result{}, statusErr
			}
		}
		if err := r.clearAgentAccessSecretReservation(ctx, &ad, authSecretRef.Name); err != nil {
			// Status already contains the replacement random name, but the old
			// Deployment may still be serving the previous startup-copied token.
			// Preserve the new Secret and its reservation for recovery while
			// removing the routing boundary and stopping that stale workload.
			clearErr := fmt.Errorf("clear published agent access Secret reservation: %w", err)
			if cleanupErr := r.stopWorkloadForAccessCredentialTransitionFailure(ctx, &ad); cleanupErr != nil {
				return ctrl.Result{}, fmt.Errorf("%w; stop workload after published agent access credential reservation failure: %v", clearErr, cleanupErr)
			}
			return ctrl.Result{}, clearErr
		}
	}

	render := renderInputs{
		cfg:                     cfg,
		binding:                 binding,
		configMapName:           configMap.Name,
		configChecksum:          configChecksum,
		serviceAccountName:      serviceAccount.Name,
		modelCredentialChecksum: modelCredentialChecksum,
		authSecretRef:           authSecretRef,
		accessTokenHash:         accessTokenChecksum,
		catalogImageRevision:    catalogImageRevision,
		writableRoot:            settings.writableRoot,
		securityOverrides:       securityOverrides,
	}
	if ad.Spec.Lifecycle == airunwayv1alpha1.AgentLifecycleJob {
		return r.reconcileJob(ctx, &ad, configMap, render)
	}
	result, workloadApplied, err := r.reconcileDeployment(ctx, &ad, render)
	if err != nil {
		return result, err
	}
	if !workloadApplied {
		// A safety-critical replacement foreground-deletes the old Deployment and
		// removes its Service before the desired workload is applied. Keep the
		// legacy credential while those old pods may still consume it; the next
		// successful apply will retire it.
		return result, nil
	}
	if err := r.deleteLegacyAgentAccessSecret(ctx, &ad, authSecretRef); err != nil {
		// reconcileDeployment has already persisted the derived Secret reference.
		// Preserve it on a transient cleanup failure so the retry does not rotate
		// the client-facing token again.
		return ctrl.Result{}, fmt.Errorf("delete migrated legacy agent access Secret: %w", err)
	}
	return result, nil
}

// reconcileStaleBinding preserves the ordinary binding hold while continuing
// to enforce revocation of the provider-owned ingress credential. It never
// allocates or rotates a credential while the binding is stale; it only checks
// the credential already published in status and stops the exact-owned routing
// boundary/workload when that credential is gone or no longer trustworthy.
func (r *ContainerProviderReconciler) reconcileStaleBinding(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (ctrl.Result, error) {
	hold := ctrl.Result{RequeueAfter: 15 * time.Second}
	workloads, err := r.staleBindingOwnedWorkloads(ctx, ad)
	if err != nil {
		return r.stopWorkloadForInvalidStaleBindingCredential(ctx, ad, err)
	}
	if err := r.staleBindingModelCredentialError(ctx, ad, workloads); err != nil {
		return r.stopWorkloadForInvalidStaleBindingCredential(ctx, ad, err)
	}
	if workloads.job != nil {
		// BindingStale deliberately holds the existing workload, but that must not
		// also defer the one-shot execution ledger. Persist the exact Job we just
		// observed before returning: a terminal Job can otherwise disappear during
		// a long stale-binding hold and leave a pre-ledger generation executable
		// again when the binding recovers.
		if err := r.preflightStaleBindingJobLedger(ctx, ad, workloads.job); err != nil {
			return ctrl.Result{}, err
		}
	}
	if workloads.deployment == nil {
		// A stale binding can overlap a lifecycle transition or an external
		// Deployment deletion. Do not leave the predictable selector Service
		// behind without its exact-owned Deployment: it can otherwise route to
		// lingering or matching foreign pods for the duration of the stale hold.
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: agentServiceName(ad), Namespace: ad.Namespace,
		}}
		if err := r.deleteExactOwnedService(ctx, ad, service); err != nil {
			return ctrl.Result{}, fmt.Errorf("remove agent Service while holding a stale binding without a Deployment: %w", err)
		}
		return hold, nil
	}

	ref := currentAccessCredentialRef(ad)
	if ref == nil {
		return r.stopWorkloadForInvalidStaleBindingCredential(ctx, ad, fmt.Errorf(
			"exact-owned agent Deployment has no published agent access Secret reference"))
	}
	if ref.Key != agentAccessTokenKey || ref.Name == "" {
		return r.stopWorkloadForInvalidStaleBindingCredential(ctx, ad, fmt.Errorf(
			"published agent access Secret reference %q/%q is invalid", ref.Name, ref.Key))
	}
	if ref.Name == legacyAgentAccessSecretName(ad) {
		// The deterministic pre-migration name could be preseeded with
		// caller-controlled token material. BindingReady rotates it through the
		// reservation-backed random-name path, but BindingStale must not mint or
		// publish replacement credentials from an unverified binding. Remove the
		// routing boundary and stop the old workload until core verifies the
		// binding again.
		return r.stopWorkloadForInvalidStaleBindingCredential(ctx, ad, fmt.Errorf(
			"legacy deterministic agent access Secret %s/%s requires rotation after the model binding is verified",
			ad.Namespace, ref.Name))
	}

	key := k8stypes.NamespacedName{Name: ref.Name, Namespace: ad.Namespace}
	var secret corev1.Secret
	if err := r.objectReader().Get(ctx, key, &secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("read agent access Secret %s while model binding is stale: %w", key, err)
		}
		return r.stopWorkloadForInvalidStaleBindingCredential(ctx, ad, fmt.Errorf(
			"agent access Secret %s no longer exists", key))
	}

	token, err := validatedAgentAccessToken(&secret, ad)
	if err != nil {
		return r.stopWorkloadForInvalidStaleBindingCredential(ctx, ad, err)
	}
	_, checksum := agentAccessCredentialResult(ref.Name, token)
	name, keyName, liveChecksum, found := deploymentAccessCredential(workloads.deployment)
	if !found || name != ref.Name || keyName != ref.Key || liveChecksum != checksum {
		return r.stopWorkloadForInvalidStaleBindingCredential(ctx, ad, fmt.Errorf(
			"exact-owned agent Deployment does not use the currently published ingress credential revision"))
	}
	return hold, nil
}

type staleBindingWorkloads struct {
	deployment *appsv1.Deployment
	job        *batchv1.Job
}

// staleBindingOwnedWorkloads authoritatively discovers both workload kinds.
// During a lifecycle transition spec.lifecycle names the desired successor, not
// necessarily the workload that is still running under the stale binding.
func (r *ContainerProviderReconciler) staleBindingOwnedWorkloads(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (staleBindingWorkloads, error) {
	key := k8stypes.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}
	var workloads staleBindingWorkloads

	var deployment appsv1.Deployment
	if err := r.objectReader().Get(ctx, key, &deployment); err != nil {
		if !apierrors.IsNotFound(err) {
			return workloads, fmt.Errorf("read agent Deployment %s while model binding is stale: %w", key, err)
		}
	} else if hasExactBlockingControllerOwner(&deployment, ad) {
		workloads.deployment = &deployment
	}

	var job batchv1.Job
	if err := r.objectReader().Get(ctx, key, &job); err != nil {
		if !apierrors.IsNotFound(err) {
			return workloads, fmt.Errorf("read agent Job %s while model binding is stale: %w", key, err)
		}
	} else if hasExactBlockingControllerOwner(&job, ad) {
		workloads.job = &job
	}
	return workloads, nil
}

// staleBindingModelCredentialError authoritatively revalidates the published
// model Secret and compares its exact reference and UID/resourceVersion digest
// with every exact-owned workload template. BindingStale deliberately does not
// render replacements, but it must still stop either lifecycle's workload when
// it is using a revoked or superseded credential revision.
func (r *ContainerProviderReconciler) staleBindingModelCredentialError(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	workloads staleBindingWorkloads,
) error {
	if ad.Status.ModelBinding == nil {
		return nil
	}

	binding := *ad.Status.ModelBinding
	checksum, err := r.modelCredentialChecksum(ctx, ad.Namespace, binding)
	if err != nil {
		return err
	}
	if workloads.deployment != nil && !podTemplateModelCredentialMatchesBinding(
		&workloads.deployment.Spec.Template, binding.CredentialsRef, checksum,
	) {
		return fmt.Errorf("exact-owned agent Deployment does not use the currently published model credential revision")
	}
	if workloads.job != nil && !podTemplateModelCredentialMatchesBinding(
		&workloads.job.Spec.Template, binding.CredentialsRef, checksum,
	) {
		return fmt.Errorf("exact-owned agent Job does not use the currently published model credential revision")
	}
	return nil
}

func (r *ContainerProviderReconciler) stopWorkloadForInvalidStaleBindingCredential(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	cause error,
) (ctrl.Result, error) {
	log.FromContext(ctx).Info(
		"Stopping agent workload after credential revocation while model binding is stale",
		"agentDeployment", client.ObjectKeyFromObject(ad),
		"reason", cause.Error(),
	)
	releasedClaim, ledgerErr := r.preflightJobLedgerBeforeBindingCleanup(ctx, ad)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: agentServiceName(ad), Namespace: ad.Namespace}}
	var cleanupErr error
	if err := r.deleteExactOwnedService(ctx, ad, service); err != nil {
		cleanupErr = fmt.Errorf("remove agent Service after credential revocation: %w", err)
	}

	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace}}
	pending, err := agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, deployment)
	if err != nil {
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("%v; stop agent Deployment after credential revocation: %w", cleanupErr, err)
		} else {
			cleanupErr = fmt.Errorf("stop agent Deployment after credential revocation: %w", err)
		}
	}

	jobPending := false
	if ledgerErr == nil {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace}}
		jobPending, err = agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, job)
		if err != nil {
			if cleanupErr != nil {
				cleanupErr = fmt.Errorf("%v; stop agent Job after credential revocation: %w", cleanupErr, err)
			} else {
				cleanupErr = fmt.Errorf("stop agent Job after credential revocation: %w", err)
			}
		}
	} else if cleanupErr != nil {
		cleanupErr = fmt.Errorf("persist agent Job execution evidence before credential revocation cleanup: %v; %w", ledgerErr, cleanupErr)
	} else {
		cleanupErr = fmt.Errorf("persist agent Job execution evidence before credential revocation cleanup: %w", ledgerErr)
	}
	if cleanupErr != nil {
		return ctrl.Result{}, cleanupErr
	}
	if pending || jobPending {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if releasedClaim {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// renderInputs bundles everything the workload renderers need beyond the
// AgentDeployment itself, so the Deployment and Job paths stay in step.
type renderInputs struct {
	cfg                     containerConfig
	binding                 airunwayv1alpha1.ModelBindingStatus
	configMapName           string
	configChecksum          string
	serviceAccountName      string
	modelCredentialChecksum string
	authSecretRef           *airunwayv1alpha1.SecretKeyRef
	accessTokenHash         string
	catalogImageRevision    string
	writableRoot            bool
	securityOverrides       *containerSecurityOverrides
}

// applyOwned atomically creates or server-side applies an owned object under
// the container field owner. The authoritative ownership read and
// resourceVersion precondition prevent stale-cache adoption of a replacement.
func (r *ContainerProviderReconciler) applyOwned(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment, obj client.Object) error {
	return agentprovider.ApplyOwned(ctx, r.Client, r.objectReader(), r.Scheme, ad, obj, ContainerFieldOwner, true)
}

// reconcileDeployment renders + applies the long-running Deployment and Service
// and reports readiness from the Deployment's available replicas.
func (r *ContainerProviderReconciler) reconcileDeployment(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	in renderInputs,
) (ctrl.Result, bool, error) {
	deployment := renderAgentDeployment(ad, in)
	service := renderAgentService(ad)
	ownershipSafe, deploymentAbsent, err := r.deploymentOwnedOrAbsent(ctx, ad, deployment)
	if err != nil {
		result, statusErr := r.failWithAccessCredentialStatus(ctx, ad, in.authSecretRef, "RenderFailed", err)
		return result, false, statusErr
	}
	if !ownershipSafe {
		result, statusErr := r.failClosedForDeploymentRouting(ctx, ad, service, in.authSecretRef, fmt.Errorf(
			"refusing to route traffic to Deployment %s/%s: it is not bound to the exact AgentDeployment",
			deployment.Namespace, deployment.Name))
		return result, false, statusErr
	}
	recreating, err := r.recreateDeploymentForSafetyCriticalChange(ctx, ad, deployment)
	if err != nil {
		result, statusErr := r.failWithAccessCredentialStatus(ctx, ad, in.authSecretRef, "RenderFailed", err)
		return result, false, statusErr
	}
	if recreating {
		// Stop the old workload before creating pods for an immutable-selector or
		// credential change. Remove its routing boundary too: foreground
		// Deployment deletion deliberately keeps the old pods alive until cleanup
		// completes, and leaving the selector Service active would keep exposing
		// those stale credential-bearing pods during the replacement window.
		if err := r.deleteExactOwnedService(ctx, ad, service); err != nil {
			result, statusErr := r.failWithAccessCredentialStatus(ctx, ad, in.authSecretRef, "RenderFailed",
				fmt.Errorf("remove agent Service while replacing Deployment: %w", err))
			return result, false, statusErr
		}
		result := ctrl.Result{RequeueAfter: 5 * time.Second}
		return result, false, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseDeploying, accessCredentialRuntime(in.authSecretRef), nil,
			metav1.ConditionFalse, "WorkloadReplacing", "Replacing the agent workload before applying a selector or credential change")
	}
	if deploymentAbsent {
		// Claim the Service name without making it routable before atomically
		// establishing Deployment ownership. A concurrent foreign Deployment can
		// still win the create, but it must never receive traffic through the
		// predictable agent selector during that ownership race.
		unroutedService := service.DeepCopy()
		unroutedService.Spec.Selector = nil
		if err := r.applyOwned(ctx, ad, unroutedService); err != nil {
			result, statusErr := r.failClosedForServiceApply(ctx, ad, service, in.authSecretRef, err)
			return result, false, statusErr
		}
		if err := r.applyOwned(ctx, ad, deployment); err != nil {
			result, statusErr := r.failClosedForDeploymentCredentialApply(ctx, ad, deployment, service, in.authSecretRef, err)
			return result, false, statusErr
		}
		if err := r.applyOwned(ctx, ad, service); err != nil {
			result, statusErr := r.failClosedForServiceApply(ctx, ad, service, in.authSecretRef, err)
			return result, false, statusErr
		}
	} else {
		// Reconcile the routing boundary before updating an existing credential-
		// bearing workload. If Service ownership/admission fails, the Deployment
		// must be stopped so it cannot keep serving behind an unmanaged or stale
		// endpoint.
		if err := r.applyOwned(ctx, ad, service); err != nil {
			result, statusErr := r.failClosedForServiceApply(ctx, ad, service, in.authSecretRef, err)
			return result, false, statusErr
		}
		if err := r.applyOwned(ctx, ad, deployment); err != nil {
			result, statusErr := r.failClosedForDeploymentCredentialApply(ctx, ad, deployment, service, in.authSecretRef, err)
			return result, false, statusErr
		}
	}

	// ApplyOwned decodes the authoritative API response back into deployment.
	// A cache read here can return NotFound or the pre-update generation.
	live := deployment
	desired := ptr.Deref(live.Spec.Replicas, 1)
	replicas := &airunwayv1alpha1.AgentReplicaStatus{
		Desired:   desired,
		Ready:     live.Status.ReadyReplicas,
		Available: live.Status.AvailableReplicas,
	}
	rt := &airunwayv1alpha1.AgentRuntimeStatus{
		WorkloadRef: &airunwayv1alpha1.RuntimeWorkloadRef{
			APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, Namespace: deployment.Namespace,
		},
		Address:       fmt.Sprintf("http://%s.%s.svc", service.Name, service.Namespace),
		AuthSecretRef: in.authSecretRef,
	}

	if deploymentRolledOut(live, desired) {
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return result, true, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseRunning, rt, replicas,
			metav1.ConditionTrue, "WorkloadReady", "Agent workload has available replicas")
	}
	reason, message := workloadNotReadyDetail(live)
	result := ctrl.Result{RequeueAfter: 15 * time.Second}
	return result, true, r.status(ctx, ad,
		airunwayv1alpha1.AgentPhaseDeploying, rt, replicas,
		metav1.ConditionFalse, reason, message)
}

// deploymentOwnedOrAbsent authoritatively rejects a same-name foreign
// Deployment before the Service is applied. The Service selector is readable
// metadata, so leaving an owned Service beside a foreign Deployment would let
// that Deployment route pods through the agent endpoint merely by copying the
// selector labels.
func (r *ContainerProviderReconciler) deploymentOwnedOrAbsent(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	desired *appsv1.Deployment,
) (ownershipSafe bool, absent bool, err error) {
	var live appsv1.Deployment
	key := client.ObjectKeyFromObject(desired)
	if err := r.objectReader().Get(ctx, key, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return true, true, nil
		}
		return false, false, fmt.Errorf("read existing agent Deployment %s before Service apply: %w", key, err)
	}
	return hasExactBlockingControllerOwner(&live, ad), false, nil
}

// recreateDeploymentForSafetyCriticalChange handles both the immutable selector
// migration and credential changes that must not use a rolling update. Existing
// Deployments rendered before the UID label was introduced cannot be patched,
// while old credential-bearing pods must be gone before replacements start: a
// stalled rollout would otherwise retain the retired ingress/model credential.
func (r *ContainerProviderReconciler) recreateDeploymentForSafetyCriticalChange(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	desired *appsv1.Deployment,
) (bool, error) {
	var live appsv1.Deployment
	key := client.ObjectKeyFromObject(desired)
	if err := r.objectReader().Get(ctx, key, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read existing agent Deployment %s: %w", key, err)
	}
	if !hasExactBlockingControllerOwner(&live, ad) {
		return false, nil
	}
	if !live.DeletionTimestamp.IsZero() {
		return true, nil
	}
	selectorMatches := apiequality.Semantic.DeepEqual(live.Spec.Selector, desired.Spec.Selector)
	if selectorMatches && deploymentSafetyCriticalTemplateMatches(&live, desired) {
		return false, nil
	}
	return agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, &live)
}

func (r *ContainerProviderReconciler) failClosedForServiceApply(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	service *corev1.Service,
	ref *airunwayv1alpha1.SecretKeyRef,
	cause error,
) (ctrl.Result, error) {
	if cleanupErr := r.deleteExactOwnedService(ctx, ad, service); cleanupErr != nil {
		cause = fmt.Errorf("%w; remove agent Service after Service apply failure: %v", cause, cleanupErr)
	}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace}}
	if _, cleanupErr := agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, deployment); cleanupErr != nil {
		cause = fmt.Errorf("%w; stop agent Deployment after Service apply failure: %v", cause, cleanupErr)
	}
	return r.failWithAccessCredentialStatus(ctx, ad, ref, "RenderFailed", cause)
}

// stopWorkloadForAccessCredentialTransitionFailure closes the existing routing
// boundary and starts foreground deletion of the exact-owned Deployment when a
// credential rotation cannot cross its publication/reservation boundary. A
// running pod otherwise keeps accepting the previously startup-copied bearer
// token while status or the recovery journal describe its replacement.
func (r *ContainerProviderReconciler) stopWorkloadForAccessCredentialTransitionFailure(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) error {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: agentServiceName(ad), Namespace: ad.Namespace}}
	var cleanupErr error
	if err := r.deleteExactOwnedService(ctx, ad, service); err != nil {
		cleanupErr = fmt.Errorf("remove agent Service: %w", err)
	}

	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace}}
	if _, err := agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, deployment); err != nil {
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("%v; stop agent Deployment: %w", cleanupErr, err)
		} else {
			cleanupErr = fmt.Errorf("stop agent Deployment: %w", err)
		}
	}
	return cleanupErr
}

// failClosedForDeploymentCredentialApply stops an existing Deployment only
// when its pod template still consumes different ingress or model credentials
// than the desired template. Ordinary apply retries against already-current
// credentials do not cause an outage, while a failed credential rollout cannot
// leave old pods serving with retired bearer material.
func (r *ContainerProviderReconciler) failClosedForDeploymentCredentialApply(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	desired *appsv1.Deployment,
	service *corev1.Service,
	ref *airunwayv1alpha1.SecretKeyRef,
	cause error,
) (ctrl.Result, error) {
	var live appsv1.Deployment
	key := client.ObjectKeyFromObject(desired)
	if err := r.objectReader().Get(ctx, key, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failClosedForDeploymentRouting(ctx, ad, service, ref, cause)
		}
		cause = fmt.Errorf("%w; verify existing agent Deployment credential after failed apply: %v", cause, err)
		return r.failWithAccessCredentialStatus(ctx, ad, ref, "RenderFailed", cause)
	}
	if !hasExactBlockingControllerOwner(&live, ad) {
		return r.failClosedForDeploymentRouting(ctx, ad, service, ref, cause)
	}
	if deploymentSafetyCriticalTemplateMatches(&live, desired) {
		return r.failWithAccessCredentialStatus(ctx, ad, ref, "RenderFailed", cause)
	}
	if cleanupErr := r.deleteExactOwnedService(ctx, ad, service); cleanupErr != nil {
		cause = fmt.Errorf("%w; remove agent Service before stopping stale credential-bearing or security-stale Deployment: %v", cause, cleanupErr)
	}
	if _, err := agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, &live); err != nil {
		cause = fmt.Errorf("%w; stop stale credential-bearing or security-stale agent Deployment: %v", cause, err)
	}
	return r.failWithAccessCredentialStatus(ctx, ad, ref, "RenderFailed", cause)
}

// failClosedForDeploymentRouting removes only an exact-owned Service when a
// failed Deployment apply leaves no exact-owned workload behind. This covers
// both replacement races and an authoritative NotFound after initial create,
// without touching foreign objects. Exact-owned Deployment apply failures take
// the credential-aware path above and preserve the Service.
func (r *ContainerProviderReconciler) failClosedForDeploymentRouting(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	service *corev1.Service,
	ref *airunwayv1alpha1.SecretKeyRef,
	cause error,
) (ctrl.Result, error) {
	if cleanupErr := r.deleteExactOwnedService(ctx, ad, service); cleanupErr != nil {
		cause = fmt.Errorf("%w; remove agent Service without an exact-owned Deployment: %v", cause, cleanupErr)
	}
	return r.failWithAccessCredentialStatus(ctx, ad, ref, "RenderFailed", cause)
}

func (r *ContainerProviderReconciler) deleteExactOwnedService(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	service *corev1.Service,
) error {
	key := client.ObjectKeyFromObject(service)
	var live corev1.Service
	if err := r.objectReader().Get(ctx, key, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read agent Service %s for cleanup: %w", key, err)
	}
	if !hasExactBlockingControllerOwner(&live, ad) {
		return nil
	}
	uid := live.UID
	policy := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, &live, &client.DeleteOptions{
		PropagationPolicy: &policy,
		Preconditions:     &metav1.Preconditions{UID: &uid},
	}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete exact-owned agent Service %s: %w", key, err)
	}
	return nil
}

func deploymentCredentialsMatch(live, desired *appsv1.Deployment) bool {
	return deploymentAccessCredentialMatches(live, desired) &&
		deploymentModelCredentialMatches(live, desired)
}

func deploymentSafetyCriticalTemplateMatches(live, desired *appsv1.Deployment) bool {
	return deploymentCredentialsMatch(live, desired) &&
		podTemplateSecurityCriticalFieldsMatch(&live.Spec.Template, &desired.Spec.Template)
}

func deploymentAccessCredentialMatches(live, desired *appsv1.Deployment) bool {
	liveName, liveKey, liveChecksum, liveFound := deploymentAccessCredential(live)
	desiredName, desiredKey, desiredChecksum, desiredFound := deploymentAccessCredential(desired)
	return liveFound && desiredFound &&
		liveName == desiredName && liveKey == desiredKey && liveChecksum == desiredChecksum
}

func deploymentAccessCredential(deployment *appsv1.Deployment) (name, key, checksum string, found bool) {
	checksum = deployment.Spec.Template.Annotations[agentAccessChecksumAnnotation]
	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		if container.Name != agentContainerName {
			continue
		}
		for j := range container.Env {
			env := &container.Env[j]
			if env.Name != agentAccessTokenEnv || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				continue
			}
			return env.ValueFrom.SecretKeyRef.Name, env.ValueFrom.SecretKeyRef.Key, checksum, true
		}
	}
	return "", "", checksum, false
}

func deploymentModelCredentialMatches(live, desired *appsv1.Deployment) bool {
	liveName, liveKey, liveChecksum, liveFound := deploymentModelCredential(live)
	desiredName, desiredKey, desiredChecksum, desiredFound := deploymentModelCredential(desired)
	if liveFound != desiredFound || liveChecksum != desiredChecksum {
		return false
	}
	return !liveFound || liveName == desiredName && liveKey == desiredKey
}

func deploymentModelCredential(deployment *appsv1.Deployment) (name, key, checksum string, found bool) {
	return podTemplateModelCredential(&deployment.Spec.Template)
}

func podTemplateModelCredentialMatchesBinding(
	template *corev1.PodTemplateSpec,
	ref *airunwayv1alpha1.SecretKeyRef,
	checksum string,
) bool {
	name, key, liveChecksum, found := podTemplateModelCredential(template)
	if ref == nil {
		return !found && liveChecksum == ""
	}
	return found && name == ref.Name && key == ref.Key && liveChecksum == checksum
}

func podTemplateModelCredential(template *corev1.PodTemplateSpec) (name, key, checksum string, found bool) {
	checksum = template.Annotations[agentModelCredentialChecksumAnnotation]
	for i := range template.Spec.Containers {
		container := &template.Spec.Containers[i]
		if container.Name != agentContainerName {
			continue
		}
		for j := range container.Env {
			env := &container.Env[j]
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				continue
			}
			switch env.Name {
			case "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AZURE_OPENAI_API_KEY":
				return env.ValueFrom.SecretKeyRef.Name, env.ValueFrom.SecretKeyRef.Key, checksum, true
			}
		}
	}
	return "", "", checksum, false
}

// workloadNotReadyDetail explains why a Deployment has not rolled out yet.
//
// "Waiting for the agent workload to become available" is true but useless when
// the pods are being actively REJECTED rather than merely starting slowly. The
// clearest case is Pod Security Admission: in a namespace enforcing a stricter
// profile than the rendered pod satisfies, the Deployment sits at 0 replicas and
// the real message lives on a ReplicaSet event the user has no reason to look
// for. The Deployment controller surfaces that as a ReplicaFailure condition, so
// pass it through verbatim.
func workloadNotReadyDetail(live *appsv1.Deployment) (reason, message string) {
	for i := range live.Status.Conditions {
		c := live.Status.Conditions[i]
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return "WorkloadRejected",
				fmt.Sprintf("The agent workload could not create pods (%s): %s", c.Reason, c.Message)
		}
	}
	return "WorkloadNotReady", "Waiting for the agent workload to become available"
}

// deploymentRolledOut reports whether the Deployment's *current* pod template
// is actually serving. Checking availableReplicas alone is not enough, in two
// separate ways:
//
//   - Right after the provider changes the pod template, the Deployment status
//     still describes the PREVIOUS generation, so a stale availableReplicas
//     would promote the new generation to Running before anything rolled.
//   - Mid-rollout the status describes BOTH ReplicaSets. The default strategy
//     at replicas=1 is maxSurge=1/maxUnavailable=0, giving replicas=2,
//     updatedReplicas=1, availableReplicas=1 — where the one available pod is
//     still the OLD one. Because maxUnavailable is 0 the old pod is never
//     scaled down until the new one goes available, so a broken new image
//     (ImagePullBackOff, CrashLoopBackOff) would report Running permanently.
//
// Requiring the controller to have observed this generation, every desired
// replica to be updated, and no old replicas to remain closes both windows.
func deploymentRolledOut(live *appsv1.Deployment, desired int32) bool {
	if desired <= 0 {
		return false
	}
	if live.Status.ObservedGeneration < live.Generation {
		return false
	}
	if live.Status.UpdatedReplicas < desired {
		return false
	}
	if live.Status.Replicas != live.Status.UpdatedReplicas {
		return false
	}
	return live.Status.AvailableReplicas >= desired
}

func (r *ContainerProviderReconciler) deletePreviousGenerationJob(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (bool, error) {
	var live batchv1.Job
	key := k8stypes.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}
	if err := r.objectReader().Get(ctx, key, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read existing agent job %s: %w", key, err)
	}
	if !hasExactBlockingControllerOwner(&live, ad) {
		return false, nil
	}
	jobGeneration := live.Annotations[agentJobGenerationAnnotation]
	if jobGeneration == "" {
		// Stop before applying the new generation's shared ConfigMap. An
		// unannotated pre-ledger Job may still be the previous generation, and
		// updating its mounted configuration before resolving that ambiguity
		// would mutate a live one-shot execution in place. Generation one is the
		// only exception: no previous AgentDeployment generation can have created
		// an exact-owned Job, so it is safe to migrate even if the controller
		// crashed before its first provider-status write.
		if !canAssignLegacyJobToCurrentGeneration(ad) {
			return false, ambiguousLegacyJobGenerationError(&live, ad)
		}
		return false, nil
	}
	if jobGeneration == strconv.FormatInt(ad.Generation, 10) {
		return false, nil
	}
	return agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, &live)
}

// reconcileJob renders + applies the one-shot Job and maps its status onto the
// agent phase. A Job is reported Failed only once it surfaces a true JobFailed
// condition (i.e. backoffLimit exhausted), Running while active, and Completed
// once it succeeds.
//
//nolint:gocyclo // The at-most-once Job ledger requires all recovery states to remain explicit.
func (r *ContainerProviderReconciler) reconcileJob(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	configMap *corev1.ConfigMap,
	in renderInputs,
) (ctrl.Result, error) {
	job, err := renderAgentJob(ad, in)
	if err != nil {
		return r.failWithStatus(ctx, ad, "RenderFailed", err)
	}

	// Validate a same-named Job before consuming this generation's durable
	// claim. Otherwise an unrelated name conflict (or a mismatched legacy Job)
	// would permanently suppress the execution even after the conflict is gone.
	key := k8stypes.NamespacedName{Name: job.Name, Namespace: job.Namespace}
	var live *batchv1.Job
	var existing batchv1.Job
	if err := r.objectReader().Get(ctx, key, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return r.failWithStatus(ctx, ad, "JobReadFailed", err)
		}
	} else {
		if err := validateAgentJobOwner(&existing, ad); err != nil {
			return r.failWithStatus(ctx, ad, "RenderFailed", fmt.Errorf(
				"refusing to adopt Job %s: %w", key, err))
		}

		generation := existing.Annotations[agentJobGenerationAnnotation]
		desiredGeneration := strconv.FormatInt(ad.Generation, 10)
		switch {
		case generation == "":
			// A Job can appear after ledger preflight. Apply the same migration
			// proof here before binding an unannotated pre-ledger execution to this
			// generation; otherwise a spec update could consume the previous
			// generation's Job.
			if !canAssignLegacyJobToCurrentGeneration(ad) {
				return r.failWithStatus(ctx, ad, "JobMigrationAmbiguous", ambiguousLegacyJobGenerationError(&existing, ad))
			}
			if err := r.annotateLegacyJobGeneration(ctx, &existing, ad.Generation); err != nil {
				return r.failWithStatus(ctx, ad, "JobClaimFailed", err)
			}
		case generation != desiredGeneration:
			deleting, deleteErr := agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, &existing)
			if deleteErr != nil {
				return r.failWithStatus(ctx, ad, "JobGenerationCleanupFailed", deleteErr)
			}
			result := ctrl.Result{RequeueAfter: 5 * time.Second}
			if !deleting {
				result.Requeue = true //nolint:staticcheck // Immediate retry after an authoritative absence check.
			}
			return result, r.status(ctx, ad,
				airunwayv1alpha1.AgentPhaseDeploying, nil, nil, metav1.ConditionFalse,
				"JobGenerationSwitching", "Removing the previous generation's agent job")
		}
		live = &existing
	}

	claimedNow, outcome, err := r.ensureJobGenerationClaim(ctx, ad, configMap)
	if err != nil {
		return r.failWithStatus(ctx, ad, "JobClaimFailed", err)
	}
	if outcome == agentJobOutcomeRetryable {
		if err := r.releaseJobGenerationClaim(ctx, ad, configMap); err != nil {
			return r.failWithStatus(ctx, ad, "JobClaimReleaseFailed", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if outcome != "" {
		return r.reportRecordedJobOutcome(ctx, ad, outcome, live)
	}
	if live != nil && terminalJobOutcome(live) == "" && !jobSecurityCriticalTemplateMatches(live, job, in) {
		// The generation claim above is durable before this fail-closed delete.
		// Never replace the Job: once it disappears, the blank claim converges to
		// JobLost and preserves the one-execution-per-generation contract.
		pending, deleteErr := agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, live)
		if deleteErr != nil {
			return r.failWithStatus(ctx, ad, "JobSecurityDriftCleanupFailed", fmt.Errorf(
				"stop current-generation agent Job with stale credentials or security posture: %w", deleteErr))
		}
		result := ctrl.Result{RequeueAfter: 5 * time.Second}
		if !pending {
			result.Requeue = true //nolint:staticcheck // Immediate retry after an authoritative absence check.
		}
		rt, replicas := liveJobStatus(live)
		return result, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseDeploying, rt, replicas,
			metav1.ConditionFalse, "JobSecurityDrift",
			"Stopping the one-shot workload because its credentials or security posture are stale")
	}

	if live == nil {
		if !claimedNow {
			if err := r.recordJobOutcome(ctx, ad, configMap, agentJobOutcomeLost); err != nil {
				return r.failWithStatus(ctx, ad, "JobOutcomeRecordFailed", err)
			}
			return r.reportRecordedJobOutcome(ctx, ad, agentJobOutcomeLost, nil)
		}
		if err := r.applyOwned(ctx, ad, job); err != nil {
			// An apply response can be ambiguous (for example a timeout after the
			// API server persisted the Job). Recover an observed owned Job instead
			// of launching twice. Release the claim only when an authoritative read
			// or a definitive admission error proves no provider Job was created.
			createErr := err
			var observed batchv1.Job
			readErr := r.objectReader().Get(ctx, key, &observed)
			switch {
			case readErr == nil && hasExactBlockingControllerOwner(&observed, ad) &&
				observed.Annotations[agentJobGenerationAnnotation] == strconv.FormatInt(ad.Generation, 10):
				live = &observed
			case readErr == nil:
				// The authoritative read proves this apply did not create the
				// exact current-generation Job. Mark the blank claim retryable
				// before releasing it, so a release conflict cannot later turn
				// the absent desired Job into a permanent JobLost outcome.
				if markerErr := r.markJobClaimRetryable(ctx, ad, configMap); markerErr != nil {
					createErr = fmt.Errorf("%w; mark unused job claim retryable: %v", createErr, markerErr)
					return r.failWithStatus(ctx, ad, "RenderFailed", createErr)
				}
				if releaseErr := r.releaseJobGenerationClaim(ctx, ad, configMap); releaseErr != nil {
					createErr = fmt.Errorf("%w; release unused job claim: %v", createErr, releaseErr)
				}
				return r.failWithStatus(ctx, ad, "RenderFailed", createErr)
			case apierrors.IsNotFound(readErr) && definitiveResourceWriteFailure(createErr):
				// Persist why this otherwise-ambiguous claim is safe to retry
				// before clearing it. If the release patch fails, the marker makes
				// the next reconcile retry the release instead of declaring the
				// absent Job permanently lost.
				if markerErr := r.markJobClaimRetryable(ctx, ad, configMap); markerErr != nil {
					createErr = fmt.Errorf("%w; mark rejected job claim retryable: %v", createErr, markerErr)
					return r.failWithStatus(ctx, ad, "RenderFailed", createErr)
				}
				if releaseErr := r.releaseJobGenerationClaim(ctx, ad, configMap); releaseErr != nil {
					createErr = fmt.Errorf("%w; release rejected job claim: %v", createErr, releaseErr)
				}
				return r.failWithStatus(ctx, ad, "RenderFailed", createErr)
			default:
				if readErr != nil && !apierrors.IsNotFound(readErr) {
					createErr = fmt.Errorf("%w; verify agent Job after failed create: %v", createErr, readErr)
				}
				return r.failWithStatus(ctx, ad, "RenderFailed", createErr)
			}
		} else {
			live = job
		}
	}

	rt, replicas := liveJobStatus(live)

	switch {
	case jobConditionTrue(live, batchv1.JobFailed):
		if err := r.recordJobOutcome(ctx, ad, configMap, agentJobOutcomeFailed); err != nil {
			return r.failWithStatus(ctx, ad, "JobOutcomeRecordFailed", err)
		}
		return ctrl.Result{}, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseFailed, rt, replicas,
			metav1.ConditionFalse, agentJobFailedReason, "Agent job failed (backoff limit exhausted)")
	case jobConditionTrue(live, batchv1.JobComplete) || live.Status.Succeeded > 0:
		if err := r.recordJobOutcome(ctx, ad, configMap, agentJobOutcomeCompleted); err != nil {
			return r.failWithStatus(ctx, ad, "JobOutcomeRecordFailed", err)
		}
		return ctrl.Result{}, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseCompleted, rt, replicas,
			metav1.ConditionTrue, agentJobCompletedReason, "Agent job completed successfully")
	case live.Status.Active > 0:
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseRunning, rt, replicas,
			metav1.ConditionTrue, "JobRunning", "Agent job is active")
	default:
		return ctrl.Result{RequeueAfter: 15 * time.Second}, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseDeploying, rt, replicas,
			metav1.ConditionFalse, "JobPending", "Waiting for the agent job to start")
	}
}

// jobSecurityCriticalTemplateMatches deliberately ignores ordinary immutable
// Job template drift (image, task config, observability, resources): replacing
// an accepted one-shot execution for those changes would violate at-most-once.
// Credential revisions and the provider's security floor are different: an
// already-running Job must be stopped, then recorded lost, rather than allowed
// to continue with revoked credentials or inherited Kubernetes identity.
func jobSecurityCriticalTemplateMatches(live, desired *batchv1.Job, in renderInputs) bool {
	liveTemplate := &live.Spec.Template
	desiredTemplate := &desired.Spec.Template
	if !podTemplateModelCredentialMatchesBinding(liveTemplate, in.binding.CredentialsRef, in.modelCredentialChecksum) {
		return false
	}
	return podTemplateSecurityCriticalFieldsMatch(liveTemplate, desiredTemplate)
}

func podTemplateSecurityCriticalFieldsMatch(liveTemplate, desiredTemplate *corev1.PodTemplateSpec) bool {
	if liveTemplate.Spec.ServiceAccountName != desiredTemplate.Spec.ServiceAccountName ||
		!apiequality.Semantic.DeepEqual(liveTemplate.Spec.AutomountServiceAccountToken, desiredTemplate.Spec.AutomountServiceAccountToken) ||
		!apiequality.Semantic.DeepEqual(liveTemplate.Spec.SecurityContext, desiredTemplate.Spec.SecurityContext) {
		return false
	}
	liveSecurity, liveFound := agentContainerSecurityContext(liveTemplate)
	desiredSecurity, desiredFound := agentContainerSecurityContext(desiredTemplate)
	return liveFound && desiredFound && apiequality.Semantic.DeepEqual(liveSecurity, desiredSecurity)
}

func agentContainerSecurityContext(template *corev1.PodTemplateSpec) (*corev1.SecurityContext, bool) {
	for i := range template.Spec.Containers {
		if template.Spec.Containers[i].Name == agentContainerName {
			return template.Spec.Containers[i].SecurityContext, true
		}
	}
	return nil, false
}

// definitiveResourceWriteFailure identifies API-server status responses that
// prove a resource write was rejected. Transport failures and conflicts remain
// ambiguous and are retried without tearing down otherwise healthy resources.
func definitiveResourceWriteFailure(err error) bool {
	return apierrors.IsAlreadyExists(err) ||
		apierrors.IsInvalid(err) ||
		apierrors.IsBadRequest(err) ||
		apierrors.IsUnauthorized(err) ||
		apierrors.IsForbidden(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsMethodNotSupported(err) ||
		apierrors.IsNotAcceptable(err) ||
		apierrors.IsUnsupportedMediaType(err) ||
		apierrors.IsRequestEntityTooLargeError(err)
}

// preflightJobLedger consults durable execution state before any mutable model
// credential metadata. Terminal Jobs no longer need those credentials, and a
// later Secret deletion or read failure must not overwrite their recorded
// outcome. It also migrates the exact terminal status shape written by the
// pre-ledger controller, so a completed or failed Job that has already been
// garbage-collected is not launched again after an upgrade.
func (r *ContainerProviderReconciler) preflightJobLedger(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	configMap *corev1.ConfigMap,
) (string, bool, error) {
	live, err := r.readExactOwnedJobLedger(ctx, ad, configMap)
	if err != nil {
		return "", false, err
	}

	generation := strconv.FormatInt(ad.Generation, 10)
	job, err := r.currentGenerationJobForLedgerPreflight(ctx, ad, generation)
	if err != nil {
		return "", false, err
	}
	if live.Annotations[agentJobGenerationAnnotation] == generation {
		switch outcome := live.Annotations[agentJobOutcomeAnnotation]; outcome {
		case agentJobOutcomeCompleted, agentJobOutcomeFailed:
			return outcome, false, nil
		case agentJobOutcomeLost:
			// A timed-out create can be observed as NotFound before the API
			// server finishes committing it. Let a subsequently visible exact
			// Job supersede the provisional lost outcome.
			if job == nil {
				return outcome, false, nil
			}
			if terminal := terminalJobOutcome(job); terminal != "" {
				if err := r.recordJobOutcome(ctx, ad, configMap, terminal); err != nil {
					return "", false, fmt.Errorf("replace lost agent job outcome with %q: %w", terminal, err)
				}
				return terminal, false, nil
			}
			if err := r.clearJobOutcome(ctx, ad, configMap, agentJobOutcomeLost); err != nil {
				return "", false, fmt.Errorf("recover late agent Job from lost outcome: %w", err)
			}
			return "", false, nil
		case agentJobOutcomeRetryable:
			if err := r.releaseJobGenerationClaim(ctx, ad, configMap); err != nil {
				return "", false, fmt.Errorf("release retryable agent job claim: %w", err)
			}
			return "", true, nil
		case "":
			// Capture a terminal Job before a simultaneous credential read
			// failure can tear it down and destroy the only outcome evidence.
			if job != nil {
				if terminal := terminalJobOutcome(job); terminal != "" {
					if err := r.recordJobOutcome(ctx, ad, configMap, terminal); err != nil {
						return "", false, fmt.Errorf("record terminal agent Job before credential read: %w", err)
					}
					return terminal, false, nil
				}
			}
		default:
			return "", false, fmt.Errorf("unknown recorded job outcome %q", outcome)
		}
	} else if job != nil {
		// An accepted live Job is already this generation's one permitted
		// execution. Persist either its terminal outcome or a blank claim before
		// ServiceAccount/credential fail-closed cleanup can remove the Job.
		if terminal := terminalJobOutcome(job); terminal != "" {
			if err := r.persistJobOutcome(ctx, ad, configMap, terminal); err != nil {
				return "", false, fmt.Errorf("migrate terminal live agent Job outcome %q: %w", terminal, err)
			}
			return terminal, false, nil
		}
		_, outcome, err := r.ensureJobGenerationClaim(ctx, ad, configMap)
		if err != nil {
			return "", false, fmt.Errorf("claim live agent Job generation before cleanup: %w", err)
		}
		if outcome == agentJobOutcomeRetryable {
			if err := r.releaseJobGenerationClaim(ctx, ad, configMap); err != nil {
				return "", false, fmt.Errorf("release retryable live agent Job claim: %w", err)
			}
			return "", true, nil
		}
		if outcome != "" {
			return outcome, false, nil
		}
	}

	legacyOutcome, ok := terminalJobOutcomeFromStatus(ad)
	if ok {
		if err := r.persistJobOutcome(ctx, ad, configMap, legacyOutcome); err != nil {
			return "", false, fmt.Errorf("migrate legacy agent job outcome %q: %w", legacyOutcome, err)
		}
		return legacyOutcome, false, nil
	}

	// JobPending and JobRunning are written only after this provider has
	// observed the exact current-generation Job. If that Job is now absent,
	// preserve at-most-once execution by migrating the existing evidence to a
	// lost outcome rather than treating the empty ledger as permission to run it
	// again.
	if job == nil && (statusProvesCurrentJobGeneration(ad) || statusHasAmbiguousLegacyJobExecutionEvidence(ad)) {
		if err := r.persistJobOutcome(ctx, ad, configMap, agentJobOutcomeLost); err != nil {
			return "", false, fmt.Errorf("migrate missing legacy agent Job to lost outcome: %w", err)
		}
		return agentJobOutcomeLost, false, nil
	}
	return "", false, nil
}

// preflightStaleBindingJobLedger persists an attributable exact-owned Job
// without changing the stale-binding hold or deleting a Job from another
// generation. The Job passed here came from an authoritative read. Using that
// observation directly closes the window where the Job disappears between the
// stale-binding check and a second read before its terminal outcome or blank
// generation claim becomes durable.
func (r *ContainerProviderReconciler) preflightStaleBindingJobLedger(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	job *batchv1.Job,
) error {
	desiredGeneration := strconv.FormatInt(ad.Generation, 10)
	jobGeneration := job.Annotations[agentJobGenerationAnnotation]
	switch {
	case jobGeneration != "" && jobGeneration != desiredGeneration:
		// BindingStale is a hold, not a lifecycle-cleanup path. Preserve a
		// definitively older Job and its mounted configuration unchanged.
		return nil
	case jobGeneration == "" && !canAssignLegacyJobToCurrentGeneration(ad):
		// The legacy Job cannot safely be assigned to either generation. Holding it
		// is safer than writing current-generation evidence or deleting it.
		return nil
	}

	configMap := &corev1.ConfigMap{}
	configKey := k8stypes.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
	if err := r.objectReader().Get(ctx, configKey, configMap); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("read agent job ledger %s while model binding is stale: %w", configKey, err)
		}
		configMap = renderAgentConfigMap(ad)
		if err := r.applyOwned(ctx, ad, configMap); err != nil {
			return fmt.Errorf("create agent job ledger while model binding is stale: %w", err)
		}
	} else if !hasExactBlockingControllerOwner(configMap, ad) || !configMap.DeletionTimestamp.IsZero() {
		return fmt.Errorf(
			"refusing to use ConfigMap %s as an agent job ledger while model binding is stale: it is not a live exact-owned object",
			configKey,
		)
	}

	if terminal := terminalJobOutcome(job); terminal != "" {
		if err := r.persistJobOutcome(ctx, ad, configMap, terminal); err != nil {
			return fmt.Errorf("record terminal agent Job while model binding is stale: %w", err)
		}
	} else {
		_, outcome, err := r.ensureJobGenerationClaim(ctx, ad, configMap)
		if err != nil {
			return fmt.Errorf("claim live agent Job while model binding is stale: %w", err)
		}
		// A live exact current-generation Job supersedes provisional markers that
		// were written only because the create result was uncertain or rejected.
		switch outcome {
		case agentJobOutcomeLost, agentJobOutcomeRetryable:
			if err := r.clearJobOutcome(ctx, ad, configMap, outcome); err != nil {
				return fmt.Errorf("recover live agent Job from %q outcome while model binding is stale: %w", outcome, err)
			}
		case "", agentJobOutcomeCompleted, agentJobOutcomeFailed:
			// A blank claim is the desired nonterminal evidence. A recorded terminal
			// outcome remains authoritative even if a late Job is still visible.
		default:
			return fmt.Errorf("unknown recorded job outcome %q while model binding is stale", outcome)
		}
	}

	if jobGeneration == "" {
		if err := r.annotateLegacyJobGeneration(ctx, job, ad.Generation); err != nil {
			// The ledger above is already durable, so a retry cannot authorize another
			// execution even if this annotation patch races with Job deletion.
			return fmt.Errorf("annotate accepted pre-ledger agent Job while model binding is stale: %w", err)
		}
	}
	return nil
}

// preflightJobLedgerBeforeBindingCleanup persists the only durable proof that
// a one-shot generation already ran before binding cleanup deletes its Job and
// replaces Job-specific provider status with WaitingForBindings. It avoids
// creating a ConfigMap for agents that have never started a Job, while safely
// materializing the ledger when an older provider left execution evidence but
// no ConfigMap survived.
func (r *ContainerProviderReconciler) preflightJobLedgerBeforeBindingCleanup(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (bool, error) {
	hasEvidence := statusProvesCurrentJobGeneration(ad) || statusHasAmbiguousLegacyJobExecutionEvidence(ad)
	if _, terminal := terminalJobOutcomeFromStatus(ad); terminal {
		hasEvidence = true
	}

	var job batchv1.Job
	jobKey := k8stypes.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}
	if err := r.objectReader().Get(ctx, jobKey, &job); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("read agent Job %s before binding cleanup: %w", jobKey, err)
		}
	} else if hasExactBlockingControllerOwner(&job, ad) {
		jobGeneration := job.Annotations[agentJobGenerationAnnotation]
		if jobGeneration != "" && jobGeneration != strconv.FormatInt(ad.Generation, 10) {
			// This Job is definitively from an older generation. Stop it before
			// recreating the shared ConfigMap from the current spec: its pod may
			// still have that ConfigMap mounted while foreground deletion waits
			// on dependents or finalizers.
			if _, err := agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, &job); err != nil {
				return false, fmt.Errorf("delete previous-generation agent Job before recreating its ConfigMap: %w", err)
			}
			// Always cross a fresh authoritative reconcile boundary, including
			// when the delete helper raced with the Job becoming NotFound.
			return true, nil
		}
		if jobGeneration == "" && !canAssignLegacyJobToCurrentGeneration(ad) {
			// Do not recreate the mounted ConfigMap until the unannotated Job can
			// be attributed to this generation. An older one-shot execution may
			// still be running, and recreating the ConfigMap with the current spec
			// would mutate that execution before the ambiguity is resolved.
			return false, ambiguousLegacyJobGenerationError(&job, ad)
		}
		hasEvidence = true
	}
	if !hasEvidence {
		return false, nil
	}

	configMap := &corev1.ConfigMap{}
	configKey := k8stypes.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
	if err := r.objectReader().Get(ctx, configKey, configMap); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("read agent job ledger %s before binding cleanup: %w", configKey, err)
		}
		configMap = renderAgentConfigMap(ad)
		if err := r.applyOwned(ctx, ad, configMap); err != nil {
			return false, fmt.Errorf("create agent job ledger before binding cleanup: %w", err)
		}
	} else if !hasExactBlockingControllerOwner(configMap, ad) || !configMap.DeletionTimestamp.IsZero() {
		return false, fmt.Errorf(
			"refusing to use ConfigMap %s as an agent job ledger before binding cleanup: it is not a live exact-owned object",
			configKey,
		)
	}

	_, releasedClaim, err := r.preflightJobLedger(ctx, ad, configMap)
	if err != nil {
		return false, fmt.Errorf("persist agent job ledger before binding cleanup: %w", err)
	}
	return releasedClaim, nil
}

func (r *ContainerProviderReconciler) jobForLedgerPreflight(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (*batchv1.Job, error) {
	var job batchv1.Job
	key := k8stypes.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}
	if err := r.objectReader().Get(ctx, key, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read agent Job %s before credential metadata: %w", key, err)
	}
	if err := validateAgentJobOwner(&job, ad); err != nil {
		return nil, fmt.Errorf("refusing to use Job %s as execution evidence: %w", key, err)
	}
	return &job, nil
}

// currentGenerationJobForLedgerPreflight accepts only an exact-owned Job whose
// generation annotation is current. An absent annotation is the pre-ledger
// upgrade shape. Generation one is intrinsically current because Kubernetes
// starts persisted generations at one; for later generations, the complete
// provider-owned status must prove this generation already observed the Job.
// Stale status is ambiguous: the Job may belong to the previous generation after
// a spec update, while deleting it may repeat a current-generation execution
// that preceded its status write. Hold both Job and ledger unchanged for operator
// resolution instead of guessing. A Job explicitly annotated for another
// generation is left to deletePreviousGenerationJob.
func (r *ContainerProviderReconciler) currentGenerationJobForLedgerPreflight(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	generation string,
) (*batchv1.Job, error) {
	job, err := r.jobForLedgerPreflight(ctx, ad)
	if err != nil || job == nil {
		return job, err
	}
	jobGeneration := job.Annotations[agentJobGenerationAnnotation]
	if jobGeneration == generation {
		return job, nil
	}
	if jobGeneration != "" {
		return nil, nil
	}
	if !canAssignLegacyJobToCurrentGeneration(ad) {
		return nil, ambiguousLegacyJobGenerationError(job, ad)
	}
	if err := r.annotateLegacyJobGeneration(ctx, job, ad.Generation); err != nil {
		return nil, fmt.Errorf("annotate accepted pre-ledger agent Job generation: %w", err)
	}
	return job, nil
}

func ambiguousLegacyJobGenerationError(job *batchv1.Job, ad *airunwayv1alpha1.AgentDeployment) error {
	return fmt.Errorf(
		"cannot safely assign unannotated pre-ledger Job %s/%s to AgentDeployment generation %d: provider status does not prove which generation created it: %w",
		job.Namespace, job.Name, ad.Generation, errAmbiguousLegacyJobGeneration)
}

// canAssignLegacyJobToCurrentGeneration decides whether an exact-owned,
// unannotated pre-ledger Job can be attributed to the current generation.
// Generation one cannot have a predecessor, so exact ownership is sufficient
// even when the provider crashed before writing status. From generation two on,
// provider status is required to distinguish a current execution from a Job
// left behind by the previous spec generation.
func canAssignLegacyJobToCurrentGeneration(ad *airunwayv1alpha1.AgentDeployment) bool {
	return ad.Generation == 1 || statusProvesCurrentJobGeneration(ad)
}

func terminalJobOutcome(job *batchv1.Job) string {
	switch {
	case jobConditionTrue(job, batchv1.JobFailed):
		return agentJobOutcomeFailed
	case jobConditionTrue(job, batchv1.JobComplete) || job.Status.Succeeded > 0:
		return agentJobOutcomeCompleted
	default:
		return ""
	}
}

func (r *ContainerProviderReconciler) persistJobOutcome(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	configMap *corev1.ConfigMap,
	outcome string,
) error {
	live, err := r.readExactOwnedJobLedger(ctx, ad, configMap)
	if err != nil {
		return err
	}
	base := live.DeepCopy()
	if live.Annotations == nil {
		live.Annotations = map[string]string{}
	}
	live.Annotations[agentJobGenerationAnnotation] = strconv.FormatInt(ad.Generation, 10)
	live.Annotations[agentJobOutcomeAnnotation] = outcome
	delete(live.Annotations, agentJobClaimNonceAnnotation)
	if err := r.Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	return nil
}

func (r *ContainerProviderReconciler) clearJobOutcome(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	configMap *corev1.ConfigMap,
	expected string,
) error {
	live, err := r.readExactOwnedJobLedger(ctx, ad, configMap)
	if err != nil {
		return err
	}
	if live.Annotations[agentJobGenerationAnnotation] != strconv.FormatInt(ad.Generation, 10) ||
		live.Annotations[agentJobOutcomeAnnotation] != expected {
		return fmt.Errorf("agent job ledger changed while clearing %q", expected)
	}
	base := live.DeepCopy()
	delete(live.Annotations, agentJobOutcomeAnnotation)
	delete(live.Annotations, agentJobClaimNonceAnnotation)
	return r.Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// readExactOwnedJobLedger binds every ledger reread to both the AgentDeployment
// incarnation and the ConfigMap incarnation established by ApplyOwned. An
// optimistic-lock patch only protects the object read immediately before that
// patch; without these checks, a stale reconcile could reread and mutate a
// same-name ledger created for a replacement AgentDeployment.
func (r *ContainerProviderReconciler) readExactOwnedJobLedger(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	configMap *corev1.ConfigMap,
) (*corev1.ConfigMap, error) {
	key := client.ObjectKeyFromObject(configMap)
	if configMap.UID == "" {
		return nil, fmt.Errorf("agent job ledger %s has no established object UID", key)
	}

	var live corev1.ConfigMap
	if err := r.objectReader().Get(ctx, key, &live); err != nil {
		return nil, fmt.Errorf("read agent job ledger %s: %w", key, err)
	}
	if !live.DeletionTimestamp.IsZero() ||
		!hasExactBlockingControllerOwner(&live, ad) ||
		live.UID != configMap.UID {
		return nil, fmt.Errorf(
			"refusing to use ConfigMap %s as an agent job ledger: expected live exact-owned UID %q, found UID %q",
			key, configMap.UID, live.UID,
		)
	}
	return &live, nil
}

// terminalJobOutcomeFromStatus accepts only the complete provider-owned status
// shape emitted for a terminal Job in this AgentDeployment generation. A
// generic Failed phase (for example RenderFailed) is deliberately insufficient
// evidence to suppress the one allowed execution.
func terminalJobOutcomeFromStatus(ad *airunwayv1alpha1.AgentDeployment) (string, bool) {
	if ad.Status.ProviderOwner != ContainerFieldOwner {
		return "", false
	}
	var providerReady *metav1.Condition
	for i := range ad.Status.Conditions {
		if ad.Status.Conditions[i].Type == airunwayv1alpha1.AgentConditionTypeProviderReady {
			providerReady = &ad.Status.Conditions[i]
			break
		}
	}
	if providerReady == nil || !providerStatusObservedCurrentGeneration(providerReady.ObservedGeneration, ad.Generation) {
		return "", false
	}
	switch {
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseFailed &&
		providerReady.Status == metav1.ConditionFalse && providerReady.Reason == "JobLost" &&
		ad.Status.Runtime == nil && ad.Status.Replicas == nil:
		return agentJobOutcomeLost, true
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseCompleted &&
		providerReady.Status == metav1.ConditionTrue && providerReady.Reason == agentJobCompletedReason &&
		hasExactJobRuntimeStatus(ad):
		return agentJobOutcomeCompleted, true
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseFailed &&
		providerReady.Status == metav1.ConditionFalse && providerReady.Reason == agentJobFailedReason &&
		hasExactJobRuntimeStatus(ad):
		return agentJobOutcomeFailed, true
	default:
		return "", false
	}
}

func hasExactJobRuntimeStatus(ad *airunwayv1alpha1.AgentDeployment) bool {
	if ad.Status.Runtime == nil || ad.Status.Runtime.WorkloadRef == nil {
		return false
	}
	ref := ad.Status.Runtime.WorkloadRef
	return ref.APIVersion == "batch/v1" && ref.Kind == "Job" &&
		ref.Name == ad.Name && ref.Namespace == ad.Namespace
}

// statusProvesCurrentJobGeneration accepts only status shapes emitted after the
// container provider observed a live Job for the current generation. Merely
// matching ProviderReady.observedGeneration is insufficient: an ambiguity error
// also advances that field. Requiring the exact Job runtime reference and a
// Job-specific phase/reason keeps repeated migration retries fail closed.
func statusProvesCurrentJobGeneration(ad *airunwayv1alpha1.AgentDeployment) bool {
	if ad.Status.ProviderOwner != ContainerFieldOwner || !hasExactJobRuntimeStatus(ad) {
		return false
	}
	var providerReady *metav1.Condition
	for i := range ad.Status.Conditions {
		if ad.Status.Conditions[i].Type == airunwayv1alpha1.AgentConditionTypeProviderReady {
			providerReady = &ad.Status.Conditions[i]
			break
		}
	}
	if providerReady == nil || !providerStatusObservedCurrentGeneration(providerReady.ObservedGeneration, ad.Generation) {
		return false
	}
	switch {
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseDeploying &&
		providerReady.Status == metav1.ConditionFalse && providerReady.Reason == "JobPending":
		return true
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseRunning &&
		providerReady.Status == metav1.ConditionTrue && providerReady.Reason == "JobRunning":
		return true
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseCompleted &&
		providerReady.Status == metav1.ConditionTrue && providerReady.Reason == agentJobCompletedReason:
		return true
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseFailed &&
		providerReady.Status == metav1.ConditionFalse && providerReady.Reason == agentJobFailedReason:
		return true
	default:
		return false
	}
}

// statusHasAmbiguousLegacyJobExecutionEvidence recognizes both the exact Job
// status shapes emitted before ProviderReady.observedGeneration was tracked and
// the explicit ambiguity marker written when an unannotated pre-ledger Job is
// observed beside prior-generation status. If the Job itself is gone,
// at-most-once semantics require consuming the current generation as lost
// rather than risking a repeat of irreversible work.
//
//nolint:gocyclo // Each legacy condition is independent evidence that must fail migration closed.
func statusHasAmbiguousLegacyJobExecutionEvidence(ad *airunwayv1alpha1.AgentDeployment) bool {
	if ad.Generation <= 1 || ad.Status.ProviderOwner != ContainerFieldOwner {
		return false
	}
	var providerReady *metav1.Condition
	for i := range ad.Status.Conditions {
		if ad.Status.Conditions[i].Type == airunwayv1alpha1.AgentConditionTypeProviderReady {
			providerReady = &ad.Status.Conditions[i]
			break
		}
	}
	if providerReady == nil {
		return false
	}
	if providerStatusObservedCurrentGeneration(providerReady.ObservedGeneration, ad.Generation) &&
		ad.Status.Phase == airunwayv1alpha1.AgentPhaseFailed &&
		providerReady.Status == metav1.ConditionFalse &&
		providerReady.Reason == agentJobMigrationAmbiguousReason && hasExactJobRuntimeStatus(ad) {
		return true
	}
	if providerReady.ObservedGeneration != 0 {
		return false
	}
	switch {
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseDeploying &&
		providerReady.Status == metav1.ConditionFalse && providerReady.Reason == "JobPending" &&
		hasExactJobRuntimeStatus(ad):
		return true
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseRunning &&
		providerReady.Status == metav1.ConditionTrue && providerReady.Reason == "JobRunning" &&
		hasExactJobRuntimeStatus(ad):
		return true
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseCompleted &&
		providerReady.Status == metav1.ConditionTrue && providerReady.Reason == agentJobCompletedReason &&
		hasExactJobRuntimeStatus(ad):
		return true
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseFailed &&
		providerReady.Status == metav1.ConditionFalse && providerReady.Reason == agentJobFailedReason &&
		hasExactJobRuntimeStatus(ad):
		return true
	case ad.Status.Phase == airunwayv1alpha1.AgentPhaseFailed &&
		providerReady.Status == metav1.ConditionFalse && providerReady.Reason == "JobLost" &&
		ad.Status.Runtime == nil && ad.Status.Replicas == nil:
		return true
	default:
		return false
	}
}

// providerStatusObservedCurrentGeneration recognizes the pre-generation-
// tracking condition shape only for an object's first generation. Kubernetes
// starts persisted object generations at one, so an omitted
// observedGeneration cannot be stale there. Once the spec has advanced,
// however, zero is ambiguous and must never authorize adopting or suppressing
// a one-shot execution.
func providerStatusObservedCurrentGeneration(observed, generation int64) bool {
	return observed == generation || (observed == 0 && generation == 1)
}

func (r *ContainerProviderReconciler) ensureJobGenerationClaim(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	configMap *corev1.ConfigMap,
) (bool, string, error) {
	live, err := r.readExactOwnedJobLedger(ctx, ad, configMap)
	if err != nil {
		return false, "", err
	}
	generation := strconv.FormatInt(ad.Generation, 10)
	if live.Annotations[agentJobGenerationAnnotation] == generation {
		return false, live.Annotations[agentJobOutcomeAnnotation], nil
	}
	nonceBytes := make([]byte, agentJobClaimNonceBytes)
	if _, err := rand.Read(nonceBytes); err != nil {
		return false, "", fmt.Errorf("generate agent job claim nonce: %w", err)
	}
	claimNonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	base := live.DeepCopy()
	if live.Annotations == nil {
		live.Annotations = map[string]string{}
	}
	live.Annotations[agentJobGenerationAnnotation] = generation
	delete(live.Annotations, agentJobOutcomeAnnotation)
	live.Annotations[agentJobClaimNonceAnnotation] = claimNonce
	if patchErr := r.Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); patchErr != nil {
		// The API server may commit the optimistic patch even when the response is
		// lost. Recover only this exact attempt; an older blank claim remains
		// intentionally ambiguous and must not authorize a second execution.
		current, readErr := r.readExactOwnedJobLedger(ctx, ad, configMap)
		if readErr != nil {
			return false, "", fmt.Errorf("claim agent job generation %d: %w; verify ambiguous claim write: %v",
				ad.Generation, patchErr, readErr)
		}
		if current.Annotations[agentJobGenerationAnnotation] == generation &&
			current.Annotations[agentJobOutcomeAnnotation] == "" &&
			current.Annotations[agentJobClaimNonceAnnotation] == claimNonce {
			return true, "", nil
		}
		return false, "", fmt.Errorf("claim agent job generation %d: %w", ad.Generation, patchErr)
	}
	return true, "", nil
}

func (r *ContainerProviderReconciler) recordJobOutcome(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	configMap *corev1.ConfigMap,
	outcome string,
) error {
	live, err := r.readExactOwnedJobLedger(ctx, ad, configMap)
	if err != nil {
		return err
	}
	if live.Annotations[agentJobGenerationAnnotation] != strconv.FormatInt(ad.Generation, 10) {
		return fmt.Errorf("agent job ledger generation changed while recording %q", outcome)
	}
	if live.Annotations[agentJobOutcomeAnnotation] == outcome && live.Annotations[agentJobClaimNonceAnnotation] == "" {
		return nil
	}
	base := live.DeepCopy()
	if live.Annotations == nil {
		live.Annotations = map[string]string{}
	}
	live.Annotations[agentJobOutcomeAnnotation] = outcome
	delete(live.Annotations, agentJobClaimNonceAnnotation)
	return r.Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// markJobClaimRetryable durably classifies a definitively rejected creation
// before the claim is released. Optimistic-lock conflicts are retried from an
// authoritative read, and a concurrent terminal or generation transition is
// never overwritten by the retry marker.
func (r *ContainerProviderReconciler) markJobClaimRetryable(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	configMap *corev1.ConfigMap,
) error {
	generation := strconv.FormatInt(ad.Generation, 10)
	return k8sretry.RetryOnConflict(k8sretry.DefaultRetry, func() error {
		live, err := r.readExactOwnedJobLedger(ctx, ad, configMap)
		if err != nil {
			return fmt.Errorf("read exact-owned agent job ledger while marking claim retryable: %w", err)
		}
		if live.Annotations[agentJobGenerationAnnotation] != generation {
			return nil
		}
		switch outcome := live.Annotations[agentJobOutcomeAnnotation]; outcome {
		case agentJobOutcomeRetryable:
			if live.Annotations[agentJobClaimNonceAnnotation] == "" {
				return nil
			}
		case "":
			// The claim is still the blank one created by this generation.
		case agentJobOutcomeCompleted, agentJobOutcomeFailed, agentJobOutcomeLost:
			return nil
		default:
			return fmt.Errorf("unknown recorded job outcome %q while marking claim retryable", outcome)
		}

		base := live.DeepCopy()
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[agentJobOutcomeAnnotation] = agentJobOutcomeRetryable
		delete(live.Annotations, agentJobClaimNonceAnnotation)
		return r.Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func (r *ContainerProviderReconciler) releaseJobGenerationClaim(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	configMap *corev1.ConfigMap,
) error {
	live, err := r.readExactOwnedJobLedger(ctx, ad, configMap)
	if err != nil {
		return fmt.Errorf("read exact-owned agent job ledger while releasing claim: %w", err)
	}
	outcome := live.Annotations[agentJobOutcomeAnnotation]
	if live.Annotations[agentJobGenerationAnnotation] != strconv.FormatInt(ad.Generation, 10) ||
		(outcome != "" && outcome != agentJobOutcomeRetryable) {
		return nil
	}
	base := live.DeepCopy()
	delete(live.Annotations, agentJobGenerationAnnotation)
	delete(live.Annotations, agentJobOutcomeAnnotation)
	delete(live.Annotations, agentJobClaimNonceAnnotation)
	return r.Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *ContainerProviderReconciler) annotateLegacyJobGeneration(ctx context.Context, job *batchv1.Job, generation int64) error {
	base := job.DeepCopy()
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[agentJobGenerationAnnotation] = strconv.FormatInt(generation, 10)
	return r.Patch(ctx, job, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *ContainerProviderReconciler) reportRecordedJobOutcome(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	outcome string,
	live *batchv1.Job,
) (ctrl.Result, error) {
	var rt *airunwayv1alpha1.AgentRuntimeStatus
	var replicas *airunwayv1alpha1.AgentReplicaStatus
	if live != nil {
		rt, replicas = liveJobStatus(live)
	} else if outcome != agentJobOutcomeLost {
		rt, replicas = recordedJobStatus(ad)
		if outcome == agentJobOutcomeCompleted {
			// The durable outcome is authoritative even when an older status write
			// omitted (or a later writer zeroed) the terminal replica counters.
			// This provider always renders a single-completion Job, so normalize the
			// retained status without requiring the deleted Job as evidence again.
			if replicas == nil {
				replicas = &airunwayv1alpha1.AgentReplicaStatus{Desired: 1, Available: 1}
			} else {
				if replicas.Desired == 0 {
					replicas.Desired = 1
				}
				if replicas.Available == 0 {
					replicas.Available = 1
				}
			}
		}
	}

	switch outcome {
	case agentJobOutcomeCompleted:
		return ctrl.Result{}, r.status(ctx, ad, airunwayv1alpha1.AgentPhaseCompleted, rt, replicas,
			metav1.ConditionTrue, agentJobCompletedReason, "Agent job completed successfully")
	case agentJobOutcomeFailed:
		return ctrl.Result{}, r.status(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, rt, replicas,
			metav1.ConditionFalse, agentJobFailedReason, "Agent job failed (backoff limit exhausted)")
	case agentJobOutcomeLost:
		return ctrl.Result{}, r.status(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
			metav1.ConditionFalse, "JobLost", "The claimed agent job no longer exists; it will not be run again for this generation")
	default:
		return r.failWithStatus(ctx, ad, "JobOutcomeInvalid", fmt.Errorf("unknown recorded job outcome %q", outcome))
	}
}

func liveJobStatus(job *batchv1.Job) (*airunwayv1alpha1.AgentRuntimeStatus, *airunwayv1alpha1.AgentReplicaStatus) {
	return &airunwayv1alpha1.AgentRuntimeStatus{
			WorkloadRef: &airunwayv1alpha1.RuntimeWorkloadRef{
				APIVersion: "batch/v1", Kind: "Job", Name: job.Name, Namespace: job.Namespace,
			},
		}, &airunwayv1alpha1.AgentReplicaStatus{
			Desired:   ptr.Deref(job.Spec.Parallelism, 1),
			Ready:     job.Status.Active,
			Available: job.Status.Succeeded,
		}
}

func recordedJobStatus(ad *airunwayv1alpha1.AgentDeployment) (*airunwayv1alpha1.AgentRuntimeStatus, *airunwayv1alpha1.AgentReplicaStatus) {
	runtimeCopy := airunwayv1alpha1.AgentRuntimeStatus{
		WorkloadRef: &airunwayv1alpha1.RuntimeWorkloadRef{
			APIVersion: "batch/v1", Kind: "Job", Name: ad.Name, Namespace: ad.Namespace,
		},
	}
	if hasExactJobRuntimeStatus(ad) {
		runtimeCopy = *ad.Status.Runtime
		refCopy := *ad.Status.Runtime.WorkloadRef
		runtimeCopy.WorkloadRef = &refCopy
	}
	replicasCopy := airunwayv1alpha1.AgentReplicaStatus{Desired: 1}
	if ad.Status.Replicas != nil {
		replicasCopy = *ad.Status.Replicas
	}
	return &runtimeCopy, &replicasCopy
}

// jobConditionTrue reports whether a Job carries the given condition with
// status True.
func jobConditionTrue(job *batchv1.Job, condType batchv1.JobConditionType) bool {
	for i := range job.Status.Conditions {
		c := job.Status.Conditions[i]
		if c.Type == condType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// deleteObsolete deletes owned workloads left over from a previous
// spec.lifecycle so a lifecycle switch does not leave both kinds running. Each
// object is looked up by the agent's name in its namespace, and is deleted only
// when it is actually controlled by this AgentDeployment.
//
// An unrelated same-named object is skipped rather than treated as an error:
// this runs on every reconcile, so failing here would let any unrelated Job or
// Deployment that happens to share the agent's name permanently block
// reconciliation of a workload the provider never conflicts with. Refusing to
// *adopt* such an object is applyOwned's job.
// Objects must already carry the name the provider renders them under, since
// those names are bounded per-kind (Services use a stricter limit).
func (r *ContainerProviderReconciler) deleteObsolete(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment, objs ...client.Object) (bool, error) {
	for _, obj := range objs {
		obj.SetNamespace(ad.Namespace)
		pending, err := agentprovider.DeleteOwnedAndWait(ctx, r.Client, r.objectReader(), ad, obj)
		if err != nil {
			return false, fmt.Errorf("delete obsolete workload from previous lifecycle: %w", err)
		}
		if pending {
			return true, nil
		}
	}
	return false, nil
}

// cleanupOwnedWorkloads deletes every workload this container agent owns so a
// revoked/unresolved binding actually stops the running agent. Unlike
// deleteObsolete it targets the workloads under their real names (including the
// suffixed ConfigMap) and silently skips any same-named object this
// AgentDeployment does not own.
func (r *ContainerProviderReconciler) cleanupOwnedWorkloads(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) (bool, error) {
	return r.cleanupOwnedWorkloadsWithOptions(ctx, ad, false, false)
}

func (r *ContainerProviderReconciler) cleanupOwnedWorkloadsForBinding(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) (bool, error) {
	return r.cleanupOwnedWorkloadsWithOptions(ctx, ad, ad.Spec.Lifecycle == airunwayv1alpha1.AgentLifecycleJob, false)
}

func (r *ContainerProviderReconciler) cleanupOwnedWorkloadsPreservingServiceAccount(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (bool, error) {
	// Remove routing before waiting on foreground workload deletion. The generic
	// cleanup helper returns as soon as its first workload is still terminating,
	// so leaving the Service in that ordered list alone could keep routing to pods
	// admitted under the unsafe ServiceAccount until their finalizers clear.
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: agentServiceName(ad), Namespace: ad.Namespace,
	}}
	if err := r.deleteExactOwnedService(ctx, ad, service); err != nil {
		return false, fmt.Errorf("remove agent Service before ServiceAccount repair: %w", err)
	}
	// Keep the non-executable ConfigMap as well as the sanitized ServiceAccount.
	// Besides mounted configuration it is the durable allocation journal for a
	// random ingress Secret, so deleting it during fail-closed workload cleanup
	// could erase the only recovery pointer after a later publication failure.
	return r.cleanupOwnedWorkloadsWithOptions(ctx, ad, true, true)
}

func (r *ContainerProviderReconciler) cleanupOwnedWorkloadsWithOptions(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	preserveJobLedger bool,
	preserveServiceAccount bool,
) (bool, error) {
	deleteConfigMap := !preserveJobLedger
	var reservationErr error
	if deleteConfigMap {
		// A failed status publication can leave the random Secret name only in
		// the ConfigMap reservation. Resolve that journal entry before deleting
		// the ConfigMap; otherwise the immutable Secret survives with no durable
		// pointer that a later reconcile can use to remove it.
		reservationErr = r.cleanupAgentAccessSecretReservation(ctx, ad)
		if reservationErr != nil {
			// Still stop executable resources, but preserve the recovery journal
			// until an ambiguous create or failed Secret deletion is resolved.
			deleteConfigMap = false
		}
	}

	owned := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: agentServiceName(ad), Namespace: ad.Namespace}},
	}
	if !preserveServiceAccount {
		owned = append(owned, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: agentServiceAccountName(ad), Namespace: ad.Namespace}})
	}
	if deleteConfigMap {
		owned = append(owned, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: agentConfigMapName(ad), Namespace: ad.Namespace}})
	}
	pending, err := agentprovider.CleanupOwnedAndWait(ctx, r.Client, r.objectReader(), ad, owned...)
	if err != nil {
		if reservationErr != nil {
			return false, fmt.Errorf("%v; clean up owned workloads: %w", reservationErr, err)
		}
		return false, err
	}
	if ad.Status.Runtime != nil {
		if err := r.deleteAgentAccessSecret(ctx, ad, ad.Status.Runtime.AuthSecretRef); err != nil {
			if reservationErr != nil {
				return false, fmt.Errorf("%v; delete published agent access Secret: %w", reservationErr, err)
			}
			return false, err
		}
	}
	if reservationErr != nil {
		return pending, reservationErr
	}
	if !pending {
		if err := r.deleteAgentAccessSecret(ctx, ad, legacyAccessCredentialRef(ad)); err != nil {
			return false, fmt.Errorf("delete legacy agent access Secret after workload cleanup: %w", err)
		}
	}
	return pending, nil
}

// cleanupAgentAccessSecretReservation resolves the ConfigMap's unpublished
// random Secret reservation, deletes a recovered Secret, and clears the journal
// only after deletion succeeds. Callers retain the ConfigMap on any error.
func (r *ContainerProviderReconciler) cleanupAgentAccessSecretReservation(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) error {
	key := k8stypes.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
	var configMap corev1.ConfigMap
	if err := r.objectReader().Get(ctx, key, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read agent access Secret reservation before cleanup: %w", err)
	}
	if !hasExactBlockingControllerOwner(&configMap, ad) {
		// CleanupOwnedAndWait will also preserve a foreign same-name ConfigMap.
		return nil
	}
	if !configMap.DeletionTimestamp.IsZero() {
		return fmt.Errorf("agent access Secret reservation ConfigMap %s is already terminating", key)
	}
	if configMap.Annotations[agentAccessPendingAnnotation] == "" {
		return nil
	}

	ref, _, recovered, err := r.recoverAgentAccessSecretReservation(ctx, ad)
	if err != nil {
		return fmt.Errorf("resolve agent access Secret reservation before cleanup: %w", err)
	}
	if !recovered {
		return nil
	}
	if err := r.deleteAgentAccessSecret(ctx, ad, ref); err != nil {
		return fmt.Errorf("delete reserved agent access Secret before cleanup: %w", err)
	}
	if err := r.clearAgentAccessSecretReservation(ctx, ad, ref.Name); err != nil {
		return fmt.Errorf("clear deleted agent access Secret reservation after cleanup: %w", err)
	}
	return nil
}

// objectReader returns the authoritative API reader used at ownership and
// deletion boundaries, falling back to Client for directly-constructed tests.
func (r *ContainerProviderReconciler) objectReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// ensureAgentServiceAccount creates or reconciles the empty ServiceAccount used
// by author-selected images. This path is deliberately stricter than the
// generic owned-object apply helper:
//
//   - the controller owner reference must name this exact AgentDeployment API,
//     kind, name and UID. blockOwnerDeletion alone authenticates none of those
//     fields: admission authorizes it against the copied owner identity, not the
//     owner-reference UID;
//   - owner references remain forgeable by a principal that can delete the
//     AgentDeployment, so adoption never treats them as provenance. The full
//     credential-bearing shape is reconciled to an allow-list: no annotations,
//     no extra labels or owners, no Secrets/ImagePullSecrets, and automount=false;
//     an exact-shape preseed is harmless because only that credential-free
//     allow-list, never creation provenance, authorizes workload use;
//   - unsafe fields are removed with an optimistic merge patch. Omitting them
//     from a typed SSA object is not enough: SSA preserves fields owned by
//     another manager, which could let admission or the kubelet consume
//     attacker-selected cloud or registry credentials for provider-created pods.
//
// The authoritative reads and resourceVersion preconditions close the
// create/replace races around adoption. ServiceAccounts themselves are mutable,
// so a caller that can patch this provider-owned object can still race after the
// final verification read. Cluster RBAC must therefore keep update/patch on
// provider-owned ServiceAccounts away from AgentDeployment authors; observed
// drift is cleared here and causes the workload to be torn down before retry.
func (r *ContainerProviderReconciler) ensureAgentServiceAccount(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (*corev1.ServiceAccount, bool, error) {
	if r.Scheme == nil {
		return nil, false, fmt.Errorf("scheme is required to reconcile the agent ServiceAccount")
	}
	desired := renderAgentServiceAccount(ad)
	if err := controllerutil.SetControllerReference(ad, desired, r.Scheme); err != nil {
		return nil, false, fmt.Errorf("set agent ServiceAccount owner: %w", err)
	}
	// Client Create/Patch methods populate desired with the API server response,
	// including fields owned by admission or another field manager. Preserve the
	// provider's credential-free allow-list before those calls so foreign fields
	// cannot become part of the shape we subsequently trust and sanitize toward.
	allowed := desired.DeepCopy()

	key := client.ObjectKeyFromObject(desired)
	var live corev1.ServiceAccount
	created := false
	if err := r.objectReader().Get(ctx, key, &live); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, false, fmt.Errorf("read agent ServiceAccount %s: %w", key, err)
		}
		if err := r.Create(ctx, desired, client.FieldOwner(ContainerFieldOwner)); err == nil {
			live = *desired
			created = true
		} else if !apierrors.IsAlreadyExists(err) {
			return nil, false, fmt.Errorf("create agent ServiceAccount %s: %w", key, err)
		} else if err := r.objectReader().Get(ctx, key, &live); err != nil {
			return nil, false, fmt.Errorf("read concurrently-created agent ServiceAccount %s: %w", key, err)
		}
	}

	if err := validateAgentServiceAccountOwner(&live, ad); err != nil {
		return nil, false, err
	}
	unsafeBeforeApply := agentServiceAccountNeedsSanitization(&live, allowed)
	if unsafeBeforeApply {
		pending, err := r.cleanupOwnedWorkloadsPreservingServiceAccount(ctx, ad)
		if err != nil {
			return nil, false, fmt.Errorf("stop agent workloads before repairing unsafe ServiceAccount %s: %w", key, err)
		}
		if pending {
			return nil, false, fmt.Errorf(
				"agent ServiceAccount %s contains unexpected credential-bearing fields; waiting for the previous workload to stop before repairing it",
				key,
			)
		}
	}

	// Apply provider-owned metadata and automount=false against the exact object
	// just validated. A concurrent replacement makes this resourceVersion
	// precondition conflict instead of being adopted.
	if !created {
		desired.ResourceVersion = live.ResourceVersion
		// Core API objects could use typed apply configurations, but this path
		// intentionally validates and reapplies the exact object revision.
		if err := r.Patch(ctx, desired, client.Apply, //nolint:staticcheck
			client.FieldOwner(ContainerFieldOwner), client.ForceOwnership); err != nil {
			return nil, false, fmt.Errorf("apply agent ServiceAccount %s: %w", key, err)
		}
	}

	var current corev1.ServiceAccount
	if err := r.objectReader().Get(ctx, key, &current); err != nil {
		return nil, false, fmt.Errorf("verify agent ServiceAccount %s: %w", key, err)
	}
	if current.UID != live.UID {
		return nil, false, fmt.Errorf("agent ServiceAccount %s was replaced while reconciling it", key)
	}
	if err := validateAgentServiceAccountOwner(&current, ad); err != nil {
		return nil, false, err
	}
	if !agentServiceAccountNeedsSanitization(&current, allowed) {
		return &current, unsafeBeforeApply, nil
	}
	if !unsafeBeforeApply {
		// Admission or a concurrent writer introduced unsafe fields after the first
		// authoritative read. Stop workloads before removing that evidence too; a
		// crash after sanitation must never make pods admitted under the polluted
		// ServiceAccount look safe on the next reconcile.
		pending, err := r.cleanupOwnedWorkloadsPreservingServiceAccount(ctx, ad)
		if err != nil {
			return nil, false, fmt.Errorf("stop agent workloads before sanitizing ServiceAccount %s: %w", key, err)
		}
		if pending {
			return nil, false, fmt.Errorf(
				"agent ServiceAccount %s acquired unexpected credential-bearing fields; waiting for the previous workload to stop before repairing it",
				key,
			)
		}
	}

	base := current.DeepCopy()
	current.Labels = maps.Clone(allowed.Labels)
	current.Annotations = nil
	current.OwnerReferences = append([]metav1.OwnerReference(nil), allowed.OwnerReferences...)
	current.AutomountServiceAccountToken = ptr.To(false)
	current.Secrets = nil
	current.ImagePullSecrets = nil
	if err := r.Patch(ctx, &current,
		client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, false, fmt.Errorf("clear unexpected Secret references from agent ServiceAccount %s: %w", key, err)
	}

	var verified corev1.ServiceAccount
	if err := r.objectReader().Get(ctx, key, &verified); err != nil {
		return nil, false, fmt.Errorf("verify sanitized agent ServiceAccount %s: %w", key, err)
	}
	if verified.UID != current.UID {
		return nil, false, fmt.Errorf("agent ServiceAccount %s was replaced while sanitizing it", key)
	}
	if err := validateAgentServiceAccountOwner(&verified, ad); err != nil {
		return nil, false, err
	}
	if agentServiceAccountNeedsSanitization(&verified, allowed) {
		return nil, false, fmt.Errorf("agent ServiceAccount %s still contains unexpected credential-bearing fields after sanitization", key)
	}
	return &verified, true, nil
}

func validateAgentServiceAccountOwner(sa *corev1.ServiceAccount, ad *airunwayv1alpha1.AgentDeployment) error {
	if !hasExactBlockingControllerOwner(sa, ad) || !sa.DeletionTimestamp.IsZero() {
		return fmt.Errorf("refusing to adopt ServiceAccount %s/%s: it is not bound to the exact AgentDeployment %s/%s by a blocking controller owner reference",
			sa.Namespace, sa.Name, ad.Namespace, ad.Name)
	}
	return nil
}

func validateAgentJobOwner(job *batchv1.Job, ad *airunwayv1alpha1.AgentDeployment) error {
	if !hasExactBlockingControllerOwner(job, ad) {
		return fmt.Errorf("it is not bound to the exact AgentDeployment %s/%s by a blocking controller owner reference",
			ad.Namespace, ad.Name)
	}
	return nil
}

func hasExactBlockingControllerOwner(obj metav1.Object, ad *airunwayv1alpha1.AgentDeployment) bool {
	owner := metav1.GetControllerOf(obj)
	return owner != nil &&
		owner.APIVersion == airunwayv1alpha1.GroupVersion.String() &&
		owner.Kind == "AgentDeployment" &&
		owner.Name == ad.Name &&
		owner.UID == ad.UID &&
		owner.BlockOwnerDeletion != nil && *owner.BlockOwnerDeletion
}

func agentServiceAccountNeedsSanitization(sa, desired *corev1.ServiceAccount) bool {
	if len(sa.Annotations) != 0 ||
		!maps.Equal(sa.Labels, desired.Labels) ||
		len(sa.OwnerReferences) != 1 ||
		len(desired.OwnerReferences) != 1 ||
		ptr.Deref(sa.AutomountServiceAccountToken, true) ||
		len(sa.Secrets) != 0 ||
		len(sa.ImagePullSecrets) != 0 {
		return true
	}

	gotOwner, wantOwner := sa.OwnerReferences[0], desired.OwnerReferences[0]
	return gotOwner.APIVersion != wantOwner.APIVersion ||
		gotOwner.Kind != wantOwner.Kind ||
		gotOwner.Name != wantOwner.Name ||
		gotOwner.UID != wantOwner.UID ||
		gotOwner.Controller == nil || !*gotOwner.Controller ||
		gotOwner.BlockOwnerDeletion == nil || !*gotOwner.BlockOwnerDeletion
}

func (r *ContainerProviderReconciler) failClosedForServiceAccount(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	cause error,
) (ctrl.Result, error) {
	if _, err := r.cleanupOwnedWorkloadsPreservingServiceAccount(ctx, ad); err != nil {
		return r.failWithAccessCredentialStatus(ctx, ad, currentAccessCredentialRef(ad), "ServiceAccountCleanupFailed",
			fmt.Errorf("%v; stop workload after unsafe ServiceAccount: %w", cause, err))
	}
	return r.failWithStatus(ctx, ad, "ServiceAccountUnsafe", cause)
}

// recoverAgentAccessSecretReservation resolves the random Secret name reserved
// on the provider-owned ConfigMap before Secret creation. The reservation is
// the durable recovery path for a crash or a simultaneous status-write and
// Secret-delete failure; it avoids listing or trusting arbitrary Secrets.
func (r *ContainerProviderReconciler) recoverAgentAccessSecretReservation(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (*airunwayv1alpha1.SecretKeyRef, string, bool, error) {
	configMap, err := r.readAgentAccessReservationConfigMap(ctx, ad)
	if err != nil {
		return nil, "", false, err
	}
	name := configMap.Annotations[agentAccessPendingAnnotation]
	if name == "" {
		return nil, "", false, nil
	}
	createStarted := configMap.Annotations[agentAccessCreateStartedAnnotation]
	if createStarted != "" && createStarted != name {
		return nil, "", false, fmt.Errorf(
			"agent access Secret reservation %q has mismatched create marker %q", name, createStarted)
	}
	createStartedAt := configMap.Annotations[agentAccessCreateStartedAtAnnotation]

	key := k8stypes.NamespacedName{Name: name, Namespace: ad.Namespace}
	var secret corev1.Secret
	if err := r.objectReader().Get(ctx, key, &secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, "", false, fmt.Errorf("read reserved agent access Secret %s: %w", key, err)
		}
		if createStarted == name {
			if createStartedAt == "" {
				// Older controllers persisted only the name marker. Start the
				// ambiguity window when this controller first observes it rather
				// than clearing a request that may still commit late.
				if err := r.markAgentAccessSecretCreateStarted(ctx, ad, name); err != nil {
					return nil, "", false, fmt.Errorf(
						"timestamp legacy agent access Secret reservation %s: %w", key, err)
				}
				return nil, "", false, fmt.Errorf(
					"agent access Secret creation for %s remains ambiguous; timestamped the legacy reservation before waiting", key)
			}
			startedAt, parseErr := time.Parse(time.RFC3339Nano, createStartedAt)
			if parseErr != nil {
				return nil, "", false, fmt.Errorf(
					"agent access Secret reservation %s has invalid create timestamp %q: %w", key, createStartedAt, parseErr)
			}
			// Create may have reached the API server even when its response was lost.
			// Keep the only known random name for a conservative interval so a
			// delayed commit remains recoverable. Once the bounded Create deadline
			// and ambiguity grace have both elapsed, this authoritative NotFound is
			// treated as a pre-Create crash and the stale reservation can be retried.
			if r.accessNow().Before(startedAt.Add(r.accessAmbiguityGrace())) {
				return nil, "", false, fmt.Errorf(
					"agent access Secret creation for %s remains ambiguous; keeping its reservation until %s",
					key, startedAt.Add(r.accessAmbiguityGrace()).Format(time.RFC3339Nano))
			}
			if err := r.clearAgentAccessSecretReservation(ctx, ad, name); err != nil {
				return nil, "", false, fmt.Errorf("clear expired agent access Secret reservation %s: %w", key, err)
			}
			return nil, "", false, nil
		}
		if err := r.clearAgentAccessSecretReservation(ctx, ad, name); err != nil {
			return nil, "", false, fmt.Errorf("clear absent agent access Secret reservation %s: %w", key, err)
		}
		return nil, "", false, nil
	}
	token, err := validatedAgentAccessToken(&secret, ad)
	if err != nil {
		return nil, "", false, fmt.Errorf("reserved agent access Secret %s is not recoverable: %w", key, err)
	}
	ref, checksum := agentAccessCredentialResult(secret.Name, token)
	return ref, checksum, true, nil
}

func (r *ContainerProviderReconciler) readAgentAccessReservationConfigMap(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (*corev1.ConfigMap, error) {
	key := k8stypes.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
	var configMap corev1.ConfigMap
	if err := r.objectReader().Get(ctx, key, &configMap); err != nil {
		return nil, fmt.Errorf("read agent access Secret reservation ConfigMap %s: %w", key, err)
	}
	if !hasExactBlockingControllerOwner(&configMap, ad) || !configMap.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("refusing to use ConfigMap %s for an agent access Secret reservation: it is not a live exact-owned object", key)
	}
	return &configMap, nil
}

func (r *ContainerProviderReconciler) reserveAgentAccessSecret(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	name string,
) error {
	return k8sretry.RetryOnConflict(k8sretry.DefaultRetry, func() error {
		configMap, err := r.readAgentAccessReservationConfigMap(ctx, ad)
		if err != nil {
			return err
		}
		if current := configMap.Annotations[agentAccessPendingAnnotation]; current != "" {
			if current == name {
				return nil
			}
			return fmt.Errorf("agent access Secret %q is already pending publication", current)
		}
		base := configMap.DeepCopy()
		if configMap.Annotations == nil {
			configMap.Annotations = map[string]string{}
		}
		configMap.Annotations[agentAccessPendingAnnotation] = name
		delete(configMap.Annotations, agentAccessCreateStartedAnnotation)
		delete(configMap.Annotations, agentAccessCreateStartedAtAnnotation)
		return r.Patch(ctx, configMap, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func (r *ContainerProviderReconciler) markAgentAccessSecretCreateStarted(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	name string,
) error {
	return k8sretry.RetryOnConflict(k8sretry.DefaultRetry, func() error {
		configMap, err := r.readAgentAccessReservationConfigMap(ctx, ad)
		if err != nil {
			return err
		}
		if configMap.Annotations[agentAccessPendingAnnotation] != name {
			return fmt.Errorf("agent access Secret %q is no longer reserved", name)
		}
		if configMap.Annotations[agentAccessCreateStartedAnnotation] == name &&
			configMap.Annotations[agentAccessCreateStartedAtAnnotation] != "" {
			return nil
		}
		base := configMap.DeepCopy()
		if configMap.Annotations == nil {
			configMap.Annotations = map[string]string{}
		}
		configMap.Annotations[agentAccessCreateStartedAnnotation] = name
		configMap.Annotations[agentAccessCreateStartedAtAnnotation] = r.accessNow().Format(time.RFC3339Nano)
		return r.Patch(ctx, configMap, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func (r *ContainerProviderReconciler) clearAgentAccessSecretReservation(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	expectedName string,
) error {
	if expectedName == "" {
		return nil
	}
	return k8sretry.RetryOnConflict(k8sretry.DefaultRetry, func() error {
		configMap, err := r.readAgentAccessReservationConfigMap(ctx, ad)
		if err != nil {
			return err
		}
		if configMap.Annotations[agentAccessPendingAnnotation] != expectedName {
			return nil
		}
		base := configMap.DeepCopy()
		delete(configMap.Annotations, agentAccessPendingAnnotation)
		delete(configMap.Annotations, agentAccessCreateStartedAnnotation)
		delete(configMap.Annotations, agentAccessCreateStartedAtAnnotation)
		return r.Patch(ctx, configMap, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// ensureAgentAccessCredentials creates or reconciles the bearer token used by
// repo-owned server images at their outward-facing Service. The token is kept in
// a separate Secret from the model credential: callers can be authorized to use
// an agent without learning the upstream provider key, and rotating one does not
// silently rotate the other.
//
// The authoritative read preserves an existing strong token across reconciles
// and refuses to adopt a same-named user Secret. New Secrets are immutable and
// created once, so this reconciler itself only requires get/create and never
// patches another Secret in the cluster.
func (r *ContainerProviderReconciler) ensureAgentAccessCredentials(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
) (*airunwayv1alpha1.SecretKeyRef, string, bool, error) {
	if r.Scheme == nil {
		return nil, "", false, fmt.Errorf("scheme is required to create agent access credentials")
	}

	if ad.Status.Runtime != nil && ad.Status.Runtime.AuthSecretRef != nil &&
		ad.Status.Runtime.AuthSecretRef.Key == agentAccessTokenKey {
		ref := ad.Status.Runtime.AuthSecretRef
		key := k8stypes.NamespacedName{Name: ref.Name, Namespace: ad.Namespace}
		var existing corev1.Secret
		if err := r.objectReader().Get(ctx, key, &existing); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, "", false, fmt.Errorf("read agent access Secret %s: %w", key, err)
			}
		} else {
			token, validationErr := validatedAgentAccessToken(&existing, ad)
			if validationErr == nil {
				ref, checksum := agentAccessCredentialResult(existing.Name, token)
				return ref, checksum, false, nil
			}
			if existing.Name == legacyAgentAccessSecretName(ad) {
				legacyErr := validatedLegacyAgentAccessToken(&existing, ad)
				if legacyErr == nil {
					// A deterministic legacy Secret name could be preseeded with a
					// caller-chosen token and forgeable owner metadata. Preserve no
					// bearer material across the migration boundary; allocate a fresh
					// random token and delete the legacy Secret only after the workload
					// and status have switched to it.
					log.FromContext(ctx).Info("Rotating legacy agent access token during deterministic-name migration", "secret", key)
				} else {
					validationErr = legacyErr
				}
			}
			log.FromContext(ctx).Info("Ignoring untrusted agent access Secret reference", "secret", key, "reason", validationErr.Error())
		}
	}
	if ref, checksum, recovered, err := r.recoverAgentAccessSecretReservation(ctx, ad); err != nil || recovered {
		return ref, checksum, recovered, err
	}

	for range agentAccessCreateAttempts {
		raw := make([]byte, agentAccessTokenBytes)
		if _, err := rand.Read(raw); err != nil {
			return nil, "", false, fmt.Errorf("generate agent access token: %w", err)
		}
		token := []byte(base64.RawURLEncoding.EncodeToString(raw))
		name := agentAccessSecretName(ad, token)
		key := k8stypes.NamespacedName{Name: name, Namespace: ad.Namespace}
		secret, err := r.renderAgentAccessSecret(ad, name, token)
		if err != nil {
			return nil, "", false, err
		}
		if err := r.reserveAgentAccessSecret(ctx, ad, name); err != nil {
			return nil, "", false, fmt.Errorf("reserve agent access Secret %s: %w", key, err)
		}
		if err := r.markAgentAccessSecretCreateStarted(ctx, ad, name); err != nil {
			return nil, "", false, fmt.Errorf("mark agent access Secret %s create started: %w", key, err)
		}
		createCtx, cancelCreate := context.WithTimeout(ctx, r.accessCreateTimeout())
		createErr := r.Create(createCtx, secret)
		cancelCreate()
		if createErr != nil {
			// Create can commit at the API server and still return a timeout or
			// transport error. Authoritatively reread the known random name before
			// allocating another one, otherwise each retry can strand a distinct
			// immutable Secret that status never references.
			var existing corev1.Secret
			readErr := r.objectReader().Get(ctx, key, &existing)
			if readErr == nil {
				existingToken, validationErr := validatedAgentAccessToken(&existing, ad)
				if validationErr == nil && string(existingToken) == string(token) {
					ref, checksum := agentAccessCredentialResult(name, token)
					return ref, checksum, true, nil
				}
				// A foreign or differently bound collision proves this attempt did
				// not create the desired Secret. Clear its reservation before trying
				// a new random name.
				if clearErr := r.clearAgentAccessSecretReservation(ctx, ad, name); clearErr != nil {
					return nil, "", false, fmt.Errorf("create agent access Secret %s: %v; clear collided reservation: %w",
						key, createErr, clearErr)
				}
				continue
			}
			if !apierrors.IsNotFound(readErr) {
				return nil, "", false, fmt.Errorf("create agent access Secret %s: %v; authoritative reread failed: %w",
					key, createErr, readErr)
			}
			if definitiveResourceWriteFailure(createErr) {
				if clearErr := r.clearAgentAccessSecretReservation(ctx, ad, name); clearErr != nil {
					return nil, "", false, fmt.Errorf("clear unused agent access Secret reservation %s: %w", key, clearErr)
				}
				if apierrors.IsAlreadyExists(createErr) {
					continue
				}
				return nil, "", false, fmt.Errorf("create agent access Secret %s: %w", key, createErr)
			}
			return nil, "", false, fmt.Errorf(
				"create agent access Secret %s: %w; authoritative reread returned NotFound, keeping the reservation because the create outcome is ambiguous",
				key, createErr)
		}
		ref, checksum := agentAccessCredentialResult(name, token)
		return ref, checksum, true, nil
	}
	return nil, "", false, fmt.Errorf("could not allocate an unguessable agent access Secret name after %d attempts", agentAccessCreateAttempts)
}

func (r *ContainerProviderReconciler) renderAgentAccessSecret(
	ad *airunwayv1alpha1.AgentDeployment,
	name string,
	token []byte,
) (*corev1.Secret, error) {
	key := k8stypes.NamespacedName{Name: name, Namespace: ad.Namespace}
	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ad.Namespace, Labels: agentLabels(ad)},
		Immutable:  ptr.To(true),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{agentAccessTokenKey: token},
	}
	if err := controllerutil.SetControllerReference(ad, secret, r.Scheme); err != nil {
		return nil, fmt.Errorf("set agent access Secret %s owner: %w", key, err)
	}
	return secret, nil
}

func validatedAgentAccessToken(
	secret *corev1.Secret,
	ad *airunwayv1alpha1.AgentDeployment,
) ([]byte, error) {
	key := k8stypes.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}
	if !secret.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("agent access Secret %s is terminating", key)
	}
	if !hasExactBlockingControllerOwner(secret, ad) {
		return nil, fmt.Errorf("refusing to adopt agent access Secret %s: it is not bound to the exact AgentDeployment %s/%s by a blocking controller owner reference",
			key, ad.Namespace, ad.Name)
	}
	token := secret.Data[agentAccessTokenKey]
	decoded, err := base64.RawURLEncoding.DecodeString(string(token))
	if err != nil || len(decoded) != agentAccessTokenBytes {
		return nil, fmt.Errorf("agent access Secret %s contains an invalid %q token", key, agentAccessTokenKey)
	}
	if secret.Name != agentAccessSecretName(ad, token) {
		return nil, fmt.Errorf("agent access Secret %s name is not bound to its token", key)
	}
	return token, nil
}

func validatedLegacyAgentAccessToken(
	secret *corev1.Secret,
	ad *airunwayv1alpha1.AgentDeployment,
) error {
	key := k8stypes.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}
	if !secret.DeletionTimestamp.IsZero() {
		return fmt.Errorf("legacy agent access Secret %s is terminating", key)
	}
	if !hasExactBlockingControllerOwner(secret, ad) {
		return fmt.Errorf("refusing to migrate legacy agent access Secret %s: it is not bound to the exact AgentDeployment %s/%s by a blocking controller owner reference",
			key, ad.Namespace, ad.Name)
	}
	if secret.Name != legacyAgentAccessSecretName(ad) {
		return fmt.Errorf("agent access Secret %s is not the legacy deterministic name", key)
	}
	token := secret.Data[agentAccessTokenKey]
	decoded, err := base64.RawURLEncoding.DecodeString(string(token))
	if err != nil || len(decoded) != agentAccessTokenBytes {
		return fmt.Errorf("legacy agent access Secret %s contains an invalid %q token", key, agentAccessTokenKey)
	}
	return nil
}

func (r *ContainerProviderReconciler) deleteAgentAccessSecret(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	ref *airunwayv1alpha1.SecretKeyRef,
) error {
	if ref == nil || ref.Key != agentAccessTokenKey {
		return nil
	}
	key := k8stypes.NamespacedName{Name: ref.Name, Namespace: ad.Namespace}
	var secret corev1.Secret
	if err := r.objectReader().Get(ctx, key, &secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	validationTarget := secret.DeepCopy()
	validationTarget.DeletionTimestamp = nil
	if _, err := validatedAgentAccessToken(validationTarget, ad); err != nil {
		if legacyErr := validatedLegacyAgentAccessToken(validationTarget, ad); legacyErr != nil {
			return nil
		}
	}
	if !secret.DeletionTimestamp.IsZero() {
		return fmt.Errorf("agent access Secret %s deletion is still pending", key)
	}
	uid := secret.UID
	if err := r.Delete(ctx, &secret, &client.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	}); err != nil {
		return client.IgnoreNotFound(err)
	}
	var current corev1.Secret
	if err := r.objectReader().Get(ctx, key, &current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if current.UID != uid {
		return nil
	}
	return fmt.Errorf("agent access Secret %s deletion is still pending", key)
}

// deleteLegacyAgentAccessSecret removes the deterministic pre-migration token
// only after the Deployment and provider status both reference a derived Secret.
// The deletion is repeated on every successful reconcile so a crash after the
// status write cannot strand the guessable legacy credential indefinitely.
func (r *ContainerProviderReconciler) deleteLegacyAgentAccessSecret(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	activeRef *airunwayv1alpha1.SecretKeyRef,
) error {
	legacyName := legacyAgentAccessSecretName(ad)
	if activeRef == nil || activeRef.Key != agentAccessTokenKey || activeRef.Name == legacyName {
		return nil
	}
	return r.deleteAgentAccessSecret(ctx, ad, &airunwayv1alpha1.SecretKeyRef{
		Name: legacyName,
		Key:  agentAccessTokenKey,
	})
}

func agentAccessCredentialResult(name string, token []byte) (*airunwayv1alpha1.SecretKeyRef, string) {
	digest := sha256.Sum256(token)
	return &airunwayv1alpha1.SecretKeyRef{Name: name, Key: agentAccessTokenKey}, fmt.Sprintf("%x", digest)
}

// containerProviderSettings holds the provider-owned rendering settings the
// container provider resolves from the framework's AgentProviderConfig (not
// from the user's AgentDeployment): whether the agent uses the container
// backend, the default catalog image (if unambiguous), and the provider-owned
// writable-root-filesystem posture.
type containerProviderSettings struct {
	isContainer   bool
	image         string
	imageErr      string
	imageRevision string
	writableRoot  bool
}

// containerSecurityOverrides is the provider override contract for the
// container backend. It allows controlled security-context overrides while
// keeping provider-owned secure defaults.
type containerSecurityOverrides struct {
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
	SecurityContext    *corev1.SecurityContext    `json:"securityContext,omitempty"`
}

// resolveContainerProvider looks up the framework's AgentProviderConfig and
// derives the provider-owned settings. The default image is taken from the
// catalog annotation only when exactly one entry carries an image; multiple
// images are ambiguous (an AgentDeployment carries no catalog-item identity), so
// the caller must require an explicit spec.config.image instead of guessing.
func (r *ContainerProviderReconciler) resolveContainerProvider(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) (containerProviderSettings, error) {
	var apc airunwayv1alpha1.AgentProviderConfig
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: ad.Spec.Framework.Name}, &apc); err != nil {
		if apierrors.IsNotFound(err) {
			return containerProviderSettings{}, nil
		}
		return containerProviderSettings{}, err
	}
	if apc.Spec.Capabilities == nil || apc.Spec.Capabilities.Backend != airunwayv1alpha1.AgentProviderBackendContainer {
		return containerProviderSettings{}, nil
	}

	s := containerProviderSettings{isContainer: true}
	s.writableRoot = apc.Spec.Capabilities.WritableRootFilesystem != nil && *apc.Spec.Capabilities.WritableRootFilesystem

	// A malformed catalog is deferred, not fatal. The catalog exists only to
	// *default* the image, so an agent that sets spec.config.image needs nothing
	// from it — failing the whole reconcile here would take out every agent on
	// the framework because one piece of marketplace UI metadata has a typo.
	// Agents that do rely on the catalog get this text in their MissingImage
	// status instead, which names the framework and the parse error.
	catalog, err := apc.CatalogItems()
	if err != nil {
		s.imageErr = fmt.Sprintf("the %s annotation on AgentProviderConfig %q could not be parsed, so no catalog image is available: %v",
			airunwayv1alpha1.AgentProviderCatalogAnnotation, apc.Name, err)
		return s, nil
	}

	var images []string
	for i := range catalog {
		if catalog[i].Image != "" {
			images = append(images, catalog[i].Image)
		}
	}
	switch {
	case len(images) == 1:
		s.image, err = resolveCatalogImageVersion(images[0], apc.Status.Version)
		if err != nil {
			s.imageErr = err.Error()
		} else if agentImageUsesMutableTag(s.image) {
			// A moving catalog tag needs a stable build signal in the Deployment
			// template. ProviderConfig resourceVersion and lastHeartbeat are
			// intentionally excluded: both change periodically and would create an
			// endless rollout loop. Local dev builds report a unique source revision;
			// main and tagged releases change the image reference itself.
			s.imageRevision = apc.Status.Version
		}
	case len(images) > 1:
		s.imageErr = "framework catalog annotation advertises multiple images; set spec.config.image explicitly to select one"
	}
	return s, nil
}

func resolveCatalogImageVersion(image, providerVersion string) (string, error) {
	if !strings.Contains(image, agentVersionPlaceholder) {
		return image, nil
	}
	prefix, version, found := strings.Cut(providerVersion, ":")
	if !found || prefix != "agent-container-provider" || version == "" {
		return "", fmt.Errorf("catalog image %q requires %s, but AgentProviderConfig.status.version %q does not contain an agent-container provider tag",
			image, agentVersionPlaceholder, providerVersion)
	}
	return strings.ReplaceAll(image, agentVersionPlaceholder, agentRuntimeImageTag(version)), nil
}

// agentRuntimeImageTag maps the provider's reported source/build version onto
// the tags emitted for the bundled runtime images. docker/metadata-action
// removes a leading "v" from semver {{version}} tags. Main builds publish the
// same immutable main-<revision> tag carried in provider status, so overlapping
// release workflows cannot make one revision resolve another revision's
// mutable latest tag. Legacy bare-main and local dev builds retain the
// published/local latest channel.
func agentRuntimeImageTag(providerVersion string) string {
	switch {
	case strings.HasPrefix(providerVersion, "main-"):
		return providerVersion
	case providerVersion == "main", providerVersion == "dev", strings.HasPrefix(providerVersion, "dev-"):
		return agentImageLatestTag
	case len(providerVersion) > 1 && providerVersion[0] == 'v' && providerVersion[1] >= '0' && providerVersion[1] <= '9':
		return normalizePublishedImageTag(providerVersion[1:])
	default:
		return normalizePublishedImageTag(providerVersion)
	}
}

// normalizePublishedImageTag mirrors docker/metadata-action's OCI tag
// sanitization for SemVer build metadata. A '+' is valid in SemVer and valid in
// a Git tag, but not in an OCI image tag; published release images therefore use
// '-' at that boundary (v1.2.3+build.4 -> 1.2.3-build.4).
func normalizePublishedImageTag(tag string) string {
	return strings.ReplaceAll(tag, "+", "-")
}

// agentImageUsesMutableTag reports whether a new pod can resolve the same image
// string to different content. A digest always pins content, even when paired
// with a moving tag.
func agentImageUsesMutableTag(image string) bool {
	if strings.Contains(image, "@") {
		return false
	}
	tag := ""
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		tag = image[colon+1:]
	}
	return tag == "" || tag == agentImageLatestTag
}

// parseContainerConfig extracts the container provider's fields from the opaque
// spec.config. A malformed config is reported rather than silently rendering as
// an empty config, which would otherwise surface as a confusing "no container
// image" failure.
func parseContainerConfig(raw *runtime.RawExtension) (containerConfig, error) {
	var cfg containerConfig
	if raw == nil || len(raw.Raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw.Raw, &cfg); err != nil {
		return containerConfig{}, fmt.Errorf("parse spec.config for the container backend: %w", err)
	}
	// A port outside the valid TCP range is later copied verbatim into the
	// container port and Service targetPort, where Kubernetes rejects it and
	// the agent loops in render failure. Reject it here with a clear reason
	// instead. Port 0 (unset) is allowed and falls back to the default.
	if cfg.Port < 0 || cfg.Port > 65535 {
		return containerConfig{}, fmt.Errorf("spec.config.port %d is out of range: must be between 1 and 65535", cfg.Port)
	}
	return cfg, nil
}

// agentConfigMapName is the name of the mounted agent.json ConfigMap, bounded
// to the Kubernetes object-name limit.
func agentConfigMapName(ad *airunwayv1alpha1.AgentDeployment) string {
	return agentprovider.BoundedResourceName(ad.Name, "-config")
}

// agentAccessSecretName binds the unguessable Secret name to the random token.
// Status persists the name; recomputing it here prevents a forged owner
// reference from redirecting the workload to attacker-chosen credential data.
func agentAccessSecretName(ad *airunwayv1alpha1.AgentDeployment, token []byte) string {
	digest := sha256.Sum256(token)
	suffix := agentAccessSecretSuffix + "-" + fmt.Sprintf("%x", digest[:agentAccessNameHashBytes])
	return agentprovider.BoundedResourceName(ad.Name, suffix)
}

func legacyAgentAccessSecretName(ad *airunwayv1alpha1.AgentDeployment) string {
	return agentprovider.BoundedResourceName(ad.Name, agentAccessSecretSuffix)
}

// renderAgentConfigMap mounts the agent's full spec.config as agent.json (the
// pinned BYO contract). An empty config yields an empty JSON object.
func renderAgentConfigMap(ad *airunwayv1alpha1.AgentDeployment) *corev1.ConfigMap {
	payload := "{}"
	if ad.Spec.Config != nil && len(ad.Spec.Config.Raw) > 0 {
		payload = string(ad.Spec.Config.Raw)
	}
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: agentConfigMapName(ad), Namespace: ad.Namespace, Labels: agentLabels(ad)},
		Data:       map[string]string{agentConfigFileName: payload},
	}
}

// agentPodSpec builds the shared pod spec for a container-backed agent: the
// BYO image, the resolved model binding injected as OpenAI-compatible env, the
// requested OTLP observability env, the mounted agent.json config, the agent's
// requested resources, and a hardened, provider-owned security posture
// (runAsNonRoot, dropped capabilities, seccomp, read-only root filesystem with
// an always-writable /tmp scratch mount). The read-only root is relaxed only
// when the framework's provider config declares it needs a writable root
// (writableRoot) — a provider-owned property. Validated
// spec.provider.overrides can further adjust pod/container security context.
func agentPodSpec(
	ad *airunwayv1alpha1.AgentDeployment,
	in renderInputs,
) corev1.PodSpec {
	cfg := in.cfg
	env := []corev1.EnvVar{
		{Name: "AIRUNWAY_AGENT_CONFIG", Value: agentConfigMountPath},
		{Name: "AIRUNWAY_AGENT_MODE", Value: agentRuntimeMode(ad)},
		{Name: "AIRUNWAY_AGENT_PORT", Value: fmt.Sprintf("%d", containerPort(cfg))},
	}
	env = append(env, modelBindingEnv(in.binding)...)
	env = append(env, otlpEnv(ad)...)
	if in.authSecretRef != nil {
		env = append(env, corev1.EnvVar{
			Name: agentAccessTokenEnv,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: in.authSecretRef.Name},
				Key:                  in.authSecretRef.Key,
			}},
		})
	}

	containerSecurity := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(!in.writableRoot),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	podSecurity := &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		// runAsNonRoot alone is not enough. When an image declares a NAMED user
		// (USER nobody), the kubelet cannot prove that name is non-root without
		// resolving it, so it refuses the container outright:
		//
		//   container has runAsNonRoot and image has non-numeric user (nobody),
		//   cannot verify user is non-root
		//
		// Most stock non-root images do exactly that, so without a numeric
		// default the container backend cannot run them at all. 65532 is the
		// conventional distroless/nonroot UID. An image needing a different one
		// can override it through spec.provider.overrides.
		RunAsUser:      ptr.To[int64](defaultAgentRunAsUser),
		RunAsGroup:     ptr.To[int64](defaultAgentRunAsUser),
		FSGroup:        ptr.To[int64](defaultAgentRunAsUser),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	applyContainerSecurityOverrides(podSecurity, containerSecurity, in.securityOverrides, in.writableRoot)

	container := corev1.Container{
		Name:            agentContainerName,
		Image:           cfg.Image,
		ImagePullPolicy: agentImagePullPolicy(cfg.Image),
		Ports: []corev1.ContainerPort{{
			Name:          agentContainerPortName,
			ContainerPort: containerPort(cfg),
		}},
		Env: env,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "agent-config", MountPath: agentConfigMountDir, ReadOnly: true},
			// Always provide a writable scratch dir so frameworks that need to
			// write (caches, sessions) work without relaxing the whole root FS.
			{Name: "tmp", MountPath: "/tmp"},
		},
		Resources:       agentResources(ad),
		SecurityContext: containerSecurity,
	}
	if ad.Spec.Lifecycle != airunwayv1alpha1.AgentLifecycleJob {
		// The generic contract has always required a server on the selected port,
		// but did not require framework-independent health paths. TCP probes retain
		// compatibility with existing BYO images while still preventing traffic
		// before their listener is accepting connections.
		container.StartupProbe = tcpAgentProbe(cfg)
		container.StartupProbe.PeriodSeconds = 5
		container.StartupProbe.FailureThreshold = 60
		container.ReadinessProbe = tcpAgentProbe(cfg)
		container.ReadinessProbe.PeriodSeconds = 5
		container.ReadinessProbe.FailureThreshold = 3
		container.LivenessProbe = tcpAgentProbe(cfg)
		container.LivenessProbe.PeriodSeconds = 10
		container.LivenessProbe.FailureThreshold = 3
	}
	if len(cfg.Command) > 0 {
		container.Command = cfg.Command
	}
	if len(cfg.Args) > 0 {
		container.Args = cfg.Args
	}

	return corev1.PodSpec{
		SecurityContext:    podSecurity,
		ServiceAccountName: in.serviceAccountName,
		// The image here is chosen by whoever wrote the AgentDeployment, so the
		// pod must not carry an API credential the author did not already hold.
		// Left unset, Kubernetes mounts the namespace's default ServiceAccount
		// token, which would let someone who can create an AgentDeployment — but
		// not a Pod — run code as that ServiceAccount and inherit whatever it can
		// do. Agents talk to a model endpoint, not to the API server, so nothing
		// here needs a token.
		AutomountServiceAccountToken: ptr.To(false),
		Containers:                   []corev1.Container{container},
		Volumes: []corev1.Volume{
			{
				Name: "agent-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: in.configMapName},
					},
				},
			},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}
}

// agentImagePullPolicy renders Kubernetes' image-name default explicitly. The
// API server defaults this field only when a Pod template is first created; it
// does not revise an existing IfNotPresent policy when a later SSA update
// changes the image to an explicit or implicit latest tag.
func agentImagePullPolicy(image string) corev1.PullPolicy {
	name := image
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
		// A digest-only reference is immutable even though it has no tag.
		if strings.LastIndex(name, ":") <= strings.LastIndex(name, "/") {
			return corev1.PullIfNotPresent
		}
	}

	tag := ""
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		tag = name[colon+1:]
	}
	if tag == "" || tag == agentImageLatestTag {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

func agentRuntimeMode(ad *airunwayv1alpha1.AgentDeployment) string {
	if ad.Spec.Lifecycle == airunwayv1alpha1.AgentLifecycleJob {
		return "job"
	}
	return "server"
}

func tcpAgentProbe(cfg containerConfig) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(containerPort(cfg)),
			},
		},
		TimeoutSeconds: 3,
	}
}

// agentResources maps spec.resources onto container requests/limits so the
// limits accepted by the CRD are actually enforced on the workload.
func agentResources(ad *airunwayv1alpha1.AgentDeployment) corev1.ResourceRequirements {
	var req corev1.ResourceRequirements
	if ad.Spec.Resources == nil {
		return req
	}
	if len(ad.Spec.Resources.Requests) > 0 {
		req.Requests = ad.Spec.Resources.Requests
	}
	if len(ad.Spec.Resources.Limits) > 0 {
		req.Limits = ad.Spec.Resources.Limits
	}
	return req
}

// modelBindingEnv renders the resolved model binding as the environment
// variable family matching its API type, so a container agent receives the
// runtime contract its SDK expects rather than always OpenAI-shaped variables.
// OpenAI-compatible endpoints (openai/custom and keyless in-cluster
// deploymentRef bindings, which carry no APIType) use the OPENAI_* family; the
// well-known Anthropic and Azure OpenAI families are emitted for those types.
func modelBindingEnv(binding airunwayv1alpha1.ModelBindingStatus) []corev1.EnvVar {
	var baseURLKey, modelKey, apiKeyKey string
	switch binding.APIType {
	case airunwayv1alpha1.ExternalAPITypeAnthropic:
		baseURLKey, modelKey, apiKeyKey = "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_API_KEY"
	case airunwayv1alpha1.ExternalAPITypeAzureOpenAI:
		baseURLKey, modelKey, apiKeyKey = "AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_MODEL", "AZURE_OPENAI_API_KEY"
	default:
		baseURLKey, modelKey, apiKeyKey = "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENAI_API_KEY"
	}

	env := []corev1.EnvVar{
		{Name: baseURLKey, Value: binding.BaseURL},
		{Name: modelKey, Value: binding.ModelName},
	}
	if binding.CredentialsRef != nil {
		env = append(env, corev1.EnvVar{
			Name: apiKeyKey,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: binding.CredentialsRef.Name},
					Key:                  binding.CredentialsRef.Key,
				},
			},
		})
	} else {
		// Keyless in-cluster model endpoints still expect an API-key variable to
		// be present in many framework SDKs; inject a harmless literal token.
		env = append(env, corev1.EnvVar{Name: apiKeyKey, Value: agentprovider.KeylessCredentialValue})
	}
	return env
}

// otlpEnv translates spec.observability.otlp into the standard OTLP exporter
// environment variables the agent runtime reads, per the API contract.
func otlpEnv(ad *airunwayv1alpha1.AgentDeployment) []corev1.EnvVar {
	if ad.Spec.Observability == nil || ad.Spec.Observability.OTLP == nil {
		return nil
	}
	otlp := ad.Spec.Observability.OTLP
	env := []corev1.EnvVar{{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: otlp.Endpoint}}
	if otlp.Protocol != "" {
		env = append(env, corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: otlp.Protocol})
	}
	return env
}

// agentPodTemplate builds the shared pod template, carrying the config
// checksum so a change to the mounted agent.json rolls the workload.
func agentPodTemplate(ad *airunwayv1alpha1.AgentDeployment, in renderInputs) corev1.PodTemplateSpec {
	annotations := map[string]string{agentConfigChecksumAnnotation: in.configChecksum}
	if in.accessTokenHash != "" {
		annotations[agentAccessChecksumAnnotation] = in.accessTokenHash
	}
	if in.modelCredentialChecksum != "" {
		annotations[agentModelCredentialChecksumAnnotation] = in.modelCredentialChecksum
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      agentLabels(ad),
			Annotations: annotations,
		},
		Spec: agentPodSpec(ad, in),
	}
}

// renderAgentDeployment renders a long-running Deployment for the agent.
func renderAgentDeployment(ad *airunwayv1alpha1.AgentDeployment, in renderInputs) *appsv1.Deployment {
	template := agentPodTemplate(ad, in)
	if in.catalogImageRevision != "" {
		// This annotation is deliberately Deployment-only. A changed provider build
		// should roll a long-running latest-tag workload, but must never authorize a
		// second execution of a one-shot Job generation.
		template.Annotations[agentCatalogImageRevisionAnnotation] = in.catalogImageRevision
	}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace, Labels: agentLabels(ad)},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: agentSelector(ad)},
			Template: template,
		},
	}
}

// renderAgentJob renders a one-shot Job for the agent (spec.lifecycle: job).
// The pod spec is shared with the Deployment path; only the restart policy
// differs (Jobs require Never or OnFailure).
//
// The rendered template's hash is recorded for drift diagnostics. A live Job is
// never recreated solely because the hash changes: it may already have started,
// and the provider's contract permits only one execution per generation.
func renderAgentJob(ad *airunwayv1alpha1.AgentDeployment, in renderInputs) (*batchv1.Job, error) {
	template := agentPodTemplate(ad, in)
	template.Spec.RestartPolicy = corev1.RestartPolicyNever

	hash, err := agentprovider.HashJSON(template)
	if err != nil {
		return nil, err
	}

	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ad.Name,
			Namespace: ad.Namespace,
			Labels:    agentLabels(ad),
			Annotations: map[string]string{
				agentTemplateHashAnnotation:  hash,
				agentJobGenerationAnnotation: strconv.FormatInt(ad.Generation, 10),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](3),
			Template:     template,
		},
	}, nil
}

// agentServiceName is the name of the Service fronting the agent. Services are
// validated as RFC 1035 DNS labels (63 characters), which is stricter than the
// AgentDeployment name limit, so it is bounded separately.
func agentServiceName(ad *airunwayv1alpha1.AgentDeployment) string {
	return agentprovider.BoundedDNSLabelName(ad.Name)
}

func agentServiceAccountName(ad *airunwayv1alpha1.AgentDeployment) string {
	return agentprovider.BoundedDNSLabelName(ad.Name + agentServiceAccountSuffix)
}

func renderAgentServiceAccount(ad *airunwayv1alpha1.AgentDeployment) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentServiceAccountName(ad),
			Namespace: ad.Namespace,
			Labels:    agentLabels(ad),
		},
		AutomountServiceAccountToken: ptr.To(false),
	}
}

// renderAgentService renders the ClusterIP Service fronting the agent.
func renderAgentService(ad *airunwayv1alpha1.AgentDeployment) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: agentServiceName(ad), Namespace: ad.Namespace, Labels: agentLabels(ad)},
		Spec: corev1.ServiceSpec{
			Selector: agentSelector(ad),
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromString(agentContainerPortName)}},
		},
	}
}

// agentSelector is the immutable pod selector for the agent's workload. Values
// are bounded to the 63-byte label limit; an AgentDeployment name may legally
// be far longer than that, and an oversized selector value makes every rendered
// workload fail admission.
func agentSelector(ad *airunwayv1alpha1.AgentDeployment) map[string]string {
	return map[string]string{
		"airunway.ai/agent":     agentprovider.BoundedLabelValue(ad.Name),
		"airunway.ai/agent-uid": string(ad.UID),
	}
}

func agentLabels(ad *airunwayv1alpha1.AgentDeployment) map[string]string {
	labels := agentSelector(ad)
	labels["airunway.ai/framework"] = agentprovider.BoundedLabelValue(ad.Spec.Framework.Name)
	return labels
}

func (r *ContainerProviderReconciler) failWithStatus(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	reason string,
	cause error,
) (ctrl.Result, error) {
	if err := r.status(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil, metav1.ConditionFalse, reason, cause.Error()); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, cause
}

func accessCredentialRuntime(ref *airunwayv1alpha1.SecretKeyRef) *airunwayv1alpha1.AgentRuntimeStatus {
	if ref == nil {
		return nil
	}
	refCopy := *ref
	return &airunwayv1alpha1.AgentRuntimeStatus{AuthSecretRef: &refCopy}
}

func currentAccessCredentialRef(ad *airunwayv1alpha1.AgentDeployment) *airunwayv1alpha1.SecretKeyRef {
	if ad.Status.Runtime == nil {
		return nil
	}
	return ad.Status.Runtime.AuthSecretRef
}

func legacyAccessCredentialRef(ad *airunwayv1alpha1.AgentDeployment) *airunwayv1alpha1.SecretKeyRef {
	return &airunwayv1alpha1.SecretKeyRef{
		Name: legacyAgentAccessSecretName(ad),
		Key:  agentAccessTokenKey,
	}
}

// failWithAccessCredentialStatus reports a workload error without discarding
// the only durable pointer to the provider-created random ingress Secret. A
// later reconcile can then reuse or explicitly delete the same credential
// instead of allocating an unreachable Secret on every retry.
func (r *ContainerProviderReconciler) failWithAccessCredentialStatus(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	ref *airunwayv1alpha1.SecretKeyRef,
	reason string,
	cause error,
) (ctrl.Result, error) {
	if err := r.status(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, accessCredentialRuntime(ref), nil,
		metav1.ConditionFalse, reason, cause.Error()); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, cause
}

// failJobLedgerMigration preserves provider status while it contains durable
// state that the ConfigMap apply/ledger write has not yet replaced. For a Job,
// replacing current-generation execution evidence could authorize a rerun. For
// a long-running agent, clearing runtime.authSecretRef would lose the only
// persisted name of its random ingress Secret while both it and the Deployment
// remain live. The reconcile error still drives a retry and remains visible in
// controller logs.
func (r *ContainerProviderReconciler) failJobLedgerMigration(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	reason string,
	cause error,
) (ctrl.Result, error) {
	if ad.Spec.Lifecycle != airunwayv1alpha1.AgentLifecycleJob {
		return r.failWithAccessCredentialStatus(ctx, ad, currentAccessCredentialRef(ad), reason, cause)
	}
	if statusProvesCurrentJobGeneration(ad) || statusHasAmbiguousLegacyJobExecutionEvidence(ad) {
		return ctrl.Result{}, cause
	}
	if _, ok := terminalJobOutcomeFromStatus(ad); ok {
		return ctrl.Result{}, cause
	}
	if errors.Is(cause, errAmbiguousLegacyJobGeneration) {
		runtimeStatus, replicas := recordedJobStatus(ad)
		if err := r.status(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, runtimeStatus, replicas,
			metav1.ConditionFalse, agentJobMigrationAmbiguousReason, cause.Error()); err != nil {
			return ctrl.Result{}, fmt.Errorf("%w; preserve ambiguous legacy Job execution evidence: %v", cause, err)
		}
		return ctrl.Result{}, cause
	}
	return r.failWithStatus(ctx, ad, reason, cause)
}

func (r *ContainerProviderReconciler) terminalFailure(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	reason, message string,
) (ctrl.Result, error) {
	if ad.Spec.Lifecycle == airunwayv1alpha1.AgentLifecycleJob {
		// Invalid configuration is often transient provider-catalog state rather
		// than an AgentDeployment spec change. Before cleanup removes a one-shot
		// Job, migrate any exact-owned pre-ledger execution evidence to the
		// durable ConfigMap ledger. On failure, leave both the Job and its status
		// untouched so the next reconcile can retry without authorizing a rerun.
		releasedClaim, err := r.preflightJobLedgerBeforeBindingCleanup(ctx, ad)
		if err != nil {
			return ctrl.Result{}, err
		}
		if releasedClaim {
			return ctrl.Result{Requeue: true}, nil
		}
	}
	pending, err := r.cleanupOwnedWorkloadsForBinding(ctx, ad)
	if err != nil {
		return r.failWithAccessCredentialStatus(ctx, ad, currentAccessCredentialRef(ad), reason+"CleanupFailed", err)
	}
	result := ctrl.Result{}
	if pending {
		result.RequeueAfter = 5 * time.Second
	}
	return result, r.status(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
		metav1.ConditionFalse, reason, message)
}

func (r *ContainerProviderReconciler) modelCredentialChecksum(
	ctx context.Context,
	namespace string,
	binding airunwayv1alpha1.ModelBindingStatus,
) (string, error) {
	if binding.CredentialsRef == nil {
		return "", nil
	}
	secret := &corev1.Secret{}
	key := k8stypes.NamespacedName{Name: binding.CredentialsRef.Name, Namespace: namespace}
	if err := r.objectReader().Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("read model credential Secret %s: %w", key, err)
	}
	if !secret.DeletionTimestamp.IsZero() {
		return "", fmt.Errorf("model credential Secret %s is terminating", key)
	}
	if _, found := secret.Data[binding.CredentialsRef.Key]; !found {
		return "", fmt.Errorf("model credential Secret %s does not contain key %q", key, binding.CredentialsRef.Key)
	}
	digest := sha256.Sum256([]byte(string(secret.UID) + "\x00" + secret.ResourceVersion + "\x00" + binding.CredentialsRef.Key))
	return fmt.Sprintf("%x", digest), nil
}

// status writes provider-owned status via the shared SSA helper.
//
//nolint:dupl // Provider-specific field ownership is intentionally visible at each provider boundary.
func (r *ContainerProviderReconciler) status(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	phase airunwayv1alpha1.AgentPhase,
	rt *airunwayv1alpha1.AgentRuntimeStatus,
	replicas *airunwayv1alpha1.AgentReplicaStatus,
	providerReady metav1.ConditionStatus,
	reason, message string,
) error {
	return agentprovider.ApplyOwnedStatus(ctx, r.Client, ad, ContainerFieldOwner, phase, rt, replicas, providerReady, reason, message)
}

func (r *ContainerProviderReconciler) mapProviderConfigToAgentDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
	apc, ok := obj.(*airunwayv1alpha1.AgentProviderConfig)
	if !ok {
		return nil
	}

	agents := agentprovider.AgentsForFramework(ctx, r.Client, apc.Name)
	reqs := make([]reconcile.Request, 0, len(agents))
	for i := range agents {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&agents[i])})
	}
	return reqs
}

// mapCredentialSecretToAgentDeployments maps metadata-only Secret events to
// agents whose core-resolved binding or provider-owned auth reference names
// that Secret. The watch never caches or reads Secret data; reconciliation
// performs the authoritative Secret read before deciding whether to roll or
// tear down the workload.
//
// The controller-owner route catches deletion between creating an auth Secret
// and publishing its random name in status. The namespaced field index catches
// stable and legacy status references even if owner metadata was removed, and
// makes every Secret event an exact lookup rather than a namespace-wide scan.
func (r *ContainerProviderReconciler) mapCredentialSecretToAgentDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() == "" || obj.GetName() == "" {
		return nil
	}

	seen := make(map[k8stypes.NamespacedName]struct{})
	reqs := make([]reconcile.Request, 0, 2)
	add := func(key k8stypes.NamespacedName) {
		if key.Namespace == "" || key.Name == "" {
			return
		}
		if _, found := seen[key]; found {
			return
		}
		seen[key] = struct{}{}
		reqs = append(reqs, reconcile.Request{NamespacedName: key})
	}

	if owner := metav1.GetControllerOf(obj); owner != nil &&
		owner.APIVersion == airunwayv1alpha1.GroupVersion.String() &&
		owner.Kind == "AgentDeployment" && owner.Name != "" && owner.UID != "" &&
		owner.BlockOwnerDeletion != nil && *owner.BlockOwnerDeletion {
		add(k8stypes.NamespacedName{Namespace: obj.GetNamespace(), Name: owner.Name})
	}

	var agents airunwayv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &agents,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{agentCredentialSecretIndexKey: credentialSecretIndexValue(obj.GetNamespace(), obj.GetName())},
	); err != nil {
		log.FromContext(ctx).Error(err, "List AgentDeployments for credential Secret event",
			"namespace", obj.GetNamespace(), "secret", obj.GetName())
		return reqs
	}

	for i := range agents.Items {
		ad := &agents.Items[i]
		modelMatch := ad.Status.ModelBinding != nil && ad.Status.ModelBinding.CredentialsRef != nil &&
			ad.Status.ModelBinding.CredentialsRef.Name == obj.GetName()
		authMatch := ad.Status.Runtime != nil && ad.Status.Runtime.AuthSecretRef != nil &&
			ad.Status.Runtime.AuthSecretRef.Name == obj.GetName() &&
			ad.Status.Runtime.AuthSecretRef.Key == agentAccessTokenKey
		if modelMatch || authMatch {
			add(client.ObjectKeyFromObject(ad))
		}
	}
	return reqs
}

func credentialSecretIndexValue(namespace, name string) string {
	return namespace + "/" + name
}

func credentialSecretIndex(raw client.Object) []string {
	ad, ok := raw.(*airunwayv1alpha1.AgentDeployment)
	if !ok || ad.Namespace == "" {
		return nil
	}
	values := make(map[string]struct{}, 2)
	if ad.Status.ModelBinding != nil && ad.Status.ModelBinding.CredentialsRef != nil &&
		ad.Status.ModelBinding.CredentialsRef.Name != "" {
		values[credentialSecretIndexValue(ad.Namespace, ad.Status.ModelBinding.CredentialsRef.Name)] = struct{}{}
	}
	if ad.Status.Runtime != nil && ad.Status.Runtime.AuthSecretRef != nil &&
		ad.Status.Runtime.AuthSecretRef.Name != "" && ad.Status.Runtime.AuthSecretRef.Key == agentAccessTokenKey {
		values[credentialSecretIndexValue(ad.Namespace, ad.Status.Runtime.AuthSecretRef.Name)] = struct{}{}
	}
	indexed := make([]string, 0, len(values))
	for value := range values {
		indexed = append(indexed, value)
	}
	return indexed
}

// SetupWithManager wires the container provider and its owned workloads.
func (r *ContainerProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := agentprovider.EnsureFrameworkIndex(mgr); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&airunwayv1alpha1.AgentDeployment{},
		agentCredentialSecretIndexKey,
		credentialSecretIndex,
	); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.AgentDeployment{},
			ctrlbuilder.WithPredicates(agentprovider.ProviderAgentDeploymentRelevantChange())).
		Owns(&appsv1.Deployment{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.ConfigMap{}).
		Watches(
			&airunwayv1alpha1.AgentProviderConfig{},
			handler.EnqueueRequestsFromMapFunc(r.mapProviderConfigToAgentDeployments),
			ctrlbuilder.WithPredicates(agentprovider.ProviderConfigRelevantChange()),
		).
		WatchesMetadata(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapCredentialSecretToAgentDeployments),
		).
		Named("agent-provider-container").
		Complete(r)
}
