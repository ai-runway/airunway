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
	"fmt"

	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// SecretAccessReviewer answers "may this user read that Secret?" on behalf of
// the admission webhook.
//
// Exported because AgentDeploymentCustomValidator.SecretAccess is exported: an
// exported field whose type is unexported cannot be set from another package,
// which forced callers (including tests) to leave it nil — and a nil reviewer
// used to mean "skip the check entirely".
type SecretAccessReviewer interface {
	CanGetSecret(ctx context.Context, user admission.Request, namespace, name string) (allowed bool, reason string, err error)
}

// sarReviewer asks the API server via SubjectAccessReview.
type sarReviewer struct{ client client.Client }

func (r *sarReviewer) CanGetSecret(ctx context.Context, req admission.Request, namespace, name string) (bool, string, error) {
	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   req.UserInfo.Username,
			UID:    req.UserInfo.UID,
			Groups: req.UserInfo.Groups,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "get",
				Group:     "",
				Resource:  "secrets",
				Name:      name,
			},
		},
	}
	// Extra is map[string]ExtraValue on the review, but authentication.ExtraValue
	// on the request; convert so impersonation and OIDC claims are preserved.
	if len(req.UserInfo.Extra) > 0 {
		sar.Spec.Extra = make(map[string]authzv1.ExtraValue, len(req.UserInfo.Extra))
		for k, v := range req.UserInfo.Extra {
			sar.Spec.Extra[k] = authzv1.ExtraValue(v)
		}
	}

	if err := r.client.Create(ctx, sar); err != nil {
		return false, "", err
	}
	return sar.Status.Allowed, sar.Status.Reason, nil
}

// validateCredentialAccess stops an AgentDeployment author borrowing the
// controller's privilege to read a Secret they cannot read themselves.
//
// The controller is a confused deputy here. It validates the referenced Secret
// with its own credentials and then injects it into a container whose image the
// same author chose (spec.config.image is arbitrary), so a caller holding only
// `create agentdeployments` — no `get secrets`, no `create pods` — could name
// any Secret in the namespace and read its value out of their own container.
//
// So the requester must be able to `get` the Secret they are referencing. That
// makes the reference a delegation of access the caller already holds, rather
// than an escalation.
//
// Checked on *every* create and update that carries a reference — not only when
// the reference itself changes. An earlier version skipped unchanged references
// so that routine edits would not fail for anyone who had not personally created
// the agent, and that left the whole escalation open through update: keep the
// reference identical and repoint `spec.config.image` at your own image, or
// `externalAPI.baseURL` at your own endpoint, and the controller injects the
// existing credential into attacker-controlled code.
//
// Since no cheap, durable line separates the fields that can weaponise a
// credential from the ones that cannot, authorization is required for any edit
// while a reference is present. Being unable to edit a credential-bearing agent
// without read access to its credential is the intended policy, not a
// regression.
func (v *AgentDeploymentCustomValidator) validateCredentialAccess(
	ctx context.Context,
	newObj *airunwayv1alpha1.AgentDeployment,
) field.ErrorList {
	ref := credentialsRefOf(newObj)
	if ref == nil {
		return nil
	}

	path := field.NewPath("spec", "model", "externalAPI", "credentialsRef", "name")

	// An unwired reviewer is a configuration bug, not a reason to skip. This
	// used to return nil "because the manager always wires one" — which meant
	// deleting the wiring in SetupAgentDeploymentWebhookWithManager silently
	// disabled the entire check and no test failed. Fail closed instead, so a
	// missing reviewer is loud at the first credential-bearing request rather
	// than reopening the escalation quietly.
	if v.SecretAccess == nil {
		return field.ErrorList{field.InternalError(path,
			fmt.Errorf("no Secret authorizer is configured, so this reference cannot be authorized; this is a controller wiring bug"))}
	}

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		// No admission request in context means this is not a real admission
		// call. Fail closed rather than silently skipping an authorization
		// check — a security control that quietly no-ops is worse than absent.
		return field.ErrorList{field.InternalError(path,
			fmt.Errorf("cannot authorize the Secret reference: no admission request in context: %w", err))}
	}

	allowed, reason, err := v.SecretAccess.CanGetSecret(ctx, req, newObj.Namespace, ref.Name)
	if err != nil {
		return field.ErrorList{field.InternalError(path,
			fmt.Errorf("could not verify access to Secret %q: %w", ref.Name, err))}
	}
	if !allowed {
		msg := fmt.Sprintf(
			"user %q is not permitted to get Secret %q in namespace %q, so it cannot be referenced here. "+
				"Referencing a Secret causes the controller to inject it into this agent's container; "+
				"allowing that without read access would let this resource be used to read Secrets you cannot otherwise see",
			req.UserInfo.Username, ref.Name, newObj.Namespace)
		if reason != "" {
			msg = fmt.Sprintf("%s (authorizer: %s)", msg, reason)
		}
		return field.ErrorList{field.Forbidden(path, msg)}
	}
	return nil
}

func credentialsRefOf(ad *airunwayv1alpha1.AgentDeployment) *airunwayv1alpha1.SecretKeyRef {
	if ad == nil || ad.Spec.Model.ExternalAPI == nil {
		return nil
	}
	return ad.Spec.Model.ExternalAPI.CredentialsRef
}
