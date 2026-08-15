/**
 * Annotation domain types and pure rendered-selection helpers.
 *
 * Durable ownership and legacy import live under `lib/user-data`; this module
 * deliberately has no storage or React lifecycle responsibilities.
 */

import {
  annotationTargetKey,
  canonicalAnnotationTarget,
  type LegacyStaleAnnotationTarget,
  type SavedContentAnnotationBlockKey,
  type SavedContentAnnotationTarget,
  type NoteAnnotationTarget,
  type SummaryAnnotationTarget,
} from './user-data/annotation-types'

export type AnnotationSource = 'self' | 'ai' | 'user'

export interface AnnotationQuote {
  readonly exact: string
  readonly prefix: string
  readonly suffix: string
}

export interface Annotation {
  id: string
  blockKey: string
  start: number
  end: number
  text: string
  note: string
  source: AnnotationSource
  createdAt: number
  updatedAt: number
  /** TextQuoteSelector context used when a host revision changes. */
  quote?: AnnotationQuote
  /** Reader-owned binding for content/content-* offsets. */
  sourceContentRevision?: number
  /** Reader-owned binding for summary offsets. */
  sourceSummaryHash?: string
  /** Reader-owned binding for note offsets. */
  sourceNoteRevision?: number
}

export type AnnotationLocator =
  | {
      readonly id: string
      readonly blockKey: SavedContentAnnotationBlockKey
      readonly target: SavedContentAnnotationTarget
    }
  | {
      readonly id: string
      readonly blockKey: 'summary'
      readonly target: SummaryAnnotationTarget
    }
  | {
      readonly id: string
      readonly blockKey: string
      readonly target: NoteAnnotationTarget
    }
  | {
      readonly id: string
      readonly blockKey: string
      readonly target: LegacyStaleAnnotationTarget
    }

/**
 * Builds the only source references that interactive Reader surfaces accept.
 * Legacy/unbound annotations remain renderable for recovery, but cannot be
 * edited through a current document target.
 */
export function annotationLocator(annotation: Annotation): AnnotationLocator | null {
  if (
    isContentAnchored(annotation.blockKey) &&
    annotation.sourceSummaryHash === undefined &&
    annotation.sourceContentRevision !== undefined &&
    annotation.sourceNoteRevision === undefined
  ) {
    const target = canonicalAnnotationTarget({
      kind: 'saved-content',
      contentRevision: annotation.sourceContentRevision,
    })
    return target?.kind === 'saved-content'
      ? { id: annotation.id, blockKey: annotation.blockKey, target }
      : null
  }
  if (
    annotation.blockKey === 'summary' &&
    annotation.sourceContentRevision === undefined &&
    annotation.sourceSummaryHash !== undefined &&
    annotation.sourceNoteRevision === undefined
  ) {
    const target = canonicalAnnotationTarget({
      kind: 'summary',
      sourceHash: annotation.sourceSummaryHash,
    })
    return target?.kind === 'summary'
      ? { id: annotation.id, blockKey: annotation.blockKey, target }
      : null
  }
  if (
    annotation.sourceContentRevision === undefined &&
    annotation.sourceSummaryHash === undefined &&
    annotation.sourceNoteRevision !== undefined
  ) {
    const target = canonicalAnnotationTarget({
      kind: 'note',
      noteRevision: annotation.sourceNoteRevision,
    })
    return target?.kind === 'note'
      ? { id: annotation.id, blockKey: annotation.blockKey, target }
      : null
  }
  return null
}

/** Stable target identity for DOM delegation, React keys, and effect ownership. */
export function annotationLocatorTargetKey(locator: AnnotationLocator): string | null {
  return annotationTargetKey(locator.target)
}

export function annotationMatchesLocator(
  annotation: Annotation,
  locator: AnnotationLocator,
): boolean {
  const candidate = annotationLocator(annotation)
  if (!candidate) return false
  const candidateTargetKey = annotationLocatorTargetKey(candidate)
  const locatorTargetKey = annotationLocatorTargetKey(locator)
  return candidateTargetKey !== null &&
    candidateTargetKey === locatorTargetKey &&
    candidate.id === locator.id &&
    candidate.blockKey === locator.blockKey
}

