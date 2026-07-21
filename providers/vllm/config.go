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

package vllm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

const (
	// ProviderConfigName is the name of the InferenceProviderConfig for vLLM
	ProviderConfigName = "vllm"

	// ProviderVersion is the version of the vLLM provider
	ProviderVersion = "vllm-provider:v0.1.0"

	// ProviderDocumentation is the documentation URL for the vLLM provider
	ProviderDocumentation = "https://github.com/kaito-project/airunway/tree/main/docs/providers/vllm.md"

	// HeartbeatInterval is the interval for updating the provider heartbeat
	HeartbeatInterval = 1 * time.Minute
)

// ProviderConfigManager handles registration and heartbeat for the vLLM provider
type ProviderConfigManager struct {
	client client.Client
}

// NewProviderConfigManager creates a new provider config manager
func NewProviderConfigManager(c client.Client) *ProviderConfigManager {
	return &ProviderConfigManager{
		client: c,
	}
}

// GetProviderConfigSpec returns the InferenceProviderConfigSpec for vLLM
func GetProviderConfigSpec() airunwayv1alpha1.InferenceProviderConfigSpec {
	return airunwayv1alpha1.InferenceProviderConfigSpec{
		Capabilities: &airunwayv1alpha1.ProviderCapabilities{
			Engines: []airunwayv1alpha1.EngineCapability{
				{
					Name: airunwayv1alpha1.EngineTypeVLLM,
					// Aggregated serving only. The internal prefill/decode
					// rendering is experimental and deliberately unadvertised.
					ServingModes: []airunwayv1alpha1.ServingMode{
						airunwayv1alpha1.ServingModeAggregated,
					},
					GPUSupport: true,
					CPUSupport: false,
				},
			},
		},
		// Direct vLLM is explicit-only: it advertises no selection rules, so the
		// core selector never auto-selects it. Managed providers such as Dynamo
		// and KubeRay remain the auto-selected defaults for vLLM GPU workloads;
		// users opt into Direct vLLM with spec.provider.name: vllm.
		SelectionRules: nil,
	}
}

// GetInstallationInfo returns the installation metadata for vLLM
func GetInstallationInfo() *airunwayv1alpha1.InstallationInfo {
	return &airunwayv1alpha1.InstallationInfo{
		Description: "vLLM provider: deploys vLLM Deployments + Services directly. Requires GPU nodes with the NVIDIA device plugin.",
		Steps: []airunwayv1alpha1.InstallationStep{
			{
				Title:       "Install NVIDIA GPU Device Plugin",
				Command:     "kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.17.0/deployments/static/nvidia-device-plugin.yml",
				Description: "Install the NVIDIA device plugin so GPU nodes advertise nvidia.com/gpu resources.",
			},
			{
				Title:       "Create HuggingFace Token Secret",
				Command:     "kubectl create secret generic vllm-hf-token --from-literal=HF_TOKEN=<your-token> -n <model-namespace>",
				Description: "Create the HuggingFace token secret in the same namespace as your ModelDeployment.",
			},
		},
	}
}

// Register creates or updates the InferenceProviderConfig for vLLM
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
	var statusErr error
	for i := 0; i < 5; i++ {
		statusErr = m.UpdateStatus(ctx, true)
		if statusErr == nil {
			break
		}
		time.Sleep(time.Duration(i+1) * 200 * time.Millisecond)
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
		UpstreamCRDVersion: "apps/v1",
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
				if err := m.UpdateStatus(ctx, true); err != nil {
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
