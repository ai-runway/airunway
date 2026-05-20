import { test, expect } from './fixtures'

test.describe('Settings page', () => {
  test('renders settings page', async ({ mockedPage: page }) => {
    await page.goto('/settings')
    await expect(page.getByRole('heading', { name: /Settings/i })).toBeVisible()
    await expect(page.locator('main')).toBeVisible()
  })

  test('navigable from sidebar', async ({ mockedPage: page }) => {
    await page.goto('/')
    await page.getByRole('link', { name: 'Settings' }).click()
    await expect(page).toHaveURL('/settings')
  })

  test('/settings?tab=runtimes opens runtimes tab', async ({ mockedPage: page }) => {
    await page.goto('/settings?tab=runtimes')
    await expect(page.locator('main')).toBeVisible()
    await expect(page.getByText(/runtime/i).first()).toBeVisible()
  })
})
