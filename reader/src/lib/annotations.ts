/**
 * Rendered-selection helpers for annotation surfaces.
 *
 * Pure domain values live in `annotations-domain`; durable ownership and
 * legacy import live under `lib/user-data`.
 */

import type { Annotation, AnnotationQuote } from './annotations-domain'

export {
  annotationLocator,
  annotationLocatorTargetKey,
  annotationMatchesLocator,
  annotationTargetKey,
  canonicalAnnotationTarget,
  isContentAnchored,
  isSavedContentAnnotationBlockKey,
} from './annotations-domain'
export type {
  Annotation,
  AnnotationInput,
  AnnotationLocator,
  AnnotationPatch,
  AnnotationQuote,
  AnnotationSource,
  AnnotationTarget,
  NoteAnnotationTarget,
  SavedContentAnnotationBlockKey,
  SavedContentAnnotationTarget,
  SummaryAnnotationTarget,
} from './annotations-domain'

/** Stable empty value for memoized rendering consumers. */
export const NO_ANNOTATIONS: Annotation[] = Object.freeze([]) as unknown as Annotation[]

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
