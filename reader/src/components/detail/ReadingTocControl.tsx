import { useEffect, useLayoutEffect, useRef, useState, type CSSProperties } from 'react'
import { MIN_TOC_HEADINGS } from '../../hooks/useReaderToc'
import type { TocHeading } from '../../lib/toc'
import { Icon } from '../Icon'

interface ReadingTocControlProps {
  items: TocHeading[]
  activeId: string | null
  onJump: (id: string) => void
}

function isCompactViewport(): boolean {
  return typeof window !== 'undefined' && window.innerWidth <= 1151
}

function menuPosition(button: HTMLButtonElement | null): CSSProperties {
  if (!button || typeof window === 'undefined') return {}
  const rect = button.getBoundingClientRect()
  const width = Math.min(300, Math.max(0, window.innerWidth - 16))
  const left = Math.max(8, Math.min(rect.left, window.innerWidth - width - 8))
  const estimatedHeight = Math.min(360, Math.max(96, 56 + 34 * 6))
  const top = rect.bottom + estimatedHeight <= window.innerHeight - 8
    ? rect.bottom + 6
    : Math.max(8, rect.top - estimatedHeight - 6)
  return {
    left,
    top,
    width: `min(300px, calc(100vw - 16px))`,
  }
}

/** A compact TOC entry point for surfaces whose rail is hidden on narrow viewports. */
export function ReadingTocControl({ items, activeId, onJump }: ReadingTocControlProps) {
  const [compact, setCompact] = useState(false)
  const [open, setOpen] = useState(false)
  const [position, setPosition] = useState<CSSProperties>({})
  const buttonRef = useRef<HTMLButtonElement>(null)
  const hasToc = items.length >= MIN_TOC_HEADINGS

  useEffect(() => {
    const update = () => {
      const next = isCompactViewport()
      setCompact((current) => (current === next ? current : next))
      if (next && open) setPosition(menuPosition(buttonRef.current))
    }
    update()
    window.addEventListener('resize', update)
    return () => window.removeEventListener('resize', update)
  }, [open])

  useLayoutEffect(() => {
    if (open) setPosition(menuPosition(buttonRef.current))
  }, [open, items.length])

  useEffect(() => {
    if (!open) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setOpen(false)
    }
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target as Node | null
      if (target && buttonRef.current?.parentElement?.contains(target)) return
      setOpen(false)
    }
    window.addEventListener('keydown', onKeyDown)
    window.addEventListener('pointerdown', onPointerDown)
    return () => {
      window.removeEventListener('keydown', onKeyDown)
      window.removeEventListener('pointerdown', onPointerDown)
    }
  }, [open])

  if (!compact || !hasToc) return null

  return (
    <div style={{ display: 'inline-flex', flex: '0 0 auto' }}>
      <button
        ref={buttonRef}
        type="button"
        className="tb-btn"
        aria-label="正文目录"
        aria-expanded={open}
        title="正文目录"
        onClick={() => setOpen((current) => !current)}
      >
        <Icon name="tree" size={16} />
      </button>
      {open && (
        <nav
          aria-label="移动正文目录"
          style={{
            ...position,
            position: 'fixed',
            zIndex: 75,
            maxHeight: 'min(60vh, 360px)',
            overflowY: 'auto',
            padding: '8px 10px',
            border: '0.5px solid var(--border-strong)',
            borderRadius: 8,
            background: 'var(--bg-elevated)',
            boxShadow: 'var(--shadow-pop)',
          }}
        >
          <div
            style={{
              padding: '3px 2px 7px',
              borderBottom: '0.5px solid var(--divider)',
              color: 'var(--text-faint)',
              font: '600 10px var(--font-mono)',
            }}
          >
            目录
          </div>
          {items.map((item) => (
            <button
              key={item.id}
              type="button"
              aria-current={activeId === item.id ? 'true' : undefined}
              title={item.text}
              onClick={() => {
                onJump(item.id)
                setOpen(false)
              }}
              style={{
                display: 'block',
                width: '100%',
                padding: '7px 2px',
                border: 0,
                background: 'transparent',
                color: activeId === item.id ? 'var(--accent)' : 'var(--text-secondary)',
                font: `${activeId === item.id ? 600 : 400} 12px/1.45 var(--font-ui)`,
                textAlign: 'left',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                whiteSpace: 'nowrap',
                paddingInlineStart: (item.level - 1) * 11 + 2,
                cursor: 'pointer',
              }}
            >
              {item.text}
            </button>
          ))}
        </nav>
      )}
    </div>
  )
}
