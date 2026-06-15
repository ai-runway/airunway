import type * as k8s from '@kubernetes/client-node';

import logger from '../lib/logger';
import { getProviderDisplayName } from '../lib/providers';

const KAITO_WORKSPACE_CRD = 'workspaces.kaito.sh';
const KAITO_NAMESPACE = 'kaito-workspace';
const KAITO_OPERATOR_POD_SELECTOR = 'app.kubernetes.io/name=workspace,app.kubernetes.io/instance=kaito-workspace';
// The AKS AI-toolchain-operator add-on installs KAITO in kube-system. Verified
// against a live `--enable-ai-toolchain-operator` cluster, the add-on operator
// POD carries ONLY the bare `app=ai-toolchain-operator` label — it does NOT
// carry `app.kubernetes.io/name` (that key is present on the Deployment but not
// propagated to the pod template). So this pod probe must match on `app`; using
// `app.kubernetes.io/name=ai-toolchain-operator` here would match nothing.
// NOTE: the Go provider shim probes the Deployment instead and intentionally
// uses `app.kubernetes.io/name=ai-toolchain-operator` — see
// providers/kaito/upstream_health.go (listWorkspaceController). The two paths
// key off different labels on purpose because they inspect different objects.
const KAITO_AKS_ADDON_POD_SELECTOR = 'app=ai-toolchain-operator';
const DYNAMO_CRD = 'dynamographdeployments.nvidia.com';
const DYNAMO_NAMESPACE = 'dynamo-system';
const DYNAMO_OPERATOR_POD_SELECTOR = 'control-plane=controller-manager,app.kubernetes.io/name=dynamo-operator,app.kubernetes.io/instance=dynamo-platform';
const KUBERAY_CRD = 'rayservices.ray.io';
const KUBERAY_NAMESPACE = 'ray-system';
const KUBERAY_OPERATOR_POD_SELECTOR = 'app.kubernetes.io/name=kuberay-operator,app.kubernetes.io/instance=kuberay-operator';

export interface InstallationStatus {
  installed: boolean;
  crdFound?: boolean;
  operatorRunning?: boolean;
  requiresCRD?: boolean;
  version?: string;
  message?: string;
}

export type RuntimeProviderId = 'kaito' | 'dynamo' | 'kuberay';

interface RuntimeInstallationProbe {
  providerName: string;
  crdDisplayName?: string;
  crdName: string;
  operatorNamespace: string;
  operatorPodSelectors: string[];
  fallbackPodSelectors: string[];
  crossNamespaceFallbackPodSelectors?: string[];
}

interface OperatorPodProbeResult {
  ready: boolean;
  namespace?: string;
  selector?: string;
  podName?: string;
  error?: string;
}

export interface RuntimeInstallationAdapter {
  checkCRDExists(crdName: string): Promise<boolean>;
  listNamespacedPods(input: {
    namespace: string;
    labelSelector: string;
    operationName: string;
  }): Promise<k8s.V1Pod[]>;
  listPodsForAllNamespaces(input: {
    labelSelector: string;
    operationName: string;
  }): Promise<k8s.V1Pod[]>;
}

function getK8sStatusCode(error: unknown): number | undefined {
  const e = error as
    | { statusCode?: number; response?: { statusCode?: number } }
    | undefined;
  return e?.statusCode || e?.response?.statusCode;
}

function getK8sErrorMessage(error: unknown): string {
  const e = error as
    | {
        body?: { message?: string };
        response?: { body?: { message?: string } };
        message?: string;
      }
    | undefined;
  return e?.body?.message || e?.response?.body?.message || e?.message || String(error);
}

const RUNTIME_INSTALLATION_PROBES: Record<RuntimeProviderId, RuntimeInstallationProbe> = {
  kaito: {
    providerName: 'KAITO',
    crdDisplayName: 'KAITO workspace CRD',
    crdName: KAITO_WORKSPACE_CRD,
    operatorNamespace: KAITO_NAMESPACE,
    operatorPodSelectors: [KAITO_OPERATOR_POD_SELECTOR],
    fallbackPodSelectors: ['app.kubernetes.io/name=workspace'],
    // The AKS add-on pod only ever lives in kube-system, never in
    // kaito-workspace, so it is matched exclusively in the cross-namespace
    // pass. Listing it explicitly here (rather than relying on the implicit
    // `crossNamespaceFallbackPodSelectors = fallbackPodSelectors` default)
    // keeps add-on detection working even if KAITO later gains other
    // same-namespace fallbacks, and avoids a guaranteed-empty query for
    // `app=ai-toolchain-operator` against kaito-workspace on every probe.
    crossNamespaceFallbackPodSelectors: ['app.kubernetes.io/name=workspace', KAITO_AKS_ADDON_POD_SELECTOR],
  },
  dynamo: {
    providerName: 'Dynamo',
    crdName: DYNAMO_CRD,
    operatorNamespace: DYNAMO_NAMESPACE,
    operatorPodSelectors: [DYNAMO_OPERATOR_POD_SELECTOR],
    fallbackPodSelectors: ['app.kubernetes.io/name=dynamo-operator', 'control-plane=controller-manager'],
    crossNamespaceFallbackPodSelectors: ['app.kubernetes.io/name=dynamo-operator'],
  },
  kuberay: {
    providerName: 'KubeRay',
    crdName: KUBERAY_CRD,
    operatorNamespace: KUBERAY_NAMESPACE,
    operatorPodSelectors: [KUBERAY_OPERATOR_POD_SELECTOR],
    fallbackPodSelectors: ['app.kubernetes.io/name=kuberay-operator'],
  },
};

