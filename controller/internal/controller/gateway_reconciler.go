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
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"github.com/kaito-project/airunway/controller/internal/gateway"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// reconcileGateway creates or updates InferencePool and HTTPRoute resources
// for a ModelDeployment that has gateway integration enabled.
func (r *ModelDeploymentReconciler) reconcileGateway(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	// Skip if no gateway detector configured
	if r.GatewayDetector == nil {
		return nil
	}

	// Skip if gateway CRDs are not available
	if !r.GatewayDetector.IsAvailable(ctx) {
		// Warn if user explicitly enabled gateway but CRDs are missing
		if md.Spec.Gateway != nil && md.Spec.Gateway.Enabled != nil && *md.Spec.Gateway.Enabled {
			logger.Info("Gateway explicitly enabled but Gateway API Inference Extension CRDs not found", "name", md.Name)
			r.setCondition(md, airunwayv1alpha1.ConditionTypeGatewayReady, metav1.ConditionFalse, "CRDsNotAvailable", "Gateway API Inference Extension CRDs are not installed in the cluster")
		}
		return nil
	}

	// Skip if explicitly disabled
	if md.Spec.Gateway != nil && md.Spec.Gateway.Enabled != nil && !*md.Spec.Gateway.Enabled {
		logger.V(1).Info("Gateway integration explicitly disabled", "name", md.Name)
		return nil
	}

	// Resolve gateway configuration
	gwConfig, err := r.resolveGatewayConfig(ctx)
	if err != nil {
		logger.Info("No gateway found for routing, skipping gateway reconciliation", "reason", err.Error())
		r.setCondition(md, airunwayv1alpha1.ConditionTypeGatewayReady, metav1.ConditionFalse, "NoGateway", err.Error())
		return nil
	}

	var gatewayCapabilities *airunwayv1alpha1.GatewayCapabilities
	// Resolve provider gateway capabilities
	if gatewayCapabilities, err = r.resolveProviderGatewayCapabilities(ctx, md); err != nil {
		logger.V(1).Info("Error resolving provider gateway capabilities, proceeding without provider-specific gateway capabilities", "error", err)
	}

	// Ensure model pods have the selector label for InferencePool
	if err := r.labelModelPods(ctx, md); err != nil {
		logger.V(1).Info("Could not label model pods", "error", err)
		// Non-fatal: pods may not exist yet or provider may handle labels
	}

	// If the ModelDeployment is in a different namespace than the Gateway, patch the Gateway
	// listener to allow routes from md.Namespace. This can be disabled globally via the
	// --patch-gateway-allowed-routes=false flag for environments where the admin manages allowedRoutes.
	if r.GatewayDetector.PatchGateway && md.Namespace != gwConfig.GatewayNamespace {
		if err := r.ensureGatewayAllowsNamespace(ctx, gwConfig, md.Namespace); err != nil {
			r.setCondition(md, airunwayv1alpha1.ConditionTypeGatewayReady, metav1.ConditionFalse, "GatewayPatchFailed", err.Error())
			return fmt.Errorf("patching Gateway allowedRoutes: %w", err)
		}
	}

	// Determine the HTTPRoute backend via the GAIE InferencePool/EPP path.
	poolName, poolNamespace := md.Name, md.Namespace

	// Use provider managed inference pool if it exists,
	// otherwise use the default inference pool.
	if ok, err := r.providerInferencePoolExistsOrCreateDefault(ctx, md, gatewayCapabilities, gwConfig); ok && err == nil {
		logger.Info("Skipping InferencePool creation, provider manages InferencePool", "provider", md.Spec.Provider.Name)

		// Resolve the InferencePool name for the provider.
		// The provider-managed pool will be configured to be named with the model deployment name and namespace.
		poolName = resolveProviderInferencePoolName(gatewayCapabilities.InferencePoolNamePattern, md.Name, md.Namespace)
		poolNamespace = resolveProviderInferencePoolName(gatewayCapabilities.InferencePoolNamespace, md.Name, md.Namespace)

		// Use provider-managed InferencePool
		providerEPPName, err := r.reconcileProviderManagedInferencePool(ctx, md, poolName, poolNamespace, gwConfig.GetBBRNamespace())
		if err != nil {
			logger.Info("Error reconciling provider-managed InferencePool", "error", err)
			return err
		}

		// Reconcile DestinationRule for provider-managed EPP (Istio TLS)
		if providerEPPName != "" {
			if err := r.reconcileEPPDestinationRule(ctx, md, providerEPPName, poolNamespace); err != nil {
				return fmt.Errorf("reconciling EPP DestinationRule for provider-managed EPP: %w", err)
			}
		}
	} else if err != nil {
		return err
	}

	if gatewayCapabilities != nil {
		logger.Info("Skipping EPP creation, provider manages EPP", "provider", md.Spec.Provider.Name)
	} else { // Use default EPP
		// Create or update EPP (EndPoint Picker) for the InferencePool
		if err := r.reconcileEPP(ctx, md); err != nil {
			r.setCondition(md, airunwayv1alpha1.ConditionTypeGatewayReady, metav1.ConditionFalse, "EPPFailed", err.Error())
			return fmt.Errorf("reconciling EPP: %w", err)
		}
	}

	backend := httpRouteBackendTarget{
		group:     "inference.networking.k8s.io",
		kind:      "InferencePool",
		name:      poolName,
		namespace: poolNamespace,
	}

	// Resolve model name early (needed for HTTPRoute header match and status)
	modelName := r.resolveModelName(ctx, md)

	// Create or update HTTPRoute (skip if user provides their own)
	if md.Spec.Gateway != nil && md.Spec.Gateway.HTTPRouteRef != "" {
		logger.V(1).Info("Using user-provided HTTPRoute", "httpRouteRef", md.Spec.Gateway.HTTPRouteRef)
	} else {
		if err := r.reconcileHTTPRoute(ctx, md, gwConfig, modelName, backend); err != nil {
			r.setCondition(md, airunwayv1alpha1.ConditionTypeGatewayReady, metav1.ConditionFalse, "HTTPRouteFailed", err.Error())
			return fmt.Errorf("reconciling HTTPRoute: %w", err)
		}
	}

	// Update gateway status
	endpoint := r.resolveGatewayEndpoint(ctx, gwConfig)
	md.Status.Gateway = &airunwayv1alpha1.GatewayStatus{
		Endpoint:         endpoint,
		ModelName:        modelName,
		GatewayNamespace: gwConfig.GatewayNamespace,
	}
	r.setCondition(md, airunwayv1alpha1.ConditionTypeGatewayReady, metav1.ConditionTrue, "GatewayConfigured", "InferencePool and HTTPRoute created")

	logger.Info("Gateway resources reconciled", "name", md.Name, "gateway", gwConfig.GatewayName, "model", modelName)
	return nil
}

