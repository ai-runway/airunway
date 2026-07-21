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
	"net"
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
	var lastErr error
	if err := fn(); err == nil {
		return
	} else {
		lastErr = err
	}

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

// PortForwardSession is a running `kubectl port-forward` process exposing a
// cluster Service on a local port. It can re-establish itself if the tunnel
// drops mid-test.
type PortForwardSession struct {
	t         *testing.T
	service   string
	namespace string
	remote    int
	local     int
	cmd       *exec.Cmd
	BaseURL   string // e.g. http://127.0.0.1:38291
}

// Stop terminates the port-forward process.
func (p *PortForwardSession) Stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
		p.cmd = nil
	}
}

// alive reports whether the local port currently accepts a TCP connection.
func (p *PortForwardSession) alive() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p.local), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// start launches the kubectl process and waits until the local port accepts
// connections (instead of a fixed sleep). Fails the test if it never comes up.
func (p *PortForwardSession) start() {
	p.t.Helper()
	cmd := exec.Command("kubectl", "port-forward",
		fmt.Sprintf("svc/%s", p.service),
		fmt.Sprintf("%d:%d", p.local, p.remote),
		"-n", p.namespace,
	)
	if err := cmd.Start(); err != nil {
		p.t.Fatalf("starting port-forward: %v", err)
	}
	p.cmd = cmd

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if p.alive() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.t.Fatalf("port-forward to svc/%s never became ready on :%d", p.service, p.local)
}

// EnsureReady re-establishes the tunnel if it has dropped. Callers that retry
// HTTP requests (e.g. the inference wait) call this so a tunnel that died
// mid-window is restarted rather than turning into connection-refused for the
// rest of the window.
func (p *PortForwardSession) EnsureReady() {
	if p.alive() {
		return
	}
	p.t.Logf("port-forward to svc/%s dropped; re-establishing", p.service)
	p.Stop()
	p.start()
}

// PortForwardService starts `kubectl port-forward svc/<service> <local>:<remote>`
// on a free local port and returns a session whose BaseURL points at it. It
// registers t.Cleanup to stop the process. Using a port-forward instead of the
// Service's external LoadBalancer IP makes inference reachable from any machine
// with kubectl access — the external IP can be blocked by network policy (e.g.
// an NSG that denies Internet-sourced inbound), which the API-server-tunneled
// port-forward sidesteps.
func PortForwardService(t *testing.T, service, namespace string, remotePort int) *PortForwardSession {
	t.Helper()

	// Pick a free local port by binding :0, reading the assigned port, then
	// closing the listener so kubectl can take it. start()'s readiness poll
	// absorbs kubectl's own bind latency (up to 15s). There is a small window
	// between the close here and kubectl binding the port; if another process
	// actually steals it in that window, start() fails the test at the poll
	// deadline — rare, and surfaced loudly rather than papered over.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free local port: %v", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	session := &PortForwardSession{
		t:         t,
		service:   service,
		namespace: namespace,
		remote:    remotePort,
		local:     localPort,
		BaseURL:   fmt.Sprintf("http://127.0.0.1:%d", localPort),
	}
	t.Logf("starting port-forward: kubectl port-forward svc/%s %d:%d -n %s",
		service, localPort, remotePort, namespace)
	session.start()
	t.Cleanup(session.Stop)
	return session
}

// GatewayChatCompletion posts a chat-completion request to baseURL (the gateway,
// reached via a port-forward), routing to (and validating against) model. It
// returns the generated text — message.content, or message.reasoning when a
// reasoning model emits its text there with content null. The gateway routes on
// the request body "model" field via the X-Gateway-Model-Name header, and the
// backend validates the same value against its served model name, so model must
// be the value the suite reads from status.gateway.modelName.
func GatewayChatCompletion(baseURL, model string, timeout time.Duration) (string, error) {
	url := baseURL + "/v1/chat/completions"
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

	return parseChatResponse(resp.StatusCode, body)
}

// parseChatResponse extracts the generated text from a chat-completion HTTP
// response. It is split out from GatewayChatCompletion (which does the HTTP) so
// the parse branches — non-200, invalid JSON, error envelope, empty choices,
// and the reasoning-model content-vs-reasoning fallback — can be unit-tested
// without a server.
func parseChatResponse(statusCode int, body []byte) (string, error) {
	if statusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", statusCode, truncate(string(body), 300))
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

// PinnedStorageClass is the StorageClass literal a Dynamo fixture pins, which
// the harness rewrites to the chosen --storage-class.
const PinnedStorageClass = "storageClassName: azurefile-premium"

// InjectStorageClass rewrites a Dynamo fixture's pinned StorageClass to sc.
//
//   - Non-Dynamo fixtures, or fixtures with no storage block at all, are
//     returned unchanged with ok=true.
//   - A Dynamo fixture that declares storage but lacks the pinned literal
//     returns ok=false (the caller should fail loudly — a silent no-op would
//     surface later as a confusing PVC mismatch).
//
// It is split out as a pure function so it can be unit-tested without a cluster.
func InjectStorageClass(provider string, raw []byte, sc string) (out []byte, ok bool) {
	s := string(raw)
	if provider != "dynamo" || !strings.Contains(s, "storageClassName:") {
		return raw, true
	}
	if !strings.Contains(s, PinnedStorageClass) {
		return raw, false
	}
	return []byte(strings.ReplaceAll(s, PinnedStorageClass, "storageClassName: "+sc)), true
}
