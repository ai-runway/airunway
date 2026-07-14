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
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
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
	agentContainerPort = 8080
)

// ContainerProviderReconciler renders any container-backend AgentDeployment
// (OpenClaw, CrewAI, LangGraph, Hermes, ...) into a Deployment + Service +
// ConfigMap. A single generic provider serves every container framework
// because the image is supplied per-deployment (spec.config.image) or by the
// framework's catalog entry — the framework is data, not code.
type ContainerProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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
// +kubebuilder:rbac:groups="",resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile renders the container workload for a container-backed AgentDeployment.
func (r *ContainerProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
		return ctrl.Result{}, nil
	}

	// Consume the core-resolved bindings; do not render until they are ready.
	if !meta.IsStatusConditionTrue(ad.Status.Conditions, airunwayv1alpha1.AgentConditionTypeModelBound) ||
		len(ad.Status.ModelBindings) == 0 {
		return ctrl.Result{}, r.status(ctx, &ad, airunwayv1alpha1.AgentPhasePending, nil, nil,
			metav1.ConditionFalse, "WaitingForBindings", "Waiting for the core controller to resolve model bindings")
	}

	cfg := parseContainerConfig(ad.Spec.Config)
	if cfg.Image == "" {
		cfg.Image = settings.image // fall back to the framework's unambiguous catalog image
	}
	if cfg.Image == "" {
		msg := "No container image: set spec.config.image or a framework catalog image"
		if settings.imageErr != "" {
			msg = settings.imageErr
		}
		return ctrl.Result{}, r.status(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
			metav1.ConditionFalse, "MissingImage", msg)
	}

	binding := ad.Status.ModelBindings[0]

	// The ConfigMap (mounted agent.json) is shared by both lifecycles.
	configMap := renderAgentConfigMap(&ad)
	if err := r.applyOwned(ctx, &ad, configMap); err != nil {
		return ctrl.Result{}, r.status(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
			metav1.ConditionFalse, "RenderFailed", err.Error())
	}

	if ad.Spec.Lifecycle == airunwayv1alpha1.AgentLifecycleJob {
		return r.reconcileJob(ctx, &ad, cfg, binding, configMap.Name, settings.writableRoot)
	}
	return r.reconcileDeployment(ctx, &ad, cfg, binding, configMap.Name, settings.writableRoot)
}

// applyOwned sets the AgentDeployment as controller owner and server-side
// applies the object under the container field owner.
func (r *ContainerProviderReconciler) applyOwned(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment, obj client.Object) error {
	if err := controllerutil.SetControllerReference(ad, obj, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference: %w", err)
	}
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(ContainerFieldOwner), client.ForceOwnership)
}

// reconcileDeployment renders + applies the long-running Deployment and Service
// and reports readiness from the Deployment's available replicas.
func (r *ContainerProviderReconciler) reconcileDeployment(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment, cfg containerConfig, binding airunwayv1alpha1.ModelBindingStatus, configMapName string, writableRoot bool) (ctrl.Result, error) {
	// A prior spec.lifecycle: job leaves a one-shot Job behind. Delete it so the
	// two lifecycles never run side by side.
	r.deleteObsolete(ctx, ad, &batchv1.Job{})

	deployment := renderAgentDeployment(ad, cfg, binding, configMapName, writableRoot)
	service := renderAgentService(ad, cfg)
	for _, obj := range []client.Object{deployment, service} {
		if err := r.applyOwned(ctx, ad, obj); err != nil {
			return ctrl.Result{}, r.status(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
				metav1.ConditionFalse, "RenderFailed", err.Error())
		}
	}

	var live appsv1.Deployment
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, &live); err != nil {
		return ctrl.Result{}, err
	}
	replicas := &airunwayv1alpha1.AgentReplicaStatus{
		Desired:   ptr.Deref(live.Spec.Replicas, 1),
		Ready:     live.Status.ReadyReplicas,
		Available: live.Status.AvailableReplicas,
	}
	rt := &airunwayv1alpha1.AgentRuntimeStatus{
		WorkloadRef: &airunwayv1alpha1.RuntimeWorkloadRef{
			APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, Namespace: deployment.Namespace,
		},
		Address: fmt.Sprintf("http://%s.%s.svc.cluster.local", service.Name, service.Namespace),
	}

	if live.Status.AvailableReplicas > 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseRunning, rt, replicas,
			metav1.ConditionTrue, "WorkloadReady", "Agent workload has available replicas")
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, r.status(ctx, ad,
		airunwayv1alpha1.AgentPhaseDeploying, rt, replicas,
		metav1.ConditionFalse, "WorkloadNotReady", "Waiting for the agent workload to become available")
}

