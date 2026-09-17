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

package dynamo

import (
	"context"
	"fmt"
	"time"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// dgdrNameLabel links generated resources without owner references.
	dgdrNameLabel = "dgdr.nvidia.com/name"
	// dgdrNamespaceLabel disambiguates requests observed cluster-wide.
	dgdrNamespaceLabel = "dgdr.nvidia.com/namespace"
	// dgdrPendingMessage keeps initial status stable across reconciliation paths.
	dgdrPendingMessage = "Dynamo deployment request is pending"
)

// ensureIntentGatewayDisabled persists the first-PR contract even when admission defaulting was bypassed.
func (r *DynamoProviderReconciler) ensureIntentGatewayDisabled(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
) (bool, error) {
	if md.Spec.Gateway != nil && md.Spec.Gateway.Enabled != nil && !*md.Spec.Gateway.Enabled {
		return false, nil
	}
	original := md.DeepCopy()
	if md.Spec.Gateway == nil {
		md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{}
	}
	md.Spec.Gateway.Enabled = boolPtr(false)
	if err := r.Patch(ctx, md, client.MergeFrom(original)); err != nil {
		return false, fmt.Errorf("disabling gateway integration for DGDR mode: %w", err)
	}
	return true, nil
}

// reconcileIntent recreates immutable DGDRs when rendered intent changes.
func (r *DynamoProviderReconciler) reconcileIntent(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	resources, err := r.Transformer.TransformIntent(ctx, md)
	if err != nil {
		message := fmt.Sprintf("Failed to generate DynamoGraphDeploymentRequest: %s", err)
		return r.failIntent(ctx, md, "TransformFailed", message)
	}
	desired := resources[0]
	result, handled, err := r.ensureIntentResource(ctx, md, desired)
	if err != nil || handled {
		return result, err
	}

	// Provider status points at DGDR while status.dgdName links to serving resources.
	md.Status.Provider.ResourceName = md.Name
	md.Status.Provider.ResourceKind = DynamoGraphDeploymentRequestKind
	r.setCondition(
		md,
		airunwayv1alpha1.ConditionTypeResourceCreated,
		metav1.ConditionTrue,
		"ResourceCreated",
		"DynamoGraphDeploymentRequest created successfully",
	)
	if err := r.syncStatus(ctx, md, desired); err != nil {
		logger.Error(err, "Failed to sync DGDR status", "name", md.Name)
	}
	if md.Status.Phase == "" {
		md.Status.Phase = airunwayv1alpha1.DeploymentPhasePending
		md.Status.Message = dgdrPendingMessage
	}
	if err := r.Status().Update(ctx, md); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueInterval}, nil
}

// ensureIntentResource creates a DGDR or initiates recreation when intent changes.
func (r *DynamoProviderReconciler) ensureIntentResource(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
	desired *unstructured.Unstructured,
) (ctrl.Result, bool, error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Get(
		ctx,
		types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()},
		existing,
	)
	if errors.IsNotFound(err) {
		return r.createIntentResource(ctx, md, desired)
	}
	if meta.IsNoMatchError(err) {
		// Direct DGD remains globally ready, so an intent request must report its own missing-CRD failure.
		message := "DynamoGraphDeploymentRequest API is not installed; " +
			"use direct mode or install DGDR support"
		result, statusErr := r.failIntent(ctx, md, "DGDRUnavailable", message)
		return result, true, statusErr
	}
	if err != nil {
		return ctrl.Result{}, true, fmt.Errorf("getting DynamoGraphDeploymentRequest: %w", err)
	}
	if err := verifyDynamoOwnership(existing, md.UID); err != nil {
		result, statusErr := r.failIntent(ctx, md, "ResourceConflict", err.Error())
		return result, true, statusErr
	}
	if existing.GetAnnotations()[IntentHashAnnotation] ==
		desired.GetAnnotations()[IntentHashAnnotation] {
		return ctrl.Result{}, false, nil
	}
	return r.recreateIntentResource(ctx, md)
}

// createIntentResource removes a direct DGD before creating the same-named DGDR.
func (r *DynamoProviderReconciler) createIntentResource(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
	desired *unstructured.Unstructured,
) (ctrl.Result, bool, error) {
	pending, err := r.deleteDirectDGDForIntent(ctx, md)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if pending {
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}
	log.FromContext(ctx).Info("Creating DynamoGraphDeploymentRequest", "name", desired.GetName())
	if err := r.Create(ctx, desired, strictFieldValidation); err != nil {
		message := fmt.Sprintf("Failed to create DynamoGraphDeploymentRequest: %s", err)
		result, statusErr := r.failIntent(ctx, md, "CreateFailed", message)
		return result, true, statusErr
	}
	return ctrl.Result{}, false, nil
}

// recreateIntentResource deletes immutable intent and records that profiling restarts.
func (r *DynamoProviderReconciler) recreateIntentResource(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
) (ctrl.Result, bool, error) {
	pending, err := r.deleteIntentResources(ctx, md)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
	md.Status.Message = "Deployment intent changed; recreating the request and restarting profiling"
	if err := r.Status().Update(ctx, md); err != nil {
		return ctrl.Result{}, true, err
	}
	if pending {
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}
	return ctrl.Result{Requeue: true}, true, nil
}

// failIntent reports an intent-path failure without reusing DGD-specific user messages.
func (r *DynamoProviderReconciler) failIntent(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
	reason string,
	message string,
) (ctrl.Result, error) {
	r.setCondition(md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, message)
	r.setCondition(md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, reason, message)
	md.Status.Endpoint = nil
	md.Status.Replicas = nil
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
	md.Status.Message = message
	if err := r.Status().Update(ctx, md); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: ExternalRecoveryInterval}, nil
}

