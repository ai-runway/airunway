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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

const (
	// AttestationAnnotation is written only after credential admission has
	// authorized the exact AgentDeployment incarnation and generation. The
	// controller verifies it before resolving a Secret reference.
	AttestationAnnotation = "agents.airunway.ai/credential-admission-attestation"

	// SigningSecretName is deliberately separate from the webhook TLS Secret:
	// certificate rotation must never rotate or expose the admission signing
	// key that existing AgentDeployments depend on.
	SigningSecretName = "airunway-credential-admission-key"
	SecretDataKey     = "key"

	attestationVersion = "v1"
	attestationKeySize = 32
	keySetupAttempts   = 8
)

// Attestor signs and verifies the behavior-bearing portion of an
// AgentDeployment together with its server-assigned incarnation and expected
// persisted generation. Status and non-behavioral metadata are intentionally
// excluded so ordinary status and metadata reconciliation do not invalidate
// the proof.
type Attestor struct {
	key      []byte
	keyID    string
	keyGuard *KeyGuard
}

// New constructs an Attestor from a 256-bit key.
func New(key []byte) (*Attestor, error) {
	if len(key) != attestationKeySize {
		return nil, fmt.Errorf("credential admission attestation key must be exactly %d bytes", attestationKeySize)
	}
	keyCopy := append([]byte(nil), key...)
	digest := sha256.Sum256(keyCopy)
	return &Attestor{
		key:   keyCopy,
		keyID: base64.RawURLEncoding.EncodeToString(digest[:8]),
	}, nil
}

// NewGuarded constructs an Attestor that authoritatively checks its immutable
// signing Secret before every sign or verify operation. A deleted or replaced
// Secret therefore makes stale replicas fail closed instead of continuing to
// accept their process-local key indefinitely.
func NewGuarded(key []byte, guard *KeyGuard) (*Attestor, error) {
	if guard == nil {
		return nil, fmt.Errorf("credential admission attestor requires a signing-key guard")
	}
	attestor, err := New(key)
	if err != nil {
		return nil, err
	}
	attestor.keyGuard = guard
	return attestor, nil
}

// CheckKey verifies that the live immutable signing Secret is the same
// incarnation and contains the same key loaded by this process.
func (a *Attestor) CheckKey(ctx context.Context) error {
	if a == nil {
		return fmt.Errorf("credential admission attestor is not configured")
	}
	if a.keyGuard == nil {
		return nil
	}
	return a.keyGuard.Check(ctx)
}

// Stamp replaces any caller-supplied value with a signature over the supplied
// server-assigned UID, expected persisted generation, and current spec.
//
// On UPDATE, uid and generation are derived from the trusted old object and
// the spec change before the API server persists the update. CREATE mutation
// cannot use this method because Kubernetes assigns UID and generation only
// after mutating admission; CREATE uses a validating-admission record instead.
func (a *Attestor) Stamp(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment, uid types.UID, generation int64) error {
	if a == nil {
		return fmt.Errorf("credential admission attestor is not configured")
	}
	if err := a.CheckKey(ctx); err != nil {
		return err
	}
	value, err := a.sign(ad, uid, generation)
	if err != nil {
		return err
	}
	if ad.Annotations == nil {
		ad.Annotations = make(map[string]string)
	}
	ad.Annotations[AttestationAnnotation] = value
	return nil
}

// Remove clears a stale proof when the request no longer references a
// credential Secret.
func (a *Attestor) Remove(ad *airunwayv1alpha1.AgentDeployment) {
	if ad == nil || ad.Annotations == nil {
		return
	}
	delete(ad.Annotations, AttestationAnnotation)
}

// Verify proves that this exact persisted object incarnation, generation, and
// spec passed credential authorization while the attestor held this key.
func (a *Attestor) Verify(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) error {
	if a == nil {
		return fmt.Errorf("credential admission attestor is not configured")
	}
	if err := a.CheckKey(ctx); err != nil {
		return err
	}
	if ad == nil {
		return fmt.Errorf("credential admission attestation cannot verify a nil AgentDeployment")
	}
	value := ad.Annotations[AttestationAnnotation]
	return a.verifyValue(ad, value)
}

// KeyGuard binds a running process to the immutable signing Secret
// incarnation from which its credential-admission key was loaded.
type KeyGuard struct {
	reader    client.Reader
	secretKey types.NamespacedName
	secretUID types.UID
	keyDigest [sha256.Size]byte
}

