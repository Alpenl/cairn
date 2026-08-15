/**
 * AI 助手侧栏。
 *
 * 两模式：
 *   · 一般助手：围绕当前链接多轮提问（ctx-chip 头）。
 *   · 划线问 AI 草稿模式：消费 chatDraft（划线 → 自动首轮），多轮追问后把满意的
 *     回复「采用为笔记」（onAdopt 写回 annotation.note，source:'ai'）。
 *
 * 快捷提问 chips 随模式切换；与 NotePanel 互斥同位（互斥逻辑在 MainView）。
 * AI 调用优先走 identity-bound ReaderClient；旧宿主没有该方法时才回退到
 * lib/ai.ts 的 window.claude bridge。后端明确返回 enabled=false 时，侧栏会
 * 锁定输入并显示 capability-off，而不是把空答案当成成功。
 * Reader API 的 link_id 是权威上下文入口；正文不会由浏览器重复拼进 prompt，
 * 避免客户端上下文与服务端 installation-scoped projection 不一致。
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { Icon, type IconName } from './Icon'
import { ChatMsg } from './ChatMsg'
import { aiAvailable, askAI, UNAVAILABLE_REPLY, type ChatMessage } from '../lib/ai'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { LinkResponse } from '../lib/api/types'
import type { AnnotationLocator } from '../lib/annotations'
import {
  buildSelectionAIRequest,
  selectionAISourceKey,
  type SelectionAIDraft,
} from '../lib/selection-ai'

/** 划线草稿（由 MainView 在「问 AI」时写入）。 */
export type ChatDraft = SelectionAIDraft

export interface ChatSidebarProps {
  client: IdentityBoundReaderClient
  link: LinkResponse | null | undefined
  /** 保留给旧宿主调用方的上下文参数；Reader API 会按 link_id 读取权威正文。 */
  contentContext?: string | null
  draft: ChatDraft | null
  /** 采用 AI 回复为划线笔记：写回该划线的 note（source 置 'ai'）。 */
  onAdopt: (annotation: AnnotationLocator, text: string) => void
  /** 退出草稿模式（清掉 MainView 的 chatDraft）。 */
  onClearDraft: () => void
  onClose: () => void
}

interface QuickChip {
  id: string
  label: string
  icon: IconName
  text: string
}

const GENERAL_QUICKS: QuickChip[] = [
  { id: 'summarize', label: '总结这条', icon: 'sparkles', text: '用三句话总结这个链接的要点。' },
  { id: 'related', label: '找相关链接', icon: 'layers', text: '库里有哪些和这条主题相近的链接？' },
  { id: 'deepen', label: '值得深挖什么', icon: 'explain', text: '如果要深入，这条链接最值得追问的问题是什么？' },
]

const DRAFT_QUICKS: QuickChip[] = [
  { id: 'shorter', label: '更精简', icon: 'edit', text: '请把上面的解读压缩成一句话，适合做笔记。' },
  { id: 'why', label: '为什么重要', icon: 'explain', text: '这段为什么重要？用一句话点出它的意义。' },
  { id: 'example', label: '举个例子', icon: 'sparkles', text: '用一个具体例子帮我理解这段。' },
]

const GREETING: ChatMessage = {
  role: 'ai',
  text: '嗨，我是你的链接库助手。划线后点「问 AI」可以就那段话多轮追问，满意的回复点「采用为笔记」即可存成划线笔记。',
}

const CAPABILITY_OFF_REPLY =
  'AI 助手当前未启用：当前部署没有可用的 AI 能力，因此不能生成真实回答。'
const IDENTITY_UNAVAILABLE_REPLY = 'AI 助手当前不可用：Reader 身份已失效，请重新连接。'
const REQUEST_FAILED_REPLY = 'AI 助手请求失败，请稍后重试。'
const REQUEST_TIMEOUT_REPLY = 'AI 助手请求超时，请稍后重试。'
const REQUEST_CANCELED_REPLY = 'AI 助手请求已取消。'
const EMPTY_REPLY = 'AI 助手没有返回内容，请重试。'

function isMissingReaderAIRoute(error: { status?: number; errorCode?: string }): boolean {
  if (error.status !== 404 && error.status !== 405 && error.status !== 501) return false
  return error.errorCode === undefined || error.errorCode === `default_${error.status}`
}

interface CompletedReply {
  text: string
  adoptable: boolean
}

function readerAIErrorReply(error: { kind?: string; status?: number; errorCode?: string }): string {
  if (error.kind === 'timeout' || error.status === 504 || error.errorCode === 'ai_timeout') {
    return REQUEST_TIMEOUT_REPLY
  }
  if (error.errorCode === 'ai_request_canceled' || error.status === 499) {
    return REQUEST_CANCELED_REPLY
  }
  return REQUEST_FAILED_REPLY
}

