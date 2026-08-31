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

package v1alpha1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/internal/credentialadmission"
)

// fakeReviewer stands in for SubjectAccessReview.
type fakeReviewer struct {
	allowed bool
	err     error
	asked   []string // "user/namespace/secret", so we can assert WHO was checked
}

type fakeCredentialAdmissionRecorder struct {
	err      error
	recorded []*airunwayv1alpha1.AgentDeployment
}

func (f *fakeCredentialAdmissionRecorder) PersistCreate(_ context.Context, ad *airunwayv1alpha1.AgentDeployment) error {
	if f.err != nil {
		return f.err
	}
	f.recorded = append(f.recorded, ad.DeepCopy())
	return nil
}

func (f *fakeReviewer) CanGetSecret(_ context.Context, req admission.Request, ns, name string) (bool, string, error) {
	f.asked = append(f.asked, req.UserInfo.Username+"/"+ns+"/"+name)
	if f.err != nil {
		return false, "", f.err
	}
	return f.allowed, "fake authorizer", nil
}

func requestAs(user string) context.Context {
	return requestAsOperation(user, admissionv1.Create, nil, nil)
}

func requestAsOperation(
	user string,
	operation admissionv1.Operation,
	oldObj *airunwayv1alpha1.AgentDeployment,
	dryRun *bool,
) context.Context {
	var oldRaw runtime.RawExtension
	if oldObj != nil {
		raw, err := json.Marshal(oldObj)
		if err != nil {
			panic(err)
		}
		oldRaw.Raw = raw
	}
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: operation,
			OldObject: oldRaw,
			DryRun:    dryRun,
			UserInfo:  authnv1.UserInfo{Username: user, Groups: []string{"system:authenticated"}},
		},
	})
}

func agentWithSecret(secret string) *airunwayv1alpha1.AgentDeployment {
	return &airunwayv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "exfil", Namespace: "team-a"},
		Spec: airunwayv1alpha1.AgentDeploymentSpec{
			Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "crewai"},
			Model: airunwayv1alpha1.ModelBinding{
				ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
					Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
					BaseURL:   "https://attacker.example/v1",
					ModelName: "x",
					CredentialsRef: &airunwayv1alpha1.SecretKeyRef{
						Name: secret, Key: "password",
					},
				},
			},
		},
	}
}

// TestCredentialAccessBlocksPrivilegeEscalation is the regression test for the
// confused deputy: the controller validates a referenced Secret with ITS
// privilege and injects it into a container the same author chose, so without
// this check a caller holding only `create agentdeployments` could read any
// Secret in the namespace.
func TestCredentialAccessBlocksPrivilegeEscalation(t *testing.T) {
	reviewer := &fakeReviewer{allowed: false}
	v := &AgentDeploymentCustomValidator{SecretAccess: reviewer}

	_, err := v.ValidateCreate(requestAs("mallory"), agentWithSecret("prod-db-password"))
	if err == nil {
		t.Fatal("a user who cannot read the Secret must not be able to reference it — this is the escalation path")
	}
	for _, want := range []string{"prod-db-password", "mallory", "not permitted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q so the user understands why; got: %v", want, err)
		}
	}
	// The check must authorize the REQUESTER, not the controller.
	if len(reviewer.asked) != 1 || reviewer.asked[0] != "mallory/team-a/prod-db-password" {
		t.Errorf("wrong subject authorized: %v", reviewer.asked)
	}
}

func TestCredentialAccessAllowsAuthorizedUser(t *testing.T) {
	recorder := &fakeCredentialAdmissionRecorder{}
	v := &AgentDeploymentCustomValidator{
		SecretAccess:      &fakeReviewer{allowed: true},
		CredentialRecords: recorder,
	}
	if _, err := v.ValidateCreate(requestAs("alice"), agentWithSecret("openai-api-key")); err != nil {
		t.Fatalf("a user who CAN read the Secret must be allowed to reference it: %v", err)
	}
	if len(recorder.recorded) != 1 {
		t.Fatalf("authorized CREATE persisted %d credential records, want 1", len(recorder.recorded))
	}
}

