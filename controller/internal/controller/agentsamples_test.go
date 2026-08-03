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
	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	webhookv1alpha1 "github.com/ai-runway/airunway/controller/internal/webhook/v1alpha1"
)

// The shipped samples are the first thing anyone runs, and `kubectl apply -k
// config/samples` is a documented step. A sample that fails admission is worse
// than no sample, so every document is applied against the real CRDs and put
// through the validating webhook here.
var _ = Describe("Agent samples", func() {
	ctx := context.Background()

	sampleFiles := []string{
		"../../config/samples/airunway_v1alpha1_agentproviderconfig.yaml",
		"../../config/samples/airunway_v1alpha1_agentdeployment.yaml",
	}

	It("apply cleanly against the real CRDs and pass the webhook", func() {
		// Two samples carry a credentialsRef. Wiring a permissive reviewer and an
		// admission context means those actually traverse the authorization path
		// instead of short-circuiting on a nil reviewer — the test claimed to run
		// samples "through the webhook" while skipping it for exactly the samples
		// where it matters.
		validator := &webhookv1alpha1.AgentDeploymentCustomValidator{SecretAccess: allowAllSecrets{}}
		admitCtx := admission.NewContextWithRequest(ctx, admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				UserInfo: authnv1.UserInfo{Username: "sample-validator"},
			},
		})
		applied := 0

		// AgentProviderConfig is cluster-scoped and the sample names overlap
		// with the ones other specs create, so everything applied here is torn
		// down again rather than leaking across specs.
		var created []*unstructured.Unstructured
		DeferCleanup(func() {
			for _, u := range created {
				_ = k8sClient.Delete(ctx, u)
			}
		})

		for _, f := range sampleFiles {
			raw, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred(), "reading %s", f)

			for _, doc := range strings.Split(string(raw), "\n---\n") {
				u := &unstructured.Unstructured{}
				if err := yaml.Unmarshal([]byte(doc), u); err != nil || u.GetKind() == "" {
					continue // comment-only block between documents
				}
				if u.GetKind() != "AgentProviderConfig" {
					u.SetNamespace("default")
				}

				By("applying " + u.GetKind() + "/" + u.GetName())
				Expect(k8sClient.Patch(ctx, u, client.Apply,
					client.FieldOwner("sample-validation"), client.ForceOwnership),
				).To(Succeed(), "%s/%s was rejected by the API server", u.GetKind(), u.GetName())
				applied++
				created = append(created, u.DeepCopy())

				if u.GetKind() != "AgentDeployment" {
					continue
				}
				ad := &airunwayv1alpha1.AgentDeployment{}
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(u), ad)).To(Succeed())
				_, err := validator.ValidateCreate(admitCtx, ad)
				Expect(err).NotTo(HaveOccurred(),
					"AgentDeployment %s was rejected by the validating webhook", ad.Name)
			}
		}

		Expect(applied).To(BeNumerically(">=", 10),
			"expected the full sample set; found only %d documents — did the split break?", applied)
	})

	It("reference only frameworks that are actually registered", func() {
		// A sample agent naming a framework with no AgentProviderConfig sits at
		// FrameworkNotRegistered forever, which reads as a broken feature.
		raw, err := os.ReadFile(sampleFiles[1])
		Expect(err).NotTo(HaveOccurred())

		configured := map[string]bool{}
		cfgRaw, err := os.ReadFile(sampleFiles[0])
		Expect(err).NotTo(HaveOccurred())
		for _, doc := range strings.Split(string(cfgRaw), "\n---\n") {
			u := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(doc), u); err == nil && u.GetKind() == "AgentProviderConfig" {
				configured[u.GetName()] = true
			}
		}

		for _, doc := range strings.Split(string(raw), "\n---\n") {
			ad := &airunwayv1alpha1.AgentDeployment{}
			if err := yaml.Unmarshal([]byte(doc), ad); err != nil || ad.Kind != "AgentDeployment" {
				continue
			}
			Expect(configured).To(HaveKey(ad.Spec.Framework.Name),
				"sample %q names framework %q, which has no AgentProviderConfig in this directory",
				ad.Name, ad.Spec.Framework.Name)
		}
	})
})

// allowAllSecrets lets the sample-validation spec exercise the credential path
// without an API server. It answers yes; the point here is that the samples are
// structurally valid, not that authorization works — that is covered by the
// dedicated tests in the webhook package.
type allowAllSecrets struct{}

func (allowAllSecrets) CanGetSecret(context.Context, admission.Request, string, string) (bool, string, error) {
	return true, "sample validation", nil
}
