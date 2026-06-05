import { describe, test, expect } from 'bun:test';
import { extractCRDVersionFromAnnotations, kubernetesService, toPodStatus, type ClusterGpuCapacity, type NodeGpuInfo, type GPUAvailability, type GPUOperatorStatus } from './kubernetes';
import { toDeploymentStatus, type ClusterStatus, type PodStatus, type DeploymentStatus, type PodPhase, type ModelDeployment } from '@airunway/shared';


describe('KubernetesService - CRD Version Annotation Extraction', () => {
  test('extracts and trims the Inference Extension bundle version from a raw CRD object', () => {
    const crd = {
      metadata: {
        annotations: {
          'inference.networking.k8s.io/bundle-version': ' v1.5.0 ',
        },
      },
    };

    expect(extractCRDVersionFromAnnotations(crd, [
      'inference.networking.k8s.io/bundle-version',
      'app.kubernetes.io/version',
    ])).toBe('v1.5.0');
  });

  test('extracts the Gateway API version from a Kubernetes response wrapper', () => {
    const response = {
      body: {
        metadata: {
          annotations: {
            'gateway.networking.k8s.io/bundle-version': 'v1.2.1',
          },
        },
      },
    };

    expect(extractCRDVersionFromAnnotations(response, [
      'gateway.networking.k8s.io/bundle-version',
      'app.kubernetes.io/version',
    ])).toBe('v1.2.1');
  });

  test('returns undefined when version annotations are missing', () => {
    const crd = {
      metadata: {
        annotations: {
          'example.com/other': 'v0.1.0',
        },
      },
    };

    expect(extractCRDVersionFromAnnotations(crd, [
      'inference.networking.k8s.io/bundle-version',
      'app.kubernetes.io/version',
    ])).toBeUndefined();
  });


  test('checks Gateway CRD status with a single read per CRD', async () => {
    const service = kubernetesService as any;
    const originalApiExtensionsApi = service.apiExtensionsApi;
    const originalGetGatewayStatus = service.getGatewayStatus;
    const readCalls: string[] = [];

    service.apiExtensionsApi = {
      readCustomResourceDefinition: async (arg: any) => {
        const crdName = arg.name as string;
        readCalls.push(crdName);

        if (crdName === 'gateways.gateway.networking.k8s.io') {
          return {
            metadata: {
              annotations: {
                'gateway.networking.k8s.io/bundle-version': 'v1.2.1',
              },
            },
          };
        }

        if (crdName === 'inferencepools.inference.networking.k8s.io') {
          return {
            metadata: {
              annotations: {
                'inference.networking.k8s.io/bundle-version': 'v1.5.0',
              },
            },
          };
        }

        throw { statusCode: 404 };
      },
    };
    service.getGatewayStatus = async () => ({ available: true, endpoint: 'gateway.example.com' });

    try {
      const status = await kubernetesService.checkGatewayCRDStatus();

      expect(readCalls).toEqual([
        'gateways.gateway.networking.k8s.io',
        'inferencepools.inference.networking.k8s.io',
      ]);
      expect(status.gatewayApiInstalled).toBe(true);
      expect(status.inferenceExtInstalled).toBe(true);
      expect(status.gatewayApiVersion).toBe('v1.2.1');
      expect(status.inferenceExtVersion).toBe('v1.5.0');
      expect(status.gatewayAvailable).toBe(true);
      expect(status.gatewayEndpoint).toBe('gateway.example.com');
    } finally {
      service.apiExtensionsApi = originalApiExtensionsApi;
      service.getGatewayStatus = originalGetGatewayStatus;
    }
  });
});


