/**
 * PF9：空闲预取的自我约束。预取不该变成骚扰。
 */
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'

import { ok } from '@webtag/api'
import { resourceStore } from '../lib/cache/store'
import { usePrefetch, type PrefetchTarget } from './usePrefetch'

afterEach(() => {
  resourceStore.clear()
  vi.unstubAllGlobals()
})

function target(key: string, load: () => Promise<unknown>): PrefetchTarget {
  return { key, load }
}

describe('usePrefetch', () => {
  it('空闲时把目标取回缓存', async () => {
    const load = vi.fn(async () => resourceStore.fetch('k1', async () => ok({ id: 'a' })))
    renderHook(() => usePrefetch([target('k1', load)]))

    await waitFor(() => expect(load).toHaveBeenCalledTimes(1))
    expect(resourceStore.has('k1')).toBe(true)
  })

  it('已在缓存里的不重复预取', async () => {
    await resourceStore.fetch('k1', async () => ok({ id: 'a' }))
    const load = vi.fn(async () => undefined)

    renderHook(() => usePrefetch([target('k1', load)]))
    await new Promise((resolve) => setTimeout(resolve, 60))

    expect(load).not.toHaveBeenCalled()
  })

  it('省流量模式下完全不预取', async () => {
    vi.stubGlobal('navigator', { ...navigator, connection: { saveData: true } })
    const load = vi.fn(async () => undefined)

    renderHook(() => usePrefetch([target('k1', load)]))
    await new Promise((resolve) => setTimeout(resolve, 60))

    expect(load).not.toHaveBeenCalled()
  })

  it('弱网（2g）下不预取', async () => {
    vi.stubGlobal('navigator', { ...navigator, connection: { effectiveType: '2g' } })
    const load = vi.fn(async () => undefined)

    renderHook(() => usePrefetch([target('k1', load)]))
    await new Promise((resolve) => setTimeout(resolve, 60))

    expect(load).not.toHaveBeenCalled()
  })

  it('同一时刻最多一个预取在途', async () => {
    let inFlight = 0
    let peak = 0
    const make = (key: string) =>
      target(key, async () => {
        inFlight += 1
        peak = Math.max(peak, inFlight)
        await new Promise((resolve) => setTimeout(resolve, 10))
        inFlight -= 1
      })

    renderHook(() => usePrefetch([make('a'), make('b'), make('c')]))
    await new Promise((resolve) => setTimeout(resolve, 300))

    expect(peak).toBe(1)
  })

  it('卸载后不再继续预取剩余目标', async () => {
    const calls: string[] = []
    const make = (key: string) =>
      target(key, async () => {
        calls.push(key)
        await new Promise((resolve) => setTimeout(resolve, 20))
      })

    const { unmount } = renderHook(() => usePrefetch([make('a'), make('b'), make('c')]))
    await new Promise((resolve) => setTimeout(resolve, 25))
    unmount()
    const afterUnmount = calls.length
    await new Promise((resolve) => setTimeout(resolve, 150))

    expect(calls.length).toBe(afterUnmount)
  })

  it('有用户主动请求在途时让路，不与之抢带宽', async () => {
    // 制造一个一直在途的用户请求。
    let release!: () => void
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    void resourceStore.fetch('user-request', async () => {
      await gate
      return ok({ id: 'u' })
    })

    const load = vi.fn(async () => undefined)
    renderHook(() => usePrefetch([target('k1', load)]))
    // 必须等过 idle 调度的延迟（jsdom 无 requestIdleCallback，降级为 300ms
    // setTimeout）再断言"没被调用"，否则等待不足会让这条断言恒真——去掉让路
    // 逻辑也照样绿。
    await new Promise((resolve) => setTimeout(resolve, 500))

    // 用户请求还在途 —— 预取必须还没开始。
    expect(load).not.toHaveBeenCalled()

    release()
    await new Promise((resolve) => setTimeout(resolve, 600))

    // 用户请求结束后，预取才接上。
    expect(load).toHaveBeenCalledTimes(1)
  })
})
