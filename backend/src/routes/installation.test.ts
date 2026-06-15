import { describe, test, expect, afterEach } from 'bun:test';
import { PINNED_GAIE_VERSION } from '@airunway/shared';
import app from '../hono-app';
import { kubernetesService } from '../services/kubernetes';
import { helmService } from '../services/helm';
import { huggingFaceService } from '../services/huggingface';
import type { HelmChart } from '../services/helm';
import { mockServiceMethod } from '../test/helpers';
import {
  mockInferenceProviderConfig,
} from '../test/fixtures';

describe('Installation Provider Routes', () => {
  function createDynamoInstallation(values: Record<string, unknown>) {
    return {
      description: 'NVIDIA Dynamo for high-performance GPU inference',
      defaultNamespace: 'dynamo-system',
      helmRepos: [],
      helmCharts: [
        {
          name: 'dynamo-platform',
          chart: 'https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-1.0.1.tgz',
          namespace: 'dynamo-system',
          createNamespace: true,
          values,
        },
      ],
      steps: [
        {
          title: 'Install Dynamo Platform',
          command: 'helm upgrade --install dynamo-platform https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-1.0.1.tgz --namespace dynamo-system --create-namespace --set-json global.grove.install=true',
          description: 'Install the Dynamo platform operator v1.0.1 with bundled Grove enabled by default. This chart includes the required CRDs.',
        },
      ],
    };
  }

  function createDynamoProviderConfig() {
    return {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'dynamo',
        annotations: {
          ...mockInferenceProviderConfig.metadata.annotations,
          'airunway.ai/installation': JSON.stringify(createDynamoInstallation({
            'global.grove.install': true,
          })),
          'airunway.ai/documentation': 'https://github.com/ai-dynamo/dynamo',
        },
      },
      spec: {
        ...mockInferenceProviderConfig.spec,
      },
    };
  }

  function createDynamoProviderConfigWithNestedValues() {
    return {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'dynamo',
        annotations: {
          ...mockInferenceProviderConfig.metadata.annotations,
          'airunway.ai/installation': JSON.stringify(createDynamoInstallation({
            'dynamo-operator': {
              controllerManager: {
                kubeRbacProxy: {
                  image: {
                    repository: 'quay.io/brancz/kube-rbac-proxy',
                    tag: 'v0.15.0',
                  },
                },
              },
            },
          })),
          'airunway.ai/documentation': 'https://github.com/ai-dynamo/dynamo',
        },
      },
      spec: {
        ...mockInferenceProviderConfig.spec,
      },
    };
  }

  function createKubeRayProviderConfig() {
    return {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'kuberay',
        annotations: {
          ...mockInferenceProviderConfig.metadata.annotations,
          'airunway.ai/installation': JSON.stringify({
            description: 'Ray Serve via KubeRay',
            defaultNamespace: 'ray-system',
            helmRepos: [],
            helmCharts: [
              {
                name: 'kuberay-operator',
                chart: 'kuberay/kuberay-operator',
                version: '1.3.0',
                namespace: 'ray-system',
                createNamespace: true,
              },
            ],
            steps: [],
          }),
          'airunway.ai/documentation': 'https://github.com/ray-project/kuberay',
        },
      },
      spec: {
        ...mockInferenceProviderConfig.spec,
      },
    };
  }

  const configWithoutInstallation = {
    ...mockInferenceProviderConfig,
    metadata: {
      ...mockInferenceProviderConfig.metadata,
      annotations: {
        'airunway.ai/documentation': mockInferenceProviderConfig.metadata.annotations['airunway.ai/documentation'],
      },
    },
  };

  function createNoCrdProviderConfigWithHelmMetadata() {
    const baseInstallation = JSON.parse(mockInferenceProviderConfig.metadata.annotations['airunway.ai/installation']);
    return {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'llmd',
        annotations: {
          ...mockInferenceProviderConfig.metadata.annotations,
          'airunway.io/provider-name': 'LLM-D',
          'airunway.ai/installation': JSON.stringify(baseInstallation),
        },
      },
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
  }

  function createLegacyNoCrdProviderConfigWithHelmMetadata() {
    const config = createNoCrdProviderConfigWithHelmMetadata();
    const capabilities = { ...config.spec.capabilities };
    delete (capabilities as Record<string, unknown>).requiresCRD;

    return {
      ...config,
      spec: {
        ...config.spec,
        capabilities,
      },
    };
  }

  function createCustomNamedNoCrdProviderConfigWithExplicitRequiresCrd() {
    const config = createNoCrdProviderConfigWithHelmMetadata();

    return {
      ...config,
      metadata: {
        ...config.metadata,
        name: 'custom-llmd-registration',
        annotations: {
          ...config.metadata.annotations,
          'airunway.io/provider-name': 'LLM-D',
        },
      },
      spec: {
        ...config.spec,
        capabilities: {
          engines: [
            { name: 'vllm', servingModes: ['aggregated'], gpuSupport: true, requiresCRD: true },
          ],
        },
      },
    };
  }

  function createCustomNoCrdProviderConfigWithPerEngineRequiresCrd() {
    // Mirrors the post-migration shape: legacy top-level requiresCRD is gone,
    // each engine carries its own requiresCRD flag. The provider id and
    // display name are non-canonical, so the canonical fallback in
    // providerRequiresRuntimeCRD cannot mask a buggy aggregation.
    const baseInstallation = JSON.parse(mockInferenceProviderConfig.metadata.annotations['airunway.ai/installation']);
    return {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'mycustom-runtime',
        annotations: {
          ...mockInferenceProviderConfig.metadata.annotations,
          'airunway.io/provider-name': 'My Custom Runtime',
          'airunway.ai/installation': JSON.stringify(baseInstallation),
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
  }

  const restores: Array<() => void> = [];

  afterEach(() => {
    restores.forEach((r) => r());
    restores.length = 0;
  });

  // ==========================================================================
  // GET /api/installation/providers/:providerId/status
  // ==========================================================================

  describe('GET /api/installation/providers/:providerId/status', () => {
    test('uses live KAITO installation status instead of provider config readiness', async () => {
      let kaitoStatusChecks = 0;

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
        mockServiceMethod(kubernetesService, 'checkKaitoInstallationStatus', async () => {
          kaitoStatusChecks += 1;
          return {
            installed: false,
            crdFound: true,
            operatorRunning: false,
            message: 'KAITO workspace CRD found but no ready KAITO operator pods were detected in kaito-workspace',
          };
        }),
      );

      const res = await app.request('/api/installation/providers/kaito/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.providerId).toBe('kaito');
      expect(data.providerName).toBe('KAITO');
      expect(kaitoStatusChecks).toBe(1);
      expect(data.installed).toBe(false);
      expect(data.crdFound).toBe(true);
      expect(data.operatorRunning).toBe(false);
      expect(data.version).toBe('0.10.0');
      expect(data.message).toBe('KAITO workspace CRD found but no ready KAITO operator pods were detected in kaito-workspace');
      expect(data.installationSteps).toBeDefined();
      expect(data.helmCommands).toBeDefined();
      expect(data.helmCommands.some((command: string) => command.includes('helm pull kaito/workspace'))).toBe(true);
      expect(data.helmCommands.some((command: string) => command.includes('kubectl apply --server-side --force-conflicts -f "$crd"'))).toBe(true);
      expect(data.helmCommands.some((command: string) => command.includes('--skip-crds'))).toBe(true);
    });

    test('uses live Dynamo installation status for non-KAITO providers', async () => {
      let kaitoStatusChecks = 0;
      let dynamoStatusChecks = 0;
      const nonKaitoConfig = {
        ...createDynamoProviderConfig(),
        status: {
          ready: false,
          version: '1.2.3',
        },
      };

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => nonKaitoConfig),
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

      const res = await app.request('/api/installation/providers/dynamo/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(kaitoStatusChecks).toBe(0);
      expect(dynamoStatusChecks).toBe(1);
      expect(data.providerId).toBe('dynamo');
      expect(data.providerName).toBe('Dynamo');
      expect(data.installed).toBe(false);
      expect(data.crdFound).toBe(false);
      expect(data.operatorRunning).toBe(false);
      expect(data.version).toBe('1.2.3');
      expect(data.message).toBe('Dynamo CRD not found');
      expect(data.helmCommands).toHaveLength(1);
      expect(data.helmCommands[0]).toContain('helm pull https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-1.0.1.tgz');
      expect(data.helmCommands[0]).toContain('kubectl apply --server-side --force-conflicts -f "$crd"');
      expect(data.helmCommands[0]).toContain('--skip-crds');
      expect(data.helmCommands[0]).toContain('--force-conflicts');
      expect(data.helmCommands[0]).toContain('global.grove.install=true');
    });

    test('marks providers without installation metadata as not installable', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => configWithoutInstallation),
        mockServiceMethod(kubernetesService, 'checkKaitoInstallationStatus', async () => ({
          installed: false,
          crdFound: false,
          operatorRunning: false,
          message: 'KAITO workspace CRD not found',
        })),
      );

      const res = await app.request('/api/installation/providers/kaito/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.installable).toBe(false);
      expect(data.helmCommands).toHaveLength(0);
      expect(data.message).toBe('No installation metadata found for provider kaito');
    });

    test('treats legacy LLM-D configs without requiresCRD as CRD-less', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => createLegacyNoCrdProviderConfigWithHelmMetadata()),
      );

      const res = await app.request('/api/installation/providers/llmd/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.providerId).toBe('llmd');
      expect(data.providerName).toBe('LLM-D');
      expect(data.installed).toBe(true);
      expect(data.requiresCRD).toBe(false);
      expect(data.installable).toBe(false);
      expect(data.helmCommands).toHaveLength(0);
      expect(data.message).toBe('Runtime is ready to use.');
    });

    test('does not mark CRD-less providers installable even if Helm metadata exists', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => createNoCrdProviderConfigWithHelmMetadata()),
      );

      const res = await app.request('/api/installation/providers/llmd/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.providerId).toBe('llmd');
      expect(data.providerName).toBe('LLM-D');
      expect(data.installed).toBe(true);
      expect(data.requiresCRD).toBe(false);
      expect(data.installable).toBe(false);
      expect(data.helmCommands).toHaveLength(0);
      expect(data.message).toBe('Runtime is ready to use.');
    });

    test('honors explicit requiresCRD metadata for custom-named CRD-less providers', async () => {
      restores.push(
        mockServiceMethod(
          kubernetesService,
          'getInferenceProviderConfig',
          async () => createCustomNamedNoCrdProviderConfigWithExplicitRequiresCrd(),
        ),
      );

      const res = await app.request('/api/installation/providers/custom-llmd-registration/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.providerId).toBe('custom-llmd-registration');
      expect(data.providerName).toBe('LLM-D');
      expect(data.installed).toBe(true);
      expect(data.requiresCRD).toBe(true);
      expect(data.installable).toBe(true);
      expect(data.helmCommands.length).toBeGreaterThan(0);
      expect(data.message).toBe('LLM-D is installed and running');
    });

    test('honors per-engine requiresCRD: false on the migrated schema for custom providers', async () => {
      restores.push(
        mockServiceMethod(
          kubernetesService,
          'getInferenceProviderConfig',
          async () => createCustomNoCrdProviderConfigWithPerEngineRequiresCrd(),
        ),
      );

      const res = await app.request('/api/installation/providers/mycustom-runtime/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.providerId).toBe('mycustom-runtime');
      expect(data.providerName).toBe('My Custom Runtime');
      expect(data.requiresCRD).toBe(false);
      expect(data.installable).toBe(false);
      expect(data.helmCommands).toHaveLength(0);
      expect(data.message).toBe('Runtime is ready to use.');
    });

    test('returns 404 for unknown provider', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => null),
      );

      const res = await app.request('/api/installation/providers/unknown/status');
      expect(res.status).toBe(404);
    });

    test('surfaces shim refuse-fast message when UpstreamReady=False is fresh', async () => {
      const freshHeartbeat = new Date(Date.now() - 30_000).toISOString();
      const configWithRefuseFast = {
        ...mockInferenceProviderConfig,
        status: {
          ready: false,
          version: 'kaito-provider:v0.1.0',
          lastHeartbeat: freshHeartbeat,
          conditions: [
            {
              type: 'UpstreamReady',
              status: 'False',
              reason: 'UpstreamControllerMissing',
              message: 'The KAITO workspace controller is not running. Install KAITO with `helm install kaito-workspace kaito/workspace`.',
            },
          ],
        },
      };

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => configWithRefuseFast),
        mockServiceMethod(kubernetesService, 'checkKaitoInstallationStatus', async () => ({
          installed: true,
          crdFound: true,
          operatorRunning: true,
          message: 'KAITO is installed and running',
        })),
      );

      const res = await app.request('/api/installation/providers/kaito/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      // Structural fields stay sourced from the live installation check.
      expect(data.installed).toBe(true);
      expect(data.operatorRunning).toBe(true);
      // Message should be the shim's specific refuse-fast guidance, not the
      // generic live-probe message.
      expect(data.message).toContain('The KAITO workspace controller is not running');
    });
  });

  // ==========================================================================
  // GET /api/installation/providers/:providerId/commands
  // ==========================================================================

  describe('GET /api/installation/providers/:providerId/commands', () => {
    test('returns commands when provider found', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
      );

      const res = await app.request('/api/installation/providers/kaito/commands');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.providerId).toBe('kaito');
      expect(data.providerName).toBe('KAITO');
      expect(data.commands).toBeDefined();
      expect(data.commands.some((command: string) => command.includes('helm pull kaito/workspace'))).toBe(true);
      expect(data.commands.some((command: string) => command.includes('kubectl apply --server-side --force-conflicts -f "$crd"'))).toBe(true);
      expect(data.commands.some((command: string) => command.includes('--skip-crds'))).toBe(true);
      expect(data.steps).toBeDefined();
    });

    test('preserves chart values in generated Dynamo CRD-safe commands', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => createDynamoProviderConfigWithNestedValues()),
      );

      const res = await app.request('/api/installation/providers/dynamo/commands');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.providerId).toBe('dynamo');
      expect(data.providerName).toBe('Dynamo');
      expect(data.commands).toHaveLength(1);
      expect(data.commands[0]).toContain('helm pull https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-1.0.1.tgz');
      expect(data.commands[0]).toContain('kubectl apply --server-side --force-conflicts -f "$crd"');
      expect(data.commands[0]).toContain('--skip-crds');
      expect(data.commands[0]).toContain('--force-conflicts');
      expect(data.commands[0]).toContain("--set-json 'dynamo-operator=");
      expect(data.commands[0]).toContain('"tag":"v0.15.0"');
    });

    test('includes helm values in generated commands when present', async () => {
      const baseInstallation = JSON.parse(mockInferenceProviderConfig.metadata.annotations['airunway.ai/installation']);
      const configWithValues = {
        ...mockInferenceProviderConfig,
        metadata: {
          ...mockInferenceProviderConfig.metadata,
          annotations: {
            ...mockInferenceProviderConfig.metadata.annotations,
            'airunway.ai/installation': JSON.stringify({
              ...baseInstallation,
              helmCharts: [
                {
                  ...baseInstallation.helmCharts[0],
                  values: {
                    'global.grove.install': true,
                  },
                },
              ],
            }),
          },
        },
      };

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => configWithValues),
      );

      const res = await app.request('/api/installation/providers/kaito/commands');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.commands.some((command: string) => command.includes('global.grove.install=true'))).toBe(true);
    });

    test('does not generate Helm install commands for CRD-less providers', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => createNoCrdProviderConfigWithHelmMetadata()),
      );

      const res = await app.request('/api/installation/providers/llmd/commands');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.providerId).toBe('llmd');
      expect(data.providerName).toBe('LLM-D');
      expect(data.commands).toHaveLength(0);
      expect(data.steps).toBeDefined();
    });

    test('returns 404 for unknown provider', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => null),
      );

      const res = await app.request('/api/installation/providers/unknown/commands');
      expect(res.status).toBe(404);
    });
  });

  // ==========================================================================
  // POST /api/installation/providers/:providerId/install
  // ==========================================================================

  describe('POST /api/installation/providers/:providerId/install', () => {
    test('returns 404 for unknown provider', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => null),
      );

      const res = await app.request('/api/installation/providers/unknown/install', { method: 'POST' });
      expect(res.status).toBe(404);
    });

    test('returns 400 when helm is not available', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: false, error: 'not found' })),
      );

      const res = await app.request('/api/installation/providers/kaito/install', { method: 'POST' });
      expect(res.status).toBe(400);
    });

    test('rejects legacy LLM-D installs before checking helm when requiresCRD is missing', async () => {
      let helmChecks = 0;
      let installAttempts = 0;

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => createLegacyNoCrdProviderConfigWithHelmMetadata()),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => {
          helmChecks += 1;
          return { available: true, version: '3.14.0' };
        }),
        mockServiceMethod(helmService, 'installProvider', async () => {
          installAttempts += 1;
          return {
            success: true,
            results: [{ step: 'install', result: { success: true, stdout: 'ok', stderr: '' } }],
          };
        }),
      );

      const res = await app.request('/api/installation/providers/llmd/install', { method: 'POST' });
      expect(res.status).toBe(400);

      const data = await res.json();
      expect(data.error.message).toContain('LLM-D is managed by provider registration and cannot be installed from this page.');
      expect(helmChecks).toBe(0);
      expect(installAttempts).toBe(0);
    });

    test('rejects CRD-less provider installs before checking helm', async () => {
      let helmChecks = 0;
      let installAttempts = 0;

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => createNoCrdProviderConfigWithHelmMetadata()),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => {
          helmChecks += 1;
          return { available: true, version: '3.14.0' };
        }),
        mockServiceMethod(helmService, 'installProvider', async () => {
          installAttempts += 1;
          return {
            success: true,
            results: [{ step: 'install', result: { success: true, stdout: 'ok', stderr: '' } }],
          };
        }),
      );

      const res = await app.request('/api/installation/providers/llmd/install', { method: 'POST' });
      expect(res.status).toBe(400);

      const data = await res.json();
      expect(data.error.message).toContain('LLM-D is managed by provider registration and cannot be installed from this page.');
      expect(helmChecks).toBe(0);
      expect(installAttempts).toBe(0);
    });

    test('rejects providers without installation metadata before checking helm', async () => {
      let helmChecks = 0;
      let installAttempts = 0;

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => configWithoutInstallation),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => {
          helmChecks += 1;
          return { available: true, version: '3.14.0' };
        }),
        mockServiceMethod(helmService, 'installProvider', async () => {
          installAttempts += 1;
          return {
            success: true,
            results: [{ step: 'install', result: { success: true, stdout: 'ok', stderr: '' } }],
          };
        }),
      );

      const res = await app.request('/api/installation/providers/kaito/install', { method: 'POST' });
      expect(res.status).toBe(400);

      const data = await res.json();
      expect(data.error.message).toContain('No installation metadata found for provider kaito');
      expect(helmChecks).toBe(0);
      expect(installAttempts).toBe(0);
    });

    test('returns 200 on successful install', async () => {
      let installCharts: HelmChart[] = [];

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'installProvider', async (_repos, charts) => {
          installCharts = charts;
          return {
            success: true,
            results: [{ step: 'install', result: { success: true, stdout: 'ok', stderr: '' } }],
          };
        }),
      );

      const res = await app.request('/api/installation/providers/kaito/install', { method: 'POST' });
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.success).toBe(true);
      expect(data.results).toBeDefined();
      expect(installCharts).toHaveLength(1);
      expect(installCharts[0].chart).toBe('kaito/workspace');
      expect(installCharts[0].preInstallMissingCrds).toBe(true);
      expect(installCharts[0].skipCrds).toBe(true);
    });

    test('uses CRD-safe chart install behavior for Dynamo', async () => {
      let installCharts: HelmChart[] = [];
      const dynamoConfig = createDynamoProviderConfig();

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => dynamoConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'installProvider', async (_repos, charts) => {
          installCharts = charts;
          return {
            success: true,
            results: [{ step: 'install', result: { success: true, stdout: 'ok', stderr: '' } }],
          };
        }),
      );

      const res = await app.request('/api/installation/providers/dynamo/install', { method: 'POST' });
      expect(res.status).toBe(200);

      expect(installCharts).toHaveLength(1);
      expect(installCharts[0].chart).toBe('https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-1.0.1.tgz');
      expect(installCharts[0].preInstallMissingCrds).toBe(true);
      expect(installCharts[0].skipCrds).toBe(true);
      expect(installCharts[0]).not.toHaveProperty('forceConflicts');
      expect(installCharts[0].values?.['global.grove.install']).toBe(true);
    });

    test('keeps standard chart install behavior for non-KAITO non-Dynamo providers', async () => {
      let installCharts: HelmChart[] = [];
      const kuberayConfig = createKubeRayProviderConfig();

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => kuberayConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'installProvider', async (_repos, charts) => {
          installCharts = charts;
          return {
            success: true,
            results: [{ step: 'install', result: { success: true, stdout: 'ok', stderr: '' } }],
          };
        }),
      );

      const res = await app.request('/api/installation/providers/kuberay/install', { method: 'POST' });
      expect(res.status).toBe(200);

      expect(installCharts).toHaveLength(1);
      expect(installCharts[0].chart).toBe('kuberay/kuberay-operator');
      expect(installCharts[0].preInstallMissingCrds).toBeUndefined();
      expect(installCharts[0].skipCrds).toBeUndefined();
    });

    test('returns clear installer RBAC guidance when provider install is forbidden', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'installProvider', async () => ({
          success: false,
          results: [{
            step: 'install-kaito-workspace',
            result: {
              success: false,
              stdout: '',
              stderr: 'customresourcedefinitions.apiextensions.k8s.io is forbidden: cannot create resource "customresourcedefinitions"',
              exitCode: 1,
            },
          }],
        })),
      );

      const res = await app.request('/api/installation/providers/kaito/install', { method: 'POST' });
      expect(res.status).toBe(403);

      const data = await res.json();
      expect(data.error.message).toContain('Automatic installation requires elevated installer permissions');
      expect(data.error.message).toContain('optional dashboard installer permissions manifest');
    });

    test('does not show installer RBAC guidance for unrelated errors mentioning bind', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'installProvider', async () => ({
          success: false,
          results: [{
            step: 'install-bind-test',
            result: {
              success: false,
              stdout: '',
              stderr: 'release bind-test failed: timed out waiting for condition',
              exitCode: 1,
            },
          }],
        })),
      );

      const res = await app.request('/api/installation/providers/kaito/install', { method: 'POST' });
      expect(res.status).toBe(500);

      const data = await res.json();
      expect(data.error.message).toBe('Internal Server Error');
      expect(data.error.message).not.toContain('Automatic installation requires elevated installer permissions');
    });

    test('returns 409 with short adoption guidance when Helm refuses to import existing CRDs', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'installProvider', async () => ({
          success: false,
          results: [{
            step: 'install-kaito-workspace',
            result: {
              success: false,
              stdout: '',
              stderr: 'Error: Unable to continue with install: CustomResourceDefinition "inferencesets.kaito.sh" in namespace "" exists and cannot be imported into the current release: invalid ownership metadata; label validation error: missing key "app.kubernetes.io/managed-by": must be set to "Helm"; annotation validation error: missing key "meta.helm.sh/release-name": must be set to "kaito-workspace"',
              exitCode: 1,
            },
          }],
        })),
      );

      const res = await app.request('/api/installation/providers/kaito/install', { method: 'POST' });
      expect(res.status).toBe(409);

      const data = await res.json();
      // 4xx errors must not be scrubbed by the global handler
      expect(data.error.message).not.toBe('Internal Server Error');
      // Concise, actionable, names the offending resource so the user knows what to look for
      expect(data.error.message).toContain('CustomResourceDefinition "inferencesets.kaito.sh"');
      expect(data.error.message).toContain('already exists on the cluster and is owned by another tool');
      expect(data.error.message).toContain('Uninstall the conflicting tool');
      expect(data.error.message).toContain('manual installation commands shown below');
      // Verbose helm boilerplate is not leaked to the UI toast (kept in backend logs)
      expect(data.error.message).not.toContain('invalid ownership metadata');
      expect(data.error.message).not.toContain('label validation error');
      expect(data.error.message).not.toContain('annotation validation error');
      // Keep message length sane for a toast/banner — under 350 chars
      expect(data.error.message.length).toBeLessThan(350);
      // Ownership errors are not installer-permission errors
      expect(data.error.message).not.toContain('Automatic installation requires elevated installer permissions');
    });

    test('falls back to generic subject when ownership error stderr lacks a parseable resource', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'installProvider', async () => ({
          success: false,
          results: [{
            step: 'install-kaito-workspace',
            result: {
              success: false,
              stdout: '',
              // Synthetic: triggers ownership detector but offers no parseable resource name
              stderr: 'rendered manifests contain a resource with invalid ownership metadata',
              exitCode: 1,
            },
          }],
        })),
      );

      const res = await app.request('/api/installation/providers/kaito/install', { method: 'POST' });
      expect(res.status).toBe(409);

      const data = await res.json();
      expect(data.error.message).toContain('a required cluster resource');
      expect(data.error.message).toContain('Uninstall the conflicting tool');
    });

  });

  // ==========================================================================
  // POST /api/installation/providers/:providerId/uninstall
  // ==========================================================================

  describe('POST /api/installation/providers/:providerId/uninstall', () => {
    test('returns 404 for unknown provider', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => null),
      );

      const res = await app.request('/api/installation/providers/unknown/uninstall', { method: 'POST' });
      expect(res.status).toBe(404);
    });

    test('returns 200 on successful uninstall', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'uninstall', async () => ({ success: true, stdout: 'ok', stderr: '' })),
      );

      const res = await app.request('/api/installation/providers/kaito/uninstall', { method: 'POST' });
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.success).toBe(true);
    });

    test('rejects CRD-less provider uninstalls before checking helm', async () => {
      let helmChecks = 0;
      let uninstallAttempts = 0;

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => createNoCrdProviderConfigWithHelmMetadata()),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => {
          helmChecks += 1;
          return { available: true, version: '3.14.0' };
        }),
        mockServiceMethod(helmService, 'uninstall', async () => {
          uninstallAttempts += 1;
          return { success: true, stdout: 'ok', stderr: '' };
        }),
      );

      const res = await app.request('/api/installation/providers/llmd/uninstall', { method: 'POST' });
      expect(res.status).toBe(400);

      const data = await res.json();
      expect(data.error.message).toContain('LLM-D is managed by provider registration and cannot be uninstalled from this page.');
      expect(helmChecks).toBe(0);
      expect(uninstallAttempts).toBe(0);
    });
  });

  // ==========================================================================
  // POST /api/installation/providers/:providerId/uninstall-crds
  // ==========================================================================

  describe('POST /api/installation/providers/:providerId/uninstall-crds', () => {
    test('returns 404 for unknown provider', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => null),
      );

      const res = await app.request('/api/installation/providers/unknown/uninstall-crds', { method: 'POST' });
      expect(res.status).toBe(404);
    });

    test('returns 200 on successful CRD removal', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
        mockServiceMethod(kubernetesService, 'deleteInferenceProviderConfig', async () => undefined),
      );

      const res = await app.request('/api/installation/providers/kaito/uninstall-crds', { method: 'POST' });
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.success).toBe(true);
    });
  });
});

