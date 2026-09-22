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
	"testing"
	"time"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestProviderUsesCoreStorageLifecycle(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{provider: providerNameKubeRay, want: true},
		{provider: providerNameLLMD, want: true},
		{provider: providerNameVLLM, want: true},
		{provider: "dynamo", want: false}, // Dynamo keeps its DGD-aware lifecycle.
		{provider: "kaito", want: false},  // KAITO rejects storage explicitly.
		{provider: "unknown", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := providerUsesCoreStorageLifecycle(tt.provider); got != tt.want {
				t.Fatalf("providerUsesCoreStorageLifecycle(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

func TestHasCurrentStorageFailure(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Status: airunwayv1alpha1.ModelDeploymentStatus{Conditions: []metav1.Condition{
			{
				Type:               airunwayv1alpha1.ConditionTypeStorageReady,
				Status:             metav1.ConditionFalse,
				Reason:             "PVCFailed",
				ObservedGeneration: 3,
			},
		}},
	}
	if !hasCurrentStorageFailure(md) {
		t.Fatal("expected current PVC failure to be recoverable")
	}

	md.Status.Conditions[0].ObservedGeneration = 2
	if hasCurrentStorageFailure(md) {
		t.Fatal("expected stale storage failure to be ignored")
	}

	md.Status.Conditions[0].ObservedGeneration = 3
	md.Status.Conditions[0].Reason = "UnrelatedFailure"
	if hasCurrentStorageFailure(md) {
		t.Fatal("expected unrelated failure to be preserved")
	}
}

func TestMarkStorageReady(t *testing.T) {
	tests := []struct {
		name       string
		phase      airunwayv1alpha1.DeploymentPhase
		message    string
		recovering bool
		wantPhase  airunwayv1alpha1.DeploymentPhase
		wantMsg    string
	}{
		{
			name:      "replaces stale PVC wait message",
			phase:     airunwayv1alpha1.DeploymentPhasePending,
			message:   "Waiting for storage PVCs to become ready",
			wantPhase: airunwayv1alpha1.DeploymentPhasePending,
			wantMsg:   "Model storage is ready; waiting for the provider workload",
		},
		{
			name:      "replaces terminating PVC teardown message",
			phase:     airunwayv1alpha1.DeploymentPhasePending,
			message:   "Stopping provider workloads and waiting for terminating storage PVCs",
			wantPhase: airunwayv1alpha1.DeploymentPhasePending,
			wantMsg:   "Model storage is ready; waiting for the provider workload",
		},
		{
			name:      "replaces stale download message",
			phase:     airunwayv1alpha1.DeploymentPhasePending,
			message:   "Model download in progress",
			wantPhase: airunwayv1alpha1.DeploymentPhasePending,
			wantMsg:   "Model storage is ready; waiting for the provider workload",
		},
		{
			name:      "describes immediately ready storage",
			phase:     airunwayv1alpha1.DeploymentPhasePending,
			wantPhase: airunwayv1alpha1.DeploymentPhasePending,
			wantMsg:   "Model storage is ready; waiting for the provider workload",
		},
		{
			name:       "recovers storage failure",
			phase:      airunwayv1alpha1.DeploymentPhaseFailed,
			message:    "Failed to prepare model storage",
			recovering: true,
			wantPhase:  airunwayv1alpha1.DeploymentPhasePending,
			wantMsg:    "Model storage is ready; waiting for the provider workload",
		},
		{
			name:      "preserves provider progress",
			phase:     airunwayv1alpha1.DeploymentPhasePending,
			message:   "Provider is preparing a workload",
			wantPhase: airunwayv1alpha1.DeploymentPhasePending,
			wantMsg:   "Provider is preparing a workload",
		},
		{
			name:      "preserves running status",
			phase:     airunwayv1alpha1.DeploymentPhaseRunning,
			message:   "Ready",
			wantPhase: airunwayv1alpha1.DeploymentPhaseRunning,
			wantMsg:   "Ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := &airunwayv1alpha1.ModelDeployment{
				Status: airunwayv1alpha1.ModelDeploymentStatus{
					Phase:   tt.phase,
					Message: tt.message,
				},
			}

			markStorageReady(md, tt.recovering)

			if md.Status.Phase != tt.wantPhase {
				t.Fatalf("phase = %q, want %q", md.Status.Phase, tt.wantPhase)
			}
			if md.Status.Message != tt.wantMsg {
				t.Fatalf("message = %q, want %q", md.Status.Message, tt.wantMsg)
			}
		})
	}
}

func TestReconcileStorageMarksTerminatingPVCWorkloadUnavailable(t *testing.T) {
	scheme := newTestScheme()
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "demo",
			Namespace:  "default",
			UID:        types.UID("md-uid"),
			Generation: 2,
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{Model: airunwayv1alpha1.ModelSpec{
			Source: airunwayv1alpha1.ModelSourceCustom,
			Storage: &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{
				Name: "data", ClaimName: "shared-data", MountPath: "/data",
			}}},
		}},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Phase: airunwayv1alpha1.DeploymentPhaseRunning,
			Endpoint: &airunwayv1alpha1.EndpointStatus{
				Service: "demo", Port: 8000,
			},
			Replicas: &airunwayv1alpha1.ReplicaStatus{Desired: 1, Ready: 1, Available: 1},
			Conditions: []metav1.Condition{{
				Type: airunwayv1alpha1.ConditionTypeReady, Status: metav1.ConditionTrue,
				Reason: "DeploymentReady", ObservedGeneration: 2,
			}},
		},
	}
	now := metav1.Now()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "shared-data",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{"kubernetes.io/pvc-protection"},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	r := newTestReconciler(scheme, nil, md, pvc)
	current := &airunwayv1alpha1.ModelDeployment{}
	key := types.NamespacedName{Name: md.Name, Namespace: md.Namespace}
	if err := r.Get(context.Background(), key, current); err != nil {
		t.Fatalf("getting ModelDeployment: %v", err)
	}
	base := current.DeepCopy()

	result, stop, err := r.reconcileStorage(context.Background(), current, base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stop || result.RequeueAfter != 5*time.Second {
		t.Fatalf("expected terminating storage to stop reconciliation and requeue after 5s, got stop=%v result=%+v", stop, result)
	}

	updated := &airunwayv1alpha1.ModelDeployment{}
	if err := r.Get(context.Background(), key, updated); err != nil {
		t.Fatalf("getting updated ModelDeployment: %v", err)
	}
	storageReady := apiMeta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeStorageReady)
	if storageReady == nil || storageReady.Status != metav1.ConditionFalse || storageReady.Reason != "PVCsTerminating" {
		t.Fatalf("unexpected StorageReady condition: %+v", storageReady)
	}
	ready := apiMeta.FindStatusCondition(updated.Status.Conditions, airunwayv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "StorageUnavailable" {
		t.Fatalf("unexpected Ready condition: %+v", ready)
	}
	if updated.Status.Endpoint != nil || updated.Status.Replicas != nil {
		t.Fatalf("expected stale serving status to be cleared, got endpoint=%+v replicas=%+v", updated.Status.Endpoint, updated.Status.Replicas)
	}
}

func TestHasHistoricalStorageFailure(t *testing.T) {
	md := &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 4},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Phase:   airunwayv1alpha1.DeploymentPhaseFailed,
			Message: "Model download failed: simulated failure",
			Conditions: []metav1.Condition{{
				Type:               airunwayv1alpha1.ConditionTypeModelDownloaded,
				Status:             metav1.ConditionFalse,
				Reason:             "DownloadFailed",
				ObservedGeneration: 3,
			}},
		},
	}
	if !hasHistoricalStorageFailure(md) {
		t.Fatal("expected a stale storage failure to be recoverable after the spec changes")
	}

	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	if hasHistoricalStorageFailure(md) {
		t.Fatal("must not recover an unrelated current phase")
	}

	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
	md.Status.Conditions[0].Reason = "ProviderFailed"
	if hasHistoricalStorageFailure(md) {
		t.Fatal("must not classify a provider failure as storage recovery")
	}

	md.Status.Conditions[0].Reason = "DownloadFailed"
	md.Status.Message = "Provider failed to create workload"
	if hasHistoricalStorageFailure(md) {
		t.Fatal("must not recover a provider failure that retained an old storage condition")
	}
}
