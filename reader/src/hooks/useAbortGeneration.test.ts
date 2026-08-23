import { renderHook } from '@testing-library/react'
import { createElement, StrictMode, type ReactNode } from 'react'
import { describe, expect, it } from 'vitest'

import { useAbortGeneration } from './useAbortGeneration'

describe('useAbortGeneration', () => {
  it('aborts the previous controller on key replacement and the current one on unmount', async () => {
    const hook = renderHook(
      ({ key }) => useAbortGeneration(key),
      { initialProps: { key: 'one' } },
    )
    const first = hook.result.current.controller

    expect(first.signal.aborted).toBe(false)
    hook.rerender({ key: 'two' })
    const second = hook.result.current.controller

    expect(first.signal.aborted).toBe(true)
    expect(second).not.toBe(first)
    expect(second.signal.aborted).toBe(false)

    hook.unmount()
    await Promise.resolve()
    expect(second.signal.aborted).toBe(true)
  })

  it('keeps the committed controller usable after root StrictMode effect replay', async () => {
    const wrapper = ({ children }: { children: ReactNode }) => createElement(StrictMode, null, children)
    const hook = renderHook(() => useAbortGeneration('strict'), { wrapper })

    await Promise.resolve()

    expect(hook.result.current.controller.signal.aborted).toBe(false)
    hook.unmount()
    await Promise.resolve()
    expect(hook.result.current.controller.signal.aborted).toBe(true)
  })
})
