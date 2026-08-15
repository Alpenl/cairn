import { act, renderHook, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { IdentityBoundReaderClient, ReaderClient } from '../lib/api/client'
import { err, ok, type ApiResult } from '../lib/api/result'
import type { PaginatedLinksResponse } from '../lib/api/types'
import { resourceStore } from '../lib/cache/store'
import { readerIdentity } from '../lib/identity'
import { makeLink } from '../test/fixtures'
import { useDomainSummaries } from './useDomainSummaries'
import { useLinks } from './useLinks'
import { useTags } from './useTags'

function bindClient(client: ReaderClient): IdentityBoundReaderClient {
  const lease = readerIdentity.activeLease
  if (!lease) throw new Error('test identity lease is not active')
  Object.defineProperty(client, 'identityLease', {
    configurable: true,
    value: lease,
  })
  return client as IdentityBoundReaderClient
}

function page(id: string): ApiResult<PaginatedLinksResponse> {
  return ok({
    items: [makeLink({ id })],
    total: 1,
    page: 0,
    limit: 30,
  })
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((settle) => {
    resolve = settle
  })
  return { promise, resolve }
}

describe('library collection reload contract', () => {
  it('returns awaitable typed results from the tags and domains hooks', async () => {
    const tagsFailure = err<never>({ kind: 'network-unreachable', message: 'tags offline' })
    const domainsFailure = err<never>({ kind: 'timeout', message: 'domains timed out' })
    const client = bindClient({
      getTags: vi.fn()
        .mockResolvedValueOnce(ok([]))
        .mockResolvedValueOnce(tagsFailure),
      getDomainSummaries: vi.fn()
        .mockResolvedValueOnce(ok({ domains: [], total: 0 }))
        .mockResolvedValueOnce(domainsFailure),
    } as unknown as ReaderClient)

    const tags = renderHook(() => useTags(client))
    const domains = renderHook(() => useDomainSummaries(client))
    await waitFor(() => {
      expect(tags.result.current.loading).toBe(false)
      expect(domains.result.current.loading).toBe(false)
    })

    let tagsReload: unknown
    let domainsReload: unknown
    act(() => {
      tagsReload = tags.result.current.reload()
      domainsReload = domains.result.current.reload()
    })

    expect(tagsReload).toBeInstanceOf(Promise)
    expect(domainsReload).toBeInstanceOf(Promise)
    await act(async () => {
      await expect(tagsReload).resolves.toEqual(tagsFailure)
      await expect(domainsReload).resolves.toEqual(domainsFailure)
    })
  })

  it('cancels a reload result and refuses its late response', async () => {
    const late = deferred<ApiResult<PaginatedLinksResponse>>()
    const getLinks = vi.fn()
      .mockResolvedValueOnce(page('initial'))
      .mockReturnValueOnce(late.promise)
    const client = bindClient({ getLinks } as unknown as ReaderClient)
    const { result } = renderHook(() => useLinks(
      client,
      { type: 'smart', id: 'all', name: '全部' },
    ))
    await waitFor(() => expect(result.current.links.map((link) => link.id)).toEqual(['initial']))

    const controller = new AbortController()
    let reload: unknown
    act(() => {
      reload = result.current.reload({ signal: controller.signal })
      controller.abort()
    })

    await expect(reload).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other', message: '资料库刷新已取消' },
    })

    await act(async () => {
      late.resolve(page('late-cancelled'))
      await late.promise
    })
    expect(result.current.links.map((link) => link.id)).toEqual(['initial'])
  })

  it('reports identity loss and refuses an old identity late response', async () => {
    const late = deferred<ApiResult<PaginatedLinksResponse>>()
    const getLinks = vi.fn()
      .mockResolvedValueOnce(page('identity-a'))
      .mockReturnValueOnce(late.promise)
    const client = bindClient({ getLinks } as unknown as ReaderClient)
    const { result } = renderHook(() => useLinks(
      client,
      { type: 'smart', id: 'all', name: '全部' },
    ))
    await waitFor(() => expect(result.current.links.map((link) => link.id)).toEqual(['identity-a']))

    let reload: unknown
    act(() => {
      reload = result.current.reload()
    })
    act(() => {
      const leaseB = readerIdentity.install({
        serverClientDataNamespace: 'server-B',
        physicalNamespace: 'physical-B',
      })
      resourceStore.activateIdentity(leaseB)
    })

    await act(async () => {
      late.resolve(page('identity-a-late'))
      await late.promise
    })
    await expect(reload).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
    expect(resourceStore.peek('GET /api/links').data).toBeUndefined()
  })
})
