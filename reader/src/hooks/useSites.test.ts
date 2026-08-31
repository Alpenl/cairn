import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useSites } from './useSites'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { ListSitesParams, PaginatedSitesResponse, SiteListItemResponse } from '../lib/api/types'
import { resourceStore } from '../lib/cache/store'
import { err, ok, type ApiResult } from '@webtag/api'
import { IdentityLease } from '../lib/identity'
import type { ReaderCapabilityLease } from '../lib/capabilities'
import { enabledReaderCapabilityLease } from '../test/capabilities'

function makeSite(index: number, prefix = 'site'): SiteListItemResponse {
  const suffix = String(index).padStart(12, '0')
  return {
    id: `00000000-0000-4000-8000-${suffix}`,
    name: `${prefix} ${index}`,
    intro: '',
    display_host: `${prefix}-${index}.example.test`,
    homepage_url: `https://${prefix}-${index}.example.test`,
    icon_url: null,
    tags: [],
    entry_count: 1,
    pinned: false,
    primary_entry: null,
    revision: index,
    first_collected_at: '2026-08-01T00:00:00Z',
    last_collected_at: '2026-08-01T00:00:00Z',
  }
}

function page(
  items: SiteListItemResponse[],
  total: number,
  pageNumber: number,
  recentCutoff?: string,
): ApiResult<PaginatedSitesResponse> {
  return ok({
    items,
    total,
    page: pageNumber,
    limit: 30,
    ...(recentCutoff ? { recent_cutoff: recentCutoff } : {}),
  })
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => { resolve = done })
  return { promise, resolve }
}

function mockClient(
  getSites: (params?: ListSitesParams) => Promise<ApiResult<PaginatedSitesResponse>>,
  lease = identityLease,
): IdentityBoundReaderClient {
  return {
    identityLease: lease,
    getSites: vi.fn(getSites),
    isIdentityCurrent: vi.fn(() => true),
  } as unknown as IdentityBoundReaderClient
}

let testNumber = 0
let identityLease: IdentityLease
let capabilityLease: ReaderCapabilityLease

function makeIdentityLease(prefix: string): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `${prefix}-server`,
    physicalNamespace: `${prefix}-physical`,
    localEpoch: testNumber,
  })
}

function identityCacheKey(baseKey: string, lease = identityLease): string {
  return `${baseKey}#${[
    lease.context.serverClientDataNamespace,
    lease.context.physicalNamespace,
    String(lease.context.localEpoch),
  ].map((part) => encodeURIComponent(part)).join(':')}`
}

beforeEach(() => {
  testNumber += 1
  identityLease = makeIdentityLease(`sites-${testNumber}`)
  resourceStore.clear()
  resourceStore.activateIdentity(identityLease)
  capabilityLease = enabledReaderCapabilityLease()
})

afterEach(() => {
  resourceStore.deactivateIdentity()
})

