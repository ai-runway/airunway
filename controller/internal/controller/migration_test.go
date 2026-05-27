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

	"github.com/go-logr/logr"
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

// TestMigrateLegacyProviderConfigs_PreservesPerEngineRequiresCRDWhenFlatAlsoPresent
// pins the behavior that, when an InferenceProviderConfig is already in the
// new per-engine object format, the migration must NOT overwrite per-engine
// requiresCRD values with a stale flat spec.capabilities.requiresCRD that
// somehow coexists in the same object. Such a hybrid is impossible to
// produce via normal API usage today, but a future refactor of the
// "is this legacy?" detection could regress this guarantee; this test
// guards against that.
func TestMigrateLegacyProviderConfigs_PreservesPerEngineRequiresCRDWhenFlatAlsoPresent(t *testing.T) {
	hybrid := &unstructured.Unstructured{}
	hybrid.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	hybrid.SetName("hybrid")
	if err := unstructured.SetNestedField(hybrid.Object, map[string]interface{}{
		// Engines already in the new object format with explicit, non-default
		// per-engine requiresCRD values.
		"engines": []interface{}{
			map[string]interface{}{
				"name":        "vllm",
				"gpuSupport":  true,
				"requiresCRD": false,
			},
			map[string]interface{}{
				"name":        "llamacpp",
				"gpuSupport":  false,
				"requiresCRD": true,
			},
		},
		// Stale flat legacy key intentionally set to the opposite of engine[0]
		// so a wrongful overwrite would be loud.
		"requiresCRD": true,
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set capabilities: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).WithObjects(hybrid).Build()

	if err := MigrateLegacyProviderConfigs(context.Background(), c); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(hybrid.GroupVersionKind())
	if err := c.Get(context.Background(), client_key("hybrid"), result); err != nil {
		t.Fatalf("failed to get migrated object: %v", err)
	}

	engines, found, err := unstructured.NestedSlice(result.Object, "spec", "capabilities", "engines")
	if err != nil || !found {
		t.Fatalf("migrated object missing engines: err=%v found=%v", err, found)
	}
	if len(engines) != 2 {
		t.Fatalf("expected 2 engines, got %d", len(engines))
	}

	eng0, ok := engines[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected engine[0] to be a map, got %T", engines[0])
	}
	if eng0["name"] != "vllm" {
		t.Errorf("expected engine[0].name=vllm, got %v", eng0["name"])
	}
	if eng0["requiresCRD"] != false {
		t.Errorf("per-engine requiresCRD was overwritten by stale flat value: expected engine[0].requiresCRD=false, got %v", eng0["requiresCRD"])
	}

	eng1, ok := engines[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected engine[1] to be a map, got %T", engines[1])
	}
	if eng1["name"] != "llamacpp" {
		t.Errorf("expected engine[1].name=llamacpp, got %v", eng1["name"])
	}
	if eng1["requiresCRD"] != true {
		t.Errorf("per-engine requiresCRD was overwritten by stale flat value: expected engine[1].requiresCRD=true, got %v", eng1["requiresCRD"])
	}
}

// TestMigrateLegacyProviderConfigs_HoistsFlatKeysFromPartialMigration covers
// the gap flagged in PR #214 review r3283298971: a partially-updated manifest
// where engines are already object-form but the legacy flat capability keys
// (gateway, requiresCRD, gpuSupport, ...) still sit on spec.capabilities. The
// migration must hoist those flat values into each engine (without
// overwriting per-engine values the author already set) and then strip the
// legacy keys.
func TestMigrateLegacyProviderConfigs_HoistsFlatKeysFromPartialMigration(t *testing.T) {
	hybrid := &unstructured.Unstructured{}
	hybrid.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	hybrid.SetName("partial")
	if err := unstructured.SetNestedField(hybrid.Object, map[string]interface{}{
		"engines": []interface{}{
			// engine[0] sets nothing — should pick up all flat values.
			map[string]interface{}{"name": "vllm"},
			// engine[1] explicitly sets gateway — must NOT be overwritten by
			// the flat top-level gateway.
			map[string]interface{}{
				"name": "llamacpp",
				"gateway": map[string]interface{}{
					"inferencePoolNamePattern": "engine-owned",
				},
			},
		},
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

	c := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).WithObjects(hybrid).Build()

	if err := MigrateLegacyProviderConfigs(context.Background(), c); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(hybrid.GroupVersionKind())
	if err := c.Get(context.Background(), client_key("partial"), result); err != nil {
		t.Fatalf("failed to get migrated object: %v", err)
	}

	engines, found, err := unstructured.NestedSlice(result.Object, "spec", "capabilities", "engines")
	if err != nil || !found {
		t.Fatalf("migrated object missing engines: err=%v found=%v", err, found)
	}
	if len(engines) != 2 {
		t.Fatalf("expected 2 engines, got %d", len(engines))
	}

	// engine[0]: every flat value should have been hoisted in.
	eng0 := engines[0].(map[string]interface{})
	if eng0["gpuSupport"] != true {
		t.Errorf("engine[0]: expected gpuSupport=true (hoisted), got %v", eng0["gpuSupport"])
	}
	if eng0["requiresCRD"] != true {
		t.Errorf("engine[0]: expected requiresCRD=true (hoisted), got %v", eng0["requiresCRD"])
	}
	gw0, ok := eng0["gateway"].(map[string]interface{})
	if !ok {
		t.Fatalf("engine[0]: expected hoisted gateway map, got %T", eng0["gateway"])
	}
	if gw0["inferencePoolNamePattern"] != "{name}-pool" {
		t.Errorf("engine[0]: expected hoisted gateway pattern, got %v", gw0["inferencePoolNamePattern"])
	}

	// engine[1]: per-engine gateway must be preserved verbatim, not clobbered.
	eng1 := engines[1].(map[string]interface{})
	gw1, ok := eng1["gateway"].(map[string]interface{})
	if !ok {
		t.Fatalf("engine[1]: expected per-engine gateway map, got %T", eng1["gateway"])
	}
	if gw1["inferencePoolNamePattern"] != "engine-owned" {
		t.Errorf("engine[1]: per-engine gateway was overwritten by hoist: got %v", gw1["inferencePoolNamePattern"])
	}
	if _, present := gw1["inferencePoolNamespace"]; present {
		t.Errorf("engine[1]: per-engine gateway was merged with flat gateway; expected verbatim preservation, got %v", gw1)
	}

	// engine[0] and engine[1] gateways must be independent maps (deep-copy
	// for hoisted, original for per-engine).
	gw0["inferencePoolNamePattern"] = "mutated"
	if gw1["inferencePoolNamePattern"] == "mutated" {
		t.Error("hoisted gateway and per-engine gateway share state")
	}

	// All legacy flat keys must be stripped.
	caps, _, _ := unstructured.NestedMap(result.Object, "spec", "capabilities")
	for _, k := range []string{"servingModes", "gpuSupport", "cpuSupport", "requiresCRD", "gateway"} {
		if _, exists := caps[k]; exists {
			t.Errorf("expected top-level %q to be removed after migration, still present", k)
		}
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
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("unexpected get target type %T", obj)
			}
			clean.DeepCopyInto(u)
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

// TestMigrateLegacyProviderConfigs_UpdateErrorPropagates ensures non-conflict
// Update failures during migration are surfaced as a summary error rather than
// silently swallowed. The migration now keeps going on per-object failure (so
// one bad object doesn't crashloop the controller and doesn't strand other
// objects un-migrated), but the summary error is still returned to callers so
// tests and the Runnable's log line can detect partial completion. (Conflict
// errors are handled separately — see TestMigrateLegacyProviderConfigs_UpdateConflictTreatedAsSuccess.)
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
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("unexpected get target type %T", obj)
			}
			legacy.DeepCopyInto(u)
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
			return apierrors.NewInternalError(errors.New("boom"))
		},
	})

	err := MigrateLegacyProviderConfigs(context.Background(), c)
	if err == nil {
		t.Fatalf("expected update error to surface as a summary error, got nil")
	}
	if !errors.Is(err, errMigrationPartial) || !containsString(err.Error(), "1 of 1 object(s) failed") {
		t.Errorf("expected partial-failure summary error, got %q", err.Error())
	}
}

