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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	storageutil "github.com/ai-runway/airunway/controller/pkg/storage"
)

// These cases cross the real API server and installed admission webhooks. In
// particular, direct defaulter calls cannot prove defaults survive CRD pruning.
var _ = Describe("ModelDeployment storage API", func() {
	var namespace string

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "storage-api-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name
	})

	deployment := func(name string, volume *airunwayv1alpha1.StorageVolume) *airunwayv1alpha1.ModelDeployment {
		md := &airunwayv1alpha1.ModelDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: airunwayv1alpha1.ModelDeploymentSpec{
				Model: airunwayv1alpha1.ModelSpec{ID: "example/model"},
			},
		}
		if volume != nil {
			md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{*volume}}
		}
		return md
	}

	It("persists managed defaults and creates an idempotent owned PVC", func() {
		size := resource.MustParse("1Gi")
		md := deployment("managed", &airunwayv1alpha1.StorageVolume{
			Name: "cache", Purpose: airunwayv1alpha1.VolumePurposeModelCache, Size: &size,
		})
		Expect(k8sClient.Create(ctx, md)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(md), md)).To(Succeed())
		volume := md.Spec.Model.Storage.Volumes[0]
		Expect(volume.ClaimName).To(Equal("managed-cache"))
		Expect(volume.MountPath).To(Equal("/model-cache"))
		Expect(volume.AccessMode).To(Equal(corev1.ReadWriteMany))
		Expect(volume.Size.Cmp(size)).To(BeZero())

		ready, err := storageutil.EnsurePVCs(ctx, k8sClient, md)
		Expect(err).NotTo(HaveOccurred())
		Expect(ready).To(BeFalse(), "a newly created claim has not bound yet")
		pvc := &corev1.PersistentVolumeClaim{}
		key := client.ObjectKey{Namespace: namespace, Name: volume.ClaimName}
		Expect(k8sClient.Get(ctx, key, pvc)).To(Succeed())
		Expect(metav1.IsControlledBy(pvc, md)).To(BeTrue())
		Expect(pvc.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}))
		original := pvc.DeepCopy()
		_, err = storageutil.EnsurePVCs(ctx, k8sClient, md)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, pvc)).To(Succeed())
		Expect(pvc.ResourceVersion).To(Equal(original.ResourceVersion))

		// A same-named claim owned by somebody else must never be adopted.
		pvc.OwnerReferences = nil
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		original = pvc.DeepCopy()
		_, err = storageutil.EnsurePVCs(ctx, k8sClient, md)
		Expect(err).To(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, pvc)).To(Succeed())
		Expect(pvc).To(Equal(original))
	})

	It("keeps existing claims namespace scoped and leaves their ownership untouched", func() {
		md := deployment("existing", &airunwayv1alpha1.StorageVolume{
			Name: "cache", ClaimName: "shared", Purpose: airunwayv1alpha1.VolumePurposeModelCache,
		})
		Expect(k8sClient.Create(ctx, md)).To(Succeed())
		Expect(md.Spec.Model.Storage.Volumes[0].AccessMode).To(BeEmpty())
		Expect(md.Spec.Model.Storage.Volumes[0].Size).To(BeNil())
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "default"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, pvc)).To(Succeed()) })
		_, err := storageutil.EnsurePVCs(ctx, k8sClient, md)
		Expect(err).To(HaveOccurred(), "a claim in another namespace cannot satisfy this deployment")

		local := pvc.DeepCopy()
		local.ObjectMeta = metav1.ObjectMeta{Name: "shared", Namespace: namespace}
		Expect(k8sClient.Create(ctx, local)).To(Succeed())
		original := local.DeepCopy()
		ready, err := storageutil.EnsurePVCs(ctx, k8sClient, md)
		Expect(err).NotTo(HaveOccurred())
		Expect(ready).To(BeFalse())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(local), local)).To(Succeed())
		Expect(local).To(Equal(original))
		local.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, local)).To(Succeed())
		ready, err = storageutil.EnsurePVCs(ctx, k8sClient, md)
		Expect(err).NotTo(HaveOccurred())
		Expect(ready).To(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(local), local)).To(Succeed())
		Expect(local.OwnerReferences).To(BeEmpty())
	})

	It("enforces download name limits through admission and preserves storage omission", func() {
		size := resource.MustParse("1Gi")
		volume := &airunwayv1alpha1.StorageVolume{
			Name: "cache", Purpose: airunwayv1alpha1.VolumePurposeModelCache, Size: &size,
		}
		accepted := deployment(strings.Repeat("a", 48), volume)
		Expect(k8sClient.Create(ctx, accepted)).To(Succeed())
		_, err := storageutil.EnsurePVCs(ctx, k8sClient, accepted)
		Expect(err).NotTo(HaveOccurred())
		_, err = storageutil.EnsureDownloadJob(ctx, k8sClient, accepted, "example.test/downloader:api-proof")
		Expect(err).NotTo(HaveOccurred(), "the first consumer must be accepted even while the managed claim is pending")
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: accepted.Name + "-model-download"}, job)).To(Succeed())
		Expect(metav1.IsControlledBy(job, accepted)).To(BeTrue())
		Expect(job.Name).To(HaveLen(63))
		rejected := deployment(strings.Repeat("a", 49), volume)
		err = k8sClient.Create(ctx, rejected)
		Expect(err).To(MatchError(And(ContainSubstring("denied the request"), ContainSubstring("63-character"))))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(rejected), &airunwayv1alpha1.ModelDeployment{}))).To(BeTrue())
		volume.Size = nil
		volume.ClaimName = "existing"
		err = k8sClient.Create(ctx, deployment(strings.Repeat("b", 49), volume))
		Expect(err).To(MatchError(And(ContainSubstring("denied the request"), ContainSubstring("63-character"))))
		volume.ReadOnly = true
		Expect(k8sClient.Create(ctx, deployment(strings.Repeat("c", 49), volume))).To(Succeed())
		withoutStorage := deployment("without-storage", nil)
		Expect(k8sClient.Create(ctx, withoutStorage)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(withoutStorage), withoutStorage)).To(Succeed())
		Expect(withoutStorage.Spec.Model.Storage).To(BeNil())
	})
})
