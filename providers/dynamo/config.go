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

package dynamo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

const (
	// ProviderConfigName is the name of the InferenceProviderConfig for Dynamo
	ProviderConfigName = "dynamo"

	// ProviderVersion is the version of the AIRunway Dynamo provider controller.
	ProviderVersion = "dynamo-provider:v0.2.0"

	// DynamoPlatformChartVersion is the upstream Dynamo platform chart version.
	DynamoPlatformChartVersion = "1.0.2"

	// DynamoPlatformChartURL is the upstream Dynamo platform chart package.
	DynamoPlatformChartURL = "https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-" + DynamoPlatformChartVersion + ".tgz"

	// ProviderDocumentation is the documentation URL for the Dynamo provider
	ProviderDocumentation = "https://github.com/kaito-project/airunway/tree/main/docs/providers/dynamo.md"

	// HeartbeatInterval is the interval for updating the provider heartbeat
	HeartbeatInterval = 1 * time.Minute

	dynamoPlatformValuesJSON      = `{"global.grove.install":true}`
	dynamoGraphDeploymentResource = "dynamographdeployments"
)

// ProviderConfigManager handles registration and heartbeat for the Dynamo provider
type ProviderConfigManager struct {
	client          client.Client
	discoveryClient discovery.DiscoveryInterface
}

// NewProviderConfigManager creates a new provider config manager
func NewProviderConfigManager(c client.Client, discoveryClients ...discovery.DiscoveryInterface) *ProviderConfigManager {
	manager := &ProviderConfigManager{
		client: c,
	}
	if len(discoveryClients) > 0 {
		manager.discoveryClient = discoveryClients[0]
	}
	return manager
}

// GetProviderConfigSpec returns the InferenceProviderConfigSpec for Dynamo
func GetProviderConfigSpec() airunwayv1alpha1.InferenceProviderConfigSpec {
	return airunwayv1alpha1.InferenceProviderConfigSpec{
		Capabilities: &airunwayv1alpha1.ProviderCapabilities{
			Engines: []airunwayv1alpha1.EngineType{
				airunwayv1alpha1.EngineTypeVLLM,
				airunwayv1alpha1.EngineTypeSGLang,
				airunwayv1alpha1.EngineTypeTRTLLM,
			},
			ServingModes: []airunwayv1alpha1.ServingMode{
				airunwayv1alpha1.ServingModeAggregated,
				airunwayv1alpha1.ServingModeDisaggregated,
			},
			CPUSupport: false,
			GPUSupport: true,
			Gateway: &airunwayv1alpha1.GatewayCapabilities{
				// The Dynamo operator creates the InferencePool as
				// "{DynamoGraphDeployment.metadata.name}-pool" in the same
				// namespace as the DGD.
				InferencePoolNamePattern: "{name}-pool",
				InferencePoolNamespace:   "{namespace}",
				// With Dynamo v1.1.0+, the frontendSidecar feature colocates a
				// frontend on each worker pod, making the InferencePool/EPP
				// path viable. No need to bypass to the Frontend
				// Service. Requests route through InferencePool directly.
			},
		},
		SelectionRules: []airunwayv1alpha1.SelectionRule{
			{
				Condition: "spec.engine.type == 'trtllm'",
				Priority:  100,
			},
			{
				Condition: "spec.engine.type == 'sglang'",
				Priority:  100,
			},
			{
				Condition: "has(spec.serving) && spec.serving.mode == 'disaggregated'",
				Priority:  90,
			},
			{
				Condition: "has(spec.resources.gpu) && spec.resources.gpu.count > 0 && spec.engine.type == 'vllm'",
				Priority:  50,
			},
		},
	}
}

// GetInstallationInfo returns the installation metadata for Dynamo
func GetInstallationInfo() *airunwayv1alpha1.InstallationInfo {
	return &airunwayv1alpha1.InstallationInfo{
		Description:      "NVIDIA Dynamo for high-performance GPU inference",
		DefaultNamespace: "dynamo-system",
		HelmRepos: []airunwayv1alpha1.HelmRepo{
			{Name: "nvidia-ai-dynamo", URL: "https://helm.ngc.nvidia.com/nvidia/ai-dynamo"},
		},
		HelmCharts: []airunwayv1alpha1.HelmChart{
			{
				Name:            "dynamo-platform",
				Chart:           DynamoPlatformChartURL,
				Namespace:       "dynamo-system",
				CreateNamespace: true,
				Values: &runtime.RawExtension{
					Raw: []byte(dynamoPlatformValuesJSON),
				},
			},
		},
		Steps: []airunwayv1alpha1.InstallationStep{
			{
				Title:       "Install Dynamo Platform",
				Command:     "helm upgrade --install dynamo-platform " + DynamoPlatformChartURL + " --namespace dynamo-system --create-namespace --set-json global.grove.install=true",
				Description: "Install the Dynamo platform operator v1.0.2 with bundled Grove enabled by default. This chart includes the required CRDs.",
			},
		},
	}
}

