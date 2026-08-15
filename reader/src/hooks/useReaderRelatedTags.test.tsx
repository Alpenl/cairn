import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { err, ok } from '../lib/api/result'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { ReaderRelatedTagsResponse } from '../lib/api/types'
import { IdentityLease } from '../lib/identity'
import { resourceStore } from '../lib/cache/store'
import { makeLink } from '../test/fixtures'
import { useReaderRelatedTags } from './useReaderRelatedTags'

let lease: IdentityLease
let testNumber = 0

function makeClient(
  getRelatedTags?: IdentityBoundReaderClient['getRelatedTags'],
): IdentityBoundReaderClient {
  return {
    identityLease: lease,
    isIdentityCurrent: vi.fn(() => true),
    ...(getRelatedTags ? { getRelatedTags } : {}),
  } as unknown as IdentityBoundReaderClient
}

function localLink() {
  return makeLink({ id: `related-${testNumber}`, tags: ['local'] })
}

beforeEach(() => {
  testNumber += 1
  lease = new IdentityLease({
    serverClientDataNamespace: `related-server-${testNumber}`,
    physicalNamespace: `related-physical-${testNumber}`,
    localEpoch: testNumber,
  })
  resourceStore.activateIdentity(lease)
})

afterEach(() => {
  resourceStore.deactivateIdentity(lease)
})

