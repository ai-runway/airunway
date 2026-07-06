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
	// WritableRootFilesystem relaxes the hardened read-only root FS default for
	// frameworks that need a writable workdir (e.g. OpenClaw). Provider-owned
	// posture, expressed per-framework — never a user-facing security knob.
	WritableRootFilesystem bool `json:"writableRootFilesystem,omitempty"`
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile renders the container workload for a container-backed AgentDeployment.
func (r *ContainerProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ad airunwayv1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &ad); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ad.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Only handle agents whose framework uses the container backend. Look up
	// the provider config to learn the backend; ignore the agent otherwise.
	isContainer, image, err := r.frameworkIsContainer(ctx, &ad)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !isContainer {
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
		cfg.Image = image // fall back to the framework catalog image
	}
	if cfg.Image == "" {
		return ctrl.Result{}, r.status(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
			metav1.ConditionFalse, "MissingImage",
			"No container image: set spec.config.image or a framework catalog image")
	}

	binding := ad.Status.ModelBindings[0]
	configMap := renderAgentConfigMap(&ad)
	deployment := renderAgentDeployment(&ad, cfg, binding, configMap.Name)
	service := renderAgentService(&ad)

	for _, obj := range []client.Object{configMap, deployment, service} {
		if err := controllerutil.SetControllerReference(&ad, obj, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set owner reference: %w", err)
		}
		if err := r.Patch(ctx, obj, client.Apply, client.FieldOwner(ContainerFieldOwner), client.ForceOwnership); err != nil {
			logger.Error(err, "Failed to apply workload object", "kind", obj.GetObjectKind().GroupVersionKind().Kind)
			return ctrl.Result{}, r.status(ctx, &ad, airunwayv1alpha1.AgentPhaseFailed, nil, nil,
				metav1.ConditionFalse, "RenderFailed", err.Error())
		}
	}

	// Read back the Deployment to report replica counts and readiness.
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
			APIVersion: "apps/v1", Kind: "Deployment",
			Name: deployment.Name, Namespace: deployment.Namespace,
		},
		Address: fmt.Sprintf("http://%s.%s.svc.cluster.local", service.Name, service.Namespace),
	}

	if live.Status.AvailableReplicas > 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.status(ctx, &ad,
			airunwayv1alpha1.AgentPhaseRunning, rt, replicas,
			metav1.ConditionTrue, "WorkloadReady", "Agent workload has available replicas")
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, r.status(ctx, &ad,
		airunwayv1alpha1.AgentPhaseDeploying, rt, replicas,
		metav1.ConditionFalse, "WorkloadNotReady", "Waiting for the agent workload to become available")
}

// frameworkIsContainer reports whether the agent's framework uses the
// container backend, and returns the framework's catalog image if any (the
// first catalog item carrying an image). It returns false when the framework
// is unregistered (the core controller surfaces that error to the user).
func (r *ContainerProviderReconciler) frameworkIsContainer(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) (bool, string, error) {
	var apc airunwayv1alpha1.AgentProviderConfig
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: ad.Spec.Framework.Name}, &apc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if apc.Spec.Capabilities == nil || apc.Spec.Capabilities.Backend != airunwayv1alpha1.AgentProviderBackendContainer {
		return false, "", nil
	}
	for i := range apc.Spec.Catalog {
		if apc.Spec.Catalog[i].Image != "" {
			return true, apc.Spec.Catalog[i].Image, nil
		}
	}
	return true, "", nil
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

// renderAgentDeployment renders the agent Deployment. It bakes in a hardened,
// provider-owned security posture (runAsNonRoot, dropped capabilities,
// seccomp) and injects the resolved model binding as OpenAI-compatible env.
func renderAgentDeployment(ad *airunwayv1alpha1.AgentDeployment, cfg containerConfig, binding airunwayv1alpha1.ModelBindingStatus, configMapName string) *appsv1.Deployment {
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

	container := corev1.Container{
		Name:  "agent",
		Image: cfg.Image,
		Ports: []corev1.ContainerPort{{ContainerPort: agentContainerPort}},
		Env:   env,
		VolumeMounts: []corev1.VolumeMount{{
			Name: "agent-config", MountPath: agentConfigMountDir, ReadOnly: true,
		}},
		// Provider-owned hardening. readOnlyRootFilesystem is relaxed only when
		// the framework declares it needs a writable workdir — expressed here,
		// in the provider, not in the user-facing API (see design §7).
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(!cfg.WritableRootFilesystem),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}

	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace, Labels: agentLabels(ad)},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: agentSelector(ad)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: agentLabels(ad)},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container},
					Volumes: []corev1.Volume{{
						Name: "agent-config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
							},
						},
					}},
				},
			},
		},
	}
}

// renderAgentService renders the ClusterIP Service fronting the agent.
func renderAgentService(ad *airunwayv1alpha1.AgentDeployment) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace, Labels: agentLabels(ad)},
		Spec: corev1.ServiceSpec{
			Selector: agentSelector(ad),
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(agentContainerPort)}},
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
		Named("agent-provider-container").
		Complete(r)
}
