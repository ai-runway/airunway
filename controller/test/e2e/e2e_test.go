//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kaito-project/airunway/controller/test/utils"
)

// namespace where the project is deployed in
const namespace = "airunway-system"

// serviceAccountName created for the project
const serviceAccountName = "airunway-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "airunway-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "airunway-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		if skipDeploy {
			By("skipping deploy (SKIP_DEPLOY=true, using existing cluster)")
			return
		}

		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "--ignore-not-found", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("cleaning up the metrics role binding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		if skipDeploy {
			By("skipping undeploy (SKIP_DEPLOY=true)")
			return
		}

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=airunway-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			By("waiting for the webhook service endpoints to be ready")
			verifyWebhookEndpointsReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpointslices.discovery.k8s.io", "-n", namespace,
					"-l", "kubernetes.io/service-name=airunway-webhook-service",
					"-o", "jsonpath={range .items[*]}{range .endpoints[*]}{.addresses[*]}{end}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Webhook endpoints should exist")
				g.Expect(output).ShouldNot(BeEmpty(), "Webhook endpoints not yet ready")
			}
			Eventually(verifyWebhookEndpointsReady, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		It("should have the webhook server cert secret", func() {
			By("validating that the webhook server cert Secret exists")
			verifyWebhookCert := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secrets", "airunway-webhook-server-cert", "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyWebhookCert).Should(Succeed())
		})

		It("should have CA injection for mutating webhooks", func() {
			By("checking CA injection for mutating webhooks")
			verifyCAInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"mutatingwebhookconfigurations.admissionregistration.k8s.io",
					"airunway-mutating-webhook-configuration",
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				mwhOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(mwhOutput)).To(BeNumerically(">", 10))
			}
			Eventually(verifyCAInjection).Should(Succeed())
		})

		It("should have CA injection for validating webhooks", func() {
			By("checking CA injection for validating webhooks")
			verifyCAInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"validatingwebhookconfigurations.admissionregistration.k8s.io",
					"airunway-validating-webhook-configuration",
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				vwhOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(vwhOutput)).To(BeNumerically(">", 10))
			}
			Eventually(verifyCAInjection).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})

	Context("ModelDeployment Lifecycle", Ordered, func() {
		// These tests require KAITO operator and provider to be installed.
		// Skip when running standalone `make test-e2e` without KAITO.
		if os.Getenv("KAITO_INSTALLED") != "true" {
			return
		}

		var portForwardCmd *exec.Cmd

		AfterAll(func() {
			By("cleaning up the CPU ModelDeployment")
			cmd := exec.Command("kubectl", "delete", "-f",
				"test/e2e/testdata/cpu-modeldeployment.yaml", "--ignore-not-found")
			_, _ = utils.Run(cmd)

			By("cleaning up the invalid ModelDeployment")
			cmd = exec.Command("kubectl", "delete", "modeldeployment", "invalid-test",
				"--ignore-not-found", "-n", "default")
			_, _ = utils.Run(cmd)

			By("cleaning up per-engine capability test ModelDeployments")
			for _, name := range []string{"e2e-vllm-no-gpu", "e2e-llamacpp-no-gpu"} {
				cmd = exec.Command("kubectl", "delete", "modeldeployment", name,
					"--ignore-not-found", "-n", "default")
				_, _ = utils.Run(cmd)
			}

			if portForwardCmd != nil && portForwardCmd.Process != nil {
				_ = portForwardCmd.Process.Kill()
			}
		})

		It("should have KAITO provider registered and ready", func() {
			By("verifying InferenceProviderConfig 'kaito' exists and is ready")
			verifyProvider := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "InferenceProviderConfig 'kaito' should exist")

				cmd = exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito",
					"-o", "jsonpath={.status.ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("true"), "KAITO provider should be ready")
			}
			Eventually(verifyProvider, 2*time.Minute, time.Second).Should(Succeed())
		})

		It("should have per-engine capabilities in KAITO provider config", func() {
			By("verifying engines are stored as objects with per-engine fields")
			cmd := exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito",
				"-o", "jsonpath={.spec.capabilities.engines[*].name}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("vllm"), "KAITO should list vllm engine")
			Expect(output).To(ContainSubstring("llamacpp"), "KAITO should list llamacpp engine")

			By("verifying vllm engine has GPU support but not CPU support")
			cmd = exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito",
				"-o", "jsonpath={.spec.capabilities.engines[?(@.name=='vllm')].gpuSupport}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("true"), "vllm engine should have GPU support")

			cmd = exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito",
				"-o", "jsonpath={.spec.capabilities.engines[?(@.name=='vllm')].cpuSupport}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			// cpuSupport is omitempty and false, so it should be empty or not present
			Expect(output).To(BeEmpty(), "vllm engine should NOT have CPU support")

			By("verifying llamacpp engine has both GPU and CPU support")
			cmd = exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito",
				"-o", "jsonpath={.spec.capabilities.engines[?(@.name=='llamacpp')].gpuSupport}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("true"), "llamacpp engine should have GPU support")

			cmd = exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito",
				"-o", "jsonpath={.spec.capabilities.engines[?(@.name=='llamacpp')].cpuSupport}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("true"), "llamacpp engine should have CPU support")

			By("verifying per-engine serving modes are set")
			cmd = exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito",
				"-o", "jsonpath={.spec.capabilities.engines[?(@.name=='vllm')].servingModes}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("aggregated"), "vllm engine should support aggregated mode")

			By("verifying top-level servingModes, cpuSupport, gpuSupport are absent (per-engine only)")
			cmd = exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito",
				"-o", "jsonpath={.spec.capabilities.servingModes}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty(), "top-level servingModes should not exist")

			cmd = exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito",
				"-o", "jsonpath={.spec.capabilities.cpuSupport}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty(), "top-level cpuSupport should not exist")

			cmd = exec.Command("kubectl", "get", "inferenceproviderconfig", "kaito",
				"-o", "jsonpath={.spec.capabilities.gpuSupport}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty(), "top-level gpuSupport should not exist")
		})

		It("should reject vllm engine without GPU using per-engine capabilities", func() {
			By("creating a ModelDeployment with vllm engine but no GPU")
			vllmNoGPUYAML := `apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: e2e-vllm-no-gpu
  namespace: default
spec:
  model:
    source: custom
  engine:
    type: vllm
  resources:
    cpu: "4"
  image: "ghcr.io/kaito-project/aikit/llama3.2:1b"`

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(vllmNoGPUYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply vllm-no-gpu ModelDeployment")

			By("verifying the controller rejects vllm without GPU via data-driven validation")
			verifyFailed := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "modeldeployment", "e2e-vllm-no-gpu",
					"-n", "default", "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Failed"),
					fmt.Sprintf("ModelDeployment phase is %q, expected Failed", output))
			}
			Eventually(verifyFailed, 30*time.Second, time.Second).Should(Succeed())

			By("verifying the failure message mentions GPU requirement")
			cmd = exec.Command("kubectl", "get", "modeldeployment", "e2e-vllm-no-gpu",
				"-n", "default", "-o", "jsonpath={.status.message}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("requires GPU"),
				"Failure message should mention GPU requirement")
		})

		It("should auto-select llamacpp for CPU-only deployment via per-engine capabilities", func() {
			By("creating a CPU-only ModelDeployment without specifying engine")
			cpuOnlyYAML := `apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: e2e-llamacpp-no-gpu
  namespace: default
spec:
  model:
    source: custom
  resources:
    cpu: "4"
  image: "ghcr.io/kaito-project/aikit/llama3.2:1b"`

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cpuOnlyYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply CPU-only ModelDeployment")

			By("verifying engine auto-selected to llamacpp (the only CPU-capable engine)")
			verifyEngine := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "modeldeployment", "e2e-llamacpp-no-gpu",
					"-n", "default", "-o", "jsonpath={.status.engine.type}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("llamacpp"),
					fmt.Sprintf("Expected engine llamacpp (CPU-capable), got %q", output))
			}
			Eventually(verifyEngine, 30*time.Second, time.Second).Should(Succeed())

			By("verifying EngineSelected condition mentions auto-selection")
			verifyCondition := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "modeldeployment", "e2e-llamacpp-no-gpu",
					"-n", "default", "-o",
					"jsonpath={.status.conditions[?(@.type=='EngineSelected')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "EngineSelected condition should be True")
			}
			Eventually(verifyCondition, 30*time.Second, time.Second).Should(Succeed())
		})

		It("should create a CPU-only ModelDeployment and reach Running phase", func() {
			By("applying the CPU ModelDeployment fixture")
			cmd := exec.Command("kubectl", "apply", "-f", "test/e2e/testdata/cpu-modeldeployment.yaml")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply CPU ModelDeployment")

			By("waiting for ModelDeployment to reach Running phase")
			verifyRunning := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "modeldeployment", "llama-cpu-e2e",
					"-n", "default", "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"),
					fmt.Sprintf("ModelDeployment phase is %q, expected Running", output))
			}
			Eventually(verifyRunning, 10*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying status conditions exist")
			verifyConditions := func(g Gomega) {
				for _, condType := range []string{"Validated", "EngineSelected", "ProviderSelected"} {
					cmd := exec.Command("kubectl", "get", "modeldeployment", "llama-cpu-e2e",
						"-n", "default", "-o",
						fmt.Sprintf("jsonpath={.status.conditions[?(@.type=='%s')].status}", condType))
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("True"),
						fmt.Sprintf("Condition %s should be True, got %q", condType, output))
				}
			}
			Eventually(verifyConditions, 2*time.Minute, time.Second).Should(Succeed())

			By("verifying provider is KAITO")
			cmd = exec.Command("kubectl", "get", "modeldeployment", "llama-cpu-e2e",
				"-n", "default", "-o", "jsonpath={.status.provider.name}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("kaito"), "Expected provider to be 'kaito'")

			By("verifying engine type is llamacpp")
			cmd = exec.Command("kubectl", "get", "modeldeployment", "llama-cpu-e2e",
				"-n", "default", "-o", "jsonpath={.status.engine.type}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("llamacpp"), "Expected engine type to be 'llamacpp'")
		})

		It("should serve inference requests", func() {
			By("starting port-forward to the ModelDeployment pod")
			portForwardCmd = exec.Command("kubectl", "port-forward",
				"pod/llama-cpu-e2e-0", "8081:5000", "-n", "default")
			// Start port-forward in the background
			portForwardCmd.Stdout = GinkgoWriter
			portForwardCmd.Stderr = GinkgoWriter
			err := portForwardCmd.Start()
			Expect(err).NotTo(HaveOccurred(), "Failed to start port-forward")

			// Give port-forward time to establish
			time.Sleep(3 * time.Second)

			By("sending an inference request")
			verifyChatCompletion := func(g Gomega) {
				requestBody := `{"model":"llama-3.2-1b-instruct","messages":[{"role":"user","content":"Say hello in one word."}],"max_tokens":10}`
				cmd := exec.Command("curl", "-s", "-X", "POST",
					"http://localhost:8081/v1/chat/completions",
					"-H", "Content-Type: application/json",
					"-d", requestBody,
					"--max-time", "30")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Inference request failed")

				_, _ = fmt.Fprintf(GinkgoWriter, "Inference response: %s\n", output)

				var response map[string]interface{}
				g.Expect(json.Unmarshal([]byte(output), &response)).To(Succeed(),
					"Response should be valid JSON")
				g.Expect(response).To(HaveKey("choices"), "Response should have 'choices' field")

				choices, ok := response["choices"].([]interface{})
				g.Expect(ok).To(BeTrue(), "choices should be an array")
				g.Expect(choices).NotTo(BeEmpty(), "choices should not be empty")

				firstChoice, ok := choices[0].(map[string]interface{})
				g.Expect(ok).To(BeTrue())
				message, ok := firstChoice["message"].(map[string]interface{})
				g.Expect(ok).To(BeTrue(), "choice should have a message")
				content, ok := message["content"].(string)
				g.Expect(ok).To(BeTrue(), "message should have content")
				g.Expect(content).NotTo(BeEmpty(), "content should not be empty")
			}
			Eventually(verifyChatCompletion, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("cleaning up port-forward")
			if portForwardCmd.Process != nil {
				_ = portForwardCmd.Process.Kill()
				_, _ = portForwardCmd.Process.Wait()
				portForwardCmd = nil
			}
		})

		It("should reject invalid ModelDeployment (KAITO context)", func() {
			By("attempting to create a ModelDeployment with missing model.id for huggingface source")
			invalidYAML := `apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: invalid-test
  namespace: default
spec:
  model:
    source: huggingface
  engine:
    type: vllm
  resources:
    gpu:
      count: 1`

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(invalidYAML)
			output, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred(),
				"Expected webhook to reject ModelDeployment without model.id, but it was accepted")
			_, _ = fmt.Fprintf(GinkgoWriter, "Expected rejection output: %s\n", output)
		})
	})

	Context("llm-d ModelDeployment Lifecycle", Ordered, func() {
		// These tests require llm-d infrastructure and provider to be installed.
		// Skip when running standalone `make test-e2e` without llm-d.
		if os.Getenv("LLMD_INSTALLED") != "true" {
			return
		}

		BeforeAll(func() {
			if skipDeploy {
				By("skipping llm-d provider deploy (SKIP_DEPLOY=true)")
				return
			}

			By("creating HuggingFace token secret for llm-d")
			cmd := exec.Command("kubectl", "create", "secret", "generic", "hf-token-secret",
				"--from-literal=HF_TOKEN=ci-placeholder-token",
				"-n", "default")
			_, _ = utils.Run(cmd) // ignore error if already exists

			By("deploying the llm-d provider")
			cmd = exec.Command("make", "-C", "../providers/llmd", "deploy",
				fmt.Sprintf("IMG=%s", llmdProviderImage))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy llm-d provider")

			By("waiting for llm-d provider deployment to be available")
			cmd = exec.Command("kubectl", "wait", "--for=condition=Available", "deployment",
				"-n", "airunway-system", "-l", "control-plane=llmd-provider", "--timeout=120s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "llm-d provider not available within 120s")
		})

		AfterAll(func() {
			By("cleaning up the llm-d ModelDeployment")
			cmd := exec.Command("kubectl", "delete", "-f",
				"test/e2e/testdata/llmd-modeldeployment.yaml", "--ignore-not-found")
			_, _ = utils.Run(cmd)

			By("cleaning up the llm-d HF token secret")
			cmd = exec.Command("kubectl", "delete", "secret", "llm-d-hf-token",
				"-n", "default", "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should have llm-d provider registered and ready", func() {
			By("verifying InferenceProviderConfig 'llmd' exists and is ready")
			verifyProvider := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "inferenceproviderconfig", "llmd")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "InferenceProviderConfig 'llmd' should exist")

				cmd = exec.Command("kubectl", "get", "inferenceproviderconfig", "llmd",
					"-o", "jsonpath={.status.ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("true"), "llm-d provider should be ready")
			}
			Eventually(verifyProvider, 2*time.Minute, time.Second).Should(Succeed())
		})

		It("should create a GPU ModelDeployment and reach Running phase", func() {
			By("applying the llm-d ModelDeployment fixture")
			cmd := exec.Command("kubectl", "apply", "-f", "test/e2e/testdata/llmd-modeldeployment.yaml")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply llm-d ModelDeployment")

			By("waiting for ModelDeployment to reach Running phase")
			verifyRunning := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "modeldeployment", "llama-llmd-e2e",
					"-n", "default", "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"),
					fmt.Sprintf("ModelDeployment phase is %q, expected Running", output))
			}
			Eventually(verifyRunning, 15*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying provider is llmd")
			cmd = exec.Command("kubectl", "get", "modeldeployment", "llama-llmd-e2e",
				"-n", "default", "-o", "jsonpath={.status.provider.name}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("llmd"), "Expected provider to be 'llmd'")

			By("verifying engine type is vllm")
			cmd = exec.Command("kubectl", "get", "modeldeployment", "llama-llmd-e2e",
				"-n", "default", "-o", "jsonpath={.status.engine.type}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("vllm"), "Expected engine type to be 'vllm'")

			By("verifying status conditions are set")
			verifyConditions := func(g Gomega) {
				for _, condType := range []string{"Validated", "EngineSelected", "ProviderSelected"} {
					cmd := exec.Command("kubectl", "get", "modeldeployment", "llama-llmd-e2e",
						"-n", "default", "-o",
						fmt.Sprintf("jsonpath={.status.conditions[?(@.type=='%s')].status}", condType))
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("True"),
						fmt.Sprintf("Condition %s should be True, got %q", condType, output))
				}
			}
			Eventually(verifyConditions, 2*time.Minute, time.Second).Should(Succeed())
		})

		It("should create Deployment and Service in the default namespace", func() {
			By("verifying the Deployment exists")
			verifyDeployment := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "llama-llmd-e2e", "-n", "default")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Deployment 'llama-llmd-e2e' should exist")
			}
			Eventually(verifyDeployment, 2*time.Minute, time.Second).Should(Succeed())

			By("verifying the Service exists")
			verifyService := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", "llama-llmd-e2e", "-n", "default")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Service 'llama-llmd-e2e' should exist")
			}
			Eventually(verifyService, 2*time.Minute, time.Second).Should(Succeed())
		})

		It("should serve inference requests", func() {
			By("starting port-forward to the llm-d service")
			portForwardCmd := exec.Command("kubectl", "port-forward",
				"svc/llama-llmd-e2e", "8082:8000", "-n", "default")
			portForwardCmd.Stdout = GinkgoWriter
			portForwardCmd.Stderr = GinkgoWriter
			err := portForwardCmd.Start()
			Expect(err).NotTo(HaveOccurred(), "Failed to start port-forward")
			defer func() {
				if portForwardCmd.Process != nil {
					_ = portForwardCmd.Process.Kill()
					_, _ = portForwardCmd.Process.Wait()
				}
			}()

			time.Sleep(3 * time.Second)

			By("sending an inference request")
			verifyChatCompletion := func(g Gomega) {
				requestBody := `{"model":"meta-llama/Llama-3.2-1B-Instruct","messages":[{"role":"user","content":"Say hello in one word."}],"max_tokens":10}`
				cmd := exec.Command("curl", "-s", "-X", "POST",
					"http://localhost:8082/v1/chat/completions",
					"-H", "Content-Type: application/json",
					"-d", requestBody,
					"--max-time", "30")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Inference request failed")

				_, _ = fmt.Fprintf(GinkgoWriter, "Inference response: %s\n", output)

				var response map[string]interface{}
				g.Expect(json.Unmarshal([]byte(output), &response)).To(Succeed(),
					"Response should be valid JSON")
				g.Expect(response).To(HaveKey("choices"), "Response should have 'choices' field")

				choices, ok := response["choices"].([]interface{})
				g.Expect(ok).To(BeTrue(), "choices should be an array")
				g.Expect(choices).NotTo(BeEmpty(), "choices should not be empty")

				firstChoice, ok := choices[0].(map[string]interface{})
				g.Expect(ok).To(BeTrue())
				message, ok := firstChoice["message"].(map[string]interface{})
				g.Expect(ok).To(BeTrue(), "choice should have a message")
				content, ok := message["content"].(string)
				g.Expect(ok).To(BeTrue(), "message should have content")
				g.Expect(content).NotTo(BeEmpty(), "content should not be empty")
			}
			Eventually(verifyChatCompletion, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should reject invalid ModelDeployment", func() {
			By("attempting to create a ModelDeployment with explicit llmd provider but no GPU")
			invalidYAML := `apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: invalid-llmd-test
  namespace: default
spec:
  model:
    id: "meta-llama/Llama-3.2-1B-Instruct"
    source: huggingface
  provider:
    name: llmd
  engine:
    type: vllm`

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(invalidYAML)
			_, _ = utils.Run(cmd)

			By("verifying ProviderCompatible condition is False")
			verifyIncompatible := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "modeldeployment", "invalid-llmd-test",
					"-n", "default", "-o",
					"jsonpath={.status.conditions[?(@.type=='ProviderCompatible')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("False"),
					"ProviderCompatible condition should be False for GPU-less llmd deployment")
			}
			Eventually(verifyIncompatible, 30*time.Second, time.Second).Should(Succeed())

			By("cleaning up the invalid llm-d ModelDeployment")
			cmd = exec.Command("kubectl", "delete", "modeldeployment", "invalid-llmd-test",
				"--ignore-not-found", "-n", "default")
			_, _ = utils.Run(cmd)
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
