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
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func consumerChain(provider string, kinds ...schema.GroupVersionKind) (*airunwayv1alpha1.ModelDeployment, *corev1.Pod, []client.Object) {
	md := newDownloadMD("model", "models")
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{Name: "cache", ClaimName: "missing-b"}}}
	md.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: provider}
	owner := metav1.OwnerReference{APIVersion: airunwayv1alpha1.GroupVersion.String(), Kind: "ModelDeployment", Name: md.Name, UID: md.UID, Controller: boolPtr(true)}
	objects := []client.Object{md}
	for i, gvk := range kinds {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		obj.SetNamespace(md.Namespace)
		obj.SetName(fmt.Sprintf("child-%d", i))
		if i == 0 {
			obj.SetName(md.Name)
			if gvk.Kind == "Job" {
				obj.SetName(md.Name + "-model-download")
			}
		}
		obj.SetUID(types.UID(fmt.Sprintf("uid-%d", i)))
		obj.SetOwnerReferences([]metav1.OwnerReference{owner})
		objects = append(objects, obj)
		owner = metav1.OwnerReference{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: obj.GetName(), UID: obj.GetUID(), Controller: boolPtr(true)}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-pod", Namespace: md.Namespace, UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{owner}},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "old-cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "old-a"}}}}}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "old-a", Namespace: md.Namespace, Finalizers: []string{"kubernetes.io/pvc-protection"}}}
	objects = append(objects, pod, pvc)
	return md, pod, objects
}

func TestLiveConsumerClaimsSurviveDesiredStorageChanges(t *testing.T) {
	deployment := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	rs := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}
	dgd := schema.GroupVersionKind{Group: "nvidia.com", Version: "v1alpha1", Kind: "DynamoGraphDeployment"}
	cases := []struct {
		name, provider string
		chain          []schema.GroupVersionKind
	}{
		{"vllm rollout", "vllm", []schema.GroupVersionKind{deployment, rs}},
		{"llmd rollout", "llmd", []schema.GroupVersionKind{deployment, rs}},
		{"downloader", "vllm", []schema.GroupVersionKind{{Group: "batch", Version: "v1", Kind: "Job"}}},
		{"kuberay", "kuberay", []schema.GroupVersionKind{{Group: "ray.io", Version: "v1", Kind: "RayService"}, {Group: "ray.io", Version: "v1", Kind: "RayCluster"}}},
		{"dynamo deployment", "dynamo", []schema.GroupVersionKind{dgd, {Group: "nvidia.com", Version: "v1alpha1", Kind: "DynamoComponentDeployment"}, deployment, rs}},
		{"dynamo lws", "dynamo", []schema.GroupVersionKind{dgd, {Group: "nvidia.com", Version: "v1alpha1", Kind: "DynamoComponentDeployment"}, {Group: "leaderworkerset.x-k8s.io", Version: "v1", Kind: "LeaderWorkerSet"}, {Group: "apps", Version: "v1", Kind: "StatefulSet"}}},
		{"dynamo grove", "dynamo", []schema.GroupVersionKind{dgd, {Group: "grove.io", Version: "v1alpha1", Kind: "PodCliqueSet"}, {Group: "grove.io", Version: "v1alpha1", Kind: "PodCliqueScalingGroup"}, {Group: "grove.io", Version: "v1alpha1", Kind: "PodClique"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testLiveConsumerTransitions(t, tc.provider, tc.chain)
		})
	}
}

