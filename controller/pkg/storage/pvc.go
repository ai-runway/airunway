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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// HasManagedPVCs returns true if any volume in the ModelDeployment has Size set,
// meaning the controller is responsible for creating PVCs.
func HasManagedPVCs(md *airunwayv1alpha1.ModelDeployment) bool {
	if md.Spec.Model.Storage == nil {
		return false
	}
	for _, vol := range md.Spec.Model.Storage.Volumes {
		if vol.Size != nil {
			return true
		}
	}
	return false
}

// HasStorageVolumes returns true if the ModelDeployment has any storage volumes configured.
func HasStorageVolumes(md *airunwayv1alpha1.ModelDeployment) bool {
	return md.Spec.Model.Storage != nil && len(md.Spec.Model.Storage.Volumes) > 0
}

// ReferencedPVCNames returns the unique claims that must already exist because
// their storage volume does not request controller-managed capacity.
func ReferencedPVCNames(md *airunwayv1alpha1.ModelDeployment) []string {
	if md.Spec.Model.Storage == nil {
		return nil
	}

	seen := make(map[string]struct{}, len(md.Spec.Model.Storage.Volumes))
	references := make([]string, 0, len(md.Spec.Model.Storage.Volumes))
	for i := range md.Spec.Model.Storage.Volumes {
		volume := &md.Spec.Model.Storage.Volumes[i]
		if volume.Size != nil {
			continue
		}
		claimName := volume.ResolvedClaimName(md.Name)
		if claimName == "" {
			continue
		}
		if _, exists := seen[claimName]; exists {
			continue
		}
		seen[claimName] = struct{}{}
		references = append(references, claimName)
	}
	return references
}

// HasTerminatingPVCs checks the configured claims without creating, deleting,
// or otherwise preparing them. It lets reconcilers prioritize consumer
// teardown even when normal validation cannot proceed.
func HasTerminatingPVCs(
	ctx context.Context,
	c client.Client,
	md *airunwayv1alpha1.ModelDeployment,
) (bool, error) {
	if md.Spec.Model.Storage == nil {
		return false, nil
	}

	seen := make(map[string]struct{}, len(md.Spec.Model.Storage.Volumes))
	terminating := false
	var firstErr error
	for i := range md.Spec.Model.Storage.Volumes {
		claimName := md.Spec.Model.Storage.Volumes[i].ResolvedClaimName(md.Name)
		if claimName == "" {
			continue
		}
		if _, exists := seen[claimName]; exists {
			continue
		}
		seen[claimName] = struct{}{}

		pvc := &corev1.PersistentVolumeClaim{}
		err := c.Get(ctx, types.NamespacedName{Name: claimName, Namespace: md.Namespace}, pvc)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to get PVC %s while checking for termination: %w", claimName, err)
			}
			continue
		}
		if !pvc.DeletionTimestamp.IsZero() {
			terminating = true
		}
	}

	return terminating, firstErr
}

// PVCState describes whether the claims required by a ModelDeployment are
// ready for consumers, still being prepared, or waiting for existing
// consumers to release a terminating claim.
type PVCState string

const (
	PVCStateReady       PVCState = "ready"
	PVCStatePending     PVCState = "pending"
	PVCStateTerminating PVCState = "terminating"
)

// EnsurePVCs ensures that all storage volume PVCs exist and are usable.
//
// For managed PVCs (Size is set): returns ready once the PVC has been created,
// even if it is still in Pending phase. This avoids a deadlock with
// WaitForFirstConsumer storage classes, where the PVC won't bind until a Pod
// (such as the model-download Job) references it.
//
// For pre-existing PVCs (Size is nil): returns ready only when the PVC is Bound,
// since these are outside the controller's control.
func EnsurePVCs(ctx context.Context, c client.Client, md *airunwayv1alpha1.ModelDeployment) (bool, error) {
	state, err := EnsurePVCsState(ctx, c, md)
	return state == PVCStateReady, err
}