describe('useSites', () => {
  it('accumulates 30-item pages, deduplicates by site ID, and stops at total', async () => {
    const first = Array.from({ length: 30 }, (_, index) => makeSite(index + 1))
    const second = [first[29], ...Array.from({ length: 29 }, (_, index) => makeSite(index + 31))]
    const client = mockClient(async (params = {}) => (
      params.page === 2 ? page(second, 59, 2) : page(first, 59, 1)
    ))

    const { result } = renderHook(() => useSites(client, capabilityLease, { view: 'all' }))

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.items).toHaveLength(30)
    expect(result.current.hasMore).toBe(true)
    expect(client.getSites).toHaveBeenCalledWith({ view: 'all', page: 1, limit: 30 })
    expect(
      resourceStore.peek<PaginatedSitesResponse>(
        identityCacheKey(`GET /api/sites?view=all&limit=30#capability=${capabilityLease.generation}`),
      ).data?.total,
    ).toBe(59)

    await act(async () => { await result.current.loadMore() })

    expect(result.current.items).toHaveLength(59)
    expect(result.current.items.map((item) => item.id)).toEqual(
      Array.from({ length: 59 }, (_, index) => makeSite(index + 1).id),
    )
    expect(result.current.hasMore).toBe(false)
    expect(client.getSites).toHaveBeenLastCalledWith({ view: 'all', page: 2, limit: 30 })
  })

  it('freezes the first recent cutoff across pages and replaces it on reload', async () => {
    const firstCutoff = '2026-07-11T02:15:30.123456789Z'
    const reloadedCutoff = '2026-07-13T02:15:30.123456789Z'
    const firstWindow = Array.from({ length: 61 }, (_, index) => makeSite(index + 1, 'first-window'))
    const reloadedWindow = Array.from({ length: 31 }, (_, index) => makeSite(index + 1, 'reloaded-window'))
    let firstPageLoads = 0
    const client = mockClient(async (params = {}) => {
      const pageNumber = params.page ?? 1
      if (pageNumber === 1) {
        firstPageLoads += 1
        const source = firstPageLoads === 1 ? firstWindow : reloadedWindow
        const cutoff = firstPageLoads === 1 ? firstCutoff : reloadedCutoff
        return page(source.slice(0, 30), source.length, 1, cutoff)
      }
      const source = params.recentCutoff === firstCutoff ? firstWindow : reloadedWindow
      const start = (pageNumber - 1) * 30
      return page(source.slice(start, start + 30), source.length, pageNumber, params.recentCutoff)
    })
    const { result } = renderHook(() => useSites(client, capabilityLease, { view: 'recent' }))

    await waitFor(() => expect(result.current.items).toHaveLength(30))
    expect(client.getSites).toHaveBeenNthCalledWith(1, { view: 'recent', page: 1, limit: 30 })
    expect(result.current.recentCutoff).toBe(firstCutoff)

    await act(async () => { await result.current.loadMore() })
    await act(async () => { await result.current.loadMore() })
    expect(client.getSites).toHaveBeenNthCalledWith(2, {
      view: 'recent',
      page: 2,
      limit: 30,
      recentCutoff: firstCutoff,
    })
    expect(client.getSites).toHaveBeenNthCalledWith(3, {
      view: 'recent',
      page: 3,
      limit: 30,
      recentCutoff: firstCutoff,
    })
    expect(result.current.items.map((item) => item.name)).toEqual(
      firstWindow.map((item) => item.name),
    )

    await act(async () => { await result.current.reload() })
    expect(client.getSites).toHaveBeenNthCalledWith(4, { view: 'recent', page: 1, limit: 30 })
    expect(result.current.recentCutoff).toBe(reloadedCutoff)
    expect(result.current.items.map((item) => item.name)).toEqual(
      reloadedWindow.slice(0, 30).map((item) => item.name),
    )

    await act(async () => { await result.current.loadMore() })
    expect(client.getSites).toHaveBeenNthCalledWith(5, {
      view: 'recent',
      page: 2,
      limit: 30,
      recentCutoff: reloadedCutoff,
    })
    expect(result.current.items.map((item) => item.name)).toEqual(
      reloadedWindow.map((item) => item.name),
    )
  })

  it('retains accumulated sites when a later page fails and retries that page', async () => {
    const first = Array.from({ length: 30 }, (_, index) => makeSite(index + 1))
    let attempts = 0
    const client = mockClient(async (params = {}) => {
      if (params.page !== 2) return page(first, 31, 1)
      attempts += 1
      return attempts === 1
        ? err({ kind: 'other', message: 'page two failed', status: 503 })
        : page([makeSite(31)], 31, 2)
    })
    const { result } = renderHook(() => useSites(client, capabilityLease, { view: 'all' }))
    await waitFor(() => expect(result.current.items).toHaveLength(30))

    await act(async () => { await result.current.loadMore() })
    expect(result.current.items).toHaveLength(30)
    expect(result.current.pageError?.message).toBe('page two failed')
    expect(result.current.hasMore).toBe(true)

    await act(async () => { await result.current.loadMore() })
    expect(result.current.items).toHaveLength(31)
    expect(result.current.pageError).toBeNull()
    expect(result.current.hasMore).toBe(false)
    expect(attempts).toBe(2)
  })

  it('reloads page one and fences a late next page from the same stream', async () => {
    const oldFirst = Array.from({ length: 30 }, (_, index) => makeSite(index + 1, 'old'))
    const lateNext = deferred<ApiResult<PaginatedSitesResponse>>()
    let firstPageLoads = 0
    const client = mockClient(async (params = {}) => {
      if (params.page === 2) return lateNext.promise
      firstPageLoads += 1
      return firstPageLoads === 1
        ? page(oldFirst, 31, 1)
        : page([makeSite(1, 'fresh')], 1, 1)
    })
    const { result } = renderHook(() => useSites(client, capabilityLease, { view: 'all' }))
    await waitFor(() => expect(result.current.items).toHaveLength(30))

    act(() => { void result.current.loadMore() })
    await waitFor(() => expect(client.getSites).toHaveBeenLastCalledWith({
      view: 'all',
      page: 2,
      limit: 30,
    }))
    await act(async () => { await result.current.reload() })
    expect(result.current.items.map((item) => item.name)).toEqual(['fresh 1'])

    await act(async () => {
      lateNext.resolve(page([makeSite(31, 'old')], 31, 2))
      await lateNext.promise
    })
    expect(result.current.items.map((item) => item.name)).toEqual(['fresh 1'])
  })

  it('clears accumulated pages for a tag change and fences an old late next page', async () => {
    const allFirst = Array.from({ length: 30 }, (_, index) => makeSite(index + 1, 'all'))
    const oldNext = deferred<ApiResult<PaginatedSitesResponse>>()
    const tagged = makeSite(1, 'tagged')
    const client = mockClient(async (params = {}) => {
      if (params.tags === 'research') return page([tagged], 1, 1)
      if (params.page === 2) return oldNext.promise
      return page(allFirst, 31, 1)
    })
    const { result, rerender } = renderHook(
      ({ tags }: { tags?: string }) => useSites(client, capabilityLease, { view: 'all', tags }),
      { initialProps: { tags: undefined } as { tags?: string } },
    )
    await waitFor(() => expect(result.current.items).toHaveLength(30))

    act(() => { void result.current.loadMore() })
    await waitFor(() => expect(client.getSites).toHaveBeenLastCalledWith({
      view: 'all',
      page: 2,
      limit: 30,
    }))

    rerender({ tags: 'research' })
    await waitFor(() => expect(result.current.items.map((item) => item.name)).toEqual(['tagged 1']))

    await act(async () => {
      oldNext.resolve(page([makeSite(31, 'all')], 31, 2))
      await oldNext.promise
    })
    expect(result.current.items.map((item) => item.name)).toEqual(['tagged 1'])
  })

  it('clears accumulated pages on a view change and ignores the old late response', async () => {
    const delayedAll = deferred<ApiResult<PaginatedSitesResponse>>()
    const pinned = makeSite(1, 'pinned')
    const client = mockClient(async (params = {}) => (
      params.view === 'pinned' ? page([pinned], 1, 1) : delayedAll.promise
    ))
    const { result, rerender } = renderHook(
      ({ view }: { view: 'all' | 'pinned' }) => useSites(client, capabilityLease, { view }),
      { initialProps: { view: 'all' } as { view: 'all' | 'pinned' } },
    )
    await waitFor(() => expect(client.getSites).toHaveBeenCalledTimes(1))

    rerender({ view: 'pinned' })
    await waitFor(() => expect(result.current.items.map((item) => item.name)).toEqual(['pinned 1']))

    await act(async () => { delayedAll.resolve(page([makeSite(99, 'all')], 1, 1)) })
    expect(result.current.items.map((item) => item.name)).toEqual(['pinned 1'])
  })

  it('clears data when the identity-owned client changes and fences its late response', async () => {
    const delayedA = deferred<ApiResult<PaginatedSitesResponse>>()
    const clientA = mockClient(async () => delayedA.promise)
    const leaseB = new IdentityLease({
      serverClientDataNamespace: 'sites-identity-b-server',
      physicalNamespace: 'sites-identity-b-physical',
      localEpoch: testNumber + 100,
    })
    const clientB = mockClient(async () => page([makeSite(2, 'identity-b')], 1, 1), leaseB)
    const { result, rerender } = renderHook(
      ({ client }: { client: IdentityBoundReaderClient }) => useSites(client, capabilityLease, { view: 'all' }),
      { initialProps: { client: clientA } },
    )
    await waitFor(() => expect(clientA.getSites).toHaveBeenCalledTimes(1))

    act(() => {
      resourceStore.activateIdentity(leaseB)
      rerender({ client: clientB })
    })
    await waitFor(() => expect(result.current.items.map((item) => item.name)).toEqual(['identity-b 2']))

    await act(async () => { delayedA.resolve(page([makeSite(1, 'identity-a')], 1, 1)) })
    expect(result.current.items.map((item) => item.name)).toEqual(['identity-b 2'])
  })

  it('never renders an old identity appended page while the new first page is pending', async () => {
    const firstA = Array.from({ length: 30 }, (_, index) => makeSite(index + 1, 'identity-a'))
    const delayedB = deferred<ApiResult<PaginatedSitesResponse>>()
    const clientA = mockClient(async (params = {}) => (
      params.page === 2
        ? page([makeSite(31, 'identity-a')], 31, 2)
        : page(firstA, 31, 1)
    ))
    const leaseB = new IdentityLease({
      serverClientDataNamespace: 'sites-render-b-server',
      physicalNamespace: 'sites-render-b-physical',
      localEpoch: testNumber + 100,
    })
    const clientB = mockClient(async () => delayedB.promise, leaseB)
    const renderSnapshots: Array<{ owner: 'a' | 'b'; names: string[] }> = []
    const { result, rerender } = renderHook(
      ({ client }: { client: IdentityBoundReaderClient }) => {
        const sites = useSites(client, capabilityLease, { view: 'all' })
        renderSnapshots.push({
          owner: client === clientA ? 'a' : 'b',
          names: sites.items.map((item) => item.name),
        })
        return sites
      },
      { initialProps: { client: clientA } },
    )
    await waitFor(() => expect(result.current.items).toHaveLength(30))
    await act(async () => { await result.current.loadMore() })
    expect(result.current.items.at(-1)?.name).toBe('identity-a 31')

    const switchRender = renderSnapshots.length
    act(() => {
      resourceStore.activateIdentity(leaseB)
      rerender({ client: clientB })
    })

    expect(
      renderSnapshots
        .slice(switchRender)
        .some(({ owner, names }) => owner === 'b' && names.includes('identity-a 31')),
    ).toBe(false)

    await act(async () => {
      delayedB.resolve(page([makeSite(1, 'identity-b')], 1, 1))
      await delayedB.promise
    })
    await waitFor(() => expect(result.current.items.map((item) => item.name)).toEqual(['identity-b 1']))
  })

  it('does not request or render active cache through an inactive explicit client', async () => {
    const activeClient = mockClient(async () => page([makeSite(1, 'active')], 1, 1))
    const active = renderHook(() => useSites(activeClient, capabilityLease, { view: 'all' }))
    await waitFor(() => expect(active.result.current.items.map((item) => item.name)).toEqual(['active 1']))
    active.unmount()

    const staleLease = new IdentityLease({
      serverClientDataNamespace: 'sites-stale-server',
      physicalNamespace: identityLease.context.physicalNamespace,
      localEpoch: identityLease.context.localEpoch + 100,
    })
    const getStaleSites = vi.fn(async () => page([makeSite(1, 'stale')], 1, 1))
    const staleClient = mockClient(getStaleSites, staleLease)
    const stale = renderHook(() => useSites(staleClient, capabilityLease, { view: 'all' }))

    expect(stale.result.current.items).toEqual([])
    expect(stale.result.current.loading).toBe(false)
    expect(getStaleSites).not.toHaveBeenCalled()
  })

  it('does not request page one, reload, or load more when siteRead is unavailable', async () => {
    const deniedLease = enabledReaderCapabilityLease({
      siteRead: false,
      siteWrite: false,
      siteAdvanced: false,
    })
    const client = mockClient(async () => page([makeSite(1)], 1, 1))
    const { result } = renderHook(() => useSites(client, deniedLease, { view: 'all' }))

    await act(async () => {})
    expect(result.current.loading).toBe(false)
    expect(result.current.items).toEqual([])
    expect(result.current.hasMore).toBe(false)
    expect(client.getSites).not.toHaveBeenCalled()

    await act(async () => {
      await result.current.reload()
      await result.current.loadMore()
    })
    expect(client.getSites).not.toHaveBeenCalled()
  })

  it('drops a late page-one response after the capability lease is revoked', async () => {
    const delayed = deferred<ApiResult<PaginatedSitesResponse>>()
    const client = mockClient(async () => delayed.promise)
    const activeLease = enabledReaderCapabilityLease()
    const deniedLease = enabledReaderCapabilityLease({
      siteRead: false,
      siteWrite: false,
      siteAdvanced: false,
    })
    const staleKey = identityCacheKey(`GET /api/sites?view=all&limit=30#capability=${activeLease.generation}`)
    const { result, rerender } = renderHook(
      ({ lease }: { lease: ReaderCapabilityLease }) => useSites(client, lease, { view: 'all' }),
      { initialProps: { lease: activeLease } },
    )
    await waitFor(() => expect(client.getSites).toHaveBeenCalledTimes(1))

    act(() => {
      activeLease.revoke()
      rerender({ lease: deniedLease })
    })
    expect(result.current.items).toEqual([])
    expect(result.current.loading).toBe(false)

    await act(async () => {
      delayed.resolve(page([makeSite(1, 'stale-capability')], 1, 1))
      await delayed.promise
    })

    expect(result.current.items).toEqual([])
    expect(client.getSites).toHaveBeenCalledTimes(1)
    expect(resourceStore.peek<PaginatedSitesResponse>(staleKey).data).toBeUndefined()
    expect(resourceStore.peek<PaginatedSitesResponse>(staleKey).error?.kind).toBe('identity-mismatch')
  })
})
