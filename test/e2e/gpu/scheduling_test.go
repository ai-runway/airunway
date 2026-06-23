//go:build e2e

package gpu

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kaito-project/airunway/test/e2e/gpu/e2eutil"
)

// schedulingDeadline bounds phase 1 (does the workload pod schedule at all). A
// pod still Pending-for-GPU after this, when no batch-mate can free a GPU, is
// classified; a pod Pending for a non-GPU reason fails fast.
const schedulingDeadline = 2 * time.Minute

// classifyScheduling implements the phase-1 scheduling check. It returns once
// the workload pod has left Pending (so phase-2 readiness can proceed), or it
// terminates the case via t.Skip (capacity) / t.Fatal (real failure):
//
//   - permanently unschedulable (pod wants more GPUs than any node has)  -> Skip
//   - Pending past the deadline solely for insufficient GPU              -> Skip
//   - Pending for a non-GPU reason (taint, image pull, quota)            -> Fatal
//   - scheduled                                                          -> return
//
// "Permanently unschedulable" is a static check: the case's max per-pod GPU
// demand against the largest node. It runs first so a hopeless case is skipped
// in seconds rather than burning the deadline.
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
			switch pod.phase {
			case "Pending":
				if reason, gpu := pendingReason(pod); !gpu {
					// Non-GPU scheduling failure: no point waiting 45 minutes.
					if time.Now().After(deadline) {
						t.Fatalf("FAILED: %s pod Pending for non-GPU reason: %s",
							tc.name, reason)
					}
				} else if time.Now().After(deadline) {
					// Still GPU-starved after the deadline. A batch-mate may yet
					// free a GPU, but we cannot prove progress here, so classify
					// as capacity rather than block the batch.
					t.Skipf("SKIPPED (capacity): %s pod Pending for GPU past %s: %s",
						tc.name, schedulingDeadline, reason)
				}
			default:
				// Scheduled (or already running/succeeded): phase 1 is done.
				return
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

// podInfo is the slice of pod status the scheduling classifier needs.
type podInfo struct {
	phase      string
	conditions []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
}

// firstWorkloadPod returns the first pod matching the case's podSelector.
func firstWorkloadPod(t *testing.T, tc testCase) (podInfo, bool) {
	t.Helper()
	out, err := e2eutil.KubectlMayFail(t, "get", "pods", "-n", tc.namespace,
		"-l", tc.podSelector, "-o", "json")
	if err != nil {
		return podInfo{}, false
	}
	var list struct {
		Items []struct {
			Status struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Items) == 0 {
		return podInfo{}, false
	}
	it := list.Items[0]
	return podInfo{phase: it.Status.Phase, conditions: it.Status.Conditions}, true
}

// pendingReason returns the PodScheduled=False message and whether it is due to
// insufficient GPU specifically.
func pendingReason(pod podInfo) (msg string, gpuInsufficient bool) {
	for _, c := range pod.conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			m := c.Message
			return m, strings.Contains(m, gpuResource)
		}
	}
	return "no PodScheduled=False condition", false
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
		return 0
	}
	// Disaggregated: the larger of the two component pods.
	if md.Spec.Scaling.Prefill != nil || md.Spec.Scaling.Decode != nil {
		max := 0
		if p := md.Spec.Scaling.Prefill; p != nil && p.GPU.Count > max {
			max = p.GPU.Count
		}
		if d := md.Spec.Scaling.Decode; d != nil && d.GPU.Count > max {
			max = d.GPU.Count
		}
		return max
	}
	return md.Spec.Resources.GPU.Count
}
