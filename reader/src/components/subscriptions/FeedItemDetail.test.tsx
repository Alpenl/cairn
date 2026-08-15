import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { FeedItemDetail } from './FeedItemDetail'

function setSelectionRange(textNode: Node): void {
  const range = document.createRange()
  range.setStart(textNode, 0)
  range.setEnd(textNode, textNode.textContent?.length ?? 0)
  Object.defineProperty(range, 'getBoundingClientRect', {
    configurable: true,
    value: () => ({
      left: 80,
      top: 120,
      right: 220,
      bottom: 140,
      width: 140,
      height: 20,
    } as DOMRect),
  })
  const browserSelection = window.getSelection()!
  browserSelection.removeAllRanges()
  browserSelection.addRange(range)
  act(() => {
    document.dispatchEvent(new Event('selectionchange'))
  })
}

describe('FeedItemDetail', () => {
  it('returns to the top when switching to another feed item', () => {
    const props = {
      loading: false,
      analyzing: false,
      onBack: vi.fn(),
      onToggleRead: vi.fn(),
      onToggleStar: vi.fn(),
      onToggleLater: vi.fn(),
      onAnalyze: vi.fn(),
      onViewAnalysis: vi.fn(),
    }
    const firstItem = {
      id: 'item-first',
      subscription_id: 'subscription-1',
      title: 'First article',
      url: 'https://example.com/first',
      content: 'First article body.',
    }
    const { container, rerender } = render(<FeedItemDetail {...props} item={firstItem} />)
    const readerScroll = container.querySelector('.reader-scroll') as HTMLDivElement
    readerScroll.scrollTop = 480

    rerender(
      <FeedItemDetail
        {...props}
        item={{
          ...firstItem,
          id: 'item-second',
          title: 'Second article',
          url: 'https://example.com/second',
          content: 'Second article body.',
        }}
      />,
    )

    expect(readerScroll.scrollTop).toBe(0)
  })

  it('keeps URL-less feed content readable but disables AI analysis', () => {
    const onAnalyze = vi.fn()
    const { container } = render(
      <FeedItemDetail
        item={{
          id: 'item-1',
          subscription_id: 'subscription-1',
          subscription_title: 'Text-only feed',
          title: 'An entry without a permalink',
          url: '',
          content: 'The publisher supplied this body directly in the feed.',
        }}
        loading={false}
        analyzing={false}
        onBack={vi.fn()}
        onToggleRead={vi.fn()}
        onToggleStar={vi.fn()}
        onToggleLater={vi.fn()}
        onAnalyze={onAnalyze}
        onViewAnalysis={vi.fn()}
      />,
    )

    expect(screen.getByText('Text-only feed')).toBeInTheDocument()
    expect(screen.getByText(/publisher supplied this body/)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '打开原文' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '无法分析' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '无法分析' })).toHaveAttribute(
      'title',
      '订阅源未提供可用的原文地址',
    )
    expect(onAnalyze).not.toHaveBeenCalled()

    const content = container.querySelector('[data-reading-source="subscription-content"]')!
    setSelectionRange(content.firstChild!)
    expect(screen.getByRole('button', { name: '无法转入阅读' })).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: '无法转入阅读' }))
    expect(onAnalyze).not.toHaveBeenCalled()
  })

  it('uses the full reading canvas for metadata and sanitized content', () => {
    const { container } = render(
      <FeedItemDetail
        item={{
          id: 'item-layout',
          subscription_id: 'subscription-1',
          subscription_title: 'Layout feed',
          title: 'A responsive feed article',
          url: 'https://example.com/posts/layout',
          summary: 'A compact summary.',
          content_html:
            '<p>Readable paragraph</p><p><img src="https://example.com/chart.png" alt="Chart"></p>',
        }}
        loading={false}
        analyzing={false}
        onBack={vi.fn()}
        onToggleRead={vi.fn()}
        onToggleStar={vi.fn()}
        onToggleLater={vi.fn()}
        onAnalyze={vi.fn()}
        onViewAnalysis={vi.fn()}
      />,
    )

    expect(screen.getByRole('heading', { level: 1, name: 'A responsive feed article' })).toHaveClass(
      'reader-prose',
    )
    expect(container.querySelector('.rss-article-source')).toHaveClass('reader-prose')
    expect(container.querySelector('.rss-article-meta')).toHaveClass('reader-prose')
    expect(container.querySelector('.rss-feed-content')).toHaveClass('reader-flow')
    expect(container.querySelector('.rss-article-footer')).toHaveClass('reader-prose')
    expect(container.querySelector('.rss-detail-pane')).toHaveAttribute('data-reading-source-kind', 'html')
    expect(container.querySelector('.rss-detail-pane')).toHaveAttribute('data-reading-source-block', 'content')

    expect(container.querySelector('.rss-feed-content img')).toBeNull()
    expect(screen.getByRole('img', { name: 'Chart' })).toHaveTextContent('Chart')
    expect(container.querySelector('.rss-feed-content')?.innerHTML).not.toContain('example.com/chart.png')
    expect(container.querySelector('.rss-feed-content script')).not.toBeInTheDocument()
  })

  // 订阅正文与阅读详情共用同一套目录组件；差别只在标题来源（清洗后的 HTML）。
  it('订阅正文也出目录，条目按层级缩进且能跳转', () => {
    const props = {
      loading: false,
      analyzing: false,
      onBack: vi.fn(),
      onToggleRead: vi.fn(),
      onToggleStar: vi.fn(),
      onToggleLater: vi.fn(),
      onAnalyze: vi.fn(),
      onViewAnalysis: vi.fn(),
    }
    const item = {
      id: 'item-toc',
      subscription_id: 'subscription-1',
      title: '带小标题的订阅文章',
      url: 'https://example.com/toc',
      content_html: '<h1>开头</h1><p>正文</p><h2>中段</h2><p>正文</p><h3>细节</h3><p>正文</p><h4>不进目录</h4>',
    }
    const scrollTo = vi.fn()
    const original = Element.prototype.scrollTo
    Element.prototype.scrollTo = scrollTo as unknown as typeof original
    try {
      const { container } = render(<FeedItemDetail {...props} item={item} />)

      const toc = screen.getByRole('navigation', { name: '正文目录' })
      const items = within(toc).getAllByRole('button')
      expect(items.map((node) => node.textContent)).toEqual(['开头', '中段', '细节'])
      expect(items.map((node) => node.getAttribute('style'))).toEqual([
        'padding-inline-start: 0;',
        'padding-inline-start: 11px;',
        'padding-inline-start: 22px;',
      ])
      // 四级标题拿到锚点但不进目录，也不参与滚动高亮。
      expect(container.querySelector('h4')?.hasAttribute('data-toc-heading')).toBe(false)

      fireEvent.click(items[1])
      expect(scrollTo).toHaveBeenCalled()
      expect(items[1]).toHaveAttribute('aria-current', 'true')
    } finally {
      Element.prototype.scrollTo = original
    }
  })

  it('没有小标题的订阅文章不出目录', () => {
    const props = {
      loading: false,
      analyzing: false,
      onBack: vi.fn(),
      onToggleRead: vi.fn(),
      onToggleStar: vi.fn(),
      onToggleLater: vi.fn(),
      onAnalyze: vi.fn(),
      onViewAnalysis: vi.fn(),
    }
    render(
      <FeedItemDetail
        {...props}
        item={{
          id: 'item-flat',
          subscription_id: 'subscription-1',
          title: '没有小标题',
          url: 'https://example.com/flat',
          content_html: '<p>只有段落</p>',
        }}
      />,
    )

    expect(screen.queryByRole('navigation', { name: '正文目录' })).not.toBeInTheDocument()
  })

  it('shares focus and reading size controls with the saved-link surface', () => {
    const { container } = render(
      <FeedItemDetail
        item={{
          id: 'item-controls',
          subscription_id: 'subscription-1',
          title: '共享阅读控制',
          url: 'https://example.com/controls',
          content: '正文',
        }}
        loading={false}
        analyzing={false}
        onBack={vi.fn()}
        onToggleRead={vi.fn()}
        onToggleStar={vi.fn()}
        onToggleLater={vi.fn()}
        onAnalyze={vi.fn()}
        onViewAnalysis={vi.fn()}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '专注模式' }))
    expect(container.querySelector('.rss-detail-pane')).toHaveClass('feed-focus-mode')

    fireEvent.click(screen.getByRole('button', { name: '阅读字号' }))
    expect(container.querySelector('.rss-detail-pane')).toHaveStyle('--reading-font-size: 17.5px')

    const lineHeightButton = container.querySelector<HTMLButtonElement>('[aria-label^="行距 "]')!
    fireEvent.click(lineHeightButton)
    expect(container.querySelector('.rss-detail-pane')).toHaveStyle('--reading-line-height: 2.12')
  })

  it('uses shared progress, back-to-top and pager controls', () => {
    const previous = vi.fn()
    const next = vi.fn()
    const { container } = render(
      <FeedItemDetail
        item={{
          id: 'item-progress',
          subscription_id: 'subscription-1',
          title: '进度与翻页',
          url: 'https://example.com/progress',
          content: '正文',
        }}
        loading={false}
        analyzing={false}
        onBack={vi.fn()}
        onToggleRead={vi.fn()}
        onToggleStar={vi.fn()}
        onToggleLater={vi.fn()}
        onAnalyze={vi.fn()}
        onViewAnalysis={vi.fn()}
        previous={{ title: '上一篇', onSelect: previous }}
        next={{ title: '下一篇', onSelect: next }}
      />,
    )

    const scroller = container.querySelector('.reader-scroll') as HTMLElement
    Object.defineProperties(scroller, {
      scrollHeight: { configurable: true, value: 1000 },
      clientHeight: { configurable: true, value: 100 },
      scrollTop: { configurable: true, writable: true, value: 450 },
      scrollTo: { configurable: true, value: vi.fn() },
    })
    fireEvent.scroll(scroller)

    expect(container.querySelector('.read-progress')).toHaveStyle({ width: '50%' })
    const backToTop = screen.getByRole('button', { name: '回到顶部' })
    fireEvent.click(backToTop)
    expect(scroller.scrollTo).toHaveBeenCalledWith({ top: 0, behavior: 'smooth' })

    fireEvent.click(screen.getByRole('button', { name: '上一条：上一篇' }))
    fireEvent.click(screen.getByRole('button', { name: '下一条：下一篇' }))
    expect(previous).toHaveBeenCalledTimes(1)
    expect(next).toHaveBeenCalledTimes(1)
  })

  it('keeps RSS analysis separate from saved-link annotation and translation controls', () => {
    const onAnalyze = vi.fn()
    const { container } = render(
      <FeedItemDetail
        item={{
          id: 'item-rss-boundary',
          subscription_id: 'subscription-1',
          subscription_title: '边界测试订阅',
          title: 'RSS 正文与阅读能力边界',
          url: 'https://example.com/rss-boundary',
          content: '订阅正文仍然可以阅读。',
        }}
        loading={false}
        analyzing={false}
        onBack={vi.fn()}
        onToggleRead={vi.fn()}
        onToggleStar={vi.fn()}
        onToggleLater={vi.fn()}
        onAnalyze={onAnalyze}
        onViewAnalysis={vi.fn()}
      />,
    )

    expect(screen.getByRole('button', { name: 'AI 分析' })).toBeEnabled()
    expect(screen.queryByRole('button', { name: '划线' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '写想法' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '翻译全文' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '问 AI' })).not.toBeInTheDocument()
    expect(container.querySelector('.sel-pop')).not.toBeInTheDocument()
    expect(container.querySelector('.note-panel')).not.toBeInTheDocument()
    expect(container.querySelector('.translation-pop')).not.toBeInTheDocument()
    expect(container.querySelector('.ann-list')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'AI 分析' }))
    expect(onAnalyze).toHaveBeenCalledTimes(1)
  })

  it('uses the RSS analysis result action without exposing saved-link AI context', () => {
    const onAnalyze = vi.fn()
    const onViewAnalysis = vi.fn()
    render(
      <FeedItemDetail
        item={{
          id: 'item-rss-analysis',
          subscription_id: 'subscription-1',
          title: '已分析的订阅文章',
          url: 'https://example.com/rss-analysis',
          content: '这篇文章已经有订阅级分析。',
          link_id: 'saved-link-1',
          analysis_status: 'done',
        }}
        loading={false}
        analyzing={false}
        onBack={vi.fn()}
        onToggleRead={vi.fn()}
        onToggleStar={vi.fn()}
        onToggleLater={vi.fn()}
        onAnalyze={onAnalyze}
        onViewAnalysis={onViewAnalysis}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '查看分析' }))
    expect(onViewAnalysis).toHaveBeenCalledWith('saved-link-1')
    expect(onAnalyze).not.toHaveBeenCalled()
    expect(screen.queryByRole('button', { name: '翻译全文' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '问 AI' })).not.toBeInTheDocument()
  })

  it('把未分析正文的选区转成阅读条目', () => {
    const onAnalyze = vi.fn()
    const item = {
      id: 'item-selection-new',
      subscription_id: 'subscription-1',
      title: '未分析文章',
      url: 'https://example.com/selection-new',
      content: '选中这段订阅正文后应出现转阅读动作。',
    }
    const { container } = render(
      <FeedItemDetail
        item={item}
        loading={false}
        analyzing={false}
        onBack={vi.fn()}
        onToggleRead={vi.fn()}
        onToggleStar={vi.fn()}
        onToggleLater={vi.fn()}
        onAnalyze={onAnalyze}
        onViewAnalysis={vi.fn()}
      />,
    )

    const content = container.querySelector('[data-reading-source="subscription-content"]')!
    const textNode = content.firstChild!
    setSelectionRange(textNode)

    fireEvent.click(screen.getByRole('button', { name: '分析并转入阅读' }))
    expect(onAnalyze).toHaveBeenCalledWith(item)
    expect(screen.queryByRole('dialog', { name: '订阅正文选区操作' })).not.toBeInTheDocument()
  })

  it('把已分析正文的选区直接带回已有阅读条目', () => {
    const onAnalyze = vi.fn()
    const onViewAnalysis = vi.fn()
    const item = {
      id: 'item-selection-done',
      subscription_id: 'subscription-1',
      title: '已分析文章',
      url: 'https://example.com/selection-done',
      content: '已分析的订阅正文可以直接去阅读中划线。',
      link_id: 'saved-link-selection',
      analysis_status: 'done' as const,
    }
    const { container } = render(
      <FeedItemDetail
        item={item}
        loading={false}
        analyzing={false}
        onBack={vi.fn()}
        onToggleRead={vi.fn()}
        onToggleStar={vi.fn()}
        onToggleLater={vi.fn()}
        onAnalyze={onAnalyze}
        onViewAnalysis={onViewAnalysis}
      />,
    )

    const content = container.querySelector('[data-reading-source="subscription-content"]')!
    const textNode = content.firstChild!
    setSelectionRange(textNode)

    fireEvent.click(screen.getByRole('button', { name: '在阅读中打开' }))
    expect(onViewAnalysis).toHaveBeenCalledWith('saved-link-selection')
    expect(onAnalyze).not.toHaveBeenCalled()
  })
})