func TestCredentialCreateRecordSideEffects(t *testing.T) {
	t.Run("record persistence failure rejects CREATE", func(t *testing.T) {
		recorder := &fakeCredentialAdmissionRecorder{err: errors.New("record store unavailable")}
		v := &AgentDeploymentCustomValidator{
			SecretAccess:      &fakeReviewer{allowed: true},
			CredentialRecords: recorder,
		}
		if _, err := v.ValidateCreate(requestAs("alice"), agentWithSecret("openai-api-key")); err == nil {
			t.Fatal("credential-bearing CREATE must fail closed when its UID-bound record cannot be persisted")
		}
	})

	t.Run("dry-run performs no record write", func(t *testing.T) {
		recorder := &fakeCredentialAdmissionRecorder{}
		v := &AgentDeploymentCustomValidator{
			SecretAccess:      &fakeReviewer{allowed: true},
			CredentialRecords: recorder,
		}
		dryRun := true
		ctx := requestAsOperation("alice", admissionv1.Create, nil, &dryRun)
		if _, err := v.ValidateCreate(ctx, agentWithSecret("openai-api-key")); err != nil {
			t.Fatalf("dry-run CREATE should validate without a persisted side effect: %v", err)
		}
		if len(recorder.recorded) != 0 {
			t.Fatalf("dry-run CREATE persisted %d records, want 0", len(recorder.recorded))
		}
	})
}

