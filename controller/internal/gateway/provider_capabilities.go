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

package gateway

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

// ProviderCapabilityResolver looks up the gateway capabilities declared by
// a provider's InferenceProviderConfig.
type ProviderCapabilityResolver interface {
	// GetGatewayCapabilities returns the GatewayCapabilities for the given
	// provider name, or nil if the provider does not declare any GatewayCapabilities.
	GetGatewayCapabilities(ctx context.Context, providerName string) *airunwayv1alpha1.GatewayCapabilities
}

// InferenceProviderConfigResolver implements ProviderCapabilityResolver by
// fetching InferenceProviderConfig custom resources from the cluster.
type InferenceProviderConfigResolver struct {
	client client.Client
}

// NewInferenceProviderConfigResolver creates a resolver that reads
// InferenceProviderConfig CRs to determine gateway capabilities.
func NewInferenceProviderConfigResolver(c client.Client) *InferenceProviderConfigResolver {
	return &InferenceProviderConfigResolver{client: c}
}

// GetGatewayCapabilities fetches the InferenceProviderConfig for the given
// provider name and returns its gateway capabilities. Returns nil if the
// provider has no gateway config.
func (r *InferenceProviderConfigResolver) GetGatewayCapabilities(ctx context.Context, providerName string) *airunwayv1alpha1.GatewayCapabilities {
	logger := log.FromContext(ctx)

	var ipc airunwayv1alpha1.InferenceProviderConfig
	if err := r.client.Get(ctx, client.ObjectKey{Name: providerName}, &ipc); err != nil {
		logger.V(1).Info("Could not fetch InferenceProviderConfig for gateway capability lookup",
			"provider", providerName, "error", err)
		return nil
	}

	if ipc.Spec.Capabilities == nil {
		return nil
	}

	return ipc.Spec.Capabilities.Gateway
}
