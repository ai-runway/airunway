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

package kuberay

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

const (
	// ProviderConfigName is the name of the InferenceProviderConfig for KubeRay
	ProviderConfigName = "kuberay"

	// ProviderVersion is the version of the KubeRay provider
	ProviderVersion = "kuberay-provider:v0.1.0"

	// ProviderDocumentation is the documentation URL for the KubeRay provider
	ProviderDocumentation = "https://github.com/kaito-project/airunway/tree/main/docs/providers/kuberay.md"

	// HeartbeatInterval is the interval for updating the provider heartbeat
	HeartbeatInterval = 1 * time.Minute

	rayServiceResource = "rayservices"
)

// ProviderConfigManager handles registration and heartbeat for the KubeRay provider
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

// GetProviderConfigSpec returns the InferenceProviderConfigSpec for KubeRay
func GetProviderConfigSpec() airunwayv1alpha1.InferenceProviderConfigSpec {
	return airunwayv1alpha1.InferenceProviderConfigSpec{
		Capabilities: &airunwayv1alpha1.ProviderCapabilities{
			Engines: []airunwayv1alpha1.EngineType{
				airunwayv1alpha1.EngineTypeVLLM,
			},
			ServingModes: []airunwayv1alpha1.ServingMode{
				airunwayv1alpha1.ServingModeAggregated,
				airunwayv1alpha1.ServingModeDisaggregated,
			},
			CPUSupport: false,
			GPUSupport: true,
		},
		SelectionRules: []airunwayv1alpha1.SelectionRule{
			{
				Condition: "has(spec.resources.gpu) && spec.resources.gpu.count > 1 && spec.engine.type == 'vllm'",
				Priority:  80,
			},
		},
	}
}

// GetInstallationInfo returns the installation metadata for KubeRay
func GetInstallationInfo() *airunwayv1alpha1.InstallationInfo {
	return &airunwayv1alpha1.InstallationInfo{
		Description:      "Ray Serve via KubeRay for distributed Ray-based model serving with vLLM",
		DefaultNamespace: "ray-system",
		HelmRepos: []airunwayv1alpha1.HelmRepo{
			{Name: "kuberay", URL: "https://ray-project.github.io/kuberay-helm/"},
		},
		HelmCharts: []airunwayv1alpha1.HelmChart{
			{
				Name:            "kuberay-operator",
				Chart:           "kuberay/kuberay-operator",
				Version:         "1.3.0",
				Namespace:       "ray-system",
				CreateNamespace: true,
			},
		},
		Steps: []airunwayv1alpha1.InstallationStep{
			{
				Title:       "Add KubeRay Helm Repository",
				Command:     "helm repo add kuberay https://ray-project.github.io/kuberay-helm/",
				Description: "Add the KubeRay Helm repository.",
			},
			{
				Title:       "Update Helm Repositories",
				Command:     "helm repo update",
				Description: "Update local Helm repository cache.",
			},
			{
				Title:       "Install KubeRay Operator",
				Command:     "helm upgrade --install kuberay-operator kuberay/kuberay-operator --version 1.3.0 -n ray-system --create-namespace --wait",
				Description: "Install the KubeRay operator v1.3.0.",
			},
		},
	}
}

// Register creates or updates the InferenceProviderConfig for KubeRay
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

	// Update status — retry briefly after create to allow cache to sync
	ready := m.checkBackendCRDInstalled()
	var statusErr error
	for i := 0; i < 5; i++ {
		statusErr = m.UpdateStatus(ctx, ready)
		if statusErr == nil {
			break
		}
		time.Sleep(time.Duration(i+1) * 200 * time.Millisecond)
	}
	if !ready {
		logger.Info("Backend CRD not installed, provider registered as not ready", "group", RayAPIGroup, "kind", RayServiceKind)
	}
	return statusErr
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
		UpstreamCRDVersion: "ray.io/v1",
	}

	if err := m.client.Status().Update(ctx, config); err != nil {
		return fmt.Errorf("failed to update InferenceProviderConfig status: %w", err)
	}

	return nil
}

// checkBackendCRDInstalled checks if the upstream RayService CRD is installed
func (m *ProviderConfigManager) checkBackendCRDInstalled() bool {
	if m.discoveryClient != nil {
		return hasAPIResource(m.discoveryClient, RayAPIGroup, RayAPIVersion, rayServiceResource)
	}

	mapper := m.client.RESTMapper()
	if mapper == nil {
		return false
	}
	_, err := mapper.RESTMapping(schema.GroupKind{
		Group: RayAPIGroup,
		Kind:  RayServiceKind,
	}, RayAPIVersion)
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
					logger.Info("Backend CRD not installed, reporting not ready", "group", RayAPIGroup, "kind", RayServiceKind)
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
