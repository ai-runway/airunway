//go:build e2e

package gpu

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ai-runway/airunway/test/e2e/gpu/e2eutil"
	"github.com/ai-runway/airunway/test/e2e/gpu/sched"
)

// schedulingDeadline bounds phase 1 (does the workload pod schedule at all). A
// pod still Pending-for-GPU after this, when no batch-mate can free a GPU, is
// classified; a pod Pending for a non-GPU reason fails fast.
const schedulingDeadline = 2 * time.Minute

// classifyScheduling implements the phase-1 scheduling check. It returns once
// the workload pod has been admitted to a node (so the phase-2 Running wait can
// own image-pull and startup latency), or it terminates the case:
//
//   - permanently unschedulable (pod wants more GPUs than any node has)  -> Skip
//   - PodScheduled=False for insufficient GPU, past the deadline          -> Skip
//   - PodScheduled=False for a non-GPU reason, past the deadline          -> Fatal
//   - scheduled (Pending-but-pulling, Running, or no PodScheduled=False)  -> return
//
// A pod that is bound to a node but still Pending (pulling a multi-GB image)
// has no PodScheduled=False condition, so it counts as scheduled and is handed
// off to the Running wait rather than failed here.
//
// "Permanently unschedulable" is a static check: the case's max per-pod GPU
// demand against the largest node. It runs first so a hopeless case is skipped
// in seconds rather than burning the deadline.
//
// The decision logic (UnschedulableReason/PodScheduledMessage) lives in the
// cluster-free sched package so it can be unit-tested in CI.
func classifyScheduling(t *testing.T, tc testCase) {
	t.Helper()

	if demand := maxPodGPUDemand(t, tc); demand > 0 {
		maxNode, err := maxNodeGPUs()
		if err == nil && demand > maxNode {
			t.Skipf("SKIPPED (capacity): %s needs %d GPU on one node, "+
				"but the largest node has %d", tc.name, demand, maxNode)
		}
	}

	deadline := time.Now().Add(schedulingDeadline)
	for {
		pod, found := firstWorkloadPod(t, tc)
		if found {
			if pod.Phase != "Pending" {
				// Running/Succeeded/Failed: the pod was admitted to a node, so
				// phase 1 is done. Image-pull and startup latency belong to the
				// phase-2 Running wait, not here.
				return
			}
			// Pending: distinguish "not yet scheduled" from "scheduled, pulling".
			// A scheduled pod has no PodScheduled=False condition (it is True or
			// absent) — it is progressing, so hand off to the Running wait.
			notScheduled, gpu := sched.UnschedulableReason(pod)
			if !notScheduled {
				return
			}
			if time.Now().After(deadline) {
				if gpu {
					// Still GPU-starved after the deadline. A batch-mate may yet
					// free a GPU, but we cannot prove progress here, so classify
					// as capacity rather than block the batch.
					t.Skipf("SKIPPED (capacity): %s pod unschedulable for GPU past %s: %s",
						tc.name, schedulingDeadline, sched.PodScheduledMessage(pod))
				}
				// Unschedulable for a non-GPU reason (taint, affinity, quota):
				// no point waiting out the 45-minute Running timeout.
				t.Fatalf("FAILED: %s pod unschedulable for non-GPU reason: %s",
					tc.name, sched.PodScheduledMessage(pod))
			}
		} else if time.Now().After(deadline) {
			// No pod created at all within the scheduling window is a real fault
			// (bad fixture, provider not reconciling) — let phase 2 surface it
			// via the Running wait, which carries richer context.
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// schedulingSelector returns the label selector that matches the GPU-holding
// pod(s) for the scheduling check. It defaults to the case's podSelector, but a
// case may set workloadSelector to narrow to the GPU worker — important for
// Dynamo, whose graph-deployment selector also matches the GPU-less frontend/EPP
// pod. If the broad selector were used, firstWorkloadPod's Items[0] could be the
// frontend (which schedules instantly), making the capacity-SKIP path dead.
func schedulingSelector(tc testCase) string {
	if tc.workloadSelector != "" {
		return tc.workloadSelector
	}
	return tc.podSelector
}

// firstWorkloadPod returns the first pod matching the case's scheduling
// selector.
func firstWorkloadPod(t *testing.T, tc testCase) (sched.PodInfo, bool) {
	t.Helper()
	out, err := e2eutil.KubectlMayFail(t, "get", "pods", "-n", tc.namespace,
		"-l", schedulingSelector(tc), "-o", "json")
	if err != nil {
		return sched.PodInfo{}, false
	}
	var list struct {
		Items []struct {
			Status struct {
				Phase      string               `json:"phase"`
				Conditions []sched.PodCondition `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Logf("[%s] warning: could not parse pod list JSON: %v", tc.name, err)
		return sched.PodInfo{}, false
	}
	if len(list.Items) == 0 {
		return sched.PodInfo{}, false
	}
	it := list.Items[0]
	return sched.PodInfo{Phase: it.Status.Phase, Conditions: it.Status.Conditions}, true
}

// maxPodGPUDemand returns the largest single-pod GPU request the case's fixture
// produces, by reading the MD spec: aggregated -> resources.gpu.count;
// disaggregated -> max(prefill, decode) per-component count. A single pod binds
// to one node, so the maximum (not the sum) is what a node must satisfy.
func maxPodGPUDemand(t *testing.T, tc testCase) int {
	t.Helper()
	out, err := e2eutil.KubectlMayFail(t, "get", "modeldeployment", tc.mdName,
		"-n", tc.namespace, "-o", "json")
	if err != nil {
		return 0
	}
	var md struct {
		Spec struct {
			Resources struct {
				GPU struct {
					Count int `json:"count"`
				} `json:"gpu"`
			} `json:"resources"`
			Scaling struct {
				Prefill *struct {
					GPU struct {
						Count int `json:"count"`
					} `json:"gpu"`
				} `json:"prefill"`
				Decode *struct {
					GPU struct {
						Count int `json:"count"`
					} `json:"gpu"`
				} `json:"decode"`
			} `json:"scaling"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &md); err != nil {
		t.Logf("[%s] warning: could not parse MD spec for GPU demand: %v", tc.name, err)
		return 0
	}
	// Disaggregated: the larger of the two component pods.
	if md.Spec.Scaling.Prefill != nil || md.Spec.Scaling.Decode != nil {
		maxDemand := 0
		if p := md.Spec.Scaling.Prefill; p != nil && p.GPU.Count > maxDemand {
			maxDemand = p.GPU.Count
		}
		if d := md.Spec.Scaling.Decode; d != nil && d.GPU.Count > maxDemand {
			maxDemand = d.GPU.Count
		}
		return maxDemand
	}
	return md.Spec.Resources.GPU.Count
}