// TestMigrateLegacyProviderConfigs_ContinuesAfterPerObjectFailure verifies the
// fix for issue #6: a per-object failure must not abort the whole migration
// run. Two legacy objects exist; Update on the first returns an internal
// error, but the second must still be attempted (and succeed), and the
// summary error reports 1 failure of 2.
func TestMigrateLegacyProviderConfigs_ContinuesAfterPerObjectFailure(t *testing.T) {
	makeLegacy := func(name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "airunway.ai",
			Version: "v1alpha1",
			Kind:    "InferenceProviderConfig",
		})
		u.SetName(name)
		u.SetResourceVersion("1")
		if err := unstructured.SetNestedField(u.Object, map[string]interface{}{
			"engines": []interface{}{"vllm"},
		}, "spec", "capabilities"); err != nil {
			t.Fatalf("failed to set capabilities for %s: %v", name, err)
		}
		return u
	}
	bad := makeLegacy("bad-update")
	good := makeLegacy("good-update")

	base := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).Build()
	updateCalls := map[string]int{}
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			ul, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			ul.Items = []unstructured.Unstructured{*bad, *good}
			return nil
		},
		Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("unexpected get target type %T", obj)
			}
			if key.Name == "bad-update" {
				bad.DeepCopyInto(u)
			} else {
				good.DeepCopyInto(u)
			}
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			updateCalls[obj.GetName()]++
			if obj.GetName() == "bad-update" {
				return apierrors.NewInternalError(errors.New("boom"))
			}
			return nil
		},
	})

	err := MigrateLegacyProviderConfigs(context.Background(), c)
	if err == nil {
		t.Fatalf("expected summary error reporting partial failure, got nil")
	}
	if !errors.Is(err, errMigrationPartial) || !containsString(err.Error(), "1 of 2 object(s) failed") {
		t.Errorf("expected summary error to report 1/2 failure, got %q", err.Error())
	}
	if updateCalls["good-update"] == 0 {
		t.Errorf("expected good-update to be attempted despite earlier failure on bad-update; calls=%v", updateCalls)
	}
}

