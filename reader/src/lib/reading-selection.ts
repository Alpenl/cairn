import {
  annotationLocator,
  annotationLocatorTargetKey,
  blockHighlights,
  type Annotation,
  type AnnotationLocator,
} from './annotations'

export interface ReadingHighlight {
  readonly annotation: Annotation
  readonly locator: AnnotationLocator | null
  readonly targetKey: string | null
  readonly classNames: readonly string[]
}

export type ReadingSelectionTextSegment =
  | {
      readonly kind: 'text'
      readonly text: string
    }
  | {
      readonly kind: 'highlight'
      readonly key: string
      readonly text: string
      readonly highlight: ReadingHighlight
    }

export interface ReadingHighlightClick {
  readonly locator: AnnotationLocator
  readonly mark: HTMLElement
}

export function readingBlockHighlights(
  annotations: readonly Annotation[],
  blockKey: string,
): ReadingHighlight[] {
  return blockHighlights([...annotations], blockKey).map((annotation) => {
    const locator = annotationLocator(annotation)
    const targetKey = locator ? annotationLocatorTargetKey(locator) : null
    return {
      annotation,
      locator,
      targetKey,
      classNames: [
        'hl',
        ...(annotation.note ? ['has-note'] : []),
        ...(annotation.source === 'ai' ? ['ai'] : []),
      ],
    }
  })
}

export function readingHighlightClassName(highlight: ReadingHighlight): string {
  return highlight.classNames.join(' ')
}

export function readingHighlightSignature(highlights: readonly ReadingHighlight[]): string {
  return highlights.map((highlight) => {
    const { annotation } = highlight
    return [
      annotation.id,
      highlight.targetKey ?? 'invalid',
      annotation.start,
      annotation.end,
      annotation.note ? 1 : 0,
      annotation.source,
    ].join(':')
  }).join('|')
}

export function splitReadingSelectionText(
  text: string,
  baseOffset: number,
  highlights: readonly ReadingHighlight[],
): ReadingSelectionTextSegment[] {
  const segmentEnd = baseOffset + text.length
  const overlaps = highlights.filter((highlight) =>
    highlight.annotation.start < segmentEnd && highlight.annotation.end > baseOffset
  )
  if (overlaps.length === 0) return [{ kind: 'text', text }]

  const pieces: ReadingSelectionTextSegment[] = []
  let cursor = baseOffset
  for (const highlight of overlaps) {
    const start = Math.max(highlight.annotation.start, baseOffset)
    const end = Math.min(highlight.annotation.end, segmentEnd)
    if (start > cursor) {
      pieces.push({ kind: 'text', text: text.slice(cursor - baseOffset, start - baseOffset) })
    }
    pieces.push({
      kind: 'highlight',
      key: `${highlight.annotation.id}\0${highlight.targetKey ?? 'invalid'}\0${start}`,
      text: text.slice(start - baseOffset, end - baseOffset),
      highlight,
    })
    cursor = end
  }
  if (cursor < segmentEnd) {
    pieces.push({ kind: 'text', text: text.slice(cursor - baseOffset) })
  }
  return pieces
}

export function resolveReadingHighlightClick(
  target: EventTarget | null,
  highlights: readonly ReadingHighlight[],
): ReadingHighlightClick | null {
  if (!(target instanceof HTMLElement)) return null
  const mark = target.closest('mark[data-ann]') as HTMLElement | null
  if (!mark) return null
  const annotationId = mark.dataset.ann
  const targetKey = mark.dataset.annTarget ?? null
  const highlight = highlights.find((item) =>
    item.annotation.id === annotationId && item.targetKey === targetKey
  )
  return highlight?.locator ? { locator: highlight.locator, mark } : null
}
