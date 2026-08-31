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
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