// EnsurePVCsState is the stateful form of EnsurePVCs. Callers that own pod
// workloads use PVCStateTerminating to stop those consumers before waiting for
// Kubernetes PVC protection to finish deletion.
func EnsurePVCsState(ctx context.Context, c client.Client, md *airunwayv1alpha1.ModelDeployment) (PVCState, error) {
	logger := log.FromContext(ctx)

	if md.Spec.Model.Storage == nil {
		return PVCStateReady, nil
	}

	state := PVCStateReady
	markPending := func() {
		if state != PVCStateTerminating {
			state = PVCStatePending
		}
	}
	var firstErr error
	recordError := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
		markPending()
	}
	for _, vol := range md.Spec.Model.Storage.Volumes {
		if vol.Size == nil {
			// Pre-existing PVC: verify it exists and is usable before proceeding
			claimName := vol.ResolvedClaimName(md.Name)
			existing := &corev1.PersistentVolumeClaim{}
			err := c.Get(ctx, types.NamespacedName{Name: claimName, Namespace: md.Namespace}, existing)
			if errors.IsNotFound(err) {
				recordError(fmt.Errorf(
					"pre-existing PVC %q not found in namespace %q (referenced by volume %q); "+
						"ensure the PVC exists before creating the ModelDeployment",
					claimName, md.Namespace, vol.Name,
				))
				continue
			}
			if err != nil {
				recordError(fmt.Errorf("failed to get pre-existing PVC %s: %w", claimName, err))
				continue
			}
			if !existing.DeletionTimestamp.IsZero() {
				logger.Info("Pre-existing PVC is terminating", "name", claimName)
				state = PVCStateTerminating
				continue
			}
			switch existing.Status.Phase {
			case corev1.ClaimBound:
				logger.Info("Pre-existing PVC is Bound", "name", claimName)
			case corev1.ClaimPending:
				logger.Info("Pre-existing PVC is Pending", "name", claimName)
				markPending()
			case corev1.ClaimLost:
				recordError(fmt.Errorf("pre-existing PVC %q is in Lost phase", claimName))
			default:
				markPending()
			}
			continue
		}

		claimName := vol.ResolvedClaimName(md.Name)

		// Check if PVC already exists
		existing := &corev1.PersistentVolumeClaim{}
		err := c.Get(ctx, types.NamespacedName{
			Name:      claimName,
			Namespace: md.Namespace,
		}, existing)

		if errors.IsNotFound(err) {
			// Create the PVC
			pvc, buildErr := buildPVC(md, &vol)
			if buildErr != nil {
				recordError(fmt.Errorf("failed to build PVC %s: %w", claimName, buildErr))
				continue
			}
			logger.Info("Creating PVC", "name", claimName, "namespace", md.Namespace, "size", vol.Size.String())
			if createErr := c.Create(ctx, pvc); createErr != nil {
				if !errors.IsAlreadyExists(createErr) {
					recordError(fmt.Errorf("failed to create PVC %s: %w", claimName, createErr))
					continue
				}
				logger.Info("PVC already exists (concurrent creation)", "name", claimName)
			}
			markPending()
			continue
		}
		if err != nil {
			recordError(fmt.Errorf("failed to get PVC %s: %w", claimName, err))
			continue
		}

		// Verify the existing PVC is owned by this ModelDeployment (same UID).
		// If a ModelDeployment is deleted and recreated with the same name, there's a
		// race window where the old PVC still exists. Only delete it when its owner
		// reference proves that it belongs to that prior ModelDeployment; labels are
		// not sufficient authority for destructive cleanup.
		if !IsOwnedByMD(existing, md.UID) {
			if !isOwnedByPriorMD(existing, md) {
				recordError(fmt.Errorf(
					"PVC %s already exists but is not owned by this ModelDeployment or a prior ModelDeployment with the same name; "+
						"refusing to delete it — remove the PVC manually or change the volume claimName",
					claimName,
				))
				continue
			}
			if err := deleteStalePVC(ctx, c, existing); err != nil {
				recordError(err)
				continue
			}
			state = PVCStateTerminating
			continue // requeue → next reconcile creates fresh PVC
		}
		if !existing.DeletionTimestamp.IsZero() {
			logger.Info("PVC is terminating", "name", claimName)
			state = PVCStateTerminating
			continue
		}

		// PVC exists and is owned by this MD — check phase.
		// For managed PVCs, Pending is acceptable: the download Job or inference
		// pod will act as the first consumer and trigger WaitForFirstConsumer binding.
		switch existing.Status.Phase {
		case corev1.ClaimBound:
			logger.Info("PVC is Bound", "name", claimName)
		case corev1.ClaimPending:
			logger.Info("PVC is Pending (will bind when a consumer pod is scheduled)", "name", claimName)
			// Don't set allReady=false — proceed to create the download Job
			// which will reference this PVC and trigger binding.
		case corev1.ClaimLost:
			recordError(fmt.Errorf("PVC %s is in Lost phase", claimName))
		default:
			markPending()
		}
	}

	return state, firstErr
}

