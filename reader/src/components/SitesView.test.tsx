import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SitesView } from './SitesView'
import type { ReaderClient } from '../lib/api/client'
import type {
  ListSitesParams,
  SiteDetailResponse,
  SiteListItemResponse,
  SiteSplitPreviewResponse,
  SiteSplitRequest,
} from '../lib/api/types'
import { resourceStore } from '../lib/cache/store'
import { err, ok } from '@webtag/api'
import { enabledReaderCapabilityLease } from '../test/capabilities'
import type { ReaderCapabilityLease } from '../lib/capabilities'

const site: SiteListItemResponse = {
  id: '11111111-1111-1111-1111-111111111111',
  name: 'Acme Docs',
  intro: 'Reference material for the Acme platform.',
  display_host: 'docs.acme.test',
  homepage_url: 'https://docs.acme.test',
  icon_url: null,
  tags: ['docs', 'platform'],
  entry_count: 1,
  pinned: false,
  primary_entry: {
    id: '22222222-2222-2222-2222-222222222222',
    url: 'https://docs.acme.test/start',
  },
  revision: 3,
  first_collected_at: '2026-07-20T00:00:00Z',
  last_collected_at: '2026-07-21T00:00:00Z',
}

const detail: SiteDetailResponse = {
  ...site,
  user_note: '',
  entries: [
    {
      id: '22222222-2222-2222-2222-222222222222',
      link_id: '33333333-3333-3333-3333-333333333333',
      name: 'Getting started',
      purpose: 'Platform documentation',
      url: 'https://docs.acme.test/start',
      first_collected_at: '2026-07-20T00:00:00Z',
      last_recollected_at: null,
    },
  ],
  related_readings: [],
}

function numberedSite(index: number): SiteListItemResponse {
  const suffix = String(index).padStart(12, '0')
  const label = String(index).padStart(3, '0')
  return {
    ...site,
    id: `00000000-0000-4000-8000-${suffix}`,
    name: `Website ${label}`,
    display_host: `site-${label}.example.test`,
    homepage_url: `https://site-${label}.example.test`,
    primary_entry: null,
    revision: index,
  }
}

function numberedDetail(item: SiteListItemResponse): SiteDetailResponse {
  return {
    ...detail,
    ...item,
    entries: [],
    related_readings: [],
  }
}

function sitePage(items: SiteListItemResponse[], params: ListSitesParams = {}) {
  const page = params.page ?? 1
  const limit = params.limit ?? 30
  const start = (page - 1) * limit
  return ok({ items: items.slice(start, start + limit), total: items.length, page, limit })
}

beforeEach(() => resourceStore.clear())

function capabilityClient(siteDetail: SiteDetailResponse = detail) {
  const getSites = vi.fn(async () => ok({ items: [site], total: 1, page: 1, limit: 30 }))
  const getSite = vi.fn(async () => ok(siteDetail))
  const updateSite = vi.fn()
  const updateSiteEntry = vi.fn()
  const setPrimarySiteEntry = vi.fn()
  const deleteSiteEntry = vi.fn()
  const getLink = vi.fn()
  const convertLink = vi.fn()
  const deleteSite = vi.fn()
  const previewSiteMerge = vi.fn()
  const executeSiteMerge = vi.fn()
  const previewSiteSplit = vi.fn()
  const executeSiteSplit = vi.fn()
  const client = {
    isIdentityCurrent: vi.fn(() => true),
    getSites,
    getSite,
    updateSite,
    updateSiteEntry,
    setPrimarySiteEntry,
    deleteSiteEntry,
    getLink,
    convertLink,
    deleteSite,
    previewSiteMerge,
    executeSiteMerge,
    previewSiteSplit,
    executeSiteSplit,
  } as unknown as ReaderClient
  return {
    client,
    getSites,
    getSite,
    writeCalls: [
      updateSite,
      updateSiteEntry,
      setPrimarySiteEntry,
      deleteSiteEntry,
      getLink,
      convertLink,
      deleteSite,
      previewSiteMerge,
      executeSiteMerge,
      previewSiteSplit,
      executeSiteSplit,
    ],
  }
}

