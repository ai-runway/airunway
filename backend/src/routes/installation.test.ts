import { describe, test, expect, afterEach } from 'bun:test';
import app from '../hono-app';
import { kubernetesService } from '../services/kubernetes';
import { helmService } from '../services/helm';
import { mockServiceMethod } from '../test/helpers';
import { mockInferenceProviderConfig } from '../test/fixtures';

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
          ...config.spec.capabilities,
          requiresCRD: true,
        },
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

    test('returns 404 for unknown provider', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => null),
      );

      const res = await app.request('/api/installation/providers/unknown/status');
      expect(res.status).toBe(404);
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
      let installCharts: any[] = [];

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'installProvider', async (_repos, charts) => {
          installCharts = charts as any[];
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
      let installCharts: any[] = [];
      const dynamoConfig = createDynamoProviderConfig();

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => dynamoConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'installProvider', async (_repos, charts) => {
          installCharts = charts as any[];
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
      let installCharts: any[] = [];
      const kuberayConfig = createKubeRayProviderConfig();

      restores.push(
        mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => kuberayConfig),
        mockServiceMethod(helmService, 'checkHelmAvailable', async () => ({ available: true, version: '3.14.0' })),
        mockServiceMethod(helmService, 'installProvider', async (_repos, charts) => {
          installCharts = charts as any[];
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
      expect(data.error.message).toContain('release bind-test failed');
      expect(data.error.message).not.toContain('Automatic installation requires elevated installer permissions');
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
          inferenceExtVersion: 'v1.5.0',
          pinnedVersion: 'v1.3.1',
          gatewayAvailable: true,
          gatewayEndpoint: '10.0.0.50',
          message: 'Gateway API and Inference Extension CRDs are installed. Gateway is available.',
          installCommands: [
            'kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml',
            'kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.3.1/manifests.yaml',
          ],
        })),
      );

      const res = await app.request('/api/installation/gateway/status');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.gatewayApiInstalled).toBe(true);
      expect(data.inferenceExtInstalled).toBe(true);
      expect(data.gatewayApiVersion).toBe('v1.2.1');
      expect(data.inferenceExtVersion).toBe('v1.5.0');
      expect(data.pinnedVersion).toBe('v1.3.1');
      expect(data.gatewayAvailable).toBe(true);
      expect(data.gatewayEndpoint).toBe('10.0.0.50');
      expect(data.installCommands).toHaveLength(2);
    });

    test('returns status when CRDs are not installed', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'checkGatewayCRDStatus', async () => ({
          gatewayApiInstalled: false,
          inferenceExtInstalled: false,
          pinnedVersion: 'v1.3.1',
          gatewayAvailable: false,
          message: 'Gateway API and Inference Extension CRDs are not installed.',
          installCommands: [
            'kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml',
            'kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.3.1/manifests.yaml',
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
});