describe('Gateway Installation Routes', () => {
  const restores: Array<() => void> = [];

  afterEach(() => {
    restores.forEach((r) => r());
    restores.length = 0;
  });

  // ==========================================================================
  // GET /api/installation/gateway/status
  // ==========================================================================

  describe('GET /api/installation/gateway/status', () => {
    test('returns gateway CRD status when CRDs are installed', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'checkGatewayCRDStatus', async () => ({
          gatewayApiInstalled: true,
          inferenceExtInstalled: true,
          gatewayApiVersion: 'v1.2.1',
          inferenceExtVersion: PINNED_GAIE_VERSION,
          pinnedVersion: PINNED_GAIE_VERSION,
          gatewayAvailable: true,
          gatewayEndpoint: '10.0.0.50',
          message: 'Gateway API and Inference Extension CRDs are installed. Gateway is available.',
          installCommands: [
            'kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml',
            `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/${PINNED_GAIE_VERSION}/manifests.yaml`,
          ],
        })),
      );

      const res = await app.request('/api/installation/gateway/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.gatewayApiInstalled).toBe(true);
      expect(data.inferenceExtInstalled).toBe(true);
      expect(data.gatewayApiVersion).toBe('v1.2.1');
      expect(data.inferenceExtVersion).toBe(PINNED_GAIE_VERSION);
      expect(data.pinnedVersion).toBe(PINNED_GAIE_VERSION);
      expect(data.gatewayAvailable).toBe(true);
      expect(data.gatewayEndpoint).toBe('10.0.0.50');
      expect(data.installCommands).toHaveLength(2);
    });

    test('returns status when CRDs are not installed', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'checkGatewayCRDStatus', async () => ({
          gatewayApiInstalled: false,
          inferenceExtInstalled: false,
          pinnedVersion: PINNED_GAIE_VERSION,
          gatewayAvailable: false,
          message: 'Gateway API and Inference Extension CRDs are not installed.',
          installCommands: [
            'kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml',
            `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/${PINNED_GAIE_VERSION}/manifests.yaml`,
          ],
        })),
      );

      const res = await app.request('/api/installation/gateway/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.gatewayApiInstalled).toBe(false);
      expect(data.inferenceExtInstalled).toBe(false);
      expect(data.gatewayAvailable).toBe(false);
    });
  });

  // ==========================================================================
  // POST /api/installation/gateway/install-crds
  // ==========================================================================

  describe('POST /api/installation/gateway/install-crds', () => {
    test('returns 200 on successful CRD installation', async () => {
      restores.push(
        mockServiceMethod(helmService, 'applyManifestUrl', async () => ({
          success: true,
          stdout: 'customresourcedefinition.apiextensions.k8s.io/gateways.gateway.networking.k8s.io created',
          stderr: '',
          exitCode: 0,
        })),
      );

      const res = await app.request('/api/installation/gateway/install-crds', { method: 'POST' });
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.success).toBe(true);
      expect(data.results).toHaveLength(2);
      expect(data.results[0].step).toBe('gateway-api-crds');
      expect(data.results[1].step).toBe('inference-extension-crds');
    });

    test('returns 500 when Gateway API CRD installation fails', async () => {
      let callCount = 0;
      restores.push(
        mockServiceMethod(helmService, 'applyManifestUrl', async () => {
          callCount++;
          if (callCount === 1) {
            return {
              success: false,
              stdout: '',
              stderr: 'connection refused',
              exitCode: 1,
            };
          }
          return { success: true, stdout: 'ok', stderr: '', exitCode: 0 };
        }),
      );

      const res = await app.request('/api/installation/gateway/install-crds', { method: 'POST' });
      expect(res.status).toBe(500);
    });
  });

  // ==========================================================================
  // GET /api/installation/gpu-throughput
  // ==========================================================================
  describe('GET /api/installation/gpu-throughput', () => {
    // A cluster with two pools: a multi-node A100 pool (8 GPUs across 4 nodes =
    // 2 per node) and a single-node H100 pool (8 GPUs on 1 node = 8 per node).
    function mockCapacity() {
      return {
        totalGpus: 16,
        allocatedGpus: 0,
        availableGpus: 16,
        maxContiguousAvailable: 8,
        maxNodeGpuCapacity: 8,
        gpuNodeCount: 5,
        totalMemoryGb: 1280,
        nodePools: [
          { name: 'a100-pool', gpuCount: 8, nodeCount: 4, availableGpus: 8, gpuModel: 'NVIDIA-A100-SXM4-80GB' },
          { name: 'h100-pool', gpuCount: 8, nodeCount: 1, availableGpus: 8, gpuModel: 'NVIDIA-H100-80GB-HBM3' },
        ],
      };
    }

    test('bounds tpSize and label to the requested pool per-node GPU count', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
      );

      // A100 pool hosts 2 GPUs per node, so a requested tpSize=8 is clamped to 2.
      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=A100-80GB&tpSize=8',
      );
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.gpuModel).toBe('A100-80GB');
      expect(data.tpSize).toBe(2);
      expect(data.capacityLabel).toBe('2x80 GB');
    });

    test('ignores a requested gpuModel absent from the cluster and falls back to highest-VRAM pool', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
      );

      // H200 is not in any pool; should fall through to the highest-VRAM pool.
      // A100 and H100 are both 80GB; the first-seen (A100) wins the >, so the
      // resolved model is whichever pool has strictly greater VRAM — here equal,
      // so the first pool (A100) is kept. Either way it must be a real pool model.
      const res = await app.request('/api/installation/gpu-throughput?gpuModel=H200');
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(['A100-80GB', 'H100-80GB']).toContain(data.gpuModel);
      // maxContiguous comes from the selected pool's per-node count (2 for A100).
      expect(data.tpSize).toBeLessThanOrEqual(8);
    });

    test('uses per-node count for the fallback (no explicit gpuModel) path', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
      );

      const res = await app.request('/api/installation/gpu-throughput?tpSize=8');
      expect(res.status).toBe(200);
      const data = await res.json();
      // Highest-VRAM pool selected (A100, first of the equal-VRAM pools): 2/node.
      expect(data.capacityLabel).toBe('2x80 GB');
      expect(data.tpSize).toBe(2);
    });

    test('returns 404 when no pool maps to a known GPU spec', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => ({
          totalGpus: 0,
          allocatedGpus: 0,
          availableGpus: 0,
          maxContiguousAvailable: 0,
          maxNodeGpuCapacity: 0,
          gpuNodeCount: 0,
          totalMemoryGb: 0,
          nodePools: [],
        })),
      );

      const res = await app.request('/api/installation/gpu-throughput?gpuModel=A100-80GB');
      expect(res.status).toBe(404);
    });

    test('returns 404 when the only pool runs a GPU absent from the static spec table', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => ({
          totalGpus: 8,
          allocatedGpus: 0,
          availableGpus: 8,
          maxContiguousAvailable: 8,
          maxNodeGpuCapacity: 8,
          gpuNodeCount: 1,
          totalMemoryGb: 0,
          // A brand-new / unsupported GPU label. It must be treated as unknown
          // (skipped) rather than silently coerced to an A10 estimate.
          nodePools: [
            { name: 'b200-pool', gpuCount: 8, nodeCount: 1, availableGpus: 8, gpuModel: 'NVIDIA-B200-192GB' },
          ],
        })),
      );

      const res = await app.request('/api/installation/gpu-throughput?gpuModel=NVIDIA-B200-192GB');
      expect(res.status).toBe(404);
    });

    test('skips an unknown-GPU pool and estimates on the known pool instead', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => ({
          totalGpus: 16,
          allocatedGpus: 0,
          availableGpus: 16,
          maxContiguousAvailable: 8,
          maxNodeGpuCapacity: 8,
          gpuNodeCount: 2,
          totalMemoryGb: 0,
          // Mixed cluster: one unrecognized GPU pool + one known A100 pool. The
          // unknown pool must be skipped so the estimate resolves to the A100,
          // never an A10 substitution for the B200.
          nodePools: [
            { name: 'b200-pool', gpuCount: 8, nodeCount: 1, availableGpus: 8, gpuModel: 'NVIDIA-B200-192GB' },
            { name: 'a100-pool', gpuCount: 8, nodeCount: 4, availableGpus: 8, gpuModel: 'NVIDIA-A100-SXM4-80GB' },
          ],
        })),
      );

      const res = await app.request('/api/installation/gpu-throughput?tpSize=8');
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.gpuModel).toBe('A100-80GB');
      expect(data.perGpuMemoryGb).toBe(80);
    });

    test('flags doesNotFit when model weights exceed available VRAM', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
        // Valid architecture so capacity is high-confidence (not lowConfidence).
        mockServiceMethod(huggingFaceService, 'getModelArchitecture', async () => ({
          numLayers: 80,
          numKvHeads: 8,
          headDim: 128,
        })),
      );

      // ~200B params in bf16 (2 bytes) → ~400GB weights; even split across the
      // A100 pool's 2 GPUs/node leaves ~200GB/GPU, far exceeding 80GB VRAM.
      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=A100-80GB&modelId=org/huge&paramCount=200000000000&quantization=bf16',
      );
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.doesNotFit).toBe(true);
      expect(data.concurrentSequences).toBe(0);
      expect(data.lowConfidence).toBe(false);
    });

    test('caps inferred context length at the model max when no contextLen is provided', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
        // Model advertises a 1M-token window but caller sends no contextLen.
        mockServiceMethod(huggingFaceService, 'getModelArchitecture', async () => ({
          numLayers: 80,
          numKvHeads: 8,
          headDim: 128,
          maxPositionEmbeddings: 1_000_000,
        })),
      );

      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=H100-80GB&modelId=org/long-ctx&paramCount=8000000000&quantization=bf16',
      );
      expect(res.status).toBe(200);
      const data = await res.json();
      // Falls back to maxPositionEmbeddings but capped at MAX_CONTEXT_LEN (32768).
      expect(data.contextLen).toBe(32768);
    });

    test('respects an explicit contextLen query param over the model max', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
        mockServiceMethod(huggingFaceService, 'getModelArchitecture', async () => ({
          numLayers: 80,
          numKvHeads: 8,
          headDim: 128,
          maxPositionEmbeddings: 1_000_000,
        })),
      );

      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=H100-80GB&modelId=org/long-ctx&paramCount=8000000000&quantization=bf16&contextLen=8192',
      );
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.contextLen).toBe(8192);
    });

    test('caps an explicit contextLen that exceeds the max', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
        mockServiceMethod(huggingFaceService, 'getModelArchitecture', async () => ({
          numLayers: 80,
          numKvHeads: 8,
          headDim: 128,
          maxPositionEmbeddings: 1_000_000,
        })),
      );

      // A caller forwarding a model's huge advertised window (here 256K) must be
      // clamped to MAX_CONTEXT_LEN so the concurrency estimate doesn't collapse.
      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=H100-80GB&modelId=org/long-ctx&paramCount=8000000000&quantization=bf16&contextLen=262144',
      );
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.contextLen).toBe(32768);
    });

    test('reports fp8Supported=true for an FP8-capable GPU', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
        mockServiceMethod(huggingFaceService, 'getModelArchitecture', async () => ({
          numLayers: 80,
          numKvHeads: 8,
          headDim: 128,
        })),
      );

      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=H100-80GB&modelId=org/m&paramCount=8000000000',
      );
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.fp8Supported).toBe(true);
      // aggregate ≈ concurrentSequences × perChatTokensPerSec (single-stream rate).
      // The response rounds perChat, so allow a small rounding margin.
      expect(data.aggregateTokensPerSec).toBeGreaterThan(0);
      expect(
        Math.abs(data.aggregateTokensPerSec - data.concurrentSequences * data.perChatTokensPerSec)
      ).toBeLessThanOrEqual(data.concurrentSequences);
    });

    test('reports fp8Supported=false for a GPU without an FP8 datapath', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
        mockServiceMethod(huggingFaceService, 'getModelArchitecture', async () => ({
          numLayers: 80,
          numKvHeads: 8,
          headDim: 128,
        })),
      );

      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=A100-80GB&modelId=org/m&paramCount=8000000000',
      );
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.fp8Supported).toBe(false);
      expect(data.aggregateTokensPerSec).toBeGreaterThan(0);
      expect(
        Math.abs(data.aggregateTokensPerSec - data.concurrentSequences * data.perChatTokensPerSec)
      ).toBeLessThanOrEqual(data.concurrentSequences);
    });

    test('degrades to a low-confidence estimate for a malformed modelId without calling the HF service', async () => {
      let archCalls = 0;
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
        mockServiceMethod(huggingFaceService, 'getModelArchitecture', async () => {
          archCalls += 1;
          return undefined;
        }),
      );

      // A malformed id (path-traversal here) no longer hard-400s: paramCount is
      // enough for a bandwidth-only estimate. Security is preserved by gating the
      // token-bearing HF fetch on isValidHfRepoId, so the id never reaches it.
      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=H100-80GB&modelId=../../etc/passwd&paramCount=8000000000',
      );
      expect(res.status).toBe(200);
      const data = await res.json();
      // No arch → low-confidence, per-chat-only (no concurrency number).
      expect(data.lowConfidence).toBe(true);
      expect(data.perChatTokensPerSec).toBeGreaterThan(0);
      expect(data.concurrentSequences).toBeUndefined();
      // Security: the invalid id must never trigger a token-bearing HF request.
      expect(archCalls).toBe(0);
    });

    test('produces a param-count-only estimate for a non-HF custom modelId without calling the HF service', async () => {
      let archCalls = 0;
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
        mockServiceMethod(huggingFaceService, 'getModelArchitecture', async () => {
          archCalls += 1;
          return undefined;
        }),
      );

      // A curated/custom id that is valid for Airunway but not a HuggingFace repo
      // (three path segments) must not 400 — it degrades to the bandwidth-only
      // estimate from paramCount and skips the HF architecture lookup entirely.
      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=H100-80GB&modelId=catalog/custom/model&paramCount=8000000000',
      );
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.lowConfidence).toBe(true);
      expect(data.perChatTokensPerSec).toBeGreaterThan(0);
      expect(data.concurrentSequences).toBeUndefined();
      expect(archCalls).toBe(0);
    });

    test('rejects a paramCount above the maximum with 400 and never estimates', async () => {
      let archCalls = 0;
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
        mockServiceMethod(huggingFaceService, 'getModelArchitecture', async () => {
          archCalls += 1;
          return undefined;
        }),
      );

      // 9T + 1 exceeds the .max(9_000_000_000_000) cap; zValidator rejects before the handler runs.
      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=H100-80GB&paramCount=9000000000001',
      );
      expect(res.status).toBe(400);
      expect(archCalls).toBe(0);
    });

    test('accepts a well-formed modelId (200)', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDetailedClusterGpuCapacity', async () => mockCapacity()),
        mockServiceMethod(huggingFaceService, 'getModelArchitecture', async () => ({
          numLayers: 80,
          numKvHeads: 8,
          headDim: 128,
        })),
      );

      const res = await app.request(
        '/api/installation/gpu-throughput?gpuModel=H100-80GB&modelId=meta-llama/Meta-Llama-3-70B&paramCount=8000000000',
      );
      expect(res.status).toBe(200);
    });
  });
});
