/**
 * 笔记工作区的两个标签页。
 *
 * 想法和笔记是一条流水线——笔记的空状态自己就写着「把想法整理成发布态内容」
 * ——所以它们属于同一个地方的两个视图，而不是主导航里两个并列入口。这里只
 * 负责渲染标签条；切换仍然是一次真实的路由跳转，URL 保持规范。
 */
import type { ReaderNavigationRequest, ReaderRoute, ReaderRouteTargets } from '../../lib/navigation/route'
import { readerRouteIsAvailable, type ReaderCapabilityPolicy } from '../../lib/capabilities'

export type NoteWorkspaceTab = 'notes' | 'thoughts'

export interface NoteWorkspaceTabsProps {
  readonly active: NoteWorkspaceTab
  readonly onNavigate: ReaderNavigationRequest
  readonly capabilityPolicy: ReaderCapabilityPolicy
}

/**
 * 想法标签必须显式带上 `thought_view=live`：不带这个参数时解析器默认落到
 * 已归档视图（route.ts 的 readerThoughtViewFromURL），点进来会看见一个空的
 * 归档列表。
 */
const TABS: ReadonlyArray<{
  id: NoteWorkspaceTab
  label: string
  route: ReaderRoute
  targets?: ReaderRouteTargets
}> = [
  { id: 'notes', label: '笔记', route: { kind: 'library', id: 'notes' } },
  { id: 'thoughts', label: '想法', route: { kind: 'tool', id: 'history' }, targets: { thoughtView: 'live' } },
]

export function NoteWorkspaceTabs({ active, onNavigate, capabilityPolicy }: NoteWorkspaceTabsProps) {
  return (
    <div className="rvx-segmented" role="group" aria-label="笔记工作区">
      {TABS.filter((tab) => readerRouteIsAvailable(tab.route, capabilityPolicy)).map((tab) => (
        <button
          key={tab.id}
          type="button"
          className={active === tab.id ? 'active' : ''}
          aria-pressed={active === tab.id}
          onClick={() => { if (active !== tab.id) void onNavigate(tab.route, tab.targets) }}
        >
          {tab.label}
        </button>
      ))}
    </div>
  )
}
