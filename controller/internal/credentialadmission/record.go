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

package credentialadmission

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

const (
	RecordLabel = "agents.airunway.ai/credential-admission-record"

	recordLabelValue                = "true"
	recordTargetNamespaceAnnotation = "agents.airunway.ai/credential-admission-target-namespace"
	recordLedgerNamePrefix          = "airunway-credential-admission-"
	recordEntryPrefix               = "name-"
	recordPersistenceAttempts       = 8
	maxRecordEntryBytes             = 2 * 1024
	// One initial CREATE and one distinct server-UID retry may be pending for
	// the same name. Re-delivery of either request is idempotent by entry key.
	maxPendingRecordsPerName = 2

	// DefaultMaxPendingRecords is deliberately low enough that even maximum-
	// sized entries remain comfortably below the Kubernetes ConfigMap size
	// limit. Each target namespace has an independent fixed ledger, so a tenant
	// can fail closed only its own namespace rather than exhausting a global
	// admission quota. Optimistic resourceVersion patches make the bound hold
	// across concurrent webhook replicas.
	DefaultMaxPendingRecords        = 256
	DefaultRecordCleanupInterval    = 5 * time.Minute
	DefaultRecordCleanupGracePeriod = 10 * time.Minute
)

type createRecord struct {
	TargetNamespace string    `json:"targetNamespace"`
	TargetName      string    `json:"targetName"`
	TargetUID       types.UID `json:"targetUID"`
	Proof           string    `json:"proof"`
	CreatedAt       time.Time `json:"createdAt"`
}

// RecordStore persists the CREATE-only bridge between validating admission,
// which sees the API-server-assigned UID/generation but cannot mutate the
// object, and reconciliation, which converts that proof into the ordinary
// UID/generation-bound annotation. UPDATE does not use records because
// mutating admission can sign the trusted old object's UID directly.
type RecordStore struct {
	Attestor          *Attestor
	Reader            client.Reader
	Writer            client.Client
	Namespace         string
	MaxPendingRecords int
}

func NewRecordStore(attestor *Attestor, reader client.Reader, writer client.Client, namespace string) (*RecordStore, error) {
	if attestor == nil {
		return nil, fmt.Errorf("credential admission record store requires an attestor")
	}
	if reader == nil || writer == nil {
		return nil, fmt.Errorf("credential admission record store requires a reader and writer")
	}
	if namespace == "" {
		return nil, fmt.Errorf("credential admission record store requires a namespace")
	}
	return &RecordStore{
		Attestor:          attestor,
		Reader:            reader,
		Writer:            writer,
		Namespace:         namespace,
		MaxPendingRecords: DefaultMaxPendingRecords,
	}, nil
}