export interface AnnotationInput {
  blockKey: string
  start: number
  end: number
  text: string
  note?: string
  source?: AnnotationSource
  quote?: AnnotationQuote
}

export type AnnotationPatch = Partial<Pick<Annotation, 'note' | 'source'>>

/** Stable empty value for memoized rendering consumers. */
export const NO_ANNOTATIONS: Annotation[] = Object.freeze([]) as unknown as Annotation[]

/** Saved-original and derivative blocks share the saved document revision. */
export function isContentAnchored(blockKey: string): blockKey is SavedContentAnnotationBlockKey {
  return blockKey === 'content' || blockKey.startsWith('content-')
}

/**
 * Filters one rendered block and greedily removes overlapping ranges.
 */
export function blockHighlights(anns: Annotation[], blockKey: string): Annotation[] {
  const list = anns.filter((annotation) => annotation.blockKey === blockKey)
    .sort((left, right) => left.start - right.start)
  const clean: Annotation[] = []
  let lastEnd = -1
  for (const highlight of list) {
    if (highlight.start >= lastEnd) {
      clean.push(highlight)
      lastEnd = highlight.end
    }
  }
  return clean
}

export interface SelectionInfo {
  blockKey: string
  /** Canonical rendered projection of the entire source block. */
  blockText: string
  start: number
  end: number
  text: string
  quote: AnnotationQuote
  rect: DOMRect
}

const ANNOTATION_QUOTE_CONTEXT = 32

export function buildAnnotationQuote(
  source: string,
  start: number,
  end: number,
  context = ANNOTATION_QUOTE_CONTEXT,
): AnnotationQuote {
  if (
    !Number.isSafeInteger(start) ||
    !Number.isSafeInteger(end) ||
    start < 0 ||
    end <= start ||
    end > source.length
  ) {
    throw new Error('annotation range is outside the source')
  }
  const boundedContext = Math.max(0, Math.floor(context))
  return {
    exact: source.slice(start, end),
    prefix: source.slice(Math.max(0, start - boundedContext), start),
    suffix: source.slice(end, Math.min(source.length, end + boundedContext)),
  }
}

/** Reads the current selection in the text-coordinate space of one source block. */
export function getSelectionInfo(
  scopeEl: HTMLElement | null,
  minimumTextLength = 2,
): SelectionInfo | null {
  const selection = window.getSelection()
  if (!selection || selection.isCollapsed || selection.rangeCount === 0) return null
  if (selection.toString().trim().length < Math.max(1, Math.floor(minimumTextLength))) return null
  const range = selection.getRangeAt(0)
  const node = range.startContainer
  let block: Element | null = node.nodeType === 3 ? node.parentElement : node as Element
  block = block?.closest('[data-hl-block]') ?? null
  if (!block || !block.contains(range.endContainer)) return null
  if (scopeEl && !scopeEl.contains(block)) return null

  const preStart = document.createRange()
  preStart.selectNodeContents(block)
  preStart.setEnd(range.startContainer, range.startOffset)
  const start = preStart.toString().length
  const preEnd = document.createRange()
  preEnd.selectNodeContents(block)
  preEnd.setEnd(range.endContainer, range.endOffset)
  const end = preEnd.toString().length
  const text = range.toString()
  const blockKey = (block as HTMLElement).dataset.hlBlock
  if (!blockKey || end <= start) return null
  return {
    blockKey,
    blockText: block.textContent ?? '',
    start,
    end,
    text,
    quote: buildAnnotationQuote(block.textContent ?? '', start, end),
    rect: range.getBoundingClientRect(),
  }
}

/** Summary first, saved content next, unsupported historical blocks last. */
export function annOrder(annotation: Pick<Annotation, 'blockKey'>): number {
  switch (annotation.blockKey) {
    case 'summary':
      return -1
    case 'content':
    case 'content-document':
      return 1
    default:
      return 9999
  }
}
