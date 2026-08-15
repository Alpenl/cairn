import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type CSSProperties } from 'react'
import { Icon } from '../Icon'
import { ArticlePager, type ArticlePagerTarget } from '../ArticlePager'
import { ReaderToc } from '../ReaderToc'
import { ReadingTocControl } from '../detail/ReadingTocControl'
import { sanitizeFeedHTML } from '../../lib/feed-html'
import { useArticleImageSizing } from '../../hooks/useArticleImageSizing'
import { useDomHeadings } from '../../hooks/useDomHeadings'
import { useReadingSurface } from '../../hooks/useReadingSurface'
import {
  READING_LINE_HEIGHTS,
  READING_LINE_HEIGHT_LABELS,
  READING_SIZES,
  htmlSource,
  plainSource,
  readingTextVersion,
} from '../../lib/reading-surface'
import { ReadingProgressStrip } from '../detail/ReadingProgress'
import type { TocHeading } from '../../lib/toc'
import {
  feedItemSourceTitle,
  formatFeedDate,
  itemAnalysisStatus,
  itemIsRead,
  itemIsReadLater,
  itemIsStarred,
  safeHTTPURL,
} from '../../lib/feed'
import type { FeedItem, FeedSubscription } from '../../lib/api/types'

const SUBSCRIPTION_DETAIL_CAPABILITIES = [
  'focus',
  'preferences',
  'progress',
  'toc',
  'back-to-top',
  'pager',
] as const

const SUBSCRIPTION_DETAIL_SLOTS = {
  toolbar: 'minimal',
  rail: 'toc-only',
  annotation: 'disabled',
} as const

interface SubscriptionSelection {
  readonly text: string
  readonly rect: DOMRect
}

function selectionPopoverStyle(rect: DOMRect): CSSProperties {
  const width = Math.min(360, Math.max(0, window.innerWidth - 16))
  const left = Math.max(8, Math.min(
    rect.left + rect.width / 2 - width / 2,
    window.innerWidth - width - 8,
  ))
  const height = 78
  const top = rect.top - height - 8 >= 8
    ? rect.top - height - 8
    : Math.min(rect.bottom + 8, window.innerHeight - height - 8)
  return { left, top, maxWidth: 'calc(100vw - 16px)' }
}

function SubscriptionSelectionPopover({
  selection,
  canViewAnalysis,
  canAnalyze,
  analysisInFlight,
  onAction,
}: {
  selection: SubscriptionSelection
  canViewAnalysis: boolean
  canAnalyze: boolean
  analysisInFlight: boolean
  onAction: () => void
}) {
  return (
    <div
      className="sel-pop subscription-selection-pop"
      role="dialog"
      aria-label="订阅正文选区操作"
      style={selectionPopoverStyle(selection.rect)}
      onMouseDown={(event) => event.preventDefault()}
    >
      <span
        style={{
          maxWidth: 220,
          color: 'var(--text-secondary)',
          fontSize: 11.5,
          lineHeight: 1.4,
        }}
      >
        {canViewAnalysis
          ? '这篇文章已转入阅读，去阅读中划线和写想法。'
          : canAnalyze
            ? '这篇还在订阅里，先转成阅读条目后再划线和写想法。'
            : '订阅源未提供可用的原文地址，暂时无法转入阅读。'}
      </span>
      <span className="div" />
      <button
        type="button"
        disabled={analysisInFlight || (!canViewAnalysis && !canAnalyze)}
        title={!canViewAnalysis && !canAnalyze ? '订阅源未提供可用的原文地址' : undefined}
        onClick={onAction}
      >
        <Icon name={canViewAnalysis ? 'external' : 'sparkles'} size={14} />
        {canViewAnalysis ? '在阅读中打开' : canAnalyze ? '分析并转入阅读' : '无法转入阅读'}
      </button>
    </div>
  )
}

interface FeedItemDetailProps {
  item: FeedItem | null
  source?: FeedSubscription
  loading: boolean
  analyzing: boolean
  onBack: () => void
  onToggleRead: (item: FeedItem) => void
  onToggleStar: (item: FeedItem) => void
  onToggleLater: (item: FeedItem) => void
  onAnalyze: (item: FeedItem) => void
  onViewAnalysis: (linkID: string) => void
  previous?: ArticlePagerTarget | null
  next?: ArticlePagerTarget | null
}

