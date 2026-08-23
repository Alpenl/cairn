import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { Annotation } from './annotation-domain'
import { IdentityLease } from './identity'
import {
  buildAnnotationAddOperation,
  buildAnnotationDeleteOperation,
  buildAnnotationUpdateOperation,
  combineAnnotationCommandSignals,
  commitAnnotationCommand,
} from './annotation-commands'
import type {
  AnnotationCommitOptions,
  AnnotationOperationInput,
} from './user-data/annotation-types'

const annotationStore = vi.hoisted(() => ({
  commitAnnotationOperation: vi.fn(),
  readAnnotationSnapshot: vi.fn(),
}))

vi.mock('./user-data/annotation-store', () => annotationStore)

const HASH_A = 'a'.repeat(64)
const HASH_B = 'b'.repeat(64)

const lease = () => new IdentityLease({
  serverClientDataNamespace: 'server-client',
  physicalNamespace: 'physical',
  localEpoch: 1,
})

function commandOperation(): AnnotationOperationInput {
  const operation = buildAnnotationAddOperation({
    linkId: 'L1',
    target: { kind: 'saved-content', contentRevision: 7 },
    annotationId: 'a1',
    opId: 'op-a1',
    now: 10,
    annotation: {
      blockKey: 'content-document',
      start: 0,
      end: 4,
      text: 'body',
    },
  })
  if (!operation) throw new Error('expected valid operation')
  return operation
}

function committed(sequence = 1) {
  return {
    ok: true,
    value: {
      status: 'committed' as const,
      sequence,
      annotationStoreVersion: sequence + 10,
      annotation: null,
    },
  }
}

const savedAnnotation: Annotation = {
  id: 'a1',
  blockKey: 'content-document',
  start: 0,
  end: 4,
  text: 'body',
  note: '',
  source: 'self',
  createdAt: 1,
  updatedAt: 1,
  sourceContentRevision: 7,
}

const summaryAnnotation: Annotation = {
  id: 's1',
  blockKey: 'summary',
  start: 0,
  end: 7,
  text: 'summary',
  note: '',
  source: 'ai',
  createdAt: 1,
  updatedAt: 1,
  sourceSummaryHash: HASH_A,
}

beforeEach(() => {
  annotationStore.commitAnnotationOperation.mockReset()
  annotationStore.readAnnotationSnapshot.mockReset()
})

describe('annotation command builders', () => {
  it('builds saved-content and summary add operations without leaking target-owned fields', () => {
    const saved = buildAnnotationAddOperation({
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      annotationId: 'a1',
      opId: 'op-a1',
      now: 10,
      annotation: {
        blockKey: 'content-document',
        start: 0,
        end: 4,
        text: 'body',
        quote: { exact: 'body', prefix: '', suffix: '' },
      },
    })
    const summary = buildAnnotationAddOperation({
      linkId: 'L1',
      target: { kind: 'summary', sourceHash: HASH_A },
      annotationId: 's1',
      opId: 'op-s1',
      now: 20,
      annotation: {
        blockKey: 'summary',
        start: 0,
        end: 7,
        text: 'summary',
        note: 'seed',
        source: 'ai',
      },
    })

    expect(saved).toMatchObject({
      kind: 'add',
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      draft: {
        id: 'a1',
        blockKey: 'content-document',
        createdAt: 10,
        updatedAt: 10,
        source: 'self',
      },
    })
    expect(summary).toMatchObject({
      kind: 'add',
      linkId: 'L1',
      target: { kind: 'summary', sourceHash: HASH_A },
      draft: {
        id: 's1',
        createdAt: 20,
        updatedAt: 20,
        note: 'seed',
        source: 'ai',
      },
    })
    if (!summary || summary.kind !== 'add') throw new Error('summary add operation expected')
    expect('blockKey' in summary.draft).toBe(false)
    expect('sourceSummaryHash' in summary.draft).toBe(false)
  })

  it('rejects update and delete operations for stale target-bound references', () => {
    expect(buildAnnotationUpdateOperation({
      linkId: 'L1',
      target: { kind: 'summary', sourceHash: HASH_B },
      annotation: summaryAnnotation,
      patch: { note: 'stale' },
      opId: 'op-stale-update',
      now: 30,
    })).toBeNull()
    expect(buildAnnotationDeleteOperation({
      linkId: 'L1',
      target: { kind: 'summary', sourceHash: HASH_B },
      annotation: summaryAnnotation,
      opId: 'op-stale-delete',
    })).toBeNull()

    expect(buildAnnotationUpdateOperation({
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      annotation: savedAnnotation,
      patch: { note: 'fresh' },
      opId: 'op-fresh-update',
      now: 30,
    })).toMatchObject({
      kind: 'update',
      annotationId: 'a1',
      patch: { note: 'fresh', updatedAt: 30 },
    })
  })

  it('combines command abort signals and detaches listeners on dispose', () => {
    const primary = new AbortController()
    const secondary = new AbortController()
    const combined = combineAnnotationCommandSignals([
      primary.signal,
      secondary.signal,
    ])

    expect(combined.signal.aborted).toBe(false)
    secondary.abort()
    expect(combined.signal.aborted).toBe(true)
    combined.dispose()
  })
})

