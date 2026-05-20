import { test, expect, mockModels } from './fixtures'

test.describe('Navigation and layout', () => {
  test('renders sidebar with nav links', async ({ mockedPage: page }) => {
    await page.goto('/')
    await expect(page.getByText('AI Runway').first()).toBeVisible()
    await expect(page.getByRole('link', { name: 'Models' })).toBeVisible()
    await expect(page.getByRole('link', { name: 'Deployments' })).toBeVisible()
    await expect(page.getByRole('link', { name: 'Settings' })).toBeVisible()
  })

  test('navigates to Models page (home)', async ({ mockedPage: page }) => {
    await page.goto('/')
    await expect(page.getByText('Model Catalog')).toBeVisible()
  })

  test('navigates to Deployments page', async ({ mockedPage: page }) => {
    await page.goto('/')
    await page.getByRole('link', { name: 'Deployments' }).click()
    await expect(page).toHaveURL('/deployments')
    await expect(page.getByRole('heading', { name: 'Deployments', exact: true })).toBeVisible()
  })

  test('navigates to Settings page', async ({ mockedPage: page }) => {
    await page.goto('/')
    await page.getByRole('link', { name: 'Settings' }).click()
    await expect(page).toHaveURL('/settings')
  })

  test('shows cluster connection status', async ({ mockedPage: page }) => {
    await page.goto('/')
    await expect(page.getByText('Connected', { exact: true }).first()).toBeVisible()
  })

  test('/installation redirects to /settings?tab=runtimes', async ({ mockedPage: page }) => {
    await page.goto('/installation')
    await expect(page).toHaveURL(/\/settings\?tab=runtimes/)
  })
})
