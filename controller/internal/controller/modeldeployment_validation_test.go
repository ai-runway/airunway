package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

func TestValidateSpecRejectsConflictingImageFields(t *testing.T) {
	r := &ModelDeploymentReconciler{}
	md := &airunwayv1alpha1.ModelDeployment{
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Image: "legacy:v1",
			Engine: airunwayv1alpha1.EngineSpec{
				Image: "engine:v2",
			},
		},
	}

	err := r.validateSpec(context.Background(), md, nil, md.ResolvedEngineType(), md.ResolvedServingMode())
	if err == nil {
		t.Fatalf("expected conflicting image fields to be rejected")
	}
	if !strings.Contains(err.Error(), "spec.image") || !strings.Contains(err.Error(), "spec.engine.image") {
		t.Fatalf("expected image conflict error, got %v", err)
	}

	cond := meta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeImageResolved)
	if cond == nil {
		t.Fatalf("expected ImageResolved condition")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected ImageResolved=False, got %s", cond.Status)
	}
	if cond.Reason != "ConflictingImageFields" {
		t.Fatalf("expected ConflictingImageFields reason, got %s", cond.Reason)
	}
	if md.Status.Image == nil {
		t.Fatalf("expected image status")
	}
	if md.Status.Image.Requested != "engine:v2" {
		t.Fatalf("expected requested image to prefer spec.engine.image, got %q", md.Status.Image.Requested)
	}
	if !strings.Contains(md.Status.Image.Message, "spec.image") || !strings.Contains(md.Status.Image.Message, "spec.engine.image") {
		t.Fatalf("expected image status message to mention both fields, got %q", md.Status.Image.Message)
	}
}

func TestReconcileRejectsConflictingImageFieldsBeforeSelection(t *testing.T) {
	scheme := newTestScheme()
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflicting-images",
			Namespace: "default",
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID:     "meta-llama/Llama-3-8B",
				Source: airunwayv1alpha1.ModelSourceHuggingFace,
			},
			Image: "legacy:v1",
			Engine: airunwayv1alpha1.EngineSpec{
				Image: "engine:v2",
			},
		},
	}
	r := newTestReconciler(scheme, nil, md)
	r.EnableProviderSelector = true

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: md.Name, Namespace: md.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var got airunwayv1alpha1.ModelDeployment
	if err := r.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &got); err != nil {
		t.Fatalf("failed to get reconciled ModelDeployment: %v", err)
	}
	if got.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Fatalf("expected failed phase, got %q", got.Status.Phase)
	}
	if got.Status.Engine != nil {
		t.Fatalf("expected engine selection to be skipped, got %#v", got.Status.Engine)
	}

	cond := meta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeImageResolved)
	if cond == nil {
		t.Fatalf("expected ImageResolved condition")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "ConflictingImageFields" {
		t.Fatalf("unexpected ImageResolved condition: %#v", cond)
	}
	validated := meta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeValidated)
	if validated == nil || validated.Status != metav1.ConditionFalse {
		t.Fatalf("expected Validated=False, got %#v", validated)
	}
}

// newProviderSwitchMD builds a ModelDeployment whose spec is valid enough to
// reach provider selection, with a pre-stamped status.provider.name.
func newProviderSwitchMD(name, specProvider, statusProvider string) *airunwayv1alpha1.ModelDeployment {
	gpu := int32(1)
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID:     "Qwen/Qwen2.5-0.5B-Instruct",
				Source: airunwayv1alpha1.ModelSourceHuggingFace,
			},
			Engine:    airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Resources: &airunwayv1alpha1.ResourceSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: gpu}},
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

// Changing spec.provider.name after a provider has already been recorded in
// status must be rejected (interim guard for the unsupported provider switch;
// see https://github.com/kaito-project/airunway/issues/325) instead of silently
// keeping the old provider.
func TestReconcileRejectsProviderChangeAfterSelection(t *testing.T) {
	scheme := newTestScheme()
	md := newProviderSwitchMD("provider-switch", "vllm", "dynamo")
	r := newTestReconciler(scheme, nil, md)
	r.EnableProviderSelector = true

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: md.Name, Namespace: md.Namespace},
	}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var got airunwayv1alpha1.ModelDeployment
	if err := r.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &got); err != nil {
		t.Fatalf("failed to get reconciled ModelDeployment: %v", err)
	}

	if got.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Fatalf("expected Failed phase on provider change, got %q", got.Status.Phase)
	}
	// The previously-selected provider must be left untouched (no silent re-point).
	if got.Status.Provider == nil || got.Status.Provider.Name != "dynamo" {
		t.Fatalf("expected status.provider.name to stay dynamo, got %#v", got.Status.Provider)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeProviderSelected)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ProviderChangeNotSupported" {
		t.Fatalf("expected ProviderSelected=False/ProviderChangeNotSupported, got %#v", cond)
	}
	if !strings.Contains(got.Status.Message, "dynamo") || !strings.Contains(got.Status.Message, "vllm") {
		t.Fatalf("expected message to name both providers, got %q", got.Status.Message)
	}
}

// Re-specifying the SAME provider already in status is a no-op, not a rejection.
func TestReconcileAllowsSameExplicitProvider(t *testing.T) {
	scheme := newTestScheme()
	md := newProviderSwitchMD("same-provider", "dynamo", "dynamo")
	r := newTestReconciler(scheme, nil, md)
	r.EnableProviderSelector = true

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: md.Name, Namespace: md.Namespace},
	}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var got airunwayv1alpha1.ModelDeployment
	if err := r.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &got); err != nil {
		t.Fatalf("failed to get reconciled ModelDeployment: %v", err)
	}
	if got.Status.Phase == airunwayv1alpha1.DeploymentPhaseFailed {
		t.Fatalf("did not expect Failed phase for an unchanged provider, message=%q", got.Status.Message)
	}
	if got.Status.Provider == nil || got.Status.Provider.Name != "dynamo" {
		t.Fatalf("expected status.provider.name to stay dynamo, got %#v", got.Status.Provider)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeProviderSelected)
	if cond != nil && cond.Reason == "ProviderChangeNotSupported" {
		t.Fatalf("unexpected ProviderChangeNotSupported for an unchanged provider")
	}
}
