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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// consumerLookup follows controller references, never labels or desired volume
// names. Its per-pass cache shares owner reads across replicas and old rollouts.
type consumerLookup struct {
	reader  client.Reader
	objects map[client.ObjectKey]map[schema.GroupVersionKind]*unstructured.Unstructured
}

func newConsumerLookup(reader client.Reader) *consumerLookup {
	return &consumerLookup{
		reader:  reader,
		objects: make(map[client.ObjectKey]map[schema.GroupVersionKind]*unstructured.Unstructured),
	}
}

// consumerOwnerKind limits traversal to the namespaced workload kinds used by
// the supported providers, including Dynamo's Deployment, LWS and Grove paths.
func consumerOwnerKind(gk schema.GroupKind) bool {
	switch gk.Group {
	case "apps":
		return gk.Kind == "ReplicaSet" || gk.Kind == "Deployment" || gk.Kind == "StatefulSet"
	case "batch":
		return gk.Kind == "Job"
	case "ray.io":
		return gk.Kind == "RayCluster" || gk.Kind == "RayService"
	case "nvidia.com":
		return gk.Kind == "DynamoComponentDeployment" || gk.Kind == "DynamoGraphDeployment"
	case "leaderworkerset.x-k8s.io":
		return gk.Kind == "LeaderWorkerSet"
	case "grove.io":
		return gk.Kind == "PodClique" || gk.Kind == "PodCliqueScalingGroup" || gk.Kind == "PodCliqueSet"
	}
	return false
}

func (l *consumerLookup) get(
	ctx context.Context, namespace string, ref *metav1.OwnerReference,
) (*unstructured.Unstructured, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	gk := schema.GroupKind{Group: gv.Group, Kind: ref.Kind}
	if err != nil || ref.UID == "" || ref.Name == "" || !consumerOwnerKind(gk) {
		return nil, nil
	}
	gvk := gv.WithKind(ref.Kind)
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if l.objects[key] == nil {
		l.objects[key] = make(map[schema.GroupVersionKind]*unstructured.Unstructured)
	}
	obj, found := l.objects[key][gvk]
	if !found {
		obj = &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := l.reader.Get(ctx, key, obj); err != nil {
			if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
				return nil, fmt.Errorf("read storage consumer owner %s %s: %w", gvk, key, err)
			}
			obj = nil
		}
		l.objects[key][gvk] = obj
	}
	if obj == nil || obj.GetUID() != ref.UID {
		return nil, nil
	}
	return obj, nil
}

// deployment finds the live ModelDeployment at the end of the consumer chain.
// Every edge must retain its UID and namespace. A replaced/missing parent,
// unsupported kind, non-controller reference or cycle proves no ownership.
func (l *consumerLookup) deployment(ctx context.Context, pod *corev1.Pod) (*airunwayv1alpha1.ModelDeployment, error) {
	var obj client.Object = pod
	seen := make(map[types.UID]bool)
	for range 16 {
		ref := metav1.GetControllerOf(obj)
		if ref == nil || ref.UID == "" || seen[ref.UID] {
			return nil, nil
		}
		seen[ref.UID] = true
		if ref.APIVersion == airunwayv1alpha1.GroupVersion.String() && ref.Kind == "ModelDeployment" {
			md := &airunwayv1alpha1.ModelDeployment{}
			if err := l.reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ref.Name}, md); err != nil {
				return nil, client.IgnoreNotFound(err)
			}
			if md.UID != ref.UID || !generatedConsumer(obj, md) {
				return nil, nil
			}
			return md, nil
		}
		owner, err := l.get(ctx, pod.Namespace, ref)
		if err != nil || owner == nil {
			return nil, err
		}
		obj = owner
	}
	return nil, nil
}

func generatedConsumer(obj client.Object, md *airunwayv1alpha1.ModelDeployment) bool {
	gk := obj.GetObjectKind().GroupVersionKind().GroupKind()
	if gk == (schema.GroupKind{Group: "batch", Kind: "Job"}) && obj.GetName() == md.Name+"-model-download" {
		return true
	}
	if md.Status.Provider == nil {
		return false
	}
	switch md.Status.Provider.Name {
	case "vllm", "llmd":
		return gk == (schema.GroupKind{Group: "apps", Kind: "Deployment"}) &&
			(obj.GetName() == md.Name || obj.GetName() == md.Name+"-prefill" || obj.GetName() == md.Name+"-decode")
	case "kuberay":
		return gk == (schema.GroupKind{Group: "ray.io", Kind: "RayService"}) && obj.GetName() == md.Name
	case "dynamo":
		return gk == (schema.GroupKind{Group: "nvidia.com", Kind: "DynamoGraphDeployment"}) && obj.GetName() == md.Name
	}
	return false
}

func podPVCNames(pod *corev1.Pod) []string {
	var names []string
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName != "" {
			names = append(names, volume.PersistentVolumeClaim.ClaimName)
		}
	}
	return names
}

// consumedPVCNames includes old Pods throughout a rollout and after desired
// storage changes/removal. It neither adopts nor deletes any resource.
func consumedPVCNames(
	ctx context.Context, reader client.Reader, md *airunwayv1alpha1.ModelDeployment,
) ([]string, error) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(md.Namespace)); err != nil {
		return nil, fmt.Errorf("list storage consumer pods: %w", err)
	}
	lookup := newConsumerLookup(reader)
	var names []string
	for i := range pods.Items {
		claims := podPVCNames(&pods.Items[i])
		if len(claims) == 0 {
			continue
		}
		owner, err := lookup.deployment(ctx, &pods.Items[i])
		if err != nil {
			return names, err
		}
		if owner != nil && owner.UID == md.UID && owner.Name == md.Name {
			names = append(names, claims...)
		}
	}
	return names, nil
}

// MapPVCConsumers supplements desired-spec indexes with live Pod ownership.
// An old claim still enqueues its deployment after a mutable A-to-B edit.
func MapPVCConsumers(ctx context.Context, reader client.Reader, obj client.Object) []reconcile.Request {
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return nil
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(pvc.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list PVC consumers")
		return nil
	}
	lookup := newConsumerLookup(reader)
	seen := make(map[client.ObjectKey]bool)
	var requests []reconcile.Request
	for i := range pods.Items {
		for _, claim := range podPVCNames(&pods.Items[i]) {
			if claim != pvc.Name {
				continue
			}
			md, err := lookup.deployment(ctx, &pods.Items[i])
			if err != nil {
				log.FromContext(ctx).Error(err, "Failed to resolve PVC consumer")
				break
			}
			if md != nil {
				key := client.ObjectKeyFromObject(md)
				if !seen[key] {
					requests = append(requests, reconcile.Request{NamespacedName: key})
					seen[key] = true
				}
			}
			break
		}
	}
	return requests
}

// MapPodConsumer wakes storage preparation as old consumers enter or leave a
// rollout. Initial Pod events also reconstruct ownership after a restart.
func MapPodConsumer(ctx context.Context, reader client.Reader, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || len(podPVCNames(pod)) == 0 {
		return nil
	}
	md, err := newConsumerLookup(reader).deployment(ctx, pod)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to resolve storage consumer")
		return nil
	}
	if md == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(md)}}
}
