// Package sched holds the pure, cluster-free decision logic for the GPU e2e
// scheduling classifier. It lives in its own package (no `e2e` build tag, no
// TestMain GPU/gateway gate) so its unit tests run under plain `go test` in CI,
// where the rest of test/e2e/gpu cannot.
package sched

import "strings"

// GPUResource is the Kubernetes resource name for NVIDIA GPUs.
const GPUResource = "nvidia.com/gpu"

// PodCondition is the subset of a pod's status condition the classifier reads.
type PodCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// PodInfo is the slice of pod status the scheduling classifier needs.
type PodInfo struct {
	Phase      string
	Conditions []PodCondition
}

// UnschedulableReason inspects a Pending pod's PodScheduled condition.
//
// It returns notScheduled=true only when there is an explicit
// PodScheduled=False condition — i.e. the scheduler could not place the pod on
// any node. A pod that is scheduled but still Pending (pulling its image,
// initializing) has PodScheduled=True or no PodScheduled condition yet, so
// notScheduled=false and the caller hands off to the Running wait. gpu reports
// whether an unschedulable pod's reason names the GPU resource.
func UnschedulableReason(pod PodInfo) (notScheduled, gpu bool) {
	for _, c := range pod.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			return true, strings.Contains(c.Message, GPUResource)
		}
	}
	return false, false
}

// PodScheduledMessage returns the PodScheduled=False message for logging, or a
// placeholder when none is present.
func PodScheduledMessage(pod PodInfo) string {
	for _, c := range pod.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			if c.Message != "" {
				return c.Message
			}
			return c.Reason
		}
	}
	return "no PodScheduled=False condition"
}
