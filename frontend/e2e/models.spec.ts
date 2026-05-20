import { test, expect, mockModels } from './fixtures'

test.describe('Models page', () => {
  test('displays model catalog heading', async ({ mockedPage: page }) => {
    await page.goto('/')
    await expect(page.getByText('Model Catalog')).toBeVisible()
    await expect(page.getByText('Browse curated models or search HuggingFace Hub')).toBeVisible()
  })

  test('renders model cards from mock data', async ({ mockedPage: page }) => {
    await page.goto('/')
    for (const model of mockModels) {
      await expect(page.getByText(model.name).first()).toBeVisible()
    }
  })

  test('shows model details on each card', async ({ mockedPage: page }) => {
    await page.goto('/')
    await expect(page.getByText('Qwen3-0.6B').first()).toBeVisible()
    await expect(page.getByText('0.6B').first()).toBeVisible()
    await expect(page.getByText('Small but capable Qwen model')).toBeVisible()
  })

  test('shows engine badges on model cards', async ({ mockedPage: page }) => {
    await page.goto('/')
    const vllmBadges = page.getByText('VLLM')
    await expect(vllmBadges.first()).toBeVisible()
  })

  test('has deploy buttons on model cards', async ({ mockedPage: page }) => {
    await page.goto('/')
    const deployButtons = page.getByRole('button', { name: /Deploy/i })
    await expect(deployButtons.first()).toBeVisible()
  })

  test('deploy button navigates to deploy page', async ({ mockedPage: page }) => {
    await page.goto('/')
    const deployButtons = page.getByRole('button', { name: /Deploy →/i })
    await deployButtons.first().click()
    await expect(page).toHaveURL(/\/deploy\//)
  })

  test('shows curated and HuggingFace tabs', async ({ mockedPage: page }) => {
    await page.goto('/')
    await expect(page.getByText('Curated Models').first()).toBeVisible()
    await expect(page.getByText('HuggingFace Hub').first()).toBeVisible()
  })

  test('shows model count', async ({ mockedPage: page }) => {
    await page.goto('/')
    await expect(page.getByText(/2 of 2 models/)).toBeVisible()
  })
})
