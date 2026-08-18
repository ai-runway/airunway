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
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
)

type failingCredentialMetadataReader struct {
	client.Reader
	target client.ObjectKey
	err    error
}

func (r failingCredentialMetadataReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if _, ok := obj.(*corev1.Secret); ok && (r.target == (client.ObjectKey{}) || r.target == key) {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

type replacingServiceAccountReader struct {
	client.Reader
	writer      client.Client
	target      client.ObjectKey
	replacement *corev1.ServiceAccount
	gets        int
	replaced    bool
}

type failingJobCreateClient struct {
	client.Client
	err error
}

type rejectingJobCreateAndFailingReleaseClient struct {
	client.Client
	createErr        error
	releaseErr       error
	nonmatchingJob   *batchv1.Job
	rejectedCreate   bool
	sawRetryableMark bool
	failedRelease    bool
}

type rejectingJobCreateAndConflictingMarkerClient struct {
	client.Client
	createErr        error
	rejectedCreate   bool
	conflictedMarker bool
}

type failingConfigMapPatchClient struct {
	client.Client
	patchType types.PatchType
	err       error
	failed    bool
}

type failingPublishedReservationClearClient struct {
	client.Client
	err    error
	failed bool
}

type failingWorkloadApplyClient struct {
	client.Client
	target   string
	err      error
	attempts int
}

type credentialDriftDeploymentApplyClient struct {
	client.Client
	err     error
	drifted bool
}

type foreignDeploymentRaceClient struct {
	client.Client
	foreign                 *appsv1.Deployment
	foreignUID              types.UID
	serviceKey              client.ObjectKey
	raced                   bool
	serviceSeen             bool
	serviceSelectorWasEmpty bool
}

type failingServiceActivationClient struct {
	client.Client
	err                    error
	inertServiceApplied    bool
	deploymentCreated      bool
	serviceActivationTried bool
}

type failingSecretDeleteClient struct {
	client.Client
	target   client.ObjectKey
	err      error
	attempts int
}

type failingServiceAccountCreateClient struct {
	client.Client
	err      error
	attempts int
}

type failingCredentialPublishClient struct {
	client.Client
	statusErr    error
	deleteErr    error
	failedStatus bool
	failedDelete bool
}

type failingCredentialStatusWriter struct {
	client.SubResourceWriter
	parent *failingCredentialPublishClient
}

type commitThenErrorSecretCreateClient struct {
	client.Client
	err       error
	committed bool
}

type commitThenErrorJobClaimClient struct {
	client.Client
	err        error
	committed  bool
	jobCreates int
}

type lateCommitAfterNotFoundSecretClient struct {
	client.Client
	err              error
	pending          *corev1.Secret
	returnedNotFound bool
	committed        bool
}

type recordingSecretCreateDeadlineClient struct {
	client.Client
	sawDeadline bool
}

func (c failingJobCreateClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	if _, ok := obj.(*batchv1.Job); ok {
		return c.err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *commitThenErrorSecretCreateClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	secret, ok := obj.(*corev1.Secret)
	if !ok || c.committed || len(secret.Data[agentAccessTokenKey]) == 0 {
		return c.Client.Create(ctx, obj, opts...)
	}
	if err := c.Client.Create(ctx, obj, opts...); err != nil {
		return err
	}
	c.committed = true
	return c.err
}

func (c *commitThenErrorJobClaimClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	if _, ok := obj.(*batchv1.Job); ok {
		c.jobCreates++
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *commitThenErrorJobClaimClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	configMap, ok := obj.(*corev1.ConfigMap)
	if !ok || c.committed || patch.Type() != types.MergePatchType ||
		configMap.Annotations[agentJobGenerationAnnotation] == "" ||
		configMap.Annotations[agentJobOutcomeAnnotation] != "" ||
		configMap.Annotations[agentJobClaimNonceAnnotation] == "" {
		return c.Client.Patch(ctx, obj, patch, opts...)
	}
	if err := c.Client.Patch(ctx, obj, patch, opts...); err != nil {
		return err
	}
	c.committed = true
	return c.err
}

func (c *lateCommitAfterNotFoundSecretClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	secret, ok := obj.(*corev1.Secret)
	if !ok || c.pending != nil || len(secret.Data[agentAccessTokenKey]) == 0 {
		return c.Client.Create(ctx, obj, opts...)
	}
	c.pending = secret.DeepCopy()
	return c.err
}

func (c *lateCommitAfterNotFoundSecretClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if _, ok := obj.(*corev1.Secret); ok && c.pending != nil &&
		key == client.ObjectKeyFromObject(c.pending) && !c.returnedNotFound {
		c.returnedNotFound = true
		if err := c.Client.Create(ctx, c.pending.DeepCopy()); err != nil {
			return fmt.Errorf("commit delayed Secret after simulated NotFound: %w", err)
		}
		c.committed = true
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *recordingSecretCreateDeadlineClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	if secret, ok := obj.(*corev1.Secret); ok && len(secret.Data[agentAccessTokenKey]) > 0 {
		_, c.sawDeadline = ctx.Deadline()
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *rejectingJobCreateAndFailingReleaseClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	if _, ok := obj.(*batchv1.Job); ok && !c.rejectedCreate {
		c.rejectedCreate = true
		if c.nonmatchingJob != nil {
			if err := c.Client.Create(ctx, c.nonmatchingJob.DeepCopy()); err != nil {
				return fmt.Errorf("create injected nonmatching Job: %w", err)
			}
		}
		return c.createErr
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *rejectingJobCreateAndFailingReleaseClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	if cm, ok := obj.(*corev1.ConfigMap); ok && patch.Type() == types.MergePatchType {
		if cm.Annotations[agentJobOutcomeAnnotation] == agentJobOutcomeRetryable {
			c.sawRetryableMark = true
		}
		if c.sawRetryableMark && !c.failedRelease &&
			cm.Annotations[agentJobGenerationAnnotation] == "" &&
			cm.Annotations[agentJobOutcomeAnnotation] == "" {
			c.failedRelease = true
			return c.releaseErr
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *rejectingJobCreateAndConflictingMarkerClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	if _, ok := obj.(*batchv1.Job); ok && !c.rejectedCreate {
		c.rejectedCreate = true
		return c.createErr
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *rejectingJobCreateAndConflictingMarkerClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	if cm, ok := obj.(*corev1.ConfigMap); ok && patch.Type() == types.MergePatchType &&
		cm.Annotations[agentJobOutcomeAnnotation] == agentJobOutcomeRetryable && !c.conflictedMarker {
		c.conflictedMarker = true
		return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name,
			fmt.Errorf("injected retryable marker conflict"))
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *failingConfigMapPatchClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	if _, ok := obj.(*corev1.ConfigMap); ok && patch.Type() == c.patchType && !c.failed {
		c.failed = true
		return c.err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *failingPublishedReservationClearClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	configMap, ok := obj.(*corev1.ConfigMap)
	if ok && !c.failed && patch.Type() == types.MergePatchType &&
		configMap.Annotations[agentAccessPendingAnnotation] == "" &&
		configMap.Annotations[agentAccessCreateStartedAnnotation] == "" {
		c.failed = true
		return c.err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *failingWorkloadApplyClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	if c.matches(obj) {
		c.attempts++
		return c.err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *foreignDeploymentRaceClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	deployment, ok := obj.(*appsv1.Deployment)
	if !ok || c.raced || client.ObjectKeyFromObject(deployment) != client.ObjectKeyFromObject(c.foreign) {
		return c.Client.Create(ctx, obj, opts...)
	}
	c.raced = true
	service := &corev1.Service{}
	if err := c.Client.Get(ctx, c.serviceKey, service); err == nil {
		c.serviceSeen = true
		c.serviceSelectorWasEmpty = len(service.Spec.Selector) == 0
	}
	created := c.foreign.DeepCopy()
	if err := c.Client.Create(ctx, created); err != nil {
		return fmt.Errorf("create injected foreign Deployment: %w", err)
	}
	c.foreignUID = created.UID
	return c.Client.Create(ctx, obj, opts...)
}

func (c *failingServiceActivationClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	switch typed := obj.(type) {
	case *corev1.Service:
		if len(typed.Spec.Selector) == 0 {
			if err := c.Client.Create(ctx, obj, opts...); err != nil {
				return err
			}
			c.inertServiceApplied = true
			return nil
		}
	case *appsv1.Deployment:
		if c.inertServiceApplied {
			typed.Finalizers = append(typed.Finalizers, "test.airunway.ai/hold-service-activation-deployment")
			if err := c.Client.Create(ctx, obj, opts...); err != nil {
				return err
			}
			c.deploymentCreated = true
			return nil
		}
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *failingServiceActivationClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	service, ok := obj.(*corev1.Service)
	if ok && patch.Type() == types.ApplyPatchType && len(service.Spec.Selector) > 0 &&
		c.inertServiceApplied && c.deploymentCreated && !c.serviceActivationTried {
		c.serviceActivationTried = true
		return c.err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *failingWorkloadApplyClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	if patch.Type() == types.ApplyPatchType && c.matches(obj) {
		c.attempts++
		return c.err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *credentialDriftDeploymentApplyClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	desired, ok := obj.(*appsv1.Deployment)
	if !ok || c.drifted || patch.Type() != types.ApplyPatchType {
		return c.Client.Patch(ctx, obj, patch, opts...)
	}

	var live appsv1.Deployment
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(desired), &live); err != nil {
		return err
	}
	if live.Spec.Template.Annotations == nil {
		live.Spec.Template.Annotations = map[string]string{}
	}
	live.Spec.Template.Annotations[agentAccessChecksumAnnotation] = "injected-stale-credential-checksum"
	live.Finalizers = append(live.Finalizers, "test.airunway.ai/hold-drifted-deployment")
	if err := c.Client.Update(ctx, &live); err != nil {
		return fmt.Errorf("inject Deployment credential drift: %w", err)
	}
	c.drifted = true
	return c.err
}

func (c *failingWorkloadApplyClient) matches(obj client.Object) bool {
	switch c.target {
	case "Deployment":
		_, ok := obj.(*appsv1.Deployment)
		return ok
	case "Service":
		_, ok := obj.(*corev1.Service)
		return ok
	default:
		return false
	}
}

func (c *failingSecretDeleteClient) Delete(
	ctx context.Context,
	obj client.Object,
	opts ...client.DeleteOption,
) error {
	if _, ok := obj.(*corev1.Secret); ok && client.ObjectKeyFromObject(obj) == c.target {
		c.attempts++
		return c.err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *failingServiceAccountCreateClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	if _, ok := obj.(*corev1.ServiceAccount); ok {
		c.attempts++
		return c.err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *failingCredentialPublishClient) Status() client.SubResourceWriter {
	return &failingCredentialStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

func (c *failingCredentialPublishClient) Delete(
	ctx context.Context,
	obj client.Object,
	opts ...client.DeleteOption,
) error {
	if _, ok := obj.(*corev1.Secret); ok && !c.failedDelete && c.deleteErr != nil {
		c.failedDelete = true
		return c.deleteErr
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (w *failingCredentialStatusWriter) Create(
	ctx context.Context,
	obj client.Object,
	subResource client.Object,
	opts ...client.SubResourceCreateOption,
) error {
	return w.SubResourceWriter.Create(ctx, obj, subResource, opts...)
}

func (w *failingCredentialStatusWriter) Update(
	ctx context.Context,
	obj client.Object,
	opts ...client.SubResourceUpdateOption,
) error {
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w *failingCredentialStatusWriter) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.SubResourcePatchOption,
) error {
	if _, ok := obj.(*airunwayv1alpha1.AgentDeployment); ok && !w.parent.failedStatus {
		w.parent.failedStatus = true
		return w.parent.statusErr
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func (w *failingCredentialStatusWriter) Apply(
	ctx context.Context,
	obj runtime.ApplyConfiguration,
	opts ...client.SubResourceApplyOption,
) error {
	return w.SubResourceWriter.Apply(ctx, obj, opts...)
}

func (r *replacingServiceAccountReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if _, ok := obj.(*corev1.ServiceAccount); ok && key == r.target {
		r.gets++
		if r.gets == 2 && !r.replaced {
			var current corev1.ServiceAccount
			if err := r.Reader.Get(ctx, key, &current); err != nil {
				return err
			}
			if err := r.writer.Delete(ctx, &current); err != nil {
				return err
			}

			deadline := time.Now().Add(2 * time.Second)
			for {
				var probe corev1.ServiceAccount
				err := r.Reader.Get(ctx, key, &probe)
				if apierrors.IsNotFound(err) {
					break
				}
				if err != nil {
					return err
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("timed out replacing ServiceAccount %s", key)
				}
				time.Sleep(10 * time.Millisecond)
			}

			replacement := r.replacement.DeepCopy()
			replacement.ResourceVersion = ""
			replacement.UID = ""
			replacement.CreationTimestamp = metav1.Time{}
			replacement.ManagedFields = nil
			if err := r.writer.Create(ctx, replacement); err != nil {
				return err
			}
			r.replaced = true
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// --- Pure render-function unit tests (no cluster) --------------------------

func containerAD(name string, cfg containerConfig, extra map[string]any) *airunwayv1alpha1.AgentDeployment {
	merged := map[string]any{}
	if cfg.Image != "" {
		merged["image"] = cfg.Image
	}
	for k, v := range extra {
		merged[k] = v
	}
	raw, _ := json.Marshal(merged)

	ad := &airunwayv1alpha1.AgentDeployment{}
	ad.Name = name
	ad.Namespace = "default"
	ad.Spec.Framework.Name = "crewai"
	ad.Spec.Config = &runtime.RawExtension{Raw: raw}
	return ad
}

func TestRenderAgentConfigMap(t *testing.T) {
	ad := containerAD("research", containerConfig{Image: "img:1"}, map[string]any{"systemPrompt": "be brief"})
	cm := renderAgentConfigMap(ad)

	if cm.Name != "research-config" {
		t.Errorf("name = %q, want research-config", cm.Name)
	}
	payload, ok := cm.Data[agentConfigFileName]
	if !ok {
		t.Fatalf("configmap missing %q key", agentConfigFileName)
	}
	// The full spec.config is mounted verbatim so the BYO image reads its
	// framework config from the pinned path.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("agent.json not valid JSON: %v", err)
	}
	if parsed["systemPrompt"] != "be brief" {
		t.Errorf("agent.json missing systemPrompt passthrough: %v", parsed)
	}
}

func TestRenderAgentDeployment_SecurityAndEnv(t *testing.T) {
	ad := containerAD("research", containerConfig{Image: "ghcr.io/x/crewai:poc"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{
		BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
		CredentialsRef: &airunwayv1alpha1.SecretKeyRef{Name: "openai-api-key", Key: "api-key"},
	}
	authRef := &airunwayv1alpha1.SecretKeyRef{Name: "research-api-auth", Key: "token"}
	dep := renderAgentDeployment(ad, renderInputs{
		cfg: containerConfig{Image: "ghcr.io/x/crewai:poc"}, binding: binding,
		configMapName: "research-config", serviceAccountName: "research-runtime",
		modelCredentialChecksum: "model-secret-hash", authSecretRef: authRef,
		accessTokenHash: "access-hash", writableRoot: false, securityOverrides: nil,
	})

	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "ghcr.io/x/crewai:poc" {
		t.Errorf("image = %q", c.Image)
	}

	// Provider-owned hardening (design §7): runAsNonRoot + seccomp at pod level.
	pod := dep.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("pod securityContext.runAsNonRoot must be true (provider-owned hardening)")
	}
	if pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("pod seccompProfile must be RuntimeDefault")
	}
	// The image is author-chosen, so the pod must not carry the namespace's
	// default ServiceAccount token: that would let someone who can create an
	// AgentDeployment, but not a Pod, act as that ServiceAccount.
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be explicitly false — an author-chosen image must not inherit the default ServiceAccount token")
	}
	if pod.ServiceAccountName != "research-runtime" {
		t.Errorf("serviceAccountName = %q, want dedicated research-runtime", pod.ServiceAccountName)
	}
	// Container: drop ALL caps, no privilege escalation, read-only root by default.
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must default to true")
	}
	if len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities must drop ALL, got %v", c.SecurityContext.Capabilities.Drop)
	}

	// Model binding injected as OpenAI-compatible env.
	env := map[string]string{}
	var apiKeyFromSecret, accessTokenFromSecret bool
	for _, e := range c.Env {
		env[e.Name] = e.Value
		if e.Name == "OPENAI_API_KEY" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			apiKeyFromSecret = true
			if e.ValueFrom.SecretKeyRef.Name != "openai-api-key" {
				t.Errorf("OPENAI_API_KEY secret = %q", e.ValueFrom.SecretKeyRef.Name)
			}
		}
		if e.Name == agentAccessTokenEnv && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			accessTokenFromSecret = true
			if e.ValueFrom.SecretKeyRef.Name != authRef.Name || e.ValueFrom.SecretKeyRef.Key != authRef.Key {
				t.Errorf("%s secret ref = %+v, want %+v", agentAccessTokenEnv, e.ValueFrom.SecretKeyRef, authRef)
			}
		}
	}
	if env["OPENAI_BASE_URL"] != "https://api.openai.com/v1" {
		t.Errorf("OPENAI_BASE_URL = %q", env["OPENAI_BASE_URL"])
	}
	if env["AIRUNWAY_AGENT_CONFIG"] != agentConfigMountPath {
		t.Errorf("AIRUNWAY_AGENT_CONFIG = %q, want %q", env["AIRUNWAY_AGENT_CONFIG"], agentConfigMountPath)
	}
	if env["AIRUNWAY_AGENT_MODE"] != "server" {
		t.Errorf("AIRUNWAY_AGENT_MODE = %q, want server", env["AIRUNWAY_AGENT_MODE"])
	}
	if env["AIRUNWAY_AGENT_PORT"] != "8080" {
		t.Errorf("AIRUNWAY_AGENT_PORT = %q, want 8080", env["AIRUNWAY_AGENT_PORT"])
	}
	if c.StartupProbe == nil || c.StartupProbe.TCPSocket == nil || c.StartupProbe.TCPSocket.Port.IntValue() != 8080 {
		t.Errorf("startup probe = %+v, want TCP :8080", c.StartupProbe)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil || c.ReadinessProbe.TCPSocket.Port.IntValue() != 8080 {
		t.Errorf("readiness probe = %+v, want TCP :8080", c.ReadinessProbe)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.TCPSocket == nil || c.LivenessProbe.TCPSocket.Port.IntValue() != 8080 {
		t.Errorf("liveness probe = %+v, want TCP :8080", c.LivenessProbe)
	}
	if !apiKeyFromSecret {
		t.Error("OPENAI_API_KEY must be sourced from the binding secret")
	}
	if !accessTokenFromSecret {
		t.Errorf("%s must be sourced from the provider-managed access Secret", agentAccessTokenEnv)
	}
	if got := dep.Spec.Template.Annotations[agentAccessChecksumAnnotation]; got != "access-hash" {
		t.Errorf("access token checksum = %q, want access-hash", got)
	}
	if got := dep.Spec.Template.Annotations[agentModelCredentialChecksumAnnotation]; got != "model-secret-hash" {
		t.Errorf("model credential checksum = %q, want model-secret-hash", got)
	}
}

func TestResolveCatalogImageVersion(t *testing.T) {
	tests := []struct {
		name            string
		providerVersion string
		wantTag         string
	}{
		{name: "release", providerVersion: "v0.8.0", wantTag: "0.8.0"},
		{name: "prerelease", providerVersion: "v0.8.0-dev.1", wantTag: "0.8.0-dev.1"},
		{name: "build metadata", providerVersion: "v0.8.0+build.4", wantTag: "0.8.0-build.4"},
		{name: "main", providerVersion: "main", wantTag: "latest"},
		{name: "main revision", providerVersion: "main-deadbeefcafe", wantTag: "main-deadbeefcafe"},
		{name: "local development", providerVersion: "dev-deadbee-dirty", wantTag: "latest"},
		{name: "already normalized", providerVersion: "0.8.0", wantTag: "0.8.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveCatalogImageVersion(
				"ghcr.io/ai-runway/agent:${AIRUNWAY_VERSION}",
				"agent-container-provider:"+tc.providerVersion,
			)
			if err != nil {
				t.Fatalf("resolveCatalogImageVersion: %v", err)
			}
			want := "ghcr.io/ai-runway/agent:" + tc.wantTag
			if got != want {
				t.Fatalf("resolved image = %q, want %q", got, want)
			}
		})
	}
	if _, err := resolveCatalogImageVersion("ghcr.io/ai-runway/agent:${AIRUNWAY_VERSION}", ""); err == nil {
		t.Fatal("missing provider version must reject a versioned catalog image")
	}
}

func TestMapCredentialSecretToAgentDeploymentsUsesNamespacedIndex(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add AgentDeployment scheme: %v", err)
	}

	modelAgent := func(name, namespace, secretName string) *airunwayv1alpha1.AgentDeployment {
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		}
		if secretName != "" {
			ad.Status.ModelBinding = &airunwayv1alpha1.ModelBindingStatus{
				CredentialsRef: &airunwayv1alpha1.SecretKeyRef{Name: secretName, Key: "api-key"},
			}
		}
		return ad
	}
	authAgent := func(name, namespace, secretName string, uid types.UID) *airunwayv1alpha1.AgentDeployment {
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: uid},
		}
		if secretName != "" {
			ad.Status.Runtime = &airunwayv1alpha1.AgentRuntimeStatus{
				AuthSecretRef: &airunwayv1alpha1.SecretKeyRef{Name: secretName, Key: agentAccessTokenKey},
			}
		}
		return ad
	}
	auth := authAgent("auth-match", "tenant-a", "agent-auth", types.UID("auth-agent-uid"))
	ownerOnly := authAgent("owner-before-status", "tenant-a", "", types.UID("owner-agent-uid"))

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			modelAgent("model-match", "tenant-a", "model-key"),
			modelAgent("other-secret", "tenant-a", "other-key"),
			modelAgent("other-namespace", "tenant-b", "model-key"),
			modelAgent("no-credential", "tenant-a", ""),
			auth,
			ownerOnly,
		).
		WithIndex(&airunwayv1alpha1.AgentDeployment{}, agentCredentialSecretIndexKey, credentialSecretIndex).
		Build()

	r := &ContainerProviderReconciler{Client: c}
	assertMapped := func(obj client.Object, want ...types.NamespacedName) {
		t.Helper()
		reqs := r.mapCredentialSecretToAgentDeployments(context.Background(), obj)
		if len(reqs) != len(want) {
			t.Fatalf("Secret mapped to %#v, want %#v", reqs, want)
		}
		got := make(map[types.NamespacedName]struct{}, len(reqs))
		for _, req := range reqs {
			got[req.NamespacedName] = struct{}{}
		}
		for _, key := range want {
			if _, found := got[key]; !found {
				t.Fatalf("Secret mapped to %#v, missing %s", reqs, key)
			}
		}
	}

	assertMapped(&metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{Name: "model-key", Namespace: "tenant-a"},
	}, types.NamespacedName{Name: "model-match", Namespace: "tenant-a"})

	// The status index remains sufficient when deletion metadata has lost its
	// owner reference (for example after an administrator repaired metadata).
	assertMapped(&metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-auth", Namespace: "tenant-a"},
	}, types.NamespacedName{Name: auth.Name, Namespace: auth.Namespace})

	// An owner event catches deletion before the random Secret name is published
	// in status. When status is already present, the owner and index paths dedupe.
	assertMapped(&metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name: "agent-auth", Namespace: "tenant-a",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: airunwayv1alpha1.GroupVersion.String(), Kind: "AgentDeployment",
			Name: auth.Name, UID: auth.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
		}},
	}}, types.NamespacedName{Name: auth.Name, Namespace: auth.Namespace})
	assertMapped(&metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name: "unpublished-auth", Namespace: "tenant-a",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: airunwayv1alpha1.GroupVersion.String(), Kind: "AgentDeployment",
			Name: ownerOnly.Name, UID: ownerOnly.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
		}},
	}}, types.NamespacedName{Name: ownerOnly.Name, Namespace: ownerOnly.Namespace})
}

func TestValidatedAgentAccessTokenRequiresBlockingBoundOwner(t *testing.T) {
	ad := containerAD("research", containerConfig{Image: "img:1"}, nil)
	ad.UID = types.UID("agent-uid")
	token := []byte(base64.RawURLEncoding.EncodeToString(make([]byte, agentAccessTokenBytes)))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentAccessSecretName(ad, token),
			Namespace: ad.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: airunwayv1alpha1.GroupVersion.String(),
				Kind:       "AgentDeployment",
				Name:       ad.Name,
				UID:        ad.UID,
				Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false),
			}},
		},
		Data: map[string][]byte{agentAccessTokenKey: token},
	}
	if _, err := validatedAgentAccessToken(secret, ad); err == nil {
		t.Fatal("a forgeable non-blocking owner reference must not authorize an access Secret")
	}
	secret.OwnerReferences[0].BlockOwnerDeletion = ptr.To(true)
	if _, err := validatedAgentAccessToken(secret, ad); err != nil {
		t.Fatalf("valid bound access Secret rejected: %v", err)
	}
	now := metav1.Now()
	secret.DeletionTimestamp = &now
	if _, err := validatedAgentAccessToken(secret, ad); err == nil {
		t.Fatal("a terminating access Secret must not remain active")
	}
}

