/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

// celEnvOnce lazily initializes a shared CEL environment for evaluating selection rules.
// The environment is safe to share across goroutines since it only declares the "spec" variable.
var (
	celEnvOnce sync.Once
	celEnvInst *cel.Env
	celEnvErr  error
)

func getCELEnv() (*cel.Env, error) {
	celEnvOnce.Do(func() {
		celEnvInst, celEnvErr = cel.NewEnv(
			cel.Variable("spec", cel.DynType),
		)
	})
	return celEnvInst, celEnvErr
}

// runSelectionAlgorithm implements the provider selection algorithm
func (r *ModelDeploymentReconciler) runSelectionAlgorithm(md *airunwayv1alpha1.ModelDeployment, providers []airunwayv1alpha1.InferenceProviderConfig, engineType airunwayv1alpha1.EngineType, servingMode airunwayv1alpha1.ServingMode) (string, string, error) {
	spec := &md.Spec

	// Determine GPU requirements
	hasGPU := false
	if spec.Resources != nil && spec.Resources.GPU != nil && spec.Resources.GPU.Count > 0 {
		hasGPU = true
	}
	if spec.Serving != nil && spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		hasGPU = true
	}

	// Convert spec to map for CEL evaluation.
	specMap, err := specToMap(spec)
	if err != nil {
		return "", "", fmt.Errorf("failed to convert spec for CEL evaluation: %w", err)
	}

	// Overlay the resolved engine type so CEL rules like `spec.engine.type == 'vllm'`
	// see the auto-selected engine even though md.Spec was never mutated.
	if engineType != "" {
		engineMap, _ := specMap["engine"].(map[string]any)
		if engineMap == nil {
			engineMap = map[string]any{}
			specMap["engine"] = engineMap
		}
		if t, _ := engineMap["type"].(string); t == "" {
			engineMap["type"] = string(engineType)
		}
	}

	// Build candidate list with scores
	type candidate struct {
		name     string
		reason   string
		priority int32
	}
	var candidates []candidate

	for _, pc := range providers {
		caps := pc.Spec.Capabilities
		if caps == nil {
			continue
		}

		// Check engine support and get per-engine capabilities
		engineCap := caps.GetEngineCapability(engineType)
		if engineCap == nil {
			continue
		}

		// Check GPU/CPU support for this specific engine
		if hasGPU && !engineCap.GPUSupport {
			continue
		}
		if !hasGPU && !engineCap.CPUSupport {
			continue
		}

		// Check serving mode support for this specific engine
		if !engineCap.SupportsServingMode(servingMode) {
			continue
		}

		// This provider is compatible
		// Evaluate CEL selection rules to calculate priority
		priority := int32(0)
		for _, rule := range pc.Spec.SelectionRules {
			matched, err := evaluateCEL(rule.Condition, specMap)
			if err != nil {
				continue // skip rules that fail to evaluate
			}
			if matched && rule.Priority > priority {
				priority = rule.Priority
			}
		}

		reason := fmt.Sprintf("matched capabilities: engine=%s, gpu=%v, mode=%s", engineType, hasGPU, servingMode)
		candidates = append(candidates, candidate{
			name:     pc.Name,
			reason:   reason,
			priority: priority,
		})
	}

	if len(candidates) == 0 {
		return "", "", nil
	}

	// Select highest priority candidate; use name as stable tiebreaker
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.priority > best.priority || (c.priority == best.priority && c.name < best.name) {
			best = c
		}
	}

	return best.name, best.reason, nil
}

// specToMap converts a ModelDeploymentSpec to a map for CEL evaluation
func specToMap(spec *airunwayv1alpha1.ModelDeploymentSpec) (map[string]any, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal spec: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("failed to unmarshal spec: %w", err)
	}
	return m, nil
}

// evaluateCEL evaluates a CEL expression against the spec map
func evaluateCEL(expression string, specMap map[string]any) (bool, error) {
	env, err := getCELEnv()
	if err != nil {
		return false, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("failed to compile CEL expression %q: %w", expression, issues.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return false, fmt.Errorf("failed to create CEL program: %w", err)
	}

	out, _, err := prg.Eval(map[string]any{
		"spec": specMap,
	})
	if err != nil {
		return false, fmt.Errorf("failed to evaluate CEL expression: %w", err)
	}

	if out.Type() != types.BoolType {
		return false, fmt.Errorf("CEL expression did not return bool, got %s", out.Type())
	}

	return out.Value().(bool), nil
}
