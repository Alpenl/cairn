/**
 * 三栏主界面外壳，连接配置留在上层 App.tsx。
 *
 * 职责：状态机（theme/view/sel/activeId/chatOpen/cmdkOpen/browse/toast）、
 * titlebar（交通灯 + 同步=真实重拉 + ⌘K 胶囊 + 主题 + AI + 设置齿轮）、
 * 真实数据流（当前列表 + truthful 标签/域名摘要 + 有界已见语料）、
 * Mail 式「详情永不因筛选自动切换」原则、⌘K/⌘J 快捷键、Toast。
 *
 * R3 接线：CommandPalette（⌘K 接后端 q= 关键词搜索）、BrowsePanel（标签 / 域名全集浏览）、
 * 划线笔记全流程（durable annotation document 提升到此处，DetailPane 与 NotePanel 共享同一投影）。
 * R4 接线：ChatSidebar（AI 助手，离线回退）、SubsView（订阅源）、网站转换与原文保存。
 * 右栏互斥：NotePanel 与 ChatSidebar 共用同一条
 * 右栏。问 AI 创建 AI 划线 + 写草稿（chatDraft），ChatSidebar 消费并「采用为笔记」回写。
 */
import { useLayoutEffect, useMemo } from 'react'
import { Sidebar } from './Sidebar'
import { ListPane } from './ListPane'
import { DetailPane } from './DetailPane'
import { Toast } from './Toast'
import { CommandPalette } from './CommandPalette'
import { BrowsePanel } from './BrowsePanel'
import { NotePanel } from './NotePanel'
import { ChatSidebar } from './ChatSidebar'
import { SubsView } from './SubsView'
import { SitesView } from './SitesView'
import { LinkConversionDialog } from './LinkConversionDialog'
import { Titlebar } from './Titlebar'
import { AddLinkDialog } from './AddLinkDialog'
import { ArchiveDownloadDialog } from './ArchiveDownloadDialog'
import { HomeSurface } from './reader-vnext/HomeSurface'
import { FeedSurface } from './reader-vnext/FeedSurface'
import { InboxSurface } from './reader-vnext/InboxSurface'
import { NotesSurface } from './reader-vnext/NotesSurface'
import { TodoSurface } from './reader-vnext/TodoSurface'
import { SettingsSurface } from './reader-vnext/SettingsSurface'
import { ThoughtHistorySurface } from './reader-vnext/ThoughtHistorySurface'
import { TrashSurface } from './reader-vnext/TrashSurface'
import { PendingInboxCountProvider } from './reader-vnext/PendingInboxCount'
import type { LibraryView } from './PrimaryNav'
import type {
  CapabilitiesResponse,
} from '../lib/api/types'
import type {
  ReaderAIPort,
  ReaderHomePort,
  ReaderInboxTodosPort,
  ReaderLibrarySitesPort,
  ReaderSessionArchivePort,
  ReaderSubscriptionsFeedPort,
  ReaderThoughtsNotesPort,
} from '../lib/reader-api-ports'
import {
  deriveReaderCapabilityPolicy,
  ReaderCapabilityLease,
  readerCapabilityFingerprint,
} from '../lib/capabilities'
import {
  useMainViewNavigation,
} from './main-view/navigation-controller'
import { useActiveResourceController } from './main-view/active-resource-controller'
import { useSavedDocumentWorkspace } from './main-view/saved-document-workspace'
import { useSyncArchiveController } from './main-view/sync-archive-controller'
import {
  useMainViewAppController,
  useMainViewToast,
} from './main-view/app-composition-controller'

type MainViewClient = ReaderLibrarySitesPort &
  ReaderSubscriptionsFeedPort &
  ReaderThoughtsNotesPort &
  ReaderInboxTodosPort &
  ReaderHomePort &
  ReaderSessionArchivePort &
  ReaderAIPort

export interface MainViewProps {
  client: MainViewClient
  /** Capabilities are fetched after identity; omitted keeps legacy tests local. */
  capabilities?: CapabilitiesResponse
  /** 打开连接配置编辑（titlebar 齿轮）。 */
  onOpenSettings: () => void
  /** Re-probe runtime capabilities as part of an explicit sync. */
  onRefreshCapabilities?: () => void
}

