import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { ok, type ApiResult } from '../../lib/api/result'
import type { ReaderHomeResponse, ReaderThoughtResponse } from '../../lib/api/types'
import { IdentityLease } from '../../lib/identity'
import { enabledReaderCapabilityLease } from '../../test/capabilities'
import { ownedDatabaseName } from '../../lib/storage-ownership'
import { commitAnnotationOperation } from '../../lib/user-data/annotation-store'
import {
  resetUserDataDatabaseHandle,
} from '../../lib/user-data/idb'
import { HomeSurface } from './HomeSurface'
import { ThoughtHistorySurface } from './ThoughtHistorySurface'

const TARGET = { kind: 'saved-content', contentRevision: 7 } as const

function lease(): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: 'server-surface-fixture',
    physicalNamespace: 'surface-fixture',
    localEpoch: 1,
  })
}

function remoteThought(): ReaderThoughtResponse {
  return {
    contract_version: 1,
    id: 'remote-thought',
    host_kind: 'link',
    host_id: 'L1',
    link_id: 'L1',
    target: {
      kind: 'saved-content',
      host_id: 'L1',
      version: { content_revision: 7 },
      block_key: 'content-document',
      range: { start: 0, end: 4 },
    },
    quote: { exact: 'text', start: 0, end: 4, prefix: '', suffix: '', block_key: 'content-document' },
    body: '服务端正文',
    source: 'self',
    deleted: false,
    last_sequence: 4,
    winner_key: { logical_clock: 4, device_id: 'remote-device', op_id: 'remote-op' },
    created_at: '2026-08-10T00:00:00.000Z',
    updated_at: '2026-08-10T00:00:00.000Z',
  } as unknown as ReaderThoughtResponse
}

function homeFixture(thought: ReaderThoughtResponse): ReaderHomeResponse {
  return {
    today: '2026-08-10',
    summary: 'fixture',
    counts: {},
    continue_reading: [],
    recent_thoughts: [thought],
    todos: [],
    stale: false,
  }
}

async function seedLocalThought(currentLease: IdentityLease): Promise<void> {
  await expect(commitAnnotationOperation(currentLease, {
    kind: 'add',
    opId: 'surface-local-operation',
    linkId: 'L1',
    target: TARGET,
    draft: {
      id: 'surface-local-thought',
      blockKey: 'content-document',
      start: 0,
      end: 4,
      text: 'text',
      note: '离线持久化正文',
      source: 'ai',
      createdAt: 1_000,
      updatedAt: 2_000,
      quote: { exact: 'text', prefix: '', suffix: '' },
    },
  })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((release) => { resolve = release })
  return { promise, resolve }
}

async function deleteUserDataDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('database delete failed'))
    request.onblocked = () => reject(new Error('database delete blocked'))
  })
}

afterEach(async () => {
  vi.restoreAllMocks()
  window.history.replaceState({}, '', '/')
  await deleteUserDataDatabase()
})

