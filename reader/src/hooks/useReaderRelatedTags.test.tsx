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
  it('uses local co-occurrence when the server endpoint is unavailable', () => {
    const link = localLink()
    const corpus = [link, makeLink({ id: 'other', tags: ['local', 'fallback'] })]
    const { result } = renderHook(() => useReaderRelatedTags(link, corpus, makeClient()))

    expect(result.current.source).toBe('local')
    expect(result.current.tags).toEqual(['fallback'])
  })

  it('prefers normalized server results', async () => {
    const response: ReaderRelatedTagsResponse = {
      items: [' server-tag ', 'server-tag', 'second-tag'],
    }
    const getRelatedTags = vi.fn(async () => ok(response))
    const { result } = renderHook(() => useReaderRelatedTags(
      localLink(),
      [],
      makeClient(getRelatedTags),
    ))

    await waitFor(() => expect(result.current.source).toBe('server'))
    expect(result.current.tags).toEqual(['server-tag', 'second-tag'])
    expect(getRelatedTags).toHaveBeenCalledWith(expect.any(String), 12)
  })

  it('sorts equal local scores by tag name', () => {
    const link = localLink()
    const corpus = [
      link,
      makeLink({ id: 'z', tags: ['local', 'zeta'] }),
      makeLink({ id: 'a', tags: ['local', 'alpha'] }),
    ]
    const { result } = renderHook(() => useReaderRelatedTags(link, corpus, makeClient()))

    expect(result.current.tags).toEqual(['alpha', 'zeta'])
  })

  it('falls back to local results when the request fails', async () => {
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
    expect(result.current.tags).toEqual(['fallback'])
    expect(result.current.error?.status).toBe(404)
  })

  it('does not publish a late result after the identity changes', async () => {
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

    const nextLease = new IdentityLease({
      serverClientDataNamespace: 'related-server-B',
      physicalNamespace: 'related-physical-B',
      localEpoch: testNumber + 100,
    })
    resourceStore.activateIdentity(nextLease)
    resolve(ok({ items: ['late-result'] }))
    await delayed

    expect(result.current.source).toBe('local')
    expect(result.current.tags).toEqual(['fallback'])
    resourceStore.deactivateIdentity(nextLease)
  })

  it('does not use an inactive explicit client', () => {
    const staleLease = new IdentityLease({
      serverClientDataNamespace: 'stale-client',
      physicalNamespace: lease.context.physicalNamespace,
      localEpoch: lease.context.localEpoch + 100,
    })
    const getRelatedTags = vi.fn(async () => ok<ReaderRelatedTagsResponse>({
      items: ['stale-only'],
    }))
    const staleClient = {
      identityLease: staleLease,
      isIdentityCurrent: vi.fn(() => true),
      getRelatedTags,
    } as unknown as IdentityBoundReaderClient
    const stale = renderHook(() => useReaderRelatedTags(localLink(), [], staleClient))

    expect(stale.result.current.source).toBe('local')
    expect(stale.result.current.tags).toEqual([])
    expect(getRelatedTags).not.toHaveBeenCalled()
  })
})
