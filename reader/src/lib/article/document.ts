import { isContentAnchored, type Annotation } from '../annotations'
import type { ApiError, ApiResult } from '../api/result'
import type {
  LinkContentResponse,
  LinkResponse,
  TranslationListResponse,
  TranslationResponse,
} from '../api/types'
import { type IdentityOperationContext, IdentityLease } from '../identity'
import {
  reanchorAnnotation,
  type ReanchorReason,
} from '../reanchor'
import { isValidLinkId } from './source-block'

const DOCUMENT_CONTEXT_TOKEN: unique symbol = Symbol('saved document context token')

export interface SavedDocumentId {
  readonly namespace: string
  readonly linkId: string
  readonly contentRevision: number
}

export type SavedContentSource = string | Readonly<Record<string, string>>

export interface HistoricalSavedContentAnnotation {
  readonly status: 'historical'
  readonly reason: ReanchorReason | 'revision-changed'
  readonly sourceContentRevision: number
  readonly annotation: Annotation
}

export type SavedContentAnnotationReanchorResult =
  | {
      readonly status: 'reanchored'
      readonly reason: 'diff-context' | 'unique-quote'
      readonly fromRevision: number
      readonly toRevision: number
      readonly annotation: Annotation
    }
  | {
      readonly status: 'historical'
      readonly reason: 'ambiguous-quote' | 'missing-quote' | 'deleted-range'
      readonly fromRevision: number
      readonly toRevision: number
      readonly annotation: Annotation
    }

export type SavedBody = Pick<
  LinkContentResponse,
  'content' | 'content_document' | 'content_format' | 'content_source'
>

export type ResourceState<T> =
  | { readonly status: 'idle' }
  | { readonly status: 'loading'; readonly revision: number }
  | { readonly status: 'error'; readonly revision: number; readonly error: ApiError }
  | { readonly status: 'ready'; readonly revision: number; readonly data: T }

export interface SavedArticleDocument {
  readonly id: SavedDocumentId
  readonly generation: number
  readonly detail: ResourceState<LinkResponse>
  readonly body: ResourceState<SavedBody>
  readonly annotations: ResourceState<Annotation[]>
  /** Historical saved-content items are retained outside the current mark projection. */
  readonly historicalAnnotations?: readonly HistoricalSavedContentAnnotation[]
  readonly translations: ResourceState<TranslationResponse[]>
}

export interface DocumentCommandContext {
  readonly id: SavedDocumentId
  readonly generation: number
  readonly signal: AbortSignal
  readonly [DOCUMENT_CONTEXT_TOKEN]: object
}

export type AnnotationCommandResult =
  | { readonly ok: true; readonly sequence: number }
  | { readonly ok: false }

export type TranslationCommandResult =
  | { readonly ok: true }
  | { readonly ok: false }

export type DurableCommandResult = AnnotationCommandResult | TranslationCommandResult

/**
 * The first phase must only prepare a durable commit. The returned commit
 * binds context.signal to its full transaction so a revision/identity abort
 * rolls back an in-flight write.
 */
export type DocumentCommand<Result extends DurableCommandResult> = (
  context: DocumentCommandContext,
) => Promise<() => Promise<Result>>

export type AnnotationCommand = DocumentCommand<AnnotationCommandResult>
export type TranslationCommand = DocumentCommand<TranslationCommandResult>

type CommandInterruption = { readonly status: 'stale' | 'disposed' }

export type AnnotationCommandOutcome =
  | { readonly status: 'committed'; readonly sequence: number }
  | { readonly status: 'failed' | 'stale' | 'disposed' }

export type TranslationCommandOutcome =
  | { readonly status: 'committed' }
  | { readonly status: 'failed' | 'stale' | 'disposed' }


type CommandExecution<Result extends DurableCommandResult> =
  | { readonly status: 'completed'; readonly result: Result }
  | CommandInterruption

interface ControllerOptions {
  readonly lease: IdentityLease
  readonly detail: LinkResponse
}

type Listener = () => void
type RevisionResources = Partial<
  Pick<SavedArticleDocument, 'detail' | 'body' | 'translations'>
>

function validRevision(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
}

function validSavedSourceRevision(value: unknown): value is number {
  return validRevision(value) && value > 0
}

function sourceForBlock(source: SavedContentSource, blockKey: string): string | null {
  if (typeof source === 'string') return source
  const value = source[blockKey]
  return typeof value === 'string' ? value : null
}

