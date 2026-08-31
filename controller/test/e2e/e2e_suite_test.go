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
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ai-runway/airunway/controller/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "example.com/controller:v0.0.1"
	// llmdProviderImage is the llm-d provider image to be built and loaded for testing.
	llmdProviderImage = "llmd-provider:e2e"
	// skipDeploy skips image build/load and deploy/undeploy when testing against an existing cluster.
	skipDeploy = os.Getenv("SKIP_DEPLOY") == "true"
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting controller e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	if skipDeploy {
		By("skipping image build and load (SKIP_DEPLOY=true)")
		return
	}

	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	// TODO(user): If you want to change the e2e test vendor from Kind,
	// ensure the image is built and available, then remove the following block.
	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	if os.Getenv("LLMD_INSTALLED") == "true" {
		By("building the llm-d provider image")
		cmd = exec.Command("make", "-C", "../providers/llmd", "docker-build",
			fmt.Sprintf("IMG=%s", llmdProviderImage))
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the llm-d provider image")

		By("loading the llm-d provider image on Kind")
		err = utils.LoadImageToKindClusterWithName(llmdProviderImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the llm-d provider image into Kind")
	}
})

var _ = AfterSuite(func() {
})
