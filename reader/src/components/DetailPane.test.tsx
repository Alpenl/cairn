import { useCallback, useMemo, useRef } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { DetailPane, type DetailPaneProps } from './DetailPane'
import { makeLink } from '../test/fixtures'
import { err, ok } from '@webtag/api'
import { contentCacheKey } from '../lib/cache/keys'
import { DEFAULT_CAPACITY, resourceStore } from '../lib/cache/store'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { LinkContentResponse, LinkResponse, TranslationResponse } from '../lib/api/types'
import type { DocumentCommandContext, SavedArticleDocument } from '../lib/article/document'
import { IdentityLease } from '../lib/identity'
import { useSavedArticleDocument } from '../hooks/useSavedArticleDocument'
import { isContentAnchored, type Annotation } from '../lib/annotations'

function translation(over: Partial<TranslationResponse> = {}): TranslationResponse {
  return {
    id: 'T1',
    link_id: 'L1',
    scope: 'selection',
    block_key: 'summary',
    start_offset: 0,
    end_offset: 5,
    source_text: 'hello',
    translated_text: '你好',
    source_format: 'plain',
    target_language: 'zh-CN',
    status: 'done',
    model: 'grok-4.3-fast',
    error_msg: null,
    source_content_revision: 1,
    stale: false,
    created_at: '2026-07-15T00:00:00Z',
    updated_at: '2026-07-15T00:00:00Z',
    ...over,
  }
}

function documentFor(link: LinkResponse | null | undefined): SavedArticleDocument | null {
  if (!link) return null
  const revision = link.content_revision ?? 1
  const body = link.content && revision > 0
    ? {
        status: 'ready' as const,
        revision,
        data: {
          content: link.content,
          content_document: link.content_document,
          content_format: link.content_format ?? 'plain' as const,
          content_source: link.content_source ?? 'fetched',
        },
      }
    : { status: 'idle' as const }
  return {
    id: { namespace: 'test-namespace', linkId: link.id, contentRevision: revision },
    generation: 1,
    detail: { status: 'ready', revision, data: { ...link, content_revision: revision } },
    body,
    annotations: { status: 'idle' },
    translations: { status: 'idle' },
  }
}

type TestDetailPaneProps = Omit<DetailPaneProps, 'onLoadContent'> & {
  loadBodySource: (context: DocumentCommandContext) => Promise<unknown>
}

function props(over: Partial<TestDetailPaneProps> = {}): TestDetailPaneProps {
  const link = over.l === undefined ? makeLink() : over.l
  const savedDocument = over.document === undefined ? documentFor(link) : over.document
  return {
    l: link,
    document: savedDocument,
    captureDocumentContext: () => null,
    onBack: vi.fn(),
    chatOpen: false,
    onChat: vi.fn(),
    onToast: vi.fn(),
    curTag: null,
    onPickTag: vi.fn(),
    corpus: [],
    annotationsEnabled: true,
    aiEnabled: true,
    relatedTagsEnabled: true,
    engagementEnabled: true,
    anns: [],
    onAddAnn: vi.fn(async (annotation) => {
      if (isContentAnchored(annotation.blockKey)) {
        return {
          id: 'an1',
          blockKey: annotation.blockKey,
          target: {
            kind: 'saved-content' as const,
            contentRevision: savedDocument?.id.contentRevision ?? 1,
          },
        }
      }
      return annotation.blockKey === 'summary'
        ? {
            id: 'an1',
            blockKey: 'summary' as const,
            target: { kind: 'summary' as const, sourceHash: 'a'.repeat(64) },
          }
        : null
    }),
    onRemoveAnn: vi.fn(async () => true),
    onOpenNote: vi.fn(),
    onAskAI: vi.fn(),
    onSaveContent: vi.fn(),
    onReplaceContent: vi.fn(),
    loadBodySource: vi.fn(async () => null),
    savingContent: null,
    translations: [],
    translationsLoading: false,
    summarySourceHash: 'a'.repeat(64),
    onTranslateSelection: vi.fn(async () => null),
    onTranslateFull: vi.fn(),
    focusMode: false,
    onToggleFocus: vi.fn(),
    ...over,
  }
}

function TestDetailPane(input: TestDetailPaneProps) {
  const { loadBodySource, ...paneProps } = input
  const lease = useRef(new IdentityLease({
    serverClientDataNamespace: 'detail-test-server',
    physicalNamespace: 'detail-test-namespace',
    localEpoch: 1,
  })).current
  const detail = useMemo(() => {
    if (!input.l || input.l.content_revision !== undefined) return input.l
    return { ...input.l, content_revision: 1 }
  }, [input.l])
  const loadBody = useCallback(async (context: DocumentCommandContext) => {
    const response = await loadBodySource(context)
    if (
      response &&
      typeof response === 'object' &&
      'link_id' in response &&
      'content_revision' in response
    ) {
      return ok(response as LinkContentResponse)
    }
    return err<LinkContentResponse>({ kind: 'other', message: 'test body unavailable' })
  }, [loadBodySource])
  const owner = useSavedArticleDocument({ lease, detail, loadBody })

  return (
    <DetailPane
      {...paneProps}
      l={detail}
      document={owner.document}
      captureDocumentContext={owner.captureContext}
      onLoadContent={owner.loadBody}
    />
  )
}

// 原文默认折叠（点击眉标展开）。凡是断言原文内容的用例都要先展开——这正是
// 用户的真实路径，helper 只是把这一步写短。
function expandOriginal(scope: HTMLElement = document.body) {
  const toggle = scope.querySelector('[aria-controls="orig-content-body"]')
  if (!toggle) throw new Error('原文折叠开关不存在（可能这条链接没有已保存原文）')
  fireEvent.click(toggle)
}

function selectElementText(element: HTMLElement) {
  const range = document.createRange()
  range.selectNodeContents(element)
  Object.defineProperty(range, 'getBoundingClientRect', {
    value: () => new DOMRect(20, 20, 160, 24),
  })
  window.getSelection()?.removeAllRanges()
  window.getSelection()?.addRange(range)
  fireEvent(document, new Event('selectionchange'))
}

describe('DetailPane 空态', () => {
  it('无选中链接显示引导', () => {
    render(<TestDetailPane {...props({ l: null })} />)
    expect(screen.getByText('选择一条链接查看详情')).toBeInTheDocument()
  })
})

