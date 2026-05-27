/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

// quantityPtr returns a pointer to a resource.Quantity parsed from s.
func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// TestStorageVolumeEqual_QuantityEquivalence guards against a regression where
// reflect.DeepEqual on StorageVolume reports inequality for semantically equal
// resource.Quantity values (e.g. "1Gi" vs "1024Mi", or one Quantity whose
// internal cached string has been populated and one whose hasn't). This used to
// falsely trigger the managed-storage immutability error.
func TestStorageVolumeEqual_QuantityEquivalence(t *testing.T) {
	t.Parallel()

	// Trigger cached-string population on one side only.
	a := &airunwayv1alpha1.StorageVolume{
		Name: "cache",
		Size: quantityPtr("1Gi"),
	}
	_ = a.Size.String() // force the internal `s` field to populate

	b := &airunwayv1alpha1.StorageVolume{
		Name: "cache",
		Size: quantityPtr("1Gi"),
	}

	if !storageVolumeEqual(a, b) {
		t.Errorf("expected 1Gi == 1Gi (with one .String() called) to be equal")
	}

	// Different units, same magnitude.
	c := &airunwayv1alpha1.StorageVolume{
		Name: "cache",
		Size: quantityPtr("1024Mi"),
	}
	if !storageVolumeEqual(a, c) {
		t.Errorf("expected 1Gi == 1024Mi to be equal")
	}
}

func TestStorageVolumeEqual_DetectsRealChanges(t *testing.T) {
	t.Parallel()

	sc := "fast"
	base := &airunwayv1alpha1.StorageVolume{
		Name:             "cache",
		ClaimName:        "",
		MountPath:        "/model-cache",
		Purpose:          airunwayv1alpha1.VolumePurposeModelCache,
		ReadOnly:         false,
		Size:             quantityPtr("100Gi"),
		StorageClassName: &sc,
		AccessMode:       corev1.ReadWriteMany,
	}

	mutate := func(f func(v *airunwayv1alpha1.StorageVolume)) *airunwayv1alpha1.StorageVolume {
		cp := *base
		// deep-copy fields the mutator might touch
		if base.Size != nil {
			q := *base.Size
			cp.Size = &q
		}
		if base.StorageClassName != nil {
			s := *base.StorageClassName
			cp.StorageClassName = &s
		}
		f(&cp)
		return &cp
	}

	tests := []struct {
		name string
		mod  func(v *airunwayv1alpha1.StorageVolume)
	}{
		{"size changed", func(v *airunwayv1alpha1.StorageVolume) { v.Size = quantityPtr("200Gi") }},
		{"mount path changed", func(v *airunwayv1alpha1.StorageVolume) { v.MountPath = "/other" }},
		{"read-only changed", func(v *airunwayv1alpha1.StorageVolume) { v.ReadOnly = true }},
		{"storage class changed", func(v *airunwayv1alpha1.StorageVolume) { *v.StorageClassName = "slow" }},
		{"access mode changed", func(v *airunwayv1alpha1.StorageVolume) { v.AccessMode = corev1.ReadWriteOnce }},
		{"claim name changed", func(v *airunwayv1alpha1.StorageVolume) { v.ClaimName = "external-pvc" }},
		{"purpose changed", func(v *airunwayv1alpha1.StorageVolume) { v.Purpose = airunwayv1alpha1.VolumePurposeCustom }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if storageVolumeEqual(base, mutate(tc.mod)) {
				t.Errorf("expected inequality after mutation %q", tc.name)
			}
		})
	}

	// Equal copy should remain equal.
	if !storageVolumeEqual(base, mutate(func(*airunwayv1alpha1.StorageVolume) {})) {
		t.Errorf("expected equal copies to compare equal")
	}
}

func TestStorageVolumeEqual_NilHandling(t *testing.T) {
	t.Parallel()
	if !storageVolumeEqual(nil, nil) {
		t.Errorf("nil == nil should be equal")
	}
	v := &airunwayv1alpha1.StorageVolume{Name: "x"}
	if storageVolumeEqual(nil, v) || storageVolumeEqual(v, nil) {
		t.Errorf("nil and non-nil should not be equal")
	}
}