function isAdoptableAIAnswer(answer: string): boolean {
  const normalized = answer.trim()
  return normalized !== '' && normalized !== UNAVAILABLE_REPLY
}

export function ChatSidebar({
  client,
  link,
  draft,
  onAdopt,
  onClearDraft,
  onClose,
}: ChatSidebarProps) {
  const [msgs, setMsgs] = useState<ChatMessage[]>([GREETING])
  const [input, setInput] = useState('')
  const [capabilityOff, setCapabilityOff] = useState(false)
  const [draftCtx, setDraftCtx] = useState<ChatDraft | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)
  const linkRef = useRef(link)
  const draftRef = useRef<ChatDraft | null>(null)
  const msgsRef = useRef<ChatMessage[]>([GREETING])
  const lastNonce = useRef<number>(0)
  const requestSeq = useRef(0)
  const requestController = useRef<AbortController | null>(null)
  const sourceKey = selectionAISourceKey(link, draft)

  const hasReaderAI = typeof client.completeReaderAI === 'function'
  const clientIdentityCurrent = hasReaderAI && client.isIdentityCurrent()
  const legacyAvailable = !hasReaderAI && aiAvailable()
  const available = hasReaderAI
    ? clientIdentityCurrent && !capabilityOff
    : legacyAvailable

  const cancelPending = useCallback(() => {
    requestSeq.current += 1
    requestController.current?.abort()
    requestController.current = null
  }, [])

  useEffect(() => {
    linkRef.current = link
  }, [link])

  // A response from another host must never fill this sidebar. Note sessions
  // use their published revision as part of the source identity.
  useEffect(() => {
    cancelPending()
    const greeting = [GREETING]
    msgsRef.current = greeting
    setMsgs(greeting)
    setInput('')
    setCapabilityOff(false)
    setDraftCtx(null)
    draftRef.current = null
    // MainView clears an old draft when the active link changes. Starting the
    // nonce fence over also lets a new-link draft trigger its automatic turn.
    lastNonce.current = Number.MIN_SAFE_INTEGER
  }, [cancelPending, sourceKey])

  useEffect(() => () => cancelPending(), [cancelPending])

  const finishRequest = useCallback(
    (sequence: number, controller: AbortController, reply: CompletedReply | null) => {
      if (sequence !== requestSeq.current || controller.signal.aborted) return
      if (requestController.current === controller) requestController.current = null
      setMsgs((current) => {
        const typingIndex = current.map((message) => !!message.typing).lastIndexOf(true)
        if (typingIndex < 0) return current
        const next = [...current]
        if (reply) {
          next[typingIndex] = {
            role: 'ai',
            text: reply.text,
            adoptable: reply.adoptable,
          }
        } else {
          next.splice(typingIndex, 1)
        }
        msgsRef.current = next
        return next
      })
    },
    [],
  )

  const send = useCallback(
    (text: string) => {
      const trimmed = text.trim()
      if (!trimmed) return

      const sequence = ++requestSeq.current
      requestController.current?.abort()
      const controller = new AbortController()
      requestController.current = controller
      const linkSnapshot = linkRef.current
      const draftSnapshot = draftRef.current
      const history: ChatMessage[] = [
        ...msgsRef.current.filter((message) => !message.typing),
        { role: 'user', text: trimmed },
      ]
      const pending: ChatMessage[] = [...history, { role: 'ai', text: '', typing: true }]
      msgsRef.current = pending
      setMsgs(pending)

      if (hasReaderAI && !client.isIdentityCurrent()) {
        finishRequest(sequence, controller, {
          text: IDENTITY_UNAVAILABLE_REPLY,
          adoptable: false,
        })
        return
      }

      void (async () => {
        let reply: CompletedReply | null = null
        if (hasReaderAI) {
          try {
            const result = await client.completeReaderAI(
              buildSelectionAIRequest(
                history.filter((message) => message !== GREETING),
                linkSnapshot,
                draftSnapshot,
              ),
              { signal: controller.signal },
            )
            if (sequence !== requestSeq.current || controller.signal.aborted) return
            if (!client.isIdentityCurrent()) {
              finishRequest(sequence, controller, null)
              return
            }
            if (!result.ok) {
              if (result.error.kind === 'identity-mismatch') {
                finishRequest(sequence, controller, null)
                return
              }
              if (isMissingReaderAIRoute(result.error)) {
                setCapabilityOff(true)
                reply = { text: CAPABILITY_OFF_REPLY, adoptable: false }
                finishRequest(sequence, controller, reply)
                return
              }
              reply = { text: readerAIErrorReply(result.error), adoptable: false }
            } else if (!result.data.enabled) {
              setCapabilityOff(true)
              reply = { text: CAPABILITY_OFF_REPLY, adoptable: false }
            } else {
              const answer = result.data.answer.trim()
              reply = {
                text: answer || EMPTY_REPLY,
                adoptable: isAdoptableAIAnswer(answer),
              }
            }
          } catch {
            if (sequence !== requestSeq.current || controller.signal.aborted) return
            if (!client.isIdentityCurrent()) {
              finishRequest(sequence, controller, null)
              return
            }
            reply = { text: REQUEST_FAILED_REPLY, adoptable: false }
          }
        } else {
          const answer = await askAI(history, linkSnapshot, draftSnapshot?.text, { signal: controller.signal })
          if (sequence !== requestSeq.current || controller.signal.aborted) return
          reply = { text: answer, adoptable: isAdoptableAIAnswer(answer) }
        }
        finishRequest(sequence, controller, reply)
      })()
    },
    [client, finishRequest, hasReaderAI],
  )

  // 新划线草稿到达 → 进入记笔记模式并自动发起首轮（nonce 去重，避免重复触发）。
  useEffect(() => {
    if (!draft) {
      cancelPending()
      setDraftCtx(null)
      draftRef.current = null
      return
    }
    if (draft.nonce === lastNonce.current) return
    setDraftCtx(draft)
    draftRef.current = draft
    // React StrictMode runs mount effects through a setup/cleanup/setup cycle.
    // Defer the automatic turn so the discarded setup can cancel its timer
    // before any request is sent; record the nonce only when the turn starts.
    const timer = window.setTimeout(() => {
      if (draft.nonce === lastNonce.current) return
      lastNonce.current = draft.nonce
      send('请帮我理解这段划线，并给一句可作为笔记的精炼解读。')
    }, 0)
    return () => window.clearTimeout(timer)
  }, [cancelPending, draft, send])

  // 新消息滚到底。
  useEffect(() => {
    if (scrollRef.current) scrollRef.current.scrollTop = scrollRef.current.scrollHeight
  }, [msgs])

  const submit = () => {
    const t = input.trim()
    if (!t) return
    setInput('')
    send(t)
  }

  const clearDraft = () => {
    cancelPending()
    setDraftCtx(null)
    draftRef.current = null
    onClearDraft()
  }

  const adopt = (text: string) => {
    if (!draftCtx) return
    if (hasReaderAI && !client.isIdentityCurrent()) return
    onAdopt(draftCtx.annotation, text)
    setMsgs((current) => {
      const next = [
        ...current,
        { role: 'ai' as const, text: '已存为这段划线的笔记 ✓ 你可以继续追问，或在正文/笔记列表里再编辑。' },
      ]
      msgsRef.current = next
      return next
    })
    clearDraft()
  }

  const unavailablePlaceholder = hasReaderAI
    ? capabilityOff
      ? CAPABILITY_OFF_REPLY
      : clientIdentityCurrent
        ? '问点什么…'
        : IDENTITY_UNAVAILABLE_REPLY
    : legacyAvailable
      ? '问点什么…'
      : UNAVAILABLE_REPLY

  const quicks = draftCtx ? DRAFT_QUICKS : GENERAL_QUICKS

  return (
    <aside className="chat">
      <div className="chat-head">
        <span className="chat-ttl">
          <Icon name="sparkles" size={15} style={{ color: 'var(--accent)' }} /> AI 助手
        </span>
        <span className="rt-grow" />
        <button
          className="tb-btn"
          style={{ minWidth: 28, height: 28 }}
          onClick={onClose}
          title="关闭"
        >
          <Icon name="close" size={15} />
        </button>
      </div>

      {draftCtx ? (
        <div className="draft-ctx">
          <span className="dc-ic">
            <Icon name="marker" size={14} />
          </span>
          <div className="dc-body">
            <div className="dc-label">为这段划线记笔记</div>
            <div className="dc-text">{draftCtx.text}</div>
          </div>
          <span className="dc-x" onClick={clearDraft} title="退出记笔记模式">
            <Icon name="close" size={14} />
          </span>
        </div>
      ) : link ? (
        <div className="ctx-chip">
          <Icon name="link" size={13} />
          <span className="ctx-name">基于：{link.title || link.url}</span>
        </div>
      ) : null}

      <div className="chat-scroll" ref={scrollRef}>
        {msgs.map((m, i) => (
          <ChatMsg key={i} m={m} canAdopt={!!draftCtx} onAdopt={adopt} />
        ))}
      </div>

      <div className="chat-quicks">
        {quicks.map((q) => (
          <button
            key={q.id}
            className="quick"
            disabled={!available}
            onClick={() => send(q.text)}
          >
            <Icon name={q.icon} size={13} /> {q.label}
          </button>
        ))}
      </div>

      <div className="chat-input">
        <textarea
          rows={1}
          value={input}
          disabled={!available}
          placeholder={available ? (draftCtx ? '继续追问，精炼这条笔记…' : '问点什么…') : unavailablePlaceholder}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault()
              submit()
            }
          }}
        />
        <button className="send-btn" onClick={submit} disabled={!available || !input.trim()}>
          <Icon name="send" size={16} />
        </button>
      </div>
    </aside>
  )
}