function sitesElement(client: ReaderClient, capabilityLease: ReaderCapabilityLease) {
  return (
    <SitesView
      client={client}
      capabilityLease={capabilityLease}
      onToast={vi.fn()}
      collapsed={false}
      onView={vi.fn()}
      navigationOpen={false}
      onCloseNavigation={vi.fn()}
    />
  )
}

async function openCapabilityDetail() {
  const cardTitle = await screen.findByText('Acme Docs')
  fireEvent.click(cardTitle.closest('button') as HTMLButtonElement)
  await screen.findByRole('heading', { name: 'Acme Docs' })
}

const splitDetail: SiteDetailResponse = {
  ...detail,
  entry_count: 3,
  entries: [
    ...detail.entries,
    {
      id: '44444444-4444-4444-4444-444444444444',
      link_id: '55555555-5555-5555-5555-555555555555',
      name: 'API reference',
      purpose: 'API documentation',
      url: 'https://github.com/acme/docs',
      first_collected_at: '2026-07-21T00:00:00Z',
      last_recollected_at: null,
    },
    {
      id: '66666666-6666-6666-6666-666666666666',
      link_id: '77777777-7777-7777-7777-777777777777',
      name: 'Status',
      purpose: 'Service status',
      url: 'https://status.acme.test',
      first_collected_at: '2026-07-22T00:00:00Z',
      last_recollected_at: null,
    },
  ],
}

async function openSplitDialog(client: ReaderClient, onToast = vi.fn()) {
  render(
    <SitesView
      client={client}
      capabilityLease={enabledReaderCapabilityLease()}
      onToast={onToast}
      collapsed={false}
      onView={vi.fn()}
      navigationOpen={false}
      onCloseNavigation={vi.fn()}
    />,
  )
  const cardTitle = await screen.findByText('Acme Docs')
  fireEvent.click(cardTitle.closest('button') as HTMLButtonElement)
  await screen.findByRole('heading', { name: 'Acme Docs' })
  fireEvent.click(screen.getByRole('button', { name: '拆分网站' }))
  return onToast
}

