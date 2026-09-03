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

// Package shim contains shared helper logic for provider registration and status
// handling used by provider-specific modules.
package shim

import (
	"context"
	"fmt"
	"maps"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	ReasonUnregistered  = "Unregistered"
	MessageUnregistered = "AI Runway integration is shutting down."
)

// RegisterProviderConfig creates or updates the cluster-scoped InferenceProviderConfig
// used to register a provider with the controller.
func RegisterProviderConfig(
	ctx context.Context,
	kubeClient client.Client,
	name string,
	annotations map[string]string,
	spec airunwayv1alpha1.InferenceProviderConfigSpec,
) error {
	logger := log.FromContext(ctx)
	config := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
		Spec: spec,
	}

	existing := &airunwayv1alpha1.InferenceProviderConfig{}
	err := kubeClient.Get(ctx, types.NamespacedName{Name: name}, existing)

	if apierrors.IsNotFound(err) {
		logger.Info("Creating InferenceProviderConfig", "name", name)
		if err := kubeClient.Create(ctx, config); err != nil {
			return fmt.Errorf("failed to create InferenceProviderConfig: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get InferenceProviderConfig: %w", err)
	} else {
		existing.Spec = config.Spec
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string)
		}
		maps.Copy(existing.Annotations, annotations)
		logger.Info("Updating InferenceProviderConfig", "name", name)
		if err := kubeClient.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update InferenceProviderConfig: %w", err)
		}
	}

	return nil
}

// UpdateProviderConfigStatus updates InferenceProviderConfig.status for a
// provider without modifying spec or annotations.
func UpdateProviderConfigStatus(
	ctx context.Context,
	kubeClient client.Client,
	name string,
	ready bool,
	version string,
	upstreamCRDVersion string,
) error {
	config := &airunwayv1alpha1.InferenceProviderConfig{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: name}, config); err != nil {
		return fmt.Errorf("failed to get InferenceProviderConfig: %w", err)
	}

	now := metav1.Now()
	config.Status = airunwayv1alpha1.InferenceProviderConfigStatus{
		Ready:              ready,
		Version:            version,
		LastHeartbeat:      &now,
		UpstreamCRDVersion: upstreamCRDVersion,
	}

	if err := kubeClient.Status().Update(ctx, config); err != nil {
		return fmt.Errorf("failed to update InferenceProviderConfig status: %w", err)
	}

	return nil
}

// MarkProviderConfigUnregistered records an immediate, provider-independent
// shutdown signal while preserving the provider's last reported status fields.
func MarkProviderConfigUnregistered(
	ctx context.Context,
	kubeClient client.Client,
	name string,
) error {
	config := &airunwayv1alpha1.InferenceProviderConfig{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: name}, config); err != nil {
		return fmt.Errorf("failed to get InferenceProviderConfig: %w", err)
	}

	now := metav1.Now()
	config.Status.Ready = false
	config.Status.LastHeartbeat = &now
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:    "UpstreamReady",
		Status:  metav1.ConditionFalse,
		Reason:  ReasonUnregistered,
		Message: MessageUnregistered,
	})

	if err := kubeClient.Status().Update(ctx, config); err != nil {
		return fmt.Errorf("failed to update InferenceProviderConfig status: %w", err)
	}
	return nil
}