// NewKeyGuard captures the live immutable Secret identity for expectedKey.
func NewKeyGuard(
	ctx context.Context,
	reader client.Reader,
	secretKey types.NamespacedName,
	expectedKey []byte,
) (*KeyGuard, error) {
	if reader == nil {
		return nil, fmt.Errorf("credential admission signing-key guard requires a reader")
	}
	if secretKey.Name == "" || secretKey.Namespace == "" {
		return nil, fmt.Errorf("credential admission signing-key guard requires a Secret name and namespace")
	}
	if len(expectedKey) != attestationKeySize {
		return nil, fmt.Errorf("credential admission signing-key guard requires exactly %d key bytes", attestationKeySize)
	}
	var secret corev1.Secret
	if err := reader.Get(ctx, secretKey, &secret); err != nil {
		return nil, fmt.Errorf("read credential admission signing Secret for guard initialization: %w", err)
	}
	guard := &KeyGuard{
		reader:    reader,
		secretKey: secretKey,
		secretUID: secret.UID,
		keyDigest: sha256.Sum256(expectedKey),
	}
	if _, err := guard.checkSecret(&secret); err != nil {
		return nil, err
	}
	return guard, nil
}

// Check fails closed on any read error or live signing-Secret mismatch.
func (g *KeyGuard) Check(ctx context.Context) error {
	_, err := g.check(ctx)
	return err
}

// Readyz removes stale replicas from the webhook Service and also reports
// transient API read failures, since such a replica cannot safely attest.
func (g *KeyGuard) Readyz(req *http.Request) error {
	if req == nil {
		return fmt.Errorf("credential admission signing-key readiness request is nil")
	}
	return g.Check(req.Context())
}

// Healthz restarts a replica only after a definitive Secret deletion,
// replacement, or mutation. Transient API failures remain readiness failures
// but do not turn an API outage into a controller restart loop.
func (g *KeyGuard) Healthz(req *http.Request) error {
	if req == nil {
		return fmt.Errorf("credential admission signing-key health request is nil")
	}
	definitive, err := g.check(req.Context())
	if definitive {
		return err
	}
	return nil
}

func (g *KeyGuard) check(ctx context.Context) (bool, error) {
	if g == nil || g.reader == nil {
		return true, fmt.Errorf("credential admission signing-key guard is not configured")
	}
	var secret corev1.Secret
	if err := g.reader.Get(ctx, g.secretKey, &secret); err != nil {
		return apierrors.IsNotFound(err), fmt.Errorf("read credential admission signing Secret: %w", err)
	}
	return g.checkSecret(&secret)
}

func (g *KeyGuard) checkSecret(secret *corev1.Secret) (bool, error) {
	if secret == nil || secret.Namespace != g.secretKey.Namespace || secret.Name != g.secretKey.Name ||
		secret.UID == "" || secret.UID != g.secretUID || !secret.DeletionTimestamp.IsZero() {
		return true, fmt.Errorf("credential admission signing Secret was deleted or replaced; restart with the current immutable key")
	}
	if secret.Immutable == nil || !*secret.Immutable {
		return true, fmt.Errorf("credential admission signing Secret is no longer immutable")
	}
	key := secret.Data[SecretDataKey]
	if len(key) != attestationKeySize || !hmac.Equal(g.keyDigest[:], sha256Digest(key)) {
		return true, fmt.Errorf("credential admission signing Secret key changed")
	}
	return false, nil
}

func sha256Digest(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}

func (a *Attestor) sign(ad *airunwayv1alpha1.AgentDeployment, uid types.UID, generation int64) (string, error) {
	if uid == "" {
		return "", fmt.Errorf("credential admission attestation requires a server-assigned AgentDeployment UID")
	}
	if generation < 1 {
		return "", fmt.Errorf("credential admission attestation requires a positive persisted generation")
	}
	payload, err := canonicalPayload(ad, uid, generation)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write(payload)
	return strings.Join([]string{
		attestationVersion,
		a.keyID,
		strconv.FormatInt(generation, 10),
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}, "."), nil
}

func (a *Attestor) verifyValue(ad *airunwayv1alpha1.AgentDeployment, value string) error {
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != attestationVersion || parts[1] != a.keyID {
		return fmt.Errorf("AgentDeployment has no valid credential admission attestation")
	}
	generation, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || generation < 1 || generation != ad.Generation {
		return fmt.Errorf("AgentDeployment credential admission attestation does not match its current generation")
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return fmt.Errorf("AgentDeployment has no valid credential admission attestation")
	}
	payload, err := canonicalPayload(ad, ad.UID, generation)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("AgentDeployment credential admission attestation does not match its current UID, generation, or spec")
	}
	return nil
}

