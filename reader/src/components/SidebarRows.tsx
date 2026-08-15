/**
 * 侧栏行/分组原子组件，从 Sidebar 拆出（保持单文件 ≤300 行）。
 */
import { useCallback, useState, type CSSProperties, type ReactNode } from 'react'
import { Icon, type IconName } from './Icon'
import { readOwnedStorage, writeOwnedStorage } from '../lib/storage-ownership'

/** 折叠态 hook，localStorage 键 webtag:sbfold:<key>（对齐 sidebar.jsx useFold）。 */
function useFold(key: string, def: boolean): [boolean, () => void] {
  const [open, setOpen] = useState<boolean>(() => {
    const v = readOwnedStorage('sidebarFold', key)
    return v == null ? def : v === '1'
  })
  const toggle = useCallback(() => {
    setOpen((o) => {
      writeOwnedStorage('sidebarFold', o ? '0' : '1', key)
      return !o
    })
  }, [key])
  return [open, toggle]
}

export interface SbRowProps {
  icon?: IconName
  name: string
  count?: number
  active?: boolean
  onClick?: () => void
  glyph?: string
  mono?: boolean
  indent?: boolean
  pinned?: boolean
  onPin?: () => void
}

export function SbRow({
  icon,
  name,
  count,
  active,
  onClick,
  glyph,
  mono,
  indent,
  pinned,
  onPin,
}: SbRowProps) {
  const nameStyle: CSSProperties | undefined = mono
    ? { fontFamily: 'var(--font-mono)', fontSize: 12.5 }
    : undefined
  return (
    <div
      className={
        'sb-row' +
        (active ? ' active' : '') +
        (indent ? ' indent' : '')
      }
      onClick={onClick}
    >
      {glyph && <span className={'tag-hash' + (glyph === '≈' ? ' rel' : '')}>{glyph}</span>}
      {icon && (
        <span className="sb-icon">
          <Icon name={icon} size={16} />
        </span>
      )}
      <span className="sb-name" style={nameStyle}>
        {name}
      </span>
      {onPin && (
        <span
          className={'pin-btn' + (pinned ? ' on' : '')}
          title={pinned ? '取消钉选' : '钉选'}
          onClick={(e) => {
            e.stopPropagation()
            onPin()
          }}
        >
          <Icon name="pin" size={12} sw={2} />
        </span>
      )}
      {count != null && <span className="sb-count">{count}</span>}
    </div>
  )
}

export interface FoldGroupProps {
  label: string
  k: string
  defaultOpen: boolean
  activeHint?: string | null
  status?: string
  children: ReactNode
}

export function FoldGroup({ label, k, defaultOpen, activeHint, status, children }: FoldGroupProps) {
  const [open, toggle] = useFold(k, defaultOpen)
  return (
    <div className="sb-group">
      <div className="sb-label foldable" onClick={toggle}>
        <span className="sb-label-text">{label}</span>
        {status && <span className="sb-label-status">{status}</span>}
        {!open && activeHint && <span className="fold-hint">{activeHint}</span>}
        <Icon
          name="chevron"
          size={11}
          sw={2.4}
          style={{
            transform: open ? 'rotate(90deg)' : 'none',
            transition: 'transform 0.16s',
            marginLeft: 'auto',
          }}
        />
      </div>
      {open && children}
    </div>
  )
}
