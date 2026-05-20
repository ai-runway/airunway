import { http, HttpResponse } from 'msw'
import { toModelDeploymentManifest } from '@airunway/shared'
import type { DeploymentConfig } from '@airunway/shared'
import {
  mockModels,
  mockDeployments,
  mockSettings,
  mockClusterStatus,
} from './data'

// Re-export shared mock data for backward compatibility
export { mockModels, mockDeployments, mockSettings } from './data'

// Use wildcard prefix to match both relative URLs (/api/...) and absolute URLs (http://localhost:3001/api/...)
const API_BASE = '*/api'

export const handlers = [
  // Models API
  http.get(`${API_BASE}/models`, () => {
    return HttpResponse.json({ models: mockModels })
  }),

  http.get(`${API_BASE}/models/:id`, ({ params }) => {
    const id = decodeURIComponent(params.id as string)
    const model = mockModels.find(m => m.id === id)
    if (!model) {
      return HttpResponse.json({ error: { message: 'Model not found' } }, { status: 404 })
    }
    return HttpResponse.json(model)
  }),

  // Deployments API
  http.get(`${API_BASE}/deployments`, ({ request }) => {
    const url = new URL(request.url)
    const namespace = url.searchParams.get('namespace')
    const filtered = namespace
      ? mockDeployments.filter(d => d.namespace === namespace)
      : mockDeployments
    return HttpResponse.json({ deployments: filtered })
  }),

  http.get(`${API_BASE}/deployments/:name`, ({ params, request }) => {
    const name = params.name as string
    const url = new URL(request.url)
    const namespace = url.searchParams.get('namespace')
    const deployment = mockDeployments.find(
      d => d.name === name && (!namespace || d.namespace === namespace)
    )
    if (!deployment) {
      return HttpResponse.json({ error: { message: 'Deployment not found' } }, { status: 404 })
    }
    return HttpResponse.json(deployment)
  }),

  http.post(`${API_BASE}/deployments/preview`, async ({ request }) => {
    const config = await request.json() as DeploymentConfig
    const manifest = toModelDeploymentManifest({
      ...config,
      namespace: config.namespace || 'airunway-system',
    })
    return HttpResponse.json({
      resources: [{
        kind: 'ModelDeployment',
        apiVersion: 'airunway.ai/v1alpha1',
        name: config.name,
        manifest: manifest as unknown as Record<string, unknown>,
      }],
      primaryResource: { kind: 'ModelDeployment', apiVersion: 'airunway.ai/v1alpha1' },
    })
  }),

  http.post(`${API_BASE}/deployments`, async ({ request }) => {
    const config = await request.json() as { name: string; namespace: string }
    return HttpResponse.json({
      message: 'Deployment created',
      name: config.name,
      namespace: config.namespace,
    })
  }),

  http.delete(`${API_BASE}/deployments/:name`, () => {
    return HttpResponse.json({ message: 'Deployment deleted' })
  }),

  http.get(`${API_BASE}/deployments/:name/pods`, ({ params }) => {
    const name = params.name as string
    const deployment = mockDeployments.find(d => d.name === name)
    return HttpResponse.json({ pods: deployment?.pods || [] })
  }),

  // Health API
  http.get(`${API_BASE}/health`, () => {
    return HttpResponse.json({
      status: 'ok',
      timestamp: new Date().toISOString(),
    })
  }),

  http.get(`${API_BASE}/cluster/status`, () => {
    return HttpResponse.json(mockClusterStatus)
  }),

  // Settings API
  http.get(`${API_BASE}/settings`, () => {
    return HttpResponse.json(mockSettings)
  }),

  http.put(`${API_BASE}/settings`, async ({ request }) => {
    const updates = await request.json() as Partial<typeof mockSettings.config>
    return HttpResponse.json({
      message: 'Settings updated',
      config: { ...mockSettings.config, ...updates },
    })
  }),

  http.get(`${API_BASE}/settings/providers`, () => {
    return HttpResponse.json({ providers: mockSettings.providers })
  }),

  http.get(`${API_BASE}/settings/providers/:id`, ({ params }) => {
    const runtimeEntry = mockSettings.providers.find(entry => entry.id === params.id)
    if (!runtimeEntry) {
      return HttpResponse.json({ error: { message: 'Runtime catalog entry not found' } }, { status: 404 })
    }
    return HttpResponse.json({
      ...runtimeEntry,
      crdConfig: {
        apiGroup: 'example.ai',
        apiVersion: 'v1alpha1',
        plural: 'runtimedeployments',
        kind: 'RuntimeDeployment',
      },
      installationSteps: [],
      helmRepos: [],
      helmCharts: [],
    })
  }),

  // Installation API
  http.get(`${API_BASE}/installation/helm/status`, () => {
    return HttpResponse.json({
      available: true,
      version: '3.14.0',
    })
  }),

  http.get(`${API_BASE}/installation/providers/:id/status`, ({ params }) => {
    const runtimeEntry = mockSettings.providers.find(entry => entry.id === params.id)
    return HttpResponse.json({
      providerId: params.id,
      providerName: runtimeEntry?.name || 'Runtime',
      installed: true,
      version: '1.0.0',
      crdFound: true,
      operatorRunning: true,
      installationSteps: [],
      helmCommands: [],
    })
  }),

  http.post(`${API_BASE}/installation/providers/:id/install`, () => {
    return HttpResponse.json({
      success: true,
      message: 'Runtime installed successfully',
    })
  }),

  http.post(`${API_BASE}/installation/providers/:id/upgrade`, () => {
    return HttpResponse.json({
      success: true,
      message: 'Runtime upgraded successfully',
    })
  }),

  http.post(`${API_BASE}/installation/providers/:id/uninstall`, () => {
    return HttpResponse.json({
      success: true,
      message: 'Runtime uninstalled successfully',
    })
  }),

  // GPU Operator API
  http.get(`${API_BASE}/installation/gpu-operator/status`, () => {
    return HttpResponse.json({
      installed: true,
      crdFound: true,
      operatorRunning: true,
      gpusAvailable: true,
      totalGPUs: 4,
      gpuNodes: ['gpu-node-1', 'gpu-node-2'],
      message: 'GPU Operator is running',
      helmCommands: [],
    })
  }),

  http.post(`${API_BASE}/installation/gpu-operator/install`, () => {
    return HttpResponse.json({
      success: true,
      message: 'GPU Operator installed successfully',
    })
  }),

  // Gateway CRD Installation API
  http.get(`${API_BASE}/installation/gateway/status`, () => {
    return HttpResponse.json({
      gatewayApiInstalled: true,
      inferenceExtInstalled: true,
      gatewayApiVersion: 'v1.2.1',
      inferenceExtVersion: 'v1.5.0',
      pinnedVersion: 'v1.3.1',
      gatewayAvailable: false,
      message: 'Gateway API and Inference Extension CRDs are installed. No active gateway detected.',
      installCommands: [
        'kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml',
        'kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.3.1/manifests.yaml',
      ],
    })
  }),

  http.post(`${API_BASE}/installation/gateway/install-crds`, () => {
    return HttpResponse.json({
      success: true,
      message: 'Gateway API and Inference Extension CRDs installed successfully',
      results: [
        { step: 'gateway-api-crds', success: true, output: 'created' },
        { step: 'inference-extension-crds', success: true, output: 'created' },
      ],
    })
  }),

  // Gateway resource status (used by useGatewayStatus to drive UI gating)
  http.get(`${API_BASE}/gateway/status`, () => {
    return HttpResponse.json({
      available: false,
    })
  }),

  // HuggingFace OAuth API
  http.get(`${API_BASE}/oauth/huggingface/config`, () => {
    return HttpResponse.json({
      clientId: 'test-client-id',
      authorizeUrl: 'https://huggingface.co/oauth/authorize',
      scopes: ['openid', 'profile', 'read-repos'],
    })
  }),

  http.post(`${API_BASE}/oauth/huggingface/token`, async ({ request }) => {
    const body = await request.json() as { code: string; codeVerifier: string; redirectUri: string }
    if (!body.code || !body.codeVerifier || !body.redirectUri) {
      return HttpResponse.json({ error: { message: 'Missing required fields' } }, { status: 400 })
    }
    return HttpResponse.json({
      accessToken: 'hf_mock_token_123',
      tokenType: 'Bearer',
      expiresIn: 3600,
      scope: 'openid profile read-repos',
      user: {
        id: 'user123',
        name: 'testuser',
        fullname: 'Test User',
        email: 'test@example.com',
        avatarUrl: 'https://huggingface.co/avatars/test.png',
      },
    })
  }),

  // HuggingFace Secrets API
  http.get(`${API_BASE}/secrets/huggingface/status`, () => {
    return HttpResponse.json({
      configured: true,
      namespaces: [
        { name: 'runtime-a-system', exists: true },
        { name: 'runtime-b-system', exists: true },
        { name: 'default', exists: true },
      ],
      user: {
        id: 'user123',
        name: 'testuser',
        fullname: 'Test User',
      },
    })
  }),

  http.post(`${API_BASE}/secrets/huggingface`, async ({ request }) => {
    const body = await request.json() as { accessToken: string }
    if (!body.accessToken) {
      return HttpResponse.json({ error: { message: 'Access token is required' } }, { status: 400 })
    }
    return HttpResponse.json({
      success: true,
      message: 'HuggingFace token saved successfully',
      user: {
        id: 'user123',
        name: 'testuser',
        fullname: 'Test User',
      },
      results: [
        { namespace: 'runtime-a-system', success: true },
        { namespace: 'runtime-b-system', success: true },
        { namespace: 'default', success: true },
      ],
    })
  }),

  http.delete(`${API_BASE}/secrets/huggingface`, () => {
    return HttpResponse.json({
      success: true,
      message: 'HuggingFace secrets deleted successfully',
      results: [
        { namespace: 'runtime-a-system', success: true },
        { namespace: 'runtime-b-system', success: true },
        { namespace: 'default', success: true },
      ],
    })
  }),
]
