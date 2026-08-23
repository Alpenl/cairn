import { describe, expect, it } from 'vitest'

import {
  blockHighlights,
} from './annotations'
import {
  annOrder,
  annotationLocator,
  annotationMatchesLocator,
  type Annotation,
} from './annotation-domain'

function annotation(overrides: Partial<Annotation> = {}): Annotation {
  return {
    id: 'a1',
    blockKey: 'summary',
    start: 0,
    end: 4,
    text: 'abcd',
    note: '',
    source: 'self',
    createdAt: 1,
    updatedAt: 1,
    ...overrides,
  }
}

describe('blockHighlights', () => {
  it('keeps only non-overlapping annotations from the requested block', () => {
    const result = blockHighlights([
      annotation({ id: 'a', start: 0, end: 5 }),
      annotation({ id: 'b', start: 3, end: 8 }),
      annotation({ id: 'c', start: 6, end: 9 }),
      annotation({ id: 'd', blockKey: 'content', start: 0, end: 2 }),
    ], 'summary')

    expect(result.map((item) => item.id)).toEqual(['a', 'c'])
  })
})

describe('annotation locator', () => {
  it('keeps source target identity and rejects the same id in another generation', () => {
    const current = annotation({
      id: 'shared',
      blockKey: 'content',
      sourceContentRevision: 8,
    })
    const locator = annotationLocator(current)

    expect(locator).toEqual({
      id: 'shared',
      blockKey: 'content',
      target: { kind: 'saved-content', contentRevision: 8 },
    })
    if (!locator) throw new Error('current saved annotation must have a locator')
    expect(annotationMatchesLocator(current, locator)).toBe(true)
    expect(annotationMatchesLocator(
      { ...current, sourceContentRevision: 9 },
      locator,
    )).toBe(false)
  })

  it('uses one discriminated target and refuses ambiguous or unbound identities', () => {
    const sourceHash = 'a'.repeat(64)
    expect(annotationLocator(annotation({ sourceSummaryHash: sourceHash }))).toEqual({
      id: 'a1',
      blockKey: 'summary',
      target: { kind: 'summary', sourceHash },
    })
    expect(annotationLocator(annotation())).toBeNull()
    expect(annotationLocator(annotation({
      sourceContentRevision: 8,
      sourceSummaryHash: sourceHash,
    }))).toBeNull()
    expect(annotationLocator(annotation({ sourceSummaryHash: 'not-a-source-hash' }))).toBeNull()
    expect(annotationLocator(annotation({
      blockKey: 'content-document',
      sourceNoteRevision: 4,
    }))).toEqual({
      id: 'a1',
      blockKey: 'content-document',
      target: { kind: 'note', noteRevision: 4 },
    })
  })
})

describe('annOrder', () => {
  it('orders summary, saved content, then unsupported historical blocks', () => {
    expect(annOrder({ blockKey: 'summary' })).toBe(-1)
    expect(annOrder({ blockKey: 'content' })).toBe(1)
    expect(annOrder({ blockKey: 'content-document' })).toBe(1)
    expect(annOrder({ blockKey: 'dr' })).toBe(9999)
  })
})