describe('DetailPane 链接信息编辑', () => {
  it('从阅读资料打开草稿，取消后恢复原投影且不发起保存', () => {
    const onEditMetadata = vi.fn(async () => ({
      status: 'saved' as const,
      metadataRevision: 2,
    }))
    const { container } = render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'L-METADATA-CANCEL',
            library_kind: 'reading',
            status: 'done',
            title: '原始标题',
            summary: '原始摘要',
            tags: ['Alpha', 'Beta'],
          }),
          onEditMetadata,
        })}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '编辑链接信息' }))

    expect(screen.getByRole('textbox', { name: '链接标题' })).toHaveValue('原始标题')
    expect(screen.getByRole('textbox', { name: '链接摘要' })).toHaveValue('原始摘要')
    expect(screen.getByRole('textbox', { name: '链接标签' })).toHaveValue('Alpha, Beta')

    fireEvent.change(screen.getByRole('textbox', { name: '链接标题' }), {
      target: { value: '未保存标题' },
    })
    fireEvent.change(screen.getByRole('textbox', { name: '链接摘要' }), {
      target: { value: '未保存摘要' },
    })
    fireEvent.click(screen.getByRole('button', { name: '取消编辑链接信息' }))

    expect(screen.queryByRole('textbox', { name: '链接标题' })).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1, name: '原始标题' })).toBeInTheDocument()
    expect(screen.getByText('原始摘要')).toBeInTheDocument()
    const articleTags = container.querySelector<HTMLElement>('.art-tags')
    expect(articleTags).not.toBeNull()
    expect(within(articleTags as HTMLElement).getByRole('button', { name: '#Alpha' })).toBeInTheDocument()
    expect(within(articleTags as HTMLElement).getByRole('button', { name: '#Beta' })).toBeInTheDocument()
    expect(onEditMetadata).not.toHaveBeenCalled()
  })

  it('保存时将空文本归一为 null，并拆分、去重标签', async () => {
    const onEditMetadata = vi.fn(async () => ({
      status: 'saved' as const,
      metadataRevision: 2,
    }))
    render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'L-METADATA-SAVE',
            library_kind: 'reading',
            status: 'done',
            metadata_revision: 1,
          }),
          onEditMetadata,
        })}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '编辑链接信息' }))
    fireEvent.change(screen.getByRole('textbox', { name: '链接标题' }), {
      target: { value: '   ' },
    })
    fireEvent.change(screen.getByRole('textbox', { name: '链接摘要' }), {
      target: { value: '\n  ' },
    })
    fireEvent.change(screen.getByRole('textbox', { name: '链接标签' }), {
      target: { value: ' Alpha, alpha  Beta，Gamma beta ' },
    })
    fireEvent.click(screen.getByRole('button', { name: '保存链接信息' }))

    await waitFor(() => {
      expect(onEditMetadata).toHaveBeenCalledWith('L-METADATA-SAVE', 1, {
        title: null,
        summary: null,
        tags: ['Alpha', 'Beta', 'Gamma'],
      })
    })
    await waitFor(() => {
      expect(screen.queryByRole('textbox', { name: '链接标题' })).not.toBeInTheDocument()
    })
  })

  it('保存时按完整 Unicode case folding 去重且保留首个拼写与顺序', async () => {
    const onEditMetadata = vi.fn(async () => ({
      status: 'saved' as const,
      metadataRevision: 2,
    }))
    render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'L-METADATA-UNICODE-FOLD',
            library_kind: 'reading',
            status: 'done',
            metadata_revision: 1,
          }),
          onEditMetadata,
        })}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '编辑链接信息' }))
    fireEvent.change(screen.getByRole('textbox', { name: '链接标签' }), {
      target: { value: ' \u03a3, \u03c2, Stra\u00dfe, STRASSE ' },
    })
    fireEvent.click(screen.getByRole('button', { name: '保存链接信息' }))

    await waitFor(() => {
      expect(onEditMetadata).toHaveBeenCalledWith('L-METADATA-UNICODE-FOLD', 1, {
        title: '示例标题',
        summary: '这是一段 AI 摘要正文。',
        tags: ['\u03a3', 'Stra\u00dfe'],
      })
    })
  })

  it.each([
    ['非完成资料', makeLink({ library_kind: 'reading', status: 'pending' })],
    ['网站资料', makeLink({ library_kind: 'site', status: 'done' })],
    ['零 metadata revision 的资料', makeLink({ library_kind: 'reading', status: 'done', metadata_revision: 0 })],
    ['非安全 metadata revision 的资料', makeLink({ library_kind: 'reading', status: 'done', metadata_revision: Number.MAX_SAFE_INTEGER + 1 })],
  ])('%s 不显示编辑链接信息入口', (_name, link) => {
    render(<TestDetailPane {...props({ l: link })} />)

    expect(screen.queryByRole('button', { name: '编辑链接信息' })).not.toBeInTheDocument()
  })
})

describe('DetailPane 文章导航', () => {
  it('用原生按钮展示并触发上一篇与下一篇', () => {
    const onPrevious = vi.fn()
    const onNext = vi.fn()
    render(
      <TestDetailPane
        {...props({
          previous: { title: '上一篇标题', onSelect: onPrevious },
          next: { title: '下一篇标题', onSelect: onNext },
        })}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '上一条：上一篇标题' }))
    fireEvent.click(screen.getByRole('button', { name: '下一条：下一篇标题' }))

    expect(onPrevious).toHaveBeenCalledTimes(1)
    expect(onNext).toHaveBeenCalledTimes(1)
  })
})

describe('DetailPane 阅读模式控制', () => {
  it('切换专注模式并在专注状态下支持 Escape 退出', () => {
    const onToggleFocus = vi.fn()
    const { rerender } = render(
      <TestDetailPane {...props({ onToggleFocus })} />,
    )

    fireEvent.click(screen.getByRole('button', { name: '专注模式' }))
    expect(onToggleFocus).toHaveBeenCalledTimes(1)

    rerender(<TestDetailPane {...props({ focusMode: true, onToggleFocus })} />)
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(onToggleFocus).toHaveBeenCalledTimes(2)
  })

  it('持久化字号和行距并在重新挂载后恢复', () => {
    const first = render(<TestDetailPane {...props()} />)
    fireEvent.click(screen.getByRole('button', { name: '阅读偏好' }))
    fireEvent.click(screen.getByRole('button', { name: 'A+' }))
    fireEvent.click(screen.getByRole('button', { name: '舒适' }))

    expect(localStorage.getItem('webtag:reading-preference')).toBe(
      JSON.stringify({ size: 2, lineHeight: 2 }),
    )
    first.unmount()

    render(<TestDetailPane {...props()} />)
    fireEvent.click(screen.getByRole('button', { name: '阅读偏好' }))
    expect(screen.getByText('17.5px')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '宽松' })).toBeInTheDocument()
  })

  it('把共享进度和回顶动作接到保存资料滚动容器', () => {
    const { container } = render(<TestDetailPane {...props()} />)
    const scroller = container.querySelector('.reader-scroll') as HTMLElement
    Object.defineProperties(scroller, {
      scrollHeight: { configurable: true, value: 1000 },
      clientHeight: { configurable: true, value: 100 },
      scrollTop: { configurable: true, writable: true, value: 450 },
      scrollTo: { configurable: true, value: vi.fn() },
    })
    fireEvent.scroll(scroller)

    expect(container.querySelector('.read-progress')).toHaveStyle({ width: '50%' })
    fireEvent.click(screen.getByRole('button', { name: '回到顶部' }))
    expect(scroller.scrollTo).toHaveBeenCalledWith({ top: 0, behavior: 'smooth' })
  })
})

describe('DetailPane capability gates', () => {
  const annotation: Annotation = {
    id: 'capability-annotation',
    blockKey: 'summary',
    start: 0,
    end: 10,
    text: 'Capability',
    note: '受保护想法',
    source: 'self',
    createdAt: 1,
    updatedAt: 1,
    sourceSummaryHash: 'a'.repeat(64),
  }

  it('hides annotation-owned UI and selection actions when annotations are unavailable', async () => {
    const onAddAnn = vi.fn(async () => null)
    const onAskAI = vi.fn()
    render(
      <TestDetailPane
        {...props({
          l: makeLink({ summary: 'Capability protected selection' }),
          annotationsEnabled: false,
          anns: [annotation],
          onAddAnn,
          onAskAI,
        })}
      />,
    )

    expect(screen.queryByTitle('划线与想法')).not.toBeInTheDocument()
    const summaryText = await waitFor(() => {
      const element = screen.getByText('Capability protected selection')
      expect(element.closest('[data-hl-block="summary"]')).toHaveClass('md')
      return element
    })
    selectElementText(summaryText)

    expect(await screen.findByRole('button', { name: '翻译' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '复制' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '划线' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '写想法' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '问 AI' })).not.toBeInTheDocument()
    expect(onAddAnn).not.toHaveBeenCalled()
    expect(onAskAI).not.toHaveBeenCalled()
  })

  it('keeps annotation actions but hides AI entry points when AI is unavailable', async () => {
    const onChat = vi.fn()
    const onAskAI = vi.fn()
    render(
      <TestDetailPane
        {...props({
          l: makeLink({ summary: 'AI protected selection' }),
          aiEnabled: false,
          onChat,
          onAskAI,
        })}
      />,
    )

    expect(screen.queryByTitle('AI 助手 (⌘J)')).not.toBeInTheDocument()
    selectElementText(screen.getByText('AI protected selection'))

    expect(await screen.findByRole('button', { name: '划线' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '写想法' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '问 AI' })).not.toBeInTheDocument()
    expect(onChat).not.toHaveBeenCalled()
    expect(onAskAI).not.toHaveBeenCalled()
  })

  it('hides progress UI and sends no engagement request when engagement is unavailable', () => {
    const lease = new IdentityLease({
      serverClientDataNamespace: 'detail-engagement-server',
      physicalNamespace: 'detail-engagement-physical',
      localEpoch: 1,
    })
    resourceStore.activateIdentity(lease)
    const engagement = {
      link_id: 'L1',
      read: false,
      progress: 0,
      read_later: false,
      last_opened: null,
      updated_at: '2026-08-11T00:00:00Z',
    }
    const getEngagement = vi.fn(async () => ok(engagement))
    const patchEngagement = vi.fn(async () => ok(engagement))
    const readerClient = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
      getEngagement,
      patchEngagement,
    } as unknown as IdentityBoundReaderClient

    const { container } = render(
      <TestDetailPane
        {...props({
          l: makeLink({ id: 'L1' }),
          readerClient,
          engagementEnabled: false,
        })}
      />,
    )
    const scroller = container.querySelector('.reader-scroll') as HTMLElement
    Object.defineProperties(scroller, {
      scrollHeight: { configurable: true, value: 1000 },
      clientHeight: { configurable: true, value: 100 },
      scrollTop: { configurable: true, writable: true, value: 450 },
    })
    fireEvent.scroll(scroller)

    expect(screen.queryByRole('progressbar', { name: '阅读进度' })).not.toBeInTheDocument()
    expect(container.querySelector('.read-progress')).toBeNull()
    expect(getEngagement).not.toHaveBeenCalled()
    expect(patchEngagement).not.toHaveBeenCalled()
    resourceStore.deactivateIdentity(lease)
  })
})