function FeedContent({
  item,
  onHeadings,
  safeHTML,
}: {
  item: FeedItem
  /** 把正文标题交给共用的目录（与阅读详情同一套渲染与滚动高亮）。 */
  onHeadings: (headings: TocHeading[]) => void
  safeHTML: string
}) {
  const contentRef = useRef<HTMLDivElement>(null)
  const htmlSource = item.content_html
  useArticleImageSizing(contentRef, safeHTML)
  // 清洗后的 HTML 由 dangerouslySetInnerHTML 注入，没有中间树可挂锚点，
  // 渲染完再扫一遍 DOM 补上——写出的属性与 markdown 那条路完全一致。
  useDomHeadings(contentRef, safeHTML, 'feed-toc', onHeadings)
  const plain = htmlSource ? null : item.content

  if (safeHTML) {
    return (
      <div
        ref={contentRef}
        className="rss-feed-content reader-flow"
        data-reading-source="subscription-content"
        dangerouslySetInnerHTML={{ __html: safeHTML }}
      />
    )
  }
  if (plain?.trim()) return <div className="rss-feed-content plain reader-prose" data-reading-source="subscription-content">{plain}</div>
  if (item.summary?.trim()) return <div className="rss-feed-content summary-only reader-prose" data-reading-source="subscription-content">{item.summary}</div>
  return (
    <div className="rss-content-empty reader-prose">
      <Icon name="doc" size={24} />
      此订阅源没有提供正文
    </div>
  )
}