// PersistCreate records a proof only after validating admission has authorized
// the CREATE request. The object has its final name, UID, and generation at
// this stage, unlike during mutating admission.
func (s *RecordStore) PersistCreate(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) error {
	if s == nil {
		return fmt.Errorf("credential admission record store is not configured")
	}
	if ad == nil {
		return fmt.Errorf("credential admission CREATE record requires an AgentDeployment")
	}
	if err := s.Attestor.CheckKey(ctx); err != nil {
		return fmt.Errorf("credential admission signing key is unavailable: %w", err)
	}
	if err := s.rejectConflictingLiveTarget(ctx, ad); err != nil {
		return err
	}
	proof, err := s.Attestor.sign(ad, ad.UID, ad.Generation)
	if err != nil {
		return err
	}
	key, err := RecordKey(s.Namespace, ad.Namespace)
	if err != nil {
		return err
	}
	entryKey := recordEntryKey(ad.Name, ad.UID)
	entry, err := encodeCreateRecord(ad, proof, time.Now().UTC())
	if err != nil {
		return err
	}
	if len(entry) > maxRecordEntryBytes {
		return fmt.Errorf("credential admission CREATE record for %s/%s is %d bytes; maximum is %d",
			ad.Namespace, ad.Name, len(entry), maxRecordEntryBytes)
	}
	limit := s.MaxPendingRecords
	if limit <= 0 {
		limit = DefaultMaxPendingRecords
	}

	for attempt := 0; attempt < recordPersistenceAttempts; attempt++ {
		var ledger corev1.ConfigMap
		err := s.Reader.Get(ctx, key, &ledger)
		if apierrors.IsNotFound(err) {
			created := desiredRecordLedger(s.Namespace, ad.Namespace, entryKey, entry)
			if createErr := s.Writer.Create(ctx, created); createErr != nil {
				// A request timeout or lost response does not prove that the API
				// server rejected the Create. Recover only when an authoritative
				// read contains this exact AgentDeployment incarnation and proof;
				// otherwise preserve the Create error (or its existing retry path).
				if s.createRecordEntryPersisted(ctx, key, ad, entryKey, proof) {
					return nil
				}
				if apierrors.IsAlreadyExists(createErr) {
					continue
				}
				return fmt.Errorf("create credential admission record ledger %s: %w", key, createErr)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read credential admission record ledger %s: %w", key, err)
		}
		if err := validateRecordLedger(&ledger, key, ad.Namespace); err != nil {
			return err
		}
		if existing, found := ledger.Data[entryKey]; found {
			record, decodeErr := decodeCreateRecord(existing)
			if decodeErr != nil {
				return fmt.Errorf("credential admission record ledger %s contains malformed entry for %s/%s: %w",
					key, ad.Namespace, ad.Name, decodeErr)
			}
			if err := verifyRecordIdentity(&record, ad); err != nil || record.Proof != proof {
				return fmt.Errorf("credential admission record ledger %s contains an invalid pending CREATE entry for %s/%s UID %s",
					key, ad.Namespace, ad.Name, ad.UID)
			}
			return nil
		}
		if pendingRecordCountForName(ledger.Data, ad.Namespace, ad.Name) >= maxPendingRecordsPerName {
			return fmt.Errorf("credential admission record ledger %s already has %d pending CREATE attempts for %s/%s; retry after the winning CREATE is stored or pending records are cleaned",
				key, maxPendingRecordsPerName, ad.Namespace, ad.Name)
		}
		if len(ledger.Data) >= limit {
			return fmt.Errorf("credential admission record ledger %s is at its %d-record capacity; retry after pending CREATE records are reconciled or cleaned",
				key, limit)
		}

		base := ledger.DeepCopy()
		if ledger.Data == nil {
			ledger.Data = make(map[string]string)
		}
		ledger.Data[entryKey] = entry
		if patchErr := s.Writer.Patch(ctx, &ledger,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); patchErr != nil {
			// Merge patches can likewise commit before their response is lost.
			// Never accept a different UID or proof that happened to appear at
			// the predictable ledger key while recovering that ambiguity.
			if s.createRecordEntryPersisted(ctx, key, ad, entryKey, proof) {
				return nil
			}
			if apierrors.IsConflict(patchErr) || apierrors.IsNotFound(patchErr) {
				continue
			}
			return fmt.Errorf("persist credential admission record in ledger %s: %w", key, patchErr)
		}
		return nil
	}

	return fmt.Errorf("could not persist credential admission record in ledger %s after %d attempts", key, recordPersistenceAttempts)
}

// createRecordEntryPersisted authoritatively resolves an ambiguous ConfigMap
// Create/Patch response. Verification failures deliberately collapse to false:
// the original write error is the useful failure to return, and a recovery read
// must never replace or obscure it.
func (s *RecordStore) createRecordEntryPersisted(
	ctx context.Context,
	key types.NamespacedName,
	ad *airunwayv1alpha1.AgentDeployment,
	entryKey string,
	proof string,
) bool {
	var ledger corev1.ConfigMap
	if err := s.Reader.Get(ctx, key, &ledger); err != nil {
		return false
	}
	if err := validateRecordLedger(&ledger, key, ad.Namespace); err != nil {
		return false
	}
	raw, found := ledger.Data[entryKey]
	if !found {
		return false
	}
	record, err := decodeCreateRecord(raw)
	if err != nil {
		return false
	}
	return verifyRecordIdentity(&record, ad) == nil && record.Proof == proof
}

func (s *RecordStore) rejectConflictingLiveTarget(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) error {
	var existing airunwayv1alpha1.AgentDeployment
	err := s.Reader.Get(ctx, client.ObjectKeyFromObject(ad), &existing)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check AgentDeployment %s/%s before persisting its credential admission record: %w",
			ad.Namespace, ad.Name, err)
	}
	if existing.UID == ad.UID {
		return nil
	}
	return apierrors.NewAlreadyExists(
		airunwayv1alpha1.GroupVersion.WithResource("agentdeployments").GroupResource(),
		ad.Name,
	)
}

func pendingRecordCountForName(data map[string]string, namespace, name string) int {
	count := 0
	for _, raw := range data {
		record, err := decodeCreateRecord(raw)
		if err == nil && record.TargetNamespace == namespace && record.TargetName == name {
			count++
		}
	}
	return count
}

