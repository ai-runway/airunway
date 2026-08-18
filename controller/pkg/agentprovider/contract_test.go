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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func TestProviderHandoffPending(t *testing.T) {
	ad := &airunwayv1alpha1.AgentDeployment{Status: airunwayv1alpha1.AgentDeploymentStatus{ProviderOwner: FieldOwner("kagent")}}
	if ProviderHandoffPending(ad, FieldOwner("kagent")) {
		t.Fatal("the current provider must not wait on itself")
	}
	if !ProviderHandoffPending(ad, FieldOwner("orka")) {
		t.Fatal("a successor must wait until the previous provider releases its workload")
	}
	ad.Status.ProviderOwner = ""
	if ProviderHandoffPending(ad, FieldOwner("orka")) {
		t.Fatal("an unowned provider status must not block initial rendering")
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
	if got := BoundedResourceName("agent", strings.Repeat("s", 300)); len(got) > MaxResourceNameLength {
		t.Errorf("BoundedResourceName with an oversized suffix = %d bytes, want <= %d", len(got), MaxResourceNameLength)
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

func TestUpstreamObjectReady(t *testing.T) {
	ready := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{map[string]interface{}{
				"type": "Ready", "status": "True", "observedGeneration": int64(2),
			}},
		},
	}}
	ready.SetKind("Agent")
	ready.SetGeneration(2)
	got, err := UpstreamObjectReady(ready)
	if err != nil || !got {
		t.Fatalf("UpstreamObjectReady = (%v, %v), want (true, nil)", got, err)
	}

	stale := ready.DeepCopy()
	stale.SetGeneration(3)
	got, err = UpstreamObjectReady(stale)
	if err != nil || got {
		t.Fatalf("stale UpstreamObjectReady = (%v, %v), want (false, nil)", got, err)
	}

	malformed := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"conditions": "not-a-list"},
	}}
	if _, err := UpstreamObjectReady(malformed); err == nil {
		t.Fatal("malformed upstream conditions must be reported as a read error, not ordinary not-ready")
	}
}

// missOnceReader models the exact stale-cache window that used to permit
// adoption: the ownership read says NotFound even though another writer has
// already created the object. ApplyOwned must recover from AlreadyExists and
// authoritatively reject that object.
type missOnceReader struct {
	client.Reader
	miss bool
}

func (r *missOnceReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if !r.miss {
		r.miss = true
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestApplyOwnedRejectsConcurrentForeignCreate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-model-noauth", Namespace: "default"},
		Data:       map[string][]byte{"foreign": []byte("preserve")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
	reader := &missOnceReader{Reader: c}
	owner := &airunwayv1alpha1.AgentDeployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: airunwayv1alpha1.GroupVersion.String(), Kind: "AgentDeployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "owner-uid"},
	}
	desired := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: foreign.Name, Namespace: foreign.Namespace},
		Data:       map[string][]byte{KeylessCredentialKey: []byte(KeylessCredentialValue)},
	}
	if err := ApplyOwned(context.Background(), c, reader, scheme, owner, desired, FieldOwner("test"), false); err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("ApplyOwned concurrent-create error = %v, want refusing-to-adopt", err)
	}

	preserved := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), preserved); err != nil {
		t.Fatal(err)
	}
	if string(preserved.Data["foreign"]) != "preserve" || len(preserved.OwnerReferences) != 0 {
		t.Fatalf("foreign Secret was modified or adopted: %#v", preserved)
	}
}

func TestAgentDeploymentOwnershipGuardsRejectUIDOnlyReference(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	owner := &airunwayv1alpha1.AgentDeployment{
		TypeMeta: metav1.TypeMeta{APIVersion: airunwayv1alpha1.GroupVersion.String(), Kind: "AgentDeployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent", Namespace: "default", UID: "owner-uid",
		},
	}
	controller, blockOwnerDeletion := true, true
	forgedRef := metav1.OwnerReference{
		APIVersion:         airunwayv1alpha1.GroupVersion.String(),
		Kind:               "AgentDeployment",
		Name:               "different-agent",
		UID:                owner.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
	foreign := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent-model-noauth", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{forgedRef},
		},
		Data: map[string][]byte{"foreign": []byte("preserve")},
	}
	if IsControlledByAgentDeployment(foreign, owner) {
		t.Fatal("a same-UID controller reference with the wrong owner name must not authorize writes or deletion")
	}
	exact := foreign.DeepCopy()
	exact.OwnerReferences[0].Name = owner.Name
	if !IsControlledByAgentDeployment(exact, owner) {
		t.Fatal("the exact blocking AgentDeployment controller reference must be accepted")
	}
	exact.OwnerReferences[0].BlockOwnerDeletion = nil
	if IsControlledByAgentDeployment(exact, owner) {
		t.Fatal("an otherwise exact non-blocking controller reference must not authorize writes or deletion")
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
	desired := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: foreign.Name, Namespace: foreign.Namespace},
		Data:       map[string][]byte{KeylessCredentialKey: []byte(KeylessCredentialValue)},
	}
	if err := VerifyOwnedOrAbsent(context.Background(), c, scheme, owner, desired); err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("VerifyOwnedOrAbsent error = %v, want refusing-to-adopt", err)
	}
	if err := ApplyOwned(context.Background(), c, c, scheme, owner, desired, FieldOwner("test"), false); err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("ApplyOwned error = %v, want refusing-to-adopt", err)
	}
	if err := DeleteOwned(context.Background(), c, owner, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: foreign.Name, Namespace: foreign.Namespace,
	}}); err != nil {
		t.Fatalf("DeleteOwned forged-reference no-op: %v", err)
	}
	pending, err := DeleteOwnedAndWait(context.Background(), c, c, owner, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: foreign.Name, Namespace: foreign.Namespace,
	}})
	if err != nil || pending {
		t.Fatalf("DeleteOwnedAndWait forged-reference result = (%v, %v), want (false, nil)", pending, err)
	}

	preserved := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), preserved); err != nil {
		t.Fatal(err)
	}
	if string(preserved.Data["foreign"]) != "preserve" || len(preserved.OwnerReferences) != 1 || preserved.OwnerReferences[0].Name != forgedRef.Name {
		t.Fatalf("foreign Secret was modified, adopted, or deleted: %#v", preserved)
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
		{"untracked generation cannot license a current binding", 3, 0, BindingStale},
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
