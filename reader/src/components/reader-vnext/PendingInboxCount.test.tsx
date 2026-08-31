import { act, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { PrimaryNav } from '../PrimaryNav'
import { refreshPendingInboxCount } from '../../lib/pending-inbox-events'
import { PendingInboxCountProvider } from './PendingInboxCount'
import { ok } from '@webtag/api'
import {
  enabledReaderCapabilityLease,
  enabledReaderCapabilityPolicy,
} from '../../test/capabilities'
import { makeReaderInbox } from '../../test/fixtures'
import { makeReaderClient } from '../../test/reader-client'

function pending(id: string) {
  return makeReaderInbox({
    id,
    url: `https://example.test/${id}`,
    title: id,
    body: '',
    summary: null,
    tags: [],
    expires_at: '2026-09-09T00:00:00Z',
    created_at: '2026-08-10T00:00:00Z',
    updated_at: '2026-08-10T00:00:00Z',
  })
}

function client(items: ReturnType<typeof pending>[], inbox = true, current = () => true): IdentityBoundReaderClient {
  return makeReaderClient({
    getCapabilities: vi.fn(async () => ok({ reader_vnext: true, reader: { inbox } })),
    listInbox: vi.fn(async () => ok({ items, next_cursor: undefined, active_count: items.length, expired_count: 0 })),
  }, { isIdentityCurrent: current })
}

type BadgeHost = 'reading' | 'sites' | 'subs' | 'notes'

const BADGE_HOSTS: readonly BadgeHost[] = ['reading', 'sites', 'subs', 'notes']

function renderNavigation(clientValue: IdentityBoundReaderClient, host: BadgeHost = 'reading') {
  return render(
    <PendingInboxCountProvider client={clientValue} capabilityLease={enabledReaderCapabilityLease()}>
      <aside className="rvx-nav" aria-label="Reader 导航">
        <PrimaryNav activeLibrary={host} policy={enabledReaderCapabilityPolicy()} onNavigate={() => {}} />
      </aside>
      <PrimaryNav activeLibrary={host} policy={enabledReaderCapabilityPolicy()} onNavigate={() => {}} />
    </PendingInboxCountProvider>,
  )
}

afterEach(() => vi.restoreAllMocks())

describe('PendingInboxCountProvider', () => {
  it.each(BADGE_HOSTS)('shows the same authoritative active count in the %s navigation host', async (host) => {
    const first = client([pending('I1'), pending('I2')])
    renderNavigation(first, host)
    await waitFor(() => expect(screen.getAllByLabelText('收件箱 2 项待处理')).toHaveLength(2))
  })

  it('hides a zero count or capability-disabled Inbox', async () => {
    const zero = renderNavigation(client([]))
    await waitFor(() => expect(screen.queryByLabelText(/项收件箱/)).not.toBeInTheDocument())

    zero.unmount()
    renderNavigation(client([pending('I1')], false))
    await waitFor(() => expect(screen.queryByLabelText(/项收件箱/)).not.toBeInTheDocument())
  })

  it('refreshes after mutations and ignores a response that becomes stale with the identity', async () => {
    const items = [pending('I1')]
    const current = vi.fn(() => true)
    let staleResolve!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn(() => {
      if (listInbox.mock.calls.length === 3) return new Promise<ReturnType<typeof ok>>((resolve) => { staleResolve = resolve })
      return Promise.resolve(ok({ items: [...items], next_cursor: undefined, active_count: items.length, expired_count: 0 }))
    })
    const currentClient = makeReaderClient({
      getCapabilities: vi.fn(async () => ok({ reader_vnext: true, reader: { inbox: true } })),
      listInbox,
    }, { isIdentityCurrent: current })
    renderNavigation(currentClient)
    await waitFor(() => expect(screen.getAllByLabelText('收件箱 1 项待处理')).toHaveLength(2))

    items.push(pending('I2'))
    refreshPendingInboxCount()
    await waitFor(() => expect(screen.getAllByLabelText('收件箱 2 项待处理')).toHaveLength(2))

    items.push(pending('I3'))
    refreshPendingInboxCount()
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(3))
    current.mockReturnValue(false)
    staleResolve(ok({ items: [...items], next_cursor: undefined, active_count: items.length, expired_count: 0 }))
    await waitFor(() => expect(screen.getAllByLabelText('收件箱 2 项待处理')).toHaveLength(2))
    expect(screen.getAllByLabelText('收件箱 2 项待处理')).toHaveLength(2)
  })

  it('keeps the newest same-identity refresh when an older request finishes last', async () => {
    let resolveInitial!: (value: ReturnType<typeof ok>) => void
    const listInbox = vi.fn()
      .mockImplementationOnce(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveInitial = resolve }))
      .mockResolvedValueOnce(ok({ items: [pending('I1'), pending('I2')], next_cursor: undefined, active_count: 2, expired_count: 0 }))
    const currentClient = makeReaderClient({
      getCapabilities: vi.fn(async () => ok({ reader_vnext: true, reader: { inbox: true } })),
      listInbox,
    })
    renderNavigation(currentClient)
    await waitFor(() => expect(listInbox).toHaveBeenCalledTimes(1))

    refreshPendingInboxCount()
    await waitFor(() => expect(screen.getAllByLabelText('收件箱 2 项待处理')).toHaveLength(2))
    await act(async () => {
      resolveInitial(ok({ items: [pending('I1')], next_cursor: undefined, active_count: 1, expired_count: 0 }))
    })

    expect(screen.getAllByLabelText('收件箱 2 项待处理')).toHaveLength(2)
  })

  it('hides the previous identity count synchronously while the next identity loads', async () => {
    const first = client([pending('I1')])
    const rendered = renderNavigation(first)
    await waitFor(() => expect(screen.getAllByLabelText('收件箱 1 项待处理')).toHaveLength(2))

    let resolveNext!: (value: ReturnType<typeof ok>) => void
    const next = makeReaderClient({
      getCapabilities: vi.fn(async () => ok({ reader_vnext: true, reader: { inbox: true } })),
      listInbox: vi.fn(() => new Promise<ReturnType<typeof ok>>((resolve) => { resolveNext = resolve })),
    })
    rendered.rerender(
      <PendingInboxCountProvider client={next} capabilityLease={enabledReaderCapabilityLease()}>
        <aside className="rvx-nav" aria-label="Reader 导航">
          <PrimaryNav activeLibrary="reading" policy={enabledReaderCapabilityPolicy()} onNavigate={() => {}} />
        </aside>
        <PrimaryNav activeLibrary="sites" policy={enabledReaderCapabilityPolicy()} onNavigate={() => {}} />
      </PendingInboxCountProvider>,
    )
    expect(screen.queryByLabelText(/项收件箱/)).not.toBeInTheDocument()
    await waitFor(() => expect(next.listInbox).toHaveBeenCalledTimes(1))

    await act(async () => {
      resolveNext(ok({ items: [pending('I2'), pending('I3')], next_cursor: undefined, active_count: 2, expired_count: 0 }))
    })
    await waitFor(() => expect(screen.getAllByLabelText('收件箱 2 项待处理')).toHaveLength(2))
  })
})