export function MainView({ client, capabilities, onOpenSettings, onRefreshCapabilities }: MainViewProps) {
  const lease = client.identityLease
	const capabilityPolicy = useMemo(
		() => deriveReaderCapabilityPolicy(capabilities),
		[capabilities],
	)
	const capabilityFingerprint = readerCapabilityFingerprint(capabilityPolicy)
	const capabilityLease = useMemo(
		() => new ReaderCapabilityLease(capabilityPolicy),
		// The fingerprint is the capability generation boundary. A new object
		// carrying the same strict values must not reset every temporary surface.
		// eslint-disable-next-line react-hooks/exhaustive-deps
		[capabilityFingerprint],
	)
	useLayoutEffect(() => {
		capabilityLease.activate()
		return () => capabilityLease.deactivate()
	}, [capabilityLease])
  const { toast, flash, dismissToast } = useMainViewToast()
  const {
    view,
    displayedView,
    siteTargetID,
    noteTargetID,
    inboxTargetID,
    activeId,
    setActiveId,
    mobilePane,
    setMobilePane,
    mobileNavOpen,
    setMobileNavOpen,
    contentEditState,
    navigationRestoreEpoch,
    pendingLinkTarget,
    getContentEditState,
    reportContentEditState,
    confirmDiscardContentEdit,
    confirmDiscardNavigation,
    commitRoute,
    navigateRoute,
    reportNotesDraftDirty,
    reportNotesPendingPersistence,
    reportNotesPrepareToLeave,
    reportInboxDraftState,
  } = useMainViewNavigation({ lease, capabilityPolicy, flash })
  const {
    selection: sel,
    setSelection: setSel,
    list,
    reloadLinks,
    reloadTags,
    reloadDomains,
    protectedListLinks,
    corpus,
    renderedActive,
    aiContentContext,
    savedArticle,
    savedDocument,
    captureSavedDocumentContext,
    loadSavedDocumentBody,
    detailLoading,
    summaryBlock,
    activeSummarySourceHash,
    summaryProjectionEpoch,
    onSummaryBlockText,
    resetSummarySourceHash,
    revisionFloor,
    noteContentRevision,
    patchKnownLink,
    onSaveLinkMetadata,
    getActiveLink,
    openLink,
    onDeleteLink,
    clearActiveResource,
    sidebarCounts,
    tagStatList,
    domainStatList,
    tagsAvailable,
    domainsAvailable,
    previousPager,
    nextPager,
  } = useActiveResourceController({
    client,
    lease,
    capabilityLease,
    activeId,
    setActiveId,
    view,
    pendingLinkTarget,
    commitRoute,
    confirmDiscardNavigation,
    confirmDiscardContentEdit,
    setMobilePane,
    setMobileNavOpen,
    flash,
    dismissToast,
  })
  const {
    librarySyncing,
    librarySyncFailures,
    thoughtSync,
    syncLibraryAndThoughts,
    subscriptionSyncRequest,
    requestSubscriptionSync,
    archiveDownloading,
    downloadArchive,
  } = useSyncArchiveController({
    client,
    lease,
    capabilityLease,
    reloadLinks,
    reloadTags,
    reloadDomains,
    onRefreshCapabilities,
    flash,
  })
  const {
    anns,
    translations,
    staleTranslations,
    translationsLoading,
    historicalAnnotations,
    historicalDegraded,
    addAnnotation,
    updateAnnotation,
    removeAnnotation,
    onSaveContent,
    onReplaceContent,
    onSaveContentEdit,
    onTranslateSelection,
    onTranslateFull,
    savingContent,
  } = useSavedDocumentWorkspace({
    client,
    lease,
    capabilityPolicy,
    capabilityLease,
    activeId,
    renderedActive,
    savedArticle,
    savedDocument,
    captureSavedDocumentContext,
    summaryBlock,
    activeSummarySourceHash,
    revisionFloor,
    getActiveLink,
    getContentEditState,
    reportContentEditState,
    resetSummarySourceHash,
    noteContentRevision,
    patchKnownLink,
    flash,
  })

  const app = useMainViewAppController({
    client,
    capabilityPolicy,
    capabilityLease,
    activeId,
    anns,
    historicalAnnotations,
    setSelection: setSel,
    setMobilePane,
    setMobileNavOpen,
    confirmDiscardContentEdit,
    confirmDiscardNavigation,
    commitRoute,
    navigateRoute,
    openLink,
    getActiveLink,
    clearActiveResource,
    reloadList: list.reload,
    updateAnnotation,
    removeAnnotation,
    onOpenSettings,
    syncLibraryAndThoughts,
    flash,
  })

  return (
    <div className="stage">
      <div className="win" key={capabilityLease.generation}>
        <Titlebar
          theme={app.theme}
          chatOpen={app.chatOpen}
          navigationOpen={mobileNavOpen}
          sidebarCollapsed={app.sidebarCollapsed}
          syncing={displayedView !== 'subs' && librarySyncing}
          thoughtSync={thoughtSync}
          onSync={
            displayedView === 'subs'
              ? requestSubscriptionSync
              : syncLibraryAndThoughts
          }
          onToggleNavigation={app.toggleNavigation}
          onAddLink={app.openAddLinkDialog}
          onOpenCmdk={app.openCommandPalette}
          onToggleTheme={app.onToggleTheme}
          onToggleChat={app.toggleChat}
          onOpenSettings={app.openSettings}
          archiveDownloading={archiveDownloading}
          onDownloadArchive={app.openArchiveDialog}
          canUseAI={capabilityPolicy.ai}
          canDownloadArchive={capabilityPolicy.archiveDownload}
        />

        {displayedView !== 'subs' && librarySyncFailures.length > 0 && (
          <div className="library-sync-error" role="alert">
            <span>资料库同步部分失败：{librarySyncFailures.join('、')}</span>
            <button type="button" onClick={syncLibraryAndThoughts} disabled={librarySyncing}>
              重试资料库同步
            </button>
          </div>
        )}

        <PendingInboxCountProvider client={client} capabilityLease={capabilityLease}>
        <div
          aria-busy={displayedView === 'reading' && detailLoading ? true : undefined}
          className={
            'body' +
            (displayedView === 'reading' && mobilePane === 'detail' ? ' mobile-detail-active' : '') +
            (mobileNavOpen ? ' mobile-nav-open' : '') +
            (app.sidebarCollapsed ? ' sidebar-collapsed' : '') +
            (app.focusMode && displayedView === 'reading' ? ' focus-mode' : '') +
            (app.chatOpen || app.notePanelEditing || contentEditState?.editing ? ' mobile-tool-open' : '')
          }
        >
          {displayedView === 'home' ? (
            <HomeSurface
              client={client}
              lease={lease}
              capabilityLease={capabilityLease}
              onNavigate={navigateRoute}
              onOpenLink={openLink}
              scrollRef={app.homeScrollRef}
              feedSlot={(
                <FeedSurface
                  client={client}
                  capabilityLease={capabilityLease}
                  onNavigate={navigateRoute}
                  onOpenLink={openLink}
                  variant="embedded"
                  hostScrollRef={app.homeScrollRef}
                />
              )}
            />
          ) : displayedView === 'feed' ? (
            <FeedSurface client={client} capabilityLease={capabilityLease} onNavigate={navigateRoute} onOpenLink={openLink} />
          ) : displayedView === 'pending' ? (
            <InboxSurface client={client} capabilityPolicy={capabilityPolicy} onNavigate={navigateRoute} onOpenLink={openLink} initialInboxID={inboxTargetID} onDraftStateChange={reportInboxDraftState} />
          ) : displayedView === 'notes' ? (
            <NotesSurface
              client={client}
              lease={lease}
              capabilityLease={capabilityLease}
              onNavigate={navigateRoute}
              initialNoteID={noteTargetID}
              onDraftDirtyChange={reportNotesDraftDirty}
              onPendingPersistenceChange={reportNotesPendingPersistence}
              onPrepareToLeaveChange={reportNotesPrepareToLeave}
              onCreateNote={app.canCreateNote ? () => { void app.createEmptyNote() } : undefined}
              creatingNote={app.creatingNote}
              annotationsEnabled={capabilityPolicy.annotations}
              aiEnabled={capabilityPolicy.ai}
              trashEnabled={capabilityPolicy.trash}
            />
          ) : displayedView === 'todo' ? (
            <TodoSurface
              client={client}
              capabilityPolicy={capabilityPolicy}
              onNavigate={navigateRoute}
              onOpenLink={openLink}
              completedExpanded={app.todoCompletedExpanded}
              onCompletedExpandedChange={app.setTodoCompletedExpanded}
            />
          ) : displayedView === 'settings' ? (
            <SettingsSurface client={client} capabilityPolicy={capabilityPolicy} onNavigate={navigateRoute} onOpenConnectionSettings={app.openSettings} />
          ) : displayedView === 'history' ? (
			<ThoughtHistorySurface client={client} lease={lease} capabilityLease={capabilityLease} onNavigate={navigateRoute} />
          ) : displayedView === 'trash' ? (
            <TrashSurface client={client} onNavigate={navigateRoute} capabilityPolicy={capabilityPolicy} onToast={flash} />
          ) : displayedView === 'subs' ? (
              <SubsView
              client={client}
              navigationOpen={mobileNavOpen}
              onCloseNavigation={() => setMobileNavOpen(false)}
              onView={app.onSidebarView}
              onNavigate={navigateRoute}
              collapsed={app.sidebarCollapsed}
              onOpenAnalysis={openLink}
              onOpenSettings={app.openSettings}
              onToast={flash}
              syncRequest={subscriptionSyncRequest}
              capabilityPolicy={capabilityPolicy}
            />
          ) : displayedView === 'sites' ? (
            <SitesView
              client={client}
              capabilityLease={capabilityLease}
              onToast={flash}
              initialSiteId={siteTargetID}
              collapsed={app.sidebarCollapsed}
              onView={app.onSidebarView}
              onNavigate={navigateRoute}
              navigationOpen={mobileNavOpen}
              onCloseNavigation={() => setMobileNavOpen(false)}
            />
          ) : (
            <>
              <Sidebar
                sel={sel}
                onSelect={app.onSidebarSelect}
                view={displayedView as LibraryView}
                onView={app.onSidebarView}
                onNavigate={navigateRoute}
                collapsed={app.sidebarCollapsed}
                pins={app.pins}
                onTogglePin={app.onTogglePin}
                onBrowse={app.onSidebarBrowse}
                tags={tagStatList}
                domains={domainStatList}
                tagsAvailable={tagsAvailable}
                domainsAvailable={domainsAvailable}
                counts={sidebarCounts}
                readerClient={client}
                capabilityPolicy={capabilityPolicy}
              />
              <button
                type="button"
                className="mobile-nav-backdrop"
                aria-label="关闭导航"
                onClick={() => setMobileNavOpen(false)}
              />
              <ListPane
                title={sel.name}
                links={protectedListLinks}
                activeId={activeId}
                onSelect={openLink}
                loading={list.loading}
                loadingMore={list.loadingMore}
                error={list.error}
                hasMore={list.hasMore}
                onLoadMore={list.loadMore}
                onReload={list.reload}
                onOpenSettings={app.openSettings}
              />
              <DetailPane
                l={renderedActive}
                document={savedDocument}
                captureDocumentContext={captureSavedDocumentContext}
                onBack={app.backToMobileList}
                chatOpen={app.chatOpen}
                onChat={app.toggleChat}
                onToast={flash}
                curTag={sel.type === 'tag' ? sel.id : null}
                onPickTag={app.onPickTag}
                corpus={corpus}
                anns={anns}
                historicalAnnotations={historicalAnnotations}
                historicalDegraded={historicalDegraded}
                onOpenHistoricalAnnotation={app.openHistoricalAnnotation}
                onAddAnn={addAnnotation}
                onRemoveAnn={removeAnnotation}
                onOpenNote={app.openNote}
                onAskAI={app.onAskAI}
                annotationsEnabled={capabilityPolicy.annotations}
                aiEnabled={capabilityPolicy.ai}
                relatedTagsEnabled={capabilityPolicy.relatedTags}
                engagementEnabled={capabilityPolicy.engagement}
                onSaveContent={onSaveContent}
                onReplaceContent={onReplaceContent}
                onEditContent={onSaveContentEdit}
                onEditMetadata={onSaveLinkMetadata}
                onContentEditStateChange={reportContentEditState}
                navigationRestoreEpoch={navigationRestoreEpoch}
                onLoadContent={loadSavedDocumentBody}
                savingContent={savingContent}
                loadingDetail={detailLoading}
                translations={translations}
                staleTranslations={staleTranslations}
                translationsLoading={translationsLoading}
                onSummaryBlockText={onSummaryBlockText}
                summarySourceHash={summaryBlock?.sourceHash ?? null}
                summaryProjectionEpoch={summaryProjectionEpoch}
                onTranslateSelection={onTranslateSelection}
                onTranslateFull={onTranslateFull}
				focusMode={app.focusMode}
				onToggleFocus={app.onToggleFocus}
				onConvertToSite={capabilityPolicy.siteWrite ? app.onConvertToSite : undefined}
				onDeleteLink={onDeleteLink}
				previous={previousPager}
				next={nextPager}
              />
            </>
          )}
          {capabilityPolicy.annotations && app.notePanelAnnotation && displayedView === 'reading' && (
            <NotePanel
              ann={app.notePanelAnnotation}
              readOnly={app.notePanelReadOnly}
              onSave={app.onSaveNotePanel}
              onDelete={app.onDeleteNotePanel}
              onAskAI={app.onAskAINotePanel}
              onClose={app.closeNotePanel}
            />
          )}
			{capabilityPolicy.siteWrite && app.convertingLink && <LinkConversionDialog capabilityLease={capabilityLease} client={client} link={app.convertingLink} initialNote={app.conversionInitialNote} onClose={app.closeConversionDialog} onToast={flash} onConverted={app.onConversionConverted} />}
          {capabilityPolicy.ai && app.chatOpen && displayedView === 'reading' && !app.notePanelAnnotation && (
            <ChatSidebar
              client={client}
              link={renderedActive}
              contentContext={aiContentContext}
              draft={app.chatDraft}
              onAdopt={app.onAdoptNote}
              onClearDraft={app.clearChatDraft}
              onClose={app.closeChat}
            />
          )}
        </div>
        </PendingInboxCountProvider>

        <CommandPalette
          open={app.cmdkOpen}
          onClose={app.closeCommandPalette}
          onCommand={app.onCommand}
          client={client}
          corpus={corpus}
          tagStats={tagStatList}
          domainStats={domainStatList}
          canCreateNote={app.canCreateNote}
          capabilityPolicy={capabilityPolicy}
        />
        {app.addLinkOpen && (
          <AddLinkDialog
            client={client}
            capabilityLease={capabilityLease}
            destination={capabilityPolicy.inbox ? 'inbox' : 'library'}
            onClose={app.closeAddLinkDialog}
            onToast={flash}
            onAdded={app.onAddLinkAdded}
          />
        )}
        <ArchiveDownloadDialog
          open={app.archiveDialogOpen}
          downloading={archiveDownloading}
          onClose={app.closeArchiveDialog}
          onDownload={downloadArchive}
        />
        {app.browse && (
          <BrowsePanel
            kind={app.browse}
            onClose={app.closeBrowse}
            pins={app.pins}
            onTogglePin={app.onTogglePin}
            tags={tagStatList}
            domains={domainStatList}
            readerClient={client}
            activityEnabled={capabilityPolicy.activity}
            onPick={app.onBrowsePick}
          />
        )}

        <Toast msg={toast?.msg ?? null} icon={toast?.icon} action={toast?.action} />
        {app.updateReady && (
          <button
            type="button"
            className="ne-btn"
            style={{ position: 'fixed', right: 20, bottom: 20, zIndex: 90 }}
            onClick={app.applyUpdate}
          >
            新版本已就绪，点击刷新
          </button>
        )}
      </div>
    </div>
  )
}
