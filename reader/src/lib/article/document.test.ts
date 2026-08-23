import { describe, expect, it, vi } from 'vitest'

import type { Annotation } from '../annotation-domain'
import { err, ok, type ApiResult } from '@webtag/api'
import type {
  LinkContentResponse,
  LinkResponse,
  TranslationListResponse,
  TranslationResponse,
} from '../api/types'
import { IdentityLease } from '../identity'
import {
  SavedArticleDocumentController,
  reanchorSavedContentAnnotations,
  type AnnotationCommand,
  type SavedArticleDocument,
} from './document'

const DETAIL = {
  id: 'L1',
  content_revision: 7,
  title: 'Article',
} as LinkResponse

function lease(namespace = 'physical-A', localEpoch = 1): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `server:${namespace}`,
    physicalNamespace: namespace,
    localEpoch,
  })
}

function createController(
  initialDetail: LinkResponse = DETAIL,
  identityLease: IdentityLease = lease(),
): SavedArticleDocumentController {
  return new SavedArticleDocumentController({ lease: identityLease, detail: initialDetail })
}

function detail(revision: number, overrides: Partial<LinkResponse> = {}): LinkResponse {
  return {
    ...DETAIL,
    content_revision: revision,
    ...overrides,
  }
}

function body(revision: number, content: string, linkId = 'L1'): LinkContentResponse {
  return {
    link_id: linkId,
    content,
    content_format: 'plain',
    content_revision: revision,
    fetcher_type: 'stored',
    content_source: 'fetched',
  }
}

function annotation(overrides: Partial<Annotation> = {}): Annotation {
  return {
    id: 'A1',
    blockKey: 'content',
    start: 0,
    end: 4,
    text: 'text',
    note: '',
    source: 'self',
    createdAt: 1,
    updatedAt: 1,
    sourceContentRevision: 7,
    ...overrides,
  }
}

function quotedAnnotation(
  source: string,
  start: number,
  end: number,
  overrides: Partial<Annotation> = {},
): Annotation {
  return annotation({
    start,
    end,
    text: source.slice(start, end),
    quote: {
      exact: source.slice(start, end),
      prefix: source.slice(Math.max(0, start - 8), start),
      suffix: source.slice(end, Math.min(source.length, end + 8)),
    },
    ...overrides,
  })
}

function translation(overrides: Partial<TranslationResponse> = {}): TranslationResponse {
  return {
    id: 'T7',
    link_id: 'L1',
    scope: 'selection',
    block_key: 'content',
    start_offset: 0,
    end_offset: 4,
    source_text: 'text',
    translated_text: 'translated',
    source_format: 'plain',
    target_language: 'zh-CN',
    source_content_revision: 7,
    status: 'done',
    model: 'test',
    error_msg: null,
    stale: false,
    created_at: '2026-08-01T00:00:00Z',
    updated_at: '2026-08-01T00:00:00Z',
    ...overrides,
  }
}

function assertCoherent(snapshot: SavedArticleDocument): void {
  if (snapshot.detail.status !== 'idle') {
    expect(snapshot.detail.revision).toBe(snapshot.id.contentRevision)
  }
  if (snapshot.body.status !== 'idle') {
    expect(snapshot.body.revision).toBe(snapshot.id.contentRevision)
  }
  if (snapshot.annotations.status !== 'idle') {
    expect(snapshot.annotations.revision).toBe(snapshot.id.contentRevision)
  }
  if (snapshot.translations.status !== 'idle') {
    expect(snapshot.translations.revision).toBe(snapshot.id.contentRevision)
  }
  if (snapshot.annotations.status === 'ready') {
    expect(snapshot.annotations.data.every(
      (item) => item.blockKey.startsWith('content') &&
        item.sourceContentRevision === snapshot.id.contentRevision,
    )).toBe(true)
  }
  if (snapshot.translations.status === 'ready') {
    expect(snapshot.translations.data.every(
      (item) => item.link_id === snapshot.id.linkId &&
        item.source_content_revision === snapshot.id.contentRevision &&
        (item.block_key === 'content' || item.block_key === 'content-document') &&
        !item.stale,
    )).toBe(true)
  }
}