// buildPVC creates a PVC spec from a StorageVolume with Size set.
func buildPVC(
	md *airunwayv1alpha1.ModelDeployment,
	vol *airunwayv1alpha1.StorageVolume,
) (*corev1.PersistentVolumeClaim, error) {
	if vol.Size == nil {
		return nil, fmt.Errorf("volume size must be set for controller-created PVCs")
	}

	claimName := vol.ResolvedClaimName(md.Name)

	// Use the typed access mode directly; default to ReadWriteMany if empty
	accessMode := vol.AccessMode
	if accessMode == "" {
		accessMode = corev1.ReadWriteMany
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: md.Namespace,
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: md.Name,
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
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *vol.Size,
				},
			},
		},
	}

	// Set storage class name directly (nil→cluster default, ""→no class, "x"→named class)
	pvc.Spec.StorageClassName = vol.StorageClassName

	return pvc, nil
}

// deleteStalePVC deletes a PVC that belongs to a previous (now-deleted) ModelDeployment.
// Tolerates NotFound in case GC already removed it.
func deleteStalePVC(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) error {
	logger := log.FromContext(ctx)
	logger.Info("Deleting stale PVC (owner UID mismatch)", "name", pvc.Name)
	if err := c.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete stale PVC %s: %w", pvc.Name, err)
	}
	return nil
}

// DeleteManagedPVCs deletes all PVCs managed by the given ModelDeployment.
// Only PVCs whose OwnerReference UID matches the ModelDeployment's UID are deleted,
// preventing accidental deletion of PVCs adopted by a recreated ModelDeployment.
func DeleteManagedPVCs(ctx context.Context, c client.Client, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcList,
		client.InNamespace(md.Namespace),
		client.MatchingLabels{
			airunwayv1alpha1.LabelManagedBy:       "airunway",
			airunwayv1alpha1.LabelModelDeployment: md.Name,
		},
	); err != nil {
		return fmt.Errorf("failed to list managed PVCs: %w", err)
	}

	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		if !IsOwnedByMD(pvc, md.UID) {
			logger.Info("Skipping PVC not owned by this ModelDeployment", "name", pvc.Name, "mdUID", md.UID)
			continue
		}
		logger.Info("Deleting managed PVC", "name", pvc.Name)
		if err := c.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete PVC %s: %w", pvc.Name, err)
		}
	}

	return nil
}

// IsOwnedByMD returns true if the object has an OwnerReference whose UID matches mdUID.
func IsOwnedByMD(obj client.Object, mdUID types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == mdUID {
			return true
		}
	}
	return false
}

// isOwnedByPriorMD reports whether an object is provably owned by an older
// ModelDeployment with the same namespace/name. It is used only to authorize
// replacement of generated-name resources after a ModelDeployment is recreated.
func isOwnedByPriorMD(obj client.Object, md *airunwayv1alpha1.ModelDeployment) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == airunwayv1alpha1.GroupVersion.String() &&
			ref.Kind == "ModelDeployment" &&
			ref.Name == md.Name &&
			ref.UID != "" &&
			ref.UID != md.UID {
			return true
		}
	}
	return false
}

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool {
	return &b
}
