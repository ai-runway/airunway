# Observability

AI Runway exposes Prometheus metrics from both the controller and inference provider pods, with a pre-built Grafana dashboard for visualization.

## What's tracked

- **Controller metrics** - reconciliation duration, errors, provider selection events
- **Deployment state** - aggregate deployment phase counts by provider/phase, replica counts
- **Platform engineering indicators** - phase transitions (deployment frequency, change failure rate), lead time (creation → ready), infrastructure provisioning duration
- **Inference engine metrics** - vLLM request queues, time-to-first-token, KV-cache utilization, token throughput (via provider PodMonitors)

> [!NOTE]
>
> The controller emits both classic histogram buckets and native histograms for improved compatibility. When using Prometheus with native histogram support enabled (`--enable-feature=native-histograms`), users may need to configure their ServiceMonitor with `scrapeClassicHistograms: true`.

## Getting started

See the [Observability Demo](../demos/observability/README.md) for a complete walkthrough covering:

1. Installing kube-prometheus-stack
2. Configuring Prometheus to scrape the controller (ServiceMonitor + RBAC)
3. Setting up PodMonitors for each inference provider (KAITO, Dynamo, KubeRay, llm-d)
4. Importing the Grafana dashboard

## Kubernetes Events

The controller also emits Kubernetes events for key lifecycle moments:

```text
Events:
  Type    Reason              Message
  ----    ------              -------
  Normal  ProviderSelected    Selected provider 'kaito': matched capabilities
  Normal  ResourceCreated     Created Workspace 'my-model'
  Warning ProviderError       Provider resource in error state: insufficient GPUs
  Warning DriftDetected       Provider resource was modified directly, reconciling
```

## See also

- [Architecture Overview](architecture.md)
- [Controller Architecture](controller-architecture.md)
