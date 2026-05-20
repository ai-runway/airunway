---
name: deploy-controller
description: Interactively build, push or load, and deploy an airunway component (controller or any provider) to the cluster
argument-hint: "[component] [registry] [push|load] [platform]"
---

Build and redeploy an airunway component to the cluster.

**⚠️ Security: This skill performs destructive cluster operations. All steps require explicit human confirmation. Never run against production clusters. Verify the target cluster context before proceeding.**

## Step 0 — Safety check

Before anything else, run:
```bash
kubectl config current-context
```
Show the output and ask: "You are targeting cluster `<context>`. This skill will build, push, and deploy images. Is this a dev/test cluster? Proceed? (yes/no)"

**Do not proceed without explicit "yes".**

## Step 1 — Gather inputs

Parse $ARGUMENTS positionally: `[component] [registry] [push|load] [platform]`

Ask only for values not already provided by $ARGUMENTS.

**Question 1 — Which component?**
```
Which component do you want to deploy?
  1. controller
  2. provider: dynamo
  3. provider: kaito
  4. provider: kuberay
  5. provider: llmd
```

**Question 2 — Container registry prefix?**
"What container registry/username prefix should be used? (e.g. `myregistry`, `ghcr.io/myorg`)"

**Question 3 — Push or load?**
"Should the image be pushed to the remote registry or loaded into the local cluster?"
```
  1. push  — PUSH=true  (publishes to remote registry)
  2. load  — PUSH=false (loads into local cluster via docker buildx --load)
```

**Question 4 — Target platform?**
"What platform(s) should the image be built for? (default: `linux/amd64`)"
Common values: `linux/amd64`, `linux/arm64`, `linux/amd64,linux/arm64`

Confirm before proceeding: "Ready to build `<image>` for `<platform>` with PUSH=<true|false>. Proceed? (yes/no)"

## Step 2 — Resolve image names

| Component  | Image name pattern                           | Deployment name (airunway-system)        |
|------------|----------------------------------------------|------------------------------------------|
| controller | `<registry>/airunway-controller:latest`      | `airunway-controller-manager`            |
| dynamo     | `<registry>/airunway-dynamo-provider:latest` | `airunway-dynamo-provider`               |
| kaito      | `<registry>/airunway-kaito-provider:latest`  | (no separate deployment — skip rollout)  |
| kuberay    | `<registry>/airunway-kuberay-provider:latest`| (no separate deployment — skip rollout)  |
| llmd       | `<registry>/airunway-llmd-provider:latest`   | `airunway-llmd-provider`                 |

## Step 3 — Build image

Run sequentially. Stop and report the full error output if any step fails.

**If component = controller:**
```bash
make controller-docker-build CONTROLLER_IMG=<image> PUSH=<true|false> PLATFORM=<platform>
```

**If component = dynamo | kaito | kuberay | llmd:**
```bash
cd providers/<component>
make docker-build IMG=<image> PUSH=<true|false> PLATFORM=<platform>
cd ../..
```

## Step 4 — Deploy manifests to cluster

**If component = controller:**
```bash
make controller-deploy CONTROLLER_IMG=<image>
```

**If component = dynamo | kaito | kuberay | llmd:**
```bash
cd providers/<component>
make deploy IMG=<image>
cd ../..
```

## Step 5 — Rollout restart (only if deployment exists)

If the component has a known deployment name (see table above):
```bash
kubectl rollout restart deployment <deployment-name> -n airunway-system
kubectl rollout status deployment <deployment-name> -n airunway-system
```

## Step 6 — Report

Summarize:
- Component and image deployed
- Platform and PUSH value used
- Whether the rollout completed successfully
- Any warnings or errors encountered
