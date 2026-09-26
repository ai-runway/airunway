import { mkdir, writeFile } from 'node:fs/promises'
import { test, expect } from './fixtures'
import { defaultDynamoIntent, toModelDeploymentSpec, type DeploymentConfig } from '@airunway/shared'

// Optional local-browser override avoids downloading a second Chromium build for UI proof.
if (process.env.DYNAMO_UI_CHROMIUM_PATH) test.use({ launchOptions: { executablePath: process.env.DYNAMO_UI_CHROMIUM_PATH } })
const proofDir = process.env.DYNAMO_UI_PROOF_DIR
const runtime = { id: 'dynamo', name: 'Dynamo', installed: true, healthy: true, version: '1.5.0',
  capabilities: { engines: ['vllm', 'sglang', 'trtllm'], modes: ['aggregated', 'disaggregated'], modelSources: ['huggingface'], routerModes: ['default'], features: {} } }

test('automatic creation emits intent without manual sizing', async ({ mockedPage: page }) => {
  let submitted: DeploymentConfig | undefined
  await page.route(/\/api\/(?:installation\/)?runtimes\/status$/, route => route.fulfill({ json: { runtimes: [runtime] } }))
  await page.route(/\/api\/deployments\/-\/pvcs/, route => route.fulfill({ json: { pvcs: [] } }))
  await page.route(/\/api\/deployments\/preview$/, route => {
    const config = route.request().postDataJSON() as DeploymentConfig
    return route.fulfill({ json: { resources: [{ kind: 'ModelDeployment', apiVersion: 'airunway.ai/v1alpha1', name: config.name,
      manifest: { apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: config.name, namespace: config.namespace }, spec: toModelDeploymentSpec(config) } }], primaryResource: { kind: 'ModelDeployment', apiVersion: 'airunway.ai/v1alpha1' } } })
  })
  await page.route(/\/api\/deployments\/?$/, route => {
    if (route.request().method() !== 'POST') return route.fallback()
    submitted = route.request().postDataJSON()
    return route.fulfill({ status: 201, json: { message: 'Created', name: submitted!.name, namespace: submitted!.namespace } })
  })
  await page.goto('/deploy/Qwen%2FQwen3-0.6B')
  await page.getByRole('radio', { name: /Automatic configuration/ }).click()
  await page.getByRole('spinbutton', { name: /GPU budget/ }).fill('2')
  await expect(page.getByText('Deployment Options', { exact: true })).toHaveCount(0)
  await expect(page.getByText('Deployment Mode', { exact: true })).toHaveCount(0)
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  if (proofDir) {
    await mkdir(proofDir, { recursive: true })
    await page.screenshot({ path: `${proofDir}/automatic-create.png`, fullPage: true })
  }
  await page.getByRole('button', { name: /Deploy Model/ }).click()
  await expect.poll(() => submitted).toBeDefined()
  expect(submitted!.modelId).toBe('Qwen/Qwen3-0.6B')
  expect(submitted!.providerOverrides).toMatchObject({ deploymentMode: 'intent', intent: { hardware: { totalGpus: 2 }, searchStrategy: 'rapid' } })
  for (const field of ['resources', 'scaling', 'replicas', 'mode', 'prefillReplicas', 'decodeReplicas', 'prefillGpus', 'decodeGpus']) expect(submitted).not.toHaveProperty(field)
  const spec = toModelDeploymentSpec(submitted!)
  expect(spec).not.toHaveProperty('resources'); expect(spec).not.toHaveProperty('scaling'); expect(spec).not.toHaveProperty('serving')
  if (proofDir) await writeFile(`${proofDir}/automatic-create-payload.json`, JSON.stringify({ submitted, spec }, null, 2))
})

test('failed automatic deployment requires explicit retry/reconfigure confirmation', async ({ mockedPage: page }) => {
  const deployment = { name: 'qwen-auto', namespace: 'models', resourceVersion: '42', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm', mode: 'aggregated', phase: 'Failed', provider: 'dynamo', configurationMode: 'automatic', intent: defaultDynamoIntent(),
    replicas: { desired: 0, ready: 0, available: 0 }, pods: [], createdAt: '2026-09-25T10:00:00Z',
    providerStatus: { intent: { phase: 'Failed', profilingPhase: 'Searching', attempt: 'previous' } }, message: 'No configuration found for the requested hardware.' }
  const writes: unknown[] = []
  await page.route(/\/api\/deployments\/qwen-auto(?:\?.*)?$/, route => route.fulfill({ json: deployment }))
  await page.route(/\/api\/deployments\/models\/qwen-auto\/reconfigure$/, route => {
    writes.push(route.request().postDataJSON())
    return route.fulfill({ json: { message: 'Configuration requested', attempt: `attempt-${writes.length}` } })
  })
  await page.goto('/deployments/qwen-auto?namespace=models')
  await expect(page.getByText('Searching', { exact: true })).toBeVisible()
  await expect(page.getByText('Automatic', { exact: true })).toBeVisible()
  await expect(page.getByRole('spinbutton')).toHaveCount(0)
  await page.getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(page.getByRole('dialog')).toContainText('requests may be interrupted')
  expect(writes).toHaveLength(0)
  if (proofDir) {
    await mkdir(proofDir, { recursive: true })
    await page.screenshot({ path: `${proofDir}/retry-confirmation.png`, fullPage: true })
  }
  await page.getByRole('button', { name: 'Confirm retry' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(writes).toEqual([{ resourceVersion: '42' }])
  await page.getByRole('button', { name: 'Reconfigure', exact: true }).click()
  await page.getByRole('spinbutton', { name: /GPU budget/ }).fill('4')
  await page.getByLabel('Model ID').fill('Qwen/Qwen3-8B')
  const dialogBounds = await page.getByRole('dialog').boundingBox()
  expect(dialogBounds!.x).toBeGreaterThanOrEqual(0)
  expect(dialogBounds!.width).toBeLessThanOrEqual(page.viewportSize()!.width)
  expect(dialogBounds!.height).toBeLessThanOrEqual(page.viewportSize()!.height)
  if (proofDir) await page.screenshot({ path: `${proofDir}/reconfigure-dialog.png`, fullPage: true })
  await page.getByRole('button', { name: 'Confirm reconfiguration' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(writes[1]).toMatchObject({ resourceVersion: '42', modelId: 'Qwen/Qwen3-8B', intent: { hardware: { totalGpus: 4 } } })
  if (proofDir) await writeFile(`${proofDir}/reconfigure-requests.json`, JSON.stringify(writes, null, 2))
})
