/**
 * 详情面板。
 *
 * R2 范围：工具栏（打开原文 / 复制 / 划线数占位 / AI 开关）、来源行、标题/URL 降级、
 * 标签区（自身 chip 可点筛选 + 相关 ≈ 标签可点）、AI 摘要正文（MarkdownView
 * 渲染，划线 R3）、采集备注、保存原文。
 *
 * R3：选区监听（限定正文 bodyRef）→ ActionPopover（划线 / 写想法 / 问 AI / 复制）、
 * 划线渲染（MarkdownView + rehype 注入 <mark>），AnnotationsList（底部划线列表）、划线数按钮接通、
 * 重叠划线检测（点已有划线打开 NotePanel 而非重复创建）。
 *
 * RF11A：SavedArticleDocument 与 DetailPane 继续拥有 generation-aware document、
 * annotation、translation 和 edit 状态；detail/ 只接收已验证 snapshot，负责正文渲染、
 * 选区浮层、目录与阅读进度。selection source identity 检查仍留在本 owner 内，避免把
 * 状态机拆成彼此不知道 generation 的 hooks。
 */
import { memo, useCallback, useEffect, useMemo, useRef, useState, type CSSProperties } from 'react'
import { foldUnicodeCase } from '@webtag/api'
import { Icon } from './Icon'
import type { ArticlePagerTarget } from './ArticlePager'
import { ReaderRail } from './ReaderRail'
import { ARTICLE_SELECTION_ACTIONS, type PopoverAction } from './ActionPopover'
import {
  ArticleBody,
  type HistoricalAnnotationView,
  type MetadataEditView,
} from './detail/ArticleBody'
import {
  SelectionOverlays,
  type TranslationPopoverState,
} from './detail/SelectionOverlays'
import { ReadingProgressStrip } from './detail/ReadingProgress'
import { ReadingTocControl } from './detail/ReadingTocControl'
import { fetcherIcon, fetcherKey } from '../lib/meta'
import { readingMinutes } from '../lib/reading-time'
import { annotationLocator, annotationMatchesLocator, getSelectionInfo, isContentAnchored, NO_ANNOTATIONS, type Annotation, type AnnotationInput, type AnnotationLocator, type SelectionInfo } from '../lib/annotations'
import { useReadingSurface } from '../hooks/useReadingSurface'
import { useReaderRelatedTags } from '../hooks/useReaderRelatedTags'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import {
  READING_LINE_HEIGHTS,
  READING_LINE_HEIGHT_LABELS,
  READING_SIZES,
  markdownSource,
  plainSource,
  readingTextVersion,
  type ReadingPreference,
  type ReadingSource,
} from '../lib/reading-surface'
import type {
  ContentEditRequest,
  LinkContentResponse,
  LinkResponse,
  ReaderLinkMetadataRequest,
  TranslationResponse,
} from '../lib/api/types'
import type { ApiError, ApiResult } from '@webtag/api'
import type {
  DocumentCommandContext,
  SavedArticleDocument,
} from '../lib/article/document'
import type { TocHeading } from '../lib/toc'

// 键为后端实际产出的 fetcher 主类型（查表前先经 fetcherKey 剥后缀）。
const FETCHER_LABEL: Record<string, string> = {
	basic: '网页',
	github: 'GitHub',
  arxiv: 'arXiv',
  pdf: 'PDF',
	jina: '渲染抓取',
	grok: 'AI 直读',
	wechat: '公众号',
}

const NO_TRANSLATION_HISTORY: TranslationResponse[] = []

function parseMetadataTags(value: string): string[] {
  const seen = new Set<string>()
  const tags: string[] = []
  for (const rawTag of value.split(/[\s,，]+/)) {
    const tag = rawTag.trim()
    if (!tag) continue
    // Match the service's Unicode case-insensitive replacement semantics while
    // keeping the first spelling and input order visible.
    const normalized = foldUnicodeCase(tag)
    if (seen.has(normalized)) continue
    seen.add(normalized)
    tags.push(tag)
  }
  return tags
}

function optionalMetadataText(value: string): string | null {
  const trimmed = value.trim()
  return trimmed || null
}

const SAVED_DETAIL_CAPABILITIES = [
  'focus',
  'preferences',
  'progress',
  'toc',
  'back-to-top',
  'annotations',
  'translation',
  'ai',
  'editing',
  'pager',
] as const

const SAVED_DETAIL_SLOTS = {
  toolbar: 'default',
  rail: 'default',
  annotation: 'enabled',
} as const

function savedDetailReadingSource({
  link,
  document,
  translations,
  summarySourceHash,
  contentOpen,
  contentLanguageView,
  contentView,
  contentEdit,
}: {
  link: LinkResponse | null | undefined
  document: SavedArticleDocument | null
  translations: TranslationResponse[]
  summarySourceHash: string | null
  contentOpen: boolean
  contentLanguageView: 'source' | 'translation'
  contentView: 'structured' | 'plain'
  contentEdit: ContentEditDraft | null
}): ReadingSource {
  const hostId = link?.id ?? 'empty-reading-surface'
  const contentRevision = link?.content_revision ?? document?.id.contentRevision ?? 0
  const contentVersion = `revision:${contentRevision}`
  const identity = { hostId, version: contentVersion }

  if (contentEdit) {
    return contentEdit.format === 'markdown'
      ? markdownSource(contentEdit.draft, identity, 'content')
      : plainSource(contentEdit.draft, identity, 'content')
  }

  const fullTranslation = translations.find(
    (item) =>
      item.scope === 'full' &&
      !item.stale &&
      item.status === 'done' &&
      Boolean(item.translated_text) &&
      item.link_id === link?.id &&
      item.source_content_revision === document?.id.contentRevision,
  )
  if (contentOpen && contentLanguageView === 'translation' && fullTranslation?.translated_text) {
    const translationIdentity = {
      hostId,
      version: `${contentVersion}:translation:${fullTranslation.id}`,
    }
    return fullTranslation.source_format === 'markdown'
      ? markdownSource(fullTranslation.translated_text, translationIdentity, 'content-translation')
      : plainSource(fullTranslation.translated_text, translationIdentity, 'content-translation')
  }

  const currentDocument = document
  const body = currentDocument !== null &&
    currentDocument.id.linkId === link?.id &&
    (link?.content_revision === undefined || currentDocument.id.contentRevision === link.content_revision) &&
    currentDocument.body.status === 'ready' &&
    currentDocument.body.revision === currentDocument.id.contentRevision
    ? currentDocument.body.data
    : null
  if (contentOpen && body) {
    if (body.content_format === 'markdown' && contentView === 'structured' && body.content_document) {
      return markdownSource(body.content_document, identity, 'content-document')
    }
    return plainSource(body.content, identity, 'content')
  }

  const summary = link?.summary ?? ''
  return markdownSource(summary, {
    hostId,
    version: summarySourceHash ?? `summary:${readingTextVersion(summary)}`,
  }, 'summary')
}