// TestMigrateLegacyProviderConfigs_FailsLoudlyOnMalformedStringEngine verifies
// the fix for issue #3: a malformed entry inside a legacy string-form engines
// list (e.g. an integer where a string is expected) must NOT be silently
// dropped. The migration must leave the object untouched (no Update) and
// return a partial-failure summary error so an operator notices.
func TestMigrateLegacyProviderConfigs_FailsLoudlyOnMalformedStringEngine(t *testing.T) {
	bad := &unstructured.Unstructured{}
	bad.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	bad.SetName("malformed-string-engine")
	bad.SetResourceVersion("1")
	// First entry is a valid string, second is a non-string — under the old
	// behavior the second was silently dropped and the truncated list was
	// written back.
	if err := unstructured.SetNestedField(bad.Object, map[string]interface{}{
		"engines": []interface{}{"vllm", int64(42)},
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set engines: %v", err)
	}

	base := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			ul, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			ul.Items = []unstructured.Unstructured{*bad}
			return nil
		},
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("unexpected get target type %T", obj)
			}
			bad.DeepCopyInto(u)
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			t.Errorf("unexpected Update call on malformed-string-engine %q — list would be silently truncated", obj.GetName())
			return nil
		},
	})

	err := MigrateLegacyProviderConfigs(context.Background(), c)
	if err == nil {
		t.Fatalf("expected partial-failure summary error for malformed engine entry, got nil")
	}
	if !errors.Is(err, errMigrationPartial) || !containsString(err.Error(), "1 of 1 object(s) failed") {
		t.Errorf("expected summary error mentioning failure count, got %q", err.Error())
	}
}

