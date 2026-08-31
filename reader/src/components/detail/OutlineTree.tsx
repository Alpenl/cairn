import type { CSSProperties } from 'react'

import type { TocHeading } from '../../lib/toc'

export type OutlineTreeVariant = 'reader' | 'rail' | 'menu'

export interface OutlineTreeProps {
  readonly items: readonly TocHeading[]
  readonly activeId: string | null
  readonly onJump: (id: string) => void
  readonly variant: OutlineTreeVariant
  readonly onAfterJump?: () => void
}

const ITEM_INDENT_PX = 11
const UNTITLED_OUTLINE_ITEM = '未命名标题'

function itemLabel(item: TocHeading): string {
  return item.text.trim() || UNTITLED_OUTLINE_ITEM
}

function itemDepth(item: TocHeading): number {
  if (!Number.isFinite(item.level)) return 1
  return Math.max(1, Math.min(6, Math.floor(item.level)))
}

function itemIndent(item: TocHeading, variant: OutlineTreeVariant): number {
  return (itemDepth(item) - 1) * ITEM_INDENT_PX + (variant === 'menu' ? 2 : 0)
}

function itemClassName(variant: OutlineTreeVariant, active: boolean): string | undefined {
  const base = variant === 'reader'
    ? 'reader-toc-item'
    : variant === 'rail'
      ? 'reader-rail-toc-item'
      : undefined
  if (!base) return undefined
  return base + (active ? ' cur' : '')
}

function itemStyle(item: TocHeading, variant: OutlineTreeVariant, active: boolean): CSSProperties {
  const paddingInlineStart = itemIndent(item, variant)
  if (variant !== 'menu') return { paddingInlineStart }
  return {
    display: 'block',
    width: '100%',
    padding: '7px 2px',
    border: 0,
    background: 'transparent',
    color: active ? 'var(--accent)' : 'var(--text-secondary)',
    font: `${active ? 600 : 400} 12px/1.45 var(--font-ui)`,
    textAlign: 'left',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
    paddingInlineStart,
    cursor: 'pointer',
  }
}

function listStyle(variant: OutlineTreeVariant): CSSProperties | undefined {
  return variant === 'menu' ? { listStyle: 'none', margin: 0, padding: 0 } : undefined
}

export function OutlineTree({
  items,
  activeId,
  onJump,
  variant,
  onAfterJump,
}: OutlineTreeProps) {
  return (
    <ul style={listStyle(variant)}>
      {items.map((item) => {
        const active = activeId === item.id
        const label = itemLabel(item)
        return (
          <li key={item.id}>
            <button
              type="button"
              className={itemClassName(variant, active)}
              style={itemStyle(item, variant, active)}
              title={label}
              aria-current={active ? 'true' : undefined}
              onClick={() => {
                onJump(item.id)
                onAfterJump?.()
              }}
            >
              {label}
            </button>
          </li>
        )
      })}
    </ul>
  )
}
