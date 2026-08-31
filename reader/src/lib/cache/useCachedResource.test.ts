/**
 * `useCachedResource` 的失效自愈守卫。
 *
 * `store.invalidate` 的文档写的是「删除条目并通知订阅者，**下一次读必然回源**」。
 * 但对一个**已经挂载**的消费者来说没有「下一次读」——校验 effect 只依赖
 * [key, enabled]，失效不会让它重跑。于是条目没了、data undefined、无 error、
 * 无在途 → loading 恒为 true，界面停在骨架屏直到用户切走再切回。
 *
 * 译文长期踩在这个坑里：条目一被打掉，已译好的内容当场消失，而 hasActiveJobs 从
 * 缓存推导，进行中的翻译任务轮询也一并停掉。
 */
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'

import { ok } from '@webtag/api'
import { resourceStore } from './store'
import { useCachedResource } from './useCachedResource'

beforeEach(() => {
  resourceStore.clear()
})

describe('useCachedResource：失效后自动重取', () => {
  it('空缓存的手动重取被取消后退出 loading，并拒绝迟到数据', async () => {
    let release!: (value: ReturnType<typeof ok<{ n: number }>>) => void
    const late = new Promise<ReturnType<typeof ok<{ n: number }>>>((resolve) => {
      release = resolve
    })
    const controller = new AbortController()
    const view = renderHook(() => useCachedResource(
      'GET probe:/cancel-empty',
      () => late,
      { enabled: false },
    ))

    let reload!: ReturnType<typeof view.result.current.reload>
    act(() => {
      reload = view.result.current.reload({ signal: controller.signal })
      controller.abort()
    })
    await act(async () => {
      await expect(reload).resolves.toMatchObject({
        ok: false,
        error: { kind: 'other', message: '资料库刷新已取消' },
      })
    })

    expect(view.result.current.loading).toBe(false)
    expect(view.result.current.revalidating).toBe(false)
    release(ok({ n: 1 }))
    await act(async () => { await late })
    expect(view.result.current.data).toBeUndefined()
    view.unmount()
  })

  it('条目被 invalidate 打掉后自动回源，不会停在永久 loading', async () => {
    let served = 0
    const fetcher = vi.fn(async () => ok({ n: ++served }))
    const view = renderHook(() => useCachedResource('GET probe:/a', fetcher))

    await waitFor(() => expect(view.result.current.data).toEqual({ n: 1 }))
    expect(fetcher).toHaveBeenCalledTimes(1)

    await act(async () => {
      resourceStore.invalidate('GET probe:/a')
    })

    // 自愈：重新取回数据，loading 不会卡住。
    await waitFor(() => expect(view.result.current.data).toEqual({ n: 2 }))
    expect(view.result.current.loading).toBe(false)
    expect(fetcher).toHaveBeenCalledTimes(2)

    view.unmount()
  })

  it('首次挂载不会因为「条目为空」而多发一次请求', async () => {
    // 守的是「挂载只发一次」这个性质本身。注意它**不**区分自愈判据的两种写法：
    // 「当前为 0」在挂载时也成立、会和校验 effect 同时开火，但 store 的在途合并
    // 会把两次并成一个请求，这条照样绿。真正的取舍理由见实现里那段注释。
    const fetcher = vi.fn(async () => ok({ n: 1 }))
    const view = renderHook(() => useCachedResource('GET probe:/b', fetcher))

    await waitFor(() => expect(view.result.current.data).toEqual({ n: 1 }))
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    expect(fetcher).toHaveBeenCalledTimes(1)
    view.unmount()
  })

  it('自愈的那次回源失败也不会陷入无限重取', async () => {
    // 自愈只补一次。失败之后条目仍然是空的，若判据写成「只要空就取」，这里就是
    // 一个反复开火的形状（本实现靠跃迁判据挡住，失败后 previous 已是 0）。
    let calls = 0
    const fetcher = vi.fn(async () => {
      calls += 1
      return calls === 1
        ? ok({ n: 1 })
        : { ok: false as const, error: { kind: 'other' as const, message: 'boom' } }
    })
    const view = renderHook(() => useCachedResource('GET probe:/c', fetcher))

    await waitFor(() => expect(view.result.current.data).toEqual({ n: 1 }))

    await act(async () => {
      resourceStore.invalidate('GET probe:/c')
    })
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20))
    })

    // 一次自愈重取，失败后停手——不是无限重试。
    expect(fetcher).toHaveBeenCalledTimes(2)
    view.unmount()
  })

  // 最凶的一条：失效**落在一次在途请求中间**。
  //
  // 自愈那次回源若不带 force，会被 store 的 in-flight 合并复用那个已经被
  // invalidate 判死的请求——它的 commit 在代际检查处被丢弃，一次 publish 都不
  // 发生，条目永远停在空。而译文有任务时每 1.2 秒静默轮询一次，用户此刻点保存
  // 原文就撞上了，窗口宽度就是一次网络往返。
  it('失效落在在途请求中间时仍然自愈，不被 in-flight 合并吞掉', async () => {
    let served = 0
    let release: (() => void) | null = null
    const fetcher = vi.fn(async () => {
      served += 1
      if (served === 2) {
        // 第二次（后台静默校验）挂住，制造"在途"。
        await new Promise<void>((resolve) => {
          release = resolve
        })
      }
      return ok({ n: served })
    })
    const view = renderHook(() => useCachedResource('GET probe:/e', fetcher))
    await waitFor(() => expect(view.result.current.data).toEqual({ n: 1 }))

    // 后台静默校验起飞并挂住。
    void view.result.current.reload({ silent: true })
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2))

    // 就在它在途时失效。
    await act(async () => {
      resourceStore.invalidate('GET probe:/e')
    })
    // 放行那个已被判死的请求：它的结果会被代际检查丢弃。
    await act(async () => {
      release?.()
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    await waitFor(() => expect(view.result.current.data).toEqual({ n: 3 }))
    expect(view.result.current.loading).toBe(false)

    view.unmount()
  })

  it('initial inflight 期间失效会请求最新 generation，旧响应不得提交', async () => {
    let calls = 0
    let releaseFirst!: () => void
    const firstGate = new Promise<void>((resolve) => {
      releaseFirst = resolve
    })
    const fetcher = vi.fn(async () => {
      const call = ++calls
      if (call === 1) await firstGate
      return ok({ n: call })
    })
    const view = renderHook(() => useCachedResource('GET probe:/initial-race', fetcher))
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1))

    act(() => resourceStore.invalidate('GET probe:/initial-race'))

    await waitFor(() => expect(view.result.current.data).toEqual({ n: 2 }))
    releaseFirst()
    await act(async () => Promise.resolve())
    expect(view.result.current.data).toEqual({ n: 2 })
    expect(resourceStore.peek('GET probe:/initial-race')).toMatchObject({
      desiredGeneration: 1,
      attemptedGeneration: 1,
      settledGeneration: 1,
    })
    view.unmount()
  })

  it('recovery inflight 期间再次失效会继续到最新 generation', async () => {
    let calls = 0
    let releaseRecovery!: () => void
    const recoveryGate = new Promise<void>((resolve) => {
      releaseRecovery = resolve
    })
    const fetcher = vi.fn(async () => {
      const call = ++calls
      if (call === 2) await recoveryGate
      return ok({ n: call })
    })
    const view = renderHook(() => useCachedResource('GET probe:/recovery-race', fetcher))
    await waitFor(() => expect(view.result.current.data).toEqual({ n: 1 }))

    act(() => resourceStore.invalidate('GET probe:/recovery-race'))
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2))
    act(() => resourceStore.invalidate('GET probe:/recovery-race'))

    await waitFor(() => expect(view.result.current.data).toEqual({ n: 3 }))
    releaseRecovery()
    await act(async () => Promise.resolve())
    expect(view.result.current.data).toEqual({ n: 3 })
    expect(resourceStore.peek('GET probe:/recovery-race')).toMatchObject({
      desiredGeneration: 2,
      attemptedGeneration: 2,
      settledGeneration: 2,
    })
    view.unmount()
  })

  it('两个消费者对同一个 generation 只发一次请求', async () => {
    const fetcher = vi.fn(async () => ok({ n: fetcher.mock.calls.length }))
    const first = renderHook(() => useCachedResource('GET probe:/shared', fetcher))
    const second = renderHook(() => useCachedResource('GET probe:/shared', fetcher))
    await waitFor(() => expect(first.result.current.data).toEqual({ n: 1 }))
    expect(second.result.current.data).toEqual({ n: 1 })

    act(() => resourceStore.invalidate('GET probe:/shared'))

    await waitFor(() => expect(first.result.current.data).toEqual({ n: 2 }))
    expect(second.result.current.data).toEqual({ n: 2 })
    expect(fetcher).toHaveBeenCalledTimes(2)
    first.unmount()
    second.unmount()
  })

  it('失败 generation 自动停手，显式 retry 推进 generation 并恢复', async () => {
    let calls = 0
    const fetcher = vi.fn(async () => {
      calls += 1
      if (calls === 2) {
        return { ok: false as const, error: { kind: 'other' as const, message: 'boom' } }
      }
      return ok({ n: calls })
    })
    const view = renderHook(() => useCachedResource('GET probe:/retry', fetcher))
    await waitFor(() => expect(view.result.current.data).toEqual({ n: 1 }))
    act(() => resourceStore.invalidate('GET probe:/retry'))
    await waitFor(() => expect(view.result.current.error?.message).toBe('boom'))
    await act(async () => new Promise((resolve) => setTimeout(resolve, 20)))
    expect(fetcher).toHaveBeenCalledTimes(2)

    await act(async () => {
      await view.result.current.reload()
    })

    expect(view.result.current.data).toEqual({ n: 3 })
    expect(view.result.current.error).toBeNull()
    expect(resourceStore.peek('GET probe:/retry')).toMatchObject({
      desiredGeneration: 2,
      attemptedGeneration: 2,
      settledGeneration: 2,
    })
    view.unmount()
  })

  it('失败 generation 停手后，新 invalidate 会推进 generation 并恢复', async () => {
    let calls = 0
    const fetcher = vi.fn(async () => {
      calls += 1
      if (calls === 2) {
        return { ok: false as const, error: { kind: 'other' as const, message: 'boom' } }
      }
      return ok({ n: calls })
    })
    const key = 'GET probe:/invalidate-after-error'
    const view = renderHook(() => useCachedResource(key, fetcher))
    await waitFor(() => expect(view.result.current.data).toEqual({ n: 1 }))

    act(() => resourceStore.invalidate(key))
    await waitFor(() => expect(view.result.current.error?.message).toBe('boom'))
    await act(async () => new Promise((resolve) => setTimeout(resolve, 20)))
    expect(fetcher).toHaveBeenCalledTimes(2)

    act(() => resourceStore.invalidate(key))

    await waitFor(() => expect(view.result.current.data).toEqual({ n: 3 }))
    expect(view.result.current.error).toBeNull()
    expect(resourceStore.peek(key)).toMatchObject({
      desiredGeneration: 2,
      attemptedGeneration: 2,
      settledGeneration: 2,
    })
    view.unmount()
  })

  it('enabled=false 时不自愈（订阅方自己说了不要取）', async () => {
    const fetcher = vi.fn(async () => ok({ n: 1 }))
    const view = renderHook(() => useCachedResource('GET probe:/d', fetcher, { enabled: false }))

    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })
    await act(async () => {
      resourceStore.invalidate('GET probe:/d')
    })

    expect(fetcher).not.toHaveBeenCalled()
    view.unmount()
  })
})
