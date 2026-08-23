import { describe, expect, it } from 'vitest'

import type { Annotation } from './annotation-domain'
import {
  buildAnnotationAddOperation,
  buildAnnotationDeleteOperation,
  buildAnnotationUpdateOperation,
  combineAnnotationCommandSignals,
} from './annotation-commands'

const HASH_A = 'a'.repeat(64)
const HASH_B = 'b'.repeat(64)

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