function validAnnotationRange(annotation: Annotation, source: string): boolean {
  return Number.isSafeInteger(annotation.start) &&
    Number.isSafeInteger(annotation.end) &&
    annotation.start >= 0 &&
    annotation.end > annotation.start &&
    annotation.end <= source.length
}

function validAnnotationQuote(annotation: Annotation, source: string): boolean {
  return annotation.quote !== undefined &&
    typeof annotation.quote.exact === 'string' &&
    annotation.quote.exact.length > 0 &&
    source.slice(annotation.start, annotation.end) === annotation.quote.exact &&
    typeof annotation.quote.prefix === 'string' &&
    typeof annotation.quote.suffix === 'string'
}

function historicalResult(
  annotation: Annotation,
  reason: 'ambiguous-quote' | 'missing-quote' | 'deleted-range',
  fromRevision: number,
  toRevision: number,
): SavedContentAnnotationReanchorResult {
  return {
    status: 'historical',
    reason,
    fromRevision,
    toRevision,
    annotation: {
      ...annotation,
      sourceContentRevision: fromRevision,
    },
  }
}

/**
 * Adapts the shared reanchor kernel to the Reader's durable Annotation shape.
 * A result is returned for every saved-content item, including items that are
 * not safe to attach to the new source. Callers can persist the two result
 * classes to separate targets without deleting the old target.
 */
export function reanchorSavedContentAnnotations(
  annotations: readonly Annotation[],
  fromRevision: number,
  toRevision: number,
  previousSource: SavedContentSource,
  currentSource: SavedContentSource,
  linkId: string,
): readonly SavedContentAnnotationReanchorResult[] {
  return annotations
    .filter((item) => isContentAnchored(item.blockKey))
    .map((item) => {
      const oldSource = sourceForBlock(previousSource, item.blockKey)
      const newSource = sourceForBlock(currentSource, item.blockKey)
      if (
        !validSavedSourceRevision(fromRevision) ||
        !validSavedSourceRevision(toRevision) ||
        toRevision <= fromRevision ||
        oldSource === null ||
        newSource === null ||
        !validAnnotationRange(item, oldSource) ||
        !validAnnotationQuote(item, oldSource ?? '')
      ) {
        return historicalResult(item, 'missing-quote', fromRevision, toRevision)
      }

      const result = reanchorAnnotation(
        {
          id: item.id,
          blockKey: item.blockKey,
          range: { start: item.start, end: item.end },
          quote: item.quote!,
          target: {
            kind: 'link-content',
            hostId: linkId,
            version: `content:${fromRevision}`,
          },
          thought: item.note,
        },
        oldSource,
        newSource,
        {
          kind: 'link-content',
          hostId: linkId,
          version: `content:${toRevision}`,
        },
      )

      if (result.status === 'historical') {
        return historicalResult(item, result.reason, fromRevision, toRevision)
      }

      return {
        status: 'reanchored',
        reason: result.reason,
        fromRevision,
        toRevision,
        annotation: {
          ...item,
          start: result.annotation.range.start,
          end: result.annotation.range.end,
          text: result.annotation.quote.exact,
          quote: result.annotation.quote,
          sourceContentRevision: toRevision,
        },
      }
    })
}

function savedBody(
  content: string,
  contentDocument: string | undefined,
  contentFormat: SavedBody['content_format'],
  contentSource: SavedBody['content_source'] = 'fetched',
): SavedBody {
  return {
    content,
    ...(contentDocument === undefined ? {} : { content_document: contentDocument }),
    content_format: contentFormat,
    content_source: contentSource,
  }
}

function bodyFromResponse(response: LinkContentResponse): SavedBody {
  return savedBody(
    response.content,
    response.content_document,
    response.content_format,
    response.content_source,
  )
}

function bodyFromDetail(detail: LinkResponse): SavedBody | null {
  if (
    typeof detail.content !== 'string' ||
    (detail.content_format !== 'plain' &&
      detail.content_format !== 'markdown' &&
      detail.content_format !== 'html')
  ) {
    return null
  }
  return savedBody(
    detail.content,
    detail.content_document,
    detail.content_format,
    detail.content_source ?? 'fetched',
  )
}

export function isSavedContentTranslationSource(
  scope: string,
  blockKey: string | null | undefined,
): boolean {
  return scope === 'full' || blockKey === 'content' || blockKey === 'content-document'
}

