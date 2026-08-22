/**
 * 窗口标题栏，从 MainView 抽出（移植自 app.jsx 的 .titlebar）。
 *
 * 同步按钮 + ⌘K 搜索胶囊 + 主题切换 + AI 助手开关 + 连接设置齿轮。
 * 纯展示组件，所有动作通过回调上抛，不持有状态。
 */
import { Icon } from './Icon'
import type { ThoughtSyncSnapshot } from '../lib/user-data/thought-sync'

export interface TitlebarProps {
  theme: 'light' | 'dark'
  chatOpen: boolean
  navigationOpen: boolean
  sidebarCollapsed: boolean
  syncing: boolean
  thoughtSync: ThoughtSyncSnapshot | null
  onSync: () => void
  onToggleNavigation: () => void
  onAddLink: () => void
  onOpenCmdk: () => void
  onToggleTheme: () => void
  onToggleChat: () => void
  onOpenSettings: () => void
  archiveDownloading: boolean
  onDownloadArchive: () => void
  canUseAI: boolean
  canDownloadArchive: boolean
}

function thoughtSyncLabel(snapshot: ThoughtSyncSnapshot): string {
  const count = `${snapshot.pendingCount} 项待同步`
  const errorCode = snapshot.errorCode ? ` · ${snapshot.errorCode}` : ''
  switch (snapshot.phase) {
    case 'offline':
      return snapshot.pendingCount > 0 ? `离线 · ${count}` : '离线'
    case 'syncing':
      return snapshot.pendingCount > 0 ? `同步中 · ${count}` : '同步中'
    case 'failed':
      return `同步失败 · ${count}` +
        (snapshot.blockedCount > 0 ? `，${snapshot.blockedCount} 项阻塞` : '') + errorCode
    case 'pending':
      return `待同步 · ${count}`
    case 'synced':
      return '已同步'
  }
}

export function Titlebar({
  theme,
  chatOpen,
  navigationOpen,
  sidebarCollapsed,
  syncing,
  thoughtSync,
  onSync,
  onToggleNavigation,
  onAddLink,
  onOpenCmdk,
  onToggleTheme,
  onToggleChat,
  onOpenSettings,
  archiveDownloading,
  onDownloadArchive,
  canUseAI,
  canDownloadArchive,
}: TitlebarProps) {
  return (
    <div className="titlebar">
      <div className="tb-section">
        <button
          type="button"
          className={'tb-btn sidebar-toggle' + (navigationOpen || sidebarCollapsed ? ' active' : '')}
          onClick={onToggleNavigation}
          title={navigationOpen ? '关闭导航' : sidebarCollapsed ? '展开侧栏' : '折叠侧栏'}
          aria-label={navigationOpen ? '关闭导航' : sidebarCollapsed ? '展开侧栏' : '折叠侧栏'}
          aria-expanded={navigationOpen}
          aria-controls="primary-navigation"
        >
          <Icon name="sidebar" size={17} />
        </button>
        <button
          className="tb-btn"
          title="同步"
          aria-label="同步"
          aria-busy={syncing || undefined}
          disabled={syncing}
          onClick={onSync}
        >
          <Icon name={syncing ? 'loader' : 'refresh'} size={17} />
        </button>
        {thoughtSync && (
          <span
            className={`thought-sync-status phase-${thoughtSync.phase}`}
            data-phase={thoughtSync.phase}
            data-error-code={thoughtSync.errorCode}
            role="status"
            aria-label={`想法同步状态：${thoughtSyncLabel(thoughtSync)}`}
            title={`想法同步状态：${thoughtSyncLabel(thoughtSync)}`}
          >
            {thoughtSyncLabel(thoughtSync)}
          </span>
        )}
        <button className="tb-btn" title="添加链接" onClick={onAddLink}>
          <Icon name="plus" size={17} />
        </button>
      </div>
      <span className="wordmark">Cairn</span>
      <span className="tb-grow" />
      <div className="search-pill" onClick={onOpenCmdk}>
        <Icon name="search" size={15} />
        <span className="ph">搜索链接</span>
        <span className="kbd">⌘K</span>
      </div>
      <span className="tb-grow" />
      <div className="tb-section">
        <button className="tb-btn" onClick={onToggleTheme} title="切换主题">
          <Icon name={theme === 'light' ? 'moon' : 'sun'} size={17} />
        </button>
        {canUseAI && <button
          className={'tb-btn' + (chatOpen ? ' active' : '')}
          onClick={onToggleChat}
          title="AI 助手 (⌘J)"
        >
          <Icon name="chat" size={18} />
        </button>}
        <button className="tb-btn" onClick={onOpenSettings} title="连接设置">
          <Icon name="globe" size={17} />
        </button>
        {canDownloadArchive && <button
          className="tb-btn"
          onClick={onDownloadArchive}
          disabled={archiveDownloading}
          title="下载归档"
          aria-label="下载归档"
        >
          <Icon name={archiveDownloading ? 'loader' : 'download'} size={17} />
        </button>}
      </div>
    </div>
  )
}