describe('KubernetesService - deployment pod lookup', () => {
  test('aggregates and de-duplicates pods across exact supported selectors', async () => {
    const service = kubernetesService as any;
    const originalCoreV1Api = service.coreV1Api;
    const selectors: string[] = [];

    const pod = (name: string, labels: Record<string, string> = {}) => ({
      metadata: { name, labels },
      status: {
        phase: 'Running',
        containerStatuses: [
          { ready: true, restartCount: 0, state: { running: {} } },
        ],
      },
    });

    service.coreV1Api = {
      listNamespacedPod: async (arg: any) => {
        const labelSelector = arg.labelSelector as string;
        selectors.push(labelSelector);

        if (labelSelector === 'app.kubernetes.io/instance=demo') {
          return { items: [pod('demo-router'), pod('demo-shared')] };
        }

        if (labelSelector === 'airunway.ai/deployment=demo') {
          return { items: [pod('demo-shared'), pod('demo-worker')] };
        }

        if (labelSelector === 'airunway.ai/model-deployment=demo') {
          return { items: [pod('demo-ray-template')] };
        }

        if (labelSelector === 'nvidia.com/dynamo-graph-deployment-name=demo') {
          return { items: [pod('demo-epp'), pod('demo-vllmworker')] };
        }

        if (labelSelector === 'app=demo') {
          return { items: [pod('unrelated-app-pod')] };
        }

        if (labelSelector === 'ray.io/cluster') {
          return {
            items: [
              pod('demo-ray-head', { 'ray.io/cluster': 'demo-raycluster' }),
              pod('demo2-ray-head', { 'ray.io/cluster': 'demo2-raycluster' }),
              pod('demo-extra-ray-head', { 'ray.io/cluster': 'demo-extra-raycluster' }),
              pod('other-ray-head', { 'ray.io/cluster': 'other-raycluster' }),
            ],
          };
        }

        return { items: [] };
      },
    };

    try {
      const pods = await kubernetesService.getDeploymentPods('demo', 'default');

      expect(pods.map((item) => item.name)).toEqual([
        'demo-epp',
        'demo-ray-head',
        'demo-ray-template',
        'demo-router',
        'demo-shared',
        'demo-vllmworker',
        'demo-worker',
      ]);
      expect(new Set(pods.map((item) => item.name)).size).toBe(pods.length);
      expect(selectors).toEqual([
        'app.kubernetes.io/instance=demo',
        'airunway.ai/deployment=demo',
        'airunway.ai/model-deployment=demo',
        'nvidia.com/dynamo-graph-deployment-name=demo',
        'kaito.sh/workspace=demo',
        'ray.io/cluster',
      ]);
    } finally {
      service.coreV1Api = originalCoreV1Api;
    }
  });

  test('uses broad app selector only as a last-resort fallback', async () => {
    const service = kubernetesService as any;
    const originalCoreV1Api = service.coreV1Api;
    const selectors: string[] = [];

    const pod = (name: string) => ({
      metadata: { name },
      status: {
        phase: 'Running',
        containerStatuses: [
          { ready: true, restartCount: 0, state: { running: {} } },
        ],
      },
    });

    service.coreV1Api = {
      listNamespacedPod: async (arg: any) => {
        const labelSelector = arg.labelSelector as string;
        selectors.push(labelSelector);

        if (labelSelector === 'app=legacy-demo') {
          return { items: [pod('legacy-demo-pod')] };
        }

        return { items: [] };
      },
    };

    try {
      const pods = await kubernetesService.getDeploymentPods('legacy-demo', 'default');

      expect(pods.map((item) => item.name)).toEqual(['legacy-demo-pod']);
      expect(selectors).toEqual([
        'app.kubernetes.io/instance=legacy-demo',
        'airunway.ai/deployment=legacy-demo',
        'airunway.ai/model-deployment=legacy-demo',
        'nvidia.com/dynamo-graph-deployment-name=legacy-demo',
        'kaito.sh/workspace=legacy-demo',
        'ray.io/cluster',
        'app=legacy-demo',
      ]);
    } finally {
      service.coreV1Api = originalCoreV1Api;
    }
  });
});


