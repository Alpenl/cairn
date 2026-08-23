import { act, renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { useVisibilityRefresh } from './useVisibilityRefresh'

function visibility(state: DocumentVisibilityState): void {
  vi.spyOn(document, 'visibilityState', 'get').mockReturnValue(state)
}

function dispatchVisibilityChange(): void {
  act(() => {
    document.dispatchEvent(new Event('visibilitychange'))
  })
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('useVisibilityRefresh', () => {
  it('runs only the latest callback when the document becomes visible', () => {
    visibility('visible')
    const first = vi.fn()
    const second = vi.fn()
    const hook = renderHook(
      ({ callback }) => useVisibilityRefresh(callback),
      { initialProps: { callback: first } },
    )

    dispatchVisibilityChange()
    hook.rerender({ callback: second })
    dispatchVisibilityChange()
    visibility('hidden')
    dispatchVisibilityChange()
    hook.unmount()
    visibility('visible')
    dispatchVisibilityChange()

    expect(first).toHaveBeenCalledTimes(1)
    expect(second).toHaveBeenCalledTimes(1)
  })

  it('does not subscribe while disabled', () => {
    visibility('visible')
    const refresh = vi.fn()
    const hook = renderHook(
      ({ enabled }) => useVisibilityRefresh(refresh, enabled),
      { initialProps: { enabled: false } },
    )

    dispatchVisibilityChange()
    hook.rerender({ enabled: true })
    dispatchVisibilityChange()
    hook.rerender({ enabled: false })
    dispatchVisibilityChange()

    expect(refresh).toHaveBeenCalledTimes(1)
  })
})
