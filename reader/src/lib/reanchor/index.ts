export interface ReanchorTarget {
  readonly kind: string
  readonly hostId: string
  readonly version: string
}

export interface ReanchorQuote {
  readonly exact: string
  readonly prefix: string
  readonly suffix: string
}

export interface ReanchorRange {
  readonly start: number
  readonly end: number
}

export interface ReanchorAnnotation {
  readonly id: string
  readonly blockKey: string
  readonly range: ReanchorRange
  readonly quote: ReanchorQuote
  readonly target: ReanchorTarget
  readonly thought: string
}

export type ReanchorReason =
  | 'diff-context'
  | 'unique-quote'
  | 'ambiguous-quote'
  | 'missing-quote'
  | 'deleted-range'

export type ReanchorResult =
  | {
      readonly status: 'reanchored'
      readonly reason: 'diff-context' | 'unique-quote'
      readonly annotation: ReanchorAnnotation
    }
  | {
      readonly status: 'historical'
      readonly reason: 'ambiguous-quote' | 'missing-quote' | 'deleted-range'
      readonly annotation: ReanchorAnnotation
    }

const DEFAULT_CONTEXT = 32

function validRange(range: ReanchorRange, length: number): boolean {
  return Number.isSafeInteger(range.start) &&
    Number.isSafeInteger(range.end) &&
    range.start >= 0 &&
    range.end > range.start &&
    range.end <= length
}

export function buildReanchorQuote(
  source: string,
  range: ReanchorRange,
  context = DEFAULT_CONTEXT,
): ReanchorQuote {
  if (!validRange(range, source.length)) throw new Error('reanchor range is outside the source')
  const boundedContext = Math.max(0, Math.floor(context))
  return {
    exact: source.slice(range.start, range.end),
    prefix: source.slice(Math.max(0, range.start - boundedContext), range.start),
    suffix: source.slice(range.end, Math.min(source.length, range.end + boundedContext)),
  }
}

function occurrences(source: string, needle: string): number[] {
  if (needle === '') return []
  const result: number[] = []
  let offset = 0
  while (offset <= source.length - needle.length) {
    const next = source.indexOf(needle, offset)
    if (next < 0) break
    result.push(next)
    offset = next + 1
  }
  return result
}

function matchesContext(
  source: string,
  index: number,
  quote: ReanchorQuote,
): boolean {
  const prefixMatches = quote.prefix === '' || (
    index >= quote.prefix.length &&
    source.slice(index - quote.prefix.length, index) === quote.prefix
  )
  const suffixStart = index + quote.exact.length
  const suffixMatches = quote.suffix === '' ||
    source.slice(suffixStart, suffixStart + quote.suffix.length) === quote.suffix
  return prefixMatches && suffixMatches
}

/**
 * Maps a changed selection through a unique pair of surrounding context
 * anchors. This is intentionally conservative: a missing or repeated anchor
 * is a historical result, never a guessed attachment.
 */
function diffContextCandidates(
  currentSource: string,
  quote: ReanchorQuote,
): ReanchorRange[] {
  if (quote.prefix === '' || quote.suffix === '') return []
  const before = occurrences(currentSource, quote.prefix)
  const after = occurrences(currentSource, quote.suffix)
  const pairs: ReanchorRange[] = []
  for (const prefixIndex of before) {
    const start = prefixIndex + quote.prefix.length
    for (const suffixIndex of after) {
      if (suffixIndex < start) continue
      const candidate = { start, end: suffixIndex }
      if (candidate.end <= candidate.start) continue
      pairs.push(candidate)
    }
  }
  return pairs
}

function mapByDiffContext(
  previousSource: string,
  currentSource: string,
  range: ReanchorRange,
  quote: ReanchorQuote,
): ReanchorRange | null {
  if (!validRange(range, previousSource.length) ||
    !matchesContext(previousSource, range.start, quote)) return null
  const pairs = diffContextCandidates(currentSource, quote)
  return pairs.length === 1 ? pairs[0] : null
}

function withRange(
  annotation: ReanchorAnnotation,
  source: string,
  range: ReanchorRange,
  target: ReanchorTarget,
): ReanchorAnnotation {
  return {
    ...annotation,
    target,
    range,
    quote: buildReanchorQuote(source, range),
  }
}

export function reanchorAnnotation(
  annotation: ReanchorAnnotation,
  previousSource: string,
  currentSource: string,
  currentTarget: ReanchorTarget = annotation.target,
): ReanchorResult {
  if (!validRange(annotation.range, previousSource.length)) {
    return { status: 'historical', reason: 'missing-quote', annotation }
  }
  const quote = annotation.quote.exact
    ? annotation.quote
    : buildReanchorQuote(previousSource, annotation.range)

  const exactMatches = occurrences(currentSource, quote.exact)
  if (exactMatches.length === 0) {
    const mapped = mapByDiffContext(previousSource, currentSource, annotation.range, quote)
    if (mapped) {
      return {
        status: 'reanchored',
        reason: 'diff-context',
        annotation: withRange(annotation, currentSource, mapped, currentTarget),
      }
    }
    return {
      status: 'historical',
      reason: quote.exact === ''
        ? 'deleted-range'
        : diffContextCandidates(currentSource, quote).length > 1
          ? 'ambiguous-quote'
          : 'missing-quote',
      annotation,
    }
  }

  const hasContext = quote.prefix !== '' || quote.suffix !== ''
  const contextualMatches = exactMatches.filter((index) => matchesContext(currentSource, index, quote))
  if (hasContext && contextualMatches.length === 0) {
    if (exactMatches.length === 1 &&
      // The old quote must be internally coherent before a unique exact
      // match can survive context loss. Once the exact text is unique, a
      // deleted adjacent line or boundary is not enough to make the
      // annotation historical.
      matchesContext(previousSource, annotation.range.start, quote)) {
      const start = exactMatches[0]
      return {
        status: 'reanchored',
        reason: 'diff-context',
        annotation: withRange(
          annotation,
          currentSource,
          { start, end: start + quote.exact.length },
          currentTarget,
        ),
      }
    }
    return {
      status: 'historical',
      reason: exactMatches.length > 1 ? 'ambiguous-quote' : 'missing-quote',
      annotation,
    }
  }
  const candidates = hasContext ? contextualMatches : exactMatches
  if (candidates.length !== 1) {
    return { status: 'historical', reason: 'ambiguous-quote', annotation }
  }
  const start = candidates[0]
  const range = { start, end: start + quote.exact.length }
  return {
    status: 'reanchored',
    reason: 'unique-quote',
    annotation: withRange(annotation, currentSource, range, currentTarget),
  }
}