describe('KubernetesService - pod logs', () => {
  test('defaults multi-container pod logs to the primary main container using pod list permission', async () => {
    const service = kubernetesService as any;
    const originalCoreV1Api = service.coreV1Api;
    let requestedContainer: string | undefined;

    service.coreV1Api = {
      listNamespacedPod: async (arg: any) => {
        expect(arg.namespace).toBe('default');
        expect(arg.fieldSelector).toBe('metadata.name=demo-worker');
        return {
          items: [
            {
              spec: {
                containers: [
                  { name: 'frontend' },
                  { name: 'main' },
                ],
              },
              status: {
                containerStatuses: [
                  { name: 'frontend', ready: true, restartCount: 0, state: { running: {} } },
                  { name: 'main', ready: true, restartCount: 0, state: { running: {} } },
                ],
              },
            },
          ],
        };
      },
      readNamespacedPodLog: async (arg: any) => {
        requestedContainer = arg.container as string | undefined;
        return 'worker logs';
      },
    };

    try {
      const logs = await kubernetesService.getPodLogs('demo-worker', 'default', { tailLines: 10 });

      expect(logs).toBe('worker logs');
      expect(requestedContainer).toBe('main');
    } finally {
      service.coreV1Api = originalCoreV1Api;
    }
  });

  test('prefers generated model containers before ready sidecars', async () => {
    const service = kubernetesService as any;
    const originalCoreV1Api = service.coreV1Api;
    let requestedContainer: string | undefined;

    service.coreV1Api = {
      listNamespacedPod: async (arg: any) => {
        expect(arg.fieldSelector).toBe('metadata.name=demo-model');
        return {
          items: [
            {
              spec: {
                containers: [
                  { name: 'istio-proxy' },
                  { name: 'vllm' },
                ],
              },
              status: {
                containerStatuses: [
                  { name: 'istio-proxy', ready: true, restartCount: 0, state: { running: {} } },
                  { name: 'vllm', ready: false, restartCount: 3, state: { waiting: { reason: 'CrashLoopBackOff' } } },
                ],
              },
            },
          ],
        };
      },
      readNamespacedPodLog: async (arg: any) => {
        requestedContainer = arg.container as string | undefined;
        return 'model logs';
      },
    };

    try {
      const logs = await kubernetesService.getPodLogs('demo-model', 'default', { tailLines: 10 });

      expect(logs).toBe('model logs');
      expect(requestedContainer).toBe('vllm');
    } finally {
      service.coreV1Api = originalCoreV1Api;
    }
  });
});

