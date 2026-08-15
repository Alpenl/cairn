import 'fake-indexeddb/auto'

import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type {
  IdentityBoundReaderClient,
  ReaderClient,
  ReaderRequestOptions,
} from '../lib/api/client'
import type { ApiResult } from '../lib/api/result'
import type { LinkResponse, PaginatedLinksResponse } from '../lib/api/types'
import { resourceStore } from '../lib/cache/store'
import { IdentityLease, readerIdentity } from '../lib/identity'
import { ownedDatabaseName, ownedStorageKeyForLease } from '../lib/storage-ownership'
import { makeLink as makeLinkFixture } from '../test/fixtures'
import {
  commitAnnotationOperation,
  listAnnotatedLinks,
} from '../lib/user-data/annotation-store'
import type { LegacyStaleAnnotationAddDraft } from '../lib/user-data/annotation-types'
import { resetUserDataDatabaseHandle } from '../lib/user-data/idb'
import {
  linkDetailCacheKey,
  useAnnotatedLinkCount,
  useAnnotatedLinks,
} from './useAnnotatedLinks'

function makeLink(over: Partial<LinkResponse> = {}): LinkResponse {
  return makeLinkFixture({ library_kind: 'reading', ...over })
}

function bindClient(client: ReaderClient, lease: IdentityLease): IdentityBoundReaderClient {
  Object.defineProperty(client, 'identityLease', {
    configurable: true,
    value: lease,
  })
  return client as IdentityBoundReaderClient
}

function dispatchAnnotationHint(
  lease: IdentityLease,
  linkId: string,
  annotationStoreVersion: number,
): void {
  const key = ownedStorageKeyForLease('annotationWakeup', lease)
  if (!key) throw new Error('annotation wakeup key is unavailable')
  window.dispatchEvent(new StorageEvent('storage', {
    key,
    newValue: JSON.stringify({
      kind: 'annotation-change',
      namespace: lease.context.physicalNamespace,
      linkId,
      documentRevision: 0,
      annotationStoreVersion,
    }),
  }))
}

async function settleAnnotationReloads(lease: IdentityLease): Promise<void> {
  const fence = await listAnnotatedLinks(lease)
  expect(fence.ok).toBe(true)
  await act(async () => {
    await Promise.resolve()
    await Promise.resolve()
  })
}

function annotation(id: string): LegacyStaleAnnotationAddDraft {
  return {
    id,
    blockKey: 'content-document',
    start: 0,
    end: 4,
    text: 'text',
    note: '',
    source: 'self',
    createdAt: 100,
    updatedAt: 100,
  }
}

async function addLegacyAnnotation(
  lease: IdentityLease,
  linkId: string,
  operationId = `op-${linkId}`,
): Promise<void> {
  const result = await commitAnnotationOperation(lease, {
    kind: 'add',
    opId: operationId,
    linkId,
    target: { kind: 'legacy-stale', sourceKey: `quarantine:${linkId}` },
    draft: annotation(`annotation-${linkId}`),
  })
  expect(result).toMatchObject({ ok: true, value: { status: 'committed' } })
}

function fakeClient(links: readonly LinkResponse[]) {
  const byId = new Map(links.map((link) => [link.id, link]))
  const getLink = vi.fn(async (
    id: string,
    _options?: ReaderRequestOptions,
  ): Promise<ApiResult<LinkResponse>> => {
    const link = byId.get(id)
    return link
      ? { ok: true, data: link }
      : { ok: false, error: { kind: 'other', message: `missing ${id}` } }
  })
  const getLinks = vi.fn(async (): Promise<ApiResult<PaginatedLinksResponse>> => ({
    ok: true,
    data: { items: links.slice(0, 100), total: 150, page: 1, limit: 100 },
  }))
  return {
    client: bindClient(
      { getLink, getLinks } as unknown as ReaderClient,
      readerIdentity.activeLease!,
    ),
    getLink,
    getLinks,
  }
}

async function deleteUserDataDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(
      request.error ?? new Error('Failed to delete the user-data test database'),
    )
    request.onblocked = () => reject(new Error('User-data test database deletion was blocked'))
  })
}

afterEach(async () => {
  await deleteUserDataDatabase()
})