func TestEnsureAgentAccessCredentialsRecoversCommittedCreateError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	createErr := fmt.Errorf("injected timeout after Secret commit")
	clientWithLostResponse := &commitThenErrorSecretCreateClient{Client: base, err: createErr}
	r := &ContainerProviderReconciler{
		Client:    clientWithLostResponse,
		APIReader: base,
		Scheme:    scheme,
	}
	ad := containerAD("ambiguous-secret-create", containerConfig{Image: "img:1"}, nil)
	ad.UID = types.UID("ambiguous-secret-create-uid")
	configMap := renderAgentConfigMap(ad)
	if err := controllerutil.SetControllerReference(ad, configMap, scheme); err != nil {
		t.Fatal(err)
	}
	if err := base.Create(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}

	ref, _, created, err := r.ensureAgentAccessCredentials(context.Background(), ad)
	if err != nil {
		t.Fatalf("committed Secret was not recovered after an ambiguous create error: %v", err)
	}
	if !created || !clientWithLostResponse.committed {
		t.Fatalf("created=%v committed=%v, want the committed Secret published to status", created, clientWithLostResponse.committed)
	}
	if ref == nil || ref.Key != agentAccessTokenKey {
		t.Fatalf("recovered access reference = %#v", ref)
	}
	var secrets corev1.SecretList
	if err := base.List(context.Background(), &secrets, client.InNamespace(ad.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 1 {
		t.Fatalf("ambiguous create left %d Secrets, want exactly the committed Secret", len(secrets.Items))
	}
	if secrets.Items[0].Name != ref.Name {
		t.Fatalf("recovered Secret = %s, status reference = %s", secrets.Items[0].Name, ref.Name)
	}
	if _, err := validatedAgentAccessToken(&secrets.Items[0], ad); err != nil {
		t.Fatalf("recovered Secret is not valid: %v", err)
	}
}

func TestEnsureAgentAccessCredentialsKeepsReservationAcrossLateCreateCommit(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	createErr := fmt.Errorf("injected timeout before delayed Secret commit")
	clientWithLateCommit := &lateCommitAfterNotFoundSecretClient{Client: base, err: createErr}
	r := &ContainerProviderReconciler{
		Client:    clientWithLateCommit,
		APIReader: clientWithLateCommit,
		Scheme:    scheme,
	}
	ad := containerAD("late-secret-create", containerConfig{Image: "img:1"}, nil)
	ad.UID = types.UID("late-secret-create-uid")
	configMap := renderAgentConfigMap(ad)
	if err := controllerutil.SetControllerReference(ad, configMap, scheme); err != nil {
		t.Fatal(err)
	}
	if err := base.Create(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := r.ensureAgentAccessCredentials(context.Background(), ad); err == nil {
		t.Fatal("ambiguous create followed by NotFound must remain retryable")
	}
	if !clientWithLateCommit.returnedNotFound || !clientWithLateCommit.committed {
		t.Fatalf("returnedNotFound=%v committed=%v, want a commit immediately after the first reread",
			clientWithLateCommit.returnedNotFound, clientWithLateCommit.committed)
	}
	var reserved corev1.ConfigMap
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(configMap), &reserved); err != nil {
		t.Fatal(err)
	}
	reservedName := reserved.Annotations[agentAccessPendingAnnotation]
	if reservedName == "" {
		t.Fatal("ambiguous create cleared the only random Secret reservation")
	}
	if got := reserved.Annotations[agentAccessCreateStartedAnnotation]; got != reservedName {
		t.Fatalf("create-started marker = %q, want reserved name %q", got, reservedName)
	}

	ref, _, created, err := r.ensureAgentAccessCredentials(context.Background(), ad)
	if err != nil {
		t.Fatalf("late committed Secret was not recovered on retry: %v", err)
	}
	if !created || ref == nil || ref.Name != reservedName {
		t.Fatalf("created=%v ref=%#v, want recovered reservation %q", created, ref, reservedName)
	}
	var secrets corev1.SecretList
	if err := base.List(context.Background(), &secrets, client.InNamespace(ad.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 1 || secrets.Items[0].Name != reservedName {
		t.Fatalf("late create left Secrets %#v, want only %q", secrets.Items, reservedName)
	}
}

func TestEnsureAgentAccessCredentialsWaitsForFreshAmbiguousReservation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	grace := 2 * time.Minute
	ad := containerAD("fresh-secret-reservation", containerConfig{Image: "img:1"}, nil)
	ad.UID = types.UID("fresh-secret-reservation-uid")
	token := []byte(base64.RawURLEncoding.EncodeToString(make([]byte, agentAccessTokenBytes)))
	reservedName := agentAccessSecretName(ad, token)
	startedAt := now.Add(-time.Minute).Format(time.RFC3339Nano)
	configMap := renderAgentConfigMap(ad)
	configMap.Annotations = map[string]string{
		agentAccessPendingAnnotation:         reservedName,
		agentAccessCreateStartedAnnotation:   reservedName,
		agentAccessCreateStartedAtAnnotation: startedAt,
	}
	if err := controllerutil.SetControllerReference(ad, configMap, scheme); err != nil {
		t.Fatal(err)
	}
	if err := base.Create(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}
	r := &ContainerProviderReconciler{
		Client:                    base,
		APIReader:                 base,
		Scheme:                    scheme,
		agentAccessNow:            func() time.Time { return now },
		agentAccessAmbiguityGrace: grace,
	}

	for range 2 {
		if _, _, _, err := r.ensureAgentAccessCredentials(context.Background(), ad); err == nil ||
			!strings.Contains(err.Error(), "remains ambiguous") {
			t.Fatalf("fresh ambiguous reservation error = %v, want remains ambiguous", err)
		}
	}
	var secrets corev1.SecretList
	if err := base.List(context.Background(), &secrets, client.InNamespace(ad.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 0 {
		t.Fatalf("fresh ambiguous reservation created %d Secrets, want none", len(secrets.Items))
	}
	var current corev1.ConfigMap
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(configMap), &current); err != nil {
		t.Fatal(err)
	}
	if got := current.Annotations[agentAccessPendingAnnotation]; got != reservedName {
		t.Fatalf("pending reservation = %q, want %q", got, reservedName)
	}
	if got := current.Annotations[agentAccessCreateStartedAtAnnotation]; got != startedAt {
		t.Fatalf("fresh reservation timestamp = %q, want unchanged %q", got, startedAt)
	}
}

func TestCleanupPreservesFreshAmbiguousAccessReservation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	ad := containerAD("cleanup-ambiguous-reservation", containerConfig{Image: "img:1"}, nil)
	ad.UID = types.UID("cleanup-ambiguous-reservation-uid")
	token := []byte(base64.RawURLEncoding.EncodeToString(make([]byte, agentAccessTokenBytes)))
	reservedName := agentAccessSecretName(ad, token)
	configMap := renderAgentConfigMap(ad)
	configMap.Annotations = map[string]string{
		agentAccessPendingAnnotation:         reservedName,
		agentAccessCreateStartedAnnotation:   reservedName,
		agentAccessCreateStartedAtAnnotation: now.Format(time.RFC3339Nano),
	}
	if err := controllerutil.SetControllerReference(ad, configMap, scheme); err != nil {
		t.Fatal(err)
	}
	if err := base.Create(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}
	r := &ContainerProviderReconciler{
		Client:                    base,
		APIReader:                 base,
		Scheme:                    scheme,
		agentAccessNow:            func() time.Time { return now },
		agentAccessAmbiguityGrace: 2 * time.Minute,
	}

	if _, err := r.cleanupOwnedWorkloads(context.Background(), ad); err == nil ||
		!strings.Contains(err.Error(), "remains ambiguous") {
		t.Fatalf("cleanup error = %v, want fresh ambiguity to block ConfigMap deletion", err)
	}
	var current corev1.ConfigMap
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(configMap), &current); err != nil {
		t.Fatal(err)
	}
	if !current.DeletionTimestamp.IsZero() {
		t.Fatal("fresh ambiguous reservation ConfigMap entered deletion")
	}
	if got := current.Annotations[agentAccessPendingAnnotation]; got != reservedName {
		t.Fatalf("pending reservation = %q, want %q", got, reservedName)
	}
}

func TestEnsureAgentAccessCredentialsRetriesExpiredPreCreateCrashReservation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	deadlineClient := &recordingSecretCreateDeadlineClient{Client: base}
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	grace := 2 * time.Minute
	ad := containerAD("expired-secret-reservation", containerConfig{Image: "img:1"}, nil)
	ad.UID = types.UID("expired-secret-reservation-uid")
	lostToken := []byte(base64.RawURLEncoding.EncodeToString(make([]byte, agentAccessTokenBytes)))
	staleName := agentAccessSecretName(ad, lostToken)
	configMap := renderAgentConfigMap(ad)
	configMap.Annotations = map[string]string{
		agentAccessPendingAnnotation:         staleName,
		agentAccessCreateStartedAnnotation:   staleName,
		agentAccessCreateStartedAtAnnotation: now.Add(-grace - time.Second).Format(time.RFC3339Nano),
	}
	if err := controllerutil.SetControllerReference(ad, configMap, scheme); err != nil {
		t.Fatal(err)
	}
	if err := base.Create(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}
	r := &ContainerProviderReconciler{
		Client:                    deadlineClient,
		APIReader:                 base,
		Scheme:                    scheme,
		agentAccessNow:            func() time.Time { return now },
		agentAccessCreateTimeout:  50 * time.Millisecond,
		agentAccessAmbiguityGrace: grace,
	}

	ref, _, created, err := r.ensureAgentAccessCredentials(context.Background(), ad)
	if err != nil {
		t.Fatalf("expired pre-Create reservation was not retried: %v", err)
	}
	if !created || ref == nil || ref.Name == staleName {
		t.Fatalf("created=%v ref=%#v, want a fresh access Secret after expired reservation %q", created, ref, staleName)
	}
	if !deadlineClient.sawDeadline {
		t.Fatal("agent access Secret Create did not receive a bounded context deadline")
	}

	secondRef, _, recovered, err := r.ensureAgentAccessCredentials(context.Background(), ad)
	if err != nil {
		t.Fatalf("created access Secret was not recoverable: %v", err)
	}
	if !recovered || secondRef == nil || secondRef.Name != ref.Name {
		t.Fatalf("recovered=%v ref=%#v, want the first replacement %q", recovered, secondRef, ref.Name)
	}
	var secrets corev1.SecretList
	if err := base.List(context.Background(), &secrets, client.InNamespace(ad.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 1 || secrets.Items[0].Name != ref.Name {
		t.Fatalf("expired reservation recovery left Secrets %#v, want only %q", secrets.Items, ref.Name)
	}
	if _, err := validatedAgentAccessToken(&secrets.Items[0], ad); err != nil {
		t.Fatalf("replacement access Secret is invalid: %v", err)
	}
}

func TestEnsureAgentAccessCredentialsTimestampsLegacyReservationBeforeRetry(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	grace := 2 * time.Minute
	ad := containerAD("legacy-secret-reservation", containerConfig{Image: "img:1"}, nil)
	ad.UID = types.UID("legacy-secret-reservation-uid")
	lostToken := []byte(base64.RawURLEncoding.EncodeToString(make([]byte, agentAccessTokenBytes)))
	staleName := agentAccessSecretName(ad, lostToken)
	configMap := renderAgentConfigMap(ad)
	configMap.Annotations = map[string]string{
		agentAccessPendingAnnotation:       staleName,
		agentAccessCreateStartedAnnotation: staleName,
	}
	if err := controllerutil.SetControllerReference(ad, configMap, scheme); err != nil {
		t.Fatal(err)
	}
	if err := base.Create(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}
	r := &ContainerProviderReconciler{
		Client:                    base,
		APIReader:                 base,
		Scheme:                    scheme,
		agentAccessNow:            func() time.Time { return now },
		agentAccessAmbiguityGrace: grace,
	}

	if _, _, _, err := r.ensureAgentAccessCredentials(context.Background(), ad); err == nil ||
		!strings.Contains(err.Error(), "timestamped the legacy reservation") {
		t.Fatalf("legacy reservation error = %v, want observation timestamp wait", err)
	}
	var observed corev1.ConfigMap
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(configMap), &observed); err != nil {
		t.Fatal(err)
	}
	if got := observed.Annotations[agentAccessCreateStartedAtAnnotation]; got != now.Format(time.RFC3339Nano) {
		t.Fatalf("legacy reservation timestamp = %q, want %q", got, now.Format(time.RFC3339Nano))
	}
	var beforeGrace corev1.SecretList
	if err := base.List(context.Background(), &beforeGrace, client.InNamespace(ad.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(beforeGrace.Items) != 0 {
		t.Fatalf("legacy reservation migration created %d Secrets before its grace elapsed", len(beforeGrace.Items))
	}

	now = now.Add(grace + time.Second)
	ref, _, created, err := r.ensureAgentAccessCredentials(context.Background(), ad)
	if err != nil {
		t.Fatalf("expired migrated legacy reservation was not retried: %v", err)
	}
	if !created || ref == nil || ref.Name == staleName {
		t.Fatalf("created=%v ref=%#v, want a fresh Secret after legacy reservation expiry", created, ref)
	}
	var afterGrace corev1.SecretList
	if err := base.List(context.Background(), &afterGrace, client.InNamespace(ad.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(afterGrace.Items) != 1 || afterGrace.Items[0].Name != ref.Name {
		t.Fatalf("legacy reservation retry left Secrets %#v, want only %q", afterGrace.Items, ref.Name)
	}
}

func TestRenderAgentDeployment_WritableRootForFramework(t *testing.T) {
	ad := containerAD("openclaw", containerConfig{Image: "img:1"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	// writableRoot is a provider-owned decision passed by the reconciler, not a
	// user-facing spec.config field.
	dep := renderAgentDeployment(ad, renderInputs{cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "openclaw-config", writableRoot: true, securityOverrides: nil})

	roFS := dep.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem
	if roFS == nil || *roFS {
		t.Error("readOnlyRootFilesystem must be false when the framework declares a writable root need")
	}

	// A writable /tmp scratch mount is always provided regardless of root FS.
	var hasTmp bool
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/tmp" {
			hasTmp = true
		}
	}
	if !hasTmp {
		t.Error("a writable /tmp mount must always be present")
	}
}

func TestRenderAgentDeployment_KeylessBindingInjectsLiteralAPIKey(t *testing.T) {
	ad := containerAD("keyless", containerConfig{Image: "img:1"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{
		BaseURL:   "http://demo-model.default.svc.cluster.local:80/v1",
		ModelName: "llama-3.2-1b-instruct",
	}
	dep := renderAgentDeployment(ad, renderInputs{cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "keyless-config", writableRoot: false, securityOverrides: nil})

	var apiKey *corev1.EnvVar
	for i := range dep.Spec.Template.Spec.Containers[0].Env {
		if dep.Spec.Template.Spec.Containers[0].Env[i].Name == "OPENAI_API_KEY" {
			apiKey = &dep.Spec.Template.Spec.Containers[0].Env[i]
			break
		}
	}
	if apiKey == nil {
		t.Fatal("OPENAI_API_KEY env var was not rendered")
	}
	if apiKey.ValueFrom != nil {
		t.Fatalf("OPENAI_API_KEY should be a literal for keyless bindings, got ValueFrom=%+v", apiKey.ValueFrom)
	}
	if apiKey.Value != agentprovider.KeylessCredentialValue {
		t.Fatalf("OPENAI_API_KEY = %q, want %q", apiKey.Value, agentprovider.KeylessCredentialValue)
	}
}

func TestModelBindingEnv_MapsFamilyByAPIType(t *testing.T) {
	cases := []struct {
		name        string
		apiType     airunwayv1alpha1.ExternalAPIType
		wantBaseKey string
		wantModel   string
		wantKeyName string
	}{
		{"openai", airunwayv1alpha1.ExternalAPITypeOpenAI, "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENAI_API_KEY"},
		{"custom", airunwayv1alpha1.ExternalAPITypeCustom, "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENAI_API_KEY"},
		{"unset-deploymentRef", "", "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENAI_API_KEY"},
		{"anthropic", airunwayv1alpha1.ExternalAPITypeAnthropic, "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_API_KEY"},
		{"azureOpenAI", airunwayv1alpha1.ExternalAPITypeAzureOpenAI, "AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_MODEL", "AZURE_OPENAI_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binding := airunwayv1alpha1.ModelBindingStatus{
				APIType:        tc.apiType,
				BaseURL:        "https://endpoint/v1",
				ModelName:      "some-model",
				CredentialsRef: &airunwayv1alpha1.SecretKeyRef{Name: "creds", Key: "api-key"},
			}
			env := map[string]corev1.EnvVar{}
			for _, e := range modelBindingEnv(binding) {
				env[e.Name] = e
			}
			if env[tc.wantBaseKey].Value != "https://endpoint/v1" {
				t.Errorf("%s = %q, want endpoint URL", tc.wantBaseKey, env[tc.wantBaseKey].Value)
			}
			if env[tc.wantModel].Value != "some-model" {
				t.Errorf("%s = %q, want model", tc.wantModel, env[tc.wantModel].Value)
			}
			keyVar, ok := env[tc.wantKeyName]
			if !ok || keyVar.ValueFrom == nil || keyVar.ValueFrom.SecretKeyRef == nil {
				t.Errorf("%s must be sourced from the binding secret, got %+v", tc.wantKeyName, keyVar)
			}
			// The OpenAI family must NOT leak in for non-OpenAI types.
			if tc.wantBaseKey != "OPENAI_BASE_URL" {
				if _, leaked := env["OPENAI_BASE_URL"]; leaked {
					t.Errorf("OPENAI_BASE_URL must not be set for APIType %q", tc.apiType)
				}
			}
		})
	}
}

func TestRenderAgentDeployment_AppliesSecurityOverrides(t *testing.T) {
	ad := containerAD("override", containerConfig{Image: "img:1"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{
		BaseURL:   "http://demo-model.default.svc.cluster.local:80/v1",
		ModelName: "llama-3.2-1b-instruct",
	}

	runAsUser := int64(2000)
	runAsGroup := int64(2001)
	fsGroup := int64(2002)
	readOnly := false
	allowPrivilegeEscalation := true
	localhostProfile := "profiles/default.json"
	overrides := &containerSecurityOverrides{
		PodSecurityContext: &corev1.PodSecurityContext{
			RunAsUser:  &runAsUser,
			RunAsGroup: &runAsGroup,
			FSGroup:    &fsGroup,
			SeccompProfile: &corev1.SeccompProfile{
				Type:             corev1.SeccompProfileTypeLocalhost,
				LocalhostProfile: &localhostProfile,
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                &runAsUser,
			AllowPrivilegeEscalation: &allowPrivilegeEscalation,
			ReadOnlyRootFilesystem:   &readOnly,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"NET_RAW"},
			},
		},
	}

	dep := renderAgentDeployment(ad, renderInputs{cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "override-config", writableRoot: false, securityOverrides: overrides})

	// Overrides that do not weaken the posture are applied as given.
	podSC := dep.Spec.Template.Spec.SecurityContext
	if podSC == nil || podSC.RunAsUser == nil || *podSC.RunAsUser != runAsUser {
		t.Fatalf("pod runAsUser override not applied: %+v", podSC)
	}
	if podSC.RunAsGroup == nil || *podSC.RunAsGroup != runAsGroup {
		t.Fatalf("pod runAsGroup override not applied: %+v", podSC)
	}
	if podSC.FSGroup == nil || *podSC.FSGroup != fsGroup {
		t.Fatalf("pod fsGroup override not applied: %+v", podSC)
	}
	if podSC.SeccompProfile == nil || podSC.SeccompProfile.Type != corev1.SeccompProfileTypeLocalhost {
		t.Fatalf("pod seccomp override not applied: %+v", podSC.SeccompProfile)
	}
	containerSC := dep.Spec.Template.Spec.Containers[0].SecurityContext
	if containerSC == nil || containerSC.RunAsUser == nil || *containerSC.RunAsUser != runAsUser {
		t.Fatalf("container runAsUser override not applied: %+v", containerSC)
	}

	// Overrides that WOULD weaken it are clamped by the render path, not merged.
	// The webhook rejects each of these too, but ENABLE_WEBHOOKS=false is a
	// supported mode, so the floor cannot depend on admission having run.
	// (readOnly=false, allowPrivilegeEscalation=true and a drop list omitting
	// ALL are all requested above.)
	if containerSC.ReadOnlyRootFilesystem == nil || *containerSC.ReadOnlyRootFilesystem == readOnly {
		t.Errorf("readOnlyRootFilesystem must be clamped back to true, got %v", containerSC.ReadOnlyRootFilesystem)
	}
	if containerSC.AllowPrivilegeEscalation == nil || *containerSC.AllowPrivilegeEscalation == allowPrivilegeEscalation {
		t.Errorf("allowPrivilegeEscalation must be clamped back to false, got %v", containerSC.AllowPrivilegeEscalation)
	}
	// The extra drop survives; ALL is added rather than replacing it.
	var hasAll, hasNetRaw bool
	for _, d := range containerSC.Capabilities.Drop {
		switch d {
		case "ALL":
			hasAll = true
		case "NET_RAW":
			hasNetRaw = true
		}
	}
	if !hasAll {
		t.Errorf("capabilities.drop must include ALL, got %v", containerSC.Capabilities.Drop)
	}
	if !hasNetRaw {
		t.Errorf("an additional drop must be preserved, got %v", containerSC.Capabilities.Drop)
	}
}

func TestParseContainerSecurityOverrides_MergesSections(t *testing.T) {
	raw := []byte(`{
		"workload": {
			"podSecurityContext": {
				"runAsUser": 1000
			}
		},
		"container": {
			"securityContext": {
				"readOnlyRootFilesystem": false,
				"allowPrivilegeEscalation": true
			}
		}
	}`)
	ad := &airunwayv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"},
		Spec: airunwayv1alpha1.AgentDeploymentSpec{
			Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "crewai"},
			Model: airunwayv1alpha1.ModelBinding{
				ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
					Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
					BaseURL:   "https://api.openai.com/v1",
					ModelName: "gpt-4o-mini",
				},
			},
			Provider: &airunwayv1alpha1.AgentProviderSpec{
				Overrides: &runtime.RawExtension{Raw: raw},
			},
		},
	}

	overrides, err := parseContainerSecurityOverrides(ad)
	if err != nil {
		t.Fatalf("parseContainerSecurityOverrides returned error: %v", err)
	}
	if overrides == nil || overrides.PodSecurityContext == nil || overrides.PodSecurityContext.RunAsUser == nil || *overrides.PodSecurityContext.RunAsUser != 1000 {
		t.Fatalf("expected merged pod runAsUser override, got %+v", overrides)
	}
	if overrides.SecurityContext == nil || overrides.SecurityContext.ReadOnlyRootFilesystem == nil || *overrides.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("expected readOnlyRootFilesystem=false override, got %+v", overrides.SecurityContext)
	}
	if overrides.SecurityContext.AllowPrivilegeEscalation == nil || !*overrides.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("expected allowPrivilegeEscalation=true override, got %+v", overrides.SecurityContext)
	}
}

func TestRenderAgentDeployment_ResourcesAndOTLP(t *testing.T) {
	ad := containerAD("obs", containerConfig{Image: "img:1"}, nil)
	ad.Spec.Resources = &airunwayv1alpha1.AgentResourceSpec{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	ad.Spec.Observability = &airunwayv1alpha1.AgentObservabilitySpec{
		OTLP: &airunwayv1alpha1.OTLPSpec{Endpoint: "http://collector:4318", Protocol: "http/protobuf"},
	}
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	dep := renderAgentDeployment(ad, renderInputs{cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "obs-config", writableRoot: false, securityOverrides: nil})
	c := dep.Spec.Template.Spec.Containers[0]

	if c.Resources.Requests.Cpu().String() != "250m" {
		t.Errorf("cpu request = %v, want 250m", c.Resources.Requests.Cpu())
	}
	if c.Resources.Limits.Memory().String() != "512Mi" {
		t.Errorf("memory limit = %v, want 512Mi", c.Resources.Limits.Memory())
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://collector:4318" {
		t.Errorf("OTEL endpoint = %q", env["OTEL_EXPORTER_OTLP_ENDPOINT"])
	}
	if env["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/protobuf" {
		t.Errorf("OTEL protocol = %q", env["OTEL_EXPORTER_OTLP_PROTOCOL"])
	}
}

func TestRenderAgentDeployment_CommandArgsPort(t *testing.T) {
	ad := containerAD("smoke", containerConfig{Image: "img:1"}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	cfg := containerConfig{Image: "img:1", Command: []string{"python", "/serve.py"}, Args: []string{"--verbose"}, Port: 9000}

	dep := renderAgentDeployment(ad, renderInputs{cfg: cfg, binding: binding, configMapName: "smoke-config", writableRoot: false, securityOverrides: nil})
	c := dep.Spec.Template.Spec.Containers[0]
	if len(c.Command) != 2 || c.Command[0] != "python" || c.Command[1] != "/serve.py" {
		t.Errorf("command = %v", c.Command)
	}
	if len(c.Args) != 1 || c.Args[0] != "--verbose" {
		t.Errorf("args = %v", c.Args)
	}
	if c.Ports[0].ContainerPort != 9000 {
		t.Errorf("containerPort = %d, want 9000", c.Ports[0].ContainerPort)
	}
	if c.Ports[0].Name != agentContainerPortName {
		t.Errorf("container port name = %q, want %q", c.Ports[0].Name, agentContainerPortName)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil || c.ReadinessProbe.TCPSocket.Port.IntValue() != 9000 {
		t.Errorf("readiness probe = %+v, want TCP :9000", c.ReadinessProbe)
	}
	// The Service targets a stable name so old and new pods can listen on
	// different numeric ports during a rolling update.
	svc := renderAgentService(ad, cfg)
	if svc.Spec.Ports[0].TargetPort.StrVal != agentContainerPortName {
		t.Errorf("service targetPort = %v, want %q", svc.Spec.Ports[0].TargetPort, agentContainerPortName)
	}
	oldDep := renderAgentDeployment(ad, renderInputs{
		cfg: containerConfig{Image: "img:1", Port: 8080}, binding: binding, configMapName: "smoke-config",
	})
	if oldDep.Spec.Template.Spec.Containers[0].Ports[0].Name != c.Ports[0].Name {
		t.Errorf("container port name changed across numeric port update: old=%q new=%q",
			oldDep.Spec.Template.Spec.Containers[0].Ports[0].Name, c.Ports[0].Name)
	}
}

func TestRenderAgentDeployment_ImagePullPolicyTracksImage(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  corev1.PullPolicy
	}{
		{name: "version tag", image: "ghcr.io/example/agent:v1", want: corev1.PullIfNotPresent},
		{name: "latest tag", image: "ghcr.io/example/agent:latest", want: corev1.PullAlways},
		{name: "implicit latest", image: "ghcr.io/example/agent", want: corev1.PullAlways},
		{name: "registry port version tag", image: "registry.example:5000/agent:v2", want: corev1.PullIfNotPresent},
		{name: "digest", image: "ghcr.io/example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: corev1.PullIfNotPresent},
		{name: "tag and digest", image: "ghcr.io/example/agent:latest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: corev1.PullAlways},
	}

	ad := containerAD("pull-policy", containerConfig{}, nil)
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := renderAgentDeployment(ad, renderInputs{
				cfg: containerConfig{Image: tt.image}, binding: binding, configMapName: "pull-policy-config",
			})
			if got := dep.Spec.Template.Spec.Containers[0].ImagePullPolicy; got != tt.want {
				t.Fatalf("imagePullPolicy(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func TestRenderAgentDeployment_MutableCatalogRevisionRollsDeploymentsOnly(t *testing.T) {
	ad := containerAD("catalog-rollout", containerConfig{}, nil)
	ad.UID = types.UID("11111111-1111-1111-1111-111111111111")
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://model.default.svc/v1", ModelName: "m"}
	oldInputs := renderInputs{
		cfg:                  containerConfig{Image: "ghcr.io/example/agent:latest"},
		binding:              binding,
		configMapName:        "catalog-rollout-config",
		serviceAccountName:   "catalog-rollout-runtime",
		catalogImageRevision: "agent-container-provider:dev-aaaaaaaaaaaa",
	}
	newInputs := oldInputs
	newInputs.catalogImageRevision = "agent-container-provider:dev-bbbbbbbbbbbb"

	oldDeployment := renderAgentDeployment(ad, oldInputs)
	newDeployment := renderAgentDeployment(ad, newInputs)
	if oldDeployment.Spec.Template.Spec.Containers[0].Image != newDeployment.Spec.Template.Spec.Containers[0].Image {
		t.Fatal("moving catalog channel must keep the same latest image reference")
	}
	if got := oldDeployment.Spec.Template.Annotations[agentCatalogImageRevisionAnnotation]; got != oldInputs.catalogImageRevision {
		t.Fatalf("old catalog revision = %q, want %q", got, oldInputs.catalogImageRevision)
	}
	if got := newDeployment.Spec.Template.Annotations[agentCatalogImageRevisionAnnotation]; got != newInputs.catalogImageRevision {
		t.Fatalf("new catalog revision = %q, want %q", got, newInputs.catalogImageRevision)
	}
	if maps.Equal(oldDeployment.Spec.Template.Annotations, newDeployment.Spec.Template.Annotations) {
		t.Fatal("provider build revision must change the Deployment pod template")
	}

	jobAgent := ad.DeepCopy()
	jobAgent.Spec.Lifecycle = airunwayv1alpha1.AgentLifecycleJob
	oldJob, err := renderAgentJob(jobAgent, oldInputs)
	if err != nil {
		t.Fatalf("render old Job: %v", err)
	}
	newJob, err := renderAgentJob(jobAgent, newInputs)
	if err != nil {
		t.Fatalf("render new Job: %v", err)
	}
	if _, found := oldJob.Spec.Template.Annotations[agentCatalogImageRevisionAnnotation]; found {
		t.Fatal("catalog rollout revision must not be added to one-shot Job templates")
	}
	if oldJob.Annotations[agentTemplateHashAnnotation] != newJob.Annotations[agentTemplateHashAnnotation] {
		t.Fatal("provider build revision must not change a one-shot Job's accepted template hash")
	}
}

func TestAgentImageUsesMutableTag(t *testing.T) {
	tests := []struct {
		image string
		want  bool
	}{
		{image: "ghcr.io/example/agent", want: true},
		{image: "ghcr.io/example/agent:latest", want: true},
		{image: "registry.example:5000/agent:latest", want: true},
		{image: "ghcr.io/example/agent:v1", want: false},
		{image: "ghcr.io/example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: false},
		{image: "ghcr.io/example/agent:latest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: false},
	}
	for _, tt := range tests {
		if got := agentImageUsesMutableTag(tt.image); got != tt.want {
			t.Errorf("agentImageUsesMutableTag(%q) = %t, want %t", tt.image, got, tt.want)
		}
	}
}

func TestAgentSelectorsIsolateDeleteRecreateIncarnations(t *testing.T) {
	oldAgent := containerAD("same-name", containerConfig{Image: "img:1"}, nil)
	oldAgent.UID = types.UID("11111111-1111-1111-1111-111111111111")
	replacement := oldAgent.DeepCopy()
	replacement.UID = types.UID("22222222-2222-2222-2222-222222222222")
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}

	oldDeployment := renderAgentDeployment(oldAgent, renderInputs{
		cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "same-name-config",
	})
	newDeployment := renderAgentDeployment(replacement, renderInputs{
		cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "same-name-config",
	})
	newService := renderAgentService(replacement, containerConfig{})
	selector := labels.SelectorFromSet(newService.Spec.Selector)

	if selector.Matches(labels.Set(oldDeployment.Spec.Template.Labels)) {
		t.Fatal("replacement Service selector matched pods from the deleted AgentDeployment incarnation")
	}
	if !selector.Matches(labels.Set(newDeployment.Spec.Template.Labels)) {
		t.Fatal("replacement Service selector did not match pods from its own AgentDeployment incarnation")
	}
	if !maps.Equal(newDeployment.Spec.Selector.MatchLabels, newService.Spec.Selector) {
		t.Fatalf("Deployment selector %v and Service selector %v diverged", newDeployment.Spec.Selector.MatchLabels, newService.Spec.Selector)
	}
}

func TestContainerPortDefault(t *testing.T) {
	if got := containerPort(containerConfig{}); got != agentContainerPort {
		t.Errorf("default port = %d, want %d", got, agentContainerPort)
	}
	if got := containerPort(containerConfig{Port: 8000}); got != 8000 {
		t.Errorf("override port = %d, want 8000", got)
	}
}

func TestParseContainerConfig(t *testing.T) {
	raw := &runtime.RawExtension{Raw: []byte(`{"image":"img:2","port":8000,"command":["/bin/serve"],"systemPrompt":"x"}`)}
	cfg, err := parseContainerConfig(raw)
	if err != nil {
		t.Fatalf("parseContainerConfig: %v", err)
	}
	if cfg.Image != "img:2" {
		t.Errorf("parsed = %+v", cfg)
	}
	if cfg.Port != 8000 {
		t.Errorf("port = %d, want 8000", cfg.Port)
	}
	if len(cfg.Command) != 1 || cfg.Command[0] != "/bin/serve" {
		t.Errorf("command = %v", cfg.Command)
	}
	if got, err := parseContainerConfig(nil); err != nil || got.Image != "" {
		t.Errorf("nil config should be empty, got %+v", got)
	}
	for _, port := range []int32{-1, 70000} {
		raw := &runtime.RawExtension{Raw: []byte(fmt.Sprintf(`{"image":"img:2","port":%d}`, port))}
		if _, err := parseContainerConfig(raw); err == nil {
			t.Errorf("port %d should be rejected, got no error", port)
		}
	}
}

func TestRenderAgentJob(t *testing.T) {
	ad := containerAD("swarm", containerConfig{Image: "img:1"}, nil)
	ad.Spec.Lifecycle = airunwayv1alpha1.AgentLifecycleJob
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}
	job, err := renderAgentJob(ad, renderInputs{cfg: containerConfig{Image: "img:1"}, binding: binding, configMapName: "swarm-config", writableRoot: false, securityOverrides: nil})
	if err != nil {
		t.Fatalf("renderAgentJob: %v", err)
	}

	if job.Kind != "Job" || job.APIVersion != "batch/v1" {
		t.Fatalf("GVK = %s/%s", job.APIVersion, job.Kind)
	}
	// Jobs require a non-Always restart policy.
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
	// Shares the hardened pod spec + image.
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "img:1" {
		t.Errorf("image = %q", c.Image)
	}
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Error("job pod must share the hardened security posture (drop ALL)")
	}
	env := map[string]string{}
	for _, value := range c.Env {
		env[value.Name] = value.Value
	}
	if env["AIRUNWAY_AGENT_MODE"] != "job" {
		t.Errorf("AIRUNWAY_AGENT_MODE = %q, want job", env["AIRUNWAY_AGENT_MODE"])
	}
	if c.StartupProbe != nil || c.ReadinessProbe != nil || c.LivenessProbe != nil {
		t.Error("one-shot jobs must not receive server probes")
	}
}

// --- envtest reconcile specs -----------------------------------------------

var _ = Describe("Container provider", func() {
	ctx := context.Background()

	makeContainerProvider := func(name string, catalogImage string) {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           airunwayv1alpha1.AgentProviderBackendContainer,
					ModelBindingModes: []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI},
				},
			},
		}
		if catalogImage != "" {
			catalog := []airunwayv1alpha1.AgentCatalogItem{
				{Name: name + "-recipe", Title: "Recipe", Image: catalogImage},
			}
			raw, err := json.Marshal(catalog)
			Expect(err).NotTo(HaveOccurred())
			apc.Annotations = map[string]string{
				airunwayv1alpha1.AgentProviderCatalogAnnotation: string(raw),
			}
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
		apc.Status.Ready = ptrBool(true)
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())
	}

	makeContainerAgent := func(name, framework, image string) {
		cfgMap := map[string]any{"systemPrompt": "You are a research assistant."}
		if image != "" {
			cfgMap["image"] = image
		}
		raw, _ := json.Marshal(cfgMap)
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: framework},
				Config:    &runtime.RawExtension{Raw: raw},
				Model: airunwayv1alpha1.ModelBinding{
					ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
						Type: airunwayv1alpha1.ExternalAPITypeOpenAI, BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })
	}

	makeContainerJobAgent := func(name, framework, image string) {
		cfgRaw, err := json.Marshal(map[string]any{"image": image, "systemPrompt": "do the task"})
		Expect(err).NotTo(HaveOccurred())
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: framework},
				Lifecycle: airunwayv1alpha1.AgentLifecycleJob,
				Config:    &runtime.RawExtension{Raw: cfgRaw},
				Model: airunwayv1alpha1.ModelBinding{
					ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
						Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
						BaseURL:   "https://api.openai.com/v1",
						ModelName: "gpt-4o-mini",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })
	}

	reconcileCore := func(name string) {
		r := newCredentialAuthorizedAgentDeploymentReconciler(k8sClient)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	reconcileContainer := func(name string) {
		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	getAgent := func(name string) *airunwayv1alpha1.AgentDeployment {
		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, out)).To(Succeed())
		return out
	}
	prCond := func(ad *airunwayv1alpha1.AgentDeployment) *metav1.Condition {
		return meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady)
	}
	markBindingStale := func(ad *airunwayv1alpha1.AgentDeployment) {
		cond := meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeModelBound)
		Expect(cond).NotTo(BeNil())
		cond.Status = metav1.ConditionFalse
		cond.Reason = "BindingRefreshFailed"
		cond.Message = "test stale binding"
		cond.ObservedGeneration = ad.Generation
		cond.LastTransitionTime = metav1.Now()
		Expect(k8sClient.Status().Update(ctx, ad)).To(Succeed())
		Expect(agentprovider.ClassifyBinding(getAgent(ad.Name))).To(Equal(agentprovider.BindingStale))
	}

	It("rejects a stale provider status apply after an AgentDeployment is deleted and recreated", func() {
		stale := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-provider-uid-race", Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "uid-race-framework"},
				Model: airunwayv1alpha1.ModelBinding{ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
					Type: airunwayv1alpha1.ExternalAPITypeOpenAI, BaseURL: "https://api.example/v1", ModelName: "model",
				}},
			},
		}
		Expect(k8sClient.Create(ctx, stale)).To(Succeed())
		key := client.ObjectKeyFromObject(stale)
		Expect(stale.UID).NotTo(BeEmpty())
		Expect(stale.ResourceVersion).NotTo(BeEmpty())
		Expect(k8sClient.Delete(ctx, stale)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &airunwayv1alpha1.AgentDeployment{}))
		}).Should(BeTrue())

		replacement := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: stale.Name, Namespace: stale.Namespace},
			Spec:       stale.Spec,
		}
		Expect(k8sClient.Create(ctx, replacement)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, replacement) })
		Expect(replacement.UID).NotTo(Equal(stale.UID))

		err := agentprovider.ApplyOwnedStatus(ctx, k8sClient, stale, ContainerFieldOwner,
			airunwayv1alpha1.AgentPhaseRunning,
			&airunwayv1alpha1.AgentRuntimeStatus{Address: "http://stale.default.svc"}, nil,
			metav1.ConditionTrue, "StaleWrite", "must not reach the replacement")
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "expected resourceVersion conflict, got %v", err)

		live := getAgent(replacement.Name)
		Expect(live.UID).To(Equal(replacement.UID))
		Expect(live.Status.ProviderOwner).To(BeEmpty())
		Expect(live.Status.Runtime).To(BeNil())
		Expect(prCond(live)).To(BeNil())

		By("also rejecting a stale release against replacement status owned by the same manager")
		Expect(agentprovider.ApplyOwnedStatus(ctx, k8sClient, replacement, ContainerFieldOwner,
			airunwayv1alpha1.AgentPhaseRunning,
			&airunwayv1alpha1.AgentRuntimeStatus{Address: "http://replacement.default.svc"}, nil,
			metav1.ConditionTrue, "CurrentWrite", "belongs to the replacement")).To(Succeed())
		err = agentprovider.ReleaseOwnedStatus(ctx, k8sClient, stale, ContainerFieldOwner)
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "expected resourceVersion conflict, got %v", err)

		live = getAgent(replacement.Name)
		Expect(live.Status.ProviderOwner).To(Equal(ContainerFieldOwner))
		Expect(live.Status.Runtime).NotTo(BeNil())
		Expect(live.Status.Runtime.Address).To(Equal("http://replacement.default.svc"))
		Expect(prCond(live)).NotTo(BeNil())
		Expect(prCond(live).Reason).To(Equal("CurrentWrite"))
	})

	It("rejects stale Job ledger mutations after the AgentDeployment and ConfigMap are replaced", func() {
		stale := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-job-ledger-uid-race", Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "uid-race-framework"},
				Lifecycle: airunwayv1alpha1.AgentLifecycleJob,
				Model: airunwayv1alpha1.ModelBinding{ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
					Type: airunwayv1alpha1.ExternalAPITypeOpenAI, BaseURL: "https://api.example/v1", ModelName: "model",
				}},
			},
		}
		Expect(k8sClient.Create(ctx, stale)).To(Succeed())
		staleKey := client.ObjectKeyFromObject(stale)
		staleLedger := renderAgentConfigMap(stale)
		Expect(controllerutil.SetControllerReference(stale, staleLedger, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, staleLedger)).To(Succeed())
		Expect(stale.UID).NotTo(BeEmpty())
		Expect(staleLedger.UID).NotTo(BeEmpty())

		staleAD := stale.DeepCopy()
		staleLedgerRef := staleLedger.DeepCopy()
		ledgerKey := client.ObjectKeyFromObject(staleLedger)
		Expect(k8sClient.Delete(ctx, staleLedger)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, ledgerKey, &corev1.ConfigMap{}))
		}).Should(BeTrue())
		Expect(k8sClient.Delete(ctx, stale)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, staleKey, &airunwayv1alpha1.AgentDeployment{}))
		}).Should(BeTrue())

		replacement := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: staleAD.Name, Namespace: staleAD.Namespace},
			Spec:       staleAD.Spec,
		}
		Expect(k8sClient.Create(ctx, replacement)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, replacement) })
		Expect(replacement.UID).NotTo(Equal(staleAD.UID))

		replacementLedger := renderAgentConfigMap(replacement)
		Expect(controllerutil.SetControllerReference(replacement, replacementLedger, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, replacementLedger)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, replacementLedger) })
		Expect(replacementLedger.UID).NotTo(Equal(staleLedgerRef.UID))

		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		generation := fmt.Sprint(staleAD.Generation)
		cases := []struct {
			name        string
			annotations map[string]string
			mutate      func() error
		}{
			{
				name: "preflight",
				annotations: map[string]string{
					agentJobGenerationAnnotation: generation,
					agentJobOutcomeAnnotation:    agentJobOutcomeCompleted,
				},
				mutate: func() error {
					_, _, err := r.preflightJobLedger(ctx, staleAD, staleLedgerRef)
					return err
				},
			},
			{
				name: "persist outcome",
				annotations: map[string]string{
					agentJobGenerationAnnotation: "replacement-sentinel-generation",
					agentJobOutcomeAnnotation:    "replacement-sentinel-outcome",
				},
				mutate: func() error {
					return r.persistJobOutcome(ctx, staleAD, staleLedgerRef, agentJobOutcomeCompleted)
				},
			},
			{
				name: "clear outcome",
				annotations: map[string]string{
					agentJobGenerationAnnotation: generation,
					agentJobOutcomeAnnotation:    agentJobOutcomeLost,
				},
				mutate: func() error {
					return r.clearJobOutcome(ctx, staleAD, staleLedgerRef, agentJobOutcomeLost)
				},
			},
			{
				name: "ensure claim",
				annotations: map[string]string{
					agentJobGenerationAnnotation: "replacement-sentinel-generation",
					agentJobOutcomeAnnotation:    agentJobOutcomeCompleted,
				},
				mutate: func() error {
					_, _, err := r.ensureJobGenerationClaim(ctx, staleAD, staleLedgerRef)
					return err
				},
			},
			{
				name: "record outcome",
				annotations: map[string]string{
					agentJobGenerationAnnotation: generation,
					agentJobClaimNonceAnnotation: "replacement-sentinel-nonce",
				},
				mutate: func() error {
					return r.recordJobOutcome(ctx, staleAD, staleLedgerRef, agentJobOutcomeCompleted)
				},
			},
			{
				name: "mark claim retryable",
				annotations: map[string]string{
					agentJobGenerationAnnotation: generation,
					agentJobClaimNonceAnnotation: "replacement-sentinel-nonce",
				},
				mutate: func() error {
					return r.markJobClaimRetryable(ctx, staleAD, staleLedgerRef)
				},
			},
			{
				name: "release claim",
				annotations: map[string]string{
					agentJobGenerationAnnotation: generation,
					agentJobOutcomeAnnotation:    agentJobOutcomeRetryable,
				},
				mutate: func() error {
					return r.releaseJobGenerationClaim(ctx, staleAD, staleLedgerRef)
				},
			},
		}

		for _, tc := range cases {
			By(tc.name)
			current := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, ledgerKey, current)).To(Succeed())
			current.Annotations = maps.Clone(tc.annotations)
			current.Data = map[string]string{agentConfigFileName: "replacement-sentinel-data"}
			Expect(k8sClient.Update(ctx, current)).To(Succeed())

			before := current.DeepCopy()
			err := tc.mutate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("expected live exact-owned UID"))

			after := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, ledgerKey, after)).To(Succeed())
			Expect(after.UID).To(Equal(before.UID))
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
			Expect(after.OwnerReferences).To(Equal(before.OwnerReferences))
			Expect(after.Annotations).To(Equal(before.Annotations))
			Expect(after.Data).To(Equal(before.Data))
		}
	})

	It("waits for core bindings before rendering", func() {
		makeContainerProvider("crewai-wait", "")
		makeContainerAgent("c-wait", "crewai-wait", "ghcr.io/x/crewai:poc")

		reconcileContainer("c-wait")
		ad := getAgent("c-wait")
		Expect(prCond(ad).Reason).To(Equal("WaitingForBindings"))

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-wait", Namespace: "default"}, dep)).NotTo(Succeed())
	})

	It("recreates a pre-UID-selector Deployment before applying the immutable selector", func() {
		makeContainerProvider("crewai-selector-migration", "")
		makeContainerAgent("c-selector-migration", "crewai-selector-migration", "ghcr.io/x/crewai:poc")
		reconcileCore("c-selector-migration")

		ad := getAgent("c-selector-migration")
		legacy := renderAgentDeployment(ad, renderInputs{
			cfg:                containerConfig{Image: "ghcr.io/x/crewai:poc"},
			binding:            *ad.Status.ModelBinding,
			configMapName:      agentConfigMapName(ad),
			serviceAccountName: agentServiceAccountName(ad),
		})
		legacy.Spec.Selector.MatchLabels = map[string]string{
			"airunway.ai/agent": agentprovider.BoundedLabelValue(ad.Name),
		}
		delete(legacy.Spec.Template.Labels, "airunway.ai/agent-uid")
		delete(legacy.Labels, "airunway.ai/agent-uid")
		Expect(controllerutil.SetControllerReference(ad, legacy, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())

		reconcileContainer(ad.Name)
		out := getAgent(ad.Name)
		Expect(prCond(out).Reason).To(Equal("WorkloadReplacing"))
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil(),
			"selector replacement must retain the only reference to the random ingress Secret")
		replacingAccessRef := *out.Status.Runtime.AuthSecretRef
		svc := &corev1.Service{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), svc))).To(BeTrue(),
			"the old selector Service must be removed while the legacy Deployment terminates")

		// envtest does not run the garbage collector that completes foreground
		// deletion, so remove its finalizer before the replacement pass.
		stale := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), stale); err == nil {
			Expect(stale.DeletionTimestamp.IsZero()).To(BeFalse())
			stale.Finalizers = nil
			Expect(k8sClient.Update(ctx, stale)).To(Succeed())
			_ = k8sClient.Delete(ctx, stale)
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), &appsv1.Deployment{}))
		}).Should(BeTrue())

		reconcileContainer(ad.Name)
		replacement := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), replacement)).To(Succeed())
		Expect(replacement.Spec.Selector.MatchLabels).To(HaveKeyWithValue("airunway.ai/agent-uid", string(ad.UID)))
		Expect(replacement.Spec.Template.Labels).To(HaveKeyWithValue("airunway.ai/agent-uid", string(ad.UID)))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), svc)).To(Succeed())
		Expect(svc.Spec.Selector).To(HaveKeyWithValue("airunway.ai/agent-uid", string(ad.UID)))
		out = getAgent(ad.Name)
		Expect(out.Status.Runtime.AuthSecretRef).To(Equal(&replacingAccessRef),
			"replacement must reuse the published ingress credential")
	})

	It("keeps routing absent while an exact-owned Deployment is already terminating", func() {
		makeContainerProvider("crewai-terminating-deployment", "")
		makeContainerAgent("c-terminating-deployment", "crewai-terminating-deployment", "ghcr.io/x/crewai:poc")
		reconcileCore("c-terminating-deployment")
		reconcileContainer("c-terminating-deployment")

		ad := getAgent("c-terminating-deployment")
		key := client.ObjectKeyFromObject(ad)
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
		oldUID := deployment.UID
		deployment.Finalizers = []string{"test.airunway.ai/hold-terminating-deployment"}
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
		Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		Eventually(func() bool {
			current := &appsv1.Deployment{}
			return k8sClient.Get(ctx, key, current) == nil && !current.DeletionTimestamp.IsZero()
		}).Should(BeTrue())

		serviceKey := types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}
		Expect(k8sClient.Get(ctx, serviceKey, &corev1.Service{})).To(Succeed())
		for range 2 {
			reconcileContainer(ad.Name)
			out := getAgent(ad.Name)
			Expect(prCond(out).Reason).To(Equal("WorkloadReplacing"))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serviceKey, &corev1.Service{}))).To(BeTrue(),
				"a terminating Deployment must not regain a routing Service")
			current := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			Expect(current.UID).To(Equal(oldUID))
			Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())
		}

		terminating := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, terminating)).To(Succeed())
		terminating.Finalizers = nil
		Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))
		}).Should(BeTrue())

		reconcileContainer(ad.Name)
		replacement := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, replacement)).To(Succeed())
		Expect(replacement.UID).NotTo(Equal(oldUID))
		Expect(k8sClient.Get(ctx, serviceKey, &corev1.Service{})).To(Succeed())
	})

	DescribeTable("deletes the old Deployment before applying a credential template change",
		func(suffix string, rotateModel bool) {
			framework := "crewai-credential-recreate-" + suffix
			name := "c-credential-recreate-" + suffix
			makeContainerProvider(framework, "")
			credential := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("test-only-value-one")},
			}
			Expect(k8sClient.Create(ctx, credential)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, credential) })

			makeContainerAgent(name, framework, "ghcr.io/x/crewai:poc")
			ad := getAgent(name)
			ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{Name: credential.Name, Key: "api-key"}
			Expect(k8sClient.Update(ctx, ad)).To(Succeed())
			reconcileCore(name)
			reconcileContainer(name)

			ad = getAgent(name)
			Expect(ad.Status.Runtime).NotTo(BeNil())
			Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
			oldAccessRef := *ad.Status.Runtime.AuthSecretRef
			key := client.ObjectKeyFromObject(ad)
			oldDeployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, oldDeployment)).To(Succeed())
			oldModelChecksum := oldDeployment.Spec.Template.Annotations[agentModelCredentialChecksumAnnotation]
			Expect(oldModelChecksum).NotTo(BeEmpty())

			if rotateModel {
				By("updating the model credential revision")
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(credential), credential)).To(Succeed())
				credential.Data["api-key"] = []byte("test-only-value-two")
				Expect(k8sClient.Update(ctx, credential)).To(Succeed())
			} else {
				By("deleting the ingress Secret so the provider rotates its bearer token")
				oldSecret := &corev1.Secret{}
				oldSecretKey := types.NamespacedName{Name: oldAccessRef.Name, Namespace: ad.Namespace}
				Expect(k8sClient.Get(ctx, oldSecretKey, oldSecret)).To(Succeed())
				Expect(k8sClient.Delete(ctx, oldSecret)).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, oldSecretKey, &corev1.Secret{}))
				}).Should(BeTrue())
			}

			reconcileContainer(name)
			out := getAgent(name)
			Expect(prCond(out).Reason).To(Equal("WorkloadReplacing"))
			Expect(out.Status.Runtime).NotTo(BeNil())
			Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil())
			terminating := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, terminating)).To(Succeed())
			Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse(),
				"credential changes must delete before replacement instead of starting a rolling update")
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}, &corev1.Service{}))).To(BeTrue(),
				"the endpoint must be removed while stale credential-bearing pods terminate")

			if rotateModel {
				Expect(out.Status.Runtime.AuthSecretRef).To(Equal(&oldAccessRef))
				newChecksum, err := (&ContainerProviderReconciler{APIReader: k8sClient}).modelCredentialChecksum(
					ctx, ad.Namespace, *ad.Status.ModelBinding)
				Expect(err).NotTo(HaveOccurred())
				Expect(newChecksum).NotTo(Equal(oldModelChecksum))
				Expect(terminating.Spec.Template.Annotations[agentModelCredentialChecksumAnnotation]).To(Equal(oldModelChecksum),
					"the old Deployment must terminate before the new credential revision is applied")
			} else {
				Expect(out.Status.Runtime.AuthSecretRef.Name).NotTo(Equal(oldAccessRef.Name))
				deployedName, _, _, found := deploymentAccessCredential(terminating)
				Expect(found).To(BeTrue())
				Expect(deployedName).To(Equal(oldAccessRef.Name),
					"the old Deployment must terminate before the new ingress token is applied")
			}

			// envtest has no garbage collector to complete foreground deletion.
			terminating.Finalizers = nil
			Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
		},
		Entry("for an ingress token rotation", "ingress", false),
		Entry("for a model Secret revision", "model", true),
	)

	It("deletes the old Deployment before applying a stronger security posture", func() {
		const framework = "crewai-security-recreate"
		const name = "c-security-recreate"
		makeContainerProvider(framework, "")

		providerKey := types.NamespacedName{Name: framework}
		provider := &airunwayv1alpha1.AgentProviderConfig{}
		Expect(k8sClient.Get(ctx, providerKey, provider)).To(Succeed())
		provider.Spec.Capabilities.WritableRootFilesystem = ptr.To(true)
		Expect(k8sClient.Update(ctx, provider)).To(Succeed())

		makeContainerAgent(name, framework, "ghcr.io/x/crewai:poc")
		reconcileCore(name)
		reconcileContainer(name)

		ad := getAgent(name)
		key := client.ObjectKeyFromObject(ad)
		oldDeployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, oldDeployment)).To(Succeed())
		oldUID := oldDeployment.UID
		oldReadOnly := oldDeployment.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem
		Expect(oldReadOnly).NotTo(BeNil())
		Expect(*oldReadOnly).To(BeFalse())
		oldDeployment.Finalizers = append(oldDeployment.Finalizers, "tests.airunway.ai/hold-security-replacement")
		Expect(k8sClient.Update(ctx, oldDeployment)).To(Succeed())

		By("strengthening the provider-owned root-filesystem policy")
		Expect(k8sClient.Get(ctx, providerKey, provider)).To(Succeed())
		provider.Spec.Capabilities.WritableRootFilesystem = ptr.To(false)
		Expect(k8sClient.Update(ctx, provider)).To(Succeed())
		reconcileContainer(name)

		out := getAgent(name)
		Expect(prCond(out).Reason).To(Equal("WorkloadReplacing"))
		terminating := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, terminating)).To(Succeed())
		Expect(terminating.UID).To(Equal(oldUID))
		Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse(),
			"security posture changes must delete before replacement instead of rolling")
		Expect(*terminating.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem).To(BeFalse(),
			"the old pod template must remain unchanged while it terminates")
		serviceKey := types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serviceKey, &corev1.Service{}))).To(BeTrue(),
			"the weaker pod must have no routing boundary during replacement")

		reconcileContainer(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serviceKey, &corev1.Service{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, key, terminating)).To(Succeed())
		Expect(terminating.UID).To(Equal(oldUID))

		terminating.Finalizers = nil
		Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))
		}).Should(BeTrue())

		reconcileContainer(name)
		replacement := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, replacement)).To(Succeed())
		Expect(replacement.UID).NotTo(Equal(oldUID))
		readOnly := replacement.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem
		Expect(readOnly).NotTo(BeNil())
		Expect(*readOnly).To(BeTrue())
		Expect(k8sClient.Get(ctx, serviceKey, &corev1.Service{})).To(Succeed())
	})

	DescribeTable("retains one ingress Secret across repeated workload apply failures",
		func(target, suffix string) {
			framework := "crewai-apply-" + suffix
			name := "c-apply-" + suffix
			makeContainerProvider(framework, "")
			makeContainerAgent(name, framework, "ghcr.io/x/crewai:poc")
			reconcileCore(name)

			applyErr := fmt.Errorf("injected %s apply failure", target)
			failingClient := &failingWorkloadApplyClient{
				Client: k8sClient,
				target: target,
				err:    applyErr,
			}
			r := &ContainerProviderReconciler{Client: failingClient, Scheme: k8sClient.Scheme()}
			key := types.NamespacedName{Name: name, Namespace: "default"}

			var accessName string
			for range 2 {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).To(MatchError(ContainSubstring(applyErr.Error())))

				out := getAgent(name)
				Expect(prCond(out).Reason).To(Equal("RenderFailed"))
				Expect(out.Status.Runtime).NotTo(BeNil())
				Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil(),
					"an apply error must retain the only reference to the random ingress Secret")
				if accessName == "" {
					accessName = out.Status.Runtime.AuthSecretRef.Name
				}
				Expect(out.Status.Runtime.AuthSecretRef.Name).To(Equal(accessName),
					"a retry must reuse the published credential instead of leaking another Secret")
			}
			Expect(failingClient.attempts).To(Equal(2))

			out := getAgent(name)
			var secrets corev1.SecretList
			Expect(k8sClient.List(ctx, &secrets,
				client.InNamespace(out.Namespace), client.MatchingLabels(agentLabels(out)))).To(Succeed())
			Expect(secrets.Items).To(HaveLen(1))
			Expect(secrets.Items[0].Name).To(Equal(accessName))
			switch target {
			case "Deployment":
				Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &corev1.Service{}))).To(BeTrue(),
					"a failed initial Deployment create must not leave its selector Service live")
			case "Service":
				Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))).To(BeTrue(),
					"a rejected Service must prevent the credential-bearing Deployment from starting")
			}
		},
		Entry("when the Deployment apply fails", "Deployment", "deployment"),
		Entry("when the Service apply fails", "Service", "service"),
	)

	It("retains the published ingress Secret across ConfigMap apply failures", func() {
		makeContainerProvider("crewai-configmap-apply", "")
		makeContainerAgent("c-configmap-apply", "crewai-configmap-apply", "ghcr.io/x/crewai:poc")
		reconcileCore("c-configmap-apply")
		reconcileContainer("c-configmap-apply")

		ad := getAgent("c-configmap-apply")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		accessRef := *ad.Status.Runtime.AuthSecretRef
		accessKey := types.NamespacedName{Name: accessRef.Name, Namespace: ad.Namespace}
		accessSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, accessKey, accessSecret)).To(Succeed())
		accessUID := accessSecret.UID

		key := client.ObjectKeyFromObject(ad)
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
		deploymentUID := deployment.UID
		applyErr := fmt.Errorf("injected existing agent ConfigMap apply failure")

		for range 2 {
			failingClient := &failingConfigMapPatchClient{
				Client: k8sClient, patchType: types.ApplyPatchType, err: applyErr,
			}
			r := &ContainerProviderReconciler{
				Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).To(MatchError(ContainSubstring(applyErr.Error())))
			Expect(failingClient.failed).To(BeTrue())

			out := getAgent(ad.Name)
			Expect(prCond(out).Reason).To(Equal("RenderFailed"))
			Expect(out.Status.Runtime).NotTo(BeNil())
			Expect(out.Status.Runtime.AuthSecretRef).To(Equal(&accessRef),
				"the ConfigMap error must not erase the only randomized Secret reference")
			Expect(k8sClient.Get(ctx, accessKey, accessSecret)).To(Succeed())
			Expect(accessSecret.UID).To(Equal(accessUID))
			Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
			Expect(deployment.UID).To(Equal(deploymentUID))
			Expect(deployment.DeletionTimestamp.IsZero()).To(BeTrue())
		}

		var secrets corev1.SecretList
		Expect(k8sClient.List(ctx, &secrets,
			client.InNamespace(ad.Namespace), client.MatchingLabels(agentLabels(ad)))).To(Succeed())
		Expect(secrets.Items).To(HaveLen(1),
			"retries must preserve the published credential instead of leaking replacement Secrets")
		Expect(secrets.Items[0].UID).To(Equal(accessUID))
	})

	It("rejects a foreign same-name Deployment before creating its Service", func() {
		makeContainerProvider("crewai-foreign-deployment", "")
		makeContainerAgent("c-foreign-deployment", "crewai-foreign-deployment", "ghcr.io/x/crewai:poc")
		reconcileCore("c-foreign-deployment")

		ad := getAgent("c-foreign-deployment")
		selector := agentSelector(ad)
		foreign := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: selector},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: selector},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "foreign", Image: "registry.k8s.io/pause:3.10",
					}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })
		foreignUID := foreign.UID

		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring("not bound to the exact AgentDeployment")))

		live := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), live)).To(Succeed())
		Expect(live.UID).To(Equal(foreignUID), "the foreign Deployment must remain untouched")
		serviceKey := types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serviceKey, &corev1.Service{}))).To(BeTrue(),
			"a foreign Deployment must be rejected before its predictable selector is exposed by a Service")
	})

	It("removes its Service when a foreign Deployment wins the create race", func() {
		makeContainerProvider("crewai-deployment-race", "")
		makeContainerAgent("c-deployment-race", "crewai-deployment-race", "ghcr.io/x/crewai:poc")
		reconcileCore("c-deployment-race")

		ad := getAgent("c-deployment-race")
		selector := agentSelector(ad)
		foreign := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: selector},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: selector},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "foreign", Image: "registry.k8s.io/pause:3.10",
					}}},
				},
			},
		}
		serviceKey := types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}
		racingClient := &foreignDeploymentRaceClient{
			Client: k8sClient, foreign: foreign, serviceKey: serviceKey,
		}
		r := &ContainerProviderReconciler{Client: racingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring("refusing to adopt Deployment")))
		Expect(racingClient.raced).To(BeTrue())
		Expect(racingClient.serviceSeen).To(BeTrue(), "the injected Deployment must win after Service creation")
		Expect(racingClient.serviceSelectorWasEmpty).To(BeTrue(),
			"the Service must remain unroutable until exact Deployment ownership is established")
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

		live := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), live)).To(Succeed())
		Expect(live.UID).To(Equal(racingClient.foreignUID), "ownership cleanup must not delete the foreign Deployment")
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, serviceKey, &corev1.Service{}))
		}).Should(BeTrue(), "the exact-owned Service must be removed after the ownership race")
	})

	It("tears down a newly-created Deployment when Service selector activation fails", func() {
		makeContainerProvider("crewai-service-activation", "")
		makeContainerAgent("c-service-activation", "crewai-service-activation", "ghcr.io/x/crewai:poc")
		reconcileCore("c-service-activation")

		ad := getAgent("c-service-activation")
		key := client.ObjectKeyFromObject(ad)
		serviceKey := types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}
		applyErr := fmt.Errorf("injected active Service selector apply failure")
		failingClient := &failingServiceActivationClient{Client: k8sClient, err: applyErr}
		r := &ContainerProviderReconciler{
			Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
		}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(applyErr.Error())))
		Expect(failingClient.inertServiceApplied).To(BeTrue())
		Expect(failingClient.deploymentCreated).To(BeTrue())
		Expect(failingClient.serviceActivationTried).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serviceKey, &corev1.Service{}))).To(BeTrue(),
			"failed selector activation must remove the inert routing boundary")

		terminating := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, terminating)).To(Succeed())
		Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse(),
			"failed selector activation must foreground-delete the credential-bearing Deployment")
		out := getAgent(ad.Name)
		Expect(prCond(out).Reason).To(Equal("RenderFailed"))
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil(),
			"the published random credential must remain reachable for retry")

		terminating.Finalizers = nil
		Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))
		}).Should(BeTrue())
	})

	It("removes its Service when Deployment credentials drift after preflight and apply fails", func() {
		makeContainerProvider("crewai-deployment-credential-race", "")
		makeContainerAgent("c-deployment-credential-race", "crewai-deployment-credential-race", "ghcr.io/x/crewai:poc")
		reconcileCore("c-deployment-credential-race")
		reconcileContainer("c-deployment-credential-race")

		ad := getAgent("c-deployment-credential-race")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		accessRef := *ad.Status.Runtime.AuthSecretRef
		deploymentKey := client.ObjectKeyFromObject(ad)
		serviceKey := types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}
		Expect(k8sClient.Get(ctx, serviceKey, &corev1.Service{})).To(Succeed())

		applyErr := fmt.Errorf("injected Deployment apply failure after credential drift")
		driftingClient := &credentialDriftDeploymentApplyClient{Client: k8sClient, err: applyErr}
		r := &ContainerProviderReconciler{Client: driftingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: deploymentKey})
		Expect(err).To(MatchError(ContainSubstring(applyErr.Error())))
		Expect(driftingClient.drifted).To(BeTrue())

		terminating := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deploymentKey, terminating)).To(Succeed())
		Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serviceKey, &corev1.Service{}))).To(BeTrue(),
			"credential-mismatch cleanup must remove routing while the stale Deployment is foreground-deleting")
		out := getAgent(ad.Name)
		Expect(prCond(out).Reason).To(Equal("RenderFailed"))
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).To(Equal(&accessRef))

		terminating.Finalizers = nil
		Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, deploymentKey, &appsv1.Deployment{}))
		}).Should(BeTrue())
	})

	It("stops a stale Deployment when a credential rotation cannot be applied", func() {
		makeContainerProvider("crewai-auth-rotation-apply", "")
		makeContainerAgent("c-auth-rotation-apply", "crewai-auth-rotation-apply", "ghcr.io/x/crewai:poc")
		reconcileCore("c-auth-rotation-apply")
		reconcileContainer("c-auth-rotation-apply")

		ad := getAgent("c-auth-rotation-apply")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		oldRef := *ad.Status.Runtime.AuthSecretRef
		oldSecretKey := types.NamespacedName{Name: oldRef.Name, Namespace: ad.Namespace}
		oldDeployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), oldDeployment)).To(Succeed())
		oldName, oldKey, _, found := deploymentAccessCredential(oldDeployment)
		Expect(found).To(BeTrue())
		Expect(oldName).To(Equal(oldRef.Name))
		Expect(oldKey).To(Equal(oldRef.Key))

		By("removing the published Secret so reconciliation must rotate the token")
		oldSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, oldSecretKey, oldSecret)).To(Succeed())
		Expect(k8sClient.Delete(ctx, oldSecret)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, oldSecretKey, &corev1.Secret{}))
		}).Should(BeTrue())

		applyErr := fmt.Errorf("injected rotated Deployment apply failure")
		failingClient := &failingWorkloadApplyClient{Client: k8sClient, target: "Deployment", err: applyErr}
		r := &ContainerProviderReconciler{Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).NotTo(HaveOccurred())
		Expect(failingClient.attempts).To(BeZero(),
			"the replacement must not be applied until the stale Deployment is gone")

		out := getAgent(ad.Name)
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		newRef := *out.Status.Runtime.AuthSecretRef
		Expect(newRef.Name).NotTo(Equal(oldRef.Name))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: newRef.Name, Namespace: out.Namespace}, &corev1.Secret{})).To(Succeed())

		By("terminating the Deployment that still consumes the retired token")
		Eventually(func(g Gomega) {
			live := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), live)
			if apierrors.IsNotFound(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(live.DeletionTimestamp.IsZero()).To(BeFalse())
		}).Should(Succeed())

		// envtest has no garbage collector to complete foreground deletion.
		terminating := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), terminating); err == nil {
			terminating.Finalizers = nil
			Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
			_ = k8sClient.Delete(ctx, terminating)
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), &appsv1.Deployment{}))
		}).Should(BeTrue())

		By("attempting the replacement only after the stale credential-bearing pods are gone")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring(applyErr.Error())))
		Expect(failingClient.attempts).To(Equal(1))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), &appsv1.Deployment{}))).To(BeTrue())
	})

	It("stops a stale Deployment only after model credentials change across an apply failure", func() {
		makeContainerProvider("crewai-model-rotation-apply", "")
		credential := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-model-rotation-apply-model", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("test-only-value-one")},
		}
		Expect(k8sClient.Create(ctx, credential)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, credential) })

		makeContainerAgent("c-model-rotation-apply", "crewai-model-rotation-apply", "ghcr.io/x/crewai:poc")
		ad := getAgent("c-model-rotation-apply")
		ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
			Name: credential.Name,
			Key:  "api-key",
		}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		reconcileContainer(ad.Name)

		ad = getAgent(ad.Name)
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		accessRef := *ad.Status.Runtime.AuthSecretRef
		key := client.ObjectKeyFromObject(ad)
		live := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, live)).To(Succeed())
		modelName, modelKey, oldChecksum, found := deploymentModelCredential(live)
		Expect(found).To(BeTrue())
		Expect(modelName).To(Equal(credential.Name))
		Expect(modelKey).To(Equal("api-key"))
		Expect(oldChecksum).NotTo(BeEmpty())

		applyErr := fmt.Errorf("injected model credential Deployment apply failure")
		failingClient := &failingWorkloadApplyClient{Client: k8sClient, target: "Deployment", err: applyErr}
		r := &ContainerProviderReconciler{Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}

		By("preserving an existing Deployment when its credential inputs are already current")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(applyErr.Error())))
		Expect(k8sClient.Get(ctx, key, live)).To(Succeed())
		Expect(live.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}, &corev1.Service{})).To(Succeed(),
			"an ordinary apply failure against the exact-owned current Deployment must preserve its Service")

		By("updating the model Secret so the old pods retain a stale credential")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(credential), credential)).To(Succeed())
		credential.Data["api-key"] = []byte("test-only-value-two")
		Expect(k8sClient.Update(ctx, credential)).To(Succeed())
		ad = getAgent(ad.Name)
		newChecksum, err := r.modelCredentialChecksum(ctx, ad.Namespace, *ad.Status.ModelBinding)
		Expect(err).NotTo(HaveOccurred())
		Expect(newChecksum).NotTo(Equal(oldChecksum))

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(failingClient.attempts).To(Equal(1),
			"the replacement must not be applied until the stale Deployment is gone")
		out := getAgent(ad.Name)
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).To(Equal(&accessRef),
			"a model credential rollout failure must not rotate the independent ingress token")

		By("terminating the Deployment that still consumes the old model credential revision")
		Eventually(func(g Gomega) {
			current := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, key, current)
			if apierrors.IsNotFound(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())
		}).Should(Succeed())

		// envtest has no garbage collector to complete foreground deletion.
		terminating := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, key, terminating); err == nil {
			terminating.Finalizers = nil
			Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
			_ = k8sClient.Delete(ctx, terminating)
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))
		}).Should(BeTrue())

		By("attempting the replacement only after the stale credential-bearing pods are gone")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(applyErr.Error())))
		Expect(failingClient.attempts).To(Equal(2))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))).To(BeTrue())
	})

	It("recovers a random access Secret when status publication and cleanup both fail", func() {
		makeContainerProvider("crewai-auth-publish-failure", "")
		makeContainerAgent("c-auth-publish-failure", "crewai-auth-publish-failure", "ghcr.io/x/crewai:poc")
		reconcileCore("c-auth-publish-failure")

		statusErr := fmt.Errorf("injected access credential status failure")
		deleteErr := fmt.Errorf("injected unpublished Secret deletion failure")
		failingClient := &failingCredentialPublishClient{
			Client:    k8sClient,
			statusErr: statusErr,
			deleteErr: deleteErr,
		}
		r := &ContainerProviderReconciler{Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		key := types.NamespacedName{Name: "c-auth-publish-failure", Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(And(
			ContainSubstring(statusErr.Error()),
			ContainSubstring(deleteErr.Error()),
		)))
		Expect(failingClient.failedStatus).To(BeTrue())
		Expect(failingClient.failedDelete).To(BeTrue())

		out := getAgent(key.Name)
		Expect(currentAccessCredentialRef(out)).To(BeNil(),
			"the injected status failure must leave the random name unpublished")
		ledger := &corev1.ConfigMap{}
		ledgerKey := types.NamespacedName{Name: agentConfigMapName(out), Namespace: out.Namespace}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		reservedName := ledger.Annotations[agentAccessPendingAnnotation]
		Expect(reservedName).NotTo(BeEmpty())
		reservedKey := types.NamespacedName{Name: reservedName, Namespace: out.Namespace}
		reserved := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, reservedKey, reserved)).To(Succeed())
		reservedUID := reserved.UID

		By("recovering and publishing the exact reserved Secret on retry")
		reconcileContainer(key.Name)
		out = getAgent(key.Name)
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef.Name).To(Equal(reservedName))
		Expect(k8sClient.Get(ctx, reservedKey, reserved)).To(Succeed())
		Expect(reserved.UID).To(Equal(reservedUID))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentAccessPendingAnnotation))

		var secrets corev1.SecretList
		Expect(k8sClient.List(ctx, &secrets,
			client.InNamespace(out.Namespace), client.MatchingLabels(agentLabels(out)))).To(Succeed())
		Expect(secrets.Items).To(HaveLen(1),
			"retry must recover the reserved Secret instead of allocating another random credential")
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
		secretName, secretKey, _, found := deploymentAccessCredential(deployment)
		Expect(found).To(BeTrue())
		Expect(secretName).To(Equal(reservedName))
		Expect(secretKey).To(Equal(agentAccessTokenKey))
	})

	It("stops the old workload when a rotated access credential cannot be published", func() {
		makeContainerProvider("crewai-auth-rotation-publish-failure", "")
		makeContainerAgent("c-auth-rotation-publish-failure", "crewai-auth-rotation-publish-failure", "ghcr.io/x/crewai:poc")
		reconcileCore("c-auth-rotation-publish-failure")
		reconcileContainer("c-auth-rotation-publish-failure")

		key := types.NamespacedName{Name: "c-auth-rotation-publish-failure", Namespace: "default"}
		ad := getAgent(key.Name)
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		oldRef := *ad.Status.Runtime.AuthSecretRef
		oldSecretKey := types.NamespacedName{Name: oldRef.Name, Namespace: ad.Namespace}
		Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: oldSecretKey.Name, Namespace: oldSecretKey.Namespace,
		}})).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, oldSecretKey, &corev1.Secret{}))
		}).Should(BeTrue())

		statusErr := fmt.Errorf("injected rotated access credential status failure")
		failingClient := &failingCredentialPublishClient{Client: k8sClient, statusErr: statusErr}
		r := &ContainerProviderReconciler{
			Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(statusErr.Error())))
		Expect(failingClient.failedStatus).To(BeTrue())

		out := getAgent(key.Name)
		Expect(currentAccessCredentialRef(out)).To(Equal(&oldRef),
			"failed publication must not claim that the replacement credential is active")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: agentServiceName(out), Namespace: out.Namespace},
			&corev1.Service{}))).To(BeTrue(),
			"the old token must stop being reachable before the unpublished replacement is discarded")

		terminating := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, terminating)).To(Succeed())
		Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse(),
			"the old credential-bearing Deployment must be foreground-deleting")

		ledger := &corev1.ConfigMap{}
		ledgerKey := types.NamespacedName{Name: agentConfigMapName(out), Namespace: out.Namespace}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentAccessPendingAnnotation),
			"successful unpublished-Secret cleanup must clear its reservation")
		var secrets corev1.SecretList
		Expect(k8sClient.List(ctx, &secrets,
			client.InNamespace(out.Namespace), client.MatchingLabels(agentLabels(out)))).To(Succeed())
		Expect(secrets.Items).To(BeEmpty(),
			"neither the revoked nor unpublished replacement Secret may remain")

		// envtest does not run the garbage collector that completes foreground
		// deletion, so release its synthetic finalizer before proving recovery.
		terminating.Finalizers = nil
		Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))
		}).Should(BeTrue())

		By("publishing a fresh credential and restoring the workload on retry")
		reconcileContainer(key.Name)
		out = getAgent(key.Name)
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef.Name).NotTo(Equal(oldRef.Name))
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: out.Status.Runtime.AuthSecretRef.Name, Namespace: out.Namespace,
		}, &corev1.Secret{})).To(Succeed())
		Expect(k8sClient.Get(ctx, key, &appsv1.Deployment{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: agentServiceName(out), Namespace: out.Namespace,
		}, &corev1.Service{})).To(Succeed())
	})

	It("stops the old workload when a published replacement credential reservation cannot be cleared", func() {
		makeContainerProvider("crewai-auth-reservation-clear-failure", "")
		makeContainerAgent("c-auth-reservation-clear-failure", "crewai-auth-reservation-clear-failure", "ghcr.io/x/crewai:poc")
		reconcileCore("c-auth-reservation-clear-failure")
		reconcileContainer("c-auth-reservation-clear-failure")

		key := types.NamespacedName{Name: "c-auth-reservation-clear-failure", Namespace: "default"}
		ad := getAgent(key.Name)
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		oldRef := *ad.Status.Runtime.AuthSecretRef

		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
		deployment.Finalizers = append(deployment.Finalizers, "tests.airunway.ai/hold-reservation-clear-workload")
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
		oldDeploymentUID := deployment.UID

		oldSecretKey := types.NamespacedName{Name: oldRef.Name, Namespace: ad.Namespace}
		Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: oldSecretKey.Name, Namespace: oldSecretKey.Namespace,
		}})).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, oldSecretKey, &corev1.Secret{}))
		}).Should(BeTrue())

		clearErr := fmt.Errorf("injected published reservation clear failure")
		failingClient := &failingPublishedReservationClearClient{Client: k8sClient, err: clearErr}
		r := &ContainerProviderReconciler{
			Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(clearErr.Error())))
		Expect(failingClient.failed).To(BeTrue())

		out := getAgent(key.Name)
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		newRef := *out.Status.Runtime.AuthSecretRef
		Expect(newRef.Name).NotTo(Equal(oldRef.Name),
			"the replacement random name must remain durably published")
		newSecretKey := types.NamespacedName{Name: newRef.Name, Namespace: out.Namespace}
		newSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, newSecretKey, newSecret)).To(Succeed())
		newSecretUID := newSecret.UID

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(out), Namespace: out.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentAccessPendingAnnotation, newRef.Name),
			"the failed clear must preserve the reservation for retry")

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: agentServiceName(out), Namespace: out.Namespace},
			&corev1.Service{}))).To(BeTrue(), "the stale token must no longer be routable")
		terminating := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, terminating)).To(Succeed())
		Expect(terminating.UID).To(Equal(oldDeploymentUID))
		Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse())

		By("clearing the retained reservation without allocating another Secret")
		reconcileContainer(key.Name)
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentAccessPendingAnnotation))
		Expect(k8sClient.Get(ctx, newSecretKey, newSecret)).To(Succeed())
		Expect(newSecret.UID).To(Equal(newSecretUID))
		Expect(currentAccessCredentialRef(getAgent(key.Name))).To(Equal(&newRef))
		var secrets corev1.SecretList
		Expect(k8sClient.List(ctx, &secrets,
			client.InNamespace(out.Namespace), client.MatchingLabels(agentLabels(out)))).To(Succeed())
		Expect(secrets.Items).To(HaveLen(1))

		terminating.Finalizers = nil
		Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
	})

	DescribeTable("cleans reserved access Secrets before deleting their recovery ConfigMap",
		func(suffix string, failDelete bool) {
			framework := "crewai-reservation-cleanup-" + suffix
			name := "c-reservation-cleanup-" + suffix
			makeContainerProvider(framework, "")
			makeContainerAgent(name, framework, "ghcr.io/x/crewai:poc")
			reconcileCore(name)

			publicationClient := &failingCredentialPublishClient{
				Client:    k8sClient,
				statusErr: fmt.Errorf("injected unpublished credential status failure"),
				deleteErr: fmt.Errorf("injected unpublished credential cleanup failure"),
			}
			key := types.NamespacedName{Name: name, Namespace: "default"}
			r := &ContainerProviderReconciler{
				Client: publicationClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

			ad := getAgent(name)
			Expect(currentAccessCredentialRef(ad)).To(BeNil())
			ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
			ledger := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
			reservedName := ledger.Annotations[agentAccessPendingAnnotation]
			Expect(reservedName).NotTo(BeEmpty())
			reservedKey := types.NamespacedName{Name: reservedName, Namespace: ad.Namespace}
			Expect(k8sClient.Get(ctx, reservedKey, &corev1.Secret{})).To(Succeed())

			By("making the agent terminally invalid before the reservation can be published")
			ad.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"port":"invalid"}`)}
			Expect(k8sClient.Update(ctx, ad)).To(Succeed())
			reconcileCore(name)

			var deleteClient *failingSecretDeleteClient
			var cleanupClient client.Client = k8sClient
			if failDelete {
				deleteClient = &failingSecretDeleteClient{
					Client: k8sClient, target: reservedKey, err: fmt.Errorf("injected reserved Secret deletion failure"),
				}
				cleanupClient = deleteClient
			}
			r = &ContainerProviderReconciler{
				Client: cleanupClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			}
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			if failDelete {
				Expect(err).To(MatchError(ContainSubstring("injected reserved Secret deletion failure")))
				Expect(deleteClient.attempts).To(Equal(1))
				preserved := &corev1.ConfigMap{}
				Expect(k8sClient.Get(ctx, ledgerKey, preserved)).To(Succeed())
				Expect(preserved.DeletionTimestamp.IsZero()).To(BeTrue(),
					"failed Secret deletion must preserve the recovery ConfigMap")
				Expect(preserved.Annotations).To(HaveKeyWithValue(agentAccessPendingAnnotation, reservedName))
				Expect(k8sClient.Get(ctx, reservedKey, &corev1.Secret{})).To(Succeed())

				By("retrying cleanup from the preserved reservation")
				r = &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
				_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			}
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, reservedKey, &corev1.Secret{}))
			}).Should(BeTrue(), "the unpublished reserved Secret must be deleted before its journal")

			current := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, ledgerKey, current); err == nil {
				Expect(current.DeletionTimestamp.IsZero()).To(BeFalse(),
					"ConfigMap deletion may start only after reserved Secret cleanup succeeds")
				Expect(current.Annotations).NotTo(HaveKey(agentAccessPendingAnnotation))
			} else {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		},
		Entry("deletes a live reserved Secret first", "success", false),
		Entry("preserves the journal when reserved Secret deletion fails", "retry", true),
	)

	It("waits for authoritative NotFound when deleting an access Secret", func() {
		makeContainerProvider("crewai-secret-delete-barrier", "")
		makeContainerAgent("c-secret-delete-barrier", "crewai-secret-delete-barrier", "ghcr.io/x/crewai:poc")
		reconcileCore("c-secret-delete-barrier")
		reconcileContainer("c-secret-delete-barrier")

		ad := getAgent("c-secret-delete-barrier")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		ref := *ad.Status.Runtime.AuthSecretRef
		key := types.NamespacedName{Name: ref.Name, Namespace: ad.Namespace}
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
		uid := secret.UID
		secret.Finalizers = []string{"test.airunway.ai/hold-access-secret-delete"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		for range 2 {
			Expect(r.deleteAgentAccessSecret(ctx, ad, &ref)).To(MatchError(ContainSubstring("deletion is still pending")))
			current := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			Expect(current.UID).To(Equal(uid))
			Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())
		}

		Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
		secret.Finalizers = nil
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &corev1.Secret{}))
		}).Should(BeTrue())
		Expect(r.deleteAgentAccessSecret(ctx, ad, &ref)).To(Succeed())
	})

	It("preserves a reserved access Secret journal while deletion is pending", func() {
		makeContainerProvider("crewai-reservation-delete-barrier", "")
		makeContainerAgent("c-reservation-delete-barrier", "crewai-reservation-delete-barrier", "ghcr.io/x/crewai:poc")
		reconcileCore("c-reservation-delete-barrier")
		reconcileContainer("c-reservation-delete-barrier")

		ad := getAgent("c-reservation-delete-barrier")
		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		rawToken := make([]byte, agentAccessTokenBytes)
		for i := range rawToken {
			rawToken[i] = 0x6b
		}
		token := []byte(base64.RawURLEncoding.EncodeToString(rawToken))
		reservedName := agentAccessSecretName(ad, token)
		reservedKey := types.NamespacedName{Name: reservedName, Namespace: ad.Namespace}
		reserved, err := r.renderAgentAccessSecret(ad, reservedName, token)
		Expect(err).NotTo(HaveOccurred())
		reserved.Finalizers = []string{"test.airunway.ai/hold-reserved-access-secret"}
		Expect(k8sClient.Create(ctx, reserved)).To(Succeed())
		reservedUID := reserved.UID

		startedAt := time.Now().UTC()
		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		base := ledger.DeepCopy()
		if ledger.Annotations == nil {
			ledger.Annotations = map[string]string{}
		}
		ledger.Annotations[agentAccessPendingAnnotation] = reservedName
		ledger.Annotations[agentAccessCreateStartedAnnotation] = reservedName
		ledger.Annotations[agentAccessCreateStartedAtAnnotation] = startedAt.Format(time.RFC3339Nano)
		Expect(k8sClient.Patch(ctx, ledger, client.MergeFrom(base))).To(Succeed())

		Expect(r.cleanupAgentAccessSecretReservation(ctx, ad)).To(MatchError(ContainSubstring("deletion is still pending")))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentAccessPendingAnnotation, reservedName))
		current := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, reservedKey, current)).To(Succeed())
		Expect(current.UID).To(Equal(reservedUID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())

		Expect(r.cleanupAgentAccessSecretReservation(ctx, ad)).To(MatchError(ContainSubstring("terminating")))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentAccessPendingAnnotation, reservedName))

		current.Finalizers = nil
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, reservedKey, &corev1.Secret{}))
		}).Should(BeTrue())

		r.agentAccessNow = func() time.Time {
			return startedAt.Add(agentAccessSecretAmbiguityGrace + time.Second)
		}
		Expect(r.cleanupAgentAccessSecretReservation(ctx, ad)).To(Succeed())
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentAccessPendingAnnotation))
	})

	It("stops an existing Deployment when Service reconciliation fails", func() {
		makeContainerProvider("crewai-service-fail-closed", "")
		makeContainerAgent("c-service-fail-closed", "crewai-service-fail-closed", "ghcr.io/x/crewai:poc")
		reconcileCore("c-service-fail-closed")
		reconcileContainer("c-service-fail-closed")

		ad := getAgent("c-service-fail-closed")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		accessRef := *ad.Status.Runtime.AuthSecretRef
		accessKey := types.NamespacedName{Name: accessRef.Name, Namespace: ad.Namespace}
		Expect(k8sClient.Get(ctx, accessKey, &corev1.Secret{})).To(Succeed())
		deploymentKey := client.ObjectKeyFromObject(ad)
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deploymentKey, deployment)).To(Succeed())
		deployment.Finalizers = append(deployment.Finalizers, "test.airunway.ai/hold-service-failure-deployment")
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
		serviceKey := types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}
		Expect(k8sClient.Get(ctx, serviceKey, &corev1.Service{})).To(Succeed())

		applyErr := fmt.Errorf("injected Service ownership failure")
		failingClient := &failingWorkloadApplyClient{Client: k8sClient, target: "Service", err: applyErr}
		r := &ContainerProviderReconciler{Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring(applyErr.Error())))

		Eventually(func(g Gomega) {
			deployment := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, deploymentKey, deployment)
			if apierrors.IsNotFound(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.DeletionTimestamp.IsZero()).To(BeFalse())
		}).Should(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serviceKey, &corev1.Service{}))).To(BeTrue(),
			"Service apply failure must remove routing while the old Deployment is foreground-deleting")
		out := getAgent(ad.Name)
		Expect(prCond(out).Reason).To(Equal("RenderFailed"))
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).To(Equal(&accessRef))
		Expect(k8sClient.Get(ctx, accessKey, &corev1.Secret{})).To(Succeed(),
			"the retryable ingress credential must remain reachable from status")

		terminating := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, deploymentKey, terminating); err == nil {
			terminating.Finalizers = nil
			Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, deploymentKey, &appsv1.Deployment{}))
		}).Should(BeTrue())
	})

	It("preserves the ingress Secret reference when terminal cleanup cannot delete it", func() {
		makeContainerProvider("crewai-cleanup-secret", "")
		makeContainerAgent("c-cleanup-secret", "crewai-cleanup-secret", "ghcr.io/x/crewai:poc")
		reconcileCore("c-cleanup-secret")
		reconcileContainer("c-cleanup-secret")

		ad := getAgent("c-cleanup-secret")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		accessRef := *ad.Status.Runtime.AuthSecretRef
		accessKey := types.NamespacedName{Name: accessRef.Name, Namespace: ad.Namespace}
		ad.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"port":"invalid"}`)}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)

		deleteErr := fmt.Errorf("injected ingress Secret deletion failure")
		failingClient := &failingSecretDeleteClient{Client: k8sClient, target: accessKey, err: deleteErr}
		r := &ContainerProviderReconciler{Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring(deleteErr.Error())))
		Expect(failingClient.attempts).To(Equal(1))

		out := getAgent(ad.Name)
		Expect(prCond(out).Reason).To(Equal("InvalidConfigCleanupFailed"))
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).To(Equal(&accessRef),
			"failed deletion must not discard the only durable pointer to the random Secret")
		Expect(k8sClient.Get(ctx, accessKey, &corev1.Secret{})).To(Succeed())
	})

	It("renders Deployment + Service + ConfigMap and tracks readiness", func() {
		makeContainerProvider("crewai-run", "")
		makeContainerAgent("c-run", "crewai-run", "ghcr.io/x/crewai:poc")

		reconcileCore("c-run")
		reconcileContainer("c-run")

		By("creating the ConfigMap with the mounted agent.json")
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run-config", Namespace: "default"}, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKey(agentConfigFileName))

		By("creating an independently scoped access token for the outward endpoint")
		ad := getAgent("c-run")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		accessRef := ad.Status.Runtime.AuthSecretRef
		Expect(accessRef.Name).To(HavePrefix("c-run-api-auth-"))
		accessSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: accessRef.Name, Namespace: "default"}, accessSecret)).To(Succeed())
		Expect(accessSecret.OwnerReferences).To(HaveLen(1))
		Expect(accessSecret.OwnerReferences[0].Name).To(Equal("c-run"))
		Expect(accessSecret.Immutable).NotTo(BeNil())
		Expect(*accessSecret.Immutable).To(BeTrue())
		accessToken := accessSecret.Data[agentAccessTokenKey]
		Expect(accessToken).To(HaveLen(43), "32 random bytes should be encoded as unpadded base64url")
		accessDigest := sha256.Sum256(accessToken)

		By("creating the Deployment with the BYO image and injected bindings")
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run", Namespace: "default"}, dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/crewai:poc"))
		Expect(dep.OwnerReferences).To(HaveLen(1))
		Expect(dep.OwnerReferences[0].Name).To(Equal("c-run"))
		Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(agentAccessChecksumAnnotation, fmt.Sprintf("%x", accessDigest)))
		var accessEnv *corev1.EnvVar
		for i := range dep.Spec.Template.Spec.Containers[0].Env {
			if dep.Spec.Template.Spec.Containers[0].Env[i].Name == agentAccessTokenEnv {
				accessEnv = &dep.Spec.Template.Spec.Containers[0].Env[i]
				break
			}
		}
		Expect(accessEnv).NotTo(BeNil())
		Expect(accessEnv.ValueFrom.SecretKeyRef.Name).To(Equal(accessRef.Name))
		Expect(accessEnv.ValueFrom.SecretKeyRef.Key).To(Equal(agentAccessTokenKey))

		By("using a dedicated empty ServiceAccount instead of the namespace default")
		sa := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run-runtime", Namespace: "default"}, sa)).To(Succeed())
		Expect(sa.ImagePullSecrets).To(BeEmpty())
		Expect(dep.Spec.Template.Spec.ServiceAccountName).To(Equal(sa.Name))

		By("creating the Service")
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run", Namespace: "default"}, svc)).To(Succeed())

		By("reporting Deploying + ProviderReady=False while no replicas are available")
		ad = getAgent("c-run")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(prCond(ad).Status).To(Equal(metav1.ConditionFalse))
		Expect(ad.Status.Runtime.Address).To(Equal("http://c-run.default.svc"))
		// Core-owned fields survive the provider write.
		Expect(ad.Status.ModelBinding).NotTo(BeNil())
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).To(Equal(&airunwayv1alpha1.SecretKeyRef{
			Name: accessRef.Name, Key: agentAccessTokenKey,
		}))

		By("staying Deploying while the Deployment status still describes the previous generation")
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		dep.Status.UpdatedReplicas = 0
		dep.Status.ObservedGeneration = dep.Generation - 1
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		reconcileContainer("c-run")
		ad = getAgent("c-run")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(prCond(ad).Status).To(Equal(metav1.ConditionFalse))

		By("flipping to Running + ProviderReady=True once the current generation has rolled out")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-run", Namespace: "default"}, dep)).To(Succeed())
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		dep.Status.UpdatedReplicas = 1
		dep.Status.ObservedGeneration = dep.Generation
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		reconcileContainer("c-run")
		ad = getAgent("c-run")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning))
		Expect(prCond(ad).Status).To(Equal(metav1.ConditionTrue))
		Expect(ad.Status.Replicas).NotTo(BeNil())
		Expect(ad.Status.Replicas.Available).To(Equal(int32(1)))
	})

	It("rotates ingress credentials immediately when the watched auth Secret is deleted", func() {
		makeContainerProvider("crewai-auth-delete", "")
		makeContainerAgent("c-auth-delete", "crewai-auth-delete", "ghcr.io/x/crewai:poc")
		reconcileCore("c-auth-delete")
		reconcileContainer("c-auth-delete")

		ad := getAgent("c-auth-delete")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		oldRef := *ad.Status.Runtime.AuthSecretRef
		oldSecretKey := types.NamespacedName{Name: oldRef.Name, Namespace: ad.Namespace}
		oldSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, oldSecretKey, oldSecret)).To(Succeed())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), dep)).To(Succeed())
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		dep.Status.UpdatedReplicas = 1
		dep.Status.ObservedGeneration = dep.Generation
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		reconcileContainer(ad.Name)
		ad = getAgent(ad.Name)
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning))

		oldChecksum := dep.Spec.Template.Annotations[agentAccessChecksumAnnotation]
		Expect(oldChecksum).NotTo(BeEmpty())
		deletedMetadata := &metav1.PartialObjectMetadata{
			ObjectMeta: metav1.ObjectMeta{Name: oldSecret.Name, Namespace: oldSecret.Namespace},
		}
		Expect(k8sClient.Delete(ctx, oldSecret)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, oldSecretKey, &corev1.Secret{}))
		}).Should(BeTrue())

		// Use a metadata-only deletion object with no owner reference. The exact
		// auth-status index must still enqueue the stable AgentDeployment.
		mapperClient := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithObjects(ad.DeepCopy()).
			WithIndex(&airunwayv1alpha1.AgentDeployment{}, agentCredentialSecretIndexKey, credentialSecretIndex).
			Build()
		mapper := &ContainerProviderReconciler{Client: mapperClient}
		reqs := mapper.mapCredentialSecretToAgentDeployments(ctx, deletedMetadata)
		Expect(reqs).To(ConsistOf(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)}))

		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		for _, req := range reqs {
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		}

		out := getAgent(ad.Name)
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		newRef := *out.Status.Runtime.AuthSecretRef
		Expect(newRef.Name).NotTo(Equal(oldRef.Name))
		rotated := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: newRef.Name, Namespace: out.Namespace}, rotated)).To(Succeed())
		Expect(hasExactBlockingControllerOwner(rotated, out)).To(BeTrue())

		By("stopping the old Deployment before applying the rotated credential")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(out), dep)).To(Succeed())
		Expect(dep.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(dep.Spec.Template.Annotations[agentAccessChecksumAnnotation]).To(Equal(oldChecksum))
		dep.Finalizers = nil
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		_ = k8sClient.Delete(ctx, dep)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(out), &appsv1.Deployment{}))
		}).Should(BeTrue())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(out)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(out), dep)).To(Succeed())
		Expect(dep.Spec.Template.Annotations[agentAccessChecksumAnnotation]).NotTo(Equal(oldChecksum))
		var accessEnv *corev1.EnvVar
		for i := range dep.Spec.Template.Spec.Containers[0].Env {
			if dep.Spec.Template.Spec.Containers[0].Env[i].Name == agentAccessTokenEnv {
				accessEnv = &dep.Spec.Template.Spec.Containers[0].Env[i]
				break
			}
		}
		Expect(accessEnv).NotTo(BeNil())
		Expect(accessEnv.ValueFrom.SecretKeyRef.Name).To(Equal(newRef.Name))
	})

	It("revokes an invalid ingress credential even while the model binding is stale", func() {
		makeContainerProvider("crewai-auth-stale", "")

		for _, tc := range []struct {
			name        string
			terminating bool
		}{
			{name: "c-auth-stale-deleted"},
			{name: "c-auth-stale-terminating", terminating: true},
		} {
			By("creating a running agent for " + tc.name)
			makeContainerAgent(tc.name, "crewai-auth-stale", "ghcr.io/x/crewai:poc")
			reconcileCore(tc.name)
			reconcileContainer(tc.name)

			ad := getAgent(tc.name)
			Expect(ad.Status.Runtime).NotTo(BeNil())
			Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
			ref := *ad.Status.Runtime.AuthSecretRef
			secretKey := types.NamespacedName{Name: ref.Name, Namespace: ad.Namespace}

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), dep)).To(Succeed())
			dep.Finalizers = append(dep.Finalizers, "tests.airunway.ai/hold-stale-credential-workload")
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())
			originalUID := dep.UID

			markBindingStale(ad)

			By("holding the workload while the published ingress Secret is still valid")
			reconcileContainer(tc.name)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), dep)).To(Succeed())
			Expect(dep.UID).To(Equal(originalUID))
			Expect(dep.DeletionTimestamp.IsZero()).To(BeTrue())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}, &corev1.Service{})).To(Succeed())

			By("revoking the published ingress Secret")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
			if tc.terminating {
				secret.Finalizers = append(secret.Finalizers, "tests.airunway.ai/hold-stale-credential-secret")
				Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			}
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			if tc.terminating {
				Eventually(func() bool {
					current := &corev1.Secret{}
					return k8sClient.Get(ctx, secretKey, current) == nil && !current.DeletionTimestamp.IsZero()
				}).Should(BeTrue())
			} else {
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))
				}).Should(BeTrue())
			}

			reconcileContainer(tc.name)

			By("removing routing and stopping the exact-owned Deployment without rotating during the stale hold")
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx,
					types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}, &corev1.Service{}))
			}).Should(BeTrue())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), dep)).To(Succeed())
			Expect(dep.UID).To(Equal(originalUID))
			Expect(dep.DeletionTimestamp.IsZero()).To(BeFalse())
			out := getAgent(tc.name)
			Expect(out.Status.Runtime).NotTo(BeNil())
			Expect(out.Status.Runtime.AuthSecretRef).To(Equal(&ref),
				"the stale-binding path must retain the only durable random Secret reference")

			dep.Finalizers = nil
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())
			if tc.terminating {
				current := &corev1.Secret{}
				Expect(k8sClient.Get(ctx, secretKey, current)).To(Succeed())
				current.Finalizers = nil
				Expect(k8sClient.Update(ctx, current)).To(Succeed())
			}
		}
	})

	It("stops a deterministic legacy ingress credential while the model binding is stale", func() {
		const framework = "crewai-auth-stale-legacy"
		const name = "c-auth-stale-legacy"
		makeContainerProvider(framework, "")
		makeContainerAgent(name, framework, "ghcr.io/x/crewai:poc")
		reconcileCore(name)
		reconcileContainer(name)

		ad := getAgent(name)
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		randomRef := *ad.Status.Runtime.AuthSecretRef
		randomKey := types.NamespacedName{Name: randomRef.Name, Namespace: ad.Namespace}
		randomSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, randomKey, randomSecret)).To(Succeed())
		Expect(k8sClient.Delete(ctx, randomSecret)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, randomKey, &corev1.Secret{}))
		}).Should(BeTrue())

		legacyRaw := make([]byte, agentAccessTokenBytes)
		for i := range legacyRaw {
			legacyRaw[i] = byte(i + 1)
		}
		legacyToken := []byte(base64.RawURLEncoding.EncodeToString(legacyRaw))
		legacy := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      legacyAgentAccessSecretName(ad),
				Namespace: ad.Namespace,
				Labels:    agentLabels(ad),
			},
			Immutable: ptr.To(true),
			Type:      corev1.SecretTypeOpaque,
			Data:      map[string][]byte{agentAccessTokenKey: legacyToken},
		}
		Expect(controllerutil.SetControllerReference(ad, legacy, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		legacyKey := client.ObjectKeyFromObject(legacy)

		legacyRef, legacyChecksum, err := agentAccessCredentialResult(legacy.Name, legacyToken)
		Expect(err).NotTo(HaveOccurred())
		deploymentKey := client.ObjectKeyFromObject(ad)
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deploymentKey, deployment)).To(Succeed())
		deployment.Finalizers = append(deployment.Finalizers, "tests.airunway.ai/hold-stale-legacy-credential")
		deployment.Spec.Template.Annotations[agentAccessChecksumAnnotation] = legacyChecksum
		foundAccessEnv := false
		for i := range deployment.Spec.Template.Spec.Containers[0].Env {
			env := &deployment.Spec.Template.Spec.Containers[0].Env[i]
			if env.Name == agentAccessTokenEnv {
				env.ValueFrom.SecretKeyRef.Name = legacyRef.Name
				env.ValueFrom.SecretKeyRef.Key = legacyRef.Key
				foundAccessEnv = true
				break
			}
		}
		Expect(foundAccessEnv).To(BeTrue())
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
		deploymentUID := deployment.UID

		legacyRuntime := *ad.Status.Runtime
		legacyRuntime.AuthSecretRef = legacyRef
		Expect(agentprovider.ApplyOwnedStatus(ctx, k8sClient, ad, ContainerFieldOwner,
			airunwayv1alpha1.AgentPhaseRunning, &legacyRuntime, ad.Status.Replicas,
			metav1.ConditionTrue, "WorkloadReady", "legacy access credential is active")).To(Succeed())
		ad = getAgent(name)
		markBindingStale(ad)

		By("removing routing and stopping the legacy workload without rotating during the stale hold")
		reconcileContainer(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace},
			&corev1.Service{}))).To(BeTrue())
		terminating := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deploymentKey, terminating)).To(Succeed())
		Expect(terminating.UID).To(Equal(deploymentUID))
		Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse())
		out := getAgent(name)
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).To(Equal(legacyRef),
			"BindingStale must retain the legacy reference until a verified binding can rotate it")
		Expect(k8sClient.Get(ctx, legacyKey, &corev1.Secret{})).To(Succeed(),
			"the legacy Secret must remain while terminating pods may still consume it")
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: agentConfigMapName(ad), Namespace: ad.Namespace,
		}, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentAccessPendingAnnotation),
			"the stale-binding path must not reserve a replacement credential")

		terminating.Finalizers = nil
		Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
	})

	It("stops exact-owned workloads when model credentials change while the binding is stale", func() {
		makeContainerProvider("crewai-model-credential-stale", "")

		for _, tc := range []struct {
			name string
			job  bool
		}{
			{name: "c-model-credential-stale-deployment"},
			{name: "c-model-credential-stale-job", job: true},
		} {
			By("creating a credential-bearing workload for " + tc.name)
			credential := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: tc.name + "-model", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("test-only-model-key-one")},
			}
			Expect(k8sClient.Create(ctx, credential)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, credential) })

			if tc.job {
				makeContainerJobAgent(tc.name, "crewai-model-credential-stale", "ghcr.io/x/task:poc")
			} else {
				makeContainerAgent(tc.name, "crewai-model-credential-stale", "ghcr.io/x/crewai:poc")
			}
			ad := getAgent(tc.name)
			ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
				Name: credential.Name,
				Key:  "api-key",
			}
			Expect(k8sClient.Update(ctx, ad)).To(Succeed())
			reconcileCore(tc.name)
			reconcileContainer(tc.name)

			ad = getAgent(tc.name)
			key := client.ObjectKeyFromObject(ad)
			var originalUID types.UID
			if tc.job {
				job := &batchv1.Job{}
				Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
				Expect(job.Spec.Template.Annotations[agentModelCredentialChecksumAnnotation]).NotTo(BeEmpty())
				job.Finalizers = append(job.Finalizers, "tests.airunway.ai/hold-stale-model-credential-job")
				Expect(k8sClient.Update(ctx, job)).To(Succeed())
				originalUID = job.UID

				By("removing the Job claim to reproduce pre-ledger execution evidence")
				ledger := &corev1.ConfigMap{}
				ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
				Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
				delete(ledger.Annotations, agentJobGenerationAnnotation)
				delete(ledger.Annotations, agentJobOutcomeAnnotation)
				delete(ledger.Annotations, agentJobClaimNonceAnnotation)
				Expect(k8sClient.Update(ctx, ledger)).To(Succeed())
			} else {
				deployment := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
				Expect(deployment.Spec.Template.Annotations[agentModelCredentialChecksumAnnotation]).NotTo(BeEmpty())
				deployment.Finalizers = append(deployment.Finalizers, "tests.airunway.ai/hold-stale-model-credential-deployment")
				Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
				originalUID = deployment.UID
			}

			markBindingStale(ad)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(credential), credential)).To(Succeed())
			credential.Data["api-key"] = []byte("test-only-model-key-two")
			Expect(k8sClient.Update(ctx, credential)).To(Succeed())

			By("stopping the stale credential-bearing workload without rendering a replacement")
			reconcileContainer(tc.name)
			if tc.job {
				ledger := &corev1.ConfigMap{}
				ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
				Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
				Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)),
					"execution evidence must be durable before the Job is stopped")

				job := &batchv1.Job{}
				Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
				Expect(job.UID).To(Equal(originalUID))
				Expect(job.DeletionTimestamp.IsZero()).To(BeFalse())
				reconcileContainer(tc.name)
				Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
				Expect(job.UID).To(Equal(originalUID), "BindingStale must never launch a replacement Job")
				job.Finalizers = nil
				Expect(k8sClient.Update(ctx, job)).To(Succeed())
			} else {
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx,
						types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}, &corev1.Service{}))
				}).Should(BeTrue())
				deployment := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
				Expect(deployment.UID).To(Equal(originalUID))
				Expect(deployment.DeletionTimestamp.IsZero()).To(BeFalse())
				reconcileContainer(tc.name)
				Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
				Expect(deployment.UID).To(Equal(originalUID), "BindingStale must never launch a replacement Deployment")
				deployment.Finalizers = nil
				Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
			}
		}
	})

	It("records a terminal legacy Job before holding a stale binding", func() {
		const framework = "crewai-stale-terminal-job"
		const name = "c-stale-terminal-job"
		makeContainerProvider(framework, "")
		makeContainerJobAgent(name, framework, "ghcr.io/x/task:poc")
		reconcileCore(name)
		reconcileContainer(name)

		ad := getAgent(name)
		Expect(ad.Generation).To(Equal(int64(1)))
		key := client.ObjectKeyFromObject(ad)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		delete(job.Annotations, agentJobGenerationAnnotation)
		Expect(k8sClient.Update(ctx, job)).To(Succeed())
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		delete(ledger.Annotations, agentJobClaimNonceAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		By("reproducing a crash before the legacy provider wrote Job-specific status")
		ad = getAgent(name)
		Expect(agentprovider.ReleaseOwnedStatus(ctx, k8sClient, ad, ContainerFieldOwner)).To(Succeed())
		ad = getAgent(name)
		markBindingStale(ad)

		By("persisting terminal execution evidence without deleting the held Job")
		reconcileContainer(name)
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted))
		currentJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, currentJob)).To(Succeed())
		Expect(currentJob.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(currentJob.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))

		By("deleting the held Job and restoring the binding without authorizing a rerun")
		Expect(k8sClient.Delete(ctx, currentJob, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())
		ad = getAgent(name)
		modelBound := meta.FindStatusCondition(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeModelBound)
		Expect(modelBound).NotTo(BeNil())
		modelBound.Status = metav1.ConditionTrue
		modelBound.Reason = "BindingResolved"
		modelBound.Message = "test binding restored"
		modelBound.ObservedGeneration = ad.Generation
		modelBound.LastTransitionTime = metav1.Now()
		Expect(k8sClient.Status().Update(ctx, ad)).To(Succeed())
		Expect(agentprovider.ClassifyBinding(getAgent(name))).To(Equal(agentprovider.BindingReady))
		reconcileContainer(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue())
		out := getAgent(name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
		Expect(prCond(out).Reason).To(Equal("JobCompleted"))
	})

	It("removes an exact-owned Service while holding a stale binding after its Deployment disappears", func() {
		const framework = "crewai-stale-service-without-deployment"
		const name = "c-stale-service-without-deployment"
		makeContainerProvider(framework, "")
		makeContainerAgent(name, framework, "ghcr.io/x/crewai:poc")
		reconcileCore(name)
		reconcileContainer(name)

		ad := getAgent(name)
		key := client.ObjectKeyFromObject(ad)
		serviceKey := types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}
		service := &corev1.Service{}
		Expect(k8sClient.Get(ctx, serviceKey, service)).To(Succeed())
		service.Finalizers = append(service.Finalizers, "tests.airunway.ai/hold-stale-orphan-service")
		Expect(k8sClient.Update(ctx, service)).To(Succeed())

		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
		Expect(k8sClient.Delete(ctx, deployment, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))
		}).Should(BeTrue())

		ad = getAgent(name)
		markBindingStale(ad)
		reconcileContainer(name)

		terminating := &corev1.Service{}
		Expect(k8sClient.Get(ctx, serviceKey, terminating)).To(Succeed())
		Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse(),
			"BindingStale must not retain a routing boundary without its exact-owned Deployment")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"BindingStale must not render a successor workload")

		terminating.Finalizers = nil
		Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
	})

	It("validates the actual Deployment after lifecycle changes to Job while the binding is stale", func() {
		makeContainerProvider("crewai-stale-deployment-to-job", "")
		makeContainerAgent("c-stale-deployment-to-job", "crewai-stale-deployment-to-job", "ghcr.io/x/crewai:poc")
		reconcileCore("c-stale-deployment-to-job")
		reconcileContainer("c-stale-deployment-to-job")

		ad := getAgent("c-stale-deployment-to-job")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		ref := *ad.Status.Runtime.AuthSecretRef
		key := client.ObjectKeyFromObject(ad)
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
		deployment.Finalizers = append(deployment.Finalizers, "tests.airunway.ai/hold-stale-deployment-to-job")
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
		originalUID := deployment.UID

		ad.Spec.Lifecycle = airunwayv1alpha1.AgentLifecycleJob
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		ad = getAgent(ad.Name)
		markBindingStale(ad)

		secretKey := types.NamespacedName{Name: ref.Name, Namespace: ad.Namespace}
		Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: secretKey.Name, Namespace: secretKey.Namespace,
		}})).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))
		}).Should(BeTrue())

		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace},
			&corev1.Service{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
		Expect(deployment.UID).To(Equal(originalUID))
		Expect(deployment.DeletionTimestamp.IsZero()).To(BeFalse(),
			"desired Job lifecycle must not hide the actual stale Deployment")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"BindingStale must never render the desired successor")

		deployment.Finalizers = nil
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
	})

	It("validates and claims the actual Job after lifecycle changes to Deployment while the binding is stale", func() {
		makeContainerProvider("crewai-stale-job-to-deployment", "")
		credential := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-stale-job-to-deployment-model", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("test-only-model-key-one")},
		}
		Expect(k8sClient.Create(ctx, credential)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, credential) })

		makeContainerJobAgent("c-stale-job-to-deployment", "crewai-stale-job-to-deployment", "ghcr.io/x/task:poc")
		ad := getAgent("c-stale-job-to-deployment")
		ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{Name: credential.Name, Key: "api-key"}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		reconcileContainer(ad.Name)

		ad = getAgent(ad.Name)
		oldGeneration := ad.Generation
		key := client.ObjectKeyFromObject(ad)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Finalizers = append(job.Finalizers, "tests.airunway.ai/hold-stale-job-to-deployment")
		Expect(k8sClient.Update(ctx, job)).To(Succeed())
		job.Status.Active = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		originalUID := job.UID

		ad.Spec.Lifecycle = airunwayv1alpha1.AgentLifecycleDeployment
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		ad = getAgent(ad.Name)
		Expect(ad.Generation).To(BeNumerically(">", oldGeneration))
		markBindingStale(ad)

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(credential), credential)).To(Succeed())
		credential.Data["api-key"] = []byte("test-only-model-key-two")
		Expect(k8sClient.Update(ctx, credential)).To(Succeed())

		reconcileContainer(ad.Name)
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		Expect(job.UID).To(Equal(originalUID))
		Expect(job.DeletionTimestamp.IsZero()).To(BeFalse(),
			"desired Deployment lifecycle must not hide the actual stale Job")
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: agentConfigMapName(ad), Namespace: ad.Namespace,
		}, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(oldGeneration)),
			"the old Job execution evidence must survive stale-binding cleanup")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))).To(BeTrue(),
			"BindingStale must never render the desired successor")

		job.Finalizers = nil
		Expect(k8sClient.Update(ctx, job)).To(Succeed())
	})

	It("rotates ingress credentials while the referenced Secret is terminating", func() {
		makeContainerProvider("crewai-auth-terminating", "")
		makeContainerAgent("c-auth-terminating", "crewai-auth-terminating", "ghcr.io/x/crewai:poc")
		reconcileCore("c-auth-terminating")
		reconcileContainer("c-auth-terminating")

		ad := getAgent("c-auth-terminating")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		oldRef := *ad.Status.Runtime.AuthSecretRef
		oldKey := types.NamespacedName{Name: oldRef.Name, Namespace: ad.Namespace}
		oldSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, oldKey, oldSecret)).To(Succeed())
		oldSecret.Finalizers = append(oldSecret.Finalizers, "tests.airunway.ai/hold-deletion")
		Expect(k8sClient.Update(ctx, oldSecret)).To(Succeed())
		Expect(k8sClient.Delete(ctx, oldSecret)).To(Succeed())
		Eventually(func() bool {
			current := &corev1.Secret{}
			return k8sClient.Get(ctx, oldKey, current) == nil && !current.DeletionTimestamp.IsZero()
		}).Should(BeTrue())
		DeferCleanup(func() {
			current := &corev1.Secret{}
			if err := k8sClient.Get(ctx, oldKey, current); err == nil {
				current.Finalizers = nil
				_ = k8sClient.Update(ctx, current)
				_ = k8sClient.Delete(ctx, current)
			}
		})

		reconcileContainer(ad.Name)
		out := getAgent(ad.Name)
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		newRef := *out.Status.Runtime.AuthSecretRef
		Expect(newRef.Name).NotTo(Equal(oldRef.Name))
		rotated := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: newRef.Name, Namespace: out.Namespace}, rotated)).To(Succeed())
		Expect(rotated.DeletionTimestamp.IsZero()).To(BeTrue())

		terminating := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, oldKey, terminating)).To(Succeed())
		Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(out), dep)).To(Succeed())
		Expect(dep.DeletionTimestamp.IsZero()).To(BeFalse())
		deployedName, _, _, found := deploymentAccessCredential(dep)
		Expect(found).To(BeTrue())
		Expect(deployedName).To(Equal(oldRef.Name),
			"the terminating workload must keep the retired credential until it is gone")
		dep.Finalizers = nil
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		_ = k8sClient.Delete(ctx, dep)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(out), &appsv1.Deployment{}))
		}).Should(BeTrue())

		reconcileContainer(out.Name)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(out), dep)).To(Succeed())
		var accessEnv *corev1.EnvVar
		for i := range dep.Spec.Template.Spec.Containers[0].Env {
			if dep.Spec.Template.Spec.Containers[0].Env[i].Name == agentAccessTokenEnv {
				accessEnv = &dep.Spec.Template.Spec.Containers[0].Env[i]
				break
			}
		}
		Expect(accessEnv).NotTo(BeNil())
		Expect(accessEnv.ValueFrom.SecretKeyRef.Name).To(Equal(newRef.Name))
	})

	It("retires published and reserved ingress Secrets only after a Deployment-to-Job switch stops the service", func() {
		makeContainerProvider("crewai-lifecycle-secret", "")
		makeContainerAgent("c-lifecycle-secret", "crewai-lifecycle-secret", "ghcr.io/x/crewai:poc")
		reconcileCore("c-lifecycle-secret")
		reconcileContainer("c-lifecycle-secret")

		ad := getAgent("c-lifecycle-secret")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		accessRef := *ad.Status.Runtime.AuthSecretRef
		accessKey := types.NamespacedName{Name: accessRef.Name, Namespace: ad.Namespace}
		Expect(k8sClient.Get(ctx, accessKey, &corev1.Secret{})).To(Succeed())

		By("recording an unpublished replacement credential in the retained ConfigMap journal")
		rawToken := make([]byte, agentAccessTokenBytes)
		for i := range rawToken {
			rawToken[i] = 0x5a
		}
		reservedToken := []byte(base64.RawURLEncoding.EncodeToString(rawToken))
		reservedName := agentAccessSecretName(ad, reservedToken)
		reservedKey := types.NamespacedName{Name: reservedName, Namespace: ad.Namespace}
		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		reservedSecret, err := r.renderAgentAccessSecret(ad, reservedName, reservedToken)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Create(ctx, reservedSecret)).To(Succeed())
		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		base := ledger.DeepCopy()
		if ledger.Annotations == nil {
			ledger.Annotations = map[string]string{}
		}
		ledger.Annotations[agentAccessPendingAnnotation] = reservedName
		ledger.Annotations[agentAccessCreateStartedAnnotation] = reservedName
		ledger.Annotations[agentAccessCreateStartedAtAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		Expect(k8sClient.Patch(ctx, ledger, client.MergeFrom(base))).To(Succeed())

		By("seeding a deterministic legacy credential that must follow the old workload lifetime")
		legacyRaw := make([]byte, agentAccessTokenBytes)
		for i := range legacyRaw {
			legacyRaw[i] = 0x31
		}
		legacyToken := []byte(base64.RawURLEncoding.EncodeToString(legacyRaw))
		legacy := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      legacyAgentAccessSecretName(ad),
				Namespace: ad.Namespace,
				Labels:    agentLabels(ad),
			},
			Immutable: ptr.To(true),
			Type:      corev1.SecretTypeOpaque,
			Data:      map[string][]byte{agentAccessTokenKey: legacyToken},
		}
		Expect(controllerutil.SetControllerReference(ad, legacy, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		legacyKey := client.ObjectKeyFromObject(legacy)

		ad.Spec.Lifecycle = airunwayv1alpha1.AgentLifecycleJob
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		reconcileContainer(ad.Name)

		By("retaining the unguessable Secret reference while the old Deployment is terminating")
		switching := getAgent(ad.Name)
		Expect(prCond(switching).Reason).To(Equal("LifecycleSwitching"))
		Expect(switching.Status.Runtime).NotTo(BeNil())
		Expect(switching.Status.Runtime.AuthSecretRef).To(Equal(&accessRef))
		Expect(k8sClient.Get(ctx, accessKey, &corev1.Secret{})).To(Succeed())
		Expect(k8sClient.Get(ctx, legacyKey, &corev1.Secret{})).To(Succeed(),
			"the legacy credential must remain while old Deployment pods may still be running")

		staleDeployment := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), staleDeployment); err == nil {
			Expect(staleDeployment.DeletionTimestamp.IsZero()).To(BeFalse())
			staleDeployment.Finalizers = nil
			Expect(k8sClient.Update(ctx, staleDeployment)).To(Succeed())
			_ = k8sClient.Delete(ctx, staleDeployment)
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), &appsv1.Deployment{}))
		}).Should(BeTrue())

		reconcileContainer(ad.Name)
		switching = getAgent(ad.Name)
		Expect(prCond(switching).Reason).To(Equal("LifecycleSwitching"))
		Expect(switching.Status.Runtime.AuthSecretRef).To(Equal(&accessRef),
			"the Service cleanup pass must not discard the random Secret name")
		staleService := &corev1.Service{}
		serviceKey := types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}
		if err := k8sClient.Get(ctx, serviceKey, staleService); err == nil {
			Expect(staleService.DeletionTimestamp.IsZero()).To(BeFalse())
			staleService.Finalizers = nil
			Expect(k8sClient.Update(ctx, staleService)).To(Succeed())
			_ = k8sClient.Delete(ctx, staleService)
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, serviceKey, &corev1.Service{}))
		}).Should(BeTrue())

		By("preserving the unpublished reservation when its Secret cannot be deleted")
		deleteErr := fmt.Errorf("injected reserved Secret deletion failure")
		deleteClient := &failingSecretDeleteClient{Client: k8sClient, target: reservedKey, err: deleteErr}
		r = &ContainerProviderReconciler{Client: deleteClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring(deleteErr.Error())))
		Expect(deleteClient.attempts).To(Equal(1))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), &batchv1.Job{}))).To(BeTrue(),
			"the Job must not start while reserved credential cleanup is incomplete")
		Expect(k8sClient.Get(ctx, reservedKey, &corev1.Secret{})).To(Succeed())
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentAccessPendingAnnotation, reservedName),
			"failed deletion must preserve the only durable pointer to the unpublished Secret")

		By("retaining the published reference until finalizer-held Secret deletion completes")
		published := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, accessKey, published)).To(Succeed())
		published.Finalizers = []string{"test.airunway.ai/hold-access-secret"}
		Expect(k8sClient.Update(ctx, published)).To(Succeed())
		r = &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		for range 2 {
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
			Expect(err).To(MatchError(ContainSubstring("deletion is still pending")))
			blocked := getAgent(ad.Name)
			Expect(blocked.Status.Runtime).NotTo(BeNil())
			Expect(blocked.Status.Runtime.AuthSecretRef).To(Equal(&accessRef),
				"a pending deletion must preserve the only durable random Secret reference")
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), &batchv1.Job{}))).To(BeTrue(),
				"the Job must not start while ingress Secret deletion is pending")
			Expect(k8sClient.Get(ctx, accessKey, published)).To(Succeed())
			Expect(published.DeletionTimestamp.IsZero()).To(BeFalse())
		}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentAccessPendingAnnotation),
			"the unpublished reservation may clear after its Secret is authoritatively deleted")

		published.Finalizers = nil
		Expect(k8sClient.Update(ctx, published)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, accessKey, &corev1.Secret{}))
		}).Should(BeTrue())

		By("starting the Job only after both ingress Secrets are authoritatively gone")
		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, accessKey, &corev1.Secret{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, reservedKey, &corev1.Secret{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, legacyKey, &corev1.Secret{}))).To(BeTrue(),
			"the deterministic legacy credential must be retired before the Job starts")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), &batchv1.Job{})).To(Succeed())
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentAccessPendingAnnotation))
		out := getAgent(ad.Name)
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).To(BeNil())
	})

	It("retires a stranded legacy ingress Secret only after generic workload cleanup completes", func() {
		makeContainerProvider("crewai-legacy-cleanup", "")
		makeContainerAgent("c-legacy-cleanup", "crewai-legacy-cleanup", "ghcr.io/x/crewai:poc")
		reconcileCore("c-legacy-cleanup")
		reconcileContainer("c-legacy-cleanup")

		ad := getAgent("c-legacy-cleanup")
		legacyRaw := make([]byte, agentAccessTokenBytes)
		for i := range legacyRaw {
			legacyRaw[i] = 0x42
		}
		legacyToken := []byte(base64.RawURLEncoding.EncodeToString(legacyRaw))
		legacy := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      legacyAgentAccessSecretName(ad),
				Namespace: ad.Namespace,
				Labels:    agentLabels(ad),
			},
			Immutable: ptr.To(true),
			Type:      corev1.SecretTypeOpaque,
			Data:      map[string][]byte{agentAccessTokenKey: legacyToken},
		}
		Expect(controllerutil.SetControllerReference(ad, legacy, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		legacyKey := client.ObjectKeyFromObject(legacy)

		deploymentKey := client.ObjectKeyFromObject(ad)
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deploymentKey, deployment)).To(Succeed())
		deployment.Finalizers = append(deployment.Finalizers, "test.airunway.ai/hold-legacy-cleanup-deployment")
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		pending, err := r.cleanupOwnedWorkloadsForBinding(ctx, ad)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeTrue())
		Expect(k8sClient.Get(ctx, legacyKey, &corev1.Secret{})).To(Succeed(),
			"the deterministic credential must remain while an old Deployment may still consume it")

		terminating := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deploymentKey, terminating)).To(Succeed())
		Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse())
		terminating.Finalizers = nil
		Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, deploymentKey, &appsv1.Deployment{}))
		}).Should(BeTrue())

		// envtest has no garbage collector to complete the other foreground
		// deletions started by CleanupOwnedAndWait. Remove only those synthetic
		// finalizers before the authoritative completion pass.
		finishForegroundDeletion := func(key client.ObjectKey, obj client.Object) {
			err := k8sClient.Get(ctx, key, obj)
			if apierrors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.GetDeletionTimestamp().IsZero()).To(BeFalse())
			obj.SetFinalizers(nil)
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			Eventually(func() bool {
				fresh := obj.DeepCopyObject().(client.Object)
				return apierrors.IsNotFound(k8sClient.Get(ctx, key, fresh))
			}).Should(BeTrue())
		}
		finishForegroundDeletion(
			types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace},
			&corev1.Service{},
		)
		finishForegroundDeletion(
			types.NamespacedName{Name: agentServiceAccountName(ad), Namespace: ad.Namespace},
			&corev1.ServiceAccount{},
		)
		finishForegroundDeletion(
			types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace},
			&corev1.ConfigMap{},
		)

		Eventually(func(g Gomega) bool {
			pending, err = r.cleanupOwnedWorkloadsForBinding(ctx, ad)
			g.Expect(err).NotTo(HaveOccurred())
			return !pending
		}).Should(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, legacyKey, &corev1.Secret{}))).To(BeTrue(),
			"generic teardown must retire the deterministic credential after all workloads are gone")
	})

	It("keeps the legacy ingress Secret until its finalizer-held Deployment is replaced", func() {
		makeContainerProvider("crewai-auth-migration-held", "")
		makeContainerAgent("c-auth-migration-held", "crewai-auth-migration-held", "ghcr.io/x/crewai:poc")
		reconcileCore("c-auth-migration-held")
		reconcileContainer("c-auth-migration-held")

		ad := getAgent("c-auth-migration-held")
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		oldRandomKey := types.NamespacedName{Name: ad.Status.Runtime.AuthSecretRef.Name, Namespace: ad.Namespace}
		oldRandom := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, oldRandomKey, oldRandom)).To(Succeed())
		Expect(k8sClient.Delete(ctx, oldRandom)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, oldRandomKey, &corev1.Secret{}))
		}).Should(BeTrue())

		raw := make([]byte, agentAccessTokenBytes)
		for i := range raw {
			raw[i] = byte(i + 1)
		}
		token := []byte(base64.RawURLEncoding.EncodeToString(raw))
		legacy := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      legacyAgentAccessSecretName(ad),
				Namespace: ad.Namespace,
				Labels:    agentLabels(ad),
			},
			Immutable: ptr.To(true),
			Type:      corev1.SecretTypeOpaque,
			Data:      map[string][]byte{agentAccessTokenKey: token},
		}
		Expect(controllerutil.SetControllerReference(ad, legacy, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		legacyKey := client.ObjectKeyFromObject(legacy)

		legacyRef, legacyChecksum, err := agentAccessCredentialResult(legacy.Name, token)
		Expect(err).NotTo(HaveOccurred())
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), deployment)).To(Succeed())
		deployment.Finalizers = append(deployment.Finalizers, "tests.airunway.ai/hold-legacy-deployment")
		deployment.Spec.Template.Annotations[agentAccessChecksumAnnotation] = legacyChecksum
		foundAccessEnv := false
		for i := range deployment.Spec.Template.Spec.Containers[0].Env {
			env := &deployment.Spec.Template.Spec.Containers[0].Env[i]
			if env.Name == agentAccessTokenEnv {
				env.ValueFrom.SecretKeyRef.Name = legacyRef.Name
				env.ValueFrom.SecretKeyRef.Key = legacyRef.Key
				foundAccessEnv = true
				break
			}
		}
		Expect(foundAccessEnv).To(BeTrue())
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

		legacyRuntime := *ad.Status.Runtime
		legacyRuntime.AuthSecretRef = legacyRef
		Expect(agentprovider.ApplyOwnedStatus(ctx, k8sClient, ad, ContainerFieldOwner,
			airunwayv1alpha1.AgentPhaseRunning, &legacyRuntime, ad.Status.Replicas,
			metav1.ConditionTrue, "WorkloadReady", "legacy access credential is active")).To(Succeed())

		By("rotating status while the old Deployment remains foreground-deleting")
		reconcileContainer(ad.Name)
		out := getAgent(ad.Name)
		Expect(prCond(out).Reason).To(Equal("WorkloadReplacing"))
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		migratedRef := *out.Status.Runtime.AuthSecretRef
		Expect(migratedRef.Name).NotTo(Equal(legacy.Name))
		Expect(k8sClient.Get(ctx, legacyKey, &corev1.Secret{})).To(Succeed(),
			"the old pods may still consume the legacy credential while deletion is finalizer-held")
		terminating := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), terminating)).To(Succeed())
		Expect(terminating.DeletionTimestamp.IsZero()).To(BeFalse())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}, &corev1.Service{}))
		}).Should(BeTrue())

		terminating.Finalizers = nil
		Expect(k8sClient.Update(ctx, terminating)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), &appsv1.Deployment{}))
		}).Should(BeTrue())

		By("retiring the legacy credential only after the desired workload is applied")
		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, legacyKey, &corev1.Secret{}))).To(BeTrue())
		replacement := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), replacement)).To(Succeed())
		deployedName, _, _, found := deploymentAccessCredential(replacement)
		Expect(found).To(BeTrue())
		Expect(deployedName).To(Equal(migratedRef.Name))
	})

	It("rotates a legacy deterministic access Secret during migration", func() {
		makeContainerProvider("crewai-auth-migration", "")
		makeContainerAgent("c-auth-migration", "crewai-auth-migration", "ghcr.io/x/crewai:poc")
		reconcileCore("c-auth-migration")

		ad := getAgent("c-auth-migration")
		token := []byte(base64.RawURLEncoding.EncodeToString(make([]byte, agentAccessTokenBytes)))
		legacy := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      legacyAgentAccessSecretName(ad),
				Namespace: ad.Namespace,
				Labels:    agentLabels(ad),
			},
			Immutable: ptr.To(true),
			Type:      corev1.SecretTypeOpaque,
			Data:      map[string][]byte{agentAccessTokenKey: token},
		}
		Expect(controllerutil.SetControllerReference(ad, legacy, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacy) })

		legacyRuntime := &airunwayv1alpha1.AgentRuntimeStatus{
			AuthSecretRef: &airunwayv1alpha1.SecretKeyRef{Name: legacy.Name, Key: agentAccessTokenKey},
		}
		Expect(agentprovider.ApplyOwnedStatus(ctx, k8sClient, ad, ContainerFieldOwner,
			airunwayv1alpha1.AgentPhaseDeploying, legacyRuntime, nil,
			metav1.ConditionFalse, "IngressCredentialProvisioned", "legacy access credential")).To(Succeed())

		reconcileContainer(ad.Name)
		out := getAgent(ad.Name)
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef.Name).NotTo(Equal(agentAccessSecretName(ad, token)),
			"legacy token material must not determine the new Secret name")
		Expect(out.Status.Runtime.AuthSecretRef.Name).NotTo(Equal(legacy.Name))

		migrated := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: out.Status.Runtime.AuthSecretRef.Name, Namespace: ad.Namespace,
		}, migrated)).To(Succeed())
		Expect(migrated.Data[agentAccessTokenKey]).NotTo(Equal(token),
			"migration must rotate caller-influenced legacy token material")
		decoded, err := base64.RawURLEncoding.DecodeString(string(migrated.Data[agentAccessTokenKey]))
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(HaveLen(agentAccessTokenBytes))

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), dep)).To(Succeed())
		var accessEnv *corev1.EnvVar
		for i := range dep.Spec.Template.Spec.Containers[0].Env {
			if dep.Spec.Template.Spec.Containers[0].Env[i].Name == agentAccessTokenEnv {
				accessEnv = &dep.Spec.Template.Spec.Containers[0].Env[i]
				break
			}
		}
		Expect(accessEnv).NotTo(BeNil())
		Expect(accessEnv.ValueFrom.SecretKeyRef.Name).To(Equal(migrated.Name))

		legacyKey := types.NamespacedName{Name: legacy.Name, Namespace: legacy.Namespace}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, legacyKey, &corev1.Secret{}))).To(BeTrue(),
			"the deterministic legacy credential must be removed after the workload and status switch")

		By("cleaning a legacy Secret stranded after the derived status write")
		stranded := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      legacy.Name,
				Namespace: legacy.Namespace,
				Labels:    agentLabels(ad),
			},
			Immutable: ptr.To(true),
			Type:      corev1.SecretTypeOpaque,
			Data:      map[string][]byte{agentAccessTokenKey: token},
		}
		Expect(controllerutil.SetControllerReference(ad, stranded, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, stranded)).To(Succeed())
		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, legacyKey, &corev1.Secret{}))).To(BeTrue())
	})

	It("tears down a running workload when credential metadata cannot be read", func() {
		makeContainerProvider("crewai-credential-read", "")
		credential := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-credential-read-model", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("test-only-value")},
		}
		Expect(k8sClient.Create(ctx, credential)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, credential) })

		makeContainerAgent("c-credential-read", "crewai-credential-read", "ghcr.io/x/crewai:poc")
		ad := getAgent("c-credential-read")
		ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
			Name: credential.Name,
			Key:  "api-key",
		}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())

		reconcileCore("c-credential-read")
		reconcileContainer("c-credential-read")

		key := types.NamespacedName{Name: "c-credential-read", Namespace: "default"}
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		ad = getAgent("c-credential-read")
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		accessKey := types.NamespacedName{Name: ad.Status.Runtime.AuthSecretRef.Name, Namespace: "default"}

		readErr := fmt.Errorf("injected model credential metadata read failure")
		r := &ContainerProviderReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			APIReader: failingCredentialMetadataReader{
				Reader: k8sClient,
				target: types.NamespacedName{Name: credential.Name, Namespace: credential.Namespace},
				err:    readErr,
			},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(readErr.Error())))

		By("removing the workload and its provider-managed access credential before retrying")
		Eventually(func(g Gomega) {
			current := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, key, current)
			if apierrors.IsNotFound(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())
		}).Should(Succeed())
		accessSecret := &corev1.Secret{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, accessKey, accessSecret))).To(BeTrue())

		out := getAgent("c-credential-read")
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseFailed))
		Expect(prCond(out).Reason).To(Equal("CredentialMetadataReadFailed"))
	})

	It("tears down a running workload when the resolved credential key disappears", func() {
		makeContainerProvider("crewai-credential-key", "")
		credential := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-credential-key-model", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("test-only-value")},
		}
		Expect(k8sClient.Create(ctx, credential)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, credential) })

		makeContainerAgent("c-credential-key", "crewai-credential-key", "ghcr.io/x/crewai:poc")
		ad := getAgent("c-credential-key")
		ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
			Name: credential.Name,
			Key:  "api-key",
		}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())

		reconcileCore(ad.Name)
		reconcileContainer(ad.Name)
		ad = getAgent(ad.Name)
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		accessKey := types.NamespacedName{Name: ad.Status.Runtime.AuthSecretRef.Name, Namespace: ad.Namespace}
		Expect(k8sClient.Get(ctx, accessKey, &corev1.Secret{})).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), &appsv1.Deployment{})).To(Succeed())

		credential.Data = map[string][]byte{"different-key": []byte("test-only-value")}
		Expect(k8sClient.Update(ctx, credential)).To(Succeed())
		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring("does not contain key \"api-key\"")))

		Eventually(func(g Gomega) {
			current := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), current)
			if apierrors.IsNotFound(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())
		}).Should(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, accessKey, &corev1.Secret{}))).To(BeTrue())
		out := getAgent(ad.Name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseFailed))
		Expect(prCond(out).Reason).To(Equal("CredentialMetadataReadFailed"))
	})

	It("tears down a running workload when the model credential Secret is terminating", func() {
		makeContainerProvider("crewai-credential-terminating", "")
		credential := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-credential-terminating-model", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("test-only-value")},
		}
		Expect(k8sClient.Create(ctx, credential)).To(Succeed())
		DeferCleanup(func() {
			current := &corev1.Secret{}
			key := client.ObjectKeyFromObject(credential)
			if err := k8sClient.Get(ctx, key, current); err == nil {
				current.Finalizers = nil
				_ = k8sClient.Update(ctx, current)
				_ = k8sClient.Delete(ctx, current)
			}
		})

		makeContainerAgent("c-credential-terminating", "crewai-credential-terminating", "ghcr.io/x/crewai:poc")
		ad := getAgent("c-credential-terminating")
		ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
			Name: credential.Name,
			Key:  "api-key",
		}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())

		reconcileCore(ad.Name)
		reconcileContainer(ad.Name)
		ad = getAgent(ad.Name)
		Expect(ad.Status.Runtime).NotTo(BeNil())
		Expect(ad.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		accessKey := types.NamespacedName{Name: ad.Status.Runtime.AuthSecretRef.Name, Namespace: ad.Namespace}
		Expect(k8sClient.Get(ctx, accessKey, &corev1.Secret{})).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), &appsv1.Deployment{})).To(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(credential), credential)).To(Succeed())
		credential.Finalizers = append(credential.Finalizers, "tests.airunway.ai/hold-deletion")
		Expect(k8sClient.Update(ctx, credential)).To(Succeed())
		Expect(k8sClient.Delete(ctx, credential)).To(Succeed())
		Eventually(func() bool {
			current := &corev1.Secret{}
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(credential), current) == nil &&
				!current.DeletionTimestamp.IsZero()
		}).Should(BeTrue())

		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring("model credential Secret default/c-credential-terminating-model is terminating")))

		Eventually(func(g Gomega) {
			current := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), current)
			if apierrors.IsNotFound(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())
		}).Should(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, accessKey, &corev1.Secret{}))).To(BeTrue())
		out := getAgent(ad.Name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseFailed))
		Expect(prCond(out).Reason).To(Equal("CredentialMetadataReadFailed"))
	})

	It("refuses a forged preseeded runtime ServiceAccount", func() {
		makeContainerProvider("crewai-sa-preseed", "")
		makeContainerAgent("c-sa-preseed", "crewai-sa-preseed", "ghcr.io/x/crewai:poc")
		reconcileCore("c-sa-preseed")

		ad := getAgent("c-sa-preseed")
		forged := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      agentServiceAccountName(ad),
				Namespace: ad.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         "v1",
					Kind:               "ConfigMap",
					Name:               "attacker-owned-decoy",
					UID:                ad.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				}},
				Annotations: map[string]string{"eks.amazonaws.com/role-arn": "attacker-role"},
			},
			Secrets:          []corev1.ObjectReference{{Name: "attacker-mount"}},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "attacker-registry"}},
		}
		Expect(k8sClient.Create(ctx, forged)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, forged) })

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: ad.Name, Namespace: ad.Namespace,
		}})
		Expect(err).To(MatchError(ContainSubstring("exact AgentDeployment")))

		current := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(forged), current)).To(Succeed())
		Expect(current.Secrets).To(HaveLen(1), "an untrusted preseed must not be adopted or rewritten")
		Expect(current.ImagePullSecrets).To(HaveLen(1))
		dep := &appsv1.Deployment{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, dep))).To(BeTrue())
		Expect(prCond(getAgent(ad.Name)).Reason).To(Equal("ServiceAccountUnsafe"))
	})

	It("accepts an exact-shape credential-free ServiceAccount preseed", func() {
		makeContainerProvider("crewai-sa-safe-preseed", "")
		makeContainerAgent("c-sa-safe-preseed", "crewai-sa-safe-preseed", "ghcr.io/x/crewai:poc")
		reconcileCore("c-sa-safe-preseed")

		ad := getAgent("c-sa-safe-preseed")
		preseed := renderAgentServiceAccount(ad)
		Expect(controllerutil.SetControllerReference(ad, preseed, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, preseed)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, preseed) })

		// The owner reference is not treated as provenance. This preseed is safe
		// only because its complete workload-facing shape is credential-free.
		reconcileContainer(ad.Name)

		current := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(preseed), current)).To(Succeed())
		Expect(current.Annotations).To(BeEmpty())
		Expect(current.Labels).To(Equal(agentLabels(ad)))
		Expect(current.Secrets).To(BeEmpty())
		Expect(current.ImagePullSecrets).To(BeEmpty())
		Expect(current.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*current.AutomountServiceAccountToken).To(BeFalse())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.ServiceAccountName).To(Equal(current.Name))
	})

	It("sanitizes an exact-owner preseed before creating a workload", func() {
		makeContainerProvider("crewai-sa-exact-preseed", "")
		makeContainerAgent("c-sa-exact-preseed", "crewai-sa-exact-preseed", "ghcr.io/x/crewai:poc")
		reconcileCore("c-sa-exact-preseed")

		ad := getAgent("c-sa-exact-preseed")
		preseed := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:        agentServiceAccountName(ad),
				Namespace:   ad.Namespace,
				Labels:      map[string]string{"attacker.example/identity": "privileged"},
				Annotations: map[string]string{"eks.amazonaws.com/role-arn": "attacker-role"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         airunwayv1alpha1.GroupVersion.String(),
					Kind:               "AgentDeployment",
					Name:               ad.Name,
					UID:                ad.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				}},
			},
			AutomountServiceAccountToken: ptr.To(true),
			Secrets:                      []corev1.ObjectReference{{Name: "attacker-mount"}},
			ImagePullSecrets:             []corev1.LocalObjectReference{{Name: "attacker-registry"}},
		}
		Expect(k8sClient.Create(ctx, preseed)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, preseed) })

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring("removed unexpected credential-bearing fields")))

		clean := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(preseed), clean)).To(Succeed())
		Expect(clean.Annotations).To(BeEmpty())
		Expect(clean.Labels).To(Equal(agentLabels(ad)))
		Expect(clean.OwnerReferences).To(HaveLen(1))
		Expect(clean.Secrets).To(BeEmpty())
		Expect(clean.ImagePullSecrets).To(BeEmpty())
		Expect(clean.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*clean.AutomountServiceAccountToken).To(BeFalse())
		dep := &appsv1.Deployment{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), dep))).To(BeTrue())

		By("creating the workload only on a later reconcile after the clean read")
		reconcileContainer(ad.Name)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.ServiceAccountName).To(Equal(clean.Name))
	})

	It("fails closed when the runtime ServiceAccount is replaced between authoritative reads", func() {
		makeContainerProvider("crewai-sa-replace", "")
		makeContainerAgent("c-sa-replace", "crewai-sa-replace", "ghcr.io/x/crewai:poc")
		reconcileCore("c-sa-replace")
		reconcileContainer("c-sa-replace")

		ad := getAgent("c-sa-replace")
		key := types.NamespacedName{Name: agentServiceAccountName(ad), Namespace: ad.Namespace}
		original := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, key, original)).To(Succeed())
		replacement := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:            original.Name,
				Namespace:       original.Namespace,
				OwnerReferences: append([]metav1.OwnerReference(nil), original.OwnerReferences...),
				Annotations:     map[string]string{"eks.amazonaws.com/role-arn": "replacement-role"},
			},
			AutomountServiceAccountToken: ptr.To(false),
		}
		reader := &replacingServiceAccountReader{
			Reader: k8sClient, writer: k8sClient, target: key, replacement: replacement,
		}
		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: reader}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring("replaced while reconciling")))
		Expect(reader.replaced).To(BeTrue())

		EventallyDeploymentGone := func(g Gomega) {
			dep := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), dep)
			if apierrors.IsNotFound(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.DeletionTimestamp.IsZero()).To(BeFalse())
		}
		Eventually(EventallyDeploymentGone).Should(Succeed())
		Expect(prCond(getAgent(ad.Name)).Reason).To(Equal("ServiceAccountUnsafe"))
	})

	It("stops workloads before repairing an unsafe ServiceAccount and crosses a clean reconcile boundary", func() {
		makeContainerProvider("crewai-sa-tamper", "")
		makeContainerAgent("c-sa-tamper", "crewai-sa-tamper", "ghcr.io/x/crewai:poc")
		reconcileCore("c-sa-tamper")
		reconcileContainer("c-sa-tamper")

		ad := getAgent("c-sa-tamper")
		key := types.NamespacedName{Name: agentServiceAccountName(ad), Namespace: ad.Namespace}
		deploymentKey := client.ObjectKeyFromObject(ad)
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deploymentKey, deployment)).To(Succeed())
		deploymentUID := deployment.UID
		deployment.Finalizers = append(deployment.Finalizers, "tests.airunway.ai/hold-sa-repair")
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

		sa := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, key, sa)).To(Succeed())
		sa.Annotations = map[string]string{"eks.amazonaws.com/role-arn": "attacker-role"}
		sa.Labels["attacker.example/identity"] = "privileged"
		sa.OwnerReferences = append(sa.OwnerReferences, metav1.OwnerReference{
			APIVersion: "v1", Kind: "ConfigMap", Name: "attacker-decoy", UID: types.UID("attacker-decoy"),
		})
		sa.Secrets = []corev1.ObjectReference{{Name: "attacker-mount"}}
		sa.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "attacker-registry"}}
		Expect(k8sClient.Update(ctx, sa)).To(Succeed())

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: ad.Name, Namespace: ad.Namespace,
		}})
		Expect(err).To(MatchError(ContainSubstring("waiting for the previous workload to stop before repairing it")))

		By("retaining the unsafe evidence while foreground deletion is pending")
		unsafe := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, key, unsafe)).To(Succeed())
		Expect(unsafe.Annotations).To(HaveKey("eks.amazonaws.com/role-arn"))
		Expect(unsafe.Secrets).To(HaveLen(1))
		Expect(unsafe.ImagePullSecrets).To(HaveLen(1))
		Expect(k8sClient.Get(ctx, deploymentKey, deployment)).To(Succeed())
		Expect(deployment.UID).To(Equal(deploymentUID))
		Expect(deployment.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: agentServiceName(ad), Namespace: ad.Namespace}, &corev1.Service{}))).To(BeTrue())
		Expect(prCond(getAgent(ad.Name)).Reason).To(Equal("ServiceAccountUnsafe"))

		By("simulating a controller restart before repair without losing the unsafe evidence")
		r = &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: deploymentKey})
		Expect(err).To(MatchError(ContainSubstring("waiting for the previous workload to stop before repairing it")))
		Expect(k8sClient.Get(ctx, key, unsafe)).To(Succeed())
		Expect(unsafe.Annotations).To(HaveKey("eks.amazonaws.com/role-arn"))
		Expect(unsafe.ImagePullSecrets).To(HaveLen(1))

		By("sanitizing only after the old workload is authoritatively absent")
		Expect(k8sClient.Get(ctx, deploymentKey, deployment)).To(Succeed())
		deployment.Finalizers = nil
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, deploymentKey, &appsv1.Deployment{}))
		}).Should(BeTrue())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: deploymentKey})
		Expect(err).To(MatchError(ContainSubstring("removed unexpected credential-bearing fields")))

		clean := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, key, clean)).To(Succeed())
		Expect(clean.Annotations).To(BeEmpty())
		Expect(clean.Labels).To(Equal(agentLabels(ad)))
		Expect(clean.OwnerReferences).To(HaveLen(1))
		Expect(clean.Secrets).To(BeEmpty())
		Expect(clean.ImagePullSecrets).To(BeEmpty())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, deploymentKey, &appsv1.Deployment{}))).To(BeTrue(),
			"the sanitation reconcile must not recreate the workload")

		By("creating the workload only after a later clean read")
		reconcileContainer(ad.Name)
		replacement := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deploymentKey, replacement)).To(Succeed())
		Expect(replacement.UID).NotTo(Equal(deploymentUID))
		Expect(replacement.Spec.Template.Spec.ServiceAccountName).To(Equal(clean.Name))
	})

	It("does not let same-manager SSA erase unsafe ServiceAccount evidence before workload teardown", func() {
		makeContainerProvider("crewai-sa-same-manager", "")
		makeContainerAgent("c-sa-same-manager", "crewai-sa-same-manager", "ghcr.io/x/crewai:poc")
		reconcileCore("c-sa-same-manager")
		reconcileContainer("c-sa-same-manager")

		ad := getAgent("c-sa-same-manager")
		key := types.NamespacedName{Name: agentServiceAccountName(ad), Namespace: ad.Namespace}
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), deployment)).To(Succeed())
		deployment.Finalizers = append(deployment.Finalizers, "tests.airunway.ai/hold-same-manager-sa-repair")
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
		polluted := renderAgentServiceAccount(ad)
		polluted.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}
		Expect(controllerutil.SetControllerReference(ad, polluted, k8sClient.Scheme())).To(Succeed())
		polluted.Annotations = map[string]string{"eks.amazonaws.com/role-arn": "attacker-role"}
		polluted.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "attacker-registry"}}
		Expect(k8sClient.Patch(ctx, polluted, client.Apply,
			client.FieldOwner(ContainerFieldOwner), client.ForceOwnership)).To(Succeed())

		current := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(current.Annotations).To(HaveKey("eks.amazonaws.com/role-arn"))
		Expect(current.ImagePullSecrets).To(HaveLen(1))

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring("waiting for the previous workload to stop before repairing it")))

		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(current.Annotations).To(HaveKey("eks.amazonaws.com/role-arn"),
			"SSA must not prune the only durable indication that existing pods were admitted under an unsafe ServiceAccount")
		Expect(current.ImagePullSecrets).To(HaveLen(1))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ad), deployment)).To(Succeed())
		Expect(deployment.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(prCond(getAgent(ad.Name)).Reason).To(Equal("ServiceAccountUnsafe"))

		deployment.Finalizers = nil
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
	})

	It("falls back to the framework catalog image when spec.config has none", func() {
		makeContainerProvider("crewai-catalog", "ghcr.io/x/from-catalog:poc")
		makeContainerAgent("c-catalog", "crewai-catalog", "") // no image in config

		reconcileCore("c-catalog")
		reconcileContainer("c-catalog")

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-catalog", Namespace: "default"}, dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/from-catalog:poc"))
	})

	It("rolls a catalog Deployment to the immutable main runtime revision", func() {
		const framework = "crewai-catalog-latest"
		const name = "c-catalog-latest"
		const oldVersion = "agent-container-provider:main-aaaaaaaaaaaa"
		const newVersion = "agent-container-provider:main-bbbbbbbbbbbb"

		makeContainerProvider(framework, "ghcr.io/x/from-catalog:${AIRUNWAY_VERSION}")
		provider := &airunwayv1alpha1.AgentProviderConfig{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: framework}, provider)).To(Succeed())
		provider.Status.Version = oldVersion
		Expect(k8sClient.Status().Update(ctx, provider)).To(Succeed())
		makeContainerAgent(name, framework, "")

		reconcileCore(name)
		reconcileContainer(name)

		key := types.NamespacedName{Name: name, Namespace: "default"}
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		oldGeneration := dep.Generation
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/from-catalog:main-aaaaaaaaaaaa"))
		Expect(dep.Spec.Template.Annotations).NotTo(HaveKey(agentCatalogImageRevisionAnnotation),
			"an immutable main revision needs no secondary rollout annotation")

		By("publishing a new immutable provider build revision for the same moving tag")
		provider = &airunwayv1alpha1.AgentProviderConfig{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: framework}, provider)).To(Succeed())
		provider.Status.Version = newVersion
		Expect(k8sClient.Status().Update(ctx, provider)).To(Succeed())
		reconcileContainer(name)

		updated := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/from-catalog:main-bbbbbbbbbbbb"))
		Expect(updated.Spec.Template.Annotations).NotTo(HaveKey(agentCatalogImageRevisionAnnotation))
		Expect(updated.Generation).To(BeNumerically(">", oldGeneration),
			"the changed build revision must produce a new Deployment pod template")
	})

	It("does not rerun a catalog Job when the immutable main runtime revision changes", func() {
		const framework = "crewai-catalog-job-main"
		const name = "c-catalog-job-main"
		const oldVersion = "agent-container-provider:main-aaaaaaaaaaaa"
		const newVersion = "agent-container-provider:main-bbbbbbbbbbbb"

		makeContainerProvider(framework, "ghcr.io/x/from-catalog:${AIRUNWAY_VERSION}")
		provider := &airunwayv1alpha1.AgentProviderConfig{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: framework}, provider)).To(Succeed())
		provider.Status.Version = oldVersion
		Expect(k8sClient.Status().Update(ctx, provider)).To(Succeed())
		makeContainerJobAgent(name, framework, "")

		reconcileCore(name)
		reconcileContainer(name)

		key := types.NamespacedName{Name: name, Namespace: "default"}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		originalUID := job.UID
		Expect(job.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/from-catalog:main-aaaaaaaaaaaa"))

		By("publishing a new provider revision without authorizing another execution")
		provider = &airunwayv1alpha1.AgentProviderConfig{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: framework}, provider)).To(Succeed())
		provider.Status.Version = newVersion
		Expect(k8sClient.Status().Update(ctx, provider)).To(Succeed())
		for range 2 {
			reconcileContainer(name)
		}

		current := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(current.UID).To(Equal(originalUID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(current.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/from-catalog:main-aaaaaaaaaaaa"),
			"a provider build must not replace an accepted one-shot execution")
	})

	It("does not let a precreated deterministic Secret block or supply ingress credentials", func() {
		makeContainerProvider("crewai-auth-conflict", "")
		makeContainerAgent("c-auth-conflict", "crewai-auth-conflict", "ghcr.io/x/crewai:poc")
		reconcileCore("c-auth-conflict")

		foreign := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-auth-conflict-api-auth", Namespace: "default"},
			Data:       map[string][]byte{agentAccessTokenKey: []byte("user-owned-token-must-survive")},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: "c-auth-conflict", Namespace: "default",
		}})
		Expect(err).NotTo(HaveOccurred())

		current := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: foreign.Name, Namespace: foreign.Namespace}, current)).To(Succeed())
		Expect(current.Data[agentAccessTokenKey]).To(Equal([]byte("user-owned-token-must-survive")))
		Expect(current.OwnerReferences).To(BeEmpty())

		out := getAgent("c-auth-conflict")
		Expect(out.Status.Runtime.AuthSecretRef).NotTo(BeNil())
		Expect(out.Status.Runtime.AuthSecretRef.Name).NotTo(Equal(foreign.Name))
		Expect(out.Status.Runtime.AuthSecretRef.Name).To(HavePrefix("c-auth-conflict-api-auth-"))
		managed := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: out.Status.Runtime.AuthSecretRef.Name, Namespace: "default"}, managed)).To(Succeed())
		Expect(managed.OwnerReferences).To(HaveLen(1))
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-auth-conflict", Namespace: "default"}, dep)).To(Succeed())
	})

	It("fails with MissingImage when neither config nor catalog supplies an image", func() {
		makeContainerProvider("crewai-noimg", "")
		makeContainerAgent("c-noimg", "crewai-noimg", "")

		reconcileCore("c-noimg")
		reconcileContainer("c-noimg")

		ad := getAgent("c-noimg")
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseFailed))
		Expect(prCond(ad).Reason).To(Equal("MissingImage"))
	})

	It("migrates a legacy Job before transient MissingImage cleanup", func() {
		const framework = "crewai-job-transient-noimg"
		const name = "c-job-transient-noimg"
		makeContainerProvider(framework, "ghcr.io/x/task:poc")
		makeContainerJobAgent(name, framework, "")
		reconcileCore(name)
		reconcileContainer(name)

		key := types.NamespacedName{Name: name, Namespace: "default"}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		originalJobUID := job.UID
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer(name)

		ad := getAgent(name)
		Expect(ad.Generation).To(Equal(int64(1)))
		Expect(prCond(ad).Reason).To(Equal("JobCompleted"))

		By("reproducing the exact-owned pre-ledger Job and empty ledger")
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		delete(job.Annotations, agentJobGenerationAnnotation)
		Expect(k8sClient.Update(ctx, job)).To(Succeed())
		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		By("temporarily removing the provider catalog image")
		provider := &airunwayv1alpha1.AgentProviderConfig{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: framework}, provider)).To(Succeed())
		catalog := provider.Annotations[airunwayv1alpha1.AgentProviderCatalogAnnotation]
		Expect(catalog).NotTo(BeEmpty())
		delete(provider.Annotations, airunwayv1alpha1.AgentProviderCatalogAnnotation)
		Expect(k8sClient.Update(ctx, provider)).To(Succeed())

		By("preserving the Job and terminal status until the ledger write succeeds")
		ledgerErr := fmt.Errorf("injected MissingImage ledger migration failure")
		failingClient := &failingConfigMapPatchClient{
			Client: k8sClient, patchType: types.MergePatchType, err: ledgerErr,
		}
		r := &ContainerProviderReconciler{Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(ledgerErr.Error())))
		Expect(failingClient.failed).To(BeTrue())
		preservedJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, preservedJob)).To(Succeed())
		Expect(preservedJob.UID).To(Equal(originalJobUID))
		Expect(preservedJob.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(prCond(getAgent(name)).Reason).To(Equal("JobCompleted"),
			"the failed ledger migration must not replace its only terminal status evidence")
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation))
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobOutcomeAnnotation))

		By("persisting the outcome before terminal cleanup removes the Job")
		reconcileContainer(name)
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted))

		deletingJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, deletingJob)).To(Succeed())
		Expect(deletingJob.DeletionTimestamp.IsZero()).To(BeFalse())
		deletingJob.Finalizers = nil
		Expect(k8sClient.Update(ctx, deletingJob)).To(Succeed())
		serviceAccountKey := types.NamespacedName{Name: agentServiceAccountName(ad), Namespace: ad.Namespace}
		deletingSA := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, serviceAccountKey, deletingSA)).To(Succeed())
		Expect(deletingSA.DeletionTimestamp.IsZero()).To(BeFalse())
		deletingSA.Finalizers = nil
		Expect(k8sClient.Update(ctx, deletingSA)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, serviceAccountKey, &corev1.ServiceAccount{}))
		}).Should(BeTrue())

		By("restoring the catalog without rerunning the completed generation")
		provider = &airunwayv1alpha1.AgentProviderConfig{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: framework}, provider)).To(Succeed())
		if provider.Annotations == nil {
			provider.Annotations = map[string]string{}
		}
		provider.Annotations[airunwayv1alpha1.AgentProviderCatalogAnnotation] = catalog
		Expect(k8sClient.Update(ctx, provider)).To(Succeed())
		reconcileContainer(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue())
		out := getAgent(name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
		Expect(prCond(out).Reason).To(Equal("JobCompleted"))
	})

	It("ignores agents whose framework is not container-backed", func() {
		// A crd-backend framework must be skipped by the container provider.
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "somecrd"},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           airunwayv1alpha1.AgentProviderBackendCRD,
					ModelBindingModes: []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI},
				},
			},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
		apc.Status.Ready = ptrBool(true)
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())

		makeContainerAgent("c-notmine", "somecrd", "img:1")
		reconcileCore("c-notmine")
		reconcileContainer("c-notmine")

		// The container provider must not have created a Deployment.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-notmine", Namespace: "default"}, dep)).NotTo(Succeed())
	})

	It("renders a one-shot Job when spec.lifecycle is job", func() {
		makeContainerProvider("crewai-job", "")
		makeContainerJobAgent("c-job", "crewai-job", "ghcr.io/x/task:poc")

		reconcileCore("c-job")
		reconcileContainer("c-job")

		By("creating a Job (not a Deployment or Service)")
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job", Namespace: "default"}, job)).To(Succeed())
		Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(job.OwnerReferences).To(HaveLen(1))
		for i := range job.Spec.Template.Spec.Containers[0].Env {
			Expect(job.Spec.Template.Spec.Containers[0].Env[i].Name).NotTo(Equal(agentAccessTokenEnv))
		}
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job", Namespace: "default"}, dep)).NotTo(Succeed())
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job", Namespace: "default"}, svc)).NotTo(Succeed())
		accessSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job-api-auth", Namespace: "default"}, accessSecret)).NotTo(Succeed())

		By("reporting Deploying while the Job has not started")
		ad2 := getAgent("c-job")
		Expect(ad2.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(prCond(ad2).Reason).To(Equal("JobPending"))
		Expect(ad2.Status.Runtime.AuthSecretRef).To(BeNil())

		By("flipping to Running once the Job reports an active pod")
		job.Status.Active = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer("c-job")
		ad2 = getAgent("c-job")
		Expect(ad2.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning))
		Expect(prCond(ad2).Status).To(Equal(metav1.ConditionTrue))

		By("flipping to Completed once the Job succeeds")
		// Reconcile server-side-applied the Job after the first status update,
		// which can advance resourceVersion while converting the create manager's
		// managed-fields entry to Apply. Refresh before the next status write.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job", Namespace: "default"}, job)).To(Succeed())
		job.Status.Active = 0
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer("c-job")
		ad2 = getAgent("c-job")
		Expect(ad2.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
		Expect(prCond(ad2).Reason).To(Equal("JobCompleted"))

		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-job-config", Namespace: "default"}, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad2.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted))

		By("preserving the terminal result without rerunning after the Job is deleted")
		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: "c-job", Namespace: "default"}, &batchv1.Job{}))
		}).Should(BeTrue())
		reconcileContainer("c-job")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: "c-job", Namespace: "default"}, &batchv1.Job{}))).To(BeTrue())
		ad2 = getAgent("c-job")
		Expect(ad2.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
		Expect(prCond(ad2).Reason).To(Equal("JobCompleted"))
		Expect(ad2.Status.Runtime).NotTo(BeNil())
		Expect(ad2.Status.Runtime.WorkloadRef).NotTo(BeNil())
		Expect(ad2.Status.Runtime.WorkloadRef.Kind).To(Equal("Job"))
		Expect(ad2.Status.Replicas).NotTo(BeNil())
		Expect(ad2.Status.Replicas.Available).To(Equal(int32(1)))
	})

	It("adopts a first-generation legacy Job created before provider status", func() {
		const framework = "crewai-job-first-gen-crash"
		const name = "c-job-first-gen-crash"
		const image = "ghcr.io/x/task:poc"
		makeContainerProvider(framework, "")
		makeContainerJobAgent(name, framework, image)
		reconcileCore(name)

		ad := getAgent(name)
		Expect(ad.Generation).To(Equal(int64(1)))
		Expect(ad.Status.Runtime).To(BeNil())
		Expect(ad.Status.ProviderOwner).To(BeEmpty())
		Expect(ad.Status.ModelBinding).NotTo(BeNil())

		configMap := renderAgentConfigMap(ad)
		Expect(controllerutil.SetControllerReference(ad, configMap, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, configMap)).To(Succeed())
		configChecksum, err := agentprovider.HashJSON(configMap.Data)
		Expect(err).NotTo(HaveOccurred())
		job, err := renderAgentJob(ad, renderInputs{
			cfg:                containerConfig{Image: image},
			binding:            *ad.Status.ModelBinding,
			configMapName:      configMap.Name,
			configChecksum:     configChecksum,
			serviceAccountName: agentServiceAccountName(ad),
		})
		Expect(err).NotTo(HaveOccurred())
		delete(job.Annotations, agentJobGenerationAnnotation)
		Expect(controllerutil.SetControllerReference(ad, job, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, job)).To(Succeed())
		originalJobUID := job.UID

		By("reconciling after the legacy controller crashed before status")
		reconcileContainer(name)
		current := &batchv1.Job{}
		key := types.NamespacedName{Name: name, Namespace: "default"}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(current.UID).To(Equal(originalJobUID),
			"the already-started first-generation execution must not be deleted or recreated")
		Expect(current.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, "1"))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())

		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configMap.Name, Namespace: configMap.Namespace}, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, "1"))
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobOutcomeAnnotation))
		out := getAgent(name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		Expect(prCond(out).Reason).To(Equal("JobPending"))
		Expect(hasExactJobRuntimeStatus(out)).To(BeTrue())
	})

	It("preserves a completed Job ledger while its provider registration is absent", func() {
		const framework = "crewai-job-provider-gap"
		const name = "c-job-provider-gap"
		makeContainerProvider(framework, "")
		makeContainerJobAgent(name, framework, "ghcr.io/x/task:poc")
		reconcileCore(name)
		reconcileContainer(name)

		key := types.NamespacedName{Name: name, Namespace: "default"}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer(name)

		ad := getAgent(name)
		generation := fmt.Sprint(ad.Generation)
		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, generation))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted))

		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		provider := &airunwayv1alpha1.AgentProviderConfig{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: framework}, provider)).To(Succeed())
		Expect(k8sClient.Delete(ctx, provider)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: framework}, &airunwayv1alpha1.AgentProviderConfig{}))
		}).Should(BeTrue())

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// envtest has no garbage collector to finish the foreground ServiceAccount
		// deletion started by provider handoff cleanup.
		serviceAccountKey := types.NamespacedName{Name: agentServiceAccountName(ad), Namespace: ad.Namespace}
		serviceAccount := &corev1.ServiceAccount{}
		if err := k8sClient.Get(ctx, serviceAccountKey, serviceAccount); err == nil {
			Expect(serviceAccount.DeletionTimestamp.IsZero()).To(BeFalse())
			serviceAccount.Finalizers = nil
			Expect(k8sClient.Update(ctx, serviceAccount)).To(Succeed())
			_ = k8sClient.Delete(ctx, serviceAccount)
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, serviceAccountKey, &corev1.ServiceAccount{}))
		}).Should(BeTrue())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(getAgent(name).Status.ProviderOwner).To(BeEmpty())

		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed(),
			"provider handoff cleanup must preserve the one-shot execution ledger")
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, generation))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted))

		replacement := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: framework},
			Spec:       provider.Spec,
		}
		Expect(k8sClient.Create(ctx, replacement)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, replacement) })
		replacement.Status.Ready = ptrBool(true)
		Expect(k8sClient.Status().Update(ctx, replacement)).To(Succeed())

		reconcileCore(name)
		reconcileContainer(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"restoring the provider must not rerun the unchanged completed generation")
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, generation))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted))
	})

	It("migrates generation-zero pre-ledger terminal Job status without rerunning a deleted Job", func() {
		makeContainerProvider("crewai-job-legacy", "")
		makeContainerJobAgent("c-job-legacy", "crewai-job-legacy", "ghcr.io/x/task:poc")

		reconcileCore("c-job-legacy")
		reconcileContainer("c-job-legacy")

		job := &batchv1.Job{}
		key := types.NamespacedName{Name: "c-job-legacy", Namespace: "default"}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer("c-job-legacy")
		Expect(getAgent("c-job-legacy").Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))

		By("reproducing a first-generation legacy condition without observedGeneration")
		ad := getAgent("c-job-legacy")
		Expect(ad.Generation).To(Equal(int64(1)))
		for i := range ad.Status.Conditions {
			if ad.Status.Conditions[i].Type == airunwayv1alpha1.AgentConditionTypeProviderReady {
				ad.Status.Conditions[i].ObservedGeneration = 0
			}
		}
		Expect(k8sClient.Status().Update(ctx, ad)).To(Succeed())
		Expect(prCond(getAgent(ad.Name)).ObservedGeneration).To(BeZero())

		By("removing the new ledger annotations to reproduce a pre-ledger upgrade")
		ledger := &corev1.ConfigMap{}
		ledgerKey := types.NamespacedName{Name: "c-job-legacy-config", Namespace: "default"}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		reconcileContainer("c-job-legacy")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"an upgrade must not rerun a terminal generation whose Job was already collected")
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation,
			fmt.Sprint(getAgent("c-job-legacy").Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted))
		Expect(getAgent("c-job-legacy").Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
	})

	It("records ambiguous generation-zero legacy Job evidence as lost after the spec advances", func() {
		makeContainerProvider("crewai-job-legacy-zero-advanced", "")
		makeContainerJobAgent("c-job-legacy-zero-advanced", "crewai-job-legacy-zero-advanced", "ghcr.io/x/task:old")

		reconcileCore("c-job-legacy-zero-advanced")
		reconcileContainer("c-job-legacy-zero-advanced")

		key := types.NamespacedName{Name: "c-job-legacy-zero-advanced", Namespace: "default"}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Status.Active = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer(key.Name)

		ad := getAgent(key.Name)
		oldGeneration := ad.Generation
		Expect(prCond(ad).Reason).To(Equal("JobRunning"))
		Expect(hasExactJobRuntimeStatus(ad)).To(BeTrue())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		newConfig, err := json.Marshal(map[string]any{"image": "ghcr.io/x/task:new", "systemPrompt": "new generation"})
		Expect(err).NotTo(HaveOccurred())
		ad.Spec.Config = &runtime.RawExtension{Raw: newConfig}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)

		ad = getAgent(ad.Name)
		Expect(ad.Generation).To(BeNumerically(">", oldGeneration))
		for i := range ad.Status.Conditions {
			if ad.Status.Conditions[i].Type == airunwayv1alpha1.AgentConditionTypeProviderReady {
				ad.Status.Conditions[i].ObservedGeneration = 0
			}
		}
		Expect(k8sClient.Status().Update(ctx, ad)).To(Succeed())
		Expect(prCond(getAgent(ad.Name)).ObservedGeneration).To(BeZero())

		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"ambiguous legacy execution evidence must suppress a potentially duplicate run")
		ad = getAgent(ad.Name)
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseFailed))
		Expect(prCond(ad).Reason).To(Equal("JobLost"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeLost))
	})

	It("preserves ambiguous legacy Job status across ConfigMap and ledger write failures", func() {
		makeContainerProvider("crewai-job-migration-errors", "")
		makeContainerJobAgent("c-job-migration-errors", "crewai-job-migration-errors", "ghcr.io/x/task:poc")
		reconcileCore("c-job-migration-errors")
		reconcileContainer("c-job-migration-errors")

		ad := getAgent("c-job-migration-errors")
		oldGeneration := ad.Generation
		key := client.ObjectKeyFromObject(ad)
		Expect(prCond(ad).Reason).To(Equal("JobPending"))
		Expect(hasExactJobRuntimeStatus(ad)).To(BeTrue())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		newConfig, err := json.Marshal(map[string]any{"image": "ghcr.io/x/task:new", "systemPrompt": "new generation"})
		Expect(err).NotTo(HaveOccurred())
		ad = getAgent(ad.Name)
		ad.Spec.Config = &runtime.RawExtension{Raw: newConfig}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		ad = getAgent(ad.Name)
		Expect(ad.Generation).To(BeNumerically(">", oldGeneration))
		for i := range ad.Status.Conditions {
			if ad.Status.Conditions[i].Type == airunwayv1alpha1.AgentConditionTypeProviderReady {
				ad.Status.Conditions[i].ObservedGeneration = 0
			}
		}
		Expect(k8sClient.Status().Update(ctx, ad)).To(Succeed())
		ad = getAgent(ad.Name)
		Expect(statusHasAmbiguousLegacyJobExecutionEvidence(ad)).To(BeTrue())

		applyErr := fmt.Errorf("injected agent ConfigMap apply failure")
		applyClient := &failingConfigMapPatchClient{
			Client: k8sClient, patchType: types.ApplyPatchType, err: applyErr,
		}
		r := &ContainerProviderReconciler{Client: applyClient, Scheme: k8sClient.Scheme()}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(applyErr.Error())))
		Expect(applyClient.failed).To(BeTrue())
		ad = getAgent(ad.Name)
		Expect(prCond(ad).Reason).To(Equal("JobPending"),
			"a ConfigMap apply error must not erase ambiguous legacy execution evidence")
		Expect(statusHasAmbiguousLegacyJobExecutionEvidence(ad)).To(BeTrue())

		ledgerErr := fmt.Errorf("injected agent Job ledger patch failure")
		ledgerClient := &failingConfigMapPatchClient{
			Client: k8sClient, patchType: types.MergePatchType, err: ledgerErr,
		}
		r = &ContainerProviderReconciler{Client: ledgerClient, Scheme: k8sClient.Scheme()}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(ledgerErr.Error())))
		Expect(ledgerClient.failed).To(BeTrue())
		ad = getAgent(ad.Name)
		Expect(prCond(ad).Reason).To(Equal("JobPending"),
			"a ledger migration error must remain retryable without destroying its source evidence")
		Expect(statusHasAmbiguousLegacyJobExecutionEvidence(ad)).To(BeTrue())

		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"the missing execution must not be launched again after migration retries")
		ad = getAgent(ad.Name)
		Expect(prCond(ad).Reason).To(Equal("JobLost"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeLost))
	})

	It("persists a legacy terminal Job before no-binding cleanup", func() {
		makeContainerProvider("crewai-job-binding-cleanup", "")
		makeContainerJobAgent("c-job-binding-cleanup", "crewai-job-binding-cleanup", "ghcr.io/x/task:poc")
		reconcileCore("c-job-binding-cleanup")
		reconcileContainer("c-job-binding-cleanup")

		key := types.NamespacedName{Name: "c-job-binding-cleanup", Namespace: "default"}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer(key.Name)

		ad := getAgent(key.Name)
		Expect(prCond(ad).Reason).To(Equal("JobCompleted"))
		originalJobUID := job.UID

		By("removing the Job and ConfigMap annotations to reproduce a pre-ledger upgrade")
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		delete(job.Annotations, agentJobGenerationAnnotation)
		Expect(k8sClient.Update(ctx, job)).To(Succeed())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		By("making the core binding unavailable")
		ad = getAgent(key.Name)
		ad.Status.ModelBinding = nil
		Expect(k8sClient.Status().Update(ctx, ad)).To(Succeed())

		By("preserving the Job and terminal status when the ledger write fails")
		ledgerErr := fmt.Errorf("injected binding-cleanup ledger failure")
		failingClient := &failingConfigMapPatchClient{
			Client: k8sClient, patchType: types.MergePatchType, err: ledgerErr,
		}
		r := &ContainerProviderReconciler{Client: failingClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(ledgerErr.Error())))
		Expect(failingClient.failed).To(BeTrue())

		preservedJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, preservedJob)).To(Succeed())
		Expect(preservedJob.UID).To(Equal(originalJobUID))
		Expect(preservedJob.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(prCond(getAgent(key.Name)).Reason).To(Equal("JobCompleted"),
			"cleanup must not replace the only status evidence before the ledger is durable")
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation))
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobOutcomeAnnotation))

		By("recording the outcome before deleting the Job on retry")
		reconcileContainer(key.Name)
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted))

		deletingJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, deletingJob)).To(Succeed())
		Expect(deletingJob.DeletionTimestamp.IsZero()).To(BeFalse())
		// envtest has no garbage collector to remove foreground finalizers.
		deletingJob.Finalizers = nil
		Expect(k8sClient.Update(ctx, deletingJob)).To(Succeed())
		deletingSA := &corev1.ServiceAccount{}
		saKey := types.NamespacedName{Name: agentServiceAccountName(ad), Namespace: ad.Namespace}
		Expect(k8sClient.Get(ctx, saKey, deletingSA)).To(Succeed())
		Expect(deletingSA.DeletionTimestamp.IsZero()).To(BeFalse())
		deletingSA.Finalizers = nil
		Expect(k8sClient.Update(ctx, deletingSA)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, saKey, &corev1.ServiceAccount{}))
		}).Should(BeTrue())

		reconcileContainer(key.Name)
		Expect(prCond(getAgent(key.Name)).Reason).To(Equal("WaitingForBindings"))

		By("not rerunning the completed generation when the binding recovers")
		reconcileCore(key.Name)
		Expect(getAgent(key.Name).Status.ModelBinding).NotTo(BeNil())
		reconcileContainer(key.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue())
		ad = getAgent(key.Name)
		Expect(ad.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
		Expect(prCond(ad).Reason).To(Equal("JobCompleted"))
	})

	It("migrates a missing pre-ledger Running Job to lost without rerunning it", func() {
		makeContainerProvider("crewai-job-legacy-running-gone", "")
		makeContainerJobAgent("c-job-legacy-running-gone", "crewai-job-legacy-running-gone", "ghcr.io/x/task:poc")
		reconcileCore("c-job-legacy-running-gone")
		reconcileContainer("c-job-legacy-running-gone")

		ad := getAgent("c-job-legacy-running-gone")
		key := client.ObjectKeyFromObject(ad)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Status.Active = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer(ad.Name)
		ad = getAgent(ad.Name)
		Expect(prCond(ad).Reason).To(Equal("JobRunning"))
		Expect(hasExactJobRuntimeStatus(ad)).To(BeTrue())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"current-generation Running status proves the one permitted execution already existed")
		ad = getAgent(ad.Name)
		Expect(prCond(ad).Reason).To(Equal("JobLost"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeLost))
	})

	It("adopts an in-flight pre-ledger Job despite controller-owned template changes", func() {
		makeContainerProvider("crewai-job-legacy-active", "")
		makeContainerJobAgent("c-job-legacy-active", "crewai-job-legacy-active", "ghcr.io/x/task:poc")
		reconcileCore("c-job-legacy-active")
		reconcileContainer("c-job-legacy-active")

		key := types.NamespacedName{Name: "c-job-legacy-active", Namespace: "default"}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		originalUID := job.UID
		delete(job.Annotations, agentJobGenerationAnnotation)
		job.Annotations[agentTemplateHashAnnotation] = "pre-upgrade-template-hash"
		Expect(k8sClient.Update(ctx, job)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Status.Active = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		ledger := &corev1.ConfigMap{}
		ledgerKey := types.NamespacedName{Name: "c-job-legacy-active-config", Namespace: "default"}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		reconcileContainer("c-job-legacy-active")
		current := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(current.UID).To(Equal(originalUID),
			"an upgrade must not recreate an already-running one-shot execution")
		Expect(current.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation,
			fmt.Sprint(getAgent("c-job-legacy-active").Generation)))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(getAgent("c-job-legacy-active").Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseRunning))
	})

	It("stops a same-generation Job when its model credential revision changes", func() {
		makeContainerProvider("crewai-job-model-drift", "")
		credential := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-job-model-drift-secret", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("test-only-model-key-one")},
		}
		Expect(k8sClient.Create(ctx, credential)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, credential) })

		makeContainerJobAgent("c-job-model-drift", "crewai-job-model-drift", "ghcr.io/x/task:poc")
		ad := getAgent("c-job-model-drift")
		ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{Name: credential.Name, Key: "api-key"}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		reconcileContainer(ad.Name)

		ad = getAgent(ad.Name)
		key := client.ObjectKeyFromObject(ad)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		oldChecksum := job.Spec.Template.Annotations[agentModelCredentialChecksumAnnotation]
		Expect(oldChecksum).NotTo(BeEmpty())
		job.Finalizers = append(job.Finalizers, "tests.airunway.ai/hold-job-model-drift")
		Expect(k8sClient.Update(ctx, job)).To(Succeed())
		job.Status.Active = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		originalUID := job.UID

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(credential), credential)).To(Succeed())
		credential.Data["api-key"] = []byte("test-only-model-key-two")
		Expect(k8sClient.Update(ctx, credential)).To(Succeed())

		reconcileContainer(ad.Name)
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		Expect(job.UID).To(Equal(originalUID))
		Expect(job.Spec.Template.Annotations[agentModelCredentialChecksumAnnotation]).To(Equal(oldChecksum))
		Expect(job.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(prCond(getAgent(ad.Name)).Reason).To(Equal("JobSecurityDrift"))
		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)),
			"the execution must be claimed before fail-closed deletion")
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobOutcomeAnnotation))

		job.Finalizers = nil
		Expect(k8sClient.Update(ctx, job)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"credential drift must never launch a replacement execution")
		Expect(prCond(getAgent(ad.Name)).Reason).To(Equal("JobLost"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeLost))
	})

	It("stops a same-generation Job that bypasses the dedicated ServiceAccount", func() {
		makeContainerProvider("crewai-job-serviceaccount-drift", "")
		makeContainerJobAgent("c-job-serviceaccount-drift", "crewai-job-serviceaccount-drift", "ghcr.io/x/task:poc")
		reconcileCore("c-job-serviceaccount-drift")
		reconcileContainer("c-job-serviceaccount-drift")

		ad := getAgent("c-job-serviceaccount-drift")
		key := client.ObjectKeyFromObject(ad)
		original := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, original)).To(Succeed())
		replacement := original.DeepCopy()
		Expect(k8sClient.Delete(ctx, original, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		replacement.ResourceVersion = ""
		replacement.UID = ""
		replacement.Generation = 0
		replacement.CreationTimestamp = metav1.Time{}
		replacement.DeletionTimestamp = nil
		replacement.DeletionGracePeriodSeconds = nil
		replacement.ManagedFields = nil
		replacement.Status = batchv1.JobStatus{}
		replacement.Spec.Selector = nil
		delete(replacement.Spec.Template.Labels, "controller-uid")
		delete(replacement.Spec.Template.Labels, "job-name")
		delete(replacement.Spec.Template.Labels, "batch.kubernetes.io/controller-uid")
		delete(replacement.Spec.Template.Labels, "batch.kubernetes.io/job-name")
		replacement.Spec.Template.Spec.ServiceAccountName = "default"
		replacement.Spec.Template.Spec.AutomountServiceAccountToken = nil
		replacement.Finalizers = []string{"tests.airunway.ai/hold-job-serviceaccount-drift"}
		Expect(k8sClient.Create(ctx, replacement)).To(Succeed())
		replacement.Status.Active = 1
		Expect(k8sClient.Status().Update(ctx, replacement)).To(Succeed())
		replacementUID := replacement.UID

		reconcileContainer(ad.Name)
		current := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(current.UID).To(Equal(replacementUID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(prCond(getAgent(ad.Name)).Reason).To(Equal("JobSecurityDrift"))
		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobOutcomeAnnotation))

		current.Finalizers = nil
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"an unsafe accepted Job must be consumed as lost rather than recreated")
		Expect(prCond(getAgent(ad.Name)).Reason).To(Equal("JobLost"))
	})

	It("does not assign an unannotated pre-upgrade Job to a newer generation", func() {
		makeContainerProvider("crewai-job-legacy-old", "")
		makeContainerJobAgent("c-job-legacy-old", "crewai-job-legacy-old", "ghcr.io/x/task:old")
		reconcileCore("c-job-legacy-old")
		reconcileContainer("c-job-legacy-old")

		key := types.NamespacedName{Name: "c-job-legacy-old", Namespace: "default"}
		ad := getAgent("c-job-legacy-old")
		oldGeneration := ad.Generation
		Expect(prCond(ad).ObservedGeneration).To(Equal(oldGeneration))
		oldJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, oldJob)).To(Succeed())
		oldUID := oldJob.UID
		delete(oldJob.Annotations, agentJobGenerationAnnotation)
		Expect(k8sClient.Update(ctx, oldJob)).To(Succeed())

		ledger := &corev1.ConfigMap{}
		ledgerKey := types.NamespacedName{Name: "c-job-legacy-old-config", Namespace: "default"}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())
		legacyConfigData := ledger.DeepCopy().Data

		newConfig, err := json.Marshal(map[string]any{"image": "ghcr.io/x/task:new", "systemPrompt": "changed task"})
		Expect(err).NotTo(HaveOccurred())
		ad.Spec.Config = &runtime.RawExtension{Raw: newConfig}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		ad = getAgent(ad.Name)
		Expect(ad.Generation).To(BeNumerically(">", oldGeneration))
		Expect(prCond(ad).ObservedGeneration).To(Equal(oldGeneration),
			"the provider has not observed a Job for the new generation")

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		for range 2 {
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).To(MatchError(ContainSubstring("cannot safely assign unannotated pre-ledger Job")))

			current := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			Expect(current.UID).To(Equal(oldUID))
			Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
			Expect(current.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation),
				"an ambiguous previous-generation Job must not be relabeled as current")
			Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
			Expect(ledger.Data).To(Equal(legacyConfigData),
				"the new generation must not mutate configuration mounted by the ambiguous Job")
			Expect(ledger.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation),
				"an ambiguous Job must not consume the new generation's execution claim")
			Expect(ledger.Annotations).NotTo(HaveKey(agentJobOutcomeAnnotation))
		}
		preserved := getAgent(ad.Name)
		Expect(prCond(preserved).Reason).To(Equal(agentJobMigrationAmbiguousReason))
		Expect(hasExactJobRuntimeStatus(preserved)).To(BeTrue())
		Expect(statusHasAmbiguousLegacyJobExecutionEvidence(preserved)).To(BeTrue(),
			"the explicit marker must preserve ambiguity after prior-generation status is replaced")

		By("consuming the current generation as lost if the ambiguous Job later disappears")
		current := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(k8sClient.Delete(ctx, current, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"an ambiguous legacy execution must never be rerun")
		out := getAgent(ad.Name)
		Expect(prCond(out).Reason).To(Equal("JobLost"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(out.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeLost))
	})

	It("preserves ambiguous observed-generation-zero Job evidence across cleanup failure", func() {
		makeContainerProvider("crewai-job-legacy-og-zero", "")
		makeContainerJobAgent("c-job-legacy-og-zero", "crewai-job-legacy-og-zero", "ghcr.io/x/task:old")
		reconcileCore("c-job-legacy-og-zero")
		reconcileContainer("c-job-legacy-og-zero")

		key := types.NamespacedName{Name: "c-job-legacy-og-zero", Namespace: "default"}
		ad := getAgent(key.Name)
		oldGeneration := ad.Generation
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		delete(job.Annotations, agentJobGenerationAnnotation)
		Expect(k8sClient.Update(ctx, job)).To(Succeed())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		delete(ledger.Annotations, agentJobClaimNonceAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		newConfig, err := json.Marshal(map[string]any{"image": "ghcr.io/x/task:new", "systemPrompt": "changed task"})
		Expect(err).NotTo(HaveOccurred())
		ad.Spec.Config = &runtime.RawExtension{Raw: newConfig}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		ad = getAgent(ad.Name)
		Expect(ad.Generation).To(BeNumerically(">", oldGeneration))
		for i := range ad.Status.Conditions {
			if ad.Status.Conditions[i].Type == airunwayv1alpha1.AgentConditionTypeProviderReady {
				ad.Status.Conditions[i].ObservedGeneration = 0
			}
		}
		Expect(k8sClient.Status().Update(ctx, ad)).To(Succeed())
		ad = getAgent(ad.Name)
		Expect(statusHasAmbiguousLegacyJobExecutionEvidence(ad)).To(BeTrue())

		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring("cannot safely assign unannotated pre-ledger Job")))
		preserved := getAgent(ad.Name)
		Expect(prCond(preserved).Reason).To(Equal("JobPending"))
		Expect(statusHasAmbiguousLegacyJobExecutionEvidence(preserved)).To(BeTrue(),
			"the cleanup error must not erase the only pre-generation execution evidence")

		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"a missing ambiguous execution must become lost rather than run again")
		out := getAgent(ad.Name)
		Expect(prCond(out).Reason).To(Equal("JobLost"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(out.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeLost))
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobClaimNonceAnnotation))
	})

	It("does not recreate a missing ConfigMap before resolving an ambiguous unannotated Job", func() {
		makeContainerProvider("crewai-job-legacy-missing-config", "")
		makeContainerJobAgent("c-job-legacy-missing-config", "crewai-job-legacy-missing-config", "ghcr.io/x/task:old")
		reconcileCore("c-job-legacy-missing-config")
		reconcileContainer("c-job-legacy-missing-config")

		key := types.NamespacedName{Name: "c-job-legacy-missing-config", Namespace: "default"}
		ad := getAgent(key.Name)
		oldGeneration := ad.Generation
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		oldJobUID := job.UID
		delete(job.Annotations, agentJobGenerationAnnotation)
		Expect(k8sClient.Update(ctx, job)).To(Succeed())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(k8sClient.Delete(ctx, ledger)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, ledgerKey, &corev1.ConfigMap{}))
		}).Should(BeTrue())

		newConfig, err := json.Marshal(map[string]any{"image": "ghcr.io/x/task:new", "systemPrompt": "new generation"})
		Expect(err).NotTo(HaveOccurred())
		ad.Spec.Config = &runtime.RawExtension{Raw: newConfig}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		ad = getAgent(ad.Name)
		Expect(ad.Generation).To(BeNumerically(">", oldGeneration))
		Expect(prCond(ad).ObservedGeneration).To(Equal(oldGeneration))

		ad.Status.ModelBinding = nil
		Expect(k8sClient.Status().Update(ctx, ad)).To(Succeed())

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		for range 2 {
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).To(MatchError(ContainSubstring("cannot safely assign unannotated pre-ledger Job")))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, ledgerKey, &corev1.ConfigMap{}))).To(BeTrue(),
				"the current spec ConfigMap must not be recreated while the live Job generation is ambiguous")

			current := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			Expect(current.UID).To(Equal(oldJobUID))
			Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
			Expect(current.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation))
		}
	})

	It("deletes a previous-generation Job before recreating its missing ConfigMap", func() {
		makeContainerProvider("crewai-job-old-config", "")
		makeContainerJobAgent("c-job-old-config", "crewai-job-old-config", "ghcr.io/x/task:old")
		reconcileCore("c-job-old-config")
		reconcileContainer("c-job-old-config")

		key := types.NamespacedName{Name: "c-job-old-config", Namespace: "default"}
		ad := getAgent(key.Name)
		oldGeneration := ad.Generation
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		oldJobUID := job.UID
		Expect(job.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(oldGeneration)))
		job.Finalizers = append(job.Finalizers, "test.airunway.ai/hold-previous-generation-job")
		Expect(k8sClient.Update(ctx, job)).To(Succeed())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(k8sClient.Delete(ctx, ledger)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, ledgerKey, &corev1.ConfigMap{}))
		}).Should(BeTrue())

		newConfig, err := json.Marshal(map[string]any{"image": "ghcr.io/x/task:new", "systemPrompt": "new generation"})
		Expect(err).NotTo(HaveOccurred())
		ad.Spec.Config = &runtime.RawExtension{Raw: newConfig}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		ad = getAgent(ad.Name)
		Expect(ad.Generation).To(BeNumerically(">", oldGeneration))

		// Force the binding-cleanup preflight that may need to materialize a
		// missing ledger before deleting a one-shot Job.
		ad.Status.ModelBinding = nil
		Expect(k8sClient.Status().Update(ctx, ad)).To(Succeed())

		r := &ContainerProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		for range 2 {
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue || result.RequeueAfter > 0).To(BeTrue())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, ledgerKey, &corev1.ConfigMap{}))).To(BeTrue(),
				"the current spec ConfigMap must stay absent while the previous-generation Job exists")

			deleting := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key, deleting)).To(Succeed())
			Expect(deleting.UID).To(Equal(oldJobUID))
			Expect(deleting.DeletionTimestamp.IsZero()).To(BeFalse())
			Expect(deleting.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(oldGeneration)))
		}

		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Finalizers = nil
		Expect(k8sClient.Update(ctx, job)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		// Once the old Job is authoritatively gone, normal reconciliation may
		// safely materialize the current generation's config and workload.
		reconcileCore(ad.Name)
		reconcileContainer(ad.Name)
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Data).To(HaveKeyWithValue(agentConfigFileName, string(newConfig)))
		currentJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, currentJob)).To(Succeed())
		Expect(currentJob.UID).NotTo(Equal(oldJobUID))
		Expect(currentJob.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation,
			fmt.Sprint(getAgent(ad.Name).Generation)))
	})

	It("reports a completed Job before inspecting a foreign ServiceAccount", func() {
		makeContainerProvider("crewai-job-terminal-sa", "")
		makeContainerJobAgent("c-job-terminal-sa", "crewai-job-terminal-sa", "ghcr.io/x/task:poc")
		reconcileCore("c-job-terminal-sa")
		reconcileContainer("c-job-terminal-sa")

		ad := getAgent("c-job-terminal-sa")
		key := client.ObjectKeyFromObject(ad)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		jobUID := job.UID
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobOutcomeAnnotation))

		serviceAccountKey := types.NamespacedName{
			Name: agentServiceAccountName(ad), Namespace: ad.Namespace,
		}
		sa := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, serviceAccountKey, sa)).To(Succeed())
		Expect(k8sClient.Delete(ctx, sa, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, serviceAccountKey, &corev1.ServiceAccount{}))
		}).Should(BeTrue())
		foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: serviceAccountKey.Name, Namespace: serviceAccountKey.Namespace,
			Annotations: map[string]string{"eks.amazonaws.com/role-arn": "attacker-role"},
		}}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		foreignUID := foreign.UID
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted),
			"the terminal outcome must be durable before status is reported")
		currentJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, currentJob)).To(Succeed())
		Expect(currentJob.UID).To(Equal(jobUID), "the completed Job must not be recreated")
		Expect(currentJob.DeletionTimestamp.IsZero()).To(BeTrue())
		currentForeign := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, serviceAccountKey, currentForeign)).To(Succeed())
		Expect(currentForeign.UID).To(Equal(foreignUID), "terminal reporting must not adopt or rewrite the foreign ServiceAccount")
		out := getAgent(ad.Name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
		Expect(prCond(out).Reason).To(Equal("JobCompleted"))
	})

	It("reports a failed Job without recreating a missing ServiceAccount", func() {
		makeContainerProvider("crewai-job-failed-sa", "")
		makeContainerJobAgent("c-job-failed-sa", "crewai-job-failed-sa", "ghcr.io/x/task:poc")
		reconcileCore("c-job-failed-sa")
		reconcileContainer("c-job-failed-sa")

		ad := getAgent("c-job-failed-sa")
		key := client.ObjectKeyFromObject(ad)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		jobUID := job.UID
		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		base := ledger.DeepCopy()
		if ledger.Annotations == nil {
			ledger.Annotations = map[string]string{}
		}
		ledger.Annotations[agentJobGenerationAnnotation] = fmt.Sprint(ad.Generation)
		ledger.Annotations[agentJobOutcomeAnnotation] = agentJobOutcomeFailed
		Expect(k8sClient.Patch(ctx, ledger, client.MergeFrom(base))).To(Succeed())

		serviceAccountKey := types.NamespacedName{
			Name: agentServiceAccountName(ad), Namespace: ad.Namespace,
		}
		sa := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, serviceAccountKey, sa)).To(Succeed())
		Expect(k8sClient.Delete(ctx, sa, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, serviceAccountKey, &corev1.ServiceAccount{}))
		}).Should(BeTrue())

		createErr := fmt.Errorf("injected ServiceAccount create failure")
		failingClient := &failingServiceAccountCreateClient{Client: k8sClient, err: createErr}
		r := &ContainerProviderReconciler{Client: failingClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(failingClient.attempts).To(Equal(0), "a durable terminal outcome must bypass ServiceAccount creation")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serviceAccountKey, &corev1.ServiceAccount{}))).To(BeTrue())
		currentJob := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, currentJob)).To(Succeed())
		Expect(currentJob.UID).To(Equal(jobUID), "the recorded failed generation must not recreate its Job")
		out := getAgent(ad.Name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseFailed))
		Expect(prCond(out).Reason).To(Equal("JobFailed"))
	})

	It("claims an accepted live Job before ServiceAccount cleanup can erase it", func() {
		makeContainerProvider("crewai-job-live-sa", "")
		makeContainerJobAgent("c-job-live-sa", "crewai-job-live-sa", "ghcr.io/x/task:poc")
		reconcileCore("c-job-live-sa")
		reconcileContainer("c-job-live-sa")

		ad := getAgent("c-job-live-sa")
		key := client.ObjectKeyFromObject(ad)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Status.Active = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		sa := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: agentServiceAccountName(ad), Namespace: ad.Namespace,
		}, sa)).To(Succeed())
		sa.Annotations = map[string]string{"eks.amazonaws.com/role-arn": "attacker-role"}
		Expect(k8sClient.Update(ctx, sa)).To(Succeed())

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring("waiting for the previous workload to stop before repairing it")))

		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)),
			"the live execution must consume this generation before fail-closed cleanup")
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobOutcomeAnnotation))

		deleting := &batchv1.Job{}
		if err := k8sClient.Get(ctx, key, deleting); err == nil {
			Expect(deleting.DeletionTimestamp.IsZero()).To(BeFalse())
			// envtest has no garbage collector to remove the foreground finalizer.
			deleting.Finalizers = nil
			Expect(k8sClient.Update(ctx, deleting)).To(Succeed())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		By("sanitizing only after the accepted Job is authoritatively absent")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring("removed unexpected credential-bearing fields")))
		clean := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sa), clean)).To(Succeed())
		Expect(clean.Annotations).To(BeEmpty())

		By("reporting the durable lost outcome only after the clean reconcile boundary")
		reconcileContainer(ad.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"a deleted accepted execution must become JobLost, never be launched again")
		out := getAgent(ad.Name)
		Expect(prCond(out).Reason).To(Equal("JobLost"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeLost))
	})

	It("annotates a terminal pre-ledger Job before carrying its outcome into the ledger", func() {
		makeContainerProvider("crewai-job-terminal-unannotated", "")
		makeContainerJobAgent("c-job-terminal-unannotated", "crewai-job-terminal-unannotated", "ghcr.io/x/task:old")
		reconcileCore("c-job-terminal-unannotated")
		reconcileContainer("c-job-terminal-unannotated")

		ad := getAgent("c-job-terminal-unannotated")
		oldGeneration := ad.Generation
		key := client.ObjectKeyFromObject(ad)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		oldUID := job.UID
		delete(job.Annotations, agentJobGenerationAnnotation)
		Expect(k8sClient.Update(ctx, job)).To(Succeed())
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		ledgerKey := types.NamespacedName{Name: agentConfigMapName(ad), Namespace: ad.Namespace}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		delete(ledger.Annotations, agentJobGenerationAnnotation)
		delete(ledger.Annotations, agentJobOutcomeAnnotation)
		Expect(k8sClient.Update(ctx, ledger)).To(Succeed())

		reconcileContainer(ad.Name)
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		Expect(job.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(oldGeneration)),
			"the Job and ledger must carry the same generation before terminal migration completes")
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(oldGeneration)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted))

		newConfig, err := json.Marshal(map[string]any{"image": "ghcr.io/x/task:new", "systemPrompt": "new task"})
		Expect(err).NotTo(HaveOccurred())
		ad = getAgent(ad.Name)
		ad.Spec.Config = &runtime.RawExtension{Raw: newConfig}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		ad = getAgent(ad.Name)
		Expect(ad.Generation).To(BeNumerically(">", oldGeneration))

		reconcileContainer(ad.Name)
		deleting := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, deleting)).To(Succeed())
		Expect(deleting.DeletionTimestamp.IsZero()).To(BeFalse(),
			"the annotated terminal Job belongs to the prior generation and must not supply the new outcome")
		deleting.Finalizers = nil
		Expect(k8sClient.Update(ctx, deleting)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		reconcileContainer(ad.Name)
		current := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(current.UID).NotTo(Equal(oldUID))
		Expect(current.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))
		Expect(current.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/task:new"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(ad.Generation)))
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobOutcomeAnnotation))
	})

	It("reports a recorded terminal Job outcome before reading mutable credential metadata", func() {
		makeContainerProvider("crewai-job-terminal-credential", "")
		credential := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-job-terminal-model", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("test-only-value")},
		}
		Expect(k8sClient.Create(ctx, credential)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, credential) })

		makeContainerJobAgent("c-job-terminal-credential", "crewai-job-terminal-credential", "ghcr.io/x/task:poc")
		ad := getAgent("c-job-terminal-credential")
		ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{
			Name: credential.Name,
			Key:  "api-key",
		}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())

		reconcileCore("c-job-terminal-credential")
		reconcileContainer("c-job-terminal-credential")
		key := types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileContainer("c-job-terminal-credential")

		readErr := fmt.Errorf("terminal outcome must bypass this credential metadata read")
		r := &ContainerProviderReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			APIReader: failingCredentialMetadataReader{
				Reader: k8sClient,
				target: types.NamespacedName{Name: credential.Name, Namespace: credential.Namespace},
				err:    readErr,
			},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		out := getAgent(ad.Name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
		Expect(prCond(out).Reason).To(Equal("JobCompleted"))
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		Expect(job.DeletionTimestamp.IsZero()).To(BeTrue(),
			"a post-completion credential error must not tear down or overwrite the terminal Job outcome")
	})

	It("records a newly terminal Job before a credential read failure can delete it", func() {
		makeContainerProvider("crewai-job-terminal-race", "")
		credential := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-job-terminal-race-model", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("test-only-value")},
		}
		Expect(k8sClient.Create(ctx, credential)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, credential) })

		makeContainerJobAgent("c-job-terminal-race", "crewai-job-terminal-race", "ghcr.io/x/task:poc")
		ad := getAgent("c-job-terminal-race")
		ad.Spec.Model.ExternalAPI.CredentialsRef = &airunwayv1alpha1.SecretKeyRef{Name: credential.Name, Key: "api-key"}
		Expect(k8sClient.Update(ctx, ad)).To(Succeed())
		reconcileCore(ad.Name)
		reconcileContainer(ad.Name)

		key := types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		readErr := fmt.Errorf("terminal live Job must be recorded before this metadata read")
		r := &ContainerProviderReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			APIReader: failingCredentialMetadataReader{
				Reader: k8sClient,
				target: types.NamespacedName{Name: credential.Name, Namespace: credential.Namespace},
				err:    readErr,
			},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		out := getAgent(ad.Name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
		Expect(prCond(out).Reason).To(Equal("JobCompleted"))
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		Expect(job.DeletionTimestamp.IsZero()).To(BeTrue())
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "c-job-terminal-race-config", Namespace: "default",
		}, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeCompleted))
	})

	It("synthesizes terminal runtime status when the Job finished before the first status write", func() {
		makeContainerProvider("crewai-job-terminal-before-status", "")
		makeContainerJobAgent("c-job-terminal-before-status", "crewai-job-terminal-before-status", "ghcr.io/x/task:poc")
		reconcileCore("c-job-terminal-before-status")

		ad := getAgent("c-job-terminal-before-status")
		Expect(ad.Status.Runtime).To(BeNil())
		job, err := renderAgentJob(ad, renderInputs{
			cfg:                containerConfig{Image: "ghcr.io/x/task:poc"},
			binding:            *ad.Status.ModelBinding,
			configMapName:      ad.Name + "-config",
			configChecksum:     "pre-status-test",
			serviceAccountName: agentServiceAccountName(ad),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(controllerutil.SetControllerReference(ad, job, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, job)).To(Succeed())
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		reconcileContainer(ad.Name)
		out := getAgent(ad.Name)
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseCompleted))
		Expect(prCond(out).Reason).To(Equal("JobCompleted"))
		Expect(out.Status.Runtime).NotTo(BeNil())
		Expect(out.Status.Runtime.WorkloadRef).To(Equal(&airunwayv1alpha1.RuntimeWorkloadRef{
			APIVersion: "batch/v1", Kind: "Job", Name: ad.Name, Namespace: ad.Namespace,
		}))
		Expect(out.Status.Replicas).NotTo(BeNil())
		Expect(out.Status.Replicas.Desired).To(Equal(int32(1)))
		Expect(out.Status.Replicas.Available).To(Equal(int32(1)))
	})

	It("does not consume a Job generation claim for a foreign name conflict", func() {
		makeContainerProvider("crewai-job-conflict", "")
		makeContainerJobAgent("c-job-conflict", "crewai-job-conflict", "ghcr.io/x/task:poc")
		reconcileCore("c-job-conflict")

		foreign := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "c-job-conflict", Namespace: "default"},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "foreign", Image: "registry.example/foreign:test"}},
			}}},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(foreign)})
		Expect(err).To(MatchError(ContainSubstring("not bound to the exact AgentDeployment")))

		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: "c-job-conflict-config", Namespace: "default"}, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation),
			"a foreign conflict must not permanently consume this generation's execution")

		Expect(k8sClient.Delete(ctx, foreign, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), &batchv1.Job{}))
		}).Should(BeTrue())

		reconcileContainer("c-job-conflict")
		created := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), created)).To(Succeed())
		Expect(metav1.IsControlledBy(created, getAgent("c-job-conflict"))).To(BeTrue())
	})

	It("rejects a Job whose controller reference only copies the AgentDeployment UID", func() {
		makeContainerProvider("crewai-job-forged-owner", "")
		makeContainerJobAgent("c-job-forged-owner", "crewai-job-forged-owner", "ghcr.io/x/task:poc")
		reconcileCore("c-job-forged-owner")

		ad := getAgent("c-job-forged-owner")
		forged := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ad.Name,
				Namespace: ad.Namespace,
				Annotations: map[string]string{
					agentJobGenerationAnnotation: fmt.Sprint(ad.Generation),
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         "v1",
					Kind:               "ConfigMap",
					Name:               "forged-controller",
					UID:                ad.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				}},
			},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "forged", Image: "registry.example/forged:test"}},
			}}},
		}
		Expect(k8sClient.Create(ctx, forged)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, forged) })

		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ad)})
		Expect(err).To(MatchError(ContainSubstring("exact AgentDeployment")))

		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "c-job-forged-owner-config", Namespace: "default",
		}, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation),
			"a forgeable UID-only owner reference must not consume the execution claim")
	})

	It("lets a late exact Job supersede a provisional JobLost outcome", func() {
		makeContainerProvider("crewai-job-late", "")
		makeContainerJobAgent("c-job-late", "crewai-job-late", "ghcr.io/x/task:poc")
		reconcileCore("c-job-late")
		reconcileContainer("c-job-late")

		key := types.NamespacedName{Name: "c-job-late", Namespace: "default"}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		late := job.DeepCopy()
		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		reconcileContainer("c-job-late")
		Expect(prCond(getAgent("c-job-late")).Reason).To(Equal("JobLost"))

		late.ResourceVersion = ""
		late.UID = ""
		late.Generation = 0
		late.CreationTimestamp = metav1.Time{}
		late.DeletionTimestamp = nil
		late.DeletionGracePeriodSeconds = nil
		late.ManagedFields = nil
		late.Status = batchv1.JobStatus{}
		late.Spec.Selector = nil
		delete(late.Spec.Template.Labels, "controller-uid")
		delete(late.Spec.Template.Labels, "job-name")
		delete(late.Spec.Template.Labels, "batch.kubernetes.io/controller-uid")
		delete(late.Spec.Template.Labels, "batch.kubernetes.io/job-name")
		Expect(k8sClient.Create(ctx, late)).To(Succeed())

		reconcileContainer("c-job-late")
		out := getAgent("c-job-late")
		Expect(prCond(out).Reason).NotTo(Equal("JobLost"))
		Expect(out.Status.Phase).To(Equal(airunwayv1alpha1.AgentPhaseDeploying))
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "c-job-late-config", Namespace: "default",
		}, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobOutcomeAnnotation))
	})

	It("preserves JobLost when the ledger ConfigMap is recreated", func() {
		makeContainerProvider("crewai-job-lost-ledger", "")
		makeContainerJobAgent("c-job-lost-ledger", "crewai-job-lost-ledger", "ghcr.io/x/task:poc")
		reconcileCore("c-job-lost-ledger")
		reconcileContainer("c-job-lost-ledger")

		key := types.NamespacedName{Name: "c-job-lost-ledger", Namespace: "default"}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		reconcileContainer("c-job-lost-ledger")
		out := getAgent("c-job-lost-ledger")
		Expect(prCond(out).Reason).To(Equal("JobLost"))
		Expect(out.Status.Runtime).To(BeNil())
		Expect(out.Status.Replicas).To(BeNil())

		ledgerKey := types.NamespacedName{Name: "c-job-lost-ledger-config", Namespace: "default"}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(k8sClient.Delete(ctx, ledger)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, ledgerKey, &corev1.ConfigMap{}))
		}).Should(BeTrue())

		reconcileContainer("c-job-lost-ledger")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))).To(BeTrue(),
			"recreating the ledger must not rerun a generation already declared lost")
		out = getAgent("c-job-lost-ledger")
		Expect(prCond(out).Reason).To(Equal("JobLost"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(out.Generation)))
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeLost))
	})

	It("recovers a committed Job claim when the patch response is lost", func() {
		makeContainerProvider("crewai-job-claim-timeout", "")
		makeContainerJobAgent("c-job-claim-timeout", "crewai-job-claim-timeout", "ghcr.io/x/task:poc")
		reconcileCore("c-job-claim-timeout")

		key := types.NamespacedName{Name: "c-job-claim-timeout", Namespace: "default"}
		wrappedClient := &commitThenErrorJobClaimClient{
			Client: k8sClient,
			err:    fmt.Errorf("injected timeout after Job claim commit"),
		}
		r := &ContainerProviderReconciler{
			Client:    wrappedClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(wrappedClient.committed).To(BeTrue())
		Expect(wrappedClient.jobCreates).To(Equal(1))

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		jobUID := job.UID
		out := getAgent(key.Name)
		Expect(prCond(out).Reason).NotTo(Equal("JobLost"))
		ledger := &corev1.ConfigMap{}
		ledgerKey := types.NamespacedName{Name: agentConfigMapName(out), Namespace: out.Namespace}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation, fmt.Sprint(out.Generation)))
		Expect(ledger.Annotations[agentJobOutcomeAnnotation]).To(BeEmpty())
		Expect(ledger.Annotations[agentJobClaimNonceAnnotation]).NotTo(BeEmpty())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(wrappedClient.jobCreates).To(Equal(1), "claim recovery must not create a second Job")
		current := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(current.UID).To(Equal(jobUID))
		Expect(prCond(getAgent(key.Name)).Reason).NotTo(Equal("JobLost"))
	})

	It("releases a Job generation claim after a definitive create rejection", func() {
		makeContainerProvider("crewai-job-rejected", "")
		makeContainerJobAgent("c-job-rejected", "crewai-job-rejected", "ghcr.io/x/task:poc")
		reconcileCore("c-job-rejected")

		key := types.NamespacedName{Name: "c-job-rejected", Namespace: "default"}
		r := &ContainerProviderReconciler{
			Client: failingJobCreateClient{
				Client: k8sClient,
				err:    apierrors.NewBadRequest("injected definitive Job admission rejection"),
			},
			Scheme:    k8sClient.Scheme(),
			APIReader: k8sClient,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring("injected definitive Job admission rejection")))

		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: "c-job-rejected-config", Namespace: "default"}, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation),
			"an authoritative rejection proves no execution started, so retry must remain possible")

		reconcileContainer("c-job-rejected")
		created := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, created)).To(Succeed())
		Expect(created.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation,
			fmt.Sprint(getAgent("c-job-rejected").Generation)))
	})

	It("releases a Job generation claim after an authoritative 429 rejection", func() {
		makeContainerProvider("crewai-job-throttled", "")
		makeContainerJobAgent("c-job-throttled", "crewai-job-throttled", "ghcr.io/x/task:poc")
		reconcileCore("c-job-throttled")

		key := types.NamespacedName{Name: "c-job-throttled", Namespace: "default"}
		r := &ContainerProviderReconciler{
			Client: failingJobCreateClient{
				Client: k8sClient,
				err:    apierrors.NewTooManyRequests("injected Job admission throttle", 1),
			},
			Scheme:    k8sClient.Scheme(),
			APIReader: k8sClient,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring("injected Job admission throttle")))

		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: "c-job-throttled-config", Namespace: "default"}, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation),
			"the authoritative NotFound proves a throttled create did not start the execution")

		reconcileContainer("c-job-throttled")
		created := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, created)).To(Succeed())
	})

	It("retries a retryable-marker conflict without converting the rejected claim to JobLost", func() {
		makeContainerProvider("crewai-job-marker-conflict", "")
		makeContainerJobAgent("c-job-marker-conflict", "crewai-job-marker-conflict", "ghcr.io/x/task:poc")
		reconcileCore("c-job-marker-conflict")

		key := types.NamespacedName{Name: "c-job-marker-conflict", Namespace: "default"}
		wrappedClient := &rejectingJobCreateAndConflictingMarkerClient{
			Client:    k8sClient,
			createErr: apierrors.NewBadRequest("injected definitive Job rejection"),
		}
		r := &ContainerProviderReconciler{
			Client:    wrappedClient,
			Scheme:    k8sClient.Scheme(),
			APIReader: k8sClient,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring("injected definitive Job rejection")))
		Expect(wrappedClient.conflictedMarker).To(BeTrue())

		ledger := &corev1.ConfigMap{}
		ledgerKey := types.NamespacedName{Name: "c-job-marker-conflict-config", Namespace: "default"}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation),
			"the retried marker and release must not leave a blank ambiguous claim")

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		created := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, created)).To(Succeed())
		Expect(prCond(getAgent("c-job-marker-conflict")).Reason).NotTo(Equal("JobLost"))
	})

	It("keeps a rejected Job claim retryable when releasing the claim fails", func() {
		makeContainerProvider("crewai-job-release-failure", "")
		makeContainerJobAgent("c-job-release-failure", "crewai-job-release-failure", "ghcr.io/x/task:poc")
		reconcileCore("c-job-release-failure")

		key := types.NamespacedName{Name: "c-job-release-failure", Namespace: "default"}
		injectedReleaseErr := fmt.Errorf("injected claim release failure")
		wrappedClient := &rejectingJobCreateAndFailingReleaseClient{
			Client:     k8sClient,
			createErr:  apierrors.NewBadRequest("injected definitive Job rejection"),
			releaseErr: injectedReleaseErr,
		}
		r := &ContainerProviderReconciler{
			Client:    wrappedClient,
			Scheme:    k8sClient.Scheme(),
			APIReader: k8sClient,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(injectedReleaseErr.Error())))
		Expect(wrappedClient.failedRelease).To(BeTrue())

		ledger := &corev1.ConfigMap{}
		ledgerKey := types.NamespacedName{Name: "c-job-release-failure-config", Namespace: "default"}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeRetryable),
			"the durable marker must survive a failed release patch")

		By("retrying the release without falling through to JobLost")
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Requeue).To(BeTrue())
		Expect(prCond(getAgent("c-job-release-failure")).Reason).NotTo(Equal("JobLost"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation))

		By("creating the Job on the next reconcile")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		created := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, created)).To(Succeed())
		Expect(prCond(getAgent("c-job-release-failure")).Reason).NotTo(Equal("JobLost"))
	})

	It("marks a claim retryable before releasing it after a nonmatching Job appears", func() {
		makeContainerProvider("crewai-job-nonmatching-release", "")
		makeContainerJobAgent("c-job-nonmatching-release", "crewai-job-nonmatching-release", "ghcr.io/x/task:poc")
		reconcileCore("c-job-nonmatching-release")

		key := types.NamespacedName{Name: "c-job-nonmatching-release", Namespace: "default"}
		foreign := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "foreign", Image: "registry.example/foreign:test"}},
			}}},
		}
		injectedReleaseErr := fmt.Errorf("injected nonmatching claim release failure")
		wrappedClient := &rejectingJobCreateAndFailingReleaseClient{
			Client:         k8sClient,
			createErr:      apierrors.NewBadRequest("injected create failure after nonmatching Job appeared"),
			releaseErr:     injectedReleaseErr,
			nonmatchingJob: foreign,
		}
		r := &ContainerProviderReconciler{
			Client:    wrappedClient,
			Scheme:    k8sClient.Scheme(),
			APIReader: k8sClient,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(MatchError(ContainSubstring(injectedReleaseErr.Error())))
		Expect(wrappedClient.sawRetryableMark).To(BeTrue())
		Expect(wrappedClient.failedRelease).To(BeTrue())
		Expect(k8sClient.Get(ctx, key, &batchv1.Job{})).To(Succeed())

		ledgerKey := types.NamespacedName{Name: "c-job-nonmatching-release-config", Namespace: "default"}
		ledger := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).To(HaveKeyWithValue(agentJobOutcomeAnnotation, agentJobOutcomeRetryable),
			"a failed blank-claim release must remain distinguishable from an executed-but-lost Job")

		Expect(k8sClient.Delete(ctx, foreign, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &batchv1.Job{}))
		}).Should(BeTrue())

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Requeue).To(BeTrue())
		Expect(prCond(getAgent(key.Name)).Reason).NotTo(Equal("JobLost"))
		Expect(k8sClient.Get(ctx, ledgerKey, ledger)).To(Succeed())
		Expect(ledger.Annotations).NotTo(HaveKey(agentJobGenerationAnnotation))

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		created := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, created)).To(Succeed())
		Expect(hasExactBlockingControllerOwner(created, getAgent(key.Name))).To(BeTrue())
		Expect(created.Annotations).To(HaveKeyWithValue(agentJobGenerationAnnotation,
			fmt.Sprint(getAgent(key.Name).Generation)))
	})
})

