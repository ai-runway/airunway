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
	"bytes"
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/internal/credentialadmission"
)

func validAgentCredentialAdmissionConfiguration() *admissionv1.ValidatingWebhookConfiguration {
	path := "/validate-airunway-ai-v1alpha1-agentdeployment"
	failurePolicy := admissionv1.Fail
	return &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: agentCredentialValidatingWebhookConfiguration},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:          agentCredentialValidatingWebhookName,
			FailurePolicy: &failurePolicy,
			ClientConfig: admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{
				Name: "airunway-webhook-service", Namespace: "airunway-system", Path: &path,
			}},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
				Rule: admissionv1.Rule{
					APIGroups: []string{"airunway.ai"}, APIVersions: []string{"v1alpha1"}, Resources: []string{"agentdeployments"},
				},
			}},
		}},
	}
}

func validAgentCredentialAdmissionUpgradeGuard() *admissionv1.MutatingWebhookConfiguration {
	path := agentCredentialUpgradeGuardServicePath
	failurePolicy := admissionv1.Fail
	matchPolicy := admissionv1.Equivalent
	sideEffects := admissionv1.SideEffectClassNone
	timeoutSeconds := int32(1)
	scope := admissionv1.NamespacedScope
	return &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: agentCredentialUpgradeGuardConfiguration},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name:                    agentCredentialUpgradeGuardWebhookName,
			AdmissionReviewVersions: []string{"v1"},
			FailurePolicy:           &failurePolicy,
			MatchPolicy:             &matchPolicy,
			SideEffects:             &sideEffects,
			TimeoutSeconds:          &timeoutSeconds,
			ClientConfig: admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{
				Name: agentCredentialUpgradeGuardServiceName, Namespace: agentCredentialUpgradeGuardServiceNamespace, Path: &path,
			}},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
				Rule: admissionv1.Rule{
					APIGroups:   []string{airunwayv1alpha1.GroupVersion.Group},
					APIVersions: []string{airunwayv1alpha1.GroupVersion.Version},
					Resources:   []string{"agentdeployments"},
					Scope:       &scope,
				},
			}},
		}},
	}
}

type credentialAdmissionTestReader struct {
	client.Reader
	config *admissionv1.ValidatingWebhookConfiguration
}

func (r *credentialAdmissionTestReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if config, ok := obj.(*admissionv1.ValidatingWebhookConfiguration); ok &&
		key.Name == agentCredentialValidatingWebhookConfiguration {
		r.config.DeepCopyInto(config)
		return nil
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// newCredentialAuthorizedAgentDeploymentReconciler gives controller tests the
// real live-configuration check without installing webhooks into this envtest.
// Tests outside the admission package use a successful attestation callback so
// they can focus on binding and provider behavior; security-specific tests
// below exercise the real attestor.
func newCredentialAuthorizedAgentDeploymentReconciler(c client.Client) *AgentDeploymentReconciler {
	config := validAgentCredentialAdmissionConfiguration()
	reader := &credentialAdmissionTestReader{Reader: c, config: config}
	return &AgentDeploymentReconciler{
		Client:    c,
		Scheme:    c.Scheme(),
		APIReader: reader,
		CredentialAdmissionCheck: func(ctx context.Context, _ *airunwayv1alpha1.AgentDeployment) error {
			return VerifyAgentCredentialAdmission(ctx, reader)
		},
		CredentialAttestationCheck: func(context.Context, *airunwayv1alpha1.AgentDeployment) error { return nil },
	}
}

var _ = Describe("AgentDeployment credential admission API defaulting", func() {
	It("accepts the staged guard after the API server defaults empty selectors", func() {
		guard := validAgentCredentialAdmissionUpgradeGuard()
		Expect(k8sClient.Create(ctx, guard)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), guard)
		})

		stored := &admissionv1.MutatingWebhookConfiguration{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: guard.Name}, stored)).To(Succeed())
		Expect(stored.Webhooks).To(HaveLen(1))
		Expect(stored.Webhooks[0].NamespaceSelector).NotTo(BeNil())
		Expect(stored.Webhooks[0].ObjectSelector).NotTo(BeNil())
		Expect(stored.Webhooks[0].NamespaceSelector.MatchLabels).To(BeEmpty())
		Expect(stored.Webhooks[0].NamespaceSelector.MatchExpressions).To(BeEmpty())
		Expect(stored.Webhooks[0].ObjectSelector.MatchLabels).To(BeEmpty())
		Expect(stored.Webhooks[0].ObjectSelector.MatchExpressions).To(BeEmpty())
		Expect(VerifyAgentCredentialAdmission(ctx, k8sClient)).To(Succeed())
	})
})

