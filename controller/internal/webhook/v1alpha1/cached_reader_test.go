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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

// countingReader counts Get calls so we can assert which Reader the validator
// used.
type countingReader struct {
	inner client.Reader
	calls int
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.calls++
	if c.inner == nil {
		return apierrors.NewNotFound(schema.GroupResource{Group: "airunway.ai", Resource: "inferenceproviderconfigs"}, key.Name)
	}
	return c.inner.Get(ctx, key, obj, opts...)
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.inner == nil {
		return nil
	}
	return c.inner.List(ctx, list, opts...)
}

func newProviderConfig(name string) *airunwayv1alpha1.InferenceProviderConfig {
	return &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			Capabilities: &airunwayv1alpha1.ProviderCapabilities{
				Engines: []airunwayv1alpha1.EngineCapability{
					{
						Name:         airunwayv1alpha1.EngineTypeVLLM,
						GPUSupport:   true,
						ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated},
					},
				},
			},
		},
	}
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := airunwayv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func newMD(providerName string) *airunwayv1alpha1.ModelDeployment {
	return &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "md", Namespace: "default"},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID:     "meta-llama/Llama-2-7b-chat-hf",
				Source: airunwayv1alpha1.ModelSourceHuggingFace,
			},
			Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Provider: &airunwayv1alpha1.ProviderSpec{Name: providerName},
			Serving:  &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeAggregated},
			Resources: &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
			},
		},
	}
}

// TestValidator_UsesCachedReaderInSteadyState pins the fix for issue #2: with
// the provider already in the cache, ValidateCreate must not touch the
// uncached APIReader. (Pre-fix the validator was wired with APIReader as
// `Reader`, hammering apiserver on every admission.)
func TestValidator_UsesCachedReaderInSteadyState(t *testing.T) {
	s := newScheme(t)

	cached := &countingReader{
		inner: fake.NewClientBuilder().WithScheme(s).WithObjects(newProviderConfig("dynamo")).Build(),
	}
	api := &countingReader{
		inner: fake.NewClientBuilder().WithScheme(s).WithObjects(newProviderConfig("dynamo")).Build(),
	}

	v := &ModelDeploymentCustomValidator{Reader: cached, APIReader: api}

	if _, err := v.ValidateCreate(context.Background(), newMD("dynamo")); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if cached.calls == 0 {
		t.Errorf("expected cached Reader to be consulted; calls=%d", cached.calls)
	}
	if api.calls != 0 {
		t.Errorf("expected APIReader to remain untouched in steady state; calls=%d", api.calls)
	}
}

// TestValidator_FallsBackToAPIReaderOnCacheMiss covers the informer-warmup
// race: a brand-new InferenceProviderConfig that the cached client hasn't yet
// observed must not produce a spurious "not found" admission error if the API
// server actually has it.
func TestValidator_FallsBackToAPIReaderOnCacheMiss(t *testing.T) {
	s := newScheme(t)

	// Cache is empty (cache miss simulates an informer that hasn't observed
	// the freshly-created provider yet).
	cached := &countingReader{
		inner: fake.NewClientBuilder().WithScheme(s).Build(),
	}
	// API server has the provider.
	api := &countingReader{
		inner: fake.NewClientBuilder().WithScheme(s).WithObjects(newProviderConfig("dynamo")).Build(),
	}

	v := &ModelDeploymentCustomValidator{Reader: cached, APIReader: api}

	warnings, err := v.ValidateCreate(context.Background(), newMD("dynamo"))
	if err != nil {
		t.Fatalf("expected fallback to API server to succeed, got error: %v (warnings=%v)", err, warnings)
	}
	if cached.calls == 0 {
		t.Errorf("expected cached Reader to be consulted first; calls=%d", cached.calls)
	}
	if api.calls == 0 {
		t.Errorf("expected APIReader to be consulted on cache miss; calls=%d", api.calls)
	}
}

// TestValidator_NotFoundIsAuthoritativeAfterFallback ensures that if BOTH the
// cache and the API server report NotFound, the validator rejects the
// admission with the user-facing "InferenceProviderConfig not found" error.
func TestValidator_NotFoundIsAuthoritativeAfterFallback(t *testing.T) {
	s := newScheme(t)

	cached := &countingReader{inner: fake.NewClientBuilder().WithScheme(s).Build()}
	api := &countingReader{inner: fake.NewClientBuilder().WithScheme(s).Build()}

	v := &ModelDeploymentCustomValidator{Reader: cached, APIReader: api}

	_, err := v.ValidateCreate(context.Background(), newMD("nonexistent"))
	if err == nil {
		t.Fatalf("expected admission error for unknown provider, got nil")
	}
	if api.calls == 0 {
		t.Errorf("expected APIReader to be consulted as fallback on cache miss; calls=%d", api.calls)
	}
}
