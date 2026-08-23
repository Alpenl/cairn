import 'fake-indexeddb/auto'

import { createHash } from 'node:crypto'
import { StrictMode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MainView } from './MainView'
import { err, ok } from '@webtag/api'
import type { ApiError, ApiResult } from '@webtag/api'
import type {
  IdentityBoundReaderClient,
  ListLinksParams,
  ReaderActivityRequestOptions,
  ReaderClient,
} from '../lib/api/client'
import type {
  DomainTreeSummaryEnvelope,
  GroupedSearchResponse,
  LinkResponse,
  LinkContentResponse,
  PaginatedLinksResponse,
  ReaderHomeResponse,
  ReaderInboxResponse,
  ReaderInboxListItemResponse,
  ReaderNoteResponse,
  CapabilitiesResponse,
  ReaderThoughtResponse,
  TagCountResponse,
  TranslationListResponse,
  TranslationResponse,
} from '../lib/api/types'
import {
  ownedDatabaseName,
  ownedStorageKey,
  writeOwnedStorage,
} from '../lib/storage-ownership'
import { makeLink } from '../test/fixtures'
import { IdentityLease, readerIdentity } from '../lib/identity'
import type { Annotation } from '../lib/annotations'
import {
  commitAnnotationOperation,
  readAnnotationSnapshot,
  type AnnotationTarget,
} from '../lib/user-data/annotation-store'
import type {
  AnnotationAddOperationInput,
  SavedContentAnnotationBlockKey,
} from '../lib/user-data/annotation-types'
import { resetUserDataDatabaseHandle } from '../lib/user-data/idb'
import { SavedArticleDocumentController } from '../lib/article/document'
import { resourceStore } from '../lib/cache/store'
import { linkDetailCacheKey } from '../lib/cache/keys'
import { ENABLED_READER_CAPABILITIES } from '../test/capabilities'

type TestMainViewProps = Omit<React.ComponentProps<typeof MainView>, 'client'> & {
  readonly client: ReaderClient
}

const DEFAULT_MAIN_VIEW_CAPABILITIES: CapabilitiesResponse = {
  ...ENABLED_READER_CAPABILITIES,
  reader: {
    ...ENABLED_READER_CAPABILITIES.reader,
    activity: false,
  },
}

function TestMainView(props: TestMainViewProps) {
  const lease = readerIdentity.activeLease
  if (!lease) throw new Error('test identity lease is not active')
  const identityAwareClient = props.client as unknown as {
    captureIdentity?: ReaderClient['captureIdentity']
    identityLease?: IdentityLease
  }
  identityAwareClient.captureIdentity ??=
    (logicalKey: string) => lease.captureOwnership(logicalKey)
  Object.defineProperty(identityAwareClient, 'identityLease', {
    configurable: true,
    value: lease,
  })
  const inboxClient = props.client as unknown as {
    listInbox?: ReaderClient['listInbox']
  }
  inboxClient.listInbox ??= vi.fn(async () => ok({ items: [], active_count: 0, expired_count: 0 }))
  const activityClient = props.client as unknown as {
    getReaderActivity?: ReaderClient['getReaderActivity']
  }
  activityClient.getReaderActivity ??= vi.fn(async (_limit, options) => ok({
    kind: options?.kind ?? 'all',
    tags: [],
    domains: [],
  }))
  const capabilities = Object.prototype.hasOwnProperty.call(props, 'capabilities')
    ? props.capabilities
    : DEFAULT_MAIN_VIEW_CAPABILITIES
  return <MainView {...props} capabilities={capabilities} client={props.client as IdentityBoundReaderClient} />
}

const NOTE_CAPABILITIES: CapabilitiesResponse = {
  library_kinds: true,
  site_library: true,
  site_management: true,
  site_advanced_management: true,
  archive_versions: [],
  reader_vnext: true,
  reader: {
    annotations: true,
    notes: true,
    inbox: true,
    todos: true,
    engagement: true,
    home: true,
    feed: true,
    ai: false,
    related_tags: true,
    activity: true,
    history: true,
    trash: true,
  },
}

const NOTE_CAPABILITIES_DISABLED: CapabilitiesResponse = {
  ...NOTE_CAPABILITIES,
  reader: { ...NOTE_CAPABILITIES.reader, notes: false },
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

async function deleteUserDataDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('failed to delete user-data database'))
    request.onblocked = () => reject(new Error('user-data database deletion was blocked'))
  })
}

function isSavedContentBlockKey(value: string): value is SavedContentAnnotationBlockKey {
  return value === 'content' || value.startsWith('content-')
}

async function seedAnnotation(
  lease: IdentityLease,
  linkId: string,
  target: AnnotationTarget,
  annotation: Annotation,
  opId: string,
): Promise<void> {
  const draft = {
    id: annotation.id,
    start: annotation.start,
    end: annotation.end,
    text: annotation.text,
    note: annotation.note,
    source: annotation.source,
    createdAt: annotation.createdAt,
    updatedAt: annotation.updatedAt,
  }
  let operation: AnnotationAddOperationInput
  switch (target.kind) {
    case 'saved-content': {
      if (!isSavedContentBlockKey(annotation.blockKey)) {
        throw new Error(`invalid saved-content block key ${annotation.blockKey}`)
      }
      operation = {
        kind: 'add',
        opId,
        linkId,
        target,
        draft: {
          ...draft,
          blockKey: annotation.blockKey,
        },
      }
      break
    }
    case 'summary':
      operation = { kind: 'add', opId, linkId, target, draft }
      break
    case 'note':
      operation = {
        kind: 'add',
        opId,
        linkId,
        target,
        draft: { ...draft, blockKey: annotation.blockKey },
      }
      break
  }
  const result = await commitAnnotationOperation(lease, operation)
  if (!result.ok || result.value.status === 'op-id-conflict') {
    throw new Error(`failed to seed annotation ${annotation.id}`)
  }
}

async function seedSavedContentAnnotation(
  contentRevision: number,
  annotationId: string,
  opId: string,
): Promise<void> {
  const lease = readerIdentity.activeLease
  if (!lease) throw new Error('test identity lease is not active')
  await seedAnnotation(
    lease,
    'L1',
    { kind: 'saved-content', contentRevision },
    {
      id: annotationId,
      blockKey: 'content',
      start: 0,
      end: 2,
      text: '正文',
      note: '',
      source: 'self',
      createdAt: 1,
      updatedAt: 1,
      sourceContentRevision: contentRevision,
    },
    opId,
  )
}

beforeEach(() => {
  window.history.replaceState({}, '', '/?view=reading')
})

afterEach(async () => {
  cleanup()
  // 先撤销 Reader identity，再等待 IndexedDB 删除。组件卸载后的异步回调不能在
  // 这段等待窗口里趁下一用例安装新 lease 后，把旧文章写入新缓存分区。
  readerIdentity.clear()
  resourceStore.deactivateIdentity()
  localStorage.clear()
  vi.restoreAllMocks()
  await deleteUserDataDatabase()
})

// LinkCard 迁到 ReaderPreviewCard 后，打开由卡片内的真实 button 承载：点击卡片根
// 节点不会冒泡进打开区域，所以这里返回那个 button（标题/摘要仍在它内部，文本断言不受影响）。
async function findCardByTitle(title: string): Promise<HTMLElement> {
  const matches = await screen.findAllByText(title)
  const card = matches.map((match) => match.closest<HTMLElement>('.reader-preview-card-main')).find(Boolean)
  if (!card) throw new Error(`expected link card for "${title}" to exist`)
  return card
}

// 原文默认折叠，详情里的原文断言都要先点开眉标（用户的真实路径）。
interface ExpandOriginalOptions {
  readonly settleDocument?: boolean
}

async function expandOriginal(options: ExpandOriginalOptions = {}): Promise<void> {
  const toggle = await waitFor(() => {
    const node = document.querySelector('[aria-controls="orig-content-body"]')
    if (!node) throw new Error('原文折叠开关尚未出现')
    return node
  })
  if (options.settleDocument) {
    await act(async () => {
      fireEvent.click(toggle)
      await new Promise((resolve) => setTimeout(resolve, 0))
    })
    return
  }
  fireEvent.click(toggle)
}

function selectRenderedText(node: Node): void {
  const range = document.createRange()
  range.selectNodeContents(node)
  Object.defineProperty(range, 'getBoundingClientRect', {
    value: () => new DOMRect(20, 20, 160, 24),
  })
  window.getSelection()?.removeAllRanges()
  window.getSelection()?.addRange(range)
  fireEvent(document, new Event('selectionchange'))
}

async function clickSelectionAction(node: () => Node, name: string): Promise<void> {
  selectRenderedText(node())
  await screen.findByRole('button', { name })
  selectRenderedText(node())
  fireEvent.click(screen.getByRole('button', { name }))
}

function revisionFloorKey(): string {
  const key = ownedStorageKey('revisionFloor')
  if (!key) throw new Error('revision floor storage requires an active identity')
  return key
}

function summarySourceHash(summary: string | null | undefined): string | null {
  return summary
    ? createHash('sha256').update(summary).digest('hex')
    : null
}

function summarySourceDigest(summary: string): ArrayBuffer {
  return Uint8Array.from(createHash('sha256').update(summary).digest()).buffer
}

function makeReaderNote(overrides: Partial<ReaderNoteResponse> = {}): ReaderNoteResponse {
  return {
    id: 'N1',
    title: '导航保护笔记',
    published_content: '已发布内容',
    published_revision: 1,
    draft_content: null,
    draft_revision: 1,
    draft_updated_at: null,
    deleted_at: null,
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    dirty: false,
    ...overrides,
  }
}

function makeReaderInbox(overrides: Partial<ReaderInboxResponse> = {}): ReaderInboxResponse {
  return {
    id: 'I1',
    url: 'https://example.com/inbox',
    source_kind: 'manual',
    title: '收件箱条目',
	body: '收件箱正文',
    note: '',
    summary: '收件箱摘要',
    suggested_tags: [],
    proposal_status: 'completed',
    tags: [],
    status: 'pending',
    metadata_revision: 1,
    expires_at: '2026-09-09T01:00:00Z',
    expired: false,
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    ...overrides,
  }
}

// The Inbox list endpoint returns cards, not detail records: the editor reads
// GET /api/inbox/{id} on demand. Tests project the same way the server does.
function makeReaderInboxCard(item: ReaderInboxResponse): ReaderInboxListItemResponse {
  return {
    id: item.id,
    url: item.url,
    source_kind: item.source_kind,
    title: item.title,
    preview: (item.summary ?? '') || item.note || item.body,
    tags: item.tags,
    status: item.status,
    metadata_revision: item.metadata_revision,
    expired: item.expired,
    updated_at: item.updated_at,
  }
}

function makeReaderThought(overrides: Partial<ReaderThoughtResponse> = {}): ReaderThoughtResponse {
  return {
    contract_version: 1,
    id: 'thought-home-1',
    host_kind: 'link',
    host_id: 'L1',
    link_id: 'L1',
    target: {},
    quote: null,
    body: '从首页打开的想法',
    source: 'self',
    deleted: false,
    last_sequence: 1,
    winner_key: { logical_clock: 1, device_id: 'device-home', op_id: 'op-home' },
    created_at: '2026-08-10T01:00:00Z',
    updated_at: '2026-08-10T01:00:00Z',
    lifecycle_status: 'active',
    lifecycle_reason: null,
    tombstoned_at: null,
    ...overrides,
  }
}

function makeClient(
  linkOverrides: Partial<LinkResponse> = {},
  detailOverrides: Partial<LinkResponse> = {},
  listItems?: LinkResponse[],
  noteItems: ReaderNoteResponse[] = [],
  searchResponse: GroupedSearchResponse = {
    reading: { items: [], total_hint: 0 },
    sites: { items: [], total_hint: 0 },
  },
  homeResponse?: ReaderHomeResponse,
): {
  client: ReaderClient
  getLink: ReturnType<typeof vi.fn>
  getContent: ReturnType<typeof vi.fn>
  saveContent: ReturnType<typeof vi.fn>
  replaceContent: ReturnType<typeof vi.fn>
  patchLinkMetadata: ReturnType<typeof vi.fn>
  getTranslations: ReturnType<typeof vi.fn>
  createTranslation: ReturnType<typeof vi.fn>
  createNote: ReturnType<typeof vi.fn>
  getCapabilities: ReturnType<typeof vi.fn>
  submitLink: ReturnType<typeof vi.fn>
  downloadArchiveV2: ReturnType<typeof vi.fn>
} {
  const link = makeLink({
    id: 'L1',
    title: '保存原文案例',
    summary: '这是一段摘要',
    content: undefined,
    // 必须非零。正文缓存键是 (id, content_revision)，而 revision 恒为 undefined 时
    // 读写两侧都退化成 `?rev=0`——任何「revision 没被正确传下去」的 bug 在这个退化点
    // 上都不可见，而线上保存过正文的链接 revision 必然非零，跑的正是没被覆盖的那一支。
    content_revision: 7,
    ...linkOverrides,
    // PF6：列表如实汇报 has_content（后端把它做成了生成列）。fixture 必须
    // 照这个来——此前列表恒报 false、由详情端补真值，那是被 PF6 消灭掉的形态，
    // 继续照旧会让测试跑在一个现实中不存在的响应组合上。
    //
    // 正文可能来自两处：detailOverrides（正常路径，正文只在详情/content 端点里）
    // 或 linkOverrides（少数用例直接把正文放进列表项）。任一有就是 true，且必须
    // 排在 `...linkOverrides` **之后**——排在前面会被那次展开里的 content 甩下。
    has_content:
      linkOverrides.has_content ?? Boolean(detailOverrides.content ?? linkOverrides.content),
  })
  const links = listItems ?? [link]
  // 后端契约：详情带 include_content=false，只回 has_content，不回正文；
  // 正文由 GET /api/links/{id}/content 单独取。fake 必须照这个来，否则测试
  // 会在一个现实中不存在的响应形态上通过。
  const detailOf = (id: string) =>
    makeLink({ ...(links.find((candidate) => candidate.id === id) ?? link), ...detailOverrides, id })
  const defaultGetLink = async (id: string) => {
    const full = detailOf(id)
    return ok({
      ...full,
      has_content: Boolean(full.content),
      content: undefined,
      content_document: undefined,
      content_format: undefined,
    })
  }
  const getLink = vi.fn(defaultGetLink)
  const defaultGetContent = async (id: string) => {
    const full = detailOf(id)
    if (!full.content) return { ok: false as const, error: { kind: 'other' as const, message: 'not found' } }
    return ok({
      link_id: id,
      content: full.content,
      content_document: full.content_document,
      content_format: full.content_format ?? ('plain' as const),
      fetcher_type: 'stored',
      content_source: full.content_source ?? 'fetched',
      content_revision: full.content_revision ?? 0,
    })
  }
  const getContent = vi.fn(defaultGetContent)

  // 写正文会递增 content_revision，两个写端点都把**自增后**的代次回给客户端。
  // fake 必须照做：让它们回 fixture 那个 7，就等于假设「写完代次没变」，而
  // 「客户端没接住新代次」这类 bug 恰恰只在代次真的变了的时候才会现形。
  const saveContent = vi.fn(async (id: string) =>
    ok({
      link_id: id,
      content: '这是保存后的原文',
      content_format: 'plain' as const,
      fetcher_type: 'basic',
      content_source: 'fetched' as const,
      content_revision: 8,
    }),
  )
  const replaceContent = vi.fn(async (id: string) =>
    ok({
      link_id: id,
      content: '这是重新抓取后的原文',
      content_document: '# 重新抓取\n\n这是重新抓取后的原文',
      content_format: 'markdown' as const,
      fetcher_type: 'basic',
      content_source: 'fetched' as const,
      content_revision: 9,
    }),
  )
  const getTranslations = vi.fn(async (id: string) => {
    const current = links.find((candidate) => candidate.id === id) ?? link
    return ok({
      current_content_revision: current.content_revision ?? 0,
      current_summary_source_hash: summarySourceHash(current.summary),
      items: [] as TranslationResponse[],
    })
  })
  const createTranslation = vi.fn(async (id: string) =>
    ok({
      id: 'T1',
      link_id: id,
      scope: 'full' as const,
      block_key: 'content',
      start_offset: 0,
      end_offset: 10,
      source_text: 'English body',
      translated_text: null,
      source_format: 'plain' as const,
      target_language: 'zh-CN' as const,
      status: 'pending' as const,
      model: null,
      error_msg: null,
      source_content_revision: link.content_revision ?? null,
      stale: false,
      created_at: '2026-07-15T00:00:00Z',
      updated_at: '2026-07-15T00:00:00Z',
    }),
  )
  const patchLinkMetadata = vi.fn(async (id: string) =>
    ok({ link_id: id, metadata_revision: 2 }),
  )

  // The vNext routes render real surfaces. Keep this characterization client
  // intentionally empty while still satisfying each surface's first request.
  const getHome = vi.fn(async () =>
    ok(homeResponse ?? {
      today: '2026-08-09',
      summary: '测试首页',
      counts: {},
      continue_reading: [],
      recent_thoughts: [],
      todos: [],
      stale: false,
    }),
  )
  const getReaderFeed = vi.fn(async () =>
    ok({ items: [], mode: 'recommended' as const }),
  )
  const listInbox = vi.fn(async () => ok({ items: [] }))
  const listCategories = vi.fn(async () => ok({ items: [] }))
  const listNotes = vi.fn(async () => ok({ items: noteItems, count: noteItems.length }))
  const getNote = vi.fn(async (id: string) => ok(noteItems.find((item) => item.id === id) ?? makeReaderNote({ id })))
  const createNote = vi.fn(async () => ok(makeReaderNote({ id: 'N-created', published_content: '', published_revision: 0, draft_content: null, draft_revision: 0, dirty: false })))
  const saveNoteDraft = vi.fn(async (id: string, request: { content: string; expected_draft_revision: number }) => {
    const current = noteItems.find((item) => item.id === id) ?? makeReaderNote({ id })
    return ok({ ...current, draft_content: request.content, draft_revision: request.expected_draft_revision + 1, dirty: true })
  })
  const listTodos = vi.fn(async () => ok({ items: [] }))
  const getCapabilities = vi.fn(async () => ok({
    ...NOTE_CAPABILITIES,
    reader: { ...NOTE_CAPABILITIES.reader, inbox: false },
  }))
  const submitLink = vi.fn(async () => ok({
    link_id: 'L-created',
    status: 'pending' as const,
  }))
  const downloadArchiveV2 = vi.fn(async () =>
    ok(new Blob(['{"schema_version":2}'], { type: 'application/json' })),
  )
  const getReaderActivity = vi.fn(async (
    _limit: number,
    options: ReaderActivityRequestOptions = {},
  ) => {
    const tagLastAt = new Map<string, string>()
    const domainLastAt = new Map<string, string>()
    for (const item of links) {
      if ((item.library_kind ?? 'reading') !== 'reading' || item.status !== 'done') continue
      for (const rawTag of item.tags) {
        const tag = rawTag.trim()
        if (!tag) continue
        const previous = tagLastAt.get(tag)
        if (!previous || item.created_at > previous) tagLastAt.set(tag, item.created_at)
      }
      const domain = item.domain?.trim() ?? ''
      if (domain) {
        const previous = domainLastAt.get(domain)
        if (!previous || item.created_at > previous) domainLastAt.set(domain, item.created_at)
      }
    }
    const kind = options.kind ?? 'all'
    return ok({
      kind,
      tags: kind === 'domain'
        ? []
        : [...tagLastAt].map(([tag, last_at]) => ({ tag, last_at })),
      domains: kind === 'tag'
        ? []
        : [...domainLastAt].map(([domain, last_at]) => ({ domain, last_at })),
    })
  })

  const client = {
    isIdentityCurrent: vi.fn(() => true),
    getLinks: vi.fn(async (params: ListLinksParams = {}) => {
      const visible = params.created_from && params.created_before
        ? links.filter((item) => (
          item.created_at >= params.created_from! && item.created_at < params.created_before!
        ))
        : links
      return ok({
        items: visible,
        total: visible.length,
        page: 1,
        limit: params.limit ?? 30,
      })
    }),
    getLink,
    getContent,
    getTags: vi.fn(async () => ok([])),
    getDomainSummaries: vi.fn(async () => ok({ domains: [], total: 0 })),
    getReaderActivity,
    saveContent,
    replaceContent,
    patchLinkMetadata,
    getTranslations,
    createTranslation,
    getHome,
    getReaderFeed,
    listInbox,
    listCategories,
    listNotes,
    getNote,
    createNote,
    saveNoteDraft,
    discardNoteDraft: vi.fn(async () => ok(true)),
    deleteNote: vi.fn(async (id: string) => ok({ host_kind: 'note' as const, host_id: id, state: 'trashed' as const, changed: true })),
    restoreNote: vi.fn(async (id: string) => ok({ host_kind: 'note' as const, host_id: id, state: 'live' as const, changed: true })),
    listTrash: vi.fn(async () => ok({ items: [], count: 0 })),
    purgeHost: vi.fn(async () => ok(true)),
    listTodos,
    getCapabilities,
    submitLink,
    downloadArchiveV2,
    // Settings reads the running Core identity off the existing /health probe.
    getHealth: vi.fn(async () => ok({
      status: 'ok' as const,
      version: '1.4.0',
      commit: '0123456789abcdef0123456789abcdef01234567',
      build_time: '2026-08-01T10:00:00Z',
    })),
    getInbox: vi.fn(async (id: string) => ok(makeReaderInbox({ id }))),
    searchLibrary: vi.fn(async () => ok(searchResponse)),
    testConnection: vi.fn(),
  } as unknown as ReaderClient

  return {
    client,
    getLink,
    getContent,
    saveContent,
    replaceContent,
    patchLinkMetadata,
    getTranslations,
    createTranslation,
    createNote,
    getCapabilities,
    submitLink,
    downloadArchiveV2,
  }
}

