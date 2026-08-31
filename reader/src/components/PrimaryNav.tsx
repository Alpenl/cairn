/**
 * 主导航：首页 / 五个内容库 / 三个工具，全局唯一一份。
 *
 * 这个组件存在的唯一理由是位置稳定。改造前 vNext 外壳（SurfaceNav）和三个
 * 旧外壳（Sidebar / SitesView / SubscriptionSidebar）各自实现了一遍同样的九个
 * 入口，两套 DOM 的内边距不同、工具组一边 `margin-top: auto` 钉在视口底部一边
 * 紧跟在内容库后面。实测同一个「设置」在首页是 y=832、点进阅读变成 y=303：
 * 每切一次路由，用户下一步要点的目标全都动了。
 *
 * 因此这里只暴露一种结构，两个外壳都必须把它作为侧栏第一个子元素渲染；纵向
 * 位置由 .wt-primary-nav 自己的内边距决定，不再受外层容器和视口高度影响。
 * 侧栏下方的上下文区（标签、域名、文件夹、分类）照旧滚动，主导航 sticky 在
 * 顶部不参与滚动——订阅那 117 行的文件夹树再长也推不动它。
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

/**
 * 四个内容库。网站原来是第五个，但它自己的副标题就写着「按站点聚合的收藏」
 * ——和阅读同源，而且阅读侧栏里本来就有一个「域名」分区在做同一件事。它降级
 * 成阅读侧栏的一个入口，`?view=sites` 路由原样保留。
 */
const LIBRARY_MODES: ReadonlyArray<{ id: LibraryView; label: string; icon: IconName }> = [
  { id: 'pending', label: '收件箱', icon: 'inbox' },
  { id: 'reading', label: '阅读', icon: 'doc' },
  { id: 'subs', label: '订阅', icon: 'rss' },
  { id: 'notes', label: '笔记', icon: 'edit' },
]

const SURFACES: ReadonlyArray<{ route: SurfaceRoute; label: string; icon: IconName }> = [
  { route: { kind: 'surface', id: 'home' }, label: '今天', icon: 'sun' },
]

/**
 * 只剩设置。TODO 降级到「今天」右栏（它本来就在那儿显示，主导航里是第二份），
 * 想法降级成笔记里的一个标签页——想法整理成笔记是一条流水线，不该隔着导航栏。
 */
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
