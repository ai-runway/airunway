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

package agentprovider

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

func TestConditionObservedGeneration(t *testing.T) {
	cases := []struct {
		name    string
		value   interface{}
		want    int64
		present bool
	}{
		{"int64", int64(3), 3, true},
		{"float64", float64(4), 4, true},
		{"int", 5, 5, true},
		{"absent", nil, 0, false},
		{"string ignored", "6", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cm := map[string]interface{}{}
			if tc.value != nil {
				cm["observedGeneration"] = tc.value
			}
			got, present := conditionObservedGeneration(cm)
			if present != tc.present || got != tc.want {
				t.Fatalf("conditionObservedGeneration(%v) = (%d,%v), want (%d,%v)", tc.value, got, present, tc.want, tc.present)
			}
		})
	}
}

// TestClassifyBinding pins the distinction the whole teardown path depends on:
// a missing binding means "stop the agent", an unverified one means "hold".
// Collapsing them is how a transient failure deletes every running workload.
func TestClassifyBinding(t *testing.T) {
	bound := []metav1.Condition{{
		Type:   airunwayv1alpha1.AgentConditionTypeModelBound,
		Status: metav1.ConditionTrue,
		Reason: "ModelBound",
	}}
	unbound := []metav1.Condition{{
		Type:   airunwayv1alpha1.AgentConditionTypeModelBound,
		Status: metav1.ConditionFalse,
		Reason: "FrameworkNotReady",
	}}
	binding := &airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1", ModelName: "m"}

	cases := []struct {
		name string
		ad   airunwayv1alpha1.AgentDeployment
		want BindingState
	}{
		{
			name: "no binding at all is unavailable",
			ad:   airunwayv1alpha1.AgentDeployment{},
			want: BindingUnavailable,
		},
		{
			name: "binding cleared by core is unavailable, even while ModelBound lingers true",
			ad: airunwayv1alpha1.AgentDeployment{
				Status: airunwayv1alpha1.AgentDeploymentStatus{Conditions: bound},
			},
			want: BindingUnavailable,
		},
		{
			name: "binding present but unverified is stale, NOT unavailable",
			ad: airunwayv1alpha1.AgentDeployment{
				Status: airunwayv1alpha1.AgentDeploymentStatus{
					ModelBinding: binding,
					Conditions:   unbound,
				},
			},
			want: BindingStale,
		},
		{
			name: "binding present and verified is ready",
			ad: airunwayv1alpha1.AgentDeployment{
				Status: airunwayv1alpha1.AgentDeploymentStatus{
					ModelBinding: binding,
					Conditions:   bound,
				},
			},
			want: BindingReady,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyBinding(&tc.ad); got != tc.want {
				t.Fatalf("ClassifyBinding = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFieldOwnerIsPerFramework(t *testing.T) {
	if FieldOwner("kagent") == FieldOwner("orka") {
		t.Fatal("distinct frameworks must get distinct field owners, or their status writes collide")
	}
	if !strings.HasPrefix(FieldOwner("kagent"), "airunway-agents-") {
		t.Errorf("unexpected field owner shape: %q", FieldOwner("kagent"))
	}
}

func TestBoundedNamesRespectKubernetesLimits(t *testing.T) {
	long := strings.Repeat("a", 300)

	if got := BoundedLabelValue(long); len(got) > MaxLabelValueLength {
		t.Errorf("BoundedLabelValue = %d bytes, want <= %d", len(got), MaxLabelValueLength)
	}
	if got := BoundedDNSLabelName(long); len(got) > MaxDNSLabelNameLength {
		t.Errorf("BoundedDNSLabelName = %d bytes, want <= %d", len(got), MaxDNSLabelNameLength)
	}
	if got := BoundedResourceName(long, "-config"); len(got) > MaxResourceNameLength {
		t.Errorf("BoundedResourceName = %d bytes, want <= %d", len(got), MaxResourceNameLength)
	}

	// Short inputs must pass through byte-identical: the Deployment selector is
	// immutable, so any change here would orphan existing workloads.
	for _, s := range []string{"agent", "my-agent-1"} {
		if got := BoundedLabelValue(s); got != s {
			t.Errorf("BoundedLabelValue(%q) = %q, want unchanged", s, got)
		}
		if got := BoundedDNSLabelName(s); got != s {
			t.Errorf("BoundedDNSLabelName(%q) = %q, want unchanged", s, got)
		}
	}

	// Distinct long names sharing a prefix must not collapse together.
	a := strings.Repeat("a", 80)
	b := strings.Repeat("a", 79) + "b"
	if BoundedLabelValue(a) == BoundedLabelValue(b) {
		t.Error("distinct long names collided onto one label value")
	}
}

func TestHashJSONIsStable(t *testing.T) {
	v := map[string]string{"b": "2", "a": "1"}
	first, err := HashJSON(v)
	if err != nil {
		t.Fatalf("HashJSON: %v", err)
	}
	second, err := HashJSON(map[string]string{"a": "1", "b": "2"})
	if err != nil {
		t.Fatalf("HashJSON: %v", err)
	}
	if first != second {
		t.Fatalf("HashJSON is not order-stable: %q vs %q — checksums would churn every reconcile", first, second)
	}
	if changed, _ := HashJSON(map[string]string{"a": "1", "b": "3"}); changed == first {
		t.Fatal("HashJSON did not change when content changed — a config edit would not roll the workload")
	}
}

// TestDeleteOwnedIgnoresMissingKind covers the case where a framework's
// operator is not installed: the CRD is not served, so nothing of that kind can
// exist and there is nothing to clean up. Treating the discovery error as a
// real failure made the provider retry forever and leak "no matches for kind"
// onto ProviderReady. Found on a real cluster — 63 occurrences in six hours.
func TestDeleteOwnedIgnoresMissingKind(t *testing.T) {
	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	obj := UnstructuredRef(
		schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "Provider"},
		"ghost-agent-provider", "default")
	owner := &airunwayv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ghost-agent", Namespace: "default"},
	}

	if err := DeleteOwned(context.Background(), c, owner, obj); err != nil {
		t.Fatalf("DeleteOwned on an unserved kind must be a no-op, got: %v", err)
	}
}

// TestClassifyBindingGating pins that a ModelBound=True carried over from an
// earlier generation does not license rendering the CURRENT spec. Without this,
// a user editing spec.model gets one reconcile where the provider renders the
// new generation against the previous endpoint or credential.
func TestClassifyBindingGating(t *testing.T) {
	ad := func(generation, observed int64) *airunwayv1alpha1.AgentDeployment {
		return &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Generation: generation},
			Status: airunwayv1alpha1.AgentDeploymentStatus{
				ModelBinding: &airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://x/v1"},
				Conditions: []metav1.Condition{{
					Type:               airunwayv1alpha1.AgentConditionTypeModelBound,
					Status:             metav1.ConditionTrue,
					Reason:             "ModelBound",
					ObservedGeneration: observed,
				}},
			},
		}
	}

	cases := []struct {
		name       string
		generation int64
		observed   int64
		want       BindingState
	}{
		{"verified for this generation renders", 3, 3, BindingReady},
		{"verified for a later generation renders", 3, 4, BindingReady},
		{"stale from a previous generation holds", 3, 2, BindingStale},
		{"untracked generation is accepted for compatibility", 3, 0, BindingReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyBinding(ad(tc.generation, tc.observed)); got != tc.want {
				t.Fatalf("ClassifyBinding(gen=%d, observed=%d) = %v, want %v",
					tc.generation, tc.observed, got, tc.want)
			}
		})
	}
}
