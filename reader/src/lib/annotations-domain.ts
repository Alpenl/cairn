/**
 * Pure annotation domain values shared by rendering, hooks, and durable stores.
 *
 * Keep this module dependency-free so lower storage modules and upper Reader
 * surfaces can depend on the same value vocabulary without forming cycles.
 */

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

export type SavedContentAnnotationBlockKey = 'content' | `content-${string}`

export interface SavedContentAnnotationTarget {
  readonly kind: 'saved-content'
  readonly contentRevision: number
}

export interface SummaryAnnotationTarget {
  readonly kind: 'summary'
  readonly sourceHash: string
}

export interface NoteAnnotationTarget {
  readonly kind: 'note'
  readonly noteRevision: number
}

export type AnnotationTarget =
  | SavedContentAnnotationTarget
  | SummaryAnnotationTarget
  | NoteAnnotationTarget

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

function isValidAnnotationSourceHash(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{64}$/.test(value)
}

function cloneTarget(target: AnnotationTarget): AnnotationTarget | null {
  switch (target.kind) {
    case 'saved-content':
      if (!Number.isSafeInteger(target.contentRevision) || target.contentRevision <= 0) {
        return null
      }
      return { kind: target.kind, contentRevision: target.contentRevision }
    case 'summary':
      if (!isValidAnnotationSourceHash(target.sourceHash)) return null
      return { kind: target.kind, sourceHash: target.sourceHash }
    case 'note':
      if (!Number.isSafeInteger(target.noteRevision) || target.noteRevision <= 0) {
        return null
      }
      return { kind: target.kind, noteRevision: target.noteRevision }
  }
}

export function canonicalAnnotationTarget(target: AnnotationTarget): AnnotationTarget | null {
  return cloneTarget(target)
}

export function annotationTargetKey(target: AnnotationTarget): string | null {
  const canonical = cloneTarget(target)
  if (!canonical) return null
  switch (canonical.kind) {
    case 'saved-content':
      return `saved-content:${canonical.contentRevision}`
    case 'summary':
      return `summary:${canonical.sourceHash}`
    case 'note':
      return `note:${canonical.noteRevision}`
  }
}

export function isSavedContentAnnotationBlockKey(
  candidate: unknown,
): candidate is SavedContentAnnotationBlockKey {
  return candidate === 'content' ||
    (typeof candidate === 'string' && candidate.startsWith('content-'))
}

/** Saved-original and derivative blocks share the saved document revision. */
export function isContentAnchored(blockKey: string): blockKey is SavedContentAnnotationBlockKey {
  return isSavedContentAnnotationBlockKey(blockKey)
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
