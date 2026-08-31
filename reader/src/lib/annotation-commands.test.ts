import 'fake-indexeddb/auto'

import { describe, expect, it, vi } from 'vitest'

import type { Annotation } from './annotations-domain'
import {
  annotationCommandTargetForBlock,
  createAnnotationDeleteCommand,
  createAnnotationSelectionCommit,
  createAnnotationUpdateCommand,
  loadAnnotationTargets,
} from './annotation-commands'
import type { IdentityLease } from './identity'
import type { AnnotationTarget } from './user-data/annotation-types'

const SAVED_TARGET: AnnotationTarget = { kind: 'saved-content', contentRevision: 7 }
const SUMMARY_TARGET: AnnotationTarget = { kind: 'summary', sourceHash: 'a'.repeat(64) }
const NOTE_TARGET: AnnotationTarget = { kind: 'note', noteRevision: 4 }

function annotation(
  overrides: Partial<Annotation> = {},
): Annotation {
  return {
    id: 'an:one',
    blockKey: 'content',
    start: 0,
    end: 4,
    text: 'body',
    note: '',
    source: 'self',
    createdAt: 1,
    updatedAt: 1,
    sourceContentRevision: 7,
    ...overrides,
  }
}

describe('annotation command targets', () => {
  it('selects saved-content and summary targets from rendered block keys', () => {
    expect(annotationCommandTargetForBlock('content-document', {
      saved: SAVED_TARGET,
      summary: SUMMARY_TARGET,
    })).toEqual(SAVED_TARGET)
    expect(annotationCommandTargetForBlock('summary', {
      saved: SAVED_TARGET,
      summary: SUMMARY_TARGET,
    })).toEqual(SUMMARY_TARGET)
    expect(annotationCommandTargetForBlock('unknown', {
      saved: SAVED_TARGET,
      summary: SUMMARY_TARGET,
    })).toBeNull()
  })

  it('uses note as the adapter fallback for note annotation blocks', () => {
    expect(annotationCommandTargetForBlock('note', { note: NOTE_TARGET })).toEqual(NOTE_TARGET)
  })
})

describe('annotation command builders', () => {
  it('builds saved-content and summary selection commits without duplicating lifecycle code', () => {
    expect(createAnnotationSelectionCommit({
      linkId: 'L1',
      target: SAVED_TARGET,
      selection: {
        blockKey: 'content-document',
        start: 0,
        end: 4,
        text: 'body',
      },
      annotationToken: 'saved',
      operationToken: 'op-saved',
      now: 100,
    })).toMatchObject({
      ok: true,
      annotationId: 'an:saved',
      operation: {
        kind: 'add',
        opId: 'op:op-saved',
        draft: { blockKey: 'content-document', updatedAt: 100 },
      },
    })

    expect(createAnnotationSelectionCommit({
      linkId: 'L1',
      target: SUMMARY_TARGET,
      selection: {
        blockKey: 'summary',
        start: 0,
        end: 7,
        text: 'summary',
      },
      annotationToken: 'summary',
      operationToken: 'op-summary',
      now: 101,
    })).toMatchObject({
      ok: true,
      annotationId: 'an:summary',
      operation: {
        kind: 'add',
        opId: 'op:op-summary',
        draft: { updatedAt: 101 },
      },
    })
  })

  it('rejects unsupported target/block combinations', () => {
    expect(createAnnotationSelectionCommit({
      linkId: 'L1',
      target: SUMMARY_TARGET,
      selection: {
        blockKey: 'content',
        start: 0,
        end: 4,
        text: 'body',
      },
      annotationToken: 'bad',
      operationToken: 'bad',
    })).toEqual({ ok: false, status: 'unsupported' })
  })

  it('builds update and delete commands only for the current target', () => {
    expect(createAnnotationUpdateCommand({
      linkId: 'L1',
      target: SAVED_TARGET,
      annotation: annotation(),
      patch: { note: 'next' },
      operationToken: 'update',
      now: 200,
    })).toMatchObject({
      ok: true,
      annotationId: 'an:one',
      operation: {
        kind: 'update',
        opId: 'op:update',
        patch: { note: 'next', updatedAt: 200 },
      },
    })

    expect(createAnnotationDeleteCommand({
      linkId: 'L1',
      target: SAVED_TARGET,
      annotation: annotation(),
      operationToken: 'delete',
    })).toMatchObject({
      ok: true,
      annotationId: 'an:one',
      operation: { kind: 'delete', opId: 'op:delete' },
    })

    expect(createAnnotationUpdateCommand({
      linkId: 'L1',
      target: SUMMARY_TARGET,
      annotation: annotation(),
      patch: { note: 'next' },
      operationToken: 'stale',
    })).toEqual({ ok: false, status: 'stale' })
  })
})

describe('annotation target loading', () => {
  it('rejects stale loads before hitting storage', async () => {
    const signal = AbortSignal.abort()
    const readExtra = vi.fn()

    await expect(loadAnnotationTargets({
      lease: {} as IdentityLease,
      linkId: 'L1',
      targets: [SAVED_TARGET],
      signal,
      emptyExtra: null,
      readExtra,
    })).resolves.toEqual({ status: 'stale' })
    expect(readExtra).not.toHaveBeenCalled()
  })

  it('returns empty when an adapter has no current target', async () => {
    await expect(loadAnnotationTargets({
      lease: {} as IdentityLease,
      linkId: 'L1',
      targets: [],
      signal: new AbortController().signal,
      emptyExtra: null,
    })).resolves.toEqual({ status: 'empty' })
  })
})