export function FeedItemDetail({
  item,
  source,
  loading,
  analyzing,
  onBack,
  onToggleRead,
  onToggleStar,
  onToggleLater,
  onAnalyze,
  onViewAnalysis,
  previous,
  next,
}: FeedItemDetailProps) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const articleRef = useRef<HTMLElement>(null)
  const [selection, setSelection] = useState<SubscriptionSelection | null>(null)
  const articleURL = item ? safeHTTPURL(item.url) : null
  const canAnalyze = Boolean(articleURL)
  const safeHTML = useMemo(
    () => (item?.content_html ? sanitizeFeedHTML(item.content_html, articleURL ?? '') : ''),
    [articleURL, item?.content_html],
  )
  const readingSource = useMemo(() => {
    const hostId = item?.id ?? 'empty-feed-reading-surface'
    const visibleText = item?.content_html
      ? item.summary ?? ''
      : item?.content?.trim()
        ? item.content
        : item?.summary ?? ''
    const version = item?.updated_at ?? item?.published_at ?? item?.created_at ??
      `content:${readingTextVersion(safeHTML || visibleText)}`
    if (safeHTML) {
      // The HTML has already been sanitized. A placeholder base keeps the
      // adapter honest even when a malformed feed item has no valid URL;
      // it is never used to resolve another network request here.
      return htmlSource(safeHTML, articleURL ?? 'https://reader.invalid/', { hostId, version })
    }
    return plainSource(visibleText, { hostId, version })
  }, [articleURL, item?.content, item?.content_html, item?.created_at, item?.id, item?.published_at, item?.summary, item?.updated_at, safeHTML])
  const readingSurface = useReadingSurface({
    source: readingSource,
    capabilities: SUBSCRIPTION_DETAIL_CAPABILITIES,
    slots: SUBSCRIPTION_DETAIL_SLOTS,
    scrollRef,
    layoutKey: 'feed',
  })
  const {
    focusMode,
    setFocusMode,
    readingPreference,
    setReadingPreference,
    progress,
    toc,
  } = readingSurface

  const analysis = item ? itemAnalysisStatus(item) : 'none'
  const analysisInFlight = analyzing || analysis === 'pending' || analysis === 'processing'
  const canViewAnalysis = analysis === 'done' && Boolean(item?.link_id)

  const clearSelection = useCallback(() => {
    try {
      window.getSelection()?.removeAllRanges()
    } catch {
      // Selection cleanup is best effort in older embedded browsers.
    }
    setSelection(null)
  }, [])

  useEffect(() => {
    const onSelectionChange = () => {
      const current = window.getSelection()
      if (!current || current.isCollapsed || current.rangeCount === 0) {
        setSelection(null)
        return
      }
      const range = current.getRangeAt(0)
      const startElement = range.startContainer.nodeType === Node.TEXT_NODE
        ? range.startContainer.parentElement
        : range.startContainer as Element
      const contentRoot = startElement?.closest<HTMLElement>('[data-reading-source="subscription-content"]')
      if (
        !contentRoot ||
        !articleRef.current?.contains(contentRoot) ||
        !contentRoot.contains(range.endContainer)
      ) {
        setSelection(null)
        return
      }
      const text = current.toString().trim()
      if (text.length < 2) {
        setSelection(null)
        return
      }
      setSelection({ text, rect: range.getBoundingClientRect() })
    }
    document.addEventListener('selectionchange', onSelectionChange)
    return () => document.removeEventListener('selectionchange', onSelectionChange)
  }, [item?.id])

  useEffect(() => {
    clearSelection()
  }, [clearSelection, item?.id])

  const onSelectionAction = useCallback(() => {
    if (!item || !selection || analysisInFlight || (!canViewAnalysis && !canAnalyze)) return
    const selectedItem = item
    clearSelection()
    if (canViewAnalysis && selectedItem.link_id) {
      onViewAnalysis(selectedItem.link_id)
      return
    }
    onAnalyze(selectedItem)
  }, [analysisInFlight, canAnalyze, canViewAnalysis, clearSelection, item, onAnalyze, onViewAnalysis, selection])

  useLayoutEffect(() => {
    if (scrollRef.current) scrollRef.current.scrollTop = 0
  }, [item?.id])

  if (!item) {
    return (
      <section className="reader-pane rss-detail-pane">
        <div className="empty">
          <Icon name="rss" size={44} />
          <div className="t">选择一篇订阅文章开始阅读</div>
        </div>
      </section>
    )
  }

  const read = itemIsRead(item)
  const starred = itemIsStarred(item)
  const later = itemIsReadLater(item)
  const mainText = (item.content ??
    (item.content_html
      ? new DOMParser().parseFromString(item.content_html, 'text/html').body.textContent
      : ''))?.replace(/\s+/g, ' ').trim()
  const summaryText = item.summary?.replace(/\s+/g, ' ').trim()
  const showSummary = Boolean(summaryText && mainText && summaryText !== mainText)

  return (
    <section
      className={'reader-pane rss-detail-pane' + (focusMode ? ' feed-focus-mode' : '')}
      data-reading-source-kind={readingSurface.contract.source.kind}
      data-reading-source-block={readingSurface.contract.source.blockKey}
      style={{
        '--reading-font-size': `${READING_SIZES[readingPreference.size]}px`,
        '--reading-line-height': READING_LINE_HEIGHTS[readingPreference.lineHeight],
      } as CSSProperties}
    >
      <div className="reader-toolbar-wrap">
      <div className="reader-toolbar rss-detail-toolbar">
        <button
          type="button"
          className="tb-btn mobile-only mobile-back"
          onClick={onBack}
          aria-label="返回订阅文章列表"
          title="返回订阅文章列表"
        >
          <Icon name="chevron" size={17} />
        </button>
        {articleURL && (
          <a
            className="tb-btn primary"
            href={articleURL}
            target="_blank"
            rel="noopener noreferrer"
            aria-label="打开原文"
            title="打开原文"
          >
            <Icon name="external" size={16} /> <span className="rss-action-label">打开原文</span>
          </a>
        )}
        <button
          type="button"
          className={'tb-btn' + (starred ? ' active' : '')}
          onClick={() => onToggleStar(item)}
          aria-label={starred ? '取消收藏' : '收藏'}
          title={starred ? '取消收藏' : '收藏'}
        >
          <Icon name={starred ? 'star_fill' : 'star'} size={16} />
        </button>
        <button
          type="button"
          className={'tb-btn' + (later ? ' active' : '')}
          onClick={() => onToggleLater(item)}
          aria-label={later ? '移出稍后读' : '加入稍后读'}
          title={later ? '移出稍后读' : '加入稍后读'}
        >
          <Icon name="bookmark" size={16} />
        </button>
        <button
          type="button"
          className="tb-btn"
          onClick={() => onToggleRead(item)}
          aria-label={read ? '标为未读' : '标为已读'}
          title={read ? '标为未读' : '标为已读'}
        >
          <Icon name={read ? 'dot' : 'check'} size={16} />
          </button>
        <button
          type="button"
          className="tb-btn reading-font-button"
          onClick={() => setReadingPreference({ size: (readingPreference.size + 1) % READING_SIZES.length })}
          aria-label="阅读字号"
          title={`阅读字号 ${READING_SIZES[readingPreference.size]}px`}
        >
          Aa
        </button>
        <button
          type="button"
          className="tb-btn line-height-button"
          onClick={() => setReadingPreference({ lineHeight: (readingPreference.lineHeight + 1) % READING_LINE_HEIGHTS.length })}
          aria-label={`行距 ${READING_LINE_HEIGHT_LABELS[readingPreference.lineHeight]}`}
          title={`行距 ${READING_LINE_HEIGHT_LABELS[readingPreference.lineHeight]}`}
        >
          {READING_LINE_HEIGHT_LABELS[readingPreference.lineHeight]}
        </button>
        <button
          type="button"
          className={'tb-btn' + (focusMode ? ' active' : '')}
          onClick={() => setFocusMode(!focusMode)}
          aria-label={focusMode ? '退出专注模式' : '专注模式'}
          title={focusMode ? '退出专注模式' : '专注模式'}
        >
          <Icon name={focusMode ? 'focus_exit' : 'focus'} size={16} />
        </button>
        <ReadingTocControl items={toc.items} activeId={toc.activeId} onJump={toc.jumpTo} />
        <span className="rt-grow" />
        {canViewAnalysis ? (
          <button type="button" className="tb-btn active rss-ai-button" onClick={() => onViewAnalysis(item.link_id as string)}>
            <Icon name="sparkles" size={16} /> 查看分析
          </button>
        ) : (
          <button
            type="button"
            className="tb-btn rss-ai-button"
            onClick={() => onAnalyze(item)}
            disabled={analysisInFlight || !canAnalyze}
            title={
              !canAnalyze
                ? '订阅源未提供可用的原文地址'
                : analysis === 'failed'
                  ? item.analysis_error ?? '上次分析失败'
                  : undefined
            }
          >
            <span className={analysisInFlight ? 'spinning' : ''}>
              <Icon name={analysisInFlight ? 'loader' : analysis === 'failed' ? 'alert' : 'sparkles'} size={16} />
            </span>
            {!canAnalyze
              ? '无法分析'
              : analysisInFlight
                ? '分析中'
                : analysis === 'failed'
                  ? '重试分析'
                  : analysis === 'done'
                    ? '分析完成'
                    : 'AI 分析'}
          </button>
        )}
      </div>
      <ReadingProgressStrip progress={progress.progress} />
      </div>

      {progress.progress > 12 && (
        <button
          className="back-to-top"
          type="button"
          title="回到顶部"
          aria-label="回到顶部"
          onClick={progress.backToTop}
        >
          <Icon name="arrowright" size={16} style={{ transform: 'rotate(-90deg)' }} />
        </button>
      )}
      <div ref={scrollRef} className="reader-scroll" onScroll={() => { progress.sync(); toc.onScroll() }}>
        <ReaderToc items={toc.items} activeId={toc.activeId} onJump={toc.jumpTo} focusMode={focusMode} />
        <article ref={articleRef} className="rss-reader-inner">
          <div className="rss-article-source reader-prose">
            <span className="rss-feed-mark"><Icon name="rss" size={13} /></span>
            <span>{feedItemSourceTitle(item, source)}</span>
            {!read && <span className="rss-unread-label">未读</span>}
          </div>
          <h1 className="reader-prose">{item.title || '无标题文章'}</h1>
          <div className="rss-article-meta reader-prose">
            {item.author?.trim() && <span>{item.author}</span>}
            {item.author?.trim() && <span aria-hidden="true">·</span>}
            <time dateTime={item.published_at ?? item.created_at}>
              {formatFeedDate(item.published_at ?? item.created_at)}
            </time>
            {loading && (
              <span className="rss-detail-loading">
                <span className="spinning"><Icon name="loader" size={12} /></span> 更新正文中
              </span>
            )}
          </div>
          {showSummary && (
            <div className="rss-feed-summary reader-prose">{item.summary}</div>
          )}
          <FeedContent item={item} onHeadings={toc.onHeadings} safeHTML={safeHTML} />
          {articleURL && (
            <footer className="rss-article-footer reader-prose">
              <a href={articleURL} target="_blank" rel="noopener noreferrer">
                <Icon name="external" size={14} /> 在原网站继续阅读
              </a>
            </footer>
          )}
          <ArticlePager previous={previous} next={next} className="rss-article-pager" />
        </article>
      </div>
      {selection && (
        <SubscriptionSelectionPopover
          selection={selection}
          canViewAnalysis={canViewAnalysis}
          canAnalyze={canAnalyze}
          analysisInFlight={analysisInFlight}
          onAction={onSelectionAction}
        />
      )}
    </section>
  )
}
