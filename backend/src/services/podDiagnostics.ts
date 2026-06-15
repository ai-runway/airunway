import type * as k8s from '@kubernetes/client-node';
import type { PodFailureReason, PodPhase, PodStatus } from '@airunway/shared';

export interface PodDiagnosticsAdapter {
  listPodsByLabelSelector(input: {
    namespace: string;
    labelSelector: string;
    operationName: string;
  }): Promise<k8s.V1Pod[]>;
  listPodsByFieldSelector(input: {
    namespace: string;
    fieldSelector: string;
    limit?: number;
    operationName: string;
  }): Promise<k8s.V1Pod[]>;
  listEvents(input: {
    namespace: string;
    fieldSelector: string;
    operationName: string;
  }): Promise<k8s.CoreV1Event[]>;
  readPodLog(input: {
    name: string;
    namespace: string;
    container?: string;
    tailLines: number;
    timestamps: boolean;
    operationName: string;
  }): Promise<string>;
  logDebug(context: Record<string, unknown>, message: string): void;
  logError(context: Record<string, unknown>, message: string): void;
  getK8sStatusCode(error: unknown): number | undefined;
  getK8sErrorMessage(error: unknown): string;
}

export function toPodStatus(pod: k8s.V1Pod): PodStatus {
  const initStatuses = pod.status?.initContainerStatuses || [];
  const containerStatuses = pod.status?.containerStatuses || [];
  const allStatuses = [...initStatuses, ...containerStatuses];
  const waitingState = allStatuses.find((status) => status.state?.waiting)?.state?.waiting;
  const terminatedState = allStatuses.find((status) => status.state?.terminated)?.state?.terminated;

  return {
    name: pod.metadata?.name || 'unknown',
    phase: (pod.status?.phase as PodPhase) || 'Unknown',
    ready: containerStatuses.length > 0 && containerStatuses.every((status) => status.ready),
    restarts: allStatuses.reduce((sum, status) => sum + status.restartCount, 0),
    node: pod.spec?.nodeName,
    reason: waitingState?.reason || terminatedState?.reason || pod.status?.reason,
    message: waitingState?.message || terminatedState?.message || pod.status?.message,
  };
}

export async function getDeploymentPods(
  adapter: PodDiagnosticsAdapter,
  name: string,
  namespace: string
): Promise<PodStatus[]> {
  const podsByName = new Map<string, k8s.V1Pod>();
  const addPods = (pods: k8s.V1Pod[]) => {
    for (const pod of pods) {
      const podName = pod.metadata?.name;
      if (podName && !podsByName.has(podName)) {
        podsByName.set(podName, pod);
      }
    }
  };

  // Try multiple exact label selectors since different providers use different labels.
  // Some deployment stacks create related modules with different labels, so
  // aggregate across all exact matches instead of stopping at the first selector.
  const exactLabelSelectors = [
    `app.kubernetes.io/instance=${name}`,      // Standard K8s label (Dynamo)
    `airunway.ai/deployment=${name}`,          // AIRunway label
    `airunway.ai/model-deployment=${name}`,    // Pod-template label used by KubeRay
    `nvidia.com/dynamo-graph-deployment-name=${name}`, // Runtime label used by Dynamo/Grove pods
    `kaito.sh/workspace=${name}`,              // KAITO workspace label
  ];

  const listPodsByLabelSelector = async (labelSelector: string, operationName = 'getDeploymentPods'): Promise<k8s.V1Pod[]> => {
    try {
      const pods = await adapter.listPodsByLabelSelector({ namespace, labelSelector, operationName });
      if (pods.length > 0) {
        adapter.logDebug({ name, namespace, labelSelector, podCount: pods.length }, 'Found pods with selector');
      }
      return pods;
    } catch (error) {
      adapter.logDebug({ error, name, namespace, labelSelector }, 'Error trying label selector');
      return [];
    }
  };

  const exactSelectorResults = await Promise.all(
    exactLabelSelectors.map(labelSelector => listPodsByLabelSelector(labelSelector))
  );
  exactSelectorResults.forEach(addPods);

  // KubeRay creates pods with ray.io/cluster label set to a generated RayCluster name.
  // Modern Airunway KubeRay pods carry airunway.ai/model-deployment (handled above),
  // but keep this as a backwards-compatible fallback. Only accept an exact name or
  // the RayService-generated "<deployment>-raycluster..." form so deployments like
  // "demo" do not match unrelated clusters like "demo2" or "demo-extra".
  try {
    const rayPods = await adapter.listPodsByLabelSelector({
      namespace,
      labelSelector: 'ray.io/cluster',
      operationName: 'getDeploymentPods:kuberay',
    });

    const matchingPods = rayPods.filter(pod => {
      const clusterLabel = pod.metadata?.labels?.['ray.io/cluster'] || '';
      return clusterLabel === name || clusterLabel.startsWith(`${name}-raycluster`);
    });

    if (matchingPods.length > 0) {
      adapter.logDebug({ name, namespace, podCount: matchingPods.length }, 'Found KubeRay pods by cluster label prefix');
      addPods(matchingPods);
    }
  } catch (error) {
    adapter.logDebug({ error, name, namespace }, 'Error trying KubeRay cluster label selector');
  }

  if (podsByName.size === 0) {
    // Last-resort fallback for older or third-party manifests that only set app=<name>.
    // Avoid aggregating this broad label with canonical matches because unrelated pods
    // can legitimately share the same app label in a namespace.
    try {
      const labelSelector = `app=${name}`;
      const pods = await listPodsByLabelSelector(labelSelector, 'getDeploymentPods:fallbackApp');
      addPods(pods);
    } catch (error) {
      adapter.logDebug({ error, name, namespace }, 'Error trying fallback app label selector');
    }
  }

  const pods = Array.from(podsByName.values())
    .sort((a, b) => (a.metadata?.name || '').localeCompare(b.metadata?.name || ''));
  if (pods.length === 0) {
    adapter.logDebug({ name, namespace }, 'No pods found with any label selector');
    return [];
  }

  adapter.logDebug({ name, namespace, podCount: pods.length }, 'Found deployment pods');
  return pods.map((pod) => toPodStatus(pod));
}

