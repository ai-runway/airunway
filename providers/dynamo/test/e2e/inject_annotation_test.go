//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// TestInjectMockerAnnotation exercises annotation injection via the unstructured
// API, including the fixture-evolution cases raised in review: an existing
// annotations block, an already-present key, and a bare metadata block. The
// assertions are semantic (re-parsed), not string-based, since the helper
// re-serializes the manifest.
func TestInjectMockerAnnotation(t *testing.T) {
	parseAnnotations := func(t *testing.T, manifest string) map[string]string {
		t.Helper()
		var obj unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(manifest), &obj.Object); err != nil {
			t.Fatalf("failed to re-parse output: %v", err)
		}
		return obj.GetAnnotations()
	}

	cases := map[string]string{
		"bare metadata, no annotations": `apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: x
  namespace: default
spec:
  model:
    id: m
`,
		"existing annotations block": `apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: x
  annotations:
    foo/bar: baz
spec: {}
`,
		"key already present": fmt.Sprintf(`apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  annotations:
    %s: %s
  name: x
spec: {}
`, mockerAnnotationKey, mockerAnnotationValue),
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			ann := parseAnnotations(t, injectMockerAnnotation(t, in))
			if ann[mockerAnnotationKey] != mockerAnnotationValue {
				t.Fatalf("mocker annotation missing/wrong: %v", ann)
			}
		})
	}

	t.Run("preserves sibling annotations and metadata fields", func(t *testing.T) {
		in := `apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: keepme
  namespace: default
  annotations:
    foo/bar: baz
spec: {}
`
		out := injectMockerAnnotation(t, in)

		var obj unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(out), &obj.Object); err != nil {
			t.Fatalf("failed to re-parse output: %v", err)
		}
		ann := obj.GetAnnotations()
		if ann["foo/bar"] != "baz" {
			t.Errorf("sibling annotation lost: %v", ann)
		}
		if ann[mockerAnnotationKey] != mockerAnnotationValue {
			t.Errorf("mocker annotation missing: %v", ann)
		}
		if obj.GetName() != "keepme" || obj.GetNamespace() != "default" {
			t.Errorf("metadata fields lost: name=%q ns=%q", obj.GetName(), obj.GetNamespace())
		}
	})

	t.Run("strips GPU fields so the spec is CPU-only", func(t *testing.T) {
		in := `apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: gpu
spec:
  resources:
    gpu:
      count: 1
  scaling:
    prefill:
      replicas: 1
      gpu:
        count: 1
    decode:
      replicas: 1
      gpu:
        count: 1
`
		out := injectMockerAnnotation(t, in)

		var obj unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(out), &obj.Object); err != nil {
			t.Fatalf("failed to re-parse output: %v", err)
		}
		for _, path := range [][]string{
			{"spec", "resources", "gpu"},
			{"spec", "scaling", "prefill", "gpu"},
			{"spec", "scaling", "decode", "gpu"},
		} {
			if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, path...); found {
				t.Errorf("expected %v to be stripped, but it is still present", path)
			}
		}
		// Sibling fields under the same parents must survive. (yaml decodes
		// numbers as float64, so compare without asserting the Go integer type.)
		if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "scaling", "prefill", "replicas"); !found {
			t.Errorf("prefill.replicas was lost when stripping prefill.gpu")
		}
	})
}