// resolveGatewayConfig determines which Gateway to use as the HTTPRoute parent.
func (r *ModelDeploymentReconciler) resolveGatewayConfig(ctx context.Context) (*gateway.GatewayConfig, error) {
	// Try explicit configuration first
	if cfg, err := r.GatewayDetector.GetGatewayConfig(); err == nil {
		return cfg, nil
	}

	// Auto-detect: list Gateway resources in the cluster
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways); err != nil {
		return nil, fmt.Errorf("failed to list gateways: %w", err)
	}

	switch len(gateways.Items) {
	case 0:
		return nil, fmt.Errorf("no Gateway resources found in cluster")
	case 1:
		gw := &gateways.Items[0]
		return gatewayConfigFromResource(gw), nil
	default:
		// Multiple gateways: look for ones with the inference-gateway label
		var labeled []*gatewayv1.Gateway
		for i := range gateways.Items {
			gw := &gateways.Items[i]
			if gw.Labels != nil && gw.Labels[gateway.LabelInferenceGateway] == "true" {
				labeled = append(labeled, gw)
			}
		}
		if len(labeled) == 0 {
			return nil, fmt.Errorf("multiple Gateways found but none labeled with %s=true", gateway.LabelInferenceGateway)
		}
		if len(labeled) > 1 {
			log.FromContext(ctx).Info("WARNING: multiple Gateways labeled with inference-gateway, using the first one. Consider using spec.gateway.gatewayRef for explicit selection.",
				"count", len(labeled), "selected", labeled[0].Name)
		}
		return gatewayConfigFromResource(labeled[0]), nil
	}
}

// gatewayConfigFromResource builds a GatewayConfig from a Gateway resource,
// reading the optional airunway.ai/bbr-namespace annotation.
func gatewayConfigFromResource(gw *gatewayv1.Gateway) *gateway.GatewayConfig {
	cfg := &gateway.GatewayConfig{
		GatewayName:      gw.Name,
		GatewayNamespace: gw.Namespace,
	}
	if gw.Annotations != nil {
		cfg.BBRNamespace = gw.Annotations[gateway.AnnotationBBRNamespace]
	}
	return cfg
}

// reconcileInferencePool creates or updates the InferencePool for a ModelDeployment.
func (r *ModelDeploymentReconciler) reconcileInferencePool(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, port int32, bbrNamespace string) error {
	pool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      md.Name,
			Namespace: md.Namespace,
		},
	}

	eppName := md.Name + "-epp"
	eppPort := r.GatewayDetector.EPPServicePort
	if eppPort == 0 {
		eppPort = 9002
	}

	result, err := ctrl.CreateOrUpdate(ctx, r.Client, pool, func() error {
		pool.Spec.Selector = inferencev1.LabelSelector{
			MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
				inferencev1.LabelKey(airunwayv1alpha1.LabelModelDeployment): inferencev1.LabelValue(md.Name),
			},
		}
		pool.Spec.TargetPorts = []inferencev1.Port{
			{Number: inferencev1.PortNumber(port)},
		}
		pool.Spec.EndpointPickerRef = inferencev1.EndpointPickerRef{
			Name: inferencev1.ObjectName(eppName),
			Port: &inferencev1.Port{Number: inferencev1.PortNumber(eppPort)},
		}
		return ctrl.SetControllerReference(md, pool, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("failed to create/update InferencePool: %w", err)
	}

	log.FromContext(ctx).V(1).Info("InferencePool reconciled", "name", pool.Name, "result", result)

	// When a new InferencePool is created, restart the BBR deployment (if present) so it
	// discovers the new model. BBR watches ConfigMaps via controller-runtime and rebuilds
	// its internal model registry on startup.
	if result == controllerutil.OperationResultCreated {
		if err := r.restartBBRIfPresent(ctx, bbrNamespace); err != nil {
			log.FromContext(ctx).Info("Could not restart BBR deployment (non-fatal)", "error", err)
		}
	}
	return nil
}

