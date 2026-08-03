//go:build e2e

package gpu

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/ai-runway/airunway/test/e2e/gpu/sched"
)

// TestMain enforces the cheap, unambiguous cluster preconditions before any case
// runs, and fails fast with an actionable message if they are not met:
//   - at least one allocatable NVIDIA GPU across the nodes
//   - the inference gateway is present and Programmed
//
// Deeper preconditions (RWX StorageClass, image pull access, NFD labels) are not
// probed here; they surface through the per-case deadlines and debug bundles.
func TestMain(m *testing.M) {
	if err := checkGPUs(); err != nil {
		fmt.Fprintf(os.Stderr, "GPU precondition failed: %v\n", err)
		os.Exit(1)
	}
	if err := checkGateway(); err != nil {
		fmt.Fprintf(os.Stderr, "gateway precondition failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// nodeList is the minimal shape of `kubectl get nodes -o json` needed to sum
// allocatable GPUs and compute per-node capacity.
type nodeList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Allocatable map[string]string `json:"allocatable"`
		} `json:"status"`
	} `json:"items"`
}

// gpuResource is the allocatable resource key summed by the GPU gate. It is
// single-sourced from the sched package so the gate and the classifier agree.
const gpuResource = sched.GPUResource

// checkGPUs sums allocatable nvidia.com/gpu across all nodes by parsing the node
// JSON in Go. This deliberately avoids `kubectl -o jsonpath` with a bracketed
// resource key, whose escaping behavior is version-dependent and can silently
// return nothing on real GPU nodes.
func checkGPUs() error {
	nodes, err := getNodes()
	if err != nil {
		return err
	}
	total := 0
	fmt.Fprintln(os.Stderr, "GPU nodes:")
	for _, n := range nodes.Items {
		q := atoiQuantity(n.Status.Allocatable[gpuResource])
		if q > 0 {
			fmt.Fprintf(os.Stderr, "  %s: %d\n", n.Metadata.Name, q)
		}
		total += q
	}
	if total == 0 {
		return fmt.Errorf("no allocatable %s found on any node "+
			"(is the NVIDIA device plugin installed?)", gpuResource)
	}
	fmt.Fprintf(os.Stderr, "total allocatable GPUs: %d\n", total)
	return nil
}

// maxNodeGPUs returns the largest per-node allocatable GPU count, used by the
// static permanent-unschedulable check: a single pod requesting more GPUs than
// any node has can never schedule.
func maxNodeGPUs() (int, error) {
	nodes, err := getNodes()
	if err != nil {
		return 0, err
	}
	maxGPUs := 0
	for _, n := range nodes.Items {
		if q := atoiQuantity(n.Status.Allocatable[gpuResource]); q > maxGPUs {
			maxGPUs = q
		}
	}
	return maxGPUs, nil
}

func getNodes() (*nodeList, error) {
	out, err := exec.Command("kubectl", "get", "nodes", "-o", "json").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl get nodes: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var nodes nodeList
	if err := json.Unmarshal(out, &nodes); err != nil {
		return nil, fmt.Errorf("parsing node JSON: %w", err)
	}
	return &nodes, nil
}

// atoiQuantity parses a plain integer resource quantity (GPU counts are always
// whole numbers); returns 0 for empty or non-integer values. strconv.Atoi
// rejects trailing junk (e.g. "5x"), unlike fmt.Sscanf which would accept it.
func atoiQuantity(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// checkGateway verifies the inference gateway is present and Programmed.
func checkGateway() error {
	out, err := exec.Command("kubectl", "get", "gateway", "inference-gateway",
		"-n", "default", "-o",
		`jsonpath={.status.conditions[?(@.type=="Programmed")].status}`).Output()
	if err != nil {
		return fmt.Errorf("inference gateway not found in namespace default "+
			"(run `make setup-gateway`): %w", err)
	}
	if strings.TrimSpace(string(out)) != "True" {
		return fmt.Errorf("inference gateway is not Programmed (status=%q)",
			strings.TrimSpace(string(out)))
	}
	return nil
}
