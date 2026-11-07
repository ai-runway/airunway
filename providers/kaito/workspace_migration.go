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

package kaito

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	migrationStateManager    = "kaito-provider-migration"
	migrationStateAnnotation = "airunway.ai/kaito-migration-state"
	migrationStateVersion    = "captured-v1"
)

// The API's Apply ownership distinguishes this capture from arbitrary legacy
// annotations written by Create/Update, even under identical manager names.
// The version field and verified owner reference retain capture evidence when
// an Update writer changes user annotations. Neither names nor hashes
// authenticate arbitrary API actors.
type workspaceMigrationState struct {
	Managers             []string `json:"managers"`
	PreviousFieldsSHA256 string   `json:"previousFieldsSHA256,omitempty"`
}

func ownsMigrationAnnotation(fields map[string]any, key string) bool {
	_, found, err := unstructured.NestedFieldNoCopy(fields, "f:metadata", "f:annotations", "f:"+key)
	return err == nil && found
}

func migrationStateError(live *unstructured.Unstructured, reason string) error {
	return fmt.Errorf("invalid Workspace %s/%s migration managers annotation: %s",
		live.GetNamespace(), live.GetName(), reason)
}

func readWorkspaceMigrationState(live *unstructured.Unstructured) (*workspaceMigrationState, error) {
	fields, err := managedFieldsForManager(live, migrationStateManager, metav1.ManagedFieldsOperationApply)
	if err != nil || fields == nil {
		return nil, err
	}
	annotations := live.GetAnnotations()
	if !ownsMigrationAnnotation(fields, migrationStateAnnotation) ||
		!ownsMigrationAnnotation(fields, migrationManagersAnnotation) ||
		annotations[migrationStateAnnotation] != migrationStateVersion {
		return nil, migrationStateError(live, "capture field ownership or version changed")
	}
	var state workspaceMigrationState
	if err := json.Unmarshal([]byte(annotations[migrationManagersAnnotation]), &state); err != nil {
		return nil, migrationStateError(live, err.Error())
	}
	if len(state.Managers) == 0 || slices.Contains(state.Managers, "") {
		return nil, migrationStateError(live, "captured managers are empty")
	}
	if state.PreviousFieldsSHA256 != "" {
		digest, err := hex.DecodeString(state.PreviousFieldsSHA256)
		if err != nil || len(digest) != sha256.Size {
			return nil, migrationStateError(live, "invalid fingerprint digest")
		}
	}
	return &state, nil
}

func workspaceFingerprintDigest(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func (s *workspaceMigrationState) fingerprint(live *unstructured.Unstructured) (string, error) {
	annotations := live.GetAnnotations()
	previous, hasCurrent := annotations[lastAppliedWorkspaceAnnotation]
	if s.PreviousFieldsSHA256 == "" {
		if hasCurrent {
			return "", migrationStateError(live, "unexpected last-applied fingerprint after capture")
		}
		return "", nil
	}
	if !hasCurrent {
		var found bool
		previous, found = annotations[migrationPreviousFieldsAnnotation]
		if !found {
			return "", migrationStateError(live, "captured fingerprint disappeared")
		}
	}
	if workspaceFingerprintDigest(previous) != s.PreviousFieldsSHA256 {
		return "", migrationStateError(live, "captured fingerprint changed or disappeared")
	}
	return previous, nil
}

func pendingMigrationManagers(live *unstructured.Unstructured) (map[string]struct{}, bool, error) {
	managers := map[string]struct{}{}
	state, err := readWorkspaceMigrationState(live)
	if err != nil || state == nil {
		return managers, state != nil, err
	}
	if _, err := state.fingerprint(live); err != nil {
		return nil, true, err
	}
	for _, manager := range state.Managers {
		managers[manager] = struct{}{}
	}
	return managers, true, nil
}

// Preparation removes only untrusted reserved annotations. It leaves the real
// last-applied fingerprint and legacy identity ownership available if capture
// fails or the process restarts before its first Apply succeeds.
func (r *KaitoProviderReconciler) prepareWorkspaceMigration(
	ctx context.Context, live *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	keys := []string{migrationStateAnnotation, migrationManagersAnnotation, migrationPreviousFieldsAnnotation}
	for _, entry := range live.GetManagedFields() {
		if entry.Operation != metav1.ManagedFieldsOperationApply || entry.Subresource != "" {
			continue
		}
		fields, err := managedFieldsForManager(live, entry.Manager, metav1.ManagedFieldsOperationApply)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			if ownsMigrationAnnotation(fields, key) {
				message := fmt.Sprintf("Workspace migration annotation %s is owned by Apply manager %q", key, entry.Manager)
				causes := []metav1.StatusCause{{Type: metav1.CauseTypeFieldManagerConflict, Message: message}}
				return nil, apierrors.NewApplyConflict(causes, message)
			}
		}
	}
	updated := live.DeepCopy()
	annotations := copyStringMap(live.GetAnnotations())
	for _, key := range keys {
		delete(annotations, key)
	}
	updated.SetAnnotations(annotations)
	return r.patchWorkspaceMigrationMetadata(ctx, live, updated)
}

