package dynamointent

import (
	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"testing"
)

func fixture(raw string) *api.ModelDeployment {
	return &api.ModelDeployment{Spec: api.ModelDeploymentSpec{Model: api.ModelSpec{ID: "Qwen/Qwen3-0.6B"}, Engine: api.EngineSpec{Type: api.EngineTypeVLLM}, Provider: &api.ProviderSpec{Name: "dynamo", Overrides: &runtime.RawExtension{Raw: []byte(raw)}}}}
}

const valid = `{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":2},"workload":{"isl":1024,"osl":256,"requestRate":1},"sla":{"ttft":1000,"itl":50}}}`

func TestTypedIntentValidation(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantError bool
	}{
		{"valid", valid, false},
		{"unknown", `{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":2},"typo":true}}`, true},
		{"mixed", `{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":2}},"spec":{}}`, true},
		{"null", `{"deploymentMode":"intent","intent":null}`, true},
		{"no budget", `{"deploymentMode":"intent","intent":{}}`, true},
		{"budget ceiling", `{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":65}}}`, true},
		{"both traffic", `{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":2},"workload":{"requestRate":1,"concurrency":2}}}`, true},
		{"negative latency", `{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":2},"sla":{"ttft":-1}}}`, true},
		{"thorough", `{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":2},"searchStrategy":"thorough"}}`, true},
		{"legacy", `{"deploymentMode":"intent","spec":{"searchStrategy":"rapid"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(fixture(tc.raw))
			if (err != nil) != tc.wantError {
				t.Fatalf("Validate()=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}
func TestFingerprintAndExplicitAttempts(t *testing.T) {
	old := fixture(valid)
	old.Status.Provider = &api.ProviderStatus{RequestRef: &api.ProviderResourceReference{Name: "request"}, Intent: &api.ProviderIntentStatus{Phase: "Profiling"}}
	next := old.DeepCopy()
	enabled := true
	next.Spec.Gateway = &api.GatewaySpec{Enabled: &enabled}
	before, _ := Fingerprint(old)
	after, _ := Fingerprint(next)
	if before != after {
		t.Fatal("gateway-only edit changed profiling input hash")
	}
	if err := ValidateUpdate(old, next); err != nil {
		t.Fatal(err)
	}
	next.Spec.Model.ID = "Qwen/Qwen3-8B"
	if err := ValidateUpdate(old, next); err == nil {
		t.Fatal("locked request accepted a changed model")
	}
	next.Annotations = map[string]string{AttemptAnnotation: "attempt-2"}
	if err := ValidateUpdate(old, next); err != nil {
		t.Fatalf("explicit attempt rejected: %v", err)
	}
	next = old.DeepCopy()
	next.Annotations = map[string]string{AttemptAnnotation: "retry-2"}
	after, _ = Fingerprint(next)
	if before != after {
		t.Fatal("retry token must not affect input fingerprint")
	}
}
func TestManualSizingRejected(t *testing.T) {
	md := fixture(valid)
	md.Spec.Resources = &api.ResourceSpec{GPU: &api.GPUSpec{Count: 2}}
	if err := Validate(md); err == nil {
		t.Fatal("manual resource sizing accepted")
	}
	md = fixture(valid)
	md.Spec.Scaling = &api.ScalingSpec{Replicas: 1}
	if err := Validate(md); err == nil {
		t.Fatal("manual replica sizing accepted")
	}
}

func TestLegacyFingerprintIgnoresObjectOrder(t *testing.T) {
	a := fixture(`{"deploymentMode":"intent","spec":{"workload":{"isl":1024,"osl":256},"searchStrategy":"rapid"}}`)
	b := fixture(`{"spec":{"searchStrategy":"rapid","workload":{"osl":256,"isl":1024}},"deploymentMode":"intent"}`)
	ha, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatal("equivalent legacy JSON changed the fingerprint")
	}
}

func TestTypedIntentRejectsUnsupportedCustomization(t *testing.T) {
	md := fixture(valid)
	md.Spec.Secrets = &api.SecretsSpec{HuggingFaceToken: "custom-token"}
	if err := Validate(md); err == nil {
		t.Fatal("custom secret name silently accepted")
	}
	md.Spec.Secrets.HuggingFaceToken = "hf-token-secret"
	if err := Validate(md); err != nil {
		t.Fatal(err)
	}
	md.Spec.Engine.EnablePrefixCaching = true
	if err := Validate(md); err != nil {
		t.Fatalf("CRD default rejected: %v", err)
	}
}
