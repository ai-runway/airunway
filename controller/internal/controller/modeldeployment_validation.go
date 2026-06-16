/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"github.com/kaito-project/airunway/controller/internal/validation"
)

func validateModelDeploymentSpec(
	md *airunwayv1alpha1.ModelDeployment,
	providerConfigs []airunwayv1alpha1.InferenceProviderConfig,
	engineType airunwayv1alpha1.EngineType,
	servingMode airunwayv1alpha1.ServingMode,
) error {
	spec := &md.Spec

	// Validate model.id is required for huggingface source
	if spec.Model.Source == airunwayv1alpha1.ModelSourceHuggingFace || spec.Model.Source == "" {
		if spec.Model.ID == "" {
			return fmt.Errorf("model.id is required when source is huggingface")
		}
	}

	if engineType == "" {
		return fmt.Errorf("engine.type must be specified or auto-selected from provider capabilities")
	}

	// Mocker mode escape hatch: a ModelDeployment annotated with
	// airunway.ai/dynamo-test-backend=mocker targeting the dynamo provider runs
	// the GPU-less python3 -m dynamo.mocker backend, so the GPU compatibility and
	// disaggregated gpu.count checks below must not reject it. This mirrors the
	// admission webhook so the two cannot drift. Mocker is vLLM-only.
	isDynamoMocker := isDynamoMockerMode(md)
	if isDynamoMocker && engineType != airunwayv1alpha1.EngineTypeVLLM {
		return fmt.Errorf("the dynamo mocker test backend only supports the vllm engine")
	}

	// Validate provider/engine/serving-mode/GPU-CPU compatibility via the
	// shared helper so the webhook and reconciler cannot drift.
	gpuCount := int32(0)
	if spec.Resources != nil && spec.Resources.GPU != nil {
		gpuCount = spec.Resources.GPU.Count
	}
	providerName := ""
	var namedConfig *airunwayv1alpha1.InferenceProviderConfig
	if spec.Provider != nil {
		providerName = spec.Provider.Name
		for i := range providerConfigs {
			if providerConfigs[i].Name == providerName {
				namedConfig = &providerConfigs[i]
				break
			}
		}
	}
	if !isDynamoMocker {
		if ces := validation.CheckProviderCompatibility(
			providerName,
			namedConfig,
			providerConfigs,
			engineType,
			servingMode,
			gpuCount,
		); len(ces) > 0 {
			// Return the first error to preserve the reconciler's existing
			// single-error contract.
			return fmt.Errorf("%s", ces[0].Message)
		}
	}

	return validateDisaggregatedScalingForReconciler(spec, isDynamoMocker)
}

func validateDisaggregatedScalingForReconciler(spec *airunwayv1alpha1.ModelDeploymentSpec, isDynamoMocker bool) error {
	if spec.Serving == nil || spec.Serving.Mode != airunwayv1alpha1.ServingModeDisaggregated {
		return nil
	}

	// Cannot specify resources.gpu in disaggregated mode
	if spec.Resources != nil && spec.Resources.GPU != nil && spec.Resources.GPU.Count > 0 {
		return fmt.Errorf("cannot specify both resources.gpu and scaling.prefill/decode in disaggregated mode")
	}

	// Must specify prefill and decode
	if spec.Scaling == nil || spec.Scaling.Prefill == nil || spec.Scaling.Decode == nil {
		return fmt.Errorf("disaggregated mode requires scaling.prefill and scaling.decode")
	}

	// The GPU-less mocker backend waives the per-component gpu.count
	// requirement, but the prefill/decode blocks themselves are still
	// required (above) so the dynamo transformer can build both workers.
	if isDynamoMocker {
		return nil
	}

	// Prefill must have GPU
	if spec.Scaling.Prefill.GPU == nil || spec.Scaling.Prefill.GPU.Count == 0 {
		return fmt.Errorf("disaggregated mode requires scaling.prefill.gpu.count > 0")
	}

	// Decode must have GPU
	if spec.Scaling.Decode.GPU == nil || spec.Scaling.Decode.GPU.Count == 0 {
		return fmt.Errorf("disaggregated mode requires scaling.decode.gpu.count > 0")
	}

	return nil
}

// validateSpec performs validation on the ModelDeployment spec.
func (r *ModelDeploymentReconciler) validateSpec(_ context.Context, md *airunwayv1alpha1.ModelDeployment, providerConfigs []airunwayv1alpha1.InferenceProviderConfig, engineType airunwayv1alpha1.EngineType, servingMode airunwayv1alpha1.ServingMode) error {
	return validateModelDeploymentSpec(md, providerConfigs, engineType, servingMode)
}
