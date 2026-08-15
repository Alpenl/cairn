import type { Annotation, AnnotationQuote } from '../annotations'
import {
  canonicalAnnotationTarget,
  type AnnotationAddDraft,
  type AnnotationTarget,
  type SavedContentAnnotationBlockKey,
} from './annotation-types'

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0
}

function firstDefined(record: Record<string, unknown>, keys: readonly string[]): unknown {
  for (const key of keys) {
    if (record[key] !== undefined) return record[key]
  }
  return undefined
}

function cloneQuote(candidate: unknown): AnnotationQuote | null {
  if (!isRecord(candidate)) return null
  const exact = firstDefined(candidate, ['exact', 'text', 'selected_text'])
  const prefix = firstDefined(candidate, ['prefix', 'before'])
  const suffix = firstDefined(candidate, ['suffix', 'after'])
  if (typeof exact !== 'string' ||
    (prefix !== undefined && typeof prefix !== 'string') ||
    (suffix !== undefined && typeof suffix !== 'string')) {
    return null
  }
  return {
    exact,
    prefix: typeof prefix === 'string' ? prefix : '',
    suffix: typeof suffix === 'string' ? suffix : '',
  }
}

export function isSafeNonNegativeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

export function isSavedContentAnnotationBlockKey(
  candidate: unknown,
): candidate is SavedContentAnnotationBlockKey {
  return candidate === 'content' ||
    (typeof candidate === 'string' && candidate.startsWith('content-'))
}

/** Decodes the persisted annotation shape without trusting its source binding. */
export function cloneAnnotation(candidate: unknown): Annotation | null {
  if (!isRecord(candidate)) return null
  if (
    !isNonEmptyString(candidate.id) ||
    !isNonEmptyString(candidate.blockKey) ||
    !isSafeNonNegativeInteger(candidate.start) ||
    !isSafeNonNegativeInteger(candidate.end) ||
    candidate.end < candidate.start ||
    typeof candidate.text !== 'string' ||
    typeof candidate.note !== 'string' ||
    (candidate.source !== 'self' && candidate.source !== 'ai' && candidate.source !== 'user') ||
    !isSafeNonNegativeInteger(candidate.createdAt) ||
    !isSafeNonNegativeInteger(candidate.updatedAt) ||
    (
      candidate.sourceContentRevision !== undefined &&
      !isSafeNonNegativeInteger(candidate.sourceContentRevision)
    ) ||
    (
      candidate.sourceSummaryHash !== undefined &&
      typeof candidate.sourceSummaryHash !== 'string'
    ) ||
    (
      candidate.sourceNoteRevision !== undefined &&
      (!isSafeNonNegativeInteger(candidate.sourceNoteRevision) || candidate.sourceNoteRevision <= 0)
    ) ||
    (
      candidate.quote !== undefined &&
      cloneQuote(candidate.quote) === null
    )
  ) {
    return null
  }

  return {
    id: candidate.id,
    blockKey: candidate.blockKey,
    start: candidate.start,
    end: candidate.end,
    text: candidate.text,
    note: candidate.note,
    source: candidate.source,
    createdAt: candidate.createdAt,
    updatedAt: candidate.updatedAt,
    ...(candidate.sourceContentRevision === undefined
      ? {}
      : { sourceContentRevision: candidate.sourceContentRevision }),
    ...(candidate.sourceSummaryHash === undefined
      ? {}
      : { sourceSummaryHash: candidate.sourceSummaryHash }),
    ...(candidate.sourceNoteRevision === undefined
      ? {}
      : { sourceNoteRevision: candidate.sourceNoteRevision }),
    ...(candidate.quote === undefined
      ? {}
      : { quote: cloneQuote(candidate.quote)! }),
  }
}

function timestamp(value: unknown): number | null {
  if (isSafeNonNegativeInteger(value)) return value
  if (typeof value !== 'string' || value.trim() === '') return null
  const parsed = Date.parse(value)
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : null
}

function revision(value: unknown): number | undefined {
  return isSafeNonNegativeInteger(value) && value > 0 ? value : undefined
}

/**
 * Reads the two annotation representations used during the compatibility
 * window. The result is deliberately still the local Annotation shape; wire
 * target binding remains the caller's responsibility.
 */
