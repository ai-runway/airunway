// Package dynamointent defines Runway's bounded Dynamo profiling contract.
package dynamointent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

const AttemptAnnotation = "airunway.ai/dynamo-attempt"
const HashAnnotation = "airunway.ai/dynamo-intent-hash"

// Spec is deliberately limited to the profiling features supported by both
// Dynamo 1.1.1 and 1.5. It does not describe a fixed serving topology.
type Spec struct {
	Hardware       Hardware  `json:"hardware"`
	SearchStrategy string    `json:"searchStrategy,omitempty"`
	Workload       *Workload `json:"workload,omitempty"`
	SLA            *SLA      `json:"sla,omitempty"`
}
type Hardware struct {
	TotalGPUs      int32    `json:"totalGpus"`
	GPUSKU         string   `json:"gpuSku,omitempty"`
	VRAMMB         *float64 `json:"vramMb,omitempty"`
	NumGPUsPerNode *int32   `json:"numGpusPerNode,omitempty"`
}
type Workload struct {
	ISL         *int32   `json:"isl,omitempty"`
	OSL         *int32   `json:"osl,omitempty"`
	RequestRate *float64 `json:"requestRate,omitempty"`
	Concurrency *float64 `json:"concurrency,omitempty"`
}
type SLA struct {
	TTFT *float64 `json:"ttft,omitempty"`
	ITL  *float64 `json:"itl,omitempty"`
	E2E  *float64 `json:"e2eLatency,omitempty"`
}

func overrides(md *api.ModelDeployment) (map[string]json.RawMessage, error) {
	if md.Spec.Provider == nil || md.Spec.Provider.Name != "dynamo" || md.Spec.Provider.Overrides == nil {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &m); err != nil {
		return nil, fmt.Errorf("invalid Dynamo overrides: %w", err)
	}
	return m, nil
}
func Enabled(md *api.ModelDeployment) bool {
	m, err := overrides(md)
	if err != nil {
		return false
	}
	var mode string
	_ = json.Unmarshal(m["deploymentMode"], &mode)
	return mode == "intent"
}

// Parse returns nil for manual mode and legacy intent overrides.
func Parse(md *api.ModelDeployment) (*Spec, error) {
	m, err := overrides(md)
	if err != nil {
		return nil, err
	}
	raw, ok := m["intent"]
	if !ok {
		return nil, nil
	}
	if !Enabled(md) {
		return nil, fmt.Errorf("provider.overrides.intent requires deploymentMode: intent")
	}
	if _, ok := m["spec"]; ok {
		return nil, fmt.Errorf("intent and legacy overrides.spec cannot be combined")
	}
	var spec *Spec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("invalid Dynamo intent: %w", err)
	}
	if spec == nil {
		return nil, fmt.Errorf("intent must be an object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("intent must contain one JSON object")
	}
	return spec, nil
}