// reconcileJob renders + applies the one-shot Job and maps its status onto the
// agent phase. A Job is only reported Failed once it surfaces a true JobFailed
// condition (i.e. backoffLimit exhausted) — individual failed pod attempts that
// the Job controller will still retry keep the agent in a pending/running state.
func (r *ContainerProviderReconciler) reconcileJob(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment, cfg containerConfig, binding airunwayv1alpha1.ModelBindingStatus, configMapName string, writableRoot bool) (ctrl.Result, error) {
	// A prior spec.lifecycle: deployment leaves a Deployment + Service behind.
	// Delete them so the two lifecycles never run side by side.
	r.deleteObsolete(ctx, ad, &appsv1.Deployment{}, &corev1.Service{})

	job := renderAgentJob(ad, cfg, binding, configMapName, writableRoot)
	if err := r.applyOwned(ctx, ad, job); err != nil {
		return ctrl.Result{}, r.status(ctx, ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
			metav1.ConditionFalse, "RenderFailed", err.Error())
	}

	var live batchv1.Job
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &live); err != nil {
		return ctrl.Result{}, err
	}
	replicas := &airunwayv1alpha1.AgentReplicaStatus{
		Desired:   ptr.Deref(live.Spec.Parallelism, 1),
		Ready:     live.Status.Active,
		Available: live.Status.Succeeded,
	}
	rt := &airunwayv1alpha1.AgentRuntimeStatus{
		WorkloadRef: &airunwayv1alpha1.RuntimeWorkloadRef{
			APIVersion: "batch/v1", Kind: "Job", Name: job.Name, Namespace: job.Namespace,
		},
	}

	switch {
	case jobConditionTrue(&live, batchv1.JobFailed):
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseFailed, rt, replicas,
			metav1.ConditionFalse, "JobFailed", "Agent job failed (backoff limit exhausted)")
	case jobConditionTrue(&live, batchv1.JobComplete) || live.Status.Succeeded > 0 || live.Status.Active > 0:
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseRunning, rt, replicas,
			metav1.ConditionTrue, "JobRunning", "Agent job is active or has completed")
	default:
		return ctrl.Result{RequeueAfter: 15 * time.Second}, r.status(ctx, ad,
			airunwayv1alpha1.AgentPhaseDeploying, rt, replicas,
			metav1.ConditionFalse, "JobPending", "Waiting for the agent job to start")
	}
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

// deleteObsolete best-effort deletes owned workloads left over from a previous
// spec.lifecycle so a lifecycle switch does not leave both kinds running. Each
// object is looked up by the agent's name in its namespace.
func (r *ContainerProviderReconciler) deleteObsolete(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment, objs ...client.Object) {
	logger := log.FromContext(ctx)
	for _, obj := range objs {
		obj.SetName(ad.Name)
		obj.SetNamespace(ad.Namespace)
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to delete obsolete workload from previous lifecycle",
				"kind", fmt.Sprintf("%T", obj), "name", ad.Name)
		}
	}
}

// containerProviderSettings holds the provider-owned rendering settings the
// container provider resolves from the framework's AgentProviderConfig (not
// from the user's AgentDeployment): whether the agent uses the container
// backend, the default catalog image (if unambiguous), and the provider-owned
// writable-root-filesystem posture.
type containerProviderSettings struct {
	isContainer  bool
	image        string
	imageErr     string
	writableRoot bool
}

// resolveContainerProvider looks up the framework's AgentProviderConfig and
// derives the provider-owned settings. The default image is taken from the
// catalog only when exactly one catalog entry carries an image; multiple images
// are ambiguous (an AgentDeployment carries no catalog-item identity), so the
// caller must require an explicit spec.config.image instead of guessing.
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

	var images []string
	for i := range apc.Spec.Catalog {
		if apc.Spec.Catalog[i].Image != "" {
			images = append(images, apc.Spec.Catalog[i].Image)
		}
	}
	switch {
	case len(images) == 1:
		s.image = images[0]
	case len(images) > 1:
		s.imageErr = "framework catalog advertises multiple images; set spec.config.image explicitly to select one"
	}
	return s, nil
}