func (r *ModelDeploymentReconciler) reconcileProviderManagedInferencePool(ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment, poolName, poolNamespace, bbrNamespace string,
) (string, error) {
	logger := log.FromContext(ctx)
	mdNamespace := md.Namespace

	// Wait for the pool to exist (requeue if not ready).
	pool := &inferencev1.InferencePool{}
	poolKey := client.ObjectKey{Name: poolName, Namespace: poolNamespace}
	if err := r.Get(ctx, poolKey, pool); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Provider-managed InferencePool not found yet, requeuing",
				"pool", poolKey)
			// Thread error through return path to trigger requeue with exponential
			// backoff in main reconcile loop.
			return "", err
		}
		return "", fmt.Errorf("failed to get provider-managed InferencePool %s: %w", poolKey, err)
	}

	logger.V(1).Info("Found provider-managed InferencePool", "pool", poolKey)

	// BBR builds its internal model registry from HTTPRoute headers at startup and
	// needs a rolling restart whenever a new model is added. For default (airunway-
	// managed) pools, reconcileInferencePool handles this when CreateOrUpdate reports
	// Created. Provider-managed pools are created by the provider's operator, so
	// there's no Created signal. This is gated on a one-shot annotation instead,
	// restart BBR exactly once per ModelDeployment, not on every reconcile.
	if md.Annotations[airunwayv1alpha1.BBRRestarted] != "true" {
		if err := r.restartBBRIfPresent(ctx, bbrNamespace); err != nil {
			logger.Info("Could not restart BBR deployment (non-fatal)", "error", err)
		} else {
			mdBase := md.DeepCopy()
			if md.Annotations == nil {
				md.Annotations = map[string]string{}
			}
			md.Annotations[airunwayv1alpha1.BBRRestarted] = "true"
			if patchErr := r.Patch(ctx, md, client.MergeFrom(mdBase)); patchErr != nil {
				logger.V(1).Info("Could not annotate ModelDeployment after BBR restart", "error", patchErr)
			}
		}
	}

	// Use it as HTTPRoute backend ref (cross-namespace ref + ReferenceGrant).
	// Create ReferenceGrant in the inference pool namespace.
	if poolNamespace != mdNamespace {
		rg := &gatewayv1beta1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      poolName + "-referencegrant",
				Namespace: poolNamespace,
			},
		}
		result, err := ctrl.CreateOrUpdate(ctx, r.Client, rg, func() error {
			rg.Spec.From = []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     "gateway.networking.k8s.io",
					Kind:      "HTTPRoute",
					Namespace: gatewayv1beta1.Namespace(mdNamespace),
				},
			}
			rg.Spec.To = []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: "inference.networking.k8s.io",
					Kind:  "InferencePool",
					Name:  (*gatewayv1beta1.ObjectName)(&pool.Name),
				},
			}
			return ctrl.SetControllerReference(pool, rg, r.Scheme)
		})
		if err != nil {
			return "", fmt.Errorf("failed to create/update ReferenceGrant for provider-managed InferencePool: %w", err)
		}

		logger.V(1).Info("ReferenceGrant for provider-managed InferencePool reconciled", "name", rg.Name, "result", result)
	}

	// Return the EPP service name from the InferencePool's EndpointPickerRef
	eppName := string(pool.Spec.EndpointPickerRef.Name)
	return eppName, nil
}

// resolveProviderInferencePoolName applies the provider's naming pattern to produce the
// concrete InferencePool name for a given ModelDeployment. If the provider has
// no pattern configured, it falls back to the ModelDeployment name.
func resolveProviderInferencePoolName(pattern, mdName, mdNamespace string) string {
	if pattern == "" {
		return mdName
	}
	result := strings.ReplaceAll(pattern, "{name}", mdName)
	result = strings.ReplaceAll(result, "{namespace}", mdNamespace)
	return result
}

// reconcileEPP creates or updates the Endpoint Picker Proxy deployment and service
// for a ModelDeployment's InferencePool.
func (r *ModelDeploymentReconciler) reconcileEPP(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) error {
	eppName := md.Name + "-epp"
	eppPort := r.GatewayDetector.EPPServicePort
	if eppPort == 0 {
		eppPort = 9002
	}
	eppImage := r.GatewayDetector.EPPImage
	if eppImage == "" {
		eppImage = "registry.k8s.io/gateway-api-inference-extension/epp:" + gateway.DefaultGAIEVersion
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       eppName,
		"app.kubernetes.io/instance":   md.Name,
		"app.kubernetes.io/managed-by": "airunway",
	}

	// ServiceAccount
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eppName,
			Namespace: md.Namespace,
		},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return ctrl.SetControllerReference(md, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to create/update EPP ServiceAccount: %w", err)
	}

	// Role for EPP (needs to watch pods and inferencepools)
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eppName,
			Namespace: md.Namespace,
		},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "watch", "list"},
			},
			{
				APIGroups: []string{"inference.networking.k8s.io"},
				Resources: []string{"inferencepools"},
				Verbs:     []string{"get", "watch", "list"},
			},
			{
				APIGroups: []string{"coordination.k8s.io"},
				Resources: []string{"leases"},
				Verbs:     []string{"create", "get", "update"},
			},
			{
				APIGroups: []string{"inference.networking.x-k8s.io"},
				Resources: []string{"inferenceobjectives", "inferencemodelrewrites"},
				Verbs:     []string{"get", "watch", "list"},
			},
		}
		return ctrl.SetControllerReference(md, role, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to create/update EPP Role: %w", err)
	}

	// RoleBinding
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eppName,
			Namespace: md.Namespace,
		},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     eppName,
		}
		rb.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      eppName,
				Namespace: md.Namespace,
			},
		}
		return ctrl.SetControllerReference(md, rb, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to create/update EPP RoleBinding: %w", err)
	}

	// ConfigMap for EPP plugins config
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eppName,
			Namespace: md.Namespace,
		},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{
			"default-plugins.yaml": `apiVersion: inference.networking.x-k8s.io/v1alpha1
kind: EndpointPickerConfig
`,
		}
		return ctrl.SetControllerReference(md, cm, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to create/update EPP ConfigMap: %w", err)
	}

	// Deployment
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eppName,
			Namespace: md.Namespace,
		},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Spec = appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:            eppName,
					TerminationGracePeriodSeconds: int64Ptr(130),
					Containers: []corev1.Container{
						{
							Name:            "epp",
							Image:           eppImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								"--pool-name", md.Name,
								"--pool-namespace", md.Namespace,
								"--zap-encoder", "json",
								"--config-file", "/config/default-plugins.yaml",
								"--tracing=false",
							},
							Ports: []corev1.ContainerPort{
								{Name: "grpc", ContainerPort: eppPort},
								{Name: "grpc-health", ContainerPort: 9003},
							},
							Env: []corev1.EnvVar{
								{Name: "NAMESPACE", ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
								}},
								{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
								}},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler:        corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 9003, Service: strPtr("inference-extension")}},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
								FailureThreshold:    5,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler:        corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 9003, Service: strPtr("inference-extension")}},
								InitialDelaySeconds: 10,
								PeriodSeconds:       5,
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "plugins-config", MountPath: "/config"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "plugins-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: eppName},
								},
							},
						},
					},
				},
			},
		}
		return ctrl.SetControllerReference(md, dep, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to create/update EPP Deployment: %w", err)
	}

	// Service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eppName,
			Namespace: md.Namespace,
		},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, svc, func() error {
		h2c := "kubernetes.io/h2c"
		svc.Spec = corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: "grpc-ext-proc", Protocol: corev1.ProtocolTCP, Port: eppPort, AppProtocol: &h2c},
			},
			Type: corev1.ServiceTypeClusterIP,
		}
		return ctrl.SetControllerReference(md, svc, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to create/update EPP Service: %w", err)
	}

	if err := r.reconcileEPPDestinationRule(ctx, md, eppName, md.Namespace); err != nil {
		return fmt.Errorf("failed to create/update EPP DestinationRule: %w", err)
	}

	log.FromContext(ctx).V(1).Info("EPP reconciled", "name", eppName, "image", eppImage)
	return nil
}

