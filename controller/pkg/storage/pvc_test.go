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
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHasManagedPVCs(t *testing.T) {
	tests := []struct {
		name string
		md   *airunwayv1alpha1.ModelDeployment
		want bool
	}{
		{
			name: "no storage",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{ID: "test"},
				},
			},
			want: false,
		},
		{
			name: "pre-existing PVC only",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{
						ID: "test",
						Storage: &airunwayv1alpha1.StorageSpec{
							Volumes: []airunwayv1alpha1.StorageVolume{
								{
									Name:      "cache",
									ClaimName: "existing-pvc",
								},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "managed PVC with size",
			md: &airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{
						ID: "test",
						Storage: &airunwayv1alpha1.StorageSpec{
							Volumes: []airunwayv1alpha1.StorageVolume{
								{
									Name: "cache",
									Size: pvcSize("100Gi"),
								},
							},
						},
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasManagedPVCs(tt.md)
			if got != tt.want {
				t.Errorf("HasManagedPVCs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsurePVCsCreation(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	size := resource.MustParse("100Gi")
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "meta-llama/Llama-2-7b",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:       "model-cache",
							Size:       &size,
							AccessMode: corev1.ReadWriteMany,
							Purpose:    airunwayv1alpha1.VolumePurposeModelCache,
						},
					},
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	allReady, err := EnsurePVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allReady {
		t.Error("expected allReady=false after creating PVC (PVC is not yet Bound)")
	}

	// Verify PVC was created
	pvc := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-cache", Namespace: "default"}, pvc)
	if err != nil {
		t.Fatalf("expected PVC to be created: %v", err)
	}

	// Verify PVC spec
	if pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("expected ReadWriteMany, got %s", pvc.Spec.AccessModes[0])
	}
	storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if storageReq.Cmp(size) != 0 {
		t.Errorf("expected size %s, got %s", size.String(), storageReq.String())
	}

	// Verify labels
	if pvc.Labels[airunwayv1alpha1.LabelManagedBy] != "airunway" {
		t.Error("expected managed-by label")
	}
	if pvc.Labels[airunwayv1alpha1.LabelModelDeployment] != "my-model" {
		t.Error("expected model-deployment label")
	}

	// Verify owner reference
	if len(pvc.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(pvc.OwnerReferences))
	}
	if pvc.OwnerReferences[0].Name != "my-model" {
		t.Errorf("expected owner name my-model, got %s", pvc.OwnerReferences[0].Name)
	}
	if pvc.OwnerReferences[0].BlockOwnerDeletion == nil || !*pvc.OwnerReferences[0].BlockOwnerDeletion {
		t.Errorf("expected OwnerReference BlockOwnerDeletion to be true")
	}

	// Verify storageClassName is nil (cluster default)
	if pvc.Spec.StorageClassName != nil {
		t.Errorf("expected nil storageClassName, got %v", *pvc.Spec.StorageClassName)
	}
}

func TestEnsurePVCsWithStorageClass(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	size := resource.MustParse("200Gi")
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:             "model-cache",
							Size:             &size,
							StorageClassName: stringPtr("fast-ssd"),
							AccessMode:       corev1.ReadWriteOnce,
						},
					},
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, err := EnsurePVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pvc := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-cache", Namespace: "default"}, pvc)
	if err != nil {
		t.Fatalf("expected PVC to be created: %v", err)
	}

	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-ssd" {
		t.Errorf("expected storageClassName fast-ssd")
	}
	if pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("expected ReadWriteOnce, got %s", pvc.Spec.AccessModes[0])
	}
}

func TestEnsurePVCsIdempotent(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	size := resource.MustParse("100Gi")
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:       "model-cache",
							Size:       &size,
							AccessMode: corev1.ReadWriteMany,
						},
					},
				},
			},
		},
	}

	// Pre-create a bound PVC
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-cache",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "my-model",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "test-uid",
				},
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: size,
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPVC).WithStatusSubresource(existingPVC).Build()

	allReady, err := EnsurePVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allReady {
		t.Error("expected allReady=true for Bound PVC")
	}
}

func TestEnsurePVCsPending(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	size := resource.MustParse("100Gi")
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:       "model-cache",
							Size:       &size,
							AccessMode: corev1.ReadWriteMany,
						},
					},
				},
			},
		},
	}

	// Pre-create a pending PVC owned by this MD
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-cache",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "test-uid",
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimPending,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPVC).WithStatusSubresource(existingPVC).Build()

	allReady, err := EnsurePVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Managed PVCs in Pending phase should be treated as ready so the
	// download Job can be created, which triggers WaitForFirstConsumer binding.
	if !allReady {
		t.Error("expected allReady=true for Pending managed PVC (WaitForFirstConsumer compatible)")
	}
}

