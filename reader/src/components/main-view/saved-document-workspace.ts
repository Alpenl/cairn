import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import { err, type ApiError, type ApiResult } from '@webtag/api'
import type { ContentEditState } from '../DetailPane'
import type { IconName } from '../Icon'
import type { ToastAction } from '../Toast'
import {
  useArticleAnnotations,
  type ArticleAnnotationCommandResult,
  type ArticleAnnotationRevisionChange,
  type HistoricalArticleAnnotation,
} from '../../hooks/useArticleAnnotations'
import { type UseSavedArticleDocumentResult } from '../../hooks/useSavedArticleDocument'
import { useTranslations } from '../../hooks/useTranslations'
import {
  isContentAnchored,
  type Annotation,
  type AnnotationInput,
  type AnnotationLocator,
  type AnnotationPatch,
  type SelectionInfo,
} from '../../lib/annotations'
import type {
  ContentEditRequest,
  LinkContentResponse,
  LinkResponse,
  TranslationResponse,
} from '../../lib/api/types'
import {
  type DocumentCommandContext,
  isSavedContentTranslationSource,
  type SavedContentSource,
} from '../../lib/article/document'
import type { SourceBlockId } from '../../lib/article/source-block'
import {
  invalidateLink,
  invalidateLinkContent,
  invalidateLinkProjection,
} from '../../lib/cache/invalidate'
import type {
  ReaderCapabilityLease,
  ReaderCapabilityPolicy,
} from '../../lib/capabilities'
import type { IdentityLease } from '../../lib/identity'
import type { ReaderLibrarySitesPort } from '../../lib/reader-api-ports'
import { readAnnotationSnapshot } from '../../lib/user-data/annotation-store'
import { contentRevisionOrUndefined } from './active-resource-controller'

const EMPTY_ANNOTATIONS: readonly Annotation[] = []

// 两个码同属「翻译来源已变」这一类 CAS 冲突，服务端都会附带权威的
// current_identity，Reader 据此刷新来源后重试。
const TRANSLATION_SOURCE_CONFLICTS = new Set([
  'content_revision_conflict',
  'source_block_conflict',
])

type Flash = (msg: string, icon?: IconName, action?: ToastAction) => void

interface CurrentSavedContent {
  readonly linkId: string
  readonly revision: number
  readonly source: SavedContentSource
}

interface UseSavedDocumentWorkspaceOptions {
  readonly client: ReaderLibrarySitesPort
  readonly lease: IdentityLease
  readonly capabilityPolicy: ReaderCapabilityPolicy
  readonly capabilityLease: ReaderCapabilityLease
  readonly activeId: string | null
  readonly renderedActive: LinkResponse | undefined
  readonly savedArticle: UseSavedArticleDocumentResult
  readonly savedDocument: UseSavedArticleDocumentResult['document']
  readonly captureSavedDocumentContext: UseSavedArticleDocumentResult['captureContext']
  readonly summaryBlock: SourceBlockId | null
  readonly activeSummarySourceHash: string | null
  readonly revisionFloor: ReadonlyMap<string, number>
  readonly getActiveLink: () => LinkResponse | undefined
  readonly getContentEditState: () => ContentEditState | null
  readonly reportContentEditState: (state: ContentEditState | null) => void
  readonly resetSummarySourceHash: (linkId: string, source: string | null) => void
  readonly noteContentRevision: (id: string, revision: number) => void
  readonly patchKnownLink: (id: string, patch: Partial<LinkResponse>) => void
  readonly flash: Flash
}