// Validate is shared by admission and provider reconciliation.
func Validate(md *api.ModelDeployment) error {
	spec, err := Parse(md)
	if err != nil || spec == nil {
		return err
	}
	if spec.Hardware.TotalGPUs < 1 || spec.Hardware.TotalGPUs > 64 {
		return fmt.Errorf("intent.hardware.totalGpus must be between 1 and 64")
	}
	if spec.SearchStrategy != "" && spec.SearchStrategy != "rapid" {
		return fmt.Errorf("automatic configuration currently supports searchStrategy: rapid only")
	}
	if spec.Hardware.NumGPUsPerNode != nil && *spec.Hardware.NumGPUsPerNode <= 0 {
		return fmt.Errorf("intent.hardware.numGpusPerNode must be positive")
	}
	if spec.Hardware.VRAMMB != nil && !positive(*spec.Hardware.VRAMMB) {
		return fmt.Errorf("intent.hardware.vramMb must be positive")
	}
	if w := spec.Workload; w != nil {
		if w.ISL != nil && *w.ISL <= 0 || w.OSL != nil && *w.OSL <= 0 {
			return fmt.Errorf("intent workload token lengths must be positive")
		}
		if w.Concurrency != nil && w.RequestRate != nil {
			return fmt.Errorf("specify requestRate or concurrency, not both")
		}
		if w.Concurrency != nil && !positive(*w.Concurrency) || w.RequestRate != nil && !positive(*w.RequestRate) {
			return fmt.Errorf("intent workload traffic must be positive")
		}
	}
	if s := spec.SLA; s != nil {
		if s.E2E != nil && (s.TTFT != nil || s.ITL != nil) {
			return fmt.Errorf("specify e2eLatency or ttft/itl, not both")
		}
		for _, v := range []*float64{s.TTFT, s.ITL, s.E2E} {
			if v != nil && !positive(*v) {
				return fmt.Errorf("intent latency targets must be positive")
			}
		}
	}
	if md.Spec.Resources != nil {
		return fmt.Errorf("automatic configuration uses intent.hardware.totalGpus, not resources")
	}
	if md.Spec.Scaling != nil {
		return fmt.Errorf("automatic configuration chooses replicas; omit scaling")
	}
	if md.Spec.Serving != nil && md.Spec.Serving.Mode == api.ServingModeDisaggregated {
		return fmt.Errorf("automatic configuration chooses topology; omit serving.mode")
	}
	if md.Spec.Image != "" || md.Spec.Engine.Image != "" || md.Spec.Engine.ContextLength != nil || md.Spec.Engine.TrustRemoteCode || md.Spec.Engine.EnforceEager || len(md.Spec.Engine.Args) > 0 || len(md.Spec.Engine.ExtraArgs) > 0 {
		return fmt.Errorf("custom engine images and arguments require manual configuration")
	}
	if md.Spec.Model.ServedName != "" {
		return fmt.Errorf("custom servedName requires manual configuration")
	}
	if len(md.Spec.Env) > 0 || len(md.Spec.NodeSelector) > 0 || len(md.Spec.Tolerations) > 0 {
		return fmt.Errorf("custom env, nodeSelector and tolerations require manual configuration")
	}
	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" && md.Spec.Secrets.HuggingFaceToken != "hf-token-secret" {
		return fmt.Errorf("automatic configuration uses hf-token-secret; custom secret names require manual configuration")
	}
	if md.Spec.Model.Storage != nil && len(md.Spec.Model.Storage.Volumes) > 0 {
		return fmt.Errorf("automatic configuration does not support storage volumes; use manual configuration or legacy overrides with an explicit modelCache.pvcModelPath")
	}
	if md.Spec.PodTemplate != nil && md.Spec.PodTemplate.Metadata != nil && (len(md.Spec.PodTemplate.Metadata.Labels) > 0 || len(md.Spec.PodTemplate.Metadata.Annotations) > 0) {
		return fmt.Errorf("custom pod metadata requires manual configuration")
	}
	if md.Annotations["airunway.ai/dynamo-test-backend"] == "mocker" {
		return fmt.Errorf("mocker settings require manual configuration")
	}
	return nil
}
func positive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

// GPUCount returns an intent budget or the ordinary aggregate GPU allocation.
func GPUCount(md *api.ModelDeployment) int32 {
	if s, err := Parse(md); err == nil && s != nil {
		return s.Hardware.TotalGPUs
	}
	if md.Spec.Resources != nil && md.Spec.Resources.GPU != nil {
		return md.Spec.Resources.GPU.Count
	}
	return 0
}

// Fingerprint excludes metadata, routing, upstream defaults and observed status.
func Fingerprint(md *api.ModelDeployment) (string, error) {
	m, err := overrides(md)
	if err != nil {
		return "", err
	}
	input := map[string]any{"model": md.Spec.Model, "backend": md.ResolvedEngineType(), "secrets": md.Spec.Secrets, "nodeSelector": md.Spec.NodeSelector, "tolerations": md.Spec.Tolerations, "env": md.Spec.Env}
	if s, err := Parse(md); err != nil {
		return "", err
	} else if s != nil {
		c := *s
		if c.SearchStrategy == "" {
			c.SearchStrategy = "rapid"
		}
		input["intent"] = c
	} else {
		input["overrides"] = m
		input["resources"] = md.Spec.Resources
		input["scaling"] = md.Spec.Scaling
		input["serving"] = md.Spec.Serving
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	// Decode raw legacy overrides before the final encoding so JSON key order
	// and whitespace never count as a new profiling intent.
	var canonical any
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return "", err
	}
	raw, err = json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum), nil
}

// ValidateUpdate enforces the upstream request lifecycle at the Runway boundary.
// Changing the attempt annotation is an explicit request for a fresh profiling run.
func ValidateUpdate(old, next *api.ModelDeployment) error {
	if !Enabled(old) && !Enabled(next) {
		return nil
	}
	attempt := next.Annotations[AttemptAnnotation]
	if len(attempt) > 64 {
		return fmt.Errorf("Dynamo attempt token must be at most 64 characters")
	}
	for _, r := range attempt {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return fmt.Errorf("Dynamo attempt token contains invalid characters")
		}
	}
	if old.Annotations[AttemptAnnotation] != attempt {
		return nil
	}
	if old.Status.Provider == nil || old.Status.Provider.RequestRef == nil {
		return nil
	}
	if old.Status.Provider.Intent != nil {
		switch old.Status.Provider.Intent.Phase {
		case "", "Pending", "Failed":
			return nil
		}
	}
	before, err := Fingerprint(old)
	if err != nil {
		return err
	}
	after, err := Fingerprint(next)
	if err != nil {
		return err
	}
	if before != after {
		return fmt.Errorf("profiling inputs are locked; use Reconfigure or change the %s annotation to create a new request", AttemptAnnotation)
	}
	return nil
}
