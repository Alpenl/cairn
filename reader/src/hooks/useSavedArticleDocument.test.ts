import { act, renderHook, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { ok, type ApiResult } from '@webtag/api'
import type { LinkContentResponse, LinkResponse } from '../lib/api/types'
import { IdentityLease } from '../lib/identity'
import { useSavedArticleDocument } from './useSavedArticleDocument'

function lease(namespace = 'physical-A'): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `server:${namespace}`,
    physicalNamespace: namespace,
    localEpoch: 1,
  })
}

function detail(revision: number, overrides: Partial<LinkResponse> = {}): LinkResponse {
  return {
    id: 'L1',
    title: 'Article',
    has_content: true,
    content_revision: revision,
    ...overrides,
  } as LinkResponse
}

function body(revision: number, content: string): LinkContentResponse {
  return {
    link_id: 'L1',
    content,
    content_format: 'plain',
    content_revision: revision,
    fetcher_type: 'stored',
    content_source: 'fetched',
  }
}

describe('useSavedArticleDocument', () => {
  it('retains one controller across revisions and aborts the old generation', async () => {
    const owner = lease()
    const load = vi.fn(async () => ok(body(7, 'seven')))
    const { result, rerender } = renderHook(
      ({ current }) => useSavedArticleDocument({ lease: owner, detail: current, loadBody: load }),
      { initialProps: { current: detail(7) } },
    )
    await waitFor(() => expect(result.current.document?.id.contentRevision).toBe(7))
    const controller = result.current.controller
    const oldContext = result.current.captureContext()

    rerender({ current: detail(8) })

    await waitFor(() => expect(result.current.document?.id.contentRevision).toBe(8))
    expect(result.current.controller).toBe(controller)
    expect(oldContext?.signal.aborted).toBe(true)
    expect(result.current.document).toMatchObject({
      body: { status: 'idle' },
      annotations: { status: 'idle' },
      translations: { status: 'idle' },
    })
  })

  it('adopts the actual newer endpoint revision without relabelling old detail metadata', async () => {
    const owner = lease()
    const load = vi.fn(async () => ok(body(8, 'eight')))
    const { result } = renderHook(() =>
      useSavedArticleDocument({ lease: owner, detail: detail(7), loadBody: load }),
    )
    await waitFor(() => expect(result.current.document).not.toBeNull())

    await act(async () => {
      await result.current.loadBody()
    })

    expect(load).toHaveBeenCalledWith(expect.objectContaining({
      id: expect.objectContaining({ contentRevision: 7 }),
    }))
    expect(result.current.document).toMatchObject({
      id: { namespace: 'physical-A', linkId: 'L1', contentRevision: 8 },
      detail: { status: 'idle' },
      body: { status: 'ready', revision: 8, data: { content: 'eight' } },
    })
  })

  it('advances a revision floor without treating older fallback metadata as current detail', async () => {
    const owner = lease()
    const load = vi.fn(async () => ok(body(8, 'eight')))
    const { result, rerender } = renderHook(
      ({ current, floor }) => useSavedArticleDocument({
        lease: owner,
        detail: current,
        revisionFloor: floor,
        loadBody: load,
      }),
      {
        initialProps: {
          current: detail(7, { title: 'Revision seven title' }),
          floor: 7,
        },
      },
    )
    await waitFor(() => expect(result.current.document?.id.contentRevision).toBe(7))

    rerender({
      current: detail(7, { title: 'Revision seven fallback' }),
      floor: 8,
    })

    await waitFor(() => expect(result.current.document).toMatchObject({
      id: { contentRevision: 8 },
      detail: { status: 'idle' },
      body: { status: 'idle' },
    }))

    rerender({
      current: detail(8, { title: 'Revision eight authoritative' }),
      floor: 8,
    })

    await waitFor(() => expect(result.current.document?.detail).toMatchObject({
      status: 'ready',
      revision: 8,
      data: { title: 'Revision eight authoritative' },
    }))
  })

  it('does not let a late body response restore detail metadata captured before await', async () => {
    const owner = lease()
    let resolveBody!: (value: ApiResult<LinkContentResponse>) => void
    const pendingBody = new Promise<ApiResult<LinkContentResponse>>((resolve) => {
      resolveBody = resolve
    })
    const load = vi.fn(() => pendingBody)
    const { result, rerender } = renderHook(
      ({ current }) => useSavedArticleDocument({ lease: owner, detail: current, loadBody: load }),
      {
        initialProps: {
          current: detail(7, { title: 'Old title', summary: 'Old summary' }),
        },
      },
    )
    await waitFor(() => expect(result.current.document?.id.contentRevision).toBe(7))

    let pending!: Promise<ApiResult<LinkContentResponse> | null>
    act(() => {
      pending = result.current.loadBody()
    })
    rerender({
      current: detail(8, { title: 'New title', summary: 'New summary' }),
    })
    await waitFor(() => expect(result.current.document).toMatchObject({
      id: { contentRevision: 8 },
      detail: {
        status: 'ready',
        revision: 8,
        data: { title: 'New title', summary: 'New summary' },
      },
    }))

    await act(async () => {
      resolveBody(ok(body(8, 'revision eight body')))
      await pending
    })

    expect(result.current.document).toMatchObject({
      id: { contentRevision: 8 },
      detail: {
        status: 'ready',
        revision: 8,
        data: { title: 'New title', summary: 'New summary' },
      },
      body: {
        status: 'ready',
        revision: 8,
        data: { content: 'revision eight body' },
      },
    })
  })

  it('merges a late body with the latest metadata from the same revision', async () => {
    const owner = lease()
    let resolveBody!: (value: ApiResult<LinkContentResponse>) => void
    const pendingBody = new Promise<ApiResult<LinkContentResponse>>((resolve) => {
      resolveBody = resolve
    })
    const load = vi.fn(() => pendingBody)
    const { result, rerender } = renderHook(
      ({ current }) => useSavedArticleDocument({ lease: owner, detail: current, loadBody: load }),
      {
        initialProps: {
          current: detail(7, { title: 'Old title', summary: 'Old summary' }),
        },
      },
    )
    await waitFor(() => expect(result.current.document?.id.contentRevision).toBe(7))

    let pending!: Promise<ApiResult<LinkContentResponse> | null>
    act(() => {
      pending = result.current.loadBody()
    })
    rerender({
      current: detail(7, { title: 'New title', summary: 'New summary' }),
    })
    await waitFor(() => expect(result.current.document).toMatchObject({
      detail: {
        status: 'ready',
        revision: 7,
        data: { title: 'New title', summary: 'New summary' },
      },
    }))

    await act(async () => {
      resolveBody(ok(body(7, 'revision seven body')))
      await pending
    })

    expect(result.current.document).toMatchObject({
      id: { contentRevision: 7 },
      detail: {
        status: 'ready',
        revision: 7,
        data: {
          title: 'New title',
          summary: 'New summary',
          content: 'revision seven body',
        },
      },
      body: {
        status: 'ready',
        revision: 7,
        data: { content: 'revision seven body' },
      },
    })
  })

  it('does not let a late old request replace a newer document body', async () => {
    const owner = lease()
    let resolveOld!: (value: ApiResult<LinkContentResponse>) => void
    const oldRequest = new Promise<ApiResult<LinkContentResponse>>((resolve) => {
      resolveOld = resolve
    })
    const load = vi.fn()
      .mockImplementationOnce(() => oldRequest)
      .mockResolvedValueOnce(ok(body(8, 'new body')))
    const { result } = renderHook(() =>
      useSavedArticleDocument({ lease: owner, detail: detail(7), loadBody: load }),
    )
    await waitFor(() => expect(result.current.document).not.toBeNull())

    let pending!: Promise<ApiResult<LinkContentResponse> | null>
    act(() => {
      pending = result.current.loadBody()
    })
    await act(async () => {
      await result.current.loadBody()
      resolveOld(ok(body(7, 'late old body')))
      await pending
    })

    expect(result.current.document).toMatchObject({
      id: { contentRevision: 8 },
      body: { status: 'ready', revision: 8, data: { content: 'new body' } },
    })
  })

  it('disposes the old controller when the selected link changes', async () => {
    const owner = lease()
    const load = vi.fn(async () => ok(body(7, 'seven')))
    const { result, rerender } = renderHook(
      ({ current }) => useSavedArticleDocument({ lease: owner, detail: current, loadBody: load }),
      { initialProps: { current: detail(7) } },
    )
    await waitFor(() => expect(result.current.document).not.toBeNull())
    const oldContext = result.current.captureContext()

    rerender({ current: detail(3, { id: 'L2' }) })

    await waitFor(() => expect(result.current.document?.id.linkId).toBe('L2'))
    expect(oldContext?.signal.aborted).toBe(true)
  })
})
