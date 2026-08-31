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

// Package validation contains pure helpers shared between the
// ModelDeployment admission webhook and the reconciler. Centralising
// these checks ensures the two callers cannot drift apart on what
// constitutes a valid spec.
package validation

import (
	"fmt"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// CompatibilityErrorKind identifies the category of compatibility
// failure so callers can map it to their preferred error shape
// (field.Invalid for the webhook, plain error for the reconciler).
type CompatibilityErrorKind string

const (
	// ErrEngineUnsupported means the named provider does not declare a
	// capability for the requested engine.
	ErrEngineUnsupported CompatibilityErrorKind = "EngineUnsupported"
	// ErrServingModeUnsupported means the engine capability does not
	// list the requested serving mode.
	ErrServingModeUnsupported CompatibilityErrorKind = "ServingModeUnsupported"
	// ErrGPURequired means aggregated mode was requested with no GPU,
	// and the (named) provider's engine capability does not declare
	// CPU support.
	ErrGPURequired CompatibilityErrorKind = "GPURequired"
)

// CompatibilityError describes a single compatibility violation.
// FieldPath holds the offending spec path as plain string segments so
// that this package does not depend on k8s field.Path; callers convert
// to whatever path representation they use.
type CompatibilityError struct {
	Kind      CompatibilityErrorKind
	FieldPath []string
	// BadValue is the rejected value, typed as any to match
	// k8s field.Invalid semantics. May be nil.
	BadValue any
	Message  string
}

// Error implements the error interface so a caller can convert directly
// to a plain Go error if desired.
func (e CompatibilityError) Error() string { return e.Message }

// CheckProviderCompatibility validates the engine / serving-mode /
// CPU-GPU combination against provider capabilities.
//
// Inputs:
//   - providerName: value of spec.provider.name; may be empty.
//   - namedConfig: the InferenceProviderConfig for providerName, or nil
//     if the caller could not find it (or providerName is empty).
//   - allConfigs: the full list of registered InferenceProviderConfigs.
//     Only consulted to answer "does any provider support CPU for this
//     engine?" when providerName is empty. Pass nil to skip that
//     cross-provider fallback (the webhook does this — it cannot
//     enumerate all providers from a single Get).
//   - engineType: spec.engine.type (may be empty — no engine-specific
//     checks run in that case).
//   - servingMode: resolved serving mode (caller should default empty
//     to ServingModeAggregated before calling).
//   - gpuCount: resolved spec.resources.gpu.count (0 if unset).
//
// Returns nil when the combination is acceptable. The function does not
// validate disaggregated-mode scaling fields — those checks live in
// each caller because the error shapes differ.
//
// CPU support semantics:
//   - When providerName is set, the CPU check uses the named provider's
//     engine capability only. A different provider advertising CPU
//     support does not satisfy a spec pinned to a GPU-only provider.
//   - When providerName is empty and allConfigs is non-empty, the check
//     passes if any provider in allConfigs declares CPU support for the
//     engine — this matches the reconciler's pre-existing fallback for
//     specs that omit spec.provider.name.
//   - When providerName is empty and allConfigs is nil/empty, the CPU
//     check is skipped (no capability data to consult).
func CheckProviderCompatibility(
	providerName string,
	namedConfig *airunwayv1alpha1.InferenceProviderConfig,
	allConfigs []airunwayv1alpha1.InferenceProviderConfig,
	engineType airunwayv1alpha1.EngineType,
	servingMode airunwayv1alpha1.ServingMode,
	gpuCount int32,
) []CompatibilityError {
	var errs []CompatibilityError

	// Resolve the engine capability for the named provider, if any.
	var namedEngineCap *airunwayv1alpha1.EngineCapability
	if providerName != "" && namedConfig != nil && namedConfig.Spec.Capabilities != nil && engineType != "" {
		namedEngineCap = namedConfig.Spec.Capabilities.GetEngineCapability(engineType)
		if namedEngineCap == nil {
			errs = append(errs, CompatibilityError{
				Kind:      ErrEngineUnsupported,
				FieldPath: []string{"engine", "type"},
				BadValue:  string(engineType),
				Message:   fmt.Sprintf("provider %s does not support engine %s", providerName, engineType),
			})
		}
	}

	if namedEngineCap != nil && !namedEngineCap.SupportsServingMode(servingMode) {
		errs = append(errs, CompatibilityError{
			Kind:      ErrServingModeUnsupported,
			FieldPath: []string{"serving", "mode"},
			BadValue:  string(servingMode),
			Message:   fmt.Sprintf("provider %s does not support %s mode for engine %s", providerName, servingMode, engineType),
		})
	}

	// CPU/GPU check applies only in aggregated mode with no GPU.
	if engineType != "" &&
		servingMode == airunwayv1alpha1.ServingModeAggregated &&
		gpuCount == 0 {
		switch {
		case namedEngineCap != nil:
			// Named provider: must declare CPU support for this engine.
			if !namedEngineCap.CPUSupport {
				errs = append(errs, CompatibilityError{
					Kind:      ErrGPURequired,
					FieldPath: []string{"resources", "gpu", "count"},
					BadValue:  gpuCount,
					Message:   fmt.Sprintf("%s engine on provider %s requires GPU (set resources.gpu.count > 0)", engineType, providerName),
				})
			}
		case providerName == "" && len(allConfigs) > 0:
			// No provider named: fall back to "does any registered
			// provider declare CPU support for this engine?". With no
			// configs at all we cannot tell — skip the check rather
			// than wrongly rejecting CPU-capable engines like llamacpp.
			if !anyProviderSupportsCPU(allConfigs, engineType) {
				errs = append(errs, CompatibilityError{
					Kind:      ErrGPURequired,
					FieldPath: []string{"resources", "gpu", "count"},
					BadValue:  gpuCount,
					Message:   fmt.Sprintf("%s engine requires GPU (set resources.gpu.count > 0)", engineType),
				})
			}
		}
	}

	return errs
}

// anyProviderSupportsCPU returns true if any provider in the given list
// declares CPU support for the engine type. Capability data is used
// regardless of provider readiness, because validation determines
// whether a spec is intrinsically valid — not whether a provider is
// currently available to serve it.
func anyProviderSupportsCPU(configs []airunwayv1alpha1.InferenceProviderConfig, engine airunwayv1alpha1.EngineType) bool {
	for _, pc := range configs {
		if pc.Spec.Capabilities == nil {
			continue
		}
		if pc.Spec.Capabilities.SupportsCPU(engine) {
			return true
		}
	}
	return false
}
