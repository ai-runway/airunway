// Package e2eutil provides shared helpers for the GPU end-to-end test suite.
//
// The helpers are intentionally dependency-free: they shell out to kubectl and
// speak HTTP with net/http, so the module needs no Kubernetes client libraries.
// All cluster interaction goes through these functions.
package e2eutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// WaitFor polls fn every interval until it returns nil or the timeout expires.
// On timeout it fails the test with the description and the last error returned
// by fn.
func WaitFor(t *testing.T, timeout, interval time.Duration, desc string, fn func() error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Evaluate once immediately so a fast-ready condition does not wait a full
	// interval before its first check.
	if err := fn(); err == nil {
		return
	}

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s (timeout %v): %v", desc, timeout, lastErr)
			return
		case <-ticker.C:
			if err := fn(); err != nil {
				lastErr = err
				t.Logf("waiting for %s: %v", desc, err)
			} else {
				return
			}
		}
	}
}

// Kubectl runs a kubectl command and returns its trimmed combined output.
// On error it fails the test.
func Kubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := KubectlMayFail(t, args...)
	if err != nil {
		t.Fatalf("kubectl %s failed: %v\nOutput: %s", strings.Join(args, " "), err, out)
	}
	return out
}

// KubectlMayFail runs a kubectl command and returns its trimmed combined output
// and error without failing the test. Callers that expect failure (polling,
// idempotent deletes) use this.
func KubectlMayFail(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	t.Logf("running: kubectl %s", strings.Join(args, " "))
	output, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(output))
	if out != "" {
		t.Logf("output: %s", out)
	}
	return out, err
}

// KubectlApply applies a manifest from a byte slice via stdin. It is used for
// applying fixtures that the suite has patched in-memory (e.g. injecting a
// StorageClass), so nothing is written to disk.
func KubectlApply(t *testing.T, manifest []byte) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(manifest)
	t.Log("running: kubectl apply -f - (in-memory manifest)")
	output, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(output))
	if out != "" {
		t.Logf("output: %s", out)
	}
	return out, err
}

// MDJSONPath returns a jsonpath field from a ModelDeployment. Empty string on
// error (the caller is typically polling).
func MDJSONPath(t *testing.T, name, namespace, jsonpath string) string {
	t.Helper()
	out, err := KubectlMayFail(t, "get", "modeldeployment", name, "-n", namespace,
		"-o", "jsonpath="+jsonpath)
	if err != nil {
		return ""
	}
	return out
}

// ChatResponse is the minimal shape of an OpenAI /v1/chat/completions response
// needed to assert a non-empty completion. Reasoning models (e.g. Qwen3 served
// by KAITO) may return the generated text in a separate reasoning field with
// content null, so both are captured.
type ChatResponse struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// GatewayChatCompletion posts a chat-completion request to the inference gateway
// LB at endpoint, routing to (and validating against) model. It returns the
// generated text — message.content, or message.reasoning when a reasoning model
// emits its text there with content null. The gateway routes on the request
// body "model" field via the X-Gateway-Model-Name header, and the backend
// validates the same value against its served model name, so model must be the
// value the suite reads from status.gateway.modelName.
func GatewayChatCompletion(endpoint, model string, timeout time.Duration) (string, error) {
	url := fmt.Sprintf("http://%s/v1/chat/completions", endpoint)
	// max_tokens is generous: reasoning models spend tokens in a think phase
	// before emitting an answer, so a tiny budget can yield empty content.
	payload := fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"Say hello in one word."}],"max_tokens":64}`,
		model,
	)

	client := &http.Client{Timeout: timeout}
	resp, err := client.Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var parsed ChatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("response is not valid JSON: %v: %s", err, truncate(string(body), 300))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("server error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("response has no choices: %s", truncate(string(body), 300))
	}
	// A reasoning model may put its generated text in reasoning with content
	// null; either non-empty field proves the model produced a completion.
	if c := parsed.Choices[0].Message.Content; c != "" {
		return c, nil
	}
	return parsed.Choices[0].Message.Reasoning, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