type attestedObject struct {
	APIVersion string                               `json:"apiVersion"`
	Kind       string                               `json:"kind"`
	Metadata   attestedMetadata                     `json:"metadata"`
	Generation int64                                `json:"generation"`
	Spec       airunwayv1alpha1.AgentDeploymentSpec `json:"spec"`
}

type attestedMetadata struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	UID       types.UID `json:"uid"`
}

func canonicalPayload(ad *airunwayv1alpha1.AgentDeployment, uid types.UID, generation int64) ([]byte, error) {
	if ad == nil {
		return nil, fmt.Errorf("credential admission attestation cannot sign a nil AgentDeployment")
	}
	payload := attestedObject{
		APIVersion: airunwayv1alpha1.GroupVersion.String(),
		Kind:       "AgentDeployment",
		Metadata: attestedMetadata{
			Name:      ad.Name,
			Namespace: ad.Namespace,
			UID:       uid,
		},
		Generation: generation,
		Spec:       ad.Spec,
	}

	// Re-decode with UseNumber before the final marshal so RawExtension JSON is
	// normalized without routing exact integer values through float64.
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal credential admission attestation payload: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var canonical any
	if err := decoder.Decode(&canonical); err != nil {
		return nil, fmt.Errorf("canonicalize credential admission attestation payload: %w", err)
	}
	canonicalRaw, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical credential admission attestation payload: %w", err)
	}
	return canonicalRaw, nil
}

// LoadOrCreateKey returns the stable attestation key stored in secretKey. It
// safely handles multiple controller replicas racing during first startup and
// refuses malformed existing key material instead of silently invalidating all
// previously admitted AgentDeployments.
func LoadOrCreateKey(ctx context.Context, c client.Client, secretKey types.NamespacedName) ([]byte, error) {
	for attempt := 0; attempt < keySetupAttempts; attempt++ {
		var secret corev1.Secret
		err := c.Get(ctx, secretKey, &secret)
		if apierrors.IsNotFound(err) {
			key, genErr := generateKey()
			if genErr != nil {
				return nil, genErr
			}
			immutable := true
			secret = corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace},
				Immutable:  &immutable,
				Data:       map[string][]byte{SecretDataKey: key},
			}
			if createErr := c.Create(ctx, &secret); createErr != nil {
				if apierrors.IsAlreadyExists(createErr) {
					continue
				}
				return nil, fmt.Errorf("create webhook Secret with credential admission key: %w", createErr)
			}
			return key, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read webhook Secret for credential admission key: %w", err)
		}

		if !secret.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("credential admission signing Secret %s/%s is being deleted", secret.Namespace, secret.Name)
		}
		if key, found := secret.Data[SecretDataKey]; found {
			if len(key) != attestationKeySize {
				return nil, fmt.Errorf("webhook Secret %s/%s contains malformed %s: expected %d bytes, got %d",
					secret.Namespace, secret.Name, SecretDataKey, attestationKeySize, len(key))
			}
			if secret.Immutable != nil && *secret.Immutable {
				return append([]byte(nil), key...), nil
			}
			base := secret.DeepCopy()
			immutable := true
			secret.Immutable = &immutable
			if patchErr := c.Patch(ctx, &secret,
				client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); patchErr != nil {
				if apierrors.IsConflict(patchErr) || apierrors.IsNotFound(patchErr) {
					continue
				}
				return nil, fmt.Errorf("make credential admission signing Secret immutable: %w", patchErr)
			}
			return append([]byte(nil), key...), nil
		}
		if secret.Immutable != nil && *secret.Immutable {
			return nil, fmt.Errorf("immutable credential admission signing Secret %s/%s is missing %s",
				secret.Namespace, secret.Name, SecretDataKey)
		}

		key, genErr := generateKey()
		if genErr != nil {
			return nil, genErr
		}
		base := secret.DeepCopy()
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data[SecretDataKey] = key
		immutable := true
		secret.Immutable = &immutable
		if patchErr := c.Patch(ctx, &secret,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); patchErr != nil {
			if apierrors.IsConflict(patchErr) || apierrors.IsNotFound(patchErr) {
				continue
			}
			return nil, fmt.Errorf("persist credential admission key in webhook Secret: %w", patchErr)
		}
		return key, nil
	}
	return nil, fmt.Errorf("could not initialize credential admission key in webhook Secret %s/%s after %d attempts",
		secretKey.Namespace, secretKey.Name, keySetupAttempts)
}

func generateKey() ([]byte, error) {
	key := make([]byte, attestationKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate credential admission attestation key: %w", err)
	}
	return key, nil
}
