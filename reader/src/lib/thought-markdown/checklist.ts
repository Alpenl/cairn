export interface ChecklistBlock {
  readonly blockRef: string
  readonly text: string
  readonly done: boolean
  readonly occurrence: number
  readonly lineStart: number
  readonly lineEnd: number
}

export type ChecklistUpdateResult =
  | { readonly status: 'updated'; readonly source: string; readonly block: ChecklistBlock }
  | { readonly status: 'not-found' }
  | { readonly status: 'ambiguous' }
  | { readonly status: 'conflict'; readonly expectedDone: boolean; readonly actualDone: boolean }

export interface ChecklistHostSnapshot {
  readonly source: string
  readonly revision: number
}

export interface ChecklistHostWriteRequest {
  readonly source: string
  readonly expectedRevision: number
}

export type ChecklistHostWriteResult =
  | { readonly status: 'updated'; readonly source: string; readonly revision: number }
  | { readonly status: 'conflict'; readonly source?: string; readonly revision?: number }

export interface ChecklistHost {
  readonly read: () => ChecklistHostSnapshot | Promise<ChecklistHostSnapshot>
  readonly write: (
    request: ChecklistHostWriteRequest,
  ) => ChecklistHostWriteResult | Promise<ChecklistHostWriteResult>
}

export interface ChecklistHostUpdateInput {
  readonly blockRef: string
  readonly desiredDone: boolean
  readonly expectedDone: boolean
  readonly occurrence?: number
  /** Omit to use the revision read immediately before the update. */
  readonly expectedRevision?: number
}

export type ChecklistHostUpdateResult =
  | {
      readonly status: 'updated'
      readonly source: string
      readonly revision: number
      readonly block: ChecklistBlock
    }
  | { readonly status: 'not-found' }
  | { readonly status: 'ambiguous' }
  | {
      readonly status: 'conflict'
      readonly conflict: 'checkbox'
      readonly expectedDone: boolean
      readonly actualDone: boolean
    }
  | {
      readonly status: 'conflict'
      readonly conflict: 'host'
      readonly expectedRevision: number
      readonly actualRevision?: number
    }

interface SourceLine {
  readonly value: string
  readonly start: number
  readonly end: number
}

interface ParsedTask extends ChecklistBlock {
  readonly markerStart: number
}

const TASK_RE = /^(\s*)([-+*]|\d+[.)])\s+\[([ xX])\]\s+(.*?)\s*$/

function hash(value: string): string {
  // FNV-1a is deterministic, fast, and sufficient for a local source marker;
  // it is not used as a security boundary or a cross-device identity.
  let result = 2166136261
  for (let index = 0; index < value.length; index += 1) {
    result ^= value.charCodeAt(index)
    result = Math.imul(result, 16777619)
  }
  // Go's strconv.FormatUint does not pad hexadecimal output; keep the
  // browser anchor byte-for-byte compatible with the server implementation.
  return (result >>> 0).toString(16)
}

function linesOf(source: string): SourceLine[] {
  const lines: SourceLine[] = []
  let start = 0
  while (start < source.length) {
    const newline = source.indexOf('\n', start)
    const end = newline < 0 ? source.length : newline + 1
    const raw = source.slice(start, end)
    lines.push({ value: raw.endsWith('\n') ? raw.slice(0, -1).replace(/\r$/, '') : raw, start, end })
    start = end
  }
  if (source.length === 0) lines.push({ value: '', start: 0, end: 0 })
  return lines
}

