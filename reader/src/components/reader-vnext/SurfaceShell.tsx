import { useCallback, useMemo, type ReactNode, type RefObject } from 'react'
import { Icon } from '../Icon'
import { type ReaderRoute } from '../../lib/navigation/route'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import { SurfaceNav } from './SurfaceNav'

// 非组件工具全部住在 lib/reader-surface.ts，本文件只留组件——过渡期的
// re-export 与它带来的 react-refresh 例外都已经删掉。

export interface SurfaceShellProps {
  readonly title: string
  readonly subtitle?: string
  readonly active?: 'pending' | 'reading' | 'sites' | 'subs' | 'notes'
  readonly activeRoute?: ReaderRoute
  readonly onNavigate: (route: ReaderRoute) => void
  readonly capabilityPolicy: ReaderCapabilityPolicy
  readonly children: ReactNode
  readonly actions?: ReactNode
  readonly onBack?: () => void
  readonly onBeforeNavigate?: () => boolean
  /** Surfaces that persist a scroll position need the shell's scroller itself. */
  readonly scrollRef?: RefObject<HTMLDivElement>
  readonly workspaceClassName?: string
}

export function SurfaceShell({
  title,
  subtitle,
  active,
  activeRoute,
  onNavigate,
  capabilityPolicy,
  children,
  actions,
  onBack,
  onBeforeNavigate,
  scrollRef,
  workspaceClassName,
}: SurfaceShellProps) {
  const inferredRoute = useMemo<ReaderRoute>(() => activeRoute ?? (active
    ? { kind: 'library', id: active }
    : title === '首页'
      ? { kind: 'surface', id: 'home' }
      : title === '混合 Feed'
        ? { kind: 'surface', id: 'feed' }
        : title === 'TODO'
          ? { kind: 'tool', id: 'todo' }
          : title === '设置'
            ? { kind: 'tool', id: 'settings' }
            : title === '想法' || title === '已归档想法'
              ? { kind: 'tool', id: 'history' }
              : { kind: 'library', id: 'reading' }), [active, activeRoute, title])

  const handleNavigate = useCallback((route: ReaderRoute) => {
    if (onBeforeNavigate && !onBeforeNavigate()) return
    onNavigate(route)
  }, [onBeforeNavigate, onNavigate])

  const handleBack = useCallback(() => {
    if (onBeforeNavigate && !onBeforeNavigate()) return
    onBack?.()
  }, [onBack, onBeforeNavigate])

  return (
    <div className={'rvx-workspace' + (workspaceClassName ? ` ${workspaceClassName}` : '')}>
      <SurfaceNav
        active={active}
        activeRoute={inferredRoute}
        onNavigate={handleNavigate}
        capabilityPolicy={capabilityPolicy}
      />
      <main className="rvx-main">
        <header className="rvx-header">
          <div className="rvx-title-block">
            {onBack && (
              <button className="rvx-icon-button" type="button" aria-label="返回" title="返回" onClick={handleBack}>
                <Icon name="arrowright" size={16} style={{ transform: 'rotate(180deg)' }} />
              </button>
            )}
            <div>
              <h1>{title}</h1>
              {subtitle && <p>{subtitle}</p>}
            </div>
          </div>
          {actions && <div className="rvx-header-actions">{actions}</div>}
        </header>
        <div className="rvx-scroll" ref={scrollRef}>{children}</div>
      </main>
    </div>
  )
}

export function SurfaceError({ message, onRetry }: { readonly message: string; readonly onRetry?: () => void }) {
  return (
    <div className="rvx-message rvx-message-error" role="alert">
      <Icon name="alert" size={18} />
      <p>{message}</p>
      {onRetry && <button className="rvx-button secondary" type="button" onClick={onRetry}>重试</button>}
    </div>
  )
}

export function SurfaceLoading({ label = '加载中' }: { readonly label?: string } = {}) {
  return (
    <div className="rvx-message" role="status" aria-label={label} aria-busy="true">
      <Icon name="loader" size={18} />
      <span>{label}</span>
    </div>
  )
}