// TestSecurityFloorSurvivesHostileOverrides is the regression test for the
// webhook-off hole. The validating webhook rejects each of these values, but
// ENABLE_WEBHOOKS=false is a supported mode and resources admitted before the
// webhook existed are never re-validated — so the render path has to hold the
// floor on its own. Every field here is one the merge would otherwise let an
// AgentDeployment author win, because overrides are merged after the defaults.
func TestSecurityFloorSurvivesHostileOverrides(t *testing.T) {
	hostile := &containerSecurityOverrides{
		PodSecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr.To(false),
			RunAsUser:      ptr.To[int64](0),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(false),
			RunAsUser:                ptr.To[int64](0),
			AllowPrivilegeEscalation: ptr.To(true),
			ReadOnlyRootFilesystem:   ptr.To(false),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			Capabilities:             &corev1.Capabilities{Drop: nil},
		},
	}

	pod := &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](defaultAgentRunAsUser)}
	ctr := &corev1.SecurityContext{RunAsNonRoot: ptr.To(true), ReadOnlyRootFilesystem: ptr.To(true)}

	applyContainerSecurityOverrides(pod, ctr, hostile, false /* writableRoot */)

	if pod.RunAsNonRoot == nil || !*pod.RunAsNonRoot {
		t.Error("pod runAsNonRoot must stay true")
	}
	if pod.RunAsUser == nil || *pod.RunAsUser == 0 {
		t.Errorf("pod runAsUser must not be root, got %v", pod.RunAsUser)
	}
	if pod.SeccompProfile == nil || pod.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccompProfile must not be Unconfined, got %v", pod.SeccompProfile)
	}
	if ctr.RunAsNonRoot == nil || !*ctr.RunAsNonRoot {
		t.Error("container runAsNonRoot must stay true")
	}
	if ctr.RunAsUser == nil || *ctr.RunAsUser == 0 {
		t.Errorf("container runAsUser must not be root, got %v", ctr.RunAsUser)
	}
	if ctr.AllowPrivilegeEscalation == nil || *ctr.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must stay false")
	}
	if ctr.ReadOnlyRootFilesystem == nil || !*ctr.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must stay true when the provider did not declare a writable root")
	}
	if ctr.SeccompProfile == nil || ctr.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("container seccompProfile must not be Unconfined, got %v", ctr.SeccompProfile)
	}
	if ctr.Capabilities == nil || len(ctr.Capabilities.Drop) != 1 || ctr.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities must still drop ALL, got %v", ctr.Capabilities)
	}
}