// TestMigrateLegacyProviderConfigs_FailsLoudlyOnMalformedObjectEngine is the
// object-form counterpart to FailsLoudlyOnMalformedStringEngine: a non-object
// entry inside an otherwise object-form engines list must not be silently
// dropped during the hoist branch.
func TestMigrateLegacyProviderConfigs_FailsLoudlyOnMalformedObjectEngine(t *testing.T) {
	bad := &unstructured.Unstructured{}
	bad.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	bad.SetName("malformed-object-engine")
	bad.SetResourceVersion("1")
	// engines[0] is a proper object, engines[1] is a string — and there's a
	// legacy flat key present so the hoist branch is exercised. Under the old
	// behavior engines[1] was silently dropped.
	if err := unstructured.SetNestedField(bad.Object, map[string]interface{}{
		"engines": []interface{}{
			map[string]interface{}{"name": "vllm"},
			"sglang", // should be a map; non-object → must error, not be dropped
		},
		"gpuSupport": true, // forces hoist branch
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set engines: %v", err)
	}

	base := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			ul, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			ul.Items = []unstructured.Unstructured{*bad}
			return nil
		},
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("unexpected get target type %T", obj)
			}
			bad.DeepCopyInto(u)
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			t.Errorf("unexpected Update call on malformed-object-engine %q — list would be silently truncated", obj.GetName())
			return nil
		},
	})

	err := MigrateLegacyProviderConfigs(context.Background(), c)
	if err == nil {
		t.Fatalf("expected partial-failure summary error for malformed engine entry, got nil")
	}
	if !errors.Is(err, errMigrationPartial) || !containsString(err.Error(), "1 of 1 object(s) failed") {
		t.Errorf("expected summary error mentioning failure count, got %q", err.Error())
	}
}

// TestMigrateLegacyProviderConfigs_UpdateConflictTreatedAsSuccess ensures
// a persistent 409 Conflict on Update does not fail the migration. With
// --leader-elect + multiple replicas (or a human editor racing), a follower
// or third party may write the same object first; since the migration is
// idempotent and the conflicting writer must have produced the same desired
// state, returning an error here would crashloop the controller for no benefit.
func TestMigrateLegacyProviderConfigs_UpdateConflictTreatedAsSuccess(t *testing.T) {
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
	updateCalls := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			ul, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			ul.Items = []unstructured.Unstructured{*legacy}
			return nil
		},
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("unexpected get target type %T", obj)
			}
			legacy.DeepCopyInto(u)
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
			updateCalls++
			return apierrors.NewConflict(schema.GroupResource{Group: "airunway.ai", Resource: "inferenceproviderconfigs"}, "kaito", errors.New("conflict"))
		},
	})

	if err := MigrateLegacyProviderConfigs(context.Background(), c); err != nil {
		t.Fatalf("expected conflict to be treated as soft success, got error: %v", err)
	}
	if updateCalls == 0 {
		t.Errorf("expected at least one Update attempt, got 0")
	}
}