export interface UseSavedDocumentWorkspaceResult {
  readonly anns: Annotation[]
  readonly translations: TranslationResponse[]
  readonly staleTranslations: TranslationResponse[]
  readonly translationsLoading: boolean
  readonly historicalAnnotations: readonly HistoricalArticleAnnotation[]
  readonly historicalDegraded: boolean
  readonly addAnnotation: (
    input: AnnotationInput,
    documentContext: DocumentCommandContext | null,
  ) => Promise<AnnotationLocator | null>
  readonly updateAnnotation: (
    annotation: Annotation,
    patch: AnnotationPatch,
  ) => Promise<boolean>
  readonly removeAnnotation: (annotation: Annotation) => Promise<boolean>
  readonly onSaveContent: (id: string) => Promise<void>
  readonly onReplaceContent: (id: string) => Promise<void>
  readonly onSaveContentEdit: (
    id: string,
    request: ContentEditRequest,
  ) => Promise<ApiResult<LinkContentResponse>>
  readonly onTranslateSelection: (
    info: SelectionInfo,
    force: boolean,
  ) => Promise<string | null>
  readonly onTranslateFull: (force: boolean) => Promise<void>
  readonly savingContent: string | null
}

function annotationCommandCommitted(
  result: ArticleAnnotationCommandResult,
): result is Extract<ArticleAnnotationCommandResult, { readonly sequence: number }> {
  return result.status === 'committed' || result.status === 'duplicate'
}

function sameDocumentCommandContext(
  left: DocumentCommandContext,
  right: DocumentCommandContext,
): boolean {
  return !left.signal.aborted &&
    !right.signal.aborted &&
    left.generation === right.generation &&
    left.id.namespace === right.id.namespace &&
    left.id.linkId === right.id.linkId &&
    left.id.contentRevision === right.id.contentRevision
}

function savedContentFromDocument(
  savedDocument: UseSavedArticleDocumentResult['document'],
): CurrentSavedContent | null {
  if (!savedDocument || savedDocument.body.status !== 'ready') return null
  if (
    savedDocument.body.revision !== savedDocument.id.contentRevision ||
    savedDocument.id.contentRevision < 0 ||
    savedDocument.body.data.content === undefined
  ) {
    return null
  }
  return {
    linkId: savedDocument.id.linkId,
    revision: savedDocument.id.contentRevision,
    source: {
      content: savedDocument.body.data.content,
      ...(savedDocument.body.data.content_document === undefined
        ? {}
        : { 'content-document': savedDocument.body.data.content_document }),
    },
  }
}