describe('SavedArticleDocumentController', () => {
  it('reanchors a unique saved-content quote into the next revision', () => {
    const previousSource = 'Intro: durable phrase stays here.'
    const currentSource = 'Intro: durable phrase stays here. An appended sentence.'
    const start = previousSource.indexOf('durable phrase')
    const [result] = reanchorSavedContentAnnotations(
      [quotedAnnotation(previousSource, start, start + 'durable phrase'.length)],
      7,
      8,
      { content: previousSource },
      { content: currentSource },
      'L1',
    )

    expect(result).toMatchObject({
      status: 'reanchored',
      reason: 'unique-quote',
      fromRevision: 7,
      toRevision: 8,
      annotation: {
        sourceContentRevision: 8,
        text: 'durable phrase',
      },
    })
    if (result?.status === 'reanchored') {
      expect(currentSource.slice(result.annotation.start, result.annotation.end)).toBe('durable phrase')
      expect(result.annotation.quote?.exact).toBe('durable phrase')
    }
  })

  it('keeps ambiguous and unquotable saved-content annotations typed as historical', () => {
    const previousSource = 'same phrase appears once; same phrase appears twice'
    const ambiguousStart = previousSource.indexOf('same phrase')
    const results = reanchorSavedContentAnnotations(
      [
        quotedAnnotation(previousSource, ambiguousStart, ambiguousStart + 'same phrase'.length, {
          quote: { exact: 'same phrase', prefix: '', suffix: '' },
        }),
        annotation({ id: 'missing-quote', text: 'legacy text', start: 0, end: 6 }),
      ],
      7,
      8,
      { content: previousSource },
      { content: previousSource },
      'L1',
    )

    expect(results).toEqual([
      expect.objectContaining({
        status: 'historical',
        reason: 'ambiguous-quote',
        annotation: expect.objectContaining({ id: 'A1', sourceContentRevision: 7 }),
      }),
      expect.objectContaining({
        status: 'historical',
        reason: 'missing-quote',
        annotation: expect.objectContaining({ id: 'missing-quote', sourceContentRevision: 7 }),
      }),
    ])
    expect(results.every((item) => item.status === 'historical')).toBe(true)
  })

  it.each(['', 'L\0other'])(
    'rejects a non-canonical initial link identity %#',
    (linkId) => {
      expect(() => createController(detail(7, { id: linkId }))).toThrow(/identity/i)
    },
  )

  it('publishes one coherent atomic transition when revision advances', () => {
    const controller = createController()
    controller.acceptBody(body(7, 'rev seven'), controller.captureContext())
    controller.acceptAnnotations(7, [annotation()], controller.captureContext())
    controller.acceptTranslations({
      current_content_revision: 7,
      current_summary_source_hash: null,
      items: [translation()],
    }, controller.captureContext())
    const oldContext = controller.captureContext()
    expect(Object.isFrozen(oldContext)).toBe(true)
    expect(Object.isFrozen(oldContext.id)).toBe(true)
    const abortListener = vi.fn()
    oldContext.signal.addEventListener('abort', abortListener)
    const observed: SavedArticleDocument[] = []
    const unsubscribe = controller.subscribe(() => {
      const snapshot = controller.getSnapshot()
      assertCoherent(snapshot)
      observed.push(snapshot)
    })

    controller.acceptDetail(detail(8), controller.captureContext())
    unsubscribe()

    expect(abortListener).toHaveBeenCalledOnce()
    expect(oldContext.signal.aborted).toBe(true)
    expect(observed).toHaveLength(1)
    expect(observed[0]).toMatchObject({
      id: { namespace: 'physical-A', linkId: 'L1', contentRevision: 8 },
      generation: 2,
      detail: { status: 'ready', revision: 8 },
      body: { status: 'idle' },
      annotations: { status: 'idle' },
      translations: { status: 'idle' },
      historicalAnnotations: [{
        status: 'historical',
        reason: 'revision-changed',
        sourceContentRevision: 7,
        annotation: { id: 'A1' },
      }],
    })
  })

  it('accepts inline, cached, and endpoint bodies only with their actual revision', async () => {
    const controller = createController(
      detail(7, {
        content: 'inline seven',
        content_document: '# inline seven',
        content_format: 'markdown',
      }),
    )
    expect(controller.getSnapshot().body).toMatchObject({
      status: 'ready',
      revision: 7,
      data: { content: 'inline seven', content_document: '# inline seven' },
    })

    expect(controller.acceptBody(body(7, 'cached seven'), controller.captureContext())).toBe(true)
    expect(controller.getSnapshot().body).toMatchObject({
      status: 'ready',
      revision: 7,
      data: { content: 'cached seven' },
    })

    await controller.loadBody(async (context) => {
      expect(context.id.contentRevision).toBe(7)
      return ok(body(8, 'endpoint eight'))
    })
    expect(controller.getSnapshot()).toMatchObject({
      id: { contentRevision: 8 },
      generation: 2,
      body: { status: 'ready', revision: 8, data: { content: 'endpoint eight' } },
    })
    expect(controller.acceptBody(body(7, 'late seven'), controller.captureContext())).toBe(false)
    expect(controller.getSnapshot().body).toMatchObject({
      status: 'ready',
      revision: 8,
      data: { content: 'endpoint eight' },
    })
  })

  it('adopts a higher response revision and ignores a lower late body', async () => {
    const controller = createController()
    let resolveSeven!: (value: ApiResult<LinkContentResponse>) => void
    const seven = new Promise<ApiResult<LinkContentResponse>>((resolve) => {
      resolveSeven = resolve
    })
    const first = controller.loadBody(() => seven)

    await controller.loadBody(async () => ok(body(8, 'rev eight')))
    resolveSeven(ok(body(7, 'rev seven late')))
    await first

    const snapshot = controller.getSnapshot()
    assertCoherent(snapshot)
    expect(snapshot.id.contentRevision).toBe(8)
    expect(snapshot.body).toMatchObject({
      status: 'ready',
      revision: 8,
      data: { content: 'rev eight' },
    })
  })

  it('settles a lower current body response as an error instead of retryable idle', async () => {
    const controller = createController(detail(9))

    await controller.loadBody(async () => ok(body(7, 'stale revision seven')))

    expect(controller.getSnapshot()).toMatchObject({
      id: { contentRevision: 9 },
      body: {
        status: 'error',
        revision: 9,
        error: {
          kind: 'other',
          message: '已保存原文响应不属于当前文档版本',
        },
      },
    })
  })

  it('still adopts an authoritative higher revision from an older request', async () => {
    const controller = createController()
    let resolveFirst!: (value: ApiResult<LinkContentResponse>) => void
    const firstResponse = new Promise<ApiResult<LinkContentResponse>>((resolve) => {
      resolveFirst = resolve
    })
    const first = controller.loadBody(() => firstResponse)

    await controller.loadBody(async () => ok(body(8, 'revision eight')))
    resolveFirst(ok(body(9, 'revision nine')))
    await first

    expect(controller.getSnapshot()).toMatchObject({
      id: { contentRevision: 9 },
      generation: 3,
      body: { status: 'ready', revision: 9, data: { content: 'revision nine' } },
    })
  })

  it('does not let an older same-generation failure replace a newer body', async () => {
    const controller = createController()
    let resolveOld!: (value: ApiResult<LinkContentResponse>) => void
    const oldRequest = new Promise<ApiResult<LinkContentResponse>>((resolve) => {
      resolveOld = resolve
    })
    const pending = controller.loadBody(() => oldRequest)

    await controller.loadBody(async () => ok(body(7, 'newest success')))
    resolveOld(err({ kind: 'network-unreachable', message: 'late failure' }))
    await pending

    expect(controller.getSnapshot().body).toMatchObject({
      status: 'ready',
      revision: 7,
      data: { content: 'newest success' },
    })
  })

  it('records a current body load error without disturbing other resources', async () => {
    const controller = createController()
    const failure = { kind: 'network-unreachable', message: 'offline' } as const
    controller.acceptAnnotations(7, [annotation()], controller.captureContext())

    await controller.loadBody(async () => err(failure))

    expect(controller.getSnapshot()).toMatchObject({
      detail: { status: 'ready', revision: 7 },
      body: { status: 'error', revision: 7, error: failure },
      annotations: { status: 'ready', revision: 7 },
      translations: { status: 'idle' },
    })
  })

  it('does not let an older same-revision success replace a newer body', async () => {
    const controller = createController()
    let resolveOld!: (value: ApiResult<LinkContentResponse>) => void
    const oldRequest = new Promise<ApiResult<LinkContentResponse>>((resolve) => {
      resolveOld = resolve
    })
    const pending = controller.loadBody(() => oldRequest)

    await controller.loadBody(async () => ok(body(7, 'newest success')))
    resolveOld(ok(body(7, 'older success')))
    await pending

    expect(controller.getSnapshot().body).toMatchObject({
      status: 'ready',
      revision: 7,
      data: { content: 'newest success' },
    })
  })

  it('does not let a pending endpoint failure replace an accepted cache body', async () => {
    const controller = createController()
    let resolveEndpoint!: (value: ApiResult<LinkContentResponse>) => void
    const endpoint = new Promise<ApiResult<LinkContentResponse>>((resolve) => {
      resolveEndpoint = resolve
    })
    const pending = controller.loadBody(() => endpoint)

    controller.acceptBody(body(7, 'cache won'), controller.captureContext())
    resolveEndpoint(err({ kind: 'network-unreachable', message: 'late endpoint failure' }))
    await pending

    expect(controller.getSnapshot().body).toMatchObject({
      status: 'ready',
      revision: 7,
      data: { content: 'cache won' },
    })
  })

  it('filters annotations and translations to the exact saved document identity', () => {
    const controller = createController()
    controller.acceptAnnotations(7, [
      annotation({ id: 'current-content' }),
      annotation({ id: 'current-document', blockKey: 'content-document' }),
      annotation({ id: 'old-content', sourceContentRevision: 6 }),
      annotation({ id: 'summary', blockKey: 'summary', sourceSummaryHash: 'a'.repeat(64) }),
    ], controller.captureContext())
    controller.acceptTranslations({
      current_content_revision: 7,
      current_summary_source_hash: null,
      items: [
        translation({ id: 'content' }),
        translation({ id: 'document', block_key: 'content-document' }),
        translation({ id: 'old', source_content_revision: 6 }),
        translation({ id: 'legacy', source_content_revision: null }),
        translation({ id: 'stale', stale: true }),
        translation({ id: 'other-link', link_id: 'L2' }),
        translation({ id: 'summary', block_key: 'summary' }),
        translation({ id: 'retired-dr', block_key: 'dr' }),
      ],
    } as TranslationListResponse, controller.captureContext())

    const snapshot = controller.getSnapshot()
    assertCoherent(snapshot)
    expect(snapshot.annotations.status === 'ready' && snapshot.annotations.data.map((item) => item.id))
      .toEqual(['current-content', 'current-document'])
    expect(snapshot.translations.status === 'ready' && snapshot.translations.data.map((item) => item.id))
      .toEqual(['content', 'document'])
  })

  it('never treats revision zero as a saved-content source identity', () => {
    const controller = createController(detail(0))

    controller.acceptAnnotations(
      0,
      [annotation({ sourceContentRevision: 0 })],
      controller.captureContext(),
    )
    controller.acceptTranslations({
      current_content_revision: 0,
      current_summary_source_hash: null,
      items: [translation({ source_content_revision: 0 })],
    }, controller.captureContext())

    expect(controller.getSnapshot().annotations).toMatchObject({ status: 'ready', data: [] })
    expect(controller.getSnapshot().translations).toMatchObject({ status: 'ready', data: [] })
    expect(controller.acceptBody(body(0, 'invalid zero body'), controller.captureContext())).toBe(false)
  })

  it('rejects a late translation response captured by an old generation', () => {
    const controller = createController()
    const oldContext = controller.captureContext()
    controller.acceptDetail(detail(8), controller.captureContext())
    const response = {
      current_content_revision: 8,
      current_summary_source_hash: null,
      items: [translation({ id: 'T8', source_content_revision: 8 })],
    }

    expect(controller.acceptTranslations(response, oldContext)).toBe(false)
    expect(controller.getSnapshot().translations.status).toBe('idle')
    expect(controller.acceptTranslations(response, controller.captureContext())).toBe(true)
    expect(controller.getSnapshot().translations).toMatchObject({
      status: 'ready',
      revision: 8,
      data: [{ id: 'T8' }],
    })
  })

  it('tracks annotation and translation loading failures independently', () => {
    const controller = createController()
    const context = controller.captureContext()
    const detailError = { kind: 'other', message: 'detail read failed' } as const
    const annotationError = { kind: 'other', message: 'annotation read failed' } as const
    const translationError = { kind: 'network-unreachable', message: 'translation read failed' } as const

    expect(controller.beginAnnotationsLoad(context)).toBe(true)
    expect(controller.beginTranslationsLoad(context)).toBe(true)
    expect(controller.getSnapshot()).toMatchObject({
      detail: { status: 'ready' },
      body: { status: 'idle' },
      annotations: { status: 'loading', revision: 7 },
      translations: { status: 'loading', revision: 7 },
    })

    expect(controller.failAnnotations(annotationError, context)).toBe(true)
    expect(controller.getSnapshot()).toMatchObject({
      annotations: { status: 'error', revision: 7, error: annotationError },
      translations: { status: 'loading', revision: 7 },
    })
    expect(controller.failTranslations(translationError, context)).toBe(true)
    expect(controller.getSnapshot()).toMatchObject({
      annotations: { status: 'error', revision: 7 },
      translations: { status: 'error', revision: 7, error: translationError },
    })
    expect(controller.beginDetailLoad(context)).toBe(true)
    expect(controller.failDetail(detailError, context)).toBe(true)
    expect(controller.getSnapshot()).toMatchObject({
      detail: { status: 'error', revision: 7, error: detailError },
      annotations: { status: 'error', revision: 7 },
      translations: { status: 'error', revision: 7 },
    })
  })

  it('never notifies a later subscriber with an older snapshot after reentry', () => {
    const controller = createController()
    const observed: number[] = []
    controller.subscribe(() => {
      if (controller.getSnapshot().id.contentRevision === 8) {
        controller.acceptDetail(detail(9), controller.captureContext())
      }
    })
    controller.subscribe(() => {
      observed.push(controller.getSnapshot().id.contentRevision)
    })

    controller.acceptDetail(detail(8), controller.captureContext())

    expect(observed).toEqual([9, 9])
  })

  it('rejects an annotation command if revision changes before durable commit', async () => {
    const controller = createController()
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    let durableCalls = 0
    const command: AnnotationCommand = async (context) => {
      expect(context.id).toEqual({
        namespace: 'physical-A',
        linkId: 'L1',
        contentRevision: 7,
      })
      expect(context.generation).toBe(1)
      await gate
      return async () => {
        durableCalls += 1
        return { ok: true, sequence: 1 }
      }
    }

    const pending = controller.annotate(command)
    controller.acceptDetail(detail(8), controller.captureContext())
    release()

    await expect(pending).resolves.toEqual({ status: 'stale' })
    expect(durableCalls).toBe(0)
    expect(controller.getSnapshot().id.contentRevision).toBe(8)
  })

  it('uses the same document fence for translation commands', async () => {
    const controller = createController()
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    const scheduled = vi.fn()
    const pending = controller.requestTranslation(async (context) => {
      await gate
      return async () => {
        scheduled(context.id.contentRevision)
        return { ok: true }
      }
    })

    controller.acceptBody(body(8, 'new revision'), controller.captureContext())
    release()

    await expect(pending).resolves.toEqual({ status: 'stale' })
    expect(scheduled).not.toHaveBeenCalled()
  })

  it('keeps a command current across a same-revision detail refresh', async () => {
    const controller = createController()
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    const pending = controller.annotate(async () => {
      await gate
      return async () => ({ ok: true, sequence: 42 })
    })

    controller.acceptDetail(
      detail(7, { title: 'Refreshed title' }),
      controller.captureContext(),
    )
    release()

    await expect(pending).resolves.toEqual({ status: 'committed', sequence: 42 })
    expect(controller.getSnapshot().generation).toBe(1)
  })

  it('disposes pending continuations', async () => {
    const controller = createController()
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    const pending = controller.annotate(async () => {
      await gate
      return async () => ({ ok: true, sequence: 1 })
    })
    controller.dispose()
    release()

    await expect(pending).resolves.toEqual({ status: 'disposed' })
  })

  it('automatically disposes and fences commands when the identity lease is revoked', async () => {
    const owner = lease('physical-A', 4)
    const controller = createController(DETAIL, owner)
    const context = controller.captureContext()
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    const durableCommit = vi.fn(async () => ({ ok: true as const, sequence: 1 }))
    const pending = controller.annotate(async () => {
      await gate
      return durableCommit
    })

    owner.revoke()
    release()

    await expect(pending).resolves.toEqual({ status: 'disposed' })
    expect(context.signal.aborted).toBe(true)
    expect(durableCommit).not.toHaveBeenCalled()
    expect(controller.acceptDetail(detail(8), context)).toBe(false)
  })
})
