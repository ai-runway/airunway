# Direct vLLM Provider

Direct vLLM runs the OpenAI-compatible `vllm serve` server directly as Kubernetes `Deployment` and `Service` resources. Use it when you want the newest vLLM model support or need a specific vLLM launch image before a managed provider has caught up.

For a less hands-on experience, prefer a managed provider such as Dynamo, KubeRay, KAITO, or llm-d when it supports your model and serving mode.

## When to use Direct vLLM

Use Direct vLLM when:

- the model is supported by vLLM but not yet available through another provider path;
- you need to choose a vLLM nightly, stable, or custom launch image;
- an official vLLM recipe provides known-good flags for the exact Hugging Face model ID;
- you want a plain Kubernetes `Deployment` instead of a provider-specific upstream CRD.

Avoid Direct vLLM when you need provider-managed routing, autoscaling, or production guardrails that are specific to Dynamo, KubeRay, KAITO, or llm-d.

## Install the provider shim

Install the core controller first, then install the Direct vLLM provider shim:

```bash
kubectl apply -f https://raw.githubusercontent.com/kaito-project/airunway/main/deploy/controller.yaml
kubectl apply -f https://raw.githubusercontent.com/kaito-project/airunway/main/providers/vllm/deploy/vllm.yaml
```

The vLLM provider registers an `InferenceProviderConfig` named `vllm`. The Web UI shows it as **Direct vLLM** after registration.

## Hugging Face token secret

For gated models, create a Kubernetes secret in the same namespace as the `ModelDeployment`:

```bash
kubectl create secret generic vllm-hf-token \
  --from-literal=HF_TOKEN=<your-token> \
  -n <model-namespace>
```

Reference it from the deployment:

```yaml
spec:
  secrets:
    huggingFaceToken: vllm-hf-token
```

## Basic deployment

Use explicit provider selection when you specifically want Direct vLLM:

```yaml
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: phi4-direct-vllm
  namespace: default
spec:
  provider:
    name: vllm
  model:
    id: microsoft/Phi-4-mini-instruct
    source: huggingface
  engine:
    type: vllm
    image: vllm/vllm-openai:cu130-nightly
    args:
      tensor-parallel-size: "1"
  resources:
    gpu:
      count: 1
```

`spec.engine.image` is the preferred image override for Direct vLLM. The older top-level `spec.image` field still exists for compatibility, but do not set both to different values.

## Disaggregated serving

Direct vLLM can also render separate prefill and decode Deployments when `serving.mode` is `disaggregated`. Provide GPU counts for both components:

```yaml
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: phi4-direct-vllm-disagg
  namespace: default
spec:
  provider:
    name: vllm
  model:
    id: microsoft/Phi-4-mini-instruct
    source: huggingface
  engine:
    type: vllm
    image: vllm/vllm-openai:cu130-nightly
  serving:
    mode: disaggregated
  scaling:
    prefill:
      replicas: 1
      gpu:
        count: 1
    decode:
      replicas: 1
      gpu:
        count: 1
```

## Official vLLM recipes

The Web UI can look up official recipes from `recipes.vllm.ai` for an **exact** Hugging Face model ID match. When you apply a recipe, Airunway materializes the recipe into normal deployment fields:

- `spec.engine.image`
- `spec.engine.args`
- `spec.engine.extraArgs`
- `spec.env`
- GPU resource defaults
- recipe provenance annotations under `metadata.annotations["airunway.ai/recipe.*"]`

The controller does not fetch recipes during reconciliation. Recipe provenance annotations are informational; they do not replace the materialized image, args, env, or resources.

Server-side recipe alternative references are restricted to the configured recipes base URL before fetching.

## Auto-selection behavior

Direct vLLM registers a low-priority selection rule for GPU vLLM workloads. Managed providers such as Dynamo and KubeRay have higher-priority rules and will be selected first when they match. To force Direct vLLM, set:

```yaml
spec:
  provider:
    name: vllm
```

## Image status

The provider records selected image details in `status.image`, including the requested image, resolved digest when available, source classification, and verification status. Digest resolution is reused when the requested image has not changed and the status already contains a resolved digest.