function isSavedTranslation(item: TranslationResponse, id: SavedDocumentId): boolean {
  // The wire does not expose source_hash; stale is the server's source-hash verdict.
  return item.link_id === id.linkId &&
    validSavedSourceRevision(item.source_content_revision) &&
    item.source_content_revision === id.contentRevision &&
    isSavedContentTranslationSource(item.scope, item.block_key) &&
    !item.stale
}

function sameDocumentId(left: SavedDocumentId, right: SavedDocumentId): boolean {
  return left.namespace === right.namespace &&
    left.linkId === right.linkId &&
    left.contentRevision === right.contentRevision
}

function historicalFromRevision(
  annotations: readonly Annotation[],
  revision: number,
): readonly HistoricalSavedContentAnnotation[] {
  return annotations
    .filter((item) => isContentAnchored(item.blockKey))
    .map((annotation) => ({
      status: 'historical' as const,
      reason: 'revision-changed' as const,
      sourceContentRevision: revision,
      annotation,
    }))
}

export class SavedArticleDocumentController {
  private state: SavedArticleDocument
  private readonly listeners = new Set<Listener>()
  private readonly identityLease: IdentityLease
  private readonly identityOperation: IdentityOperationContext
  private readonly contextToken = Object.freeze({})
  private generationController = new AbortController()
  private bodyRequestSequence = 0
  private disposed = false
  private readonly onIdentityAbort = (): void => this.dispose()

  constructor(options: ControllerOptions) {
    const identityOperation = options.lease.capture('saved article document')
    if (
      !options.lease.isCurrent(identityOperation) ||
      !identityOperation.physicalNamespace ||
      !isValidLinkId(options.detail.id) ||
      !validRevision(options.detail.content_revision)
    ) {
      throw new Error('Saved article identity is invalid')
    }
    this.identityLease = options.lease
    this.identityOperation = identityOperation
    identityOperation.signal.addEventListener('abort', this.onIdentityAbort, { once: true })
    const revision = options.detail.content_revision
    const inlineBody = validSavedSourceRevision(revision)
      ? bodyFromDetail(options.detail)
      : null
    this.state = {
      id: {
        namespace: identityOperation.physicalNamespace,
        linkId: options.detail.id,
        contentRevision: revision,
      },
      generation: 1,
      detail: {
        status: 'ready',
        revision,
        data: options.detail,
      },
      body: inlineBody === null
        ? { status: 'idle' }
        : { status: 'ready', revision, data: inlineBody },
      annotations: { status: 'idle' },
      historicalAnnotations: [],
      translations: { status: 'idle' },
    }
  }

  getSnapshot(): SavedArticleDocument {
    return this.state
  }

  captureContext(): DocumentCommandContext {
    return Object.freeze({
      id: Object.freeze({ ...this.state.id }),
      generation: this.state.generation,
      signal: this.generationController.signal,
      [DOCUMENT_CONTEXT_TOKEN]: this.contextToken,
    })
  }

  subscribe(listener: Listener): () => void {
    this.listeners.add(listener)
    return () => this.listeners.delete(listener)
  }

  private notify(): void {
    if (this.disposed) return
    for (const listener of this.listeners) listener()
  }

  private publish(next: SavedArticleDocument): void {
    if (this.disposed) return
    this.state = next
    this.notify()
  }

  private identityIsCurrent(): boolean {
    return this.identityLease.isCurrent(this.identityOperation)
  }

  private contextIsCurrent(context: DocumentCommandContext): boolean {
    return !this.disposed &&
      this.identityIsCurrent() &&
      context[DOCUMENT_CONTEXT_TOKEN] === this.contextToken &&
      !context.signal.aborted &&
      context.generation === this.state.generation &&
      sameDocumentId(context.id, this.state.id)
  }

  private advanceRevision(revision: number, resources: RevisionResources = {}): void {
    const previousController = this.generationController
    const carriedHistorical = this.state.annotations.status === 'ready'
      ? historicalFromRevision(this.state.annotations.data, this.state.id.contentRevision)
      : []
    const next: SavedArticleDocument = {
      id: { ...this.state.id, contentRevision: revision },
      generation: this.state.generation + 1,
      detail: resources.detail ?? { status: 'idle' },
      body: resources.body ?? { status: 'idle' },
      annotations: { status: 'idle' },
      historicalAnnotations: [
        ...(this.state.historicalAnnotations ?? []),
        ...carriedHistorical,
      ],
      translations: resources.translations ?? { status: 'idle' },
    }

    this.generationController = new AbortController()
    this.bodyRequestSequence += 1
    this.state = next
    previousController.abort()
    if (this.state === next) this.notify()
  }