// VerifyOrFinalize accepts an existing UID/generation-bound annotation, or
// consumes the matching CREATE record and atomically patches that proof onto
// the persisted AgentDeployment before credential resolution continues.
func (s *RecordStore) VerifyOrFinalize(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) error {
	if s == nil {
		return fmt.Errorf("credential admission record store is not configured")
	}
	if err := s.Attestor.Verify(ctx, ad); err == nil {
		s.deleteRecordBestEffort(ctx, ad)
		return nil
	}

	key, err := RecordKey(s.Namespace, ad.Namespace)
	if err != nil {
		return err
	}
	var ledger corev1.ConfigMap
	if err := s.Reader.Get(ctx, key, &ledger); err != nil {
		return fmt.Errorf("AgentDeployment has no verifiable credential admission proof: read CREATE record ledger %s: %w", key, err)
	}
	if err := validateRecordLedger(&ledger, key, ad.Namespace); err != nil {
		return err
	}
	_, raw, found := findRecordEntry(ledger.Data, ad)
	if !found {
		return fmt.Errorf("AgentDeployment has no verifiable credential admission proof in CREATE record ledger %s", key)
	}
	record, err := decodeCreateRecord(raw)
	if err != nil {
		return fmt.Errorf("credential admission CREATE record in ledger %s is malformed: %w", key, err)
	}
	if err := verifyRecordIdentity(&record, ad); err != nil {
		return err
	}
	proof := record.Proof
	if err := s.Attestor.verifyValue(ad, proof); err != nil {
		return fmt.Errorf("credential admission CREATE record in ledger %s is invalid: %w", key, err)
	}

	base := ad.DeepCopy()
	if ad.Annotations == nil {
		ad.Annotations = make(map[string]string)
	}
	ad.Annotations[AttestationAnnotation] = proof
	if err := s.Writer.Patch(ctx, ad,
		client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("finalize credential admission attestation on AgentDeployment %s/%s: %w", ad.Namespace, ad.Name, err)
	}
	if err := s.Attestor.Verify(ctx, ad); err != nil {
		return fmt.Errorf("finalized credential admission attestation is invalid: %w", err)
	}
	s.deleteRecordBestEffort(ctx, ad)
	return nil
}

func (s *RecordStore) deleteRecordBestEffort(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) {
	if err := s.deleteRecord(ctx, ad); err != nil {
		log.FromContext(ctx).Error(err, "credential admission record cleanup deferred",
			"agentDeployment", client.ObjectKeyFromObject(ad), "uid", ad.UID)
	}
}

func (s *RecordStore) deleteRecord(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) error {
	if ad == nil {
		return fmt.Errorf("credential admission record cleanup requires an AgentDeployment")
	}
	key, err := RecordKey(s.Namespace, ad.Namespace)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < recordPersistenceAttempts; attempt++ {
		var ledger corev1.ConfigMap
		if err := s.Reader.Get(ctx, key, &ledger); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("read credential admission record ledger %s for cleanup: %w", key, err)
		}
		if err := validateRecordLedger(&ledger, key, ad.Namespace); err != nil {
			return err
		}
		entryKeys, err := matchingRecordEntryKeys(ledger.Data, ad)
		if err != nil {
			return fmt.Errorf("inspect credential admission record ledger %s for cleanup: %w", key, err)
		}
		if len(entryKeys) == 0 {
			return nil
		}
		if len(entryKeys) == len(ledger.Data) {
			uidPrecondition := ledger.UID
			rvPrecondition := ledger.ResourceVersion
			if err := s.Writer.Delete(ctx, &ledger, client.Preconditions{
				UID: &uidPrecondition, ResourceVersion: &rvPrecondition,
			}); err != nil {
				if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("delete empty credential admission record ledger %s: %w", key, err)
			}
			return nil
		}
		base := ledger.DeepCopy()
		for _, entryKey := range entryKeys {
			delete(ledger.Data, entryKey)
		}
		if err := s.Writer.Patch(ctx, &ledger,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("delete consumed credential admission record from ledger %s: %w", key, err)
		}
		return nil
	}
	return fmt.Errorf("could not delete consumed credential admission record from ledger %s after %d attempts",
		key, recordPersistenceAttempts)
}

func desiredRecordLedger(namespace, targetNamespace, entryKey, entry string) *corev1.ConfigMap {
	key, _ := RecordKey(namespace, targetNamespace)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: namespace,
			Labels: map[string]string{
				RecordLabel: recordLabelValue,
			},
			Annotations: map[string]string{
				recordTargetNamespaceAnnotation: targetNamespace,
			},
		},
		Data: map[string]string{entryKey: entry},
	}
}