// reconcileEPPDestinationRule creates or updates the Istio DestinationRule for the EPP service,
// but only if Istio is detected (i.e. the DestinationRule CRD is registered in the cluster).
// EPP serves TLS by default (--secure-serving=true) with a self-signed certificate.
// kGateway handles this natively, but Istio's sidecar needs a DestinationRule with
// mode: SIMPLE + insecureSkipVerify to connect to the EPP's TLS endpoint.
func (r *ModelDeploymentReconciler) reconcileEPPDestinationRule(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, eppName, eppNamespace string) error {
	gk := schema.GroupKind{Group: "networking.istio.io", Kind: "DestinationRule"}
	if _, err := r.Client.RESTMapper().RESTMapping(gk); err != nil {
		log.FromContext(ctx).V(1).Info("Istio not detected, skipping DestinationRule", "eppName", eppName)
		return nil
	}

	dr := &unstructured.Unstructured{}
	dr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.istio.io",
		Version: "v1beta1",
		Kind:    "DestinationRule",
	})
	dr.SetName(eppName)
	dr.SetNamespace(eppNamespace)

	_, err := ctrl.CreateOrUpdate(ctx, r.Client, dr, func() error {
		if err := unstructured.SetNestedField(dr.Object, map[string]interface{}{
			"host": fmt.Sprintf("%s.%s.svc.cluster.local", eppName, eppNamespace),
			"trafficPolicy": map[string]interface{}{
				"tls": map[string]interface{}{
					"mode":               "SIMPLE",
					"insecureSkipVerify": true,
				},
			},
		}, "spec"); err != nil {
			return err
		}
		return ctrl.SetControllerReference(md, dr, r.Scheme)
	})
	return err
}

func int64Ptr(i int64) *int64 { return &i }
func strPtr(s string) *string { return &s }

// resolveProviderGatewayCapabilities retrieves provider gateway capabilities from InferenceProviderConfig.
func (r *ModelDeploymentReconciler) resolveProviderGatewayCapabilities(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) (*airunwayv1alpha1.GatewayCapabilities, error) {
	var providerName string
	if md.Spec.Provider != nil {
		providerName = md.Spec.Provider.Name
	} else if md.Status.Provider != nil {
		providerName = md.Status.Provider.Name
	} else {
		return nil, fmt.Errorf("provider name not specified in ModelDeployment %s/%s", md.Namespace, md.Name)
	}

	gatewayCapabilities := r.ProviderResolver.GetGatewayCapabilities(ctx, providerName)
	if gatewayCapabilities == nil {
		return nil, fmt.Errorf("failed to resolve provider capabilities for ModelDeployment %s/%s", md.Namespace, md.Name)
	}

	return gatewayCapabilities, nil
}

// httpRouteBackendTarget describes where an HTTPRoute should forward traffic
// via a GAIE InferencePool backend.
type httpRouteBackendTarget struct {
	// group is the backend API group (e.g. "inference.networking.k8s.io").
	group gatewayv1.Group
	// kind is the backend kind (e.g. "InferencePool").
	kind gatewayv1.Kind
	// name is the backend object name.
	name string
	// namespace is the backend object namespace. May differ from the
	// ModelDeployment namespace for provider-managed backends.
	namespace string
}

func buildHTTPRouteSpec(gwConfig *gateway.GatewayConfig, modelName string, backend httpRouteBackendTarget) gatewayv1.HTTPRouteSpec {
	ns := gatewayv1.Namespace(gwConfig.GatewayNamespace)
	pathPrefix := gatewayv1.PathMatchPathPrefix
	timeout := gatewayv1.Duration("300s")

	match := gatewayv1.HTTPRouteMatch{
		Path: &gatewayv1.HTTPPathMatch{
			Type:  &pathPrefix,
			Value: strPtr("/"),
		},
	}
	headerExact := gatewayv1.HeaderMatchExact
	match.Headers = []gatewayv1.HTTPHeaderMatch{
		{
			Type:  &headerExact,
			Name:  "X-Gateway-Model-Name", // https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/pkg/bbr/README.md
			Value: modelName,
		},
	}

	backendGroup := backend.group
	backendKind := backend.kind
	backendNs := gatewayv1.Namespace(backend.namespace)
	backendRef := gatewayv1.BackendObjectReference{
		Group:     &backendGroup,
		Kind:      &backendKind,
		Name:      gatewayv1.ObjectName(backend.name),
		Namespace: &backendNs,
	}

	return gatewayv1.HTTPRouteSpec{
		CommonRouteSpec: gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{
				{
					Name:      gatewayv1.ObjectName(gwConfig.GatewayName),
					Namespace: &ns,
				},
			},
		},
		Rules: []gatewayv1.HTTPRouteRule{
			{
				Matches: []gatewayv1.HTTPRouteMatch{match},
				BackendRefs: []gatewayv1.HTTPBackendRef{
					{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: backendRef,
						},
					},
				},
				Timeouts: &gatewayv1.HTTPRouteTimeouts{
					Request: &timeout,
				},
			},
		},
	}
}