describe('KubernetesService - service proxy', () => {
  test('uses caller token kubeconfig for proxied GET and streaming POST requests', async () => {
    const service = kubernetesService as any;
    const originalKubeConfig = service.kc;
    const originalCreateUserKubeConfig = service.createUserKubeConfig;
    const originalFetch = globalThis.fetch;
    const createUserKubeConfigCalls: string[] = [];
    const fetchCalls: Array<{ url: string; init: RequestInit & { userToken?: string } }> = [];

    const fakeKubeConfig = (authHeader: string) => ({
      getCurrentCluster: () => ({ server: 'https://cluster.example', skipTLSVerify: false }),
      applyToFetchOptions: async (requestOptions: any) => {
        return {
          ...requestOptions,
          headers: {
            ...(requestOptions?.headers ?? {}),
            Authorization: authHeader,
          },
        };
      },
      applyToHTTPSOptions: async () => undefined,
    });

    service.kc = fakeKubeConfig('Bearer shared-service-account-token');
    service.createUserKubeConfig = (userToken: string) => {
      createUserKubeConfigCalls.push(userToken);
      return fakeKubeConfig(`Bearer ${userToken}`);
    };
    globalThis.fetch = (async (input: any, init?: any) => {
      fetchCalls.push({ url: String(input), init });
      return new Response(fetchCalls.length === 1 ? 'models' : 'stream', {
        status: 200,
        statusText: 'OK',
      });
    }) as typeof fetch;

    try {
      const getBody = await kubernetesService.proxyServiceGet(
        'model-svc',
        'tenant-ns',
        8000,
        'v1/models',
        { accept: 'application/json', userToken: 'user-token' },
      );
      const postResponse = await kubernetesService.proxyServicePostStream(
        'model-svc',
        'tenant-ns',
        8000,
        'v1/chat/completions',
        { messages: [{ role: 'user', content: 'Hello' }] },
        { 'X-Trace-Id': 'trace-1' },
        { userToken: 'user-token' },
      );

      expect(getBody).toBe('models');
      expect(await postResponse.text()).toBe('stream');
      expect(createUserKubeConfigCalls).toEqual(['user-token', 'user-token']);
      expect(fetchCalls.map((call) => call.url)).toEqual([
        'https://cluster.example/api/v1/namespaces/tenant-ns/services/model-svc:8000/proxy/v1/models',
        'https://cluster.example/api/v1/namespaces/tenant-ns/services/model-svc:8000/proxy/v1/chat/completions',
      ]);

      const getHeaders = new Headers(fetchCalls[0].init.headers);
      expect(fetchCalls[0].init.method).toBe('GET');
      expect(getHeaders.get('authorization')).toBe('Bearer user-token');
      expect(getHeaders.get('authorization')).not.toBe('Bearer shared-service-account-token');
      expect(getHeaders.get('accept')).toBe('application/json');
      expect(fetchCalls[0].init.userToken).toBeUndefined();

      const postHeaders = new Headers(fetchCalls[1].init.headers);
      expect(fetchCalls[1].init.method).toBe('POST');
      expect(postHeaders.get('authorization')).toBe('Bearer user-token');
      expect(postHeaders.get('accept')).toBe('text/event-stream');
      expect(postHeaders.get('content-type')).toBe('application/json');
      expect(postHeaders.get('x-trace-id')).toBe('trace-1');
      expect(fetchCalls[1].init.body).toBe(JSON.stringify({
        messages: [{ role: 'user', content: 'Hello' }],
      }));
      expect(fetchCalls[1].init.userToken).toBeUndefined();
    } finally {
      service.kc = originalKubeConfig;
      service.createUserKubeConfig = originalCreateUserKubeConfig;
      globalThis.fetch = originalFetch;
    }
  });
});

