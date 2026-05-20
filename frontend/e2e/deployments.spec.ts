import { test, expect, mockDeployments } from './fixtures'

test.describe('Deployments page', () => {
  test('displays deployments heading', async ({ mockedPage: page }) => {
    await page.goto('/deployments')
    await expect(page.getByRole('heading', { name: 'Deployments', exact: true })).toBeVisible()
    await expect(page.getByText('Manage your model deployments')).toBeVisible()
  })

  test('shows deployment count', async ({ mockedPage: page }) => {
    await page.goto('/deployments')
    await expect(page.getByText(/2 active/)).toBeVisible()
  })

  test('renders deployment entries', async ({ mockedPage: page }) => {
    await page.goto('/deployments')
    await expect(page.getByText('qwen3-0-6b-vllm-abc123')).toBeVisible()
    await expect(page.getByText('llama-1b-pending-def456')).toBeVisible()
  })

  test('shows replica ready status for deployments', async ({ mockedPage: page }) => {
    await page.goto('/deployments')
    await expect(page.getByText('1/1 ready')).toBeVisible()
    await expect(page.getByText('0/1 ready')).toBeVisible()
  })

  test('shows engine badges', async ({ mockedPage: page }) => {
    await page.goto('/deployments')
    await expect(page.getByText('VLLM').first()).toBeVisible()
    await expect(page.getByText('SGLANG')).toBeVisible()
  })

  test('has New Deployment button that navigates to model catalog', async ({ mockedPage: page }) => {
    await page.goto('/deployments')
    const newBtn = page.getByRole('link', { name: /New Deployment/i })
    await expect(newBtn).toBeVisible()
    await newBtn.click()
    await expect(page).toHaveURL('/')
  })

  test('shows auto-refresh message', async ({ mockedPage: page }) => {
    await page.goto('/deployments')
    await expect(page.getByText(/refreshes automatically/i)).toBeVisible()
  })
})

test.describe('Deployment detail page', () => {
  test('shows deployment details for running deployment', async ({ mockedPage: page }) => {
    await page.goto('/deployments/qwen3-0-6b-vllm-abc123')
    await expect(page.getByText('qwen3-0-6b-vllm-abc123')).toBeVisible()
    await expect(page.getByText('Qwen/Qwen3-0.6B').first()).toBeVisible()
  })

  test('shows not found for non-existent deployment', async ({ mockedPage: page }) => {
    await page.goto('/deployments/does-not-exist')
    // With React Query retries disabled via __E2E_TEST__, 404 is shown immediately
    await expect(page.getByText('Deployment not found')).toBeVisible()
  })

  test('navigating from list to detail works', async ({ mockedPage: page }) => {
    await page.goto('/deployments')
    await page.getByText('qwen3-0-6b-vllm-abc123').click()
    await expect(page).toHaveURL(/\/deployments\/qwen3-0-6b-vllm-abc123/)
  })
})
