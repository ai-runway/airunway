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
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNeedsDownloadJob(t *testing.T) {
	tests := []struct {
		name string
		md   *airunwayv1alpha1.ModelDeployment
		want bool
	}{
		{
			name: "huggingface with modelCache volume",
			md:   newDownloadMD("test", "default"),
			want: true,
		},
		{
			name: "custom source",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{
						Source: airunwayv1alpha1.ModelSourceCustom,
						Storage: &airunwayv1alpha1.StorageSpec{
							Volumes: []airunwayv1alpha1.StorageVolume{
								{Name: "cache", Purpose: airunwayv1alpha1.VolumePurposeModelCache},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "huggingface without modelCache volume",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{
						Source: airunwayv1alpha1.ModelSourceHuggingFace,
						Storage: &airunwayv1alpha1.StorageSpec{
							Volumes: []airunwayv1alpha1.StorageVolume{
								{Name: "custom", Purpose: airunwayv1alpha1.VolumePurposeCustom},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "huggingface without storage",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{
						Source: airunwayv1alpha1.ModelSourceHuggingFace,
					},
				},
			},
			want: false,
		},
		{
			name: "huggingface with readOnly modelCache volume",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{
						Source: airunwayv1alpha1.ModelSourceHuggingFace,
						Storage: &airunwayv1alpha1.StorageSpec{
							Volumes: []airunwayv1alpha1.StorageVolume{
								{Name: "cache", Purpose: airunwayv1alpha1.VolumePurposeModelCache, ReadOnly: true, ClaimName: "my-pvc"},
							},
						},
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NeedsDownloadJob(tt.md)
			if got != tt.want {
				t.Errorf("NeedsDownloadJob() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsureDownloadJobCreation(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := newDownloadMD("my-model", "default")
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	completed, err := EnsureDownloadJob(context.Background(), c, md, DefaultDownloadJobImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed {
		t.Error("expected completed=false after creating Job")
	}

	// Verify Job was created
	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{
		Name:      "my-model-model-download",
		Namespace: "default",
	}, job)
	if err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}

	// Verify Job spec
	if job.Spec.Template.Spec.Containers[0].Image != DefaultDownloadJobImage {
		t.Errorf("expected image %s, got %s", DefaultDownloadJobImage, job.Spec.Template.Spec.Containers[0].Image)
	}
	expectedBackoffLimit := int32(6)
	if job.Spec.BackoffLimit == nil {
		t.Fatal("expected backoff limit to be set")
	}
	if *job.Spec.BackoffLimit != expectedBackoffLimit {
		t.Errorf("expected backoff limit %d, got %d", expectedBackoffLimit, *job.Spec.BackoffLimit)
	}

	// Verify args use exec form (not shell) to prevent injection.
	// The container image has ENTRYPOINT ["hf"], so Args appends to it.
	container := job.Spec.Template.Spec.Containers[0]
	expectedArgs := []string{"download", "meta-llama/Llama-2-7b-chat-hf"}
	if len(container.Args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, container.Args)
	}
	for i, arg := range expectedArgs {
		if container.Args[i] != arg {
			t.Errorf("expected args[%d]=%s, got %s", i, arg, container.Args[i])
		}
	}
	if len(container.Command) != 0 {
		t.Errorf("expected no command override (using ENTRYPOINT), got %v", container.Command)
	}

	// Verify env vars (MODEL_NAME should NOT be present — model ID is passed directly in Args)
	foundHFHome := false
	for _, env := range container.Env {
		switch env.Name {
		case "MODEL_NAME":
			t.Error("MODEL_NAME env var should not be set — model ID is passed directly in Args")
		case "HF_HOME":
			foundHFHome = true
			if env.Value != "/model-cache" {
				t.Errorf("expected HF_HOME=/model-cache, got %s", env.Value)
			}
		}
	}
	if !foundHFHome {
		t.Error("expected HF_HOME env var")
	}

	// Verify volume mount
	if len(container.VolumeMounts) != 1 {
		t.Fatalf("expected 1 volume mount, got %d", len(container.VolumeMounts))
	}
	if container.VolumeMounts[0].MountPath != "/model-cache" {
		t.Errorf("expected mount path /model-cache, got %s", container.VolumeMounts[0].MountPath)
	}

	// Verify resource requests and limits
	expectedCPURequest := resource.MustParse("500m")
	if cpuReq, ok := container.Resources.Requests[corev1.ResourceCPU]; !ok {
		t.Error("expected CPU request to be set")
	} else if !cpuReq.Equal(expectedCPURequest) {
		t.Errorf("expected CPU request %s, got %s", expectedCPURequest.String(), cpuReq.String())
	}

	expectedMemoryRequest := resource.MustParse("2Gi")
	if memReq, ok := container.Resources.Requests[corev1.ResourceMemory]; !ok {
		t.Error("expected memory request to be set")
	} else if !memReq.Equal(expectedMemoryRequest) {
		t.Errorf("expected memory request %s, got %s", expectedMemoryRequest.String(), memReq.String())
	}

	expectedMemoryLimit := resource.MustParse("16Gi")
	if memLim, ok := container.Resources.Limits[corev1.ResourceMemory]; !ok {
		t.Error("expected memory limit to be set")
	} else if !memLim.Equal(expectedMemoryLimit) {
		t.Errorf("expected memory limit %s, got %s", expectedMemoryLimit.String(), memLim.String())
	}

	// Verify PVC volume
	if len(job.Spec.Template.Spec.Volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(job.Spec.Template.Spec.Volumes))
	}
	if job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "my-model-model-cache" {
		t.Errorf("expected PVC claim name my-model-model-cache, got %s",
			job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	}

	// Verify labels
	if job.Labels[airunwayv1alpha1.LabelJobType] != "model-download" {
		t.Error("expected job-type label")
	}
	if job.Labels[airunwayv1alpha1.LabelManagedBy] != "airunway" {
		t.Error("expected managed-by label")
	}

	// Verify owner reference
	if len(job.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(job.OwnerReferences))
	}
	if job.OwnerReferences[0].BlockOwnerDeletion == nil || !*job.OwnerReferences[0].BlockOwnerDeletion {
		t.Errorf("expected OwnerReference BlockOwnerDeletion to be true")
	}

	// Verify no envFrom (no HF token secret configured)
	if len(container.EnvFrom) != 0 {
		t.Errorf("expected no envFrom when no HF token secret, got %d", len(container.EnvFrom))
	}
}

func TestEnsureDownloadJobWithHFToken(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := newDownloadMD("my-model", "default")
	md.Spec.Secrets = &airunwayv1alpha1.SecretsSpec{
		HuggingFaceToken: "hf-token-secret",
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := EnsureDownloadJob(context.Background(), c, md, DefaultDownloadJobImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify Job was created with envFrom
	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{
		Name:      "my-model-model-download",
		Namespace: "default",
	}, job)
	if err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if len(container.EnvFrom) != 1 {
		t.Fatalf("expected 1 envFrom, got %d", len(container.EnvFrom))
	}
	if container.EnvFrom[0].SecretRef.Name != "hf-token-secret" {
		t.Errorf("expected secret ref hf-token-secret, got %s", container.EnvFrom[0].SecretRef.Name)
	}
}

func TestEnsureDownloadJobCompleted(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := newDownloadMD("my-model", "default")

	// Pre-create a completed Job
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-download",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "test-uid",
				},
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
			Conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobComplete,
					Status:  corev1.ConditionTrue,
					Message: "Job completed successfully",
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingJob).WithStatusSubresource(existingJob).Build()

	completed, err := EnsureDownloadJob(context.Background(), c, md, DefaultDownloadJobImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !completed {
		t.Error("expected completed=true for succeeded Job")
	}
}

func TestEnsureDownloadJobStillRunning(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := newDownloadMD("my-model", "default")

	// Pre-create a running Job
	backoffLimit := int32(3)
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-download",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "test-uid",
				},
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
		},
		Status: batchv1.JobStatus{
			Active: 1,
			Failed: 1,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingJob).WithStatusSubresource(existingJob).Build()

	completed, err := EnsureDownloadJob(context.Background(), c, md, DefaultDownloadJobImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed {
		t.Error("expected completed=false for running Job")
	}
}

func TestEnsureDownloadJobFailed(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := newDownloadMD("my-model", "default")

	// Pre-create a permanently failed Job
	backoffLimit := int32(3)
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-download",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "test-uid",
				},
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
		},
		Status: batchv1.JobStatus{
			Failed: 4, // exceeds backoffLimit of 3
			Conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingJob).WithStatusSubresource(existingJob).Build()

	_, err := EnsureDownloadJob(context.Background(), c, md, DefaultDownloadJobImage)
	if err == nil {
		t.Fatal("expected error for permanently failed Job")
	}
	if !strings.Contains(err.Error(), "failed permanently") {
		t.Errorf("expected permanent failure error, got: %v", err)
	}
}

func TestEnsureDownloadJobFailedByConditionOnly(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := newDownloadMD("my-model", "default")

	// Job failed via activeDeadlineSeconds: JobFailed condition is set,
	// but Failed count is below the backoff limit.
	backoffLimit := int32(3)
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-download",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "test-uid",
				},
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
		},
		Status: batchv1.JobStatus{
			Failed: 1, // below backoffLimit
			Conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Message: "Job was active longer than specified deadline",
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingJob).WithStatusSubresource(existingJob).Build()

	_, err := EnsureDownloadJob(context.Background(), c, md, DefaultDownloadJobImage)
	if err == nil {
		t.Fatal("expected error for Job failed by condition (activeDeadlineSeconds)")
	}
	if !strings.Contains(err.Error(), "failed permanently") {
		t.Errorf("expected permanent failure error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "active longer than specified deadline") {
		t.Errorf("expected condition message in error, got: %v", err)
	}
}

func TestEnsureDownloadJobFailedAtBackoffLimit(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := newDownloadMD("my-model", "default")

	// Job with Failed == BackoffLimit but no condition set yet.
	// The >= fallback should catch this.
	backoffLimit := int32(3)
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-download",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "test-uid",
				},
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
		},
		Status: batchv1.JobStatus{
			Failed: 3, // exactly at backoffLimit, no conditions
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingJob).WithStatusSubresource(existingJob).Build()

	_, err := EnsureDownloadJob(context.Background(), c, md, DefaultDownloadJobImage)
	if err == nil {
		t.Fatal("expected error when Failed == BackoffLimit (fallback detection)")
	}
	if !strings.Contains(err.Error(), "failed permanently") {
		t.Errorf("expected permanent failure error, got: %v", err)
	}
}