describe('MainView Reading sidebar authority', () => {
  function sidebarAllRow(): HTMLElement {
    const row = document.querySelector<HTMLElement>('#primary-navigation .sb-row')
    if (!row) throw new Error('missing sidebar all row')
    return row
  }

  function sidebarQueries() {
    const sidebar = document.querySelector<HTMLElement>('#primary-navigation')
    if (!sidebar) throw new Error('missing sidebar')
    return within(sidebar)
  }

  it('显式请求 reading 聚合，不显示 site-only bucket 或全库 total', async () => {
    const reading = makeLink({
      id: 'reading-1',
      title: 'Reading item',
      library_kind: 'reading',
      tags: ['reading-tag'],
      domain: 'reading.example',
    })
    const { client } = makeClient({}, {}, [reading])
    const scoped = client as unknown as {
      getTags: ReturnType<typeof vi.fn>
      getDomainSummaries: ReturnType<typeof vi.fn>
    }
    scoped.getTags = vi.fn(async (scope?: string) =>
      scope === 'reading'
        ? ok([{ tag: 'reading-tag', count: 1, reading_count: 1, site_count: 0 }])
        : ok([{ tag: 'site-only', count: 9 }]),
    )
    scoped.getDomainSummaries = vi.fn(async (scope?: string) =>
      scope === 'reading'
        ? ok({ library_kind: 'reading' as const, domains: [{ domain: 'reading.example', count: 1 }], total: 1 })
        : ok({ domains: [{ domain: 'site-only.example', count: 9 }], total: 9 }),
    )

    render(
      <TestMainView
        client={client}
        capabilities={ENABLED_READER_CAPABILITIES}
        onOpenSettings={() => {}}
      />,
    )

    await sidebarQueries().findByText('reading-tag')
    fireEvent.click(sidebarQueries().getByText('域名'))
    expect(sidebarQueries().getByText('reading.example')).toBeInTheDocument()
    expect(sidebarQueries().queryByText('site-only')).not.toBeInTheDocument()
    expect(sidebarQueries().queryByText('site-only.example')).not.toBeInTheDocument()
    expect(sidebarAllRow()).toHaveTextContent('1')
    expect(scoped.getTags).toHaveBeenCalledWith('reading', expect.any(Object))
    expect(scoped.getDomainSummaries).toHaveBeenCalledWith('reading', expect.any(Object))
  })

  it('旧后端聚合不可用且 corpus 不完整时只显示 links total', async () => {
    const partial = makeLink({
      id: 'partial-1',
      title: 'Partial item',
      tags: ['must-not-look-exact'],
      domain: 'partial.example',
    })
    const { client } = makeClient({}, {}, [partial])
    const scoped = client as unknown as {
      getLinks: ReturnType<typeof vi.fn>
      getTags: ReturnType<typeof vi.fn>
      getDomainSummaries: ReturnType<typeof vi.fn>
    }
    scoped.getLinks = vi.fn(async () => ok({ items: [partial], total: 2, page: 1, limit: 30 }))
    const unavailable = { ok: false as const, error: { kind: 'other' as const, message: 'scoped aggregate unavailable' } }
    scoped.getTags = vi.fn(async () => unavailable)
    scoped.getDomainSummaries = vi.fn(async () => unavailable)

    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await screen.findByRole('heading', { level: 1, name: 'Partial item' })
    await waitFor(() => expect(sidebarAllRow()).toHaveTextContent('2'))
    expect(sidebarQueries().queryByText('must-not-look-exact')).not.toBeInTheDocument()
    expect(sidebarQueries().queryByText('partial.example')).not.toBeInTheDocument()
  })

  it('旧后端聚合不可用时只对已证明完整的 corpus 做本地聚合', async () => {
    const complete = makeLink({
      id: 'complete-1',
      title: 'Complete item',
      library_kind: 'reading',
      tags: ['complete-tag'],
      domain: 'complete.example',
    })
    const { client } = makeClient({}, {}, [complete])
    const scoped = client as unknown as {
      getTags: ReturnType<typeof vi.fn>
      getDomainSummaries: ReturnType<typeof vi.fn>
    }
    const unavailable = { ok: false as const, error: { kind: 'other' as const, message: 'scoped aggregate unavailable' } }
    scoped.getTags = vi.fn(async () => unavailable)
    scoped.getDomainSummaries = vi.fn(async () => unavailable)

    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await sidebarQueries().findByText('complete-tag')
    fireEvent.click(sidebarQueries().getByText('域名'))
    expect(sidebarQueries().getByText('complete.example')).toBeInTheDocument()
    expect(sidebarAllRow()).toHaveTextContent('1')
  })
})

function linksPage(items: LinkResponse[]): ApiResult<PaginatedLinksResponse> {
  return ok({ items, total: items.length, page: 0, limit: 30 })
}

describe('MainView library synchronization', () => {
  it('waits for all resources, disables duplicate clicks, and only then reports success', async () => {
    window.history.replaceState({}, '', '/?view=reading')
    const links = deferred<ApiResult<PaginatedLinksResponse>>()
    const tags = deferred<ApiResult<TagCountResponse[]>>()
    const domains = deferred<ApiResult<DomainTreeSummaryEnvelope>>()
    const initial = makeLink({ id: 'sync-initial', title: '同步初始资料' })
    const { client } = makeClient({}, {}, [initial])
    client.getLinks = vi.fn()
      .mockResolvedValueOnce(linksPage([initial]))
      .mockReturnValueOnce(links.promise)
    client.getTags = vi.fn()
      .mockResolvedValueOnce(ok([]))
      .mockReturnValueOnce(tags.promise)
    client.getDomainSummaries = vi.fn()
      .mockResolvedValueOnce(ok({ domains: [], total: 1 }))
      .mockReturnValueOnce(domains.promise)
    const onRefreshCapabilities = vi.fn()

    render(<TestMainView client={client} onRefreshCapabilities={onRefreshCapabilities} onOpenSettings={() => {}} />)
    await screen.findByText('同步初始资料')
    const syncButton = screen.getByTitle('同步')
    fireEvent.click(syncButton)
    fireEvent.click(syncButton)

    expect(syncButton).toBeDisabled()
    expect(client.getLinks).toHaveBeenCalledTimes(2)
    expect(client.getTags).toHaveBeenCalledTimes(2)
    expect(client.getDomainSummaries).toHaveBeenCalledTimes(2)
    expect(onRefreshCapabilities).toHaveBeenCalledTimes(1)
    expect(screen.queryByText('资料库已同步')).not.toBeInTheDocument()

    await act(async () => {
      links.resolve(linksPage([initial]))
      await links.promise
    })
    expect(screen.queryByText('资料库已同步')).not.toBeInTheDocument()

    await act(async () => {
      tags.resolve(ok([]))
      await tags.promise
    })
    expect(screen.queryByText('资料库已同步')).not.toBeInTheDocument()

    await act(async () => {
      domains.resolve(ok({ domains: [], total: 1 }))
      await domains.promise
    })
    expect(await screen.findByText('资料库已同步')).toBeInTheDocument()
    expect(syncButton).not.toBeDisabled()
  })

  it('keeps successful data, names a single failed resource, and retries one non-overlapping group', async () => {
    window.history.replaceState({}, '', '/?view=reading')
    const initial = makeLink({ id: 'sync-old', title: '同步前资料' })
    const refreshed = makeLink({ id: 'sync-new', title: '已保留的成功资料' })
    const { client } = makeClient({}, {}, [initial])
    client.getLinks = vi.fn()
      .mockResolvedValueOnce(linksPage([initial]))
      .mockResolvedValueOnce(linksPage([refreshed]))
      .mockResolvedValueOnce(linksPage([refreshed]))
    client.getTags = vi.fn()
      .mockResolvedValueOnce(ok([]))
      .mockResolvedValueOnce(err({ kind: 'network-unreachable', message: 'offline' }))
      .mockResolvedValueOnce(ok([]))
    client.getDomainSummaries = vi.fn()
      .mockResolvedValueOnce(ok({ domains: [], total: 1 }))
      .mockResolvedValueOnce(ok({ domains: [{ domain: 'kept.example', count: 1 }], total: 1 }))
      .mockResolvedValueOnce(ok({ domains: [{ domain: 'kept.example', count: 1 }], total: 1 }))

    render(<TestMainView client={client} onOpenSettings={() => {}} />)
    await screen.findByText('同步前资料')
    fireEvent.click(screen.getByTitle('同步'))

    expect(await screen.findByText('资料库同步部分失败：tags')).toBeInTheDocument()
    expect(await screen.findByText('已保留的成功资料')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '重试资料库同步' }))

    expect(await screen.findByText('资料库已同步')).toBeInTheDocument()
    expect(client.getLinks).toHaveBeenCalledTimes(3)
    expect(client.getTags).toHaveBeenCalledTimes(3)
    expect(client.getDomainSummaries).toHaveBeenCalledTimes(3)
  })

  it('reports multiple failures once in stable resource order', async () => {
    window.history.replaceState({}, '', '/?view=reading')
    const initial = makeLink({ id: 'sync-multi', title: '多项失败资料' })
    const { client } = makeClient({}, {}, [initial])
    client.getLinks = vi.fn()
      .mockResolvedValueOnce(linksPage([initial]))
      .mockResolvedValueOnce(err({ kind: 'timeout', message: 'links timeout' }))
    client.getTags = vi.fn()
      .mockResolvedValueOnce(ok([]))
      .mockResolvedValueOnce(ok([]))
    client.getDomainSummaries = vi.fn()
      .mockResolvedValueOnce(ok({ domains: [], total: 1 }))
      .mockResolvedValueOnce(err({ kind: 'network-unreachable', message: 'domains offline' }))

    render(<TestMainView client={client} onOpenSettings={() => {}} />)
    await screen.findByText('多项失败资料')
    fireEvent.click(screen.getByTitle('同步'))

    expect(await screen.findByText('资料库同步部分失败：links、domains')).toBeInTheDocument()
    expect(screen.queryByText('资料库已同步')).not.toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: '重试资料库同步' })).toHaveLength(1)
  })

  it('does not report an old identity result after the active lease changes', async () => {
    window.history.replaceState({}, '', '/?view=reading')
    const links = deferred<ApiResult<PaginatedLinksResponse>>()
    const tags = deferred<ApiResult<TagCountResponse[]>>()
    const domains = deferred<ApiResult<DomainTreeSummaryEnvelope>>()
    const initial = makeLink({ id: 'sync-identity', title: '身份切换资料' })
    const { client } = makeClient({}, {}, [initial])
    client.getLinks = vi.fn()
      .mockResolvedValueOnce(linksPage([initial]))
      .mockReturnValueOnce(links.promise)
    client.getTags = vi.fn()
      .mockResolvedValueOnce(ok([]))
      .mockReturnValueOnce(tags.promise)
    client.getDomainSummaries = vi.fn()
      .mockResolvedValueOnce(ok({ domains: [], total: 1 }))
      .mockReturnValueOnce(domains.promise)

    render(<TestMainView client={client} onOpenSettings={() => {}} />)
    await screen.findByText('身份切换资料')
    fireEvent.click(screen.getByTitle('同步'))

    act(() => {
      const leaseB = readerIdentity.install({
        serverClientDataNamespace: 'sync-server-B',
        physicalNamespace: 'sync-physical-B',
      })
      resourceStore.activateIdentity(leaseB)
    })
    await act(async () => {
      links.resolve(linksPage([makeLink({ id: 'old-late' })]))
      tags.resolve(ok([]))
      domains.resolve(ok({ domains: [], total: 0 }))
      await Promise.all([links.promise, tags.promise, domains.promise])
    })

    expect(screen.queryByText('资料库已同步')).not.toBeInTheDocument()
    expect(screen.queryByText(/资料库同步部分失败/)).not.toBeInTheDocument()
  })
})

