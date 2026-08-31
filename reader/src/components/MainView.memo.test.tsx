/**
 * PF2 的**调用侧**守卫。
 *
 * MarkdownView.memo.test.tsx 与 DetailPane.memo.test.tsx 都跑在合成 harness 上，
 * 用的是模块级稳定 props。那样测出来的是「memo 这个包装存在」，而不是「真实
 * 调用侧确实提供了稳定的 props」。后果是下面这些改动可以在 322 个测试全绿的
 * 情况下发生，而 PF2 的收益全部归零：
 *
 *   · MainView 把 onPickTag / onToggleFocus / onConvertToSite / previous / next
 *     任意一个改回 JSX 内联字面量 → DetailPane 的 memo 100% 失效
 *   · useReaderToc 的 onHeadings 去掉 useCallback → MarkdownView 在正文路径上
 *     的 memo 恒失效，长文每次局部状态变化都重新 parse
 *   · annotations 的 NO_ANNOTATIONS 改回 `[]` 字面量 → 「这篇还没有划线」这个
 *     最常见的情形下 memo 失效
 *
 * 本文件渲染**真实的 MainView 组件树**，在其上制造一次与文章无关的状态变化，
 * 断言 markdown 没有被重新 parse。上述三条改动都会让它变红。
 */
import 'fake-indexeddb/auto'

import { createHash } from 'node:crypto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'

import { MainView } from './MainView'
import { ok } from '@webtag/api'
import type { IdentityBoundReaderClient, ReaderClient } from '../lib/api/client'
import type { LinkResponse, TranslationResponse } from '../lib/api/types'
import { ownedDatabaseName } from '../lib/storage-ownership'
import { makeLink } from '../test/fixtures'
import { readerIdentity } from '../lib/identity'
import { commitAnnotationOperation } from '../lib/user-data/annotation-store'
import { resetUserDataDatabaseHandle } from '../lib/user-data/idb'
import { ENABLED_READER_CAPABILITIES } from '../test/capabilities'

type TestMainViewProps = Omit<React.ComponentProps<typeof MainView>, 'client'> & {
  readonly client: ReaderClient
}

function TestMainView(props: TestMainViewProps) {
  const lease = readerIdentity.activeLease
  if (!lease) throw new Error('test identity lease is not active')
  Object.defineProperty(props.client, 'identityLease', {
    configurable: true,
    value: lease,
  })
  const inboxClient = props.client as unknown as {
    listInbox?: ReaderClient['listInbox']
  }
  inboxClient.listInbox ??= vi.fn(async () => ok({ items: [], active_count: 0, expired_count: 0 }))
  const capabilities = Object.prototype.hasOwnProperty.call(props, 'capabilities')
    ? props.capabilities
    : ENABLED_READER_CAPABILITIES
  return <MainView {...props} capabilities={capabilities} client={props.client as IdentityBoundReaderClient} />
}

const probe = vi.hoisted(() => ({ parses: 0, detailRenders: 0 }))

function summarySourceHash(summary: string | null | undefined): string | null {
  return summary
    ? createHash('sha256').update(summary).digest('hex')
    : null
}

// 计数点选在 react-markdown 上：它才是真正昂贵的那一步（v10 的 Markdown() 在
// 组件体里直接跑 createProcessor + parse + runSync，无任何记忆化）。
vi.mock('react-markdown', () => ({
  default: ({ children }: { children?: string }) => {
    probe.parses += 1
    return <div data-testid="md-output">{children}</div>
  },
}))

// ArticlePager 只被 DetailPane 引用、且无条件渲染，因此它的渲染次数就是
// DetailPane 函数体的执行次数。用它而不是 Icon：Icon 同时被 Titlebar /
// Sidebar / ListPane 使用，开关 AI 侧栏本来就会让它们重渲染，信号会被淹没。
vi.mock('./ArticlePager', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./ArticlePager')>()
  return {
    ...actual,
    ArticlePager: (props: Parameters<typeof actual.ArticlePager>[0]) => {
      probe.detailRenders += 1
      return <actual.ArticlePager {...props} />
    },
  }
})

async function deleteUserDataDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(
      request.error ?? new Error('failed to delete user-data database'),
    )
    request.onblocked = () => reject(new Error('user-data database deletion was blocked'))
  })
}

beforeEach(() => {
  window.history.replaceState({}, '', '/?view=reading')
})

afterEach(async () => {
  localStorage.clear()
  vi.restoreAllMocks()
  await deleteUserDataDatabase()
})

