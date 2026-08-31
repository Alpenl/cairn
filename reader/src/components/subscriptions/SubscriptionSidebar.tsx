import { useEffect, useMemo, useRef, useState, type DragEvent } from 'react'
import { Icon } from '../Icon'
import { LibraryModeNav, type LibraryView } from '../LibraryModeNav'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import type { ReaderRoute } from '../../lib/navigation/route'
import { feedError, feedTitle } from '../../lib/feed'
import type {
  FeedFolder,
  FeedSubscription,
  SubscriptionsResponse,
} from '../../lib/api/types'
import { readOwnedStorage, writeOwnedStorage } from '../../lib/storage-ownership'
import type { FeedSelection } from './model'

const DRAG_TYPE = 'application/x-webtag-subscription'
export const FAILED_SUBSCRIPTION_THRESHOLD = 3

function loadCollapsedFolders(): Set<string> {
  try {
    const value = JSON.parse(readOwnedStorage('collapsedFeedFolders') || '[]') as unknown
    if (Array.isArray(value)) {
      return new Set(value.filter((id): id is string => typeof id === 'string' && id !== ''))
    }
  } catch {
    // Ignore damaged browser preferences and start expanded.
  }
  return new Set()
}

function saveCollapsedFolders(folders: Set<string>): void {
  writeOwnedStorage('collapsedFeedFolders', JSON.stringify([...folders]))
}

function isFailedSubscription(subscription: FeedSubscription): boolean {
  return (
    (subscription.failure_count ?? 0) >= FAILED_SUBSCRIPTION_THRESHOLD &&
    Boolean(feedError(subscription))
  )
}

interface SubscriptionSidebarProps {
  data: SubscriptionsResponse
  selection: FeedSelection
  loading: boolean
  onSelect: (selection: FeedSelection) => void
  onView: (view: LibraryView) => void
  onNavigate?: (route: ReaderRoute) => void
  capabilityPolicy: ReaderCapabilityPolicy
  collapsed: boolean
  onAddSubscription: () => void
  onAddFolder: () => void
  onEditFolder: (folder: FeedFolder) => void
  onDeleteFolder: (folder: FeedFolder) => void
  onMoveSubscription: (subscription: FeedSubscription, folderID: string | null) => void
  onRefreshSubscription: (subscription: FeedSubscription) => void
  onDeleteSubscription: (subscription: FeedSubscription) => void
  batchBusy?: boolean
  onBatchRefresh: (subscriptions: FeedSubscription[]) => void
  onBatchMove: (subscriptions: FeedSubscription[], folderID: string | null) => void
  onBatchDelete: (subscriptions: FeedSubscription[]) => void
  onDeleteFailed: (subscriptions: FeedSubscription[]) => void
  onImportOPML: (file: File) => void
  onExportOPML: () => void
}

const SMART_VIEWS = [
  { id: 'all', name: '全部文章', icon: 'stack', count: 'all' },
  { id: 'unread', name: '未读', icon: 'inbox', count: 'unread' },
  { id: 'starred', name: '收藏', icon: 'star', count: 'starred' },
  { id: 'later', name: '稍后读', icon: 'bookmark', count: 'later' },
] as const