func TestEnsurePVCsNoStorage(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{ID: "test"},
		},
	}

	allReady, err := EnsurePVCs(context.Background(), nil, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allReady {
		t.Error("expected allReady=true when no storage")
	}
}

func TestDeleteManagedPVCs(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
	}

	// Create PVCs with matching labels and OwnerReference
	pvc1 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-cache",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "my-model",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "test-uid",
				},
			},
		},
	}
	// Create an unrelated PVC
	pvc2 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated-pvc",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "other-model",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc1, pvc2).Build()

	err := DeleteManagedPVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify our PVC was deleted
	pvc := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-cache", Namespace: "default"}, pvc)
	if err == nil {
		t.Error("expected managed PVC to be deleted")
	}

	// Verify unrelated PVC still exists
	err = c.Get(context.Background(), types.NamespacedName{Name: "unrelated-pvc", Namespace: "default"}, pvc)
	if err != nil {
		t.Error("expected unrelated PVC to still exist")
	}
}

func TestDeleteManagedPVCsSkipsNonOwned(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("new-uid"),
		},
	}

	// Create a PVC with correct labels but OwnerReference pointing to a different UID
	pvc1 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-cache",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "my-model",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: airunwayv1alpha1.GroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "old-uid",
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc1).Build()

	err := DeleteManagedPVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify PVC was NOT deleted (UID mismatch)
	pvc := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-cache", Namespace: "default"}, pvc)
	if err != nil {
		t.Error("expected PVC with mismatched UID to NOT be deleted")
	}
}

func TestEnsurePVCsWithEmptyStorageClass(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	size := resource.MustParse("100Gi")
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:             "model-cache",
							Size:             &size,
							StorageClassName: stringPtr(""),
							AccessMode:       corev1.ReadWriteMany,
						},
					},
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, err := EnsurePVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pvc := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-cache", Namespace: "default"}, pvc)
	if err != nil {
		t.Fatalf("expected PVC to be created: %v", err)
	}

	// Verify storageClassName is non-nil and equals empty string (disables dynamic provisioning)
	if pvc.Spec.StorageClassName == nil {
		t.Fatal("expected non-nil storageClassName, got nil")
	}
	if *pvc.Spec.StorageClassName != "" {
		t.Errorf("expected empty storageClassName, got %q", *pvc.Spec.StorageClassName)
	}
}

func TestBuildPVCNilSize(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
	}
	vol := &airunwayv1alpha1.StorageVolume{
		Name: "model-cache",
		Size: nil,
	}

	_, err := buildPVC(md, vol)
	if err == nil {
		t.Fatal("expected error when vol.Size is nil, got nil")
	}

	expected := "volume size must be set for controller-created PVCs"
	if err.Error() != expected {
		t.Errorf("expected error %q, got %q", expected, err.Error())
	}
}

func TestEnsurePVCsStaleBound(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	size := resource.MustParse("100Gi")
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:       "model-cache",
							Size:       &size,
							AccessMode: corev1.ReadWriteMany,
						},
					},
				},
			},
		},
	}

	// Pre-create a Bound PVC owned by a previous ModelDeployment (different UID)
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-cache",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "my-model",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "old-uid",
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPVC).WithStatusSubresource(existingPVC).Build()

	allReady, err := EnsurePVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allReady {
		t.Error("expected allReady=false when stale PVC is deleted")
	}

	// Verify stale PVC was deleted
	pvc := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-cache", Namespace: "default"}, pvc)
	if err == nil {
		t.Error("expected stale Bound PVC to be deleted")
	}
}

func TestEnsurePVCsStalePending(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	size := resource.MustParse("100Gi")
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:       "model-cache",
							Size:       &size,
							AccessMode: corev1.ReadWriteMany,
						},
					},
				},
			},
		},
	}

	// Pre-create a Pending PVC owned by a previous ModelDeployment (different UID)
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-cache",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "my-model",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "airunway.ai/v1alpha1",
					Kind:       "ModelDeployment",
					Name:       "my-model",
					UID:        "old-uid",
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimPending,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPVC).WithStatusSubresource(existingPVC).Build()

	allReady, err := EnsurePVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allReady {
		t.Error("expected allReady=false when stale PVC is deleted")
	}

	// Verify stale PVC was deleted
	pvc := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-cache", Namespace: "default"}, pvc)
	if err == nil {
		t.Error("expected stale Pending PVC to be deleted")
	}
}

