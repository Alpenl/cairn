import { describe, expect, it } from 'vitest'
import type { Annotation } from '../annotations-domain'
import {
  annotationFromAddDraft,
  annotationSignatureFields,
  annotationsEqual,
  bindAnnotationToTarget,
  cloneAnnotation,
  cloneTargetAnnotation,
  decodeAnnotationWire,
  isSavedContentAnnotationBlockKey,
} from './annotation-codec'
import type { AnnotationAddDraft } from './annotation-types'

const SUMMARY_HASH = 'a'.repeat(64)

function annotation(overrides: Partial<Annotation> = {}): Annotation {
  return {
    id: 'a1',
    blockKey: 'content-document',
    start: 1,
    end: 5,
    text: 'text',
    note: '',
    source: 'self',
    createdAt: 10,
    updatedAt: 11,
    ...overrides,
  }
}

describe('annotation codec', () => {
  it('decodes and clones the complete persisted annotation shape', () => {
    const source = {
      ...annotation({ sourceContentRevision: 7 }),
      ignored: 'not persisted',
    }

    const decoded = cloneAnnotation(source)

    expect(decoded).toEqual(annotation({ sourceContentRevision: 7 }))
    expect(decoded).not.toBe(source)
    expect(cloneAnnotation({ ...source, end: 0 })).toBeNull()
    expect(cloneAnnotation({ ...source, source: 'external' })).toBeNull()
  })

  it('uses one content block rule for target selection and target binding', () => {
    expect(isSavedContentAnnotationBlockKey('content')).toBe(true)
    expect(isSavedContentAnnotationBlockKey('content-document')).toBe(true)
    expect(isSavedContentAnnotationBlockKey('summary')).toBe(false)

    expect(bindAnnotationToTarget(
      annotation({ sourceContentRevision: 99, sourceSummaryHash: SUMMARY_HASH }),
      { kind: 'saved-content', contentRevision: 7 },
    )).toEqual(annotation({ sourceContentRevision: 7 }))
    expect(bindAnnotationToTarget(
      annotation({ blockKey: 'summary' }),
      { kind: 'saved-content', contentRevision: 7 },
    )).toBeNull()
  })

  it('distinguishes source rebinding from strict durable decoding', () => {
    const target = { kind: 'summary', sourceHash: SUMMARY_HASH } as const
    const bound = annotation({
      blockKey: 'summary',
      sourceSummaryHash: SUMMARY_HASH,
    })

    expect(cloneTargetAnnotation(bound, target)).toEqual(bound)
    expect(cloneTargetAnnotation({
      ...bound,
      sourceSummaryHash: 'b'.repeat(64),
    }, target)).toBeNull()
    expect(bindAnnotationToTarget({
      ...bound,
      sourceContentRevision: 8,
      sourceSummaryHash: 'b'.repeat(64),
    }, target)).toEqual(bound)
  })

  it('materializes target-discriminated drafts without trusting source identity fields', () => {
    const base = {
      id: 'a1',
      start: 1,
      end: 5,
      text: 'text',
      note: '',
      source: 'self' as const,
      createdAt: 10,
      updatedAt: 11,
    }

    expect(annotationFromAddDraft({
      ...base,
      blockKey: 'content-document',
      sourceContentRevision: 99,
      sourceSummaryHash: SUMMARY_HASH,
    } as unknown as AnnotationAddDraft, {
      kind: 'saved-content',
      contentRevision: 7,
    })).toEqual(annotation({ sourceContentRevision: 7 }))
    expect(annotationFromAddDraft({
      ...base,
      blockKey: 'caller-controlled',
    } as unknown as AnnotationAddDraft, {
      kind: 'summary',
      sourceHash: SUMMARY_HASH,
    })).toEqual(annotation({
      blockKey: 'summary',
      sourceSummaryHash: SUMMARY_HASH,
    }))

    expect(annotationFromAddDraft({
      ...base,
      blockKey: 'content-document',
      quote: { exact: 'text', prefix: 'before ', suffix: ' after' },
    } as unknown as AnnotationAddDraft, {
      kind: 'note',
      noteRevision: 4,
    })).toEqual(annotation({
      sourceNoteRevision: 4,
      quote: { exact: 'text', prefix: 'before ', suffix: ' after' },
    }))

    expect(annotationFromAddDraft({
      ...base,
      quote: { exact: 'text', prefix: '', suffix: '' },
    } as unknown as AnnotationAddDraft, {
      kind: 'note',
      noteRevision: 5,
    })).toEqual(annotation({
      blockKey: 'note',
      sourceNoteRevision: 5,
      quote: { exact: 'text', prefix: '', suffix: '' },
    }))
  })

  it('dual-reads legacy snake-case annotation wires and nested thought wires', () => {
    expect(decodeAnnotationWire({
      annotation_id: 'legacy-a1',
      block_key: 'content-document',
      range: { start: 2, end: 7 },
      selected_text: 'legacy',
      body: 'old thought',
      origin: 'user',
      source_content_revision: 7,
      created_at: 10,
      updated_at: '1970-01-01T00:00:00.011Z',
      quote: { selected_text: 'legacy', before: 'a', after: 'b' },
    })).toEqual({
      id: 'legacy-a1',
      blockKey: 'content-document',
      start: 2,
      end: 7,
      text: 'legacy',
      note: 'old thought',
      source: 'self',
      createdAt: 10,
      updatedAt: 11,
      sourceContentRevision: 7,
      quote: { exact: 'legacy', prefix: 'a', suffix: 'b' },
    })
    expect(decodeAnnotationWire({
      annotation: {
        id: 'nested-a1',
        blockKey: 'note',
        start: 0,
        end: 3,
        text: 'new',
        note: 'nested thought',
        source: 'self',
        createdAt: 1,
        updatedAt: 2,
        sourceNoteRevision: 4,
      },
    })).toEqual(expect.objectContaining({
      id: 'nested-a1',
      sourceNoteRevision: 4,
    }))
  })

  it('keeps one stable field order for equality and persisted signatures', () => {
    const value = annotation({ sourceContentRevision: 7 })

    expect(annotationSignatureFields(value)).toEqual([
      'a1',
      'content-document',
      1,
      5,
      'text',
      '',
      'self',
      10,
      11,
      7,
      null,
      null,
      null,
      null,
      null,
    ])
    expect(annotationsEqual(value, { ...value })).toBe(true)
    expect(annotationsEqual(value, { ...value, note: 'changed' })).toBe(false)
  })
})
