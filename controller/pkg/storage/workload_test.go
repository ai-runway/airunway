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

package storage

import (
	"context"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureConsumerWorkloadsAbsent(t *testing.T) {
	ctx := context.Background()
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "models", UID: types.UID("md-uid")},
	}
	gvk := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

	t.Run("deletes owned workload", func(t *testing.T) {
		workload := &unstructured.Unstructured{}
		workload.SetGroupVersionKind(gvk)
		workload.SetName("demo")
		workload.SetNamespace("models")
		workload.SetOwnerReferences([]metav1.OwnerReference{{UID: md.UID}})
		c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(workload).Build()

		absent, err := EnsureConsumerWorkloadsAbsent(ctx, c, md, ConsumerWorkload{
			GroupVersionKind: gvk,
			Name:             workload.GetName(),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if absent {
			t.Fatal("expected first pass to report deletion in progress")
		}
		remaining := &unstructured.Unstructured{}
		remaining.SetGroupVersionKind(gvk)
		err = c.Get(ctx, types.NamespacedName{Name: workload.GetName(), Namespace: workload.GetNamespace()}, remaining)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected owned workload deletion, got %v", err)
		}
	})

	t.Run("preserves foreign workload", func(t *testing.T) {
		workload := &unstructured.Unstructured{}
		workload.SetGroupVersionKind(gvk)
		workload.SetName("demo")
		workload.SetNamespace("models")
		workload.SetOwnerReferences([]metav1.OwnerReference{{UID: types.UID("someone-else")}})
		c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(workload).Build()

		absent, err := EnsureConsumerWorkloadsAbsent(ctx, c, md, ConsumerWorkload{
			GroupVersionKind: gvk,
			Name:             workload.GetName(),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !absent {
			t.Fatal("foreign workload must not count as this deployment's active consumer")
		}
		remaining := &unstructured.Unstructured{}
		remaining.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, types.NamespacedName{Name: workload.GetName(), Namespace: workload.GetNamespace()}, remaining); err != nil {
			t.Fatalf("expected foreign workload to be preserved: %v", err)
		}
	})

	t.Run("deletes workload from prior deployment incarnation", func(t *testing.T) {
		workload := &unstructured.Unstructured{}
		workload.SetGroupVersionKind(gvk)
		workload.SetName("demo")
		workload.SetNamespace("models")
		workload.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: airunwayv1alpha1.GroupVersion.String(),
			Kind:       "ModelDeployment",
			Name:       md.Name,
			UID:        types.UID("prior-md-uid"),
		}})
		c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(workload).Build()

		absent, err := EnsureConsumerWorkloadsAbsent(ctx, c, md, ConsumerWorkload{
			GroupVersionKind: gvk,
			Name:             workload.GetName(),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if absent {
			t.Fatal("expected prior-owned workload deletion to be in progress")
		}
		remaining := &unstructured.Unstructured{}
		remaining.SetGroupVersionKind(gvk)
		err = c.Get(ctx, types.NamespacedName{Name: workload.GetName(), Namespace: workload.GetNamespace()}, remaining)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected prior-owned workload deletion, got %v", err)
		}
	})
}