// The floor must not override what the provider legitimately declared, or a
// framework that genuinely needs a writable root could never run.
func TestSecurityFloorHonoursProviderWritableRoot(t *testing.T) {
	pod := &corev1.PodSecurityContext{}
	ctr := &corev1.SecurityContext{}
	applyContainerSecurityOverrides(pod, ctr, nil, true /* writableRoot */)
	if ctr.ReadOnlyRootFilesystem == nil || *ctr.ReadOnlyRootFilesystem {
		t.Error("a provider declaring writableRootFilesystem must get a writable root")
	}
	// Everything else is still clamped.
	if ctr.AllowPrivilegeEscalation == nil || *ctr.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false even with a writable root")
	}
}

// A localhost seccomp profile is a cluster-admin artefact, not something an
// agent author can forge, so it must be preserved rather than flattened.
func TestSecurityFloorPreservesLocalhostSeccomp(t *testing.T) {
	pod := &corev1.PodSecurityContext{}
	ctr := &corev1.SecurityContext{}
	overrides := &containerSecurityOverrides{
		SecurityContext: &corev1.SecurityContext{
			SeccompProfile: &corev1.SeccompProfile{
				Type:             corev1.SeccompProfileTypeLocalhost,
				LocalhostProfile: ptr.To("operator/agent.json"),
			},
		},
	}
	applyContainerSecurityOverrides(pod, ctr, overrides, false)
	if ctr.SeccompProfile == nil || ctr.SeccompProfile.Type != corev1.SeccompProfileTypeLocalhost {
		t.Errorf("localhost seccomp profile must be preserved, got %v", ctr.SeccompProfile)
	}
}

