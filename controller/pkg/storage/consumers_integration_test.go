//go:build integration

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

package storage_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/storage"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestPVCConsumerGarbageCollection requires a dedicated disposable cluster with
// the ModelDeployment CRD and busybox:latest preloaded. It deliberately uses a
// CPU sleeping container: the boundary under test is Kubernetes PVC protection
// and Deployment/ReplicaSet/Pod garbage collection, not model inference.
func TestPVCConsumerGarbageCollection(t *testing.T) {
	kubeconfig := os.Getenv("STORAGE_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set STORAGE_TEST_KUBECONFIG to a disposable cluster")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, airunwayv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(log.IntoContext(context.Background(), logr.Discard()), 8*time.Minute)
	defer cancel()
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending-replacement", true: "storage-removed"}[removed], func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			eventually := func(description string, check func() bool) {
				t.Helper()
				deadline := time.Now().Add(90 * time.Second)
				for time.Now().Before(deadline) && ctx.Err() == nil {
					if check() {
						return
					}
					time.Sleep(250 * time.Millisecond)
				}
				t.Fatalf("timed out: %s", description)
			}

			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pr410-pvc-"}}
			must(c.Create(ctx, ns))
			md := &airunwayv1alpha1.ModelDeployment{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: ns.Name}, Spec: airunwayv1alpha1.ModelDeploymentSpec{
				Model: airunwayv1alpha1.ModelSpec{ID: "example/model", Source: airunwayv1alpha1.ModelSourceCustom, Storage: &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{Name: "cache", ClaimName: "old-a", MountPath: "/cache", Purpose: airunwayv1alpha1.VolumePurposeCustom}}}},
			}}
			must(c.Create(ctx, md))
			md.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: "vllm"}
			must(c.Status().Update(ctx, md))
			createClaim := func(name string) *corev1.PersistentVolumeClaim {
				capacity := corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
				hostType := corev1.HostPathDirectoryOrCreate
				pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: ns.Name + "-" + name}, Spec: corev1.PersistentVolumeSpec{
					Capacity: capacity, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
					PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/" + ns.Name + "-" + name, Type: &hostType}},
				}}
				must(c.Create(ctx, pv))
				empty := ""
				pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name}, Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: &empty, VolumeName: pv.Name, Resources: corev1.VolumeResourceRequirements{Requests: capacity},
				}}
				must(c.Create(ctx, pvc))
				eventually("PVC bound", func() bool {
					return c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc) == nil && pvc.Status.Phase == corev1.ClaimBound
				})
				return pvc
			}
			pvc := createClaim("old-a")
			one := int32(1)
			grace := int64(1)
			yes := true
			labels := map[string]string{"app": "storage-proof"}
			dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: md.Name, Namespace: ns.Name, OwnerReferences: []metav1.OwnerReference{{APIVersion: airunwayv1alpha1.GroupVersion.String(), Kind: "ModelDeployment", Name: md.Name, UID: md.UID, Controller: &yes, BlockOwnerDeletion: &yes}}}, Spec: appsv1.DeploymentSpec{
				Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: labels}, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: ptrIntOrString(intstr.FromInt32(1)), MaxUnavailable: ptrIntOrString(intstr.FromInt32(0))}},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{TerminationGracePeriodSeconds: &grace,
					Containers: []corev1.Container{{Name: "consumer", Image: "busybox:latest", ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"sh", "-c", "sleep 3600"}, VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}}}},
					Volumes:    []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}}}},
				}},
			}}
			must(c.Create(ctx, dep))
			rootUID := dep.UID
			eventually("owned Pod running on A", func() bool {
				return c.Get(ctx, client.ObjectKeyFromObject(dep), dep) == nil && dep.Status.AvailableReplicas == 1
			})
			must(c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc))
			if len(pvc.OwnerReferences) != 0 {
				t.Fatal("existing PVC was adopted")
			}
			md.Spec.Model.Storage.Volumes[0].ClaimName = "missing-b"
			if removed {
				md.Spec.Model.Storage = nil
			}
			must(c.Update(ctx, md))
			// Put the template on B as well: the old ReplicaSet remains on A during
			// a real rolling update, while the new Pod cannot start without B.
			dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "missing-b"
			must(c.Update(ctx, dep))
			eventually("both rollout generations exist", func() bool {
				var pods corev1.PodList
				if c.List(ctx, &pods, client.InNamespace(ns.Name)) != nil {
					return false
				}
				return len(pods.Items) >= 2
			})
			terminating, err := storage.HasTerminatingPVCs(ctx, c, md)
			must(err)
			if terminating {
				t.Fatal("healthy A must not trigger teardown while B waits")
			}
			must(c.Get(ctx, client.ObjectKeyFromObject(dep), dep))
			if dep.UID != rootUID || dep.Status.AvailableReplicas != 1 {
				t.Fatal("healthy service was not preserved")
			}
			must(c.Delete(ctx, pvc))
			eventually("PVC protected by live old Pod", func() bool {
				if c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc) != nil {
					return false
				}
				for _, f := range pvc.Finalizers {
					if f == "kubernetes.io/pvc-protection" && !pvc.DeletionTimestamp.IsZero() {
						return true
					}
				}
				return false
			})
			// New client reconstructs live ownership; no process-local applied state.
			restarted, err := client.New(config, client.Options{Scheme: scheme})
			must(err)
			terminating, err = storage.HasTerminatingPVCs(ctx, restarted, md)
			must(err)
			if !terminating {
				t.Fatal("restart lost the terminating mounted old claim")
			}
			requests := storage.MapPVCConsumers(ctx, restarted, pvc)
			if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(md) {
				t.Fatalf("old PVC event did not map to deployment: %v", requests)
			}
			ref := storage.ConsumerWorkload{GroupVersionKind: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, Name: dep.Name}
			_, err = storage.EnsureConsumerWorkloadsAbsent(ctx, restarted, md, ref)
			must(err)
			eventually("foreground GC releases old claim and all Pods", func() bool {
				_, err := storage.EnsureConsumerWorkloadsAbsent(ctx, restarted, md, ref)
				if err != nil {
					return false
				}
				var pods corev1.PodList
				return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{})) && apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(dep), &appsv1.Deployment{})) && c.List(ctx, &pods, client.InNamespace(ns.Name)) == nil && len(pods.Items) == 0
			})
			terminating, err = storage.HasTerminatingPVCs(ctx, restarted, md)
			must(err)
			if terminating {
				t.Fatal("termination did not clear after GC")
			}
			// Recovery uses a new claim and workload, leaving the user-owned PV intact.
			replacement := createClaim("missing-b")
			dep.ObjectMeta = metav1.ObjectMeta{Name: md.Name, Namespace: ns.Name, OwnerReferences: []metav1.OwnerReference{{APIVersion: airunwayv1alpha1.GroupVersion.String(), Kind: "ModelDeployment", Name: md.Name, UID: md.UID, Controller: &yes}}}
			dep.Status = appsv1.DeploymentStatus{}
			must(c.Create(ctx, dep))
			eventually("replacement workload running", func() bool {
				return c.Get(ctx, client.ObjectKeyFromObject(dep), dep) == nil && dep.Status.AvailableReplicas == 1
			})
			must(c.Get(ctx, client.ObjectKeyFromObject(replacement), replacement))
			if len(replacement.OwnerReferences) != 0 {
				t.Fatal("replacement existing claim was adopted")
			}
			t.Logf("%s: healthy A preserved; real pvc-protection held deletion; live UID chain survived restart; foreground GC released A; replacement B running", ns.Name)
		})
	}
}

func ptrIntOrString(v intstr.IntOrString) *intstr.IntOrString { return &v }