// reconcileHTTPRoute creates the HTTPRoute for a ModelDeployment on first reconcile.
// If the HTTPRoute is subsequently deleted by the user the controller will not recreate.
// The deletion is treated as intentional. The ModelDeployment is
// annotated with HTTPRouteCreated after the initial creation so that future
// reconciles will skip recreating a missing route.
func (r *ModelDeploymentReconciler) reconcileHTTPRoute(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, gwConfig *gateway.GatewayConfig, modelName string, backend httpRouteBackendTarget) error {
	logger := log.FromContext(ctx)

	existing := &gatewayv1.HTTPRoute{}
	err := r.Get(ctx, client.ObjectKey{Name: md.Name, Namespace: md.Namespace}, existing)
	if err == nil {
		// HTTPRoute exists — update it in case model name or gateway changed.
		existing.Spec = buildHTTPRouteSpec(gwConfig, modelName, backend)
		if updateErr := r.Update(ctx, existing); updateErr != nil {
			return fmt.Errorf("failed to update HTTPRoute: %w", updateErr)
		}
		logger.V(1).Info("HTTPRoute updated", "name", existing.Name)
		return nil
	}
	if apierrors.IsNotFound(err) {
		// HTTPRoute is missing. If we created one previously the user deleted it
		// intentionally — respect that and do not recreate.
		if md.Annotations[airunwayv1alpha1.HTTPRouteCreated] == "true" {
			logger.V(1).Info("HTTPRoute was deleted by user, skipping recreation", "name", md.Name)
			return nil
		}

		// First-time creation.
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      md.Name,
				Namespace: md.Namespace,
			},
			Spec: buildHTTPRouteSpec(gwConfig, modelName, backend),
		}
		if setErr := ctrl.SetControllerReference(md, route, r.Scheme); setErr != nil {
			return fmt.Errorf("setting controller reference: %w", setErr)
		}
		if createErr := r.Create(ctx, route); createErr != nil {
			return fmt.Errorf("failed to create HTTPRoute: %w", createErr)
		}
		logger.Info("HTTPRoute created", "name", route.Name)

		// Annotate the ModelDeployment so future reconciles know we created a route.
		patch := client.MergeFrom(md.DeepCopy())
		if md.Annotations == nil {
			md.Annotations = make(map[string]string)
		}
		md.Annotations[airunwayv1alpha1.HTTPRouteCreated] = "true"
		if patchErr := r.Patch(ctx, md, patch); patchErr != nil {
			// Non-fatal: worst case we recreate the route once on the next reconcile.
			logger.V(1).Info("Could not annotate ModelDeployment after HTTPRoute creation", "error", patchErr)
		}
		return nil
	}
	return fmt.Errorf("getting HTTPRoute: %w", err)
}

// resolveGatewayEndpoint reads the Gateway resource's status to find the actual endpoint address.
func (r *ModelDeploymentReconciler) resolveGatewayEndpoint(ctx context.Context, gwConfig *gateway.GatewayConfig) string {
	var gw gatewayv1.Gateway
	if err := r.Get(ctx, client.ObjectKey{Name: gwConfig.GatewayName, Namespace: gwConfig.GatewayNamespace}, &gw); err != nil {
		log.FromContext(ctx).V(1).Info("Could not read Gateway status for endpoint", "error", err)
		return ""
	}
	for _, addr := range gw.Status.Addresses {
		if addr.Value != "" {
			return addr.Value
		}
	}
	return ""
}

// resolveModelName determines the model name for gateway routing.
// Priority: spec.gateway.modelName > spec.model.servedName > auto-discovered from /v1/models > spec.model.id
func (r *ModelDeploymentReconciler) resolveModelName(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) string {
	// Use explicit overrides first
	if md.Spec.Gateway != nil && md.Spec.Gateway.ModelName != "" {
		return md.Spec.Gateway.ModelName
	}
	if shouldUseServedNameForGateway(md) {
		return md.Spec.Model.ServedName
	}

	// Auto-discover from the running model server
	if md.Status.Endpoint != nil && md.Status.Endpoint.Service != "" {
		// Look up the actual service port (status.endpoint.port may be the container port)
		port := r.resolveServicePort(ctx, md.Status.Endpoint.Service, md.Namespace)
		if port == 0 {
			port = md.Status.Endpoint.Port
		}
		if port == 0 {
			port = 8000
		}
		if discovered := r.discoverModelName(ctx, md.Status.Endpoint.Service, md.Namespace, port); discovered != "" {
			log.FromContext(ctx).Info("Auto-discovered model name from server", "name", md.Name, "modelName", discovered)
			return discovered
		}
	}

	return md.Spec.Model.ID
}

func shouldUseServedNameForGateway(md *airunwayv1alpha1.ModelDeployment) bool {
	if md.Spec.Model.ServedName == "" {
		return false
	}

	if md.ResolvedEngineType() == airunwayv1alpha1.EngineTypeLlamaCpp && resolvedProviderName(md) == "kaito" {
		return false
	}

	return true
}

func resolvedProviderName(md *airunwayv1alpha1.ModelDeployment) string {
	if md.Spec.Provider != nil && md.Spec.Provider.Name != "" {
		return md.Spec.Provider.Name
	}
	if md.Status.Provider != nil && md.Status.Provider.Name != "" {
		return md.Status.Provider.Name
	}
	return ""
}

// resolveServicePort looks up the first HTTP port on the named service.
func (r *ModelDeploymentReconciler) resolveServicePort(ctx context.Context, serviceName, namespace string) int32 {
	var svc corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: namespace}, &svc); err != nil {
		return 0
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == "http" || p.Port == 80 || p.Port == 8080 {
			return p.Port
		}
	}
	if len(svc.Spec.Ports) > 0 {
		return svc.Spec.Ports[0].Port
	}
	return 0
}

