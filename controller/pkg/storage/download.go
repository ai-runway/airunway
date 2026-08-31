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
	"fmt"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// DefaultDownloadJobImage is the default container image for model download jobs.
	// This image has huggingface_hub (with hf_xet) pre-installed.
	DefaultDownloadJobImage = "ghcr.io/ai-runway/airunway/model-downloader:latest"

	// downloadJobSuffix is the suffix appended to the ModelDeployment name to form the Job name
	downloadJobSuffix = "-model-download"

	// defaultBackoffLimit is the number of retries for the download Job
	defaultBackoffLimit int32 = 6

	// Resource defaults for the download Job container.
	// The download job uses hf_xet (chunk-based Xet storage) for fast downloads.
	// Memory needs scale with model size — large models (70B+) with many shards
	// can require several GiB for concurrent chunk assembly and hash verification.
	defaultDownloadJobCPURequest    = "500m"
	defaultDownloadJobMemoryRequest = "2Gi"
	defaultDownloadJobMemoryLimit   = "16Gi"
)

// NeedsDownloadJob returns true when a model download Job should be created:
// - Model source is huggingface
// - A volume with purpose=modelCache exists
// - The modelCache volume is not readOnly (readOnly implies pre-populated data)
func NeedsDownloadJob(md *airunwayv1alpha1.ModelDeployment) bool {
	if md.Spec.Model.Source != airunwayv1alpha1.ModelSourceHuggingFace {
		return false
	}
	vol := findModelCacheVolume(md)
	if vol == nil {
		return false
	}
	// readOnly modelCache means the model is pre-populated — no download needed
	if vol.ReadOnly {
		return false
	}
	return true
}

// findModelCacheVolume returns the first volume with purpose=modelCache, or nil.
func findModelCacheVolume(md *airunwayv1alpha1.ModelDeployment) *airunwayv1alpha1.StorageVolume {
	if md.Spec.Model.Storage == nil {
		return nil
	}
	for i, vol := range md.Spec.Model.Storage.Volumes {
		if vol.Purpose == airunwayv1alpha1.VolumePurposeModelCache {
			return &md.Spec.Model.Storage.Volumes[i]
		}
	}
	return nil
}

// deleteStaleJob deletes a Job that belongs to a previous (now-deleted) ModelDeployment.
// Uses background propagation (matching DeleteManagedJobs) and tolerates NotFound
// in case GC already removed it.
func deleteStaleJob(ctx context.Context, c client.Client, job *batchv1.Job) error {
	logger := log.FromContext(ctx)
	logger.Info("Deleting stale download Job (owner UID mismatch)", "name", job.Name)
	propagation := metav1.DeletePropagationBackground
	if err := c.Delete(ctx, job, &client.DeleteOptions{
		PropagationPolicy: &propagation,
	}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete stale download Job %s: %w", job.Name, err)
	}
	return nil
}

// EnsureDownloadJob ensures a model download Job exists and tracks its completion.
// Returns completed=true when the Job has succeeded.
func EnsureDownloadJob(ctx context.Context, c client.Client, md *airunwayv1alpha1.ModelDeployment, downloadJobImage string) (bool, error) {
	logger := log.FromContext(ctx)

	vol := findModelCacheVolume(md)
	if vol == nil {
		return true, nil // nothing to do
	}

	jobName := downloadJobName(md.Name)

	// Check if Job already exists
	existing := &batchv1.Job{}
	err := c.Get(ctx, types.NamespacedName{
		Name:      jobName,
		Namespace: md.Namespace,
	}, existing)

	if errors.IsNotFound(err) {
		// Create the download Job
		job := buildDownloadJob(md, vol, downloadJobImage)
		logger.Info("Creating model download Job", "name", jobName, "model", md.Spec.Model.ID)
		if createErr := c.Create(ctx, job); createErr != nil {
			if !errors.IsAlreadyExists(createErr) {
				return false, fmt.Errorf("failed to create download Job %s: %w", jobName, createErr)
			}
			logger.Info("Download Job already exists (concurrent creation)", "name", jobName)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get download Job %s: %w", jobName, err)
	}

	// Verify the existing Job is owned by this ModelDeployment (same UID).
	// If a ModelDeployment is deleted and recreated with the same name, there's a
	// race window where the old Job still exists. Delete it and requeue so the
	// next reconcile creates a fresh Job.
	if !IsOwnedByMD(existing, md.UID) {
		if err := deleteStaleJob(ctx, c, existing); err != nil {
			return false, err
		}
		return false, nil // requeue → next reconcile creates fresh Job
	}

	// Job exists — check conditions (authoritative) then counters (fallback).
	for _, cond := range existing.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			logger.Info("Model download Job completed", "name", jobName)
			return true, nil
		case batchv1.JobFailed:
			return false, fmt.Errorf("model download Job %s failed permanently: %s",
				jobName, cond.Message)
		}
	}

	// Fallback: counter-based detection for older clusters or edge cases
	// where conditions haven't been set yet.
	if existing.Status.Succeeded >= 1 {
		logger.Info("Model download Job completed (counter)", "name", jobName)
		return true, nil
	}

	backoffLimit := defaultBackoffLimit
	if existing.Spec.BackoffLimit != nil {
		backoffLimit = *existing.Spec.BackoffLimit
	}
	if existing.Status.Failed >= backoffLimit {
		return false, fmt.Errorf("model download Job %s failed permanently (failed=%d, backoffLimit=%d)",
			jobName, existing.Status.Failed, backoffLimit)
	}

	logger.Info("Model download Job still running", "name", jobName,
		"active", existing.Status.Active, "failed", existing.Status.Failed)
	return false, nil
}

