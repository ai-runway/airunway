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

// secretAccessReviewer answers "may this user read that Secret?" on behalf of
// the admission webhook. It is an interface so tests can drive both answers
// without an API server.
type secretAccessReviewer interface {
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
// Checked on create, and on update only when the reference changes: re-checking
// an unchanged reference would make routine edits fail for anyone who did not
// personally create the agent.
func (v *AgentDeploymentCustomValidator) validateCredentialAccess(
	ctx context.Context,
	oldObj, newObj *airunwayv1alpha1.AgentDeployment,
) field.ErrorList {
	ref := credentialsRefOf(newObj)
	if ref == nil {
		return nil
	}
	if oldObj != nil && sameCredentialsRef(credentialsRefOf(oldObj), ref) {
		return nil
	}

	path := field.NewPath("spec", "model", "externalAPI", "credentialsRef", "name")

	// No reviewer wired (unit tests constructing the validator directly) means
	// there is nothing to enforce against; the manager always wires one.
	if v.SecretAccess == nil {
		return nil
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

func sameCredentialsRef(a, b *airunwayv1alpha1.SecretKeyRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name && a.Key == b.Key
}
