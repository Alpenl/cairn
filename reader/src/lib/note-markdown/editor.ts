export interface MarkdownSelection {
  readonly start: number
  readonly end: number
}

export interface MarkdownEdit extends MarkdownSelection {
  readonly text: string
  readonly handled: boolean
}

export type MarkdownFormat = 'bold' | 'italic'

export type SlashCommandID =
  | 'h1'
  | 'h2'
  | 'h3'
  | 'unordered-list'
  | 'ordered-list'
  | 'task-list'
  | 'quote'
  | 'code-block'
  | 'divider'

export interface SlashCommand {
  readonly id: SlashCommandID
  readonly label: string
  readonly keywords: readonly string[]
  readonly replacement: string
  readonly caretOffset?: number
}

export interface SlashQuery {
  readonly query: string
  readonly start: number
  readonly end: number
}

export const NOTE_SLASH_COMMANDS: readonly SlashCommand[] = Object.freeze([
  { id: 'h1', label: '一级标题', keywords: ['h1', 'heading 1', 'title', '标题'], replacement: '# ' },
  { id: 'h2', label: '二级标题', keywords: ['h2', 'heading 2', 'subtitle', '标题'], replacement: '## ' },
  { id: 'h3', label: '三级标题', keywords: ['h3', 'heading 3', 'subtitle', '标题'], replacement: '### ' },
  { id: 'unordered-list', label: '无序列表', keywords: ['ul', 'bullet', 'list', '列表'], replacement: '- ' },
  { id: 'ordered-list', label: '有序列表', keywords: ['ol', 'number', 'list', '列表'], replacement: '1. ' },
  { id: 'task-list', label: '任务列表', keywords: ['task', 'todo', 'check', '任务'], replacement: '- [ ] ' },
  { id: 'quote', label: '引用', keywords: ['quote', 'blockquote', '引用'], replacement: '> ' },
  {
    id: 'code-block',
    label: '代码块',
    keywords: ['code', 'fence', '代码'],
    replacement: '```\n\n```',
    caretOffset: 4,
  },
  { id: 'divider', label: '分隔线', keywords: ['divider', 'rule', 'hr', '分隔'], replacement: '---' },
])

interface LineBounds {
  readonly start: number
  readonly end: number
}

interface ParsedListLine {
  readonly kind: 'unordered' | 'ordered' | 'task'
  readonly indent: string
  readonly marker: string
  readonly bodyStart: number
  readonly body: string
}

interface Replacement {
  readonly start: number
  readonly end: number
  readonly value: string
}

function clampSelection(text: string, selection: MarkdownSelection): MarkdownSelection {
  const start = Math.max(0, Math.min(text.length, Math.floor(selection.start)))
  const end = Math.max(start, Math.min(text.length, Math.floor(selection.end)))
  return { start, end }
}

function lineBoundsAt(text: string, offset: number): LineBounds {
  const bounded = Math.max(0, Math.min(text.length, offset))
  const previousBreak = text.lastIndexOf('\n', Math.max(0, bounded - 1))
  const nextBreak = text.indexOf('\n', bounded)
  return {
    start: previousBreak < 0 ? 0 : previousBreak + 1,
    end: nextBreak < 0 ? text.length : nextBreak,
  }
}

function parseListLine(line: string): ParsedListLine | null {
  const task = /^([ ]*)([-+*])\s+\[([ xX])\]\s+(.*)$/.exec(line)
  if (task) {
    const marker = `${task[2]} [${task[3]}] `
    return {
      kind: 'task',
      indent: task[1],
      marker,
      bodyStart: task[1].length + marker.length,
      body: task[4],
    }
  }

  const ordered = /^([ ]*)(\d+)([.)])\s+(.*)$/.exec(line)
  if (ordered) {
    const marker = `${ordered[2]}${ordered[3]} `
    return {
      kind: 'ordered',
      indent: ordered[1],
      marker,
      bodyStart: ordered[1].length + marker.length,
      body: ordered[4],
    }
  }

  const unordered = /^([ ]*)([-+*])\s+(.*)$/.exec(line)
  if (!unordered) return null
  const marker = `${unordered[2]} `
  return {
    kind: 'unordered',
    indent: unordered[1],
    marker,
    bodyStart: unordered[1].length + marker.length,
    body: unordered[3],
  }
}

