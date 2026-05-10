import { afterEach, describe, expect, test } from 'bun:test';
import { kubernetesService } from './kubernetes';
import { mockServiceMethod } from '../test/helpers';
import { mockInferenceProviderConfig } from '../test/fixtures';

describe('KubernetesService - Runtime Status', () => {
  const restores: Array<() => void> = [];
  const kaitoOperatorSelector = 'app.kubernetes.io/name=workspace,app.kubernetes.io/instance=kaito-workspace';
  const dynamoOperatorSelector = 'control-plane=controller-manager,app.kubernetes.io/name=dynamo-operator,app.kubernetes.io/instance=dynamo-platform';
  const kuberayOperatorSelector = 'app.kubernetes.io/name=kuberay-operator,app.kubernetes.io/instance=kuberay-operator';

  afterEach(() => {
    restores.forEach((restore) => restore());
    restores.length = 0;
  });

  function mockProviderConfigs(items: any[]) {
    const service = kubernetesService as any;
    const original = service.customObjectsApi.listClusterCustomObject;
    service.customObjectsApi.listClusterCustomObject = async () => ({ body: { items } });
    restores.push(() => {
      service.customObjectsApi.listClusterCustomObject = original;
    });
  }

  function podMatchesSelector(pod: any, selector?: string): boolean {
    if (!selector) return true;
    const labels = pod.metadata?.labels || {};
    return selector.split(',').every((part) => {
      const [key, value] = part.split('=');
      return labels[key] === value;
    });
  }

  function mockOperatorPods(namespace: string, selector: string, items: any[], allNamespaceItems: any[] = []) {
    const service = kubernetesService as any;
    const originalNamespaced = service.coreV1Api.listNamespacedPod;
    const originalAllNamespaces = service.coreV1Api.listPodForAllNamespaces;
    service.coreV1Api.listNamespacedPod = async (...args: any[]) => {
      expect(args[0]).toBe(namespace);
      const requestedSelector = args[5];
      return { body: { items: requestedSelector === selector ? items : items.filter((pod) => podMatchesSelector(pod, requestedSelector)) } };
    };
    service.coreV1Api.listPodForAllNamespaces = async (...args: any[]) => {
      const requestedSelector = args[3];
      return { body: { items: allNamespaceItems.filter((pod) => podMatchesSelector(pod, requestedSelector)) } };
    };
    restores.push(() => {
      service.coreV1Api.listNamespacedPod = originalNamespaced;
      service.coreV1Api.listPodForAllNamespaces = originalAllNamespaces;
    });
  }

  test('uses live KAITO installation status for KAITO runtime entries', async () => {
    let kaitoStatusChecks = 0;

    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDInstallation', async () => ({ installed: true })),
      mockServiceMethod(kubernetesService, 'checkKaitoInstallationStatus', async () => {
        kaitoStatusChecks += 1;
        return {
          installed: false,
          crdFound: false,
          operatorRunning: false,
          message: 'KAITO workspace CRD not found',
        };
      }),
    );
    mockProviderConfigs([mockInferenceProviderConfig]);

    const runtimes = await kubernetesService.getRuntimesStatus();
    const kaito = runtimes.find((runtime) => runtime.id === 'kaito');

    expect(kaitoStatusChecks).toBe(1);
    expect(kaito).toBeDefined();
    expect(kaito?.installed).toBe(false);
    expect(kaito?.healthy).toBe(false);
    expect(kaito?.version).toBe('0.10.0');
    expect(kaito?.message).toBe('KAITO workspace CRD not found');
  });

  test('uses live Dynamo installation status for Dynamo runtime entries', async () => {
    let kaitoStatusChecks = 0;
    let dynamoStatusChecks = 0;
    const nonKaitoConfig = {
      ...mockInferenceProviderConfig,
      metadata: { ...mockInferenceProviderConfig.metadata, name: 'dynamo' },
      status: {
        ready: false,
        version: '1.2.3',
      },
    };

    restores.push(
      mockServiceMethod(kubernetesService, 'checkCRDInstallation', async () => ({ installed: true })),
      mockServiceMethod(kubernetesService, 'checkKaitoInstallationStatus', async () => {
        kaitoStatusChecks += 1;
        return {
          installed: true,
          crdFound: true,
          operatorRunning: true,
          message: 'should not be used',
        };
      }),
      mockServiceMethod(kubernetesService, 'checkDynamoInstallationStatus', async () => {
        dynamoStatusChecks += 1;
        return {
          installed: false,
          crdFound: false,
          operatorRunning: false,
          message: 'Dynamo CRD not found',
        };
      }),
    );
    mockProviderConfigs([nonKaitoConfig]);

    const runtimes = await kubernetesService.getRuntimesStatus();
    const dynamo = runtimes.find((runtime) => runtime.id === 'dynamo');

    expect(kaitoStatusChecks).toBe(0);
    expect(dynamoStatusChecks).toBe(1);
    expect(dynamo).toBeDefined();
    expect(dynamo?.installed).toBe(false);
    expect(dynamo?.healthy).toBe(false);
    expect(dynamo?.version).toBe('1.2.3');
    expect(dynamo?.message).toBe('Dynamo CRD not found');
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
    const customVllmConfig = {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'custom-vllm-registration',
        annotations: {
          ...mockInferenceProviderConfig.metadata.annotations,
          'airunway.ai/provider-name': 'vLLM',
        },
      },
      spec: {
        ...mockInferenceProviderConfig.spec,
        capabilities: {
          ...mockInferenceProviderConfig.spec.capabilities,
          requiresCRD: true,
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
    const service = kubernetesService as any;
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