// parseContainerConfig extracts the container provider's fields from the opaque
// spec.config.
func parseContainerConfig(raw *runtime.RawExtension) containerConfig {
	var cfg containerConfig
	if raw == nil || len(raw.Raw) == 0 {
		return cfg
	}
	_ = json.Unmarshal(raw.Raw, &cfg)
	return cfg
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
		ObjectMeta: metav1.ObjectMeta{Name: ad.Name + "-config", Namespace: ad.Namespace, Labels: agentLabels(ad)},
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
// (writableRoot) — a provider-owned property, never a user-facing knob.
func agentPodSpec(ad *airunwayv1alpha1.AgentDeployment, cfg containerConfig, binding airunwayv1alpha1.ModelBindingStatus, configMapName string, writableRoot bool) corev1.PodSpec {
	env := []corev1.EnvVar{
		{Name: "AIRUNWAY_AGENT_CONFIG", Value: agentConfigMountPath},
		{Name: "OPENAI_BASE_URL", Value: binding.BaseURL},
		{Name: "OPENAI_MODEL", Value: binding.ModelName},
	}
	if binding.CredentialsRef != nil {
		env = append(env, corev1.EnvVar{
			Name: "OPENAI_API_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: binding.CredentialsRef.Name},
					Key:                  binding.CredentialsRef.Key,
				},
			},
		})
	}
	env = append(env, otlpEnv(ad)...)

	container := corev1.Container{
		Name:  "agent",
		Image: cfg.Image,
		Ports: []corev1.ContainerPort{{ContainerPort: containerPort(cfg)}},
		Env:   env,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "agent-config", MountPath: agentConfigMountDir, ReadOnly: true},
			// Always provide a writable scratch dir so frameworks that need to
			// write (caches, sessions) work without relaxing the whole root FS.
			{Name: "tmp", MountPath: "/tmp"},
		},
		Resources: agentResources(ad),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(!writableRoot),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if len(cfg.Command) > 0 {
		container.Command = cfg.Command
	}
	if len(cfg.Args) > 0 {
		container.Args = cfg.Args
	}

	return corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr.To(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{container},
		Volumes: []corev1.Volume{
			{
				Name: "agent-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
					},
				},
			},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
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

// renderAgentDeployment renders a long-running Deployment for the agent.
func renderAgentDeployment(ad *airunwayv1alpha1.AgentDeployment, cfg containerConfig, binding airunwayv1alpha1.ModelBindingStatus, configMapName string, writableRoot bool) *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace, Labels: agentLabels(ad)},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: agentSelector(ad)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: agentLabels(ad)},
				Spec:       agentPodSpec(ad, cfg, binding, configMapName, writableRoot),
			},
		},
	}
}

// renderAgentJob renders a one-shot Job for the agent (spec.lifecycle: job).
// The pod spec is shared with the Deployment path; only the restart policy
// differs (Jobs require Never or OnFailure).
func renderAgentJob(ad *airunwayv1alpha1.AgentDeployment, cfg containerConfig, binding airunwayv1alpha1.ModelBindingStatus, configMapName string, writableRoot bool) *batchv1.Job {
	pod := agentPodSpec(ad, cfg, binding, configMapName, writableRoot)
	pod.RestartPolicy = corev1.RestartPolicyNever
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace, Labels: agentLabels(ad)},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](3),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: agentLabels(ad)},
				Spec:       pod,
			},
		},
	}
}

// renderAgentService renders the ClusterIP Service fronting the agent.
func renderAgentService(ad *airunwayv1alpha1.AgentDeployment, cfg containerConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace, Labels: agentLabels(ad)},
		Spec: corev1.ServiceSpec{
			Selector: agentSelector(ad),
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(containerPort(cfg))}},
		},
	}
}

func agentSelector(ad *airunwayv1alpha1.AgentDeployment) map[string]string {
	return map[string]string{"airunway.ai/agent": ad.Name}
}

func agentLabels(ad *airunwayv1alpha1.AgentDeployment) map[string]string {
	return map[string]string{
		"airunway.ai/agent":     ad.Name,
		"airunway.ai/framework": ad.Spec.Framework.Name,
	}
}

// status writes provider-owned status via the shared SSA helper.
func (r *ContainerProviderReconciler) status(
	ctx context.Context,
	ad *airunwayv1alpha1.AgentDeployment,
	phase airunwayv1alpha1.AgentPhase,
	rt *airunwayv1alpha1.AgentRuntimeStatus,
	replicas *airunwayv1alpha1.AgentReplicaStatus,
	providerReady metav1.ConditionStatus,
	reason, message string,
) error {
	return applyProviderOwnedStatus(ctx, r.Client, ad, ContainerFieldOwner, phase, rt, replicas, providerReady, reason, message)
}

// SetupWithManager wires the container provider and its owned workloads.
func (r *ContainerProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.AgentDeployment{}).
		Owns(&appsv1.Deployment{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Named("agent-provider-container").
		Complete(r)
}