func TestVerifyAgentCredentialAdmission(t *testing.T) {
	valid := validAgentCredentialAdmissionConfiguration
	newReader := func(objects ...runtime.Object) *fake.ClientBuilder {
		scheme := runtime.NewScheme()
		if err := admissionv1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).WithReturnManagedFields()
	}
	newAgentReader := func(objects ...runtime.Object) client.Client {
		scheme := runtime.NewScheme()
		if err := admissionv1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).WithReturnManagedFields().Build()
	}
	aksNamespaceSelector := func() *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "control-plane", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"true"}},
			{Key: "kubernetes.azure.com/managedby", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"aks"}},
		}}
	}

	t.Run("accepts the installed fail-closed AgentDeployment rule", func(t *testing.T) {
		reader := newReader(valid()).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err != nil {
			t.Fatalf("VerifyAgentCredentialAdmission: %v", err)
		}
	})

	t.Run("accepts the exact staged upgrade guard when the normal validator is disabled", func(t *testing.T) {
		normal := valid()
		normal.Webhooks[0].Name = "vmodeldeployment-v1alpha1.kb.io"
		reader := newReader(normal, validAgentCredentialAdmissionUpgradeGuard()).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err != nil {
			t.Fatalf("exact staged upgrade guard should authorize existing credential reconciliation: %v", err)
		}
	})

	t.Run("accepts API-defaulted empty selectors", func(t *testing.T) {
		normal := valid()
		normal.Webhooks[0].NamespaceSelector = &metav1.LabelSelector{}
		normal.Webhooks[0].ObjectSelector = &metav1.LabelSelector{}
		if err := VerifyAgentCredentialAdmission(context.Background(), newReader(normal).Build()); err != nil {
			t.Fatalf("empty match-all selectors on the normal validator should be accepted: %v", err)
		}

		normal.Webhooks[0].Name = "vmodeldeployment-v1alpha1.kb.io"
		guard := validAgentCredentialAdmissionUpgradeGuard()
		guard.Webhooks[0].NamespaceSelector = &metav1.LabelSelector{}
		guard.Webhooks[0].ObjectSelector = &metav1.LabelSelector{}
		reader := newReader(normal, guard).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err != nil {
			t.Fatalf("empty match-all selectors on the staged guard should be accepted: %v", err)
		}
	})

	t.Run("accepts a managed-cluster namespace selector for a covered AgentDeployment namespace", func(t *testing.T) {
		config := valid()
		config.Webhooks[0].NamespaceSelector = aksNamespaceSelector()
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-agents"}}
		ad := &airunwayv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
			Name: "credential-agent", Namespace: namespace.Name,
		}}
		reader := newAgentReader(config, namespace)

		if err := VerifyAgentCredentialAdmissionForAgent(context.Background(), reader, ad); err != nil {
			t.Fatalf("matching managed-cluster namespace selector should cover the AgentDeployment: %v", err)
		}
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err == nil {
			t.Fatal("manifest-level verification without a concrete namespace must remain strict")
		}
	})

	t.Run("rejects a managed-cluster namespace selector for an excluded AgentDeployment namespace", func(t *testing.T) {
		config := valid()
		config.Webhooks[0].NamespaceSelector = aksNamespaceSelector()
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "managed-agents",
			Labels: map[string]string{
				"kubernetes.azure.com/managedby": "aks",
			},
		}}
		ad := &airunwayv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
			Name: "credential-agent", Namespace: namespace.Name,
		}}

		if err := VerifyAgentCredentialAdmissionForAgent(
			context.Background(), newAgentReader(config, namespace), ad,
		); err == nil {
			t.Fatal("an excluded namespace must not authorize credential resolution")
		}
	})

	t.Run("applies namespace selector evaluation to the staged upgrade guard", func(t *testing.T) {
		normal := valid()
		normal.Webhooks[0].Name = "vmodeldeployment-v1alpha1.kb.io"
		guard := validAgentCredentialAdmissionUpgradeGuard()
		guard.Webhooks[0].NamespaceSelector = aksNamespaceSelector()
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-agents"}}
		ad := &airunwayv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
			Name: "credential-agent", Namespace: namespace.Name,
		}}

		if err := VerifyAgentCredentialAdmissionForAgent(
			context.Background(), newAgentReader(normal, guard, namespace), ad,
		); err != nil {
			t.Fatalf("matching staged guard namespace selector should cover the AgentDeployment: %v", err)
		}
	})

	t.Run("fails closed when the AgentDeployment namespace cannot be read", func(t *testing.T) {
		config := valid()
		config.Webhooks[0].NamespaceSelector = aksNamespaceSelector()
		ad := &airunwayv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
			Name: "credential-agent", Namespace: "missing-namespace",
		}}

		if err := VerifyAgentCredentialAdmissionForAgent(
			context.Background(), newAgentReader(config), ad,
		); err == nil {
			t.Fatal("a namespace read failure must not authorize credential resolution")
		}
	})

	t.Run("rejects the legacy validating guard because validators can run in parallel", func(t *testing.T) {
		normal := valid()
		normal.Webhooks[0].Name = "vmodeldeployment-v1alpha1.kb.io"
		guard := validAgentCredentialAdmissionUpgradeGuard()
		legacy := &admissionv1.ValidatingWebhookConfiguration{
			ObjectMeta: guard.ObjectMeta,
			Webhooks: []admissionv1.ValidatingWebhook{{
				Name:                    guard.Webhooks[0].Name,
				ClientConfig:            guard.Webhooks[0].ClientConfig,
				Rules:                   guard.Webhooks[0].Rules,
				FailurePolicy:           guard.Webhooks[0].FailurePolicy,
				MatchPolicy:             guard.Webhooks[0].MatchPolicy,
				NamespaceSelector:       guard.Webhooks[0].NamespaceSelector,
				ObjectSelector:          guard.Webhooks[0].ObjectSelector,
				SideEffects:             guard.Webhooks[0].SideEffects,
				TimeoutSeconds:          guard.Webhooks[0].TimeoutSeconds,
				AdmissionReviewVersions: guard.Webhooks[0].AdmissionReviewVersions,
				MatchConditions:         guard.Webhooks[0].MatchConditions,
			}},
		}
		reader := newReader(normal, legacy).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err == nil {
			t.Fatal("a validating guard must not authorize credential resolution")
		}
	})

	t.Run("fails closed when the configuration is absent", func(t *testing.T) {
		reader := newReader().Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err == nil {
			t.Fatal("missing admission configuration must not authorize credential resolution")
		}
	})

	t.Run("a live configuration check cannot bypass a missing attestation verifier", func(t *testing.T) {
		reader := newReader().Build()
		r := &AgentDeploymentReconciler{
			Client: reader,
			CredentialAdmissionCheck: func(context.Context, *airunwayv1alpha1.AgentDeployment) error {
				return nil
			},
		}
		ad := &airunwayv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
			Name:      "credential-agent",
			Namespace: "default",
		}}
		model := &airunwayv1alpha1.ModelBinding{ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
			Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
			BaseURL:   "https://api.openai.com/v1",
			ModelName: "gpt-4o-mini",
			CredentialsRef: &airunwayv1alpha1.SecretKeyRef{
				Name: "openai-creds",
				Key:  "token",
			},
		}}

		_, ok, requeue, reason, _ := r.resolveExternalAPI(
			context.Background(), ad, model, airunwayv1alpha1.ModelBindingStatus{},
		)
		if ok || !requeue || reason != "CredentialAuthorizationUnavailable" {
			t.Fatalf("configuration-only authorization resolved as ok=%v requeue=%v reason=%q", ok, requeue, reason)
		}
	})

	t.Run("rejects a forged attestation even with future managed fields", func(t *testing.T) {
		attestor, err := credentialadmission.New(bytes.Repeat([]byte{0x42}, 32))
		if err != nil {
			t.Fatal(err)
		}
		reader := newReader(valid()).Build()
		r := &AgentDeploymentReconciler{
			Client: reader,
			CredentialAdmissionCheck: func(ctx context.Context, _ *airunwayv1alpha1.AgentDeployment) error {
				return VerifyAgentCredentialAdmission(ctx, reader)
			},
			CredentialAttestationCheck: func(ctx context.Context, ad *airunwayv1alpha1.AgentDeployment) error {
				return attestor.Verify(ctx, ad)
			},
		}
		future := metav1.NewTime(time.Now().Add(365 * 24 * time.Hour))
		ad := &airunwayv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{
			Name:      "forged-credential-agent",
			Namespace: "default",
			Annotations: map[string]string{
				credentialadmission.AttestationAnnotation: "v1.forged.forged",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "attacker", Time: &future}},
		}}
		model := &airunwayv1alpha1.ModelBinding{ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
			Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
			BaseURL:   "https://api.openai.com/v1",
			ModelName: "gpt-4o-mini",
			CredentialsRef: &airunwayv1alpha1.SecretKeyRef{
				Name: "openai-creds",
				Key:  "token",
			},
		}}

		_, ok, requeue, reason, _ := r.resolveExternalAPI(
			context.Background(), ad, model, airunwayv1alpha1.ModelBindingStatus{},
		)
		if ok || !requeue || reason != "CredentialAuthorizationUnavailable" {
			t.Fatalf("forged attestation resolved as ok=%v requeue=%v reason=%q", ok, requeue, reason)
		}
	})

	t.Run("rejects a fail-open rule", func(t *testing.T) {
		config := valid()
		ignore := admissionv1.Ignore
		config.Webhooks[0].FailurePolicy = &ignore
		reader := newReader(config).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err == nil {
			t.Fatal("failurePolicy Ignore must not authorize credential resolution")
		}
	})

	t.Run("accepts an omitted failure policy because v1 defaults it to Fail", func(t *testing.T) {
		config := valid()
		config.Webhooks[0].FailurePolicy = nil
		reader := newReader(config).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err != nil {
			t.Fatalf("the v1 fail-closed default should authorize credential resolution: %v", err)
		}
	})

	t.Run("rejects namespace exclusions", func(t *testing.T) {
		config := valid()
		config.Webhooks[0].NamespaceSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"airunway.ai/credential-admission": "enabled"},
		}
		reader := newReader(config).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err == nil {
			t.Fatal("namespaceSelector must not authorize credential resolution")
		}
	})

	t.Run("rejects object exclusions", func(t *testing.T) {
		config := valid()
		config.Webhooks[0].ObjectSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"airunway.ai/credential-admission": "enabled"},
		}
		reader := newReader(config).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err == nil {
			t.Fatal("objectSelector must not authorize credential resolution")
		}
	})

	t.Run("rejects conditional matching", func(t *testing.T) {
		config := valid()
		config.Webhooks[0].MatchConditions = []admissionv1.MatchCondition{{
			Name:       "only-selected-agents",
			Expression: "object.metadata.labels['airunway.ai/credential-admission'] == 'enabled'",
		}}
		reader := newReader(config).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err == nil {
			t.Fatal("matchConditions must not authorize credential resolution")
		}
	})

	t.Run("rejects a cluster-only resource rule", func(t *testing.T) {
		config := valid()
		cluster := admissionv1.ClusterScope
		config.Webhooks[0].Rules[0].Rule.Scope = &cluster
		reader := newReader(config).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err == nil {
			t.Fatal("scope Cluster cannot cover namespaced AgentDeployments")
		}
	})

	t.Run("accepts create and update coverage split across namespaced rules", func(t *testing.T) {
		config := valid()
		namespaced := admissionv1.NamespacedScope
		config.Webhooks[0].Rules = []admissionv1.RuleWithOperations{
			{
				Operations: []admissionv1.OperationType{admissionv1.Create},
				Rule: admissionv1.Rule{
					APIGroups: []string{"airunway.ai"}, APIVersions: []string{"v1alpha1"},
					Resources: []string{"agentdeployments"}, Scope: &namespaced,
				},
			},
			{
				Operations: []admissionv1.OperationType{admissionv1.Update},
				Rule: admissionv1.Rule{
					APIGroups: []string{"airunway.ai"}, APIVersions: []string{"v1alpha1"},
					Resources: []string{"agentdeployments"}, Scope: &namespaced,
				},
			},
		}
		reader := newReader(config).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err != nil {
			t.Fatalf("split CREATE/UPDATE rules should cover AgentDeployments: %v", err)
		}
	})

	t.Run("rejects weakened or non-exact staged upgrade guards", func(t *testing.T) {
		normal := valid()
		normal.Webhooks[0].Name = "vmodeldeployment-v1alpha1.kb.io"
		tests := []struct {
			name   string
			mutate func(*admissionv1.MutatingWebhookConfiguration)
		}{
			{
				name: "additional webhook",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					config.Webhooks = append(config.Webhooks, config.Webhooks[0])
				},
			},
			{
				name: "fail-open policy",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					ignore := admissionv1.Ignore
					config.Webhooks[0].FailurePolicy = &ignore
				},
			},
			{
				name: "omitted explicit fail policy",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					config.Webhooks[0].FailurePolicy = nil
				},
			},
			{
				name: "conditional selector",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					config.Webhooks[0].ObjectSelector = &metav1.LabelSelector{
						MatchLabels: map[string]string{"airunway.ai/credential-admission": "enabled"},
					}
				},
			},
			{
				name: "conditional match expression",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					config.Webhooks[0].MatchConditions = []admissionv1.MatchCondition{{Name: "skip", Expression: "false"}}
				},
			},
			{
				name: "wrong match policy",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					exact := admissionv1.Exact
					config.Webhooks[0].MatchPolicy = &exact
				},
			},
			{
				name: "wrong side effects",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					noneOnDryRun := admissionv1.SideEffectClassNoneOnDryRun
					config.Webhooks[0].SideEffects = &noneOnDryRun
				},
			},
			{
				name: "wrong timeout",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					two := int32(2)
					config.Webhooks[0].TimeoutSeconds = &two
				},
			},
			{
				name: "wrong admission review version",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					config.Webhooks[0].AdmissionReviewVersions = []string{"v1beta1"}
				},
			},
			{
				name: "wrong service target",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					config.Webhooks[0].ClientConfig.Service.Name = "webhook-service"
				},
			},
			{
				name: "custom service port",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					port := int32(9443)
					config.Webhooks[0].ClientConfig.Service.Port = &port
				},
			},
			{
				name: "missing update coverage",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					config.Webhooks[0].Rules[0].Operations = []admissionv1.OperationType{admissionv1.Create}
				},
			},
			{
				name: "wildcard resource coverage",
				mutate: func(config *admissionv1.MutatingWebhookConfiguration) {
					config.Webhooks[0].Rules[0].Rule.Resources = []string{"*"}
				},
			},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				guard := validAgentCredentialAdmissionUpgradeGuard()
				test.mutate(guard)
				reader := newReader(normal.DeepCopy(), guard).Build()
				if err := VerifyAgentCredentialAdmission(context.Background(), reader); err == nil {
					t.Fatal("non-exact staged upgrade guard must not authorize credential resolution")
				}
			})
		}
	})

}

