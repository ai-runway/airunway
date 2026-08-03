package sched

import "testing"

// TestUnschedulableReason pins the phase-1 classifier's decision logic — the
// site of the false-FAIL bug where a scheduled-but-image-pulling pod was treated
// as unschedulable. A pod is only "not scheduled" when it carries an explicit
// PodScheduled=False condition; PodScheduled=True or an absent condition means
// the scheduler placed it and the Running wait owns the rest.
//
// This test is intentionally in its own cluster-free package so it runs under
// plain `go test` in CI (the e2e package's TestMain os.Exit()s without a GPU).
func TestUnschedulableReason(t *testing.T) {
	cases := []struct {
		name             string
		conditions       []PodCondition
		wantNotScheduled bool
		wantGPU          bool
	}{
		{
			name:             "scheduled but pulling image (no PodScheduled=False)",
			conditions:       []PodCondition{{Type: "PodScheduled", Status: "True"}},
			wantNotScheduled: false,
			wantGPU:          false,
		},
		{
			name:             "no conditions yet (just created)",
			conditions:       nil,
			wantNotScheduled: false,
			wantGPU:          false,
		},
		{
			name: "unschedulable for insufficient GPU",
			conditions: []PodCondition{{Type: "PodScheduled", Status: "False",
				Message: "0/4 nodes are available: 4 Insufficient nvidia.com/gpu."}},
			wantNotScheduled: true,
			wantGPU:          true,
		},
		{
			name: "unschedulable for a non-GPU reason (taint)",
			conditions: []PodCondition{{Type: "PodScheduled", Status: "False",
				Message: "0/4 nodes are available: 4 node(s) had untolerated taint."}},
			wantNotScheduled: true,
			wantGPU:          false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := PodInfo{Phase: "Pending", Conditions: tc.conditions}
			notScheduled, gpu := UnschedulableReason(pod)
			if notScheduled != tc.wantNotScheduled {
				t.Errorf("notScheduled = %v, want %v", notScheduled, tc.wantNotScheduled)
			}
			if gpu != tc.wantGPU {
				t.Errorf("gpu = %v, want %v", gpu, tc.wantGPU)
			}
		})
	}
}

// TestPodScheduledMessage covers the message-extraction helper.
func TestPodScheduledMessage(t *testing.T) {
	t.Run("returns the message", func(t *testing.T) {
		pod := PodInfo{Conditions: []PodCondition{
			{Type: "PodScheduled", Status: "False", Reason: "Unschedulable", Message: "no GPU"},
		}}
		if got := PodScheduledMessage(pod); got != "no GPU" {
			t.Errorf("got %q, want %q", got, "no GPU")
		}
	})
	t.Run("falls back to reason when message empty", func(t *testing.T) {
		pod := PodInfo{Conditions: []PodCondition{
			{Type: "PodScheduled", Status: "False", Reason: "Unschedulable"},
		}}
		if got := PodScheduledMessage(pod); got != "Unschedulable" {
			t.Errorf("got %q, want %q", got, "Unschedulable")
		}
	})
	t.Run("placeholder when no PodScheduled=False", func(t *testing.T) {
		pod := PodInfo{Conditions: []PodCondition{{Type: "PodScheduled", Status: "True"}}}
		if got := PodScheduledMessage(pod); got != "no PodScheduled=False condition" {
			t.Errorf("got %q, want placeholder", got)
		}
	})
}