func TestDeleteManagedJobs(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
	}

	// Create a managed Job with matching OwnerReference
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-download",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "my-model",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "test-uid",
				},
			},
		},
	}
	// Create an unrelated Job
	otherJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-model-download",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "other-model",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job, otherJob).Build()

	err := DeleteManagedJobs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify our Job was deleted
	got := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-download", Namespace: "default"}, got)
	if err == nil {
		t.Error("expected managed Job to be deleted")
	}

	// Verify unrelated Job still exists
	err = c.Get(context.Background(), types.NamespacedName{Name: "other-model-download", Namespace: "default"}, got)
	if err != nil {
		t.Error("expected unrelated Job to still exist")
	}
}

func TestDeleteManagedJobsSkipsNonOwned(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("new-uid"),
		},
	}

	// Create a Job with correct labels but OwnerReference pointing to a different UID
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-download",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "my-model",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "old-uid",
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).Build()

	err := DeleteManagedJobs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify Job was NOT deleted (UID mismatch)
	got := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-download", Namespace: "default"}, got)
	if err != nil {
		t.Error("expected Job with mismatched UID to NOT be deleted")
	}
}

func TestDownloadJobName(t *testing.T) {
	if downloadJobName("my-model") != "my-model-model-download" {
		t.Errorf("unexpected job name: %s", downloadJobName("my-model"))
	}
}