describe('DetailPane 阅读布局语义', () => {
  it('正文元信息与结构化原文共享完整阅读画布', () => {
    const { container } = render(
      <TestDetailPane
        {...props({
          l: makeLink({
            status: 'done',
            title: '响应式阅读案例',
            summary: '这是一段摘要。',
            content: '原始正文',
            content_format: 'markdown',
            content_document: '正文段落\n\n![图](https://example.com/chart.png)',
          }),
        })}
      />,
    )
    expect(container.querySelector('.reader-pane')).toHaveAttribute('data-reading-source-kind', 'markdown')
    expect(container.querySelector('.reader-pane')).toHaveAttribute('data-reading-source-block', 'summary')
    expandOriginal(container)

    expect(screen.getByRole('heading', { level: 1, name: '响应式阅读案例' })).toHaveClass(
      'reader-prose',
    )
    expect(container.querySelector('.art-source')).toHaveClass('reader-prose')
    expect(container.querySelector('.summary-lead')).toHaveClass('reader-prose')
    expect(container.querySelector('.orig-content-head')).toHaveClass('reader-prose')
    expect(container.querySelector('.orig-content')).toHaveClass('reader-flow')
    expect(container.querySelector('.reader-pane')).toHaveAttribute('data-reading-source-kind', 'markdown')
    expect(container.querySelector('.reader-pane')).toHaveAttribute('data-reading-source-block', 'content-document')
  })
})

describe('DetailPane 已归档想法折叠入口', () => {
  const historical = {
    status: 'historical' as const,
    reason: 'ambiguous-quote' as const,
    sourceContentRevision: 1,
    sourceKey: 'saved-content:1:historical:ambiguous-quote',
    annotation: {
      id: 'historical-1',
      blockKey: 'content',
      start: 0,
      end: 8,
      text: '旧正文片段',
      note: '保留这个想法',
      source: 'self' as const,
      createdAt: 1,
      updatedAt: 1,
      sourceContentRevision: 1,
    },
  }

  it('默认折叠已归档想法，展开后通过独立回调打开历史 annotation', () => {
    const onOpenHistoricalAnnotation = vi.fn()
    render(
      <TestDetailPane
        {...props({
          historicalAnnotations: [historical],
          onOpenHistoricalAnnotation,
        })}
      />,
    )

    const toggle = screen.getByRole('button', { name: '已归档想法 (1)' })
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('保留这个想法')).not.toBeInTheDocument()

    fireEvent.click(toggle)
    expect(toggle).toHaveAttribute('aria-expanded', 'true')
    const item = screen.getByRole('button', { name: /旧正文片段.*保留这个想法/ })
    fireEvent.click(item)

    expect(onOpenHistoricalAnnotation).toHaveBeenCalledWith(historical.annotation)
  })

  it('同一链接与 revision 的 degraded 状态只通知一次', () => {
    const onToast = vi.fn()
    const link = makeLink({ id: 'L-HISTORICAL-DEGRADED' })
    const { rerender } = render(
      <TestDetailPane {...props({ l: link, historicalDegraded: true, onToast })} />,
    )

    expect(onToast).toHaveBeenCalledTimes(1)
    rerender(<TestDetailPane {...props({ l: link, historicalDegraded: true, onToast })} />)
    expect(onToast).toHaveBeenCalledTimes(1)
    expect(onToast).toHaveBeenCalledWith('部分划线已归档为想法', 'alert')
  })
})

describe('DetailPane 正文目录', () => {
  const tocLink = () =>
    makeLink({
      id: 'LTOC',
      status: 'done',
      content: '纯文本投影',
      content_format: 'markdown',
      content_document: '# 开头\n\n正文\n\n## 中段\n\n正文\n\n### 末段\n\n正文',
    })

  it('按正文标题生成目录，缩进反映层级', async () => {
    render(<TestDetailPane {...props({ l: tocLink() })} />)
    expandOriginal()

    const toc = await screen.findByRole('navigation', { name: '正文目录' })
    const items = within(toc).getAllByRole('button')
    expect(items.map((item) => item.textContent)).toEqual(['开头', '中段', '末段'])
    expect(items.map((item) => item.getAttribute('style'))).toEqual([
      'padding-inline-start: 0;',
      'padding-inline-start: 11px;',
      'padding-inline-start: 22px;',
    ])
  })

  it('点条目滚动到对应标题并高亮当前章节', () => {
    const scrollTo = vi.fn()
    const original = Element.prototype.scrollTo
    Element.prototype.scrollTo = scrollTo as unknown as typeof original
    try {
      const { container } = render(<TestDetailPane {...props({ l: tocLink() })} />)
      expandOriginal(container)
      const heading = container.querySelector('#toc-h1') as HTMLElement
      vi.spyOn(heading, 'getBoundingClientRect').mockReturnValue({ top: 420 } as DOMRect)
      const scroller = container.querySelector('.reader-scroll') as HTMLElement
      vi.spyOn(scroller, 'getBoundingClientRect').mockReturnValue({ top: 100 } as DOMRect)
      scroller.scrollTop = 60

      fireEvent.click(screen.getByRole('button', { name: '中段' }))

      expect(scrollTo).toHaveBeenCalledWith({ top: 60 + 320 - 18, behavior: 'smooth' })
      expect(screen.getByRole('button', { name: '中段' })).toHaveAttribute('aria-current', 'true')
      // 焦点交给标题，键盘 / 读屏用户接着往下读而不是留在目录里。
      expect(document.activeElement).toBe(heading)
      expect(heading).toHaveAttribute('tabindex', '-1')
    } finally {
      Element.prototype.scrollTo = original
    }
  })

  it('换到大纲文字相同的另一篇文章，目录依然在（不被内容判重吃掉）', async () => {
    const outline = '# 简介\n\n正文\n\n## 安装\n\n正文\n\n### 配置\n\n正文'
    const first = makeLink({ id: 'LA', status: 'done', content: '纯文本', content_format: 'markdown', content_document: outline })
    const second = makeLink({ id: 'LB', status: 'done', content: '纯文本', content_format: 'markdown', content_document: outline })

    const { rerender } = render(<TestDetailPane {...props({ l: first })} />)
    expandOriginal()
    expect(await screen.findByRole('navigation', { name: '正文目录' })).toBeInTheDocument()

    rerender(<TestDetailPane {...props({ l: second })} />)
    // 换文章会回到折叠态，目录也随之消失——这是「默认折叠」的应有之义。
    expect(screen.queryByRole('navigation', { name: '正文目录' })).not.toBeInTheDocument()
    expandOriginal()

    const toc = await screen.findByRole('navigation', { name: '正文目录' })
    expect(within(toc).getAllByRole('button').map((item) => item.textContent)).toEqual(['简介', '安装', '配置'])
  })

  it('少于三条标题不出目录', () => {
    render(
      <TestDetailPane
        {...props({
          l: makeLink({ id: 'L1H', status: 'done', content: '正文', content_format: 'markdown', content_document: '# 标题一\n\n正文\n\n## 标题二\n\n正文' }),
        })}
      />,
    )
    expect(screen.queryByRole('navigation', { name: '正文目录' })).not.toBeInTheDocument()
  })

  it('切到纯文本视图后目录消失，切回排版又回来', async () => {
    render(<TestDetailPane {...props({ l: tocLink() })} />)
    expandOriginal()
    expect(await screen.findByRole('navigation', { name: '正文目录' })).toBeInTheDocument()

    fireEvent.click(screen.getByText('文本'))
    expect(screen.queryByRole('navigation', { name: '正文目录' })).not.toBeInTheDocument()

    fireEvent.click(screen.getByText('排版'))
    expect(await screen.findByRole('navigation', { name: '正文目录' })).toBeInTheDocument()
  })

  it('切到中文译文时目录跟着换成译文的标题', async () => {
    const link = tocLink()
    render(
      <TestDetailPane
        {...props({
          l: link,
          translations: [
            translation({
              id: 'TF1',
              link_id: link.id,
              scope: 'full',
              block_key: 'content-document',
              source_text: link.content_document as string,
              translated_text: '# Opening\n\n body\n\n## Middle\n\nbody\n\n### Details\n\nbody',
              source_format: 'markdown',
            }),
          ],
        })}
      />,
    )
    expandOriginal()

    fireEvent.click(screen.getByRole('button', { name: '中文译文' }))

    const toc = await screen.findByRole('navigation', { name: '正文目录' })
    expect(within(toc).getAllByRole('button').map((item) => item.textContent)).toEqual([
      'Opening',
      'Middle',
      'Details',
    ])
    expect(document.querySelector('.reader-pane')).toHaveAttribute('data-reading-source-block', 'content-translation')
  })
})