export function decodeAnnotationWire(candidate: unknown): Annotation | null {
  const direct = cloneAnnotation(candidate)
  if (direct) return direct
  if (!isRecord(candidate)) return null

  const nested = isRecord(candidate.annotation) ? candidate.annotation : undefined
  const source = nested ?? candidate
  const rangeCandidate = firstDefined(source, ['range', 'selection']) ??
    firstDefined(candidate, ['range', 'selection'])
  const range = isRecord(rangeCandidate)
    ? rangeCandidate
    : undefined
  const quoteCandidate = firstDefined(source, ['quote', 'selector']) ??
    firstDefined(candidate, ['quote', 'selector'])
  const quote = quoteCandidate === undefined
    ? cloneQuote(source)
    : cloneQuote(quoteCandidate)
  const quoteRecord = isRecord(quoteCandidate) ? quoteCandidate : undefined
  const identityCandidate = firstDefined(source, ['sourceIdentity', 'source_identity']) ??
    firstDefined(candidate, ['sourceIdentity', 'source_identity'])
  const sourceIdentity = isRecord(identityCandidate) ? identityCandidate : undefined
  const id = firstDefined(source, ['id', 'annotation_id', 'annotationId']) ??
    firstDefined(candidate, ['id', 'annotation_id', 'annotationId'])
  const blockKey = firstDefined(source, ['blockKey', 'block_key']) ??
    firstDefined(candidate, ['blockKey', 'block_key']) ??
    (quoteRecord && 'block_key' in quoteRecord
      ? quoteRecord.block_key
      : undefined)
  const start = firstDefined(source, ['start', 'start_offset', 'startOffset']) ??
    firstDefined(candidate, ['start', 'start_offset', 'startOffset']) ??
    (range ? firstDefined(range, ['start', 'start_offset', 'startOffset']) : undefined) ??
    (quoteRecord ? firstDefined(quoteRecord, ['start', 'start_offset', 'startOffset']) : undefined)
  const end = firstDefined(source, ['end', 'end_offset', 'endOffset']) ??
    firstDefined(candidate, ['end', 'end_offset', 'endOffset']) ??
    (range ? firstDefined(range, ['end', 'end_offset', 'endOffset']) : undefined) ??
    (quoteRecord ? firstDefined(quoteRecord, ['end', 'end_offset', 'endOffset']) : undefined)
  const text = firstDefined(source, ['text', 'exact', 'selected_text']) ?? quote?.exact
  const note = firstDefined(source, ['note', 'body', 'thought']) ??
    firstDefined(candidate, ['note', 'body', 'thought']) ?? ''
  const sourceValue = firstDefined(source, ['source', 'origin']) ??
    firstDefined(candidate, ['source', 'origin']) ?? 'self'
  const createdAt = timestamp(firstDefined(source, ['createdAt', 'created_at', 'created'])) ??
    timestamp(firstDefined(candidate, ['createdAt', 'created_at', 'created']))
  const updatedAt = timestamp(firstDefined(source, ['updatedAt', 'updated_at', 'updated'])) ??
    timestamp(firstDefined(candidate, ['updatedAt', 'updated_at', 'updated']))
  const contentRevision = revision(firstDefined(source, [
    'sourceContentRevision',
    'source_content_revision',
    'contentRevision',
    'content_revision',
  ]) ?? firstDefined(sourceIdentity ?? {}, [
    'sourceContentRevision',
    'source_content_revision',
    'contentRevision',
    'content_revision',
  ]) ?? firstDefined(candidate, [
    'sourceContentRevision',
    'source_content_revision',
    'contentRevision',
    'content_revision',
  ]))
  const sourceHash = firstDefined(source, [
    'sourceSummaryHash',
    'source_summary_hash',
    'summarySourceHash',
    'summary_source_hash',
  ]) ?? firstDefined(sourceIdentity ?? {}, [
    'sourceSummaryHash',
    'source_summary_hash',
    'summarySourceHash',
    'summary_source_hash',
  ]) ?? firstDefined(candidate, [
    'sourceSummaryHash',
    'source_summary_hash',
    'summarySourceHash',
    'summary_source_hash',
  ])
  const noteRevision = revision(firstDefined(source, [
    'sourceNoteRevision',
    'source_note_revision',
    'noteRevision',
    'note_revision',
  ]) ?? firstDefined(sourceIdentity ?? {}, [
    'sourceNoteRevision',
    'source_note_revision',
    'noteRevision',
    'note_revision',
  ]) ?? firstDefined(candidate, [
    'sourceNoteRevision',
    'source_note_revision',
    'noteRevision',
    'note_revision',
  ]))

  if (
    typeof id !== 'string' || id.length === 0 ||
    typeof blockKey !== 'string' || blockKey.length === 0 ||
    !isSafeNonNegativeInteger(start) || !isSafeNonNegativeInteger(end) ||
    end < start || typeof text !== 'string' || typeof note !== 'string' ||
    (sourceValue !== 'self' && sourceValue !== 'ai' && sourceValue !== 'user') ||
    createdAt === null || updatedAt === null ||
    (quoteCandidate !== undefined && quote === null) ||
    (sourceHash !== undefined && typeof sourceHash !== 'string')
  ) return null

  return cloneAnnotation({
    id,
    blockKey,
    start,
    end,
    text,
    note,
    source: sourceValue === 'ai' ? 'ai' : 'self',
    createdAt,
    updatedAt,
    ...(contentRevision === undefined ? {} : { sourceContentRevision: contentRevision }),
    ...(typeof sourceHash !== 'string' ? {} : { sourceSummaryHash: sourceHash }),
    ...(noteRevision === undefined ? {} : { sourceNoteRevision: noteRevision }),
    ...(quote === null ? {} : { quote }),
  })
}

