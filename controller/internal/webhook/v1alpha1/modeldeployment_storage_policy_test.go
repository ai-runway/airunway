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
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

func TestKAITOStorageAdmissionPolicy(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		old := &airunwayv1alpha1.ModelDeployment{}
		if explicit {
			old.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "kaito"}
		} else {
			old.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: "kaito"}
		}
		next := old.DeepCopy()
		next.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{Name: "cache", ClaimName: "a"}}}
		if errs := validateKAITOStorage(old, next); len(errs) != 1 {
			t.Fatalf("unsupported addition accepted (explicit=%v): %v", explicit, errs)
		}
		if errs := validateKAITOStorage(next, next.DeepCopy()); len(errs) != 0 {
			t.Fatalf("unchanged legacy storage blocked status updates: %v", errs)
		}
		removed := next.DeepCopy()
		removed.Spec.Model.Storage = nil
		if errs := validateKAITOStorage(next, removed); len(errs) != 0 {
			t.Fatalf("removing legacy storage blocked: %v", errs)
		}
		changed := next.DeepCopy()
		changed.Spec.Model.Storage.Volumes[0].ClaimName = "b"
		if errs := validateKAITOStorage(next, changed); len(errs) != 1 {
			t.Fatalf("unsupported changed storage accepted: %v", errs)
		}
		if explicit && len(validateKAITOStorage(nil, next)) != 1 {
			t.Fatal("unsupported explicit KAITO create accepted")
		}
	}
	supported := &airunwayv1alpha1.ModelDeployment{}
	supported.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: "vllm"}
	supported.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{Name: "cache", ClaimName: "a"}}}
	if len(validateKAITOStorage(nil, supported)) != 0 {
		t.Fatal("supported storage rejected")
	}
}
