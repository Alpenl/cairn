import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { ReviewView } from './ReviewView'
import { ok, type ApiResult } from '../lib/api/result'
import type { ReaderClient } from '../lib/api/client'
import type { LibraryReviewResponse } from '../lib/api/types'
import { readerIdentity } from '../lib/identity'
import { enabledReaderCapabilityLease } from '../test/capabilities'

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

function review(
  id: string,
  kind: LibraryReviewResponse['kind'],
  revision = 1,
): LibraryReviewResponse {
  return {
    id,
    kind,
    payload: {},
    status: 'pending',
    revision,
    created_at: '2026-07-30T00:00:00Z',
  }
}

describe('ReviewView identity ownership', () => {
  it('does not let an A-era list response replace B reviews', async () => {
    const leaseA = readerIdentity.activeLease!
    const ownershipA = leaseA.capture('review list test')
    const delayedA = deferred<ApiResult<LibraryReviewResponse[]>>()
    const clientA = {
      getLibraryReviews: vi.fn(() => delayedA.promise),
      resolveLibraryReview: vi.fn(),
      isIdentityCurrent: vi.fn(() => leaseA.isCurrent(ownershipA)),
    } as unknown as ReaderClient
    const reviewB = review('review-B', 'note_conflict')
    const clientB = {
      getLibraryReviews: vi.fn(async () => ok([reviewB])),
      resolveLibraryReview: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const onToast = vi.fn()
    const capabilityLease = enabledReaderCapabilityLease()
    const rendered = render(<ReviewView client={clientA} capabilityLease={capabilityLease} onToast={onToast} />)

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    rendered.rerender(<ReviewView client={clientB} capabilityLease={capabilityLease} onToast={onToast} />)
    await screen.findByText('备注冲突')

    await act(async () => {
      delayedA.resolve(ok([review('review-A', 'classification_uncertain')]))
      await delayedA.promise
    })

    expect(screen.getByText('备注冲突')).toBeInTheDocument()
    expect(screen.queryByText('分类待确认')).not.toBeInTheDocument()
    expect(onToast).not.toHaveBeenCalled()
  })

  it('does not let an A-era resolution remove B review state with the same id', async () => {
    const leaseA = readerIdentity.activeLease!
    const ownershipA = leaseA.capture('review resolution test')
    const sharedID = 'review-shared'
    const reviewA = review(sharedID, 'classification_uncertain', 1)
    const reviewB = review(sharedID, 'note_conflict', 2)
    const delayedResolution = deferred<ApiResult<LibraryReviewResponse>>()
    const clientA = {
      getLibraryReviews: vi.fn(async () => ok([reviewA])),
      resolveLibraryReview: vi.fn(() => delayedResolution.promise),
      isIdentityCurrent: vi.fn(() => leaseA.isCurrent(ownershipA)),
    } as unknown as ReaderClient
    const clientB = {
      getLibraryReviews: vi.fn(async () => ok([reviewB])),
      resolveLibraryReview: vi.fn(),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const onToast = vi.fn()
    const capabilityLease = enabledReaderCapabilityLease()
    const rendered = render(<ReviewView client={clientA} capabilityLease={capabilityLease} onToast={onToast} />)
    await screen.findByText('分类待确认')
    fireEvent.click(screen.getByRole('button', { name: '忽略' }))
    await waitFor(() => expect(clientA.resolveLibraryReview).toHaveBeenCalledTimes(1))

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    rendered.rerender(<ReviewView client={clientB} capabilityLease={capabilityLease} onToast={onToast} />)
    await screen.findByText('备注冲突')

    await act(async () => {
      delayedResolution.resolve(ok(reviewA))
      await delayedResolution.promise
    })

    expect(screen.getByText('备注冲突')).toBeInTheDocument()
    expect(screen.queryByText('当前没有待审核项')).not.toBeInTheDocument()
    expect(onToast).not.toHaveBeenCalled()
  })
})