  acceptRevisionFloor(revision: number): boolean {
    if (
      this.disposed ||
      !this.identityIsCurrent() ||
      !validRevision(revision) ||
      revision <= this.state.id.contentRevision
    ) {
      return false
    }
    this.advanceRevision(revision)
    return true
  }

  acceptDetail(detail: LinkResponse, context: DocumentCommandContext): boolean {
    if (
      this.disposed ||
      !this.contextIsCurrent(context) ||
      detail.id !== this.state.id.linkId ||
      !validRevision(detail.content_revision) ||
      detail.content_revision < this.state.id.contentRevision
    ) {
      return false
    }
    const revision = detail.content_revision
    const detailResource: ResourceState<LinkResponse> = {
      status: 'ready',
      revision,
      data: detail,
    }
    const inlineBody = validSavedSourceRevision(revision) ? bodyFromDetail(detail) : null
    const bodyResource: ResourceState<SavedBody> | undefined = inlineBody === null
      ? undefined
      : { status: 'ready', revision, data: inlineBody }

    if (revision > this.state.id.contentRevision) {
      this.advanceRevision(revision, {
        detail: detailResource,
        ...(bodyResource === undefined ? {} : { body: bodyResource }),
      })
    } else {
      if (bodyResource !== undefined) this.bodyRequestSequence += 1
      this.publish({
        ...this.state,
        detail: detailResource,
        body: bodyResource ?? this.state.body,
      })
    }
    return true
  }

  private responseMatchesDocument(
    response: LinkContentResponse,
    context: DocumentCommandContext | null,
  ): boolean {
    return !this.disposed &&
      this.identityIsCurrent() &&
      (context === null || this.contextIsCurrent(context)) &&
      response.link_id === this.state.id.linkId &&
      validSavedSourceRevision(response.content_revision) &&
      response.content_revision >= this.state.id.contentRevision
  }

  private acceptBodyResponse(
    response: LinkContentResponse,
    context: DocumentCommandContext | null,
  ): boolean {
    if (!this.responseMatchesDocument(response, context)) return false
    const revision = response.content_revision
    const resource: ResourceState<SavedBody> = {
      status: 'ready',
      revision,
      data: bodyFromResponse(response),
    }
    if (revision > this.state.id.contentRevision) {
      this.advanceRevision(revision, { body: resource })
    } else {
      this.publish({ ...this.state, body: resource })
    }
    return true
  }

  acceptBody(response: LinkContentResponse, context: DocumentCommandContext): boolean {
    if (!this.responseMatchesDocument(response, context)) return false
    this.bodyRequestSequence += 1
    return this.acceptBodyResponse(response, context)
  }

  async loadBody(
    load: (context: DocumentCommandContext) => Promise<ApiResult<LinkContentResponse>>,
  ): Promise<ApiResult<LinkContentResponse>> {
    const context = this.captureContext()
    const revision = context.id.contentRevision
    const requestSequence = ++this.bodyRequestSequence
    this.publish({ ...this.state, body: { status: 'loading', revision } })
    const result = await load(context)
    if (!this.contextIsCurrent(context)) {
      if (
        result.ok &&
        this.identityIsCurrent() &&
        result.data.content_revision > this.state.id.contentRevision
      ) {
        this.acceptBodyResponse(result.data, null)
      }
      return result
    }
    if (result.ok) {
      if (
        requestSequence !== this.bodyRequestSequence &&
        result.data.content_revision <= this.state.id.contentRevision
      ) {
        return result
      }
      const accepted = this.acceptBodyResponse(result.data, context)
      if (
        !accepted &&
        requestSequence === this.bodyRequestSequence &&
        this.state.body.status === 'loading'
      ) {
        this.publish({
          ...this.state,
          body: {
            status: 'error',
            revision,
            error: {
              kind: 'other',
              message: '已保存原文响应不属于当前文档版本',
            },
          },
        })
      }
      return result
    }
    if (requestSequence === this.bodyRequestSequence) {
      this.publish({
        ...this.state,
        body: { status: 'error', revision, error: result.error },
      })
    }
    return result
  }

