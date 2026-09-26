import { describe, expect, test } from 'bun:test';
import { defaultDynamoIntent, deploymentRequest, toModelDeploymentSpec, toDeploymentStatus, buildPortForwardCommand, type DeploymentConfig, type ModelDeployment } from '@airunway/shared';

const config: DeploymentConfig = { name: 'auto', namespace: 'models', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm', provider: 'dynamo', mode: 'aggregated', replicas: 1, routerMode: 'default', enforceEager: false, enablePrefixCaching: false, trustRemoteCode: false,
  resources: { gpu: 1 }, prefillReplicas: 1, decodeReplicas: 1,
  providerOverrides: { deploymentMode: 'intent', intent: defaultDynamoIntent() } };

describe('automatic deployment conversion', () => {
  test('strips manual fields from both wire payload and manifest', () => {
    const payload = deploymentRequest(config);
    for (const key of ['resources', 'replicas', 'mode', 'prefillReplicas', 'decodeReplicas']) expect(payload).not.toHaveProperty(key);
    const spec = toModelDeploymentSpec(config);
    expect(spec).not.toHaveProperty('resources'); expect(spec).not.toHaveProperty('scaling'); expect(spec).not.toHaveProperty('serving');
    expect(spec.provider?.overrides).toEqual(config.providerOverrides);
    expect(deploymentRequest({ ...config, providerOverrides: undefined })).toEqual({ ...config, providerOverrides: undefined });
  });

  test('does not infer readiness or endpoint from profiling pods', () => {
    const md: ModelDeployment = { apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: 'auto', namespace: 'models' }, spec: toModelDeploymentSpec(config), status: { phase: 'Deploying', provider: { intent: { phase: 'Profiling' } } } };
    const result = toDeploymentStatus(md, [{ name: 'profiler', phase: 'Running', ready: true, restarts: 0 }]);
    expect(result.phase).toBe('Deploying'); expect(result.frontendService).toBeUndefined();
    expect(result.replicas).toEqual({ desired: 0, ready: 0, available: 0 });
  });

  test('normalizes omitted zero counters without treating profiler pods as serving', () => {
    const md: ModelDeployment = { apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: 'auto', namespace: 'models' }, spec: toModelDeploymentSpec(config), status: { phase: 'Failed', replicas: {} } };
    const pods = [{ name: 'profiler', phase: 'Running' as const, ready: true, restarts: 0 }];
    expect(toDeploymentStatus(md, pods).replicas).toEqual({ desired: 0, ready: 0, available: 0 });
    md.status!.replicas = { desired: 2 };
    expect(toDeploymentStatus(md, pods).replicas).toEqual({ desired: 2, ready: 0, available: 0 });
  });

  test('exposes real endpoint, namespace, refs and optimistic revision', () => {
    const md: ModelDeployment = { apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: 'auto', namespace: 'models', resourceVersion: '42' }, spec: toModelDeploymentSpec(config), status: { phase: 'Running', endpoint: { service: 'custom-frontend', port: 9000 }, provider: { resourceKind: 'DynamoGraphDeploymentRequest', resourceName: 'request-1', workloadRef: { name: 'custom', namespace: 'serving', kind: 'DynamoGraphDeployment', uid: 'uid' }, intent: { phase: 'Deployed', attempt: 'attempt' } }, replicas: { desired: 2, ready: 2, available: 2 } } };
    const result = toDeploymentStatus(md);
    expect(result.frontendService).toBe('custom-frontend:9000'); expect(result.frontendNamespace).toBe('serving');
    expect(result.providerStatus?.resourceName).toBe('request-1'); expect(result.resourceVersion).toBe('42');
    expect(buildPortForwardCommand(result)).toContain('svc/custom-frontend 8000:9000 -n serving');
  });
});
