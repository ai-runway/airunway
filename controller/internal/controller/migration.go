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

// errMigrationPartial signals that the migration listed objects successfully
// but failed to migrate one or more of them. Safe to swallow at Start because
// the migration is idempotent and the next leader election will retry. Any
// other error returned from MigrateLegacyProviderConfigs (e.g. List/setup
// failure) does NOT wrap this sentinel and is treated as a hard failure.
var errMigrationPartial = errors.New("legacy InferenceProviderConfig migration partial failure")

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
	//
	// Per-object failures are logged but do NOT abort the whole migration:
	// returning an error here would (a) leave previously-migrated objects
	// committed while later ones are skipped, and (b) bubble up to the manager
	// and crashloop the controller. The migration is idempotent, so on the
	// next leader election the unfinished objects get another attempt.
	var migrated, failed int
	for i := range list.Items {
		name := list.Items[i].GetName()
		key := client.ObjectKey{Namespace: list.Items[i].GetNamespace(), Name: name}
		if err := migrateAndUpdate(ctx, c, key); err != nil {
			failed++
			logger.Error(err, "failed to migrate InferenceProviderConfig; leaving object untouched and continuing",
				"name", name)
			continue
		}
		migrated++
	}

	logger.Info("legacy InferenceProviderConfig migration finished",
		"total", len(list.Items), "succeeded", migrated, "failed", failed)
	if failed > 0 {
		// Return a summary error so callers/tests can detect partial migration,
		// but the manager Runnable swallows it (see Start) to avoid crashlooping
		// the controller on a single bad object.
		return fmt.Errorf("%w: %d of %d object(s) failed", errMigrationPartial, failed, len(list.Items))
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

		legacy := readLegacyFlatValues(capabilities)

		for i, e := range engines {
			eng, ok := e.(map[string]interface{})
			if !ok {
				// A malformed entry inside an otherwise object-form engines
				// list should NOT be silently dropped: writing back the
				// truncated list would discard the operator's data. Fail
				// loudly so an operator can fix the source CR.
				return false, "", fmt.Errorf("malformed engine entry at spec.capabilities.engines[%d]: expected object, got %T", i, e)
			}
			if _, set := eng["gpuSupport"]; !set {
				eng["gpuSupport"] = legacy.gpuSupport
			}
			if _, set := eng["cpuSupport"]; !set {
				eng["cpuSupport"] = legacy.cpuSupport
			}
			if _, set := eng["servingModes"]; !set && len(legacy.servingModes) > 0 {
				modes := make([]interface{}, len(legacy.servingModes))
				for j, m := range legacy.servingModes {
					modes[j] = m
				}
				eng["servingModes"] = modes
			}
			if _, set := eng["requiresCRD"]; !set && legacy.hasRequiresCRD {
				eng["requiresCRD"] = legacy.requiresCRD
			}
			if _, set := eng["gateway"]; !set && legacy.hasGateway && len(legacy.gateway) > 0 {
				// Deep-copy so each engine owns its own map.
				eng["gateway"] = runtime.DeepCopyJSONValue(legacy.gateway)
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
	legacy := readLegacyFlatValues(capabilities)

	newEngines := make([]interface{}, 0, len(engines))
	for i, e := range engines {
		engineName, ok := e.(string)
		if !ok {
			// String-form branch: a non-string entry here means the source CR
			// is malformed. Returning an error preserves the original list
			// instead of writing back a silently-truncated one.
			return false, "", fmt.Errorf("malformed engine entry at spec.capabilities.engines[%d]: expected string, got %T", i, e)
		}

		engineCap := map[string]interface{}{
			"name":       engineName,
			"gpuSupport": legacy.gpuSupport,
			"cpuSupport": legacy.cpuSupport,
		}
		if len(legacy.servingModes) > 0 {
			modes := make([]interface{}, len(legacy.servingModes))
			for i, m := range legacy.servingModes {
				modes[i] = m
			}
			engineCap["servingModes"] = modes
		}
		if legacy.hasRequiresCRD {
			engineCap["requiresCRD"] = legacy.requiresCRD
		}
		if legacy.hasGateway && len(legacy.gateway) > 0 {
			// Deep-copy so each engine gets its own map.
			engineCap["gateway"] = runtime.DeepCopyJSONValue(legacy.gateway)
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

// legacyFlatValues bundles the legacy top-level capability fields read from a
// capabilities map. The two `has*` flags distinguish "absent" from "present and
// false" for optional bool/map fields.
type legacyFlatValues struct {
	servingModes   []string
	gpuSupport     bool
	cpuSupport     bool
	requiresCRD    bool
	hasRequiresCRD bool
	gateway        map[string]interface{}
	hasGateway     bool
}

// readLegacyFlatValues extracts the legacy top-level capability fields from a
// capabilities map. Both the string-engine migration branch and the
// partial-migration hoist branch reuse it.
func readLegacyFlatValues(caps map[string]interface{}) legacyFlatValues {
	var v legacyFlatValues
	v.servingModes, _, _ = unstructured.NestedStringSlice(caps, "servingModes")
	v.gpuSupport, _, _ = unstructured.NestedBool(caps, "gpuSupport")
	v.cpuSupport, _, _ = unstructured.NestedBool(caps, "cpuSupport")
	v.requiresCRD, v.hasRequiresCRD, _ = unstructured.NestedBool(caps, "requiresCRD")
	v.gateway, v.hasGateway, _ = unstructured.NestedMap(caps, "gateway")
	return v
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
//
// Error handling distinguishes two cases:
//
//   - Partial per-object failures (errors.Is(err, errMigrationPartial)): the
//     List succeeded and at least one object migrated, but some did not. This
//     is logged and swallowed. The migration is idempotent and the next leader
//     election retries the unmigrated objects; tearing the manager down for
//     one bad CR would crashloop the controller and prevent it from serving
//     traffic for objects that already migrated successfully.
//
//   - Setup / List failure (any other non-nil error): the migration could not
//     execute at all (e.g. the bounded backoff in MigrateLegacyProviderConfigs
//     exhausted retries, or List returned Forbidden/Unauthorized). This is
//     propagated so the manager tears down, the pod restarts, and the next
//     leader election retries the migration from scratch. Proceeding silently
//     would risk subsequent typed reads against un-migrated legacy objects
//     failing to deserialize.
func (m *LegacyProviderConfigMigrator) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("migration")
	c, err := client.New(m.Config, client.Options{Scheme: m.Scheme})
	if err != nil {
		return fmt.Errorf("migration: build direct client: %w", err)
	}
	if err := MigrateLegacyProviderConfigs(ctx, c); err != nil {
		return classifyMigrationError(logger, err)
	}
	return nil
}

// classifyMigrationError applies the Start-time policy that distinguishes
// partial per-object failures (safe to swallow) from setup/List failures
// (must propagate). Extracted so it can be unit-tested without standing up
// a real rest.Config / manager.
func classifyMigrationError(logger logr.Logger, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errMigrationPartial) {
		// Partial per-object failure: idempotent, next leader retries.
		logger.Error(err, "legacy InferenceProviderConfig migration completed with per-object errors; controller will continue and retry unmigrated objects on next leader election")
		return nil
	}
	// Migration did not execute (List/setup failure). Propagate so the
	// manager tears down and the pod restarts rather than silently
	// proceeding with un-migrated legacy objects on disk.
	return fmt.Errorf("legacy InferenceProviderConfig migration could not execute: %w", err)
}
