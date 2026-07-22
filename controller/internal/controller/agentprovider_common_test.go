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

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

func ownershipScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(airunwayv1alpha1.AddToScheme(s))
	return s
}

func ownerAgentDeployment(name string) *airunwayv1alpha1.AgentDeployment {
	return &airunwayv1alpha1.AgentDeployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: airunwayv1alpha1.GroupVersion.String(), Kind: "AgentDeployment"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(name + "-uid")},
	}
}

func TestVerifyOwnedOrAbsent(t *testing.T) {
	s := ownershipScheme(t)
	ad := ownerAgentDeployment("agent")

	// Owned by ad → allowed.
	owned := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "ns"}}
	if err := controllerutil.SetControllerReference(ad, owned, s); err != nil {
		t.Fatalf("set owner ref: %v", err)
	}
	// Unrelated object with the same name → must be rejected.
	unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "ns"}}

	t.Run("absent is allowed", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).Build()
		desired := &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "ns"},
		}
		if err := verifyOwnedOrAbsent(context.Background(), c, s, ad, desired); err != nil {
			t.Fatalf("expected absent object to be allowed, got %v", err)
		}
	})

	t.Run("owned is allowed", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(owned).Build()
		desired := &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "ns"},
		}
		if err := verifyOwnedOrAbsent(context.Background(), c, s, ad, desired); err != nil {
			t.Fatalf("expected owned object to be allowed, got %v", err)
		}
	})

	t.Run("unrelated is rejected", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(unrelated).Build()
		desired := &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "ns"},
		}
		if err := verifyOwnedOrAbsent(context.Background(), c, s, ad, desired); err == nil {
			t.Fatal("expected unowned object to be rejected")
		}
	})
}

func TestDeleteOwnedObject(t *testing.T) {
	s := ownershipScheme(t)
	ad := ownerAgentDeployment("agent")

	t.Run("owned is deleted", func(t *testing.T) {
		owned := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
		if err := controllerutil.SetControllerReference(ad, owned, s); err != nil {
			t.Fatalf("set owner ref: %v", err)
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(owned).Build()

		target := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
		if err := deleteOwnedObject(context.Background(), c, ad, target); err != nil {
			t.Fatalf("delete owned: %v", err)
		}
		got := &corev1.Service{}
		err := c.Get(context.Background(), types.NamespacedName{Name: "agent", Namespace: "ns"}, got)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected owned service to be deleted, got err=%v", err)
		}
	})

	t.Run("unowned is left intact", func(t *testing.T) {
		unrelated := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(unrelated).Build()

		target := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
		if err := deleteOwnedObject(context.Background(), c, ad, target); err != nil {
			t.Fatalf("delete unowned should be a no-op, got %v", err)
		}
		got := &corev1.Service{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "agent", Namespace: "ns"}, got); err != nil {
			t.Fatalf("expected unowned service to be left intact, got %v", err)
		}
	})

	t.Run("absent is a no-op", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).Build()
		target := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
		if err := deleteOwnedObject(context.Background(), c, ad, target); err != nil {
			t.Fatalf("delete absent should be a no-op, got %v", err)
		}
	})
}

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