// resolveTargetPort looks up the target (container) port from the service's first HTTP port.
func (r *ModelDeploymentReconciler) resolveTargetPort(ctx context.Context, serviceName, namespace string) int32 {
	var svc corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: namespace}, &svc); err != nil {
		return 0
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == "http" || p.Port == 80 || p.Port == 8080 {
			if p.TargetPort.IntValue() > 0 {
				return int32(p.TargetPort.IntValue())
			}
			return p.Port
		}
	}
	if len(svc.Spec.Ports) > 0 {
		if svc.Spec.Ports[0].TargetPort.IntValue() > 0 {
			return int32(svc.Spec.Ports[0].TargetPort.IntValue())
		}
		return svc.Spec.Ports[0].Port
	}
	return 0
}

// labelModelPods finds pods backing the model's service and ensures they have the
// airunway.ai/model-deployment label so the InferencePool selector can match them.
func (r *ModelDeploymentReconciler) labelModelPods(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) error {
	if md.Status.Endpoint == nil || md.Status.Endpoint.Service == "" {
		return nil
	}

	// Get the service to find its selector
	var svc corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Name: md.Status.Endpoint.Service, Namespace: md.Namespace}, &svc); err != nil {
		return fmt.Errorf("failed to get service: %w", err)
	}

	if len(svc.Spec.Selector) == 0 {
		return nil
	}

	// List pods matching the service selector
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(md.Namespace),
		client.MatchingLabels(svc.Spec.Selector),
	); err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	labelKey := airunwayv1alpha1.LabelModelDeployment
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Labels[labelKey] == md.Name {
			continue // already labeled
		}
		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		pod.Labels[labelKey] = md.Name
		if err := r.Patch(ctx, pod, patch); err != nil {
			log.FromContext(ctx).V(1).Info("Could not label pod", "pod", pod.Name, "error", err)
			continue
		}
		log.FromContext(ctx).V(1).Info("Labeled pod for InferencePool", "pod", pod.Name)
	}

	return nil
}

// discoverModelName probes the model server's /v1/models endpoint to find the actual served model name.
func (r *ModelDeploymentReconciler) discoverModelName(ctx context.Context, service, namespace string, port int32) string {
	url := fmt.Sprintf("http://%s.%s.svc:%d/v1/models", service, namespace, port)

	httpClient := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.FromContext(ctx).V(1).Info("Could not probe model endpoint", "url", url, "error", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}

	if len(result.Data) > 0 && result.Data[0].ID != "" {
		return result.Data[0].ID
	}
	return ""
}

// ensureGatewayAllowsNamespace patches every listener on the Gateway so its
// allowedRoutes selector includes the given namespace. The selector uses a
// matchExpressions In-list so that multiple cross-namespace ModelDeployments
// can coexist.
func (r *ModelDeploymentReconciler) ensureGatewayAllowsNamespace(ctx context.Context, gwConfig *gateway.GatewayConfig, namespace string) error {
	var gw gatewayv1.Gateway
	if err := r.Get(ctx, client.ObjectKey{Name: gwConfig.GatewayName, Namespace: gwConfig.GatewayNamespace}, &gw); err != nil {
		return fmt.Errorf("getting Gateway: %w", err)
	}

	existing := allowedNamespacesFromGateway(&gw)
	if existing[namespace] {
		return nil // already allowed
	}
	existing[namespace] = true

	if err := r.patchGatewayListenerSelector(ctx, gwConfig, existing); err != nil {
		return err
	}

	log.FromContext(ctx).Info("Patched Gateway listeners to allow routes from namespace",
		"gateway", gwConfig.GatewayName, "namespace", namespace)
	return nil
}

// patchGatewayListenerSelector fetches the Gateway fresh and patches the listener selectors.
func (r *ModelDeploymentReconciler) patchGatewayListenerSelector(ctx context.Context, gwConfig *gateway.GatewayConfig, namespaces map[string]bool) error {
	var gw gatewayv1.Gateway
	if err := r.Get(ctx, client.ObjectKey{Name: gwConfig.GatewayName, Namespace: gwConfig.GatewayNamespace}, &gw); err != nil {
		return fmt.Errorf("getting Gateway: %w", err)
	}

	base := gw.DeepCopy()
	fromSelector := gatewayv1.NamespacesFromSelector
	selector := namespaceSelectorFromSet(namespaces)

	for i := range gw.Spec.Listeners {
		if gw.Spec.Listeners[i].AllowedRoutes == nil {
			gw.Spec.Listeners[i].AllowedRoutes = &gatewayv1.AllowedRoutes{}
		}
		gw.Spec.Listeners[i].AllowedRoutes.Namespaces = &gatewayv1.RouteNamespaces{
			From:     &fromSelector,
			Selector: selector,
		}
	}
	if err := r.Patch(ctx, &gw, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patching Gateway listeners: %w", err)
	}
	return nil
}

// allowedNamespacesFromGateway extracts the set of namespaces currently allowed
// by the Gateway's listener selectors (supports both matchLabels and matchExpressions).
func allowedNamespacesFromGateway(gw *gatewayv1.Gateway) map[string]bool {
	ns := make(map[string]bool)
	for _, l := range gw.Spec.Listeners {
		if l.AllowedRoutes == nil || l.AllowedRoutes.Namespaces == nil || l.AllowedRoutes.Namespaces.Selector == nil {
			continue
		}
		sel := l.AllowedRoutes.Namespaces.Selector
		// Legacy single-namespace matchLabels
		if v, ok := sel.MatchLabels["kubernetes.io/metadata.name"]; ok {
			ns[v] = true
		}
		// matchExpressions In-list
		for _, expr := range sel.MatchExpressions {
			if expr.Key == "kubernetes.io/metadata.name" && expr.Operator == metav1.LabelSelectorOpIn {
				for _, v := range expr.Values {
					ns[v] = true
				}
			}
		}
		break // all listeners share the same selector
	}
	return ns
}

// namespaceSelectorFromSet builds a LabelSelector with a matchExpressions In-list
// for the given namespace set.
func namespaceSelectorFromSet(namespaces map[string]bool) *metav1.LabelSelector {
	values := make([]string, 0, len(namespaces))
	for ns := range namespaces {
		values = append(values, ns)
	}
	sort.Strings(values)
	return &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   values,
			},
		},
	}
}