describe('KubernetesService - Type Definitions', () => {
  describe('ClusterGpuCapacity', () => {
    test('creates valid capacity with GPU nodes', () => {
      const nodes: NodeGpuInfo[] = [
        { nodeName: 'node-1', totalGpus: 8, allocatedGpus: 4, availableGpus: 4 },
        { nodeName: 'node-2', totalGpus: 8, allocatedGpus: 2, availableGpus: 6 },
      ];

      const capacity: ClusterGpuCapacity = {
        totalGpus: 16,
        allocatedGpus: 6,
        availableGpus: 10,
        maxContiguousAvailable: 6,
        maxNodeGpuCapacity: 8,
        gpuNodeCount: 2,
        totalMemoryGb: 80,
        nodes,
      };

      expect(capacity.totalGpus).toBe(16);
      expect(capacity.availableGpus).toBe(10);
      expect(capacity.maxContiguousAvailable).toBe(6);
      expect(capacity.nodes).toHaveLength(2);
    });

    test('handles cluster with no GPUs', () => {
      const capacity: ClusterGpuCapacity = {
        totalGpus: 0,
        allocatedGpus: 0,
        availableGpus: 0,
        maxContiguousAvailable: 0,
        maxNodeGpuCapacity: 0,
        gpuNodeCount: 0,
        nodes: [],
      };

      expect(capacity.totalGpus).toBe(0);
      expect(capacity.gpuNodeCount).toBe(0);
      expect(capacity.nodes).toHaveLength(0);
    });

    test('totalMemoryGb is optional', () => {
      const capacity: ClusterGpuCapacity = {
        totalGpus: 4,
        allocatedGpus: 0,
        availableGpus: 4,
        maxContiguousAvailable: 4,
        maxNodeGpuCapacity: 4,
        gpuNodeCount: 1,
        nodes: [{ nodeName: 'node-1', totalGpus: 4, allocatedGpus: 0, availableGpus: 4 }],
      };

      expect(capacity.totalMemoryGb).toBeUndefined();
    });
  });

  describe('GPUAvailability', () => {
    test('creates available GPU status', () => {
      const availability: GPUAvailability = {
        available: true,
        totalGPUs: 8,
        gpuNodes: ['node-1', 'node-2'],
      };

      expect(availability.available).toBe(true);
      expect(availability.totalGPUs).toBe(8);
      expect(availability.gpuNodes).toHaveLength(2);
    });

    test('creates unavailable GPU status', () => {
      const availability: GPUAvailability = {
        available: false,
        totalGPUs: 0,
        gpuNodes: [],
      };

      expect(availability.available).toBe(false);
      expect(availability.totalGPUs).toBe(0);
    });
  });

  describe('GPUOperatorStatus', () => {
    test('creates fully installed status', () => {
      const status: GPUOperatorStatus = {
        installed: true,
        crdFound: true,
        operatorRunning: true,
        gpusAvailable: true,
        totalGPUs: 4,
        gpuNodes: ['gpu-node-1'],
        message: 'GPUs enabled: 4 GPU(s) on 1 node(s)',
      };

      expect(status.installed).toBe(true);
      expect(status.operatorRunning).toBe(true);
      expect(status.gpusAvailable).toBe(true);
    });

    test('creates not installed status', () => {
      const status: GPUOperatorStatus = {
        installed: false,
        crdFound: false,
        operatorRunning: false,
        gpusAvailable: false,
        totalGPUs: 0,
        gpuNodes: [],
        message: 'GPU Operator not installed',
      };

      expect(status.installed).toBe(false);
      expect(status.message).toContain('not installed');
    });

    test('creates partial status (CRD found but not running)', () => {
      const status: GPUOperatorStatus = {
        installed: false,
        crdFound: true,
        operatorRunning: false,
        gpusAvailable: false,
        totalGPUs: 0,
        gpuNodes: [],
        message: 'GPU Operator CRD found but operator is not running',
      };

      expect(status.installed).toBe(false);
      expect(status.crdFound).toBe(true);
      expect(status.operatorRunning).toBe(false);
    });
  });

  describe('ClusterStatus', () => {
    test('creates connected status', () => {
      const status: ClusterStatus = {
        connected: true,
        namespace: 'default',
        clusterName: 'my-cluster',
      };

      expect(status.connected).toBe(true);
      expect(status.error).toBeUndefined();
    });

    test('creates disconnected status with error', () => {
      const status: ClusterStatus = {
        connected: false,
        namespace: 'default',
        error: 'Unable to connect to cluster',
      };

      expect(status.connected).toBe(false);
      expect(status.error).toBeDefined();
    });
  });

  describe('PodStatus', () => {
    test('creates running pod status', () => {
      const pod: PodStatus = {
        name: 'my-pod-abc123',
        phase: 'Running',
        ready: true,
        restarts: 0,
        node: 'worker-node-1',
      };

      expect(pod.phase).toBe('Running');
      expect(pod.ready).toBe(true);
    });

    test('creates pending pod status', () => {
      const pod: PodStatus = {
        name: 'my-pod-pending',
        phase: 'Pending',
        ready: false,
        restarts: 0,
      };

      expect(pod.phase).toBe('Pending');
      expect(pod.ready).toBe(false);
      expect(pod.node).toBeUndefined();
    });

    test('creates failed pod with restarts', () => {
      const pod: PodStatus = {
        name: 'crashloop-pod',
        phase: 'Running',
        ready: false,
        restarts: 5,
        node: 'worker-node-2',
      };

      expect(pod.restarts).toBe(5);
      expect(pod.ready).toBe(false);
    });

    test('maps waiting reason and keeps empty container status lists as not ready', () => {
      const pod = toPodStatus({
        metadata: { name: 'nemotron-pod' },
        spec: { nodeName: 'gpu-node-1' },
        status: {
          phase: 'Pending',
          containerStatuses: [],
          initContainerStatuses: [
            {
              name: 'init',
              ready: false,
              restartCount: 0,
              image: 'busybox',
              imageID: 'busybox:latest',
              state: {
                waiting: {
                  reason: 'ContainerCreating',
                  message: 'Creating container',
                },
              },
            },
          ],
        },
      } as any);

      expect(pod.phase).toBe('Pending');
      expect(pod.ready).toBe(false);
      expect(pod.node).toBe('gpu-node-1');
      expect(pod.reason).toBe('ContainerCreating');
    });
  });
});