interface DetailToolbarProps {
  l: LinkResponse
  onBack: () => void
  onChat: () => void
  aiEnabled: boolean
  annotationsEnabled: boolean
  progressEnabled: boolean
  chatOpen: boolean
  onCopy: () => void
  annCount: number
  onJumpNotes: () => void
  onConvertToSite?: () => void
  onDeleteLink?: () => void
  onToggleFont: () => void
  focusMode: boolean
  onToggleFocus: () => void
  progress: number
  editing: boolean
  tocItems: TocHeading[]
  activeTocId: string | null
  onJumpToc: (id: string) => void
}

function DetailToolbar({ l, onBack, onChat, chatOpen, aiEnabled, annotationsEnabled, progressEnabled, onCopy, annCount, onJumpNotes, onConvertToSite, onDeleteLink, onToggleFont, focusMode, onToggleFocus, progress, editing, tocItems, activeTocId, onJumpToc }: DetailToolbarProps) {
  return (
    <div className="reader-toolbar-wrap">
      <div className="reader-toolbar">
      <button
        type="button"
        className="tb-btn mobile-only mobile-back"
        onClick={onBack}
        title="返回链接列表"
        aria-label="返回链接列表"
      >
        <Icon name="chevron" size={17} />
      </button>
      {!editing && (
        <>
          <a className="tb-btn primary" href={l.url} target="_blank" rel="noopener noreferrer">
            <Icon name="external" size={16} /> 打开原文
          </a>
          <button className="tb-btn" onClick={onCopy} title="复制链接">
            <Icon name="copy" size={16} />
          </button>
          {l.status === 'done' && l.library_kind === 'reading' && onConvertToSite && (
            <button className="tb-btn" onClick={onConvertToSite} title="移到网站收藏" aria-label="移到网站收藏">
              <Icon name="layers" size={16} />
            </button>
          )}
          {onDeleteLink && (
            <button
              type="button"
              className="tb-btn"
              onClick={onDeleteLink}
              title="删除"
              aria-label="删除"
            >
              <Icon name="trash" size={16} />
            </button>
          )}
          <ReadingTocControl items={tocItems} activeId={activeTocId} onJump={onJumpToc} />
        </>
      )}
      <span className="rt-grow" />
      {!editing && (
        <>
          <button className="tb-btn reading-font-button" onClick={onToggleFont} title="阅读偏好" aria-label="阅读偏好">Aa</button>
          <button className={'tb-btn' + (focusMode ? ' active' : '')} onClick={onToggleFocus} title={focusMode ? '退出专注模式' : '专注模式'} aria-label={focusMode ? '退出专注模式' : '专注模式'}>
            <Icon name={focusMode ? 'focus_exit' : 'focus'} size={16} />
          </button>
          {annotationsEnabled && annCount > 0 && (
            <button className="tb-btn" onClick={onJumpNotes} title="划线与想法">
              <Icon name="marker" size={16} />{' '}
              <span style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{annCount}</span>
            </button>
          )}
          {aiEnabled && <button className={'tb-btn' + (chatOpen ? ' active' : '')} onClick={onChat} title="AI 助手 (⌘J)">
            <Icon name="chat" size={17} />
          </button>}
        </>
      )}
      </div>
      {!editing && progressEnabled && <ReadingProgressStrip progress={progress} />}
    </div>
  )
}

export interface ContentEditState {
  readonly linkId: string
  readonly expectedRevision: number
  readonly editing: boolean
  readonly dirty: boolean
  readonly saving: boolean
}

export type MetadataEditOutcome =
  | {
      readonly status: 'saved'
      readonly metadataRevision: number
    }
  | {
      readonly status: 'conflict'
      readonly metadataRevision: number
      readonly error: ApiError
    }
  | {
      readonly status: 'error'
      readonly error: ApiError
    }

interface ContentEditDraft {
  readonly initial: string
  readonly draft: string
  readonly format: 'plain' | 'markdown'
  readonly expectedRevision: number
  readonly error: ApiError | null
  readonly saving: boolean
}

interface MetadataEditDraft extends MetadataEditView {
  readonly expectedRevision: number
}

