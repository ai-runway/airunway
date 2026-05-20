package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

func newModelDeploymentForProviderWatch(name, namespace, specProvider, statusProvider string) *airunwayv1alpha1.ModelDeployment {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID:     "test-model",
				Source: airunwayv1alpha1.ModelSourceHuggingFace,
			},
		},
	}

	if specProvider != "" {
		md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: specProvider}
	}
	if statusProvider != "" {
		md.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: statusProvider}
	}

	return md
}

func assertRequestsMatch(t *testing.T, requests []reconcile.Request, expected ...string) {
	t.Helper()

	got := make(map[string]struct{}, len(requests))
	for _, request := range requests {
		got[request.Namespace+"/"+request.Name] = struct{}{}
	}

	if len(got) != len(expected) {
		t.Fatalf("expected %d requests, got %d: %#v", len(expected), len(got), got)
	}

	for _, item := range expected {
		if _, ok := got[item]; !ok {
			t.Fatalf("expected request for %s, got %#v", item, got)
		}
	}
}

func TestMapProviderConfigToModelDeployments(t *testing.T) {
	scheme := newTestScheme()
	reconciler := newTestReconciler(
		scheme,
		nil,
		newModelDeploymentForProviderWatch("needs-selection", "default", "", ""),
		newModelDeploymentForProviderWatch("selected-provider", "default", "", "dynamo"),
		newModelDeploymentForProviderWatch("pinned-provider", "default", "dynamo", ""),
		newModelDeploymentForProviderWatch("other-provider", "default", "", "kuberay"),
	)

	requests := reconciler.mapProviderConfigToModelDeployments(context.Background(), &airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "dynamo"},
	})

	assertRequestsMatch(
		t,
		requests,
		"default/needs-selection",
		"default/selected-provider",
		"default/pinned-provider",
	)
}
