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

package kaito

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

const (
	// ProviderConfigName is the name of the InferenceProviderConfig for KAITO
	ProviderConfigName = "kaito"

	// ProviderVersion is the version of the KAITO provider
	ProviderVersion = "kaito-provider:v0.1.0"

	// ProviderDocumentation is the documentation URL for the KAITO provider
	ProviderDocumentation = "https://github.com/kaito-project/airunway/tree/main/docs/providers/kaito.md"

	// HeartbeatInterval is the interval for updating the provider heartbeat
	HeartbeatInterval = 1 * time.Minute
)

// ProviderConfigManager handles registration and heartbeat for the KAITO provider
type ProviderConfigManager struct {
	client       client.Client
	directClient client.Client
}

// NewProviderConfigManager creates a new provider config manager
func NewProviderConfigManager(c client.Client, direct client.Client) *ProviderConfigManager {
	return &ProviderConfigManager{
		client:       c,
		directClient: direct,
	}
}

// GetProviderConfigSpec returns the InferenceProviderConfigSpec for KAITO
func GetProviderConfigSpec() airunwayv1alpha1.InferenceProviderConfigSpec {
	return airunwayv1alpha1.InferenceProviderConfigSpec{
		Capabilities: &airunwayv1alpha1.ProviderCapabilities{
			Engines: []airunwayv1alpha1.EngineCapability{
				{
					Name: airunwayv1alpha1.EngineTypeVLLM,
					ServingModes: []airunwayv1alpha1.ServingMode{
						airunwayv1alpha1.ServingModeAggregated,
					},
					GPUSupport: true,
				},
				{
					Name: airunwayv1alpha1.EngineTypeLlamaCpp,
					ServingModes: []airunwayv1alpha1.ServingMode{
						airunwayv1alpha1.ServingModeAggregated,
					},
					GPUSupport: true,
					CPUSupport: true,
					// KAITO's llama.cpp deployment does not expose an
					// OpenAI-style served-name endpoint, so gateway routing
					// must fall back to spec.model.id rather than honoring
					// spec.model.servedName.
					Gateway: &airunwayv1alpha1.GatewayCapabilities{
						IgnoresServedName: true,
					},
				},
			},
		},
		SelectionRules: []airunwayv1alpha1.SelectionRule{
			{
				Condition: "!has(spec.resources.gpu) || spec.resources.gpu.count == 0",
				Priority:  100,
			},
			{
				Condition: "spec.engine.type == 'llamacpp'",
				Priority:  100,
			},
		},
	}
}

// GetInstallationInfo returns the installation metadata for KAITO
func GetInstallationInfo() *airunwayv1alpha1.InstallationInfo {
	return &airunwayv1alpha1.InstallationInfo{
		Description:      "Kubernetes AI Toolchain Operator for simplified model deployment",
		DefaultNamespace: "kaito-workspace",
		HelmRepos: []airunwayv1alpha1.HelmRepo{
			{Name: "kaito", URL: "https://kaito-project.github.io/kaito/charts/kaito"},
		},
		HelmCharts: []airunwayv1alpha1.HelmChart{
			{
				Name:            "kaito-workspace",
				Chart:           "kaito/workspace",
				Version:         "0.10.0",
				Namespace:       "kaito-workspace",
				CreateNamespace: true,
			},
		},
		Steps: []airunwayv1alpha1.InstallationStep{
			{
				Title:       "Add KAITO Helm Repository",
				Command:     "helm repo add kaito https://kaito-project.github.io/kaito/charts/kaito",
				Description: "Add the KAITO Helm repository.",
			},
			{
				Title:       "Update Helm Repositories",
				Command:     "helm repo update",
				Description: "Update local Helm repository cache.",
			},
			{
				Title:       "Install KAITO Workspace Operator",
				Command:     "helm upgrade --install kaito-workspace kaito/workspace --version 0.10.0 -n kaito-workspace --create-namespace --set featureGates.disableNodeAutoProvisioning=true --set nvidiaDevicePlugin.enabled=false --set localCSIDriver.useLocalCSIDriver=false --set gpu-feature-discovery.gfd.enabled=false --set gpu-feature-discovery.nfd.master.deploy=false --set gpu-feature-discovery.nfd.worker.deploy=false --wait",
				Description: "Install the KAITO workspace operator v0.10.0 with Node Auto-Provisioning disabled (BYO nodes mode), and sub-chart dependencies disabled.",
			},
		},
	}
}

// Register creates or updates the InferenceProviderConfig for KAITO
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
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		statusErr = m.UpdateStatusFromProbe(probeCtx)
		cancel()
		if statusErr == nil {
			break
		}
		time.Sleep(time.Duration(i+1) * 200 * time.Millisecond)
	}
	return statusErr
}

// UpdateStatusFromProbe runs probeUpstreamController and writes the result into
// InferenceProviderConfig.status.
func (m *ProviderConfigManager) UpdateStatusFromProbe(ctx context.Context) error {
	logger := log.FromContext(ctx)

	probe := probeUpstreamController(ctx, m.directClient)
	if probe.Reason == ReasonProbeFailed {
		logger.Info("upstream probe failed", "reason", probe.Reason, "message", probe.Message)
	}

	config := &airunwayv1alpha1.InferenceProviderConfig{}
	if err := m.client.Get(ctx, types.NamespacedName{Name: ProviderConfigName}, config); err != nil {
		return fmt.Errorf("failed to get InferenceProviderConfig: %w", err)
	}

	now := metav1.Now()
	config.Status.Ready = probe.Healthy
	config.Status.Version = ProviderVersion
	config.Status.LastHeartbeat = &now
	config.Status.UpstreamCRDVersion = "kaito.sh/v1beta1"

	// SetStatusCondition preserves LastTransitionTime when Status/Reason/Message
	// don't change, so monitoring/alerting based on transition time keeps working
	// across heartbeats.
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:    "UpstreamReady",
		Status:  boolToConditionStatus(probe.Healthy),
		Reason:  probe.Reason,
		Message: probe.Message,
	})

	if err := m.client.Status().Update(ctx, config); err != nil {
		return fmt.Errorf("failed to update InferenceProviderConfig status: %w", err)
	}
	return nil
}

// MarkUnregistered sets status.ready=false unconditionally. Used by shim shutdown.
func (m *ProviderConfigManager) MarkUnregistered(ctx context.Context) error {
	config := &airunwayv1alpha1.InferenceProviderConfig{}
	if err := m.client.Get(ctx, types.NamespacedName{Name: ProviderConfigName}, config); err != nil {
		return fmt.Errorf("failed to get InferenceProviderConfig: %w", err)
	}

	now := metav1.Now()
	config.Status.Ready = false
	config.Status.LastHeartbeat = &now
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:    "UpstreamReady",
		Status:  metav1.ConditionFalse,
		Reason:  ReasonUnregistered,
		Message: "Shim is shutting down.",
	})

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
				tickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := m.UpdateStatusFromProbe(tickCtx); err != nil {
					logger.Error(err, "Failed to update heartbeat")
				}
				cancel()
			}
		}
	}()
}

// Unregister marks the provider as not ready
func (m *ProviderConfigManager) Unregister(ctx context.Context) error {
	return m.MarkUnregistered(ctx)
}

func boolToConditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
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
