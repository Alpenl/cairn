import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type SetStateAction } from 'react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import type {
  ReaderCategoryResponse,
  ReaderInboxBulkResponse,
  ReaderInboxConfirmAIProposalsResponse,
  ReaderInboxJobResponse,
  ReaderInboxPartition,
  ReaderInboxResponse,
} from '../../lib/api/types'
import type { ReaderRoute } from '../../lib/navigation/route'
import { Icon } from '../Icon'
import { SurfaceError, SurfaceLoading, SurfaceShell, formatRelativeDate, errorMessage } from './SurfaceShell'
import { refreshPendingInboxCount } from './PendingInboxCount'

export interface InboxSurfaceProps {
  readonly client: IdentityBoundReaderClient
  readonly onNavigate: (route: ReaderRoute) => void
  readonly onOpenLink: (id: string) => void
  readonly capabilityPolicy: ReaderCapabilityPolicy
  readonly initialInboxID?: string
  readonly onDraftStateChange?: (state: InboxDraftLeaveState) => void
}

export interface InboxDraftLeaveState {
  readonly dirty: boolean
  readonly saving: boolean
}

function parseTags(value: string): string[] {
  return [...new Set(value.split(/[\s,，]+/).map((tag) => tag.trim()).filter(Boolean))]
}

function statusLabel(status: ReaderInboxResponse['status']): string {
  if (status === 'pending') return '待处理'
  if (status === 'discarded') return '已丢弃'
  return '已入库'
}

function otherInboxPartition(partition: ReaderInboxPartition): ReaderInboxPartition {
  return partition === 'active' ? 'expired' : 'active'
}

function preferNewerInbox(
  current: ReaderInboxResponse | undefined,
  incoming: ReaderInboxResponse,
): ReaderInboxResponse {
  return current && incoming.metadata_revision < current.metadata_revision ? current : incoming
}

function upsertInboxItem(
  current: ReaderInboxResponse[],
  incoming: ReaderInboxResponse,
  placement: 'append' | 'prepend' | 'ignore' = 'ignore',
): ReaderInboxResponse[] {
  const index = current.findIndex((item) => item.id === incoming.id)
  if (index < 0) {
    if (placement === 'prepend') return [incoming, ...current]
    return placement === 'append' ? [...current, incoming] : current
  }
  const next = preferNewerInbox(current[index], incoming)
  return next === current[index]
    ? current
    : [...current.slice(0, index), next, ...current.slice(index + 1)]
}

function mergeInboxItems(
  current: ReaderInboxResponse[],
  incoming: ReaderInboxResponse[],
  append: boolean,
  preserveIDs?: ReadonlySet<string>,
): ReaderInboxResponse[] {
  if (append) return incoming.reduce((items, item) => upsertInboxItem(items, item, 'append'), current)

  const currentByID = new Map(current.map((item) => [item.id, item]))
  const incomingByID = new Map<string, ReaderInboxResponse>()
  const incomingIDs: string[] = []
  for (const item of incoming) {
    const previous = incomingByID.get(item.id)
    if (!previous) incomingIDs.push(item.id)
    incomingByID.set(item.id, preferNewerInbox(previous, item))
  }
  return [
    ...incomingIDs.map((id) => preferNewerInbox(currentByID.get(id), incomingByID.get(id)!)),
    ...current.filter((item) =>
      (item.status === 'discarded' || preserveIDs?.has(item.id)) && !incomingByID.has(item.id)),
  ]
}

function updateInboxStatus(
  items: ReaderInboxResponse[],
  ids: ReadonlySet<string>,
  status: ReaderInboxResponse['status'],
): ReaderInboxResponse[] {
  return items.map((item) => ids.has(item.id) ? { ...item, status } : item)
}

interface InboxBulkResult {
  readonly action: 'confirm' | 'discard' | 'confirm-ai'
  readonly response: ReaderInboxBulkResponse | ReaderInboxConfirmAIProposalsResponse
}

interface InboxPartitionPage {
  readonly items: ReaderInboxResponse[]
  readonly nextCursor?: string
  readonly loaded: boolean
  readonly loading: boolean
  readonly loadingMore: boolean
  readonly error: string | null
}

interface InboxCounts {
  readonly activeCount: number
  readonly expiredCount: number
}

function emptyInboxPartitionPage(): InboxPartitionPage {
  return {
    items: [],
    nextCursor: undefined,
    loaded: false,
    loading: false,
    loadingMore: false,
    error: null,
  }
}

interface InboxDraftFields {
  readonly title: string
  readonly body: string
  readonly note: string
  readonly summary: string
  readonly tags: string
}

interface InboxDraftBaseline extends InboxDraftFields {
  readonly revision: number
}

interface InboxDraft extends InboxDraftFields {
  readonly revision: number
  readonly baseline: InboxDraftBaseline
  readonly conflict: boolean
}

interface InboxActionOwner {
  readonly client: IdentityBoundReaderClient
  readonly initialInboxID: string | undefined
  readonly scopeGeneration: number
  readonly surfaceOwnerGeneration: number
}

interface InboxTargetLoadOwner extends InboxActionOwner {
  readonly inboxID: string
  readonly requestGeneration: number
}

function draftFieldsFromInbox(item: ReaderInboxResponse): InboxDraftFields {
  return {
    title: item.title ?? '',
    body: item.body,
    note: item.note,
    summary: item.summary ?? '',
    tags: item.tags.join(', '),
  }
}

function draftFromInbox(item: ReaderInboxResponse): InboxDraft {
  const baseline: InboxDraftBaseline = {
    ...draftFieldsFromInbox(item),
    revision: item.metadata_revision,
  }
  return {
    ...baseline,
    revision: item.metadata_revision,
    baseline,
    conflict: false,
  }
}

function draftFieldsDiffer(left: InboxDraftFields, right: InboxDraftFields): boolean {
  return left.title !== right.title ||
    left.body !== right.body ||
    left.note !== right.note ||
    left.summary !== right.summary ||
    left.tags !== right.tags
}

function draftHasLocalChanges(draft: InboxDraft): boolean {
  return draftFieldsDiffer(draft, draft.baseline)
}

function mergeDraftAfterServerRefresh(
  refreshed: ReaderInboxResponse,
  current?: InboxDraft,
  conflict = false,
): InboxDraft {
  if (current && refreshed.metadata_revision < current.baseline.revision) return current
  const remote = draftFromInbox(refreshed)
  if (!current) return remote
  const baseline = current.baseline
  return {
    title: current.title !== baseline.title ? current.title : remote.title,
    body: current.body !== baseline.body ? current.body : remote.body,
    note: current.note !== baseline.note ? current.note : remote.note,
    summary: current.summary !== baseline.summary ? current.summary : remote.summary,
    tags: current.tags !== baseline.tags ? current.tags : remote.tags,
    revision: remote.revision,
    baseline: remote.baseline,
    conflict: conflict || current.conflict,
  }
}

function mergeDraftsAfterServerRefresh(
  current: Record<string, InboxDraft>,
  refreshedItems: ReaderInboxResponse[],
): Record<string, InboxDraft> {
  let next = current
  for (const item of refreshedItems) {
    const draft = current[item.id]
    if (!draft) continue
    if (next === current) next = { ...current }
    next[item.id] = mergeDraftAfterServerRefresh(item, draft)
  }
  return next
}

