import { mkdirSync, writeFileSync } from 'fs';
import { join } from 'path';
import { describe, test, expect, afterEach } from 'bun:test';
import type { HelmResult, HelmRelease, HelmRepo, HelmChart } from './helm';
import { GPU_OPERATOR_REPO, GPU_OPERATOR_CHART, helmService } from './helm';

describe('HelmService - GPU Operator Constants', () => {
  test('GPU_OPERATOR_REPO has correct configuration', () => {
    expect(GPU_OPERATOR_REPO.name).toBe('nvidia');
    expect(GPU_OPERATOR_REPO.url).toBe('https://helm.ngc.nvidia.com/nvidia');
  });

  test('GPU_OPERATOR_CHART has correct configuration', () => {
    expect(GPU_OPERATOR_CHART.name).toBe('gpu-operator');
    expect(GPU_OPERATOR_CHART.chart).toBe('nvidia/gpu-operator');
    expect(GPU_OPERATOR_CHART.namespace).toBe('gpu-operator');
    expect(GPU_OPERATOR_CHART.createNamespace).toBe(true);
  });
});

describe('HelmService - HelmResult Structure', () => {
  test('creates successful result', () => {
    const result: HelmResult = {
      success: true,
      stdout: 'Release "my-app" has been installed.',
      stderr: '',
      exitCode: 0,
    };

    expect(result.success).toBe(true);
    expect(result.exitCode).toBe(0);
    expect(result.stderr).toBe('');
  });

  test('creates failed result', () => {
    const result: HelmResult = {
      success: false,
      stdout: '',
      stderr: 'Error: release "my-app" already exists',
      exitCode: 1,
    };

    expect(result.success).toBe(false);
    expect(result.exitCode).toBe(1);
    expect(result.stderr).toContain('already exists');
  });

  test('creates timeout result', () => {
    const result: HelmResult = {
      success: false,
      stdout: 'Partial output...',
      stderr: 'Command timed out',
      exitCode: null,
    };

    expect(result.success).toBe(false);
    expect(result.exitCode).toBeNull();
    expect(result.stderr).toContain('timed out');
  });

  test('creates execution error result', () => {
    const result: HelmResult = {
      success: false,
      stdout: '',
      stderr: 'Failed to execute helm: ENOENT',
      exitCode: null,
    };

    expect(result.success).toBe(false);
    expect(result.stderr).toContain('Failed to execute');
  });
});

describe('HelmService - HelmRelease Structure', () => {
  test('creates valid release info', () => {
    const release: HelmRelease = {
      name: 'dynamo',
      namespace: 'dynamo',
      revision: '1',
      updated: '2024-01-15 10:30:00.123456789 +0000 UTC',
      status: 'deployed',
      chart: 'dynamo-0.1.0',
      appVersion: '0.1.0',
    };

    expect(release.name).toBe('dynamo');
    expect(release.status).toBe('deployed');
    expect(release.revision).toBe('1');
  });

  test('handles pending-install status', () => {
    const release: HelmRelease = {
      name: 'gpu-operator',
      namespace: 'gpu-operator',
      revision: '1',
      updated: '2024-01-15 10:30:00.123456789 +0000 UTC',
      status: 'pending-install',
      chart: 'gpu-operator-23.9.0',
      appVersion: '23.9.0',
    };

    expect(release.status).toBe('pending-install');
  });

  test('handles failed status', () => {
    const release: HelmRelease = {
      name: 'broken-app',
      namespace: 'default',
      revision: '3',
      updated: '2024-01-15 10:30:00.123456789 +0000 UTC',
      status: 'failed',
      chart: 'my-chart-1.0.0',
      appVersion: '1.0.0',
    };

    expect(release.status).toBe('failed');
  });
});

