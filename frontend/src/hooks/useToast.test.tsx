import type { DependencyList, EffectCallback } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const effectLifecycle = vi.hoisted(() => ({ runs: 0, cleanups: 0 }))

vi.mock('react', async (importOriginal) => {
  const actual = await importOriginal<typeof import('react')>()
  return {
    ...actual,
    useEffect(effect: EffectCallback, deps?: DependencyList) {
      actual.useEffect(() => {
        effectLifecycle.runs += 1
        const cleanup = effect()
        return () => {
          effectLifecycle.cleanups += 1
          if (typeof cleanup === 'function') cleanup()
        }
      }, deps)
    },
  }
})

import { act, renderHook, waitFor } from '@testing-library/react'
import { useToast } from './useToast'

describe('useToast', () => {
  beforeEach(() => {
    effectLifecycle.runs = 0
    effectLifecycle.cleanups = 0
  })

  it('keeps one listener subscription across toast state updates', async () => {
    const { result, unmount } = renderHook(() => useToast())

    expect(effectLifecycle.runs).toBe(1)

    act(() => {
      result.current.toast({ title: 'First toast' })
    })
    await waitFor(() => expect(result.current.toasts[0]?.title).toBe('First toast'))

    act(() => {
      result.current.toast({ title: 'Second toast' })
    })
    await waitFor(() => expect(result.current.toasts[0]?.title).toBe('Second toast'))

    expect(effectLifecycle.runs).toBe(1)
    expect(effectLifecycle.cleanups).toBe(0)

    unmount()
    expect(effectLifecycle.cleanups).toBe(1)
  })
})
