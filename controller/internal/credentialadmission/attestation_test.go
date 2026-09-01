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
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

func TestAttestationBindsUserControlledAgentDeployment(t *testing.T) {
	attestor, err := New(bytes.Repeat([]byte{0x42}, attestationKeySize))
	if err != nil {
		t.Fatal(err)
	}
	ad := &airunwayv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "signed-agent",
			Namespace:   "team-a",
			UID:         types.UID("server-assigned"),
			Generation:  7,
			Labels:      map[string]string{"team": "a"},
			Annotations: map[string]string{"example.com/note": "original"},
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

	if err := attestor.Stamp(context.Background(), ad, ad.UID, ad.Generation); err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	if err := attestor.Verify(context.Background(), ad); err != nil {
		t.Fatalf("Verify stamped object: %v", err)
	}

	// API-server-populated fields and controller-owned status do not change the
	// admitted request shape.
	ad.ResourceVersion = "42"
	ad.CreationTimestamp = metav1.NewTime(time.Now())
	ad.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "attacker", Time: timePtr(time.Now().Add(24 * time.Hour))}}
	ad.Status.Phase = airunwayv1alpha1.AgentPhaseRunning
	if err := attestor.Verify(context.Background(), ad); err != nil {
		t.Fatalf("server-owned fields must not invalidate attestation: %v", err)
	}

	ad.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"image":"ghcr.io/mallory/exfil:latest"}`)}
	if err := attestor.Verify(context.Background(), ad); err == nil {
		t.Fatal("changing the workload image must invalidate credential admission")
	}
	ad.Spec.Config = nil
	ad.UID = types.UID("replacement")
	if err := attestor.Verify(context.Background(), ad); err == nil {
		t.Fatal("replaying an attestation onto a replacement UID must fail")
	}
	ad.UID = types.UID("server-assigned")
	ad.Generation++
	if err := attestor.Verify(context.Background(), ad); err == nil {
		t.Fatal("replaying an attestation onto a later generation must fail")
	}
}

func TestAttestationRejectsCallerSuppliedManagedFieldsProof(t *testing.T) {
	attestor, err := New(bytes.Repeat([]byte{0x24}, attestationKeySize))
	if err != nil {
		t.Fatal(err)
	}
	ad := &airunwayv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
		Name:       "forged",
		Namespace:  "team-a",
		UID:        types.UID("forged-uid"),
		Generation: 1,
		Annotations: map[string]string{
			AttestationAnnotation: "v1.forged.forged",
		},
		ManagedFields: []metav1.ManagedFieldsEntry{{
			Manager: "mallory",
			Time:    timePtr(time.Now().Add(365 * 24 * time.Hour)),
		}},
	}}
	if err := attestor.Verify(context.Background(), ad); err == nil {
		t.Fatal("future-dated managedFields must not substitute for a webhook attestation")
	}
}

func TestLoadOrCreateKeyPersistsAndReusesWebhookSecretData(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secretKey := types.NamespacedName{Name: "webhook-cert", Namespace: "airunway-system"}
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace},
		Data:       map[string][]byte{"tls.crt": []byte("preserve-me")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	first, err := LoadOrCreateKey(context.Background(), c, secretKey)
	if err != nil {
		t.Fatalf("LoadOrCreateKey first call: %v", err)
	}
	second, err := LoadOrCreateKey(context.Background(), c, secretKey)
	if err != nil {
		t.Fatalf("LoadOrCreateKey second call: %v", err)
	}
	if !bytes.Equal(first, second) || len(first) != attestationKeySize {
		t.Fatalf("key was not stable: first=%d bytes second=%d bytes", len(first), len(second))
	}

	var stored corev1.Secret
	if err := c.Get(context.Background(), secretKey, &stored); err != nil {
		t.Fatal(err)
	}
	if string(stored.Data["tls.crt"]) != "preserve-me" {
		t.Fatal("adding the attestation key replaced existing certificate data")
	}
	if !bytes.Equal(stored.Data[SecretDataKey], first) {
		t.Fatal("stored attestation key does not match returned key")
	}
	if stored.Immutable == nil || !*stored.Immutable {
		t.Fatal("credential admission signing Secret was not made immutable")
	}
}

func TestGuardedAttestorRejectsReplacedSigningSecret(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x42}, attestationKeySize)
	immutable := true
	secretKey := types.NamespacedName{Name: SigningSecretName, Namespace: "airunway-system"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretKey.Name, Namespace: secretKey.Namespace, UID: types.UID("signing-key-v1"),
		},
		Immutable: &immutable,
		Data:      map[string][]byte{SecretDataKey: key},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	guard, err := NewKeyGuard(ctx, c, secretKey, key)
	if err != nil {
		t.Fatal(err)
	}
	attestor, err := NewGuarded(key, guard)
	if err != nil {
		t.Fatal(err)
	}
	ad := &airunwayv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
		Name: "signed-agent", Namespace: "team-a", UID: types.UID("agent-v1"), Generation: 1,
	}}
	if err := attestor.Stamp(ctx, ad, ad.UID, ad.Generation); err != nil {
		t.Fatalf("initial guarded stamp failed: %v", err)
	}

	if err := c.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	replacement := secret.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = types.UID("signing-key-v2")
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := attestor.Verify(ctx, ad); err == nil {
		t.Fatal("stale replica verified a proof after the signing Secret was replaced")
	}
	req := httptest.NewRequest("GET", "/healthz", nil)
	if err := guard.Healthz(req); err == nil {
		t.Fatal("definitive signing Secret replacement did not fail liveness")
	}
	if err := guard.Readyz(req); err == nil {
		t.Fatal("definitive signing Secret replacement did not fail readiness")
	}
}

func TestKeyGuardKeepsTransientReadFailureOutOfLiveness(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x24}, attestationKeySize)
	immutable := true
	secretKey := types.NamespacedName{Name: SigningSecretName, Namespace: "airunway-system"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretKey.Name, Namespace: secretKey.Namespace, UID: types.UID("signing-key-v1"),
		},
		Immutable: &immutable,
		Data:      map[string][]byte{SecretDataKey: key},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	guard, err := NewKeyGuard(ctx, c, secretKey, key)
	if err != nil {
		t.Fatal(err)
	}
	guard.reader = getFailingReader{Reader: c, err: errors.New("temporary API failure")}
	req := httptest.NewRequest("GET", "/healthz", nil)
	if err := guard.Readyz(req); err == nil {
		t.Fatal("transient signing Secret read failure did not fail readiness")
	}
	if err := guard.Healthz(req); err != nil {
		t.Fatalf("transient API failure should not restart the controller: %v", err)
	}
}

func TestLoadOrCreateKeyRejectsMalformedExistingKey(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "webhook-cert", Namespace: "airunway-system"},
		Data:       map[string][]byte{SecretDataKey: []byte("short")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	if _, err := LoadOrCreateKey(context.Background(), c, clientKey(secret)); err == nil {
		t.Fatal("malformed persisted key must fail closed")
	}
}

func clientKey(obj metav1.Object) types.NamespacedName {
	return types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
}

func timePtr(value time.Time) *metav1.Time {
	t := metav1.NewTime(value)
	return &t
}

type getFailingReader struct {
	client.Reader
	err error
}

func (r getFailingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}
