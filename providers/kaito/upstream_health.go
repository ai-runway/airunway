/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package kaito

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// UpstreamHealth summarises the state of the real KAITO workspace controller.
// The probe is called from both the heartbeat loop (writing InferenceProviderConfig.status)
// and the ModelDeployment reconcile loop (refuse-fast when upstream is broken).
type UpstreamHealth struct {
	Healthy bool
	Reason  string // one of the Reason* constants
	Message string // user-facing, safe to surface in CR status
}

// Reason codes stamped into InferenceProviderConfig.status.conditions[UpstreamReady]
// and into ModelDeployment.status.conditions[Ready] on the refuse-fast path.
const (
	ReasonUpstreamHealthy            = "UpstreamHealthy"
	ReasonCRDMissing                 = "CRDMissing"
	ReasonUpstreamControllerMissing  = "UpstreamControllerMissing"
	ReasonUpstreamControllerNotReady = "UpstreamControllerNotReady"
	ReasonProbeFailed                = "ProbeFailed"
	ReasonUnregistered               = "Unregistered" // stamped by MarkUnregistered on shim shutdown
)

// Well-known resource selectors the probe looks up.
const (
	kaitoDeploymentSelectorKey    = "app.kubernetes.io/name"
	kaitoDeploymentSelectorValue  = "workspace"
	controllerMissingUserMessage  = "The KAITO workspace controller is not running. Install KAITO with `helm install kaito-workspace kaito/workspace`. If this cluster was provisioned with `--enable-ai-toolchain-operator`, disable the AKS extension first (`az aks update --disable-ai-toolchain-operator ...`)."
	controllerNotReadyUserMessage = "The KAITO workspace controller Deployment %s/%s exists but has no ready replicas."
	crdMissingUserMessage         = "KAITO Workspace CRD not found. Install KAITO."
)

// probeUpstreamController checks whether the upstream kaito-workspace controller
// is installed and running. The caller is responsible for applying a bounded
// timeout (e.g. context.WithTimeout(ctx, 10*time.Second)) and for passing an
// uncached direct client — NOT the manager's cached client. The function
// performs a handful of direct API calls per invocation and does not rely on
// informer caches.
//
// Probe order:
//  1. Detect CRD presence via a RESTMapper lookup for kaito.sh/Workspace
//     (NoKindMatchError ⇒ CRD missing).
//  2. Find the controller Deployment by label and check ReadyReplicas.
//  3. Any unexpected API error returns Reason=ProbeFailed.
func probeUpstreamController(ctx context.Context, direct client.Client) UpstreamHealth {
	// Step 1: Detect CRD presence via the REST mapper.
	workspaceGVK := schema.GroupVersionKind{
		Group:   "kaito.sh",
		Version: "v1beta1",
		Kind:    "Workspace",
	}
	_, err := direct.RESTMapper().RESTMapping(workspaceGVK.GroupKind())
	if isNoKindMatch(err) {
		return UpstreamHealth{
			Healthy: false,
			Reason:  ReasonCRDMissing,
			Message: crdMissingUserMessage,
		}
	}
	if err != nil {
		return UpstreamHealth{
			Healthy: false,
			Reason:  ReasonProbeFailed,
			Message: fmt.Sprintf("check workspace crd: %v", err),
		}
	}

	// Step 2: Find the controller Deployment by label.
	deploy, found, err := listWorkspaceController(ctx, direct)
	if err != nil {
		return UpstreamHealth{
			Healthy: false,
			Reason:  ReasonProbeFailed,
			Message: err.Error(),
		}
	}
	if !found {
		return UpstreamHealth{
			Healthy: false,
			Reason:  ReasonUpstreamControllerMissing,
			Message: controllerMissingUserMessage,
		}
	}

	// Step 3: Deployment found — healthy if ReadyReplicas > 0, otherwise NotReady.
	if deploy.Status.ReadyReplicas > 0 {
		return UpstreamHealth{
			Healthy: true,
			Reason:  ReasonUpstreamHealthy,
			Message: fmt.Sprintf("KAITO workspace controller %s/%s is ready", deploy.Namespace, deploy.Name),
		}
	}
	return UpstreamHealth{
		Healthy: false,
		Reason:  ReasonUpstreamControllerNotReady,
		Message: fmt.Sprintf(controllerNotReadyUserMessage, deploy.Namespace, deploy.Name),
	}
}

// isNoKindMatch returns true when err (possibly wrapped) indicates that the
// REST mapper has no mapping for the queried kind — i.e. the CRD is not
// installed in the cluster.
func isNoKindMatch(err error) bool {
	if err == nil {
		return false
	}
	var nkm *meta.NoKindMatchError
	return errors.As(err, &nkm)
}

// listWorkspaceController returns the first Deployment matching the KAITO
// workspace controller label selector. It also returns a second return value
// indicating whether any Deployment with the selector was found (so callers
// can distinguish "missing" from "not ready").
func listWorkspaceController(ctx context.Context, direct client.Client) (*appsv1.Deployment, bool, error) {
	list := &appsv1.DeploymentList{}
	if err := direct.List(ctx, list, client.MatchingLabels{kaitoDeploymentSelectorKey: kaitoDeploymentSelectorValue}); err != nil {
		return nil, false, fmt.Errorf("list deployments: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, false, nil
	}
	// Prefer a ready one; otherwise return the first item so the caller can
	// reference the namespace/name in the message.
	for i := range list.Items {
		d := &list.Items[i]
		if d.Status.ReadyReplicas > 0 {
			return d, true, nil
		}
	}
	return &list.Items[0], true, nil
}