export interface DetailPaneProps {
  l: LinkResponse | null | undefined
  /** Current saved-original document. Summary remains an independent block. */
  document: SavedArticleDocument | null
  captureDocumentContext: () => DocumentCommandContext | null
  /** 手机单栏布局返回链接列表；桌面端按钮隐藏。 */
  onBack: () => void
  chatOpen: boolean
  onChat: () => void
  onToast: (msg: string, icon?: import('./Icon').IconName) => void
  /** 当前筛选的标签（高亮 cur）。 */
  curTag: string | null
  onPickTag: (t: string) => void
  /** 相关标签语料（已加载链接集合）。 */
  corpus: LinkResponse[]
  /** Optional explicit client; the mounted Reader supplies the active one through the client hook. */
  readerClient?: IdentityBoundReaderClient
  annotationsEnabled: boolean
  aiEnabled: boolean
  relatedTagsEnabled: boolean
  engagementEnabled: boolean
  /** 当前链接的划线集合（来自 durable annotation document，由 MainView 提升）。 */
  anns: Annotation[]
  /** Historical saved-content annotations are optional until MainView adopts the seam. */
  historicalAnnotations?: readonly HistoricalAnnotationView[]
  /** Opens a historical annotation in a recovery surface owned by the caller. */
  onOpenHistoricalAnnotation?: (annotation: Annotation) => void
  /** One revision transition had at least one ambiguous/missing reanchor result. */
  historicalDegraded?: boolean
  /** Optional observer for callers that want the degraded event in addition to the toast. */
  onHistoricalDegraded?: () => void
  /** 新建划线，durable commit 后返回 source-aware reference。 */
  onAddAnn: (
    a: AnnotationInput,
    context: DocumentCommandContext | null,
  ) => Promise<AnnotationLocator | null>
  /** 删除划线。 */
  onRemoveAnn: (annotation: Annotation) => Promise<boolean>
  /** 打开某条划线的 NotePanel。 */
  onOpenNote: (annotation: AnnotationLocator) => void
  /** 问 AI：创建 AI 划线后进入对话草稿（ChatSidebar 消费）。 */
  onAskAI: (annotation: AnnotationLocator, text: string) => void
  /** 保存原文（解析完成后可选）。 */
  onSaveContent: (id: string) => void
  /** 重新抓取并替换已保存原文。 */
  onReplaceContent: (id: string) => void
  /** Replace the saved original through the revision-bound edit endpoint. */
  onEditContent?: (
    id: string,
    request: ContentEditRequest,
  ) => Promise<ApiResult<LinkContentResponse>>
  /** Replace title, summary, and tags through the metadata revision CAS endpoint. */
  onEditMetadata?: (
    id: string,
    revision: number,
    request: ReaderLinkMetadataRequest,
  ) => Promise<MetadataEditOutcome>
  /** Report edit lifecycle so MainView can protect navigation and unload. */
  onContentEditStateChange?: (state: ContentEditState | null) => void
  /** Reconcile controlled editor UI after browser history restores a form snapshot. */
  navigationRestoreEpoch?: number
  /**
   * 按需读取已保存原文（GET /api/links/{id}/content）。原文默认折叠，进详情页
   * 不再随详情把整篇正文拖回来，展开时才调这个。失败返回 null，由本组件在折叠
   * 区里给内联错误 + 重试。
   */
  onLoadContent: () => Promise<unknown>
  /** 正在保存原文的链接 id（按钮 disabled）。 */
  savingContent: string | null
  /** 正在读取单条详情；列表响应本身不携带原文。 */
  loadingDetail?: boolean
  /** 当前链接从数据库恢复并持续轮询的译文。 */
  translations: TranslationResponse[]
  /** 属于其它 saved-content generation 或已被 source hash 判 stale 的历史译文。 */
  staleTranslations?: TranslationResponse[]
  translationsLoading: boolean
  /** Reports the mounted canonical rendered summary projection. */
  onSummaryBlockText?: (
    linkId: string,
    source: string | null,
    renderedText: string | null,
  ) => void
  /** Canonical rendered summary projection identity, independent of document revision. */
  summarySourceHash?: string | null
  /** Forces the mounted canonical summary projection to be reported again. */
  summaryProjectionEpoch?: number
  onTranslateSelection: (info: SelectionInfo, force: boolean) => Promise<string | null>
  onTranslateFull: (force: boolean) => void
	onConvertToSite?: () => void
	onDeleteLink?: () => void
  focusMode: boolean
  onToggleFocus: () => void
  previous?: ArticlePagerTarget | null
  next?: ArticlePagerTarget | null
}

const EDIT_CONTENT_UNAVAILABLE: NonNullable<DetailPaneProps['onEditContent']> = async () => ({
  ok: false,
  error: { kind: 'other', message: '正文编辑不可用' },
})

const EDIT_METADATA_UNAVAILABLE: NonNullable<DetailPaneProps['onEditMetadata']> = async () => ({
  status: 'error',
  error: { kind: 'other', message: '链接信息编辑不可用' },
})

const IGNORE_CONTENT_EDIT_STATE: NonNullable<
  DetailPaneProps['onContentEditStateChange']
> = () => {}