function nearestHeading(lines: readonly SourceLine[], index: number): string {
  for (let cursor = index - 1; cursor >= 0; cursor -= 1) {
    const value = lines[cursor].value.trim()
    if (/^#{1,6} /.test(value)) return value
  }
  return ''
}

function blockRef(
  lines: readonly SourceLine[],
  index: number,
  text: string,
): string {
  const heading = nearestHeading(lines, index)
  // Keep this payload byte-for-byte compatible with internal/readertext. The
  // line number and following content are deliberately absent; occurrence
  // disambiguates repeats while nearby edits leave the anchor unchanged.
  return `task:${hash(JSON.stringify([text.trim(), heading]))}`
}

function parseTasks(source: string): ParsedTask[] {
  const lines = linesOf(source)
  const tasks: ParsedTask[] = []
  const occurrences = new Map<string, number>()
  let fence: { readonly marker: '`' | '~'; readonly length: number } | null = null
  lines.forEach((line, index) => {
    const fenceMatch = /^\s{0,3}(`{3,}|~{3,})/.exec(line.value)
    if (fenceMatch) {
      const marker = fenceMatch[1][0] as '`' | '~'
      if (!fence) fence = { marker, length: fenceMatch[1].length }
      else if (fence.marker === marker && fenceMatch[1].length >= fence.length) fence = null
      return
    }
    if (fence) return
    const match = TASK_RE.exec(line.value)
    if (!match) return
    const markerStart = match[1].length + match[2].length + 2
    const ref = blockRef(lines, index, match[4])
    const occurrence = (occurrences.get(ref) ?? 0) + 1
    occurrences.set(ref, occurrence)
    tasks.push({
      blockRef: ref,
      text: match[4],
      done: match[3].toLowerCase() === 'x',
      occurrence,
      lineStart: line.start,
      lineEnd: line.end,
      markerStart: line.start + markerStart,
    })
  })
  return tasks
}

export function listChecklistBlocks(source: string): readonly ChecklistBlock[] {
  return parseTasks(source).map((task) => toChecklistBlock(task))
}

function toChecklistBlock(task: ParsedTask): ChecklistBlock {
  return {
    blockRef: task.blockRef,
    text: task.text,
    done: task.done,
    occurrence: task.occurrence,
    lineStart: task.lineStart,
    lineEnd: task.lineEnd,
  }
}

export function updateChecklistState(
  source: string,
  blockRef: string,
  done: boolean,
  occurrence?: number,
  expectedDone?: boolean,
): ChecklistUpdateResult {
  const parsed = parseTasks(source)
  const normalizedOccurrence = occurrence !== undefined && occurrence <= 0 ? 1 : occurrence
  const matches = parsed.filter((task) => task.blockRef === blockRef)
  if (matches.length === 0) return { status: 'not-found' }
  const task = normalizedOccurrence === undefined
    ? matches.length === 1 ? matches[0] : undefined
    : matches.find((candidate) => candidate.occurrence === normalizedOccurrence)
  if (!task) return normalizedOccurrence === undefined ? { status: 'ambiguous' } : { status: 'not-found' }
  if (expectedDone !== undefined && task.done !== expectedDone) {
    return { status: 'conflict', expectedDone, actualDone: task.done }
  }
  const markerOffset = task.markerStart
  const current = source[markerOffset]
  if (current !== ' ' && current !== 'x' && current !== 'X') return { status: 'not-found' }
  const nextSource = source.slice(0, markerOffset) + (done ? 'x' : ' ') + source.slice(markerOffset + 1)
  return {
    status: 'updated',
    source: nextSource,
    block: {
      ...toChecklistBlock(task),
      done,
    },
  }
}

/** Explicit desired-state form for callers that want to make the CAS intent visible. */
export function updateChecklistStateCAS(
  source: string,
  blockRef: string,
  expectedDone: boolean,
  desiredDone: boolean,
  occurrence?: number,
): ChecklistUpdateResult {
  return updateChecklistState(source, blockRef, desiredDone, occurrence, expectedDone)
}

/**
 * Couples source-level checklist CAS with the host's revision CAS. The host
 * owns persistence; this adapter owns locating exactly one task and producing
 * a desired-state source patch.
 */
export class HostChecklistAdapter {
  public constructor(private readonly host: ChecklistHost) {}

  public async list(): Promise<readonly ChecklistBlock[]> {
    const snapshot = await this.host.read()
    return listChecklistBlocks(snapshot.source)
  }

  public async update(input: ChecklistHostUpdateInput): Promise<ChecklistHostUpdateResult> {
    const snapshot = await this.host.read()
    if (input.expectedRevision !== undefined && snapshot.revision !== input.expectedRevision) {
      return {
        status: 'conflict',
        conflict: 'host',
        expectedRevision: input.expectedRevision,
        actualRevision: snapshot.revision,
      }
    }

    const result = updateChecklistStateCAS(
      snapshot.source,
      input.blockRef,
      input.expectedDone,
      input.desiredDone,
      input.occurrence,
    )
    if (result.status === 'not-found' || result.status === 'ambiguous') return result
    if (result.status === 'conflict') {
      return { ...result, conflict: 'checkbox' }
    }
    if (result.source === snapshot.source) {
      return {
        status: 'updated',
        source: snapshot.source,
        revision: snapshot.revision,
        block: result.block,
      }
    }

    const write = await this.host.write({
      source: result.source,
      expectedRevision: snapshot.revision,
    })
    if (write.status === 'conflict') {
      return {
        status: 'conflict',
        conflict: 'host',
        expectedRevision: snapshot.revision,
        ...(write.revision === undefined ? {} : { actualRevision: write.revision }),
      }
    }
    return { ...write, block: result.block }
  }

  public setDone(
    blockRef: string,
    expectedDone: boolean,
    desiredDone: boolean,
    occurrence?: number,
    expectedRevision?: number,
  ): Promise<ChecklistHostUpdateResult> {
    return this.update({
      blockRef,
      expectedDone,
      desiredDone,
      ...(occurrence === undefined ? {} : { occurrence }),
      ...(expectedRevision === undefined ? {} : { expectedRevision }),
    })
  }
}