export async function getPodFailureReasons(
  adapter: PodDiagnosticsAdapter,
  podName: string,
  namespace: string,
): Promise<PodFailureReason[]> {
  try {
    const events = await adapter.listEvents({
      namespace,
      fieldSelector: `involvedObject.name=${podName}`,
      operationName: 'getPodFailureReasons',
    });

    const reasons: PodFailureReason[] = [];

    for (const event of events) {
      // Focus on Warning events related to scheduling failures
      if (event.type !== 'Warning') {
        continue;
      }

      const reason = event.reason || 'Unknown';
      const message = event.message || '';

      // Analyze the event to determine if it's a resource constraint
      const isResourceConstraint = reason === 'FailedScheduling' ||
        message.toLowerCase().includes('insufficient');

      let resourceType: 'gpu' | 'cpu' | 'memory' | undefined;
      let canAutoscalerHelp = false;

      if (isResourceConstraint) {
        // Detect resource type from message
        if (message.includes('nvidia.com/gpu')) {
          resourceType = 'gpu';
          canAutoscalerHelp = true; // Autoscaler can add GPU nodes
        } else if (message.toLowerCase().includes('cpu')) {
          resourceType = 'cpu';
          canAutoscalerHelp = true;
        } else if (message.toLowerCase().includes('memory')) {
          resourceType = 'memory';
          canAutoscalerHelp = true;
        }

        // Check for taint-related failures (autoscaler can't help with these)
        if (message.toLowerCase().includes('taint') ||
          message.toLowerCase().includes('toleration')) {
          canAutoscalerHelp = false;
        }

        // Check for node selector failures (autoscaler can't help with these)
        if (message.toLowerCase().includes('node selector') ||
          message.toLowerCase().includes('didn\'t match')) {
          canAutoscalerHelp = false;
        }
      }

      reasons.push({
        reason,
        message,
        isResourceConstraint,
        resourceType,
        canAutoscalerHelp,
      });
    }

    return reasons;
  } catch (error) {
    adapter.logError({ error, podName, namespace }, 'Error getting pod failure reasons');
    return [];
  }
}

export async function getPodLogs(
  adapter: PodDiagnosticsAdapter,
  podName: string,
  namespace: string,
  options?: {
    container?: string;
    tailLines?: number;
    timestamps?: boolean;
  },
): Promise<string> {
  try {
    const container = await resolveLogContainer(adapter, podName, namespace, options?.container);
    const response = await adapter.readPodLog({
      name: podName,
      namespace,
      container,
      tailLines: options?.tailLines ?? 100,
      timestamps: options?.timestamps ?? false,
      operationName: 'getPodLogs',
    });

    // Strip ANSI color codes from logs
    const logs = response || '';
    const ansiRegex = /\x1b\[[0-9;]*m/g;
    return logs.replace(ansiRegex, '');
  } catch (error) {
    const statusCode = adapter.getK8sStatusCode(error);
    if (statusCode === 404) {
      throw new Error(`Pod '${podName}' not found in namespace '${namespace}'`);
    }
    adapter.logError({ error, podName, namespace }, 'Error getting pod logs');
    throw new Error(`Failed to get logs for pod '${podName}': ${adapter.getK8sErrorMessage(error)}`);
  }
}

export function selectLogContainer(pod: k8s.V1Pod): string | undefined {
  const containers = pod.spec?.containers || [];
  if (containers.length === 0) {
    return undefined;
  }

  const statuses = new Map((pod.status?.containerStatuses || []).map(status => [status.name, status]));
  const preferredNames = ['main', 'vllm', 'model', 'ray-head', 'ray-worker', 'inference', 'worker', 'server', 'frontend'];

  for (const name of preferredNames) {
    if (containers.some(container => container.name === name)) {
      return name;
    }
  }

  const readyContainer = containers.find(container => statuses.get(container.name)?.ready);
  return readyContainer?.name || containers[0].name;
}

async function resolveLogContainer(
  adapter: PodDiagnosticsAdapter,
  podName: string,
  namespace: string,
  requestedContainer?: string
): Promise<string | undefined> {
  if (requestedContainer) {
    return requestedContainer;
  }

  const pods = await adapter.listPodsByFieldSelector({
    namespace,
    fieldSelector: `metadata.name=${podName}`,
    limit: 1,
    operationName: 'getPodLogs:listPodByName',
  });

  const pod = pods[0];
  return pod ? selectLogContainer(pod) : undefined;
}