describe('SitesView card grid', () => {
  it('keeps read-only site access free of write controls and write requests', async () => {
    const fixture = capabilityClient()
    const lease = enabledReaderCapabilityLease({ siteWrite: false, siteAdvanced: false })

    render(sitesElement(fixture.client, lease))
    await openCapabilityDetail()

    for (const name of [
      '编辑网站资料',
      '置顶',
      '删除网站',
      '移到阅读',
      '编辑入口',
      '删除入口',
      '合并网站',
      '拆分网站',
    ]) {
      expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
    }
    for (const request of fixture.writeCalls) expect(request).not.toHaveBeenCalled()
  })

  it('offers ordinary site writes without exposing advanced management', async () => {
    const fixture = capabilityClient()
    const lease = enabledReaderCapabilityLease({ siteAdvanced: false })

    render(sitesElement(fixture.client, lease))
    await openCapabilityDetail()

    for (const name of ['编辑网站资料', '置顶', '删除网站', '移到阅读', '编辑入口', '删除入口']) {
      expect(screen.getByRole('button', { name })).toBeInTheDocument()
    }
    for (const name of ['合并网站', '拆分网站']) {
      expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
    }
    expect(fixture.writeCalls.slice(7).every((request) => request.mock.calls.length === 0)).toBe(true)
  })

  it('exposes merge and split management with siteAdvanced', async () => {
    const fixture = capabilityClient()

    render(sitesElement(fixture.client, enabledReaderCapabilityLease()))
    await openCapabilityDetail()

    expect(screen.getByRole('button', { name: '合并网站' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '拆分网站' })).toBeInTheDocument()
  })

  it('clears an open write dialog when siteWrite is revoked', async () => {
    const fixture = capabilityClient()
    const activeLease = enabledReaderCapabilityLease()
    const { rerender } = render(sitesElement(fixture.client, activeLease))
    await openCapabilityDetail()
    fireEvent.click(screen.getByRole('button', { name: '编辑网站资料' }))
    expect(screen.getByRole('heading', { name: '编辑网站资料' })).toBeInTheDocument()

    const readOnlyLease = enabledReaderCapabilityLease({ siteWrite: false, siteAdvanced: false })
    activeLease.revoke()
    rerender(sitesElement(fixture.client, readOnlyLease))
    await waitFor(() => expect(screen.queryByRole('heading', { name: '编辑网站资料' })).not.toBeInTheDocument())

    const restoredLease = enabledReaderCapabilityLease()
    readOnlyLease.revoke()
    rerender(sitesElement(fixture.client, restoredLease))
    await waitFor(() => expect(screen.queryByRole('heading', { name: '编辑网站资料' })).not.toBeInTheDocument())
    expect(fixture.client.updateSite).not.toHaveBeenCalled()
  })

  it('clears an open advanced dialog when siteAdvanced is revoked', async () => {
    const fixture = capabilityClient()
    const activeLease = enabledReaderCapabilityLease()
    const { rerender } = render(sitesElement(fixture.client, activeLease))
    await openCapabilityDetail()
    fireEvent.click(screen.getByRole('button', { name: '合并网站' }))
    expect(await screen.findByRole('dialog', { name: '合并到 Acme Docs' })).toBeInTheDocument()

    const writeLease = enabledReaderCapabilityLease({ siteAdvanced: false })
    activeLease.revoke()
    rerender(sitesElement(fixture.client, writeLease))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '合并到 Acme Docs' })).not.toBeInTheDocument())

    const restoredLease = enabledReaderCapabilityLease()
    writeLease.revoke()
    rerender(sitesElement(fixture.client, restoredLease))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '合并到 Acme Docs' })).not.toBeInTheDocument())
    expect(fixture.client.previewSiteMerge).not.toHaveBeenCalled()
    expect(fixture.client.executeSiteMerge).not.toHaveBeenCalled()
  })

  it('reserves no detail pane until a card is selected and toggles it closed', async () => {
    const getSite = vi.fn(async () => ok(detail))
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getSites: vi.fn(async () =>
        ok({ items: [site], total: 1, page: 1, limit: 30 }),
      ),
      getSite,
    } as unknown as ReaderClient
    const { container } = render(
      <SitesView
        client={client}
        capabilityLease={enabledReaderCapabilityLease()}
        onToast={vi.fn()}
        collapsed={false}
        onView={vi.fn()}
        navigationOpen={false}
        onCloseNavigation={vi.fn()}
      />,
    )

    const cardTitle = await screen.findByText('Acme Docs')
    const cardButton = cardTitle.closest('button')
    if (!cardButton) throw new Error('expected website card button to exist')
    expect(container.querySelector('.site-detail')).toBeNull()
    expect(container.querySelector('.sites-view')).not.toHaveClass('has-detail')

    fireEvent.click(cardButton)
    expect(await screen.findByRole('heading', { name: 'Acme Docs' })).toBeInTheDocument()
    expect(getSite).toHaveBeenCalledWith(site.id)
    expect(container.querySelector('.sites-view')).toHaveClass('has-detail')

    const activeCardTitle = screen
      .getAllByText('Acme Docs')
      .find((element) => element.closest('.site-card'))
    if (!activeCardTitle) throw new Error('expected active website card to remain visible')
    fireEvent.click(activeCardTitle.closest('button') as HTMLButtonElement)

    await waitFor(() => expect(container.querySelector('.site-detail')).toBeNull())
    expect(container.querySelector('.sites-view')).not.toHaveClass('has-detail')
  })

  it('网站工作区复用 canonical surface/tool 导航', async () => {
    const onNavigate = vi.fn()
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getSites: vi.fn(async () => ok({ items: [], total: 0, page: 1, limit: 30 })),
    } as unknown as ReaderClient

    render(
      <SitesView
        client={client}
        capabilityLease={enabledReaderCapabilityLease()}
        onToast={vi.fn()}
        collapsed={false}
        onView={vi.fn()}
        onNavigate={onNavigate}
        navigationOpen={false}
        onCloseNavigation={vi.fn()}
      />,
    )

    await waitFor(() => expect(client.getSites).toHaveBeenCalled())

    expect(screen.queryByRole('button', { name: '混合 Feed' })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '今天' }))
    // 网站 is hosted by 阅读 now, so the rail owes the user a way back.
    fireEvent.click(screen.getByRole('button', { name: '返回阅读' }))

    expect(onNavigate.mock.calls).toEqual([
      [{ kind: 'surface', id: 'home' }],
      [{ kind: 'library', id: 'reading' }],
    ])
  })

  it('reaches the 61st website and refreshes page one without closing its authoritative detail', async () => {
    const items = Array.from({ length: 61 }, (_, index) => numberedSite(index + 1))
    const getSites = vi.fn(async (params: ListSitesParams = {}) => sitePage(items, params))
    const getSite = vi.fn(async (id: string) => {
      const item = items.find((candidate) => candidate.id === id)
      if (!item) throw new Error(`unknown site ${id}`)
      return ok(numberedDetail(item))
    })
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getSites,
      getSite,
    } as unknown as ReaderClient
    const { container } = render(
      <SitesView
        client={client}
        capabilityLease={enabledReaderCapabilityLease()}
        onToast={vi.fn()}
        collapsed={false}
        onView={vi.fn()}
        navigationOpen={false}
        onCloseNavigation={vi.fn()}
      />,
    )

    await screen.findByText('Website 001')
    expect(screen.queryByText('Website 061')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '加载更多网站' }))
    await screen.findByText('Website 060')
    fireEvent.click(screen.getByRole('button', { name: '加载更多网站' }))
    const last = await screen.findByText('Website 061')

    expect(screen.queryByRole('button', { name: '加载更多网站' })).not.toBeInTheDocument()
    fireEvent.click(last.closest('button') as HTMLButtonElement)
    expect(await screen.findByRole('heading', { name: 'Website 061' })).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '刷新网站' }))
    await waitFor(() => expect(container.querySelectorAll('.site-card')).toHaveLength(30))
    expect(screen.getByRole('heading', { name: 'Website 061' })).toBeInTheDocument()
    expect(getSites.mock.calls.filter(([params]) => (params?.page ?? 1) === 1)).toHaveLength(2)
  })

  it('keeps a selected later-page merge candidate and submits the 101st candidate by ID', async () => {
    const items = Array.from({ length: 102 }, (_, index) => numberedSite(index + 1))
    const target = items[0]
    const selectedEarlier = items[59]
    const selectedLast = items[101]
    const getSites = vi.fn(async (params: ListSitesParams = {}) => sitePage(items, params))
    const getSite = vi.fn(async () => ok(numberedDetail(target)))
    const previewSiteMerge = vi.fn(async () => ok({
      target_site_id: target.id,
      target_revision: target.revision,
      entries: [],
      tags: [],
      identity_keys: [],
      field_conflicts: [],
      requires_resolution: false,
    }))
    const executeSiteMerge = vi.fn(async () => ok({
      site_id: target.id,
      revision: target.revision + 1,
      moved_entries: 2,
      deleted_duplicate_links: 0,
    }))
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getSites,
      getSite,
      previewSiteMerge,
      executeSiteMerge,
    } as unknown as ReaderClient
    const { container } = render(
      <SitesView
        client={client}
        capabilityLease={enabledReaderCapabilityLease()}
        onToast={vi.fn()}
        collapsed={false}
        onView={vi.fn()}
        navigationOpen={false}
        onCloseNavigation={vi.fn()}
      />,
    )

    const targetCard = await screen.findByText(target.name)
    fireEvent.click(targetCard.closest('button') as HTMLButtonElement)
    await screen.findByRole('heading', { name: target.name })
    fireEvent.click(screen.getByRole('button', { name: '合并网站' }))
    const dialog = await screen.findByRole('dialog', { name: `合并到 ${target.name}` })

    fireEvent.click(within(dialog).getByRole('button', { name: '加载更多可合并网站' }))
    const earlierCheckbox = await within(dialog).findByRole('checkbox', { name: new RegExp(selectedEarlier.name) })
    fireEvent.click(earlierCheckbox)
    expect(earlierCheckbox).toBeChecked()

    fireEvent.click(within(dialog).getByRole('button', { name: '加载更多可合并网站' }))
    await within(dialog).findByText('Website 090')
    expect(earlierCheckbox).toBeChecked()
    fireEvent.click(within(dialog).getByRole('button', { name: '加载更多可合并网站' }))
    const lastCheckbox = await within(dialog).findByRole('checkbox', { name: new RegExp(selectedLast.name) })
    fireEvent.click(lastCheckbox)

    fireEvent.click(within(dialog).getByRole('button', { name: '预览合并' }))
    await waitFor(() => expect(previewSiteMerge).toHaveBeenCalledWith({
      target_site_id: target.id,
      target_revision: target.revision,
      sources: [
        { site_id: selectedEarlier.id, revision: selectedEarlier.revision },
        { site_id: selectedLast.id, revision: selectedLast.revision },
      ],
    }))
    fireEvent.click(within(dialog).getByRole('button', { name: '确认合并' }))
    await waitFor(() => expect(executeSiteMerge).toHaveBeenCalledWith({
      target_site_id: target.id,
      target_revision: target.revision,
      sources: [
        { site_id: selectedEarlier.id, revision: selectedEarlier.revision },
        { site_id: selectedLast.id, revision: selectedLast.revision },
      ],
      resolutions: [],
    }))
    await waitFor(() => expect(container.querySelector('[role="dialog"]')).toBeNull())
  })

  it('收集空白起始的完整资料，并把 preview 返回的同一 payload 用于 execute', async () => {
    let finalPreviewPayload: SiteSplitRequest | undefined
    const previewSiteSplit = vi.fn(async (_siteID: string, request: SiteSplitRequest) => {
      finalPreviewPayload = request
      const selectedIdentity = request.identity_keys_for_new_site?.[0]
      return ok<SiteSplitPreviewResponse>({
        source_site_id: splitDetail.id,
        source_revision: request.expected_revision,
        payload: request,
        entries: splitDetail.entries
          .filter((entry) => request.entry_ids.includes(entry.id))
          .map((entry) => ({ id: entry.id, site_id: splitDetail.id, link_id: entry.link_id, name: entry.name, url: entry.url, duplicate: false })),
        identities: [
          { identity_key: 'v1:host:docs.acme.test', eligible_for_new_site: false, owner: 'source' },
          { identity_key: 'v1:github:acme/docs', eligible_for_new_site: true, owner: selectedIdentity === 'v1:github:acme/docs' ? 'new_site' : 'source' },
        ],
        tags: ['docs'],
      })
    })
    const executeSiteSplit = vi.fn(async (_siteID: string, _request: SiteSplitRequest) => ok({ source_site_id: splitDetail.id, source_revision: 4, new_site_id: '88888888-8888-8888-8888-888888888888', new_site_revision: 1, moved_entries: 1 }))
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getSites: vi.fn(async () => ok({ items: [site], total: 1, page: 1, limit: 60 })),
      getSite: vi.fn(async () => ok(splitDetail)),
      previewSiteSplit,
      executeSiteSplit,
    } as unknown as ReaderClient
    await openSplitDialog(client)

    expect(screen.getByLabelText('新网站名称')).toHaveValue('')
    expect(screen.getByLabelText('简介')).toHaveValue('')
    expect(screen.getByLabelText('主页')).toHaveValue('')
    expect(screen.getByLabelText('图标')).toHaveValue('')
    expect(screen.getByLabelText('用户备注')).toHaveValue('')

    fireEvent.click(screen.getByRole('checkbox', { name: /API reference/ }))
    fireEvent.change(screen.getByLabelText('新网站名称'), { target: { value: '   ' } })
    fireEvent.click(screen.getByRole('button', { name: '预览拆分' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('名称不能为空')
    expect(previewSiteSplit).not.toHaveBeenCalled()

    fireEvent.change(screen.getByLabelText('新网站名称'), { target: { value: 'Acme API' } })
    fireEvent.change(screen.getByLabelText('简介'), { target: { value: 'Independent API docs' } })
    fireEvent.change(screen.getByLabelText('主页'), { target: { value: 'ftp://docs.acme.test' } })
    fireEvent.change(screen.getByLabelText('图标'), { target: { value: 'https://docs.acme.test/icon.png' } })
    fireEvent.change(screen.getByLabelText('用户备注'), { target: { value: 'Owned by docs team' } })
    fireEvent.click(screen.getByRole('button', { name: '预览拆分' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('HTTP(S)')
    expect(previewSiteSplit).not.toHaveBeenCalled()

    fireEvent.change(screen.getByLabelText('主页'), { target: { value: 'https://docs.acme.test' } })
    fireEvent.click(screen.getByRole('button', { name: '预览拆分' }))
    await screen.findByRole('heading', { name: '最终拆分' })
    expect(previewSiteSplit).toHaveBeenCalledWith(splitDetail.id, {
      expected_revision: splitDetail.revision,
      entry_ids: ['44444444-4444-4444-4444-444444444444'],
      name: 'Acme API',
      primary_entry_id: '44444444-4444-4444-4444-444444444444',
      intro: 'Independent API docs',
      homepage_url: 'https://docs.acme.test',
      icon_url: 'https://docs.acme.test/icon.png',
      user_note: 'Owned by docs team',
    })

    fireEvent.change(screen.getByLabelText('站点身份'), { target: { value: 'v1:github:acme/docs' } })
    expect(screen.queryByRole('heading', { name: '最终拆分' })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '更新预览' }))
    await screen.findByRole('heading', { name: '最终拆分' })
    expect(screen.getByText('v1:github:acme/docs：新网站')).toBeInTheDocument()
    const payloadFromFinalPreview = finalPreviewPayload
    fireEvent.click(screen.getByRole('button', { name: '确认拆分' }))
    await waitFor(() => expect(executeSiteSplit).toHaveBeenCalledTimes(1))
    expect(executeSiteSplit.mock.calls[0]?.[0]).toBe(splitDetail.id)
    expect(executeSiteSplit.mock.calls[0]?.[1]).toBe(payloadFromFinalPreview)
  })

  it('stale revision 后保留全部未提交输入并要求重新 preview', async () => {
    const previewSiteSplit = vi.fn(async (_siteID: string, request: SiteSplitRequest) => ok<SiteSplitPreviewResponse>({
      source_site_id: splitDetail.id,
      source_revision: request.expected_revision,
      payload: request,
      entries: [{ id: splitDetail.entries[1].id, site_id: splitDetail.id, link_id: splitDetail.entries[1].link_id, name: splitDetail.entries[1].name, url: splitDetail.entries[1].url, duplicate: false }],
      identities: [],
      tags: [],
    }))
    const executeSiteSplit = vi.fn(async () => err({ kind: 'other', status: 409, errorCode: 'site_merge_conflict', message: 'site was changed' }))
    const onToast = vi.fn()
    const refreshedSplitDetail = { ...splitDetail, revision: splitDetail.revision + 1 }
    let detailReads = 0
    const getSite = vi.fn(async () => {
      detailReads += 1
      return ok(detailReads === 1 ? splitDetail : refreshedSplitDetail)
    })
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getSites: vi.fn(async () => ok({ items: [site], total: 1, page: 1, limit: 60 })),
      getSite,
      previewSiteSplit,
      executeSiteSplit,
    } as unknown as ReaderClient
    await openSplitDialog(client, onToast)

    fireEvent.click(screen.getByRole('checkbox', { name: /API reference/ }))
    fireEvent.change(screen.getByLabelText('新网站名称'), { target: { value: 'Unsubmitted draft' } })
    fireEvent.change(screen.getByLabelText('简介'), { target: { value: 'Keep this intro' } })
    fireEvent.change(screen.getByLabelText('主页'), { target: { value: 'https://draft.acme.test' } })
    fireEvent.change(screen.getByLabelText('图标'), { target: { value: 'https://draft.acme.test/icon.png' } })
    fireEvent.change(screen.getByLabelText('用户备注'), { target: { value: 'Keep this note' } })
    fireEvent.click(screen.getByRole('button', { name: '预览拆分' }))
    await screen.findByRole('heading', { name: '最终拆分' })
    fireEvent.click(screen.getByRole('button', { name: '确认拆分' }))

    await waitFor(() => expect(onToast).toHaveBeenCalledWith(expect.stringContaining('输入已保留'), 'alert'))
    await waitFor(() => expect(getSite).toHaveBeenCalledTimes(2))
    expect(screen.getByRole('checkbox', { name: /API reference/ })).toBeChecked()
    expect(screen.getByLabelText('新网站名称')).toHaveValue('Unsubmitted draft')
    expect(screen.getByLabelText('简介')).toHaveValue('Keep this intro')
    expect(screen.getByLabelText('主页')).toHaveValue('https://draft.acme.test')
    expect(screen.getByLabelText('图标')).toHaveValue('https://draft.acme.test/icon.png')
    expect(screen.getByLabelText('用户备注')).toHaveValue('Keep this note')
    expect(screen.getByLabelText('新网站主入口')).toHaveValue(splitDetail.entries[1].id)
    expect(screen.queryByRole('heading', { name: '最终拆分' })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '更新预览' }))
    await waitFor(() => expect(previewSiteSplit).toHaveBeenCalledTimes(2))
    expect(previewSiteSplit).toHaveBeenLastCalledWith(splitDetail.id, {
      expected_revision: refreshedSplitDetail.revision,
      entry_ids: [splitDetail.entries[1].id],
      name: 'Unsubmitted draft',
      primary_entry_id: splitDetail.entries[1].id,
      intro: 'Keep this intro',
      homepage_url: 'https://draft.acme.test',
      icon_url: 'https://draft.acme.test/icon.png',
      user_note: 'Keep this note',
    })
  })
})