// cleanupGatewayResources removes gateway resources when gateway is disabled or
// the deployment is no longer running. Also sets GatewayReady=False.
func (r *ModelDeploymentReconciler) cleanupGatewayResources(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	// Resolve provider gateway capabilities
	var gatewayCapabilities *airunwayv1alpha1.GatewayCapabilities
	var err error
	if gatewayCapabilities, err = r.resolveProviderGatewayCapabilities(ctx, md); err != nil {
		logger.Info("Error resolving provider gateway capabilities, proceeding without provider-specific gateway capabilities", "error", err)
	}
	providerManagedPool := gatewayCapabilities != nil

	eppName := md.Name + "-epp"

	if !providerManagedPool {
		// Delete InferencePool if it exists
		pool := &inferencev1.InferencePool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      md.Name,
				Namespace: md.Namespace,
			},
		}
		if err := r.Delete(ctx, pool); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete InferencePool: %w", err)
		}
	} else {
		logger.V(1).Info("Skipping InferencePool cleanup because provider manages the pool")
	}

	// Delete auto-created HTTPRoute (skip if user-provided)
	if md.Spec.Gateway == nil || md.Spec.Gateway.HTTPRouteRef == "" {
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      md.Name,
				Namespace: md.Namespace,
			},
		}
		if err := r.Delete(ctx, route); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete HTTPRoute: %w", err)
		}
	}

	if !providerManagedPool {
		// Delete EPP resources
		eppResources := []client.Object{
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: eppName, Namespace: md.Namespace}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: eppName, Namespace: md.Namespace}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: eppName, Namespace: md.Namespace}},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: eppName, Namespace: md.Namespace}},
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: eppName, Namespace: md.Namespace}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: eppName, Namespace: md.Namespace}},
		}

		// Conditionally delete the DestinationRule if Istio is present
		if _, err := r.Client.RESTMapper().RESTMapping(schema.GroupKind{Group: "networking.istio.io", Kind: "DestinationRule"}); err == nil {
			dr := &unstructured.Unstructured{}
			dr.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1beta1", Kind: "DestinationRule"})
			dr.SetName(eppName)
			dr.SetNamespace(md.Namespace)
			eppResources = append(eppResources, dr)
		}

		for _, obj := range eppResources {
			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				logger.V(1).Info("Could not delete EPP resource", "resource", obj.GetObjectKind(), "error", err)
			}
		}
	} else {
		logger.V(1).Info("Skipping deletion of EPP resources because provider manages EPP")
	}

	// Revert Gateway allowedRoutes if no other ModelDeployments in this namespace need gateway access.
	if r.GatewayDetector != nil && r.GatewayDetector.PatchGateway {
		if err := r.cleanupGatewayAllowedRoutes(ctx, md); err != nil {
			logger.V(1).Info("Could not revert Gateway allowedRoutes", "error", err)
		}
	}

	md.Status.Gateway = nil
	r.setCondition(md, airunwayv1alpha1.ConditionTypeGatewayReady, metav1.ConditionFalse, "GatewayDisabled", "Gateway resources cleaned up")

	// Clear the httproute-created annotation so the controller will recreate the
	// HTTPRoute when the deployment recovers to Running. Without this, a transient
	// phase change (e.g. crash-loop) would permanently suppress HTTPRoute recreation.
	if md.Annotations[airunwayv1alpha1.HTTPRouteCreated] == "true" {
		base := md.DeepCopy()
		delete(md.Annotations, airunwayv1alpha1.HTTPRouteCreated)
		if err := r.Patch(ctx, md, client.MergeFrom(base)); err != nil {
			logger.V(1).Info("Could not clear httproute-created annotation during cleanup", "error", err)
		}
	}

	logger.Info("Gateway resources cleaned up", "name", md.Name)
	return nil
}

func (r *ModelDeploymentReconciler) providerInferencePoolExistsOrCreateDefault(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, gatewayCapabilitities *airunwayv1alpha1.GatewayCapabilities, gwConfig *gateway.GatewayConfig) (bool, error) {
	logger := log.FromContext(ctx)

	if gatewayCapabilitities != nil {
		// Provider manages the pool.
		return true, nil
	}

	// Traffic routed to the InferencePool will be forwarded to this port on selected pods (needs the pod/container port, not service port).
	port := int32(8000) // sensible default
	if md.Status.Endpoint != nil && md.Status.Endpoint.Service != "" {
		// Look up the service's target port (the actual container port)
		if targetPort := r.resolveTargetPort(ctx, md.Status.Endpoint.Service, md.Namespace); targetPort > 0 {
			port = targetPort
		} else if md.Status.Endpoint.Port > 0 {
			port = md.Status.Endpoint.Port
		}
	}

	// Ensure model pods have the selector label for InferencePool
	if err := r.labelModelPods(ctx, md); err != nil {
		logger.V(1).Info("Could not label model pods", "error", err)
		// Non-fatal: pods may not exist yet or provider may handle labels
	}

	// Create or update InferencePool
	if err := r.reconcileInferencePool(ctx, md, port, gwConfig.GetBBRNamespace()); err != nil {
		r.setCondition(md, airunwayv1alpha1.ConditionTypeGatewayReady, metav1.ConditionFalse, "InferencePoolFailed", err.Error())
		return false, fmt.Errorf("reconciling InferencePool: %w", err)
	}

	return false, nil
}

