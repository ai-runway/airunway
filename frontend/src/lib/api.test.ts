import { beforeEach, describe, expect, it, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { deploymentsApi, modelsApi } from './api'
import { server } from '@/test/mocks/server'

const AIRUNWAY_AUTH_ERROR_HEADER = 'X-Airunway-Auth-Error'

function listenForUnauthorized() {
  const onUnauthorized = vi.fn()
  window.addEventListener('auth:unauthorized', onUnauthorized)

  return {
    onUnauthorized,
    cleanup: () => window.removeEventListener('auth:unauthorized', onUnauthorized),
  }
}

describe('api auth handling', () => {
  beforeEach(() => {
    window.localStorage?.clear()
  })

  it('dispatches auth:unauthorized for Airunway auth 401s', async () => {
    server.use(
      http.get('*/api/models', () =>
        HttpResponse.json(
          { error: { message: 'Authentication required' } },
          {
            status: 401,
            headers: { [AIRUNWAY_AUTH_ERROR_HEADER]: 'true' },
          }
        )
      )
    )

    const { onUnauthorized, cleanup } = listenForUnauthorized()
    try {
      await expect(modelsApi.list()).rejects.toThrow('Authentication required')

      expect(onUnauthorized).toHaveBeenCalledTimes(1)
    } finally {
      cleanup()
    }
  })

  it('does not dispatch auth:unauthorized for upstream 401s without the Airunway auth header', async () => {
    server.use(
      http.get('*/api/models', () =>
        HttpResponse.json(
          { error: { message: 'Upstream provider unauthorized' } },
          { status: 401 }
        )
      )
    )

    const { onUnauthorized, cleanup } = listenForUnauthorized()
    try {
      await expect(modelsApi.list()).rejects.toThrow('Upstream provider unauthorized')

      expect(onUnauthorized).not.toHaveBeenCalled()
    } finally {
      cleanup()
    }
  })

  it('does not dispatch auth:unauthorized for chat upstream 401s without the Airunway auth header', async () => {
    server.use(
      http.post('*/api/deployments/:name/chat', () =>
        HttpResponse.json(
          { error: { message: 'Upstream model provider unauthorized' } },
          { status: 401 }
        )
      )
    )

    const { onUnauthorized, cleanup } = listenForUnauthorized()
    try {
      const response = await deploymentsApi.chat('test-deployment', {
        messages: [{ role: 'user', content: 'hello' }],
      })

      expect(response.status).toBe(401)
      expect(onUnauthorized).not.toHaveBeenCalled()
    } finally {
      cleanup()
    }
  })

  it('dispatches auth:unauthorized for chat Airunway auth 401s', async () => {
    server.use(
      http.post('*/api/deployments/:name/chat', () =>
        HttpResponse.json(
          { error: { message: 'Authentication required' } },
          {
            status: 401,
            headers: { [AIRUNWAY_AUTH_ERROR_HEADER]: 'true' },
          }
        )
      )
    )

    const { onUnauthorized, cleanup } = listenForUnauthorized()
    try {
      const response = await deploymentsApi.chat('test-deployment', {
        messages: [{ role: 'user', content: 'hello' }],
      })

      expect(response.status).toBe(401)
      expect(onUnauthorized).toHaveBeenCalledTimes(1)
    } finally {
      cleanup()
    }
  })
})
