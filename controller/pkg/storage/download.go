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
	"crypto/sha256"
	"fmt"
	"maps"
	"strings"

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

// DefaultDownloadJobImage is the build-injected default container image for
// model download Jobs. Release binaries embed an immutable digest. Source
// builds intentionally leave it empty so they cannot silently launch a
// mutable image; callers may provide an explicit runtime override instead.
var DefaultDownloadJobImage = ""

const (
	// downloadJobSuffix is the suffix appended to the ModelDeployment name to form the Job name
	downloadJobSuffix = "-model-download"
	// downloadJobInputHashAnnotation ties Job completion to the exact model-cache PVC identity and inputs.
	downloadJobInputHashAnnotation = "airunway.ai/download-input-hash"

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
	if md.Spec.Model.Source != "" && md.Spec.Model.Source != airunwayv1alpha1.ModelSourceHuggingFace {
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

// deleteDownloadJob removes a controller-owned Job and waits for its pods to terminate
// before a replacement workload can proceed.
func deleteDownloadJob(ctx context.Context, c client.Client, job *batchv1.Job, reason string) error {
	logger := log.FromContext(ctx)
	logger.Info("Deleting model download Job", "name", job.Name, "reason", reason)
	propagation := metav1.DeletePropagationForeground
	if err := c.Delete(ctx, job, &client.DeleteOptions{
		PropagationPolicy: &propagation,
	}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete download Job %s: %w", job.Name, err)
	}
	return nil
}

func downloadJobInputHash(
	md *airunwayv1alpha1.ModelDeployment,
	vol *airunwayv1alpha1.StorageVolume,
	downloadJobImage string,
	pvcUID types.UID,
) string {
	huggingFaceToken := ""
	if md.Spec.Secrets != nil {
		huggingFaceToken = md.Spec.Secrets.HuggingFaceToken
	}
	parts := []string{
		md.Spec.Model.ID,
		string(md.Spec.Model.Source),
		vol.Name,
		vol.ResolvedClaimName(md.Name),
		VolumeMountPath(*vol),
		downloadJobImage,
		huggingFaceToken,
		string(pvcUID),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%x", sum)
}

func resolveDownloadJobInputHash(
	ctx context.Context,
	c client.Client,
	md *airunwayv1alpha1.ModelDeployment,
	vol *airunwayv1alpha1.StorageVolume,
	downloadJobImage string,
) (string, error) {
	claimName := vol.ResolvedClaimName(md.Name)
	pvc := &corev1.PersistentVolumeClaim{}
	err := c.Get(ctx, types.NamespacedName{Name: claimName, Namespace: md.Namespace}, pvc)
	if err != nil && !errors.IsNotFound(err) {
		return "", fmt.Errorf("failed to get model-cache PVC %s for download Job identity: %w", claimName, err)
	}

	pvcUID := types.UID("")
	if err == nil {
		pvcUID = pvc.UID
	}
	return downloadJobInputHash(md, vol, downloadJobImage, pvcUID), nil
}

// EnsureDownloadJob ensures a model download Job exists and tracks its completion.
// Returns completed=true when the Job has succeeded.
func EnsureDownloadJob(
	ctx context.Context,
	c client.Client,
	md *airunwayv1alpha1.ModelDeployment,
	downloadJobImage string,
) (bool, error) {
	logger := log.FromContext(ctx)

	vol := findModelCacheVolume(md)
	if vol == nil {
		return true, nil // nothing to do
	}
	if downloadJobImage == "" {
		return false, fmt.Errorf(
			"model downloader image is not configured; set --model-downloader-image on the controller " +
				"or --download-job-image on the Dynamo provider, or inject storage.DefaultDownloadJobImage at build time",
		)
	}

	inputHash, err := resolveDownloadJobInputHash(ctx, c, md, vol, downloadJobImage)
	if err != nil {
		return false, err
	}

	jobName := downloadJobName(md.Name)

	// Check if Job already exists
	existing := &batchv1.Job{}
	err = c.Get(ctx, types.NamespacedName{
		Name:      jobName,
		Namespace: md.Namespace,
	}, existing)

	if errors.IsNotFound(err) {
		// Create the download Job
		job := buildDownloadJob(md, vol, downloadJobImage, inputHash)
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

	// A name collision is only safe to delete when the owner reference proves
	// that the Job belongs to an older ModelDeployment with this same name.
	if !IsOwnedByMD(existing, md.UID) {
		if !isOwnedByPriorMD(existing, md) {
			return false, fmt.Errorf(
				"download Job %s already exists but is not owned by this ModelDeployment "+
					"or a prior ModelDeployment with the same name; refusing to delete it",
				jobName,
			)
		}
		if err := deleteDownloadJob(ctx, c, existing, "owner UID mismatch"); err != nil {
			return false, err
		}
		return false, nil // requeue → next reconcile creates fresh Job
	}

	if !existing.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if existing.Annotations[downloadJobInputHashAnnotation] != inputHash {
		if err := deleteDownloadJob(ctx, c, existing, "model-cache inputs changed"); err != nil {
			return false, err
		}
		return false, nil // requeue → next reconcile creates a Job for the current PVC/input identity
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

// EnsureDownloadJobAbsent removes a current or provably prior-owned download
// Job when the desired storage configuration no longer requires one. Foreign
// Jobs are preserved.
func EnsureDownloadJobAbsent(
	ctx context.Context,
	c client.Client,
	md *airunwayv1alpha1.ModelDeployment,
) (bool, error) {
	jobName := downloadJobName(md.Name)
	existing := &batchv1.Job{}
	err := c.Get(ctx, types.NamespacedName{Name: jobName, Namespace: md.Namespace}, existing)
	if errors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get download Job %s: %w", jobName, err)
	}
	if !IsOwnedByMD(existing, md.UID) &&
		!isOwnedByPriorMD(existing, md) {
		log.FromContext(ctx).Info("Preserving download Job not owned by this ModelDeployment", "name", jobName)
		return true, nil
	}
	if !existing.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if err := deleteDownloadJob(ctx, c, existing, "download no longer required"); err != nil {
		return false, err
	}
	return false, nil
}

// buildDownloadJob creates a batch Job that downloads a HuggingFace model.
func buildDownloadJob(
	md *airunwayv1alpha1.ModelDeployment,
	vol *airunwayv1alpha1.StorageVolume,
	downloadJobImage string,
	inputHash string,
) *batchv1.Job {
	claimName := vol.ResolvedClaimName(md.Name)
	mountPath := VolumeMountPath(*vol)
	backoffLimit := defaultBackoffLimit
	completions := int32(1)
	parallelism := int32(1)
	var nodeSelector map[string]string
	if len(md.Spec.NodeSelector) > 0 {
		nodeSelector = maps.Clone(md.Spec.NodeSelector)
	}
	var tolerations []corev1.Toleration
	if len(md.Spec.Tolerations) > 0 {
		tolerations = make([]corev1.Toleration, len(md.Spec.Tolerations))
		for i := range md.Spec.Tolerations {
			md.Spec.Tolerations[i].DeepCopyInto(&tolerations[i])
		}
	}

	envVars := []corev1.EnvVar{
		{
			Name:  "HF_HOME",
			Value: mountPath,
		},
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      downloadJobName(md.Name),
			Namespace: md.Namespace,
			Annotations: map[string]string{
				downloadJobInputHashAnnotation: inputHash,
			},
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
					NodeSelector:  nodeSelector,
					Tolerations:   tolerations,
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
									MountPath: mountPath,
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