// Register creates or updates the InferenceProviderConfig for Dynamo
func (m *ProviderConfigManager) Register(ctx context.Context) error {
	logger := log.FromContext(ctx)

	annotations, err := buildAnnotations()
	if err != nil {
		return fmt.Errorf("failed to build annotations: %w", err)
	}

	config := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ProviderConfigName,
			Annotations: annotations,
		},
		Spec: GetProviderConfigSpec(),
	}

	existing := &airunwayv1alpha1.InferenceProviderConfig{}
	err = m.client.Get(ctx, types.NamespacedName{Name: ProviderConfigName}, existing)

	if errors.IsNotFound(err) {
		logger.Info("Creating InferenceProviderConfig", "name", ProviderConfigName)
		if err := m.client.Create(ctx, config); err != nil {
			return fmt.Errorf("failed to create InferenceProviderConfig: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get InferenceProviderConfig: %w", err)
	} else {
		existing.Spec = config.Spec
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string)
		}
		for k, v := range annotations {
			existing.Annotations[k] = v
		}
		logger.Info("Updating InferenceProviderConfig", "name", ProviderConfigName)
		if err := m.client.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update InferenceProviderConfig: %w", err)
		}
	}

	// Update status — check if backend CRD is installed
	ready := m.checkBackendCRDInstalled()
	if !ready {
		logger.Info("Backend CRD not installed, provider registered as not ready", "group", DynamoAPIGroup, "kind", DynamoGraphDeploymentKind)
	}
	return m.UpdateStatus(ctx, ready)
}

// checkBackendCRDInstalled checks if the upstream DynamoGraphDeployment CRD is installed
func (m *ProviderConfigManager) checkBackendCRDInstalled() bool {
	if m.discoveryClient != nil {
		return hasAPIResource(m.discoveryClient, DynamoAPIGroup, DynamoAPIVersion, dynamoGraphDeploymentResource)
	}

	mapper := m.client.RESTMapper()
	if mapper == nil {
		return false
	}
	_, err := mapper.RESTMapping(schema.GroupKind{
		Group: DynamoAPIGroup,
		Kind:  DynamoGraphDeploymentKind,
	}, DynamoAPIVersion)
	return err == nil
}

func hasAPIResource(discoveryClient discovery.DiscoveryInterface, group, version, resource string) bool {
	resources, err := discoveryClient.ServerResourcesForGroupVersion(fmt.Sprintf("%s/%s", group, version))
	if err != nil {
		return false
	}

	for _, apiResource := range resources.APIResources {
		if apiResource.Name == resource {
			return true
		}
	}

	return false
}

// UpdateStatus updates the status of the InferenceProviderConfig
func (m *ProviderConfigManager) UpdateStatus(ctx context.Context, ready bool) error {
	config := &airunwayv1alpha1.InferenceProviderConfig{}
	if err := m.client.Get(ctx, types.NamespacedName{Name: ProviderConfigName}, config); err != nil {
		return fmt.Errorf("failed to get InferenceProviderConfig: %w", err)
	}

	now := metav1.Now()
	config.Status = airunwayv1alpha1.InferenceProviderConfigStatus{
		Ready:              ready,
		Version:            ProviderVersion,
		LastHeartbeat:      &now,
		UpstreamCRDVersion: fmt.Sprintf("%s/%s", DynamoAPIGroup, DynamoAPIVersion),
	}

	if err := m.client.Status().Update(ctx, config); err != nil {
		return fmt.Errorf("failed to update InferenceProviderConfig status: %w", err)
	}

	return nil
}

// StartHeartbeat starts a goroutine that periodically updates the provider heartbeat
func (m *ProviderConfigManager) StartHeartbeat(ctx context.Context) {
	logger := log.FromContext(ctx)

	go func() {
		ticker := time.NewTicker(HeartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				logger.Info("Stopping heartbeat goroutine")
				return
			case <-ticker.C:
				ready := m.checkBackendCRDInstalled()
				if !ready {
					logger.Info("Backend CRD not installed, reporting not ready", "group", DynamoAPIGroup, "kind", DynamoGraphDeploymentKind)
				}
				if err := m.UpdateStatus(ctx, ready); err != nil {
					logger.Error(err, "Failed to update heartbeat")
				}
			}
		}
	}()
}

// Unregister marks the provider as not ready
func (m *ProviderConfigManager) Unregister(ctx context.Context) error {
	return m.UpdateStatus(ctx, false)
}

func buildAnnotations() (map[string]string, error) {
	installJSON, err := json.Marshal(GetInstallationInfo())
	if err != nil {
		return nil, fmt.Errorf("failed to marshal installation info: %w", err)
	}
	return map[string]string{
		airunwayv1alpha1.AnnotationInstallation:  string(installJSON),
		airunwayv1alpha1.AnnotationDocumentation: ProviderDocumentation,
	}, nil
}