describe('HelmService - Command Building Logic', () => {
  // Test the logic for building helm commands (mirrors actual helm.ts behavior)
  // Helper to convert values to --set-json args (mirrors helm.ts valuesToSetJsonArgs)
  function valuesToSetJsonArgs(values: Record<string, unknown>): string[] {
    const args: string[] = [];
    for (const [key, value] of Object.entries(values)) {
      args.push('--set-json', `${key}=${JSON.stringify(value)}`);
    }
    return args;
  }

  function buildInstallCommand(chart: HelmChart): string[] {
    const args = ['upgrade', chart.name, chart.chart, '--install'];
    args.push('--namespace', chart.namespace);

    if (chart.createNamespace) {
      args.push('--create-namespace');
    }

    if (chart.version) {
      args.push('--version', chart.version);
    }

    if (chart.values) {
      args.push(...valuesToSetJsonArgs(chart.values));
    }

    return args;
  }

  test('builds basic install command', () => {
    const chart: HelmChart = {
      name: 'my-app',
      chart: 'repo/my-chart',
      namespace: 'my-namespace',
    };

    const args = buildInstallCommand(chart);
    expect(args).toContain('upgrade');
    expect(args).toContain('my-app');
    expect(args).toContain('repo/my-chart');
    expect(args).toContain('--install');
    expect(args).toContain('--namespace');
    expect(args).toContain('my-namespace');
  });

  test('adds --create-namespace when specified', () => {
    const chart: HelmChart = {
      name: 'my-app',
      chart: 'repo/my-chart',
      namespace: 'new-namespace',
      createNamespace: true,
    };

    const args = buildInstallCommand(chart);
    expect(args).toContain('--create-namespace');
  });

  test('does not add --create-namespace when false', () => {
    const chart: HelmChart = {
      name: 'my-app',
      chart: 'repo/my-chart',
      namespace: 'existing-namespace',
      createNamespace: false,
    };

    const args = buildInstallCommand(chart);
    expect(args).not.toContain('--create-namespace');
  });

  test('adds version when specified', () => {
    const chart: HelmChart = {
      name: 'my-app',
      chart: 'repo/my-chart',
      namespace: 'default',
      version: '1.2.3',
    };

    const args = buildInstallCommand(chart);
    expect(args).toContain('--version');
    expect(args).toContain('1.2.3');
  });

  test('adds --set-json with values when specified', () => {
    const chart: HelmChart = {
      name: 'kaito-workspace',
      chart: 'kaito/workspace',
      namespace: 'kaito-workspace',
      version: '0.10.0',
      values: {
        featureGates: {
          enableInferenceSetController: true,
          disableNodeAutoProvisioning: true,
        },
      },
    };

    const args = buildInstallCommand(chart);
    expect(args).toContain('--set-json');
    // New format: --set-json 'key={"nested":"value"}'
    const setJsonIndex = args.indexOf('--set-json');
    const valuesArg = args[setJsonIndex + 1];
    expect(valuesArg).toBe('featureGates={"enableInferenceSetController":true,"disableNodeAutoProvisioning":true}');
  });

  test('handles nested values object correctly', () => {
    const chart: HelmChart = {
      name: 'test-chart',
      chart: 'repo/chart',
      namespace: 'test-ns',
      values: {
        level1: {
          level2: {
            level3: 'deep-value',
          },
          simple: 123,
        },
        array: [1, 2, 3],
      },
    };

    const args = buildInstallCommand(chart);
    // Should have two --set-json entries (one per top-level key)
    const setJsonIndices = args.reduce<number[]>((indices, arg, i) => {
      if (arg === '--set-json') indices.push(i);
      return indices;
    }, []);
    expect(setJsonIndices.length).toBe(2);
    
    // Check level1 value
    const level1Arg = args.find(arg => arg.startsWith('level1='));
    expect(level1Arg).toBeDefined();
    const level1Value = JSON.parse(level1Arg!.split('=').slice(1).join('='));
    expect(level1Value.level2.level3).toBe('deep-value');
    expect(level1Value.simple).toBe(123);
    
    // Check array value
    const arrayArg = args.find(arg => arg.startsWith('array='));
    expect(arrayArg).toBeDefined();
    const arrayValue = JSON.parse(arrayArg!.split('=').slice(1).join('='));
    expect(arrayValue).toEqual([1, 2, 3]);
  });
});