// TestMigrateLegacyProviderConfigs_RetriesAfterConflict verifies that a
// transient 409 Conflict on Update is recovered by re-Getting the object and
// re-applying the migration against the fresh resourceVersion. Before the
// fix, the retry closure re-submitted the same stale object captured by
// List, so every retry deterministically conflicted again and the loop
// exhausted into the soft-success path even when the conflict was benign.
func TestMigrateLegacyProviderConfigs_RetriesAfterConflict(t *testing.T) {
	legacy := &unstructured.Unstructured{}
	legacy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	legacy.SetName("kaito")
	legacy.SetResourceVersion("1")
	if err := unstructured.SetNestedField(legacy.Object, map[string]interface{}{
		"engines":    []interface{}{"vllm"},
		"gpuSupport": true,
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set capabilities: %v", err)
	}

	base := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).Build()

	var (
		getCalls    int
		updateCalls int
		// stored simulates the apiserver's view of the object — its
		// resourceVersion changes between Get calls to simulate a concurrent
		// writer bumping it.
		stored = legacy.DeepCopy()
	)

	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			ul, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			ul.Items = []unstructured.Unstructured{*stored}
			return nil
		},
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			getCalls++
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("unexpected get target type %T", obj)
			}
			// Simulate a concurrent writer bumping resourceVersion before our
			// second Get returns — that's the signal a correctly-implemented
			// retry must pick up.
			if getCalls == 2 {
				stored.SetResourceVersion("2")
			}
			stored.DeepCopyInto(u)
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			updateCalls++
			u := obj.(*unstructured.Unstructured)
			// First Update: reject with 409 — simulating a stale resourceVersion.
			if updateCalls == 1 {
				return apierrors.NewConflict(schema.GroupResource{Group: "airunway.ai", Resource: "inferenceproviderconfigs"}, u.GetName(), errors.New("resourceVersion mismatch"))
			}
			// Second Update: must carry the new resourceVersion. If the retry
			// closure forgot to re-Get, we'd still see "1" here.
			if rv := u.GetResourceVersion(); rv != "2" {
				return fmt.Errorf("expected retry to use refreshed resourceVersion=2, got %q", rv)
			}
			// Accept the write: persist into our simulated apiserver state.
			u.DeepCopyInto(stored)
			return nil
		},
	})

	if err := MigrateLegacyProviderConfigs(context.Background(), c); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	if updateCalls != 2 {
		t.Errorf("expected exactly 2 Update attempts (1 conflict + 1 success), got %d", updateCalls)
	}
	if getCalls < 2 {
		t.Errorf("expected at least 2 Get attempts (initial + post-conflict re-read), got %d", getCalls)
	}

	// Verify the final stored object reflects the new per-engine schema.
	engines, found, err := unstructured.NestedSlice(stored.Object, "spec", "capabilities", "engines")
	if err != nil || !found {
		t.Fatalf("stored object missing engines after retry: err=%v found=%v", err, found)
	}
	if len(engines) != 1 {
		t.Fatalf("expected 1 migrated engine, got %d", len(engines))
	}
	eng0, ok := engines[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected engine[0] to be a map post-migration, got %T", engines[0])
	}
	if eng0["name"] != "vllm" {
		t.Errorf("expected engine[0].name=vllm, got %v", eng0["name"])
	}
	if eng0["gpuSupport"] != true {
		t.Errorf("expected hoisted gpuSupport=true on engine[0], got %v", eng0["gpuSupport"])
	}
	caps, _, _ := unstructured.NestedMap(stored.Object, "spec", "capabilities")
	if _, exists := caps["gpuSupport"]; exists {
		t.Error("expected flat gpuSupport to be stripped from capabilities after retried migration")
	}
}

// TestLegacyProviderConfigMigrator_NeedLeaderElection pins the leader-elected
// nature of the migrator. With multiple replicas, only the leader may perform
// the rewrites — followers would otherwise race the leader's Update and
// crashloop on 409 Conflict. A future refactor must not silently flip this to
// false.
func TestLegacyProviderConfigMigrator_NeedLeaderElection(t *testing.T) {
	m := &LegacyProviderConfigMigrator{}
	if !m.NeedLeaderElection() {
		t.Error("expected LegacyProviderConfigMigrator.NeedLeaderElection() = true")
	}
}

// TestMigrateLegacyProviderConfigs_MalformedCapabilities pins the log-and-skip
// behavior in applyMigration when spec.capabilities cannot be read as a map
// (e.g. someone hand-wrote it as a string). The migration must not crash and
// must not write back to the object.
func TestMigrateLegacyProviderConfigs_MalformedCapabilities(t *testing.T) {
	bad := &unstructured.Unstructured{}
	bad.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	bad.SetName("malformed-caps")
	bad.SetResourceVersion("1")
	// spec.capabilities is a string instead of a map — NestedMap will error.
	if err := unstructured.SetNestedField(bad.Object, "not-a-map", "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set malformed capabilities: %v", err)
	}

	base := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			ul, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			ul.Items = []unstructured.Unstructured{*bad}
			return nil
		},
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("unexpected get target type %T", obj)
			}
			bad.DeepCopyInto(u)
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			t.Errorf("unexpected Update call on malformed object %q", obj.GetName())
			return nil
		},
	})

	if err := MigrateLegacyProviderConfigs(context.Background(), c); err != nil {
		t.Fatalf("expected malformed input to be skipped without error, got %v", err)
	}
}

