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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(airunwayv1alpha1.AddToScheme(s))
	return s
}

func TestGetGatewayCapabilities_ProviderWithGateway(t *testing.T) {
	scheme := newTestScheme()
	ipc := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "dynamo"},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			Capabilities: &airunwayv1alpha1.ProviderCapabilities{
				Gateway: &airunwayv1alpha1.GatewayCapabilities{
					InferencePoolNamePattern: "{namespace}-{name}-pool",
					InferencePoolNamespace:   "dynamo-system",
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ipc).Build()
	resolver := NewInferenceProviderConfigResolver(c)

	caps := resolver.GetGatewayCapabilities(context.Background(), "dynamo")
	if caps == nil {
		t.Fatal("expected gateway capabilities, got nil")
	}
	if caps.InferencePoolNamePattern != "{namespace}-{name}-pool" {
		t.Errorf("expected pattern '{namespace}-{name}-pool', got %q", caps.InferencePoolNamePattern)
	}
	if caps.InferencePoolNamespace != "dynamo-system" {
		t.Errorf("expected namespace 'dynamo-system', got %q", caps.InferencePoolNamespace)
	}
}

func TestGetGatewayCapabilities_ProviderNotFound(t *testing.T) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	resolver := NewInferenceProviderConfigResolver(c)

	caps := resolver.GetGatewayCapabilities(context.Background(), "nonexistent")
	if caps != nil {
		t.Errorf("expected nil capabilities for missing provider, got %+v", caps)
	}
}

func TestGetGatewayCapabilities_ProviderWithNilCapabilities(t *testing.T) {
	scheme := newTestScheme()
	ipc := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "kaito"},
		Spec:       airunwayv1alpha1.InferenceProviderConfigSpec{},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ipc).Build()
	resolver := NewInferenceProviderConfigResolver(c)

	caps := resolver.GetGatewayCapabilities(context.Background(), "kaito")
	if caps != nil {
		t.Errorf("expected nil capabilities for provider without capabilities, got %+v", caps)
	}
}

func TestGetGatewayCapabilities_ProviderWithNoGateway(t *testing.T) {
	scheme := newTestScheme()
	ipc := &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "kaito"},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			Capabilities: &airunwayv1alpha1.ProviderCapabilities{
				GPUSupport: true,
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ipc).Build()
	resolver := NewInferenceProviderConfigResolver(c)

	caps := resolver.GetGatewayCapabilities(context.Background(), "kaito")
	if caps != nil {
		t.Errorf("expected nil capabilities for provider without gateway config, got %+v", caps)
	}
}