func TestEnsurePVCsStaleNoOwnerRef(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	size := resource.MustParse("100Gi")
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:       "model-cache",
							Size:       &size,
							AccessMode: corev1.ReadWriteMany,
						},
					},
				},
			},
		},
	}

	// Pre-create a PVC with no OwnerReferences but with managed-by labels
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-cache",
			Namespace: "default",
			Labels: map[string]string{
				airunwayv1alpha1.LabelManagedBy:       "airunway",
				airunwayv1alpha1.LabelModelDeployment: "my-model",
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPVC).WithStatusSubresource(existingPVC).Build()

	allReady, err := EnsurePVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allReady {
		t.Error("expected allReady=false when stale PVC (no owner ref) is deleted")
	}

	// Verify stale PVC was deleted
	pvc := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-cache", Namespace: "default"}, pvc)
	if err == nil {
		t.Error("expected stale PVC with no OwnerReferences to be deleted")
	}
}

func TestEnsurePVCsRefusesToDeleteUnmanagedPVC(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	size := resource.MustParse("100Gi")
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:       "model-cache",
							Size:       &size,
							AccessMode: corev1.ReadWriteMany,
						},
					},
				},
			},
		},
	}

	// Pre-create an unmanaged PVC (no airunway labels, different UID)
	unmanagedPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model-model-cache",
			Namespace: "default",
			Labels: map[string]string{
				"app": "something-else",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "other-workload",
					UID:        "other-uid",
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(unmanagedPVC).WithStatusSubresource(unmanagedPVC).Build()

	_, err := EnsurePVCs(context.Background(), c, md)
	if err == nil {
		t.Fatal("expected error when PVC exists without airunway managed-by label")
	}

	// Verify the error message is actionable
	if !strings.Contains(err.Error(), "was not created by airunway") {
		t.Errorf("expected error to mention 'was not created by airunway', got: %s", err.Error())
	}

	// Verify the unmanaged PVC was NOT deleted
	pvc := &corev1.PersistentVolumeClaim{}
	getErr := c.Get(context.Background(), types.NamespacedName{Name: "my-model-model-cache", Namespace: "default"}, pvc)
	if getErr != nil {
		t.Error("expected unmanaged PVC to still exist after refusal")
	}
}

func TestEnsurePVCsPreExistingBound(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:      "model-cache",
							ClaimName: "existing-pvc",
							// Size is nil — pre-existing PVC
						},
					},
				},
			},
		},
	}

	// Pre-create the PVC in Bound phase
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-pvc",
			Namespace: "default",
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPVC).WithStatusSubresource(existingPVC).Build()

	allReady, err := EnsurePVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allReady {
		t.Error("expected allReady=true for Bound pre-existing PVC")
	}
}

func TestEnsurePVCsPreExistingPending(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:      "model-cache",
							ClaimName: "existing-pvc",
							// Size is nil — pre-existing PVC
						},
					},
				},
			},
		},
	}

	// Pre-create the PVC in Pending phase
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-pvc",
			Namespace: "default",
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimPending,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPVC).WithStatusSubresource(existingPVC).Build()

	allReady, err := EnsurePVCs(context.Background(), c, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allReady {
		t.Error("expected allReady=false for Pending pre-existing PVC")
	}
}

func TestEnsurePVCsPreExistingNotFound(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:      "model-cache",
							ClaimName: "nonexistent-pvc",
							// Size is nil — pre-existing PVC
						},
					},
				},
			},
		},
	}

	// No PVC created — simulate a typo or missing PVC
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := EnsurePVCs(context.Background(), c, md)
	if err == nil {
		t.Fatal("expected error when pre-existing PVC does not exist")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected error to contain 'not found', got: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "model-cache") {
		t.Errorf("expected error to mention volume name 'model-cache', got: %s", err.Error())
	}
}

func TestEnsurePVCsPreExistingLost(t *testing.T) {
	scheme := newScheme()
	_ = corev1.AddToScheme(scheme)

	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-model",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID: "test-model",
				Storage: &airunwayv1alpha1.StorageSpec{
					Volumes: []airunwayv1alpha1.StorageVolume{
						{
							Name:      "model-cache",
							ClaimName: "lost-pvc",
							// Size is nil — pre-existing PVC
						},
					},
				},
			},
		},
	}

	// Pre-create the PVC in Lost phase
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lost-pvc",
			Namespace: "default",
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimLost,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPVC).WithStatusSubresource(existingPVC).Build()

	_, err := EnsurePVCs(context.Background(), c, md)
	if err == nil {
		t.Fatal("expected error when pre-existing PVC is in Lost phase")
	}
	if !strings.Contains(err.Error(), "Lost phase") {
		t.Errorf("expected error to contain 'Lost phase', got: %s", err.Error())
	}
}
