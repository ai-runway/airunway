/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"github.com/kaito-project/airunway/controller/internal/validation"
)

type providerCompatibilityValidationResult struct {
	Warnings       admission.Warnings
	Errors         field.ErrorList
	IsDynamoMocker bool
}

func validateProviderCompatibility(
	ctx context.Context,
	obj *airunwayv1alpha1.ModelDeployment,
	specPath *field.Path,
	servingMode airunwayv1alpha1.ServingMode,
	reader client.Reader,
	apiReader client.Reader,
) providerCompatibilityValidationResult {
	var result providerCompatibilityValidationResult
	spec := &obj.Spec
	result.IsDynamoMocker = isDynamoMockerDeployment(obj)

	// The Dynamo mocker backend only simulates the vLLM engine. Enforce the
	// vLLM-only constraint at admission so a non-vllm engine + mocker annotation
	// is rejected here rather than admitted and failing later during provider
	// reconciliation (the dynamo provider re-validates this too). An empty engine
	// type is allowed — the provider defaults it to vllm.
	if result.IsDynamoMocker && spec.Engine.Type != "" && spec.Engine.Type != airunwayv1alpha1.EngineTypeVLLM {
		result.Errors = append(result.Errors, field.Invalid(
			specPath.Child("engine", "type"),
			spec.Engine.Type,
			"the dynamo mocker test backend only supports the vllm engine",
		))
		return result
	}

	if result.IsDynamoMocker || spec.Provider == nil || spec.Provider.Name == "" || spec.Engine.Type == "" || reader == nil {
		return result
	}

	var providerConfig airunwayv1alpha1.InferenceProviderConfig
	err := reader.Get(ctx, client.ObjectKey{Name: spec.Provider.Name}, &providerConfig)
	if apierrors.IsNotFound(err) && apiReader != nil {
		// Cache may be stale for a just-created provider; confirm against
		// the API server before we tell the user the provider doesn't
		// exist. Any error from the fallback is preserved verbatim so
		// the existing switch below classifies it the same way it would
		// have under the old all-APIReader path.
		err = apiReader.Get(ctx, client.ObjectKey{Name: spec.Provider.Name}, &providerConfig)
	}

	switch {
	case apierrors.IsNotFound(err):
		// Reject obviously-bogus provider names at admission time so the
		// user gets immediate feedback rather than waiting for reconcile.
		result.Errors = append(result.Errors, field.Invalid(
			specPath.Child("provider", "name"),
			spec.Provider.Name,
			fmt.Sprintf("InferenceProviderConfig %q not found", spec.Provider.Name),
		))
	case meta.IsNoMatchError(err):
		// CRD is not installed (cluster mid-bootstrap). Skip — the
		// controller will catch this during reconciliation.
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		// Webhook RBAC is misconfigured (e.g. ServiceAccount missing
		// `get` on InferenceProviderConfig). Do NOT silently skip
		// validation — that would mask a serious misconfiguration and
		// disable admission-time enforcement cluster-wide. Surface it
		// as an InternalError so the apiserver rejects admission with
		// an actionable diagnostic.
		result.Errors = append(result.Errors, field.InternalError(
			specPath.Child("provider", "name"),
			fmt.Errorf("cannot verify provider %q: %w", spec.Provider.Name, err),
		))
	case err != nil:
		// Transient API error (timeout, connection refused, etc.). Do not
		// block admission on infra flakes — log and skip so the controller
		// can re-validate later.
		logf.FromContext(ctx).Info(
			"failed to look up InferenceProviderConfig for webhook validation; skipping provider/engine compatibility check",
			"provider", spec.Provider.Name,
			"error", err.Error(),
		)
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"could not verify provider %q compatibility at admission time (%v); the controller will re-validate during reconciliation",
			spec.Provider.Name, err,
		))
	case providerConfig.Spec.Capabilities != nil:
		gpuCount := int32(0)
		if spec.Resources != nil && spec.Resources.GPU != nil {
			gpuCount = spec.Resources.GPU.Count
		}
		for _, ce := range validation.CheckProviderCompatibility(
			spec.Provider.Name,
			&providerConfig,
			nil,
			spec.Engine.Type,
			servingMode,
			gpuCount,
		) {
			fp := specPath
			for _, seg := range ce.FieldPath {
				fp = fp.Child(seg)
			}
			result.Errors = append(result.Errors, field.Invalid(fp, ce.BadValue, ce.Message))
		}
	}

	return result
}

// isDynamoMockerDeployment identifies the test-only Dynamo mocker backend
// escape hatch. Keep the annotation key literal here to avoid importing the
// provider module from the controller webhook (see providers/dynamo/mocker.go
// AnnotationDynamoTestBackend / DynamoTestBackendMocker).
func isDynamoMockerDeployment(obj *airunwayv1alpha1.ModelDeployment) bool {
	return obj.Annotations["airunway.ai/dynamo-test-backend"] == "mocker" &&
		obj.Spec.Provider != nil && obj.Spec.Provider.Name == "dynamo"
}
