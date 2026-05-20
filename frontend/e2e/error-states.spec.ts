import { test as base, expect } from '@playwright/test'
import { mockApiRoutes } from './fixtures'
import { mockSettings } from '../src/test/mocks/data'

const test = base

/**
 * Helper to set up the __E2E_TEST__ flag for tests that don't use the
 * mockedPage fixture (because they need custom route overrides).
 */
async function setupE2EFlag(page: import('@playwright/test').Page) {
  await page.addInitScript(() => {
    ;(window as any).__E2E_TEST__ = true
  })
}

test.describe('Error states', () => {
  test('shows fallback models when models API fails', async ({ page }) => {
    await setupE2EFlag(page)
    // Register base mocks first, then override specific routes.
    // Playwright matches routes in LIFO order, so later routes take priority.
    await mockApiRoutes(page)
    await page.route(/\/api\/models\/?$/, (route) => {
      return route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: { message: 'Internal server error' } }) })
    })
    await page.goto('/')
    // Fallback models are shown when API is unavailable
    await expect(page.getByText('Model Catalog')).toBeVisible()
    await expect(page.getByText(/of \d+ models/)).toBeVisible()
  })

  test('shows error when deployments API fails', async ({ page }) => {
    await setupE2EFlag(page)
    await mockApiRoutes(page)
    await page.route(/\/api\/deployments\/?$/, (route) => {
      return route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: { message: 'Internal server error' } }) })
    })
    await page.goto('/deployments')
    // With retries disabled, error appears immediately
    await expect(page.getByText(/Failed to load deployments/i)).toBeVisible()
  })
})

test.describe('Auth redirect', () => {
  test('redirects to login when auth is enabled and no token', async ({ page }) => {
    await setupE2EFlag(page)
    const authSettings = { ...mockSettings, auth: { enabled: true } }
    await mockApiRoutes(page)
    // Override settings to enable auth
    await page.route(/\/api\/settings\/?$/, (route) => {
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(authSettings) })
    })
    await page.goto('/')
    await expect(page).toHaveURL('/login')
    await expect(page.getByText('Authentication Required')).toBeVisible()
  })

  test('login page shows CLI instructions', async ({ page }) => {
    await setupE2EFlag(page)
    const authSettings = { ...mockSettings, auth: { enabled: true } }
    await mockApiRoutes(page)
    await page.route(/\/api\/settings\/?$/, (route) => {
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(authSettings) })
    })
    await page.goto('/login')
    await expect(page.getByText('airunway login')).toBeVisible()
    await expect(page.getByText('Paste Token')).toBeVisible()
  })
})

test.describe('Loading states', () => {
  test('shows loading skeleton while models load', async ({ page }) => {
    await setupE2EFlag(page)
    await mockApiRoutes(page)
    // Override models route with a delayed response
    await page.route(/\/api\/models\/?$/, (route) => {
      return new Promise((resolve) => {
        setTimeout(() => {
          resolve(route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ models: [] }) }))
        }, 2000)
      })
    })
    await page.goto('/')
    // Should show skeleton loading grid while waiting for models
    await expect(page.locator('.animate-pulse').first()).toBeVisible()
    // After models load, catalog heading should still be visible
    await expect(page.getByText('Model Catalog')).toBeVisible({ timeout: 5000 })
  })
})
