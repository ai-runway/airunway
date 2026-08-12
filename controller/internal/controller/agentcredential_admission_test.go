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

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestVerifyAgentCredentialAdmission(t *testing.T) {
	valid := func() *admissionv1.ValidatingWebhookConfiguration {
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
	newReader := func(objects ...runtime.Object) *fake.ClientBuilder {
		scheme := runtime.NewScheme()
		if err := admissionv1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...)
	}

	t.Run("accepts the installed fail-closed AgentDeployment rule", func(t *testing.T) {
		reader := newReader(valid()).Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err != nil {
			t.Fatalf("VerifyAgentCredentialAdmission: %v", err)
		}
	})

	t.Run("fails closed when the configuration is absent", func(t *testing.T) {
		reader := newReader().Build()
		if err := VerifyAgentCredentialAdmission(context.Background(), reader); err == nil {
			t.Fatal("missing admission configuration must not authorize credential resolution")
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
}