function SourceRow({
  subscription,
  folders,
  active,
  onSelect,
  onMove,
  onRefresh,
  onDelete,
  batchMode,
  batchBusy,
  selected,
  onToggleSelected,
}: {
  subscription: FeedSubscription
  folders: FeedFolder[]
  active: boolean
  onSelect: () => void
  onMove: (folderID: string | null) => void
  onRefresh: () => void
  onDelete: () => void
  batchMode: boolean
  batchBusy: boolean
  selected: boolean
  onToggleSelected: () => void
}) {
  const title = feedTitle(subscription)
  const error = feedError(subscription)
  const onDragStart = (event: DragEvent<HTMLDivElement>) => {
    event.dataTransfer.setData(DRAG_TYPE, subscription.id)
    event.dataTransfer.effectAllowed = 'move'
  }

  return (
    <div
      className={'rss-source-row' + (active ? ' active' : '') + (batchMode ? ' batch' : '')}
      draggable={!batchMode}
      onDragStart={onDragStart}
      title={error ? `${title}\n最近错误：${error}` : title}
    >
      {batchMode && (
        <input
          className="rss-source-check"
          type="checkbox"
          aria-label={`选择订阅源 ${title}`}
          checked={selected}
          disabled={batchBusy}
          onChange={onToggleSelected}
        />
      )}
      <button
        type="button"
        className="rss-source-main"
        aria-pressed={batchMode ? selected : undefined}
        disabled={batchMode && batchBusy}
        onClick={batchMode ? onToggleSelected : onSelect}
      >
        <span className={'rss-feed-mark' + (error ? ' error' : '')}>
          <Icon name={subscription.refreshing ? 'loader' : error ? 'alert' : 'rss'} size={13} />
        </span>
        <span className="sb-name">{title}</span>
        {(subscription.unread_count ?? 0) > 0 && (
          <span className="unread-pill">{subscription.unread_count}</span>
        )}
      </button>
      {!batchMode && (
        <details className="rss-menu">
          <summary
            aria-label={`管理订阅源 ${title}`}
            title="管理订阅源"
            aria-disabled={batchBusy}
            onClick={(event) => {
              if (batchBusy) event.preventDefault()
            }}
          >
            <Icon name="more" size={14} />
          </summary>
          <div className="rss-menu-pop">
            <button type="button" onClick={onRefresh}>
              <Icon name="refresh" size={14} /> 刷新订阅源
            </button>
            <label className="rss-menu-select">
              <span>移动到文件夹</span>
              <select
                aria-label={`移动 ${title} 到文件夹`}
                value={subscription.folder_id ?? ''}
                onChange={(event) => onMove(event.target.value || null)}
              >
                <option value="">未分组</option>
                {folders.map((folder) => (
                  <option value={folder.id} key={folder.id}>
                    {folder.name}
                  </option>
                ))}
              </select>
            </label>
            <button type="button" className="danger" onClick={onDelete}>
              <Icon name="trash" size={14} /> 取消订阅
            </button>
          </div>
        </details>
      )}
    </div>
  )
}

