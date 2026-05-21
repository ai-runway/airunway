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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// inferenceProviderConfigGVK is the GVK used for unstructured reads/writes of
// the legacy and current InferenceProviderConfig schemas during migration.
var inferenceProviderConfigGVK = schema.GroupVersionKind{
	Group:   "airunway.ai",
	Version: "v1alpha1",
	Kind:    "InferenceProviderConfig",
}

// legacyFlatKeys are the fields that used to live directly on
// spec.capabilities but have since moved into each EngineCapability.
// The migration must strip these from the stored object whether or not
// engines were present, so a hand-crafted legacy CR with engines: [] but
// stale flat keys doesn't leave dead fields lying around.
//
// FORWARD-COMPATIBILITY HAZARD: these keys are unconditionally deleted
// from spec.capabilities on every migration pass. If a future change to
// ProviderCapabilities (controller/api/v1alpha1/inferenceproviderconfig_types.go)
// reintroduces a top-level field whose JSON tag collides with one of these
// names, the migration will silently strip it at controller startup. When
// extending ProviderCapabilities, either pick a JSON tag that does not
// appear in this list, or update both this list and the hoist/cleanup
// branches in applyMigration to preserve the new field.
var legacyFlatKeys = []string{"servingModes", "gpuSupport", "cpuSupport", "requiresCRD", "gateway"}

// MigrateLegacyProviderConfigs reads all InferenceProviderConfig resources using
// an unstructured client and rewrites any that still use the legacy flat engine
// format (engines: ["vllm"]) to the new per-engine capability format
// (engines: [{name: "vllm", gpuSupport: true, ...}]).
//
// This migration is idempotent: objects already in the new format are skipped.
// It must run before any typed Get/List calls to avoid deserialization failures
// during upgrades from the flat ProviderCapabilities schema.
func MigrateLegacyProviderConfigs(ctx context.Context, c client.Client) error {
	logger := log.FromContext(ctx).WithName("migration")

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   inferenceProviderConfigGVK.Group,
		Version: inferenceProviderConfigGVK.Version,
		Kind:    inferenceProviderConfigGVK.Kind + "List",
	})

	// Retry transient API server errors with a bounded exponential backoff so a
	// brief apiserver hiccup at pod start doesn't crashloop the controller.
	// Steps: ~1s, 2s, 4s, 8s, 16s (capped) ≈ 31s total.
	//
	// Terminal conditions short-circuit the loop:
	//   * NoMatch / NotFound — CRD not installed; benign skip.
	//   * Forbidden / Unauthorized — configuration error; fail fast.
	// Anything else is treated as transient.
	backoff := wait.Backoff{
		Duration: time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    5,
		Cap:      16 * time.Second,
	}
	var (
		crdAbsent bool
		lastErr   error
	)
	waitErr := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		err := c.List(ctx, list)
		switch {
		case err == nil:
			return true, nil
		case meta.IsNoMatchError(err) || apierrors.IsNotFound(err):
			crdAbsent = true
			return true, nil
		case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
			lastErr = err
			return false, err
		default:
			lastErr = err
			logger.Info("transient error listing InferenceProviderConfig; will retry", "error", err.Error())
			return false, nil
		}
	})
	if crdAbsent {
		logger.Info("InferenceProviderConfig CRD not present; skipping migration")
		return nil
	}
	if waitErr != nil {
		// Prefer the underlying API error over the generic timeout wrapper.
		if lastErr == nil {
			lastErr = waitErr
		}
		return fmt.Errorf("failed to list InferenceProviderConfig for migration after retries: %w", lastErr)
	}

	// For each discovered object, run the migration against a fresh read inside
	// a RetryOnConflict loop. The List above is only used for discovery; the
	// actual mutation must operate on a freshly-Get'd object so that retries
	// after a 409 use an up-to-date resourceVersion (rather than spinning on
	// the stale copy returned by List).
	for i := range list.Items {
		name := list.Items[i].GetName()
		key := client.ObjectKey{Namespace: list.Items[i].GetNamespace(), Name: name}
		if err := migrateAndUpdate(ctx, c, key); err != nil {
			return fmt.Errorf("failed to update migrated InferenceProviderConfig %s: %w", name, err)
		}
	}

	return nil
}

