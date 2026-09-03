# controller
// TODO(user): Add simple overview of use/purpose

## Description
// TODO(user): An in-depth paragraph about your project and overview of use

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/controller:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/controller:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/controller:tag
```

**NOTE:** The target generates three staged manifests:

- `dist/agentdeployment-webhook-guard.yaml` blocks AgentDeployment CREATE and
  UPDATE during mutating admission before the rollout starts.
- `dist/install.yaml` rolls out the controller without the AgentDeployment
  validating or mutating routes.
- `dist/agentdeployment-webhook.yaml` applies the complete validating and
  mutating configurations after the rollout.

2. Using the installer

Apply the guard before changing the Deployment, wait for the controller
rollout, apply the complete admission configuration, and only then remove the
guard:

```bash
set -euo pipefail

kubectl apply -f "https://raw.githubusercontent.com/<org>/controller/<tag-or-branch>/dist/agentdeployment-webhook-guard.yaml"
kubectl apply -f "https://raw.githubusercontent.com/<org>/controller/<tag-or-branch>/dist/install.yaml"
kubectl rollout restart deployment/airunway-controller-manager -n airunway-system
kubectl rollout status deployment/airunway-controller-manager -n airunway-system --timeout=5m
kubectl apply -f "https://raw.githubusercontent.com/<org>/controller/<tag-or-branch>/dist/agentdeployment-webhook.yaml"
kubectl wait secret/airunway-webhook-server-cert -n airunway-system --for=jsonpath='{.data.ca\.crt}' --timeout=5m
AIRUNWAY_WEBHOOK_CA_BUNDLE="$(kubectl get secret/airunway-webhook-server-cert -n airunway-system -o jsonpath='{.data.ca\.crt}')"
test -n "${AIRUNWAY_WEBHOOK_CA_BUNDLE}"
kubectl wait mutatingwebhookconfiguration/airunway-mutating-webhook-configuration --for="jsonpath={.webhooks[?(@.name==\"magentdeployment-v1alpha1.kb.io\")].clientConfig.caBundle}=${AIRUNWAY_WEBHOOK_CA_BUNDLE}" --timeout=5m
kubectl wait validatingwebhookconfiguration/airunway-validating-webhook-configuration --for="jsonpath={.webhooks[?(@.name==\"vagentdeployment-v1alpha1.kb.io\")].clientConfig.caBundle}=${AIRUNWAY_WEBHOOK_CA_BUNDLE}" --timeout=5m
kubectl delete --ignore-not-found=true -f "https://raw.githubusercontent.com/<org>/controller/<tag-or-branch>/dist/agentdeployment-webhook-guard.yaml"
```

If rollout or activation fails, leave the guard installed, correct the failure,
and resume from the failed command. The CA waits verify both activated routes
trust the controller's current serving certificate. Deleting the guard earlier
can send credential admission to incompatible or untrusted webhook endpoints.

Build the installer with an immutable digest or a new image tag for upgrades.
The explicit restart prevents an unchanged image string from short-circuiting
the rollout check against old pods.

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

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
