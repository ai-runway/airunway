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
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

func TestRecordStorePersistsAndFinalizesCreateProof(t *testing.T) {
	ctx := context.Background()
	ad := recordTestAgent("uid-create")
	store, c := newRecordTestStore(t, ad)

	if err := store.PersistCreate(ctx, ad); err != nil {
		t.Fatalf("PersistCreate: %v", err)
	}
	recordKey, err := RecordKey(store.Namespace, ad.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	var record corev1.ConfigMap
	if err := c.Get(ctx, recordKey, &record); err != nil {
		t.Fatalf("read persisted record: %v", err)
	}

	var stored airunwayv1alpha1.AgentDeployment
	if err := c.Get(ctx, client.ObjectKeyFromObject(ad), &stored); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyOrFinalize(ctx, &stored); err != nil {
		t.Fatalf("VerifyOrFinalize: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ad), &stored); err != nil {
		t.Fatal(err)
	}
	if err := store.Attestor.Verify(ctx, &stored); err != nil {
		t.Fatalf("persisted annotation did not verify: %v", err)
	}
	if err := c.Get(ctx, recordKey, &record); !apierrors.IsNotFound(err) {
		t.Fatalf("consumed record still exists or returned unexpected error: %v", err)
	}
}

func TestRecordStoreRecoversCommittedLedgerCreateAfterResponseError(t *testing.T) {
	ctx := context.Background()
	ad := recordTestAgent("uid-create-response-loss")
	store, c := newRecordTestStore(t)
	responseErr := errors.New("injected lost ConfigMap Create response")
	writer := &commitThenErrorConfigMapClient{Client: c, createErr: responseErr}
	store.Writer = writer

	if err := store.PersistCreate(ctx, ad); err != nil {
		t.Fatalf("PersistCreate rejected a committed ledger Create: %v", err)
	}
	if !writer.createCommitted {
		t.Fatal("test client did not commit the ConfigMap Create")
	}
	assertRecordExists(t, c, ad, true)
}

func TestRecordStoreRecoversCommittedLedgerPatchAfterResponseError(t *testing.T) {
	ctx := context.Background()
	first := recordTestAgent("uid-patch-existing")
	ad := recordTestAgent("uid-patch-response-loss")
	store, c := newRecordTestStore(t)
	if err := store.PersistCreate(ctx, first); err != nil {
		t.Fatal(err)
	}
	responseErr := errors.New("injected lost ConfigMap Patch response")
	writer := &commitThenErrorConfigMapClient{Client: c, patchErr: responseErr}
	store.Writer = writer

	if err := store.PersistCreate(ctx, ad); err != nil {
		t.Fatalf("PersistCreate rejected a committed ledger Patch: %v", err)
	}
	if !writer.patchCommitted {
		t.Fatal("test client did not commit the ConfigMap Patch")
	}
	assertRecordExists(t, c, first, true)
	assertRecordExists(t, c, ad, true)
}

func TestRecordStorePreservesCreateErrorWhenRecoveryEntryDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	ad := recordTestAgent("uid-create-wrong-proof")
	store, c := newRecordTestStore(t)
	wrongEntry, err := encodeCreateRecord(ad, "wrong-proof", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	responseErr := errors.New("injected ConfigMap Create response error")
	store.Writer = &commitThenErrorConfigMapClient{
		Client:    c,
		createErr: responseErr,
		mutate: func(ledger *corev1.ConfigMap) {
			ledger.Data[recordEntryKey(ad.Name, ad.UID)] = wrongEntry
		},
	}

	if err := store.PersistCreate(ctx, ad); !errors.Is(err, responseErr) {
		t.Fatalf("PersistCreate error = %v, want original Create error", err)
	}
}

func TestRecordStorePreservesPatchErrorWhenRecoveryEntryDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	first := recordTestAgent("uid-patch-wrong-existing")
	ad := recordTestAgent("uid-patch-wrong-proof")
	store, c := newRecordTestStore(t)
	if err := store.PersistCreate(ctx, first); err != nil {
		t.Fatal(err)
	}
	wrongEntry, err := encodeCreateRecord(ad, "wrong-proof", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	responseErr := errors.New("injected ConfigMap Patch response error")
	store.Writer = &commitThenErrorConfigMapClient{
		Client:   c,
		patchErr: responseErr,
		mutate: func(ledger *corev1.ConfigMap) {
			ledger.Data[recordEntryKey(ad.Name, ad.UID)] = wrongEntry
		},
	}

	if err := store.PersistCreate(ctx, ad); !errors.Is(err, responseErr) {
		t.Fatalf("PersistCreate error = %v, want original Patch error", err)
	}
}

func TestRecordStoreRejectsTamperedOrReplayedCreateProof(t *testing.T) {
	ctx := context.Background()
	ad := recordTestAgent("uid-create")
	store, c := newRecordTestStore(t, ad)
	if err := store.PersistCreate(ctx, ad); err != nil {
		t.Fatal(err)
	}

	var tampered airunwayv1alpha1.AgentDeployment
	if err := c.Get(ctx, client.ObjectKeyFromObject(ad), &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.Spec.Model.ExternalAPI.ModelName = "attacker-model"
	if err := store.VerifyOrFinalize(ctx, &tampered); err == nil {
		t.Fatal("a CREATE record was replayed onto a different spec")
	}
	if _, found := tampered.Annotations[AttestationAnnotation]; found {
		t.Fatal("failed verification mutated the AgentDeployment proof")
	}
	recordKey, _ := RecordKey(store.Namespace, ad.Namespace)
	var record corev1.ConfigMap
	if err := c.Get(ctx, recordKey, &record); err != nil {
		t.Fatalf("failed verification consumed the CREATE record: %v", err)
	}
}

func TestRecordStoreDoesNotBlockOnConsumedRecordDeleteFailure(t *testing.T) {
	ctx := context.Background()
	ad := recordTestAgent("uid-delete-failure")
	store, c := newRecordTestStore(t, ad)
	store.Writer = &deleteFailingClient{Client: c, err: errors.New("temporary delete failure")}
	if err := store.PersistCreate(ctx, ad); err != nil {
		t.Fatal(err)
	}

	var stored airunwayv1alpha1.AgentDeployment
	if err := c.Get(ctx, client.ObjectKeyFromObject(ad), &stored); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyOrFinalize(ctx, &stored); err != nil {
		t.Fatalf("valid finalization was blocked by record cleanup: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ad), &stored); err != nil {
		t.Fatal(err)
	}
	if err := store.Attestor.Verify(ctx, &stored); err != nil {
		t.Fatalf("finalized proof did not persist: %v", err)
	}
	assertRecordExists(t, c, ad, true)

	store.Writer = c
	if err := store.VerifyOrFinalize(ctx, &stored); err != nil {
		t.Fatalf("idempotent verification did not retry record cleanup: %v", err)
	}
	assertRecordExists(t, c, ad, false)
}

func TestRecordStoreCleanupPreservesOnlyLiveUnfinalizedTargets(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	grace := 10 * time.Minute

	live := recordTestAgent("uid-live")
	finalized := recordTestAgent("uid-finalized")
	attestor, err := New(bytes.Repeat([]byte{0x42}, attestationKeySize))
	if err != nil {
		t.Fatal(err)
	}
	if err := attestor.Stamp(ctx, finalized, finalized.UID, finalized.Generation); err != nil {
		t.Fatal(err)
	}

	objects := []client.Object{live, finalized}
	ledgerKey, err := RecordKey("airunway-system", live.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ledgerKey.Name,
			Namespace: "airunway-system",
			Labels:    map[string]string{RecordLabel: recordLabelValue},
			Annotations: map[string]string{
				recordTargetNamespaceAnnotation: live.Namespace,
			},
		},
		Data: map[string]string{},
	}
	for _, ad := range []*airunwayv1alpha1.AgentDeployment{live, finalized} {
		proof, signErr := attestor.sign(ad, ad.UID, ad.Generation)
		if signErr != nil {
			t.Fatal(signErr)
		}
		raw, encodeErr := encodeCreateRecord(ad, proof, now.Add(-2*grace))
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		ledger.Data[recordEntryKey(ad.Name, ad.UID)] = raw
	}

	orphan := recordTestAgent("uid-orphan")
	orphanProof, err := attestor.sign(orphan, orphan.UID, orphan.Generation)
	if err != nil {
		t.Fatal(err)
	}
	orphanRaw, err := encodeCreateRecord(orphan, orphanProof, now.Add(-2*grace))
	if err != nil {
		t.Fatal(err)
	}
	ledger.Data[recordEntryKey(orphan.Name, orphan.UID)] = orphanRaw

	stale := recordTestAgent("uid-stale")
	staleProof, err := attestor.sign(stale, stale.UID, stale.Generation)
	if err != nil {
		t.Fatal(err)
	}
	staleRaw, err := encodeCreateRecord(stale, staleProof, now.Add(-2*grace))
	if err != nil {
		t.Fatal(err)
	}
	ledger.Data[recordEntryKey(stale.Name, stale.UID)] = staleRaw
	stale.Generation++
	stale.Spec.Model.ExternalAPI.CredentialsRef = nil
	objects = append(objects, stale)
	ledger.Data["malformed-record"] = "not-json"
	objects = append(objects, ledger)

	store, c := newRecordTestStoreWithAttestor(t, attestor, objects...)
	if err := store.cleanupOrphans(ctx, now, grace); err != nil {
		t.Fatalf("cleanupOrphans: %v", err)
	}

	assertRecordExists(t, c, live, true)
	assertRecordExists(t, c, finalized, false)
	assertRecordExists(t, c, orphan, false)
	assertRecordExists(t, c, stale, false)
	var got corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(ledger), &got); err != nil {
		t.Fatal(err)
	}
	if _, found := got.Data["malformed-record"]; found {
		t.Fatal("malformed expired record was not deleted")
	}
}

func TestRecordStoreBoundsPendingCreateRecords(t *testing.T) {
	ctx := context.Background()
	store, c := newRecordTestStore(t)
	store.MaxPendingRecords = 2

	first := recordTestAgent("uid-cap-one")
	second := recordTestAgent("uid-cap-two")
	third := recordTestAgent("uid-cap-three")
	otherTenant := recordTestAgent("uid-cap-other-tenant")
	otherTenant.Namespace = "team-b"
	if err := store.PersistCreate(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistCreate(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistCreate(ctx, third); err == nil {
		t.Fatal("record store accepted a CREATE beyond the target namespace's configured capacity")
	}
	if err := store.PersistCreate(ctx, otherTenant); err != nil {
		t.Fatalf("one target namespace exhausted another namespace's credential CREATE capacity: %v", err)
	}

	key, err := RecordKey(store.Namespace, first.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	var ledger corev1.ConfigMap
	if err := c.Get(ctx, key, &ledger); err != nil {
		t.Fatal(err)
	}
	if len(ledger.Data) != 2 {
		t.Fatalf("ledger contains %d records, want hard bound 2", len(ledger.Data))
	}
	var ledgers corev1.ConfigMapList
	if err := c.List(ctx, &ledgers, client.InNamespace(store.Namespace),
		client.MatchingLabels{RecordLabel: recordLabelValue}); err != nil {
		t.Fatal(err)
	}
	if len(ledgers.Items) != 2 {
		t.Fatalf("created %d credential record ConfigMaps, want one fixed ledger per target namespace", len(ledgers.Items))
	}
}

func TestRecordStoreDoesNotPersistCreateForExistingName(t *testing.T) {
	ctx := context.Background()
	existing := recordTestAgent("uid-existing")
	store, c := newRecordTestStore(t, existing)

	for i := range DefaultMaxPendingRecords + 1 {
		attempt := recordTestAgent(types.UID(fmt.Sprintf("uid-rejected-%d", i)))
		attempt.Name = existing.Name
		if err := store.PersistCreate(ctx, attempt); !apierrors.IsAlreadyExists(err) {
			t.Fatalf("PersistCreate rejected conflict %d with %v, want AlreadyExists", i, err)
		}
	}

	key, err := RecordKey(store.Namespace, existing.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	var ledger corev1.ConfigMap
	if err := c.Get(ctx, key, &ledger); !apierrors.IsNotFound(err) {
		t.Fatalf("rejected AlreadyExists CREATEs left a credential ledger: %v", err)
	}
}

func TestRecordStoreBoundsPendingRetriesForOneName(t *testing.T) {
	ctx := context.Background()
	store, c := newRecordTestStore(t)
	store.MaxPendingRecords = 4

	first := recordTestAgent("uid-retry-one")
	second := recordTestAgent("uid-retry-two")
	third := recordTestAgent("uid-retry-three")
	second.Name = first.Name
	third.Name = first.Name
	other := recordTestAgent("uid-other-name")

	if err := store.PersistCreate(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistCreate(ctx, second); err != nil {
		t.Fatalf("one uncertain retry for the same object name was rejected: %v", err)
	}
	if err := store.PersistCreate(ctx, first); err != nil {
		t.Fatalf("same-UID admission redelivery was not idempotent at the per-name bound: %v", err)
	}
	if err := store.PersistCreate(ctx, third); err == nil {
		t.Fatal("one object name consumed more than its bounded retry allowance")
	}
	if err := store.PersistCreate(ctx, other); err != nil {
		t.Fatalf("one object's retries blocked an independent object name: %v", err)
	}

	key, err := RecordKey(store.Namespace, first.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	var ledger corev1.ConfigMap
	if err := c.Get(ctx, key, &ledger); err != nil {
		t.Fatal(err)
	}
	if len(ledger.Data) != 3 {
		t.Fatalf("ledger contains %d records, want two retries plus one independent name", len(ledger.Data))
	}
}

func TestRecordStorePreservesUncertainRetriesForOneName(t *testing.T) {
	ctx := context.Background()
	first := recordTestAgent("uid-collision-one")
	second := recordTestAgent("uid-collision-two")
	second.Name = first.Name
	store, c := newRecordTestStore(t)

	if err := store.PersistCreate(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistCreate(ctx, second); err != nil {
		t.Fatalf("an uncertain retry for the same object name was rejected: %v", err)
	}

	key, err := RecordKey(store.Namespace, first.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	var ledger corev1.ConfigMap
	if err := c.Get(ctx, key, &ledger); err != nil {
		t.Fatal(err)
	}
	if len(ledger.Data) != 2 {
		t.Fatalf("uncertain CREATE retries produced %d ledger entries, want one per UID", len(ledger.Data))
	}
	for _, ad := range []*airunwayv1alpha1.AgentDeployment{first, second} {
		raw, found := ledger.Data[recordEntryKey(ad.Name, ad.UID)]
		if !found {
			t.Fatalf("ledger does not contain the incarnation entry for UID %s", ad.UID)
		}
		record, err := decodeCreateRecord(raw)
		if err != nil {
			t.Fatal(err)
		}
		if record.TargetUID != ad.UID {
			t.Fatalf("pending CREATE UID = %s, want %s", record.TargetUID, ad.UID)
		}
	}

	if err := c.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	var stored airunwayv1alpha1.AgentDeployment
	if err := c.Get(ctx, client.ObjectKeyFromObject(first), &stored); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyOrFinalize(ctx, &stored); err != nil {
		t.Fatalf("committed first incarnation could not consume its exact proof: %v", err)
	}
	assertRecordExists(t, c, first, false)
	assertRecordExists(t, c, second, true)

	if err := c.Delete(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(second), &stored); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyOrFinalize(ctx, &stored); err != nil {
		t.Fatalf("committed retry incarnation could not consume its exact proof: %v", err)
	}
	assertRecordExists(t, c, second, false)
}

func TestRecordStoreFinalizesLegacyNameOnlyEntry(t *testing.T) {
	ctx := context.Background()
	ad := recordTestAgent("uid-legacy-entry")
	attestor, err := New(bytes.Repeat([]byte{0x42}, attestationKeySize))
	if err != nil {
		t.Fatal(err)
	}
	proof, err := attestor.sign(ad, ad.UID, ad.Generation)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encodeCreateRecord(ad, proof, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ledger := desiredRecordLedger(
		"airunway-system",
		ad.Namespace,
		legacyRecordEntryKey(ad.Name),
		raw,
	)
	store, c := newRecordTestStoreWithAttestor(t, attestor, ad, ledger)

	var stored airunwayv1alpha1.AgentDeployment
	if err := c.Get(ctx, client.ObjectKeyFromObject(ad), &stored); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyOrFinalize(ctx, &stored); err != nil {
		t.Fatalf("legacy CREATE record could not be finalized: %v", err)
	}
	if err := store.Attestor.Verify(ctx, &stored); err != nil {
		t.Fatalf("legacy CREATE record did not produce a valid proof: %v", err)
	}
	var remaining corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(ledger), &remaining); !apierrors.IsNotFound(err) {
		t.Fatalf("consumed legacy CREATE record ledger still exists or returned an unexpected error: %v", err)
	}
}

func newRecordTestStore(t *testing.T, objects ...client.Object) (*RecordStore, client.Client) {
	t.Helper()
	attestor, err := New(bytes.Repeat([]byte{0x42}, attestationKeySize))
	if err != nil {
		t.Fatal(err)
	}
	return newRecordTestStoreWithAttestor(t, attestor, objects...)
}

func newRecordTestStoreWithAttestor(
	t *testing.T,
	attestor *Attestor,
	objects ...client.Object,
) (*RecordStore, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	store, err := NewRecordStore(attestor, c, c, "airunway-system")
	if err != nil {
		t.Fatal(err)
	}
	return store, c
}

func recordTestAgent(uid types.UID) *airunwayv1alpha1.AgentDeployment {
	return &airunwayv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "agent-" + string(uid),
			Namespace:  "team-a",
			UID:        uid,
			Generation: 1,
		},
		Spec: airunwayv1alpha1.AgentDeploymentSpec{
			Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "crewai"},
			Model: airunwayv1alpha1.ModelBinding{ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
				Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
				BaseURL:   "https://api.openai.com/v1",
				ModelName: "gpt-4o-mini",
				CredentialsRef: &airunwayv1alpha1.SecretKeyRef{
					Name: "openai", Key: "token",
				},
			}},
		},
	}
}

func assertRecordExists(t *testing.T, c client.Client, ad *airunwayv1alpha1.AgentDeployment, want bool) {
	t.Helper()
	key, err := RecordKey("airunway-system", ad.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	var record corev1.ConfigMap
	err = c.Get(context.Background(), key, &record)
	if want && err != nil {
		t.Fatalf("record %s should exist: %v", key, err)
	}
	if apierrors.IsNotFound(err) {
		if want {
			t.Fatalf("record %s should exist", key)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	_, found := record.Data[recordEntryKey(ad.Name, ad.UID)]
	if found != want {
		t.Fatalf("record entry for %s/%s UID %s exists=%v, want %v",
			ad.Namespace, ad.Name, ad.UID, found, want)
	}
}

type deleteFailingClient struct {
	client.Client
	err error
}

func (c *deleteFailingClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return c.err
}

type commitThenErrorConfigMapClient struct {
	client.Client
	createErr       error
	patchErr        error
	mutate          func(*corev1.ConfigMap)
	createCommitted bool
	patchCommitted  bool
}

func (c *commitThenErrorConfigMapClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	ledger, ok := obj.(*corev1.ConfigMap)
	if !ok || c.createErr == nil || c.createCommitted {
		return c.Client.Create(ctx, obj, opts...)
	}
	committed := ledger.DeepCopy()
	if c.mutate != nil {
		c.mutate(committed)
	}
	if err := c.Client.Create(ctx, committed, opts...); err != nil {
		return err
	}
	c.createCommitted = true
	return c.createErr
}

func (c *commitThenErrorConfigMapClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	ledger, ok := obj.(*corev1.ConfigMap)
	if !ok || c.patchErr == nil || c.patchCommitted {
		return c.Client.Patch(ctx, obj, patch, opts...)
	}
	committed := ledger.DeepCopy()
	if c.mutate != nil {
		c.mutate(committed)
	}
	if err := c.Client.Patch(ctx, committed, patch, opts...); err != nil {
		return err
	}
	c.patchCommitted = true
	return c.patchErr
}