//nolint:goconst // Repeated user/resource identities are the authorization boundary under test.
func TestCredentialDefaulterAttestsOnlyAuthorizedRequests(t *testing.T) {
	attestor, err := credentialadmission.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("authorized CREATE removes a caller proof until UID-bound validation", func(t *testing.T) {
		reviewer := &fakeReviewer{allowed: true}
		defaulter := &AgentDeploymentCustomDefaulter{SecretAccess: reviewer, Attestor: attestor}
		ad := agentWithSecret("openai-api-key")
		ad.Annotations = map[string]string{credentialadmission.AttestationAnnotation: "v1.forged.forged"}
		if err := defaulter.Default(requestAs("alice"), ad); err != nil {
			t.Fatalf("Default: %v", err)
		}
		if _, found := ad.Annotations[credentialadmission.AttestationAnnotation]; found {
			t.Fatal("CREATE mutation retained a caller-supplied credential proof")
		}
		if len(reviewer.asked) != 1 || reviewer.asked[0] != "alice/team-a/openai-api-key" {
			t.Fatalf("wrong subject authorized: %v", reviewer.asked)
		}
	})

	t.Run("dry-run UPDATE returns no replayable production proof", func(t *testing.T) {
		reviewer := &fakeReviewer{allowed: true}
		defaulter := &AgentDeploymentCustomDefaulter{SecretAccess: reviewer, Attestor: attestor}
		validator := &AgentDeploymentCustomValidator{SecretAccess: reviewer, Attestor: attestor}
		old := agentWithSecret("openai-api-key")
		old.UID = "uid-dry-run"
		old.Generation = 1
		updated := old.DeepCopy()
		updated.Generation = 2
		updated.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"image":"ghcr.io/example/agent:v2"}`)}
		updated.Annotations = map[string]string{credentialadmission.AttestationAnnotation: "v1.forged.forged"}
		dryRun := true
		dryRunCtx := requestAsOperation("alice", admissionv1.Update, old, &dryRun)

		if err := defaulter.Default(dryRunCtx, updated); err != nil {
			t.Fatalf("dry-run Default: %v", err)
		}
		if _, found := updated.Annotations[credentialadmission.AttestationAnnotation]; found {
			t.Fatal("dry-run UPDATE returned a reusable credential attestation")
		}
		if _, err := validator.ValidateUpdate(dryRunCtx, old, updated); err != nil {
			t.Fatalf("authorized dry-run UPDATE should validate without a production proof: %v", err)
		}
		if err := attestor.Verify(context.Background(), updated); err == nil {
			t.Fatal("dry-run UPDATE unexpectedly produced a controller-verifiable proof")
		}

		realCtx := requestAsOperation("alice", admissionv1.Update, old, nil)
		if _, err := validator.ValidateUpdate(realCtx, old, updated); err == nil {
			t.Fatal("a proof-free dry-run response was replayed as a real UPDATE")
		}
	})

	t.Run("authorized UPDATE is stamped and bound to UID generation and spec", func(t *testing.T) {
		reviewer := &fakeReviewer{allowed: true}
		defaulter := &AgentDeploymentCustomDefaulter{SecretAccess: reviewer, Attestor: attestor}
		old := agentWithSecret("openai-api-key")
		old.UID = "uid-1"
		old.Generation = 4
		ad := old.DeepCopy()
		ad.Generation = 5
		ad.Annotations = map[string]string{credentialadmission.AttestationAnnotation: "v1.forged.forged"}
		ad.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"image":"ghcr.io/example/agent:v2"}`)}
		ctx := requestAsOperation("alice", admissionv1.Update, old, nil)
		if err := defaulter.Default(ctx, ad); err != nil {
			t.Fatalf("Default: %v", err)
		}
		if err := attestor.Verify(context.Background(), ad); err != nil {
			t.Fatalf("authorized UPDATE was not durably attested: %v", err)
		}
		firstProof := ad.Annotations[credentialadmission.AttestationAnnotation]
		if err := defaulter.Default(ctx, ad); err != nil {
			t.Fatalf("reinvoked Default: %v", err)
		}
		if got := ad.Annotations[credentialadmission.AttestationAnnotation]; got != firstProof {
			t.Fatalf("reinvocation changed an unchanged object's proof: first=%q second=%q", firstProof, got)
		}
		if err := attestor.Verify(context.Background(), ad); err != nil {
			t.Fatalf("reinvoked UPDATE was not durably attested: %v", err)
		}
		if len(reviewer.asked) != 2 || reviewer.asked[0] != "alice/team-a/openai-api-key" ||
			reviewer.asked[1] != "alice/team-a/openai-api-key" {
			t.Fatalf("wrong subject authorized: %v", reviewer.asked)
		}

		ad.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"image":"ghcr.io/mallory/exfil:latest"}`)}
		if err := attestor.Verify(context.Background(), ad); err == nil {
			t.Fatal("changing the image after admission must invalidate the attestation")
		}
	})

	t.Run("unauthorized request is rejected without a proof", func(t *testing.T) {
		defaulter := &AgentDeploymentCustomDefaulter{
			SecretAccess: &fakeReviewer{allowed: false},
			Attestor:     attestor,
		}
		ad := agentWithSecret("prod-db-password")
		if err := defaulter.Default(requestAs("mallory"), ad); err == nil {
			t.Fatal("unauthorized credential reference must not be attested")
		}
		if _, found := ad.Annotations[credentialadmission.AttestationAnnotation]; found {
			t.Fatal("rejected request retained an attestation")
		}
	})

	t.Run("dropping the credential clears a stale proof", func(t *testing.T) {
		ad := agentWithSecret("openai-api-key")
		ad.UID = "uid-1"
		ad.Generation = 1
		if err := attestor.Stamp(context.Background(), ad, ad.UID, ad.Generation); err != nil {
			t.Fatal(err)
		}
		ad.Spec.Model.ExternalAPI.CredentialsRef = nil
		defaulter := &AgentDeploymentCustomDefaulter{Attestor: attestor}
		if err := defaulter.Default(context.Background(), ad); err != nil {
			t.Fatal(err)
		}
		if _, found := ad.Annotations[credentialadmission.AttestationAnnotation]; found {
			t.Fatal("keyless binding retained a credential attestation")
		}
	})
}

// A failing authorizer must not fall open: a security control that silently
// no-ops when it errors is worse than not having one.
func TestCredentialAccessFailsClosed(t *testing.T) {
	v := &AgentDeploymentCustomValidator{SecretAccess: &fakeReviewer{err: errors.New("apiserver down")}}
	if _, err := v.ValidateCreate(requestAs("alice"), agentWithSecret("openai-api-key")); err == nil {
		t.Fatal("an authorization failure must reject, not admit")
	}
}

// Without an admission request there is no subject to authorize. Skipping the
// check in that case would make the control trivially bypassable.
func TestCredentialAccessRequiresAdmissionContext(t *testing.T) {
	v := &AgentDeploymentCustomValidator{SecretAccess: &fakeReviewer{allowed: true}}
	if _, err := v.ValidateCreate(context.Background(), agentWithSecret("openai-api-key")); err == nil {
		t.Fatal("a call with no admission request must reject rather than skip the check")
	}
}

func TestCredentialAccessSkipsWhenNoSecretReferenced(t *testing.T) {
	reviewer := &fakeReviewer{allowed: false}
	v := &AgentDeploymentCustomValidator{SecretAccess: reviewer}

	// A keyless in-cluster binding references no Secret, so there is nothing to
	// authorize and no reason to cost an API call.
	ad := &airunwayv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "keyless", Namespace: "team-a"},
		Spec: airunwayv1alpha1.AgentDeploymentSpec{
			Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "crewai"},
			Model: airunwayv1alpha1.ModelBinding{
				DeploymentRef: &airunwayv1alpha1.ModelDeploymentBinding{Name: "llama"},
			},
		},
	}
	if _, err := v.ValidateCreate(requestAs("bob"), ad); err != nil {
		t.Fatalf("a binding with no credentialsRef needs no authorization: %v", err)
	}
	if len(reviewer.asked) != 0 {
		t.Errorf("no SubjectAccessReview should be issued, got %v", reviewer.asked)
	}
}

// Every update carrying a reference is authorized, not only those that change
// it. An earlier version skipped unchanged references and that left the entire
// escalation reachable through update.
func TestCredentialAccessOnUpdate(t *testing.T) {
	t.Run("unchanged reference is STILL authorized", func(t *testing.T) {
		// The regression test for the update bypass: keep the reference
		// identical and repoint the image at your own. Nothing about the
		// credentialsRef changed, but the credential now lands in a different
		// container, so the updater must be authorized against it.
		reviewer := &fakeReviewer{allowed: false}
		v := &AgentDeploymentCustomValidator{SecretAccess: reviewer}

		old := agentWithSecret("prod-db-password")
		updated := agentWithSecret("prod-db-password")
		updated.Spec.Config = &runtime.RawExtension{
			Raw: []byte(`{"image":"ghcr.io/mallory/exfil:latest"}`),
		}

		if _, err := v.ValidateUpdate(requestAs("mallory"), old, updated); err == nil {
			t.Fatal("retaining a reference while changing the image must be rejected — this is the update bypass")
		}
		if len(reviewer.asked) != 1 || !strings.HasSuffix(reviewer.asked[0], "prod-db-password") {
			t.Errorf("the retained secret must be authorized on update, got %v", reviewer.asked)
		}
	})

	t.Run("an authorized user may still edit", func(t *testing.T) {
		reviewer := &fakeReviewer{allowed: true}
		attestor, err := credentialadmission.New(bytes.Repeat([]byte{0x42}, 32))
		if err != nil {
			t.Fatal(err)
		}
		v := &AgentDeploymentCustomValidator{SecretAccess: reviewer, Attestor: attestor}
		old, updated := agentWithSecret("openai-api-key"), agentWithSecret("openai-api-key")
		old.UID, updated.UID = "uid-1", "uid-1"
		old.Generation, updated.Generation = 1, 1
		if err := attestor.Stamp(context.Background(), updated, updated.UID, updated.Generation); err != nil {
			t.Fatal(err)
		}
		if _, err := v.ValidateUpdate(requestAs("alice"), old, updated); err != nil {
			t.Fatalf("a user who can read the Secret must still be able to edit: %v", err)
		}
		if len(reviewer.asked) != 1 {
			t.Errorf("expected exactly one authorization call, got %v", reviewer.asked)
		}
	})

	t.Run("switching to another Secret is authorized", func(t *testing.T) {
		reviewer := &fakeReviewer{allowed: false}
		v := &AgentDeploymentCustomValidator{SecretAccess: reviewer}
		old, updated := agentWithSecret("openai-api-key"), agentWithSecret("prod-db-password")
		if _, err := v.ValidateUpdate(requestAs("mallory"), old, updated); err == nil {
			t.Fatal("repointing at a Secret the user cannot read must be rejected")
		}
		if len(reviewer.asked) != 1 || !strings.HasSuffix(reviewer.asked[0], "prod-db-password") {
			t.Errorf("the NEW secret must be authorized, got %v", reviewer.asked)
		}
	})

	t.Run("dropping the reference needs no authorization", func(t *testing.T) {
		reviewer := &fakeReviewer{allowed: false}
		v := &AgentDeploymentCustomValidator{SecretAccess: reviewer}
		old := agentWithSecret("prod-db-password")
		updated := agentWithSecret("prod-db-password")
		updated.Spec.Model.ExternalAPI.CredentialsRef = nil
		if _, err := v.ValidateUpdate(requestAs("bob"), old, updated); err != nil {
			t.Fatalf("removing the credential reference consumes no credential: %v", err)
		}
		if len(reviewer.asked) != 0 {
			t.Errorf("expected no authorization call, got %v", reviewer.asked)
		}
	})
}