func validateRecordLedger(ledger *corev1.ConfigMap, key types.NamespacedName, targetNamespace string) error {
	if ledger == nil || ledger.Namespace != key.Namespace || ledger.Name != key.Name ||
		ledger.Labels[RecordLabel] != recordLabelValue ||
		ledger.Annotations[recordTargetNamespaceAnnotation] != targetNamespace ||
		!ledger.DeletionTimestamp.IsZero() {
		return fmt.Errorf("refusing credential admission record ledger %s: object identity or label is invalid", key)
	}
	return nil
}

func encodeCreateRecord(ad *airunwayv1alpha1.AgentDeployment, proof string, createdAt time.Time) (string, error) {
	record := createRecord{
		TargetNamespace: ad.Namespace,
		TargetName:      ad.Name,
		TargetUID:       ad.UID,
		Proof:           proof,
		CreatedAt:       createdAt,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("encode credential admission CREATE record: %w", err)
	}
	return string(raw), nil
}

func decodeCreateRecord(raw string) (createRecord, error) {
	var record createRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return createRecord{}, err
	}
	if record.TargetNamespace == "" || record.TargetName == "" || record.TargetUID == "" ||
		record.Proof == "" || record.CreatedAt.IsZero() {
		return createRecord{}, fmt.Errorf("required record fields are missing")
	}
	return record, nil
}

func verifyRecordIdentity(record *createRecord, ad *airunwayv1alpha1.AgentDeployment) error {
	if record == nil || ad == nil ||
		record.TargetNamespace != ad.Namespace || record.TargetName != ad.Name || record.TargetUID != ad.UID {
		return fmt.Errorf("credential admission CREATE record does not identify AgentDeployment %s/%s UID %s", ad.Namespace, ad.Name, ad.UID)
	}
	return nil
}

func recordEntryKey(targetName string, targetUID types.UID) string {
	digest := sha256.Sum256([]byte(targetName + "\x00" + string(targetUID)))
	return fmt.Sprintf("%s%x", recordEntryPrefix, digest[:])
}

// legacyRecordEntryKey is retained so controller upgrades can finalize and
// clean pending records written before entries became incarnation-specific.
func legacyRecordEntryKey(targetName string) string {
	digest := sha256.Sum256([]byte(targetName))
	return fmt.Sprintf("%s%x", recordEntryPrefix, digest[:])
}

func findRecordEntry(data map[string]string, ad *airunwayv1alpha1.AgentDeployment) (string, string, bool) {
	if ad == nil {
		return "", "", false
	}
	entryKey := recordEntryKey(ad.Name, ad.UID)
	if raw, found := data[entryKey]; found {
		return entryKey, raw, true
	}
	legacyKey := legacyRecordEntryKey(ad.Name)
	raw, found := data[legacyKey]
	return legacyKey, raw, found
}

func matchingRecordEntryKeys(data map[string]string, ad *airunwayv1alpha1.AgentDeployment) ([]string, error) {
	if ad == nil {
		return nil, nil
	}
	keys := make([]string, 0, 2)
	for _, entryKey := range []string{
		recordEntryKey(ad.Name, ad.UID),
		legacyRecordEntryKey(ad.Name),
	} {
		raw, found := data[entryKey]
		if !found {
			continue
		}
		record, err := decodeCreateRecord(raw)
		if err != nil {
			return nil, fmt.Errorf("entry %q is malformed: %w", entryKey, err)
		}
		if err := verifyRecordIdentity(&record, ad); err != nil {
			if entryKey == legacyRecordEntryKey(ad.Name) {
				continue
			}
			return nil, err
		}
		keys = append(keys, entryKey)
	}
	return keys, nil
}

// RecordKey returns the fixed, bounded CREATE-record ledger key for one target
// namespace. Ledgers live in the controller namespace, while the hash keeps
// target namespace names out of object names and within DNS length limits.
func RecordKey(namespace, targetNamespace string) (types.NamespacedName, error) {
	if namespace == "" {
		return types.NamespacedName{}, fmt.Errorf("credential admission record namespace is empty")
	}
	if targetNamespace == "" {
		return types.NamespacedName{}, fmt.Errorf("credential admission record requires a target namespace")
	}
	digest := sha256.Sum256([]byte(targetNamespace))
	return types.NamespacedName{
		Namespace: namespace,
		Name:      fmt.Sprintf("%s%x", recordLedgerNamePrefix, digest[:12]),
	}, nil
}

