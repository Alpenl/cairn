/**
 * 右侧划线笔记面板。
 *
 * 与 AI 助手（R4）同位置同尺寸，从右侧滑入（右栏互斥逻辑在 MainView）。
 * 结构：head（标题 + 删除 + 关闭）→ quote（划线引文）→ source row（来源 + 时间）→
 * textarea（⌘Enter 保存）→ actions（问 AI / 取消 / 保存）。
 *
 * 「问 AI」会把当前未保存草稿（textarea 值）通过 onAskAI 第三参回传，
 * 上层据此回写避免丢字（草稿管线，ChatSidebar 消费）。
 */
import { useEffect, useRef, useState } from 'react'
import { Icon } from './Icon'
import { relDate } from '../lib/metadata'
import {
  annotationLocator,
  annotationLocatorTargetKey,
  type Annotation,
  type AnnotationLocator,
} from '../lib/annotation-domain'

export interface NotePanelProps {
  ann: Annotation
  /** Historical annotations are view-only until an explicit reattach succeeds. */
  readOnly?: boolean
  onSave: (val: string) => void | Promise<void>
  onDelete: () => void | Promise<void>
  onClose: () => void
  /** 问 AI：传完整 target identity、划线文本与当前草稿值。 */
  onAskAI: (
    annotation: AnnotationLocator,
    text: string,
    draftVal: string,
  ) => void | Promise<void>
}

export function NotePanel({
  ann,
  readOnly = false,
  onSave,
  onDelete,
  onClose,
  onAskAI,
}: NotePanelProps) {
  const [val, setVal] = useState(ann.note || '')
  const [busy, setBusy] = useState(false)
  const taRef = useRef<HTMLTextAreaElement>(null)
  const locator = annotationLocator(ann)
  const targetKey = locator ? annotationLocatorTargetKey(locator) : null
  const locatorIdentity = locator && targetKey
    ? `${locator.id}\0${locator.blockKey}\0${targetKey}`
    : null

  const run = (operation: () => void | Promise<void>) => {
    if (busy) return
    const outcome = operation()
    if (!outcome) return
    setBusy(true)
    // Supply both handlers on the original promise. `finally()` creates a new
    // rejected promise when the operation fails, which would otherwise become
    // an unhandled rejection even when the caller owns the visible error UI.
    void outcome.then(
      () => setBusy(false),
      () => setBusy(false),
    )
  }

  // 同 ID 可存在于不同 source target；完整 locator 改变也必须重置草稿与焦点。
  useEffect(() => {
    setVal(ann.note || '')
  }, [ann.note, locatorIdentity])
  useEffect(() => {
    const ta = taRef.current
    if (ta) {
      ta.focus()
      ta.setSelectionRange(ta.value.length, ta.value.length)
    }
  }, [locatorIdentity])

  return (
    <aside className="note-panel">
      <div className="chat-head">
        <span className="chat-ttl">
          <Icon name="marker" size={15} style={{ color: 'var(--accent)' }} /> 划线笔记
        </span>
        <span className="rt-grow" />
        <button
          className="tb-btn"
          style={{ minWidth: 28, height: 28 }}
          onClick={() => run(onDelete)}
          disabled={busy || readOnly}
          title="删除划线"
        >
          <Icon name="trash" size={15} />
        </button>
        <button
          className="tb-btn"
          style={{ minWidth: 28, height: 28 }}
          onClick={onClose}
          title="关闭"
        >
          <Icon name="close" size={15} />
        </button>
      </div>
      <div className="np-scroll">
        <div className="ne-quote">
          <span className="ne-bar" />
          <span className="ne-text np-text">{ann.text}</span>
        </div>
        <div className="ne-srcrow" style={{ margin: '14px 0 8px' }}>
          <span className={'ne-src' + (ann.source === 'ai' ? ' ai' : '')}>
            <Icon name={ann.source === 'ai' ? 'sparkles' : 'pencil'} size={12} />
            {readOnly ? '已归档想法 · 只读' : ann.source === 'ai' ? 'AI 笔记 · 可编辑' : '我的想法'}
          </span>
          <span className="rt-grow" />
          <span className="np-date">{relDate(new Date(ann.updatedAt).toISOString())}</span>
        </div>
        <textarea
          ref={taRef}
          className="np-ta"
          value={val}
          onChange={(e) => setVal(e.target.value)}
          disabled={busy}
          readOnly={readOnly}
          placeholder="写下你的想法…"
          onKeyDown={(e) => {
            if (!readOnly && (e.metaKey || e.ctrlKey) && e.key === 'Enter') {
              e.preventDefault()
              run(() => onSave(val))
            }
          }}
        />
      </div>
      <div className="ne-actions" style={{ padding: '0 14px 14px', marginTop: 0 }}>
        <button
          className="ne-btn askai"
          title="就这段划线进入多轮对话，满意的回复可采用为笔记"
          onClick={() => {
            if (locator) run(() => onAskAI(locator, ann.text, val))
          }}
          disabled={busy || readOnly || !locator}
        >
          <Icon name="sparkles" size={14} /> 问 AI
        </button>
        <span className="rt-grow" />
        <button className="ne-btn ghost" onClick={onClose} disabled={busy}>
          取消
        </button>
        <button
          className="ne-btn primary"
          onClick={() => run(() => onSave(val))}
          disabled={busy || readOnly}
        >
          保存
        </button>
      </div>
    </aside>
  )
}