describe('Deployment status mapping', () => {
  function createModelDeployment(phase: 'Pending' | 'Deploying' | 'Running' | 'Failed' | 'Terminating'): ModelDeployment {
    return {
      apiVersion: 'airunway.ai/v1alpha1',
      kind: 'ModelDeployment',
      metadata: {
        name: 'nemotron',
        namespace: 'kaito-workspace',
        creationTimestamp: new Date().toISOString(),
      },
      spec: {
        model: { id: 'nvidia/Nemotron-3-Nano-4B-gguf' },
        engine: { type: 'llamacpp' },
        provider: { name: 'kaito' },
        serving: { mode: 'aggregated' },
      },
      status: {
        phase,
        replicas: {
          desired: 1,
          ready: 0,
          available: 0,
        },
      },
    };
  }

  test('treats ContainerCreating pods as deploying even when CRD phase says failed', () => {
    const deployment = toDeploymentStatus(createModelDeployment('Failed'), [
      {
        name: 'nvidia-nemotron-3-nano-4b-gguf-zftr-0',
        phase: 'Pending',
        ready: false,
        restarts: 0,
        node: 'gpu-node-1',
        reason: 'ContainerCreating',
      },
    ]);

    expect(deployment.phase).toBe('Deploying');
  });

  test('keeps fatal pod startup errors as failed', () => {
    const deployment = toDeploymentStatus(createModelDeployment('Failed'), [
      {
        name: 'nvidia-nemotron-3-nano-4b-gguf-zftr-0',
        phase: 'Pending',
        ready: false,
        restarts: 0,
        node: 'gpu-node-1',
        reason: 'ImagePullBackOff',
      },
    ]);

    expect(deployment.phase).toBe('Failed');
  });

  test('keeps unscheduled pending pods as pending', () => {
    const deployment = toDeploymentStatus(createModelDeployment('Failed'), [
      {
        name: 'nvidia-nemotron-3-nano-4b-gguf-zftr-0',
        phase: 'Pending',
        ready: false,
        restarts: 0,
      },
    ]);

    expect(deployment.phase).toBe('Pending');
  });
});