function nextListMarker(line: ParsedListLine): string {
  if (line.kind === 'task') return `${line.indent}${line.marker[0]} [ ] `
  if (line.kind === 'unordered') return `${line.indent}${line.marker}`
  const match = /^(\d+)([.)]) /.exec(line.marker)
  if (!match) return `${line.indent}${line.marker}`
  return `${line.indent}${Number(match[1]) + 1}${match[2]} `
}

export function continueMarkdownList(
  text: string,
  selection: MarkdownSelection,
): MarkdownEdit {
  const current = clampSelection(text, selection)
  if (current.start !== current.end) return { text, ...current, handled: false }

  const bounds = lineBoundsAt(text, current.start)
  const line = text.slice(bounds.start, bounds.end)
  const parsed = parseListLine(line)
  const offsetInLine = current.start - bounds.start
  if (!parsed || offsetInLine < parsed.bodyStart) {
    return { text, ...current, handled: false }
  }

  if (parsed.body.trim() === '') {
    const nextText = text.slice(0, bounds.start) + text.slice(bounds.end)
    return { text: nextText, start: bounds.start, end: bounds.start, handled: true }
  }

  const marker = nextListMarker(parsed)
  const insertion = `\n${marker}`
  const suffixStart = text[current.start] === ' ' ? current.start + 1 : current.end
  const nextOffset = current.start + insertion.length
  return {
    text: text.slice(0, current.start) + insertion + text.slice(suffixStart),
    start: nextOffset,
    end: nextOffset,
    handled: true,
  }
}

function selectedLineBounds(text: string, selection: MarkdownSelection): LineBounds[] {
  const current = clampSelection(text, selection)
  const finalCoveredOffset = current.end > current.start ? current.end - 1 : current.end
  const first = lineBoundsAt(text, current.start)
  const last = lineBoundsAt(text, finalCoveredOffset)
  const lines: LineBounds[] = []
  let start = first.start
  while (start <= last.start) {
    const end = text.indexOf('\n', start)
    lines.push({ start, end: end < 0 ? text.length : end })
    if (end < 0 || end >= last.start) break
    start = end + 1
  }
  return lines
}

function replaceAll(text: string, replacements: readonly Replacement[]): string {
  let next = text
  for (const replacement of [...replacements].sort((left, right) => right.start - left.start)) {
    next = next.slice(0, replacement.start) + replacement.value + next.slice(replacement.end)
  }
  return next
}

function mapOffset(offset: number, replacements: readonly Replacement[]): number {
  let mapped = offset
  for (const replacement of [...replacements].sort((left, right) => left.start - right.start)) {
    const removed = replacement.end - replacement.start
    const inserted = replacement.value.length
    if (offset < replacement.start) break
    if (offset >= replacement.end) {
      mapped += inserted - removed
      continue
    }
    mapped = replacement.start + inserted
  }
  return mapped
}

export function indentMarkdownLists(
  text: string,
  selection: MarkdownSelection,
  direction: 'indent' | 'outdent',
): MarkdownEdit {
  const current = clampSelection(text, selection)
  const lines = selectedLineBounds(text, current)
  if (lines.length === 0 || lines.some((bounds) => !parseListLine(text.slice(bounds.start, bounds.end)))) {
    return { text, ...current, handled: false }
  }

  const replacements: Replacement[] = direction === 'indent'
    ? lines.map((bounds) => ({ start: bounds.start, end: bounds.start, value: '  ' }))
    : lines.flatMap((bounds) => {
        const line = text.slice(bounds.start, bounds.end)
        const removable = line.startsWith('  ') ? 2 : line.startsWith(' ') ? 1 : 0
        return removable > 0
          ? [{ start: bounds.start, end: bounds.start + removable, value: '' }]
          : []
      })

  if (replacements.length === 0) return { text, ...current, handled: true }
  return {
    text: replaceAll(text, replacements),
    start: mapOffset(current.start, replacements),
    end: mapOffset(current.end, replacements),
    handled: true,
  }
}