// 六个 Dialog 迁到 ReaderDialog 后，外壳只剩 props：关闭规则、busy 封锁和初始焦点
// 都由外壳执行。这些用例把每个调用方声明的规则钉在这里，防止后续统一成一种。
describe('SitesView 对话框外壳', () => {
  function backdropOf(dialog: HTMLElement): HTMLElement {
    const element = dialog.closest('.reader-dialog-backdrop')
    if (!(element instanceof HTMLElement)) throw new Error('expected dialog backdrop to exist')
    return element
  }

  it('「合并网站」空闲时按下 backdrop 关闭', async () => {
    const fixture = capabilityClient()
    render(sitesElement(fixture.client, enabledReaderCapabilityLease()))
    await openCapabilityDetail()

    fireEvent.click(screen.getByRole('button', { name: '合并网站' }))
    const dialog = await screen.findByRole('dialog', { name: '合并到 Acme Docs' })

    fireEvent.mouseDown(backdropOf(dialog))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '合并到 Acme Docs' })).not.toBeInTheDocument())
  })

  it('「拆分网站」空闲时按下 backdrop 关闭，表单仍由 submit 触发预览', async () => {
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getSites: vi.fn(async () => ok({ items: [site], total: 1, page: 1, limit: 60 })),
      getSite: vi.fn(async () => ok(splitDetail)),
    } as unknown as ReaderClient
    await openSplitDialog(client)
    const dialog = await screen.findByRole('dialog', { name: '拆分网站' })

    expect(within(dialog).getByRole('button', { name: '预览拆分' })).toHaveAttribute('type', 'submit')
    expect(within(dialog).getByRole('button', { name: '预览拆分' }).closest('form')).not.toBeNull()

    fireEvent.mouseDown(backdropOf(dialog))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '拆分网站' })).not.toBeInTheDocument())
  })

  it('「编辑网站资料」聚焦名称输入框，按下 backdrop 与 Escape 都不关闭', async () => {
    const fixture = capabilityClient()
    render(sitesElement(fixture.client, enabledReaderCapabilityLease()))
    await openCapabilityDetail()

    fireEvent.click(screen.getByRole('button', { name: '编辑网站资料' }))
    const dialog = await screen.findByRole('dialog', { name: '编辑网站资料' })
    expect(document.activeElement).toBe(within(dialog).getByLabelText('名称'))

    fireEvent.mouseDown(backdropOf(dialog))
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.getByRole('dialog', { name: '编辑网站资料' })).toBeInTheDocument()
  })

  it('「编辑入口」聚焦名称输入框，按下 backdrop 与 Escape 都不关闭', async () => {
    const fixture = capabilityClient()
    render(sitesElement(fixture.client, enabledReaderCapabilityLease()))
    await openCapabilityDetail()

    fireEvent.click(screen.getByRole('button', { name: '编辑入口' }))
    const dialog = await screen.findByRole('dialog', { name: '编辑入口' })
    expect(document.activeElement).toBe(within(dialog).getByLabelText('名称'))

    fireEvent.mouseDown(backdropOf(dialog))
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.getByRole('dialog', { name: '编辑入口' })).toBeInTheDocument()
  })

  it('「移到阅读」聚焦取消按钮，Tab 在对话框内循环，Escape 关闭', async () => {
    const fixture = capabilityClient()
    render(sitesElement(fixture.client, enabledReaderCapabilityLease()))
    await openCapabilityDetail()

    fireEvent.click(screen.getByRole('button', { name: '移到阅读' }))
    const dialog = await screen.findByRole('dialog', { name: '移到阅读' })
    const close = within(dialog).getByRole('button', { name: '关闭' })
    const cancel = within(dialog).getByRole('button', { name: '取消' })
    const confirm = within(dialog).getByRole('button', { name: '确认转换' })
    expect(document.activeElement).toBe(cancel)

    confirm.focus()
    fireEvent.keyDown(confirm, { key: 'Tab' })
    expect(document.activeElement).toBe(close)
    fireEvent.keyDown(close, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(confirm)

    fireEvent.keyDown(document, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '移到阅读' })).not.toBeInTheDocument())
  })

  it('转换进行中，关闭按钮、Escape 和 backdrop 三条路径都关不掉「移到阅读」', async () => {
    let release: ((value: unknown) => void) | undefined
    const convertLink = vi.fn(() => new Promise((resolve) => { release = resolve }))
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getSites: vi.fn(async () => ok({ items: [site], total: 1, page: 1, limit: 30 })),
      getSite: vi.fn(async () => ok(detail)),
      getLink: vi.fn(async () => ok({ content_revision: 7 })),
      convertLink,
    } as unknown as ReaderClient
    render(sitesElement(client, enabledReaderCapabilityLease()))
    await openCapabilityDetail()

    fireEvent.click(screen.getByRole('button', { name: '移到阅读' }))
    const dialog = await screen.findByRole('dialog', { name: '移到阅读' })
    fireEvent.click(within(dialog).getByRole('button', { name: '确认转换' }))
    await within(dialog).findByRole('button', { name: '转换中' })

    const close = within(dialog).getByRole('button', { name: '关闭' })
    expect(close).toBeDisabled()
    expect(within(dialog).getByRole('button', { name: '取消' })).toBeDisabled()
    fireEvent.click(close)
    fireEvent.keyDown(document, { key: 'Escape' })
    fireEvent.mouseDown(backdropOf(dialog))
    expect(screen.getByRole('dialog', { name: '移到阅读' })).toBeInTheDocument()

    release?.(ok({}))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '移到阅读' })).not.toBeInTheDocument())
    expect(convertLink).toHaveBeenCalledTimes(1)
  })
})