  acceptAnnotations(
    revision: number,
    annotations: readonly Annotation[],
    context: DocumentCommandContext,
    historicalAnnotations?: readonly HistoricalSavedContentAnnotation[],
  ): boolean {
    if (
      this.disposed ||
      !this.contextIsCurrent(context) ||
      revision !== this.state.id.contentRevision
    ) {
      return false
    }
    const current = annotations.filter(
      (item) => isContentAnchored(item.blockKey) &&
        validSavedSourceRevision(item.sourceContentRevision) &&
        item.sourceContentRevision === revision,
    )
    this.publish({
      ...this.state,
      annotations: { status: 'ready', revision, data: current },
      ...(historicalAnnotations === undefined ? {} : { historicalAnnotations }),
    })
    return true
  }

  acceptTranslations(
    response: TranslationListResponse,
    context: DocumentCommandContext,
  ): boolean {
    const revision = response.current_content_revision
    if (
      this.disposed ||
      !this.contextIsCurrent(context) ||
      !validRevision(revision) ||
      revision < this.state.id.contentRevision
    ) {
      return false
    }
    const targetId = { ...this.state.id, contentRevision: revision }
    const resource: ResourceState<TranslationResponse[]> = {
      status: 'ready',
      revision,
      data: response.items.filter((item) => isSavedTranslation(item, targetId)),
    }
    if (revision > this.state.id.contentRevision) {
      this.advanceRevision(revision, { translations: resource })
    } else {
      this.publish({ ...this.state, translations: resource })
    }
    return true
  }

  beginDetailLoad(context: DocumentCommandContext): boolean {
    if (!this.contextIsCurrent(context)) return false
    const revision = this.state.id.contentRevision
    this.publish({ ...this.state, detail: { status: 'loading', revision } })
    return true
  }

  failDetail(error: ApiError, context: DocumentCommandContext): boolean {
    if (!this.contextIsCurrent(context)) return false
    const revision = this.state.id.contentRevision
    this.publish({ ...this.state, detail: { status: 'error', revision, error } })
    return true
  }

  beginAnnotationsLoad(context: DocumentCommandContext): boolean {
    if (!this.contextIsCurrent(context)) return false
    const revision = this.state.id.contentRevision
    this.publish({ ...this.state, annotations: { status: 'loading', revision } })
    return true
  }

  failAnnotations(error: ApiError, context: DocumentCommandContext): boolean {
    if (!this.contextIsCurrent(context)) return false
    const revision = this.state.id.contentRevision
    this.publish({ ...this.state, annotations: { status: 'error', revision, error } })
    return true
  }

  beginTranslationsLoad(context: DocumentCommandContext): boolean {
    if (!this.contextIsCurrent(context)) return false
    const revision = this.state.id.contentRevision
    this.publish({ ...this.state, translations: { status: 'loading', revision } })
    return true
  }

  failTranslations(error: ApiError, context: DocumentCommandContext): boolean {
    if (!this.contextIsCurrent(context)) return false
    const revision = this.state.id.contentRevision
    this.publish({ ...this.state, translations: { status: 'error', revision, error } })
    return true
  }

  private async executeCommand<Result extends DurableCommandResult>(
    command: DocumentCommand<Result>,
  ): Promise<CommandExecution<Result>> {
    if (this.disposed) return { status: 'disposed' }
    const context = this.captureContext()
    const commit = await command(context)
    if (this.disposed) return { status: 'disposed' }
    if (!this.contextIsCurrent(context)) return { status: 'stale' }
    const result = await commit()
    if (this.disposed) return { status: 'disposed' }
    if (!this.contextIsCurrent(context)) return { status: 'stale' }
    return { status: 'completed', result }
  }

  async annotate(command: AnnotationCommand): Promise<AnnotationCommandOutcome> {
    const execution = await this.executeCommand(command)
    if (execution.status !== 'completed') return execution
    return execution.result.ok
      ? { status: 'committed', sequence: execution.result.sequence }
      : { status: 'failed' }
  }

  async requestTranslation(command: TranslationCommand): Promise<TranslationCommandOutcome> {
    const execution = await this.executeCommand(command)
    if (execution.status !== 'completed') return execution
    return execution.result.ok ? { status: 'committed' } : { status: 'failed' }
  }

  dispose(): void {
    if (this.disposed) return
    this.disposed = true
    this.bodyRequestSequence += 1
    this.identityOperation.signal.removeEventListener('abort', this.onIdentityAbort)
    this.generationController.abort()
    this.listeners.clear()
  }
}
