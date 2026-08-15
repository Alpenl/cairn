/**
 * 单条对话消息。
 *
 * · user：右侧气泡。
 * · ai：头像 + 气泡（typing 三点动画 / renderRich 富文本），草稿模式下完成的 AI 回复
 *   带「采用为笔记」按钮。
 */
import { Fragment } from 'react'
import { Icon } from './Icon'
import type { ChatMessage } from '../lib/ai'

/** 行内 `**粗体**` 富文本渲染（对齐 chat.jsx renderRich）。 */
function renderRich(text: string) {
  return text.split('\n').map((line, i) => (
    <div key={i} style={{ minHeight: line ? 0 : 8 }}>
      {line
        .split(/(\*\*[^*]+\*\*)/g)
        .filter(Boolean)
        .map((p, j) =>
          p.startsWith('**') && p.endsWith('**') ? (
            <strong key={j}>{p.slice(2, -2)}</strong>
          ) : (
            <Fragment key={j}>{p}</Fragment>
          ),
        )}
    </div>
  ))
}

export interface ChatMsgProps {
  m: ChatMessage
  /** 当前是否处于划线草稿模式（决定能否采用为笔记）。 */
  canAdopt: boolean
  onAdopt: (text: string) => void
}

export function ChatMsg({ m, canAdopt, onAdopt }: ChatMsgProps) {
  if (m.role === 'user') {
    return (
      <div className="cm user">
        <div className="bubble">{m.text}</div>
      </div>
    )
  }
  return (
    <div className="cm ai">
      <div className="ai-ava">
        <Icon name="sparkles" size={13} />
      </div>
      <div className="ai-col">
        <div className="bubble ai-bubble">
          {m.typing ? (
            <span className="typing">
              <i />
              <i />
              <i />
            </span>
          ) : (
            renderRich(m.text)
          )}
        </div>
        {!m.typing && canAdopt && m.adoptable && (
          <button className="adopt-btn" onClick={() => onAdopt(m.text)}>
            <Icon name="marker" size={13} /> 采用为笔记
          </button>
        )}
      </div>
    </div>
  )
}