describe('commitAnnotationCommand', () => {
  it('commits directly without a preflight snapshot read', async () => {
    annotationStore.readAnnotationSnapshot.mockResolvedValue({ ok: false })
    annotationStore.commitAnnotationOperation.mockResolvedValue(committed(3))

    const result = await commitAnnotationCommand({
      lease: lease(),
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      operation: commandOperation(),
      annotationId: 'a1',
      commandSignal: new AbortController().signal,
    })

    expect(annotationStore.readAnnotationSnapshot).not.toHaveBeenCalled()
    expect(annotationStore.commitAnnotationOperation).toHaveBeenCalledTimes(1)
    expect(result).toEqual({
      status: 'committed',
      annotationId: 'a1',
      sequence: 3,
      annotationStoreVersion: 13,
    })
  })

  it('propagates already aborted external signals before opening the write transaction', async () => {
    const external = new AbortController()
    external.abort()

    const result = await commitAnnotationCommand({
      lease: lease(),
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      operation: commandOperation(),
      annotationId: 'a1',
      commandSignal: new AbortController().signal,
      externalSignals: [external.signal],
    })

    expect(result).toEqual({ status: 'stale' })
    expect(annotationStore.commitAnnotationOperation).not.toHaveBeenCalled()
  })

  it('returns stale when an external signal aborts while the write transaction is pending', async () => {
    const external = new AbortController()
    annotationStore.commitAnnotationOperation.mockImplementation(
      async (_lease: IdentityLease, _operation: AnnotationOperationInput, options: AnnotationCommitOptions) => {
        expect(options.signal?.aborted).toBe(false)
        external.abort()
        expect(options.signal?.aborted).toBe(true)
        return { ok: false }
      },
    )

    const result = await commitAnnotationCommand({
      lease: lease(),
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      operation: commandOperation(),
      annotationId: 'a1',
      commandSignal: new AbortController().signal,
      externalSignals: [external.signal],
    })

    expect(result).toEqual({ status: 'stale' })
  })

  it('maps op id conflicts without running afterCommit', async () => {
    const afterCommit = vi.fn()
    annotationStore.commitAnnotationOperation.mockResolvedValue({
      ok: true,
      value: { status: 'op-id-conflict' },
    })

    const result = await commitAnnotationCommand({
      lease: lease(),
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      operation: commandOperation(),
      annotationId: 'a1',
      commandSignal: new AbortController().signal,
      afterCommit,
    })

    expect(result).toEqual({ status: 'op-id-conflict' })
    expect(afterCommit).not.toHaveBeenCalled()
  })
})
