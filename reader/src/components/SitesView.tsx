import { type FormEvent, useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from './Icon'
import { LibraryModeNav, type LibraryView } from './LibraryModeNav'
import { ReaderDialog } from './ui/ReaderDialog'
import { useSites } from '../hooks/useSites'
import { invalidateLibrary, invalidateLink } from '../lib/cache/invalidate'
import type { ReaderRoute } from '../lib/navigation/route'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { ReaderCapabilityLease } from '../lib/capabilities'
import type { SiteDetailResponse, SiteEntryResponse, SiteListItemResponse, SiteMergePreviewResponse, SiteSplitPreviewResponse, SiteSplitRequest, SiteUpdateRequest } from '../lib/api/types'

type SiteView = 'all' | 'pinned' | 'recent'
const labels: Record<SiteView, string> = { all: '全部网站', pinned: '已置顶', recent: '最近收藏' }
type SiteSplitOutcome = 'success' | 'conflict' | 'error'

interface SitesViewProps {
	client: IdentityBoundReaderClient
	onToast: (message: string, icon?: import('./Icon').IconName) => void
	initialSiteId?: string
	collapsed: boolean
	onView: (view: LibraryView) => void
	onNavigate?: (route: ReaderRoute) => void
	navigationOpen: boolean
	onCloseNavigation: () => void
	capabilityLease: ReaderCapabilityLease
}

export function SitesView({ client, onToast, initialSiteId, collapsed, onView, onNavigate, navigationOpen, onCloseNavigation, capabilityLease }: SitesViewProps) {
	const policy = capabilityLease.policy
	const canRead = () => client.isIdentityCurrent() && capabilityLease.isCurrent('siteRead')
	const canWrite = () => client.isIdentityCurrent() && capabilityLease.isCurrent('siteWrite')
	const canManage = () => client.isIdentityCurrent() && capabilityLease.isCurrent('siteAdvanced')
	const [view, setView] = useState<SiteView>('all')
	const [active, setActive] = useState<SiteDetailResponse | null>(null)
	const [loadingDetail, setLoadingDetail] = useState(false)
	const [mobileDetail, setMobileDetail] = useState(false)
	const [editingProfile, setEditingProfile] = useState(false)
	const [editingEntry, setEditingEntry] = useState<SiteEntryResponse | null>(null)
	const [pendingReadingEntry, setPendingReadingEntry] = useState<SiteEntryResponse | null>(null)
	const [convertingReading, setConvertingReading] = useState(false)
	const [mergingSite, setMergingSite] = useState<SiteDetailResponse | null>(null)
	const [splittingSite, setSplittingSite] = useState<SiteDetailResponse | null>(null)
	const sites = useSites(client, capabilityLease, { view })
		useEffect(() => {
			if (!policy.siteRead) {
				setActive(null)
				setLoadingDetail(false)
				setMobileDetail(false)
			}
			if (!policy.siteWrite) {
				setEditingProfile(false)
				setEditingEntry(null)
				setPendingReadingEntry(null)
				setConvertingReading(false)
			}
			if (!policy.siteAdvanced) {
				setMergingSite(null)
				setSplittingSite(null)
			}
		}, [policy.siteAdvanced, policy.siteRead, policy.siteWrite])
		useEffect(() => { setActive(null); setMobileDetail(false) }, [view])
		const open = async (site: SiteListItemResponse) => {
			if (!canRead()) return
			if (active?.id === site.id) {
				setActive(null)
				setMobileDetail(false)
				return
			}
			setLoadingDetail(true)
		const result = await client.getSite(site.id)
		if (!canRead()) return
		setLoadingDetail(false)
		if (!result.ok) { onToast(`加载网站详情失败：${result.error.message}`, 'alert'); return }
		setActive(result.data)
		setMobileDetail(true)
	}
	useEffect(() => {
		if (!initialSiteId || !capabilityLease.isCurrent('siteRead')) return
		void (async () => {
				setLoadingDetail(true)
				const result = await client.getSite(initialSiteId)
				if (!client.isIdentityCurrent() || !capabilityLease.isCurrent('siteRead')) return
				setLoadingDetail(false)
			if (!result.ok) { onToast(`加载网站详情失败：${result.error.message}`, 'alert'); return }
			setActive(result.data)
			setMobileDetail(true)
		})()
	}, [capabilityLease, client, initialSiteId, onToast])
		const apply = async (request: ReturnType<IdentityBoundReaderClient['updateSite']>) => {
			const result = await request
			if (!canWrite()) return false
			if (!result.ok) { onToast(result.error.errorCode === 'site_revision_conflict' ? '网站已在其他窗口修改，请重新打开' : `保存失败：${result.error.message}`, 'alert'); return false }
		setActive(result.data)
		void sites.reload()
		return true
	}
	const togglePin = async () => {
		if (active && canWrite()) await apply(client.updateSite(active.id, active.revision, { pinned: !active.pinned }))
	}
	const saveProfile = async (patch: SiteUpdateRequest) => {
		if (!active || !canWrite()) return false
		const saved = await apply(client.updateSite(active.id, active.revision, patch))
		if (saved) setEditingProfile(false)
		return saved
	}
	const saveEntry = async (entry: SiteEntryResponse, patch: { name: string; purpose: string }) => {
			if (!active || !canWrite()) return false
			const result = await client.updateSiteEntry(active.id, entry.id, active.revision, patch)
			if (!canWrite()) return false
			if (!result.ok) { onToast(`保存入口失败：${result.error.message}`, 'alert'); return false }
		setActive(result.data); setEditingEntry(null); void sites.reload(); return true
	}
	const setPrimary = async (entry: SiteEntryResponse) => {
			if (!active || !canWrite() || active.primary_entry?.id === entry.id) return
			const result = await client.setPrimarySiteEntry(active.id, entry.id, active.revision)
			if (!canWrite()) return
			if (!result.ok) { onToast(`更换主入口失败：${result.error.message}`, 'alert'); return }
		setActive(result.data); void sites.reload()
	}
	const removeEntry = async (entry: SiteEntryResponse) => {
			if (!active || !canWrite() || !window.confirm(`删除入口“${entry.name || entry.url}”及其收藏记录？此操作无法恢复。`)) return
			const result = await client.deleteSiteEntry(active.id, entry.id, active.revision)
			if (!canWrite()) return
			if (!result.ok) { onToast(`删除入口失败：${result.error.message}`, 'alert'); return }
			if (result.data.deleted_site) setActive(null)
			else { const refreshed = await client.getSite(active.id); if (!canWrite()) return; if (refreshed.ok) setActive(refreshed.data) }
		void sites.reload()
	}
	const moveEntryToReading = async (entry: SiteEntryResponse) => {
			if (!active || !canWrite()) return
			const link = await client.getLink(entry.link_id)
			if (!canWrite()) return
			if (!link.ok) { onToast(`读取入口状态失败：${link.error.message}`, 'alert'); return }
		if (link.data.content_revision === undefined) { onToast('后端未返回内容版本，无法安全转换', 'alert'); return }
			const result = await client.convertLink(entry.link_id, { target_kind: 'reading', expected_content_revision: link.data.content_revision, expected_site_revision: active.revision })
			if (!canWrite()) return
			if (!result.ok) { onToast(result.error.errorCode === 'revision_conflict' ? '入口或网站已变化，请重新打开后再试' : `转换失败：${result.error.message}`, 'alert'); return }
			if (active.entry_count === 1) setActive(null)
			else { const refreshed = await client.getSite(active.id); if (!canWrite()) return; if (refreshed.ok) setActive(refreshed.data) }
		// 站点条目转回阅读库：阅读侧的列表、标签与域名聚合都变了。
		invalidateLibrary()
		// 转换（convertSiteToReadingSQL）会把这条的 content 置空，同时 content_revision
		// 加一——所以旧键**读不到了**，这一下是回收孤儿条目，不是纠错。正文缓存自成
		// 命名空间、库级失效够不着它，不显式打掉就会一直占着内存与磁盘配额。
		invalidateLink(entry.link_id)
		void sites.reload(); onToast('已移到阅读资料库', 'refresh')
	}
	const confirmMoveEntryToReading = async () => {
			if (!pendingReadingEntry || convertingReading || !canWrite()) return
		setConvertingReading(true)
		try {
			await moveEntryToReading(pendingReadingEntry)
			setPendingReadingEntry(null)
		} finally {
			setConvertingReading(false)
		}
	}
	const removeSite = async () => {
			if (!active || !canWrite() || !window.confirm(`删除网站“${active.name}”及其 ${active.entry_count} 个入口？此操作无法恢复。`)) return
			const result = await client.deleteSite(active.id, active.revision, active.entry_count)
			if (!canWrite()) return
			if (!result.ok) { onToast(`删除网站失败：${result.error.message}`, 'alert'); return }
		setActive(null); void sites.reload(); onToast('网站已删除', 'trash')
	}
		const mergeSite = async (request: Parameters<IdentityBoundReaderClient['executeSiteMerge']>[0]) => {
			if (!canManage()) return false
			const result = await client.executeSiteMerge(request)
			if (!canManage()) return false
			if (!result.ok) { onToast(result.error.errorCode === 'site_merge_conflict' ? '网站已变化，请重新预览合并' : `合并失败：${result.error.message}`, 'alert'); return false }
			const refreshed = await client.getSite(result.data.site_id)
			if (!canManage()) return false
		if (refreshed.ok) setActive(refreshed.data)
		setMergingSite(null); void sites.reload(); onToast(`已合并 ${result.data.moved_entries} 个入口`, 'check'); return true
	}
		const splitSite = async (siteID: string, request: Parameters<IdentityBoundReaderClient['executeSiteSplit']>[1]) => {
			if (!canManage()) return 'error' as const
			const result = await client.executeSiteSplit(siteID, request)
			if (!canManage()) return 'error' as const
			if (!result.ok) {
				const conflict = result.error.errorCode === 'site_merge_conflict'
				let conflictMessage = '网站已变化，输入已保留，请重新预览拆分'
				if (conflict) {
					const refreshed = await client.getSite(siteID)
					if (!canManage()) return 'error' as const
					if (refreshed.ok) {
						setActive(refreshed.data)
						setSplittingSite(refreshed.data)
					} else {
						conflictMessage += `；刷新最新详情失败：${refreshed.error.message}`
					}
				}
				onToast(conflict ? conflictMessage : `拆分失败：${result.error.message}`, 'alert')
				return conflict ? 'conflict' as const : 'error' as const
			}
			const refreshed = await client.getSite(result.data.source_site_id); if (!canManage()) return 'error' as const; if (refreshed.ok) setActive(refreshed.data)
		setSplittingSite(null); void sites.reload(); onToast(`已拆出 ${result.data.moved_entries} 个入口`, 'check'); return 'success' as const
	}
		return <section className={'sites-view' + (active ? ' has-detail' : '')}>
			<aside className={'sites-sidebar' + (collapsed ? ' collapsed' : '')} id="primary-navigation" aria-label="网站导航">
				<LibraryModeNav view="sites" onView={onView} onNavigate={onNavigate} policy={policy} />
				<div className="sites-filter-group">
					{/* 网站是阅读的一种聚合视图，主导航里高亮的是「阅读」，所以这里
					    要给一条明确的回去的路。 */}
					<button type="button" className="sites-nav sites-back-nav" onClick={() => onNavigate?.({ kind: 'library', id: 'reading' })}><Icon name="doc" size={13} /><span>返回阅读</span></button>
					<span className="sites-eyebrow">分类</span>
					{(Object.keys(labels) as SiteView[]).map((item) => <button key={item} type="button" className={'sites-nav' + (view === item ? ' active' : '')} onClick={() => setView(item)}><span>{labels[item]}</span>{item === view && <small>{sites.total}</small>}</button>)}
				</div>
			</aside>
			<button type="button" className={'sites-nav-backdrop' + (navigationOpen ? ' open' : '')} aria-label="关闭网站导航" onClick={onCloseNavigation} />
			<main className={'sites-main' + (mobileDetail ? ' mobile-hidden' : '')}>
				<header className="sites-heading"><div><span className="sites-kicker">{labels[view]} · {sites.total} 个</span><h1>网站</h1><p>按站点聚合的收藏</p></div><button className="tb-btn" type="button" title="刷新网站" aria-label="刷新网站" onClick={() => void sites.reload()}><Icon name="refresh" size={16} /></button></header>
				{sites.loading && sites.items.length === 0 ? <div className="sites-empty">正在加载网站</div> : sites.error && sites.items.length === 0 ? <div className="sites-empty">加载失败：{sites.error.message}<button type="button" onClick={() => void sites.reload()}>重试</button></div> : <><div className="site-grid">{sites.items.map((site) => <SiteCard key={site.id} site={site} active={active?.id === site.id} onOpen={open} />)}</div>{sites.error && <div className="sites-page-error" role="alert">刷新失败：{sites.error.message}<button type="button" onClick={() => void sites.reload()}>重试</button></div>}{sites.pageError && <div className="sites-page-error" role="alert">加载更多网站失败：{sites.pageError.message}<button type="button" onClick={() => void sites.loadMore()}>重试加载更多网站</button></div>}{sites.hasMore && <button type="button" className="rvx-load-more sites-load-more" onClick={() => void sites.loadMore()} disabled={sites.loadingMore}>{sites.loadingMore ? '正在加载更多网站' : '加载更多网站'}</button>}</>}
				{!sites.loading && !sites.error && sites.items.length === 0 && <div className="sites-empty">这个视图还没有网站</div>}
			</main>
			{active && <aside className={'site-detail' + (mobileDetail ? ' mobile-active' : '')}><SiteDetail site={active} loading={loadingDetail} canWrite={policy.siteWrite} canManage={policy.siteAdvanced} onBack={() => { setActive(null); setMobileDetail(false) }} onPin={togglePin} onEdit={() => { if (canWrite()) setEditingProfile(true) }} onMerge={() => { if (canManage()) setMergingSite(active) }} onSplit={() => { if (canManage()) setSplittingSite(active) }} onDelete={() => void removeSite()} onEditEntry={(entry) => { if (canWrite()) setEditingEntry(entry) }} onSetPrimary={(entry) => void setPrimary(entry)} onDeleteEntry={(entry) => void removeEntry(entry)} onMoveToReading={(entry) => { if (canWrite()) setPendingReadingEntry(entry) }} /></aside>}
		{policy.siteWrite && editingProfile && active && <SiteProfileDialog site={active} onClose={() => setEditingProfile(false)} onSave={saveProfile} />}
		{policy.siteWrite && editingEntry && active && <SiteEntryDialog entry={editingEntry} onClose={() => setEditingEntry(null)} onSave={(patch) => saveEntry(editingEntry, patch)} />}
		{/* 转换中不许关闭已经由 ReaderDialog 的 busy 一处封掉，这里不再重复判断。 */}
		{policy.siteWrite && pendingReadingEntry && <MoveEntryToReadingDialog entry={pendingReadingEntry} busy={convertingReading} onClose={() => setPendingReadingEntry(null)} onConfirm={() => void confirmMoveEntryToReading()} />}
		{policy.siteAdvanced && mergingSite && <SiteMergeDialog capabilityLease={capabilityLease} client={client} target={mergingSite} onClose={() => setMergingSite(null)} onMerge={mergeSite} onToast={onToast} />}
		{policy.siteAdvanced && splittingSite && <SiteSplitDialog capabilityLease={capabilityLease} client={client} site={splittingSite} onClose={() => setSplittingSite(null)} onSplit={splitSite} onToast={onToast} />}
	</section>
}

function SiteCard({ site, active, onOpen }: { site: SiteListItemResponse; active: boolean; onOpen: (site: SiteListItemResponse) => void }) {
	const primary = site.primary_entry?.url
	const intro = site.intro || '未提供简介'
	const visibleTags = site.tags.slice(0, 3)
		return <article className={'site-card' + (active ? ' active' : '')}>
		<button type="button" className="site-card-main" onClick={() => void onOpen(site)}>
			<span className="site-avatar">{site.display_host.slice(0, 1).toUpperCase()}</span>
			<span className="site-card-heading"><strong>{site.name}</strong><small>{site.display_host}</small></span>
			<span className="site-intro" title={site.intro || undefined}>{intro}</span>
			<span className="site-card-foot">
				<span className="site-card-tags">{visibleTags.map((tag) => <i key={tag}>#{tag}</i>)}{site.tags.length > visibleTags.length && <i>+{site.tags.length - visibleTags.length}</i>}</span>
				<em>{site.entry_count} 个入口</em>
			</span>
		</button>
		{primary && <a className="site-open" href={primary} target="_blank" rel="noreferrer" aria-label={`打开 ${site.name}`} title="在新标签页打开"><Icon name="external" size={15} /></a>}
	</article>
}

function SiteDetail({ site, loading, canWrite, canManage, onBack, onPin, onEdit, onMerge, onSplit, onDelete, onEditEntry, onSetPrimary, onDeleteEntry, onMoveToReading }: { site: SiteDetailResponse; loading: boolean; canWrite: boolean; canManage: boolean; onBack: () => void; onPin: () => void; onEdit: () => void; onMerge: () => void; onSplit: () => void; onDelete: () => void; onEditEntry: (entry: SiteEntryResponse) => void; onSetPrimary: (entry: SiteEntryResponse) => void; onDeleteEntry: (entry: SiteEntryResponse) => void; onMoveToReading: (entry: SiteEntryResponse) => void }) {
	return <div className="site-detail-inner"><header><span className="site-avatar">{site.display_host.slice(0, 1).toUpperCase()}</span><div><h2>{site.name}</h2><small>{site.display_host}</small></div>{canWrite && <button type="button" className="tb-btn" onClick={onEdit} title="编辑网站资料" aria-label="编辑网站资料"><Icon name="edit" size={15} /></button>}{canManage && <button type="button" className="tb-btn" onClick={onMerge} title="合并网站" aria-label="合并网站"><Icon name="arrowright" size={15} /></button>}{canManage && <button type="button" className="tb-btn" onClick={onSplit} title="拆分网站" aria-label="拆分网站"><Icon name="external" size={15} /></button>}{canWrite && <button type="button" className={'tb-btn' + (site.pinned ? ' active' : '')} onClick={() => void onPin()} title={site.pinned ? '取消置顶' : '置顶'} aria-label={site.pinned ? '取消置顶' : '置顶'}><Icon name="pin" size={15} /></button>}{canWrite && <button type="button" className="tb-btn danger" onClick={onDelete} title="删除网站" aria-label="删除网站"><Icon name="trash" size={15} /></button>}<button type="button" className="tb-btn site-detail-close" onClick={onBack} title="关闭网站详情" aria-label="关闭网站详情"><Icon name="close" size={15} /></button></header><p>{site.intro || '未提供简介'}</p>{site.homepage_url && <a className="site-homepage" href={site.homepage_url} target="_blank" rel="noreferrer">{site.homepage_url}</a>}<div className="site-detail-tags">{site.tags.map((tag) => <span key={tag}>#{tag}</span>)}</div><h3>入口</h3>{loading ? <div className="sites-empty">加载中</div> : <ul className="site-entry-list">{site.entries.map((entry) => <li key={entry.id}><a href={entry.url} target="_blank" rel="noreferrer">{entry.name || entry.url}</a><small>{entry.purpose || entry.url}</small>{(canWrite || site.primary_entry?.id === entry.id) && <span className="site-entry-actions">{site.primary_entry?.id === entry.id ? <b>主入口</b> : canWrite ? <button type="button" title="设为主入口" aria-label="设为主入口" onClick={() => onSetPrimary(entry)}><Icon name="pin" size={13} /></button> : null}{canWrite && <button type="button" title="移到阅读" aria-label="移到阅读" onClick={() => onMoveToReading(entry)}><Icon name="arrowright" size={13} /></button>}{canWrite && <button type="button" title="编辑入口" aria-label="编辑入口" onClick={() => onEditEntry(entry)}><Icon name="edit" size={13} /></button>}{canWrite && <button type="button" title="删除入口" aria-label="删除入口" onClick={() => onDeleteEntry(entry)}><Icon name="trash" size={13} /></button>}</span>}</li>)}</ul>}<h3>用户备注</h3><p className="site-note">{site.user_note || '暂无备注'}</p><RelatedReadings items={site.related_readings} /></div>
}

function RelatedReadings({ items }: { items: SiteDetailResponse['related_readings'] }) { const [open, setOpen] = useState(false); if (!items.length) return null; return <section className="related-readings"><button type="button" onClick={() => setOpen((current) => !current)}>{open ? '收起相关阅读' : `相关阅读 (${items.length})`}</button>{open && <ul>{items.map((item) => <li key={item.id}><a href={`?view=reading&link_id=${encodeURIComponent(item.id)}`}>{item.title}</a></li>)}</ul>}</section> }

function SiteMergeDialog({ client, capabilityLease, target, onClose, onMerge, onToast }: { client: IdentityBoundReaderClient; capabilityLease: ReaderCapabilityLease; target: SiteDetailResponse; onClose: () => void; onMerge: (request: Parameters<IdentityBoundReaderClient['executeSiteMerge']>[0]) => Promise<boolean>; onToast: (message: string, icon?: import('./Icon').IconName) => void }) {
	const candidatesPage = useSites(client, capabilityLease, { view: 'all' })
	const candidates = useMemo(() => candidatesPage.items.filter((item) => item.id !== target.id), [candidatesPage.items, target.id])
	const [selected, setSelected] = useState<string[]>([])
	const [preview, setPreview] = useState<SiteMergePreviewResponse | null>(null)
	const [choices, setChoices] = useState<Record<string, 'target' | 'source'>>({})
	const [busy, setBusy] = useState(false)
	const toggle = (id: string) => { setPreview(null); setSelected((current) => current.includes(id) ? current.filter((item) => item !== id) : [...current, id]) }
	const selectedSources = () => candidates.filter((item) => selected.includes(item.id)).map((item) => ({ site_id: item.id, revision: item.revision }))
	const loadPreview = async () => { const sources = selectedSources(); if (!sources.length || !capabilityLease.isCurrent('siteAdvanced')) return; setBusy(true); const result = await client.previewSiteMerge({ target_site_id: target.id, target_revision: target.revision, sources }); if (!client.isIdentityCurrent() || !capabilityLease.isCurrent('siteAdvanced')) return; setBusy(false); if (!result.ok) { onToast(`合并预览失败：${result.error.message}`, 'alert'); return } setPreview(result.data); setChoices(Object.fromEntries(result.data.field_conflicts.map((conflict) => [`${conflict.field}:${conflict.source_site_id}`, 'target']))) }
	const execute = async () => { if (!preview || !capabilityLease.isCurrent('siteAdvanced')) return; setBusy(true); const sources = selectedSources(); const resolutions = preview.field_conflicts.map((conflict) => ({ field: conflict.field as 'name' | 'intro' | 'homepage_url' | 'icon_url' | 'user_note', choice: choices[`${conflict.field}:${conflict.source_site_id}`] ?? 'target', source_site_id: conflict.source_site_id })); const saved = await onMerge({ target_site_id: target.id, target_revision: target.revision, sources, resolutions }); if (!client.isIdentityCurrent() || !capabilityLease.isCurrent('siteAdvanced')) return; setBusy(false); if (!saved) return }
	// busy 一开，backdrop / 关闭按钮两条路径由外壳一起封掉；Escape 迁移前就不关，保持不关。
	return <ReaderDialog title={`合并到 ${target.name}`} titleId="site-merge-title" busy={busy} dismissOnEscape={false} onClose={onClose}>{!preview ? <><p>选择要并入当前网站的来源。重复 URL 的来源收藏会被删除，其余入口、标签和站点身份会迁移。</p>{candidatesPage.loading && candidates.length === 0 ? <div className="sites-empty">正在加载可合并网站</div> : candidatesPage.error && candidates.length === 0 ? <div className="sites-page-error">加载可合并网站失败：{candidatesPage.error.message}<button type="button" onClick={() => void candidatesPage.reload()}>重试</button></div> : <><ul className="site-selection-list">{candidates.map((site) => <li key={site.id}><label><input type="checkbox" checked={selected.includes(site.id)} onChange={() => toggle(site.id)} /> <strong>{site.name}</strong><small>{site.entry_count} 个入口 · {site.display_host}</small></label></li>)}</ul>{candidatesPage.pageError && <div className="sites-page-error" role="alert">加载更多可合并网站失败：{candidatesPage.pageError.message}<button type="button" onClick={() => void candidatesPage.loadMore()}>重试加载更多可合并网站</button></div>}{candidatesPage.hasMore && <button type="button" className="rvx-load-more sites-load-more" onClick={() => void candidatesPage.loadMore()} disabled={candidatesPage.loadingMore}>{candidatesPage.loadingMore ? '正在加载更多可合并网站' : '加载更多可合并网站'}</button>}</>}</> : <><p>将迁移 {preview.entries.filter((entry) => !entry.duplicate).length} 个入口，删除 {preview.entries.filter((entry) => entry.duplicate).length} 个重复收藏。</p>{preview.field_conflicts.map((conflict) => { const key = `${conflict.field}:${conflict.source_site_id}`; return <label key={key}>{conflict.field}<select value={choices[key] ?? 'target'} onChange={(event) => setChoices((current) => ({ ...current, [key]: event.target.value as 'target' | 'source' }))}><option value="target">保留当前值：{conflict.target_value}</option><option value="source">采用来源值：{conflict.source_value}</option></select></label> })}</>}<footer><button type="button" onClick={onClose} disabled={busy}>取消</button>{preview ? <button type="button" onClick={() => void execute()} disabled={busy}>{busy ? '合并中' : '确认合并'}</button> : <button type="button" onClick={() => void loadPreview()} disabled={busy || !selected.length}>{busy ? '加载中' : '预览合并'}</button>}</footer></ReaderDialog>
}

function SiteSplitDialog({ client, capabilityLease, site, onClose, onSplit, onToast }: { client: IdentityBoundReaderClient; capabilityLease: ReaderCapabilityLease; site: SiteDetailResponse; onClose: () => void; onSplit: (siteID: string, request: SiteSplitRequest) => Promise<SiteSplitOutcome>; onToast: (message: string, icon?: import('./Icon').IconName) => void }) {
	const [selected, setSelected] = useState<string[]>([])
	const [name, setName] = useState('')
	const [intro, setIntro] = useState('')
	const [homepage, setHomepage] = useState('')
	const [icon, setIcon] = useState('')
	const [note, setNote] = useState('')
	const [primaryEntryID, setPrimaryEntryID] = useState('')
	const [identityKey, setIdentityKey] = useState('')
	const [identityOptions, setIdentityOptions] = useState<SiteSplitPreviewResponse['identities']>([])
	const [preview, setPreview] = useState<SiteSplitPreviewResponse | null>(null)
	const [hasPreviewed, setHasPreviewed] = useState(false)
	const [validationError, setValidationError] = useState('')
	const [busy, setBusy] = useState(false)
	const invalidatePreview = () => { setPreview(null); setValidationError('') }
	const toggle = (id: string) => {
		const next = selected.includes(id) ? selected.filter((item) => item !== id) : [...selected, id]
		setSelected(next)
		if (!next.includes(primaryEntryID)) setPrimaryEntryID(next[0] ?? '')
		setIdentityKey('')
		setIdentityOptions([])
		invalidatePreview()
	}
	const optional = (value: string) => value.trim() || undefined
	const validHTTPURL = (value: string) => {
		if (!value.trim()) return true
		try {
			const parsed = new URL(value.trim())
			return (parsed.protocol === 'http:' || parsed.protocol === 'https:') && !!parsed.hostname && !parsed.username && !parsed.password
		} catch {
			return false
		}
	}
	const payload = (): SiteSplitRequest | null => {
		if (!selected.length || selected.length >= site.entries.length) { setValidationError('请选择要移动的入口，并至少保留一个来源入口。'); return null }
		if (!name.trim()) { setValidationError('新网站名称不能为空。'); return null }
		if (!selected.includes(primaryEntryID)) { setValidationError('请选择新网站主入口。'); return null }
		if (name.trim().length > 256 || intro.trim().length > 1000 || note.trim().length > 10000) { setValidationError('网站资料超过允许长度。'); return null }
		if (!validHTTPURL(homepage) || !validHTTPURL(icon)) { setValidationError('主页和图标必须是有效的 HTTP(S) URL。'); return null }
		return {
			expected_revision: site.revision,
			entry_ids: selected,
			name: name.trim(),
			primary_entry_id: primaryEntryID,
			...(optional(intro) ? { intro: optional(intro) } : {}),
			...(optional(homepage) ? { homepage_url: optional(homepage) } : {}),
			...(optional(icon) ? { icon_url: optional(icon) } : {}),
			...(optional(note) ? { user_note: optional(note) } : {}),
			...(identityKey ? { identity_keys_for_new_site: [identityKey] } : {}),
		}
	}
	const makePreview = async () => {
		const request = payload()
		if (!request || !capabilityLease.isCurrent('siteAdvanced')) return
		setBusy(true)
		const result = await client.previewSiteSplit(site.id, request)
		if (!client.isIdentityCurrent() || !capabilityLease.isCurrent('siteAdvanced')) return
		setBusy(false)
		if (!result.ok) { onToast(result.error.errorCode === 'site_merge_conflict' ? '网站已变化，输入已保留，请重新打开后预览' : `拆分预览失败：${result.error.message}`, 'alert'); return }
		setPreview(result.data)
		setHasPreviewed(true)
		setIdentityOptions(result.data.identities)
		setValidationError('')
	}
	const execute = async () => {
		if (!preview || !capabilityLease.isCurrent('siteAdvanced')) return
		setBusy(true)
		const outcome = await onSplit(site.id, preview.payload)
		if (!client.isIdentityCurrent() || !capabilityLease.isCurrent('siteAdvanced')) return
		setBusy(false)
		if (outcome === 'conflict') setPreview(null)
	}
	const eligibleIdentities = identityOptions.filter((identity) => identity.eligible_for_new_site)
	// 外壳的 panel 是 div，原来当 panel 用的 <form> 下沉成 children 的第一层。
	return <ReaderDialog title="拆分网站" titleId="site-split-title" busy={busy} dismissOnEscape={false} onClose={onClose}><form onSubmit={(event) => { event.preventDefault(); void makePreview() }}><p>选择要移动的入口，并为新网站单独填写资料。</p><ul className="site-selection-list">{site.entries.map((entry) => <li key={entry.id}><label><input type="checkbox" checked={selected.includes(entry.id)} onChange={() => toggle(entry.id)} /> <strong>{entry.name || entry.url}</strong><small>{entry.url}</small></label></li>)}</ul><label>新网站名称<input value={name} onChange={(event) => { setName(event.target.value); invalidatePreview() }} required maxLength={256} /></label><label>简介<textarea value={intro} onChange={(event) => { setIntro(event.target.value); invalidatePreview() }} maxLength={1000} /></label><label>主页<input type="url" value={homepage} onChange={(event) => { setHomepage(event.target.value); invalidatePreview() }} maxLength={2048} /></label><label>图标<input type="url" value={icon} onChange={(event) => { setIcon(event.target.value); invalidatePreview() }} maxLength={2048} /></label><label>用户备注<textarea value={note} onChange={(event) => { setNote(event.target.value); invalidatePreview() }} maxLength={10000} /></label><label>新网站主入口<select value={primaryEntryID} onChange={(event) => { setPrimaryEntryID(event.target.value); invalidatePreview() }} required><option value="">请选择</option>{site.entries.filter((entry) => selected.includes(entry.id)).map((entry) => <option key={entry.id} value={entry.id}>{entry.name || entry.url}</option>)}</select></label>{identityOptions.length > 0 && <label>站点身份<select value={identityKey} onChange={(event) => { setIdentityKey(event.target.value); invalidatePreview() }}><option value="">全部留在原网站</option>{eligibleIdentities.map((identity) => <option key={identity.identity_key} value={identity.identity_key}>转移 {identity.identity_key}</option>)}</select></label>}{identityOptions.length > 0 && eligibleIdentities.length === 0 && <p>被移动入口没有可转移的站点身份。</p>}{validationError && <p role="alert">{validationError}</p>}{preview && <section aria-label="拆分预览"><h3>最终拆分</h3><p>新网站：{preview.payload.name}</p><p>简介：{preview.payload.intro || '空'}</p><p>主页：{preview.payload.homepage_url || '空'}</p><p>图标：{preview.payload.icon_url || '空'}</p><p>用户备注：{preview.payload.user_note || '空'}</p><p>移动入口：{preview.entries.map((entry) => entry.name || entry.url).join('、')}</p><ul>{preview.identities.map((identity) => <li key={identity.identity_key}>{identity.identity_key}：{identity.owner === 'new_site' ? '新网站' : '原网站'}</li>)}</ul></section>}<footer><button type="button" onClick={onClose} disabled={busy}>取消</button>{preview ? <button type="button" onClick={() => void execute()} disabled={busy}>{busy ? '拆分中' : '确认拆分'}</button> : <button type="submit" disabled={busy}>{busy ? '加载中' : hasPreviewed ? '更新预览' : '预览拆分'}</button>}</footer></form></ReaderDialog>
}

function SiteProfileDialog({ site, onClose, onSave }: { site: SiteDetailResponse; onClose: () => void; onSave: (patch: SiteUpdateRequest) => Promise<boolean> }) { const [name, setName] = useState(site.name); const [intro, setIntro] = useState(site.intro); const [homepage, setHomepage] = useState(site.homepage_url ?? ''); const [note, setNote] = useState(site.user_note); const [tags, setTags] = useState(site.tags.join(', ')); const [saving, setSaving] = useState(false); const nameRef = useRef<HTMLInputElement>(null); const submit = async (event: FormEvent) => { event.preventDefault(); const nextTags = tags.split(',').map((tag) => tag.trim()).filter(Boolean); const existing = new Map(site.tags.map((tag) => [tag.toLocaleLowerCase(), tag])); const next = new Map(nextTags.map((tag) => [tag.toLocaleLowerCase(), tag])); const add = [...next].filter(([key]) => !existing.has(key)).map(([, tag]) => tag); const remove = [...existing].filter(([key]) => !next.has(key)).map(([, tag]) => tag); const patch: SiteUpdateRequest = { name, intro, user_note: note }; if (homepage.trim()) patch.homepage_url = homepage.trim(); if (add.length || remove.length) patch.tags = { add, remove }; setSaving(true); const saved = await onSave(patch); setSaving(false); if (saved) onClose() }; /* 迁移前 backdrop 没有 onMouseDown、也没有 Escape，两条路径都保持关不掉；焦点显式落在名称输入框，而不是外壳默认的关闭按钮。 */ return <ReaderDialog title="编辑网站资料" titleId="site-profile-title" dismissOnBackdrop={false} dismissOnEscape={false} initialFocusRef={nameRef} onClose={onClose}><form onSubmit={(event) => void submit(event)}><label>名称<input ref={nameRef} value={name} onChange={(event) => setName(event.target.value)} required maxLength={256} /></label><label>简介<textarea value={intro} onChange={(event) => setIntro(event.target.value)} maxLength={1000} /></label><label>主页<input type="url" value={homepage} onChange={(event) => setHomepage(event.target.value)} /></label><label>标签<input value={tags} onChange={(event) => setTags(event.target.value)} /></label><label>用户备注<textarea value={note} onChange={(event) => setNote(event.target.value)} maxLength={10000} /></label><footer><button type="button" onClick={onClose}>取消</button><button type="submit" disabled={saving}>{saving ? '保存中' : '保存'}</button></footer></form></ReaderDialog> }

function SiteEntryDialog({ entry, onClose, onSave }: { entry: SiteEntryResponse; onClose: () => void; onSave: (patch: { name: string; purpose: string }) => Promise<boolean> }) { const [name, setName] = useState(entry.name); const [purpose, setPurpose] = useState(entry.purpose); const [saving, setSaving] = useState(false); const nameRef = useRef<HTMLInputElement>(null); const submit = async (event: FormEvent) => { event.preventDefault(); setSaving(true); const saved = await onSave({ name, purpose }); setSaving(false); if (saved) onClose() }; /* 关闭规则同「编辑网站资料」：backdrop 与 Escape 迁移前都关不掉，焦点显式给名称输入框。 */ return <ReaderDialog title="编辑入口" titleId="site-entry-title" size="compact" dismissOnBackdrop={false} dismissOnEscape={false} initialFocusRef={nameRef} onClose={onClose}><form onSubmit={(event) => void submit(event)}><label>名称<input ref={nameRef} value={name} onChange={(event) => setName(event.target.value)} required maxLength={256} /></label><label>用途<textarea value={purpose} onChange={(event) => setPurpose(event.target.value)} maxLength={1000} /></label><footer><button type="button" onClick={onClose}>取消</button><button type="submit" disabled={saving}>{saving ? '保存中' : '保存'}</button></footer></form></ReaderDialog> }

function MoveEntryToReadingDialog({ entry, busy, onClose, onConfirm }: { entry: SiteEntryResponse; busy: boolean; onClose: () => void; onConfirm: () => void }) {
	// 焦点循环、Escape、backdrop 和 busy 封锁都归外壳；这里只保留「打开时焦点落在取消」这条本地规则。
	const cancelRef = useRef<HTMLButtonElement>(null)
	return <ReaderDialog title="移到阅读" titleId="move-entry-to-reading-title" size="compact" busy={busy} initialFocusRef={cancelRef} onClose={onClose}><p>入口“{entry.name || entry.url}”将离开网站库，并重新抓取和解析为阅读内容。</p><footer><button ref={cancelRef} type="button" onClick={onClose} disabled={busy}>取消</button><button type="button" onClick={onConfirm} disabled={busy}>{busy ? '转换中' : '确认转换'}</button></footer></ReaderDialog>
}
