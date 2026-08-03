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

package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/agentprovider"
)

func ownershipScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(airunwayv1alpha1.AddToScheme(s))
	return s
}

func ownerAgentDeployment(name string) *airunwayv1alpha1.AgentDeployment {
	return &airunwayv1alpha1.AgentDeployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: airunwayv1alpha1.GroupVersion.String(), Kind: "AgentDeployment"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(name + "-uid")},
	}
}

func TestVerifyOwnedOrAbsent(t *testing.T) {
	s := ownershipScheme(t)
	ad := ownerAgentDeployment("agent")

	// Owned by ad → allowed.
	owned := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "ns"}}
	if err := controllerutil.SetControllerReference(ad, owned, s); err != nil {
		t.Fatalf("set owner ref: %v", err)
	}
	// Unrelated object with the same name → must be rejected.
	unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "ns"}}

	t.Run("absent is allowed", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).Build()
		desired := &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "ns"},
		}
		if err := agentprovider.VerifyOwnedOrAbsent(context.Background(), c, s, ad, desired); err != nil {
			t.Fatalf("expected absent object to be allowed, got %v", err)
		}
	})

	t.Run("owned is allowed", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(owned).Build()
		desired := &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "ns"},
		}
		if err := agentprovider.VerifyOwnedOrAbsent(context.Background(), c, s, ad, desired); err != nil {
			t.Fatalf("expected owned object to be allowed, got %v", err)
		}
	})

	t.Run("unrelated is rejected", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(unrelated).Build()
		desired := &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "ns"},
		}
		if err := agentprovider.VerifyOwnedOrAbsent(context.Background(), c, s, ad, desired); err == nil {
			t.Fatal("expected unowned object to be rejected")
		}
	})
}

func TestDeleteOwnedObject(t *testing.T) {
	s := ownershipScheme(t)
	ad := ownerAgentDeployment("agent")

	t.Run("owned is deleted", func(t *testing.T) {
		owned := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
		if err := controllerutil.SetControllerReference(ad, owned, s); err != nil {
			t.Fatalf("set owner ref: %v", err)
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(owned).Build()

		target := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
		if err := agentprovider.DeleteOwned(context.Background(), c, ad, target); err != nil {
			t.Fatalf("delete owned: %v", err)
		}
		got := &corev1.Service{}
		err := c.Get(context.Background(), types.NamespacedName{Name: "agent", Namespace: "ns"}, got)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected owned service to be deleted, got err=%v", err)
		}
	})

	t.Run("unowned is left intact", func(t *testing.T) {
		unrelated := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(unrelated).Build()

		target := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
		if err := agentprovider.DeleteOwned(context.Background(), c, ad, target); err != nil {
			t.Fatalf("delete unowned should be a no-op, got %v", err)
		}
		got := &corev1.Service{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "agent", Namespace: "ns"}, got); err != nil {
			t.Fatalf("expected unowned service to be left intact, got %v", err)
		}
	})

	t.Run("absent is a no-op", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(s).Build()
		target := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
		if err := agentprovider.DeleteOwned(context.Background(), c, ad, target); err != nil {
			t.Fatalf("delete absent should be a no-op, got %v", err)
		}
	})
}

