/* eslint-disable @typescript-eslint/no-explicit-any */
import { afterEach, describe, expect, test } from 'bun:test';
import { kubernetesService } from './kubernetes';
import { mockServiceMethod } from '../test/helpers';
import { mockInferenceProviderConfig } from '../test/fixtures';

// Minimal pod shape these tests construct and match against label selectors.
interface PodLike {
  metadata?: { name?: string; labels?: Record<string, string> };
  status?: unknown;
}

// Arguments the Kubernetes client passes to the pod-listing methods we stub.
interface K8sCallArg {
  namespace?: string;
  labelSelector?: string;
}

// Writable view of the private KubernetesService internals these tests stub.
type MockableKubernetesService = {
  coreV1Api: Record<string, (arg: K8sCallArg) => Promise<unknown>>;
  customObjectsApi: Record<string, (arg?: K8sCallArg) => Promise<unknown>>;
};

const asMockable = (): MockableKubernetesService =>
  kubernetesService as unknown as MockableKubernetesService;

describe('KubernetesService - Runtime Status', () => {
  const restores: Array<() => void> = [];
  const kaitoOperatorSelector = 'app.kubernetes.io/name=workspace,app.kubernetes.io/instance=kaito-workspace';
  const dynamoOperatorSelector = 'control-plane=controller-manager,app.kubernetes.io/name=dynamo-operator,app.kubernetes.io/instance=dynamo-platform';
  const kuberayOperatorSelector = 'app.kubernetes.io/name=kuberay-operator,app.kubernetes.io/instance=kuberay-operator';

  afterEach(() => {
    restores.forEach((restore) => restore());
    restores.length = 0;
  });

  function mockProviderConfigs(items: unknown[]) {
    const service = asMockable();
    const original = service.customObjectsApi.listClusterCustomObject;
    service.customObjectsApi.listClusterCustomObject = async () => ({ items });
    restores.push(() => {
      service.customObjectsApi.listClusterCustomObject = original;
    });
  }

  function podMatchesSelector(pod: PodLike, selector?: string): boolean {
    if (!selector) return true;
    const labels = pod.metadata?.labels || {};
    return selector.split(',').every((part) => {
      const [key, value] = part.split('=');
      return labels[key] === value;
    });
  }

  function mockOperatorPods(namespace: string, selector: string, items: PodLike[], allNamespaceItems: PodLike[] = []) {
    const service = asMockable();
    const originalNamespaced = service.coreV1Api.listNamespacedPod;
    const originalAllNamespaces = service.coreV1Api.listPodForAllNamespaces;
    service.coreV1Api.listNamespacedPod = async (arg: K8sCallArg) => {
      expect(arg.namespace).toBe(namespace);
      const requestedSelector = arg.labelSelector;
      return { items: requestedSelector === selector ? items : items.filter((pod) => podMatchesSelector(pod, requestedSelector)) };
    };
    service.coreV1Api.listPodForAllNamespaces = async (arg: K8sCallArg) => {
      const requestedSelector = arg?.labelSelector;
      return { items: allNamespaceItems.filter((pod) => podMatchesSelector(pod, requestedSelector)) };
    };
    restores.push(() => {
      service.coreV1Api.listNamespacedPod = originalNamespaced;
      service.coreV1Api.listPodForAllNamespaces = originalAllNamespaces;
    });
  }

  test('uses annotation-driven installation status for KAITO runtime entries', async () => {
    const providerStatusChecks: any[] = [];

    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDInstallation', async () => ({ installed: true })),
      mockServiceMethod(kubernetesService as any, 'checkProviderInstallationStatus', async (...args: any[]) => {
        providerStatusChecks.push(args);
        return {
          installed: false,
          crdFound: false,
          operatorRunning: false,
          message: 'KAITO workspace CRD not found',
        };
      }),
      mockServiceMethod(kubernetesService, 'checkKaitoInstallationStatus', async () => {
        throw new Error('getRuntimesStatus should use annotation-driven provider status checks');
      }),
    );
    mockProviderConfigs([mockInferenceProviderConfig]);

    const runtimes = await kubernetesService.getRuntimesStatus();
    const kaito = runtimes.find((runtime) => runtime.id === 'kaito');

    expect(providerStatusChecks).toHaveLength(1);
    expect(providerStatusChecks[0][0]).toBe('kaito');
    expect(providerStatusChecks[0][1]).toEqual(mockInferenceProviderConfig.status);
    expect(providerStatusChecks[0][2]).toBe('KAITO');
    expect(providerStatusChecks[0][3]).toMatchObject({
      crds: [{ name: 'workspaces.kaito.sh', displayName: 'KAITO Workspace CRD' }],
      operatorPods: [{
        namespace: 'kaito-workspace',
        selectors: [
          'app.kubernetes.io/name=kaito',
          'app=kaito',
          'control-plane=controller-manager',
        ],
      }],
    });
    expect(kaito).toBeDefined();
    expect(kaito?.name).toBe('KAITO');
    expect(kaito?.installed).toBe(false);
    expect(kaito?.healthy).toBe(false);
    expect(kaito?.version).toBe('0.10.0');
    expect(kaito?.message).toBe('KAITO workspace CRD not found');
  });

  test('uses annotation-driven installation status for Dynamo runtime entries', async () => {
    const providerStatusChecks: any[] = [];
    const dynamoHealth = {
      crds: [
        {
          name: 'dynamographdeployments.nvidia.com',
          displayName: 'DynamoGraphDeployment CRD',
        },
      ],
      operatorPods: [
        {
          namespace: 'dynamo-system',
          selectors: [
            dynamoOperatorSelector,
            'app.kubernetes.io/name=dynamo-operator',
            'control-plane=controller-manager',
          ],
        },
        {
          selectors: ['app.kubernetes.io/name=dynamo-operator'],
        },
      ],
    };
    const dynamoConfig = {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'dynamo',
        annotations: {
          ...mockInferenceProviderConfig.metadata.annotations,
          'airunway.ai/display-name': 'Dynamo',
          'airunway.ai/description': 'NVIDIA Dynamo for high-performance GPU inference',
          'airunway.ai/default-namespace': 'dynamo-system',
          'airunway.ai/documentation-url': 'https://github.com/kaito-project/airunway/tree/main/docs/providers/dynamo.md',
          'airunway.ai/capabilities': JSON.stringify({
            engines: ['vllm', 'sglang', 'trtllm'],
            servingModes: ['aggregated', 'disaggregated'],
          }),
          'airunway.ai/health': JSON.stringify(dynamoHealth),
        },
      },
      status: {
        ready: false,
        version: '1.2.3',
      },
    };

    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDInstallation', async () => ({ installed: true })),
      mockServiceMethod(kubernetesService as any, 'checkProviderInstallationStatus', async (...args: any[]) => {
        providerStatusChecks.push(args);
        return {
          installed: false,
          crdFound: false,
          operatorRunning: false,
          message: 'DynamoGraphDeployment CRD not found',
        };
      }),
      mockServiceMethod(kubernetesService, 'checkKaitoInstallationStatus', async () => {
        throw new Error('getRuntimesStatus should not use KAITO-specific status checks');
      }),
      mockServiceMethod(kubernetesService, 'checkDynamoInstallationStatus', async () => {
        throw new Error('getRuntimesStatus should not use Dynamo-specific status checks');
      }),
    );
    mockProviderConfigs([dynamoConfig]);

    const runtimes = await kubernetesService.getRuntimesStatus();
    const dynamo = runtimes.find((runtime) => runtime.id === 'dynamo');

    expect(providerStatusChecks).toHaveLength(1);
    expect(providerStatusChecks[0][0]).toBe('dynamo');
    expect(providerStatusChecks[0][1]).toEqual(dynamoConfig.status);
    expect(providerStatusChecks[0][2]).toBe('Dynamo');
    expect(providerStatusChecks[0][3]).toEqual(dynamoHealth);
    expect(dynamo).toBeDefined();
    expect(dynamo?.name).toBe('Dynamo');
    expect(dynamo?.defaultNamespace).toBe('dynamo-system');
    expect(dynamo?.capabilities?.engines).toEqual(['vllm', 'sglang', 'trtllm']);
    expect(dynamo?.capabilities?.modes).toEqual(['aggregated', 'disaggregated']);
    expect(dynamo?.installed).toBe(false);
    expect(dynamo?.healthy).toBe(false);
    expect(dynamo?.version).toBe('1.2.3');
    expect(dynamo?.message).toBe('DynamoGraphDeployment CRD not found');
  });

  test('treats legacy LLM-D and vLLM provider configs without requiresCRD as CRD-less', async () => {
    const llmdConfig = {
      ...mockInferenceProviderConfig,
      metadata: { ...mockInferenceProviderConfig.metadata, name: 'llmd' },
      status: {
        ready: true,
        version: '0.1.0',
      },
    };
    const vllmConfig = {
      ...mockInferenceProviderConfig,
      metadata: { ...mockInferenceProviderConfig.metadata, name: 'vllm' },
      status: {
        ready: true,
        version: '0.8.0',
      },
    };

    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDInstallation', async () => ({ installed: true })),
    );
    mockProviderConfigs([llmdConfig, vllmConfig]);

    const runtimes = await kubernetesService.getRuntimesStatus();
    const llmd = runtimes.find((runtime) => runtime.id === 'llmd');
    const vllm = runtimes.find((runtime) => runtime.id === 'vllm');

    expect(llmd).toBeDefined();
    expect(llmd?.name).toBe('LLM-D');
    expect(llmd?.installed).toBe(true);
    expect(llmd?.healthy).toBe(true);
    expect(llmd?.requiresCRD).toBe(false);
    expect(llmd?.version).toBe('0.1.0');
    expect(llmd?.message).toBe('Runtime is ready to use.');

    expect(vllm).toBeDefined();
    expect(vllm?.name).toBe('vLLM');
    expect(vllm?.installed).toBe(true);
    expect(vllm?.healthy).toBe(true);
    expect(vllm?.requiresCRD).toBe(false);
    expect(vllm?.version).toBe('0.8.0');
    expect(vllm?.message).toBe('Runtime is ready to use.');
  });

  test('honors explicit requiresCRD metadata for custom-named CRD-less runtime entries', async () => {
    const { 'airunway.ai/health': _health, 'airunway.ai/capabilities': _capabilities, ...annotationsWithoutHealth } = mockInferenceProviderConfig.metadata.annotations;
    const customVllmConfig = {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'custom-vllm-registration',
        annotations: {
          ...annotationsWithoutHealth,
          'airunway.ai/provider-name': 'vLLM',
        },
      },
      spec: {
        ...mockInferenceProviderConfig.spec,
        capabilities: {
          engines: [
            { name: 'vllm', servingModes: ['aggregated'], gpuSupport: true, requiresCRD: true },
          ],
        },
      },
      status: {
        ready: true,
        version: '0.8.0',
      },
    };

    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDInstallation', async () => ({ installed: true })),
    );
    mockProviderConfigs([customVllmConfig]);

    const runtimes = await kubernetesService.getRuntimesStatus();
    const vllm = runtimes.find((runtime) => runtime.id === 'custom-vllm-registration');

    expect(vllm).toBeDefined();
    expect(vllm?.name).toBe('vLLM');
    expect(vllm?.installed).toBe(true);
    expect(vllm?.healthy).toBe(true);
    expect(vllm?.crdFound).toBe(true);
    expect(vllm?.operatorRunning).toBe(true);
    expect(vllm?.requiresCRD).toBe(true);
    expect(vllm?.version).toBe('0.8.0');
    expect(vllm?.message).toBe('vLLM is installed and running');
  });

  test('honors per-engine requiresCRD: false on the migrated schema for custom-named runtime entries', async () => {
    // Post-migration: legacy top-level capabilities.requiresCRD has been
    // stripped, and the verdict lives on each engine. The provider id/display
    // name are non-canonical, so the canonical-id fallback cannot mask a
    // buggy aggregation.
    const { 'airunway.ai/health': _health, 'airunway.ai/capabilities': _capabilities, ...annotationsWithoutHealth } = mockInferenceProviderConfig.metadata.annotations;
    const customConfig = {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'mycustom-runtime',
        annotations: {
          ...annotationsWithoutHealth,
          'airunway.ai/provider-name': 'My Custom Runtime',
        },
      },
      spec: {
        capabilities: {
          engines: [
            { name: 'vllm', servingModes: ['aggregated'], gpuSupport: true, requiresCRD: false },
            { name: 'sglang', servingModes: ['aggregated'], gpuSupport: true, requiresCRD: false },
          ],
        },
      },
      status: {
        ready: true,
        version: '0.1.0',
      },
    };

    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDInstallation', async () => ({ installed: true })),
    );
    mockProviderConfigs([customConfig]);

    const runtimes = await kubernetesService.getRuntimesStatus();
    const custom = runtimes.find((runtime) => runtime.id === 'mycustom-runtime');

    expect(custom).toBeDefined();
    expect(custom?.name).toBe('My Custom Runtime');
    expect(custom?.installed).toBe(true);
    expect(custom?.requiresCRD).toBe(false);
    expect(custom?.crdFound).toBe(true);
    expect(custom?.operatorRunning).toBe(true);
    expect(custom?.message).toBe('Runtime is ready to use.');
  });

  test('honors annotation-derived requiresCRD when spec capabilities are absent', async () => {
    const { 'airunway.ai/health': _health, 'airunway.ai/capabilities': _capabilities, ...annotationsWithoutHealth } = mockInferenceProviderConfig.metadata.annotations;
    const customConfig = {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'annotation-native-runtime',
        annotations: {
          ...annotationsWithoutHealth,
          'airunway.ai/provider-name': 'Annotation Native Runtime',
          'airunway.ai/capabilities': JSON.stringify({
            engines: [
              { name: 'vllm', servingModes: ['aggregated'], gpuSupport: true, requiresCRD: false },
            ],
          }),
        },
      },
      spec: {},
      status: {
        ready: false,
        version: '0.1.0',
      },
    };

    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDInstallation', async () => ({ installed: true })),
    );
    mockProviderConfigs([customConfig]);

    const runtimes = await kubernetesService.getRuntimesStatus();
    const custom = runtimes.find((runtime) => runtime.id === 'annotation-native-runtime');

    expect(custom).toBeDefined();
    expect(custom?.name).toBe('Annotation Native Runtime');
    expect(custom?.installed).toBe(false);
    expect(custom?.requiresCRD).toBe(false);
    expect(custom?.crdFound).toBe(true);
    expect(custom?.operatorRunning).toBe(false);
    expect(custom?.message).toBe('Provider is registered but not ready yet.');
  });

  test('reports ready providers that do not require runtime CRDs as installed without probing an operator', async () => {
    const llmdConfig = {
      ...mockInferenceProviderConfig,
      metadata: { ...mockInferenceProviderConfig.metadata, name: 'llmd' },
      spec: {
        ...mockInferenceProviderConfig.spec,
        capabilities: {
          ...mockInferenceProviderConfig.spec.capabilities,
          requiresCRD: false,
        },
      },
      status: {
        ready: true,
        version: '0.1.0',
      },
    };

    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDInstallation', async () => ({ installed: true })),
    );
    mockProviderConfigs([llmdConfig]);

    const runtimes = await kubernetesService.getRuntimesStatus();
    const llmd = runtimes.find((runtime) => runtime.id === 'llmd');

    expect(llmd).toBeDefined();
    expect(llmd?.installed).toBe(true);
    expect(llmd?.healthy).toBe(true);
    expect(llmd?.crdFound).toBe(true);
    expect(llmd?.operatorRunning).toBe(true);
    expect(llmd?.requiresCRD).toBe(false);
    expect(llmd?.version).toBe('0.1.0');
    expect(llmd?.message).toBe('Runtime is ready to use.');
  });

  test('honors provider readiness for runtimes that do not require runtime CRDs', async () => {
    const llmdConfig = {
      ...mockInferenceProviderConfig,
      metadata: { ...mockInferenceProviderConfig.metadata, name: 'llmd' },
      spec: {
        ...mockInferenceProviderConfig.spec,
        capabilities: {
          ...mockInferenceProviderConfig.spec.capabilities,
          requiresCRD: false,
        },
      },
      status: {
        ready: false,
        version: '0.1.0',
      },
    };

    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDInstallation', async () => ({ installed: true })),
    );
    mockProviderConfigs([llmdConfig]);

    const runtimes = await kubernetesService.getRuntimesStatus();
    const llmd = runtimes.find((runtime) => runtime.id === 'llmd');

    expect(llmd).toBeDefined();
    expect(llmd?.installed).toBe(false);
    expect(llmd?.healthy).toBe(false);
    expect(llmd?.crdFound).toBe(true);
    expect(llmd?.operatorRunning).toBe(false);
    expect(llmd?.requiresCRD).toBe(false);
    expect(llmd?.version).toBe('0.1.0');
    expect(llmd?.message).toBe('Provider is registered but not ready yet.');
  });

  test('runs annotation health probes even when provider status is ready', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async () => false),
    );

    const status = await kubernetesService.checkProviderInstallationStatus(
      'custom-provider',
      { ready: true },
      'Custom Provider',
      { crds: [{ name: 'customproviders.example.com', displayName: 'Custom Provider CRD' }] },
      true,
    );

    expect(status.installed).toBe(false);
    expect(status.crdFound).toBe(false);
    expect(status.operatorRunning).toBe(false);
    expect(status.requiresCRD).toBe(true);
    expect(status.message).toBe('Custom Provider CRD not found');
  });

  test('ignores malformed health CRD display names instead of throwing', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async () => false),
    );

    const status = await kubernetesService.checkProviderInstallationStatus(
      'bad-health-provider',
      { ready: false },
      'Bad Health Provider',
      { crds: [{ name: 'badhealth.example.com', displayName: 123 }] } as any,
      true,
    );

    expect(status.installed).toBe(false);
    expect(status.crdFound).toBe(false);
    expect(status.message).toBe('badhealth.example.com not found');
  });


  test('does not report CRD-only providers installed until provider status is ready', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async () => true),
    );

    const status = await kubernetesService.checkProviderInstallationStatus(
      'crd-only-provider',
      { ready: false },
      'CRD Only Provider',
      { crds: [{ name: 'crdonly.example.com', displayName: 'CRD Only CRD' }] },
      true,
    );

    expect(status.installed).toBe(false);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(false);
    expect(status.message).toBe('CRD Only CRD found but CRD Only Provider is not ready');
  });

  test('reports KAITO as not fully installed when the CRD exists but no ready operator pod is found', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async () => true),
    );
    mockOperatorPods('kaito-workspace', kaitoOperatorSelector, [
      {
        metadata: { name: 'workspace-operator-abc123' },
        status: {
          phase: 'Running',
          containerStatuses: [
            { ready: false, restartCount: 2 },
          ],
        },
      },
    ]);

    const status = await kubernetesService.checkKaitoInstallationStatus();

    expect(status.installed).toBe(false);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(false);
    expect(status.message).toBe('KAITO workspace CRD found but no ready KAITO operator pods were detected in kaito-workspace or matching known provider labels');
  });

  test('reports KAITO as installed when a ready operator pod is found', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async () => true),
    );
    mockOperatorPods('kaito-workspace', kaitoOperatorSelector, [
      {
        metadata: { name: 'workspace-operator-ready' },
        status: {
          phase: 'Running',
          containerStatuses: [
            { ready: true, restartCount: 0 },
          ],
        },
      },
    ]);

    const status = await kubernetesService.checkKaitoInstallationStatus();

    expect(status.installed).toBe(true);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(true);
    expect(status.message).toBe('KAITO workspace CRD found and KAITO operator pods are ready');
  });

  test('reports KAITO as installed when the AKS AI-toolchain-operator add-on pod is running in kube-system', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async (crdName: string) => crdName === 'workspaces.kaito.sh'),
    );
    // The AKS add-on runs the KAITO operator in kube-system, labeled
    // app=ai-toolchain-operator rather than the upstream Helm chart labels, so
    // it only surfaces through the cross-namespace fallback search.
    mockOperatorPods('kaito-workspace', kaitoOperatorSelector, [], [
      {
        metadata: { namespace: 'kube-system', name: 'kaito-workspace-557dbc5ffb-smczp', labels: { app: 'ai-toolchain-operator' } },
        status: {
          phase: 'Running',
          containerStatuses: [
            { ready: true, restartCount: 0 },
          ],
        },
      },
    ]);

    const status = await kubernetesService.checkKaitoInstallationStatus();

    expect(status.installed).toBe(true);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(true);
    expect(status.message).toBe('KAITO workspace CRD found and KAITO operator pods are ready in kube-system');
  });

  test('reports Dynamo as installed when a ready operator pod is found', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async (crdName: string) => crdName === 'dynamographdeployments.nvidia.com'),
    );
    mockOperatorPods('dynamo-system', dynamoOperatorSelector, [
      {
        metadata: { name: 'dynamo-operator-ready' },
        status: {
          phase: 'Running',
          containerStatuses: [
            { ready: true, restartCount: 0 },
          ],
        },
      },
    ]);

    const status = await kubernetesService.checkDynamoInstallationStatus();

    expect(status.installed).toBe(true);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(true);
    expect(status.message).toBe('Dynamo CRD found and Dynamo operator pods are ready');
  });

  test('reports Dynamo as not fully installed when the CRD exists but no ready operator pod is found', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async (crdName: string) => crdName === 'dynamographdeployments.nvidia.com'),
    );
    mockOperatorPods('dynamo-system', dynamoOperatorSelector, [
      {
        metadata: { name: 'dynamo-operator-abc123' },
        status: {
          phase: 'Running',
          containerStatuses: [
            { ready: false, restartCount: 1 },
          ],
        },
      },
    ]);

    const status = await kubernetesService.checkDynamoInstallationStatus();

    expect(status.installed).toBe(false);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(false);
    expect(status.message).toBe('Dynamo CRD found but no ready Dynamo operator pods were detected in dynamo-system or matching known provider labels');
  });

  test('does not treat unrelated controller-manager pods as Dynamo operator pods', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async (crdName: string) => crdName === 'dynamographdeployments.nvidia.com'),
    );
    mockOperatorPods('dynamo-system', dynamoOperatorSelector, [], [
      {
        metadata: {
          namespace: 'kube-system',
          name: 'unrelated-controller-manager',
          labels: { 'control-plane': 'controller-manager' },
        },
        status: {
          phase: 'Running',
          containerStatuses: [
            { ready: true, restartCount: 0 },
          ],
        },
      },
    ]);

    const status = await kubernetesService.checkDynamoInstallationStatus();

    expect(status.installed).toBe(false);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(false);
    expect(status.message).toBe('Dynamo CRD found but no ready Dynamo operator pods were detected in dynamo-system or matching known provider labels');
  });


  test('reports KubeRay as not fully installed when the CRD exists but no ready operator pod is found', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async (crdName: string) => crdName === 'rayservices.ray.io'),
    );
    mockOperatorPods('ray-system', kuberayOperatorSelector, [
      {
        metadata: { name: 'kuberay-operator-starting' },
        status: {
          phase: 'Running',
          containerStatuses: [
            { ready: false, restartCount: 1 },
          ],
        },
      },
    ]);

    const status = await kubernetesService.checkKubeRayInstallationStatus();

    expect(status.installed).toBe(false);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(false);
    expect(status.message).toBe('KubeRay CRD found but no ready KubeRay operator pods were detected in ray-system or matching known provider labels');
  });


  test('finds KubeRay operator pods installed in a custom namespace with standard labels', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async (crdName: string) => crdName === 'rayservices.ray.io'),
    );
    mockOperatorPods('ray-system', kuberayOperatorSelector, [], [
      {
        metadata: { namespace: 'ray-ops', name: 'kuberay-operator-ready', labels: { 'app.kubernetes.io/name': 'kuberay-operator' } },
        status: {
          phase: 'Running',
          containerStatuses: [
            { ready: true, restartCount: 0 },
          ],
        },
      },
    ]);

    const status = await kubernetesService.checkKubeRayInstallationStatus();

    expect(status.installed).toBe(true);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(true);
    expect(status.message).toBe('KubeRay CRD found and KubeRay operator pods are ready in ray-ops');
  });

  test('surfaces operator pod probe errors instead of reporting pods as simply not ready', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async () => true),
    );
    const service = asMockable();
    const originalNamespaced = service.coreV1Api.listNamespacedPod;
    const originalAllNamespaces = service.coreV1Api.listPodForAllNamespaces;
    const forbidden = { statusCode: 403, body: { message: 'pods is forbidden' } };
    service.coreV1Api.listNamespacedPod = async () => { throw forbidden; };
    service.coreV1Api.listPodForAllNamespaces = async () => { throw forbidden; };
    restores.push(() => {
      service.coreV1Api.listNamespacedPod = originalNamespaced;
      service.coreV1Api.listPodForAllNamespaces = originalAllNamespaces;
    });

    const status = await kubernetesService.checkKaitoInstallationStatus();

    expect(status.installed).toBe(false);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(false);
    expect(status.message).toBe('KAITO workspace CRD found but KAITO operator pods could not be checked: pods is forbidden');
  });

  test('reports KubeRay as installed when a ready operator pod is found', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDExists', async (crdName: string) => crdName === 'rayservices.ray.io'),
    );
    mockOperatorPods('ray-system', kuberayOperatorSelector, [
      {
        metadata: { name: 'kuberay-operator-ready' },
        status: {
          phase: 'Running',
          containerStatuses: [
            { ready: true, restartCount: 0 },
          ],
        },
      },
    ]);

    const status = await kubernetesService.checkKubeRayInstallationStatus();

    expect(status.installed).toBe(true);
    expect(status.crdFound).toBe(true);
    expect(status.operatorRunning).toBe(true);
    expect(status.message).toBe('KubeRay CRD found and KubeRay operator pods are ready');
  });
});