// cleanupGatewayAllowedRoutes removes the namespace from the Gateway's allowedRoutes
// if no other gateway-enabled ModelDeployments remain in that namespace.
func (r *ModelDeploymentReconciler) cleanupGatewayAllowedRoutes(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	// Resolve gateway config; if we can't find the gateway, nothing to revert.
	gwConfig, err := r.resolveGatewayConfig(ctx)
	if err != nil {
		return nil
	}

	// Only relevant for cross-namespace routing.
	if md.Namespace == gwConfig.GatewayNamespace {
		return nil
	}

	// Check if any other ModelDeployments in the same namespace still need gateway access.
	var mdList airunwayv1alpha1.ModelDeploymentList
	if err := r.List(ctx, &mdList, client.InNamespace(md.Namespace)); err != nil {
		return fmt.Errorf("listing ModelDeployments: %w", err)
	}
	for i := range mdList.Items {
		other := &mdList.Items[i]
		if other.UID == md.UID {
			continue
		}
		// If another MD exists that hasn't opted out of gateway, keep the route.
		if other.Spec.Gateway == nil || other.Spec.Gateway.Enabled == nil || *other.Spec.Gateway.Enabled {
			return nil
		}
	}

	// No other MDs need gateway in this namespace — remove it from the In-list.
	var gw gatewayv1.Gateway
	if err := r.Get(ctx, client.ObjectKey{Name: gwConfig.GatewayName, Namespace: gwConfig.GatewayNamespace}, &gw); err != nil {
		return fmt.Errorf("getting Gateway: %w", err)
	}

	existing := allowedNamespacesFromGateway(&gw)
	if !existing[md.Namespace] {
		return nil // not in the list, nothing to do
	}
	delete(existing, md.Namespace)

	if len(existing) == 0 {
		// No cross-namespace routes remain — revert to SameNamespace.
		fromSame := gatewayv1.NamespacesFromSame
		base := gw.DeepCopy()
		for i := range gw.Spec.Listeners {
			if gw.Spec.Listeners[i].AllowedRoutes != nil {
				gw.Spec.Listeners[i].AllowedRoutes.Namespaces = &gatewayv1.RouteNamespaces{
					From: &fromSame,
				}
			}
		}
		if err := r.Patch(ctx, &gw, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("reverting Gateway listeners: %w", err)
		}
	} else {
		// Other namespaces still need access — update the In-list without this namespace.
		if err := r.patchGatewayListenerSelector(ctx, gwConfig, existing); err != nil {
			return fmt.Errorf("updating Gateway listeners: %w", err)
		}
	}

	logger.Info("Removed namespace from Gateway allowedRoutes", "gateway", gwConfig.GatewayName, "namespace", md.Namespace)
	return nil
}

// cleanupGatewayAllowedRoutesForNamespace removes a namespace from the Gateway's
// allowedRoutes when a ModelDeployment has already been deleted (no MD object available).
// It checks whether any remaining MDs in the namespace still need gateway access.
func (r *ModelDeploymentReconciler) cleanupGatewayAllowedRoutesForNamespace(ctx context.Context, namespace string) {
	logger := log.FromContext(ctx)

	if r.GatewayDetector == nil || !r.GatewayDetector.PatchGateway {
		return
	}
	if !r.GatewayDetector.IsAvailable(ctx) {
		return
	}

	gwConfig, err := r.resolveGatewayConfig(ctx)
	if err != nil {
		return
	}
	if namespace == gwConfig.GatewayNamespace {
		return
	}

	// Check if any remaining MDs in the namespace still need gateway access.
	var mdList airunwayv1alpha1.ModelDeploymentList
	if err := r.List(ctx, &mdList, client.InNamespace(namespace)); err != nil {
		logger.V(1).Info("Could not list ModelDeployments for gateway cleanup", "namespace", namespace, "error", err)
		return
	}
	for i := range mdList.Items {
		other := &mdList.Items[i]
		if other.Spec.Gateway == nil || other.Spec.Gateway.Enabled == nil || *other.Spec.Gateway.Enabled {
			return // another MD still needs gateway
		}
	}

	// No MDs need gateway in this namespace — remove it from the In-list.
	var gw gatewayv1.Gateway
	if err := r.Get(ctx, client.ObjectKey{Name: gwConfig.GatewayName, Namespace: gwConfig.GatewayNamespace}, &gw); err != nil {
		logger.V(1).Info("Could not get Gateway for cleanup", "error", err)
		return
	}

	existing := allowedNamespacesFromGateway(&gw)
	if !existing[namespace] {
		return
	}
	delete(existing, namespace)

	if len(existing) == 0 {
		fromSame := gatewayv1.NamespacesFromSame
		base := gw.DeepCopy()
		for i := range gw.Spec.Listeners {
			if gw.Spec.Listeners[i].AllowedRoutes != nil {
				gw.Spec.Listeners[i].AllowedRoutes.Namespaces = &gatewayv1.RouteNamespaces{
					From: &fromSame,
				}
			}
		}
		if err := r.Patch(ctx, &gw, client.MergeFrom(base)); err != nil {
			logger.V(1).Info("Could not revert Gateway listeners", "error", err)
			return
		}
	} else {
		if err := r.patchGatewayListenerSelector(ctx, gwConfig, existing); err != nil {
			logger.V(1).Info("Could not update Gateway listeners", "error", err)
			return
		}
	}

	logger.Info("Removed namespace from Gateway allowedRoutes after MD deletion", "gateway", gwConfig.GatewayName, "namespace", namespace)
}

// restartBBRIfPresent triggers a rolling restart of the body-based-router Deployment (if present
// in the given namespace) by updating its restart annotation. This is necessary because BBR builds
// its internal model registry on startup and does not dynamically watch InferencePools.
//
// The namespace is resolved by GatewayConfig.GetBBRNamespace(), which reads the
// airunway.ai/bbr-namespace annotation from the Gateway resource, falling back to the
// Gateway's own namespace.
func (r *ModelDeploymentReconciler) restartBBRIfPresent(ctx context.Context, namespace string) error {
	var bbr appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Name: "body-based-router", Namespace: namespace}, &bbr); err != nil {
		return client.IgnoreNotFound(err)
	}
	patch := []byte(`{"spec":{"template":{"metadata":{"annotations":{"airunway.ai/restartedAt":"` + time.Now().UTC().Format(time.RFC3339) + `"}}}}}`)
	if err := r.Patch(ctx, &bbr, client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
		return fmt.Errorf("patching body-based-router: %w", err)
	}
	log.FromContext(ctx).Info("Triggered BBR rolling restart to discover new InferencePool", "namespace", namespace)
	return nil
}