// RecordJanitor deletes CREATE records whose request never committed, while
// preserving records for live matching AgentDeployments until reconciliation
// has copied the proof onto the object.
type RecordJanitor struct {
	Store       *RecordStore
	Interval    time.Duration
	GracePeriod time.Duration
}

func (j *RecordJanitor) Start(ctx context.Context) error {
	if j == nil || j.Store == nil {
		return fmt.Errorf("credential admission record janitor is not configured")
	}
	interval := j.Interval
	if interval <= 0 {
		interval = DefaultRecordCleanupInterval
	}
	grace := j.GracePeriod
	if grace <= 0 {
		grace = DefaultRecordCleanupGracePeriod
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := j.Store.cleanupOrphans(ctx, time.Now(), grace); err != nil {
			log.FromContext(ctx).Error(err, "credential admission record cleanup failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (j *RecordJanitor) NeedLeaderElection() bool { return true }

func (s *RecordStore) cleanupOrphans(ctx context.Context, now time.Time, grace time.Duration) error {
	if err := s.Attestor.CheckKey(ctx); err != nil {
		return fmt.Errorf("credential admission signing key is unavailable: %w", err)
	}
	var ledgers corev1.ConfigMapList
	if err := s.Reader.List(ctx, &ledgers, client.InNamespace(s.Namespace),
		client.MatchingLabels{RecordLabel: recordLabelValue}); err != nil {
		return fmt.Errorf("list credential admission record ledgers for cleanup: %w", err)
	}
	for i := range ledgers.Items {
		ledger := &ledgers.Items[i]
		targetNamespace := ledger.Annotations[recordTargetNamespaceAnnotation]
		key, err := RecordKey(s.Namespace, targetNamespace)
		if err != nil {
			return fmt.Errorf("credential admission record ledger %s/%s has invalid target identity: %w",
				ledger.Namespace, ledger.Name, err)
		}
		if err := validateRecordLedger(ledger, key, targetNamespace); err != nil {
			return err
		}
		if err := s.cleanupLedger(ctx, key, targetNamespace, now, grace); err != nil {
			return err
		}
	}
	return nil
}

func (s *RecordStore) cleanupLedger(
	ctx context.Context,
	key types.NamespacedName,
	targetNamespace string,
	now time.Time,
	grace time.Duration,
) error {
	for attempt := 0; attempt < recordPersistenceAttempts; attempt++ {
		var ledger corev1.ConfigMap
		if err := s.Reader.Get(ctx, key, &ledger); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("read credential admission record ledger for cleanup: %w", err)
		}
		if err := validateRecordLedger(&ledger, key, targetNamespace); err != nil {
			return err
		}

		base := ledger.DeepCopy()
		changed := false
		for entryKey, raw := range ledger.Data {
			record, decodeErr := decodeCreateRecord(raw)
			if decodeErr != nil || record.TargetNamespace != targetNamespace ||
				(entryKey != recordEntryKey(record.TargetName, record.TargetUID) &&
					entryKey != legacyRecordEntryKey(record.TargetName)) ||
				record.CreatedAt.After(now.Add(grace)) {
				delete(ledger.Data, entryKey)
				changed = true
				continue
			}
			if now.Before(record.CreatedAt.Add(grace)) {
				continue
			}

			target := types.NamespacedName{Namespace: record.TargetNamespace, Name: record.TargetName}
			var ad airunwayv1alpha1.AgentDeployment
			readErr := s.Reader.Get(ctx, target, &ad)
			if readErr == nil && ad.UID == record.TargetUID &&
				s.Attestor.Verify(ctx, &ad) != nil && s.Attestor.verifyValue(&ad, record.Proof) == nil {
				continue
			}
			if readErr != nil && !apierrors.IsNotFound(readErr) {
				return fmt.Errorf("read target AgentDeployment %s for credential admission record cleanup: %w", target, readErr)
			}
			delete(ledger.Data, entryKey)
			changed = true
		}
		if !changed {
			return nil
		}
		if len(ledger.Data) == 0 {
			uidPrecondition := ledger.UID
			rvPrecondition := ledger.ResourceVersion
			if err := s.Writer.Delete(ctx, &ledger, client.Preconditions{
				UID: &uidPrecondition, ResourceVersion: &rvPrecondition,
			}); err != nil {
				if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("delete empty credential admission record ledger %s: %w", key, err)
			}
			return nil
		}
		if err := s.Writer.Patch(ctx, &ledger,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("prune credential admission record ledger %s: %w", key, err)
		}
		return nil
	}
	return fmt.Errorf("could not prune credential admission record ledger %s after %d attempts",
		key, recordPersistenceAttempts)
}