func TestDeletingBindingTargetsAreNotResolved(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := airunwayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	deletingAt := metav1.NewTime(time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC))

	t.Run("ModelDeployment", func(t *testing.T) {
		md := &airunwayv1alpha1.ModelDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "deleting-model",
				Namespace:         "default",
				DeletionTimestamp: &deletingAt,
				Finalizers:        []string{"test.airunway.ai/hold"},
			},
			Status: airunwayv1alpha1.ModelDeploymentStatus{
				Endpoint: &airunwayv1alpha1.EndpointStatus{Service: "still-published", Port: 80},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).Build()
		r := &AgentDeploymentReconciler{Client: c}
		ad := &airunwayv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"}}
		model := &airunwayv1alpha1.ModelBinding{DeploymentRef: &airunwayv1alpha1.ModelDeploymentBinding{Name: md.Name}}

		_, ok, requeue, reason, _ := r.resolveDeploymentRef(
			context.Background(), ad, model,
			airunwayv1alpha1.ModelBindingStatus{BindingMode: airunwayv1alpha1.ModelBindingModeDeploymentRef},
		)
		if ok || !requeue || reason != "ModelDeploymentDeleting" {
			t.Fatalf("deleting ModelDeployment resolved as ok=%v requeue=%v reason=%q", ok, requeue, reason)
		}
	})

	t.Run("Gateway", func(t *testing.T) {
		gateway := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "Gateway",
			"metadata": map[string]interface{}{
				"name":              "deleting-gateway",
				"namespace":         "default",
				"deletionTimestamp": deletingAt.Format(time.RFC3339),
				"finalizers":        []interface{}{"test.airunway.ai/hold"},
			},
			"status": map[string]interface{}{
				"addresses": []interface{}{map[string]interface{}{"value": "10.0.0.1"}},
			},
		}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gateway).Build()
		r := &AgentDeploymentReconciler{Client: c}
		ad := &airunwayv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"}}
		model := &airunwayv1alpha1.ModelBinding{GatewayEndpoint: &airunwayv1alpha1.GatewayEndpointBinding{
			GatewayRef: airunwayv1alpha1.GatewayResourceRef{Name: gateway.GetName()},
			ModelName:  "model",
		}}

		_, ok, requeue, reason, _ := r.resolveGatewayEndpointBinding(
			context.Background(), ad, model,
			airunwayv1alpha1.ModelBindingStatus{BindingMode: airunwayv1alpha1.ModelBindingModeGatewayEndpoint},
		)
		if ok || !requeue || reason != "GatewayDeleting" {
			t.Fatalf("deleting Gateway resolved as ok=%v requeue=%v reason=%q", ok, requeue, reason)
		}
	})
}

func TestProviderReadyForGenerationRequiresTrackedGeneration(t *testing.T) {
	conditions := []metav1.Condition{{
		Type:               airunwayv1alpha1.AgentConditionTypeProviderReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 0,
	}}
	if providerReadyForGeneration(conditions, 1) {
		t.Fatal("generation-zero ProviderReady must not make a generation-one AgentDeployment ready")
	}

	conditions[0].ObservedGeneration = 1
	if !providerReadyForGeneration(conditions, 1) {
		t.Fatal("ProviderReady observed for the current generation should be accepted")
	}
}