describe('DetailPane 原文折叠', () => {
  const contentLink = (over = {}) =>
    makeLink({
      id: 'LFOLD',
      status: 'done',
      summary: '摘要',
      content: '折叠起来的原文正文',
      content_format: 'markdown',
      content_document: '# 一级\n\n折叠起来的原文正文\n\n## 二级\n\n更多正文\n\n### 三级\n\n最后正文',
      ...over,
    })

  it('默认折叠：只有眉标和展开提示，正文与工具条都不渲染', () => {
    const { container } = render(<TestDetailPane {...props({ l: contentLink() })} />)

    const toggle = container.querySelector('[aria-controls="orig-content-body"]') as HTMLElement
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(toggle.textContent).toContain('点击展开')
    // 容器常驻但 hidden——aria-controls 指向的元素必须始终存在。
    expect(container.querySelector('#orig-content-body')).toHaveAttribute('hidden')
    expect(container.querySelector('#orig-content-body')?.textContent).toBe('')
    expect(screen.queryByText('折叠起来的原文正文')).not.toBeInTheDocument()
    // 工具条（排版/文本、翻译全文、重新抓取）跟着正文一起收起，不在折叠态下占位。
    expect(screen.queryByRole('button', { name: '翻译全文' })).not.toBeInTheDocument()
    expect(screen.queryByText('重新抓取')).not.toBeInTheDocument()
    // 摘要不受影响——它才是详情页的主角。
    expect(screen.getByText('摘要')).toBeInTheDocument()
  })

  it('点一下展开，再点一下收回', async () => {
    const { container } = render(<TestDetailPane {...props({ l: contentLink() })} />)

    expandOriginal(container)
    expect(container.querySelector('[aria-controls="orig-content-body"]')).toHaveAttribute('aria-expanded', 'true')
    expect(await screen.findByText('折叠起来的原文正文')).toBeInTheDocument()
    expect(screen.getByText('重新抓取')).toBeInTheDocument()

    expandOriginal(container)
    expect(screen.queryByText('折叠起来的原文正文')).not.toBeInTheDocument()
  })

  it('换文章回到折叠态，目录也跟着作废（不留指向空锚点的条目）', async () => {
    const { container, rerender } = render(<TestDetailPane {...props({ l: contentLink() })} />)
    expandOriginal(container)
    expect(await screen.findByRole('navigation', { name: '正文目录' })).toBeInTheDocument()

    rerender(<TestDetailPane {...props({ l: contentLink({ id: 'LFOLD2' }) })} />)

    expect(container.querySelector('[aria-controls="orig-content-body"]')).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByRole('navigation', { name: '正文目录' })).not.toBeInTheDocument()
  })

  it('全文翻译失败时，折叠态也看得见错误与「展开重试」', () => {
    const link = contentLink()
    const failed = translation({
      id: 'TFAIL',
      link_id: link.id,
      scope: 'full',
      block_key: 'content',
      status: 'failed',
      translated_text: null,
      error_msg: '翻译服务暂时不可用，请重试',
      source_content_revision: 1,
    })
    const { container } = render(
      <TestDetailPane {...props({ l: link, translations: [failed] })} />,
    )

    // 折叠着——失败提示不能被一起收起来，否则用户永远不知道要点开看。
    expect(container.querySelector('[aria-controls="orig-content-body"]')).toHaveAttribute('aria-expanded', 'false')
    expect(screen.getByRole('alert')).toHaveTextContent('翻译服务暂时不可用，请重试')

    fireEvent.click(screen.getByRole('button', { name: '展开重试' }))
    expect(container.querySelector('[aria-controls="orig-content-body"]')).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByRole('button', { name: '重试全文翻译' })).toBeInTheDocument()
  })

  // 「展开重试」是眉标之外的**第二条展开入口**。它曾经裸调 setContentOpen(true)，
  // 不取正文——正文还没加载时，展开出来的是一片完全空白：没有正文、没有加载态、
  // 也没有错误块（`savedContent === null ? null` 那一支）。
  it('「展开重试」也会取正文，不会展开出一片空白', async () => {
    const failed = translation({
      id: 'TFAIL',
      link_id: 'LLAZY2',
      scope: 'full',
      block_key: 'content',
      status: 'failed',
      translated_text: null,
      error_msg: '翻译服务暂时不可用，请重试',
      source_content_revision: 9,
    })
    const lazy = makeLink({ id: 'LLAZY2', status: 'done', summary: '摘要', has_content: true, content_revision: 9 })
    const body = {
      link_id: 'LLAZY2',
      content: '重试后取回的正文',
      content_format: 'plain' as const,
      fetcher_type: 'stored',
      content_revision: 9,
    }
    const loadBodySource = vi.fn(async () => body)
    const { container } = render(
      <TestDetailPane {...props({ l: lazy, translations: [failed], loadBodySource })} />,
    )

    fireEvent.click(screen.getByRole('button', { name: '展开重试' }))

    expect(loadBodySource).toHaveBeenCalledWith(expect.objectContaining({
      id: expect.objectContaining({ linkId: 'LLAZY2', contentRevision: 9 }),
    }))
    expect(await screen.findByText('重试后取回的正文')).toBeInTheDocument()
    expect(container.querySelector('#orig-content-body')?.innerHTML).not.toBe('')
  })

  it('未保存原文时没有折叠开关，仍然是「保存原文」按钮', () => {
    const { container } = render(
      <TestDetailPane {...props({ l: makeLink({ id: 'LNONE', status: 'done', summary: '摘要' }) })} />,
    )
    expect(container.querySelector('[aria-controls="orig-content-body"]')).toBeNull()
    expect(screen.getByText('保存原文')).toBeInTheDocument()
  })
})

