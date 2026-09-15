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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/internal/credentialadmission"
)

var _ = Describe("AgentDeployment credential attestation webhook", func() {
	It("replaces a forged proof and persists a verifiable admission attestation", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "attestation-integration-key", Namespace: "default"},
			Data:       map[string][]byte{"token": []byte("test-only")},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })

		ad := &airunwayv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "attestation-integration-agent",
				Namespace: "default",
				Annotations: map[string]string{
					credentialadmission.AttestationAnnotation: "v1.forged.forged",
				},
			},
			Spec: airunwayv1alpha1.AgentDeploymentSpec{
				Framework: airunwayv1alpha1.AgentFrameworkRef{Name: "crewai"},
				Model: airunwayv1alpha1.ModelBinding{ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
					Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
					BaseURL:   "https://api.openai.com/v1",
					ModelName: "gpt-4o-mini",
					CredentialsRef: &airunwayv1alpha1.SecretKeyRef{
						Name: secret.Name, Key: "token",
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ad) })

		var stored airunwayv1alpha1.AgentDeployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, &stored)).To(Succeed())
		Expect(stored.Annotations).NotTo(HaveKey(credentialadmission.AttestationAnnotation),
			"CREATE mutation must remove caller-supplied proofs until the UID-bound validating record is finalized")

		recordKey, err := credentialadmission.RecordKey("default", stored.Namespace)
		Expect(err).NotTo(HaveOccurred())
		var record corev1.ConfigMap
		Expect(k8sClient.Get(ctx, recordKey, &record)).To(Succeed())

		records, err := credentialadmission.NewRecordStore(testCredentialAttestor, k8sClient, k8sClient, "default")
		Expect(err).NotTo(HaveOccurred())
		Expect(records.VerifyOrFinalize(ctx, &stored)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, &stored)).To(Succeed())
		Expect(stored.Annotations[credentialadmission.AttestationAnnotation]).NotTo(Equal("v1.forged.forged"))
		Expect(testCredentialAttestor.Verify(ctx, &stored)).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, recordKey, &record))).To(BeTrue())

		previousProof := stored.Annotations[credentialadmission.AttestationAnnotation]
		stored.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"image":"ghcr.io/example/agent:v2"}`)}
		stored.Annotations[credentialadmission.AttestationAnnotation] = "v1.forged.forged"
		Expect(k8sClient.Update(ctx, &stored)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, &stored)).To(Succeed())
		Expect(stored.Generation).To(Equal(int64(2)))
		Expect(stored.Annotations[credentialadmission.AttestationAnnotation]).NotTo(Equal(previousProof))
		Expect(stored.Annotations[credentialadmission.AttestationAnnotation]).NotTo(Equal("v1.forged.forged"))
		Expect(testCredentialAttestor.Verify(ctx, &stored)).To(Succeed())
	})
})
