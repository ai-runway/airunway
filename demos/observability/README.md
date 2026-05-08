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

To see metrics populate, create a sample ModelDeployment. Without a real provider installed, it will stay in `Pending` phase — but that's enough to generate controller metrics:

```bash
kubectl apply -f ./demos/observability/sample-modeldeployment.yaml
```

Verify metrics are being emitted (with the port-forward from step 5 still running):

```bash
curl -s http://localhost:9090/api/v1/query?query=airunway_deployment_phase | python3 -m json.tool | head -20
```

Clean up when done:

```bash
kubectl delete -f ./demos/observability/sample-modeldeployment.yaml
```

> [!NOTE]
> If the Prometheus server is still being port-forwarded, you can stop it with `Ctrl+C` in the terminal where it's running.

## 7. Set up provider PodMonitors (optional)

Each provider (KAITO, Dynamo, KubeRay, llm-d) runs inference pods that can expose vLLM or engine-specific metrics. PodMonitor manifests are provided in this directory for each provider.

Apply the ones matching the providers you have installed:

```bash
# KAITO
kubectl apply -f ./demos/observability/kaito-podmonitor.yaml

# Dynamo
kubectl apply -f ./demos/observability/dynamo-podmonitor.yaml

# KubeRay
kubectl apply -f ./demos/observability/kuberay-podmonitor.yaml

# llm-d
kubectl apply -f ./demos/observability/llmd-podmonitor.yaml
```

Each PodMonitor:

- Targets pods by provider-specific labels (e.g., `kaito.sh/workspace`, `ray.io/node-type`)
- Adds a `provider` label for cross-provider querying
- Adds a `model_deployment` label linking metrics back to the ModelDeployment name

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

## Cleanup

Delete the Kind cluster when done:

```bash
kind delete cluster --name airunway
```

## Controller metrics reference

The AI Runway controller exposes the following Prometheus metrics:

### Operational

| Metric                                     | Type      | Labels                   | Description                                                                      |
| ------------------------------------------ | --------- | ------------------------ | -------------------------------------------------------------------------------- |
| `airunway_reconciliation_duration_seconds` | Histogram | `provider`               | Duration of each reconciliation loop                                             |
| `airunway_reconciliation_errors_total`     | Counter   | `provider`, `error_type` | Reconciliation errors by type (validation, engine_selection, provider_selection) |
| `airunway_provider_selection_total`        | Counter   | `provider`, `reason`     | Provider auto-selection events                                                   |

### Deployment state

| Metric                         | Type  | Labels                       | Description                                                      |
| ------------------------------ | ----- | ---------------------------- | ---------------------------------------------------------------- |
| `airunway_deployment_phase`    | Gauge | `name`, `namespace`, `phase` | Current phase of each ModelDeployment (1 = active, 0 = inactive) |
| `airunway_deployment_replicas` | Gauge | `name`, `namespace`, `state` | Replica counts (desired, ready, available)                       |

### Platform engineering

| Metric                                           | Type      | Labels                               | Description                                                                           |
| ------------------------------------------------ | --------- | ------------------------------------ | ------------------------------------------------------------------------------------- |
| `airunway_deployment_phase_transitions_total`    | Counter   | `provider`, `from_phase`, `to_phase` | Phase transition events — use to compute deployment frequency and change failure rate |
| `airunway_deployment_ready_duration_seconds`     | Histogram | `provider`                           | Time from ModelDeployment creation to Running phase                                   |
| `airunway_deployment_provision_duration_seconds` | Histogram | `provider`                           | Time from ResourceCreated to Ready condition (infrastructure provisioning)            |

### Useful PromQL queries

```promql
# Total deployments
count(airunway_deployment_phase == 1)

# Deployments by phase
count by (phase) (airunway_deployment_phase == 1)

# Deployment frequency (last 24h)
sum(increase(airunway_deployment_phase_transitions_total{to_phase="Deploying"}[24h]))

# Lead time p95
histogram_quantile(0.95, sum by (le) (rate(airunway_deployment_ready_duration_seconds_bucket[1h])))

# Change failure rate
sum(rate(airunway_deployment_phase_transitions_total{to_phase="Failed"}[1h]))
  / sum(rate(airunway_deployment_phase_transitions_total{to_phase="Deploying"}[1h]))

# Provision duration p95 by provider
histogram_quantile(0.95, sum by (le, provider) (rate(airunway_deployment_provision_duration_seconds_bucket[1h])))
```