// TestMigrateLegacyProviderConfigs_MalformedEngines pins the log-and-skip
// behavior in applyMigration when spec.capabilities.engines cannot be read as
// a slice (e.g. someone hand-wrote it as a map). The migration must not crash
// and must not write back.
func TestMigrateLegacyProviderConfigs_MalformedEngines(t *testing.T) {
	bad := &unstructured.Unstructured{}
	bad.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "airunway.ai",
		Version: "v1alpha1",
		Kind:    "InferenceProviderConfig",
	})
	bad.SetName("malformed-engines")
	bad.SetResourceVersion("1")
	// spec.capabilities.engines is a map instead of a slice — NestedSlice will error.
	if err := unstructured.SetNestedField(bad.Object, map[string]interface{}{
		"engines": map[string]interface{}{"not": "a slice"},
	}, "spec", "capabilities"); err != nil {
		t.Fatalf("failed to set malformed engines: %v", err)
	}

	base := fake.NewClientBuilder().WithScheme(newUnstructuredScheme()).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			ul, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			ul.Items = []unstructured.Unstructured{*bad}
			return nil
		},
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("unexpected get target type %T", obj)
			}
			bad.DeepCopyInto(u)
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			t.Errorf("unexpected Update call on malformed-engines object %q", obj.GetName())
			return nil
		},
	})

	if err := MigrateLegacyProviderConfigs(context.Background(), c); err != nil {
		t.Fatalf("expected malformed engines to be skipped without error, got %v", err)
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

// TestClassifyMigrationError verifies the Start-time error policy from
// PR #214 review r3284868255: partial per-object failures must be swallowed
// (the migration is idempotent and the next leader retries), but list/setup
// failures must propagate so the manager tears down and the pod restarts —
// otherwise the controller would start with un-migrated legacy objects on
// disk and subsequent typed reads could fail to deserialize.
func TestClassifyMigrationError(t *testing.T) {
	logger := logr.Discard()

	t.Run("nil passes through", func(t *testing.T) {
		if err := classifyMigrationError(logger, nil); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("partial per-object failure is swallowed", func(t *testing.T) {
		in := fmt.Errorf("%w: 1 of 2 object(s) failed", errMigrationPartial)
		if err := classifyMigrationError(logger, in); err != nil {
			t.Fatalf("expected nil for partial failure, got %v", err)
		}
	})

	t.Run("wrapped partial failure is swallowed", func(t *testing.T) {
		in := fmt.Errorf("outer: %w", fmt.Errorf("%w: 1 of 1 object(s) failed", errMigrationPartial))
		if err := classifyMigrationError(logger, in); err != nil {
			t.Fatalf("expected nil for wrapped partial failure, got %v", err)
		}
	})

	t.Run("list failure is propagated", func(t *testing.T) {
		listErr := fmt.Errorf("failed to list InferenceProviderConfig for migration after retries: %w", errors.New("kaboom"))
		err := classifyMigrationError(logger, listErr)
		if err == nil {
			t.Fatalf("expected list failure to propagate, got nil")
		}
		if errors.Is(err, errMigrationPartial) {
			t.Fatalf("list failure must not be treated as partial: %v", err)
		}
		if !containsString(err.Error(), "could not execute") {
			t.Errorf("expected wrapped error to mention could-not-execute, got %q", err.Error())
		}
		if !errors.Is(err, listErr) {
			t.Errorf("expected wrapped error to keep underlying list error in its chain")
		}
	})

	t.Run("forbidden error is propagated", func(t *testing.T) {
		forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "airunway.ai", Resource: "inferenceproviderconfigs"}, "", errors.New("no access"))
		err := classifyMigrationError(logger, forbidden)
		if err == nil {
			t.Fatalf("expected forbidden error to propagate, got nil")
		}
		if errors.Is(err, errMigrationPartial) {
			t.Fatalf("forbidden must not be treated as partial: %v", err)
		}
	})
}