describe('DetailPane 原文按需加载', () => {
  const lazyLink = () =>
    makeLink({ id: 'LLAZY', status: 'done', summary: '摘要', has_content: true, content_revision: 7 })
  const snapshot = {
    link_id: 'LLAZY',
    content: '按需取回的正文',
    content_document: '# 标题\n\n按需取回的正文',
    content_format: 'markdown' as const,
    fetcher_type: 'stored',
    content_revision: 7,
  }

  it('进详情页不请求原文，展开时才请求', async () => {
    const loadBodySource = vi.fn(async () => snapshot)
    const { container } = render(<TestDetailPane {...props({ l: lazyLink(), loadBodySource })} />)

    // 光是打开文章：一次都没请求过正文。
    expect(loadBodySource).not.toHaveBeenCalled()
    expect(container.querySelector('[aria-controls="orig-content-body"]')).toBeInTheDocument()

    expandOriginal(container)

    // revision 由 document owner 捕获并交给 source；DetailPane 只发出零参数命令。
    expect(loadBodySource).toHaveBeenCalledWith(expect.objectContaining({
      id: expect.objectContaining({ linkId: 'LLAZY', contentRevision: 7 }),
    }))
    expect(await screen.findByText('按需取回的正文')).toBeInTheDocument()
  })

  it('请求期间给读取提示，回来后渲染正文与目录', async () => {
    let resolve: (v: typeof snapshot) => void = () => {}
    const loadBodySource = vi.fn(() => new Promise<typeof snapshot>((r) => { resolve = r }))
    const { container } = render(<TestDetailPane {...props({ l: lazyLink(), loadBodySource })} />)

    expandOriginal(container)
    expect(screen.getByText('读取原文中…')).toBeInTheDocument()

    await act(async () => {
      resolve(snapshot)
    })
    expect(screen.getByText('按需取回的正文')).toBeInTheDocument()
    expect(screen.queryByText('读取原文中…')).not.toBeInTheDocument()
  })

  it('revision 变化后忽略旧 getContent 的迟到响应并保留新正文', async () => {
    const oldBody = {
      link_id: 'LLAZY',
      content: '迟到的第七代正文',
      content_format: 'plain' as const,
      fetcher_type: 'stored',
      content_revision: 7,
    }
    const newBody = {
      link_id: 'LLAZY',
      content: '先完成的第八代正文',
      content_format: 'plain' as const,
      fetcher_type: 'stored',
      content_revision: 8,
    }
    let resolveOld!: (value: typeof oldBody) => void
    const oldRequest = new Promise<typeof oldBody>((resolve) => { resolveOld = resolve })
    const loadBodySource = vi.fn((context: DocumentCommandContext) =>
      context.id.contentRevision === 7 ? oldRequest : Promise.resolve(newBody),
    )
    const { container, rerender } = render(
      <TestDetailPane {...props({ l: lazyLink(), loadBodySource })} />,
    )

    expandOriginal(container)
    await waitFor(() => expect(loadBodySource).toHaveBeenCalledWith(expect.objectContaining({
      id: expect.objectContaining({ linkId: 'LLAZY', contentRevision: 7 }),
    })))
    rerender(
      <TestDetailPane
        {...props({
          l: { ...lazyLink(), content_revision: 8 },
          loadBodySource,
        })}
      />,
    )

    expect(await screen.findByText('先完成的第八代正文')).toBeInTheDocument()
    await act(async () => {
      resolveOld(oldBody)
    })
    expect(screen.getByText('先完成的第八代正文')).toBeInTheDocument()
    expect(screen.queryByText('迟到的第七代正文')).not.toBeInTheDocument()
    expect(loadBodySource).toHaveBeenCalledTimes(2)
  })

  it('读取失败给内联错误，可重试', async () => {
    const loadBodySource = vi.fn(async () => null)
    const { container } = render(<TestDetailPane {...props({ l: lazyLink(), loadBodySource })} />)

    expandOriginal(container)
    expect(await screen.findByText('原文读取失败')).toBeInTheDocument()

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: '重试' }))
    })
    expect(loadBodySource).toHaveBeenCalledTimes(2)
  })

  it('收起再展开不重复请求（同一篇缓存住）', async () => {
    const loadBodySource = vi.fn(async () => snapshot)
    const { container } = render(<TestDetailPane {...props({ l: lazyLink(), loadBodySource })} />)

    expandOriginal(container)
    expect(await screen.findByText('按需取回的正文')).toBeInTheDocument()
    expandOriginal(container)
    expandOriginal(container)

    expect(loadBodySource).toHaveBeenCalledTimes(1)
  })

  // 用户报的那条：展开 → 折叠 → 切到别的页面 → 回来再展开，又请求一遍。
  // loadedContent 是组件内 state，DetailPane 一卸载就没了；接得上全靠组件对缓存
  // 键的订阅（useCachedLinkContent）。这里不注入假的读缓存回调——组件直接读真实
  // 的 resourceStore，键算歪了（比如 revision 没传下去）这条就会红。
  it('卸载重挂后再展开，直接用缓存，不再请求', async () => {
    // resourceStore 的跨用例清理由 test/setup.ts 的 afterEach 统一负责。
    const getContent = vi.fn(async () => ok(snapshot))
    const loadBodySource = async (context: DocumentCommandContext) => {
      const res = await resourceStore.fetch<typeof snapshot>(
        contentCacheKey(context.id.linkId, context.id.contentRevision),
        getContent,
      )
      return res.ok ? res.data : null
    }

    const first = render(<TestDetailPane {...props({ l: lazyLink(), loadBodySource })} />)
    expandOriginal(first.container)
    expect(await screen.findByText('按需取回的正文')).toBeInTheDocument()
    expect(getContent).toHaveBeenCalledTimes(1)
    // 组件订阅的键，必须就是 (l.id, l.content_revision) 那一个。
    expect(resourceStore.has(contentCacheKey('LLAZY', 7))).toBe(true)

    // 折叠，然后「切到订阅页」——整个 DetailPane 卸载。
    expandOriginal(first.container)
    first.unmount()

    // 切回来再展开。
    const second = render(<TestDetailPane {...props({ l: lazyLink(), loadBodySource })} />)
    expandOriginal(second.container)

    // 关键断言：正文**同步**就在（没有 loading 一闪），也没有第二次网络请求。
    expect(screen.getByText('按需取回的正文')).toBeInTheDocument()
    expect(screen.queryByText('读取原文中…')).not.toBeInTheDocument()
    expect(getContent).toHaveBeenCalledTimes(1)
  })

  // 订阅还有一层作用：store 的 LRU 淘汰豁免「有订阅者」的键。只 peek 不订阅的话，
  // 正文（最大最多的那批条目）会在原文正展开着的时候被淘汰，界面当场塌成完全
  // 空白——没有正文、没有加载态、也没有错误块。
  //
  // 时序必须是「缓存供给」而不是「loadBodySource 拉回来」：后者会把正文同时留在
  // 组件内的 loadedContent 里，淘汰再狠也塌不了，UI 断言就成了假证人。同理，
  // 灌完填充键之后要再渲染一次——没有订阅就没有通知，不重渲染看不出已经空了。
  it('展开着的原文不会被 LRU 淘汰掉（订阅换来的豁免）', () => {
    const key = contentCacheKey('LLAZY', 7)
    resourceStore.set(key, snapshot)
    const loadBodySource = vi.fn(async () => null)

    const { container, rerender } = render(<TestDetailPane {...props({ l: lazyLink(), loadBodySource })} />)
    expandOriginal(container)
    expect(screen.getByText('按需取回的正文')).toBeInTheDocument()
    // 正文只可能来自缓存这一条路——组件内没有任何备份。
    expect(loadBodySource).not.toHaveBeenCalled()

    // 灌满缓存，逼 store 走一轮淘汰。
    for (let i = 0; i < DEFAULT_CAPACITY + 20; i += 1) {
      resourceStore.set(`GET /api/filler/${i}`, { i })
    }
    // 列表 30 秒静默校验换掉 l 的引用，是这个界面上最常见的重渲染来源。
    rerender(<TestDetailPane {...props({ l: lazyLink(), loadBodySource })} />)

    expect(resourceStore.has(key)).toBe(true)
    expect(screen.getByText('按需取回的正文')).toBeInTheDocument()
  })

  // has_content 闸门的另外两支。上面那条 LPURGE 只走 cachedContent，而
  // l.content 与 loadedContent 各自都能独立绕过闸门——三支得各有各的守卫。
  //
  // status 必须保持 done：pending 会让整个原文区被「解析中」态盖掉，闸门根本
  // 不参与求值，用例会变成恒绿。
  it('本地还攥着 content、但列表已回报 has_content:false 时不渲染', () => {
    // 重新解析会把服务端的 content 置空，而列表投影本来就不含 content，
    // MainView 的合并 effect 于是会无限期保住本地那份。
    const saved = makeLink({
      id: 'LKEEP',
      status: 'done',
      summary: '摘要',
      content: '保存过的正文',
      content_format: 'plain',
    })
    const { container, rerender } = render(<TestDetailPane {...props({ l: saved })} />)
    expandOriginal(container)
    expect(screen.getByText('保存过的正文')).toBeInTheDocument()

    rerender(<TestDetailPane {...props({ l: { ...saved, has_content: false } })} />)

    expect(screen.queryByText('保存过的正文')).not.toBeInTheDocument()
    expect(container.querySelector('[aria-controls="orig-content-body"]')).toBeNull()
    expect(screen.getByText('保存原文')).toBeInTheDocument()
  })

  it('展开后拉到的正文，也随 has_content 翻假一起收起', async () => {
    const loadBodySource = vi.fn(async () => snapshot)
    const { container, rerender } = render(<TestDetailPane {...props({ l: lazyLink(), loadBodySource })} />)
    expandOriginal(container)
    expect(await screen.findByText('按需取回的正文')).toBeInTheDocument()

    rerender(<TestDetailPane {...props({ l: { ...lazyLink(), has_content: false }, loadBodySource })} />)

    expect(screen.queryByText('按需取回的正文')).not.toBeInTheDocument()
    expect(screen.getByText('保存原文')).toBeInTheDocument()
  })

  // RF5A 之后后端 clear 与 generation 在同一事务推进，但 Reader 的 link projection、
  // loaded state 与 revision-keyed cache 尚未由一个原子 document reducer 持有。只要
  // 当前权威投影已回报 has_content:false，任何残留缓存都必须立即被存在性闸门压住。
  it('列表回报 has_content:false 时，不拿缓存里的旧正文顶上', () => {
    const purged = makeLink({ id: 'LPURGE', status: 'done', summary: '摘要', has_content: false, content_revision: 7 })
    // 模拟升级前遗留或非原子响应窗口中仍可见的旧缓存。
    resourceStore.set(contentCacheKey('LPURGE', 7), {
      ...snapshot,
      link_id: 'LPURGE',
      content: '服务端已经删掉的旧正文',
    })
    const { container } = render(<TestDetailPane {...props({ l: purged })} />)

    expect(container.querySelector('[aria-controls="orig-content-body"]')).toBeNull()
    expect(screen.queryByText('服务端已经删掉的旧正文')).not.toBeInTheDocument()
    expect(screen.getByText('保存原文')).toBeInTheDocument()
  })

  // 没有已保存原文的链接，折叠头显示的是**摘要**的时长。这一支此前无守卫——
  // 把它写死成任意常数，394 条测试一条都不会红。
  it('没有原文的链接，按摘要估阅读时长（走同一条公式）', () => {
    // 400 个汉字 = 1 分钟；1200 个 → 3 分钟。
    const noContent = makeLink({ id: 'LSUM', status: 'done', summary: '字'.repeat(1200) })
    render(<TestDetailPane {...props({ l: noContent })} />)

    expect(screen.getByText(/约\s*3\s*分钟/)).toBeInTheDocument()
  })

  it('阅读时长在展开前后是同一个数字（中英混排也不跳）', async () => {
    // 故意用英文为主的正文：若折叠态按「字符数 / 400」估，会算出和展开后
    // 差好几倍的分钟数——这正是上一版的毛病。
    const body = 'word '.repeat(500) + '中文'.repeat(100)
    const { container } = render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'LMIX',
            status: 'done',
            summary: '短摘要',
            has_content: true,
            content_cjk_chars: 200,
            content_words: 500,
          }),
          loadBodySource: vi.fn(async () => ({
            link_id: 'LMIX',
            content: body,
            content_format: 'plain' as const,
            fetcher_type: 'stored',
            content_revision: 1,
          })),
        })}
      />,
    )

    const minutes = () => container.querySelector('.art-source')?.textContent?.match(/约 (\d+) 分钟/)?.[1]
    const collapsed = minutes()
    expect(collapsed).toBe('3') // ceil(200/400 + 500/220)

    expandOriginal(container)
    expect(await screen.findByText(/word/)).toBeInTheDocument()
    expect(minutes()).toBe(collapsed)
  })

  // 上一条测的是「展开前后一致」，它有个结构性弱点：把展开态也改成吃后端计数，
  // 两边天然相等、断言恒真。这一条反过来——让后端计数与正文实数**刻意不一致**，
  // 于是折叠态必须用后端计数、展开态必须现数正文，两个数字必须不同。
  //
  // 顺带钉死实参顺序：对调之后折叠态是 ceil(0/400 + 400/220) = 2，不再是 1。
  it('折叠态用后端计数、展开态现数正文（两者刻意不一致）', async () => {
    const body = '字'.repeat(4000) // 现数 = ceil(4000/400) = 10 分钟
    const { container } = render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'LDIFF',
            status: 'done',
            summary: '短摘要',
            has_content: true,
            content_revision: 5,
            content_cjk_chars: 400, // 后端计数 = ceil(400/400) = 1 分钟
            content_words: 0,
          }),
          loadBodySource: vi.fn(async () => ({
            link_id: 'LDIFF',
            content: body,
            content_format: 'plain' as const,
            fetcher_type: 'stored',
            content_revision: 5,
          })),
        })}
      />,
    )

    const minutes = () => container.querySelector('.art-source')?.textContent?.match(/约 (\d+) 分钟/)?.[1]
    expect(minutes()).toBe('1')

    expandOriginal(container)
    expect(await screen.findByText(body)).toBeInTheDocument()
    expect(minutes()).toBe('10')
  })

  it('has_content=false 时既没有折叠区也不会请求', () => {
    const loadBodySource = vi.fn(async () => snapshot)
    const { container } = render(
      <TestDetailPane {...props({ l: makeLink({ id: 'LNO', status: 'done', summary: '摘要' }), loadBodySource })} />,
    )

    expect(container.querySelector('[aria-controls="orig-content-body"]')).toBeNull()
    expect(screen.getByText('保存原文')).toBeInTheDocument()
    expect(loadBodySource).not.toHaveBeenCalled()
  })

  it('详情里已带正文（刚保存/刚重抓）时展开不再请求', () => {
    const loadBodySource = vi.fn(async () => snapshot)
    const { container } = render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'LINLINE',
            status: 'done',
            has_content: true,
            content: '刚保存的正文',
            content_format: 'plain',
            content_revision: 1,
          }),
          loadBodySource,
        })}
      />,
    )

    expandOriginal(container)

    expect(screen.getByText('刚保存的正文')).toBeInTheDocument()
    expect(loadBodySource).not.toHaveBeenCalled()
  })
})