describe('KubernetesService - GPU Memory Detection Logic', () => {
  // Test the GPU memory detection from product names
  function detectGpuMemoryFromProduct(gpuProduct: string): number | undefined {
    const product = gpuProduct.toLowerCase();

    // NVIDIA Data Center GPUs
    if (product.includes('a100') && product.includes('80')) return 80;
    if (product.includes('a100') && product.includes('40')) return 40;
    if (product.includes('a100')) return 40;
    if (product.includes('h100') && product.includes('80')) return 80;
    if (product.includes('h100')) return 80;
    if (product.includes('h200')) return 141;
    if (product.includes('a10g')) return 24;
    if (product.includes('a10')) return 24;
    if (product.includes('l40s')) return 48;
    if (product.includes('l40')) return 48;
    if (product.includes('l4')) return 24;
    if (product.includes('t4')) return 16;
    if (product.includes('v100') && product.includes('32')) return 32;
    if (product.includes('v100')) return 16;

    // NVIDIA Consumer GPUs
    if (product.includes('4090')) return 24;
    if (product.includes('4080')) return 16;
    if (product.includes('3090')) return 24;
    if (product.includes('3080') && product.includes('12')) return 12;
    if (product.includes('3080')) return 10;

    return undefined;
  }

  test('detects A100 80GB', () => {
    expect(detectGpuMemoryFromProduct('NVIDIA-A100-SXM4-80GB')).toBe(80);
    expect(detectGpuMemoryFromProduct('Tesla-A100-80GB-PCIe')).toBe(80);
  });

  test('detects A100 40GB', () => {
    expect(detectGpuMemoryFromProduct('NVIDIA-A100-40GB')).toBe(40);
    expect(detectGpuMemoryFromProduct('Tesla-A100-PCIE-40GB')).toBe(40);
  });

  test('detects A100 default as 40GB', () => {
    expect(detectGpuMemoryFromProduct('NVIDIA-A100-SXM')).toBe(40);
    expect(detectGpuMemoryFromProduct('a100')).toBe(40);
  });

  test('detects H100', () => {
    expect(detectGpuMemoryFromProduct('NVIDIA-H100-80GB')).toBe(80);
    expect(detectGpuMemoryFromProduct('H100-SXM')).toBe(80);
  });

  test('detects H200', () => {
    expect(detectGpuMemoryFromProduct('NVIDIA-H200')).toBe(141);
  });

  test('detects A10G', () => {
    expect(detectGpuMemoryFromProduct('NVIDIA-A10G')).toBe(24);
  });

  test('detects L40S', () => {
    expect(detectGpuMemoryFromProduct('NVIDIA-L40S')).toBe(48);
  });

  test('detects L4', () => {
    expect(detectGpuMemoryFromProduct('NVIDIA-L4')).toBe(24);
  });

  test('detects T4', () => {
    expect(detectGpuMemoryFromProduct('Tesla-T4')).toBe(16);
    expect(detectGpuMemoryFromProduct('NVIDIA-T4')).toBe(16);
  });

  test('detects V100 32GB', () => {
    expect(detectGpuMemoryFromProduct('Tesla-V100-32GB')).toBe(32);
  });

  test('detects V100 default as 16GB', () => {
    expect(detectGpuMemoryFromProduct('Tesla-V100-SXM2')).toBe(16);
    expect(detectGpuMemoryFromProduct('V100')).toBe(16);
  });

  test('detects consumer GPUs', () => {
    expect(detectGpuMemoryFromProduct('GeForce-RTX-4090')).toBe(24);
    expect(detectGpuMemoryFromProduct('RTX-4080')).toBe(16);
    expect(detectGpuMemoryFromProduct('GeForce-RTX-3090')).toBe(24);
    expect(detectGpuMemoryFromProduct('RTX-3080-12GB')).toBe(12);
    expect(detectGpuMemoryFromProduct('RTX-3080')).toBe(10);
  });

  test('returns undefined for unknown GPU', () => {
    expect(detectGpuMemoryFromProduct('Unknown-GPU')).toBeUndefined();
    expect(detectGpuMemoryFromProduct('AMD-MI250X')).toBeUndefined();
    expect(detectGpuMemoryFromProduct('')).toBeUndefined();
  });
});

describe('KubernetesService - Label Selector Logic', () => {
  // Test the label selector patterns used for finding pods
  const labelSelectors = [
    'app.kubernetes.io/instance={name}',
    'kaito.sh/workspace={name}',
    'app={name}',
  ];

  test('generates correct standard K8s label selector', () => {
    const deploymentName = 'my-llm';
    const selector = labelSelectors[0].replace('{name}', deploymentName);
    expect(selector).toBe('app.kubernetes.io/instance=my-llm');
  });

  test('generates correct KAITO label selector', () => {
    const deploymentName = 'kaito-model';
    const selector = labelSelectors[1].replace('{name}', deploymentName);
    expect(selector).toBe('kaito.sh/workspace=kaito-model');
  });

  test('generates correct fallback label selector', () => {
    const deploymentName = 'legacy-app';
    const selector = labelSelectors[2].replace('{name}', deploymentName);
    expect(selector).toBe('app=legacy-app');
  });
});

