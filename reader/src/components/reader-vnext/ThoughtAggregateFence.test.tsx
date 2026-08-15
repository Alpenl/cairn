import { act, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { ok } from '../../lib/api/result'
import type { ReaderHomeResponse, ReaderThoughtResponse } from '../../lib/api/types'
import { IdentityLease } from '../../lib/identity'
import { enabledReaderCapabilityLease } from '../../test/capabilities'

const thoughtReadModel = vi.hoisted(() => ({
  cacheServerThoughtPage: vi.fn(),
  selectThoughtReadModel: vi.fn(),
}))

vi.mock('../../lib/user-data/thought-sync', () => ({
  cacheServerThoughtPage: thoughtReadModel.cacheServerThoughtPage,
  selectThoughtReadModel: thoughtReadModel.selectThoughtReadModel,
  sortThoughtReadModel: (items: readonly unknown[]) => [...items],
}))

import { HomeSurface } from './HomeSurface'
import { ThoughtHistorySurface } from './ThoughtHistorySurface'

function makeLease(identity: string, epoch: number): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `server-${identity}`,
    physicalNamespace: `physical-${identity}`,
    localEpoch: epoch,
  })
}

function thought(id: string, body: string): ReaderThoughtResponse {
  return {
    contract_version: 1,
    id,
    host_kind: 'link',
    host_id: 'L1',
    link_id: 'L1',
    target: {
      kind: 'saved-content',
      host_id: 'L1',
      version: { content_revision: 1 },
      block_key: 'content-document',
      range: { start: 0, end: 4 },
    },
    quote: { exact: 'text', start: 0, end: 4, prefix: '', suffix: '', block_key: 'content-document' },
    body,
    source: 'self',
    deleted: false,
    last_sequence: 1,
    winner_key: { logical_clock: 1, device_id: 'device', op_id: `op-${id}` },
    created_at: '2026-08-10T00:00:00.000Z',
    updated_at: '2026-08-10T00:00:00.000Z',
  } as unknown as ReaderThoughtResponse
}

function homeFixture(item: ReaderThoughtResponse): ReaderHomeResponse {
  return {
    today: '2026-08-10',
    summary: 'fixture',
    counts: {},
    continue_reading: [],
    recent_thoughts: [item],
    todos: [],
    stale: false,
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((release) => { resolve = release })
  return { promise, resolve }
}

afterEach(() => {
  thoughtReadModel.cacheServerThoughtPage.mockReset()
  thoughtReadModel.selectThoughtReadModel.mockReset()
  window.history.replaceState({}, '', '/')
})

describe('Thought aggregate lease fences', () => {
  it('ignores an old Home selector refresh after the identity request generation changes', async () => {
    const oldThought = thought('old-home', '旧身份想法')
    const newThought = thought('new-home', '新身份想法')
    const lateSelector = deferred<{ readonly ok: true; readonly value: readonly ReaderThoughtResponse[] }>()
    thoughtReadModel.cacheServerThoughtPage.mockResolvedValue({ ok: true, value: 1 })
    thoughtReadModel.selectThoughtReadModel
      .mockResolvedValueOnce({ ok: true, value: [oldThought] })
      .mockResolvedValueOnce({ ok: true, value: [oldThought] })
      .mockImplementationOnce(() => lateSelector.promise)
      .mockResolvedValue({ ok: true, value: [newThought] })

    const clientA = {
      getHome: vi.fn(async () => ok(homeFixture(oldThought))),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as IdentityBoundReaderClient
    const clientB = {
      getHome: vi.fn(async () => ok(homeFixture(newThought))),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as IdentityBoundReaderClient

    const home = render(<HomeSurface capabilityLease={enabledReaderCapabilityLease()} client={clientA} lease={makeLease('A', 1)} onNavigate={vi.fn()} onOpenLink={vi.fn()} />)
    await screen.findByText('旧身份想法')

    act(() => window.dispatchEvent(new Event('webtag:annotations-change')))
    home.rerender(<HomeSurface capabilityLease={enabledReaderCapabilityLease()} client={clientB} lease={makeLease('B', 2)} onNavigate={vi.fn()} onOpenLink={vi.fn()} />)
    await screen.findByText('新身份想法')

    await act(async () => {
      lateSelector.resolve({ ok: true, value: [oldThought] })
      await lateSelector.promise
    })

    await waitFor(() => expect(screen.queryByText('旧身份想法')).not.toBeInTheDocument())
    expect(screen.getByText('新身份想法')).toBeInTheDocument()
  })

  it('ignores an old 我的想法 selector refresh after the identity request generation changes', async () => {
    const oldThought = thought('old-history', '旧身份聚合')
    const newThought = thought('new-history', '新身份聚合')
    const lateSelector = deferred<{ readonly ok: true; readonly value: readonly ReaderThoughtResponse[] }>()
    thoughtReadModel.cacheServerThoughtPage.mockResolvedValue({ ok: true, value: 1 })
    thoughtReadModel.selectThoughtReadModel
      .mockResolvedValueOnce({ ok: true, value: [oldThought] })
      .mockResolvedValueOnce({ ok: true, value: [oldThought] })
      .mockImplementationOnce(() => lateSelector.promise)
      .mockResolvedValue({ ok: true, value: [newThought] })

    const clientA = {
      listThoughts: vi.fn(async () => ok({ contract_version: 1 as const, items: [oldThought] })),
      listTodos: vi.fn(async () => ok({ items: [] })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as IdentityBoundReaderClient
    const clientB = {
      listThoughts: vi.fn(async () => ok({ contract_version: 1 as const, items: [newThought] })),
      listTodos: vi.fn(async () => ok({ items: [] })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as IdentityBoundReaderClient

    window.history.replaceState({}, '', '/?tool=history&thought_view=live')
    const aggregate = render(<ThoughtHistorySurface capabilityLease={enabledReaderCapabilityLease()} client={clientA} lease={makeLease('A', 1)} onNavigate={vi.fn()} />)
    await screen.findByText('旧身份聚合')

    act(() => window.dispatchEvent(new Event('webtag:annotations-change')))
    aggregate.rerender(<ThoughtHistorySurface capabilityLease={enabledReaderCapabilityLease()} client={clientB} lease={makeLease('B', 2)} onNavigate={vi.fn()} />)
    await screen.findByText('新身份聚合')

    await act(async () => {
      lateSelector.resolve({ ok: true, value: [oldThought] })
      await lateSelector.promise
    })

    await waitFor(() => expect(screen.queryByText('旧身份聚合')).not.toBeInTheDocument())
    expect(screen.getByText('新身份聚合')).toBeInTheDocument()
  })
})
