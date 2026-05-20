import { test, expect } from './fixtures'

test.describe('Deploy flow', () => {
  test('shows model info on deploy page', async ({ mockedPage: page }) => {
    await page.goto('/deploy/Qwen%2FQwen3-0.6B')
    await expect(page.getByText('Qwen3-0.6B').first()).toBeVisible()
  })

  test('shows model not found for invalid model', async ({ mockedPage: page }) => {
    await page.goto('/deploy/nonexistent%2Fmodel')
    await expect(page.getByText(/Model not found/i)).toBeVisible()
  })

  test('has back to catalog button', async ({ mockedPage: page }) => {
    await page.goto('/deploy/nonexistent%2Fmodel')
    const backBtn = page.getByRole('button', { name: /Back to Catalog/i })
    await expect(backBtn).toBeVisible()
    await backBtn.click()
    await expect(page).toHaveURL('/')
  })

  test('full flow: model catalog → deploy page', async ({ mockedPage: page }) => {
    await page.goto('/')
    // Click Deploy on first model card
    const deployBtn = page.getByRole('button', { name: /Deploy →/i }).first()
    await deployBtn.click()
    await expect(page).toHaveURL(/\/deploy\//)
    // Should show the model name somewhere on the page
    await expect(page.getByText('Qwen3-0.6B').first()).toBeVisible()
  })
})
