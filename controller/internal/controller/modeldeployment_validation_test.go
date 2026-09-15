package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
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
// see https://github.com/ai-runway/airunway/issues/325) instead of silently
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
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != providerChangeNotSupportedReason {
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
	if cond != nil && cond.Reason == providerChangeNotSupportedReason {
		t.Fatalf("unexpected ProviderChangeNotSupported for an unchanged provider")
	}
}

func TestReconcileClearsResolvedCoreValidationMessage(t *testing.T) {
	tests := []struct {
		name              string
		conditionStatus   metav1.ConditionStatus
		conditionReason   string
		conditionMessage  string
		deploymentMessage string
	}{
		{
			name:              "failure resolves during this reconciliation",
			conditionStatus:   metav1.ConditionFalse,
			conditionReason:   "ValidationFailed",
			conditionMessage:  "model.id is required when source is huggingface",
			deploymentMessage: "Validation failed: model.id is required when source is huggingface",
		},
		{
			name:              "condition recovered before controller upgrade",
			conditionStatus:   metav1.ConditionTrue,
			conditionReason:   "ValidationPassed",
			conditionMessage:  "Schema validation passed",
			deploymentMessage: "Validation failed: stale error from an earlier reconciliation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			md := newProviderSwitchMD("recovered-validation", "dynamo", "dynamo")
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = tt.deploymentMessage
			md.Status.Conditions = []metav1.Condition{{
				Type:               airunwayv1alpha1.ConditionTypeValidated,
				Status:             tt.conditionStatus,
				Reason:             tt.conditionReason,
				Message:            tt.conditionMessage,
				LastTransitionTime: metav1.Now(),
			}}

			r := newTestReconciler(scheme, nil, md)
			r.EnableProviderSelector = false

			if _, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: md.Name, Namespace: md.Namespace},
			}); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			var got airunwayv1alpha1.ModelDeployment
			if err := r.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &got); err != nil {
				t.Fatalf("failed to get reconciled ModelDeployment: %v", err)
			}
			if got.Status.Message != "" {
				t.Fatalf("expected resolved core validation message to be cleared, got %q", got.Status.Message)
			}
			validated := meta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeValidated)
			if validated == nil || validated.Status != metav1.ConditionTrue {
				t.Fatalf("expected Validated=True after recovery, got %#v", validated)
			}
		})
	}
}

func TestReconcilePreservesProviderStatusMessage(t *testing.T) {
	scheme := newTestScheme()
	md := newProviderSwitchMD("provider-progress", "dynamo", "dynamo")
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
	md.Status.Message = "DynamoGraphDeployment created, waiting for pods to be ready"

	r := newTestReconciler(scheme, nil, md)
	r.EnableProviderSelector = false

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: md.Name, Namespace: md.Namespace},
	}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var got airunwayv1alpha1.ModelDeployment
	if err := r.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, &got); err != nil {
		t.Fatalf("failed to get reconciled ModelDeployment: %v", err)
	}
	if got.Status.Message != md.Status.Message {
		t.Fatalf("expected provider-owned status message to be preserved, got %q", got.Status.Message)
	}
}

func TestReconcileRecoversRejectedProviderChange(t *testing.T) {
	for _, selector := range []bool{false, true} {
		for _, correction := range []string{"revert", "remove", "empty name"} {
			t.Run(fmt.Sprintf("selector=%t/%s", selector, correction), func(t *testing.T) {
				testRejectedProviderRecovery(t, selector, correction)
			})
		}
	}
}

func testRejectedProviderRecovery(t *testing.T, selector bool, correction string) {
	t.Helper()
	ctx := context.Background()
	md := newProviderSwitchMD("provider-recovery", "vllm", "dynamo")
	md.Status.Provider.ResourceName = "existing-workload"
	r := newTestReconciler(newTestScheme(), nil, md)
	r.EnableProviderSelector = selector
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(md)}
	reconcileAndGet := func() *airunwayv1alpha1.ModelDeployment {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		got := &airunwayv1alpha1.ModelDeployment{}
		if err := r.Get(ctx, req.NamespacedName, got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := reconcileAndGet()
	assertProviderChangeRejected(t, got, md.Status.Provider)
	got = reconcileAndGet()
	assertProviderChangeRejected(t, got, md.Status.Provider)
	switch correction {
	case "revert":
		got.Spec.Provider.Name = "dynamo"
	case "remove":
		got.Spec.Provider = nil
	case "empty name":
		got.Spec.Provider.Name = ""
	}
	if err := r.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = reconcileAndGet()
	condition := meta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeProviderSelected)
	if condition == nil || condition.Status != metav1.ConditionTrue ||
		got.Status.Phase != airunwayv1alpha1.DeploymentPhasePending || got.Status.Message != "" {
		t.Fatalf("expected recovery to selected/Pending without stale error, got %#v", got.Status)
	}
	if !reflect.DeepEqual(got.Status.Provider, md.Status.Provider) {
		t.Fatalf("provider identity changed during recovery: %#v", got.Status.Provider)
	}
	recovered := got.Status.DeepCopy()
	got = reconcileAndGet()
	if !reflect.DeepEqual(got.Status, *recovered) {
		t.Fatalf("recovered status is not stable: before=%#v after=%#v", *recovered, got.Status)
	}
	got.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "vllm"}
	if err := r.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	assertProviderChangeRejected(t, reconcileAndGet(), md.Status.Provider)
}

func assertProviderChangeRejected(t *testing.T, got *airunwayv1alpha1.ModelDeployment, wantProvider *airunwayv1alpha1.ProviderStatus) {
	t.Helper()
	condition := meta.FindStatusCondition(got.Status.Conditions, airunwayv1alpha1.ConditionTypeProviderSelected)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != providerChangeNotSupportedReason ||
		got.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed || got.Status.Message != condition.Message {
		t.Fatalf("expected provider switch rejection, got %#v", got.Status)
	}
	if !reflect.DeepEqual(got.Status.Provider, wantProvider) {
		t.Fatalf("provider identity changed: %#v", got.Status.Provider)
	}
}

func TestReconcileSelectsInitialProvider(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		selector bool
	}{
		{name: "explicit without selector", provider: "dynamo"},
		{name: "explicit with selector", provider: "dynamo", selector: true},
		{name: "automatic", selector: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			md := newProviderSwitchMD("initial-selection", tc.provider, "")
			provider := providerWithEngineRule("dynamo", airunwayv1alpha1.EngineTypeVLLM, 1)
			r := newTestReconciler(newTestScheme(), nil, md, &provider)
			r.EnableProviderSelector = tc.selector
			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(md)}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, req.NamespacedName, md); err != nil {
				t.Fatal(err)
			}
			if md.Status.Provider == nil || md.Status.Provider.Name != provider.Name ||
				!meta.IsStatusConditionTrue(md.Status.Conditions, airunwayv1alpha1.ConditionTypeProviderSelected) {
				t.Fatalf("expected initial provider selection, got %#v", md.Status)
			}
		})
	}
}