describe('DetailPane 解析状态不进入阅读主线', () => {
  it('processing 不显示解析状态提示', () => {
    render(<TestDetailPane {...props({ l: makeLink({ status: 'processing' }) })} />)
    expect(screen.queryByText(/正在抓取页面并生成 AI 摘要/)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重新解析' })).not.toBeInTheDocument()
  })

  it('failed 不显示错误分类或重新解析入口', () => {
    render(
      <TestDetailPane
        {...props({
          l: makeLink({
            status: 'failed',
            error_category: 'upstream_http',
            error_msg: '502',
          }),
        })}
      />,
    )
    expect(screen.queryByText(/解析失败（upstream_http）/)).not.toBeInTheDocument()
    expect(screen.queryByText('502')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重新解析' })).not.toBeInTheDocument()
  })

  it('low confidence 不显示提示或重新解析入口', () => {
    render(
      <TestDetailPane
        {...props({
          l: makeLink({
            is_low_confidence: true,
            low_confidence_reason: 'thin_content',
            summary: '摘要',
          }),
        })}
      />,
    )
    expect(screen.queryByText(/低置信度结果/)).not.toBeInTheDocument()
    expect(screen.queryByText(/thin_content/)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重新解析' })).not.toBeInTheDocument()
  })
})

describe('DetailPane 原文操作', () => {
  it('done 且未保存原文：显示保存按钮并触发 onSaveContent', () => {
    const onSaveContent = vi.fn()
    render(
      <TestDetailPane
        {...props({
          l: makeLink({ id: 'L3', status: 'done', summary: '摘要' }),
          onSaveContent,
        })}
      />,
    )
    fireEvent.click(screen.getByText('保存原文'))
    expect(onSaveContent).toHaveBeenCalledWith('L3')
  })

  it('已保存原文：显示原文、不显示保存按钮', () => {
    render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'L4',
            status: 'done',
            summary: '摘要',
            content: '这是网页原文正文',
            content_format: 'plain',
            content_revision: 1,
          }),
        })}
      />,
    )
    // 折叠态下原文不渲染，展开后才出现——这条用例顺带钉住默认折叠。
    expect(screen.queryByText('这是网页原文正文')).not.toBeInTheDocument()
    expandOriginal()
    expect(screen.getByText('这是网页原文正文')).toBeInTheDocument()
    expect(screen.queryByText('保存原文')).not.toBeInTheDocument()
  })

  it('结构化原文默认渲染标题、列表、链接和代码块', () => {
    const { container } = render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'L5',
            content: '章节\n\n第一段\n\n条目\n\nconst n = 1\n\n文档',
            content_document:
              '## 章节\n\n第一段\n\n- 条目\n\n```ts\nconst n = 1\n```\n\n[文档](https://example.com/docs)',
            content_format: 'markdown',
          }),
        })}
      />,
    )
    expandOriginal(container)
    const original = container.querySelector('.orig-content')
    expect(original?.querySelector('h2')?.textContent).toBe('章节')
    expect(original?.querySelector('li')?.textContent).toBe('条目')
    expect(original?.querySelector('pre code')?.textContent).toContain('const n = 1')
    expect(original?.querySelector('a')).toHaveAttribute('href', 'https://example.com/docs')
    expect(original).toHaveAttribute('data-hl-block', 'content-document')
  })

  it('可切换为规范纯文本，历史 content 划线也默认走纯文本坐标', () => {
    const link = makeLink({
      id: 'L6',
      content: '纯文本投影',
      content_document: '## 排版文档',
      content_format: 'markdown',
    })
    const { container, rerender } = render(<TestDetailPane {...props({ l: link })} />)
    expandOriginal(container)
    fireEvent.click(screen.getByText('文本'))
    expect(screen.getByText('纯文本投影')).toBeInTheDocument()
    expect(container.querySelector('.orig-content')).toHaveAttribute('data-hl-block', 'content')

    rerender(
      <TestDetailPane
        {...props({
          l: link,
          anns: [
            {
              id: 'old-content-ann',
              blockKey: 'content',
              start: 0,
              end: 2,
              text: '纯文',
              note: '',
              source: 'self',
              createdAt: 1,
              updatedAt: 1,
            },
          ],
        })}
      />,
    )
    expect(container.querySelector('.orig-content')).toHaveAttribute('data-hl-block', 'content')
  })

  it('危险 Markdown 不创建 script 或 javascript 链接', () => {
    const { container } = render(
      <TestDetailPane
        {...props({
          l: makeLink({
            content: '安全正文',
            content_document:
              '<script>alert(1)</script>\n\n[危险](javascript:alert(2))\n\n安全正文',
            content_format: 'markdown',
          }),
        })}
      />,
    )
    expect(container.querySelector('.orig-content script')).toBeNull()
    const unsafeLink = screen.queryByText('危险')?.closest('a')
    expect(unsafeLink?.getAttribute('href') ?? '').not.toMatch(/^javascript:/i)
  })

  it('已保存原文可显式触发重新抓取', () => {
    const onReplaceContent = vi.fn()
    render(
      <TestDetailPane
        {...props({
          l: makeLink({ id: 'L7', content: '旧正文', content_format: 'plain' }),
          onReplaceContent,
        })}
      />,
    )
    expandOriginal()
    fireEvent.click(screen.getByText('重新抓取'))
    expect(onReplaceContent).toHaveBeenCalledWith('L7')
  })

  it('已保存原文可发起全文翻译', () => {
    const onTranslateFull = vi.fn()
    render(
      <TestDetailPane
        {...props({
          l: makeLink({ id: 'L8', content: 'English body', content_format: 'plain' }),
          onTranslateFull,
        })}
      />,
    )
    expandOriginal()

    fireEvent.click(screen.getByRole('button', { name: '翻译全文' }))

    expect(onTranslateFull).toHaveBeenCalledWith(false)
  })

  it('全文翻译完成后可在原文与中文译文间切换', () => {
    const full = translation({
      id: 'TF',
      scope: 'full',
      block_key: 'content-document',
      source_text: '# Heading\n\nEnglish body',
      translated_text: '# 中文标题\n\n中文正文',
      source_format: 'markdown',
    })
    const { container } = render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'L1',
            content: 'Heading\n\nEnglish body',
            content_document: '# Heading\n\nEnglish body',
            content_format: 'markdown',
          }),
          translations: [full],
        })}
      />,
    )
    expandOriginal(container)

    fireEvent.click(screen.getByRole('button', { name: '中文译文' }))

    expect(screen.getByRole('heading', { name: '中文标题' })).toBeInTheDocument()
    expect(screen.getByText('中文正文')).toBeInTheDocument()
    expect(container.querySelector('.orig-content')).toHaveAttribute(
      'data-hl-block',
      'content-translation',
    )
  })

  it('全文译文变为 stale 时立即退回原文并要求更新', async () => {
    const link = makeLink({ id: 'L1', content: 'New source', content_format: 'plain' })
    const full = translation({
      id: 'TF',
      scope: 'full',
      block_key: 'content',
      source_text: 'Old source',
      translated_text: '旧中文译文',
      source_format: 'plain',
    })
    const { container, rerender } = render(
      <TestDetailPane {...props({ l: link, translations: [full] })} />,
    )
    expandOriginal()
    fireEvent.click(screen.getByRole('button', { name: '中文译文' }))
    expect(screen.getByText('旧中文译文')).toBeInTheDocument()

    rerender(
      <TestDetailPane
        {...props({
          l: link,
          translations: [],
          staleTranslations: [{ ...full, stale: true }],
        })}
      />,
    )

    await waitFor(() =>
      expect(container.querySelector('.orig-content')).toHaveTextContent('New source'),
    )
    expect(container.querySelector('.orig-content')).not.toHaveTextContent('旧中文译文')
    expect(screen.getByRole('button', { name: '更新全文翻译' })).toBeInTheDocument()
  })

  it('全文翻译进行中禁用按钮，失败后可重试', () => {
    const onTranslateFull = vi.fn()
    const link = makeLink({ id: 'L1', content: 'English body', content_format: 'plain' })
    const { rerender } = render(
      <TestDetailPane
        {...props({
          l: link,
          translations: [translation({ scope: 'full', status: 'processing', translated_text: null })],
          onTranslateFull,
        })}
      />,
    )
    expandOriginal()
    expect(screen.getByRole('button', { name: '全文翻译中…' })).toBeDisabled()

    rerender(
      <TestDetailPane
        {...props({
          l: link,
          translations: [
            translation({
              scope: 'full',
              status: 'failed',
              translated_text: null,
              error_msg: '翻译服务暂时不可用，请重试',
            }),
          ],
          onTranslateFull,
        })}
      />,
    )
    // 同一篇文章内 rerender 不会收起原文，这里不用再展开一次。
    fireEvent.click(screen.getByRole('button', { name: '重试全文翻译' }))
    expect(onTranslateFull).toHaveBeenCalledWith(true)
  })

  it('显示从数据库恢复的选段译文', () => {
    render(
      <TestDetailPane
        {...props({
          translations: [translation({ source_text: 'hello world', translated_text: '你好，世界' })],
        })}
      />,
    )

    expect(screen.getByText(/选段译文/)).toBeInTheDocument()
    expect(screen.getByText('hello world')).toBeInTheDocument()
    expect(screen.getByText('你好，世界')).toBeInTheDocument()
  })

  it('把过期译文放在独立历史区域且不当作当前译文', () => {
    const stale = translation({
      id: 'stale',
      scope: 'full',
      block_key: 'content',
      source_text: 'Old saved body',
      translated_text: '旧正文译文',
      source_content_revision: 7,
      stale: true,
    })
		render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'L1',
            content: 'Current saved body',
            content_format: 'plain',
            content_revision: 8,
				}),
				staleTranslations: [stale],
			})}
      />,
    )

		expect(screen.getByText('已过期译文')).toBeInTheDocument()
		expect(screen.getByText('旧正文译文')).toBeInTheDocument()
    expandOriginal()
    expect(screen.queryByRole('button', { name: '中文译文' })).not.toBeInTheDocument()
  })

})

