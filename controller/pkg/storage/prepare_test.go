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
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPrepareExistingClaim(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-models", UID: types.UID("md-uid")},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{Model: airunwayv1alpha1.ModelSpec{
			Source: airunwayv1alpha1.ModelSourceCustom,
			Storage: &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{
				{Name: "data", ClaimName: "shared-data", MountPath: "/data", Purpose: airunwayv1alpha1.VolumePurposeCustom},
			}},
		}},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-data", Namespace: "team-models"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&corev1.PersistentVolumeClaim{}).
		WithObjects(pvc).
		Build()

	stage, err := Prepare(context.Background(), c, md, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stage != PreparationReady {
		t.Fatalf("expected existing bound claim to be ready, got %s", stage)
	}

	pvc.Status.Phase = corev1.ClaimPending
	if err := c.Status().Update(context.Background(), pvc); err != nil {
		t.Fatalf("updating PVC: %v", err)
	}
	stage, err = Prepare(context.Background(), c, md, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stage != PreparationPVCsPending {
		t.Fatalf("expected existing pending claim to block, got %s", stage)
	}
}

func TestPrepareManagedModelCacheThroughDownload(t *testing.T) {
	ctx := context.Background()
	md := newDownloadMD("demo", "team-models")
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&corev1.PersistentVolumeClaim{}, &batchv1.Job{}).
		Build()

	stage, err := Prepare(ctx, c, md, "example.test/model-downloader:v1")
	if err != nil {
		t.Fatalf("unexpected error creating managed claim: %v", err)
	}
	if stage != PreparationPVCsPending {
		t.Fatalf("expected newly created claim to requeue, got %s", stage)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, types.NamespacedName{Name: "demo-model-cache", Namespace: "team-models"}, pvc); err != nil {
		t.Fatalf("expected managed PVC in ModelDeployment namespace: %v", err)
	}
	if !IsOwnedByMD(pvc, md.UID) {
		t.Fatal("expected managed PVC to be owned by the ModelDeployment")
	}

	pvc.Status.Phase = corev1.ClaimPending
	if err := c.Status().Update(ctx, pvc); err != nil {
		t.Fatalf("updating managed PVC: %v", err)
	}
	stage, err = Prepare(ctx, c, md, "example.test/model-downloader:v1")
	if err != nil {
		t.Fatalf("unexpected error creating download Job: %v", err)
	}
	if stage != PreparationDownloadPending {
		t.Fatalf("expected download stage, got %s", stage)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: "demo-model-download", Namespace: "team-models"}, job); err != nil {
		t.Fatalf("expected download Job in ModelDeployment namespace: %v", err)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != "example.test/model-downloader:v1" {
		t.Fatalf("expected configured download image, got %q", got)
	}

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatalf("updating download Job: %v", err)
	}
	stage, err = Prepare(ctx, c, md, "example.test/model-downloader:v1")
	if err != nil {
		t.Fatalf("unexpected error completing preparation: %v", err)
	}
	if stage != PreparationReady {
		t.Fatalf("expected storage preparation to finish, got %s", stage)
	}
}

func TestPrepareRejectsMissingDownloadImage(t *testing.T) {
	ctx := context.Background()
	md := newDownloadMD("demo", "team-models")
	md.Spec.Model.Storage.Volumes[0].Size = nil
	md.Spec.Model.Storage.Volumes[0].ClaimName = "shared-model-cache"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-model-cache", Namespace: "team-models"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&corev1.PersistentVolumeClaim{}, &batchv1.Job{}).
		WithObjects(pvc).
		Build()

	stage, err := Prepare(ctx, c, md, "")
	if err == nil {
		t.Fatal("expected an unconfigured download image to fail before creating a Job")
	}
	if stage != PreparationDownloadPending {
		t.Fatalf("expected download stage, got %s", stage)
	}
	job := &batchv1.Job{}
	getErr := c.Get(ctx, types.NamespacedName{Name: "demo-model-download", Namespace: "team-models"}, job)
	if getErr == nil {
		t.Fatal("expected no download Job with an empty image")
	}
}

func TestPrunePreparationConditions(t *testing.T) {
	newConditions := func() []metav1.Condition {
		return []metav1.Condition{
			{Type: airunwayv1alpha1.ConditionTypeStorageReady, Status: metav1.ConditionTrue},
			{Type: airunwayv1alpha1.ConditionTypeModelDownloaded, Status: metav1.ConditionTrue},
		}
	}

	t.Run("removes all storage conditions when storage is absent", func(t *testing.T) {
		md := &airunwayv1alpha1.ModelDeployment{Status: airunwayv1alpha1.ModelDeploymentStatus{Conditions: newConditions()}}
		if !HasPreparationConditions(md) {
			t.Fatal("expected preparation conditions before pruning")
		}
		if !PrunePreparationConditions(md) || len(md.Status.Conditions) != 0 {
			t.Fatalf("expected both conditions removed, got %#v", md.Status.Conditions)
		}
	})

	t.Run("keeps storage readiness but removes download state for compilation cache only", func(t *testing.T) {
		md := &airunwayv1alpha1.ModelDeployment{
			Spec: airunwayv1alpha1.ModelDeploymentSpec{Model: airunwayv1alpha1.ModelSpec{
				Storage: &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{
					Name: "compile", Purpose: airunwayv1alpha1.VolumePurposeCompilationCache,
				}}},
			}},
			Status: airunwayv1alpha1.ModelDeploymentStatus{Conditions: newConditions()},
		}
		if !PrunePreparationConditions(md) {
			t.Fatal("expected obsolete download condition to be pruned")
		}
		if apiMeta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeStorageReady) == nil {
			t.Fatal("expected StorageReady to remain")
		}
		if apiMeta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeModelDownloaded) != nil {
			t.Fatal("expected ModelDownloaded to be removed")
		}
	})

	t.Run("keeps both conditions for a writable Hugging Face model cache", func(t *testing.T) {
		md := newDownloadMD("demo", "default")
		md.Status.Conditions = newConditions()
		if PrunePreparationConditions(md) {
			t.Fatalf("expected no pruning, got %#v", md.Status.Conditions)
		}
	})
}