function bindClonedAnnotation(
  annotation: Annotation,
  target: AnnotationTarget,
): Annotation | null {
  const base: Annotation = {
    id: annotation.id,
    blockKey: annotation.blockKey,
    start: annotation.start,
    end: annotation.end,
    text: annotation.text,
    note: annotation.note,
    source: annotation.source,
    createdAt: annotation.createdAt,
    updatedAt: annotation.updatedAt,
    ...(annotation.quote === undefined ? {} : { quote: annotation.quote }),
  }
  switch (target.kind) {
    case 'saved-content':
      if (!isSavedContentAnnotationBlockKey(base.blockKey)) return null
      return { ...base, sourceContentRevision: target.contentRevision }
    case 'summary':
      if (base.blockKey !== 'summary') return null
      return { ...base, sourceSummaryHash: target.sourceHash }
    case 'note':
      if (!isNonEmptyString(base.blockKey)) return null
      return { ...base, sourceNoteRevision: target.noteRevision }
    case 'legacy-stale':
      return base
  }
}

/** Rebinds a decoded legacy annotation to migration-owned source identity. */
export function bindAnnotationToTarget(
  candidate: unknown,
  target: AnnotationTarget,
): Annotation | null {
  const canonicalTarget = canonicalAnnotationTarget(target)
  const annotation = cloneAnnotation(candidate)
  if (!canonicalTarget || !annotation) return null
  return bindClonedAnnotation(annotation, canonicalTarget)
}

/** Decodes only annotations whose persisted binding already matches the target. */
export function cloneTargetAnnotation(
  candidate: unknown,
  target: AnnotationTarget,
): Annotation | null {
  const annotation = cloneAnnotation(candidate)
  const canonicalTarget = canonicalAnnotationTarget(target)
  if (!annotation || !canonicalTarget) return null
  const bound = bindClonedAnnotation(annotation, canonicalTarget)
  if (!bound || !annotationsEqual(annotation, bound)) return null
  return bound
}

/** Materializes a caller draft while deriving every source-owned field from the target. */
export function annotationFromAddDraft(
  candidate: AnnotationAddDraft,
  target: AnnotationTarget,
): Annotation | null {
  if (!isRecord(candidate)) return null
  if (
    !isNonEmptyString(candidate.id) ||
    !isSafeNonNegativeInteger(candidate.start) ||
    !isSafeNonNegativeInteger(candidate.end) ||
    candidate.end < candidate.start ||
    typeof candidate.text !== 'string' ||
    typeof candidate.note !== 'string' ||
    (candidate.source !== 'self' && candidate.source !== 'ai' && candidate.source !== 'user') ||
    !isSafeNonNegativeInteger(candidate.createdAt) ||
    !isSafeNonNegativeInteger(candidate.updatedAt)
  ) {
    return null
  }

  const canonicalTarget = canonicalAnnotationTarget(target)
  if (!canonicalTarget) return null
  const quote = candidate.quote === undefined ? undefined : cloneQuote(candidate.quote)
  if (candidate.quote !== undefined && quote === null) return null
  const normalizedQuote = quote ?? undefined
  let blockKey: unknown
  switch (canonicalTarget.kind) {
    case 'saved-content':
    case 'legacy-stale':
      blockKey = 'blockKey' in candidate ? candidate.blockKey : undefined
      break
    case 'summary':
      blockKey = 'summary'
      break
    case 'note':
      blockKey = 'blockKey' in candidate && isNonEmptyString(candidate.blockKey)
        ? candidate.blockKey
        : 'note'
      break
  }
  if (!isNonEmptyString(blockKey)) return null

  return bindClonedAnnotation({
    id: candidate.id,
    blockKey,
    start: candidate.start,
    end: candidate.end,
    text: candidate.text,
    note: candidate.note,
    source: candidate.source,
    createdAt: candidate.createdAt,
    updatedAt: candidate.updatedAt,
    ...(normalizedQuote === undefined ? {} : { quote: normalizedQuote }),
  }, canonicalTarget)
}

/** Stable field order used by operation receipts, imports, equality, and migration tie-breaks. */
export function annotationSignatureFields(annotation: Annotation): readonly unknown[] {
  return [
    annotation.id,
    annotation.blockKey,
    annotation.start,
    annotation.end,
    annotation.text,
    annotation.note,
    annotation.source,
    annotation.createdAt,
    annotation.updatedAt,
    annotation.sourceContentRevision ?? null,
    annotation.sourceSummaryHash ?? null,
    annotation.sourceNoteRevision ?? null,
    annotation.quote?.exact ?? null,
    annotation.quote?.prefix ?? null,
    annotation.quote?.suffix ?? null,
  ]
}

export function annotationsEqual(left: Annotation, right: Annotation): boolean {
  const leftFields = annotationSignatureFields(left)
  const rightFields = annotationSignatureFields(right)
  return leftFields.every((value, index) => value === rightFields[index])
}