// migrateAndUpdate Gets the object, applies the migration, and Updates it back,
// all inside a RetryOnConflict loop. Re-reading inside the closure is what makes
// the retry actually meaningful: on a 409, we pick up the new resourceVersion
// (and any other writer's changes) before trying again, instead of resubmitting
// the same stale object and guaranteeing another conflict.
//
// If a conflict persists past RetryOnConflict's bounded retries, we treat it as
// a soft success: the migration is idempotent and a concurrent writer (another
// replica that lost leader election, a human operator) must have produced
// equivalent state, so re-reading would just confirm the object is migrated.
func migrateAndUpdate(ctx context.Context, c client.Client, key client.ObjectKey) error {
	logger := log.FromContext(ctx).WithName("migration")

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(inferenceProviderConfigGVK)
		if err := c.Get(ctx, key, fresh); err != nil {
			if apierrors.IsNotFound(err) {
				// Object was deleted out from under us between List and Get;
				// nothing to migrate.
				return nil
			}
			return err
		}

		changed, kind, err := applyMigration(fresh, logger)
		if err != nil {
			return err
		}
		if !changed {
			// Either already in the new format, or a concurrent writer migrated
			// it after our List. Nothing to do.
			return nil
		}

		logger.Info("writing migrated InferenceProviderConfig", "name", key.Name, "kind", kind)
		return c.Update(ctx, fresh)
	})
	if apierrors.IsConflict(err) {
		logger.Info("InferenceProviderConfig was updated concurrently; assuming migration completed by another writer",
			"name", key.Name)
		return nil
	}
	return err
}

// applyMigration inspects a freshly-read InferenceProviderConfig and mutates it
// in place to bring it onto the new per-engine schema. It returns:
//   - changed: true if the object was modified and needs a write-back.
//   - kind:    a short label ("migrated", "hoisted", "cleaned") for logging.
//   - err:     non-nil on internal unstructured manipulation failures.
//
// applyMigration is intentionally pure (no closures over outer state) so the
// RetryOnConflict closure can call it again against a re-Get'd object after a
// conflict and produce the same result.
func applyMigration(obj *unstructured.Unstructured, logger logr.Logger) (bool, string, error) {
	capabilities, found, err := unstructured.NestedMap(obj.Object, "spec", "capabilities")
	if err != nil {
		logger.Info("skipping InferenceProviderConfig with malformed spec.capabilities",
			"name", obj.GetName(), "error", err.Error())
		return false, "", nil
	}
	if !found || capabilities == nil {
		return false, "", nil
	}

	engines, _, err := unstructured.NestedSlice(capabilities, "engines")
	if err != nil {
		logger.Info("skipping InferenceProviderConfig with malformed spec.capabilities.engines",
			"name", obj.GetName(), "error", err.Error())
		return false, "", nil
	}

	// Branch 1: engines is missing/empty. Nothing to convert, but still strip
	// any stale legacy flat keys so a hand-crafted CR doesn't keep dead fields.
	if len(engines) == 0 {
		if !hasAnyLegacyFlatKey(capabilities) {
			return false, "", nil
		}
		for _, k := range legacyFlatKeys {
			delete(capabilities, k)
		}
		if err := unstructured.SetNestedField(obj.Object, capabilities, "spec", "capabilities"); err != nil {
			return false, "", fmt.Errorf("set cleaned capabilities: %w", err)
		}
		return true, "cleaned", nil
	}

	// Branch 2: object-form engines. Either fully migrated (no flat keys → no
	// change), or a partially-updated manifest where someone authored object
	// engines but the legacy flat keys still sit on spec.capabilities. In that
	// partial case we hoist the flat values into each engine (without
	// overwriting per-engine values the author already set) and then strip the
	// legacy keys — otherwise gateway/CRD-requirement data is silently lost.
	if _, isString := engines[0].(string); !isString {
		if !hasAnyLegacyFlatKey(capabilities) {
			return false, "", nil
		}

		oldServingModes, oldGPUSupport, oldCPUSupport,
			oldRequiresCRD, hasRequiresCRD,
			oldGateway, hasGateway := readLegacyFlatValues(capabilities)

		for i, e := range engines {
			eng, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			if _, set := eng["gpuSupport"]; !set {
				eng["gpuSupport"] = oldGPUSupport
			}
			if _, set := eng["cpuSupport"]; !set {
				eng["cpuSupport"] = oldCPUSupport
			}
			if _, set := eng["servingModes"]; !set && len(oldServingModes) > 0 {
				modes := make([]interface{}, len(oldServingModes))
				for j, m := range oldServingModes {
					modes[j] = m
				}
				eng["servingModes"] = modes
			}
			if _, set := eng["requiresCRD"]; !set && hasRequiresCRD {
				eng["requiresCRD"] = oldRequiresCRD
			}
			if _, set := eng["gateway"]; !set && hasGateway && len(oldGateway) > 0 {
				// Deep-copy so each engine owns its own map.
				eng["gateway"] = runtime.DeepCopyJSONValue(oldGateway)
			}
			engines[i] = eng
		}

		capabilities["engines"] = engines
		for _, k := range legacyFlatKeys {
			delete(capabilities, k)
		}
		if err := unstructured.SetNestedField(obj.Object, capabilities, "spec", "capabilities"); err != nil {
			return false, "", fmt.Errorf("set hoisted capabilities: %w", err)
		}
		return true, "hoisted", nil
	}

	// Branch 3: string-form engines — the original legacy schema. Convert each
	// string engine into an EngineCapability object using the flat top-level
	// values, then strip the legacy keys.
	oldServingModes, oldGPUSupport, oldCPUSupport,
		oldRequiresCRD, hasRequiresCRD,
		oldGateway, hasGateway := readLegacyFlatValues(capabilities)

	newEngines := make([]interface{}, 0, len(engines))
	for _, e := range engines {
		engineName, ok := e.(string)
		if !ok {
			continue
		}

		engineCap := map[string]interface{}{
			"name":       engineName,
			"gpuSupport": oldGPUSupport,
			"cpuSupport": oldCPUSupport,
		}
		if len(oldServingModes) > 0 {
			modes := make([]interface{}, len(oldServingModes))
			for i, m := range oldServingModes {
				modes[i] = m
			}
			engineCap["servingModes"] = modes
		}
		if hasRequiresCRD {
			engineCap["requiresCRD"] = oldRequiresCRD
		}
		if hasGateway && len(oldGateway) > 0 {
			// Deep-copy so each engine gets its own map.
			engineCap["gateway"] = runtime.DeepCopyJSONValue(oldGateway)
		}
		newEngines = append(newEngines, engineCap)
	}

	// Mutate the existing capabilities map in place so that any other top-level
	// keys (present today or added in the future) are preserved rather than
	// silently dropped.
	capabilities["engines"] = newEngines
	for _, k := range legacyFlatKeys {
		delete(capabilities, k)
	}
	if err := unstructured.SetNestedField(obj.Object, capabilities, "spec", "capabilities"); err != nil {
		return false, "", fmt.Errorf("set migrated capabilities: %w", err)
	}
	return true, "migrated", nil
}