describe('DetailPane 选段翻译', () => {
  it('选中文字后把块锚点和原文交给翻译回调', async () => {
    const onTranslateSelection = vi.fn(async () => 'T1')
    render(
      <TestDetailPane
        {...props({
          l: makeLink({ id: 'L1', summary: 'Translate this sentence' }),
          onTranslateSelection,
        })}
      />,
    )
    const paragraph = screen.getByText('Translate this sentence')
    const range = document.createRange()
    range.selectNodeContents(paragraph)
    Object.defineProperty(range, 'getBoundingClientRect', {
      value: () => new DOMRect(20, 20, 160, 24),
    })
    const selection = window.getSelection()
    selection?.removeAllRanges()
    selection?.addRange(range)
    fireEvent(document, new Event('selectionchange'))

    fireEvent.click(await screen.findByRole('button', { name: '翻译' }))

    await waitFor(() =>
      expect(onTranslateSelection).toHaveBeenCalledWith(
        expect.objectContaining({
          blockKey: 'summary',
          start: 0,
          end: 23,
          text: 'Translate this sentence',
        }),
        false,
      ),
    )
  })

  it('任务完成后在选区附近显示中文结果并可关闭', async () => {
    const onTranslateSelection = vi.fn(async () => 'T1')
    const baseProps = props({
      l: makeLink({ id: 'L1', summary: 'Translate this sentence' }),
      onTranslateSelection,
    })
    const { rerender } = render(<TestDetailPane {...baseProps} />)
    const paragraph = screen.getByText('Translate this sentence')
    const range = document.createRange()
    range.selectNodeContents(paragraph)
    Object.defineProperty(range, 'getBoundingClientRect', {
      value: () => new DOMRect(20, 20, 160, 24),
    })
    window.getSelection()?.removeAllRanges()
    window.getSelection()?.addRange(range)
    fireEvent(document, new Event('selectionchange'))
    fireEvent.click(await screen.findByRole('button', { name: '翻译' }))
    await waitFor(() => expect(onTranslateSelection).toHaveBeenCalled())

    rerender(
      <TestDetailPane
        {...baseProps}
        translations={[translation({ id: 'T1', translated_text: '翻译这个句子' })]}
      />,
    )

    expect(await screen.findAllByText('翻译这个句子')).toHaveLength(2)
    fireEvent.click(screen.getByRole('button', { name: '关闭翻译' }))
    expect(screen.queryByText('中文翻译')).not.toBeInTheDocument()
  })
})