function isRunningAndReadyPod(pod: k8s.V1Pod): boolean {
  const containerStatuses = pod.status?.containerStatuses || [];
  return pod.status?.phase === 'Running'
    && containerStatuses.length > 0
    && containerStatuses.every((status) => status.ready);
}

export async function checkRuntimeProviderInstallationStatus(
  adapter: RuntimeInstallationAdapter,
  providerId: string,
  status?: { ready?: boolean },
  providerName?: string,
  requiresCRD = true,
): Promise<InstallationStatus> {
  if (!requiresCRD) {
    const ready = status?.ready === true;
    return {
      installed: ready,
      crdFound: true,
      operatorRunning: ready,
      requiresCRD: false,
      message: ready
        ? 'Runtime is ready to use.'
        : 'Provider is registered but not ready yet.',
    };
  }

  if (isRuntimeProviderId(providerId)) {
    return checkOperatorBackedRuntimeInstallationStatus(adapter, providerId);
  }

  const installed = status?.ready === true;
  const displayName = providerName || getProviderDisplayName(providerId);
  return {
    installed,
    crdFound: installed,
    operatorRunning: installed,
    requiresCRD: true,
    message: installed
      ? `${displayName} is installed and running`
      : `${displayName} is registered but not ready`,
  };
}

export async function checkOperatorBackedRuntimeInstallationStatus(
  adapter: RuntimeInstallationAdapter,
  providerId: RuntimeProviderId
): Promise<InstallationStatus> {
  const probe = RUNTIME_INSTALLATION_PROBES[providerId];
  const crdDisplayName = probe.crdDisplayName || `${probe.providerName} CRD`;
  const [crdFound, operatorProbe] = await Promise.all([
    adapter.checkCRDExists(probe.crdName),
    findReadyOperatorPod(
      adapter,
      probe.operatorNamespace,
      probe.operatorPodSelectors,
      probe.fallbackPodSelectors,
      `check${probe.providerName.replace(/[^a-zA-Z0-9]/g, '')}OperatorPods`,
      probe.crossNamespaceFallbackPodSelectors
    ),
  ]);
  const operatorRunning = operatorProbe.ready;
  const installed = crdFound && operatorRunning;

  let message: string;
  if (crdFound && operatorRunning) {
    const location = operatorProbe.namespace && operatorProbe.namespace !== probe.operatorNamespace
      ? ` in ${operatorProbe.namespace}`
      : '';
    message = `${crdDisplayName} found and ${probe.providerName} operator pods are ready${location}`;
  } else if (crdFound && operatorProbe.error) {
    message = `${crdDisplayName} found but ${probe.providerName} operator pods could not be checked: ${operatorProbe.error}`;
  } else if (crdFound) {
    message = `${crdDisplayName} found but no ready ${probe.providerName} operator pods were detected in ${probe.operatorNamespace} or matching known provider labels`;
  } else {
    message = `${crdDisplayName} not found`;
  }

  return {
    installed,
    crdFound,
    operatorRunning,
    requiresCRD: true,
    message,
  };
}

async function findReadyOperatorPod(
  adapter: RuntimeInstallationAdapter,
  namespace: string,
  operatorPodSelectors: string[],
  fallbackPodSelectors: string[],
  operationName: string,
  crossNamespaceFallbackPodSelectors: string[] = fallbackPodSelectors,
): Promise<OperatorPodProbeResult> {
  const selectors = Array.from(new Set([...operatorPodSelectors, ...fallbackPodSelectors]));
  const crossNamespaceSelectors = Array.from(new Set(crossNamespaceFallbackPodSelectors));
  let firstError: string | undefined;

  for (const selector of selectors) {
    try {
      const pods = await adapter.listNamespacedPods({
        namespace,
        labelSelector: selector,
        operationName: `${operationName}:${namespace}`,
      });
      const readyPod = pods.find((pod) => isRunningAndReadyPod(pod));
      if (readyPod) {
        return {
          ready: true,
          namespace,
          selector,
          podName: readyPod.metadata?.name,
        };
      }
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode !== 404 && !firstError) {
        firstError = getK8sErrorMessage(error);
        logger.warn({ error: firstError, namespace, selector }, 'Unable to check provider operator pods in expected namespace');
      }
    }
  }

  for (const selector of crossNamespaceSelectors) {
    try {
      const pods = await adapter.listPodsForAllNamespaces({
        labelSelector: selector,
        operationName: `${operationName}:all-namespaces`,
      });
      const readyPod = pods.find((pod) => isRunningAndReadyPod(pod));
      if (readyPod) {
        return {
          ready: true,
          namespace: readyPod.metadata?.namespace,
          selector,
          podName: readyPod.metadata?.name,
        };
      }
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode !== 404 && !firstError) {
        firstError = getK8sErrorMessage(error);
        logger.warn({ error: firstError, selector }, 'Unable to check provider operator pods across namespaces');
      }
    }
  }

  return { ready: false, error: firstError };
}

function isRuntimeProviderId(providerId: string): providerId is RuntimeProviderId {
  return providerId === 'kaito' || providerId === 'dynamo' || providerId === 'kuberay';
}