describe('Thought aggregate surfaces', () => {
  it('renders a pre-existing local thought on Home and 我的想法 while offline', async () => {
    const currentLease = lease()
    await seedLocalThought(currentLease)

    const getHome = vi.fn(async () => { throw new Error('offline home') })
    const listThoughts = vi.fn(async () => { throw new Error('offline thoughts') })
    const pushThoughtOps = vi.fn()
    const offlineClient = {
      getHome,
      listThoughts,
      listTodos: vi.fn(async () => ok({ items: [] })),
      pushThoughtOps,
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as IdentityBoundReaderClient

    // The record is seeded before either surface mounts, so no post-mount
    // annotations-change event can be responsible for rendering it.
    const home = render(<HomeSurface capabilityLease={enabledReaderCapabilityLease()} client={offlineClient} lease={currentLease} onNavigate={vi.fn()} onOpenLink={vi.fn()} />)
    await screen.findByText('离线持久化正文')
    expect(getHome).toHaveBeenCalledTimes(1)
    expect(pushThoughtOps).not.toHaveBeenCalled()
    home.unmount()

    window.history.replaceState({}, '', '/?tool=history&thought_view=live')
    const history = render(<ThoughtHistorySurface capabilityLease={enabledReaderCapabilityLease()} client={offlineClient} lease={currentLease} onNavigate={vi.fn()} />)
    await screen.findByText('离线持久化正文')
    expect(listThoughts).toHaveBeenCalledTimes(1)
    expect(pushThoughtOps).not.toHaveBeenCalled()
    history.unmount()
  })

  it('renders the same local-first body, active state, and order in Home and 我的想法', async () => {
    const currentLease = lease()
    await expect(commitAnnotationOperation(currentLease, {
      kind: 'add',
      opId: 'local-surface-op',
      linkId: 'L1',
      target: TARGET,
      draft: {
        id: 'local-thought',
        blockKey: 'content-document',
        start: 0,
        end: 4,
        text: 'text',
        note: '本地正文',
        source: 'self',
        createdAt: Date.parse('2026-08-11T00:00:00.000Z'),
        updatedAt: Date.parse('2026-08-11T00:00:00.000Z'),
        quote: { exact: 'text', prefix: '', suffix: '' },
      },
    })).resolves.toMatchObject({ ok: true })
    await expect(commitAnnotationOperation(currentLease, {
      kind: 'update',
      opId: 'local-surface-update',
      linkId: 'L1',
      target: TARGET,
      annotationId: 'remote-thought',
      patch: {
        note: '本地覆盖正文',
        updatedAt: Date.parse('2026-08-12T00:00:00.000Z'),
      },
    })).resolves.toMatchObject({ ok: true })
    await expect(commitAnnotationOperation(currentLease, {
      kind: 'delete',
      opId: 'local-surface-delete',
      linkId: 'L1',
      target: TARGET,
      annotationId: 'server-deleted',
    })).resolves.toMatchObject({ ok: true })

    const thought = remoteThought()
    const deleted = {
      ...remoteThought(),
      id: 'server-deleted',
      body: '已删除的服务端正文',
      last_sequence: 5,
      winner_key: { logical_clock: 5, device_id: 'remote-device', op_id: 'server-deleted-op' },
    }
    const getHome = vi.fn(async () => ok({
      ...homeFixture(thought),
      recent_thoughts: [thought, deleted],
    }))
    const currentClient = {
      getHome,
      listThoughts: vi.fn(async () => ok({
        contract_version: 1 as const,
        items: [thought, deleted],
        next_cursor: undefined,
      })),
      listTodos: vi.fn(async () => ok({ items: [] })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as IdentityBoundReaderClient
    const home = render(<HomeSurface capabilityLease={enabledReaderCapabilityLease()} client={currentClient} lease={currentLease} onNavigate={vi.fn()} onOpenLink={vi.fn()} />)
    await screen.findByText('本地覆盖正文')
    const homeBodies = [...home.container.querySelectorAll('.rvx-thought-list li p')]
      .map((element) => element.textContent)
    expect(homeBodies).toEqual(['本地覆盖正文', '本地正文'])
    expect(homeBodies).not.toContain('已删除的服务端正文')
    home.unmount()

    window.history.replaceState({}, '', '/?tool=history&thought_view=live')
    const history = render(<ThoughtHistorySurface capabilityLease={enabledReaderCapabilityLease()} client={currentClient} lease={currentLease} onNavigate={vi.fn()} />)
    await screen.findByText('本地覆盖正文')
    const historyBodies = [...history.container.querySelectorAll('.rvx-history-list strong')]
      .map((element) => element.textContent)
    expect(historyBodies).toEqual(['本地覆盖正文', '本地正文'])
    expect(historyBodies).not.toContain('已删除的服务端正文')
  })

  it('keeps the server cursor opaque while pages reapply local add and delete overlays', async () => {
    const currentLease = lease()
    await expect(commitAnnotationOperation(currentLease, {
      kind: 'add',
      opId: 'paged-local-add',
      linkId: 'L1',
      target: TARGET,
      draft: {
        id: 'paged-local-add',
        blockKey: 'content-document',
        start: 0,
        end: 4,
        text: 'text',
        note: '本地分页想法',
        source: 'self',
        createdAt: Date.parse('2026-08-12T00:00:00.000Z'),
        updatedAt: Date.parse('2026-08-12T00:00:00.000Z'),
        quote: { exact: 'text', prefix: '', suffix: '' },
      },
    })).resolves.toMatchObject({ ok: true })
    await expect(commitAnnotationOperation(currentLease, {
      kind: 'delete',
      opId: 'paged-local-delete',
      linkId: 'L1',
      target: TARGET,
      annotationId: 'server-deleted',
    })).resolves.toMatchObject({ ok: true })

    const pageOne = [
      { ...remoteThought(), id: 'server-first', body: '服务端第一页', last_sequence: 4 },
      { ...remoteThought(), id: 'server-deleted', body: '不应复活', last_sequence: 5 },
    ] as ReaderThoughtResponse[]
    const pageTwo = [
      { ...pageOne[0] },
      {
        ...remoteThought(),
        id: 'server-second',
        body: '服务端第二页',
        last_sequence: 3,
        updated_at: '2026-08-09T00:00:00.000Z',
      },
    ] as ReaderThoughtResponse[]
    const listThoughts = vi.fn(async (options: { after?: string }) => ok({
      contract_version: 1 as const,
      items: options.after === 'opaque-page-two' ? pageTwo : pageOne,
      next_cursor: options.after === 'opaque-page-two' ? undefined : 'opaque-page-two',
    }))
    const currentClient = {
      listThoughts,
      listTodos: vi.fn(async () => ok({ items: [] })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as IdentityBoundReaderClient

    window.history.replaceState({}, '', '/?tool=history&thought_view=live')
    const aggregate = render(<ThoughtHistorySurface capabilityLease={enabledReaderCapabilityLease()} client={currentClient} lease={currentLease} onNavigate={vi.fn()} />)
    await screen.findByText('本地分页想法')
    fireEvent.click(await screen.findByRole('button', { name: '更多' }))

    await waitFor(() => expect(listThoughts).toHaveBeenNthCalledWith(2, {
      after: 'opaque-page-two',
      limit: 30,
    }))
    await screen.findByText('服务端第二页')
    const bodies = [...aggregate.container.querySelectorAll('.rvx-history-list strong')]
      .map((element) => element.textContent)
    expect(bodies).toEqual(['本地分页想法', '服务端第一页', '服务端第二页'])
    expect(bodies).not.toContain('不应复活')
  })

  it('does not paint a late page from an old identity after the new lease has loaded', async () => {
    const leaseA = new IdentityLease({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'identity-A',
      localEpoch: 1,
    })
    const leaseB = new IdentityLease({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'identity-B',
      localEpoch: 2,
    })
    const delayedA = deferred<ApiResult<ReaderHomeResponse>>()
    const clientA = {
      getHome: vi.fn(() => delayedA.promise),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as IdentityBoundReaderClient
    const thoughtB = { ...remoteThought(), id: 'thought-B', body: '身份 B 想法' }
    const clientB = {
      getHome: vi.fn(async () => ok(homeFixture(thoughtB))),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as IdentityBoundReaderClient

    const home = render(<HomeSurface capabilityLease={enabledReaderCapabilityLease()} client={clientA} lease={leaseA} onNavigate={vi.fn()} onOpenLink={vi.fn()} />)
    home.rerender(<HomeSurface capabilityLease={enabledReaderCapabilityLease()} client={clientB} lease={leaseB} onNavigate={vi.fn()} onOpenLink={vi.fn()} />)
    await screen.findByText('身份 B 想法')

    await act(async () => {
      delayedA.resolve(ok(homeFixture({ ...remoteThought(), id: 'thought-A', body: '身份 A 想法' })))
      await delayedA.promise
    })

    await waitFor(() => expect(screen.queryByText('身份 A 想法')).not.toBeInTheDocument())
    expect(screen.getByText('身份 B 想法')).toBeInTheDocument()
  })
})
