# Observability Demo

End-to-end walkthrough: spin up a local cluster, deploy the AI Runway controller, install Prometheus + Grafana, and see metrics flowing.

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/)
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [Helm](https://helm.sh/docs/intro/install/)

> [!IMPORTANT]
> Run the following commands from the root of the repository.

## 1. Create a Kind cluster

```bash
kind create cluster --name airunway
```

Verify it's running:

```bash
kubectl cluster-info --context kind-airunway
```

## 2. Build and deploy the AI Runway controller

Deploy the controller:

```bash
kubectl apply -f ./deploy/controller.yaml
```

Wait for the controller to be ready before proceeding:

```bash
kubectl rollout status deployment/airunway-controller-manager -n airunway-system --timeout=120s
```

## 3. Install kube-prometheus-stack

Add the Prometheus Community Helm repo and install the kube-prometheus-stack chart:

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

helm install prometheus prometheus-community/kube-prometheus-stack \
--namespace monitoring \
--create-namespace \
--set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
--set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false
```

> The `*SelectorNilUsesHelmValues=false` flags tell Prometheus to discover ServiceMonitors and PodMonitors in **all namespaces**, not just those installed by the Helm chart.

Wait for the stack to be ready:

```bash
kubectl rollout status deployment/prometheus-kube-prometheus-operator -n monitoring --timeout=120s
kubectl rollout status deployment/prometheus-grafana -n monitoring --timeout=120s
```

## 4. Configure Prometheus to scrape the controller

The controller exposes metrics on port `8443` over HTTPS with authn/authz enabled. The deployed manifest includes the metrics Service and RBAC roles, but the ServiceMonitor must be created separately.

Deploy a ServiceMonitor for the airunway-controller:

```bash
kubectl apply -f ./demos/observability/airunway-servicemonitor.yaml
```

> [!NOTE]
>
> The controller emits both classic and native histograms. By default, the [ServiceMonitor](./airunway-servicemonitor.yaml) has been configured with `scrapeClassicHistograms: true`, and the [sample dashboard](./sample-dashboard.json) queries classic `_bucket` metrics. To switch to native histograms, remove the `scrapeClassicHistograms: true` line and update the dashboard queries accordingly.

To grant Prometheus access to the metrics endpoint, a `ClusterRoleBinding` is needed for the Prometheus ServiceAccount.

```bash
kubectl apply -f ./demos/observability/metrics-rbac.yaml
```

> [!NOTE]
> If your Prometheus ServiceAccount has a different name, edit the `subjects` in [metrics-rbac.yaml](metrics-rbac.yaml) before applying. Run `kubectl get sa -n monitoring` to find the correct name.

## 5. Verify the target is up

Port-forward the Prometheus server to access the UI:

```bash
kubectl port-forward -n monitoring svc/prometheus-kube-prometheus-prometheus 9090:9090
```

Open [http://localhost:9090/targets](http://localhost:9090/targets) and look for the `airunway-controller-manager-metrics-monitor` target. It should show as **UP**.

## 6. Deploy a ModelDeployment

> [!TIP]
> Keep the port-forward for Prometheus server running and run the following commands in a new terminal tab.

To see metrics populate, create a KAITO provider then a sample KAITO-based ModelDeployment.

```bash
kubectl apply -f ./providers/kaito/deploy/kaito.yaml
```

Your KIND cluster will not have enough resources to run the model, so it will stay in `Pending` phase — but that's enough to generate controller metrics:

```bash
kubectl apply -f ./demos/observability/sample-modeldeployment.yaml
```

Verify metrics are being emitted (with the port-forward from step 5 still running):

```bash
curl -s http://localhost:9090/api/v1/query?query=airunway_deployment_status | python3 -m json.tool | head -20
```

Clean up when done:

```bash
kubectl delete -f ./demos/observability/sample-modeldeployment.yaml
```

> [!NOTE]
> If the Prometheus server is still being port-forwarded, you can stop it with `Ctrl+C` in the terminal where it's running.

## 7. Set up provider PodMonitors (optional)

Each provider (KAITO, KubeRay, llm-d) runs inference pods that can expose vLLM or engine-specific metrics. PodMonitor manifests are provided in this directory for each provider.

> [!WARNING]
> Review the PodMonitor manifests and adjust the selectors as needed.

Apply the ones matching the providers you have installed:

```bash
# KAITO
kubectl apply -f ./demos/observability/kaito-podmonitor.yaml

# KubeRay
kubectl apply -f ./demos/observability/kuberay-podmonitor.yaml

# llm-d
kubectl apply -f ./demos/observability/llmd-podmonitor.yaml
```

> [!NOTE]
> **Dynamo** is not listed here because it ships its own PodMonitors (`dynamo-worker`, `dynamo-frontend`, etc.) that scrape its pods automatically. No additional PodMonitor is needed.

Each PodMonitor:

- Targets pods by provider-specific labels (e.g., `kaito.sh/workspace`, `ray.io/node-type`, etc.)
- Allows cross-namespace discovery of provider pods
- Adds a `provider` label for cross-provider querying

### Correlating vLLM metrics with ModelDeployments

The provided PodMonitors add a `provider` label (`kuberay`, `kaito`, or `llm-d`) and normalize all vLLM metrics to use the `vllm:` prefix. vLLM metrics natively include a `model_name` label identifying the served model. Together, `provider` and `model_name` let you aggregate and filter across providers.

Dynamo uses its own PodMonitors and will not have a `provider` label. Filter Dynamo metrics by `namespace` or `job` instead.

| Provider | Notes                                                                                 |
| -------- | ------------------------------------------------------------------------------------- |
| KubeRay  | Ray Serve emits `ray_vllm_*` metrics; the PodMonitor normalizes them to `vllm:*`      |
| KAITO    | Standalone vLLM (vLLM presets only; llamacpp presets do not expose inference metrics) |
| Dynamo   | Ships its own PodMonitors; also emits `dynamo_*` runtime metrics                      |
| llm-d    | Standalone vLLM                                                                       |

All providers expose metrics under the `vllm:` prefix. KubeRay metrics are automatically normalized from `ray_vllm_*` to `vllm:*` via `metricRelabelings` in the PodMonitor, so the same PromQL queries work across all providers.

## 8. Import the Grafana dashboard

Get the Grafana admin password:

```bash
kubectl get secret -n monitoring -l app.kubernetes.io/component=admin-secret -o jsonpath="{.items[0].data.admin-password}" | base64 --decode ; echo
```

Port-forward the Grafana service:

```bash
kubectl port-forward -n monitoring svc/prometheus-grafana 3000:80
```

Open [http://localhost:3000](http://localhost:3000) (username is `admin`, password is the one retrieved in the previous step), then:

1. Go to **Dashboards → Import**
2. Import the [sample dashboard](./sample-dashboard.json)
3. Select your Prometheus data source

The dashboard includes panels for key controller and provider metrics, as well as DORA metrics for platform engineering teams:

| Section                        | What it shows                                                                               |
| ------------------------------ | ------------------------------------------------------------------------------------------- |
| **Deployment Status**          | Total deployments, phase breakdown, replica health                                          |
| **Reconciliation Performance** | Reconcile duration (p50/p95/p99), rate, errors by provider                                  |
| **DORA Metrics**               | Deployment frequency, lead time (creation → ready), change failure rate, provision duration |
| **Provider Activity**          | Per-provider reconciliation rate, deployment status table                                   |
| **Inference Engine Metrics**   | vLLM request queues, TTFT, KV-cache utilization, token throughput                           |

> [!TIP]
> The DORA Metrics section uses a **Deployment Frequency Window** dropdown (top of the dashboard) that controls the time range for deployment frequency, lead time, and provision duration queries. The default is **7 days**. Choose a shorter window (1h, 6h) during active development or a longer one (30d) for monthly reviews.

> [!NOTE]
> This dashboard is a starting point to get you up and running quickly. In production, consider splitting it into separate dashboards: an **operational** dashboard (Deployment Status, Reconciliation, Provider Activity) for on-call engineers answering "what's broken?" and a **platform engineering** dashboard (DORA Metrics, Inference Engine) for tracking trends over time.

## 9. Deploy alerting rules (optional)

Sample alerting rules are provided following Prometheus best practices: symptom-based alerts for paging, cause-based alerts for troubleshooting dashboards.

```bash
kubectl apply -f ./demos/observability/airunway-alerting-rules.yaml
```

The rules include:

| Alert                                  | Severity | What it detects                                         |
| -------------------------------------- | -------- | ------------------------------------------------------- |
| `AirunwayReconciliationErrorRateHigh`  | warning  | Error rate above 10% for 5+ minutes                     |
| `AirunwayReconciliationLatencyHigh`    | warning  | p95 reconciliation latency above 10s for 5+ minutes     |
| `AirunwayDeploymentStuck`              | warning  | Deployments stuck in Pending/Deploying for 1+ hour      |
| `AirunwayProviderReconciliationErrors` | info     | Sustained errors by provider and type (cause-based)     |
| `AirunwayChangeFailureRateHigh`        | info     | Change failure rate above 25% over 1 hour (cause-based) |

> [!NOTE]
> The `warning` alerts are symptom-based and suitable for paging. The `info` alerts are cause-based and intended for dashboards or ticket queues, not pagers. Tune thresholds and `for` durations to match your environment.

## Cleanup

Delete the Kind cluster when done:

```bash
kind delete cluster --name airunway
```

## Controller metrics reference

The AI Runway controller exposes the following Prometheus metrics:

### Operational

| Metric                                     | Type      | Labels                   | Description                                                                               |
| ------------------------------------------ | --------- | ------------------------ | ----------------------------------------------------------------------------------------- |
| `airunway_reconciliation_duration_seconds` | Histogram | `provider`               | Duration of each reconciliation loop                                                      |
| `airunway_reconciliation_errors_total`     | Counter   | `provider`, `error_type` | Reconciliation errors by type (validation, engine_selection, provider_selection, gateway) |
| `airunway_provider_selection_total`        | Counter   | `provider`, `reason`     | Provider selection events (reason: `manual` or `auto`)                                    |

### Deployment state

| Metric                         | Type  | Labels              | Description                                                                     |
| ------------------------------ | ----- | ------------------- | ------------------------------------------------------------------------------- |
| `airunway_deployment_status`   | Gauge | `provider`, `phase` | Number of ModelDeployments by provider and phase                                |
| `airunway_deployment_replicas` | Gauge | `provider`, `state` | Aggregate replica count across all ModelDeployments (desired, ready, available) |

### Platform engineering

| Metric                                           | Type      | Labels                               | Description                                                                           |
| ------------------------------------------------ | --------- | ------------------------------------ | ------------------------------------------------------------------------------------- |
| `airunway_deployment_phase_transitions_total`    | Counter   | `provider`, `from_phase`, `to_phase` | Phase transition events - use to compute deployment frequency and change failure rate |
| `airunway_deployment_ready_duration_seconds`     | Histogram | `provider`                           | Time from ModelDeployment creation to Running phase                                   |
| `airunway_deployment_provision_duration_seconds` | Histogram | `provider`                           | Time from first controller-observed Deploying phase to Running phase                  |

### Useful PromQL queries

```promql
# Total deployments
sum(airunway_deployment_status)

# Deployments by phase
sum by (phase) (airunway_deployment_status)

# Deployment frequency (last 24h)
sum(increase(airunway_deployment_phase_transitions_total{to_phase="Deploying"}[24h]))

# Lead time p95
histogram_quantile(0.95, sum by (le) (rate(airunway_deployment_ready_duration_seconds_bucket[1h])))

# Change failure rate
sum(rate(airunway_deployment_phase_transitions_total{to_phase="Failed"}[1h]))
  / clamp_min(sum(rate(airunway_deployment_phase_transitions_total{to_phase="Deploying"}[1h])), 1e-9)

# Provision duration p95 by provider
histogram_quantile(0.95, sum by (le, provider) (rate(airunway_deployment_provision_duration_seconds_bucket[1h])))
```