function mergeDraftAfterConflict(
  baseline: ReaderInboxResponse,
  refreshed: ReaderInboxResponse,
  current?: InboxDraft,
): InboxDraft {
  return mergeDraftAfterServerRefresh(refreshed, current ?? draftFromInbox(baseline), true)
}

export function InboxSurface({ client, onNavigate, onOpenLink, capabilityPolicy, initialInboxID, onDraftStateChange }: InboxSurfaceProps) {
  const [partition, setPartition] = useState<ReaderInboxPartition>('active')
  const [inboxPages, setInboxPages] = useState<Record<ReaderInboxPartition, InboxPartitionPage>>(() => ({
    active: emptyInboxPartitionPage(),
    expired: emptyInboxPartitionPage(),
  }))
  const [inboxCounts, setInboxCounts] = useState<InboxCounts>({ activeCount: 0, expiredCount: 0 })
  const [categories, setCategories] = useState<ReaderCategoryResponse[]>([])
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const [selectedIDs, setSelectedIDs] = useState<Set<string>>(new Set())
  const [drafts, setDrafts] = useState<Record<string, InboxDraft>>({})
  const [createTitle, setCreateTitle] = useState('')
  const [createNote, setCreateNote] = useState('')
  const [createTags, setCreateTags] = useState('')
  const [url, setUrl] = useState('')
  const [createBody, setCreateBody] = useState('')
  const [previewBody, setPreviewBody] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  const [newCategory, setNewCategory] = useState('')
  const [membershipsByInbox, setMembershipsByInbox] = useState<Record<string, Set<string>>>({})
  const [loadingTarget, setLoadingTarget] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [job, setJob] = useState<ReaderInboxJobResponse | null>(null)
  const [bulkResult, setBulkResult] = useState<InboxBulkResult | null>(null)
  const categoryRequestGenerationsRef = useRef(new Map<string, number>())
  const loadScopeGenerationRef = useRef(0)
  const loadRequestGenerationRef = useRef<Record<ReaderInboxPartition, number>>({ active: 0, expired: 0 })
  const aggregateRequestGenerationRef = useRef(0)
  const aggregateAppliedGenerationRef = useRef(0)
  const listItemRequestGenerationRef = useRef(0)
  const listItemAppliedGenerationRef = useRef(new Map<string, number>())
  const inboxLifecycleGenerationRef = useRef(0)
  // CAS rereads may run in bulk, so each Inbox item needs an independent token.
  const inboxRefreshGenerationsRef = useRef(new Map<string, number>())
  const inboxOperationGenerationRef = useRef(0)
  const inboxOwnerClientRef = useRef(client)
  const inboxOwnerTargetRef = useRef(initialInboxID)
  const inboxSurfaceOwnerGenerationRef = useRef(0)
  const inboxMountedRef = useRef(false)
  const inboxSavingGenerationRef = useRef(0)
  const inboxTargetLoadGenerationRef = useRef(0)
  const inboxDataClientRef = useRef(client)

  const partitionRef = useRef(partition)
  const page = inboxPages[partition]
  const items = page.items
  const nextCursor = page.nextCursor
  const loading = page.loading || !page.loaded
  const loadingMore = page.loadingMore
  const inboxError = page.error

  const updateInboxPage = useCallback((
    target: ReaderInboxPartition,
    update: (current: InboxPartitionPage) => InboxPartitionPage,
  ) => {
    setInboxPages((current) => ({ ...current, [target]: update(current[target]) }))
  }, [])

  const updatePartitionAndEvictOther = useCallback((
    target: ReaderInboxPartition,
    ids: ReadonlySet<string>,
    update: (current: InboxPartitionPage) => InboxPartitionPage,
  ) => {
    const other = otherInboxPartition(target)
    setInboxPages((current) => ({
      ...current,
      [target]: update(current[target]),
      [other]: ids.size === 0
        ? current[other]
        : { ...current[other], items: current[other].items.filter((item) => !ids.has(item.id)) },
    }))
  }, [])

  const adjustInboxCounts = useCallback((activeDelta: number, expiredDelta: number) => {
    setInboxCounts((current) => ({
      activeCount: Math.max(0, current.activeCount + activeDelta),
      expiredCount: Math.max(0, current.expiredCount + expiredDelta),
    }))
  }, [])

  const upsertAuthoritativeInbox = useCallback((
    incoming: ReaderInboxResponse,
    placement: 'append' | 'prepend' | 'ignore' = 'ignore',
  ) => {
    const target: ReaderInboxPartition = incoming.expired ? 'expired' : 'active'
    const ids = new Set([incoming.id])
    updatePartitionAndEvictOther(target, ids, (current) => ({
      ...current,
      items: upsertInboxItem(current.items, incoming, placement),
    }))
    return target
  }, [updatePartitionAndEvictOther])

  const setItems = useCallback((update: SetStateAction<ReaderInboxResponse[]>) => {
    updateInboxPage(partition, (current) => ({
      ...current,
      items: typeof update === 'function'
        ? update(current.items)
        : update,
    }))
  }, [partition, updateInboxPage])

  const selected = useMemo(() => items.find((item) => item.id === selectedID) ?? items[0] ?? null, [items, selectedID])
  const selectedDraft = selected ? drafts[selected.id] ?? draftFromInbox(selected) : null
  const title = selectedDraft?.title ?? ''
  const note = selectedDraft?.note ?? ''
  const summary = selectedDraft?.summary ?? ''
  const tags = selectedDraft?.tags ?? ''
  const body = selectedDraft?.body ?? ''
  const memberships = useMemo(
    () => selected ? membershipsByInbox[selected.id] ?? new Set(selected.category_ids) : new Set<string>(),
    [membershipsByInbox, selected],
  )
  const pendingItems = useMemo(() => items.filter((item) => item.status === 'pending'), [items])
  const discardedCount = items.filter((item) => item.status === 'discarded').length
  const selectedPendingItems = useMemo(
    () => pendingItems.filter((item) => selectedIDs.has(item.id)),
    [pendingItems, selectedIDs],
  )
  const selectedPendingCount = selectedPendingItems.length
  const selectedPendingHasEmptyTitle = selectedPendingItems.some((item) =>
    !(drafts[item.id]?.title ?? item.title ?? '').trim())
  const allPendingSelected = pendingItems.length > 0 && selectedPendingCount === pendingItems.length
  const displayError = error ?? inboxError

  useEffect(() => {
    inboxOperationGenerationRef.current += 1
  }, [selected?.id])

  useLayoutEffect(() => {
    inboxMountedRef.current = true
    return () => {
      inboxMountedRef.current = false
    }
  }, [])

  useLayoutEffect(() => {
    inboxOwnerClientRef.current = client
    inboxOwnerTargetRef.current = initialInboxID
    inboxSurfaceOwnerGenerationRef.current += 1
    inboxOperationGenerationRef.current += 1
    inboxSavingGenerationRef.current += 1
    categoryRequestGenerationsRef.current.clear()
    inboxRefreshGenerationsRef.current.clear()
    setSaving(false)
  }, [client, initialInboxID])

  const captureActionOwner = useCallback((): InboxActionOwner | null => {
    if (!client.isIdentityCurrent()) return null
    return {
      client,
      initialInboxID,
      scopeGeneration: loadScopeGenerationRef.current,
      surfaceOwnerGeneration: inboxSurfaceOwnerGenerationRef.current,
    }
  }, [client, initialInboxID])

  const isCurrentActionOwner = useCallback((owner: InboxActionOwner): boolean =>
    owner.client === inboxOwnerClientRef.current &&
    owner.initialInboxID === inboxOwnerTargetRef.current &&
    owner.scopeGeneration === loadScopeGenerationRef.current &&
    owner.surfaceOwnerGeneration === inboxSurfaceOwnerGenerationRef.current &&
    owner.client.isIdentityCurrent(), [])

  // This deliberately excludes identity-currentness. It is only for releasing
  // state owned by the same mounted surface after an identity-revoked result;
  // response writes still require isCurrentActionOwner above.
  const isSameMountedActionOwner = useCallback((owner: InboxActionOwner): boolean =>
    inboxMountedRef.current &&
    owner.client === inboxOwnerClientRef.current &&
    owner.initialInboxID === inboxOwnerTargetRef.current &&
    owner.scopeGeneration === loadScopeGenerationRef.current &&
    owner.surfaceOwnerGeneration === inboxSurfaceOwnerGenerationRef.current, [])

  const isCurrentTargetLoadOwner = useCallback((owner: InboxTargetLoadOwner): boolean =>
    isSameMountedActionOwner(owner) &&
    owner.requestGeneration === inboxTargetLoadGenerationRef.current, [isSameMountedActionOwner])

  const beginSavingForOwner = useCallback((): number => {
    const generation = ++inboxSavingGenerationRef.current
    setSaving(true)
    return generation
  }, [])

  const finishSavingForOwner = useCallback((owner: InboxActionOwner, generation: number) => {
    if (
      isSameMountedActionOwner(owner) &&
      inboxSavingGenerationRef.current === generation
    ) {
      setSaving(false)
    }
  }, [isSameMountedActionOwner])

  const advanceInboxLifecycle = useCallback((inboxIDs: readonly string[] = []) => {
    inboxLifecycleGenerationRef.current += 1
    for (const inboxID of inboxIDs) {
      const current = inboxRefreshGenerationsRef.current.get(inboxID)
      if (current !== undefined) inboxRefreshGenerationsRef.current.set(inboxID, current + 1)
    }
  }, [])

  const beginInboxRefresh = useCallback((inboxID: string): number => {
    const next = (inboxRefreshGenerationsRef.current.get(inboxID) ?? 0) + 1
    inboxRefreshGenerationsRef.current.set(inboxID, next)
    return next
  }, [])

  const load = useCallback(async (
    target: ReaderInboxPartition = partitionRef.current,
    append = false,
    cursor?: string,
    scopeGeneration = loadScopeGenerationRef.current,
  ) => {
    const requestGeneration = ++loadRequestGenerationRef.current[target]
    const aggregateRequestGeneration = ++aggregateRequestGenerationRef.current
    const itemRequestGeneration = ++listItemRequestGenerationRef.current
    const lifecycleGeneration = inboxLifecycleGenerationRef.current
    const refreshGenerations = new Map(inboxRefreshGenerationsRef.current)
    updateInboxPage(target, (current) => ({
      ...current,
      loading: !append,
      loadingMore: append,
      error: null,
    }))
    setError(null)
    try {
      const [inboxResult, categoryResult] = await Promise.all([
        client.listInbox({ partition: target, after: cursor, limit: 50 }),
        append ? Promise.resolve(null) : client.listCategories(),
      ])
      if (
        !client.isIdentityCurrent() ||
        loadScopeGenerationRef.current !== scopeGeneration ||
        loadRequestGenerationRef.current[target] !== requestGeneration ||
        inboxLifecycleGenerationRef.current !== lifecycleGeneration
      ) return
      if (!inboxResult.ok) {
        updateInboxPage(target, (current) => ({
          ...current,
          loaded: true,
          loading: false,
          loadingMore: false,
          error: errorMessage(inboxResult.error),
        }))
      }
      else {
        if (aggregateRequestGeneration > aggregateAppliedGenerationRef.current) {
          aggregateAppliedGenerationRef.current = aggregateRequestGeneration
          setInboxCounts({
            activeCount: inboxResult.data.active_count,
            expiredCount: inboxResult.data.expired_count,
          })
        }
        const skippedIDs = new Set(inboxResult.data.items
          .filter((item) =>
            inboxRefreshGenerationsRef.current.get(item.id) !== refreshGenerations.get(item.id) ||
            (listItemAppliedGenerationRef.current.get(item.id) ?? 0) > itemRequestGeneration)
          .map((item) => item.id))
        const mergedItems = inboxResult.data.items.filter((item) => !skippedIDs.has(item.id))
        for (const item of mergedItems) {
          listItemAppliedGenerationRef.current.set(item.id, itemRequestGeneration)
        }
        const mergedIDs = new Set(mergedItems.map((item) => item.id))
        updatePartitionAndEvictOther(target, mergedIDs, (current) => ({
          ...current,
          items: mergeInboxItems(current.items, mergedItems, append, skippedIDs),
          nextCursor: inboxResult.data.next_cursor,
          loaded: true,
          loading: false,
          loadingMore: false,
          error: null,
        }))
        setDrafts((current) => mergeDraftsAfterServerRefresh(current, mergedItems))
        const requested = initialInboxID ? mergedItems.find((item) => item.id === initialInboxID) : undefined
        if (partitionRef.current === target) {
          setSelectedID((current) => requested?.id ?? current ?? mergedItems[0]?.id ?? null)
        }
      }
      if (categoryResult && !categoryResult.ok) setError(errorMessage(categoryResult.error))
      else if (categoryResult?.ok) setCategories(categoryResult.data.items)
    } finally {
      if (
        loadScopeGenerationRef.current === scopeGeneration &&
        loadRequestGenerationRef.current[target] === requestGeneration
      ) {
        updateInboxPage(target, (current) => ({ ...current, loading: false, loadingMore: false }))
      }
    }
  }, [client, initialInboxID, updateInboxPage, updatePartitionAndEvictOther])

  useLayoutEffect(() => {
    const clientChanged = inboxDataClientRef.current !== client
    inboxDataClientRef.current = client
    const scopeGeneration = ++loadScopeGenerationRef.current
    loadRequestGenerationRef.current = { active: 0, expired: 0 }
    aggregateRequestGenerationRef.current = 0
    aggregateAppliedGenerationRef.current = 0
    listItemRequestGenerationRef.current = 0
    listItemAppliedGenerationRef.current.clear()
    if (clientChanged) {
      partitionRef.current = 'active'
      setPartition('active')
      setInboxPages({ active: emptyInboxPartitionPage(), expired: emptyInboxPartitionPage() })
      setInboxCounts({ activeCount: 0, expiredCount: 0 })
      setCategories([])
      setSelectedID(null)
      setSelectedIDs(new Set())
      setMembershipsByInbox({})
      setJob(null)
      setError(null)
      setBulkResult(null)
    } else {
      const target = partitionRef.current
      setInboxPages((current) => ({
        ...current,
        [target]: {
          ...current[target],
          loaded: false,
          loading: false,
          loadingMore: false,
          error: null,
        },
      }))
    }
    return () => {
      if (loadScopeGenerationRef.current === scopeGeneration) {
        loadScopeGenerationRef.current += 1
        loadRequestGenerationRef.current = { active: 0, expired: 0 }
      }
    }
  }, [client, initialInboxID])

  useEffect(() => {
    partitionRef.current = partition
  }, [partition])

  useEffect(() => {
    if (!page.loaded && !page.loading) void load(partition)
  }, [load, page.loaded, page.loading, partition])

  useEffect(() => {
    if (!initialInboxID || loading || items.some((item) => item.id === initialInboxID)) {
      setLoadingTarget(false)
      return
    }
    let active = true
    const owner: InboxTargetLoadOwner = {
      client,
      initialInboxID,
      scopeGeneration: loadScopeGenerationRef.current,
      surfaceOwnerGeneration: inboxSurfaceOwnerGenerationRef.current,
      inboxID: initialInboxID,
      requestGeneration: ++inboxTargetLoadGenerationRef.current,
    }
    const ownsTargetLoad = () => active && isCurrentTargetLoadOwner(owner)
    setLoadingTarget(true)
    void client.getInbox(initialInboxID).then((result) => {
      if (!ownsTargetLoad() || !client.isIdentityCurrent()) return
      if (!result.ok) {
        setError(errorMessage(result.error))
        return
      }
      const target: ReaderInboxPartition = result.data.expired ? 'expired' : 'active'
      const ids = new Set([result.data.id])
      advanceInboxLifecycle([result.data.id])
      updatePartitionAndEvictOther(target, ids, (current) => ({
        ...current,
        items: upsertInboxItem(current.items, result.data, 'prepend'),
      }))
      setDrafts((current) => mergeDraftsAfterServerRefresh(current, [result.data]))
      setPartition(target)
      setSelectedID(result.data.id)
    }).finally(() => {
      if (ownsTargetLoad()) setLoadingTarget(false)
    })
    return () => { active = false }
  }, [advanceInboxLifecycle, client, initialInboxID, isCurrentTargetLoadOwner, items, loading, updatePartitionAndEvictOther])

  useEffect(() => {
    const pendingIDs = new Set(pendingItems.map((item) => item.id))
    setSelectedIDs((current) => {
      const next = new Set([...current].filter((id) => pendingIDs.has(id)))
      return next.size === current.size ? current : next
    })
  }, [pendingItems])

  useEffect(() => {
    if (!selected) return
    setDrafts((current) => {
      const draft = current[selected.id]
      return {
        ...current,
        [selected.id]: mergeDraftAfterServerRefresh(selected, draft),
      }
    })
    setMembershipsByInbox((current) => current[selected.id]
      ? current
      : { ...current, [selected.id]: new Set(selected.category_ids) })
    setPreviewBody(false)
  }, [selected])

  const dirty = Boolean(selectedDraft && draftHasLocalChanges(selectedDraft))

  const updateSelectedDraft = useCallback((update: Partial<Pick<InboxDraft, 'title' | 'body' | 'note' | 'summary' | 'tags'>>) => {
    if (!selected) return
    setDrafts((current) => ({
      ...current,
      [selected.id]: { ...(current[selected.id] ?? draftFromInbox(selected)), ...update, conflict: false },
    }))
  }, [selected])

  useLayoutEffect(() => {
    onDraftStateChange?.({ dirty, saving })
  }, [dirty, onDraftStateChange, saving])

  useLayoutEffect(() => () => {
    onDraftStateChange?.({ dirty: false, saving: false })
  }, [onDraftStateChange])

  useEffect(() => {
    const jobID = selected?.job_id
    if (!jobID) {
      setJob(null)
      return
    }
    let active = true
    let timer: number | null = null
    const poll = async () => {
      const result = await client.getInboxJob(jobID)
      if (!active || !client.isIdentityCurrent()) return
      if (!result.ok) {
        setError(errorMessage(result.error))
        return
      }
      setJob(result.data)
      if (result.data.status === 'completed') {
        const lifecycleGeneration = inboxLifecycleGenerationRef.current
        const refreshed = await client.getInbox(selected?.id ?? '')
        if (
          !active ||
          !client.isIdentityCurrent() ||
          inboxLifecycleGenerationRef.current !== lifecycleGeneration
        ) return
        if (refreshed.ok) {
          advanceInboxLifecycle([refreshed.data.id])
          upsertAuthoritativeInbox(refreshed.data)
          setDrafts((current) => mergeDraftsAfterServerRefresh(current, [refreshed.data]))
        }
        else setError(errorMessage(refreshed.error))
        return
      }
      if (result.data.status === 'failed') {
        setError(result.data.error || '摘要任务失败，请稍后重试。')
        return
      }
      timer = window.setTimeout(() => { void poll() }, 1500)
    }
    void poll()
    return () => {
      active = false
      if (timer !== null) window.clearTimeout(timer)
    }
  }, [advanceInboxLifecycle, client, selected?.id, selected?.job_id, upsertAuthoritativeInbox])

  const refreshConflictRevision = useCallback(async (
    item: ReaderInboxResponse,
    owner: InboxActionOwner,
  ): Promise<ReaderInboxResponse | null> => {
    if (!isCurrentActionOwner(owner)) return null
    const requestGeneration = beginInboxRefresh(item.id)
    const refreshed = await client.getInbox(item.id)
    if (
      !isCurrentActionOwner(owner) ||
      inboxRefreshGenerationsRef.current.get(item.id) !== requestGeneration ||
      !refreshed.ok
    ) return null
    advanceInboxLifecycle([item.id])
    upsertAuthoritativeInbox(refreshed.data)
    setDrafts((current) => ({
      ...current,
      [item.id]: mergeDraftAfterConflict(item, refreshed.data, current[item.id]),
    }))
    setMembershipsByInbox((current) => ({
      ...current,
      [item.id]: current[item.id] ?? new Set(refreshed.data.category_ids),
    }))
    return refreshed.data
  }, [advanceInboxLifecycle, beginInboxRefresh, client, isCurrentActionOwner, upsertAuthoritativeInbox])

  const saveMetadata = useCallback(async (owner = captureActionOwner()): Promise<ReaderInboxResponse | null> => {
    if (!owner) return null
    const operationGeneration = inboxOperationGenerationRef.current
    const isCurrentSave = () =>
      isCurrentActionOwner(owner) &&
      inboxOperationGenerationRef.current === operationGeneration
    if (!selected || selected.status === 'confirmed') return selected
    const submittedDraft = selectedDraft ?? draftFromInbox(selected)
    const savingGeneration = beginSavingForOwner()
    setError(null)
    try {
      const result = await client.patchInbox(selected.id, submittedDraft.revision, {
        title: title.trim(),
        body,
        note,
        summary: summary.trim(),
        tags: parseTags(tags),
      })
      if (!isCurrentSave()) return null
      if (!result.ok) {
        if (result.error.status === 409) {
          setDrafts((current) => current[selected.id]
            ? { ...current, [selected.id]: { ...current[selected.id], conflict: true } }
            : current)
          const refreshed = await refreshConflictRevision(selected, owner)
          if (!isCurrentSave()) return null
          setError(refreshed
            ? '此条目已在其他位置更新。已保留本地草稿并同步最新版本，请再次保存。'
            : '此条目已在其他位置更新。已保留本地草稿，但暂时无法读取最新版本。')
        } else {
          setError(errorMessage(result.error))
        }
        return null
      }
      advanceInboxLifecycle([selected.id])
      upsertAuthoritativeInbox(result.data)
      setDrafts((current) => {
        const latest = current[selected.id]
        if (latest && result.data.metadata_revision < latest.baseline.revision) return current
        const savedDraft = draftFromInbox(result.data)
        if (!latest || !draftFieldsDiffer(latest, submittedDraft)) {
          return { ...current, [selected.id]: savedDraft }
        }
        return {
          ...current,
          [selected.id]: {
            ...savedDraft,
            title: latest.title !== submittedDraft.title ? latest.title : savedDraft.title,
            body: latest.body !== submittedDraft.body ? latest.body : savedDraft.body,
            note: latest.note !== submittedDraft.note ? latest.note : savedDraft.note,
            summary: latest.summary !== submittedDraft.summary ? latest.summary : savedDraft.summary,
            tags: latest.tags !== submittedDraft.tags ? latest.tags : savedDraft.tags,
          },
        }
      })
      return result.data
    } finally {
      finishSavingForOwner(owner, savingGeneration)
    }
  }, [advanceInboxLifecycle, beginSavingForOwner, body, captureActionOwner, client, finishSavingForOwner, isCurrentActionOwner, note, refreshConflictRevision, selected, selectedDraft, summary, tags, title, upsertAuthoritativeInbox])

  const selectInbox = useCallback(async (id: string) => {
    if (id === selected?.id) return
    const operationGeneration = ++inboxOperationGenerationRef.current
    if (dirty && !await saveMetadata()) return
    if (inboxOperationGenerationRef.current !== operationGeneration) return
    setSelectedID(id)
  }, [dirty, saveMetadata, selected?.id])

  const createInbox = useCallback(async () => {
    const owner = captureActionOwner()
    if (!owner) return
    if (!url.trim()) return
    const savingGeneration = beginSavingForOwner()
    setError(null)
    try {
      const result = await client.createInbox({
        url: url.trim(),
        source_kind: 'manual',
        title: createTitle.trim() || null,
        body: createBody,
        note: createNote,
        tags: parseTags(createTags),
      })
      if (!isCurrentActionOwner(owner)) return
      if (!result.ok) setError(errorMessage(result.error))
      else {
        advanceInboxLifecycle([result.data.id])
        if (result.data.status === 'pending') {
          adjustInboxCounts(result.data.expired ? 0 : 1, result.data.expired ? 1 : 0)
        }
        const target = upsertAuthoritativeInbox(result.data, 'prepend')
        setPartition(target)
        setSelectedID(result.data.id)
        setCreateOpen(false)
        setUrl('')
        setCreateTitle('')
        setCreateBody('')
        setCreateNote('')
        setCreateTags('')
        refreshPendingInboxCount()
        void load(target)
      }
    } finally {
      finishSavingForOwner(owner, savingGeneration)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginSavingForOwner, captureActionOwner, client, createBody, createNote, createTags, createTitle, finishSavingForOwner, isCurrentActionOwner, load, upsertAuthoritativeInbox, url])

  const confirm = useCallback(async () => {
    const owner = captureActionOwner()
    if (!owner) return
    if (!selected || selected.status !== 'pending') return
    if (!title.trim()) {
      setError('缺少标题，无法确认入库。')
      return
    }
    const inboxID = selected.id
    const operationGeneration = ++inboxOperationGenerationRef.current
    const isCurrentConfirmation = () =>
      isCurrentActionOwner(owner) &&
      inboxOperationGenerationRef.current === operationGeneration
    const saved = await saveMetadata(owner)
    if (!saved || !isCurrentConfirmation()) return
    const savingGeneration = beginSavingForOwner()
    setError(null)
    try {
      const result = await client.confirmInbox(inboxID, saved.metadata_revision)
      if (!isCurrentConfirmation()) return
      if (!result.ok) {
        if (result.error.status === 409) {
          setDrafts((current) => current[inboxID]
            ? { ...current, [inboxID]: { ...current[inboxID], conflict: true } }
            : current)
          const refreshed = await refreshConflictRevision(saved, owner)
          if (!isCurrentConfirmation()) return
          setError(refreshed
            ? '此条目已在其他位置更新。已保留本地草稿并同步最新版本，请再次确认。'
            : '此条目已在其他位置更新。已保留本地草稿，但暂时无法读取最新版本。')
        } else {
          setError(errorMessage(result.error))
        }
      } else {
        const target: ReaderInboxPartition = saved.expired ? 'expired' : 'active'
        const ids = new Set([inboxID])
        advanceInboxLifecycle([inboxID])
        adjustInboxCounts(saved.expired ? 0 : -1, saved.expired ? -1 : 0)
        updatePartitionAndEvictOther(target, ids, (current) => ({
          ...current,
          items: current.items.filter((item) => !ids.has(item.id)),
        }))
        setSelectedIDs((current) => {
          const next = new Set(current)
          next.delete(inboxID)
          return next
        })
        setSelectedID((current) => current === inboxID ? null : current)
        setBulkResult(null)
        onOpenLink(result.data.link_id)
        refreshPendingInboxCount()
        void load(target)
      }
    } finally {
      finishSavingForOwner(owner, savingGeneration)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginSavingForOwner, captureActionOwner, client, finishSavingForOwner, isCurrentActionOwner, load, onOpenLink, refreshConflictRevision, saveMetadata, selected, title, updatePartitionAndEvictOther])

  const discard = useCallback(async () => {
    const owner = captureActionOwner()
    if (!owner) return
    if (!selected || selected.status !== 'pending') return
    const savingGeneration = beginSavingForOwner()
    setError(null)
    try {
      const result = await client.discardInbox(selected.id)
      if (!isCurrentActionOwner(owner)) return
      if (!result.ok) setError(errorMessage(result.error))
      else {
        const ids = new Set([selected.id])
        const target: ReaderInboxPartition = selected.expired ? 'expired' : 'active'
        advanceInboxLifecycle([selected.id])
        adjustInboxCounts(selected.expired ? 0 : -1, selected.expired ? -1 : 0)
        updatePartitionAndEvictOther(target, ids, (current) => ({
          ...current,
          items: updateInboxStatus(current.items, ids, 'discarded'),
        }))
        setSelectedIDs((current) => {
          const next = new Set(current)
          next.delete(selected.id)
          return next
        })
        setBulkResult(null)
        refreshPendingInboxCount()
        void load(target)
      }
    } finally {
      finishSavingForOwner(owner, savingGeneration)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginSavingForOwner, captureActionOwner, client, finishSavingForOwner, isCurrentActionOwner, load, selected, updatePartitionAndEvictOther])

  const restore = useCallback(async () => {
    const owner = captureActionOwner()
    if (!owner) return
    if (!selected || (selected.status !== 'discarded' && !selected.expired)) return
    const savingGeneration = beginSavingForOwner()
    setError(null)
    try {
      const result = await client.restoreInbox(selected.id)
      if (!isCurrentActionOwner(owner)) return
      if (!result.ok) setError(errorMessage(result.error))
      else {
        const ids = new Set([selected.id])
        advanceInboxLifecycle([selected.id])
        adjustInboxCounts(1, selected.status === 'pending' && selected.expired ? -1 : 0)
        loadRequestGenerationRef.current.expired += 1
        setInboxPages((current) => ({
          active: {
            ...current.active,
            items: current.active.items.filter((item) => !ids.has(item.id)),
            loaded: false,
            loading: false,
            loadingMore: false,
            error: null,
          },
          expired: {
            ...current.expired,
            items: current.expired.items.filter((item) => !ids.has(item.id)),
            nextCursor: undefined,
            loaded: false,
            loading: false,
            loadingMore: false,
            error: null,
          },
        }))
        setSelectedIDs((current) => {
          const next = new Set(current)
          next.delete(selected.id)
          return next
        })
        setSelectedID(null)
        setPartition('active')
        setBulkResult(null)
        refreshPendingInboxCount()
        void load('active')
      }
    } finally {
      finishSavingForOwner(owner, savingGeneration)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginSavingForOwner, captureActionOwner, client, finishSavingForOwner, isCurrentActionOwner, load, selected])

  const runBulk = useCallback(async (action: 'confirm' | 'discard') => {
    const owner = captureActionOwner()
    if (!owner) return
    if (action === 'confirm' && selectedPendingHasEmptyTitle) {
      setError('缺少标题，无法批量确认。')
      return
    }
    const ids = selectedPendingItems.map((item) => item.id)
    if (ids.length === 0) return
    let savedSelected: ReaderInboxResponse | null = null
    if (action === 'confirm' && selected && selectedIDs.has(selected.id)) {
      savedSelected = await saveMetadata(owner)
      if (!savedSelected) return
    }
    if (!isCurrentActionOwner(owner)) return
    const savingGeneration = beginSavingForOwner()
    setError(null)
    setBulkResult(null)
    try {
      const request = action === 'confirm'
        ? {
            inbox_ids: ids,
            expected_revisions: Object.fromEntries(selectedPendingItems.map((item) => [
              item.id,
              item.id === savedSelected?.id
                ? savedSelected.metadata_revision
                : drafts[item.id]?.revision ?? item.metadata_revision,
            ])),
          }
        : { inbox_ids: ids }
      const result = action === 'confirm'
        ? await client.confirmInboxBulk(request)
        : await client.discardInboxBulk(request)
      if (!isCurrentActionOwner(owner)) return
      if (!result.ok) {
        if (action === 'confirm' && result.error.status === 409) {
          await Promise.all(selectedPendingItems.map((item) => refreshConflictRevision(item, owner)))
          if (!isCurrentActionOwner(owner)) return
        }
        setError(errorMessage(result.error))
      } else {
        const idSet = new Set(ids)
        advanceInboxLifecycle(ids)
        const activeCount = selectedPendingItems.filter((item) => !item.expired).length
        const expiredCount = selectedPendingItems.length - activeCount
        adjustInboxCounts(-activeCount, -expiredCount)
        setBulkResult({ action, response: result.data })
        setSelectedIDs((current) => {
          const next = new Set(current)
          for (const id of ids) next.delete(id)
          return next
        })
        updatePartitionAndEvictOther(partition, idSet, (current) => ({
          ...current,
          items: action === 'confirm'
            ? current.items.filter((item) => !idSet.has(item.id))
            : updateInboxStatus(current.items, idSet, 'discarded'),
        }))
        if (action === 'confirm') {
          setSelectedID((current) => current && idSet.has(current) ? null : current)
        }
        refreshPendingInboxCount()
        void load(partition)
      }
    } finally {
      finishSavingForOwner(owner, savingGeneration)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginSavingForOwner, captureActionOwner, client, drafts, finishSavingForOwner, isCurrentActionOwner, load, partition, refreshConflictRevision, saveMetadata, selected, selectedIDs, selectedPendingHasEmptyTitle, selectedPendingItems, updatePartitionAndEvictOther])

  const confirmAIProposals = useCallback(async () => {
    const owner = captureActionOwner()
    if (!owner || saving) return
    const target = partition
    const confirmedItems: ReaderInboxConfirmAIProposalsResponse['items'] = []
    const savingGeneration = beginSavingForOwner()
    let actionError: string | null = null
    setError(null)
    setBulkResult(null)
    try {
      for (;;) {
        const result = await client.confirmAIProposals({ partition: target })
        if (!isCurrentActionOwner(owner)) return
        if (!result.ok) {
          actionError = errorMessage(result.error)
          break
        }
        const confirmedIDs = new Set(result.data.items.map((item) => item.inbox_id))
        if (confirmedIDs.size > 0) {
          advanceInboxLifecycle([...confirmedIDs])
          adjustInboxCounts(
            target === 'active' ? -confirmedIDs.size : 0,
            target === 'expired' ? -confirmedIDs.size : 0,
          )
          updatePartitionAndEvictOther(target, confirmedIDs, (current) => ({
            ...current,
            items: current.items.filter((item) => !confirmedIDs.has(item.id)),
          }))
          setSelectedIDs((current) => {
            const next = new Set(current)
            for (const id of confirmedIDs) next.delete(id)
            return next
          })
          setSelectedID((current) => current && confirmedIDs.has(current) ? null : current)
        }
        confirmedItems.push(...result.data.items)
        if (result.data.remaining_count === 0) {
          setBulkResult({
            action: 'confirm-ai',
            response: { ...result.data, items: confirmedItems, remaining_count: 0 },
          })
          break
        }
        if (result.data.items.length === 0) {
          actionError = 'AI 确认队列未前进，请刷新后重试。'
          break
        }
      }
    } catch (cause) {
      if (isCurrentActionOwner(owner)) {
        actionError = cause instanceof Error ? cause.message : '批量确认失败，请重试。'
      }
    } finally {
      if (isCurrentActionOwner(owner)) {
        await load(target)
        if (isCurrentActionOwner(owner)) {
          refreshPendingInboxCount()
          if (actionError) setError(actionError)
        }
      }
      finishSavingForOwner(owner, savingGeneration)
    }
  }, [adjustInboxCounts, advanceInboxLifecycle, beginSavingForOwner, captureActionOwner, client, finishSavingForOwner, isCurrentActionOwner, load, partition, saving, updatePartitionAndEvictOther])

  const toggleSelection = useCallback((id: string) => {
    setSelectedIDs((current) => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])

  const toggleAllSelection = useCallback(() => {
    setSelectedIDs(() => {
      if (allPendingSelected) return new Set()
      return new Set(pendingItems.map((item) => item.id))
    })
  }, [allPendingSelected, pendingItems])

  const selectPartition = useCallback(async (next: ReaderInboxPartition) => {
    if (next === partition || saving) return
    const operationGeneration = ++inboxOperationGenerationRef.current
    if (dirty && !await saveMetadata()) return
    if (inboxOperationGenerationRef.current !== operationGeneration) return
    setPartition(next)
    setSelectedID(null)
    setSelectedIDs(new Set())
    setJob(null)
    setError(null)
    setBulkResult(null)
  }, [dirty, partition, saveMetadata, saving])

  const resummarize = useCallback(async () => {
    const owner = captureActionOwner()
    if (!owner) return
    if (!selected || selected.status !== 'pending') return
    const savingGeneration = beginSavingForOwner()
    setError(null)
    try {
      const result = await client.resummarizeInbox(selected.id)
      if (!isCurrentActionOwner(owner)) return
      if (!result.ok) setError(errorMessage(result.error))
      else {
        advanceInboxLifecycle([selected.id])
        setJob(result.data)
        setItems((current) => current.map((item) => item.id === selected.id ? { ...item, job_id: result.data.job_id } : item))
      }
    } finally {
      finishSavingForOwner(owner, savingGeneration)
    }
  }, [advanceInboxLifecycle, beginSavingForOwner, captureActionOwner, client, finishSavingForOwner, isCurrentActionOwner, selected, setItems])

  const addCategory = useCallback(async () => {
    const owner = captureActionOwner()
    if (!owner) return
    if (!newCategory.trim()) return
    const result = await client.createCategory({ name: newCategory.trim() })
    if (!isCurrentActionOwner(owner)) return
    if (!result.ok) setError(errorMessage(result.error))
    else {
      setCategories((current) => [...current, result.data])
      setNewCategory('')
    }
  }, [captureActionOwner, client, isCurrentActionOwner, newCategory])

  const toggleCategory = useCallback(async (category: ReaderCategoryResponse) => {
    const owner = captureActionOwner()
    if (!owner) return
    if (!selected) return
    const inboxID = selected.id
    const present = memberships.has(category.id)
    const initialCategoryIDs = selected.category_ids
    const requestKey = `${inboxID}:${category.id}`
    const requestGeneration = (categoryRequestGenerationsRef.current.get(requestKey) ?? 0) + 1
    categoryRequestGenerationsRef.current.set(requestKey, requestGeneration)
    const result = await client.setCategoryMembership(category.id, { host_kind: 'inbox', host_id: inboxID, present: !present })
    if (
      !isCurrentActionOwner(owner) ||
      categoryRequestGenerationsRef.current.get(requestKey) !== requestGeneration
    ) return
    if (!result.ok) setError(errorMessage(result.error))
    else {
      advanceInboxLifecycle([inboxID])
      setMembershipsByInbox((current) => {
        const next = new Set(current[inboxID] ?? initialCategoryIDs)
        if (present) next.delete(category.id)
        else next.add(category.id)
        return { ...current, [inboxID]: next }
      })
      setItems((current) => current.map((item) => item.id === inboxID
        ? {
            ...item,
            category_ids: present
              ? item.category_ids.filter((id) => id !== category.id)
              : [...new Set([...item.category_ids, category.id])],
          }
        : item))
    }
    if (categoryRequestGenerationsRef.current.get(requestKey) === requestGeneration) {
      categoryRequestGenerationsRef.current.delete(requestKey)
    }
  }, [advanceInboxLifecycle, captureActionOwner, client, isCurrentActionOwner, memberships, selected, setItems])

  const adoptSuggestedTag = useCallback((tag: string) => {
    updateSelectedDraft({ tags: parseTags([...parseTags(tags), tag].join(', ')).join(', ') })
  }, [tags, updateSelectedDraft])

  return (
    <SurfaceShell
      title="收件箱"
      subtitle={`活跃 ${inboxCounts.activeCount} 项 · 已过期 ${inboxCounts.expiredCount} 项${discardedCount > 0 ? ` · ${discardedCount} 项已丢弃` : ''}`}
      active="pending"
      onNavigate={onNavigate}
      capabilityPolicy={capabilityPolicy}
      actions={<button className="rvx-button primary" type="button" onClick={() => setCreateOpen((open) => !open)}><Icon name="plus" size={15} />添加条目</button>}
    >
      <div className="rvx-editor-actions" role="tablist" aria-label="收件箱分区" style={{ margin: '0 0 18px' }}>
        <button
          className={partition === 'active' ? 'rvx-button primary' : 'rvx-button secondary'}
          type="button"
          role="tab"
          aria-selected={partition === 'active'}
          onClick={() => selectPartition('active')}
        >
          活跃 ({inboxCounts.activeCount})
        </button>
        <button
          className={partition === 'expired' ? 'rvx-button primary' : 'rvx-button secondary'}
          type="button"
          role="tab"
          aria-selected={partition === 'expired'}
          onClick={() => selectPartition('expired')}
        >
          已过期 ({inboxCounts.expiredCount})
        </button>
      </div>
      {displayError && <SurfaceError message={displayError} onRetry={() => void load()} />}
      {bulkResult && (
        <section className="rvx-proposal" aria-label="批量操作结果" role="status" aria-live="polite">
          <div className="rvx-section-head">
            <div>
              <h3>{bulkResult.action === 'confirm-ai'
                ? 'AI 建议确认已完成'
                : `批量${bulkResult.action === 'confirm' ? '确认' : '丢弃'}完成`}</h3>
            </div>
          </div>
          <p>本次操作共完成 {bulkResult.response.items.length} 项。</p>
          <ul style={{ margin: '8px 0 0', paddingLeft: '18px' }}>
            {bulkResult.response.items.map((item, index) => (
              <li key={`${item.inbox_id}-${index}`}>
                <span>{item.inbox_id} · {item.status === 'confirmed' ? '已确认入库' : '已丢弃'}</span>
                {item.link_id && <button className="rvx-link-button" type="button" onClick={() => onOpenLink(item.link_id as string)}>打开已确认链接 {item.link_id}</button>}
              </li>
            ))}
          </ul>
        </section>
      )}
      {createOpen && (
        <section className="rvx-editor rvx-create-panel" aria-label="添加条目">
          <div className="rvx-form-grid">
            <label>网址<input value={url} onChange={(event) => setUrl(event.target.value)} placeholder="https://..." type="url" /></label>
            <label>标题<input value={createTitle} onChange={(event) => setCreateTitle(event.target.value)} /></label>
			<label>笔记<textarea value={createNote} onChange={(event) => setCreateNote(event.target.value)} rows={2} /></label>
            <label>标签<input value={createTags} onChange={(event) => setCreateTags(event.target.value)} placeholder="用逗号或空格分隔" /></label>
            <label>原始内容<textarea value={createBody} onChange={(event) => setCreateBody(event.target.value)} rows={3} /></label>
          </div>
          <div className="rvx-editor-actions"><button className="rvx-button secondary" type="button" onClick={() => setCreateOpen(false)}>取消</button><button className="rvx-button primary" type="button" disabled={saving || !url.trim()} onClick={() => void createInbox()}>创建</button></div>
        </section>
      )}
      {(loading || loadingTarget) && items.length === 0 ? <SurfaceLoading /> : inboxError && items.length === 0 ? null : items.length === 0 ? <div className="rvx-empty"><Icon name="inbox" size={24} /><h2>{partition === 'expired' ? '没有已过期内容' : '收件箱是空的'}</h2><p>{partition === 'expired' ? '过期内容会保留在这里，可恢复有效期、确认或丢弃。' : '从扩展、批量采集或上面的手动入口添加内容。'}</p></div> : (
        <>
          {pendingItems.length > 0 && (
            <section className="rvx-editor" aria-label="批量操作" style={{ marginBottom: '18px' }}>
              <div className="rvx-section-head">
                <label style={{ display: 'inline-flex', alignItems: 'center', gap: '7px' }}>
                  <input type="checkbox" aria-label="全选" checked={allPendingSelected} disabled={saving} onChange={toggleAllSelection} />
                  <span>全选</span>
                </label>
                <span className="rvx-muted">已选择 {selectedPendingCount} 项</span>
                <div className="rvx-editor-actions" style={{ marginTop: 0 }}>
                  <button className="rvx-button primary" type="button" disabled={saving} onClick={() => void confirmAIProposals()}><Icon name="sparkles" size={15} />确认全部 AI 建议</button>
                  <button className="rvx-button secondary" type="button" disabled={saving || selectedPendingCount === 0 || selectedPendingHasEmptyTitle} onClick={() => void runBulk('confirm')}><Icon name="check" size={15} />确认所选</button>
                  <button className="rvx-button secondary danger" type="button" disabled={saving || selectedPendingCount === 0} onClick={() => void runBulk('discard')}><Icon name="trash" size={15} />批量丢弃</button>
                </div>
              </div>
            </section>
          )}
        <div className="rvx-split">
          <aside className="rvx-list-column" aria-label="收件箱列表">
            <ul className="rvx-compact-list">
              {items.map((item) => <li key={item.id}>
                <div style={{ display: 'flex', alignItems: 'stretch' }}>
                  <label style={{ display: 'flex', alignItems: 'center', padding: '0 8px' }}>
                    <input type="checkbox" aria-label={`选择 ${item.title || item.url}`} checked={selectedIDs.has(item.id)} disabled={saving || item.status !== 'pending'} onChange={() => toggleSelection(item.id)} />
                  </label>
                  <button type="button" className={item.id === selected?.id ? 'active' : ''} onClick={() => void selectInbox(item.id)}><strong>{item.title || item.url}</strong><small>{item.expired ? '已过期 · ' : ''}{statusLabel(item.status)} · {formatRelativeDate(item.updated_at)}</small></button>
                </div>
              </li>)}
            </ul>
            {nextCursor && <button className="rvx-load-more" type="button" disabled={loadingMore || saving} onClick={() => void load(partition, true, nextCursor)}>{loadingMore ? '加载中…' : '更多'}</button>}
          </aside>
          {selected && (
            <section className="rvx-detail-column" aria-label="条目编辑器">
              <div className="rvx-detail-heading"><div><span className="rvx-source-chip">{selected.source_kind}</span><h2>{selected.title || '未命名条目'}</h2><a href={selected.url} target="_blank" rel="noreferrer">{selected.url}</a></div><span className="rvx-status-chip">{selected.expired ? '已过期' : statusLabel(selected.status)}</span></div>
              {selectedDraft?.conflict && <p className="rvx-proposal" role="status">检测到其他位置的更新。本地草稿已保留，并会在下次保存时使用最新版本。</p>}
              <label className="rvx-field">标题<input value={title} disabled={saving} onChange={(event) => updateSelectedDraft({ title: event.target.value })} /></label>
              <label className="rvx-field">笔记<textarea value={note} disabled={saving} onChange={(event) => updateSelectedDraft({ note: event.target.value })} rows={4} /></label>
              <label className="rvx-field">摘要<textarea value={summary} disabled={saving} onChange={(event) => updateSelectedDraft({ summary: event.target.value })} rows={4} /></label>
              <label className="rvx-field">标签<input value={tags} disabled={saving} onChange={(event) => updateSelectedDraft({ tags: event.target.value })} /></label>
              <div className="rvx-field"><span>正文</span><div className="rvx-editor-actions"><button type="button" className="rvx-button secondary" disabled={saving} onClick={() => setPreviewBody(false)} aria-pressed={!previewBody}>编辑</button><button type="button" className="rvx-button secondary" disabled={saving} onClick={() => setPreviewBody(true)} aria-pressed={previewBody}>预览</button></div>{previewBody ? <pre>{body || '暂无正文'}</pre> : <textarea aria-label="正文" value={body} disabled={saving} onChange={(event) => updateSelectedDraft({ body: event.target.value })} rows={9} />}</div>
              <div className="rvx-proposal"><span className="rvx-eyebrow">AI 建议标签</span><div className="rvx-chip-row">{selected.suggested_tags.filter((tag) => !parseTags(tags).includes(tag)).map((tag) => <button key={tag} type="button" className="rvx-tag-chip" disabled={saving} onClick={() => adoptSuggestedTag(tag)}>采用 #{tag}</button>)}</div></div>
              <div className="rvx-category-row"><span>分类</span>{categories.map((category) => <button key={category.id} type="button" disabled={saving} className={'rvx-tag-chip' + (memberships.has(category.id) ? ' active' : '')} onClick={() => void toggleCategory(category)}>{category.name}</button>)}<input value={newCategory} disabled={saving} onChange={(event) => setNewCategory(event.target.value)} placeholder="新分类" onKeyDown={(event) => { if (event.key === 'Enter') void addCategory() }} /></div>
              <details className="rvx-source-details"><summary>查看原始内容</summary><pre>{selected.body}</pre></details>
              {selected.job_id && <p className="rvx-muted">摘要任务：{job?.status ?? 'queued'} · {selected.job_id}</p>}
              <div className="rvx-editor-actions">
                {selected.status === 'discarded' ? <button className="rvx-button primary" type="button" disabled={saving} onClick={() => void restore()}><Icon name="refresh" size={15} />恢复到收件箱</button> : selected.status === 'pending' ? <>{selected.expired && <button className="rvx-button secondary" type="button" disabled={saving} onClick={() => void restore()}><Icon name="refresh" size={15} />恢复有效期</button>}<button className="rvx-button secondary" type="button" disabled={saving} onClick={() => void resummarize()}><Icon name="sparkles" size={15} />重新生成摘要</button><button className="rvx-button secondary danger" type="button" disabled={saving} onClick={() => void discard()}><Icon name="trash" size={15} />丢弃</button><button className="rvx-button primary" type="button" disabled={saving || !title.trim()} onClick={() => void confirm()}><Icon name="check" size={15} />确认入库</button></> : <span className="rvx-muted">该内容已确认入库。</span>}
                {selected.status !== 'confirmed' && <button className="rvx-button secondary" type="button" disabled={saving} onClick={() => void saveMetadata()}>保存元数据</button>}
              </div>
            </section>
          )}
        </div>
        </>
      )}
    </SurfaceShell>
  )
}
