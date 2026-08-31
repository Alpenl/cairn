/**
 * 选区操作浮层。
 *
 * 定位于选区附近并夹在视口边界内，提供：划线 / 写想法 / 翻译 / 问 AI / 复制。
 * onMouseDown preventDefault 避免点击按钮时清空选区。
 */
import { useLayoutEffect, useRef, useState } from 'react'
import {
  ARTICLE_SELECTION_ACTIONS,
  type PopoverAction,
} from '../lib/selection-actions'
import { Icon, type IconName } from './Icon'

interface ActionDefinition {
  readonly action: PopoverAction
  readonly label: string
  readonly icon: IconName
  readonly accent?: boolean
  readonly annotationAction?: boolean
}

const ACTION_DEFINITIONS: readonly ActionDefinition[] = Object.freeze([
  { action: 'highlight', label: '划线', icon: 'marker', annotationAction: true },
  { action: 'note', label: '写想法', icon: 'pencil', annotationAction: true },
  { action: 'translate', label: '翻译', icon: 'translate', accent: true },
  { action: 'ai', label: '问 AI', icon: 'sparkles', accent: true, annotationAction: true },
  { action: 'copy', label: '复制', icon: 'copy' },
])

export interface ActionPopoverProps {
  rect: DOMRect | null
  onAct: (act: PopoverAction) => void
  annotationActionsPending?: boolean
  actions?: readonly PopoverAction[]
}

export function ActionPopover({
  rect,
  onAct,
  annotationActionsPending = false,
  actions = ARTICLE_SELECTION_ACTIONS,
}: ActionPopoverProps) {
  const popRef = useRef<HTMLDivElement>(null)
  const [measuredSize, setMeasuredSize] = useState({ width: 0, height: 0 })
  useLayoutEffect(() => {
    if (popRef.current) {
      setMeasuredSize({
        width: popRef.current.offsetWidth,
        height: popRef.current.offsetHeight,
      })
    }
  }, [rect])

  if (!rect) return null
  const viewportPadding = 8
  const gap = 8
  const popWidth = Math.min(
    measuredSize.width || 380,
    Math.max(0, window.innerWidth - viewportPadding * 2),
  )
  const popHeight = measuredSize.height || 44
  const centeredLeft = rect.left + rect.width / 2 - popWidth / 2
  const left = Math.max(
    viewportPadding,
    Math.min(centeredLeft, window.innerWidth - popWidth - viewportPadding),
  )
  const aboveTop = rect.top - gap - popHeight
  const belowTop = rect.bottom + gap
  const maxTop = Math.max(viewportPadding, window.innerHeight - popHeight - viewportPadding)
  const top =
    aboveTop >= viewportPadding
      ? aboveTop
      : belowTop + popHeight <= window.innerHeight - viewportPadding
        ? belowTop
        : Math.max(viewportPadding, Math.min(belowTop, maxTop))
  const visibleActions = ACTION_DEFINITIONS.filter((definition) => actions.includes(definition.action))
  return (
    <div
      ref={popRef}
      className="sel-pop"
      style={{ left, top }}
      onMouseDown={(e) => e.preventDefault()}
    >
      {visibleActions.map((definition, index) => (
        <span key={definition.action} className="sel-pop-slot">
          {definition.action === 'copy' && index > 0 && <span className="div" />}
          <button
            disabled={Boolean(definition.annotationAction && annotationActionsPending)}
            onClick={() => onAct(definition.action)}
          >
            <Icon
              name={definition.icon}
              size={14}
              style={definition.accent ? { color: 'var(--accent)' } : undefined}
            /> {definition.label}
          </button>
        </span>
      ))}
    </div>
  )
}