describe('MainView vNext route characterization', () => {
  it.each([
    ['surface=home', '今天', '?surface=home'],
    ['surface=feed', '混合 Feed', '?surface=feed'],
    ['view=pending', '收件箱', '?view=pending'],
    ['view=notes', '笔记', '?view=notes'],
    ['tool=todo', 'TODO', '?tool=todo'],
    ['tool=settings', '设置', '?tool=settings'],
  ])('%s renders its surface and keeps the canonical URL', async (query, heading, search) => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', `/?${query}`)

    try {
      const { client } = makeClient({ title: '现有阅读回落' })
      render(<TestMainView client={client} onOpenSettings={() => {}} />)

      expect(
        await screen.findByRole('heading', { level: 1, name: heading }),
      ).toBeInTheDocument()
      await waitFor(() => {
        expect(new URL(window.location.href).search).toBe(search)
      })
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('preserves global focus across Reading and Subscriptions until the user closes it', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=reading')

    try {
      const { client } = makeClient({ title: '专注状态文章' })
      render(<TestMainView client={client} onOpenSettings={() => {}} />)

      fireEvent.click(await screen.findByRole('button', { name: '专注模式' }))
      expect(screen.getByRole('button', { name: '退出专注模式' })).toBeInTheDocument()

      fireEvent.click(screen.getByRole('tab', { name: '订阅' }))
      await waitFor(() => expect(window.location.search).toBe('?view=subs'))
      fireEvent.click(screen.getByRole('tab', { name: '阅读' }))

      expect(await screen.findByRole('button', { name: '退出专注模式' })).toBeInTheDocument()
      fireEvent.click(screen.getByRole('button', { name: '退出专注模式' }))
      expect(screen.getByRole('button', { name: '专注模式' })).toBeInTheDocument()
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('commits the Home recent-thought CTA through the live canonical route and restores it after remount', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?surface=home')
    const home: ReaderHomeResponse = {
      today: '2026-08-10',
      summary: '测试首页',
      counts: {},
      continue_reading: [],
      recent_thoughts: [makeReaderThought()],
      todos: [],
      stale: false,
    }

    try {
      const { client } = makeClient({}, {}, undefined, [], undefined, home)
      const mounted = render(<TestMainView client={client} onOpenSettings={() => {}} />)
      await screen.findByText('从首页打开的想法')
      fireEvent.click(screen.getByRole('button', { name: '查看全部想法' }))

      await waitFor(() => expect(window.location.search).toBe('?tool=history&thought_view=live'))
      expect(await screen.findByRole('heading', { level: 1, name: '想法' })).toBeInTheDocument()

      mounted.unmount()
      render(<TestMainView client={client} onOpenSettings={() => {}} />)
      expect(await screen.findByRole('heading', { level: 1, name: '想法' })).toBeInTheDocument()
      expect(window.location.search).toBe('?tool=history&thought_view=live')
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it.each([
    {
      label: '首页链接想法',
      thought: makeReaderThought({ body: '首页链接想法', host_kind: 'link', host_id: 'L-home', link_id: 'L-home' }),
      expectedSearch: '?view=reading&link_id=L-home',
    },
    {
      label: '首页笔记想法',
      thought: makeReaderThought({ body: '首页笔记想法', host_kind: 'note', host_id: 'N-home', link_id: null }),
      expectedSearch: '?view=notes&note_id=N-home',
    },
    {
      label: '首页收件箱想法',
      thought: makeReaderThought({ body: '首页收件箱想法', host_kind: 'inbox', host_id: 'I-home', link_id: null }),
      expectedSearch: '?view=pending&inbox_id=I-home',
    },
  ])('commits $label through its canonical URL', async ({ label, thought, expectedSearch }) => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?surface=home')
    const home: ReaderHomeResponse = {
      today: '2026-08-10',
      summary: '测试首页',
      counts: {},
      continue_reading: [],
      recent_thoughts: [thought],
      todos: [],
      stale: false,
    }

    try {
      const { client } = makeClient({}, {}, undefined, [], undefined, home)
      render(<TestMainView client={client} onOpenSettings={() => {}} />)
      const row = (await screen.findByText(label)).closest('li')
      if (!row) throw new Error(`missing Home thought row for ${label}`)
      fireEvent.click(within(row).getByRole('button', { name: '回到来源' }))

      await waitFor(() => expect(window.location.search).toBe(expectedSearch))
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })
})

describe('MainView Command-K canonical navigation', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  async function runCommandSearch(
    response: GroupedSearchResponse,
    expectedSearch: string,
    label: string,
    options: { readonly links?: LinkResponse[]; readonly notes?: ReaderNoteResponse[] } = {},
  ): Promise<void> {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=reading')
    vi.useFakeTimers()
    try {
      const { client } = makeClient({}, {}, options.links, options.notes, response)
      render(<TestMainView client={client} onOpenSettings={() => {}} />)
      fireEvent.click(screen.getByText('搜索链接'))
      const input = screen.getByPlaceholderText('搜索标题、摘要、域名… 输入 # 搜标签')
      fireEvent.change(input, { target: { value: '命中' } })
      await act(async () => { await vi.advanceTimersByTimeAsync(300) })
      const commandLabel = screen.getAllByText(label)
        .find((node) => node.closest('.cmdk-item'))
      if (!commandLabel) throw new Error(`expected Command-K result ${label}`)
      await act(async () => {
        fireEvent.click(commandLabel.closest('.cmdk-item') as HTMLElement)
      })
      expect(window.location.search).toBe(expectedSearch)
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  }

  it('routes a link hit through the canonical reading target', async () => {
    const link = makeLink({ id: 'L-command', title: '命中链接', has_content: false })
    await runCommandSearch(
      { reading: { items: [link], total_hint: 1 }, sites: { items: [], total_hint: 0 } },
      '?view=reading&link_id=L-command',
      '命中链接',
      { links: [link] },
    )
  })

  it('routes a thought hosted by a link through the same canonical link target', async () => {
    const link = makeLink({ id: 'L-thought', title: '想法出处', has_content: false })
    await runCommandSearch(
      {
        reading: { items: [], total_hint: 0 },
        sites: { items: [], total_hint: 0 },
        thoughts: {
          items: [{ id: 'T-link', host_kind: 'link', host_id: link.id, link_id: link.id, snippet: '命中想法', updated_at: '2026-08-10T01:00:00Z' }],
          total_hint: 1,
        },
      },
      `?view=reading&link_id=${link.id}`,
      '命中想法',
      { links: [link] },
    )
  })

  it('routes a thought hosted by a note with note_id', async () => {
    const note = makeReaderNote({ id: 'N-command', title: '命中笔记' })
    await runCommandSearch(
      {
        reading: { items: [], total_hint: 0 },
        sites: { items: [], total_hint: 0 },
        thoughts: {
          items: [{ id: 'T-note', host_kind: 'note', host_id: note.id, snippet: '命中笔记想法', updated_at: '2026-08-10T01:00:00Z' }],
          total_hint: 1,
        },
      },
      `?view=notes&note_id=${note.id}`,
      '命中笔记想法',
      { notes: [note] },
    )
  })

  it('routes a thought hosted by an inbox with inbox_id', async () => {
    await runCommandSearch(
      {
        reading: { items: [], total_hint: 0 },
        sites: { items: [], total_hint: 0 },
        thoughts: {
          items: [{ id: 'T-inbox', host_kind: 'inbox', host_id: 'I-command', snippet: '命中收件箱想法', updated_at: '2026-08-10T01:00:00Z' }],
          total_hint: 1,
        },
      },
      '?view=pending&inbox_id=I-command',
      '命中收件箱想法',
    )
  })

  it('executes the single pending command through the canonical pending route', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=reading')
    try {
      const { client } = makeClient()
      render(<TestMainView client={client} onOpenSettings={() => {}} />)
      fireEvent.click(screen.getByText('搜索链接'))
      const input = screen.getByPlaceholderText('搜索标题、摘要、域名… 输入 # 搜标签')
      fireEvent.change(input, { target: { value: '收件箱' } })
      const pendingCommands = screen.getAllByText('收件箱').filter((node) => node.closest('.cmdk-item'))
      expect(pendingCommands).toHaveLength(1)
      fireEvent.click(pendingCommands[0]!.closest('.cmdk-item') as HTMLElement)
      await waitFor(() => expect(window.location.search).toBe('?view=pending'))
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })
})

describe('MainView Inbox add-link routing', () => {
  it('navigates an Inbox-capable submission to its exact canonical inbox_id', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=reading')
    try {
      const { client, getCapabilities, submitLink } = makeClient()
      getCapabilities.mockResolvedValue(ok(NOTE_CAPABILITIES))
      submitLink.mockResolvedValue(ok({
        inbox_id: 'I-real-server-id',
        destination: 'inbox' as const,
        status: 'pending' as const,
      }))
      render(<TestMainView client={client} onOpenSettings={() => {}} />)

      fireEvent.click(screen.getByTitle('添加链接'))
      const input = screen.getByPlaceholderText('粘贴 https:// 链接，回车提交')
      fireEvent.change(input, { target: { value: 'https://example.com/new-inbox' } })
      fireEvent.submit(input.closest('form') as HTMLFormElement)

      await waitFor(() => expect(submitLink).toHaveBeenCalledWith({
        url: 'https://example.com/new-inbox',
        requested_library_kind: 'auto',
        destination: 'inbox',
      }))
      await waitFor(() => expect(window.location.search).toBe('?view=pending&inbox_id=I-real-server-id'))
      expect(await screen.findByRole('heading', { name: '收件箱条目' })).toBeInTheDocument()
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })
})

describe('MainView Notes navigation protection', () => {
  it('keeps Command-K link navigation and detail loading behind a failed async Notes save', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')
    try {
      const note = makeReaderNote({ id: 'N-command-guard' })
      const target = makeLink({ id: 'L-command-guard', title: '守卫目标链接', has_content: false })
      const { client, getLink } = makeClient({}, {}, [target], [note])
      const pendingSave = deferred<ApiResult<ReaderNoteResponse>>()
      const saveNoteDraft = client.saveNoteDraft as ReturnType<typeof vi.fn>
      saveNoteDraft.mockReturnValue(pendingSave.promise)
      render(<TestMainView client={client} capabilities={NOTE_CAPABILITIES} onOpenSettings={() => {}} />)

      const textarea = await screen.findByRole('textbox', { name: '笔记内容' })
      fireEvent.change(textarea, { target: { value: '命令导航前必须保留的逐字草稿' } })
      getLink.mockClear()
      fireEvent.click(screen.getByText('搜索链接'))
      const input = screen.getByPlaceholderText('搜索标题、摘要、域名… 输入 # 搜标签')
      fireEvent.change(input, { target: { value: '守卫目标链接' } })
      const result = (await screen.findAllByText('守卫目标链接'))
        .find((node) => node.closest('.cmdk-item'))
      if (!result) throw new Error('expected guarded Command-K link result')
      fireEvent.click(result.closest('.cmdk-item') as HTMLElement)

      await waitFor(() => expect(saveNoteDraft).toHaveBeenCalledTimes(1))
      expect(window.location.search).toBe('?view=notes')
      expect(getLink).not.toHaveBeenCalledWith(target.id)
      expect(screen.getByRole('textbox', { name: '笔记内容' })).toHaveValue('命令导航前必须保留的逐字草稿')

      await act(async () => {
        pendingSave.resolve(err({ kind: 'other', message: '保存失败，仍在当前笔记' }))
        await pendingSave.promise
      })
      expect(window.location.search).toBe('?view=notes')
      expect(getLink).not.toHaveBeenCalledWith(target.id)
      expect(screen.getByRole('textbox', { name: '笔记内容' })).toHaveValue('命令导航前必须保留的逐字草稿')
      expect(await screen.findByText('保存失败，仍在当前笔记')).toBeInTheDocument()
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('commits one Command-K link route only after the delayed Notes save succeeds', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')
    try {
      const note = makeReaderNote({ id: 'N-command-success' })
      const target = makeLink({ id: 'L-command-success', title: '延迟成功目标', has_content: false })
      const { client } = makeClient({}, {}, [target], [note])
      const pendingSave = deferred<ApiResult<ReaderNoteResponse>>()
      const saveNoteDraft = client.saveNoteDraft as ReturnType<typeof vi.fn>
      saveNoteDraft.mockReturnValue(pendingSave.promise)
      render(<TestMainView client={client} capabilities={NOTE_CAPABILITIES} onOpenSettings={() => {}} />)

      fireEvent.change(await screen.findByRole('textbox', { name: '笔记内容' }), {
        target: { value: '只保存一次再离开' },
      })
      fireEvent.click(screen.getByText('搜索链接'))
      const input = screen.getByPlaceholderText('搜索标题、摘要、域名… 输入 # 搜标签')
      fireEvent.change(input, { target: { value: '延迟成功目标' } })
      await screen.findAllByText('延迟成功目标')
      fireEvent.keyDown(input, { key: 'Enter' })

      await waitFor(() => expect(saveNoteDraft).toHaveBeenCalledTimes(1))
      expect(window.location.search).toBe('?view=notes')
      await act(async () => {
        pendingSave.resolve(ok(makeReaderNote({
          ...note,
          draft_content: '只保存一次再离开',
          draft_revision: note.draft_revision + 1,
          dirty: true,
        })))
        await pendingSave.promise
      })

      await waitFor(() => expect(window.location.search).toBe(`?view=reading&link_id=${target.id}`))
      expect(saveNoteDraft).toHaveBeenCalledTimes(1)
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('uses one guarded, identity-fenced intent for sidebar and Command-K note creation', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')
    try {
      const existing = makeReaderNote({ id: 'N-existing', published_content: '已发布内容', draft_revision: 1 })
      const created = makeReaderNote({ id: 'N-real-server-id', published_content: '', published_revision: 0, draft_content: null, draft_revision: 0, dirty: false })
      const pending = deferred<ApiResult<ReaderNoteResponse>>()
      const { client, createNote } = makeClient({}, {}, undefined, [existing])
      const getNote = client.getNote as ReturnType<typeof vi.fn>
      getNote.mockImplementation(async (id: string) => ok(id === created.id ? created : existing))
      createNote.mockReturnValue(pending.promise)
      render(<TestMainView client={client} capabilities={NOTE_CAPABILITIES} onOpenSettings={() => {}} />)

      const textarea = await screen.findByRole('textbox', { name: '笔记内容' })
      fireEvent.change(textarea, { target: { value: '先保存当前草稿' } })
      const createButton = screen.getByRole('button', { name: '新建笔记' })
      fireEvent.click(createButton)
      fireEvent.click(createButton)

      const saveNoteDraft = client.saveNoteDraft as ReturnType<typeof vi.fn>
      await waitFor(() => expect(saveNoteDraft).toHaveBeenCalledTimes(1))
      await waitFor(() => expect(createNote).toHaveBeenCalledTimes(1))
      expect(saveNoteDraft.mock.invocationCallOrder[0]).toBeLessThan(createNote.mock.invocationCallOrder[0])

      fireEvent.click(screen.getByText('搜索链接'))
      const commandInput = screen.getByPlaceholderText('搜索标题、摘要、域名… 输入 # 搜标签')
      fireEvent.keyDown(commandInput, { key: 'Enter' })
      fireEvent.keyDown(commandInput, { key: 'Enter' })
      expect(createNote).toHaveBeenCalledTimes(1)

      const [, options] = createNote.mock.calls[0]
      expect(options).toEqual({ idempotencyKey: expect.any(String) })
      await act(async () => { pending.resolve(ok(created)) })

      await waitFor(() => expect(window.location.search).toBe(`?view=notes&note_id=${created.id}`))
      expect(await screen.findByRole('textbox', { name: '笔记内容' })).toHaveFocus()
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it.each([
    ['unknown', undefined, '?view=reading', '搜索链接'],
    ['false', NOTE_CAPABILITIES_DISABLED, '?surface=home', '搜索链接'],
  ] as const)('does not mount Notes or call its API when Notes capability is %s', async (_name, capabilities, expectedSearch, commandLabel) => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')
    try {
      const { client, createNote } = makeClient({}, {}, undefined, [makeReaderNote()])
      const historyLength = window.history.length
      render(<TestMainView client={client} capabilities={capabilities} onOpenSettings={() => {}} />)
      expect(screen.queryByRole('textbox', { name: '笔记内容' })).not.toBeInTheDocument()
      expect(client.listNotes).not.toHaveBeenCalled()
      await waitFor(() => expect(window.location.search).toBe(expectedSearch))
      expect(window.history.length).toBe(historyLength)
      expect(screen.queryByRole('button', { name: '新建笔记' })).not.toBeInTheDocument()
      fireEvent.click(screen.getByText(commandLabel))
      expect(screen.queryByText('新建笔记')).not.toBeInTheDocument()
      expect(createNote).not.toHaveBeenCalled()
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('replaces an active Notes route and drops a late create response when Notes is revoked', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')
    try {
      const pending = deferred<ApiResult<ReaderNoteResponse>>()
      const created = makeReaderNote({
        id: 'N-late-after-revoke',
        published_content: '',
        published_revision: 0,
        draft_content: null,
        draft_revision: 0,
        dirty: false,
      })
      const { client, createNote } = makeClient({}, {}, undefined, [makeReaderNote({ id: 'N-existing' })])
      createNote.mockReturnValue(pending.promise)
      const historyLength = window.history.length
      const { rerender } = render(
        <TestMainView client={client} capabilities={NOTE_CAPABILITIES} onOpenSettings={() => {}} />,
      )

      const createButton = await screen.findByRole('button', { name: '新建笔记' })
      fireEvent.click(createButton)
      await waitFor(() => expect(createNote).toHaveBeenCalledTimes(1))
      expect(createButton).toBeDisabled()

      rerender(
        <TestMainView client={client} capabilities={NOTE_CAPABILITIES_DISABLED} onOpenSettings={() => {}} />,
      )

      await waitFor(() => expect(window.location.search).toBe('?surface=home'))
      expect(window.history.length).toBe(historyLength)
      expect(screen.queryByRole('button', { name: '新建笔记' })).not.toBeInTheDocument()
      expect(screen.queryByRole('textbox', { name: '笔记内容' })).not.toBeInTheDocument()

      await act(async () => {
        pending.resolve(ok(created))
        await pending.promise
      })

      expect(window.location.search).toBe('?surface=home')
      expect(window.history.length).toBe(historyLength)

      rerender(
        <TestMainView client={client} capabilities={NOTE_CAPABILITIES} onOpenSettings={() => {}} />,
      )
      await waitFor(() => expect(window.location.search).toBe('?surface=home'))
      expect(screen.queryByText(created.title)).not.toBeInTheDocument()
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('creates from a clean Note without issuing a draft write', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')
    try {
      const existing = makeReaderNote({ id: 'N-clean' })
      const created = makeReaderNote({ id: 'N-clean-created', published_content: '', published_revision: 0, draft_content: null, draft_revision: 0, dirty: false })
      const { client, createNote } = makeClient({}, {}, undefined, [existing])
      createNote.mockResolvedValue(ok(created))
      render(<TestMainView client={client} capabilities={NOTE_CAPABILITIES} onOpenSettings={() => {}} />)

      fireEvent.click(await screen.findByRole('button', { name: '新建笔记' }))

      await waitFor(() => expect(createNote).toHaveBeenCalledTimes(1))
      expect(client.saveNoteDraft).not.toHaveBeenCalled()
      await waitFor(() => expect(window.location.search).toBe(`?view=notes&note_id=${created.id}`))
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it.each([
    ['API failure', { kind: 'network-unreachable', message: '新建请求失败' } satisfies ApiError],
    ['malformed response', { kind: 'other', message: '响应体格式不符：ReaderNoteResponse' } satisfies ApiError],
  ])('keeps the current route and draft after %s', async (_name, createError) => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')
    try {
      const existing = makeReaderNote({ id: 'N-create-error', title: '保留当前笔记' })
      const { client, createNote } = makeClient({}, {}, undefined, [existing])
      createNote.mockResolvedValue(err(createError))
      render(<TestMainView client={client} capabilities={NOTE_CAPABILITIES} onOpenSettings={() => {}} />)

      const textarea = await screen.findByRole('textbox', { name: '笔记内容' })
      fireEvent.change(textarea, { target: { value: '失败后仍保留的草稿' } })
      fireEvent.click(screen.getByRole('button', { name: '新建笔记' }))

      await waitFor(() => expect(createNote).toHaveBeenCalledTimes(1))
      expect(window.location.search).toBe('?view=notes')
      expect(screen.getByRole('textbox', { name: '笔记内容' })).toHaveValue('失败后仍保留的草稿')
      expect(await screen.findByText(createError.message)).toBeInTheDocument()
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('does not create when the shared request-navigation guard is canceled', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=reading')
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    try {
      const { client, createNote } = makeClient(
        { has_content: true, content_revision: 7 },
        {
          content: '这是需要先保存的原文',
          content_format: 'plain',
          content_revision: 7,
        },
      )
      render(<TestMainView client={client} capabilities={NOTE_CAPABILITIES} onOpenSettings={() => {}} />)

      fireEvent.click(await findCardByTitle('保存原文案例'))
      await expandOriginal({ settleDocument: true })
      fireEvent.click(await screen.findByRole('button', { name: '编辑已保存原文' }))
      const editor = screen.getByRole('textbox', { name: '编辑已保存原文' })
      fireEvent.change(editor, { target: { value: '尚未保存的正文编辑' } })
      const routeBeforeCreate = window.location.search
      expect(routeBeforeCreate).toBe('?view=reading&link_id=L1')

      fireEvent.click(screen.getByText('搜索链接'))
      fireEvent.keyDown(screen.getByPlaceholderText('搜索标题、摘要、域名… 输入 # 搜标签'), { key: 'Enter' })

      await waitFor(() => expect(confirmSpy).toHaveBeenCalledWith('当前正文有未保存修改，确定放弃？'))
      expect(createNote).not.toHaveBeenCalled()
      expect(screen.getByRole('textbox', { name: '编辑已保存原文' })).toHaveValue('尚未保存的正文编辑')
      expect(window.location.search).toBe(routeBeforeCreate)
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('does not create or leave the current note when prepareToLeave fails', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')
    try {
      const { client, createNote } = makeClient({}, {}, undefined, [makeReaderNote({ id: 'N-save-failure' })])
      const saveNoteDraft = client.saveNoteDraft as ReturnType<typeof vi.fn>
      saveNoteDraft.mockResolvedValue(err({ kind: 'other', message: '保存失败' }))
      render(<TestMainView client={client} capabilities={NOTE_CAPABILITIES} onOpenSettings={() => {}} />)

      fireEvent.change(await screen.findByRole('textbox', { name: '笔记内容' }), { target: { value: '不能离开的草稿' } })
      fireEvent.click(screen.getByRole('button', { name: '新建笔记' }))

      await waitFor(() => expect(saveNoteDraft).toHaveBeenCalledTimes(1))
      expect(createNote).not.toHaveBeenCalled()
      expect(window.location.search).toBe('?view=notes')
      expect(screen.getByRole('textbox', { name: '笔记内容' })).toHaveValue('不能离开的草稿')
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('drops an old-identity create response without navigating or inserting a local note', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')
    try {
      const existing = makeReaderNote({ id: 'N-identity-current', title: '当前笔记' })
      const created = makeReaderNote({ id: 'N-identity-old', title: '旧身份笔记', published_content: '', published_revision: 0, draft_content: null, draft_revision: 0 })
      const { client, createNote } = makeClient({}, {}, undefined, [existing])
      const isIdentityCurrent = client.isIdentityCurrent as ReturnType<typeof vi.fn>
      createNote.mockImplementation(async () => {
        isIdentityCurrent.mockReturnValue(false)
        return ok(created)
      })
      render(<TestMainView client={client} capabilities={NOTE_CAPABILITIES} onOpenSettings={() => {}} />)

      fireEvent.click(await screen.findByRole('button', { name: '新建笔记' }))
      await waitFor(() => expect(createNote).toHaveBeenCalledTimes(1))
      await waitFor(() => expect(window.location.search).toBe('?view=notes'))
      expect(screen.queryByText('旧身份笔记')).not.toBeInTheDocument()
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('opens connection settings for a clean note state', async () => {
    window.history.replaceState({}, '', '/?view=notes')
    const onOpenSettings = vi.fn()
    const { client } = makeClient({}, {}, undefined, [makeReaderNote()])
    render(<TestMainView client={client} onOpenSettings={onOpenSettings} />)

    await screen.findByRole('textbox', { name: '笔记内容' })
    fireEvent.click(screen.getByTitle('连接设置'))

    await waitFor(() => expect(onOpenSettings).toHaveBeenCalledTimes(1))
    expect(window.location.search).toBe('?view=notes')
  })

  it('waits for the note CAS save before opening connection settings', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')
    try {
      const onOpenSettings = vi.fn()
      const { client } = makeClient({}, {}, undefined, [makeReaderNote()])
      const saveNoteDraft = client.saveNoteDraft as ReturnType<typeof vi.fn>
      render(<TestMainView client={client} onOpenSettings={onOpenSettings} />)

      fireEvent.change(await screen.findByRole('textbox', { name: '笔记内容' }), { target: { value: '保存后离开' } })
      fireEvent.click(screen.getByTitle('连接设置'))

      await waitFor(() => expect(saveNoteDraft).toHaveBeenCalledWith('N1', {
        content: '保存后离开', expected_draft_revision: 1,
      }))
      await waitFor(() => expect(onOpenSettings).toHaveBeenCalledTimes(1))
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('blocks an unmarked browser traversal before MainView consumes popstate without overwriting its target', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')

    try {
      const { client } = makeClient({}, {}, undefined, [makeReaderNote()])
      render(<TestMainView client={client} onOpenSettings={() => {}} />)

      const textarea = await screen.findByRole('textbox', { name: '笔记内容' })
      fireEvent.change(textarea, { target: { value: '遍历前的本地内容' } })
      await waitFor(() => {
        expect(screen.getByRole('button', { name: '立即保存' })).not.toBeDisabled()
      })

      act(() => {
        window.history.pushState({}, '', '/?view=reading')
        window.dispatchEvent(new PopStateEvent('popstate'))
      })

      await waitFor(() => expect(window.location.search).toBe('?view=reading'))
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('protects beforeunload only while the rendered note draft is dirty', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')

    try {
      const { client } = makeClient({}, {}, undefined, [makeReaderNote()])
      render(<TestMainView client={client} onOpenSettings={() => {}} />)

      const cleanEvent = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent
      window.dispatchEvent(cleanEvent)
      expect(cleanEvent.defaultPrevented).toBe(false)

      const textarea = await screen.findByRole('textbox', { name: '笔记内容' })
      fireEvent.change(textarea, { target: { value: '离页前的未保存内容' } })
      await waitFor(() => {
        expect(screen.getByRole('button', { name: '立即保存' })).not.toBeDisabled()
      })

      const dirtyEvent = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent
      window.dispatchEvent(dirtyEvent)
      expect(dirtyEvent.defaultPrevented).toBe(true)
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('keeps navigation prompt-free when a note draft is clean', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=notes')

    try {
      const { client } = makeClient({}, {}, undefined, [makeReaderNote()])
      render(<TestMainView client={client} onOpenSettings={() => {}} />)

      await screen.findByRole('textbox', { name: '笔记内容' })
      const confirmSpy = vi.spyOn(window, 'confirm')
      fireEvent.click(screen.getByRole('tab', { name: '阅读' }))

      await waitFor(() => expect(window.location.search).toBe('?view=reading'))
      expect(confirmSpy).not.toHaveBeenCalled()
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })
})

describe('MainView Inbox beforeunload protection', () => {
  it('covers every metadata field, saving, failure retention, and successful cleanup', async () => {
    const previousURL = window.location.href
    window.history.replaceState({}, '', '/?view=pending')
    try {
      let serverItem = makeReaderInbox({ id: 'I-beforeunload' })
      const { client } = makeClient()
      client.listInbox = vi.fn(async () => ok({
        items: [makeReaderInboxCard(serverItem)],
        active_count: 1,
        expired_count: 0,
      }))
      client.getInbox = vi.fn(async () => ok(serverItem))
      const patchInbox = vi.fn(async (
        _id: string,
        _revision: number,
        request: { title: string | null; body: string; note: string; summary: string | null; tags: string[] },
      ) => {
        serverItem = {
          ...serverItem,
          ...request,
          metadata_revision: serverItem.metadata_revision + 1,
        }
        return ok(serverItem)
      })
      client.patchInbox = patchInbox
      render(<TestMainView client={client} onOpenSettings={() => {}} />)

      await screen.findByRole('textbox', { name: '标题' })
      const beforeUnload = () => {
        const event = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent
        window.dispatchEvent(event)
        return event
      }
      expect(beforeUnload().defaultPrevented).toBe(false)

      const fields = [
        ['标题', '新标题'],
        ['正文', '新正文'],
        ['笔记', '新笔记'],
        ['摘要', '新摘要'],
        ['标签', '标签一 标签二'],
      ] as const
      for (const [name, value] of fields) {
        fireEvent.change(screen.getByRole('textbox', { name }), { target: { value } })
        expect(beforeUnload().defaultPrevented).toBe(true)
        fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
        await waitFor(() => expect(screen.getByRole('button', { name: '保存元数据' })).toBeDisabled())
        expect(beforeUnload().defaultPrevented).toBe(false)
      }

      const pendingSave = deferred<ApiResult<ReaderInboxResponse>>()
      patchInbox.mockImplementationOnce(() => pendingSave.promise)
      fireEvent.change(screen.getByRole('textbox', { name: '标题' }), {
        target: { value: '失败后保留的标题' },
      })
      fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
      await waitFor(() => expect(patchInbox).toHaveBeenCalledTimes(fields.length + 1))
      expect(beforeUnload().defaultPrevented).toBe(true)

      await act(async () => {
        pendingSave.resolve(err({ kind: 'network-unreachable', message: 'Inbox 保存失败' }))
        await pendingSave.promise
      })
      expect(await screen.findByRole('alert')).toHaveTextContent('Inbox 保存失败')
      expect(screen.getByRole('textbox', { name: '标题' })).toHaveValue('失败后保留的标题')
      expect(beforeUnload().defaultPrevented).toBe(true)

      fireEvent.click(screen.getByRole('button', { name: '保存元数据' }))
      await waitFor(() => expect(patchInbox).toHaveBeenCalledTimes(fields.length + 2))
      await waitFor(() => expect(beforeUnload().defaultPrevented).toBe(false))
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })
})

describe('MainView 保存原文', () => {
  // PF6 的量化指标：列表如实汇报之后，打开一条**已在列表里且已解析完成**的
  // 链接不再需要详情请求——冷启动因此从 5 个 API 降到 4 个。
  it('列表加载后自动打开首篇，且不再为它发详情请求', async () => {
    const { client, getLink } = makeClient({ title: '自动打开首篇' })
    const { container } = render(<TestMainView client={client} onOpenSettings={() => {}} />)

    expect(
      await screen.findByRole('heading', { level: 1, name: '自动打开首篇' }),
    ).toBeInTheDocument()
    await waitFor(() => {
      expect(container.querySelector('.summary-lead')).toHaveClass('md')
    })
    expect(getLink).not.toHaveBeenCalled()
    expect(container.querySelector('.body')).not.toHaveClass('mobile-detail-active')
  })

  it('链接仍在解析中时照常请求详情（字段还会变，值得一次权威读取）', async () => {
    const { client, getLink } = makeClient({ title: '解析中', status: 'pending' })
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await screen.findByRole('heading', { level: 1, name: '解析中' })
    await waitFor(() => expect(getLink).toHaveBeenCalledWith('L1'))
  })

  it('真实详情请求驱动 document detail 的 loading 与 error 状态', async () => {
    const { client, getLink } = makeClient({
      title: '详情资源状态案例',
      status: 'pending',
      content_revision: 7,
    })
    const detailError: ApiError = {
      kind: 'network-unreachable',
      message: 'detail endpoint offline',
    }
    let resolveDetail!: (value: { ok: false; error: ApiError }) => void
    const pendingDetail = new Promise<{ ok: false; error: ApiError }>((resolve) => {
      resolveDetail = resolve
    })
    getLink.mockImplementation(() => pendingDetail)

    const { container } = render(
      <TestMainView client={client} onOpenSettings={() => {}} />,
    )

    await screen.findByRole('heading', { level: 1, name: '详情资源状态案例' })
    await waitFor(() => expect(getLink).toHaveBeenCalledWith('L1'))
    // Let the newly selected article install its controller. `aria-busy` is
    // deliberately derived from the controller snapshot, not request-local UI state.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })
    expect(container.querySelector('.body')).toHaveAttribute('aria-busy', 'true')

    await act(async () => {
      resolveDetail({ ok: false, error: detailError })
      await pendingDetail
    })

    await waitFor(() => {
      expect(container.querySelector('.body')).not.toHaveAttribute('aria-busy')
    })
    expect(
      await screen.findByText('加载链接详情失败：detail endpoint offline'),
    ).toBeInTheDocument()
  })

  it('按当前可见列表顺序导航到下一篇并保留回到上一篇的入口', async () => {
    const first = makeLink({
      id: 'L1',
      title: '可见列表第一篇',
      created_at: '2026-07-16T00:00:00Z',
    })
    const second = makeLink({
      id: 'L2',
      title: '可见列表第二篇',
      created_at: '2026-07-15T00:00:00Z',
    })
    const { client } = makeClient({}, {}, [first, second])
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    fireEvent.click(await findCardByTitle('可见列表第一篇'))
    expect(
      await screen.findByRole('heading', { level: 1, name: '可见列表第一篇' }),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /上一条/ })).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '下一条：可见列表第二篇' }))

    expect(
      await screen.findByRole('heading', { level: 1, name: '可见列表第二篇' }),
    ).toBeInTheDocument()
    // PF6：两条都在列表里且已解析完成，因此导航不再触发详情请求。
    // 这条用例真正要守的是「导航到了第二篇、且回上一篇的入口出现了」。
    expect(screen.getByRole('button', { name: '上一条：可见列表第一篇' })).toBeInTheDocument()
  })

  it('列表静默刷新不会把 has_content 冲掉（折叠头不会变回「保存原文」）', async () => {
    const { client } = makeClient(
      {},
      { content: '这是之前已经保存的原文', content_format: 'plain' },
    )
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    fireEvent.click(await findCardByTitle('保存原文案例'))
    // PF6 之后「详情就绪」不再以 getLink 为标志——列表数据本身就够渲染。
    await waitFor(() =>
      expect(document.querySelector('[aria-controls="orig-content-body"]')).not.toBeNull(),
    )

    // 列表每 30 秒静默刷新一次；列表项的 has_content 恒为 false（列表不背正文），
    // 直接合并会把详情里的 true 冲掉，折叠头当场变回「保存原文」。这里不等真的
    // 30 秒，直接触发一次列表刷新（visibilitychange 走的是同一条刷新路径）。
    const before = (client.getLinks as unknown as { mock: { calls: unknown[] } }).mock.calls.length
    await act(async () => {
      document.dispatchEvent(new Event('visibilitychange'))
      await Promise.resolve()
    })
    await waitFor(() =>
      expect(
        (client.getLinks as unknown as { mock: { calls: unknown[] } }).mock.calls.length,
      ).toBeGreaterThan(before),
    )

    expect(document.querySelector('[aria-controls="orig-content-body"]')).not.toBeNull()
    expect(screen.queryByText('保存原文')).not.toBeInTheDocument()
  })

  it('打开链接时不请求原文，展开原文才请求', async () => {
    const { client, getContent } = makeClient(
      {},
      { content: '这是之前已经保存的原文', content_format: 'plain' },
    )
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    fireEvent.click(await findCardByTitle('保存原文案例'))
    await waitFor(() =>
      expect(document.querySelector('[aria-controls="orig-content-body"]')).not.toBeNull(),
    )

    // 详情已经渲染出来了，但正文一次都没请求过——这正是本次改动的目的。
    expect(getContent).not.toHaveBeenCalled()

    await expandOriginal({ settleDocument: true })

    await waitFor(() => expect(getContent).toHaveBeenCalledWith('L1'))
    expect(await screen.findByText('这是之前已经保存的原文')).toBeInTheDocument()
  })

  it('高代正文到达后保持 detail idle，直到同代权威详情到达', async () => {
    let resolveBody!: (value: ApiResult<LinkContentResponse>) => void
    let resolveDetail!: (value: ApiResult<LinkResponse>) => void
    const pendingBody = new Promise<ApiResult<LinkContentResponse>>((resolve) => {
      resolveBody = resolve
    })
    const pendingDetail = new Promise<ApiResult<LinkResponse>>((resolve) => {
      resolveDetail = resolve
    })
    const { client, getContent, getLink } = makeClient(
      {
        title: 'Revision seven title',
        summary: 'Revision seven summary',
        content_revision: 7,
      },
      { content: 'Revision seven body', content_format: 'plain' },
    )
    getContent.mockImplementation(() => pendingBody)
    getLink.mockImplementation(() => pendingDetail)

    const originalLoadBody = SavedArticleDocumentController.prototype.loadBody
    let observedController: SavedArticleDocumentController | null = null
    const observeController = (controller: SavedArticleDocumentController) => {
      observedController = controller
    }
    vi.spyOn(SavedArticleDocumentController.prototype, 'loadBody').mockImplementation(
      function (this: SavedArticleDocumentController, load) {
        observeController(this)
        return originalLoadBody.call(this, load)
      },
    )

    render(<TestMainView client={client} onOpenSettings={() => {}} />)
    await screen.findByRole('heading', { level: 1, name: 'Revision seven title' })
    await expandOriginal({ settleDocument: true })
    await waitFor(() => expect(getContent).toHaveBeenCalledWith('L1'))

    await act(async () => {
      resolveBody(ok({
        link_id: 'L1',
        content: 'Revision eight body',
        content_format: 'plain',
        fetcher_type: 'stored',
        content_source: 'fetched',
        content_revision: 8,
      }))
      await pendingBody
    })

    await waitFor(() => {
      expect(observedController?.getSnapshot()).toMatchObject({
        id: { contentRevision: 8 },
        detail: { status: 'idle' },
        body: {
          status: 'ready',
          revision: 8,
          data: { content: 'Revision eight body' },
        },
      })
    })
    expect(await screen.findByText('Revision eight body')).toBeInTheDocument()
    await waitFor(() => expect(getLink).toHaveBeenCalledWith('L1'))

    await act(async () => {
      resolveDetail(ok(makeLink({
        id: 'L1',
        title: 'Revision eight title',
        summary: 'Revision eight summary',
        has_content: true,
        content_revision: 8,
      })))
      await pendingDetail
    })

    await waitFor(() => {
      expect(observedController?.getSnapshot().detail).toMatchObject({
        status: 'ready',
        revision: 8,
        data: {
          title: 'Revision eight title',
          summary: 'Revision eight summary',
        },
      })
    })
  })

  // 用户报的那条，走完整链路：展开 → 折叠 → 切到订阅页（DetailPane 卸载）→
  // 切回来 → 再展开。这里跑的是真实的读侧：DetailPane 自己订阅缓存键，
  // MainView 提供 onLoadContent。
  //
  // 最后那三条断言**必须同步**（不能 await）。一 await，onLoadContent 内部那层
  // peek 就会在微任务里把正文补上，于是「渲染期 peek 坏掉」这种退化看不出来——
  // 两个机制互相掩护，测试恒绿。同步看到的差别是实打实的：渲染期 peek 生效时
  // loadContent 根本不会被调用，连一帧 loading 都没有。
  it('切走再回来展开原文，用缓存同步渲染，不再请求', async () => {
    const { client, getContent } = makeClient(
      {},
      { content: '这是之前已经保存的原文', content_format: 'plain' },
    )
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    fireEvent.click(await findCardByTitle('保存原文案例'))
    await expandOriginal({ settleDocument: true })
    expect(await screen.findByText('这是之前已经保存的原文')).toBeInTheDocument()
    expect(getContent).toHaveBeenCalledTimes(1)

    await expandOriginal() // 折叠
    fireEvent.click(screen.getByRole('tab', { name: '订阅' }))
    await waitFor(() =>
      expect(document.querySelector('[aria-controls="orig-content-body"]')).toBeNull(),
    )
    fireEvent.click(screen.getByRole('tab', { name: '阅读' }))
    await expandOriginal()

    expect(screen.getByText('这是之前已经保存的原文')).toBeInTheDocument()
    expect(screen.queryByText('读取原文中…')).not.toBeInTheDocument()
    expect(getContent).toHaveBeenCalledTimes(1)
  })

  it('重新进入页面后，点开链接会加载详情；原文默认折叠，展开后显示', async () => {
    const { client } = makeClient(
      {},
      {
        content: '这是之前已经保存的原文',
        content_document: '## 已保存原文\n\n这是之前已经保存的原文',
        content_format: 'markdown',
      },
    )
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const card = await findCardByTitle('保存原文案例')
    fireEvent.click(card)

    await waitFor(() =>
      expect(document.querySelector('[aria-controls="orig-content-body"]')).not.toBeNull(),
    )
    await expandOriginal({ settleDocument: true })
    expect(await screen.findByText('这是之前已经保存的原文')).toBeInTheDocument()
    expect(screen.queryByText('保存原文')).not.toBeInTheDocument()
  })

  it('保存成功后，当前详情立即显示原文', async () => {
    const { client, saveContent } = makeClient()
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const card = await findCardByTitle('保存原文案例')
    await waitFor(() => expect(resourceStore.has(linkDetailCacheKey('L1'))).toBe(true))
    fireEvent.click(card)

    fireEvent.click(await screen.findByText('保存原文'))

    await waitFor(() => expect(saveContent).toHaveBeenCalledWith('L1'))
    await expandOriginal()
    await waitFor(() => expect(screen.getByText('这是保存后的原文')).toBeInTheDocument())
    expect(screen.queryByText('保存原文')).not.toBeInTheDocument()
    expect(resourceStore.has(linkDetailCacheKey('L1'))).toBe(false)
  })

  // HIGH-2 回归：保存原文后，客户端必须**当场**接住响应里的新 content_revision。
  //
  // 不接住的后果不是慢一拍，而是文档身份错乱：active.content_revision 会停在
  // 旧值直到 useLinks 下次静默刷新，这段窗里的 selection/annotation command
  // 会携带旧 document identity，被拒绝或指向错误 durable target。
  //
  // 这里用「重新抓取的确认框」当探针：那个框只在**当前代次下**正文侧真有划线时
  // 才弹（MainView 的 hasContentAnnotations）。durable snapshot 落在保存后的代次 8 上，
  // 组件若仍以为自己在代次 7，正文划线就是不可见的，框也就不会弹。
  it('保存原文后立即采用响应里的新代次，正文划线不被判成失配', async () => {
    await seedSavedContentAnnotation(8, 'save-content-revision-8', 'seed:save-content-revision-8')
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const { client, saveContent } = makeClient()
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const card = await findCardByTitle('保存原文案例')
    fireEvent.click(card)
    fireEvent.click(await screen.findByText('保存原文'))
    await waitFor(() => expect(saveContent).toHaveBeenCalledWith('L1'))

    await expandOriginal()
    fireEvent.click(await screen.findByText('重新抓取'))

    await waitFor(() => expect(confirmSpy).toHaveBeenCalled())
  })

  // 回填新代次还不够：`patchKnownLink` 落的是**乐观补丁**，`useLinks` 在列表
  // 数据换引用时会 `setPatches({})` 把它整批清掉，随后合并 effect 让列表值赢。
  // 于是一份「在写操作之前就已发出」的列表响应落地时，会把代次拖回旧值——
  // 窗口内的正文划线照样被判失配丢掉，与 HIGH-2 同一种形状。
  //
  // 守的是 MainView 里 active 汇合点的 revisionFloor（按 link id 记下界）。
  // 代次由服务端单调自增，本机见过的最大值必然来自一次已落库的写，让它往回退
  // 在任何情况下都是错的。
  it('陈旧的列表响应不会把 content_revision 拖回旧值', async () => {
    // 代次 9 的 durable snapshot 必须在 render 之前写入：它与重抓后的
    // 新代次一致，与列表里的旧代次 7 不一致。
    await seedSavedContentAnnotation(9, 'stale-list-revision-9', 'seed:stale-list-revision-9')
    const stale = makeLink({
      id: 'L1',
      title: '陈旧列表案例',
      summary: '摘要',
      content: '旧正文',
      content_format: 'plain',
      content_revision: 7,
      has_content: true,
    })
    let listCalls = 0
    const replaceContent = vi.fn(async (id: string) =>
      ok({
        link_id: id,
        content: '重抓后的正文',
        content_format: 'plain' as const,
        fetcher_type: 'basic',
        content_revision: 9,
      }),
    )
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getLinks: vi.fn(async (params?: { limit?: number }) => {
        // 这两条用例全程停在默认选区 smart/all，所以每一次 getLinks 都是同一条
        // 列表流；真要改到其他选区，这里的“第二次”得跟着重新定义。
        listCalls += 1
        // 第二次起返回**同一个旧代次**，但 updated_at 变了。这一点是必须的：
        // resourceStore 只在内容真变时才换引用，不变的话 `setPatches({})` 根本
        // 不触发，整个场景复现不出来。
        const item = listCalls > 1 ? { ...stale, updated_at: '2026-06-11T00:00:00Z' } : stale
        return ok({
          items: [item],
          total: 1,
          page: 1,
          limit: params?.limit ?? 30,
        })
      }),
      getLink: vi.fn(async () => ok({ ...stale, content: undefined, content_document: undefined })),
      getContent: vi.fn(async (id: string) =>
        ok({
          link_id: id,
          content: '旧正文',
          content_format: 'plain' as const,
          fetcher_type: 'stored',
          content_source: 'fetched' as const,
          content_revision: 7,
        }),
      ),
      getTags: vi.fn(async () => ok([])),
      getDomainSummaries: vi.fn(async () => ok({ domains: [], total: 0 })),
      getTranslations: vi.fn(async () =>
        ok({
          current_content_revision: 7,
          current_summary_source_hash: summarySourceHash(stale.summary),
          items: [] as TranslationResponse[],
        }),
      ),
      createTranslation: vi.fn(),
      saveContent: vi.fn(),
      replaceContent,
      testConnection: vi.fn(),
    } as unknown as ReaderClient

    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await findCardByTitle('陈旧列表案例')
    await expandOriginal()

    // 第一次重抓：此刻代次是 7、durable snapshot 是 9，正文划线不可见，所以不弹确认框。
    fireEvent.click(await screen.findByText('重新抓取'))
    await waitFor(() => expect(replaceContent).toHaveBeenCalledWith('L1'))
    expect(confirmSpy).not.toHaveBeenCalled()

    // 让那份陈旧的列表响应落地（同步按钮走的就是 list.reload()）。
    fireEvent.click(document.querySelector('button[title="同步"]') as HTMLElement)
    await waitFor(() => expect(listCalls).toBeGreaterThan(1))
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    // 代次没被拖回 7，划线仍然可见 —— 第二次重抓必须弹确认框。
    fireEvent.click(await screen.findByText('重新抓取'))
    await waitFor(() => expect(confirmSpy).toHaveBeenCalled())
  })

  // 上一条守的是「开着这一篇不动，陈旧列表落地」。这一条守的是**切走再切回**：
  // openLink 的 settled 分支直接 `setActiveDetail(known)`，`known` 取自列表投影，
  // 代次照样是写操作之前的。
  //
  // 这条路径解释了为什么保护必须是「按 id 记下界」而不是「在合并点取 max」：
  // 中间隔了另一篇文章，activeDetail 早被 L2 覆盖，"当前持有的更大值"根本不存在。
  it('切走再切回同一篇时 content_revision 不回退', async () => {
    await seedSavedContentAnnotation(9, 'switch-back-revision-9', 'seed:switch-back-revision-9')
    const stale = makeLink({
      id: 'L1',
      title: '切回案例',
      summary: '摘要',
      content: '旧正文',
      content_format: 'plain',
      content_revision: 7,
      has_content: true,
      created_at: '2026-07-20T00:00:00Z',
    })
    const other = makeLink({
      id: 'L2',
      title: '中转的另一篇',
      summary: '另一篇的摘要',
      created_at: '2026-07-19T00:00:00Z',
    })
    let listCalls = 0
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getLinks: vi.fn(async (params?: { limit?: number }) => {
        // 这两条用例全程停在默认选区 smart/all，所以每一次 getLinks 都是同一条
        // 列表流；真要改到其他选区，这里的“第二次”得跟着重新定义。
        listCalls += 1
        // 第二次起代次仍是旧的 7，但 updated_at 变了——迫使 resourceStore 换引用，
        // `useLinks` 随之 `setPatches({})` 清掉写操作留下的乐观补丁。不这么做的话
        // 列表投影里还带着 9，切回来时根本没有可回退的东西，用例就是恒绿的。
        const item = listCalls > 1 ? { ...stale, updated_at: '2026-07-21T00:00:00Z' } : stale
        return ok({
          items: [item, other],
          total: 2,
          page: 1,
          limit: params?.limit ?? 30,
        })
      }),
      getLink: vi.fn(async (id: string) =>
        ok({ ...(id === 'L2' ? other : stale), content: undefined, content_document: undefined }),
      ),
      getContent: vi.fn(async (id: string) =>
        ok({
          link_id: id,
          content: '旧正文',
          content_format: 'plain' as const,
          fetcher_type: 'stored',
          content_source: 'fetched' as const,
          content_revision: 7,
        }),
      ),
      getTags: vi.fn(async () => ok([])),
      getDomainSummaries: vi.fn(async () => ok({ domains: [], total: 0 })),
      getTranslations: vi.fn(async (id: string) => {
        const current = id === 'L2' ? other : stale
        return ok({
          current_content_revision: 7,
          current_summary_source_hash: summarySourceHash(current.summary),
          items: [] as TranslationResponse[],
        })
      }),
      createTranslation: vi.fn(),
      saveContent: vi.fn(),
      replaceContent: vi.fn(async (id: string) =>
        ok({
          link_id: id,
          content: '重抓后的正文',
          content_format: 'plain' as const,
          fetcher_type: 'basic',
          content_revision: 9,
        }),
      ),
      testConnection: vi.fn(),
    } as unknown as ReaderClient

    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    fireEvent.click(await findCardByTitle('切回案例'))
    await expandOriginal()
    fireEvent.click(await screen.findByText('重新抓取'))
    await waitFor(() => expect(client.replaceContent).toHaveBeenCalledWith('L1'))
    expect(confirmSpy).not.toHaveBeenCalled()

    // 让陈旧列表落地，把乐观补丁冲掉：此后列表投影里的代次退回 7。
    fireEvent.click(document.querySelector('button[title="同步"]') as HTMLElement)
    await waitFor(() => expect(listCalls).toBeGreaterThan(1))
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    // 切到另一篇，再切回来——回来走的是 openLink 的 settled 分支。
    fireEvent.click(await findCardByTitle('中转的另一篇'))
    await screen.findByRole('heading', { level: 1, name: '中转的另一篇' })
    fireEvent.click(await findCardByTitle('切回案例'))
    await screen.findByRole('heading', { level: 1, name: '切回案例' })
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    await expandOriginal()
    fireEvent.click(await screen.findByText('重新抓取'))
    await waitFor(() => expect(confirmSpy).toHaveBeenCalled())
  })

  // 第三条代次守卫，也是最凶的一条：**刷新页面**。
  //
  // 列表缓存持久化在 IndexedDB，写操作之后它仍持有旧行；下界若只活在内存里，
  // 刷新一次就会 hydrate 出旧代次、同时丢掉下界，当前文档便会读取错误
  // revision 的 durable snapshot。所以下界必须落盘
  // （lib/cache/revision-floor.ts）。
  //
  // unmount + 重新 render 就是这个场景：内存全丢，localStorage 留着。
  it('刷新页面后 content_revision 不回退到列表里的旧值', async () => {
    await seedSavedContentAnnotation(9, 'reload-revision-9', 'seed:reload-revision-9')
    // 列表恒定回旧代次 7——正是「缓存里那份还没追上」的形状。
    const stale = makeLink({
      id: 'L1',
      title: '刷新案例',
      summary: '摘要',
      content: '旧正文',
      content_format: 'plain',
      content_revision: 7,
      has_content: true,
    })
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getLinks: vi.fn(async (params?: { limit?: number }) =>
        ok({
          items: [stale],
          total: 1,
          page: 1,
          limit: params?.limit ?? 30,
        }),
      ),
      getLink: vi.fn(async () => ok({ ...stale, content: undefined, content_document: undefined })),
      getContent: vi.fn(async (id: string) =>
        ok({
          link_id: id,
          content: '旧正文',
          content_format: 'plain' as const,
          fetcher_type: 'stored',
          content_revision: 7,
        }),
      ),
      getTags: vi.fn(async () => ok([])),
      getDomainSummaries: vi.fn(async () => ok({ domains: [], total: 0 })),
      getTranslations: vi.fn(async () =>
        ok({
          current_content_revision: 7,
          current_summary_source_hash: summarySourceHash(stale.summary),
          items: [] as TranslationResponse[],
        }),
      ),
      createTranslation: vi.fn(),
      saveContent: vi.fn(),
      replaceContent: vi.fn(async (id: string) =>
        ok({
          link_id: id,
          content: '重抓后的正文',
          content_format: 'plain' as const,
          fetcher_type: 'basic',
          content_revision: 9,
        }),
      ),
      testConnection: vi.fn(),
    } as unknown as ReaderClient

    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const first = render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await findCardByTitle('刷新案例')
    await expandOriginal()
    fireEvent.click(await screen.findByText('重新抓取'))
    await waitFor(() => expect(client.replaceContent).toHaveBeenCalledWith('L1'))
    expect(confirmSpy).not.toHaveBeenCalled()
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    // 刷新：内存状态（含下界的 ref）全部丢弃，localStorage 保留。
    first.unmount()
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await findCardByTitle('刷新案例')
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })
    await expandOriginal()

    // 下界从盘上读了回来，代次仍是 9，正文划线还在——确认框必须弹。
    fireEvent.click(await screen.findByText('重新抓取'))
    await waitFor(() => expect(confirmSpy).toHaveBeenCalled())
  })

  // 跨标签页的另一半。durable annotation 的提示与重读由 annotation channel 守着；
  // 这里守的是**代次**必须一起跟进——只同步 annotation 反而更糟：
  // B 页拿到 A 页在新代次上建的
  // 划线，自己的代次却还停在旧值，判失配，用户一动就以幸存者重建覆写掉 A 页那份。
  it('其他标签页写的代次下界，本页通过 storage 事件跟进', async () => {
    await seedSavedContentAnnotation(9, 'storage-event-revision-9', 'seed:storage-event-revision-9')
    const stale = makeLink({
      id: 'L1',
      title: '跨页案例',
      summary: '摘要',
      content: '旧正文',
      content_format: 'plain',
      content_revision: 7,
      has_content: true,
    })
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getLinks: vi.fn(async (params?: { limit?: number }) =>
        ok({
          items: [stale],
          total: 1,
          page: 1,
          limit: params?.limit ?? 30,
        }),
      ),
      getLink: vi.fn(async () => ok({ ...stale, content: undefined, content_document: undefined })),
      getContent: vi.fn(async (id: string) =>
        ok({
          link_id: id,
          content: '旧正文',
          content_format: 'plain' as const,
          fetcher_type: 'stored',
          content_revision: 7,
        }),
      ),
      getTags: vi.fn(async () => ok([])),
      getDomainSummaries: vi.fn(async () => ok({ domains: [], total: 0 })),
      getTranslations: vi.fn(async () =>
        ok({
          current_content_revision: 7,
          current_summary_source_hash: summarySourceHash(stale.summary),
          items: [] as TranslationResponse[],
        }),
      ),
      createTranslation: vi.fn(),
      saveContent: vi.fn(),
      // 两点都要命：
      //   · 必须返回**真实结果形状**——返回 undefined 会在 res.ok 上抛出未处理的
      //     Promise 拒绝（失配态下第一次点「重新抓取」不弹确认框、直接落到这里）。
      //   · 但代次必须仍是 **7**。回 9 的话，这次点击自己就把下界推到 9 了
      //     （onReplaceContent → noteContentRevision），第二次点击自然弹框，
      //     storage 事件变成纯装饰——用例恒真，删掉整条 storage 订阅照样绿。
      replaceContent: vi.fn(async (id: string) =>
        ok({
          link_id: id,
          content: '旧正文',
          content_format: 'plain' as const,
          fetcher_type: 'stored',
          content_revision: 7,
        }),
      ),
      testConnection: vi.fn(),
    } as unknown as ReaderClient

    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await findCardByTitle('跨页案例')
    await expandOriginal()

    // 此刻本页代次是 7、durable snapshot 是 9 → 失配，正文划线不可见，不弹确认框。
    fireEvent.click(await screen.findByText('重新抓取'))
    await waitFor(() => expect(client.replaceContent).toHaveBeenCalledWith('L1'))
    expect(confirmSpy).not.toHaveBeenCalled()

    // 另一个标签页保存了原文，把下界写到了 9。
    act(() => {
      writeOwnedStorage('revisionFloor', JSON.stringify({ L1: 9 }))
      window.dispatchEvent(new StorageEvent('storage', { key: revisionFloorKey() }))
    })

    // 事件只是提示；等 UI 从 durable store 重读到代次 9 的划线。
    await waitFor(() =>
      expect(document.querySelector('.reader-rail-annotation-quote')).toHaveTextContent('正文'),
    )

    // 本页代次跟进到 9 → 与 durable snapshot 对上 → 确认框必须弹。
    fireEvent.click(await screen.findByText('重新抓取'))
    await waitFor(() => expect(confirmSpy).toHaveBeenCalled())
  })

  it('重新抓取成功后覆盖纯文本并切换到结构化正文', async () => {
    const { client, replaceContent, getTranslations } = makeClient({
      content: '旧正文',
      content_format: 'plain',
    })
    getTranslations
      .mockResolvedValueOnce(ok({
        current_content_revision: 7,
        current_summary_source_hash: summarySourceHash('这是一段摘要'),
        items: [],
      }))
      .mockResolvedValueOnce(ok({
        current_content_revision: 9,
        current_summary_source_hash: summarySourceHash('这是一段摘要'),
        items: [],
      }))
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const card = await findCardByTitle('保存原文案例')
    await waitFor(() => expect(resourceStore.has(linkDetailCacheKey('L1'))).toBe(true))
    fireEvent.click(card)
    await expandOriginal()
    fireEvent.click(await screen.findByText('重新抓取'))

    await waitFor(() => expect(replaceContent).toHaveBeenCalledWith('L1'))
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: '重新抓取' })).toBeInTheDocument(),
    )
    expect(screen.getByText('这是重新抓取后的原文')).toBeInTheDocument()
    expect(resourceStore.has(linkDetailCacheKey('L1'))).toBe(false)
    await waitFor(() => expect(getTranslations).toHaveBeenCalledTimes(2))
  })

  it('重新抓取等待期间切换文章不会使新文章译文失效', async () => {
    const firstTitle = '第一篇待重新抓取'
    const secondTitle = '第二篇保持当前'
    const first = makeLink({
      id: 'L1',
      title: firstTitle,
      content: '第一篇旧原文',
      content_format: 'plain',
      content_revision: 7,
    })
    const second = makeLink({
      id: 'L2',
      title: secondTitle,
      content: 'Second saved source',
      content_format: 'plain',
      content_revision: 7,
    })
    let resolveReplace!: (value: {
      ok: true
      data: {
        link_id: string
        content: string
        content_format: 'plain'
        fetcher_type: string
      }
    }) => void
    const replaceRequest = new Promise<{
      ok: true
      data: {
        link_id: string
        content: string
        content_format: 'plain'
        fetcher_type: string
      }
    }>((resolve) => {
      resolveReplace = resolve
    })
    const secondTranslation: TranslationResponse = {
      id: 'T2',
      link_id: 'L2',
      scope: 'full',
      block_key: 'content',
      start_offset: 0,
      end_offset: second.content?.length ?? 0,
      source_text: second.content ?? '',
      translated_text: '第二篇中文译文',
      source_format: 'plain',
      target_language: 'zh-CN',
      status: 'done',
      model: 'grok-4.3-fast',
      error_msg: null,
      source_content_revision: second.content_revision ?? null,
      stale: false,
      created_at: '2026-07-15T00:00:00Z',
      updated_at: '2026-07-15T00:00:01Z',
    }
    let resolveSecondTranslations!: (value: {
      ok: true
      data: TranslationListResponse
    }) => void
    const secondTranslationsRequest = new Promise<{
      ok: true
      data: TranslationListResponse
    }>((resolve) => {
      resolveSecondTranslations = resolve
    })
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getLinks: vi.fn(async (params?: { limit?: number }) =>
        ok({
          items: [first, second],
          total: 2,
          page: 1,
          limit: params?.limit ?? 30,
        }),
      ),
      getLink: vi.fn(async (id: string) => ok(id === 'L1' ? first : second)),
      getTags: vi.fn(async () => ok([])),
      getDomainSummaries: vi.fn(async () => ok({ domains: [], total: 0 })),
      saveContent: vi.fn(),
      replaceContent: vi.fn(() => replaceRequest),
      getTranslations: vi.fn((id: string) =>
        id === 'L2'
          ? secondTranslationsRequest
          : Promise.resolve(
              ok({
                current_content_revision: first.content_revision ?? 0,
                current_summary_source_hash: summarySourceHash(first.summary),
                items: [] as TranslationResponse[],
              }),
            ),
      ),
      createTranslation: vi.fn(),
      testConnection: vi.fn(),
    } as unknown as ReaderClient
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const firstCard = await findCardByTitle(firstTitle)
    const secondCard = await findCardByTitle(secondTitle)
    fireEvent.click(firstCard)
    await expandOriginal()
    expect(await screen.findByText('第一篇旧原文')).toBeInTheDocument()
    fireEvent.click(await screen.findByText('重新抓取'))
    await waitFor(() => expect(client.replaceContent).toHaveBeenCalledWith('L1'))

    fireEvent.click(secondCard)
    await expandOriginal()
    expect(await screen.findByText('Second saved source')).toBeInTheDocument()
    await act(async () => {
      resolveReplace({
        ok: true,
        data: {
          link_id: 'L1',
          content: '第一篇新原文',
          content_format: 'plain' as const,
          fetcher_type: 'basic',
        },
      })
      await replaceRequest
      resolveSecondTranslations({
        ok: true,
        data: {
          current_content_revision: second.content_revision ?? 0,
          current_summary_source_hash: summarySourceHash(second.summary),
          items: [secondTranslation],
        },
      })
      await secondTranslationsRequest
    })

    expect(screen.getByRole('heading', { level: 1, name: secondTitle })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '更新翻译' })).not.toBeInTheDocument()
    fireEvent.click(await screen.findByRole('button', { name: '中文译文' }))
    expect(screen.getByText('第二篇中文译文')).toBeInTheDocument()
  })

  it('全文翻译通过持久化接口创建任务', async () => {
    const { client, createTranslation } = makeClient({
      content: 'English body',
      content_format: 'plain',
    })
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const card = await findCardByTitle('保存原文案例')
    fireEvent.click(card)
    await expandOriginal()
    fireEvent.click(await screen.findByRole('button', { name: '翻译全文' }))

    await waitFor(() =>
      expect(createTranslation).toHaveBeenCalledWith('L1', {
        scope: 'full',
        expected_content_revision: 7,
        force: false,
      }),
    )
  })

  it('全文翻译由 document controller 仲裁，stale 时不执行持久化命令', async () => {
    const requestTranslation = vi
      .spyOn(SavedArticleDocumentController.prototype, 'requestTranslation')
      .mockResolvedValue({ status: 'stale' })
    const { client, createTranslation } = makeClient(
      {},
      { content: 'Controller-owned translation body', content_format: 'plain' },
    )
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await expandOriginal({ settleDocument: true })
    expect(await screen.findByText('Controller-owned translation body')).toBeInTheDocument()
    fireEvent.click(await screen.findByRole('button', { name: '翻译全文' }))

    expect(await screen.findByText('翻译失败：当前正文来源已经更新')).toBeInTheDocument()
    expect(requestTranslation).toHaveBeenCalledTimes(1)
    expect(requestTranslation).toHaveBeenCalledWith(expect.any(Function))
    expect(createTranslation).not.toHaveBeenCalled()
    expect(screen.queryByText('已开始全文翻译')).not.toBeInTheDocument()
    expect(screen.queryByText('全文译文已就绪')).not.toBeInTheDocument()
  })

  it('全文译文只渲染 document controller 接受的 resource', async () => {
    const translation: TranslationResponse = {
      id: 'controller-owned-translation',
      link_id: 'L1',
      scope: 'full',
      block_key: 'content',
      start_offset: 0,
      end_offset: 34,
      source_text: 'Controller-owned translation body',
      translated_text: '不应绕过 controller 显示',
      source_format: 'plain',
      target_language: 'zh-CN',
      status: 'done',
      model: 'test-model',
      error_msg: null,
      source_content_revision: 7,
      stale: false,
      created_at: '2026-07-15T00:00:00Z',
      updated_at: '2026-07-15T00:00:00Z',
    }
    const acceptTranslations = vi
      .spyOn(SavedArticleDocumentController.prototype, 'acceptTranslations')
      .mockReturnValue(false)
    const { client, getTranslations } = makeClient(
      {},
      { content: 'Controller-owned translation body', content_format: 'plain' },
    )
    getTranslations.mockResolvedValue(ok({
      current_content_revision: 7,
      current_summary_source_hash: summarySourceHash('这是一段摘要'),
      items: [translation],
    }))

    render(<TestMainView client={client} onOpenSettings={() => {}} />)
    await expandOriginal({ settleDocument: true })

    await waitFor(() => expect(acceptTranslations).toHaveBeenCalledWith(
      expect.objectContaining({ items: [translation] }),
      expect.any(Object),
    ))
    expect(screen.queryByRole('button', { name: '中文译文' })).not.toBeInTheDocument()
    expect(screen.queryByText(translation.translated_text!)).not.toBeInTheDocument()
  })

  it('正文划线只渲染 document controller 接受的 resource', async () => {
    await seedSavedContentAnnotation(7, 'controller-owned-resource', 'seed:controller-owned-resource')
    const acceptAnnotations = vi
      .spyOn(SavedArticleDocumentController.prototype, 'acceptAnnotations')
      .mockReturnValue(false)
    const { client } = makeClient(
      {},
      { content: '正文 controller resource', content_format: 'plain' },
    )

    render(<TestMainView client={client} onOpenSettings={() => {}} />)
    await expandOriginal({ settleDocument: true })

    await waitFor(() => expect(acceptAnnotations).toHaveBeenCalledWith(
      7,
      [expect.objectContaining({ id: 'controller-owned-resource' })],
      expect.any(Object),
    ))
    expect(document.querySelector(
      '[data-hl-block="content"] mark[data-ann="controller-owned-resource"]',
    )).toBeNull()
  })

  it('正文选段发送完整块锚点与 observed saved-content revision', async () => {
    const { client, createTranslation } = makeClient({
      summary: null,
      content: 'English body',
      content_format: 'plain',
      content_revision: 7,
    })
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await expandOriginal()
    await screen.findByText('English body')
    await clickSelectionAction(() => screen.getByText('English body'), '翻译')

    await waitFor(() =>
      expect(createTranslation).toHaveBeenCalledWith('L1', {
        scope: 'selection',
        block_key: 'content',
        start_offset: 0,
        end_offset: 12,
        source_text: 'English body',
        expected_content_revision: 7,
        force: false,
      }),
    )
  })

  it('摘要选段发送整个 rendered block 的 expected_source_hash 且不伪造 revision', async () => {
    const { client, createTranslation, getTranslations } = makeClient({
      summary: 'Translate this sentence',
      content: undefined,
      content_revision: 7,
    })
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await screen.findByText('Translate this sentence')
    await waitFor(() => expect(getTranslations).toHaveBeenCalledWith('L1'))
    await clickSelectionAction(
      () => document.querySelector<HTMLElement>('[data-hl-block="summary"]') as HTMLElement,
      '翻译',
    )

    await waitFor(() =>
      expect(createTranslation).toHaveBeenCalledWith('L1', {
        scope: 'selection',
        block_key: 'summary',
        start_offset: 0,
        end_offset: 23,
        source_text: 'Translate this sentence',
        expected_source_hash: 'c800eccf8f15512a49feb9dbd82de723dbed8278d1d475b0ef83db7ba2858b99',
        force: false,
      }),
    )
  })

  it('summary hash 未就绪时仍加载 saved-content，拿到 rendered hash 后再校验摘要', async () => {
    let resolveDigest!: (value: ArrayBuffer) => void
    vi.spyOn(globalThis.crypto.subtle, 'digest').mockImplementationOnce(
      () => new Promise<ArrayBuffer>((resolve) => { resolveDigest = resolve }),
    )
    const { client, getTranslations } = makeClient(
      {
        summary: 'Pending summary identity',
        content_revision: 7,
      },
      { content: 'Saved content remains independently translatable', content_format: 'plain' },
    )
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await screen.findByText('Pending summary identity')
    await waitFor(() => expect(globalThis.crypto.subtle.digest).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(getTranslations).toHaveBeenCalledTimes(1))
    expect(getTranslations).toHaveBeenCalledWith('L1')
    await expandOriginal({ settleDocument: true })
    expect(await screen.findByText('Saved content remains independently translatable'))
      .toBeInTheDocument()
    expect(screen.getByRole('button', { name: '翻译全文' })).toBeEnabled()
    expect(screen.queryByRole('button', { name: '读取译文中…' })).not.toBeInTheDocument()

    await act(async () => {
      resolveDigest(summarySourceDigest('Pending summary identity'))
    })
    await act(async () => {})
    expect(getTranslations).toHaveBeenCalledTimes(1)
  })

  it('旧链接的 summary hash Promise 不会解锁新链接的译文缓存', async () => {
    const digestRequests: Array<{
      text: string
      resolve: (value: ArrayBuffer) => void
    }> = []
    vi.spyOn(globalThis.crypto.subtle, 'digest').mockImplementation(
      (_algorithm, data) => new Promise<ArrayBuffer>((resolve) => {
        digestRequests.push({
          text: new TextDecoder().decode(data),
          resolve,
        })
      }),
    )
    const first = makeLink({
      id: 'L1',
      title: '第一篇摘要来源',
      summary: 'First pending summary',
      content_revision: 7,
    })
    const second = makeLink({
      id: 'L2',
      title: '第二篇摘要来源',
      summary: 'Second pending summary',
      content_revision: 9,
    })
    const { client, getTranslations } = makeClient({}, {}, [first, second])
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    fireEvent.click(await findCardByTitle('第一篇摘要来源'))
    await waitFor(() =>
      expect(digestRequests.some((request) => request.text === 'First pending summary')).toBe(true),
    )
    fireEvent.click(await findCardByTitle('第二篇摘要来源'))
    await waitFor(() =>
      expect(digestRequests.some((request) => request.text === 'Second pending summary')).toBe(true),
    )

    await act(async () => {
      digestRequests
        .filter((request) => request.text === 'First pending summary')
        .forEach((request) => request.resolve(summarySourceDigest(request.text)))
    })
    expect(getTranslations.mock.calls.filter(([id]) => id === 'L1')).toHaveLength(1)
    expect(getTranslations.mock.calls.filter(([id]) => id === 'L2')).toHaveLength(1)

    await act(async () => {
      digestRequests
        .filter((request) => request.text === 'Second pending summary')
        .forEach((request) => request.resolve(summarySourceDigest(request.text)))
    })
    await act(async () => {})
    expect(getTranslations.mock.calls.filter(([id]) => id === 'L2')).toHaveLength(1)
    expect(getTranslations.mock.calls.filter(([id]) => id === 'L1')).toHaveLength(1)
  })

  it('content revision 冲突刷新 canonical link 与译文且不会静默重试', async () => {
    const {
      client,
      createTranslation,
      getLink,
      getContent,
      getTranslations,
    } = makeClient({
      content: 'Old body',
      content_format: 'plain',
      content_revision: 7,
    })
    const currentLink = makeLink({
      id: 'L1',
      title: '保存原文案例',
      summary: '这是一段摘要',
      content: 'New body',
      content_format: 'plain',
      content_revision: 8,
      has_content: true,
    })
    createTranslation
      .mockResolvedValueOnce({
        ok: false as const,
        error: {
          kind: 'other' as const,
          status: 409,
          errorCode: 'content_revision_conflict',
          message: 'saved content changed',
          currentIdentity: { content_revision: 8, block_key: 'content' },
        },
      })
      .mockResolvedValueOnce(
        ok({
          id: 'T8',
          link_id: 'L1',
          scope: 'full' as const,
          block_key: 'content',
          start_offset: 0,
          end_offset: 8,
          source_text: 'New body',
          translated_text: null,
          source_format: 'plain' as const,
          target_language: 'zh-CN' as const,
          source_content_revision: 8,
          status: 'pending' as const,
          model: null,
          error_msg: null,
          stale: false,
          created_at: '2026-08-01T00:00:00Z',
          updated_at: '2026-08-01T00:00:00Z',
        }),
      )
    getTranslations
      .mockResolvedValueOnce(ok({
        current_content_revision: 7,
        current_summary_source_hash: summarySourceHash('这是一段摘要'),
        items: [],
      }))
      .mockResolvedValueOnce(ok({
        current_content_revision: 8,
        current_summary_source_hash: summarySourceHash('这是一段摘要'),
        items: [],
      }))
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await expandOriginal()
    expect(await screen.findByText('Old body')).toBeInTheDocument()
    getLink.mockResolvedValueOnce(ok(currentLink))
    getContent.mockResolvedValueOnce(
      ok({
        link_id: 'L1',
        content: 'New body',
        content_format: 'plain' as const,
        fetcher_type: 'stored',
        content_revision: 8,
      }),
    )
    fireEvent.click(await screen.findByRole('button', { name: '翻译全文' }))

    await waitFor(() => expect(getLink).toHaveBeenCalledWith('L1'))
    await waitFor(() => expect(getTranslations).toHaveBeenCalledTimes(2))
    expect(createTranslation).toHaveBeenCalledTimes(1)
    expect(screen.getByText('翻译来源已更新，请重新确认后再翻译')).toBeInTheDocument()
    expect(screen.getByText('New body')).toBeInTheDocument()
    expect(screen.queryByText('Old body')).not.toBeInTheDocument()

    fireEvent.click(await screen.findByRole('button', { name: '翻译全文' }))
    await waitFor(() => expect(createTranslation).toHaveBeenCalledTimes(2))
    expect(createTranslation).toHaveBeenLastCalledWith('L1', {
      scope: 'full',
      expected_content_revision: 8,
      force: false,
    })
  })

  it('content revision 冲突后的正文刷新失败会撤下旧正文并允许重试', async () => {
    const {
      client,
      createTranslation,
      getLink,
      getContent,
      getTranslations,
    } = makeClient({
      content: 'Old body',
      content_format: 'plain',
      content_revision: 7,
    })
    createTranslation.mockResolvedValueOnce({
      ok: false as const,
      error: {
        kind: 'other' as const,
        status: 409,
        errorCode: 'content_revision_conflict',
        message: 'saved content changed',
        currentIdentity: { content_revision: 8, block_key: 'content' },
      },
    })
    const contentFailure = {
      ok: false as const,
      error: { kind: 'other' as const, message: 'content temporarily unavailable' },
    }
    getTranslations
      .mockResolvedValueOnce(ok({
        current_content_revision: 7,
        current_summary_source_hash: summarySourceHash('这是一段摘要'),
        items: [],
      }))
      .mockResolvedValue(ok({
        current_content_revision: 8,
        current_summary_source_hash: summarySourceHash('这是一段摘要'),
        items: [],
      }))
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await expandOriginal()
    expect(await screen.findByText('Old body')).toBeInTheDocument()
    getLink.mockResolvedValueOnce(
      ok(
        makeLink({
          id: 'L1',
          title: '保存原文案例',
          summary: '这是一段摘要',
          content: undefined,
          content_document: undefined,
          content_format: undefined,
          content_revision: 8,
          has_content: true,
        }),
      ),
    )
    getContent
      .mockResolvedValueOnce(contentFailure)
      .mockResolvedValueOnce(contentFailure)
      .mockResolvedValueOnce(
        ok({
          link_id: 'L1',
          content: 'New body',
          content_format: 'plain' as const,
          fetcher_type: 'stored',
          content_revision: 8,
        }),
      )
    fireEvent.click(await screen.findByRole('button', { name: '翻译全文' }))

    await waitFor(() => expect(screen.getByText('原文读取失败')).toBeInTheDocument())
    expect(screen.queryByText('Old body')).not.toBeInTheDocument()
    expect(screen.getByText('翻译来源已变化，原文刷新失败，请重试')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    await waitFor(() => expect(screen.getByText('New body')).toBeInTheDocument())
    expect(getContent).toHaveBeenLastCalledWith('L1')
    expect(createTranslation).toHaveBeenCalledTimes(1)
  })

  it('content revision 冲突时 getLink 失败不会采用孤立正文，随后按 reported revision 重读', async () => {
    const {
      client,
      createTranslation,
      getLink,
      getContent,
    } = makeClient({
      content: 'Old body',
      content_format: 'plain',
      content_revision: 7,
    })
    createTranslation.mockResolvedValueOnce({
      ok: false as const,
      error: {
        kind: 'other' as const,
        status: 409,
        errorCode: 'content_revision_conflict',
        message: 'saved content changed',
        currentIdentity: { content_revision: 8, block_key: 'content' },
      },
    })
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await expandOriginal()
    expect(await screen.findByText('Old body')).toBeInTheDocument()
    getLink.mockResolvedValueOnce({
      ok: false as const,
      error: { kind: 'other' as const, message: 'link temporarily unavailable' },
    })
    getContent
      .mockResolvedValueOnce(
        ok({
          link_id: 'L1',
          content: 'Unpaired candidate body',
          content_format: 'plain' as const,
          fetcher_type: 'stored',
          content_revision: 8,
        }),
      )
      .mockResolvedValueOnce(
        ok({
          link_id: 'L1',
          content: 'Retried canonical body',
          content_format: 'plain' as const,
          fetcher_type: 'stored',
          content_revision: 8,
        }),
      )
    fireEvent.click(await screen.findByRole('button', { name: '翻译全文' }))

    expect(await screen.findByText('Retried canonical body')).toBeInTheDocument()
    expect(screen.queryByText('Old body')).not.toBeInTheDocument()
    expect(screen.queryByText('Unpaired candidate body')).not.toBeInTheDocument()
    expect(getLink).toHaveBeenCalledWith('L1')
    expect(getContent).toHaveBeenCalledTimes(3)
    expect(JSON.parse(localStorage.getItem(revisionFloorKey()) ?? '{}').L1).toBe(8)
  })

  it('content revision 冲突的 link/content 不一致时提升到所有响应的最高 revision', async () => {
    const {
      client,
      createTranslation,
      getLink,
      getContent,
    } = makeClient({
      content: 'Old body',
      content_format: 'plain',
      content_revision: 7,
    })
    createTranslation.mockResolvedValueOnce({
      ok: false as const,
      error: {
        kind: 'other' as const,
        status: 409,
        errorCode: 'content_revision_conflict',
        message: 'saved content changed again',
        currentIdentity: { content_revision: 9, block_key: 'content' },
      },
    })
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await expandOriginal()
    expect(await screen.findByText('Old body')).toBeInTheDocument()
    getLink.mockResolvedValueOnce(
      ok(
        makeLink({
          id: 'L1',
          title: '保存原文案例',
          summary: '这是一段摘要',
          content_revision: 8,
          has_content: true,
        }),
      ),
    )
    getContent
      .mockResolvedValueOnce(
        ok({
          link_id: 'L1',
          content: 'Mismatched candidate body',
          content_format: 'plain' as const,
          fetcher_type: 'stored',
          content_revision: 9,
        }),
      )
      .mockResolvedValueOnce(
        ok({
          link_id: 'L1',
          content: 'Revision nine body',
          content_format: 'plain' as const,
          fetcher_type: 'stored',
          content_revision: 9,
        }),
      )
    fireEvent.click(await screen.findByRole('button', { name: '翻译全文' }))

    expect(await screen.findByText('Revision nine body')).toBeInTheDocument()
    expect(screen.queryByText('Old body')).not.toBeInTheDocument()
    expect(screen.queryByText('Mismatched candidate body')).not.toBeInTheDocument()
    expect(JSON.parse(localStorage.getItem(revisionFloorKey()) ?? '{}').L1).toBe(9)
  })

  it('summary block 冲突刷新 canonical source 与译文并要求重新选择', async () => {
    const lease = readerIdentity.activeLease
    if (!lease) throw new Error('test identity lease is not active')
    const summaryHash =
      'c800eccf8f15512a49feb9dbd82de723dbed8278d1d475b0ef83db7ba2858b99'
    await seedAnnotation(
      lease,
      'L1',
      { kind: 'summary', sourceHash: summaryHash },
      {
        id: 'summary-old',
        blockKey: 'summary',
        start: 0,
        end: 23,
        text: 'Translate this sentence',
        note: '',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
        sourceSummaryHash: summaryHash,
      },
      'seed:summary-conflict',
    )
    const { client, createTranslation, getLink, getTranslations } = makeClient({
      summary: 'Translate this sentence',
      content_revision: 7,
    })
    createTranslation.mockResolvedValueOnce({
      ok: false as const,
      error: {
        kind: 'other' as const,
        status: 409,
        errorCode: 'source_block_conflict',
        message: 'summary changed',
        currentIdentity: {
          block_key: 'summary',
          source_hash: 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
        },
      },
    })
    getLink.mockResolvedValueOnce(
      ok(
        makeLink({
          id: 'L1',
          title: '保存原文案例',
          summary: 'Canonical new summary',
          content_revision: 7,
        }),
      ),
    )
    getTranslations
      .mockResolvedValueOnce(ok({
        current_content_revision: 7,
        current_summary_source_hash: summarySourceHash('Translate this sentence'),
        items: [],
      }))
      .mockResolvedValueOnce(ok({
        current_content_revision: 7,
        current_summary_source_hash: summarySourceHash('Canonical new summary'),
        items: [],
      }))
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await screen.findByText('Translate this sentence')
    await waitFor(() =>
      expect(document.querySelector('[data-hl-block="summary"] mark')).toHaveTextContent(
        'Translate this sentence',
      ),
    )
    await clickSelectionAction(
      () => document.querySelector<HTMLElement>('[data-hl-block="summary"]') as HTMLElement,
      '翻译',
    )

    await waitFor(() =>
      expect(document.querySelector('[data-hl-block="summary"]')).toHaveTextContent(
        'Canonical new summary',
      ),
    )
    await waitFor(() => expect(getTranslations).toHaveBeenCalledTimes(2))
    expect(getLink).toHaveBeenCalledWith('L1')
    expect(createTranslation).toHaveBeenCalledTimes(1)
    expect(screen.getByText('翻译来源已更新，请重新选择后再翻译')).toBeInTheDocument()
    expect(screen.queryByText('中文翻译')).not.toBeInTheDocument()
    expect(document.querySelector('[data-hl-block="summary"] mark')).toBeNull()
  })

  it('summary block 冲突后即使服务端返回相同摘要也会重新建立 hash identity', async () => {
    const { client, createTranslation, getLink, getTranslations } = makeClient({
      summary: 'Translate this sentence',
      content_revision: 7,
    })
    createTranslation.mockResolvedValueOnce({
      ok: false as const,
      error: {
        kind: 'other' as const,
        status: 409,
        errorCode: 'source_block_conflict',
        message: 'summary identity changed',
        currentIdentity: {
          block_key: 'summary',
          source_hash: 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
        },
      },
    })
    getLink.mockResolvedValueOnce(ok(makeLink({
      id: 'L1',
      title: '保存原文案例',
      summary: 'Translate this sentence',
      content_revision: 7,
    })))
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await screen.findByText('Translate this sentence')
    await waitFor(() => expect(getTranslations).toHaveBeenCalledTimes(1))
    await clickSelectionAction(
      () => document.querySelector<HTMLElement>('[data-hl-block="summary"]') as HTMLElement,
      '翻译',
    )

    await waitFor(() => expect(createTranslation).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(getLink).toHaveBeenCalledWith('L1'))
    await waitFor(() => expect(getTranslations).toHaveBeenCalledTimes(2))

    await clickSelectionAction(
      () => document.querySelector<HTMLElement>('[data-hl-block="summary"]') as HTMLElement,
      '翻译',
    )
    await waitFor(() => expect(createTranslation).toHaveBeenCalledTimes(2))
    expect(createTranslation.mock.calls[1][1]).toMatchObject({
      block_key: 'summary',
      expected_source_hash:
        'c800eccf8f15512a49feb9dbd82de723dbed8278d1d475b0ef83db7ba2858b99',
    })
  })

  it('summary block 冲突的 link 刷新失败后仍从现有 DOM 重建 hash identity', async () => {
    const { client, createTranslation, getLink, getTranslations } = makeClient({
      summary: 'Translate this sentence',
      content_revision: 7,
    })
    createTranslation.mockResolvedValueOnce({
      ok: false as const,
      error: {
        kind: 'other' as const,
        status: 409,
        errorCode: 'source_block_conflict',
        message: 'summary identity changed',
        currentIdentity: {
          block_key: 'summary',
          source_hash: 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
        },
      },
    })
    getLink.mockResolvedValueOnce({
      ok: false,
      error: { kind: 'other', message: 'link refresh failed', status: 503 },
    })
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await screen.findByText('Translate this sentence')
    await waitFor(() => expect(getTranslations).toHaveBeenCalledTimes(1))
    await clickSelectionAction(
      () => document.querySelector<HTMLElement>('[data-hl-block="summary"]') as HTMLElement,
      '翻译',
    )

    expect(await screen.findByText('翻译来源刷新失败，请稍后重试')).toBeInTheDocument()
    await waitFor(() => expect(getTranslations).toHaveBeenCalledTimes(2))

    await clickSelectionAction(
      () => document.querySelector<HTMLElement>('[data-hl-block="summary"]') as HTMLElement,
      '翻译',
    )
    await waitFor(() => expect(createTranslation).toHaveBeenCalledTimes(2))
    expect(createTranslation.mock.calls[1][1]).toMatchObject({
      block_key: 'summary',
      expected_source_hash:
        'c800eccf8f15512a49feb9dbd82de723dbed8278d1d475b0ef83db7ba2858b99',
    })
  })

  it('重新打开文章时恢复数据库中的全文译文', async () => {
    const { client, getTranslations } = makeClient({
      content: 'English body',
      content_format: 'plain',
    })
    getTranslations.mockResolvedValue(
      ok({
        current_content_revision: 7,
        current_summary_source_hash: summarySourceHash('这是一段摘要'),
        items: [
          {
            id: 'TF',
            link_id: 'L1',
            scope: 'full',
            block_key: 'content',
            start_offset: 0,
            end_offset: 12,
            source_text: 'English body',
            translated_text: '中文正文',
            source_format: 'plain',
            target_language: 'zh-CN',
            status: 'done',
            model: 'grok',
            error_msg: null,
            source_content_revision: 7,
            stale: false,
            created_at: '2026-07-15T00:00:00Z',
            updated_at: '2026-07-15T00:00:01Z',
          } satisfies TranslationResponse,
        ],
      }),
    )
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const card = await findCardByTitle('保存原文案例')
    fireEvent.click(card)
    await expandOriginal()
    fireEvent.click(await screen.findByRole('button', { name: '中文译文' }))

    expect(screen.getByText('中文正文')).toBeInTheDocument()
  })

  it('正文划线由 document controller 仲裁，stale 时不写持久化快照', async () => {
    const lease = readerIdentity.activeLease
    if (!lease) throw new Error('test identity lease is not active')
    const annotate = vi
      .spyOn(SavedArticleDocumentController.prototype, 'annotate')
      .mockResolvedValue({ status: 'stale' })
    const { client } = makeClient(
      {},
      { content: 'Controller-owned annotation body', content_format: 'plain' },
    )
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await expandOriginal({ settleDocument: true })
    await screen.findByText('Controller-owned annotation body')
    await clickSelectionAction(
      () => screen.getByText('Controller-owned annotation body'),
      '划线',
    )

    expect(await screen.findByText('内容来源已更新，请重新选择')).toBeInTheDocument()
    expect(annotate).toHaveBeenCalledTimes(1)
    expect(annotate).toHaveBeenCalledWith(expect.any(Function))
    expect(screen.queryByText('已划线')).not.toBeInTheDocument()
    expect(document.querySelector('[data-hl-block="content"] mark')).toBeNull()
    await expect(readAnnotationSnapshot(lease, 'L1', {
      kind: 'saved-content',
      contentRevision: 7,
    })).resolves.toMatchObject({
      ok: true,
      value: { annotations: [] },
    })
  })

  it('durable 保存失败时保留 NotePanel 和草稿，且不显示成功 toast', async () => {
    const lease = readerIdentity.activeLease
    if (!lease) throw new Error('test identity lease is not active')
    const linkId = 'L-DURABLE-SAVE'
    const annotation: Annotation = {
      id: 'durable-save-note',
      blockKey: 'content',
      start: 0,
      end: 12,
      text: 'Durable save quote',
      note: 'old durable note',
      source: 'self',
      createdAt: 1,
      updatedAt: 1,
      sourceContentRevision: 7,
    }
    await seedAnnotation(
      lease,
      linkId,
      { kind: 'saved-content', contentRevision: 7 },
      annotation,
      'seed:durable-save-failure',
    )
    const { client } = makeClient(
      { id: linkId, title: 'Durable save failure', content_revision: 7 },
      { content: 'Durable source body', content_format: 'plain' },
    )
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const saveQuote = (await screen.findAllByText('Durable save quote'))
      .map((node) => node.closest('.reader-rail-annotation'))
      .find((node): node is HTMLElement => node !== null)
    if (!saveQuote) throw new Error('ReaderRail annotation not found')
    fireEvent.click(saveQuote)
    const textarea = await screen.findByPlaceholderText('写下你的想法…')
    fireEvent.change(textarea, { target: { value: 'draft survives failure' } })
    act(() => lease.revoke())
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: '保存' }))
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    expect(screen.getByText('划线笔记')).toBeInTheDocument()
    expect(textarea).toHaveValue('draft survives failure')
    expect(screen.getByText('保存笔记失败，请重试')).toBeInTheDocument()
    expect(screen.queryByText('笔记已保存')).not.toBeInTheDocument()

    const verifier = new IdentityLease({
      ...lease.context,
      localEpoch: lease.context.localEpoch + 1000,
    })
    await expect(readAnnotationSnapshot(verifier, linkId, {
      kind: 'saved-content',
      contentRevision: 7,
    })).resolves.toMatchObject({
      ok: true,
      value: { annotations: [expect.objectContaining({ note: 'old durable note' })] },
    })
    verifier.revoke()
  })

  it('durable 删除失败时保留 NotePanel 和原划线', async () => {
    const lease = readerIdentity.activeLease
    if (!lease) throw new Error('test identity lease is not active')
    const linkId = 'L-DURABLE-DELETE'
    const annotation: Annotation = {
      id: 'durable-delete-note',
      blockKey: 'content',
      start: 0,
      end: 14,
      text: 'Durable delete quote',
      note: 'keep this note',
      source: 'self',
      createdAt: 1,
      updatedAt: 1,
      sourceContentRevision: 7,
    }
    await seedAnnotation(
      lease,
      linkId,
      { kind: 'saved-content', contentRevision: 7 },
      annotation,
      'seed:durable-delete-failure',
    )
    const { client } = makeClient(
      { id: linkId, title: 'Durable delete failure', content_revision: 7 },
      { content: 'Durable source body', content_format: 'plain' },
    )
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const deleteQuote = (await screen.findAllByText('Durable delete quote'))
      .map((node) => node.closest('.reader-rail-annotation'))
      .find((node): node is HTMLElement => node !== null)
    if (!deleteQuote) throw new Error('ReaderRail annotation not found')
    fireEvent.click(deleteQuote)
    await screen.findByText('划线笔记')
    act(() => lease.revoke())
    await act(async () => {
      fireEvent.click(screen.getByTitle('删除划线'))
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    expect(screen.getByText('划线笔记')).toBeInTheDocument()
    expect(screen.getByText('删除划线失败，请重试')).toBeInTheDocument()
    expect(screen.getAllByText('Durable delete quote').length).toBeGreaterThan(0)

    const verifier = new IdentityLease({
      ...lease.context,
      localEpoch: lease.context.localEpoch + 1000,
    })
    await expect(readAnnotationSnapshot(verifier, linkId, {
      kind: 'saved-content',
      contentRevision: 7,
    })).resolves.toMatchObject({
      ok: true,
      value: { annotations: [expect.objectContaining({ id: annotation.id })] },
    })
    verifier.revoke()
  })

  it('saved revision 前进只撤下旧正文划线，summary hash 与划线保持独立', async () => {
    const lease = readerIdentity.activeLease
    if (!lease) throw new Error('test identity lease is not active')
    const linkId = 'L-REVISION-INDEPENDENT'
    const summaryHash =
      'c800eccf8f15512a49feb9dbd82de723dbed8278d1d475b0ef83db7ba2858b99'
    const contentAnnotation: Annotation = {
      id: 'content-old-revision',
      blockKey: 'content',
      start: 0,
      end: 3,
      text: 'Old',
      note: '',
      source: 'self',
      createdAt: 1,
      updatedAt: 1,
      sourceContentRevision: 7,
    }
    const summaryAnnotation: Annotation = {
      id: 'summary-stable-source',
      blockKey: 'summary',
      start: 0,
      end: 9,
      text: 'Translate',
      note: '',
      source: 'self',
      createdAt: 2,
      updatedAt: 2,
      sourceSummaryHash: summaryHash,
    }
    await seedAnnotation(
      lease,
      linkId,
      { kind: 'saved-content', contentRevision: 7 },
      contentAnnotation,
      'seed:revision-independent-content',
    )
    await seedAnnotation(
      lease,
      linkId,
      { kind: 'summary', sourceHash: summaryHash },
      summaryAnnotation,
      'seed:revision-independent-summary',
    )
    const { client, replaceContent } = makeClient(
      {
        id: linkId,
        title: 'Independent summary source',
        summary: 'Translate this sentence',
        content_revision: 7,
      },
      { content: 'Old body source', content_format: 'plain' },
    )
    replaceContent.mockResolvedValueOnce(ok({
      link_id: linkId,
      content: 'Revision eight body',
      content_format: 'plain' as const,
      fetcher_type: 'basic',
      content_revision: 8,
    }))
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await expandOriginal({ settleDocument: true })
    await waitFor(() => {
      expect(document.querySelector(
        '[data-hl-block="content"] mark[data-ann="content-old-revision"]',
      )).not.toBeNull()
      expect(document.querySelector(
        '[data-hl-block="summary"] mark[data-ann="summary-stable-source"]',
      )).not.toBeNull()
    })

    fireEvent.click(screen.getByText('重新抓取'))
    expect(confirmSpy).toHaveBeenCalledWith(
      '重新抓取会替换当前原文。原文与译文上的划线会保留在历史版本中，但不会应用到新原文；摘要划线不受影响。继续吗？',
    )
    expect(await screen.findByText('Revision eight body')).toBeInTheDocument()
    await waitFor(() => {
      expect(document.querySelector(
        '[data-hl-block="content"] mark[data-ann="content-old-revision"]',
      )).toBeNull()
      expect(document.querySelector(
        '[data-hl-block="summary"] mark[data-ann="summary-stable-source"]',
      )).not.toBeNull()
    })
    expect(screen.queryByText('Old body source')).not.toBeInTheDocument()

    await expect(readAnnotationSnapshot(lease, linkId, {
      kind: 'saved-content',
      contentRevision: 7,
    })).resolves.toMatchObject({
      ok: true,
      value: { annotations: [expect.objectContaining({ id: contentAnnotation.id })] },
    })
    await expect(readAnnotationSnapshot(lease, linkId, {
      kind: 'summary',
      sourceHash: summaryHash,
    })).resolves.toMatchObject({
      ok: true,
      value: {
        annotations: [expect.objectContaining({
          id: summaryAnnotation.id,
          sourceSummaryHash: summaryHash,
        })],
      },
    })
  })

  it('AI 草稿的旧 revision locator 不会采用到新 revision 的同 ID 划线', async () => {
    const lease = readerIdentity.activeLease
    if (!lease) throw new Error('test identity lease is not active')
    const linkId = 'L-AI-REVISION-TARGET'
    const annotationId = 'same-id-across-revisions'
    await seedAnnotation(
      lease,
      linkId,
      { kind: 'saved-content', contentRevision: 7 },
      {
        id: annotationId,
        blockKey: 'content',
        start: 0,
        end: 8,
        text: 'Revision',
        note: 'revision seven note',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
        sourceContentRevision: 7,
      },
      'seed:ai-target-revision-seven',
    )
    await seedAnnotation(
      lease,
      linkId,
      { kind: 'saved-content', contentRevision: 8 },
      {
        id: annotationId,
        blockKey: 'content',
        start: 0,
        end: 8,
        text: 'Revision',
        note: 'revision eight note',
        source: 'self',
        createdAt: 2,
        updatedAt: 2,
        sourceContentRevision: 8,
      },
      'seed:ai-target-revision-eight',
    )

    let resolveReply!: (reply: string) => void
    const reply = new Promise<string>((resolve) => { resolveReply = resolve })
    window.claude = { complete: vi.fn(() => reply) }
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    const { client, replaceContent } = makeClient(
      {
        id: linkId,
        title: 'AI target revision ownership',
        content_revision: 7,
      },
      { content: 'Revision seven body', content_format: 'plain' },
    )
    replaceContent.mockResolvedValueOnce(ok({
      link_id: linkId,
      content: 'Revision eight body',
      content_format: 'plain' as const,
      fetcher_type: 'basic',
      content_revision: 8,
    }))

    try {
      render(<TestMainView client={client} onOpenSettings={() => {}} />)
      await expandOriginal({ settleDocument: true })
      const oldMark = await waitFor(() => {
        const mark = document.querySelector<HTMLElement>(
          `[data-hl-block="content"] mark[data-ann="${annotationId}"][data-ann-target="saved-content:7"]`,
        )
        if (!mark) throw new Error('revision seven mark is not rendered')
        return mark
      })
      fireEvent.click(oldMark)
      fireEvent.click(await screen.findByRole('button', { name: '问 AI' }))
      expect(await screen.findByText('为这段划线记笔记')).toBeInTheDocument()

      fireEvent.click(screen.getByText('重新抓取'))
      expect(confirmSpy).toHaveBeenCalled()
      expect(await screen.findByText('Revision eight body')).toBeInTheDocument()
      await waitFor(() => {
        expect(document.querySelector(
          `[data-hl-block="content"] mark[data-ann="${annotationId}"][data-ann-target="saved-content:8"]`,
        )).not.toBeNull()
      })

      const annotate = vi.spyOn(SavedArticleDocumentController.prototype, 'annotate')
      await act(async () => {
        resolveReply('reply for revision seven only')
        await reply
      })
      fireEvent.click(await screen.findByText('采用为笔记'))
      await act(async () => { await Promise.resolve() })

      expect(annotate).not.toHaveBeenCalled()
      await expect(readAnnotationSnapshot(lease, linkId, {
        kind: 'saved-content',
        contentRevision: 8,
      })).resolves.toMatchObject({
        ok: true,
        value: {
          annotations: [expect.objectContaining({
            id: annotationId,
            note: 'revision eight note',
          })],
        },
      })
    } finally {
      delete window.claude
    }
  })
})

describe('MainView 链接信息编辑', () => {
  it('保存后立即更新详情和列表，低版本 canonical reread 不会覆盖新投影', async () => {
    const { client, getLink, patchLinkMetadata } = makeClient({
      library_kind: 'reading',
      title: '保存前标题',
      summary: '保存前摘要',
      tags: ['旧标签'],
      metadata_revision: 1,
    })
    getLink.mockResolvedValue(ok(makeLink({
      id: 'L1',
      library_kind: 'reading',
      title: '陈旧 canonical 标题',
      summary: '陈旧 canonical 摘要',
      tags: ['陈旧标签'],
      metadata_revision: 1,
    })))

    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    fireEvent.click(await findCardByTitle('保存前标题'))
    await screen.findByRole('heading', { level: 1, name: '保存前标题' })
    fireEvent.click(screen.getByRole('button', { name: '编辑链接信息' }))
    fireEvent.change(screen.getByRole('textbox', { name: '链接标题' }), {
      target: { value: '已保存标题' },
    })
    fireEvent.change(screen.getByRole('textbox', { name: '链接摘要' }), {
      target: { value: '已保存摘要' },
    })
    fireEvent.change(screen.getByRole('textbox', { name: '链接标签' }), {
      target: { value: 'Alpha, alpha, Beta Alpha' },
    })
    fireEvent.click(screen.getByRole('button', { name: '保存链接信息' }))

    await waitFor(() => {
      expect(patchLinkMetadata).toHaveBeenCalledWith('L1', 1, {
        title: '已保存标题',
        summary: '已保存摘要',
        tags: ['Alpha', 'Beta'],
      })
    })
    await screen.findByRole('heading', { level: 1, name: '已保存标题' })
    const updatedCard = await findCardByTitle('已保存标题')
    expect(updatedCard).toHaveTextContent('已保存摘要')
    await waitFor(() => expect(getLink).toHaveBeenCalledWith('L1'))
    await act(async () => { await Promise.resolve() })

    expect(screen.getByRole('heading', { level: 1, name: '已保存标题' })).toBeInTheDocument()
    expect(document.querySelector('.summary-lead')).toHaveTextContent('已保存摘要')
    expect(document.querySelector('.reader-inner .art-tags')).toHaveTextContent('Alpha')
    expect(document.querySelector('.reader-inner .art-tags')).toHaveTextContent('Beta')
  })

  it('普通保存错误保留可编辑草稿，并以原 metadata revision 重试', async () => {
    const { client, patchLinkMetadata } = makeClient({
      library_kind: 'reading',
      title: '错误前标题',
      metadata_revision: 4,
    })
    patchLinkMetadata.mockResolvedValue(err({
      kind: 'other',
      message: 'metadata service unavailable',
    }))
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    fireEvent.click(await findCardByTitle('错误前标题'))
    fireEvent.click(await screen.findByRole('button', { name: '编辑链接信息' }))
    fireEvent.change(screen.getByRole('textbox', { name: '链接标题' }), {
      target: { value: '保留的本地标题' },
    })
    fireEvent.click(screen.getByRole('button', { name: '保存链接信息' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('metadata service unavailable')
    expect(screen.getByRole('textbox', { name: '链接标题' })).toHaveValue('保留的本地标题')
    expect(patchLinkMetadata).toHaveBeenLastCalledWith('L1', 4, expect.any(Object))

    fireEvent.click(screen.getByRole('button', { name: '保存链接信息' }))
    await waitFor(() => expect(patchLinkMetadata).toHaveBeenCalledTimes(2))
    expect(patchLinkMetadata).toHaveBeenLastCalledWith('L1', 4, {
      title: '保留的本地标题',
      summary: '这是一段摘要',
      tags: ['LLM'],
    })
    expect(screen.getByRole('textbox', { name: '链接标题' })).toHaveValue('保留的本地标题')
  })

  it('冲突后同步远端投影、保留草稿，并用刷新后的 revision 重试', async () => {
    const { client, getLink, patchLinkMetadata } = makeClient({
      library_kind: 'reading',
      title: '冲突前标题',
      summary: '冲突前摘要',
      tags: ['本地旧标签'],
      metadata_revision: 1,
    })
    const remote = makeLink({
      id: 'L1',
      library_kind: 'reading',
      title: '远端新标题',
      summary: '远端新摘要',
      tags: ['远端标签'],
      metadata_revision: 2,
    })
    patchLinkMetadata
      .mockResolvedValueOnce(err({
        kind: 'other',
        status: 409,
        errorCode: 'metadata_revision_conflict',
        message: 'metadata changed elsewhere',
      }))
      .mockResolvedValueOnce(ok({ link_id: 'L1', metadata_revision: 3 }))

    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    fireEvent.click(await findCardByTitle('冲突前标题'))
    fireEvent.click(await screen.findByRole('button', { name: '编辑链接信息' }))
    fireEvent.change(screen.getByRole('textbox', { name: '链接标题' }), {
      target: { value: '本地冲突草稿' },
    })
    fireEvent.change(screen.getByRole('textbox', { name: '链接摘要' }), {
      target: { value: '本地冲突摘要' },
    })
    fireEvent.change(screen.getByRole('textbox', { name: '链接标签' }), {
      target: { value: '本地标签, 重试标签' },
    })
    getLink.mockResolvedValue(ok(remote))
    fireEvent.click(screen.getByRole('button', { name: '保存链接信息' }))

    await waitFor(() => {
      expect(patchLinkMetadata).toHaveBeenCalledWith('L1', 1, {
        title: '本地冲突草稿',
        summary: '本地冲突摘要',
        tags: ['本地标签', '重试标签'],
      })
    })
    expect(await screen.findByRole('alert')).toHaveTextContent(
      '链接信息已在其他位置更新。已保留本地草稿并同步最新版本，请再次保存。',
    )
    expect(screen.getByRole('textbox', { name: '链接标题' })).toHaveValue('本地冲突草稿')
    expect(await findCardByTitle('远端新标题')).toHaveTextContent('远端新摘要')

    fireEvent.click(screen.getByRole('button', { name: '保存链接信息' }))
    await waitFor(() => {
      expect(patchLinkMetadata).toHaveBeenLastCalledWith('L1', 2, {
        title: '本地冲突草稿',
        summary: '本地冲突摘要',
        tags: ['本地标签', '重试标签'],
      })
    })
    expect(await screen.findByRole('heading', { level: 1, name: '本地冲突草稿' })).toBeInTheDocument()
  })

  it('写入前已发出的详情读取在稍后返回旧 metadata revision 时不会回退保存结果', async () => {
    let resolveStaleRead!: (value: ApiResult<LinkResponse>) => void
    const staleRead = new Promise<ApiResult<LinkResponse>>((resolve) => {
      resolveStaleRead = resolve
    })
    const { client, getLink, patchLinkMetadata } = makeClient({
      library_kind: 'reading',
      title: '延迟读取前标题',
      summary: '延迟读取前摘要',
      tags: ['旧标签'],
      metadata_revision: 1,
      has_content: true,
    })
    getLink
      .mockImplementationOnce(() => staleRead)
      .mockResolvedValueOnce(ok(makeLink({
        id: 'L1',
        library_kind: 'reading',
        title: '延迟读取后保存标题',
        summary: '延迟读取后保存摘要',
        tags: ['保存标签'],
        metadata_revision: 2,
        has_content: true,
      })))

    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await waitFor(() => expect(getLink).toHaveBeenCalledWith('L1'))
    await screen.findByRole('heading', { level: 1, name: '延迟读取前标题' })
    fireEvent.click(screen.getByRole('button', { name: '编辑链接信息' }))
    fireEvent.change(screen.getByRole('textbox', { name: '链接标题' }), {
      target: { value: '延迟读取后保存标题' },
    })
    fireEvent.change(screen.getByRole('textbox', { name: '链接摘要' }), {
      target: { value: '延迟读取后保存摘要' },
    })
    fireEvent.change(screen.getByRole('textbox', { name: '链接标签' }), {
      target: { value: '保存标签' },
    })
    fireEvent.click(screen.getByRole('button', { name: '保存链接信息' }))

    await waitFor(() => {
      expect(patchLinkMetadata).toHaveBeenCalledWith('L1', 1, {
        title: '延迟读取后保存标题',
        summary: '延迟读取后保存摘要',
        tags: ['保存标签'],
      })
    })
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(2))
    await screen.findByRole('heading', { level: 1, name: '延迟读取后保存标题' })

    await act(async () => {
      resolveStaleRead(ok(makeLink({
        id: 'L1',
        library_kind: 'reading',
        title: '陈旧延迟标题',
        summary: '陈旧延迟摘要',
        tags: ['陈旧标签'],
        metadata_revision: 1,
        has_content: true,
      })))
      await staleRead
    })
    await act(async () => { await Promise.resolve() })

    expect(screen.getByRole('heading', { level: 1, name: '延迟读取后保存标题' })).toBeInTheDocument()
    expect(document.querySelector('.summary-lead')).toHaveTextContent('延迟读取后保存摘要')
    expect(await findCardByTitle('延迟读取后保存标题')).toHaveTextContent('延迟读取后保存摘要')
    expect(document.querySelector('.reader-inner .art-tags')).toHaveTextContent('保存标签')
  })
})

describe('MainView active 与 corpus ownership', () => {
  it('Today 组件流可通过四个游标页读到第 101 条', async () => {
    const items = Array.from({ length: 101 }, (_, index) => makeLink({
      id: `today-component-${String(index).padStart(3, '0')}`,
      title: `自然日条目 ${String(index + 1).padStart(3, '0')}`,
      created_at: new Date(Date.now() - index * 1000).toISOString(),
    }))
    const { client } = makeClient({}, {}, items)
    const starts = new Map<string, number>([
      ['', 0],
      ['today-cursor-30', 30],
      ['today-cursor-60', 60],
      ['today-cursor-90', 90],
    ])
    const getLinks = vi.fn(async (params: ListLinksParams = {}) => {
      if (!params.created_from || !params.created_before) {
        return ok({ items: [], total: 0, page: 0, limit: params.limit ?? 30 })
      }
      const start = starts.get(params.after ?? '')
      if (start === undefined) throw new Error(`unexpected Today cursor ${params.after}`)
      const pageItems = items.slice(start, start + 30)
      return ok({
        items: pageItems,
        total: 0,
        page: 0,
        limit: 30,
        ...(start + pageItems.length < items.length
          ? { next_cursor: `today-cursor-${start + pageItems.length}` }
          : {}),
      })
    })
    ;(client as unknown as { getLinks: typeof getLinks }).getLinks = getLinks

    const { container } = render(
      <TestMainView client={client} onOpenSettings={() => {}} />,
    )
    fireEvent.click(screen.getByText('今天新增'))
    expect(await screen.findByText('自然日条目 030')).toBeInTheDocument()

    const expectedPages = ['自然日条目 060', '自然日条目 090', '自然日条目 101']
    for (const [index, title] of expectedPages.entries()) {
      const scroller = container.querySelector<HTMLElement>('.list-scroll')
      if (!scroller) throw new Error('list scroller not found')
      fireEvent.scroll(scroller)
      await waitFor(() => {
        const todayCallCount = getLinks.mock.calls
          .filter(([params]) => Boolean(params?.created_from))
          .length
        expect(todayCallCount).toBe(index + 2)
      })
      expect(await screen.findByText(title)).toBeInTheDocument()
    }

    expect(screen.getByText('101 条')).toBeInTheDocument()
    const todayCalls = getLinks.mock.calls
      .map(([params]) => params)
      .filter((params): params is ListLinksParams => Boolean(params?.created_from))
    expect(todayCalls).toHaveLength(4)
    const firstTodayCall = todayCalls[0]
    if (!firstTodayCall) throw new Error('Today request not recorded')
    const range = {
      created_from: firstTodayCall.created_from,
      created_before: firstTodayCall.created_before,
    }
    for (const params of todayCalls) {
      expect(params).toMatchObject({ ...range, limit: 30 })
    }
    expect(todayCalls.map((params) => params.after)).toEqual([
      '',
      'today-cursor-30',
      'today-cursor-60',
      'today-cursor-90',
    ])
  })

  it('筛选后的当前列表为空时仍保留详情与本地搜索语料', async () => {
    const { client } = makeClient()
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const card = await findCardByTitle('保存原文案例')
    fireEvent.click(card)

    fireEvent.click(screen.getByText('今天新增'))
    await screen.findByText('这个筛选下还没有链接')
    expect(screen.getByRole('heading', { name: '保存原文案例' })).toBeInTheDocument()

    fireEvent.click(screen.getByText('搜索链接'))
    fireEvent.change(screen.getByPlaceholderText('搜索标题、摘要、域名… 输入 # 搜标签'), {
      target: { value: '保存原文案例' },
    })
    await waitFor(() => expect(screen.getAllByText('保存原文案例')).toHaveLength(2))
  })

  it('快速切换链接时忽略较早返回的详情', async () => {
    // 第一篇设为 pending：PF6 之后只有「仍在解析中」或「不在列表里」的链接
    // 才会发详情请求，而这条用例守的正是那条请求路径上的竞态防线。
    const first = makeLink({
      id: 'L1',
      title: '第一篇文章',
      summary: null,
      status: 'pending',
    })
    const second = makeLink({
      id: 'L2',
      title: '第二篇文章',
      summary: null,
      has_content: true,
      content_revision: 7,
    })
    let resolveFirst!: (value: { ok: true; data: LinkResponse }) => void
    const firstDetail = new Promise<{ ok: true; data: LinkResponse }>((resolve) => {
      resolveFirst = resolve
    })
    const getLink = vi.fn((id: string) =>
      id === 'L1'
        ? firstDetail
        : Promise.resolve(ok(makeLink({ ...second, content: '第二篇已保存原文' }))),
    )
    const client = {
      isIdentityCurrent: vi.fn(() => true),
      getLinks: vi.fn(async (params?: { limit?: number }) =>
        ok({
          items: [first, second],
          total: 2,
          page: 1,
          limit: params?.limit ?? 30,
        }),
      ),
      getLink,
      getContent: vi.fn(async (id: string) =>
        id === 'L2'
          ? ok({
              link_id: id,
              content: '第二篇已保存原文',
              content_format: 'plain' as const,
              fetcher_type: 'stored',
              content_revision: second.content_revision,
            })
          : { ok: false as const, error: { kind: 'other' as const, message: 'not found' } },
      ),
      getTags: vi.fn(async () => ok([])),
      getDomainSummaries: vi.fn(async () => ok({ domains: [], total: 0 })),
      saveContent: vi.fn(),
      replaceContent: vi.fn(),
      getTranslations: vi.fn(async () => ok({
        current_content_revision: 7,
        current_summary_source_hash: null,
        items: [],
      })),
      createTranslation: vi.fn(),
      testConnection: vi.fn(),
    } as unknown as ReaderClient
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const firstCard = await findCardByTitle('第一篇文章')
    const secondCard = await findCardByTitle('第二篇文章')
    fireEvent.click(firstCard)
    // 第一篇的详情请求挂住不返回。
    await waitFor(() => expect(getLink).toHaveBeenCalledWith('L1'))
    await act(async () => {
      fireEvent.click(secondCard)
      await new Promise((resolve) => setTimeout(resolve, 0))
    })
    await expandOriginal({ settleDocument: true })

    expect(await screen.findByText('第二篇已保存原文')).toBeInTheDocument()
    await act(async () => {
      resolveFirst({
        ok: true,
        data: makeLink({
          ...first,
          title: '第一篇迟到标题',
          content: '第一篇迟到的原文',
        }),
      })
      await firstDetail
      await new Promise((resolve) => setTimeout(resolve, 0))
    })
    expect(screen.getByRole('heading', { level: 1, name: '第二篇文章' })).toBeInTheDocument()
    expect(screen.queryByText('第一篇迟到标题')).not.toBeInTheDocument()
    expect(screen.queryByText('第一篇迟到的原文')).not.toBeInTheDocument()
  })
})

describe('MainView 移动端面板导航', () => {
  it('打开链接后进入详情，返回按钮恢复列表', async () => {
    const { client } = makeClient({ title: '移动端阅读案例' })
    const { container } = render(<TestMainView client={client} onOpenSettings={() => {}} />)

    const card = await findCardByTitle('移动端阅读案例')
    fireEvent.click(card)
    await screen.findByText('保存原文')

    expect(container.querySelector('.body')).toHaveClass('mobile-detail-active')
    fireEvent.click(screen.getByRole('button', { name: '返回链接列表' }))
    expect(container.querySelector('.body')).not.toHaveClass('mobile-detail-active')
  })

  it('AI 工具层打开时标记底层内容为隐藏状态', async () => {
    const { client } = makeClient()
    const { container } = render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await screen.findByText('保存原文')
    const titlebarChat = container.querySelector<HTMLButtonElement>(
      '.titlebar button[title="AI 助手 (⌘J)"]',
    )
    if (!titlebarChat) throw new Error('expected titlebar AI assistant button to exist')
    fireEvent.click(titlebarChat)
    expect(container.querySelector('.body')).toHaveClass('mobile-tool-open')

    fireEvent.click(screen.getByRole('button', { name: '关闭' }))
    expect(container.querySelector('.body')).not.toHaveClass('mobile-tool-open')
  })
})

describe('MainView 归档下载', () => {
  function openArchiveDialog(): HTMLElement {
    fireEvent.click(screen.getByRole('button', { name: '下载归档' }))
    return screen.getByRole('dialog', { name: '下载归档' })
  }

  it('keeps the archive capability active after the Strict Mode effect replay', async () => {
    const { client, downloadArchiveV2 } = makeClient()
    downloadArchiveV2.mockResolvedValue(err({ kind: 'other', message: '诊断响应' }))
    render(
      <StrictMode>
        <TestMainView client={client} onOpenSettings={() => {}} />
      </StrictMode>,
    )

    const dialog = openArchiveDialog()
    fireEvent.click(within(dialog).getByRole('button', { name: '下载' }))

    await waitFor(() => expect(downloadArchiveV2).toHaveBeenCalledTimes(1))
    expect(await screen.findByText('归档下载失败：诊断响应')).toBeInTheDocument()
  })

  it('每次打开均重置私有组，并将明确的 canonical selector 交给客户端', async () => {
    const { client, downloadArchiveV2 } = makeClient()
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    let dialog = openArchiveDialog()
    const firstDialog = within(dialog)
    const firstThoughts = firstDialog.getByRole('checkbox', { name: '想法' })
    const firstNotes = firstDialog.getByRole('checkbox', { name: '笔记' })
    expect(firstThoughts).toBeChecked()
    expect(firstNotes).toBeChecked()

    fireEvent.click(firstThoughts)
    fireEvent.click(firstNotes)
    expect(firstThoughts).not.toBeChecked()
    expect(firstNotes).not.toBeChecked()
    const baseOnlySubmit = firstDialog.getByRole('button', { name: '下载' })
    expect(baseOnlySubmit).toHaveAttribute('data-archive-sections', 'base')
    downloadArchiveV2.mockResolvedValue(err({ kind: 'other', message: '请重试' }))
    fireEvent.click(baseOnlySubmit)
    await waitFor(() => {
      expect(downloadArchiveV2).toHaveBeenCalledWith({ includeThoughts: false, includeNotes: false })
    })
    await waitFor(() => expect(firstDialog.getByRole('button', { name: '取消' })).not.toBeDisabled())
    fireEvent.click(firstDialog.getByRole('button', { name: '取消' }))

    dialog = openArchiveDialog()
    const reopened = within(dialog)
    const thoughts = reopened.getByRole('checkbox', { name: '想法' })
    const notes = reopened.getByRole('checkbox', { name: '笔记' })
    expect(thoughts).toBeChecked()
    expect(notes).toBeChecked()

    fireEvent.click(thoughts)
    const submit = reopened.getByRole('button', { name: '下载' })
    expect(submit).toHaveAttribute('data-archive-sections', 'base,notes')
  })

  it('only creates and clicks an object URL after a validated successful result', async () => {
    const { client, downloadArchiveV2 } = makeClient()
    const blob = new Blob(['verified archive bytes'], { type: 'application/json' })
    downloadArchiveV2.mockResolvedValue(ok(blob))
    const createObjectURL = vi.fn(() => 'blob:archive-v2')
    const revokeObjectURL = vi.fn()
    const originalCreateObjectURL = URL.createObjectURL
    const originalRevokeObjectURL = URL.revokeObjectURL
    Object.defineProperties(URL, {
      createObjectURL: { configurable: true, value: createObjectURL },
      revokeObjectURL: { configurable: true, value: revokeObjectURL },
    })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})

    try {
      render(<TestMainView client={client} onOpenSettings={() => {}} />)
      const dialog = openArchiveDialog()
      fireEvent.click(within(dialog).getByRole('button', { name: '下载' }))

      await waitFor(() => expect(createObjectURL).toHaveBeenCalledWith(blob))
      expect(click).toHaveBeenCalledTimes(1)
      expect(revokeObjectURL).toHaveBeenCalledWith('blob:archive-v2')
      expect(screen.getByText('归档已下载')).toBeInTheDocument()
    } finally {
      Object.defineProperties(URL, {
        createObjectURL: { configurable: true, value: originalCreateObjectURL },
        revokeObjectURL: { configurable: true, value: originalRevokeObjectURL },
      })
    }
  })

  it('keeps the dialog retryable and avoids browser download side effects on failure', async () => {
    const { client, downloadArchiveV2 } = makeClient()
    downloadArchiveV2.mockResolvedValue(err({
      kind: 'other',
      message: '归档校验失败',
    }))
    const createObjectURL = vi.fn(() => 'blob:must-not-exist')
    const originalCreateObjectURL = URL.createObjectURL
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})

    try {
      render(<TestMainView client={client} onOpenSettings={() => {}} />)
      const dialog = openArchiveDialog()
      fireEvent.click(within(dialog).getByRole('button', { name: '下载' }))

      expect(await screen.findByText('归档下载失败：归档校验失败')).toBeInTheDocument()
      expect(createObjectURL).not.toHaveBeenCalled()
      expect(click).not.toHaveBeenCalled()
      expect(screen.getByRole('dialog', { name: '下载归档' })).toBeInTheDocument()
    } finally {
      Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: originalCreateObjectURL })
    }
  })

  it('drops a validated result without publishing old-session UI if identity changes', async () => {
    const { client, downloadArchiveV2 } = makeClient()
    let identityCurrent = true
    const identityClient = client as unknown as { isIdentityCurrent: ReturnType<typeof vi.fn> }
    identityClient.isIdentityCurrent = vi.fn(() => identityCurrent)
    downloadArchiveV2.mockImplementation(async () => {
      identityCurrent = false
      return ok(new Blob(['validated under the old identity'], { type: 'application/json' }))
    })
    const createObjectURL = vi.fn(() => 'blob:must-not-exist')
    const originalCreateObjectURL = URL.createObjectURL
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})

    try {
      render(<TestMainView client={client} onOpenSettings={() => {}} />)
      const dialog = openArchiveDialog()
      fireEvent.click(within(dialog).getByRole('button', { name: '下载' }))

      await waitFor(() => {
        expect(downloadArchiveV2).toHaveBeenCalledTimes(1)
        expect(createObjectURL).not.toHaveBeenCalled()
        expect(click).not.toHaveBeenCalled()
        expect(screen.queryByText(/归档下载失败/)).not.toBeInTheDocument()
        expect(screen.queryByText('归档已下载')).not.toBeInTheDocument()
      })
    } finally {
      Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: originalCreateObjectURL })
    }
  })

  it('drops a validated late result when archive download capability is revoked', async () => {
    const { client, downloadArchiveV2 } = makeClient()
    const pending = deferred<ApiResult<Blob>>()
    downloadArchiveV2.mockReturnValue(pending.promise)
    const createObjectURL = vi.fn(() => 'blob:must-not-exist')
    const originalCreateObjectURL = URL.createObjectURL
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const revokedCapabilities: CapabilitiesResponse = {
      ...ENABLED_READER_CAPABILITIES,
      archive_versions: [],
    }

    try {
      const { rerender } = render(
        <TestMainView
          client={client}
          capabilities={ENABLED_READER_CAPABILITIES}
          onOpenSettings={() => {}}
        />,
      )
      const dialog = openArchiveDialog()
      fireEvent.click(within(dialog).getByRole('button', { name: '下载' }))
      await waitFor(() => expect(downloadArchiveV2).toHaveBeenCalledTimes(1))

      rerender(
        <TestMainView
          client={client}
          capabilities={revokedCapabilities}
          onOpenSettings={() => {}}
        />,
      )
      await waitFor(() => {
        expect(screen.queryByRole('dialog', { name: '下载归档' })).not.toBeInTheDocument()
        expect(screen.queryByRole('button', { name: '下载归档' })).not.toBeInTheDocument()
      })

      await act(async () => {
        pending.resolve(ok(new Blob(['validated after revocation'], { type: 'application/json' })))
        await pending.promise
        await Promise.resolve()
      })

      expect(createObjectURL).not.toHaveBeenCalled()
      expect(click).not.toHaveBeenCalled()
      expect(screen.queryByText('归档已下载')).not.toBeInTheDocument()

      rerender(
        <TestMainView
          client={client}
          capabilities={ENABLED_READER_CAPABILITIES}
          onOpenSettings={() => {}}
        />,
      )
      expect(await screen.findByRole('button', { name: '下载归档' })).not.toBeDisabled()
    } finally {
      Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: originalCreateObjectURL })
    }
  })
})

describe('MainView Thought sync command', () => {
  it('waits for an annotated library point read before reporting that manual sync outcome', async () => {
    await seedSavedContentAnnotation(7, 'titlebar-annotated-reload', 'seed:titlebar-annotated-reload')
    const { client, getLink } = makeClient({ title: '划线同步文章' })
    type ThoughtClient = {
      pushThoughtOps: ReturnType<typeof vi.fn>
      syncThoughts: ReturnType<typeof vi.fn>
    }
    type ThoughtOperation = {
      readonly op_id: string
      readonly device_id: string
      readonly logical_clock: number
    }
    const thoughtClient = client as unknown as ThoughtClient
    thoughtClient.pushThoughtOps = vi.fn(async (request: { readonly ops: readonly ThoughtOperation[] }) =>
      ok(request.ops.map((operation, index) => ({
        contract_version: 1 as const,
        op_id: operation.op_id,
        sequence: index + 1,
        disposition: 'applied' as const,
        submitted_key: {
          logical_clock: operation.logical_clock,
          device_id: operation.device_id,
          op_id: operation.op_id,
        },
        current_winner_key: {
          logical_clock: operation.logical_clock,
          device_id: operation.device_id,
          op_id: operation.op_id,
        },
      })),
    ))
    thoughtClient.syncThoughts = vi.fn(async () => ok({ contract_version: 1 as const, items: [] }))

    render(<TestMainView client={client} onOpenSettings={() => {}} />)
    await screen.findByText('已同步')
    const annotatedRow = screen.getByText('有划线', { selector: '.sb-name' }).closest('.sb-row')
    if (!annotatedRow) throw new Error('expected annotated sidebar row')
    act(() => {
      resourceStore.invalidate(linkDetailCacheKey('L1'))
      fireEvent.click(annotatedRow)
    })
    await waitFor(() => expect(getLink).toHaveBeenCalled())
    await waitFor(() => expect(annotatedRow).toHaveClass('active'))

    getLink.mockClear()
    let resolvePointRead!: (result: ApiResult<LinkResponse>) => void
    const pendingPointRead = new Promise<ApiResult<LinkResponse>>((resolve) => {
      resolvePointRead = resolve
    })
    getLink.mockImplementationOnce(() => pendingPointRead)
    act(() => {
      resourceStore.invalidate(linkDetailCacheKey('L1'))
      fireEvent.click(screen.getByTitle('同步'))
    })
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(thoughtClient.syncThoughts).toHaveBeenCalledTimes(2))
    expect(screen.queryByText('资料库已同步；想法已同步')).not.toBeInTheDocument()
    expect(screen.queryByText(/资料库同步失败：annotated point read failed/)).not.toBeInTheDocument()

    await act(async () => {
      resolvePointRead({
        ok: false,
        error: { kind: 'other', message: 'annotated point read failed' },
      })
      await pendingPointRead
    })
    expect(await screen.findByText(
      '资料库同步失败：annotated point read failed；想法已同步',
    )).toBeInTheDocument()
    expect(screen.queryByText('资料库已同步；想法已同步')).not.toBeInTheDocument()
  })

  it('waits for both library and Thought results, then identifies a partial failure by source', async () => {
    const { client } = makeClient({ title: '同步状态文章' })
    type ThoughtClient = {
      pushThoughtOps: ReturnType<typeof vi.fn>
      syncThoughts: ReturnType<typeof vi.fn>
    }
    const thoughtClient = client as unknown as ThoughtClient
    thoughtClient.pushThoughtOps = vi.fn(async () => ok([]))
    thoughtClient.syncThoughts = vi.fn(async () => ok({ contract_version: 1 as const, items: [] }))

    render(<TestMainView client={client} onOpenSettings={() => {}} />)
    await screen.findByText('已同步')

    let resolveLibrary!: (result: ApiResult<{ items: LinkResponse[]; total: number; page: number; limit: number }>) => void
    const pendingLibrary = new Promise<ApiResult<{ items: LinkResponse[]; total: number; page: number; limit: number }>>(
      (resolve) => { resolveLibrary = resolve },
    )
    let resolveThought!: (result: ApiResult<{ contract_version: 1; items: [] }>) => void
    const pendingThought = new Promise<ApiResult<{ contract_version: 1; items: [] }>>(
      (resolve) => { resolveThought = resolve },
    )
    const getLinks = client.getLinks as ReturnType<typeof vi.fn>
    getLinks.mockImplementationOnce(() => pendingLibrary)
    thoughtClient.syncThoughts.mockImplementationOnce(() => pendingThought)

    fireEvent.click(screen.getByTitle('同步'))
    await waitFor(() => expect(thoughtClient.syncThoughts).toHaveBeenCalledTimes(2))
    expect(getLinks).toHaveBeenCalled()
    expect(screen.queryByText('资料库已同步；想法已同步')).not.toBeInTheDocument()

    await act(async () => {
      resolveLibrary({
        ok: false,
        error: { kind: 'other', message: 'library endpoint failed' },
      })
      resolveThought({ ok: true, data: { contract_version: 1, items: [] } })
      await pendingLibrary
      await pendingThought
    })

    expect(await screen.findByText('资料库同步失败：library endpoint failed；想法已同步')).toBeInTheDocument()
    expect(screen.queryByText('资料库已同步；想法已同步')).not.toBeInTheDocument()
  })

  it('reports a Thought failure independently without exposing its transport text', async () => {
    const { client } = makeClient({ title: 'Thought 部分失败文章' })
    type ThoughtClient = {
      pushThoughtOps: ReturnType<typeof vi.fn>
      syncThoughts: ReturnType<typeof vi.fn>
    }
    const thoughtClient = client as unknown as ThoughtClient
    thoughtClient.pushThoughtOps = vi.fn(async () => ok([]))
    thoughtClient.syncThoughts = vi.fn(async () => ok({ contract_version: 1 as const, items: [] }))

    render(<TestMainView client={client} onOpenSettings={() => {}} />)
    await screen.findByText('已同步')

    let resolveLibrary!: (result: ApiResult<{ items: LinkResponse[]; total: number; page: number; limit: number }>) => void
    const pendingLibrary = new Promise<ApiResult<{ items: LinkResponse[]; total: number; page: number; limit: number }>>(
      (resolve) => { resolveLibrary = resolve },
    )
    let resolveThought!: (result: ApiResult<{ contract_version: 1; items: [] }>) => void
    const pendingThought = new Promise<ApiResult<{ contract_version: 1; items: [] }>>(
      (resolve) => { resolveThought = resolve },
    )
    const getLinks = client.getLinks as ReturnType<typeof vi.fn>
    getLinks.mockImplementationOnce(() => pendingLibrary)
    thoughtClient.syncThoughts.mockImplementationOnce(() => pendingThought)

    fireEvent.click(screen.getByTitle('同步'))
    await waitFor(() => expect(thoughtClient.syncThoughts).toHaveBeenCalledTimes(2))
    expect(screen.queryByText(/资料库已同步；想法同步失败/)).not.toBeInTheDocument()

    const privateTransportText = 'opaque request body=private-note quote=private-target'
    await act(async () => {
      resolveLibrary(ok({ items: [], total: 0, page: 1, limit: 30 }))
      resolveThought({
        ok: false,
        error: {
          kind: 'other',
          status: 500,
          errorCode: 'thought_retry',
          message: privateTransportText,
        },
      })
      await pendingLibrary
      await pendingThought
    })

    expect(await screen.findByText(
      '资料库已同步；想法同步失败，0 项待同步（other:thought_retry:500）',
    )).toBeInTheDocument()
    expect(screen.queryByText(privateTransportText)).not.toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveAttribute(
      'data-error-code',
      'other:thought_retry:500',
    )
  })
})