function starRunBefore(text: string, offset: number): number {
  let count = 0
  for (let index = offset - 1; index >= 0 && text[index] === '*'; index -= 1) count += 1
  return count
}

function starRunAfter(text: string, offset: number): number {
  let count = 0
  for (let index = offset; index < text.length && text[index] === '*'; index += 1) count += 1
  return count
}

function selectedMarkerWidth(value: string, format: MarkdownFormat): number {
  const before = starRunAfter(value, 0)
  const after = starRunBefore(value, value.length)
  if (format === 'bold') return before >= 2 && after >= 2 ? 2 : 0
  return before % 2 === 1 && after % 2 === 1 ? 1 : 0
}

function surroundingMarkerWidth(
  text: string,
  selection: MarkdownSelection,
  format: MarkdownFormat,
): number {
  const before = starRunBefore(text, selection.start)
  const after = starRunAfter(text, selection.end)
  if (format === 'bold') return before >= 2 && after >= 2 ? 2 : 0
  return before % 2 === 1 && after % 2 === 1 ? 1 : 0
}

export function toggleMarkdownFormat(
  text: string,
  selection: MarkdownSelection,
  format: MarkdownFormat,
): MarkdownEdit {
  const current = clampSelection(text, selection)
  const marker = format === 'bold' ? '**' : '*'
  const selected = text.slice(current.start, current.end)
  const insideWidth = current.start === current.end ? 0 : selectedMarkerWidth(selected, format)

  if (insideWidth > 0) {
    const inner = selected.slice(insideWidth, selected.length - insideWidth)
    return {
      text: text.slice(0, current.start) + inner + text.slice(current.end),
      start: current.start,
      end: current.end - insideWidth * 2,
      handled: true,
    }
  }

  const outsideWidth = surroundingMarkerWidth(text, current, format)
  if (outsideWidth > 0) {
    return {
      text: text.slice(0, current.start - outsideWidth) +
        selected +
        text.slice(current.end + outsideWidth),
      start: current.start - outsideWidth,
      end: current.end - outsideWidth,
      handled: true,
    }
  }

  if (current.start === current.end) {
    const pair = marker + marker
    const caret = current.start + marker.length
    return {
      text: text.slice(0, current.start) + pair + text.slice(current.end),
      start: caret,
      end: caret,
      handled: true,
    }
  }

  return {
    text: text.slice(0, current.start) + marker + selected + marker + text.slice(current.end),
    start: current.start + marker.length,
    end: current.end + marker.length,
    handled: true,
  }
}

export function slashQueryAt(
  text: string,
  selection: MarkdownSelection,
): SlashQuery | null {
  const current = clampSelection(text, selection)
  if (current.start !== current.end) return null
  const bounds = lineBoundsAt(text, current.start)
  if (current.start !== bounds.end) return null
  const beforeCaret = text.slice(bounds.start, current.start)
  const match = /^([ ]*)\/([^\s/]*)$/.exec(beforeCaret)
  if (!match) return null
  const slashStart = bounds.start + match[1].length
  return { query: match[2], start: slashStart, end: current.start }
}

export function filterSlashCommands(query: string): readonly SlashCommand[] {
  const normalized = query.trim().toLocaleLowerCase()
  if (!normalized) return NOTE_SLASH_COMMANDS
  return NOTE_SLASH_COMMANDS.filter((command) =>
    command.id.includes(normalized) ||
    command.label.toLocaleLowerCase().includes(normalized) ||
    command.keywords.some((keyword) => keyword.toLocaleLowerCase().includes(normalized)),
  )
}

export function applySlashCommand(
  text: string,
  query: SlashQuery,
  command: SlashCommand,
): MarkdownEdit {
  const start = Math.max(0, Math.min(text.length, query.start))
  const end = Math.max(start, Math.min(text.length, query.end))
  const caret = start + (command.caretOffset ?? command.replacement.length)
  return {
    text: text.slice(0, start) + command.replacement + text.slice(end),
    start: caret,
    end: caret,
    handled: true,
  }
}