describe('HelmService - getInstallCommands Logic', () => {
  function getInstallCommands(repos: HelmRepo[], charts: HelmChart[]): string[] {
    return helmService.getInstallCommands(repos, charts);
  }

  test('generates repo add commands', () => {
    const repos: HelmRepo[] = [
      { name: 'nvidia', url: 'https://helm.ngc.nvidia.com/nvidia' },
    ];
    const charts: HelmChart[] = [];

    const commands = getInstallCommands(repos, charts);
    expect(commands).toContain('helm repo add nvidia https://helm.ngc.nvidia.com/nvidia');
    expect(commands).toContain('helm repo update');
  });

  test('generates install commands', () => {
    const repos: HelmRepo[] = [];
    const charts: HelmChart[] = [
      { name: 'my-app', chart: 'repo/chart', namespace: 'default' },
    ];

    const commands = getInstallCommands(repos, charts);
    expect(commands[0]).toContain('helm install my-app repo/chart');
    expect(commands[0]).toContain('--namespace default');
  });

  test('includes simple values in generated install commands', () => {
    const repos: HelmRepo[] = [];
    const charts: HelmChart[] = [
      {
        name: 'dynamo-platform',
        chart: 'https://example.com/dynamo-platform.tgz',
        namespace: 'dynamo-system',
        createNamespace: true,
        values: {
          'global.grove.install': true,
        },
      },
    ];

    const commands = getInstallCommands(repos, charts);
    expect(commands[0]).toContain('global.grove.install=true');
  });

  test('generates commands for GPU Operator', () => {
    const commands = getInstallCommands([GPU_OPERATOR_REPO], [GPU_OPERATOR_CHART]);

    expect(commands[0]).toBe('helm repo add nvidia https://helm.ngc.nvidia.com/nvidia');
    expect(commands[1]).toBe('helm repo update');
    expect(commands[2]).toContain('helm install gpu-operator nvidia/gpu-operator');
    expect(commands[2]).toContain('--namespace gpu-operator');
    expect(commands[2]).toContain('--create-namespace');
  });

  test('handles charts with fetchUrl', () => {
    const charts: HelmChart[] = [
      {
        name: 'custom-chart',
        chart: '/tmp/chart.tgz',
        namespace: 'custom-ns',
        fetchUrl: 'https://example.com/chart.tgz',
        createNamespace: true,
      },
    ];

    const commands = getInstallCommands([], charts);
    expect(commands[0]).toContain('helm fetch https://example.com/chart.tgz');
    expect(commands[0]).toContain('--create-namespace');
  });

  test('includes --set-json overrides in generated install commands', () => {
    const charts: HelmChart[] = [
      {
        name: 'dynamo-platform',
        chart: 'https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-0.7.1.tgz',
        namespace: 'dynamo-system',
        createNamespace: true,
        values: {
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
        },
      },
    ];

    const commands = getInstallCommands([], charts);
    expect(commands).toHaveLength(1);
    expect(commands[0]).toContain("--set-json 'dynamo-operator=");
    expect(commands[0]).toContain('"repository":"quay.io/brancz/kube-rbac-proxy"');
    expect(commands[0]).toContain('"tag":"v0.15.0"');
  });

  test('includes pre-CRD apply commands and --skip-crds when requested', () => {
    const charts: HelmChart[] = [
      {
        name: 'kaito-workspace',
        chart: 'kaito/workspace',
        namespace: 'kaito-workspace',
        createNamespace: true,
        preCrdUrls: ['https://example.com/crd.yaml'],
        skipCrds: true,
      },
    ];

    const commands = getInstallCommands([], charts);
    expect(commands[0]).toBe('kubectl apply -f https://example.com/crd.yaml');
    expect(commands[1]).toContain('helm install kaito-workspace kaito/workspace');
    expect(commands[1]).toContain('--skip-crds');
  });

  test('emits selective chart CRD setup commands when preInstallMissingCrds is requested', () => {
    const charts: HelmChart[] = [
      {
        name: 'kaito-workspace',
        chart: 'kaito/workspace',
        version: '0.9.0',
        namespace: 'kaito-workspace',
        createNamespace: true,
        preInstallMissingCrds: true,
        skipCrds: true,
      },
    ];

    const commands = getInstallCommands([], charts);
    expect(commands).toHaveLength(1);
    expect(commands[0]).toContain('(KAITO_WORKSPACE_CHART_DIR=$(mktemp -d)');
    expect(commands[0]).toContain("trap 'rm -rf -- \"$KAITO_WORKSPACE_CHART_DIR\"' EXIT");
    expect(commands[0]).toContain('helm pull kaito/workspace --untar --untardir "$KAITO_WORKSPACE_CHART_DIR" --version 0.9.0');
    expect(commands[0]).toContain('kubectl create --dry-run=client -f "$crd" -o name');
    expect(commands[0]).toContain('kubectl get "$crd_name" --ignore-not-found -o name');
    expect(commands[0]).toContain('kubectl apply --server-side --force-conflicts -f "$crd"');
    expect(commands[0]).toContain('*.yml');
    expect(commands[0]).toContain('helm install kaito-workspace "$KAITO_WORKSPACE_CHART_PATH"');
    expect(commands[0]).not.toContain('helm install kaito-workspace "$KAITO_WORKSPACE_CHART_PATH" --namespace kaito-workspace --create-namespace --version');
    expect(commands[0]).toContain('--skip-crds');
  });
});