var _ = Describe("Container provider: catalog and backend-switch handling", func() {
	ctx := context.Background()

	malformedCatalogProvider := func(name string) {
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Annotations: map[string]string{
					// Valid YAML, invalid catalog JSON — exactly the typo case.
					airunwayv1alpha1.AgentProviderCatalogAnnotation: `[{"name": "broken",`,
				},
			},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           airunwayv1alpha1.AgentProviderBackendContainer,
					ModelBindingModes: []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI},
				},
			},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
		apc.Status.Ready = ptrBool(true)
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())
	}

	agentOn := func(name, framework, image string) {
		cfg := map[string]any{"systemPrompt": "hi"}
		if image != "" {
			cfg["image"] = image
		}
		raw, _ := json.Marshal(cfg)
		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: framework},
				Config:    &runtime.RawExtension{Raw: raw},
				Model: airunwayv1alpha1.ModelBinding{
					ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
						Type: airunwayv1alpha1.ExternalAPITypeOpenAI, BaseURL: "https://api.openai.com/v1", ModelName: "gpt-4o-mini",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })
	}

	core := func(name string) {
		r := newCredentialAuthorizedAgentDeploymentReconciler(k8sClient)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	container := func(name string) error {
		r := &ContainerProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		return err
	}

	It("renders an agent with an explicit image despite a malformed catalog", func() {
		// A typo in marketplace UI metadata must not take down an agent that
		// never reads the catalog. This previously failed twice over: readiness
		// went false (tearing down every agent on the framework) and the
		// provider returned an error from every reconcile.
		malformedCatalogProvider("crewai-badcat")
		agentOn("c-explicit", "crewai-badcat", "ghcr.io/x/crewai:poc")

		core("c-explicit")
		Expect(container("c-explicit")).To(Succeed(), "a malformed catalog must not fail the reconcile")

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-explicit", Namespace: "default"}, dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/x/crewai:poc"))
	})

	It("fails only the agent that actually needed the catalog, naming the parse error", func() {
		malformedCatalogProvider("crewai-badcat2")
		agentOn("c-needs-catalog", "crewai-badcat2", "") // no explicit image

		core("c-needs-catalog")
		Expect(container("c-needs-catalog")).To(Succeed())

		out := &airunwayv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-needs-catalog", Namespace: "default"}, out)).To(Succeed())
		cond := meta.FindStatusCondition(out.Status.Conditions, airunwayv1alpha1.AgentConditionTypeProviderReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("MissingImage"))
		Expect(cond.Message).To(ContainSubstring("could not be parsed"),
			"the agent that needs the catalog should be told the catalog is broken")
	})

	It("tears down its workload when the framework registration goes away", func() {
		// spec.capabilities.backend is immutable, so a framework can only move
		// to another backend by being deleted and recreated. The leak window is
		// the delete: without cleanup here the Deployment/Service/ConfigMap keep
		// running unmanaged, and once the framework is recreated on a CRD
		// backend that provider renders a second workload beside them.
		apc := &airunwayv1alpha1.AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "switch-fw"},
			Spec: airunwayv1alpha1.AgentProviderConfigSpec{
				Capabilities: &airunwayv1alpha1.AgentProviderCapabilities{
					Backend:           airunwayv1alpha1.AgentProviderBackendContainer,
					ModelBindingModes: []airunwayv1alpha1.ModelBindingMode{airunwayv1alpha1.ModelBindingModeExternalAPI},
				},
			},
		}
		Expect(k8sClient.Create(ctx, apc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, apc) })
		apc.Status.Ready = ptrBool(true)
		Expect(k8sClient.Status().Update(ctx, apc)).To(Succeed())

		agentOn("c-switch", "switch-fw", "ghcr.io/x/crewai:poc")
		core("c-switch")
		Expect(container("c-switch")).To(Succeed())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "c-switch", Namespace: "default"}, dep)).To(Succeed())

		By("deleting the framework registration")
		Expect(k8sClient.Delete(ctx, apc)).To(Succeed())

		Expect(container("c-switch")).To(Succeed())
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "c-switch", Namespace: "default"}, dep)
		if err == nil {
			Expect(dep.DeletionTimestamp.IsZero()).To(BeFalse(),
				"the orphaned Deployment must be terminating, not left running unmanaged")
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}

		By("also starting foreground removal of the Service and ConfigMap")
		svc := &corev1.Service{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "c-switch", Namespace: "default"}, svc)
		if err == nil {
			Expect(svc.DeletionTimestamp.IsZero()).To(BeFalse())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
		cm := &corev1.ConfigMap{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "c-switch-config", Namespace: "default"}, cm)
		if err == nil {
			Expect(cm.DeletionTimestamp.IsZero()).To(BeFalse())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})
})

