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
	"time"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
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

// RetryStatusUpdate retries the supplied status update callback with a linear
// backoff schedule: baseDelay, 2*baseDelay, ..., n*baseDelay.
func RetryStatusUpdate(ctx context.Context, attempts int, baseDelay time.Duration, update func(context.Context) error) error {
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := update(ctx)
		if err == nil {
			return nil
		}
		lastErr = err

		if i == attempts-1 {
			break
		}

		delay := time.Duration(i+1) * baseDelay
		if delay <= 0 {
			continue
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}

	return lastErr
}

// StartHeartbeatLoop runs a provider heartbeat ticker loop in a goroutine.
// The tick callback is executed every interval; callback errors are logged and
// do not stop the loop.
func StartHeartbeatLoop(ctx context.Context, interval time.Duration, tick func(context.Context) error) {
	logger := log.FromContext(ctx)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				logger.Info("Stopping heartbeat goroutine")
				return
			case <-ticker.C:
				if err := tick(ctx); err != nil {
					logger.Error(err, "Failed to update heartbeat")
				}
			}
		}
	}()
}

// IsAPIResourceInstalled checks whether a backend API resource is available.
// It prefers discovery when a discovery client is provided, and falls back to
// RESTMapper lookup otherwise.
func IsAPIResourceInstalled(
	kubeClient client.Client,
	discoveryClient discovery.DiscoveryInterface,
	group, version, kind, resource string,
) bool {
	if discoveryClient != nil {
		return hasAPIResource(discoveryClient, group, version, resource)
	}

	mapper := kubeClient.RESTMapper()
	if mapper == nil {
		return false
	}

	_, err := mapper.RESTMapping(schema.GroupKind{Group: group, Kind: kind}, version)
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