describe('HelmService - Release Status Detection', () => {
  function isProblematicStatus(status: string): boolean {
    const s = status.toLowerCase();
    // Only 'failed' is truly problematic
    // pending-install/upgrade are normal during installation
    return s === 'failed';
  }

  function isPendingStatus(status: string): boolean {
    const s = status.toLowerCase();
    return s === 'pending-install' || s === 'pending-upgrade' || s === 'pending-rollback';
  }

  test('identifies failed status as problematic', () => {
    expect(isProblematicStatus('failed')).toBe(true);
    expect(isProblematicStatus('Failed')).toBe(true);
    expect(isProblematicStatus('FAILED')).toBe(true);
  });

  test('does not treat pending statuses as problematic', () => {
    expect(isProblematicStatus('pending-install')).toBe(false);
    expect(isProblematicStatus('pending-upgrade')).toBe(false);
    expect(isProblematicStatus('pending-rollback')).toBe(false);
  });

  test('does not treat deployed as problematic', () => {
    expect(isProblematicStatus('deployed')).toBe(false);
  });

  test('identifies pending statuses', () => {
    expect(isPendingStatus('pending-install')).toBe(true);
    expect(isPendingStatus('pending-upgrade')).toBe(true);
    expect(isPendingStatus('pending-rollback')).toBe(true);
    expect(isPendingStatus('Pending-Install')).toBe(true);
  });

  test('does not treat deployed as pending', () => {
    expect(isPendingStatus('deployed')).toBe(false);
    expect(isPendingStatus('failed')).toBe(false);
  });
});

