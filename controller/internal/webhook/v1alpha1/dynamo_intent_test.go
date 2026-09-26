package v1alpha1

import (
	"context"
	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	"k8s.io/apimachinery/pkg/runtime"
	"testing"
)

func TestDynamoIntentDoesNotReceiveManualDefaults(t *testing.T) {
	md := &api.ModelDeployment{Spec: api.ModelDeploymentSpec{Model: api.ModelSpec{ID: "Qwen/Qwen3-0.6B"}, Engine: api.EngineSpec{Type: api.EngineTypeVLLM}, Provider: &api.ProviderSpec{Name: "dynamo", Overrides: &runtime.RawExtension{Raw: []byte(`{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":2}}}`)}}}}
	if err := (&ModelDeploymentCustomDefaulter{}).Default(context.Background(), md); err != nil {
		t.Fatal(err)
	}
	if md.Spec.Scaling != nil || md.Spec.Resources != nil {
		t.Fatal("automatic configuration acquired manual sizing defaults")
	}
	if err := dynamointent.Validate(md); err != nil {
		t.Fatal(err)
	}
	if _, err := (&ModelDeploymentCustomValidator{}).ValidateCreate(context.Background(), md); err != nil {
		t.Fatal(err)
	}
	old := md.DeepCopy()
	old.Status.Provider = &api.ProviderStatus{RequestRef: &api.ProviderResourceReference{Name: "request"}, Intent: &api.ProviderIntentStatus{Phase: "Profiling"}}
	next := old.DeepCopy()
	next.Spec.Model.ID = "Qwen/Qwen3-8B"
	if _, err := (&ModelDeploymentCustomValidator{}).ValidateUpdate(context.Background(), old, next); err == nil {
		t.Fatal("ordinary edit to locked model accepted")
	}
	next.Annotations = map[string]string{dynamointent.AttemptAnnotation: "next"}
	if _, err := (&ModelDeploymentCustomValidator{}).ValidateUpdate(context.Background(), old, next); err != nil {
		t.Fatalf("explicit reconfigure rejected: %v", err)
	}
}
