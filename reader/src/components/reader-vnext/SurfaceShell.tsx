import { useCallback, useMemo, type ReactNode, type RefObject } from 'react'
import { Icon } from '../Icon'
import { type ReaderRoute } from '../../lib/navigation/route'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import { SurfaceNav } from './SurfaceNav'

// 非组件工具已经搬到 lib/reader-surface.ts。这里的 re-export 只是过渡：
// 调用方改成直接从该模块导入后就删除，本文件只留组件。
/* eslint-disable react-refresh/only-export-components */
export {
  errorMessage,
  formatRelativeDate,
  identityIsCurrent,
  isIdentityError,
  navigateReaderTarget,
  SURFACE_IDENTITY_ERROR,
  todoDesiredStatePatch,
} from '../../lib/reader-surface'
/* eslint-enable react-refresh/only-export-components */

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

export function SurfaceLoading() {
  return <div className="rvx-message" aria-busy="true"><Icon name="loader" size={18} /><span>加载中</span></div>
}