export function SubscriptionSidebar({
  data,
  selection,
  loading,
  onSelect,
  onView,
  onNavigate,
  capabilityPolicy,
  collapsed,
  onAddSubscription,
  onAddFolder,
  onEditFolder,
  onDeleteFolder,
  onMoveSubscription,
  onRefreshSubscription,
  onDeleteSubscription,
  batchBusy = false,
  onBatchRefresh,
  onBatchMove,
  onBatchDelete,
  onDeleteFailed,
  onImportOPML,
  onExportOPML,
}: SubscriptionSidebarProps) {
  const sidebarRef = useRef<HTMLElement>(null)
  const importRef = useRef<HTMLInputElement>(null)
  const [collapsedFolders, setCollapsedFolders] = useState<Set<string>>(loadCollapsedFolders)
  const [batchMode, setBatchMode] = useState(false)
  const [selectedIDs, setSelectedIDs] = useState<Set<string>>(() => new Set())
  const folders = useMemo(
    () => [...data.folders].sort((a, b) => a.name.localeCompare(b.name, 'zh-CN')),
    [data.folders],
  )
  const subscriptions = useMemo(
    () =>
      data.subscriptions
        .filter((subscription) => subscription.active !== false)
        .sort((a, b) => feedTitle(a).localeCompare(feedTitle(b), 'zh-CN')),
    [data.subscriptions],
  )
  const failedSubscriptions = useMemo(
    () => subscriptions.filter(isFailedSubscription),
    [subscriptions],
  )
  const selectedSubscriptions = useMemo(
    () => subscriptions.filter((subscription) => selectedIDs.has(subscription.id)),
    [selectedIDs, subscriptions],
  )
  const ungroupedSubscriptions = useMemo(
    () => subscriptions.filter((subscription) => !subscription.folder_id),
    [subscriptions],
  )
  const allSelected =
    subscriptions.length > 0 && selectedSubscriptions.length === subscriptions.length

  useEffect(() => {
    const closeMenusOutsideTarget = (event: PointerEvent) => {
      const target = event.target
      if (!(target instanceof Node)) return
      sidebarRef.current
        ?.querySelectorAll<HTMLDetailsElement>('details.rss-menu[open]')
        .forEach((menu) => {
          if (!menu.contains(target)) menu.open = false
        })
    }

    document.addEventListener('pointerdown', closeMenusOutsideTarget)
    return () =>
      document.removeEventListener('pointerdown', closeMenusOutsideTarget)
  }, [])

  useEffect(() => {
    const available = new Set(subscriptions.map((subscription) => subscription.id))
    setSelectedIDs((current) => {
      const next = new Set([...current].filter((id) => available.has(id)))
      return next.size === current.size ? current : next
    })
  }, [subscriptions])

  const toggleFolder = (folderID: string) => {
    setCollapsedFolders((current) => {
      const next = new Set(current)
      if (next.has(folderID)) next.delete(folderID)
      else next.add(folderID)
      saveCollapsedFolders(next)
      return next
    })
  }

  const toggleSelected = (subscriptionID: string) => {
    setSelectedIDs((current) => {
      const next = new Set(current)
      if (next.has(subscriptionID)) next.delete(subscriptionID)
      else next.add(subscriptionID)
      return next
    })
  }

  const toggleSubscriptions = (targets: FeedSubscription[]) => {
    const ids = targets.map((subscription) => subscription.id)
    setSelectedIDs((current) => {
      const next = new Set(current)
      const everySelected = ids.length > 0 && ids.every((id) => current.has(id))
      for (const id of ids) {
        if (everySelected) next.delete(id)
        else next.add(id)
      }
      return next
    })
  }

  const toggleBatchMode = () => {
    if (batchMode) setSelectedIDs(new Set())
    setBatchMode(!batchMode)
  }

  const toggleAllSubscriptions = () => {
    setSelectedIDs(
      allSelected ? new Set() : new Set(subscriptions.map((subscription) => subscription.id)),
    )
  }

  const dropInto = (event: DragEvent<HTMLElement>, folderID: string | null) => {
    event.preventDefault()
    const id = event.dataTransfer.getData(DRAG_TYPE)
    const subscription = subscriptions.find((candidate) => candidate.id === id)
    if (subscription && (subscription.folder_id ?? null) !== folderID) {
      onMoveSubscription(subscription, folderID)
    }
  }

  const renderSource = (subscription: FeedSubscription) => (
    <SourceRow
      key={subscription.id}
      subscription={subscription}
      folders={folders}
      active={selection.kind === 'subscription' && selection.id === subscription.id}
      onSelect={() =>
        onSelect({ kind: 'subscription', id: subscription.id, name: feedTitle(subscription) })
      }
      onMove={(folderID) => onMoveSubscription(subscription, folderID)}
      onRefresh={() => onRefreshSubscription(subscription)}
      onDelete={() => onDeleteSubscription(subscription)}
      batchMode={batchMode}
      batchBusy={batchBusy}
      selected={selectedIDs.has(subscription.id)}
      onToggleSelected={() => toggleSelected(subscription.id)}
    />
  )

  return (
    <aside
      ref={sidebarRef}
      className={'sidebar rss-sidebar' + (collapsed ? ' collapsed' : '')}
      id="primary-navigation"
      aria-label="订阅导航"
      aria-busy={batchBusy}
    >
      <LibraryModeNav view="subs" onView={onView} onNavigate={onNavigate} policy={capabilityPolicy} />

      <div className="rss-sidebar-title">
        <span>条目</span>
        <button
          type="button"
          onClick={onAddSubscription}
          aria-label="添加订阅"
          title="添加订阅"
          disabled={batchBusy}
        >
          <Icon name="plus" size={16} />
        </button>
      </div>

      <div className="sb-group smart-group rss-smart-group">
        {SMART_VIEWS.map((view) => (
          <button
            type="button"
            className={
              'sb-row rss-nav-row' +
              (selection.kind === 'view' && selection.id === view.id ? ' active' : '')
            }
            onClick={() => onSelect({ kind: 'view', id: view.id, name: view.name })}
            key={view.id}
          >
            <span className="sb-icon">
              <Icon name={view.icon} size={15} />
            </span>
            <span className="sb-name">{view.name}</span>
            <span className="sb-count">{data.counts[view.count]}</span>
          </button>
        ))}
      </div>

      <div className="rss-folder-heading">
        <span>文件夹</span>
        <button
          type="button"
          onClick={onAddFolder}
          aria-label="新建文件夹"
          title="新建文件夹"
          disabled={batchBusy}
        >
          <Icon name="plus" size={14} />
        </button>
      </div>

      <div className="rss-folder-list">
        {folders.map((folder) => {
          const children = subscriptions.filter((subscription) => subscription.folder_id === folder.id)
          const unread = children.reduce(
            (sum, subscription) => sum + (subscription.unread_count ?? 0),
            0,
          )
          const collapsed = collapsedFolders.has(folder.id)
          const childrenID = `rss-folder-children-${folder.id}`
          return (
            <section
              className="rss-folder-group"
              key={folder.id}
              onDragOver={(event) => event.preventDefault()}
              onDrop={(event) => dropInto(event, folder.id)}
            >
              <div className="rss-folder-row">
                <button
                  type="button"
                  className="rss-folder-toggle"
                  aria-label={`${collapsed ? '展开' : '折叠'}文件夹 ${folder.name}`}
                  aria-expanded={!collapsed}
                  aria-controls={childrenID}
                  title={collapsed ? '展开文件夹' : '折叠文件夹'}
                  onClick={() => toggleFolder(folder.id)}
                >
                  <Icon name="chevron" size={12} />
                </button>
                <button
                  type="button"
                  className={
                    'rss-folder-main' +
                    (selection.kind === 'folder' && selection.id === folder.id
                      ? ' active'
                      : '')
                  }
                  aria-pressed={
                    batchMode && children.length > 0
                      ? children.every((subscription) => selectedIDs.has(subscription.id))
                      : undefined
                  }
                  onClick={() =>
                    batchMode
                      ? toggleSubscriptions(children)
                      : onSelect({ kind: 'folder', id: folder.id, name: folder.name })
                  }
                >
                  <Icon name="folder" size={14} />
                  <span>{folder.name}</span>
                  {unread > 0 && <span className="sb-count">{unread}</span>}
                </button>
                {!batchMode && (
                  <details className="rss-menu">
                    <summary
                      aria-label={`管理文件夹 ${folder.name}`}
                      title="管理文件夹"
                      aria-disabled={batchBusy}
                      onClick={(event) => {
                        if (batchBusy) event.preventDefault()
                      }}
                    >
                      <Icon name="more" size={14} />
                    </summary>
                    <div className="rss-menu-pop">
                      <button type="button" onClick={() => onEditFolder(folder)}>
                        <Icon name="edit" size={14} /> 重命名
                      </button>
                      <button
                        type="button"
                        className="danger"
                        onClick={() => onDeleteFolder(folder)}
                      >
                        <Icon name="trash" size={14} /> 删除文件夹
                      </button>
                    </div>
                  </details>
                )}
              </div>
              <div id={childrenID} hidden={collapsed}>
                {children.map(renderSource)}
              </div>
            </section>
          )
        })}

        <section
          className="rss-folder-group ungrouped"
          onDragOver={(event) => event.preventDefault()}
          onDrop={(event) => dropInto(event, null)}
        >
          <button
            type="button"
            className={
              'rss-ungrouped-row' +
              (selection.kind === 'folder' && selection.id === 'ungrouped' ? ' active' : '')
            }
            aria-pressed={
              batchMode && ungroupedSubscriptions.length > 0
                ? ungroupedSubscriptions.every((subscription) => selectedIDs.has(subscription.id))
                : undefined
            }
            onClick={() => {
              if (batchMode) toggleSubscriptions(ungroupedSubscriptions)
              else onSelect({ kind: 'folder', id: 'ungrouped', name: '未分组' })
            }}
          >
            <Icon name="folder" size={14} /> 未分组
          </button>
          <div>
            {ungroupedSubscriptions.map(renderSource)}
          </div>
        </section>

        {!loading && subscriptions.length === 0 && (
          <button type="button" className="rss-sidebar-empty" onClick={onAddSubscription}>
            <Icon name="rss" size={17} /> 添加第一个订阅源
          </button>
        )}
      </div>

      <div className="rss-sidebar-footer">
        {batchMode ? (
          <div className="rss-batch-toolbar" role="toolbar" aria-label="订阅源批量操作">
            <span
              className="rss-batch-count"
              title={`已选 ${selectedSubscriptions.length} 个订阅源`}
            >
              {selectedSubscriptions.length}
            </span>
            <button
              type="button"
              aria-label={allSelected ? '取消全选订阅源' : '全选订阅源'}
              title={allSelected ? '取消全选' : '全选'}
              disabled={batchBusy || subscriptions.length === 0}
              onClick={toggleAllSubscriptions}
            >
              <Icon name="check" size={14} />
            </button>
            <button
              type="button"
              aria-label="批量刷新"
              title="刷新已选订阅源"
              disabled={batchBusy || selectedSubscriptions.length === 0}
              onClick={() => onBatchRefresh(selectedSubscriptions)}
            >
              <Icon
                name={batchBusy ? 'loader' : 'refresh'}
                size={14}
                style={batchBusy ? { animation: 'spin 0.9s linear infinite' } : undefined}
              />
            </button>
            <details className="rss-menu rss-batch-move">
              <summary
                aria-label="批量移动"
                title="移动已选订阅源"
                aria-disabled={batchBusy || selectedSubscriptions.length === 0}
                onClick={(event) => {
                  if (batchBusy || selectedSubscriptions.length === 0) event.preventDefault()
                }}
              >
                <Icon name="folder" size={14} />
              </summary>
              <div className="rss-menu-pop">
                <button
                  type="button"
                  onClick={(event) => {
                    onBatchMove(selectedSubscriptions, null)
                    const menu = event.currentTarget.closest('details')
                    if (menu) menu.open = false
                  }}
                >
                  未分组
                </button>
                {folders.map((folder) => (
                  <button
                    type="button"
                    key={folder.id}
                    onClick={(event) => {
                      onBatchMove(selectedSubscriptions, folder.id)
                      const menu = event.currentTarget.closest('details')
                      if (menu) menu.open = false
                    }}
                  >
                    {folder.name}
                  </button>
                ))}
              </div>
            </details>
            <button
              type="button"
              className="danger"
              aria-label="批量删除"
              title="删除已选订阅源"
              disabled={batchBusy || selectedSubscriptions.length === 0}
              onClick={() => onBatchDelete(selectedSubscriptions)}
            >
              <Icon name="trash" size={14} />
            </button>
            <button
              type="button"
              aria-label="退出批量管理"
              title="退出批量管理"
              disabled={batchBusy}
              onClick={toggleBatchMode}
            >
              <Icon name="close" size={14} />
            </button>
          </div>
        ) : (
          <div className="rss-sidebar-manage">
            <button
              type="button"
              aria-label="批量管理"
              disabled={batchBusy}
              onClick={toggleBatchMode}
            >
              <Icon name="layers" size={14} /> 批量管理
            </button>
            {failedSubscriptions.length > 0 && (
              <button
                type="button"
                className="danger"
                aria-label={`清理 ${failedSubscriptions.length} 个失效源`}
                title={`删除连续失败至少 ${FAILED_SUBSCRIPTION_THRESHOLD} 次的订阅源`}
                disabled={batchBusy}
                onClick={() => onDeleteFailed(failedSubscriptions)}
              >
                <Icon
                  name={batchBusy ? 'loader' : 'alert'}
                  size={14}
                  style={batchBusy ? { animation: 'spin 0.9s linear infinite' } : undefined}
                />{' '}
                失效 {failedSubscriptions.length}
              </button>
            )}
          </div>
        )}
        <div className="rss-sidebar-portability">
          <input
            ref={importRef}
            type="file"
            accept=".opml,.xml,text/xml,application/xml"
            hidden
            onChange={(event) => {
              const file = event.target.files?.[0]
              if (file) onImportOPML(file)
              event.target.value = ''
            }}
          />
          <button type="button" disabled={batchBusy} onClick={() => importRef.current?.click()}>
            <Icon name="upload" size={14} /> 导入 OPML
          </button>
          <button type="button" disabled={batchBusy} onClick={onExportOPML}>
            <Icon name="download" size={14} /> 导出
          </button>
        </div>
      </div>
    </aside>
  )
}