function makeMemoClient(linkOver: Partial<LinkResponse> = {}): ReaderClient {
  // 带已保存原文：展开后会挂载走 headingIdPrefix/onHeadings 的那条
  // MarkdownView，useReaderToc 的回调稳定性才在测试范围内。
  const link = makeLink({
    id: 'L1',
    title: '真实组件树下的 memo 守卫',
    summary: '# 摘要标题\n\n这是一段会被 markdown 渲染的摘要正文。',
    content: '# 正文标题\n\n这是已保存的原文正文。',
    content_document: '# 正文标题\n\n这是已保存的原文正文。',
    content_format: 'markdown',
    content_revision: 1,
    created_at: '2026-06-11T10:00:00Z',
    ...linkOver,
  })
  // 三条链接、测试点开**中间**那条：只有这样 previous 与 next 才同时非空。
  // 链接不足时对应的 pager 恒为 null，内联字面量与 useMemo 产出的值完全一样，
  // 「pager 对象稳定性」根本进不了测试范围（previous 那一路曾因此漏测）。
  const newest = makeLink({
    id: 'L0', title: '最新的一篇', summary: '最新那篇的摘要',
    content: undefined, created_at: '2026-06-12T10:00:00Z',
  })
  const older = makeLink({
    id: 'L2', title: '较早的一篇', summary: '较早那篇的摘要',
    content: undefined, created_at: '2026-06-10T10:00:00Z',
  })
  return {
    isIdentityCurrent: vi.fn(() => true),
    getLinks: vi.fn(async (params?: { limit?: number }) =>
      ok({
        items: [newest, link, older],
        total: 3,
        page: 1,
        limit: params?.limit ?? 30,
      }),
    ),
    getLink: vi.fn(async (id: string) => {
      const target = id === 'L2' ? older : id === 'L0' ? newest : link
      return ok({
        ...target, id,
        has_content: Boolean(target.content),
        content: undefined, content_document: undefined,
      })
    }),
    getContent: vi.fn(async (id: string) =>
      ok({
        link_id: id,
        content: link.content ?? '',
        content_document: link.content_document,
        content_format: 'markdown' as const,
        fetcher_type: 'stored',
        // 必填字段。这份 fake 整体走 `as unknown as ReaderClient`，typecheck
        // 抓不到漏项——漏了也编译得过，只会在下次有人依赖它测代次相关行为时踩空。
        content_revision: link.content_revision ?? 0,
      }),
    ),
    getTags: vi.fn(async () => ok([])),
    getDomainSummaries: vi.fn(async () => ok({ domains: [], total: 0 })),
    getTranslations: vi.fn(async () =>
      ok({
        current_content_revision: link.content_revision ?? 0,
        current_summary_source_hash: summarySourceHash(link.summary),
        items: [] as TranslationResponse[],
      }),
    ),
    createTranslation: vi.fn(),
    refreshLink: vi.fn(),
    saveContent: vi.fn(),
    replaceContent: vi.fn(),
    testConnection: vi.fn(),
  } as unknown as ReaderClient
}