describe('useAnnotatedLinks complete durable view', () => {
  it('fails closed when the bound client lease does not own the active cache partition', async () => {
    const leaseB = readerIdentity.activeLease!
    const leaseA = new IdentityLease({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: leaseB.context.localEpoch + 1,
    })
    await addLegacyAnnotation(leaseA, 'A-private-link')
    const getLink = vi.fn(async () => ({
      ok: true as const,
      data: makeLink({ id: 'A-private-link', title: 'A private title' }),
    }))
    const clientA = bindClient({ getLink } as unknown as ReaderClient, leaseA)

    // @ts-expect-error the second parameter is only the enabled flag; a B lease cannot be paired
    const rejectedLeaseParameter: Parameters<typeof useAnnotatedLinks>[1] = leaseB
    expect(rejectedLeaseParameter).toBe(leaseB)

    const { result } = renderHook(() => useAnnotatedLinks(clientA))

    await waitFor(() => expect(result.current.error?.kind).toBe('identity-mismatch'))
    expect(getLink).not.toHaveBeenCalled()
    expect(resourceStore.peek(linkDetailCacheKey('A-private-link')).data).toBeUndefined()
    leaseA.revoke()
  })

  it('derives the sidebar count from resolved done Reading links in the durable index', async () => {
    const leaseA = readerIdentity.activeLease!
    const leaseB = new IdentityLease({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
      localEpoch: 2,
    })
    const states = new Map([
      ['A-one', makeLink({ id: 'A-one' })],
      ['A-site', makeLink({ id: 'A-site', library_kind: 'site' })],
      ['A-pending', makeLink({ id: 'A-pending', status: 'pending' })],
      ['B-one', makeLink({ id: 'B-one' })],
    ])
    for (const linkID of ['A-one', 'A-site', 'A-pending']) {
      await addLegacyAnnotation(leaseA, linkID)
    }
    await addLegacyAnnotation(leaseB, 'B-one')
    const makeCountClient = (owner: IdentityLease) => bindClient({
      getLink: vi.fn(async (id: string) => ({ ok: true as const, data: states.get(id)! })),
    } as unknown as ReaderClient, owner)
    const clientA = makeCountClient(leaseA)
    const clientB = makeCountClient(leaseB)
    const { result, rerender } = renderHook(
      ({ client }: { client: IdentityBoundReaderClient }) => useAnnotatedLinkCount(client),
      { initialProps: { client: clientA } },
    )

    await waitFor(() => expect(result.current).toBe(1))
    states.set('A-three', makeLink({ id: 'A-three' }))
    await addLegacyAnnotation(leaseA, 'A-three')
    act(() => window.dispatchEvent(new Event('webtag:annotations-change')))
    await waitFor(() => expect(result.current).toBe(2))

    resourceStore.activateIdentity(leaseB)
    rerender({ client: clientB })
    expect(result.current).toBeUndefined()
    await waitFor(() => expect(result.current).toBe(1))
    leaseB.revoke()
  })

  it('suppresses stale count hints while local and visibility fallbacks force a durable reread', async () => {
    const lease = readerIdentity.activeLease!
    await addLegacyAnnotation(lease, 'first-count-link')
    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
    const client = bindClient({
      getLink: vi.fn(async (id: string) => ({ ok: true as const, data: makeLink({ id }) })),
    } as unknown as ReaderClient, lease)
    const { result } = renderHook(() => useAnnotatedLinkCount(client))
    await waitFor(() => expect(result.current).toBe(1))

    await addLegacyAnnotation(lease, 'second-count-link')
    act(() => dispatchAnnotationHint(lease, 'first-count-link', 1))
    await settleAnnotationReloads(lease)
    expect(result.current).toBe(1)

    act(() => dispatchAnnotationHint(lease, 'second-count-link', 2))
    await waitFor(() => expect(result.current).toBe(2))

    await addLegacyAnnotation(lease, 'third-count-link')
    act(() => dispatchAnnotationHint(lease, 'second-count-link', 2))
    await settleAnnotationReloads(lease)
    expect(result.current).toBe(2)

    act(() => document.dispatchEvent(new Event('visibilitychange')))
    await waitFor(() => expect(result.current).toBe(3))

    await addLegacyAnnotation(lease, 'fourth-count-link')
    act(() => window.dispatchEvent(new Event('webtag:annotations-change')))
    await waitFor(() => expect(result.current).toBe(4))
    visibility.mockRestore()
  })

  it('finds the annotated 150th-oldest link without filtering the latest 100', async () => {
    const lease = readerIdentity.activeLease!
    const links = Array.from({ length: 150 }, (_, index) => makeLink({
      id: `link-${index + 1}`,
      created_at: new Date(Date.UTC(2026, 0, 150 - index)).toISOString(),
    }))
    const oldest = links[149]
    await addLegacyAnnotation(lease, oldest.id)
    const { client, getLink, getLinks } = fakeClient(links)

    const { result } = renderHook(() => useAnnotatedLinks(bindClient(client, lease)))

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links.map((link) => link.id)).toEqual([oldest.id])
    expect(getLink).toHaveBeenCalledTimes(1)
    expect(getLink).toHaveBeenCalledWith(oldest.id, expect.objectContaining({
      signal: expect.any(AbortSignal),
    }))
    expect(getLinks).not.toHaveBeenCalled()
  })

  it('enumerates all 150 indexed links without truncating the durable index', async () => {
    const lease = readerIdentity.activeLease!
    const links = Array.from({ length: 150 }, (_, index) => makeLink({
      id: `indexed-${String(index + 1).padStart(3, '0')}`,
      created_at: new Date(Date.UTC(2026, 0, 1, 0, 0, index)).toISOString(),
    }))
    for (const link of links) await addLegacyAnnotation(lease, link.id)
    const { client, getLink, getLinks } = fakeClient(links)

    const { result } = renderHook(() => useAnnotatedLinks(bindClient(client, lease)))

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links).toHaveLength(150)
    expect(result.current.links.map((link) => link.id)).toContain(links[149].id)
    expect(new Set(getLink.mock.calls.map(([id]) => id))).toEqual(
      new Set(links.map((link) => link.id)),
    )
    expect(getLink).toHaveBeenCalledTimes(150)
    expect(getLinks).not.toHaveBeenCalled()
  })

  it('point-reads only cache misses from a mixed durable index', async () => {
    const lease = readerIdentity.activeLease!
    const cached = makeLink({ id: 'mixed-cached', title: 'Cached title' })
    const missing = makeLink({ id: 'mixed-missing', title: 'Point-read title' })
    await addLegacyAnnotation(lease, cached.id)
    await addLegacyAnnotation(lease, missing.id)
    resourceStore.set(linkDetailCacheKey(cached.id), cached)
    const getLink = vi.fn(async (
      id: string,
      _options?: ReaderRequestOptions,
    ): Promise<ApiResult<LinkResponse>> => ({
      ok: true,
      data: { ...missing, id },
    }))
    const client = { getLink } as unknown as ReaderClient

    const { result } = renderHook(() => useAnnotatedLinks(bindClient(client, lease)))

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links.map((link) => link.id).sort()).toEqual([
      cached.id,
      missing.id,
    ])
    expect(getLink).toHaveBeenCalledTimes(1)
    expect(getLink).toHaveBeenCalledWith(missing.id, expect.objectContaining({
      signal: expect.any(AbortSignal),
    }))
  })

  it('renders a namespace-owned point-cache hit without issuing a point read', async () => {
    const lease = readerIdentity.activeLease!
    const cached = makeLink({ id: 'cached-legacy-link', title: 'Cached title' })
    await addLegacyAnnotation(lease, cached.id)
    resourceStore.set(linkDetailCacheKey(cached.id), cached)
    const getLink = vi.fn(async (): Promise<ApiResult<LinkResponse>> => ({
      ok: true,
      data: makeLink({ id: cached.id, title: 'Unexpected network title' }),
    }))
    const client = { getLink } as unknown as ReaderClient

    const { result } = renderHook(() => useAnnotatedLinks(bindClient(client, lease)))

    await waitFor(() => expect(result.current.links).toEqual([cached]))
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links).toEqual([cached])
    expect(getLink).not.toHaveBeenCalled()
  })

  it('reload point-reads data after its cache entry is explicitly invalidated', async () => {
    const lease = readerIdentity.activeLease!
    const first = makeLink({ id: 'reload-cached-link', title: 'First title' })
    const updated = makeLink({ ...first, title: 'Updated title' })
    await addLegacyAnnotation(lease, first.id)
    const getLink = vi.fn()
      .mockResolvedValueOnce({ ok: true, data: first })
      .mockResolvedValueOnce({ ok: true, data: updated })
    const client = { getLink } as unknown as ReaderClient
    const { result } = renderHook(() => useAnnotatedLinks(bindClient(client, lease)))

    await waitFor(() => expect(result.current.links).toEqual([first]))
    act(() => {
      resourceStore.invalidate(linkDetailCacheKey(first.id))
      void result.current.reload()
    })
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(result.current.links).toEqual([updated]))
  })

  it('cancels an annotated reload promptly, keeps visible data, and ignores its late point read', async () => {
    const lease = readerIdentity.activeLease!
    const first = makeLink({ id: 'cancel-annotated-link', title: 'Visible title' })
    const late = makeLink({ ...first, title: 'Late title' })
    await addLegacyAnnotation(lease, first.id)
    let resolveLate!: (result: ApiResult<LinkResponse>) => void
    const lateRead = new Promise<ApiResult<LinkResponse>>((resolve) => {
      resolveLate = resolve
    })
    const getLink = vi.fn()
      .mockResolvedValueOnce({ ok: true, data: first })
      .mockReturnValueOnce(lateRead)
    const client = bindClient({ getLink } as unknown as ReaderClient, lease)
    const { result } = renderHook(() => useAnnotatedLinks(client))
    await waitFor(() => expect(result.current.links).toEqual([first]))

    const controller = new AbortController()
    let reload!: ReturnType<typeof result.current.reload>
    act(() => {
      resourceStore.invalidate(linkDetailCacheKey(first.id))
      reload = result.current.reload({ signal: controller.signal })
    })
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(2))
    expect(result.current.loading).toBe(true)
    act(() => controller.abort())

    await act(async () => {
      await expect(reload).resolves.toMatchObject({
        ok: false,
        error: { kind: 'other', message: '资料库刷新已取消' },
      })
    })
    expect(result.current).toMatchObject({ links: [first], loading: false, error: null })

    await act(async () => {
      resolveLate({ ok: true, data: late })
      await lateRead
      await Promise.resolve()
    })
    expect(result.current.links).toEqual([first])
    expect(resourceStore.peek(linkDetailCacheKey(first.id)).data).toBeUndefined()
  })

  it('settles a same-tick superseded reload instead of leaving its waiter pending', async () => {
    const lease = readerIdentity.activeLease!
    const link = makeLink({ id: 'superseded-annotated-link' })
    await addLegacyAnnotation(lease, link.id)
    resourceStore.set(linkDetailCacheKey(link.id), link)
    const client = bindClient({ getLink: vi.fn() } as unknown as ReaderClient, lease)
    const { result } = renderHook(() => useAnnotatedLinks(client))
    await waitFor(() => expect(result.current.links).toEqual([link]))

    let firstReload!: ReturnType<typeof result.current.reload>
    let secondReload!: ReturnType<typeof result.current.reload>
    act(() => {
      firstReload = result.current.reload()
      secondReload = result.current.reload()
    })

    await expect(firstReload).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other', message: '资料库刷新已取消' },
    })
    await act(async () => {
      await expect(secondReload).resolves.toMatchObject({ ok: true, data: [link] })
    })
  })

  it('uses per-link version high-water for hints but always honors visibility recovery', async () => {
    const lease = readerIdentity.activeLease!
    const link = makeLink({ id: 'hinted-link' })
    await addLegacyAnnotation(lease, link.id)
    const { client, getLink } = fakeClient([link])
    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
    renderHook(() => useAnnotatedLinks(bindClient(client, lease)))
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(1))

    act(() => dispatchAnnotationHint(lease, link.id, 1))
    await settleAnnotationReloads(lease)
    expect(getLink).toHaveBeenCalledTimes(1)

    act(() => {
      resourceStore.invalidate(linkDetailCacheKey(link.id))
      dispatchAnnotationHint(lease, link.id, 3)
    })
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(2))

    act(() => {
      dispatchAnnotationHint(lease, link.id, 2)
      dispatchAnnotationHint(lease, link.id, 3)
    })
    await settleAnnotationReloads(lease)
    expect(getLink).toHaveBeenCalledTimes(2)

    act(() => {
      resourceStore.invalidate(linkDetailCacheKey(link.id))
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(3))
    visibility.mockRestore()
  })

  it('keeps readable annotations visible when one indexed link no longer resolves', async () => {
    const lease = readerIdentity.activeLease!
    const readable = [makeLink({ id: 'readable-A' }), makeLink({ id: 'readable-B' })]
    const orphanId = 'deleted-orphan'
    for (const linkId of [readable[0].id, orphanId, readable[1].id]) {
      await addLegacyAnnotation(lease, linkId)
    }
    const byId = new Map(readable.map((link) => [link.id, link]))
    const getLink = vi.fn(async (id: string): Promise<ApiResult<LinkResponse>> => {
      const link = byId.get(id)
      return link
        ? { ok: true, data: link }
        : { ok: false, error: { kind: 'other', message: 'HTTP 404', status: 404 } }
    })
    const client = { getLink } as unknown as ReaderClient

    const { result } = renderHook(() => useAnnotatedLinks(bindClient(client, lease)))

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links.map((link) => link.id).sort()).toEqual(['readable-A', 'readable-B'])
    expect(result.current.error).toBeNull()
    expect(getLink).toHaveBeenCalledTimes(3)
  })

  it('preserves the error state for a non-404 point-read failure', async () => {
    const lease = readerIdentity.activeLease!
    const links = [makeLink({ id: 'before-error' }), makeLink({ id: 'after-error' })]
    const failedId = 'server-failure'
    for (const linkId of [links[0].id, failedId, links[1].id]) {
      await addLegacyAnnotation(lease, linkId)
    }
    const byId = new Map(links.map((link) => [link.id, link]))
    const failure = { kind: 'other' as const, message: 'HTTP 503', status: 503 }
    const getLink = vi.fn(async (id: string): Promise<ApiResult<LinkResponse>> => {
      const link = byId.get(id)
      return link ? { ok: true, data: link } : { ok: false, error: failure }
    })
    const client = { getLink } as unknown as ReaderClient

    const { result } = renderHook(() => useAnnotatedLinks(bindClient(client, lease)))

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links).toEqual([])
    expect(result.current.error).toEqual(failure)
    expect(getLink).toHaveBeenCalledTimes(3)
  })

  it('runs at most six point reads and starts queued work only as a slot opens', async () => {
    const lease = readerIdentity.activeLease!
    const links = Array.from({ length: 8 }, (_, index) => makeLink({ id: `queued-${index}` }))
    for (const link of links) await addLegacyAnnotation(lease, link.id)
    const byId = new Map(links.map((link) => [link.id, link]))
    const releases = new Map<string, () => void>()
    let active = 0
    let maximumActive = 0
    const getLink = vi.fn((id: string, options?: ReaderRequestOptions) =>
      new Promise<ApiResult<LinkResponse>>((resolve) => {
        active += 1
        maximumActive = Math.max(maximumActive, active)
        let settled = false
        const finish = (result: ApiResult<LinkResponse>) => {
          if (settled) return
          settled = true
          active -= 1
          resolve(result)
        }
        releases.set(id, () => finish({ ok: true, data: byId.get(id)! }))
        options?.signal?.addEventListener('abort', () => finish({
          ok: false,
          error: { kind: 'timeout', message: 'aborted' },
        }), { once: true })
      }))
    const client = { getLink } as unknown as ReaderClient

    const { result } = renderHook(() => useAnnotatedLinks(bindClient(client, lease)))

    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(6), { timeout: 500 })
    expect(maximumActive).toBe(6)
    act(() => releases.get(getLink.mock.calls[0][0])?.())
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(7))
    expect(maximumActive).toBe(6)

    act(() => {
      for (const release of releases.values()) release()
    })
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(8))
    act(() => {
      for (const release of releases.values()) release()
    })
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links).toHaveLength(8)
    expect(maximumActive).toBe(6)
  })

  it('aborts in-flight reads and never starts queued IDs after the selection changes', async () => {
    const lease = readerIdentity.activeLease!
    const links = Array.from({ length: 8 }, (_, index) => makeLink({ id: `selection-${index}` }))
    for (const link of links) await addLegacyAnnotation(lease, link.id)
    const signals: AbortSignal[] = []
    const releases = new Map<string, (result: ApiResult<LinkResponse>) => void>()
    const getLink = vi.fn((id: string, options?: ReaderRequestOptions) =>
      new Promise<ApiResult<LinkResponse>>((resolve) => {
        const signal = options?.signal
        expect(signal).toBeDefined()
        signals.push(signal!)
        releases.set(id, resolve)
      }))
    const client = { getLink } as unknown as ReaderClient

    const { result, rerender } = renderHook(
      ({ enabled }: { enabled: boolean }) => useAnnotatedLinks(bindClient(client, lease), enabled),
      { initialProps: { enabled: true } },
    )
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(6))

    rerender({ enabled: false })

    expect(signals).toHaveLength(6)
    expect(signals.every((signal) => signal.aborted)).toBe(true)
    expect(result.current).toMatchObject({ links: [], loading: false, error: null })
    await act(async () => {
      for (const [id, release] of releases) {
        release({ ok: true, data: links.find((link) => link.id === id)! })
      }
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(getLink).toHaveBeenCalledTimes(6)
    expect(result.current).toMatchObject({ links: [], loading: false, error: null })
    for (const link of links) {
      expect(resourceStore.peek(linkDetailCacheKey(link.id)).data).toBeUndefined()
    }
  })

  it('switches namespace atomically and aborts the previous IdentityLease work', async () => {
    const leaseA = new IdentityLease({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: 1,
    })
    const leaseB = new IdentityLease({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
      localEpoch: 2,
    })
    const linksA = Array.from({ length: 7 }, (_, index) => makeLink({ id: `A-${index}` }))
    const linkB = makeLink({ id: 'B-only' })
    for (const link of linksA) await addLegacyAnnotation(leaseA, link.id)
    await addLegacyAnnotation(leaseB, linkB.id)
    const signalsA: AbortSignal[] = []
    const getLink = vi.fn((id: string, options?: ReaderRequestOptions): Promise<ApiResult<LinkResponse>> => {
      if (id === linkB.id) return Promise.resolve({ ok: true, data: linkB })
      return new Promise((resolve) => {
        signalsA.push(options!.signal!)
        options?.signal?.addEventListener('abort', () => resolve({
          ok: false,
          error: { kind: 'timeout', message: 'A aborted' },
        }), { once: true })
      })
    })
    const client = { getLink } as unknown as ReaderClient

    resourceStore.activateIdentity(leaseA)

    const { result, rerender } = renderHook(
      ({ lease }: { lease: IdentityLease }) => useAnnotatedLinks(bindClient(client, lease)),
      { initialProps: { lease: leaseA } },
    )
    await waitFor(() => expect(signalsA).toHaveLength(6))

    resourceStore.activateIdentity(leaseB)
    rerender({ lease: leaseB })

    expect(signalsA.every((signal) => signal.aborted)).toBe(true)
    expect(result.current.links).toEqual([])
    await waitFor(() => expect(result.current.links.map((link) => link.id)).toEqual([linkB.id]))
    expect(getLink.mock.calls.filter(([id]) => String(id).startsWith('A-'))).toHaveLength(6)
  })

  it('re-enumerates the durable index when a missed-change page becomes visible', async () => {
    const lease = readerIdentity.activeLease!
    const late = makeLink({ id: 'late-quarantine-link' })
    const { client, getLink } = fakeClient([late])
    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
    const { result } = renderHook(() => useAnnotatedLinks(bindClient(client, lease)))
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links).toEqual([])

    await addLegacyAnnotation(lease, late.id)
    act(() => document.dispatchEvent(new Event('visibilitychange')))

    await waitFor(() => expect(result.current.links.map((link) => link.id)).toEqual([late.id]))
    expect(getLink).toHaveBeenCalledTimes(1)
    visibility.mockRestore()
  })
})