func testLiveConsumerTransitions(t *testing.T, provider string, chain []schema.GroupVersionKind) {
	t.Helper()

	ctx := context.Background()
	md, pod, objects := consumerChain(provider, chain...)
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objects...).Build()
	terminating, err := HasTerminatingPVCs(ctx, c, md)
	if err != nil || terminating {
		t.Fatalf("healthy A must survive missing B: %v %v", terminating, err)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	key := client.ObjectKey{Namespace: md.Namespace, Name: "old-a"}
	if err := c.Get(ctx, key, pvc); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []bool{false, true} {
		if removed {
			md.Spec.Model.Storage = nil
			if err := c.Update(ctx, md); err != nil {
				t.Fatal(err)
			}
		}
		assertTerminatingConsumerMapping(t, c, md, pod, pvc)
	}
	// Once old Pods leave the rollout, A is no longer attributed to it.
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	terminating, err = HasTerminatingPVCs(ctx, c, md)
	if err != nil || terminating {
		t.Fatalf("completed rollout must forget A: %v %v", terminating, err)
	}
}

func assertTerminatingConsumerMapping(t *testing.T, c client.Client, md *airunwayv1alpha1.ModelDeployment, pod *corev1.Pod, pvc *corev1.PersistentVolumeClaim) {
	t.Helper()
	ctx := context.Background()

	terminating, err := HasTerminatingPVCs(ctx, c, md)
	if err != nil || !terminating {
		t.Fatalf("terminating old A must remain visible : %v %v", terminating, err)
	}
	for name, requests := range map[string]int{"PVC": len(MapPVCConsumers(ctx, c, pvc)), "Pod": len(MapPodConsumer(ctx, c, pod))} {
		if requests != 1 {
			t.Fatalf("%s watch lost live consumer after desired edit: %d", name, requests)
		}
	}
}

func TestLiveConsumerOwnershipIsolation(t *testing.T) {
	deployment := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	rs := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}
	cases := []struct {
		name   string
		mutate func(*airunwayv1alpha1.ModelDeployment, *corev1.Pod, []client.Object)
	}{
		{"replaced root UID", func(_ *airunwayv1alpha1.ModelDeployment, _ *corev1.Pod, o []client.Object) {
			o[1].SetUID("replacement")
		}},
		{"replaced MD UID", func(md *airunwayv1alpha1.ModelDeployment, _ *corev1.Pod, _ []client.Object) {
			md.UID = "replacement-md"
		}},
		{"same-name foreign root", func(_ *airunwayv1alpha1.ModelDeployment, _ *corev1.Pod, o []client.Object) {
			o[1].SetOwnerReferences(nil)
		}},
		{"non-controller edge", func(_ *airunwayv1alpha1.ModelDeployment, p *corev1.Pod, _ []client.Object) {
			p.OwnerReferences[0].Controller = boolPtr(false)
		}},
		{"other namespace", func(_ *airunwayv1alpha1.ModelDeployment, p *corev1.Pod, _ []client.Object) { p.Namespace = "other" }},
		{"other generated name", func(_ *airunwayv1alpha1.ModelDeployment, _ *corev1.Pod, o []client.Object) {
			o[1].SetName("foreign")
			refs := o[2].GetOwnerReferences()
			refs[0].Name = "foreign"
			o[2].SetOwnerReferences(refs)
		}},
		{"unsupported owner", func(_ *airunwayv1alpha1.ModelDeployment, p *corev1.Pod, _ []client.Object) {
			p.OwnerReferences[0].Kind = "Secret"
			p.OwnerReferences[0].APIVersion = "v1"
		}},
		{"owner cycle", func(_ *airunwayv1alpha1.ModelDeployment, p *corev1.Pod, o []client.Object) {
			o[1].SetOwnerReferences(p.OwnerReferences)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md, pod, objects := consumerChain("vllm", deployment, rs)
			tc.mutate(md, pod, objects)
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objects...).Build()
			claims, err := consumedPVCNames(context.Background(), c, md)
			if err != nil || len(claims) != 0 {
				t.Fatalf("foreign ownership attributed claims: %v %v", claims, err)
			}
			if requests := MapPodConsumer(context.Background(), c, pod); len(requests) != 0 {
				t.Fatalf("foreign Pod enqueued MD: %v", requests)
			}
		})
	}
}