// TestEnsureBindingCredentialsRefusesToAdoptForeignSecret is the regression test
// for a real, reproduced adoption path.
//
// The keyless Secret name is derived from the agent name, so it can collide with
// one a user already manages. An earlier version relied on the unforced
// server-side apply to prevent adoption, on the theory that SSA would report a
// conflict. It does not: SSA conflicts only on fields another manager owns AND
// this apply changes — adding a new data key, new labels and a new
// ownerReferences entry to an unowned Secret is all "added", so the apply
// succeeded and attached a controller reference. Deleting the AgentDeployment
// would then garbage-collect the user's Secret, which is deletion power the
// manager's RBAC does not otherwise grant (it has no `delete` on secrets).
func TestEnsureBindingCredentialsRefusesToAdoptForeignSecret(t *testing.T) {
	scheme := ownershipScheme(t)
	ad := ownerAgentDeployment("svc")
	secretName := agentprovider.KeylessCredentialSecretName(ad.Name)

	// A Secret the agent author does not own, sitting at the derived name.
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ad.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ad, foreign).Build()

	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://m/v1", ModelName: "m"}
	_, err := agentprovider.EnsureBindingCredentials(context.Background(), c, scheme, ad, binding, "airunway-agents-kagent")
	if err == nil {
		t.Fatal("a pre-existing Secret this AgentDeployment does not own must not be adopted")
	}

	// And it must be left completely untouched.
	var after corev1.Secret
	if getErr := c.Get(context.Background(), types.NamespacedName{Name: secretName, Namespace: ad.Namespace}, &after); getErr != nil {
		t.Fatalf("the foreign Secret should still exist: %v", getErr)
	}
	if _, injected := after.Data[agentprovider.KeylessCredentialKey]; injected {
		t.Error("a key was injected into a Secret owned by someone else")
	}
	if len(after.OwnerReferences) != 0 {
		t.Errorf("an owner reference was attached, which would garbage-collect the user's Secret: %v", after.OwnerReferences)
	}
}

// The normal path must still work: absent means create, already-ours means keep.
func TestEnsureBindingCredentialsCreatesAndIsIdempotent(t *testing.T) {
	scheme := ownershipScheme(t)
	ad := ownerAgentDeployment("svc")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ad).Build()
	binding := airunwayv1alpha1.ModelBindingStatus{BaseURL: "http://m/v1", ModelName: "m"}

	out, err := agentprovider.EnsureBindingCredentials(context.Background(), c, scheme, ad, binding, "airunway-agents-kagent")
	if err != nil {
		t.Fatalf("creating the keyless Secret must succeed when nothing is in the way: %v", err)
	}
	if out.CredentialsRef == nil || out.CredentialsRef.Name != agentprovider.KeylessCredentialSecretName(ad.Name) {
		t.Fatalf("binding should reference the created Secret, got %+v", out.CredentialsRef)
	}

	// Second pass must not trip its own ownership check.
	if _, err := agentprovider.EnsureBindingCredentials(context.Background(), c, scheme, ad, binding, "airunway-agents-kagent"); err != nil {
		t.Errorf("re-reconciling must not refuse a Secret this AgentDeployment already owns: %v", err)
	}
}

// TestDeleteOwnedUsesBackgroundPropagation pins the delete policy.
//
// batch/v1 Job is the one kind here whose server-side default is
// OrphanDependents, kept for backwards compatibility. Deleting a Job without an
// explicit policy strips the ownerReferences from its pods and removes only the
// Job — the pods keep running, and having lost their owner are never collected
// when the AgentDeployment goes away. That turns teardown into a leak in exactly
// the case it exists for: stopping an agent that still holds a revoked
// credential.
func TestDeleteOwnedUsesBackgroundPropagation(t *testing.T) {
	scheme := ownershipScheme(t)
	ad := ownerAgentDeployment("jobagent")

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace}}
	if err := controllerutil.SetControllerReference(ad, job, scheme); err != nil {
		t.Fatalf("set owner: %v", err)
	}

	var seen []client.DeleteOption
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ad, job).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				seen = opts
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()

	target := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: ad.Name, Namespace: ad.Namespace}}
	if err := agentprovider.DeleteOwned(context.Background(), c, ad, target); err != nil {
		t.Fatalf("DeleteOwned: %v", err)
	}

	var policy *metav1.DeletionPropagation
	for _, o := range seen {
		if d, ok := o.(*client.DeleteOptions); ok && d.PropagationPolicy != nil {
			policy = d.PropagationPolicy
		}
	}
	if policy == nil {
		t.Fatal("no propagation policy was set — a Job delete would orphan its pods")
	}
	if *policy != metav1.DeletePropagationBackground {
		t.Errorf("propagation policy = %v, want Background", *policy)
	}
}
