/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

type stubProviderReader struct {
	get func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
}

func (s stubProviderReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if s.get != nil {
		return s.get(ctx, key, obj, opts...)
	}
	return nil
}

func (s stubProviderReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return nil
}

func providerValidationDeployment() *airunwayv1alpha1.ModelDeployment {
	return &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model:     airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
			Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Provider:  &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
			Serving:   &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeAggregated},
			Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}},
		},
	}
}

func gpuOnlyDynamoConfig() *airunwayv1alpha1.InferenceProviderConfig {
	return &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "dynamo"},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			Capabilities: &airunwayv1alpha1.ProviderCapabilities{
				Engines: []airunwayv1alpha1.EngineCapability{{
					Name:         airunwayv1alpha1.EngineTypeVLLM,
					ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated},
					GPUSupport:   true,
					CPUSupport:   false,
				}},
			},
		},
	}
}

func TestValidateProviderCompatibility_DynamoMocker(t *testing.T) {
	t.Parallel()
	obj := providerValidationDeployment()
	obj.Annotations = map[string]string{"airunway.ai/dynamo-test-backend": "mocker"}

	result := validateProviderCompatibility(context.Background(), obj, field.NewPath("spec"), airunwayv1alpha1.ServingModeAggregated, nil, nil)
	if !result.IsDynamoMocker {
		t.Fatalf("expected Dynamo mocker detection")
	}
	if len(result.Errors) != 0 {
		t.Fatalf("expected vllm mocker to be accepted, got %v", result.Errors.ToAggregate())
	}

	obj.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang
	result = validateProviderCompatibility(context.Background(), obj, field.NewPath("spec"), airunwayv1alpha1.ServingModeAggregated, nil, nil)
	if len(result.Errors) == 0 || !strings.Contains(result.Errors.ToAggregate().Error(), "only supports the vllm engine") {
		t.Fatalf("expected non-vllm mocker error, got %v", result.Errors.ToAggregate())
	}
}

func TestValidateProviderCompatibility_ProviderLookup(t *testing.T) {
	t.Parallel()
	providerResource := schema.GroupResource{Group: "airunway.ai", Resource: "inferenceproviderconfigs"}

	t.Run("uses APIReader fallback when cache misses", func(t *testing.T) {
		obj := providerValidationDeployment()
		cache := stubProviderReader{get: func(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewNotFound(providerResource, "dynamo")
		}}
		api := stubProviderReader{get: func(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			*obj.(*airunwayv1alpha1.InferenceProviderConfig) = *gpuOnlyDynamoConfig()
			return nil
		}}

		result := validateProviderCompatibility(context.Background(), obj, field.NewPath("spec"), airunwayv1alpha1.ServingModeAggregated, cache, api)
		if len(result.Errors) != 0 {
			t.Fatalf("expected APIReader fallback to satisfy lookup, got %v", result.Errors.ToAggregate())
		}
	})

	t.Run("rejects missing provider after fallback", func(t *testing.T) {
		obj := providerValidationDeployment()
		missing := stubProviderReader{get: func(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewNotFound(providerResource, "missing")
		}}

		result := validateProviderCompatibility(context.Background(), obj, field.NewPath("spec"), airunwayv1alpha1.ServingModeAggregated, missing, missing)
		if len(result.Errors) == 0 || !strings.Contains(result.Errors.ToAggregate().Error(), "InferenceProviderConfig") {
			t.Fatalf("expected missing provider error, got %v", result.Errors.ToAggregate())
		}
	})

	t.Run("warns and skips on transient lookup errors", func(t *testing.T) {
		obj := providerValidationDeployment()
		transient := stubProviderReader{get: func(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("temporary network error")
		}}

		result := validateProviderCompatibility(context.Background(), obj, field.NewPath("spec"), airunwayv1alpha1.ServingModeAggregated, transient, nil)
		if len(result.Errors) != 0 {
			t.Fatalf("expected transient lookup error not to block admission, got %v", result.Errors.ToAggregate())
		}
		if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "could not verify provider") {
			t.Fatalf("expected transient warning, got %v", result.Warnings)
		}
	})
}

func TestValidateProviderCompatibility_CapabilityErrors(t *testing.T) {
	t.Parallel()
	obj := providerValidationDeployment()
	obj.Spec.Resources.GPU.Count = 0
	reader := stubProviderReader{get: func(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
		*obj.(*airunwayv1alpha1.InferenceProviderConfig) = *gpuOnlyDynamoConfig()
		return nil
	}}

	result := validateProviderCompatibility(context.Background(), obj, field.NewPath("spec"), airunwayv1alpha1.ServingModeAggregated, reader, nil)
	if len(result.Errors) == 0 || !strings.Contains(result.Errors.ToAggregate().Error(), "requires GPU") {
		t.Fatalf("expected GPU required compatibility error, got %v", result.Errors.ToAggregate())
	}
}

// Ensure stubProviderReader satisfies client.Reader even if interface changes are caught at compile time.
var _ client.Reader = stubProviderReader{}
var _ runtime.Object = &airunwayv1alpha1.InferenceProviderConfig{}
