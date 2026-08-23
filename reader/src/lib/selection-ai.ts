import type { AnnotationLocator } from './annotation-domain'
import type { ChatMessage } from './ai'
import type { LinkResponse, ReaderAIRequest } from './api/types'

export type SelectionAISource =
  | {
      readonly type: 'link'
      readonly hostId: string
      readonly version: string
      readonly start: number
      readonly end: number
    }
  | {
      readonly type: 'note'
      readonly hostId: string
      readonly revision: number
      readonly start: number
      readonly end: number
    }

export interface SelectionAIDraft {
  readonly annotation: AnnotationLocator
  readonly text: string
  readonly nonce: number
  /** Omitted by legacy link callers; their `link` prop remains authoritative. */
  readonly source?: SelectionAISource
}

function noteSourceMetadata(source: Extract<SelectionAISource, { readonly type: 'note' }>): string {
  return JSON.stringify({
    source_type: 'note',
    host_id: source.hostId,
    version: { note_revision: source.revision },
    range: { start: source.start, end: source.end },
  })
}

export function selectionAISourceKey(
  link: LinkResponse | null | undefined,
  draft: SelectionAIDraft | null | undefined,
): string {
  const source = draft?.source
  if (source?.type === 'note') return `note:${source.hostId}:${source.revision}`
  return `link:${source?.type === 'link' ? source.hostId : link?.id ?? 'none'}`
}

export function buildSelectionAIRequest(
  history: readonly ChatMessage[],
  link: LinkResponse | null | undefined,
  draft: SelectionAIDraft | null | undefined,
): ReaderAIRequest {
  const source = draft?.source
  const title = source?.type === 'note' ? '' : link ? link.title || link.url : ''
  const conversation = history
    .filter((message) => !message.typing)
    .map((message) => `${message.role === 'user' ? '用户' : '助手'}：${message.text}`)
    .join('\n')
  const metadata = source?.type === 'note'
    ? `Selection source metadata: ${noteSourceMetadata(source)}`
    : ''
  const prompt = [
    '你是一位中文阅读助手。请基于给定的阅读上下文回答用户问题，回答具体、简洁，使用纯文本，不要编造上下文中没有的信息。',
    title ? `当前链接：${title}` : '',
    metadata,
    conversation ? `对话：\n${conversation}` : '请回答用户的问题。',
  ]
    .filter(Boolean)
    .join('\n\n')

  const selectedText = draft?.text.trim() ? draft.text : undefined
  const request: ReaderAIRequest = {
    prompt,
    scope: selectedText ? 'selection' : 'general',
  }
  if (source?.type !== 'note' && link?.id) request.link_id = link.id
  if (selectedText) request.selected_text = selectedText
  return request
}