function DetailPaneInner({
  l,
  document: savedDocument,
  captureDocumentContext,
  onBack,
  chatOpen,
  onChat,
  onToast,
  curTag,
  onPickTag,
  corpus,
  readerClient,
  annotationsEnabled,
  aiEnabled,
  relatedTagsEnabled,
  engagementEnabled,
  anns,
  historicalAnnotations = [],
  onOpenHistoricalAnnotation,
  historicalDegraded = false,
  onHistoricalDegraded,
  onAddAnn,
  onRemoveAnn,
  onOpenNote,
  onAskAI,
  onSaveContent,
  onReplaceContent,
  onEditContent = EDIT_CONTENT_UNAVAILABLE,
  onEditMetadata = EDIT_METADATA_UNAVAILABLE,
  onContentEditStateChange = IGNORE_CONTENT_EDIT_STATE,
  navigationRestoreEpoch = 0,
  onLoadContent,
  savingContent,
  loadingDetail = false,
  translations,
  staleTranslations = NO_TRANSLATION_HISTORY,
  translationsLoading,
  onSummaryBlockText,
  summarySourceHash = null,
  summaryProjectionEpoch = 0,
  onTranslateSelection,
  onTranslateFull,
	onConvertToSite,
	onDeleteLink,
  focusMode: parentFocusMode,
  onToggleFocus,
	previous,
	next,
}: DetailPaneProps) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const bodyRef = useRef<HTMLDivElement>(null)
  const reportedSummaryProjection = useRef<string | null>(null)
  const annotationActionPending = useRef(false)
  const historicalDegradedNotice = useRef<string | null>(null)
  const notesRef = useRef<HTMLDivElement>(null)
  const [annotationActionsPending, setAnnotationActionsPending] = useState(false)
  const [pop, setPop] = useState<{
    info: SelectionInfo
    documentContext: DocumentCommandContext | null
    summarySourceHash: string | null
  } | null>(null)
  const [translationPop, setTranslationPop] = useState<TranslationPopoverState | null>(null)
  const [contentView, setContentView] = useState<'structured' | 'plain'>('structured')
  // 原文默认折叠：详情页的主角是 AI 摘要，原文是「需要时再展开」的核对材料。
  // 每换一篇文章都回到折叠态，不做跨文章记忆——否则「默认折叠」就名存实亡。
  const [contentOpen, setContentOpen] = useState(false)
  const [contentLanguageView, setContentLanguageView] = useState<'source' | 'translation'>('source')
  const [fontPopoverOpen, setFontPopoverOpen] = useState(false)
  const [contentEdit, setContentEdit] = useState<ContentEditDraft | null>(null)
  const contentEditRef = useRef<ContentEditDraft | null>(null)
  const [metadataEdit, setMetadataEdit] = useState<MetadataEditDraft | null>(null)
  const metadataEditRef = useRef<MetadataEditDraft | null>(null)
  const editTextareaRef = useRef<HTMLTextAreaElement>(null)
  const hasLegacyContentHighlight = useMemo(
    () => anns.some((ann) => ann.blockKey === 'content'),
    [anns],
  )

  const linkId = l?.id

  useEffect(() => {
    if (!historicalDegraded || !linkId) return
    const revision = l?.content_revision ?? (
      savedDocument?.id.linkId === linkId
        ? savedDocument.id.contentRevision
        : 'unknown'
    )
    const key = `${linkId}:${revision}`
    if (historicalDegradedNotice.current === key) return
    historicalDegradedNotice.current = key
    onToast('部分划线已归档为想法', 'alert')
    onHistoricalDegraded?.()
  }, [historicalDegraded, l?.content_revision, linkId, onHistoricalDegraded, onToast, savedDocument?.id.contentRevision, savedDocument?.id.linkId])

  const related = useReaderRelatedTags(l, corpus, readerClient, { enabled: relatedTagsEnabled })
  const rel = useMemo(
    () => relatedTagsEnabled ? related.tags.slice(0, 3) : [],
    [related.tags, relatedTagsEnabled],
  )
  const surfaceCapabilities = useMemo(
    () => SAVED_DETAIL_CAPABILITIES.filter((capability) => {
      if (capability === 'annotations') return annotationsEnabled
      if (capability === 'ai') return aiEnabled
      if (capability === 'progress') return engagementEnabled
      return true
    }),
    [aiEnabled, annotationsEnabled, engagementEnabled],
  )
  const readingSource = useMemo(
    () => savedDetailReadingSource({
      link: l,
      document: savedDocument,
      translations,
      summarySourceHash,
      contentOpen,
      contentLanguageView,
      contentView,
      contentEdit,
    }),
    [
      contentEdit,
      contentLanguageView,
      contentOpen,
      contentView,
      l,
      savedDocument,
      summarySourceHash,
      translations,
    ],
  )
  const tocSourceKey = useMemo(
    () => JSON.stringify([
      readingSource.kind,
      readingSource.blockKey,
      readingSource.identity.hostId,
      readingSource.identity.version,
      contentOpen,
      contentLanguageView,
      contentView,
      contentEdit ? 'edit' : 'read',
    ]),
    [contentEdit, contentLanguageView, contentOpen, contentView, readingSource],
  )
  const readingSurface = useReadingSurface({
    source: readingSource,
    capabilities: surfaceCapabilities,
    slots: SAVED_DETAIL_SLOTS,
    scrollRef,
    layoutKey: `${contentOpen}|${contentView}|${contentLanguageView}|${contentEdit ? 'edit' : 'read'}`,
    tocSourceKey,
    progressSourceKey: `${linkId ?? 'empty-reading-surface'}:${l?.content_revision ?? l?.updated_at ?? 'empty'}`,
    engagementLinkID: linkId,
    readerClient,
  })
  const {
    focusMode: surfaceFocusMode,
    readingPreference,
    setReadingPreference,
    progress: readingProgress,
    toc,
  } = readingSurface
  const focusMode = parentFocusMode || surfaceFocusMode
  // Keep the old callback contract for MainView while making the store the
  // source of truth shared by the other reading consumer.
  const toggleFocus = useCallback(() => {
    onToggleFocus()
  }, [onToggleFocus])
  const readProgress = readingProgress.progress
  const syncReadProgress = readingProgress.sync
  const resetReadProgress = readingProgress.reset
  const backToTop = readingProgress.backToTop
  const summarySource = l?.summary ?? null
  const fullTranslation = translations.find(
    (item) =>
      item.scope === 'full' &&
      !item.stale &&
      savedDocument?.id.linkId === item.link_id &&
      item.source_content_revision === savedDocument.id.contentRevision,
  )
  const hasStaleFullTranslation = staleTranslations.some(
    (item) => item.scope === 'full' && item.stale,
  )
  const selectionTranslations = translations.filter((item) => item.scope === 'selection')
  const popTranslation = translationPop?.translationId
    ? translations.find((item) => item.id === translationPop.translationId)
    : undefined

  // 选区监听（限定正文 bodyRef 内），对齐 reader.jsx DetailPane。
  useEffect(() => {
    const handler = () => {
      if (contentEditRef.current || metadataEditRef.current) {
        setPop(null)
        return
      }
      const info = getSelectionInfo(bodyRef.current)
      if (!info || info.blockKey === 'content-translation') {
        setPop(null)
        return
      }
      setPop({
        info,
        documentContext: isContentAnchored(info.blockKey)
          ? captureDocumentContext()
          : null,
        summarySourceHash: info.blockKey === 'summary' ? summarySourceHash : null,
      })
    }
    document.addEventListener('selectionchange', handler)
    return () => document.removeEventListener('selectionchange', handler)
  }, [captureDocumentContext, linkId, summarySourceHash])

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      if (contentEditRef.current) return
      if (fontPopoverOpen) {
        setFontPopoverOpen(false)
        return
      }
      if (focusMode) onToggleFocus()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [focusMode, fontPopoverOpen, onToggleFocus])
  // 切换链接时清掉浮层。
  useEffect(() => {
    setPop(null)
    setTranslationPop(null)
    setContentLanguageView('source')
    setContentOpen(false)
    resetReadProgress()
    contentEditRef.current = null
    setContentEdit(null)
    metadataEditRef.current = null
    setMetadataEdit(null)
    onContentEditStateChange(null)
    if (scrollRef.current) scrollRef.current.scrollTop = 0
  }, [linkId, onContentEditStateChange, resetReadProgress])
  // A selection is meaningful only for the exact rendered source it came
  // from. Source identity changes clear both action and translation popovers
  // before an old offset can be committed against the new block.
  useEffect(() => {
    setPop(null)
    setTranslationPop(null)
  }, [savedDocument?.generation, l?.summary, summarySourceHash])

  useEffect(() => {
    if (
      contentLanguageView === 'translation' &&
      !(fullTranslation?.status === 'done' && fullTranslation.translated_text)
    ) {
      setContentLanguageView('source')
    }
  }, [contentLanguageView, fullTranslation])
  useEffect(() => {
    setContentView(hasLegacyContentHighlight ? 'plain' : 'structured')
  }, [linkId, hasLegacyContentHighlight])
  useEffect(() => {
    if (fullTranslation?.status !== 'done' || !fullTranslation.translated_text) {
      setContentLanguageView('source')
    }
  }, [fullTranslation?.status, fullTranslation?.translated_text])

  // useCallback：onClickHL 传进已 memo 的 MarkdownView / PlainTextView，
  // 每渲染新建会让那层 memo 恒失效。
  //
  // 必须待在下面这个 `if (!l)` 早退**之前**：hook 调用顺序在每次渲染中都要
  // 一致，放到早退之后会在「无选中链接 → 选中链接」的那一次渲染上多出一个
  // hook，React 直接抛 "Rendered more hooks than during the previous render"。
  const openAnnotation = useCallback((annotation: Annotation) => {
    const locator = annotationLocator(annotation)
    if (locator) onOpenNote(locator)
  }, [onOpenNote])

  const onClickHL = useCallback((locator: AnnotationLocator) => {
    const annotation = anns.find((item) => annotationMatchesLocator(item, locator))
    if (annotation) onOpenNote(locator)
  }, [anns, onOpenNote])

  useEffect(() => {
    if (!linkId || !onSummaryBlockText) return
    if (!summarySource) {
      reportedSummaryProjection.current = `${summaryProjectionEpoch}\u0000${linkId}\u0000`
      onSummaryBlockText(linkId, null, null)
      return
    }
    const root = bodyRef.current
    if (!root) return
    const report = () => {
      const block = root.querySelector<HTMLElement>('[data-hl-block="summary"]')
      const renderedText = block?.textContent ?? null
      const signature =
        `${summaryProjectionEpoch}\u0000${linkId}\u0000${summarySource}\u0000${renderedText ?? ''}`
      if (reportedSummaryProjection.current === signature) return
      reportedSummaryProjection.current = signature
      onSummaryBlockText(linkId, summarySource, renderedText)
    }
    report()
    // LazyMarkdownView can mount after this parent effect. Observe the existing
    // reader body so the identity is emitted only once the real DOM projection
    // replaces the lazy fallback.
    const observer = new MutationObserver(report)
    observer.observe(root, { childList: true, characterData: true, subtree: true })
    return () => observer.disconnect()
  }, [linkId, onSummaryBlockText, summaryProjectionEpoch, summarySource])

  // 有效原文、「这条到底有没有已保存原文」、阅读量。三件事合在一个 memo 里：
  //
  // · 待在下面的早退**之前**是 hook 顺序的要求（理由同上）；
  // · 阅读量要对**全文**跑两遍正则，而滚动会以 readProgress 驱动本组件逐帧重渲染，
  //   缓存命中之后连折叠态都有正文可数——裸算等于每帧给一篇两万字的文章分配两个
  //   两万元素的数组；
  // · hasSavedContent 也从这里出，是为了让 `l.has_content` 这个闸门在整个文件里
  //   **只出现一次**。它曾经在两处各写一遍（这里一个、JSX 的渲染条件一个），于是
  //   删掉任何**单独一处**测试都不会红——两处互相掩护，正是「看起来无害的简化」
  //   最容易钻的空子。
  const { savedContent, hasSavedContent, readMinutes } = useMemo(() => {
    if (!l) return { savedContent: null, hasSavedContent: false, readMinutes: 1 }
    if (!l.has_content) {
      // 没有原文时折叠头显示的是**摘要**的时长——原文那两项后端计数此刻必然是 0
      // （每一条把 content 置空的 SQL 都在同一句里把它们归零）。
      return { savedContent: null, hasSavedContent: false, readMinutes: readingMinutes(l.summary || '') }
    }
    const documentMatches =
      savedDocument?.id.linkId === l.id &&
      (l.content_revision === undefined ||
        savedDocument.id.contentRevision === l.content_revision)
    const source = documentMatches && savedDocument.body.status === 'ready'
      ? savedDocument.body.data
      : null
    const saved: {
      content: string
      document?: string
      format: 'plain' | 'markdown' | 'html'
      source: 'fetched' | 'user'
    } | null = source
      ? {
          content: source.content,
          document: source.content_document,
          format: source.content_format,
          source: source.content_source,
        }
      : null
    return {
      savedContent: saved,
      hasSavedContent: true,
      // 手里有正文就现数，只有折叠头时用后端数好的两项计数——输入同源、公式同一条
      // （readingMinutes），所以展开前后不会给出两个数字。
      readMinutes: readingMinutes(saved?.content || '', l.content_cjk_chars, l.content_words),
    }
  }, [l, savedDocument])

  const contentLoading = Boolean(
    l &&
      savedDocument?.id.linkId === l.id &&
      savedDocument.id.contentRevision === l.content_revision &&
      savedDocument.body.status === 'loading',
  )
  const contentFailed = Boolean(
    l &&
      savedDocument?.id.linkId === l.id &&
      savedDocument.id.contentRevision === l.content_revision &&
      savedDocument.body.status === 'error',
  )

  const currentContentSource = savedContent?.source ?? l?.content_source ?? 'fetched'
  const fullTranslationBusy = fullTranslation?.status === 'pending' || fullTranslation?.status === 'processing'
  const editableBodyReady = Boolean(
    l &&
      savedContent &&
      savedDocument?.id.linkId === l.id &&
      savedDocument.body.status === 'ready' &&
      savedDocument.body.revision === savedDocument.id.contentRevision &&
      savedDocument.id.contentRevision > 0,
  )
  const canEditContent = Boolean(
    l?.status === 'done' &&
      l.library_kind !== 'site' &&
      hasSavedContent &&
      editableBodyReady &&
      !metadataEdit &&
      !contentLoading &&
      !fullTranslationBusy &&
      savingContent !== l.id &&
      !translationsLoading,
  )
  const canEditMetadata = Boolean(
    l?.status === 'done' &&
      l.library_kind === 'reading' &&
      Number.isSafeInteger(l.metadata_revision) &&
      l.metadata_revision >= 1 &&
      !contentEdit,
  )

  const setEditSession = useCallback((next: ContentEditDraft | null) => {
    contentEditRef.current = next
    setContentEdit(next)
    onContentEditStateChange(next && l
      ? {
          linkId: l.id,
          expectedRevision: next.expectedRevision,
          editing: true,
          dirty: next.draft !== next.initial,
          saving: next.saving,
        }
      : null)
  }, [l, onContentEditStateChange])

  const enterContentEdit = useCallback(() => {
    if (!l || !canEditContent || !savedContent || !savedDocument) return
    const contentAnnotations = anns.filter((annotation) => isContentAnchored(annotation.blockKey))
    if (
      contentAnnotations.length > 0 &&
      !window.confirm(`这篇原文有 ${contentAnnotations.length} 条划线，保存编辑后将全部失效且不可恢复。继续编辑吗？`)
    ) {
      return
    }
    const initial = savedContent.format === 'plain'
      ? savedContent.content
      : savedContent.document ?? savedContent.content
    setPop(null)
    setTranslationPop(null)
    setContentLanguageView('source')
    setContentOpen(true)
    setEditSession({
      initial,
      draft: initial,
      format: savedContent.format === 'plain' ? 'plain' : 'markdown',
      expectedRevision: savedDocument.id.contentRevision,
      error: null,
      saving: false,
    })
  }, [anns, canEditContent, l, savedContent, savedDocument, setEditSession])

  const updateEditDraft = useCallback((draft: string) => {
    const current = contentEditRef.current
    if (!current || current.saving) return
    setEditSession({ ...current, draft, error: null })
  }, [setEditSession])

  const cancelContentEdit = useCallback(() => {
    const current = contentEditRef.current
    if (!current) return
    if (
      current.draft !== current.initial &&
      !window.confirm('当前正文有未保存修改，确定放弃？')
    ) {
      return
    }
    setEditSession(null)
    setContentOpen(true)
    setContentLanguageView('source')
    setPop(null)
    setTranslationPop(null)
  }, [setEditSession])

  const saveContentEdit = useCallback(async () => {
    const current = contentEditRef.current
    if (!current || current.saving || !l || current.draft === current.initial) return
    setEditSession({ ...current, saving: true, error: null })
    const result = await onEditContent(l.id, {
      content: current.draft,
      expected_content_revision: current.expectedRevision,
    })
    if (result.ok) {
      setEditSession(null)
      setContentOpen(true)
      setContentLanguageView('source')
      setContentView(current.format === 'markdown' ? 'structured' : 'plain')
      setPop(null)
      setTranslationPop(null)
      return
    }
    const latest = contentEditRef.current
    if (latest) {
      setEditSession({
        ...latest,
        saving: false,
        error: result.error,
      })
    }
  }, [l, onEditContent, setEditSession])

  const setMetadataEditSession = useCallback((next: MetadataEditDraft | null) => {
    metadataEditRef.current = next
    setMetadataEdit(next)
  }, [])

  const enterMetadataEdit = useCallback(() => {
    if (!l || !canEditMetadata) return
    setPop(null)
    setTranslationPop(null)
    const title = l.title ?? ''
    const summary = l.summary ?? ''
    const tags = l.tags.join(', ')
    setMetadataEditSession({
      title,
      summary,
      tags,
      expectedRevision: l.metadata_revision,
      error: null,
      conflict: false,
      saving: false,
    })
  }, [canEditMetadata, l, setMetadataEditSession])

  const updateMetadataEdit = useCallback((patch: Partial<Pick<MetadataEditView, 'title' | 'summary' | 'tags'>>) => {
    const current = metadataEditRef.current
    if (!current || current.saving) return
    setMetadataEditSession({ ...current, ...patch, error: null, conflict: false })
  }, [setMetadataEditSession])

  const cancelMetadataEdit = useCallback(() => {
    if (!metadataEditRef.current) return
    setMetadataEditSession(null)
    setPop(null)
    setTranslationPop(null)
  }, [setMetadataEditSession])

  const saveMetadataEdit = useCallback(async () => {
    const current = metadataEditRef.current
    if (!current || current.saving || !l) return
    const savingSession = { ...current, saving: true, error: null, conflict: false }
    setMetadataEditSession(savingSession)
    const outcome = await onEditMetadata(l.id, current.expectedRevision, {
      title: optionalMetadataText(current.title),
      summary: optionalMetadataText(current.summary),
      tags: parseMetadataTags(current.tags),
    })
    // A save can settle after navigation. Do not let that earlier session close
    // or alter an editor that the user opened for the next link.
    if (metadataEditRef.current !== savingSession) return
    if (outcome.status === 'saved') {
      setMetadataEditSession(null)
      return
    }
    setMetadataEditSession({
      ...savingSession,
      expectedRevision: outcome.status === 'conflict'
        ? outcome.metadataRevision
        : savingSession.expectedRevision,
      saving: false,
      error: outcome.error,
      conflict: outcome.status === 'conflict',
    })
  }, [l, onEditMetadata, setMetadataEditSession])

  const contentEditActive = contentEdit !== null
  const metadataEditActive = metadataEdit !== null

  useEffect(() => {
    const current = contentEditRef.current
    if (!current) return
    // History traversal may restore an older native form snapshot even though
    // React retained this edit session. Re-publish the controlled draft.
    setContentOpen(true)
    setContentEdit({ ...current })
  }, [navigationRestoreEpoch])

  useEffect(() => {
    const current = metadataEditRef.current
    if (!current) return
    // Keep the controlled fields authoritative after browser history restores
    // an older native form snapshot.
    setMetadataEdit({ ...current })
  }, [navigationRestoreEpoch])

  useEffect(() => {
    if (contentEditActive) editTextareaRef.current?.focus()
  }, [contentEditActive])

  useEffect(() => {
    const onEditKeyDown = (event: KeyboardEvent) => {
      if (metadataEditRef.current) {
        if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 's') {
          event.preventDefault()
          void saveMetadataEdit()
          return
        }
        if (event.key === 'Escape') {
          event.preventDefault()
          cancelMetadataEdit()
        }
        return
      }
      if (!contentEditRef.current) return
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 's') {
        event.preventDefault()
        void saveContentEdit()
        return
      }
      if (event.key === 'Escape') {
        event.preventDefault()
        cancelContentEdit()
      }
    }
    window.addEventListener('keydown', onEditKeyDown)
    return () => window.removeEventListener('keydown', onEditKeyDown)
  }, [cancelContentEdit, cancelMetadataEdit, saveContentEdit, saveMetadataEdit])

  const loadContent = useCallback(() => {
    if (!linkId || !savedDocument || savedDocument.id.linkId !== linkId) return
    void onLoadContent()
  }, [linkId, onLoadContent, savedDocument])

  useEffect(() => {
    if (
      !l ||
      !contentOpen ||
      !hasSavedContent ||
      savedContent ||
      contentLoading ||
      contentFailed
    ) {
      return
    }
    loadContent()
  }, [contentFailed, contentLoading, contentOpen, hasSavedContent, l, loadContent, savedContent])

  if (!l)
    return (
      <section className="reader-pane">
        <div className="empty">
          <Icon name="inbox" size={44} />
          <div className="t">选择一条链接查看详情</div>
        </div>
      </section>
    )

  const fIcon = fetcherIcon(l.fetcher_type)
  const fetcherLabel = l.fetcher_type ? FETCHER_LABEL[fetcherKey(l.fetcher_type)] : undefined
  const onCopy = () => {
    try {
      void navigator.clipboard?.writeText(l.url)
    } catch {
      // clipboard 不可用时静默；toast 仍给反馈。
    }
    onToast('已复制链接', 'copy')
  }

  const clearSel = () => {
    try {
      window.getSelection()?.removeAllRanges()
    } catch {
      // 某些环境下 removeAllRanges 不可用，忽略。
    }
    setPop(null)
  }

  // 选区与现有划线在同一块内重叠 → 返回完整引用（避免跨 source 误操作）。
  const overlappingAnnotation = (info: SelectionInfo): Annotation | null => {
    return anns.find(
      (a) => a.blockKey === info.blockKey && info.start < a.end && info.end > a.start,
    ) ?? null
  }

  const selectionIsCurrent = (
    selection: NonNullable<typeof pop>,
  ): boolean => {
    if (isContentAnchored(selection.info.blockKey)) {
      const context = selection.documentContext
      return Boolean(
        context &&
        !context.signal.aborted &&
        savedDocument &&
        context.id.namespace === savedDocument.id.namespace &&
        context.id.linkId === savedDocument.id.linkId &&
        context.id.contentRevision === savedDocument.id.contentRevision &&
        context.generation === savedDocument.generation,
      )
    }
    return selection.info.blockKey === 'summary' &&
      selection.summarySourceHash !== null &&
      selection.summarySourceHash === summarySourceHash
  }

  const onAction = (act: PopoverAction) => {
    const selection = pop
    const info = selection?.info
    if (!selection || !info) return
    if (act === 'copy') {
      try {
        void navigator.clipboard?.writeText(info.text)
      } catch {
        // clipboard 不可用时静默。
      }
      onToast('已复制划线文字', 'copy')
      clearSel()
      return
    }
    if (act === 'translate') {
      if (!selectionIsCurrent(selection)) {
        onToast('内容来源已更新，请重新选择', 'alert')
        clearSel()
        return
      }
      const requestedLinkId = l.id
      setTranslationPop({ linkId: requestedLinkId, translationId: null, info })
      clearSel()
      void onTranslateSelection(info, false).then((translationId) => {
        if (!translationId) {
          setTranslationPop((current) =>
            current?.linkId === requestedLinkId ? null : current,
          )
          return
        }
        setTranslationPop((current) =>
          current?.linkId === requestedLinkId
            ? { ...current, translationId }
            : current,
        )
      })
      return
    }
    if (!annotationsEnabled || (act === 'ai' && !aiEnabled)) {
      clearSel()
      return
    }
    if (!selectionIsCurrent(selection)) {
      onToast('内容来源已更新，请重新选择', 'alert')
      clearSel()
      return
    }
    const dup = overlappingAnnotation(info)
    if (dup) {
      clearSel()
      openAnnotation(dup)
      return
    }
    if (annotationActionPending.current) return
    annotationActionPending.current = true
    setAnnotationActionsPending(true)
    const base = {
      blockKey: info.blockKey,
      start: info.start,
      end: info.end,
      text: info.text,
      quote: info.quote,
    }
    if (act === 'highlight') {
      void onAddAnn({ ...base, source: 'self' }, selection.documentContext).then((reference) => {
        if (!reference) {
          onToast('内容来源已更新，请重新选择', 'alert')
          clearSel()
          return
        }
        onToast('已划线', 'marker')
        clearSel()
      }).finally(() => {
        annotationActionPending.current = false
        setAnnotationActionsPending(false)
      })
      return
    }
    if (act === 'note') {
      void onAddAnn({ ...base, source: 'self' }, selection.documentContext).then((reference) => {
        if (!reference) {
          onToast('内容来源已更新，请重新选择', 'alert')
          clearSel()
          return
        }
        clearSel()
        onOpenNote(reference)
      }).finally(() => {
        annotationActionPending.current = false
        setAnnotationActionsPending(false)
      })
      return
    }
    if (act === 'ai') {
      void onAddAnn({ ...base, source: 'ai', note: '' }, selection.documentContext).then((reference) => {
        if (!reference) {
          onToast('内容来源已更新，请重新选择', 'alert')
          clearSel()
          return
        }
        clearSel()
        onAskAI(reference, info.text)
      }).finally(() => {
        annotationActionPending.current = false
        setAnnotationActionsPending(false)
      })
      return
    }
  }


  const copyTranslation = (text: string) => {
    try {
      void navigator.clipboard?.writeText(text)
    } catch {
      // clipboard 不可用时静默；toast 仍给反馈。
    }
    onToast('已复制中文译文', 'copy')
  }

  const retrySelectionTranslation = () => {
    if (!translationPop) return
    const requestedLinkId = l.id
    setTranslationPop((current) =>
      current?.linkId === requestedLinkId ? { ...current, translationId: null } : current,
    )
    void onTranslateSelection(translationPop.info, true).then((translationId) => {
      if (!translationId) {
        setTranslationPop((current) =>
          current?.linkId === requestedLinkId ? null : current,
        )
        return
      }
      setTranslationPop((current) =>
        current?.linkId === requestedLinkId ? { ...current, translationId } : current,
      )
    })
  }

  const jumpToNotes = () => {
    const sc = scrollRef.current
    const el = notesRef.current
    if (!sc || !el) return
    const delta = el.getBoundingClientRect().top - sc.getBoundingClientRect().top
    sc.scrollTo({ top: sc.scrollTop + delta - 24, behavior: 'smooth' })
  }

  const updateReadingPreference = (patch: Partial<ReadingPreference>) => {
    setReadingPreference(patch)
  }

  const onReaderScroll = () => {
    syncReadProgress()
    if (fontPopoverOpen) setFontPopoverOpen(false)
    toc.onScroll()
  }

  // 展开原文。**所有**展开入口都必须走这里，不能各自裸调 setContentOpen(true)：
  // 展开而不取数，折叠区会渲染成一片完全空白——没有正文、没有加载态、也没有错误块
  // （`savedContent === null ? null` 那一支）。
  const expandContent = () => {
    setContentOpen(true)
    // 只在真正需要时请求：已经有正文、或正在读都不发。
    // 「这条根本没有已保存原文」不必在这里再判一次——展开入口只在 hasSavedContent
    // 分支里渲染，没有原文时压根不可达。多写一次 `l.has_content` 的代价是实打实的：
    // 同一个不变量落在两处，删掉任何单独一处测试都不会红（两处互相掩护），这正是
    // 这次 bug 反复躲过测试的手法。闸门只留 memo 里那一处。
    if (!savedContent && !contentLoading) loadContent()
  }

  const toggleContent = () => {
    if (contentOpen) {
      setContentOpen(false)
      return
    }
    expandContent()
  }

  const readingFontSize = READING_SIZES[readingPreference.size] + (focusMode ? 1.5 : 0)
  const readingStyle = {
    '--reading-font-size': `${readingFontSize}px`,
    '--summary-font-size': `${readingFontSize + 1.5}px`,
    '--reading-line-height': READING_LINE_HEIGHTS[readingPreference.lineHeight],
  } as CSSProperties
  const selectionActions = ARTICLE_SELECTION_ACTIONS.filter((action) => {
    if ((action === 'highlight' || action === 'note') && !annotationsEnabled) return false
    if (action === 'ai' && (!annotationsEnabled || !aiEnabled)) return false
    return true
  })
  const visibleAnnotations = annotationsEnabled ? anns : NO_ANNOTATIONS

  return (
    <section
      className="reader-pane"
      style={readingStyle}
      data-reading-source-kind={readingSurface.contract.source.kind}
      data-reading-source-block={readingSurface.contract.source.blockKey}
    >
      <DetailToolbar
        l={l}
        onBack={onBack}
        onChat={onChat}
        chatOpen={chatOpen}
        aiEnabled={aiEnabled}
        annotationsEnabled={annotationsEnabled}
        progressEnabled={engagementEnabled}
        onCopy={onCopy}
        annCount={visibleAnnotations.length}
        onJumpNotes={jumpToNotes}
			onConvertToSite={onConvertToSite}
			onDeleteLink={onDeleteLink}
        onToggleFont={() => setFontPopoverOpen((open) => !open)}
        focusMode={focusMode}
        onToggleFocus={() => { setFontPopoverOpen(false); toggleFocus() }}
        progress={readProgress}
        editing={contentEditActive || metadataEditActive}
        tocItems={toc.items}
        activeTocId={toc.activeId}
        onJumpToc={toc.jumpTo}
      />
      {fontPopoverOpen && (
        <div className="reading-preference-popover" role="dialog" aria-label="阅读偏好">
          <header>
            <span>阅读偏好</span>
            <button type="button" title="关闭" aria-label="关闭" onClick={() => setFontPopoverOpen(false)}><Icon name="close" size={12} /></button>
          </header>
          <div className="reading-preference-row">
            <span>字号</span>
            <button type="button" onClick={() => updateReadingPreference({ size: Math.max(0, readingPreference.size - 1) })} disabled={readingPreference.size === 0}>A-</button>
            <output>{READING_SIZES[readingPreference.size]}px</output>
            <button type="button" onClick={() => updateReadingPreference({ size: Math.min(READING_SIZES.length - 1, readingPreference.size + 1) })} disabled={readingPreference.size === READING_SIZES.length - 1}>A+</button>
          </div>
          <div className="reading-preference-row">
            <span>行距</span>
            <button className="line-height-button" type="button" onClick={() => updateReadingPreference({ lineHeight: (readingPreference.lineHeight + 1) % READING_LINE_HEIGHTS.length })}>
              {READING_LINE_HEIGHT_LABELS[readingPreference.lineHeight]}
            </button>
          </div>
        </div>
      )}
      {!contentEditActive && !metadataEditActive && readProgress > 12 && (
        <button className="back-to-top" type="button" title="回到顶部" aria-label="回到顶部" onClick={backToTop}>
          <Icon name="arrowright" size={16} style={{ transform: 'rotate(-90deg)' }} />
        </button>
      )}
      <div className="reader-scroll" ref={scrollRef} onScroll={onReaderScroll}>
        <ReaderRail
          tags={l.tags}
          relatedTags={rel}
          currentTag={curTag}
          onPickTag={onPickTag}
          progress={readProgress}
          progressEnabled={engagementEnabled}
          readMinutes={readMinutes}
          tocItems={toc.items}
          activeTocId={toc.activeId}
          onJumpToc={toc.jumpTo}
          annotations={visibleAnnotations}
          onOpenAnnotation={onOpenNote}
          editing={contentEditActive || metadataEditActive}
        />
        <ArticleBody
          article={l}
          bodyRef={bodyRef}
          notesRef={notesRef}
          editTextareaRef={editTextareaRef}
          focusMode={focusMode}
          fetcherIcon={fIcon}
          fetcherLabel={fetcherLabel}
          readMinutes={readMinutes}
          relatedTags={rel}
          currentTag={curTag}
          annotations={visibleAnnotations}
          historicalAnnotations={historicalAnnotations}
          canEditMetadata={canEditMetadata}
          metadataEdit={metadataEdit}
          loadingDetail={loadingDetail}
          hasSavedContent={hasSavedContent}
          savedContent={savedContent}
          contentOpen={contentOpen}
          contentLanguageView={contentLanguageView}
          contentView={contentView}
          contentSource={currentContentSource}
          contentEdit={contentEdit}
          contentLoading={contentLoading}
          contentFailed={contentFailed}
          canEditContent={canEditContent}
          savingContent={savingContent}
          translationsLoading={translationsLoading}
          fullTranslation={fullTranslation}
          hasStaleFullTranslation={hasStaleFullTranslation}
          selectionTranslations={selectionTranslations}
          historicalTranslations={staleTranslations}
          previous={previous}
          next={next}
          onPickTag={onPickTag}
          onEnterMetadataEdit={enterMetadataEdit}
          onUpdateMetadataEdit={updateMetadataEdit}
          onSaveMetadataEdit={() => void saveMetadataEdit()}
          onCancelMetadataEdit={cancelMetadataEdit}
          onClickHighlight={onClickHL}
          onHeadings={toc.onHeadings}
          onToggleContent={toggleContent}
          onExpandContent={expandContent}
          onSetContentLanguageView={setContentLanguageView}
          onSetContentView={setContentView}
          onSaveContentEdit={() => void saveContentEdit()}
          onCancelContentEdit={cancelContentEdit}
          onUpdateEditDraft={updateEditDraft}
          onTranslateFull={onTranslateFull}
          onEnterContentEdit={enterContentEdit}
          onReplaceContent={onReplaceContent}
          onLoadContent={loadContent}
          onSaveContent={onSaveContent}
          onCopyTranslation={copyTranslation}
          onOpenAnnotation={openAnnotation}
          onOpenHistoricalAnnotation={onOpenHistoricalAnnotation}
          onRemoveAnnotation={onRemoveAnn}
        />
      </div>

      <SelectionOverlays
        actionSelection={pop?.info ?? null}
        annotationActionsPending={annotationActionsPending}
        translation={translationPop}
        translationResult={popTranslation}
        onAction={onAction}
        onCloseTranslation={() => setTranslationPop(null)}
        onRetryTranslation={retrySelectionTranslation}
        onCopyTranslation={copyTranslation}
        actions={selectionActions}
      />
    </section>
  )
}

/**
 * React.memo：DetailPane 挂在 MainView 之下，而 MainView 的状态churn 极其
 * 频繁（toast、chatOpen、mobileNavOpen、翻译轮询 1.2 秒一次、列表静默刷新
 * 30 秒一次）。这些状态没有一个与正在阅读的文章有关，但过去每一次都会让
 * DetailPane 连同它内部的 MarkdownView 整棵重渲染。
 *
 * 与 MarkdownView 上那层 memo 是两道独立的闸：这一道挡住 MainView 的无关
 * 状态，那一道挡住 DetailPane 自身的局部状态（原文折叠、字号、目录滚动）
 * 波及整篇 markdown 的重新 parse。
 *
 * 同样依赖调用侧的 props 稳定性——MainView 里的 onPickTag / onToggleFocus /
 * onConvertToSite 与 previous / next 两个 pager 对象都是为此从 JSX 内联
 * 字面量提升成 useCallback / useMemo 的。
 */
export const DetailPane = memo(DetailPaneInner)