func TestEnsureDownloadJobStaleCompleted(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := newDownloadMD("my-model", "default")

	// Pre-create a completed Job owned by a previous ModelDeployment (different UID)
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-download",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "old-uid",
				},
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingJob).WithStatusSubresource(existingJob).Build()

	completed, err := EnsureDownloadJob(context.Background(), c, md, DefaultDownloadJobImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed {
		t.Error("expected completed=false when stale Job is deleted")
	}

	// Verify stale Job was deleted
	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-download", Namespace: "default"}, job)
	if err == nil {
		t.Error("expected stale completed Job to be deleted")
	}
}

func TestEnsureDownloadJobStaleFailed(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := newDownloadMD("my-model", "default")

	// Pre-create a failed Job owned by a previous ModelDeployment
	backoffLimit := int32(3)
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-download",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "old-uid",
				},
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
		},
		Status: batchv1.JobStatus{
			Failed: 4,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingJob).WithStatusSubresource(existingJob).Build()

	completed, err := EnsureDownloadJob(context.Background(), c, md, DefaultDownloadJobImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed {
		t.Error("expected completed=false when stale Job is deleted")
	}

	// Verify stale Job was deleted
	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-download", Namespace: "default"}, job)
	if err == nil {
		t.Error("expected stale failed Job to be deleted")
	}
}

func TestEnsureDownloadJobStaleRunning(t *testing.T) {
	scheme := newScheme()
	_ = batchv1.AddToScheme(scheme)

	md := newDownloadMD("my-model", "default")

	// Pre-create a running Job owned by a previous ModelDeployment
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-download",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "old-uid",
				},
			},
		},
		Status: batchv1.JobStatus{
			Active: 1,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingJob).WithStatusSubresource(existingJob).Build()

	completed, err := EnsureDownloadJob(context.Background(), c, md, DefaultDownloadJobImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed {
		t.Error("expected completed=false when stale Job is deleted")
	}

	// Verify stale Job was deleted
	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-download", Namespace: "default"}, job)
	if err == nil {
		t.Error("expected stale running Job to be deleted")
	}
}

func TestIsOwnedByMD(t *testing.T) {
	tests := []struct {
		name   string
		job    *batchv1.Job
		mdUID  types.UID
		expect bool
	}{
		{
			name: "matching UID",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{UID: "abc-123"},
					},
				},
			},
			mdUID:  "abc-123",
			expect: true,
		},
		{
			name: "wrong UID",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{UID: "old-uid"},
					},
				},
			},
			mdUID:  "new-uid",
			expect: false,
		},
		{
			name: "no owner references",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{},
			},
			mdUID:  "abc-123",
			expect: false,
		},
		{
			name: "multiple refs with match",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{UID: "other-uid"},
						{UID: "abc-123"},
					},
				},
			},
			mdUID:  "abc-123",
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsOwnedByMD(tt.job, tt.mdUID)
			if got != tt.expect {
				t.Errorf("IsOwnedByMD() = %v, want %v", got, tt.expect)
			}
		})
	}
}