// deleteDirectDGDForIntent avoids deleting a DGD already linked to a DGDR.
func (r *DynamoProviderReconciler) deleteDirectDGDForIntent(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
) (bool, error) {
	dgd := newDynamoResource(DynamoAPIVersion, DynamoGraphDeploymentKind)
	err := r.Get(ctx, types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, dgd)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
if dgd.GetLabels()[dgdrNameLabel] != "" {
		if !isDGDLinkedToModelDeployment(dgd, md) {
			return false, &resourceConflictError{namespace: dgd.GetNamespace(), name: dgd.GetName()}
		}
		return false, nil
	}
	if err := verifyDynamoOwnership(dgd, md.UID); err != nil {
		return false, err
	}
	if dgd.GetDeletionTimestamp() == nil {
		if err := r.Delete(ctx, dgd); err != nil && !errors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}

// deleteIntentResources removes both resources because Dynamo does not chain ownership.
func (r *DynamoProviderReconciler) deleteIntentResources(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
) (bool, error) {
	dgdName, dgdrFound, err := r.deleteOwnedDGDR(ctx, md)
	if err != nil {
		return false, err
	}
	intentKnown := dgdrFound ||
		md.Status.Provider != nil && md.Status.Provider.ResourceKind == DynamoGraphDeploymentRequestKind
	if !intentKnown {
		return false, nil
	}
	dgdFound, err := r.deleteLinkedDGD(ctx, md, dgdName)
	if err != nil {
		return false, err
	}
	return dgdrFound || dgdFound, nil
}

// deleteOwnedDGDR deletes the request and returns its authoritative generated DGD name.
func (r *DynamoProviderReconciler) deleteOwnedDGDR(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
) (string, bool, error) {
	dgdr := newDynamoResource(DynamoGraphDeploymentRequestAPIVersion, DynamoGraphDeploymentRequestKind)
	err := r.Get(ctx, types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, dgdr)
	dgdrFound := err == nil
	if err != nil && !upstreamResourceUnavailable(err) {
		return md.Name, false, err
	}

	dgdName := md.Name
	if dgdrFound {
		if err := verifyDynamoOwnership(dgdr, md.UID); err != nil {
			return dgdName, false, err
		}
		// status.dgdName is authoritative when an override changes the generated resource name.
		statusName, found, _ := unstructured.NestedString(dgdr.Object, "status", "dgdName")
		if found && statusName != "" {
			dgdName = statusName
		}
		if dgdr.GetDeletionTimestamp() == nil {
			if err := r.Delete(ctx, dgdr); err != nil && !upstreamResourceUnavailable(err) {
				return dgdName, false, err
			}
		}
	}
	return dgdName, dgdrFound, nil
}

// deleteLinkedDGD removes only a generated DGD with a verified owner or link label.
func (r *DynamoProviderReconciler) deleteLinkedDGD(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
	dgdName string,
) (bool, error) {
	dgd := newDynamoResource(DynamoAPIVersion, DynamoGraphDeploymentKind)
	err := r.Get(ctx, types.NamespacedName{Name: dgdName, Namespace: md.Namespace}, dgd)
	if err == nil {
		if !isDGDLinkedToModelDeployment(dgd, md) {
			return false, &resourceConflictError{namespace: dgd.GetNamespace(), name: dgd.GetName()}
		}
		if dgd.GetDeletionTimestamp() == nil {
			if err := r.Delete(ctx, dgd); err != nil && !upstreamResourceUnavailable(err) {
				return false, err
			}
		}
	} else if !upstreamResourceUnavailable(err) {
		return false, err
	}
	return err == nil, nil
}

// isDGDLinkedToModelDeployment accepts ownership or documented labels, never name alone.
func isDGDLinkedToModelDeployment(
	dgd *unstructured.Unstructured,
	md *airunwayv1alpha1.ModelDeployment,
) bool {
	if verifyDynamoOwnership(dgd, md.UID) == nil {
		return true
	}
	labels := dgd.GetLabels()
	managedByAirunway := labels[airunwayv1alpha1.LabelManagedBy] == "airunway"
	linkedDeployment := labels[airunwayv1alpha1.LabelModelDeployment] == md.Name
	if managedByAirunway && linkedDeployment {
		return true
	}
	dgdrMatches := labels[dgdrNameLabel] == md.Name
	namespaceMatches := labels[dgdrNamespaceLabel] == "" ||
		labels[dgdrNamespaceLabel] == md.Namespace
	return dgdrMatches && namespaceMatches
}

// newDynamoResource creates an unstructured object with the requested upstream GVK.
func newDynamoResource(version, kind string) *unstructured.Unstructured {
	resource := &unstructured.Unstructured{}
	resource.SetGroupVersionKind(schema.GroupVersionKind{Group: DynamoAPIGroup, Version: version, Kind: kind})
	return resource
}

// mapDynamoResourceToModelDeployment resolves owners and label-linked resources.
func mapDynamoResourceToModelDeployment(_ context.Context, obj client.Object) []reconcile.Request {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == airunwayv1alpha1.GroupVersion.String() && ref.Kind == "ModelDeployment" {
			key := types.NamespacedName{Name: ref.Name, Namespace: obj.GetNamespace()}
			return []reconcile.Request{{NamespacedName: key}}
		}
	}
	labels := obj.GetLabels()
	deployment := labels[airunwayv1alpha1.LabelModelDeployment]
	if deployment == "" {
		deployment = labels[dgdrNameLabel]
	}
	if deployment == "" {
		return nil
	}
	namespace := obj.GetNamespace()
	if linkedNamespace := labels[dgdrNamespaceLabel]; linkedNamespace != "" {
		namespace = linkedNamespace
	}
	key := types.NamespacedName{Name: deployment, Namespace: namespace}
	return []reconcile.Request{{NamespacedName: key}}
}
