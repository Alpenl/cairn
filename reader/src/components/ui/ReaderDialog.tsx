/**
 * Reader 通用 Dialog 外壳。
 *
 * Reader 里原本有四套彼此独立的 backdrop / panel / header / close / Escape /
 * focus 代码（Sites、AddLink、订阅、Inbox），行为各差一点：有的漏了 focus trap，
 * 有的 busy 时还能按 Escape 关掉正在写库的对话框。这里只负责**外壳与焦点**，
 * 不接收 client、不接收领域对象：表单、preview/execute、API 错误仍归调用方。
 *
 * 固定 DOM：
 *   .reader-dialog-backdrop
 *   └── .reader-dialog[role=dialog][aria-modal][aria-labelledby]
 *       ├── .reader-dialog-header（h2 + 关闭按钮）
 *       └── 调用方 children
 */
import { useEffect, useRef, type ReactNode, type RefObject } from 'react'
import { Icon } from '../Icon'

/** Dialog 宽度档位。compact 用于确认框，wide 用于带预览的表单。 */
export type ReaderDialogSize = 'compact' | 'default' | 'wide'

export interface ReaderDialogProps {
  title: ReactNode
  titleId: string
  size?: ReaderDialogSize
  busy?: boolean
  dismissOnBackdrop?: boolean
  dismissOnEscape?: boolean
  initialFocusRef?: RefObject<HTMLElement | null>
  className?: string
  onClose: () => void
  children: ReactNode
}

// 焦点环取自 WAI-ARIA 的可聚焦元素集合。disabled 的控件被 :not([disabled])
// 排除；tabindex="-1"（含 panel 自身）只能被程序聚焦，不进 Tab 序列。
const FOCUSABLE_SELECTOR = [
  'a[href]',
  'area[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  'iframe',
  'audio[controls]',
  'video[controls]',
  '[contenteditable="true"]',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function focusableWithin(root: HTMLElement | null): HTMLElement[] {
  if (!root) return []
  return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)).filter(
    (element) => !element.hasAttribute('disabled') && element.getAttribute('aria-hidden') !== 'true',
  )
}

export function ReaderDialog({
  title,
  titleId,
  size = 'default',
  busy = false,
  dismissOnBackdrop = true,
  dismissOnEscape = true,
  initialFocusRef,
  className,
  onClose,
  children,
}: ReaderDialogProps) {
  const panelRef = useRef<HTMLDivElement>(null)
  // keydown 监听只在挂载时装一次：把随渲染变化的 props 走 ref 读，避免每次
  // busy/onClose 变化都拆装一遍监听器（旧实现正是这样反复重装的）。
  const latest = useRef({ busy, dismissOnEscape, onClose, initialFocusRef })
  latest.current = { busy, dismissOnEscape, onClose, initialFocusRef }

  // 打开即接管焦点，卸载后还给打开它的元素。
  useEffect(() => {
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null
    const panel = panelRef.current
    const target = latest.current.initialFocusRef?.current ?? focusableWithin(panel)[0] ?? panel
    target?.focus()
    return () => {
      if (opener && opener.isConnected) opener.focus()
    }
  }, [])

  // Escape 与 Tab 都在这里处理：Tab 环绕保证焦点不会滑到 backdrop 后面的页面上。
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        if (latest.current.busy || !latest.current.dismissOnEscape) return
        event.preventDefault()
        latest.current.onClose()
        return
      }
      if (event.key !== 'Tab') return
      const panel = panelRef.current
      if (!panel) return
      const items = focusableWithin(panel)
      if (items.length === 0) {
        // 没有可聚焦控件时也不放焦点出去，否则 Tab 会跳到底层页面。
        event.preventDefault()
        panel.focus()
        return
      }
      const first = items[0]
      const last = items[items.length - 1]
      const active = document.activeElement
      const inside = active instanceof HTMLElement && panel.contains(active)
      if (event.shiftKey) {
        if (!inside || active === first) {
          event.preventDefault()
          last.focus()
        }
        return
      }
      if (!inside || active === last) {
        event.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [])

  return (
    <div
      className="reader-dialog-backdrop"
      role="presentation"
      onMouseDown={(event) => {
        // 只认落在 backdrop 自身上的按下：点 Dialog 内部（包括从内部拖到外部
        // 松手）都不算关闭意图。
        if (event.target !== event.currentTarget) return
        if (busy || !dismissOnBackdrop) return
        onClose()
      }}
    >
      <div
        ref={panelRef}
        className={`reader-dialog reader-dialog-${size}${className ? ` ${className}` : ''}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
      >
        <div className="reader-dialog-header">
          <h2 id={titleId}>{title}</h2>
          <button
            type="button"
            className="reader-dialog-close"
            title="关闭"
            aria-label="关闭"
            disabled={busy}
            onClick={onClose}
          >
            <Icon name="close" size={15} />
          </button>
        </div>
        {children}
      </div>
    </div>
  )
}