describe('useReaderRelatedTags', () => {
  it('旧后端没有 related-tags 能力时保留本地共现降级', () => {
    const link = localLink()
    const corpus = [
      link,
      makeLink({ id: 'other', tags: ['local', 'fallback'] }),
    ]
    const { result } = renderHook(() => useReaderRelatedTags(link, corpus, makeClient()))

    expect(result.current.source).toBe('local')
    expect(result.current.mode).toBe('local')
    expect(result.current.degraded).toBe(true)
    expect(result.current.model).toBeNull()
    expect(result.current.tags).toEqual(['fallback'])
  })

  it('server 成功时优先使用权威标签，即使服务端标记 degraded', async () => {
    const response: ReaderRelatedTagsResponse = {
      items: [' semantic-tag ', 'semantic-tag', 'second-tag'],
      model: 'cooccurrence-fallback',
      degraded: true,
    }
    const getRelatedTags = vi.fn(async () => ok(response))
    const { result } = renderHook(() => useReaderRelatedTags(
      localLink(),
      [],
      makeClient(getRelatedTags),
    ))

    await waitFor(() => expect(result.current.source).toBe('server'))
    expect(result.current.tags).toEqual(['semantic-tag', 'second-tag'])
    expect(result.current.mode).toBe('cooccurrence')
    expect(result.current.model).toBe('cooccurrence-fallback')
    expect(result.current.degraded).toBe(true)
    expect(getRelatedTags).toHaveBeenCalledWith(expect.any(String), 12)
  })

  it('即使旧服务端漏报 degraded，也不会把 cooccurrence 模型显示为完整语义结果', async () => {
    const getRelatedTags = vi.fn(async () => ok<ReaderRelatedTagsResponse>({
      items: ['fallback-tag'],
      model: 'cooccurrence-v1',
      degraded: false,
    }))
    const { result } = renderHook(() => useReaderRelatedTags(
      localLink(),
      [],
      makeClient(getRelatedTags),
    ))

    await waitFor(() => expect(result.current.source).toBe('server'))
    expect(result.current.model).toBe('cooccurrence-v1')
    expect(result.current.mode).toBe('cooccurrence')
    expect(result.current.degraded).toBe(true)
  })

  it('缺少模型代次时保留服务端标签但明确标记降级', async () => {
    const getRelatedTags = vi.fn(async () => ok<ReaderRelatedTagsResponse>({
      items: ['unversioned-tag'],
      model: '   ',
      degraded: false,
    }))
    const { result } = renderHook(() => useReaderRelatedTags(
      localLink(),
      [],
      makeClient(getRelatedTags),
    ))

    await waitFor(() => expect(result.current.source).toBe('server'))
    expect(result.current.tags).toEqual(['unversioned-tag'])
    expect(result.current.model).toBeNull()
    expect(result.current.mode).toBe('cooccurrence')
    expect(result.current.degraded).toBe(true)
  })

  it('本地共现同分时按标签名稳定排序，而不是按 corpus 插入顺序漂移', () => {
    const link = localLink()
    const corpus = [
      link,
      makeLink({ id: 'z', tags: ['local', 'zeta'] }),
      makeLink({ id: 'a', tags: ['local', 'alpha'] }),
    ]
    const { result } = renderHook(() => useReaderRelatedTags(link, corpus, makeClient()))

    expect(result.current.tags).toEqual(['alpha', 'zeta'])
  })

  it('请求错误时回退本地结果，不把失败响应当成空标签', async () => {
    const link = localLink()
    const getRelatedTags = vi.fn(async () => err<ReaderRelatedTagsResponse>({
      kind: 'other',
      status: 404,
      message: 'old backend',
    }))
    const { result } = renderHook(() => useReaderRelatedTags(
      link,
      [link, makeLink({ id: 'other', tags: ['local', 'fallback'] })],
      makeClient(getRelatedTags),
    ))

    await waitFor(() => expect(getRelatedTags).toHaveBeenCalled())
    expect(result.current.source).toBe('local')
    expect(result.current.degraded).toBe(true)
    expect(result.current.mode).toBe('local')
    expect(result.current.tags).toEqual(['fallback'])
    expect(result.current.error?.status).toBe(404)
  })

  it('identity 切换后不提交旧租户的迟到标签', async () => {
    let resolve!: (value: ReturnType<typeof ok<ReaderRelatedTagsResponse>>) => void
    const delayed = new Promise<ReturnType<typeof ok<ReaderRelatedTagsResponse>>>((release) => {
      resolve = release
    })
    const getRelatedTags = vi.fn(() => delayed)
    const link = localLink()
    const { result } = renderHook(() => useReaderRelatedTags(
      link,
      [link, makeLink({ id: 'other', tags: ['local', 'fallback'] })],
      makeClient(getRelatedTags),
    ))

    const leaseB = new IdentityLease({
      serverClientDataNamespace: 'related-server-B',
      physicalNamespace: 'related-physical-B',
      localEpoch: testNumber + 100,
    })
    resourceStore.activateIdentity(leaseB)
    resolve(ok({ items: ['late-result'], model: 'semantic', degraded: false }))
    await delayed

    expect(result.current.source).toBe('local')
    expect(result.current.tags).toEqual(['fallback'])
    resourceStore.deactivateIdentity(leaseB)
  })

  it('过期显式 client 不读取活跃身份的同 namespace 缓存', async () => {
    const link = localLink()
    const getActiveRelatedTags = vi.fn(async () => ok<ReaderRelatedTagsResponse>({
      items: ['active-only'],
      model: 'semantic-v1:test',
      degraded: false,
    }))
    const active = renderHook(() => useReaderRelatedTags(
      link,
      [],
      makeClient(getActiveRelatedTags),
    ))
    await waitFor(() => expect(active.result.current.source).toBe('server'))
    active.unmount()

    const staleLease = new IdentityLease({
      serverClientDataNamespace: 'stale-client',
      physicalNamespace: lease.context.physicalNamespace,
      localEpoch: lease.context.localEpoch + 100,
    })
    const getStaleRelatedTags = vi.fn(async () => ok<ReaderRelatedTagsResponse>({
      items: ['stale-only'],
      model: 'semantic-v1:stale',
      degraded: false,
    }))
    const staleClient = {
      identityLease: staleLease,
      isIdentityCurrent: vi.fn(() => true),
      getRelatedTags: getStaleRelatedTags,
    } as unknown as IdentityBoundReaderClient
    const stale = renderHook(() => useReaderRelatedTags(link, [], staleClient))

    expect(stale.result.current.source).toBe('local')
    expect(stale.result.current.tags).toEqual([])
    expect(getStaleRelatedTags).not.toHaveBeenCalled()
  })
})