describe('MainView 调用侧 props 稳定性（PF2 守卫）', () => {
  it('MainView 的无关状态变化不触发文章重新 parse', async () => {
    probe.parses = 0
    probe.detailRenders = 0
    render(<TestMainView client={makeMemoClient()} onOpenSettings={() => {}} />)

    // 自动打开的是最新那篇；手动点开**中间**那条，让 previous 与 next 同时非空。
    await screen.findByRole('heading', { level: 1, name: '最新的一篇' })
    const middleCard = (await screen.findAllByText('真实组件树下的 memo 守卫'))
      .map((node) => node.closest<HTMLElement>('.reader-preview-card-main'))
      .find(Boolean)
    if (!middleCard) throw new Error('中间那条链接的列表卡片不存在')
    fireEvent.click(middleCard)

    await screen.findByRole('heading', { level: 1, name: '真实组件树下的 memo 守卫' })
    await waitFor(() => expect(probe.parses).toBeGreaterThan(0))

    // 展开原文：挂载走 headingIdPrefix / onHeadings 的那条 MarkdownView，
    // 把 useReaderToc 回调的稳定性也纳入本用例的覆盖范围。
    const toggle = await waitFor(() => {
      const node = document.querySelector('[aria-controls="orig-content-body"]')
      if (!node) throw new Error('原文折叠开关尚未出现')
      return node
    })
    fireEvent.click(toggle)
    await waitFor(() => expect(screen.getAllByTestId('md-output').length).toBeGreaterThan(1))

    // 等异步落定再取基线：详情、译文、列表校验都是异步的，它们的最后一次
    // 提交可能晚于上面的 waitFor。不等的话基线会把一次初始化渲染算到后面
    // 那批交互头上。
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    const parsesAfterMount = probe.parses
    const rendersAfterMount = probe.detailRenders
    expect(rendersAfterMount).toBeGreaterThan(0)

    // 制造与当前文章完全无关的 MainView 状态变化：开关 ⌘K 命令面板。
    //
    // 刻意**不用** chatOpen：它本身就是 DetailPane 的一个 prop（用于右栏互斥），
    // 拿它当"无关状态"会让本用例恒红——那是测试选错了刺激源，不是 memo 失效。
    // cmdkOpen 不在 DetailPane 的 props 里，是干净的信号。
    // 用 ⌘K 快捷键而不是点搜索胶囊：胶囊是 setCmdkOpen(true)，连点第二次
    // 值没变、React 直接 bail out，整个循环只产生**一次**状态变化——那样测不出
    // 「重渲染是否随交互次数线性增长」。⌘K 走的是 setCmdkOpen((o) => !o)，
    // 每一次都是真的状态翻转。
    const TOGGLES = 8
    for (let i = 0; i < TOGGLES; i += 1) {
      fireEvent.keyDown(window, { key: 'k', metaKey: true })
    }

    // 两条独立的断言，各自守住一层 memo：
    //   · DetailPane 整棵不该重渲染（守 MainView 传下来的回调 / pager 的稳定性）
    //   · 文章不该被重新 parse（守 MarkdownView 那一层，含 useReaderToc.onHeadings）
    // 判据是「不随交互次数增长」，不是「一次都不许有」：DetailPane 自己有
    // 内部状态（目录高亮、阅读进度），命令面板开合会碰到 resize / scroll 监听，
    // 从而产生**一次**与交互次数无关的收敛渲染。真正的回归形态是线性增长——
    // prop 不稳定时 8 次开合就是 8 次以上重渲染。
    const detailIncrement = probe.detailRenders - rendersAfterMount
    expect(detailIncrement).toBeLessThan(TOGGLES)
    // markdown 一次都不该被重新 parse。
    expect(probe.parses - parsesAfterMount).toBe(0)

    // 第二阶段：切换专注模式。focusMode **是** DetailPane 的 prop，所以它
    // 理应重渲染——但文章内容一个字没变，就不该被重新 parse。这一段守的是
    // DetailPane 内部传给 MarkdownView 的那些 prop（onClickHL、以及
    // useReaderToc 的 onHeadings）的稳定性：它们只在 DetailPane 真的重渲染
    // 时才有机会失稳，因此上一阶段测不到。
    const focusButton = document.querySelector<HTMLElement>('button[aria-label="专注模式"]')
    if (!focusButton) throw new Error('专注模式按钮不存在')
    const parsesBeforeFocus = probe.parses
    const rendersBeforeFocus = probe.detailRenders
    for (let i = 0; i < 4; i += 1) {
      fireEvent.click(
        document.querySelector<HTMLElement>('button[aria-label="专注模式"], button[aria-label="退出专注模式"]') ??
          focusButton,
      )
    }
    expect(probe.detailRenders).toBeGreaterThan(rendersBeforeFocus) // 确实重渲染了
    expect(probe.parses - parsesBeforeFocus).toBe(0)
  })

  // 上面那条用例没有 annotation，因此 `anns` 走冻结空值，引用天然稳定。这里通过
  // durable store 写入一个当前 summary target 和一个旧 content target，等真实
  // hook 只投影出 summary 后再取基线。这样 `anns` 是非空的合并结果；若
  // MainView 每次 render 都重建数组，memo(DetailPaneInner) 会随交互线性失效。
  //
  // 失配态并不罕见：任何「先在摘要上划线、之后保存/重抓原文」的链接都会长期停在
  // 这一支，正好是划线用得最重的那批链接，所以它需要自己的守卫。
  it('当前 summary 与旧正文划线并存时，anns 引用仍然稳定', async () => {
    probe.parses = 0
    probe.detailRenders = 0
    const lease = readerIdentity.activeLease
    if (!lease) throw new Error('test identity lease is not active')
    const summary = '# 摘要标题\n\n这是一段会被 markdown 渲染的摘要正文。'
    const sourceHash = summarySourceHash(summary)
    if (!sourceHash) throw new Error('summary source hash is unavailable')
    await expect(commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'memo-current-summary',
      linkId: 'L1',
      target: { kind: 'summary', sourceHash },
      draft: {
        id: 'keep',
        start: 0,
        end: 2,
        text: '摘要',
        note: '',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
      },
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
    await expect(commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'memo-old-content',
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 3 },
      draft: {
        id: 'history',
        blockKey: 'content',
        start: 0,
        end: 2,
        text: '正文',
        note: '',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
      },
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
    const client = makeMemoClient({ content_revision: 4 })
    render(<TestMainView client={client} onOpenSettings={() => {}} />)

    await screen.findByRole('heading', { level: 1, name: '最新的一篇' })
    const middleCard = (await screen.findAllByText('真实组件树下的 memo 守卫'))
      .map((node) => node.closest<HTMLElement>('.reader-preview-card-main'))
      .find(Boolean)
    if (!middleCard) throw new Error('中间那条链接的列表卡片不存在')
    fireEvent.click(middleCard)

    await screen.findByRole('heading', { level: 1, name: '真实组件树下的 memo 守卫' })
    await screen.findByText('划线与想法 · 1')
    await screen.findByRole('button', { name: '已归档想法 (1)' })
    await waitFor(() => expect(client.getLink).toHaveBeenCalledWith('L1'))
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    const rendersAfterMount = probe.detailRenders
    expect(rendersAfterMount).toBeGreaterThan(0)

    const TOGGLES = 8
    await act(async () => {
      for (let i = 0; i < TOGGLES; i += 1) {
        fireEvent.keyDown(window, { key: 'k', metaKey: true })
      }
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    // 判据同上一条：看的是「不随交互次数线性增长」，不是「一次都不许有」。
    expect(probe.detailRenders - rendersAfterMount).toBeLessThan(TOGGLES)
  })

  it('对照组：切换到另一篇文章时仍然重新 parse', async () => {
    // created_at 必须显式且不同：列表按它降序排，两条相同的话排序结果不确定，
    // 「自动打开的是哪一篇」就成了掷骰子。
    const first = makeLink({
      id: 'L1', title: '第一篇', summary: '第一篇的摘要正文',
      content: undefined, created_at: '2026-06-11T10:00:00Z',
    })
    const second = makeLink({
      id: 'L2', title: '第二篇', summary: '第二篇的摘要正文',
      content: undefined, created_at: '2026-06-10T10:00:00Z',
    })
    const getTranslations = vi.fn(async (id: string) => {
      const target = id === 'L2' ? second : first
      return ok({
        current_content_revision: 0,
        current_summary_source_hash: summarySourceHash(target.summary),
        items: [] as TranslationResponse[],
      })
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
      getLink: vi.fn(async (id: string) => {
        const target = id === 'L2' ? second : first
        return ok({ ...target, has_content: false, content: undefined, content_document: undefined })
      }),
      getContent: vi.fn(async () => ({
        ok: false as const,
        error: { kind: 'other' as const, message: 'no content' },
      })),
      getTags: vi.fn(async () => ok([])),
      getDomainSummaries: vi.fn(async () => ok({ domains: [], total: 0 })),
      getTranslations,
      createTranslation: vi.fn(),
      refreshLink: vi.fn(),
      saveContent: vi.fn(),
      replaceContent: vi.fn(),
      testConnection: vi.fn(),
    } as unknown as ReaderClient

    probe.parses = 0
    render(<TestMainView client={client} onOpenSettings={() => {}} />)
    await screen.findByRole('heading', { level: 1, name: '第一篇' })
    await waitFor(() => expect(probe.parses).toBeGreaterThan(0))
    await waitFor(() => expect(getTranslations).toHaveBeenCalledTimes(1))
    const afterMount = probe.parses

    const secondCard = (await screen.findAllByText('第二篇'))
      .map((node) => node.closest<HTMLElement>('.reader-preview-card-main'))
      .find(Boolean)
    if (!secondCard) throw new Error('第二篇的列表卡片不存在')
    fireEvent.click(secondCard)

    await screen.findByRole('heading', { level: 1, name: '第二篇' })
    await waitFor(() => expect(getTranslations).toHaveBeenCalledTimes(2))
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })
    expect(probe.parses).toBeGreaterThan(afterMount)
  })
})