describe('KubernetesService - Protected Namespaces', () => {
  const protectedNamespaces = ['default', 'kube-system', 'kube-public', 'kube-node-lease'];

  test('identifies protected namespaces', () => {
    expect(protectedNamespaces.includes('default')).toBe(true);
    expect(protectedNamespaces.includes('kube-system')).toBe(true);
    expect(protectedNamespaces.includes('kube-public')).toBe(true);
    expect(protectedNamespaces.includes('kube-node-lease')).toBe(true);
  });

  test('allows deletion of non-protected namespaces', () => {
    expect(protectedNamespaces.includes('my-namespace')).toBe(false);
    expect(protectedNamespaces.includes('airunway-system')).toBe(false);
    expect(protectedNamespaces.includes('dynamo')).toBe(false);
  });
});

describe('KubernetesService - ANSI Color Code Stripping', () => {
  // Test the ANSI color code regex used in getPodLogs
  const ansiRegex = /\x1b\[[0-9;]*m/g;

  function stripAnsiCodes(text: string): string {
    return text.replace(ansiRegex, '');
  }

  test('strips ANSI color codes from logs', () => {
    const coloredLog = '\x1b[32mINFO\x1b[0m: Application started';
    expect(stripAnsiCodes(coloredLog)).toBe('INFO: Application started');
  });

  test('strips multiple ANSI codes', () => {
    const coloredLog = '\x1b[31mERROR\x1b[0m: \x1b[33mWarning\x1b[0m detected';
    expect(stripAnsiCodes(coloredLog)).toBe('ERROR: Warning detected');
  });

  test('handles text without ANSI codes', () => {
    const plainLog = 'Normal log message';
    expect(stripAnsiCodes(plainLog)).toBe('Normal log message');
  });

  test('handles empty string', () => {
    expect(stripAnsiCodes('')).toBe('');
  });

  test('strips bold and other formatting codes', () => {
    const formattedLog = '\x1b[1mBold\x1b[0m and \x1b[4munderline\x1b[0m';
    expect(stripAnsiCodes(formattedLog)).toBe('Bold and underline');
  });
});

describe('KubernetesService - Node Pool Label Detection', () => {
  // Test the logic for detecting node pool names from labels
  function getNodePoolName(labels: Record<string, string>): string {
    return (
      labels['agentpool'] ||
      labels['kubernetes.azure.com/agentpool'] ||
      labels['cloud.google.com/gke-nodepool'] ||
      labels['eks.amazonaws.com/nodegroup'] ||
      'default'
    );
  }

  test('detects AKS agentpool label', () => {
    const labels = { agentpool: 'gpupool' };
    expect(getNodePoolName(labels)).toBe('gpupool');
  });

  test('detects AKS kubernetes.azure.com/agentpool label', () => {
    const labels = { 'kubernetes.azure.com/agentpool': 'gpu-nodepool' };
    expect(getNodePoolName(labels)).toBe('gpu-nodepool');
  });

  test('detects GKE nodepool label', () => {
    const labels = { 'cloud.google.com/gke-nodepool': 'gpu-pool' };
    expect(getNodePoolName(labels)).toBe('gpu-pool');
  });

  test('detects EKS nodegroup label', () => {
    const labels = { 'eks.amazonaws.com/nodegroup': 'gpu-nodes' };
    expect(getNodePoolName(labels)).toBe('gpu-nodes');
  });

  test('prefers agentpool over other labels', () => {
    const labels = {
      agentpool: 'aks-pool',
      'cloud.google.com/gke-nodepool': 'gke-pool',
    };
    expect(getNodePoolName(labels)).toBe('aks-pool');
  });

  test('returns default for empty labels', () => {
    expect(getNodePoolName({})).toBe('default');
  });

  test('returns default for unrecognized labels', () => {
    const labels = { 'custom-label': 'value' };
    expect(getNodePoolName(labels)).toBe('default');
  });
});