describe('HelmService - Managed Chart CRDs', () => {
  const service = helmService as any;
  const originalExecute = service.execute;
  const originalExecuteKubectl = service.executeKubectl;

  afterEach(() => {
    service.execute = originalExecute;
    service.executeKubectl = originalExecuteKubectl;
  });

  test('preinstalls only missing chart CRDs before installing with --skip-crds', async () => {
    const kubectlCalls: string[][] = [];
    const helmCalls: string[][] = [];

    service.execute = async (args: string[]) => {
      helmCalls.push(args);

      if (args[0] === 'pull') {
        const untarDirIndex = args.indexOf('--untardir');
        const untarDir = args[untarDirIndex + 1];
        mkdirSync(join(untarDir, 'workspace-0.9.0.tgz'), { recursive: true });
        const chartDir = join(untarDir, 'workspace');
        const crdsDir = join(chartDir, 'crds');
        mkdirSync(crdsDir, { recursive: true });
        writeFileSync(join(chartDir, 'Chart.yaml'), 'apiVersion: v2\nname: workspace\nversion: 0.9.0\n', 'utf8');
        writeFileSync(
          join(crdsDir, 'workspaces.kaito.sh.yaml'),
          [
            'apiVersion: apiextensions.k8s.io/v1',
            'kind: CustomResourceDefinition',
            'metadata:',
            '  name: workspaces.kaito.sh',
            'spec:',
            '  group: kaito.sh',
          ].join('\n'),
          'utf8',
        );
        writeFileSync(
          join(crdsDir, 'inferencepools.inference.networking.k8s.io.yaml'),
          [
            'apiVersion: apiextensions.k8s.io/v1',
            'kind: CustomResourceDefinition',
            'metadata:',
            '  name: inferencepools.inference.networking.k8s.io',
            'spec:',
            '  group: inference.networking.k8s.io',
          ].join('\n'),
          'utf8',
        );
        const subchartCrdsDir = join(chartDir, 'charts', 'scheduler', 'crds');
        mkdirSync(subchartCrdsDir, { recursive: true });
        writeFileSync(
          join(subchartCrdsDir, 'podgroups.scheduler.example.com.yaml'),
          [
            'apiVersion: apiextensions.k8s.io/v1',
            'kind: CustomResourceDefinition',
            'metadata:',
            '  name: podgroups.scheduler.example.com',
            'spec:',
            '  group: scheduler.example.com',
          ].join('\n'),
          'utf8',
        );

        return { success: true, stdout: '', stderr: '', exitCode: 0 };
      }

      if (args[0] === 'upgrade') {
        expect(args).toContain('--skip-crds');
        expect(args).not.toContain('--version');
        expect(args[2]).toContain('/workspace');
        return { success: true, stdout: 'installed', stderr: '', exitCode: 0 };
      }

      return { success: true, stdout: '', stderr: '', exitCode: 0 };
    };

    service.executeKubectl = async (args: string[]) => {
      kubectlCalls.push(args);

      if (args[0] === 'get' && args[2] === 'workspaces.kaito.sh') {
        return { success: true, stdout: '', stderr: '', exitCode: 0 };
      }

      if (args[0] === 'get' && args[2] === 'inferencepools.inference.networking.k8s.io') {
        return {
          success: true,
          stdout: 'crd/inferencepools.inference.networking.k8s.io\n',
          stderr: '',
          exitCode: 0,
        };
      }

      if (args[0] === 'apply') {
        return { success: true, stdout: 'applied', stderr: '', exitCode: 0 };
      }

      return { success: true, stdout: '', stderr: '', exitCode: 0 };
    };

    const result = await helmService.installProvider([], [
      {
        name: 'kaito-workspace',
        chart: 'kaito/workspace',
        version: '0.9.0',
        namespace: 'kaito-workspace',
        createNamespace: true,
        preInstallMissingCrds: true,
        skipCrds: true,
      },
    ]);

    expect(result.success).toBe(true);
    expect(result.results.some((step) => step.step === 'apply-crd-workspaces-kaito-sh')).toBe(true);
    expect(result.results.some((step) => step.step === 'skip-crd-inferencepools-inference-networking-k8s-io')).toBe(true);
    expect(kubectlCalls.some((args) => args[0] === 'apply')).toBe(true);
    expect(helmCalls.some((args) => args[0] === 'upgrade')).toBe(true);
  });
});
