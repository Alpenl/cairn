/**
 * 主导航：首页 / 四个内容库 / 设置，全局唯一一份。
 *
 * 所有外壳都直接渲染这一份结构；纵向位置由 .wt-primary-nav 自己决定，不受
 * 侧栏下方上下文区长度影响。
 */
import { useRef, type KeyboardEvent } from 'react'
import { Icon, type IconName } from './Icon'
import type { ReaderRoute } from '../lib/navigation/route'
import {
  deriveReaderCapabilityPolicy,
  readerRouteIsAvailable,
  type ReaderCapabilityPolicy,
} from '../lib/capabilities'
import { usePendingInboxCount } from './reader-vnext/pending-inbox-count-context'

export type LibraryView = 'pending' | 'reading' | 'sites' | 'subs' | 'notes'

type SurfaceRoute = Extract<ReaderRoute, { readonly kind: 'surface' | 'tool' }>

export interface PrimaryNavProps {
  /** 权威路由；缺省时回退到 activeLibrary（旧外壳的调用方式）。 */
  readonly activeRoute?: ReaderRoute
  readonly activeLibrary?: LibraryView | null
  readonly onNavigate: (route: ReaderRoute) => void
  readonly policy?: ReaderCapabilityPolicy
}

const DEFAULT_POLICY = deriveReaderCapabilityPolicy(undefined)

/** 四个内容库；`?view=sites` 仍由阅读库承载。 */
const LIBRARY_MODES: ReadonlyArray<{ id: LibraryView; label: string; icon: IconName }> = [
  { id: 'pending', label: '收件箱', icon: 'inbox' },
  { id: 'reading', label: '阅读', icon: 'doc' },
  { id: 'subs', label: '订阅', icon: 'rss' },
  { id: 'notes', label: '笔记', icon: 'edit' },
]

const SURFACES: ReadonlyArray<{ route: SurfaceRoute; label: string; icon: IconName }> = [
  { route: { kind: 'surface', id: 'home' }, label: '今天', icon: 'sun' },
]

/** 全局工具只保留设置；TODO 和想法由各自工作区承载。 */
const TOOLS: ReadonlyArray<{ route: SurfaceRoute; label: string; icon: IconName }> = [
  { route: { kind: 'tool', id: 'settings' }, label: '设置', icon: 'more' },
]

function routeIsActive(activeRoute: ReaderRoute | undefined, route: SurfaceRoute): boolean {
  return activeRoute?.kind === route.kind && activeRoute.id === route.id
}

function NavAction({
  route,
  label,
  icon,
  active,
  onNavigate,
}: {
  readonly route: SurfaceRoute
  readonly label: string
  readonly icon: IconName
  readonly active: boolean
  readonly onNavigate: (route: ReaderRoute) => void
}) {
  return (
    <button
      className={'rvx-nav-action' + (active ? ' active' : '')}
      type="button"
      aria-current={active ? 'page' : undefined}
      title={label}
      onClick={() => onNavigate(route)}
    >
      <span className="sb-icon"><Icon name={icon} size={15} /></span>
      <span className="sb-name">{label}</span>
    </button>
  )
}

export function PrimaryNav({ activeRoute, activeLibrary, onNavigate, policy = DEFAULT_POLICY }: PrimaryNavProps) {
  const pendingCount = usePendingInboxCount()
  const tabRefs = useRef<Array<HTMLButtonElement | null>>([])
  const visibleSurfaces = SURFACES.filter((surface) => readerRouteIsAvailable(surface.route, policy))
  const visibleModes = LIBRARY_MODES.filter((mode) =>
    readerRouteIsAvailable({ kind: 'library', id: mode.id }, policy),
  )
  const visibleTools = TOOLS.filter((tool) => readerRouteIsAvailable(tool.route, policy))
  const currentLibrary = activeRoute
    ? activeRoute.kind === 'library' ? activeRoute.id : null
    : activeLibrary ?? null
  // 降级后的库仍是合法路由，只是没有自己的入口了——高亮落在收留它的那个库上，
  // 用户才能看出自己身在何处。
  const highlighted = currentLibrary === 'sites' ? 'reading' : currentLibrary
  const selectedIndex = visibleModes.findIndex((mode) => mode.id === highlighted)

  const focusTab = (index: number) => {
    if (visibleModes.length === 0) return
    const normalized = (index + visibleModes.length) % visibleModes.length
    tabRefs.current[normalized]?.focus()
  }

  const onLibraryKeyDown = (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
    if (event.key === 'ArrowRight' || event.key === 'ArrowDown') {
      event.preventDefault()
      focusTab(index + 1)
    } else if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') {
      event.preventDefault()
      focusTab(index - 1)
    } else if (event.key === 'Home') {
      event.preventDefault()
      focusTab(0)
    } else if (event.key === 'End') {
      event.preventDefault()
      focusTab(visibleModes.length - 1)
    }
  }

  return (
    <div className="wt-primary-nav">
      <div className="wt-nav-group wt-nav-surfaces">
        {visibleSurfaces.map((surface) => (
          <NavAction
            key={`${surface.route.kind}:${surface.route.id}`}
            route={surface.route}
            label={surface.label}
            icon={surface.icon}
            active={routeIsActive(activeRoute, surface.route)}
            onNavigate={onNavigate}
          />
        ))}
      </div>
      <div className="library-mode-nav" role="tablist" aria-label="内容库">
        {visibleModes.map((mode, index) => {
          const selected = selectedIndex === index
          return (
            <button
              key={mode.id}
              ref={(element) => { tabRefs.current[index] = element }}
              type="button"
              role="tab"
              aria-selected={selected}
              tabIndex={selected || (selectedIndex < 0 && index === 0) ? 0 : -1}
              className={'library-mode-row' + (selected ? ' active' : '')}
              title={mode.label}
              onClick={() => onNavigate({ kind: 'library', id: mode.id })}
              onKeyDown={(event) => onLibraryKeyDown(event, index)}
            >
              <span className="sb-icon"><Icon name={mode.icon} size={15} /></span>
              <span className="sb-name">{mode.label}</span>
              {mode.id === 'pending' && pendingCount !== null && pendingCount > 0 && (
                <span className="sb-count" aria-label={`收件箱 ${pendingCount} 项待处理`}>{pendingCount}</span>
              )}
            </button>
          )
        })}
      </div>
      <div className="wt-nav-group wt-nav-tools">
        {visibleTools.map((tool) => (
          <NavAction
            key={`${tool.route.kind}:${tool.route.id}`}
            route={tool.route}
            label={tool.label}
            icon={tool.icon}
            active={routeIsActive(activeRoute, tool.route)}
            onNavigate={onNavigate}
          />
        ))}
      </div>
    </div>
  )
}