export function useSavedDocumentWorkspace({
  client,
  lease,
  capabilityPolicy,
  capabilityLease,
  activeId,
  renderedActive,
  savedArticle,
  savedDocument,
  captureSavedDocumentContext,
  summaryBlock,
  activeSummarySourceHash,
  revisionFloor,
  getActiveLink,
  getContentEditState,
  reportContentEditState,
  resetSummarySourceHash,
  noteContentRevision,
  patchKnownLink,
  flash,
}: UseSavedDocumentWorkspaceOptions): UseSavedDocumentWorkspaceResult {
  const [savingContent, setSavingContent] = useState<string | null>(null)

  const translationContentRevision = savedDocument?.id.contentRevision ??
    contentRevisionOrUndefined(renderedActive?.content_revision) ??
    null
  const {
    items: translationSnapshot,
    staleItems: staleTranslations,
    currentContentRevision: translationsContentRevision,
    loading: translationSnapshotLoading,
    error: translationsError,
    create: createTranslation,
    reloadAfterSourceChange: reloadTranslationsAfterSourceChange,
  } = useTranslations(client, renderedActive?.id ?? activeId, {
    contentRevision: translationContentRevision,
    summarySourceHash: renderedActive?.summary ? activeSummarySourceHash : undefined,
  })

  const currentSavedContent = useMemo(
    () => savedContentFromDocument(savedDocument),
    [savedDocument],
  )
  const previousSavedContentRef = useRef<CurrentSavedContent | null>(null)
  const previousSavedContent = previousSavedContentRef.current
  const revisionChange = useMemo<ArticleAnnotationRevisionChange | undefined>(() => {
    if (
      !currentSavedContent ||
      !previousSavedContent ||
      currentSavedContent.linkId !== previousSavedContent.linkId ||
      currentSavedContent.revision <= previousSavedContent.revision ||
      previousSavedContent.revision <= 0 ||
      currentSavedContent.revision <= 0
    ) {
      return undefined
    }
    return {
      previousRevision: previousSavedContent.revision,
      previousSource: previousSavedContent.source,
      currentSource: currentSavedContent.source,
      currentRevision: currentSavedContent.revision,
    }
  }, [currentSavedContent, previousSavedContent])
  const articleAnnotations = useArticleAnnotations(
    lease,
    capabilityPolicy.annotations ? savedDocument?.id ?? null : null,
    capabilityPolicy.annotations ? summaryBlock : null,
    capabilityPolicy.annotations && revisionChange ? { revisionChange } : undefined,
  )

  // This effect deliberately follows useArticleAnnotations: its revision-change
  // effect must observe the previous source before this ref advances.
  useEffect(() => {
    if (!currentSavedContent) return
    const previous = previousSavedContentRef.current
    if (
      !previous ||
      previous.linkId !== currentSavedContent.linkId ||
      currentSavedContent.revision >= previous.revision
    ) {
      previousSavedContentRef.current = currentSavedContent
    }
  }, [currentSavedContent])

  const { add, update, remove } = articleAnnotations
  const annotationSnapshot = capabilityPolicy.annotations
    ? articleAnnotations.anns
    : EMPTY_ANNOTATIONS

  const runSavedAnnotationCommand = useCallback(async (
    operation: (context: DocumentCommandContext) => Promise<ArticleAnnotationCommandResult>,
  ): Promise<ArticleAnnotationCommandResult> => {
    const operationLease = capabilityLease
    if (!operationLease.isCurrent('annotations')) return { status: 'stale' }
    const controller = savedArticle.controller
    if (!controller) return { status: 'stale' }
    const durable = { value: null as ArticleAnnotationCommandResult | null }
    const outcome = await controller.annotate(async (context) => async () => {
      const result = await operation(context)
      if (!operationLease.isCurrent('annotations')) return { ok: false }
      durable.value = result
      return annotationCommandCommitted(result)
        ? { ok: true, sequence: result.sequence }
        : { ok: false }
    })
    if (
      outcome.status === 'committed' &&
      durable.value &&
      annotationCommandCommitted(durable.value)
    ) {
      return durable.value
    }
    if (
      outcome.status === 'failed' &&
      durable.value &&
      !annotationCommandCommitted(durable.value)
    ) {
      return durable.value
    }
    return outcome.status === 'failed' ? { status: 'failed' } : { status: 'stale' }
  }, [capabilityLease, savedArticle.controller])

  const runSavedTranslationCommand = useCallback(async (
    operation: (context: DocumentCommandContext) => Promise<ApiResult<TranslationResponse>>,
  ): Promise<ApiResult<TranslationResponse>> => {
    const controller = savedArticle.controller
    if (!controller) {
      return err({ kind: 'identity-mismatch', message: '当前正文来源已经更新' })
    }
    const durable = { value: null as ApiResult<TranslationResponse> | null }
    const outcome = await controller.requestTranslation(async (context) => async () => {
      const result = await operation(context)
      durable.value = result
      return { ok: result.ok }
    })
    if (outcome.status === 'committed' && durable.value?.ok) return durable.value
    if (outcome.status === 'failed' && durable.value && !durable.value.ok) {
      return durable.value
    }
    return err({ kind: 'identity-mismatch', message: '当前正文来源已经更新' })
  }, [savedArticle.controller])

  useEffect(() => {
    const controller = savedArticle.controller
    if (!controller) return
    const context = controller.captureContext()
    if (articleAnnotations.loading) {
      controller.beginAnnotationsLoad(context)
      return
    }
    if (articleAnnotations.error) {
      controller.failAnnotations(
        { kind: 'other', message: '无法读取本地划线' },
        context,
      )
      return
    }
    controller.acceptAnnotations(context.id.contentRevision, annotationSnapshot, context)
  }, [
    annotationSnapshot,
    articleAnnotations.error,
    articleAnnotations.loading,
    savedArticle.controller,
    savedDocument?.id.contentRevision,
  ])

  useEffect(() => {
    const controller = savedArticle.controller
    if (!controller) return
    const context = controller.captureContext()
    if (translationSnapshotLoading) {
      controller.beginTranslationsLoad(context)
      return
    }
    if (translationsError) {
      controller.failTranslations(translationsError, context)
      return
    }
    if (translationsContentRevision === null) return
    controller.acceptTranslations({
      current_content_revision: translationsContentRevision,
      current_summary_source_hash: null,
      items: translationSnapshot,
    }, context)
  }, [
    savedArticle.controller,
    savedDocument?.id.contentRevision,
    translationSnapshot,
    translationsContentRevision,
    translationsError,
    translationSnapshotLoading,
  ])

  const anns = useMemo<Annotation[]>(() => {
    const independent = annotationSnapshot.filter(
      (annotation) => !isContentAnchored(annotation.blockKey),
    )
    const saved = savedDocument?.annotations.status === 'ready'
      ? savedDocument.annotations.data
      : []
    if (independent.length === 0) return [...saved]
    if (saved.length === 0) return [...independent]
    return [...independent, ...saved].sort(
      (left, right) => left.createdAt - right.createdAt || left.id.localeCompare(right.id),
    )
  }, [annotationSnapshot, savedDocument?.annotations])

  const translations = useMemo<TranslationResponse[]>(() => {
    const independent = translationSnapshot.filter(
      (item) => !isSavedContentTranslationSource(item.scope, item.block_key),
    )
    const saved = savedDocument?.translations.status === 'ready'
      ? savedDocument.translations.data
      : []
    return saved.length === 0 ? [...independent] : [...saved, ...independent]
  }, [savedDocument?.translations, translationSnapshot])

  // `idle` means the document has not accepted an authoritative list yet; it
  // is not itself proof that a request is running. In particular, summary hash
  // verification deliberately disables the hook resource without fabricating
  // an empty response for the saved-content aggregate.
  const translationsLoading = savedDocument
    ? savedDocument.translations.status === 'loading' ||
      (savedDocument.translations.status === 'idle' && translationSnapshotLoading)
    : translationSnapshotLoading

  const addAnnotation = useCallback(async (
    input: AnnotationInput,
    documentContext: DocumentCommandContext | null,
  ): Promise<AnnotationLocator | null> => {
    if (!capabilityLease.isCurrent('annotations')) return null
    const result = isContentAnchored(input.blockKey)
      ? documentContext
        ? await runSavedAnnotationCommand(async (controllerContext) => {
            if (!sameDocumentCommandContext(documentContext, controllerContext)) {
              return { status: 'stale' }
            }
            return add(input, { documentContext })
          })
        : { status: 'stale' as const }
      : await add(input)
    if (!('annotationId' in result)) return null
    if (isContentAnchored(input.blockKey)) {
      return documentContext
        ? {
            id: result.annotationId,
            blockKey: input.blockKey,
            target: {
              kind: 'saved-content',
              contentRevision: documentContext.id.contentRevision,
            },
          }
        : null
    }
    return input.blockKey === 'summary' && summaryBlock
      ? {
          id: result.annotationId,
          blockKey: input.blockKey,
          target: { kind: 'summary', sourceHash: summaryBlock.sourceHash },
        }
      : null
  }, [add, capabilityLease, runSavedAnnotationCommand, summaryBlock])

  const currentSavedRevision = savedDocument?.id.contentRevision ?? null
  const currentSummaryHash = summaryBlock?.sourceHash ?? null
  const annotationIsCurrent = useCallback((annotation: Annotation): boolean => {
    if (isContentAnchored(annotation.blockKey)) {
      return currentSavedRevision !== null &&
        annotation.sourceContentRevision === currentSavedRevision
    }
    return annotation.blockKey === 'summary' &&
      currentSummaryHash !== null &&
      annotation.sourceSummaryHash === currentSummaryHash
  }, [currentSavedRevision, currentSummaryHash])

  const updateAnnotation = useCallback(async (
    annotation: Annotation,
    patch: AnnotationPatch,
  ): Promise<boolean> => {
    if (!capabilityLease.isCurrent('annotations')) return false
    if (!annotationIsCurrent(annotation)) return false
    const savedContent = isContentAnchored(annotation.blockKey)
    const result = savedContent
      ? await runSavedAnnotationCommand((context) =>
          update(annotation, patch, { documentContext: context }))
      : await update(annotation, patch)
    return result.status === 'committed' || result.status === 'duplicate'
  }, [annotationIsCurrent, capabilityLease, runSavedAnnotationCommand, update])

  const removeAnnotation = useCallback(async (annotation: Annotation): Promise<boolean> => {
    if (!capabilityLease.isCurrent('annotations')) return false
    if (!annotationIsCurrent(annotation)) return false
    const savedContent = isContentAnchored(annotation.blockKey)
    const result = savedContent
      ? await runSavedAnnotationCommand((context) =>
          remove(annotation, { documentContext: context }))
      : await remove(annotation)
    if (result.status !== 'committed' && result.status !== 'duplicate') {
      flash('删除划线失败，请重试', 'alert')
      return false
    }
    return true
  }, [annotationIsCurrent, capabilityLease, flash, remove, runSavedAnnotationCommand])

  // 保存原文：解析完成后按需抓取网页全文并保存，乐观把 content 合进当前 Link。
  const onSaveContent = useCallback(
    async (id: string) => {
      const documentController = savedArticle.controller
      const documentContext = captureSavedDocumentContext()
      setSavingContent(id)
      const res = await client.saveContent(id)
      if (!client.isIdentityCurrent()) return
      setSavingContent(null)
      if (res.ok) {
        if (documentContext?.id.linkId === id) {
          documentController?.acceptBody(res.data, documentContext)
        }
        patchKnownLink(id, {
          has_content: true,
          content_source: res.data.content_source,
        })
        // 补丁会被 useLinks 在下一次列表换引用时清掉，下界不会。
        noteContentRevision(id, res.data.content_revision)
        invalidateLinkContent(id)
        invalidateLinkProjection(id)
        flash('已保存原文', 'check')
      } else if (res.error.kind === 'rate-limited') {
        const wait = res.error.retryAfterSeconds
        flash(wait ? `操作冷却中，请 ${wait}s 后重试` : '操作冷却中，请稍后重试', 'clock')
      } else {
        flash('保存原文失败：' + res.error.message, 'alert')
      }
    },
    [
      captureSavedDocumentContext,
      client,
      flash,
      noteContentRevision,
      patchKnownLink,
      savedArticle.controller,
    ],
  )

  const onReplaceContent = useCallback(
    async (id: string) => {
      // 复用共享的 isContentAnchored 判据，不手抄 block key 列表。换正文会
      // 递增 content_revision；旧 revision 的正文/译文划线仍保留在 durable store，
      // 但不再应用到新正文。摘要有独立 source hash，不受 saved revision 前进影响。
      const current = getActiveLink()
      const projectedContentAnnotations = anns.filter((ann) => isContentAnchored(ann.blockKey))
      let contentAnnotationCount = projectedContentAnnotations.length

      // A save/replace response advances the document generation before the
      // annotation hook has finished reading the matching IDB target. Do not
      // let that small projection gap bypass the destructive-action guard.
      if (contentAnnotationCount === 0 && lease && current?.id === id) {
        const revision = Math.max(
          contentRevisionOrUndefined(current.content_revision) ?? 0,
          savedDocument?.id.linkId === id ? savedDocument.id.contentRevision : 0,
          revisionFloor.get(id) ?? 0,
        )
        if (revision > 0) {
          const snapshot = await readAnnotationSnapshot(lease, id, {
            kind: 'saved-content',
            contentRevision: revision,
          })
          if (snapshot.ok) contentAnnotationCount = snapshot.value.annotations.length
        }
      }
      const hasContentAnnotations = contentAnnotationCount > 0
      const hasUserContent = current?.id === id && current.content_source === 'user'
      if (hasUserContent || hasContentAnnotations) {
        const message = hasUserContent
          ? hasContentAnnotations
            ? `当前原文是人工编辑过的，且有 ${contentAnnotationCount} 条划线。重新抓取会丢弃人工编辑并使当前 revision 的原文与译文划线失效；摘要划线不受影响。继续吗？`
            : '当前原文是人工编辑过的。重新抓取会丢弃你的编辑，用重新抓取的正文覆盖。继续吗？'
          : '重新抓取会替换当前原文。原文与译文上的划线会保留在历史版本中，但不会应用到新原文；摘要划线不受影响。继续吗？'
        if (!window.confirm(message)) return
      }
      const documentController = savedArticle.controller
      const documentContext = captureSavedDocumentContext()
      setSavingContent(id)
      const res = await client.replaceContent(id)
      if (!client.isIdentityCurrent()) return
      setSavingContent(null)
      if (res.ok) {
        if (documentContext?.id.linkId === id) {
          documentController?.acceptBody(res.data, documentContext)
        }
        patchKnownLink(id, {
          has_content: true,
          content_source: res.data.content_source,
        })
        noteContentRevision(id, res.data.content_revision)
        invalidateLinkContent(id)
        invalidateLinkProjection(id)
        // The active content revision changes in the same patch. useTranslations
        // observes that identity transition and owns the single authoritative
        // reload; this call only quarantines the old full translation now.
        await reloadTranslationsAfterSourceChange(id, { reload: false })
        flash('已重新抓取原文', 'check')
      } else if (res.error.kind === 'rate-limited') {
        const wait = res.error.retryAfterSeconds
        flash(wait ? `操作冷却中，请 ${wait}s 后重试` : '操作冷却中，请稍后重试', 'clock')
      } else {
        flash('重新抓取原文失败：' + res.error.message, 'alert')
      }
    },
    [
      anns,
      captureSavedDocumentContext,
      client,
      flash,
      getActiveLink,
      lease,
      noteContentRevision,
      patchKnownLink,
      reloadTranslationsAfterSourceChange,
      revisionFloor,
      savedArticle.controller,
      savedDocument?.id,
    ],
  )

  const onSaveContentEdit = useCallback(
    async (
      id: string,
      request: ContentEditRequest,
    ): Promise<ApiResult<LinkContentResponse>> => {
      const current = getContentEditState()
      if (!current || current.linkId !== id || !current.editing) {
        return err({ kind: 'identity-mismatch', message: '当前编辑会话已经结束' })
      }
      if (current.saving) {
        return err({ kind: 'other', message: '正文正在保存' })
      }
      const controller = savedArticle.controller
      const context = captureSavedDocumentContext()
      const savingState: ContentEditState = { ...current, saving: true }
      reportContentEditState(savingState)

      const result = await client.editContent(id, request)
      if (!client.isIdentityCurrent()) {
        return err({ kind: 'identity-mismatch', message: 'Reader identity changed' })
      }

      if (!result.ok) {
        reportContentEditState({ ...savingState, saving: false })
        return result
      }

      // The request may have crossed another local resource transition. Use the
      // current controller context when possible, while still accepting the
      // canonical response only for this link and a non-older revision.
      const currentContext = captureSavedDocumentContext()
      if (controller && currentContext?.id.linkId === id) {
        controller.acceptBody(result.data, currentContext)
      } else if (controller && context?.id.linkId === id) {
        controller.acceptBody(result.data, context)
      }
      patchKnownLink(id, {
        has_content: true,
        content_source: result.data.content_source,
        content_revision: result.data.content_revision,
      })
      noteContentRevision(id, result.data.content_revision)
      invalidateLinkContent(id)
      invalidateLinkProjection(id)
      await reloadTranslationsAfterSourceChange(id, { reload: false })
      reportContentEditState(null)
      return result
    },
    [
      captureSavedDocumentContext,
      client,
      getContentEditState,
      noteContentRevision,
      patchKnownLink,
      reloadTranslationsAfterSourceChange,
      reportContentEditState,
      savedArticle.controller,
    ],
  )

  const flashTranslationError = useCallback(
    (message: string, kind: string, retryAfterSeconds?: number) => {
      if (kind === 'rate-limited') {
        flash(
          retryAfterSeconds
            ? `翻译请求过多，请 ${retryAfterSeconds}s 后重试`
            : '翻译请求过多，请稍后重试',
          'clock',
        )
        return
      }
      if (kind === 'unauthorized') {
        flash('鉴权失败，请检查连接配置', 'alert')
        return
      }
      flash('翻译失败：' + message, 'alert')
    },
    [flash],
  )

  const refreshAfterTranslationConflict = useCallback(
    async (requestedLinkId: string, error: ApiError): Promise<boolean> => {
      if (
        error.status !== 409 ||
        !error.errorCode ||
        !TRANSLATION_SOURCE_CONFLICTS.has(error.errorCode)
      ) {
        return false
      }

      const reportedRevision = contentRevisionOrUndefined(
        error.currentIdentity?.content_revision,
      )
      const blockKey = error.currentIdentity?.block_key
      const refreshSavedContent =
        error.errorCode === 'content_revision_conflict' ||
        blockKey === 'content' ||
        blockKey === 'content-document'
      if (!refreshSavedContent) {
        const current = getActiveLink()
        resetSummarySourceHash(
          requestedLinkId,
          current?.id === requestedLinkId ? (current.summary ?? null) : null,
        )
      }
      if (refreshSavedContent) {
        invalidateLinkContent(requestedLinkId)
        invalidateLinkProjection(requestedLinkId)
      } else {
        invalidateLink(requestedLinkId)
      }
      const [linkResult, contentResult] = await Promise.all([
        client.getLink(requestedLinkId),
        refreshSavedContent ? client.getContent(requestedLinkId) : Promise.resolve(null),
      ])
      if (!client.isIdentityCurrent()) return true

      if (refreshSavedContent) {
        const linkRevision = linkResult.ok
          ? contentRevisionOrUndefined(linkResult.data.content_revision)
          : undefined
        const contentRevision = contentResult?.ok
          ? contentRevisionOrUndefined(contentResult.data.content_revision)
          : undefined
        const coherentSource =
          linkResult.ok &&
          contentResult?.ok === true &&
          linkRevision !== undefined &&
          linkRevision === contentRevision &&
          (reportedRevision === undefined || reportedRevision === contentRevision)
        if (coherentSource && contentResult?.ok) {
          const patch: Partial<LinkResponse> = {
            ...linkResult.data,
            has_content: true,
            content: contentResult.data.content,
            content_document: contentResult.data.content_document,
            content_format: contentResult.data.content_format,
            content_revision: contentResult.data.content_revision,
          }
          noteContentRevision(requestedLinkId, contentResult.data.content_revision)
          patchKnownLink(requestedLinkId, patch)
          if (getActiveLink()?.id === requestedLinkId) {
            flash('翻译来源已更新，请重新确认后再翻译', 'refresh')
          }
          return true
        }

        // The old body must not survive under a newly reported generation.
        // Keep has_content from the authoritative link projection so the open
        // pane can retry GET /content, but explicitly clear every body field.
        const blockedPatch: Partial<LinkResponse> = linkResult.ok
          ? {
              ...linkResult.data,
              content: undefined,
              content_document: undefined,
              content_format: undefined,
            }
          : {
              content: undefined,
              content_document: undefined,
              content_format: undefined,
            }
        const observedRevisions = [reportedRevision, linkRevision, contentRevision].filter(
          (candidate): candidate is number => candidate !== undefined,
        )
        const blockedRevision = observedRevisions.length > 0
          ? Math.max(...observedRevisions)
          : undefined
        if (blockedRevision !== undefined && blockedRevision > 0) {
          // A conflict or body response proves only the revision floor. Keep
          // the detail projection at the revision actually returned by
          // getLink; otherwise its older metadata would be relabelled as this
          // higher generation and accepted by SavedArticleDocument.
          noteContentRevision(requestedLinkId, blockedRevision)
        }
        patchKnownLink(requestedLinkId, blockedPatch)
        if (getActiveLink()?.id === requestedLinkId) {
          flash('翻译来源已变化，原文刷新失败，请重试', 'alert')
        }
        return true
      }

      if (linkResult.ok) {
        const patch: Partial<LinkResponse> = { ...linkResult.data }
        if (patch.content_revision !== undefined && patch.content_revision > 0) {
          noteContentRevision(requestedLinkId, patch.content_revision)
        }
        patchKnownLink(requestedLinkId, patch)
      }

      if (getActiveLink()?.id === requestedLinkId) {
        flash(
          linkResult.ok
            ? '翻译来源已更新，请重新选择后再翻译'
            : '翻译来源刷新失败，请稍后重试',
          linkResult.ok ? 'refresh' : 'alert',
        )
      }
      return true
    },
    [client, flash, getActiveLink, noteContentRevision, patchKnownLink, resetSummarySourceHash],
  )

  const onTranslateSelection = useCallback(
    async (info: SelectionInfo, force: boolean): Promise<string | null> => {
      const sourceLink = getActiveLink()
      if (!sourceLink) return null
      const request = {
        scope: 'selection',
        block_key: info.blockKey,
        start_offset: info.start,
        end_offset: info.end,
        source_text: info.text,
        force,
      } as const
      const savedSelection = info.blockKey === 'content' || info.blockKey === 'content-document'
      if (savedSelection) {
        const revision = sourceLink.content_revision
        if (revision === undefined || !Number.isInteger(revision) || revision <= 0) {
          flash('后端未返回正文版本，无法安全创建翻译', 'alert')
          return null
        }
      } else if (info.blockKey === 'summary') {
        if (!activeSummarySourceHash) {
          flash('浏览器无法校验摘要版本，请刷新后重试', 'alert')
          return null
        }
      } else {
        flash('该内容块暂不支持翻译', 'alert')
        return null
      }
      if (getActiveLink()?.id !== sourceLink.id) return null

      const res = savedSelection
        ? await runSavedTranslationCommand(async (context) => {
            if (
              context.id.linkId !== sourceLink.id ||
              context.id.contentRevision !== sourceLink.content_revision
            ) {
              return err({ kind: 'identity-mismatch', message: '当前正文来源已经更新' })
            }
            return createTranslation(request)
          })
        : await createTranslation(request)
      if (res.ok) return res.data.id
      if (await refreshAfterTranslationConflict(sourceLink.id, res.error)) return null
      flashTranslationError(
        res.error.message,
        res.error.kind,
        res.error.kind === 'rate-limited' ? res.error.retryAfterSeconds : undefined,
      )
      return null
    },
    [
      activeSummarySourceHash,
      createTranslation,
      flash,
      flashTranslationError,
      getActiveLink,
      refreshAfterTranslationConflict,
      runSavedTranslationCommand,
    ],
  )

  const onTranslateFull = useCallback(
    async (force: boolean) => {
      const sourceLink = getActiveLink()
      const revision = sourceLink?.content_revision
      if (!sourceLink || revision === undefined || !Number.isInteger(revision) || revision <= 0) {
        flash('后端未返回正文版本，无法安全创建翻译', 'alert')
        return
      }
      const res = await runSavedTranslationCommand(async (context) => {
        if (
          context.id.linkId !== sourceLink.id ||
          context.id.contentRevision !== revision
        ) {
          return err({ kind: 'identity-mismatch', message: '当前正文来源已经更新' })
        }
        return createTranslation({
          scope: 'full',
          force,
        })
      })
      if (res.ok) {
        flash(
          res.data.status === 'done' ? '全文译文已就绪' : '已开始全文翻译',
          res.data.status === 'done' ? 'check' : 'translate',
        )
        return
      }
      if (await refreshAfterTranslationConflict(sourceLink.id, res.error)) return
      flashTranslationError(
        res.error.message,
        res.error.kind,
        res.error.kind === 'rate-limited' ? res.error.retryAfterSeconds : undefined,
      )
    },
    [
      createTranslation,
      flashTranslationError,
      flash,
      getActiveLink,
      refreshAfterTranslationConflict,
      runSavedTranslationCommand,
    ],
  )

  return {
    anns,
    translations,
    staleTranslations,
    translationsLoading,
    historicalAnnotations: articleAnnotations.historicalAnnotations,
    historicalDegraded: articleAnnotations.degraded,
    addAnnotation,
    updateAnnotation,
    removeAnnotation,
    onSaveContent,
    onReplaceContent,
    onSaveContentEdit,
    onTranslateSelection,
    onTranslateFull,
    savingContent,
  }
}
