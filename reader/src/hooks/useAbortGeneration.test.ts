import { renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { useAbortGeneration } from './useAbortGeneration'

describe('useAbortGeneration', () => {
  it('aborts the previous controller on key replacement and the current one on unmount', () => {
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
    expect(second.signal.aborted).toBe(true)
  })
})