// buildDownloadJob creates a batch Job that downloads a HuggingFace model.
func buildDownloadJob(md *airunwayv1alpha1.ModelDeployment, vol *airunwayv1alpha1.StorageVolume, downloadJobImage string) *batchv1.Job {
	claimName := vol.ResolvedClaimName(md.Name)
	backoffLimit := defaultBackoffLimit
	completions := int32(1)
	parallelism := int32(1)

	envVars := []corev1.EnvVar{
		{
			Name:  "HF_HOME",
			Value: vol.MountPath,
		},
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      downloadJobName(md.Name),
			Namespace: md.Namespace,
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: md.Name,
				airunwayv1alpha1.LabelJobType:         "model-download",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         airunwayv1alpha1.GroupVersion.String(),
					Kind:               "ModelDeployment",
					Name:               md.Name,
					UID:                md.UID,
					Controller:         boolPtr(true),
					BlockOwnerDeletion: boolPtr(true),
				},
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Completions:  &completions,
			Parallelism:  &parallelism,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "model-download",
							Image: downloadJobImage,
							Args:  []string{"download", md.Spec.Model.ID},
							Env:   envVars,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(defaultDownloadJobCPURequest),
									corev1.ResourceMemory: resource.MustParse(defaultDownloadJobMemoryRequest),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse(defaultDownloadJobMemoryLimit),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "model-cache",
									MountPath: vol.MountPath,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "model-cache",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: claimName,
								},
							},
						},
					},
				},
			},
		},
	}

	// Add HuggingFace token secret if configured
	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" {
		job.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{
			{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: md.Spec.Secrets.HuggingFaceToken,
					},
				},
			},
		}
	}

	return job
}

// downloadJobName returns the Job name for a ModelDeployment.
func downloadJobName(mdName string) string {
	return mdName + downloadJobSuffix
}

// DeleteManagedJobs deletes all Jobs managed by the given ModelDeployment.
// Only Jobs whose OwnerReference UID matches the ModelDeployment's UID are deleted,
// preventing accidental deletion of Jobs adopted by a recreated ModelDeployment.
func DeleteManagedJobs(ctx context.Context, c client.Client, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	jobList := &batchv1.JobList{}
	if err := c.List(ctx, jobList,
		client.InNamespace(md.Namespace),
		client.MatchingLabels{
			airunwayv1alpha1.LabelManagedBy:       "airunway",
			airunwayv1alpha1.LabelModelDeployment: md.Name,
		},
	); err != nil {
		return fmt.Errorf("failed to list managed Jobs: %w", err)
	}

	propagation := metav1.DeletePropagationBackground
	for i := range jobList.Items {
		job := &jobList.Items[i]
		if !IsOwnedByMD(job, md.UID) {
			logger.Info("Skipping Job not owned by this ModelDeployment", "name", job.Name, "mdUID", md.UID)
			continue
		}
		logger.Info("Deleting managed Job", "name", job.Name)
		if err := c.Delete(ctx, job, &client.DeleteOptions{
			PropagationPolicy: &propagation,
		}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete Job %s: %w", job.Name, err)
		}
	}

	return nil
}