// hasAnyLegacyFlatKey reports whether the capabilities map contains any of the
// legacy top-level keys that have since moved into per-engine EngineCapability.
func hasAnyLegacyFlatKey(caps map[string]interface{}) bool {
	for _, k := range legacyFlatKeys {
		if _, ok := caps[k]; ok {
			return true
		}
	}
	return false
}

// readLegacyFlatValues extracts the legacy top-level capability fields from a
// capabilities map. Returned as named values so both the string-engine
// migration branch and the partial-migration hoist branch can reuse it.
func readLegacyFlatValues(caps map[string]interface{}) (
	servingModes []string,
	gpuSupport, cpuSupport bool,
	requiresCRD bool, hasRequiresCRD bool,
	gateway map[string]interface{}, hasGateway bool,
) {
	servingModes, _, _ = unstructured.NestedStringSlice(caps, "servingModes")
	gpuSupport, _, _ = unstructured.NestedBool(caps, "gpuSupport")
	cpuSupport, _, _ = unstructured.NestedBool(caps, "cpuSupport")
	requiresCRD, hasRequiresCRD, _ = unstructured.NestedBool(caps, "requiresCRD")
	gateway, hasGateway, _ = unstructured.NestedMap(caps, "gateway")
	return
}

// LegacyProviderConfigMigrator runs MigrateLegacyProviderConfigs as a
// leader-elected manager.Runnable. With --leader-elect enabled and multiple
// replicas, only the leader performs the rewrites — followers would otherwise
// race the leader's Update and crashloop on 409 Conflict.
//
// The Runnable uses a direct (non-cached) client because the manager's
// informer cache is not yet started for leader-elected runnables when Start
// is invoked, and the migration must use unstructured reads to avoid
// deserialization failures on legacy schema objects.
type LegacyProviderConfigMigrator struct {
	Config *rest.Config
	Scheme *runtime.Scheme
}

// NeedLeaderElection marks the migrator as leader-elected so controller-runtime
// only invokes Start on the elected leader.
func (m *LegacyProviderConfigMigrator) NeedLeaderElection() bool { return true }

// Start performs the migration and returns. Returning nil from a Runnable is
// fine: the manager simply considers this runnable finished and continues
// running the others (the reconciler, the webhook server, etc.).
func (m *LegacyProviderConfigMigrator) Start(ctx context.Context) error {
	c, err := client.New(m.Config, client.Options{Scheme: m.Scheme})
	if err != nil {
		return fmt.Errorf("migration: build direct client: %w", err)
	}
	return MigrateLegacyProviderConfigs(ctx, c)
}
