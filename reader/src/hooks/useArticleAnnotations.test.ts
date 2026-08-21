import 'fake-indexeddb/auto'

import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Annotation, AnnotationInput } from '../lib/annotations'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import { SavedArticleDocumentController } from '../lib/article/document'
import type { SourceBlockId } from '../lib/article/source-block'
import type { LinkResponse } from '../lib/api/types'
import { resourceStore } from '../lib/cache/store'
import { IdentityLease } from '../lib/identity'
import { ownedDatabaseName, ownedStorageKeyForLease } from '../lib/storage-ownership'
import {
  commitAnnotationOperation,
  listAnnotatedLinks,
  readAnnotationSnapshot,
} from '../lib/user-data/annotation-store'
import { resetUserDataDatabaseHandle } from '../lib/user-data/idb'
import { listAnnotationOperationsForTest } from '../test/annotation-operations'
import { useAnnotatedLinkCount } from './useAnnotatedLinks'
import { useArticleAnnotations } from './useArticleAnnotations'

const HASH = 'a'.repeat(64)
const HASH_B = 'b'.repeat(64)
const activeLeases: IdentityLease[] = []

function lease(): IdentityLease {
  const owner = new IdentityLease({
    serverClientDataNamespace: 'server-A',
    physicalNamespace: 'physical-A',
    localEpoch: 1,
  })
  activeLeases.push(owner)
  return owner
}

function detail(revision = 7): LinkResponse {
  return {
    id: 'L1',
    title: 'Article',
    status: 'done',
    library_kind: 'reading',
    content_revision: revision,
  } as LinkResponse
}

function summary(): SourceBlockId {
  return {
    namespace: 'physical-A',
    linkId: 'L1',
    blockKind: 'summary',
    sourceHash: HASH,
  }
}

const BODY_INPUT: AnnotationInput = {
  blockKey: 'content-document',
  start: 0,
  end: 4,
  text: 'body',
}

const SUMMARY_INPUT: AnnotationInput = {
  blockKey: 'summary',
  start: 0,
  end: 7,
  text: 'summary',
}

const invalidAddCases: readonly {
  readonly name: string
  readonly documentId: Parameters<typeof useArticleAnnotations>[1]
  readonly sourceBlock: Parameters<typeof useArticleAnnotations>[2]
  readonly input: AnnotationInput
  readonly expectedStatus: 'stale' | 'unsupported'
}[] = [
  {
    name: 'empty link ID',
    documentId: { namespace: 'physical-A', linkId: '', contentRevision: 7 },
    sourceBlock: null,
    input: BODY_INPUT,
    expectedStatus: 'stale',
  },
  ...[0, -1, 1.5, Number.NaN].map((contentRevision) => ({
    name: `invalid saved-content revision ${String(contentRevision)}`,
    documentId: { namespace: 'physical-A', linkId: 'L1', contentRevision },
    sourceBlock: null,
    input: BODY_INPUT,
    expectedStatus: 'unsupported' as const,
  })),
  ...[
    { name: 'null summary hash', sourceHash: null },
    { name: 'undefined summary hash', sourceHash: undefined },
    { name: 'invalid summary hash', sourceHash: 'not-a-sha256' },
  ].map(({ name, sourceHash }) => ({
    name,
    documentId: null,
    sourceBlock: { ...summary(), sourceHash } as unknown as SourceBlockId,
    input: SUMMARY_INPUT,
    expectedStatus: 'unsupported' as const,
  })),
]

async function dispatchAnnotationHint(
  owner: IdentityLease,
  input: {
    readonly linkId?: string
    readonly documentRevision: number
    readonly annotationStoreVersion: number
  },
): Promise<void> {
  const key = ownedStorageKeyForLease('annotationWakeup', owner)
  if (!key) throw new Error('annotation wakeup storage key is unavailable')
  await act(async () => {
    window.dispatchEvent(new StorageEvent('storage', {
      key,
      newValue: JSON.stringify({
        kind: 'annotation-change',
        namespace: owner.context.physicalNamespace,
        linkId: input.linkId ?? 'L1',
        documentRevision: input.documentRevision,
        annotationStoreVersion: input.annotationStoreVersion,
      }),
    }))
    await new Promise<void>((resolve) => setTimeout(resolve, 0))
  })
}

