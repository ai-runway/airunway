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

package storage

import (
	"context"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PreparationStage identifies the first storage prerequisite that is not ready.
type PreparationStage string

const (
	// PreparationReady means claims are usable and any required model download finished.
	PreparationReady PreparationStage = "ready"
	// PreparationPVCsPending means at least one claim is not ready for a consumer yet.
	PreparationPVCsPending PreparationStage = "pvcsPending"
	// PreparationDownloadPending means the model download Job has not completed yet.
	PreparationDownloadPending PreparationStage = "downloadPending"
)

// Prepare ensures the same-namespace claims and optional HuggingFace download Job
// required by a ModelDeployment. Provider reconcilers use the returned stage to
// keep inference workloads from starting before their model data is available.
func Prepare(
	ctx context.Context,
	c client.Client,
	md *airunwayv1alpha1.ModelDeployment,
	downloadJobImage string,
) (PreparationStage, error) {
	ready, err := EnsurePVCs(ctx, c, md)
	if err != nil {
		return PreparationPVCsPending, err
	}
	if !ready {
		return PreparationPVCsPending, nil
	}

	if !NeedsDownloadJob(md) {
		absent, cleanupErr := EnsureDownloadJobAbsent(ctx, c, md)
		if cleanupErr != nil {
			return PreparationDownloadPending, cleanupErr
		}
		if !absent {
			return PreparationDownloadPending, nil
		}
		return PreparationReady, nil
	}
	if downloadJobImage == "" {
		downloadJobImage = DefaultDownloadJobImage
	}

	completed, err := EnsureDownloadJob(ctx, c, md, downloadJobImage)
	if err != nil {
		return PreparationDownloadPending, err
	}
	if !completed {
		return PreparationDownloadPending, nil
	}

	return PreparationReady, nil
}

// WorkloadReady reports whether the current ModelDeployment generation has
// completed the storage phases required before an inference workload starts.
// Storage-free deployments remain ready for backward compatibility.
func WorkloadReady(md *airunwayv1alpha1.ModelDeployment) bool {
	if !HasStorageVolumes(md) {
		return true
	}

	storageCondition := apiMeta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeStorageReady)
	if !conditionIsCurrentAndTrue(storageCondition, md.Generation) {
		return false
	}

	if !NeedsDownloadJob(md) {
		return true
	}
	downloadCondition := apiMeta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeModelDownloaded)
	return conditionIsCurrentAndTrue(downloadCondition, md.Generation)
}

// HasPreparationConditions reports whether storage lifecycle status remains on the deployment.
func HasPreparationConditions(md *airunwayv1alpha1.ModelDeployment) bool {
	return apiMeta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeStorageReady) != nil ||
		apiMeta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeModelDownloaded) != nil
}

// PrunePreparationConditions removes conditions that no longer apply to the desired storage state.
func PrunePreparationConditions(md *airunwayv1alpha1.ModelDeployment) bool {
	changed := false
	if !HasStorageVolumes(md) {
		changed = apiMeta.RemoveStatusCondition(&md.Status.Conditions, airunwayv1alpha1.ConditionTypeStorageReady) || changed
	}
	if !NeedsDownloadJob(md) {
		changed = apiMeta.RemoveStatusCondition(
			&md.Status.Conditions,
			airunwayv1alpha1.ConditionTypeModelDownloaded,
		) || changed
	}
	return changed
}

func conditionIsCurrentAndTrue(condition *metav1.Condition, generation int64) bool {
	return condition != nil &&
		condition.Status == metav1.ConditionTrue &&
		condition.ObservedGeneration == generation
}
