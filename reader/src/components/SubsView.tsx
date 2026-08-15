import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  FAILED_SUBSCRIPTION_THRESHOLD,
  SubscriptionSidebar,
} from './subscriptions/SubscriptionSidebar'
import { FeedItemsPane } from './subscriptions/FeedItemsPane'
import { FeedItemDetail } from './subscriptions/FeedItemDetail'
import { AddSubscriptionDialog, FolderDialog } from './subscriptions/SubscriptionDialogs'
import type { LibraryView } from './LibraryModeNav'
import {
  ALL_FEEDS_SELECTION,
  loadFeedSelection,
  saveFeedSelection,
  selectionFilters,
  type FeedSelection,
} from './subscriptions/model'
import { useSubscriptions } from '../hooks/useSubscriptions'
import { useFeedItems } from '../hooks/useFeedItems'
import { invalidateFeeds } from '../lib/cache/invalidate'
import type { ReaderRoute } from '../lib/navigation/route'
import {
  feedTitle,
  itemAnalysisStatus,
  itemIsRead,
  itemIsReadLater,
  itemIsStarred,
  patchItemState,
} from '../lib/feed'
import { ok, type ApiResult } from '../lib/api/result'
import type { ReaderClient } from '../lib/api/client'
import type {
  FeedFolder,
  FeedItem,
  FeedItemStatePatch,
  FeedSubscription,
} from '../lib/api/types'
import type { IdentityOwnership } from '../lib/identity'
import type { ReaderCapabilityPolicy } from '../lib/capabilities'

export interface SubsViewProps {
  client: ReaderClient
  navigationOpen: boolean
  onCloseNavigation: () => void
  onView: (view: LibraryView) => void
  /** Canonical route owner shared with the vNext surface rail. */
  onNavigate?: (route: ReaderRoute) => void
  collapsed: boolean
  onOpenAnalysis: (linkID: string) => void
  onOpenSettings: () => void
  onToast: (message: string, icon?: import('./Icon').IconName) => void
  syncRequest?: number
  capabilityPolicy: ReaderCapabilityPolicy
}

interface SubscriptionBatchResult {
  succeeded: FeedSubscription[]
  failures: string[]
}

function staleComponentResult<T>(): ApiResult<T> {
  return {
    ok: false,
    error: {
      kind: 'identity-mismatch',
      message: 'Component continuation belongs to a stale identity epoch',
    },
  }
}

async function runSubscriptionBatch(
  subscriptions: FeedSubscription[],
  ownership: IdentityOwnership,
  action: (
    subscription: FeedSubscription,
    ownership: IdentityOwnership,
  ) => Promise<ApiResult<unknown>>,
): Promise<SubscriptionBatchResult> {
  const succeeded: FeedSubscription[] = []
  const failures: string[] = []
  let cursor = 0
  const worker = async () => {
    while (cursor < subscriptions.length) {
      if (!ownership.lease.isOwnershipCurrent(ownership)) return
      const subscription = subscriptions[cursor++]
      try {
        const result = await action(subscription, ownership)
        if (result.ok) succeeded.push(subscription)
        else failures.push(result.error.message)
      } catch (error) {
        failures.push(error instanceof Error ? error.message : '未知错误')
      }
    }
  }
  const workers = Array.from(
    { length: Math.min(4, subscriptions.length) },
    () => worker(),
  )
  await Promise.all(workers)
  return { succeeded, failures }
}