async function deleteDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('database delete failed'))
    request.onblocked = () => reject(new Error('database delete was blocked'))
  })
}

afterEach(async () => {
  resourceStore.deactivateIdentity()
  for (const owner of activeLeases.splice(0)) owner.revoke()
  vi.unstubAllGlobals()
  await deleteDatabase()
})

describe('useArticleAnnotations', () => {
  it('reanchors a unique saved-content annotation while retaining the old target', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const previousSource = 'Intro: durable phrase stays here.'
    const currentSource = 'Intro: durable phrase stays here. Appended sentence.'
    const start = previousSource.indexOf('durable phrase')
    const committed = await commitAnnotationOperation(owner, {
      kind: 'add',
      opId: 'reanchor-unique-seed',
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      draft: {
        id: 'reanchor-unique',
        blockKey: 'content',
        start,
        end: start + 'durable phrase'.length,
        text: 'durable phrase',
        note: 'keep this thought',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
        quote: {
          exact: 'durable phrase',
          prefix: previousSource.slice(Math.max(0, start - 8), start),
          suffix: previousSource.slice(start + 'durable phrase'.length, start + 'durable phrase'.length + 8),
        },
      },
    })
    expect(committed.ok).toBe(true)

    const { result } = renderHook(() => useArticleAnnotations(
      owner,
      { namespace: 'physical-A', linkId: 'L1', contentRevision: 8 },
      null,
      {
        revisionChange: {
          previousRevision: 7,
          previousSource: { content: previousSource },
          currentSource: { content: currentSource },
        },
      },
    ))

    await waitFor(() => expect(result.current.reanchoring).toBe(false))
    await waitFor(() => expect(result.current.anns).toEqual([
      expect.objectContaining({
        id: 'reanchor-unique',
        sourceContentRevision: 8,
        text: 'durable phrase',
        start: currentSource.indexOf('durable phrase'),
        note: 'keep this thought',
      }),
    ]))
    expect(result.current.historicalAnnotations).toEqual([])
    expect(result.current.degraded).toBe(false)
    await expect(readAnnotationSnapshot(owner, 'L1', {
      kind: 'saved-content',
      contentRevision: 7,
    })).resolves.toMatchObject({
      ok: true,
      value: { annotations: [expect.objectContaining({ id: 'reanchor-unique', sourceContentRevision: 7 })] },
    })
    await expect(readAnnotationSnapshot(owner, 'L1', {
      kind: 'saved-content',
      contentRevision: 8,
    })).resolves.toMatchObject({
      ok: true,
      value: { annotations: [expect.objectContaining({ id: 'reanchor-unique', sourceContentRevision: 8 })] },
    })
  })

  it('keeps annotations on an old saved-content revision as read-only history', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const source = 'same phrase appears once; same phrase appears twice'
    for (const draft of [
      {
        id: 'ambiguous-reanchor',
        start: 0,
        end: 'same phrase'.length,
        text: 'same phrase',
        quote: { exact: 'same phrase', prefix: '', suffix: '' },
      },
      {
        id: 'missing-reanchor',
        start: 0,
        end: 6,
        text: 'legacy',
      },
    ]) {
      const committed = await commitAnnotationOperation(owner, {
        kind: 'add',
        opId: `${draft.id}-seed`,
        linkId: 'L1',
        target: { kind: 'saved-content', contentRevision: 7 },
        draft: {
          ...draft,
          blockKey: 'content',
          note: draft.id,
          source: 'self',
          createdAt: 1,
          updatedAt: 1,
        },
      })
      expect(committed.ok).toBe(true)
    }

    const { result } = renderHook(() => useArticleAnnotations(
      owner,
      { namespace: 'physical-A', linkId: 'L1', contentRevision: 8 },
      null,
      {
        revisionChange: {
          previousRevision: 7,
          previousSource: { content: source },
          currentSource: { content: source },
        },
      },
    ))

    await waitFor(() => expect(result.current.reanchoring).toBe(false))
    expect(result.current.anns).toEqual([])
    expect(result.current.historicalAnnotations).toEqual([
      expect.objectContaining({
        status: 'historical',
        reason: 'revision-changed',
        sourceContentRevision: 7,
        annotation: expect.objectContaining({ id: 'ambiguous-reanchor' }),
      }),
      expect.objectContaining({
        status: 'historical',
        reason: 'revision-changed',
        sourceContentRevision: 7,
        annotation: expect.objectContaining({ id: 'missing-reanchor' }),
      }),
    ])
    expect(result.current.degraded).toBe(true)
    await expect(readAnnotationSnapshot(owner, 'L1', {
      kind: 'saved-content',
      contentRevision: 7,
    })).resolves.toMatchObject({
      ok: true,
      value: { annotations: [
        expect.objectContaining({ id: 'ambiguous-reanchor' }),
        expect.objectContaining({ id: 'missing-reanchor' }),
      ] },
    })
    const annotated = await listAnnotatedLinks(owner)
    expect(annotated).toMatchObject({
      ok: true,
      value: expect.arrayContaining([
        expect.objectContaining({
          target: { kind: 'saved-content', contentRevision: 7 },
        }),
      ]),
    })
  })

  it.each(invalidAddCases)(
    'fails closed without a durable operation for $name',
    async ({ documentId, sourceBlock, input, expectedStatus }) => {
      vi.stubGlobal('BroadcastChannel', undefined)
      const owner = lease()
      const { result } = renderHook(() =>
        useArticleAnnotations(owner, documentId, sourceBlock),
      )
      await waitFor(() => expect(result.current.loading).toBe(false))

      await act(async () => {
        await expect(result.current.add(input)).resolves.toEqual({
          status: expectedStatus,
        })
      })

      await expect(listAnnotatedLinks(owner)).resolves.toEqual({ ok: true, value: [] })
    },
  )

  it('durably combines independent saved-content and summary targets', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const controller = new SavedArticleDocumentController({
      lease: owner,
      detail: detail(),
    })
    const { result, unmount } = renderHook(() =>
      useArticleAnnotations(owner, controller.getSnapshot().id, summary()),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))

    let contentId = ''
    let summaryId = ''
    await act(async () => {
      const content = await result.current.add({
        blockKey: 'content-document',
        start: 0,
        end: 4,
        text: 'body',
      }, { documentContext: controller.captureContext() })
      const summaryResult = await result.current.add({
        blockKey: 'summary',
        start: 1,
        end: 5,
        text: 'intro',
        note: 'seed note',
        source: 'ai',
      })
      if (!('annotationId' in content) || !('annotationId' in summaryResult)) {
        throw new Error('both annotation writes must commit')
      }
      contentId = content.annotationId
      summaryId = summaryResult.annotationId
    })

    expect(result.current.anns).toEqual([
      expect.objectContaining({
        id: contentId,
        blockKey: 'content-document',
        note: '',
        source: 'self',
        sourceContentRevision: 7,
      }),
      expect.objectContaining({
        id: summaryId,
        blockKey: 'summary',
        note: 'seed note',
        source: 'ai',
        sourceSummaryHash: HASH,
      }),
    ])
    const contentAnnotation = result.current.anns.find((item) => item.id === contentId)
    const summaryAnnotation = result.current.anns.find((item) => item.id === summaryId)
    if (!contentAnnotation || !summaryAnnotation) {
      throw new Error('both durable annotations must be materialized')
    }

    await act(async () => {
      await result.current.update(
        summaryAnnotation,
        { note: 'durable note' },
      )
      await result.current.remove(
        contentAnnotation,
        { documentContext: controller.captureContext() },
      )
    })
    expect(result.current.anns).toEqual([
      expect.objectContaining({ id: summaryId, note: 'durable note' }),
    ])

    unmount()
    const reopened = renderHook(() =>
      useArticleAnnotations(owner, controller.getSnapshot().id, summary()),
    )
    await waitFor(() => expect(reopened.result.current.loading).toBe(false))
    expect(reopened.result.current.anns).toEqual([
      expect.objectContaining({ id: summaryId, note: 'durable note' }),
    ])
    reopened.unmount()
    controller.dispose()
  })

  it('persists selection quote context for future saved-content reanchoring', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const controller = new SavedArticleDocumentController({
      lease: owner,
      detail: detail(),
    })
    const { result } = renderHook(() =>
      useArticleAnnotations(owner, controller.getSnapshot().id, null),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))

    const quote = {
      exact: 'body',
      prefix: 'before ',
      suffix: ' after',
    }
    let annotationId = ''
    await act(async () => {
      const outcome = await result.current.add({
        ...BODY_INPUT,
        quote,
      }, { documentContext: controller.captureContext() })
      if (!('annotationId' in outcome)) throw new Error('annotation add must commit')
      annotationId = outcome.annotationId
    })

    expect(result.current.anns).toEqual([
      expect.objectContaining({ id: annotationId, quote }),
    ])
  })

  it('settles an invalid revision-change request as stale instead of leaving reanchoring active', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const { result } = renderHook(() => useArticleAnnotations(
      owner,
      { namespace: 'physical-A', linkId: 'L1', contentRevision: 7 },
      null,
      {
        revisionChange: {
          previousRevision: 7,
          previousSource: 'old body',
          currentSource: 'current body',
        },
      },
    ))

    await waitFor(() => expect(result.current.reanchoring).toBe(false))
    await expect(result.current.reanchor({
      previousRevision: 7,
      previousSource: 'old body',
      currentSource: 'current body',
    })).resolves.toMatchObject({ status: 'stale' })
    expect(result.current.reanchoring).toBe(false)
  })

  it('updates the same-page annotated count when BroadcastChannel is unavailable', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const controller = new SavedArticleDocumentController({
      lease: owner,
      detail: detail(),
    })
    resourceStore.activateIdentity(owner)
    const client = {
      identityLease: owner,
      isIdentityCurrent: () => true,
      getLink: async () => ({ ok: true as const, data: detail() }),
    } as unknown as IdentityBoundReaderClient
    const { result } = renderHook(() => ({
      annotations: useArticleAnnotations(owner, controller.getSnapshot().id, null),
      count: useAnnotatedLinkCount(client),
    }))
    await waitFor(() => expect(result.current.annotations.loading).toBe(false))
    expect(result.current.count).toBe(0)

    let annotationId = ''
    await act(async () => {
      const outcome = await result.current.annotations.add({
        blockKey: 'content',
        start: 0,
        end: 4,
        text: 'body',
      }, { documentContext: controller.captureContext() })
      if (!('annotationId' in outcome)) throw new Error('annotation add must commit')
      annotationId = outcome.annotationId
    })
    await waitFor(() => expect(result.current.count).toBe(1))

    const annotation = result.current.annotations.anns.find((item) => item.id === annotationId)
    if (!annotation) throw new Error('committed annotation must be visible')
    await act(async () => {
      const outcome = await result.current.annotations.remove(
        annotation,
        { documentContext: controller.captureContext() },
      )
      expect(outcome.status).toBe('committed')
    })
    await waitFor(() => expect(result.current.count).toBe(0))
    controller.dispose()
  })

  it('aborts a saved-content operation when its document revision advances', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const controller = new SavedArticleDocumentController({
      lease: owner,
      detail: detail(),
    })
    const { result } = renderHook(() =>
      useArticleAnnotations(owner, controller.getSnapshot().id, null),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))
    const context = controller.captureContext()

    let pending!: ReturnType<typeof result.current.add>
    act(() => {
      pending = result.current.add({
        blockKey: 'content',
        start: 0,
        end: 4,
        text: 'old body',
      }, { documentContext: context })
      controller.acceptDetail(detail(8), context)
    })

    await expect(pending).resolves.toEqual({ status: 'stale' })
    await expect(readAnnotationSnapshot(owner, 'L1', {
      kind: 'saved-content',
      contentRevision: 7,
    })).resolves.toMatchObject({ ok: true, value: { annotations: [] } })
    controller.dispose()
  })

  it('keeps a summary operation alive when only saved-content revision advances', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const controller = new SavedArticleDocumentController({
      lease: owner,
      detail: detail(),
    })
    let documentId = controller.getSnapshot().id
    const { result, rerender } = renderHook(() =>
      useArticleAnnotations(owner, documentId, summary()),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))
    const context = controller.captureContext()

    let outcome: Awaited<ReturnType<typeof result.current.add>> | undefined
    await act(async () => {
      const pending = result.current.add({
        blockKey: 'summary',
        start: 0,
        end: 7,
        text: 'summary',
      })
      controller.acceptDetail(detail(8), context)
      documentId = controller.getSnapshot().id
      rerender()
      outcome = await pending
    })
    expect(outcome).toMatchObject({ status: 'committed' })
    await waitFor(() => {
      expect(result.current.loading).toBe(false)
      expect(result.current.anns).toEqual([
        expect.objectContaining({ blockKey: 'summary', text: 'summary' }),
      ])
    })
    await expect(readAnnotationSnapshot(owner, 'L1', {
      kind: 'summary',
      sourceHash: HASH,
    })).resolves.toMatchObject({
      ok: true,
      value: { annotations: [expect.objectContaining({ text: 'summary' })] },
    })
    controller.dispose()
  })

  it('synchronously hides annotations when the summary source identity changes', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    await commitAnnotationOperation(owner, {
      kind: 'add',
      opId: 'summary-a',
      linkId: 'L1',
      target: { kind: 'summary', sourceHash: HASH },
      draft: {
        id: 'summary-a',
        start: 0,
        end: 7,
        text: 'summary',
        note: '',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
      },
    })
    let sourceBlock = summary()
    let captureFrames = false
    const frames: (readonly Annotation[])[] = []
    const { result, rerender } = renderHook(() => {
      const annotations = useArticleAnnotations(owner, null, sourceBlock)
      if (captureFrames) frames.push(annotations.anns)
      return annotations
    })
    await waitFor(() => expect(result.current.anns).toEqual([
      expect.objectContaining({ id: 'summary-a', sourceSummaryHash: HASH }),
    ]))

    sourceBlock = { ...summary(), sourceHash: HASH_B }
    captureFrames = true
    rerender()
    captureFrames = false

    expect(frames[0]).toEqual([])
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.anns).toEqual([])
  })

  it('synchronously hides summary annotations when its source hash becomes null', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    await commitAnnotationOperation(owner, {
      kind: 'add',
      opId: 'summary-before-null',
      linkId: 'L1',
      target: { kind: 'summary', sourceHash: HASH },
      draft: {
        id: 'summary-before-null',
        start: 0,
        end: 7,
        text: 'summary',
        note: '',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
      },
    })
    let sourceBlock: SourceBlockId | null = summary()
    let captureFrames = false
    const frames: (readonly Annotation[])[] = []
    const { result, rerender } = renderHook(() => {
      const annotations = useArticleAnnotations(owner, null, sourceBlock)
      if (captureFrames) frames.push(annotations.anns)
      return annotations
    })
    await waitFor(() => expect(result.current.anns).toEqual([
      expect.objectContaining({ id: 'summary-before-null', sourceSummaryHash: HASH }),
    ]))

    sourceBlock = null
    captureFrames = true
    rerender()
    captureFrames = false

    expect(frames[0]).toEqual([])
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.anns).toEqual([])
  })

  it('persists only note and source from a runtime-forged update patch', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const controller = new SavedArticleDocumentController({
      lease: owner,
      detail: detail(),
    })
    const { result, unmount } = renderHook(() =>
      useArticleAnnotations(owner, controller.getSnapshot().id, null),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))

    let annotationId = ''
    await act(async () => {
      const outcome = await result.current.add(BODY_INPUT, {
        documentContext: controller.captureContext(),
      })
      if (!('annotationId' in outcome)) throw new Error('annotation add must commit')
      annotationId = outcome.annotationId
    })
    const before = result.current.anns.find((item) => item.id === annotationId)
    if (!before) throw new Error('committed annotation must be visible')

    const forgedPatch = {
      note: 'allowed note',
      source: 'ai',
      blockKey: 'summary',
      start: 99,
      end: 100,
      text: 'forged text',
      sourceContentRevision: 999,
      sourceSummaryHash: HASH_B,
    } as unknown as Parameters<typeof result.current.update>[1]
    await act(async () => {
      await expect(result.current.update(before, forgedPatch, {
        documentContext: controller.captureContext(),
      })).resolves.toMatchObject({ status: 'committed' })
    })
    await waitFor(() => expect(result.current.anns).toEqual([
      expect.objectContaining({
        id: annotationId,
        blockKey: 'content-document',
        note: 'allowed note',
        source: 'ai',
        sourceContentRevision: 7,
      }),
    ]))
    unmount()

    await expect(readAnnotationSnapshot(owner, 'L1', {
      kind: 'saved-content',
      contentRevision: 7,
    })).resolves.toMatchObject({
      ok: true,
      value: {
        annotations: [{
          id: annotationId,
          blockKey: 'content-document',
          start: 0,
          end: 4,
          text: 'body',
          note: 'allowed note',
          source: 'ai',
          sourceContentRevision: 7,
        }],
      },
    })
    await expect(listAnnotationOperationsForTest(owner, 'L1', {
      kind: 'saved-content',
      contentRevision: 7,
    })).resolves.toMatchObject({
      ok: true,
      value: [
        { kind: 'add' },
        {
          kind: 'update',
          patch: {
            note: 'allowed note',
            source: 'ai',
            updatedAt: expect.any(Number),
          },
        },
      ],
    })
    controller.dispose()
  })

  it('rejects stale source-bound references even when the current target reuses the ID', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    for (const [sourceHash, note] of [[HASH, 'source A'], [HASH_B, 'source B']] as const) {
      const committed = await commitAnnotationOperation(owner, {
        kind: 'add',
        opId: `seed-${sourceHash}`,
        linkId: 'L1',
        target: { kind: 'summary', sourceHash },
        draft: {
          id: 'shared-id',
          start: 0,
          end: 7,
          text: 'summary',
          note,
          source: 'self',
          createdAt: 1,
          updatedAt: 1,
        },
      })
      if (!committed.ok || committed.value.status === 'op-id-conflict') {
        throw new Error('same-ID summary seeds must commit to independent targets')
      }
    }
    const oldSnapshot = await readAnnotationSnapshot(owner, 'L1', {
      kind: 'summary',
      sourceHash: HASH,
    })
    if (!oldSnapshot.ok || oldSnapshot.value.annotations.length !== 1) {
      throw new Error('old summary reference must be readable')
    }
    const oldReference = oldSnapshot.value.annotations[0]
    const currentSummary = { ...summary(), sourceHash: HASH_B }
    const { result } = renderHook(() =>
      useArticleAnnotations(owner, null, currentSummary),
    )
    await waitFor(() => expect(result.current.anns).toEqual([
      expect.objectContaining({ id: 'shared-id', note: 'source B' }),
    ]))

    await act(async () => {
      await expect(result.current.update(oldReference, { note: 'cross-target edit' }))
        .resolves.toEqual({ status: 'stale' })
      await expect(result.current.remove(oldReference))
        .resolves.toEqual({ status: 'stale' })
    })

    await expect(readAnnotationSnapshot(owner, 'L1', {
      kind: 'summary',
      sourceHash: HASH_B,
    })).resolves.toMatchObject({
      ok: true,
      value: {
        annotations: [expect.objectContaining({ id: 'shared-id', note: 'source B' })],
      },
    })
  })

  it('rereads saved-content only for a newer hint at the current revision', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const documentId = {
      namespace: 'physical-A',
      linkId: 'L1',
      contentRevision: 7,
    }
    const { result } = renderHook(() =>
      useArticleAnnotations(owner, documentId, null),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))
    const committed = await commitAnnotationOperation(owner, {
      kind: 'add',
      opId: 'channel-saved',
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      draft: {
        id: 'channel-saved',
        blockKey: 'content',
        start: 0,
        end: 4,
        text: 'body',
        note: '',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
      },
    })
    if (!committed.ok || committed.value.status === 'op-id-conflict') {
      throw new Error('saved annotation seed must commit')
    }
    const transaction = vi.spyOn(IDBDatabase.prototype, 'transaction')

    await dispatchAnnotationHint(owner, {
      documentRevision: 7,
      annotationStoreVersion: 0,
    })
    expect(transaction).not.toHaveBeenCalled()
    expect(result.current.anns).toEqual([])

    await dispatchAnnotationHint(owner, {
      documentRevision: 8,
      annotationStoreVersion: committed.value.annotationStoreVersion,
    })
    expect(transaction).not.toHaveBeenCalled()
    expect(result.current.anns).toEqual([])

    await dispatchAnnotationHint(owner, {
      documentRevision: 7,
      annotationStoreVersion: committed.value.annotationStoreVersion,
    })
    await waitFor(() => expect(result.current.anns).toEqual([
      expect.objectContaining({ id: 'channel-saved' }),
    ]))
    expect(transaction).toHaveBeenCalled()

    transaction.mockClear()
    await dispatchAnnotationHint(owner, {
      documentRevision: 7,
      annotationStoreVersion: committed.value.annotationStoreVersion,
    })
    expect(transaction).not.toHaveBeenCalled()
  })

  it('routes summary change hints through revision zero', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const { result } = renderHook(() =>
      useArticleAnnotations(owner, null, summary()),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))
    const committed = await commitAnnotationOperation(owner, {
      kind: 'add',
      opId: 'channel-summary',
      linkId: 'L1',
      target: { kind: 'summary', sourceHash: HASH },
      draft: {
        id: 'channel-summary',
        start: 0,
        end: 7,
        text: 'summary',
        note: '',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
      },
    })
    if (!committed.ok || committed.value.status === 'op-id-conflict') {
      throw new Error('summary annotation seed must commit')
    }
    const transaction = vi.spyOn(IDBDatabase.prototype, 'transaction')

    await dispatchAnnotationHint(owner, {
      documentRevision: 7,
      annotationStoreVersion: committed.value.annotationStoreVersion,
    })
    expect(transaction).not.toHaveBeenCalled()
    expect(result.current.anns).toEqual([])

    await dispatchAnnotationHint(owner, {
      documentRevision: 0,
      annotationStoreVersion: committed.value.annotationStoreVersion,
    })
    await waitFor(() => expect(result.current.anns).toEqual([
      expect.objectContaining({ id: 'channel-summary' }),
    ]))
    expect(transaction).toHaveBeenCalled()
  })

  it('publishes revision zero for summary writes and the target revision for body writes', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const controller = new SavedArticleDocumentController({
      lease: owner,
      detail: detail(),
    })
    const { result } = renderHook(() =>
      useArticleAnnotations(owner, controller.getSnapshot().id, summary()),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))
    const key = ownedStorageKeyForLease('annotationWakeup', owner)
    if (!key) throw new Error('annotation wakeup storage key is unavailable')

    let summaryOutcome: Awaited<ReturnType<typeof result.current.add>> | undefined
    await act(async () => {
      summaryOutcome = await result.current.add({
        blockKey: 'summary',
        start: 0,
        end: 7,
        text: 'summary',
      })
    })
    await waitFor(() => expect(result.current.loading).toBe(false))
    if (!summaryOutcome || !('annotationStoreVersion' in summaryOutcome)) {
      throw new Error('summary annotation must commit')
    }
    expect(JSON.parse(localStorage.getItem(key) ?? '{}')).toMatchObject({
      linkId: 'L1',
      documentRevision: 0,
      annotationStoreVersion: summaryOutcome.annotationStoreVersion,
    })

    let bodyOutcome: Awaited<ReturnType<typeof result.current.add>> | undefined
    await act(async () => {
      bodyOutcome = await result.current.add({
        blockKey: 'content-document',
        start: 0,
        end: 4,
        text: 'body',
      }, { documentContext: controller.captureContext() })
    })
    await waitFor(() => expect(result.current.loading).toBe(false))
    if (!bodyOutcome || !('annotationStoreVersion' in bodyOutcome)) {
      throw new Error('body annotation must commit')
    }
    expect(JSON.parse(localStorage.getItem(key) ?? '{}')).toMatchObject({
      linkId: 'L1',
      documentRevision: 7,
      annotationStoreVersion: bodyOutcome.annotationStoreVersion,
    })
    controller.dispose()
  })

  it('rereads durable state on visibilitychange without any channel message', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const documentId = {
      namespace: 'physical-A',
      linkId: 'L1',
      contentRevision: 7,
    }
    const { result } = renderHook(() =>
      useArticleAnnotations(owner, documentId, null),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))

    await commitAnnotationOperation(owner, {
      kind: 'add',
      opId: 'missed-message',
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      draft: {
        id: 'from-other-page',
        blockKey: 'content',
        start: 0,
        end: 4,
        text: 'body',
        note: '',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
      },
    })
    expect(result.current.anns).toEqual([])

    await act(async () => {
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await waitFor(() => expect(result.current.anns).toEqual([
      expect.objectContaining({ id: 'from-other-page' }),
    ]))
  })

  it('treats storage payload as a wakeup only and reloads the durable projection', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const documentId = {
      namespace: 'physical-A',
      linkId: 'L1',
      contentRevision: 7,
    }
    const { result } = renderHook(() =>
      useArticleAnnotations(owner, documentId, null),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))
    const key = ownedStorageKeyForLease('annotationWakeup', owner)

    await commitAnnotationOperation(owner, {
      kind: 'add',
      opId: 'durable-wins',
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      draft: {
        id: 'durable',
        blockKey: 'content',
        start: 0,
        end: 4,
        text: 'body',
        note: '',
        source: 'self',
        createdAt: 1,
        updatedAt: 1,
      },
    })
    await act(async () => {
      window.dispatchEvent(new StorageEvent('storage', {
        key,
        newValue: JSON.stringify({
          kind: 'annotation-change',
          namespace: 'physical-A',
          linkId: 'L1',
          documentRevision: 7,
          annotationStoreVersion: 999,
          items: [{ id: 'forged' }],
        }),
      }))
    })
    await waitFor(() => expect(result.current.anns).toEqual([
      expect.objectContaining({ id: 'durable' }),
    ]))
    expect(result.current.anns).not.toEqual([
      expect.objectContaining({ id: 'forged' }),
    ])
  })
})
