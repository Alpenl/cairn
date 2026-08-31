import { describe, expect, it } from 'vitest'

import type { Annotation } from './annotations'
import {
  readingBlockHighlights,
  readingHighlightClassName,
  readingHighlightSignature,
  resolveReadingHighlightClick,
  splitReadingSelectionText,
} from './reading-selection'

function annotation(over: Partial<Annotation> = {}): Annotation {
  return {
    id: 'ann-1',
    blockKey: 'content',
    start: 2,
    end: 5,
    text: 'cde',
    note: '',
    source: 'self',
    createdAt: 1,
    updatedAt: 1,
    sourceContentRevision: 7,
    ...over,
  }
}

describe('reading selection rendering plan', () => {
  it('splits text-node ranges in the same block coordinate space used by selection mapping', () => {
    const highlights = readingBlockHighlights([annotation()], 'content')

    expect(splitReadingSelectionText('ab', 0, highlights)).toEqual([{ kind: 'text', text: 'ab' }])
    expect(splitReadingSelectionText('cdef', 2, highlights)).toEqual([
      {
        kind: 'highlight',
        key: ['ann-1', 'saved-content:7', '2'].join('\0'),
        text: 'cde',
        highlight: highlights[0],
      },
      { kind: 'text', text: 'f' },
    ])
  })

  it('keeps DOM mark identity and classes target-aware for every reader implementation', () => {
    const highlights = readingBlockHighlights([
      annotation({ id: 'self', note: 'reader note', source: 'self' }),
      annotation({ id: 'ai', start: 6, end: 8, text: 'gh', note: '', source: 'ai' }),
    ], 'content')

    expect(readingHighlightSignature(highlights)).toBe(
      'self:saved-content:7:2:5:1:self|ai:saved-content:7:6:8:0:ai',
    )
    expect(readingHighlightClassName(highlights[0])).toBe('hl has-note')
    expect(readingHighlightClassName(highlights[1])).toBe('hl ai')
  })

  it('resolves clicked marks through annotation id and target key, not id alone', () => {
    const highlights = readingBlockHighlights([
      annotation({ id: 'shared', start: 0, end: 3, sourceContentRevision: 7 }),
      annotation({ id: 'shared', start: 4, end: 7, sourceContentRevision: 8 }),
    ], 'content')
    document.body.innerHTML = '<mark data-ann="shared" data-ann-target="saved-content:8">new</mark>'
    const mark = document.querySelector('mark')

    expect(resolveReadingHighlightClick(mark, highlights)?.locator.target).toEqual({
      kind: 'saved-content',
      contentRevision: 8,
    })
  })
})