export function SubsView({
  client,
  navigationOpen,
  onCloseNavigation,
  onView,
  onNavigate,
  collapsed,
  onOpenAnalysis,
  onOpenSettings,
  onToast,
  syncRequest = 0,
  capabilityPolicy,
}: SubsViewProps) {
  const overview = useSubscriptions(client)
  const [selection, setSelection] = useState<FeedSelection>(loadFeedSelection)
  const [query, setQuery] = useState('')
  const [debouncedQuery, setDebouncedQuery] = useState('')
  const [activeID, setActiveID] = useState<string | null>(null)
  const [activeDetail, setActiveDetail] = useState<FeedItem | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [mobilePane, setMobilePane] = useState<'list' | 'detail'>('list')
  const [desktopDetailVisible, setDesktopDetailVisible] = useState(
    () => window.innerWidth > 1024,
  )
  const [addOpen, setAddOpen] = useState(false)
  const [folderDialog, setFolderDialog] = useState<{ open: boolean; folder?: FeedFolder }>({ open: false })
  const [refreshingID, setRefreshingID] = useState<string | 'all' | null>(null)
  const [analyzingID, setAnalyzingID] = useState<string | null>(null)
  const [batchBusy, setBatchBusy] = useState(false)
  const detailRequestSequence = useRef(0)
  const automaticOpenRef = useRef<string | null>(null)
  const handledSyncRequest = useRef(syncRequest)

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedQuery(query.trim()), 300)
    return () => window.clearTimeout(timer)
  }, [query])

  useEffect(() => {
    const update = () => setDesktopDetailVisible(window.innerWidth > 1024)
    window.addEventListener('resize', update)
    return () => window.removeEventListener('resize', update)
  }, [])

  const filters = useMemo(
    () => ({ ...selectionFilters(selection), q: debouncedQuery || undefined }),
    [selection, debouncedQuery],
  )
  const feedItems = useFeedItems(client, filters)
  const rememberSelection = useCallback((next: FeedSelection) => {
    setSelection(next)
    saveFeedSelection(next)
  }, [])

  useEffect(() => {
    if (overview.loading || selection.kind === 'view') return
    if (selection.kind === 'folder') {
      if (selection.id === 'ungrouped') return
      const folder = overview.data.folders.find((candidate) => candidate.id === selection.id)
      if (!folder) {
        rememberSelection(ALL_FEEDS_SELECTION)
      } else if (folder.name !== selection.name) {
        rememberSelection({ kind: 'folder', id: folder.id, name: folder.name })
      }
      return
    }
    const subscription = overview.data.subscriptions.find(
      (candidate) => candidate.id === selection.id,
    )
    if (!subscription) {
      rememberSelection(ALL_FEEDS_SELECTION)
    } else if (feedTitle(subscription) !== selection.name) {
      rememberSelection({
        kind: 'subscription',
        id: subscription.id,
        name: feedTitle(subscription),
      })
    }
  }, [overview.data.folders, overview.data.subscriptions, overview.loading, rememberSelection, selection])

  useEffect(() => {
    if (syncRequest === handledSyncRequest.current) return
    handledSyncRequest.current = syncRequest
    setRefreshingID('all')
    void client.refreshSubscriptions().then((result) => {
      if (!client.isIdentityCurrent()) return
      if (!result.ok) onToast(`刷新失败：${result.error.message}`, 'alert')
      else onToast('已请求刷新全部订阅源', 'refresh')
      void Promise.all([overview.reload(true), feedItems.reload(true)]).finally(() => {
        setRefreshingID(null)
      })
    })
  }, [client, feedItems, onToast, overview, syncRequest])

  const selectedSubscription = useMemo(
    () =>
      selection.kind === 'subscription'
        ? overview.data.subscriptions.find((subscription) => subscription.id === selection.id)
        : undefined,
    [overview.data.subscriptions, selection],
  )
  const active =
    (activeDetail?.id === activeID ? activeDetail : undefined) ??
    feedItems.items.find((item) => item.id === activeID) ??
    null
  const activeSource = active
    ? overview.data.subscriptions.find((subscription) => subscription.id === active.subscription_id)
    : undefined

  useEffect(() => {
    const latest = feedItems.items.find((item) => item.id === activeID)
    if (!latest) return
    setActiveDetail((current) =>
      current?.id === latest.id ? { ...current, ...latest } : current,
    )
  }, [activeID, feedItems.items])

  const patchEverywhere = useCallback(
    (id: string, patch: Partial<FeedItem>) => {
      feedItems.patchItem(id, patch)
      setActiveDetail((current) =>
        current?.id === id ? { ...current, ...patch } : current,
      )
    },
    [feedItems],
  )

  const stateLeavesCurrentView = useCallback(
    (item: FeedItem, patch: FeedItemStatePatch): boolean => {
      if (selection.kind !== 'view') return false
      if (selection.id === 'unread') return patch.read === true
      if (selection.id === 'starred') return patch.starred === false && itemIsStarred(item)
      if (selection.id === 'later') return patch.read_later === false && itemIsReadLater(item)
      return false
    },
    [selection],
  )

  const updateItemState = useCallback(
    async (item: FeedItem, patch: FeedItemStatePatch): Promise<void> => {
      const optimistic = patchItemState(item, patch)
      patchEverywhere(item.id, optimistic)
      if (stateLeavesCurrentView(item, patch)) feedItems.removeItem(item.id)

      const result = await client.updateFeedItem(item.id, patch)
      if (!client.isIdentityCurrent()) return
      if (!result.ok) {
        onToast(`保存文章状态失败：${result.error.message}`, 'alert')
        void feedItems.reload(true)
        void overview.reload(true)
        return
      }
      if (result.data) patchEverywhere(item.id, result.data)
      void overview.reload(true)
    },
    [client, feedItems, onToast, overview, patchEverywhere, stateLeavesCurrentView],
  )

  const openItem = useCallback(
    (item: FeedItem, revealMobileDetail = true) => {
      const shouldMarkRead = !itemIsRead(item)
      const optimistic = shouldMarkRead ? patchItemState(item, { read: true }) : item
      setActiveID(item.id)
      setActiveDetail(optimistic)
      if (revealMobileDetail) setMobilePane('detail')
      onCloseNavigation()

      const sequence = ++detailRequestSequence.current
      setDetailLoading(true)
      void client.getFeedItem(item.id).then((result) => {
        if (!client.isIdentityCurrent()) return
        if (sequence !== detailRequestSequence.current) return
        setDetailLoading(false)
        if (!result.ok) {
          if (shouldMarkRead) {
            setActiveDetail(item)
            feedItems.patchItem(item.id, item)
          }
          onToast(`加载订阅正文失败：${result.error.message}`, 'alert')
          return
        }
        const detail = shouldMarkRead ? patchItemState(result.data, { read: true }) : result.data
        setActiveDetail(detail)
        feedItems.patchItem(item.id, detail)
        if (shouldMarkRead) void overview.reload(true)
      })
    },
    [client, feedItems, onCloseNavigation, onToast, overview],
  )

  useEffect(() => {
    if (activeID) {
      automaticOpenRef.current = null
      return
    }
    if (!desktopDetailVisible || feedItems.loading || feedItems.items.length === 0) return
    const first = feedItems.items[0]
    if (automaticOpenRef.current === first.id) return
    automaticOpenRef.current = first.id
    openItem(first, false)
  }, [activeID, desktopDetailVisible, feedItems.items, feedItems.loading, openItem])

  const activeListIndex = activeID
    ? feedItems.items.findIndex((item) => item.id === activeID)
    : -1
  const previousItem = activeListIndex > 0 ? feedItems.items[activeListIndex - 1] : null
  const nextItem =
    activeListIndex >= 0 && activeListIndex < feedItems.items.length - 1
      ? feedItems.items[activeListIndex + 1]
      : null

  const pickSelection = (next: FeedSelection) => {
    rememberSelection(next)
    setMobilePane('list')
    onCloseNavigation()
  }

  // SubscriptionSidebar still owns the historical `onView` prop and embeds
  // LibraryModeNav. Adapt both its five library ids and the new surface/tool
  // routes to the same MainView route owner without changing that workspace's
  // data or interaction model.
  const routeAwareView = useCallback(
    (destination: LibraryView | ReaderRoute) => {
      if (onNavigate) {
        onNavigate(typeof destination === 'string'
          ? { kind: 'library', id: destination }
          : destination)
        return
      }
      if (typeof destination === 'string') {
        onView(destination)
        return
      }
      const legacyNavigate = onView as unknown as (route: ReaderRoute) => void
      legacyNavigate(destination)
    },
    [onNavigate, onView],
  )

  const refresh = async () => {
    const subscription = selectedSubscription
    const id = subscription?.id ?? 'all'
    setRefreshingID(id)
    if (subscription) overview.patchSubscription(subscription.id, { refreshing: true })
    const result = subscription
      ? await client.refreshSubscription(subscription.id)
      : await client.refreshSubscriptions()
    if (!client.isIdentityCurrent()) return
    if (!result.ok) {
      onToast(`刷新失败：${result.error.message}`, 'alert')
    } else {
      onToast(subscription ? '已请求刷新当前订阅源' : '已请求刷新全部订阅源', 'refresh')
    }
    await Promise.all([overview.reload(true), feedItems.reload(true)])
    setRefreshingID(null)
  }

  const markAllRead = async () => {
    const result = await client.markFeedItemsRead(filters)
    if (!client.isIdentityCurrent()) return
    if (!result.ok) {
      onToast(`标记失败：${result.error.message}`, 'alert')
      return
    }
    invalidateFeeds() // 这类写操作会改变多个筛选下的条目与计数，按前缀整片失效
    if (active) setActiveDetail(patchItemState(active, { read: true }))
    onToast('已全部标为已读', 'check')
    void overview.reload(true)
    void feedItems.reload(true)
  }

  const analyze = async (item: FeedItem) => {
    if (itemAnalysisStatus(item) === 'done' && item.link_id) {
      onOpenAnalysis(item.link_id)
      return
    }
    setAnalyzingID(item.id)
    patchEverywhere(item.id, { analysis_status: 'pending', analysis_error: null })
    const result = await client.analyzeFeedItem(item.id)
    if (!client.isIdentityCurrent()) return
    setAnalyzingID(null)
    if (!result.ok) {
      patchEverywhere(item.id, { analysis_status: 'failed', analysis_error: result.error.message })
      onToast(`AI 分析提交失败：${result.error.message}`, 'alert')
      return
    }
    if (result.data.item) {
      patchEverywhere(item.id, result.data.item)
    } else {
      patchEverywhere(item.id, {
        analysis_status: result.data.status ?? 'pending',
        link_id: result.data.link_id ?? item.link_id,
      })
    }
    onToast('已加入 AI 分析队列', 'sparkles')
    void feedItems.reload(true)
  }

  const moveSubscription = async (subscription: FeedSubscription, folderID: string | null) => {
    overview.patchSubscription(subscription.id, { folder_id: folderID })
    const result = await client.updateSubscription(subscription.id, { folder_id: folderID })
    if (!client.isIdentityCurrent()) return
    if (!result.ok) {
      overview.patchSubscription(subscription.id, { folder_id: subscription.folder_id ?? null })
      onToast(`移动订阅源失败：${result.error.message}`, 'alert')
      return
    }
    onToast(folderID ? '已移动订阅源' : '已移到未分组', 'folder')
    void overview.reload(true)
  }

  const deleteSubscription = async (subscription: FeedSubscription) => {
    const confirmed = window.confirm(
      `取消订阅“${feedTitle(subscription)}”？收藏、稍后读和已生成的 AI 分析会继续保留。`,
    )
    if (!confirmed) return
    const result = await client.deleteSubscription(subscription.id)
    if (!client.isIdentityCurrent()) return
    if (!result.ok) {
      onToast(`取消订阅失败：${result.error.message}`, 'alert')
      return
    }
    invalidateFeeds() // 这类写操作会改变多个筛选下的条目与计数，按前缀整片失效
    if (selection.kind === 'subscription' && selection.id === subscription.id) {
      rememberSelection(ALL_FEEDS_SELECTION)
    }
    if (active?.subscription_id === subscription.id) {
      setActiveID(null)
      setActiveDetail(null)
      setMobilePane('list')
    }
    onToast('已取消订阅', 'check')
    void overview.reload(true)
    void feedItems.reload(true)
  }

  const performSubscriptionBatch = async (
    subscriptions: FeedSubscription[],
    action: (
      subscription: FeedSubscription,
      ownership: IdentityOwnership,
    ) => Promise<ApiResult<unknown>>,
  ): Promise<SubscriptionBatchResult | null> => {
    if (batchBusy || subscriptions.length === 0) return null
    const ownership = client.captureIdentity('SubsView subscription batch')
    if (!ownership) return null
    setBatchBusy(true)
    try {
      const result = await runSubscriptionBatch(subscriptions, ownership, action)
      if (!ownership.lease.isOwnershipCurrent(ownership)) return null
      await Promise.all([overview.reload(true), feedItems.reload(true)])
      if (!ownership.lease.isOwnershipCurrent(ownership)) return null
      return result
    } finally {
      setBatchBusy(false)
    }
  }

  const reportBatchResult = (
    action: string,
    result: SubscriptionBatchResult,
    icon: import('./Icon').IconName,
  ) => {
    if (result.failures.length === 0) {
      onToast(`已${action} ${result.succeeded.length} 个订阅源`, icon)
      return
    }
    onToast(
      `${action}完成：${result.succeeded.length} 个成功，${result.failures.length} 个失败`,
      'alert',
    )
  }

  const refreshSubscriptionBatch = async (subscriptions: FeedSubscription[]) => {
    const result = await performSubscriptionBatch(subscriptions, (subscription, ownership) => {
      overview.patchSubscriptionForIdentity(ownership, subscription.id, { refreshing: true })
      return client.refreshSubscription(subscription.id)
    })
    if (result) reportBatchResult('刷新', result, 'refresh')
  }

  const moveSubscriptionBatch = async (
    subscriptions: FeedSubscription[],
    folderID: string | null,
  ) => {
    const result = await performSubscriptionBatch(subscriptions, (subscription) =>
      client.updateSubscription(subscription.id, { folder_id: folderID }),
    )
    if (result) reportBatchResult('移动', result, 'folder')
  }

  const deleteSubscriptionBatch = async (
    subscriptions: FeedSubscription[],
    failedOnly: boolean,
  ) => {
    if (subscriptions.length === 0) return
    const message = failedOnly
      ? `删除 ${subscriptions.length} 个连续失败至少 ${FAILED_SUBSCRIPTION_THRESHOLD} 次的订阅源？`
      : `删除已选的 ${subscriptions.length} 个订阅源？`
    if (!window.confirm(`${message}收藏、稍后读和已生成的 AI 分析会继续保留。`)) return

    const result = await performSubscriptionBatch(subscriptions, (subscription) =>
      client.deleteSubscription(subscription.id),
    )
    if (!result) return
    const deletedIDs = new Set(result.succeeded.map((subscription) => subscription.id))
    if (selection.kind === 'subscription' && deletedIDs.has(selection.id)) {
      rememberSelection(ALL_FEEDS_SELECTION)
    }
    if (active && deletedIDs.has(active.subscription_id)) {
      setActiveID(null)
      setActiveDetail(null)
      setMobilePane('list')
    }
    reportBatchResult(failedOnly ? '清理' : '删除', result, 'check')
  }

  const saveFolder = async (name: string): Promise<ApiResult<true>> => {
    const result = folderDialog.folder
      ? await client.updateFeedFolder(folderDialog.folder.id, name)
      : await client.createFeedFolder(name)
    if (!client.isIdentityCurrent()) return staleComponentResult<true>()
    if (!result.ok) return result
    void overview.reload(true)
    return ok(true)
  }

  const deleteFolder = async (folder: FeedFolder) => {
    if (!window.confirm(`删除文件夹“${folder.name}”？其中的订阅源会移到未分组。`)) return
    const result = await client.deleteFeedFolder(folder.id)
    if (!client.isIdentityCurrent()) return
    if (!result.ok) {
      onToast(`删除文件夹失败：${result.error.message}`, 'alert')
      return
    }
    invalidateFeeds() // 这类写操作会改变多个筛选下的条目与计数，按前缀整片失效
    if (selection.kind === 'folder' && selection.id === folder.id) {
      rememberSelection(ALL_FEEDS_SELECTION)
    }
    onToast('已删除文件夹，订阅源已移到未分组', 'folder')
    void overview.reload(true)
    void feedItems.reload(true)
  }

  const subscribe = async (url: string, folderID: string | null): Promise<ApiResult<true>> => {
    const result = await client.createSubscription({ url, folder_id: folderID })
    if (!client.isIdentityCurrent()) return staleComponentResult<true>()
    if (!result.ok) return result
    invalidateFeeds() // 这类写操作会改变多个筛选下的条目与计数，按前缀整片失效
    rememberSelection({ kind: 'subscription', id: result.data.id, name: feedTitle(result.data) })
    setMobilePane('list')
    onToast(`已订阅 ${feedTitle(result.data)}`, 'rss')
    void overview.reload(true)
    void feedItems.reload(true)
    return ok(true)
  }

  const importOPML = async (file: File) => {
    if (file.size > 768 * 1024) {
      onToast('OPML 文件不能超过 768 KB', 'alert')
      return
    }
    let opml: string
    try {
      opml = await file.text()
    } catch {
      onToast('无法读取 OPML 文件', 'alert')
      return
    }
    if (!client.isIdentityCurrent()) return
    const result = await client.importSubscriptionsOPML(opml)
    if (!client.isIdentityCurrent()) return
    if (!result.ok) {
      onToast(`导入 OPML 失败：${result.error.message}`, 'alert')
      return
    }
    invalidateFeeds() // 这类写操作会改变多个筛选下的条目与计数，按前缀整片失效
    onToast(
      `OPML 导入完成：新增 ${result.data.imported} 个，自动跳过 ${result.data.skipped} 个`,
      'check',
    )
    void overview.reload(true)
    void feedItems.reload(true)
  }

  const exportOPML = async () => {
    const result = await client.exportSubscriptionsOPML()
    if (!client.isIdentityCurrent()) return
    if (!result.ok) {
      onToast(`导出 OPML 失败：${result.error.message}`, 'alert')
      return
    }
    try {
      const href = URL.createObjectURL(new Blob([result.data], { type: 'text/x-opml+xml' }))
      const anchor = document.createElement('a')
      anchor.href = href
      anchor.download = `webtag-subscriptions-${new Date().toISOString().slice(0, 10)}.opml`
      anchor.click()
      window.setTimeout(() => URL.revokeObjectURL(href), 0)
      onToast('OPML 已导出', 'download')
    } catch {
      onToast('浏览器无法保存 OPML 文件', 'alert')
    }
  }

  return (
    <div className={'rss-workspace' + (mobilePane === 'detail' ? ' rss-mobile-detail' : '')}>
      <SubscriptionSidebar
        data={overview.data}
        selection={selection}
        loading={overview.loading}
        onSelect={pickSelection}
        onView={routeAwareView}
        capabilityPolicy={capabilityPolicy}
        collapsed={collapsed}
        onAddSubscription={() => setAddOpen(true)}
        onAddFolder={() => setFolderDialog({ open: true })}
        onEditFolder={(folder) => setFolderDialog({ open: true, folder })}
        onDeleteFolder={(folder) => void deleteFolder(folder)}
        onMoveSubscription={(subscription, folderID) => void moveSubscription(subscription, folderID)}
        onRefreshSubscription={(subscription) => {
          if (selection.kind !== 'subscription' || selection.id !== subscription.id) {
            rememberSelection({
              kind: 'subscription',
              id: subscription.id,
              name: feedTitle(subscription),
            })
          }
          void (async () => {
            setRefreshingID(subscription.id)
            overview.patchSubscription(subscription.id, { refreshing: true })
            const result = await client.refreshSubscription(subscription.id)
            if (!client.isIdentityCurrent()) return
            if (!result.ok) onToast(`刷新失败：${result.error.message}`, 'alert')
            else onToast('已请求刷新当前订阅源', 'refresh')
            await Promise.all([overview.reload(true), feedItems.reload(true)])
            setRefreshingID(null)
          })()
        }}
        onDeleteSubscription={(subscription) => void deleteSubscription(subscription)}
        batchBusy={batchBusy}
        onBatchRefresh={(subscriptions) => void refreshSubscriptionBatch(subscriptions)}
        onBatchMove={(subscriptions, folderID) =>
          void moveSubscriptionBatch(subscriptions, folderID)
        }
        onBatchDelete={(subscriptions) => void deleteSubscriptionBatch(subscriptions, false)}
        onDeleteFailed={(subscriptions) => void deleteSubscriptionBatch(subscriptions, true)}
        onImportOPML={(file) => void importOPML(file)}
        onExportOPML={() => void exportOPML()}
      />
      <button
        type="button"
        className={'rss-nav-backdrop' + (navigationOpen ? ' open' : '')}
        aria-label="关闭订阅导航"
        onClick={onCloseNavigation}
      />
      <FeedItemsPane
        title={selection.name}
        items={feedItems.items}
        total={feedItems.total}
        subscriptions={overview.data.subscriptions}
        selectedSubscription={selectedSubscription}
        activeID={activeID}
        query={query}
        loading={feedItems.loading}
        loadingMore={feedItems.loadingMore}
        error={feedItems.error ?? overview.error}
        hasMore={feedItems.hasMore}
        refreshing={
          refreshingID === (selectedSubscription?.id ?? 'all') ||
          Boolean(selectedSubscription?.refreshing)
        }
        onQueryChange={setQuery}
        onSelect={openItem}
        onToggleStar={(item) => void updateItemState(item, { starred: !itemIsStarred(item) })}
        onToggleLater={(item) => void updateItemState(item, { read_later: !itemIsReadLater(item) })}
        onLoadMore={() => void feedItems.loadMore()}
        onReload={() => void feedItems.reload()}
        onRefresh={() => void refresh()}
        onMarkAllRead={() => void markAllRead()}
        onOpenSettings={onOpenSettings}
      />
      <FeedItemDetail
        item={active}
        source={activeSource}
        loading={detailLoading}
        analyzing={analyzingID === active?.id}
        onBack={() => setMobilePane('list')}
        onToggleRead={(item) => void updateItemState(item, { read: itemIsRead(item) ? false : true })}
        onToggleStar={(item) => void updateItemState(item, { starred: !itemIsStarred(item) })}
        onToggleLater={(item) => void updateItemState(item, { read_later: !itemIsReadLater(item) })}
        onAnalyze={(item) => void analyze(item)}
        onViewAnalysis={onOpenAnalysis}
        previous={
          previousItem
            ? {
                title: previousItem.title?.trim() || '无标题文章',
                onSelect: () => openItem(previousItem),
              }
            : null
        }
        next={
          nextItem
            ? {
                title: nextItem.title?.trim() || '无标题文章',
                onSelect: () => openItem(nextItem),
              }
            : null
        }
      />

      <AddSubscriptionDialog
        open={addOpen}
        folders={overview.data.folders}
        onClose={() => setAddOpen(false)}
        onDiscover={(url) => client.discoverFeeds(url)}
        onSubscribe={subscribe}
      />
      <FolderDialog
        open={folderDialog.open}
        folder={folderDialog.folder}
        onClose={() => setFolderDialog({ open: false })}
        onSave={saveFolder}
      />
    </div>
  )
}