// A hardening override must survive the clamp. The webhook accepts
// readOnlyRootFilesystem: true, so pinning the value to the provider capability
// in both directions silently discarded it — the one direction the "can harden,
// never weaken" rule is supposed to permit.
func TestSecurityFloorAllowsHardeningAboveProviderDefault(t *testing.T) {
	overrides := &containerSecurityOverrides{
		SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true)},
	}
	pod := &corev1.PodSecurityContext{}
	ctr := &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(false)} // provider default for writableRoot

	applyContainerSecurityOverrides(pod, ctr, overrides, true /* writableRoot */)

	if ctr.ReadOnlyRootFilesystem == nil || !*ctr.ReadOnlyRootFilesystem {
		t.Error("an author hardening a writable-root framework must keep readOnlyRootFilesystem: true")
	}
}

// The other direction is still forced: a framework that never declared a
// writable root cannot have read-only turned off, webhook or no webhook.
func TestSecurityFloorStillForcesReadOnlyWhenNotDeclared(t *testing.T) {
	overrides := &containerSecurityOverrides{
		SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(false)},
	}
	pod := &corev1.PodSecurityContext{}
	ctr := &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true)}

	applyContainerSecurityOverrides(pod, ctr, overrides, false /* writableRoot */)

	if ctr.ReadOnlyRootFilesystem == nil || !*ctr.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must stay true when the provider did not declare a writable root")
	}
}
