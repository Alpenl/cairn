import { useCallback, useMemo, type ReactNode, type RefObject } from 'react'
import { Icon } from '../Icon'
import { type ReaderNavigationRequest, type ReaderRoute, type ReaderRouteTargets } from '../../lib/navigation/route'
import type { ReaderTodoPatchRequest, ReaderTodoResponse } from '../../lib/api/types'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import { SurfaceNav } from './SurfaceNav'

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

export const SURFACE_IDENTITY_ERROR = 'Reader 身份已失效，请重新连接。'

// Keep the surface boundary tolerant of the small client doubles used by
// older embedded hosts while still failing closed for a revoked lease.
// eslint-disable-next-line react-refresh/only-export-components
export function identityIsCurrent(client: { readonly isIdentityCurrent?: () => boolean }): boolean {
  try {
    return typeof client.isIdentityCurrent !== 'function' || client.isIdentityCurrent()
  } catch {
    return false
  }
}

// eslint-disable-next-line react-refresh/only-export-components
export function isIdentityError(value: unknown): boolean {
  return typeof value === 'object' && value !== null && (value as { readonly kind?: unknown }).kind === 'identity-mismatch'
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

/** Build a patch from the user's desired done state, preserving the host CAS boundary. */
// eslint-disable-next-line react-refresh/only-export-components
export function todoDesiredStatePatch(todo: ReaderTodoResponse, done: boolean): ReaderTodoPatchRequest | null {
  if (todo.origin_kind === 'standalone') return { done }
  if (!Number.isSafeInteger(todo.host_revision) || todo.host_revision < 0) return null
  return { done, expected_host_revision: todo.host_revision }
}

// Keep a source target in the URL when a surface delegates navigation to the
// legacy app shell. The route callback changes the rendered surface; the
// replace plus popstate carries the exact resource identity to MainView.
// eslint-disable-next-line react-refresh/only-export-components
export function navigateReaderTarget(
  route: ReaderRoute,
  onNavigate: ReaderNavigationRequest,
  targets: ReaderRouteTargets = {},
): void {
  if (Object.keys(targets).length === 0) onNavigate(route)
  else onNavigate(route, targets)
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

// Shared by the surface components; keeping these small helpers beside the
// shell avoids a second utility module for a single Reader surface family.
// eslint-disable-next-line react-refresh/only-export-components
export function formatRelativeDate(value: string | null | undefined): string {
  if (!value) return '暂无时间'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '暂无时间'
  return new Intl.DateTimeFormat('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(date)
}

// eslint-disable-next-line react-refresh/only-export-components
export function errorMessage(error: { readonly message: string; readonly status?: number }): string {
  if (error.status === 409) return '内容已经被其他窗口更新，请刷新后重试。'
  if (error.status === 412) return '版本已经变化，请刷新后重试。'
  if (error.status === 403) return '当前身份没有执行此操作的权限。'
  return error.message || '请求失败，请稍后重试。'
}
