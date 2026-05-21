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
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// newUnstructuredScheme returns a fresh runtime.Scheme. Using a dedicated scheme
// avoids "double registration" panics when the Ginkgo suite_test.go registers
// the typed InferenceProviderConfig into the global scheme in the same test binary.
func newUnstructuredScheme() *runtime.Scheme {
	return runtime.NewScheme()
}

func TestMigrateLegacyProviderConfigs_FlatToNested(t *testing.T) {
	// Create a legacy InferenceProviderConfig with flat engine strings
	legacy := &unstructured.Unstructured{}
	legacy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	legacy.SetName("kaito")
	if err := unstructured.SetNestedField(legacy.Object, map[string]interface{}{
		"engines":      []interface{}{"vllm", "llamacpp"},
		"servingModes": []interface{}{"aggregated"},
		"gpuSupport":   true,
		"cpuSupport":   true,
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set capabilities: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).WithObjects(legacy).Build()

	err := MigrateLegacyProviderConfigs(context.Background(), c)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Read back and verify
	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(legacy.GroupVersionKind())
	if err := c.Get(context.Background(), client_key("kaito"), result); err != nil {
		t.Fatalf("failed to get migrated object: %v", err)
	}

	engines, found, err := unstructured.NestedSlice(result.Object, "spec", "capabilities", "engines")
	if err != nil || !found {
		t.Fatalf("migrated object missing engines: err=%v found=%v", err, found)
	}
	if len(engines) != 2 {
		t.Fatalf("expected 2 engines, got %d", len(engines))
	}

	// Check first engine is an object with name field
	eng0, ok := engines[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected engine[0] to be a map, got %T", engines[0])
	}
	if eng0["name"] != "vllm" {
		t.Errorf("expected engine[0].name=vllm, got %v", eng0["name"])
	}
	if eng0["gpuSupport"] != true {
		t.Errorf("expected engine[0].gpuSupport=true, got %v", eng0["gpuSupport"])
	}

	eng1, ok := engines[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected engine[1] to be a map, got %T", engines[1])
	}
	if eng1["name"] != "llamacpp" {
		t.Errorf("expected engine[1].name=llamacpp, got %v", eng1["name"])
	}
	if eng1["cpuSupport"] != true {
		t.Errorf("expected engine[1].cpuSupport=true, got %v", eng1["cpuSupport"])
	}

	// Verify old top-level fields are gone
	caps, _, _ := unstructured.NestedMap(result.Object, "spec", "capabilities")
	if _, exists := caps["servingModes"]; exists {
		t.Error("expected top-level servingModes to be removed after migration")
	}
	if _, exists := caps["gpuSupport"]; exists {
		t.Error("expected top-level gpuSupport to be removed after migration")
	}
	if _, exists := caps["cpuSupport"]; exists {
		t.Error("expected top-level cpuSupport to be removed after migration")
	}
}

// TestMigrateLegacyProviderConfigs_HoistsRequiresCRDAndGateway verifies the
// fix for the review feedback on PR #214: the migration must hoist legacy
// top-level requiresCRD and gateway into every per-engine EngineCapability
// (since those fields have since moved into EngineCapability), and must not
// silently drop them.
func TestMigrateLegacyProviderConfigs_HoistsRequiresCRDAndGateway(t *testing.T) {
	legacy := &unstructured.Unstructured{}
	legacy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	legacy.SetName("dynamo")
	if err := unstructured.SetNestedField(legacy.Object, map[string]interface{}{
		"engines":      []interface{}{"vllm", "llamacpp"},
		"servingModes": []interface{}{"aggregated"},
		"gpuSupport":   true,
		"requiresCRD":  true,
		"gateway": map[string]interface{}{
			"inferencePoolNamePattern": "{name}-pool",
			"inferencePoolNamespace":   "{namespace}",
		},
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set capabilities: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).WithObjects(legacy).Build()

	if err := MigrateLegacyProviderConfigs(context.Background(), c); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(legacy.GroupVersionKind())
	if err := c.Get(context.Background(), client_key("dynamo"), result); err != nil {
		t.Fatalf("failed to get migrated object: %v", err)
	}

	engines, found, err := unstructured.NestedSlice(result.Object, "spec", "capabilities", "engines")
	if err != nil || !found {
		t.Fatalf("migrated object missing engines: err=%v found=%v", err, found)
	}
	if len(engines) != 2 {
		t.Fatalf("expected 2 engines, got %d", len(engines))
	}

	// Every engine must carry requiresCRD=true and the full gateway block.
	for i, e := range engines {
		eng, ok := e.(map[string]interface{})
		if !ok {
			t.Fatalf("expected engine[%d] to be a map, got %T", i, e)
		}
		if eng["requiresCRD"] != true {
			t.Errorf("engine[%d] (%v): expected requiresCRD=true, got %v", i, eng["name"], eng["requiresCRD"])
		}
		gw, ok := eng["gateway"].(map[string]interface{})
		if !ok {
			t.Fatalf("engine[%d] (%v): expected gateway map, got %T", i, eng["name"], eng["gateway"])
		}
		if gw["inferencePoolNamePattern"] != "{name}-pool" {
			t.Errorf("engine[%d] (%v): expected inferencePoolNamePattern=%q, got %v",
				i, eng["name"], "{name}-pool", gw["inferencePoolNamePattern"])
		}
		if gw["inferencePoolNamespace"] != "{namespace}" {
			t.Errorf("engine[%d] (%v): expected inferencePoolNamespace=%q, got %v",
				i, eng["name"], "{namespace}", gw["inferencePoolNamespace"])
		}
	}

	// The two engines must own distinct gateway maps (deep-copy), otherwise
	// mutating one would silently affect the other.
	eng0Gw := engines[0].(map[string]interface{})["gateway"].(map[string]interface{})
	eng1Gw := engines[1].(map[string]interface{})["gateway"].(map[string]interface{})
	eng0Gw["inferencePoolNamePattern"] = "mutated"
	if eng1Gw["inferencePoolNamePattern"] == "mutated" {
		t.Error("engines share the same gateway map; expected deep-copied per-engine maps")
	}

	// Legacy top-level keys must be gone.
	caps, _, _ := unstructured.NestedMap(result.Object, "spec", "capabilities")
	for _, k := range []string{"servingModes", "gpuSupport", "cpuSupport", "requiresCRD", "gateway"} {
		if _, exists := caps[k]; exists {
			t.Errorf("expected top-level %q to be removed from capabilities after migration", k)
		}
	}
}

func TestMigrateLegacyProviderConfigs_AlreadyMigrated(t *testing.T) {
	// Create an InferenceProviderConfig already in the new format
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	obj.SetName("dynamo")
	if err := unstructured.SetNestedField(obj.Object, map[string]interface{}{
		"engines": []interface{}{
			map[string]interface{}{
				"name":         "vllm",
				"gpuSupport":   true,
				"servingModes": []interface{}{"aggregated", "disaggregated"},
			},
		},
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set capabilities: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).WithObjects(obj).Build()

	err := MigrateLegacyProviderConfigs(context.Background(), c)
	if err != nil {
		t.Fatalf("migration failed on already-migrated object: %v", err)
	}

	// Read back — should be unchanged
	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(obj.GroupVersionKind())
	if err := c.Get(context.Background(), client_key("dynamo"), result); err != nil {
		t.Fatalf("failed to get object: %v", err)
	}

	engines, _, _ := unstructured.NestedSlice(result.Object, "spec", "capabilities", "engines")
	if len(engines) != 1 {
		t.Fatalf("expected 1 engine, got %d", len(engines))
	}
	eng0, ok := engines[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected engine to be a map, got %T", engines[0])
	}
	if eng0["name"] != "vllm" {
		t.Errorf("expected name=vllm, got %v", eng0["name"])
	}
}

func TestMigrateLegacyProviderConfigs_NoObjects(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).Build()

	err := MigrateLegacyProviderConfigs(context.Background(), c)
	if err != nil {
		t.Fatalf("migration should not fail with no objects: %v", err)
	}
}

// TestMigrateLegacyProviderConfigs_StripsStaleFlatKeysWithEmptyEngines covers
// the edge case where a hand-crafted legacy InferenceProviderConfig has
// `engines: []` (or no engines) but still carries legacy flat capability
// keys. Typed decode would ignore the unknown fields, but the migration
// should still scrub them so the stored object stays clean.
func TestMigrateLegacyProviderConfigs_StripsStaleFlatKeysWithEmptyEngines(t *testing.T) {
	legacy := &unstructured.Unstructured{}
	legacy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	legacy.SetName("ghost")
	if err := unstructured.SetNestedField(legacy.Object, map[string]interface{}{
		"engines":      []interface{}{},
		"servingModes": []interface{}{"aggregated"},
		"gpuSupport":   true,
		"cpuSupport":   false,
		"requiresCRD":  true,
		"gateway": map[string]interface{}{
			"inferencePoolNamePattern": "{name}-pool",
		},
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set capabilities: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).WithObjects(legacy).Build()

	if err := MigrateLegacyProviderConfigs(context.Background(), c); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(legacy.GroupVersionKind())
	if err := c.Get(context.Background(), client_key("ghost"), result); err != nil {
		t.Fatalf("failed to get migrated object: %v", err)
	}

	caps, _, _ := unstructured.NestedMap(result.Object, "spec", "capabilities")
	for _, k := range []string{"servingModes", "gpuSupport", "cpuSupport", "requiresCRD", "gateway"} {
		if _, exists := caps[k]; exists {
			t.Errorf("expected top-level %q to be removed after migration, still present", k)
		}
	}

	// engines may be an empty slice or absent — both are acceptable post-cleanup.
	if engines, found, _ := unstructured.NestedSlice(caps, "engines"); found && len(engines) != 0 {
		t.Errorf("expected engines to remain empty, got %v", engines)
	}
}

// TestMigrateLegacyProviderConfigs_NoUpdateWhenClean ensures the migration
// does NOT call Update on a capabilities map that is already clean (empty
// engines, no legacy flat keys). This prevents needless writes / resource
// version churn during controller startup.
func TestMigrateLegacyProviderConfigs_NoUpdateWhenClean(t *testing.T) {
	clean := &unstructured.Unstructured{}
	clean.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	clean.SetName("clean")
	clean.SetResourceVersion("1")
	if err := unstructured.SetNestedField(clean.Object, map[string]interface{}{
		"engines": []interface{}{},
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set capabilities: %v", err)
	}

	base := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			ul, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			ul.Items = []unstructured.Unstructured{*clean}
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			t.Errorf("unexpected Update call on already-clean object %q", obj.GetName())
			return nil
		},
	})

	if err := MigrateLegacyProviderConfigs(context.Background(), c); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
}

// client_key is a helper to create a client.ObjectKey for cluster-scoped resources.
func client_key(name string) client.ObjectKey {
	return client.ObjectKey{Name: name}
}

// TestMigrateLegacyProviderConfigs_PropagatesListErrors verifies the fix for the
// review feedback on PR #214: List errors other than "CRD not installed" must be
// surfaced rather than silently swallowed.
func TestMigrateLegacyProviderConfigs_PropagatesListErrors(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	}

	cases := []struct {
		name      string
		listErr   error
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "no-match error (CRD not registered) is swallowed",
			listErr: &meta.NoKindMatchError{GroupKind: gvk.GroupKind()},
			wantErr: false,
		},
		{
			name:    "NotFound error is swallowed",
			listErr: apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: "inferenceproviderconfigs"}, ""),
			wantErr: false,
		},
		{
			name:      "Forbidden error is propagated",
			listErr:   apierrors.NewForbidden(schema.GroupResource{Group: gvk.Group, Resource: "inferenceproviderconfigs"}, "", errors.New("no access")),
			wantErr:   true,
			errSubstr: "failed to list InferenceProviderConfig",
		},
		{
			name:      "ServerTimeout error is propagated",
			listErr:   apierrors.NewServerTimeout(schema.GroupResource{Group: gvk.Group, Resource: "inferenceproviderconfigs"}, "list", 1),
			wantErr:   true,
			errSubstr: "failed to list InferenceProviderConfig",
		},
		{
			name:      "generic error is propagated",
			listErr:   errors.New("kaboom"),
			wantErr:   true,
			errSubstr: "kaboom",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).Build()
			c := interceptor.NewClient(base, interceptor.Funcs{
				List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
					return tc.listErr
				},
			})

			err := MigrateLegacyProviderConfigs(context.Background(), c)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.errSubstr != "" && !containsString(err.Error(), tc.errSubstr) {
					t.Errorf("expected error to contain %q, got %q", tc.errSubstr, err.Error())
				}
			} else if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

// TestMigrateLegacyProviderConfigs_UpdateErrorPropagates ensures Update failures
// during migration are also surfaced rather than swallowed.
func TestMigrateLegacyProviderConfigs_UpdateErrorPropagates(t *testing.T) {
	legacy := &unstructured.Unstructured{}
	legacy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	legacy.SetName("kaito")
	legacy.SetResourceVersion("1")
	if err := unstructured.SetNestedField(legacy.Object, map[string]interface{}{
		"engines": []interface{}{"vllm"},
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set capabilities: %v", err)
	}

	base := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).Build()
	// We can't seed unstructured objects via WithObjects without scheme registration,
	// so simulate the list returning the legacy object via interceptor.
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			ul, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			ul.Items = []unstructured.Unstructured{*legacy}
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: "airunway.ai", Resource: "inferenceproviderconfigs"}, "kaito", errors.New("conflict"))
		},
	})

	err := MigrateLegacyProviderConfigs(context.Background(), c)
	if err == nil {
		t.Fatalf("expected update conflict to propagate, got nil")
	}
	if !containsString(err.Error(), "failed to update migrated InferenceProviderConfig kaito") {
		t.Errorf("expected wrapped update error, got %q", err.Error())
	}
}

func containsString(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