// Persist the selected managers before moving discovery clues. The capture is
// small: it hashes the original fingerprint instead of duplicating it near the
// Kubernetes annotation-size limit. Every write uses the observed version.
func (r *KaitoProviderReconciler) markLegacyWorkspaceMigration(
	ctx context.Context, live *unstructured.Unstructured, ownerUID types.UID, managers map[string]struct{},
) (*unstructured.Unstructured, error) {
	if err := verifyOwnerReference(live, ownerUID); err != nil {
		return nil, err
	}
	if _, err := lastAppliedWorkspaceConfiguration(live); err != nil {
		return nil, err
	}
	state := workspaceMigrationState{Managers: make([]string, 0, len(managers))}
	for manager := range managers {
		state.Managers = append(state.Managers, manager)
	}
	slices.Sort(state.Managers)
	if fingerprint, found := live.GetAnnotations()[lastAppliedWorkspaceAnnotation]; found {
		state.PreviousFieldsSHA256 = workspaceFingerprintDigest(fingerprint)
	}
	record, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	prepared, err := r.prepareWorkspaceMigration(ctx, live)
	if err != nil {
		return nil, err
	}
	capture := workspaceConfigurationWithIdentity(map[string]any{}, prepared)
	// A legacy renderer can overwrite every user annotation. Share only the
	// already-verified owner reference so its unchanged value retains capture
	// evidence even if both annotation fields are overwritten or removed.
	for _, reference := range live.GetOwnerReferences() {
		if reference.UID == ownerUID {
			capture.SetOwnerReferences([]metav1.OwnerReference{reference})
			break
		}
	}
	capture.SetAnnotations(map[string]string{
		migrationStateAnnotation:    migrationStateVersion,
		migrationManagersAnnotation: string(record),
	})
	captured, err := r.applyWorkspaceAs(ctx, capture, migrationStateManager, prepared.GetResourceVersion())
	if err != nil {
		return nil, fmt.Errorf("failed to mark legacy Workspace migration: %w", err)
	}
	return r.moveWorkspaceMigrationFingerprint(ctx, captured)
}

func (r *KaitoProviderReconciler) moveWorkspaceMigrationFingerprint(
	ctx context.Context, live *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	state, err := readWorkspaceMigrationState(live)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, migrationStateError(live, "capture is missing")
	}
	previous, err := state.fingerprint(live)
	if err != nil {
		return nil, err
	}
	updated := live.DeepCopy()
	annotations := copyStringMap(live.GetAnnotations())
	delete(annotations, lastAppliedWorkspaceAnnotation)
	delete(annotations, migrationPreviousFieldsAnnotation)
	if state.PreviousFieldsSHA256 != "" {
		annotations[migrationPreviousFieldsAnnotation] = previous
	}
	updated.SetAnnotations(annotations)
	return r.patchWorkspaceMigrationMetadata(ctx, live, updated)
}

func (r *KaitoProviderReconciler) patchWorkspaceMigrationMetadata(
	ctx context.Context, live, updated *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	if equality.Semantic.DeepEqual(live.Object, updated.Object) {
		return live, nil
	}
	if err := r.Patch(
		ctx, updated,
		client.MergeFromWithOptions(live.DeepCopy(), client.MergeFromWithOptimisticLock{}),
		client.FieldOwner(FieldManager), strictFieldValidation,
	); err != nil {
		return nil, fmt.Errorf("failed to mark legacy Workspace %s/%s migration: %w",
			live.GetNamespace(), live.GetName(), err)
	}
	return updated, nil
}

// Keep the capture's owner-reference value until its ownership is retired.
// Kubernetes treats each keyed owner reference atomically, so changing even
// an optional flag while capture shares it would conflict. The final desired
// Apply follows cleanup, still respecting every unrelated field owner.
func workspaceConfigurationDuringMigration(desired, live *unstructured.Unstructured) *unstructured.Unstructured {
	configuration := withoutLastAppliedWorkspaceAnnotation(desired)
	references := configuration.GetOwnerReferences()
	for i, reference := range references {
		for _, existing := range live.GetOwnerReferences() {
			if existing.UID == reference.UID {
				references[i] = existing
				break
			}
		}
	}
	configuration.SetOwnerReferences(references)
	return configuration
}