describe('DetailPane 划线持久化', () => {
  it('同 ID 的 summary 与 content mark 打开各自 target identity', async () => {
    const onOpenNote = vi.fn()
    const sharedId = 'same-id-different-target'
    const annotations: Annotation[] = [
      {
        id: sharedId,
        blockKey: 'summary',
        start: 0,
        end: 4,
        text: 'Same',
        note: 'summary note',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
        sourceSummaryHash: 'a'.repeat(64),
      },
      {
        id: sharedId,
        blockKey: 'content',
        start: 0,
        end: 4,
        text: 'Same',
        note: 'content note',
        source: 'self',
        createdAt: 2,
        updatedAt: 2,
        sourceContentRevision: 7,
      },
    ]
    render(
      <TestDetailPane
        {...props({
          l: makeLink({
            id: 'L1',
            status: 'done',
            summary: 'Same summary',
            content: 'Same content',
            content_format: 'plain',
            content_revision: 7,
            has_content: true,
          }),
          anns: annotations,
          onOpenNote,
        })}
      />,
    )

    const summaryMark = await waitFor(() => {
      const mark = document.querySelector<HTMLElement>(
        `[data-hl-block="summary"] mark[data-ann="${sharedId}"]`,
      )
      if (!mark) throw new Error('summary mark is not rendered')
      return mark
    })
    fireEvent.click(summaryMark)
    expect(onOpenNote).toHaveBeenLastCalledWith(expect.objectContaining({
      id: sharedId,
      blockKey: 'summary',
      target: { kind: 'summary', sourceHash: 'a'.repeat(64) },
    }))

    expandOriginal()
    const contentMark = await waitFor(() => {
      const mark = document.querySelector<HTMLElement>(
        `[data-hl-block="content"] mark[data-ann="${sharedId}"]`,
      )
      if (!mark) throw new Error('content mark is not rendered')
      return mark
    })
    fireEvent.click(contentMark)
    expect(onOpenNote).toHaveBeenLastCalledWith(expect.objectContaining({
      id: sharedId,
      blockKey: 'content',
      target: { kind: 'saved-content', contentRevision: 7 },
    }))
  })

  it('durable add 失败时只显示失败提示，不打开笔记或显示成功 toast', async () => {
    const onAddAnn = vi.fn(async () => null)
    const onOpenNote = vi.fn()
    const onToast = vi.fn()
    render(
      <TestDetailPane
        {...props({
          l: makeLink({ id: 'L1', summary: 'Durable add selection' }),
          onAddAnn,
          onOpenNote,
          onToast,
        })}
      />,
    )
    const paragraph = screen.getByText('Durable add selection')
    const range = document.createRange()
    range.selectNodeContents(paragraph)
    Object.defineProperty(range, 'getBoundingClientRect', {
      value: () => new DOMRect(20, 20, 160, 24),
    })
    window.getSelection()?.removeAllRanges()
    window.getSelection()?.addRange(range)
    fireEvent(document, new Event('selectionchange'))
    const highlight = await screen.findByRole('button', { name: '划线' })

    await act(async () => {
      fireEvent.click(highlight)
      await new Promise((resolve) => setTimeout(resolve, 0))
    })

    expect(onAddAnn).toHaveBeenCalledWith(
      expect.objectContaining({
        blockKey: 'summary',
        start: 0,
        end: 21,
        text: 'Durable add selection',
      }),
      null,
    )
    expect(onToast).toHaveBeenCalledWith('内容来源已更新，请重新选择', 'alert')
    expect(onToast).not.toHaveBeenCalledWith('已划线', 'marker')
    expect(onOpenNote).not.toHaveBeenCalled()
  })

  it('首次 durable add 未结算时共享锁会拦住划线双击与其它持久化动作', async () => {
    type AddResult = Awaited<ReturnType<DetailPaneProps['onAddAnn']>>
    const pending: Array<(value: AddResult) => void> = []
    const onAddAnn = vi.fn(
      () => new Promise<AddResult>((resolve) => pending.push(resolve)),
    )
    const onOpenNote = vi.fn()
    const onAskAI = vi.fn()
    const onToast = vi.fn()
    const locator = {
      id: 'ann-pending',
      blockKey: 'summary' as const,
      target: { kind: 'summary' as const, sourceHash: 'a'.repeat(64) },
    }
    render(
      <TestDetailPane
        {...props({
          l: makeLink({ id: 'L1', summary: 'Pending durable selection' }),
          onAddAnn,
          onOpenNote,
          onAskAI,
          onToast,
        })}
      />,
    )
    const paragraph = screen.getByText('Pending durable selection')
    const selectParagraph = () => {
      const range = document.createRange()
      range.selectNodeContents(paragraph)
      Object.defineProperty(range, 'getBoundingClientRect', {
        value: () => new DOMRect(20, 20, 160, 24),
      })
      window.getSelection()?.removeAllRanges()
      window.getSelection()?.addRange(range)
      fireEvent(document, new Event('selectionchange'))
    }
    selectParagraph()

    const highlight = await screen.findByRole('button', { name: '划线' })
    act(() => {
      highlight.click()
      highlight.click()
    })
    expect(highlight).toBeDisabled()
    expect(screen.getByRole('button', { name: '写想法' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '问 AI' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '翻译' })).not.toBeDisabled()
    expect(screen.getByRole('button', { name: '复制' })).not.toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: '写想法' }))
    fireEvent.click(screen.getByRole('button', { name: '问 AI' }))

    expect(onAddAnn).toHaveBeenCalledTimes(1)
    expect(onOpenNote).not.toHaveBeenCalled()
    expect(onAskAI).not.toHaveBeenCalled()

    await act(async () => {
      pending[0]?.(locator)
    })
    expect(onToast).toHaveBeenCalledWith('已划线', 'marker')

    selectParagraph()
    fireEvent.click(await screen.findByRole('button', { name: '写想法' }))
    expect(onAddAnn).toHaveBeenCalledTimes(2)

    await act(async () => {
      pending[1]?.(locator)
    })
    expect(onOpenNote).toHaveBeenCalledWith(locator)
  })
})

describe('DetailPane 标签筛选', () => {
  it('点击标签 chip 触发 onPickTag', () => {
    const onPickTag = vi.fn()
    const { container } = render(<TestDetailPane {...props({ l: makeLink({ tags: ['LLM'] }), onPickTag })} />)
    fireEvent.click(container.querySelector('.art-tags button') as HTMLElement)
    expect(onPickTag).toHaveBeenCalledWith('LLM')
  })

  it('优先消费 server related-tags，而不是只用本地语料近似', async () => {
    const lease = new IdentityLease({
      serverClientDataNamespace: 'detail-related-server',
      physicalNamespace: 'detail-related-physical',
      localEpoch: 1,
    })
    resourceStore.activateIdentity(lease)
    const getRelatedTags = vi.fn(async () => ok({
      items: ['server-related'],
    }))
    const readerClient = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
      getRelatedTags,
    } as unknown as IdentityBoundReaderClient

    render(
      <TestDetailPane
        {...props({
          readerClient,
          corpus: [makeLink({ id: 'other', tags: ['local-related'] })],
        })}
      />,
    )

    await waitFor(() => {
      expect(screen.getAllByRole('button', { name: '≈ server-related' })).toHaveLength(2)
    })
    expect(screen.queryByRole('button', { name: '≈ local-related' })).not.toBeInTheDocument()
    expect(getRelatedTags).toHaveBeenCalledWith(expect.any(String), 12)
    resourceStore.deactivateIdentity(lease)
  })

  it('does not request or render related tags when the capability is unavailable', () => {
    const lease = new IdentityLease({
      serverClientDataNamespace: 'detail-related-disabled-server',
      physicalNamespace: 'detail-related-disabled-physical',
      localEpoch: 1,
    })
    resourceStore.activateIdentity(lease)
    const getRelatedTags = vi.fn(async () => ok({
      items: ['server-related'],
    }))
    const readerClient = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
      getRelatedTags,
    } as unknown as IdentityBoundReaderClient

    render(
      <TestDetailPane
        {...props({
          l: makeLink({ id: 'L-related-tags-off', tags: ['LLM'] }),
          readerClient,
          relatedTagsEnabled: false,
          corpus: [makeLink({ id: 'other', tags: ['LLM', 'local-related'] })],
        })}
      />,
    )

    expect(getRelatedTags).not.toHaveBeenCalled()
    expect(screen.queryByRole('button', { name: '≈ server-related' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '≈ local-related' })).not.toBeInTheDocument()
    resourceStore.deactivateIdentity(lease)
  })
})
