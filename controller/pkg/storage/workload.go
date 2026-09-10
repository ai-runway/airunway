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

package storage

import (
	"context"
	"fmt"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ConsumerWorkload identifies a provider resource whose descendant pods can
// mount ModelDeployment storage.
type ConsumerWorkload struct {
	GroupVersionKind schema.GroupVersionKind
	Name             string
}

// EnsureConsumerWorkloadsAbsent deletes workloads owned by the current
// ModelDeployment or provably owned by an older deployment with the same name.
// Foreign name collisions are preserved; they cannot be this deployment's PVC
// consumer and will be reported normally if creation is later attempted.
func EnsureConsumerWorkloadsAbsent(
	ctx context.Context,
	c client.Client,
	md *airunwayv1alpha1.ModelDeployment,
	workloads ...ConsumerWorkload,
) (bool, error) {
	logger := log.FromContext(ctx)
	allAbsent := true
	foreground := metav1.DeletePropagationForeground

	for _, ref := range workloads {
		workload := &unstructured.Unstructured{}
		workload.SetGroupVersionKind(ref.GroupVersionKind)
		err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: md.Namespace}, workload)
		if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("failed to get storage consumer %s %s: %w", ref.GroupVersionKind.Kind, ref.Name, err)
		}
		if !IsOwnedByMD(workload, md.UID) && !isOwnedByPriorMD(workload, md) {
			logger.Info("Preserving storage consumer not owned by this ModelDeployment",
				"kind", ref.GroupVersionKind.Kind, "name", ref.Name)
			continue
		}

		allAbsent = false
		if !workload.GetDeletionTimestamp().IsZero() {
			continue
		}

		logger.Info("Deleting storage consumer to release terminating PVC",
			"kind", ref.GroupVersionKind.Kind, "name", ref.Name)
		deleteErr := c.Delete(ctx, workload, &client.DeleteOptions{PropagationPolicy: &foreground})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return false, fmt.Errorf(
				"failed to delete storage consumer %s %s: %w",
				ref.GroupVersionKind.Kind,
				ref.Name,
				deleteErr,
			)
		}
	}

	return allAbsent, nil
}
