import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { err, ok } from '@webtag/api'
import { IdentityLease } from '../../lib/identity'
import { resourceStore } from '../../lib/cache/store'
import type { ReaderEngagementResponse } from '../../lib/api/types'
import { useReadingProgress } from '../../hooks/useReadingProgress'

function response(over: Partial<ReaderEngagementResponse> = {}): ReaderEngagementResponse {
  return {
    link_id: 'L1',
    read: false,
    progress: 0,
    read_later: false,
    last_opened: null,
    updated_at: '2026-08-10T00:00:00Z',
    ...over,
  }
}

function scroller(): HTMLElement {
  return {
    scrollHeight: 1000,
    clientHeight: 100,
    scrollTop: 0,
    scrollTo: vi.fn(),
  } as unknown as HTMLElement
}

let lease: IdentityLease

beforeEach(() => {
  vi.useFakeTimers()
  lease = new IdentityLease({
    serverClientDataNamespace: 'engagement-test-server',
    physicalNamespace: 'engagement-test-physical',
    localEpoch: 1,
  })
  resourceStore.activateIdentity(lease)
})

afterEach(() => {
  vi.useRealTimers()
  resourceStore.deactivateIdentity(lease)
})

describe('useReadingProgress shared engagement', () => {
  it('打开链接写回 read，并恢复服务端保存的进度', async () => {
    const element = scroller()
    const getEngagement = vi.fn(async () => ok(response({ progress: 0.42, read: true })))
    const patchEngagement = vi.fn(async (_id: string, patch: { read?: boolean; progress?: number }) =>
      ok(response({ read: patch.read ?? true, progress: patch.progress ?? 0.42 })))
    const client = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
      getEngagement,
      patchEngagement,
    } as unknown as IdentityBoundReaderClient

    const { result } = renderHook(() => useReadingProgress({
      scrollRef: { current: element },
      sourceKey: 'L1:revision-1',
      layoutKey: 'article',
      engagementLinkID: 'L1',
      readerClient: client,
    }))

    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(result.current.progress).toBe(42)
    expect(getEngagement).toHaveBeenCalledWith('L1')
    expect(patchEngagement).toHaveBeenCalledWith('L1', { read: true })
    expect(element.scrollTop).toBeCloseTo(378)
  })

  it('滚动只更新内存投影，节流后把比例写入同一个 engagement endpoint', async () => {
    const element = scroller()
    const patchEngagement = vi.fn(async (_id: string, patch: { read?: boolean; progress?: number }) =>
      ok(response({ read: patch.read ?? true, progress: patch.progress ?? 0 })))
    const client = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
      getEngagement: vi.fn(async () => ok(response())),
      patchEngagement,
    } as unknown as IdentityBoundReaderClient
    const { result } = renderHook(() => useReadingProgress({
      scrollRef: { current: element },
      sourceKey: 'L1:revision-1',
      layoutKey: 'article',
      engagementLinkID: 'L1',
      readerClient: client,
    }))

    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    patchEngagement.mockClear()
    element.scrollTop = 450
    act(() => result.current.sync())
    expect(result.current.progress).toBe(50)
    expect(patchEngagement).not.toHaveBeenCalled()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(700)
    })
    expect(patchEngagement).toHaveBeenCalledWith('L1', { progress: 0.5 })
    expect(localStorage.length).toBe(0)
  })

  it('旧后端没有 engagement 方法时保留本地进度且不抛错', () => {
    const element = scroller()
    const client = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as IdentityBoundReaderClient
    const { result } = renderHook(() => useReadingProgress({
      scrollRef: { current: element },
      sourceKey: 'L1:revision-1',
      layoutKey: 'article',
      engagementLinkID: 'L1',
      readerClient: client,
    }))

    element.scrollTop = 270
    act(() => result.current.sync())
    expect(result.current.progress).toBe(30)
  })

  it('identity 失效后的迟到读取不会污染当前进度', async () => {
    const element = scroller()
    let resolveGet!: (value: ReturnType<typeof ok<ReaderEngagementResponse>>) => void
    const delayed = new Promise<ReturnType<typeof ok<ReaderEngagementResponse>>>((resolve) => {
      resolveGet = resolve
    })
    let current = true
    const client = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => current),
      getEngagement: vi.fn(() => delayed),
      patchEngagement: vi.fn(async () => err<ReaderEngagementResponse>({ kind: 'identity-mismatch', message: 'stale' })),
    } as unknown as IdentityBoundReaderClient
    const { result } = renderHook(() => useReadingProgress({
      scrollRef: { current: element },
      sourceKey: 'L1:revision-1',
      layoutKey: 'article',
      engagementLinkID: 'L1',
      readerClient: client,
    }))

    current = false
    resolveGet(ok(response({ progress: 0.9 })))
    await delayed
    expect(result.current.progress).toBe(0)
  })
})
