import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type Dispatch,
  type MutableRefObject,
  type SetStateAction,
} from 'react'
import type { MetadataEditOutcome } from '../DetailPane'
import type { IconName } from '../Icon'
import type { ToastAction } from '../Toast'
import { useLinks, type Selection } from '../../hooks/useLinks'
import { useAnnotatedLinkCount } from '../../hooks/useAnnotatedLinks'
import { useDomainSummaries } from '../../hooks/useDomainSummaries'
import { usePrefetch, type PrefetchTarget } from '../../hooks/usePrefetch'
import { invalidateReaderActivity } from '../../hooks/useReaderActivity'
import { invalidateReaderRelatedTags } from '../../hooks/useReaderRelatedTags'
import { useSavedArticleDocument } from '../../hooks/useSavedArticleDocument'
import { useSidebarData } from '../../hooks/useSidebarData'
import { translationsKey } from '../../hooks/useTranslations'
import { useTags } from '../../hooks/useTags'
import {
  invalidateLibrary,
  invalidateLink,
  invalidateLinkProjection,
} from '../../lib/cache/invalidate'
import { loadLinkContent } from '../../lib/cache/link-content'
import {
  loadRevisionFloors,
  mergeRevisionFloors,
  noteRevisionFloor,
  revisionFloorStorageKey,
} from '../../lib/cache/revision-floor'
import { resourceStore } from '../../lib/cache/store'
import type {
  DocumentCommandContext,
  SavedArticleDocumentController,
} from '../../lib/article/document'
import type { SourceBlockId } from '../../lib/article/source-block'
import type { IdentityLease } from '../../lib/identity'
import type { ReaderLibrarySitesPort } from '../../lib/reader-api-ports'
import type {
  LinkResponse,
  ReaderLinkMetadataRequest,
} from '../../lib/api/types'
import type { ApiError } from '@webtag/api'
import {
  type ReaderCapabilityLease,
} from '../../lib/capabilities'
import type { ReaderRoute, ReaderRouteTargets } from '../../lib/navigation/route'
import {
  type MainViewRoute,
  type OpenLinkOptions,
} from './navigation-controller'

const CORPUS_LIMIT = 100

interface SummarySourceIdentity {
  linkId: string
  source: string | null
  hash: string | null
}

type DetailRequestState =
  | {
      readonly id: string
      readonly sequence: number
      readonly status: 'loading'
    }
  | {
      readonly id: string
      readonly sequence: number
      readonly status: 'error'
      readonly error: ApiError
    }

interface OwnedDetailRequest {
  readonly id: string
  readonly sequence: number
  readonly controller: SavedArticleDocumentController
  readonly context: DocumentCommandContext
}

interface MetadataProjection {
  readonly metadataRevision: number
  readonly title: string | null
  readonly summary: string | null
  readonly tags: readonly string[]
}

type Flash = (msg: string, icon?: IconName, action?: ToastAction) => void

interface UseActiveResourceControllerOptions {
  readonly client: ReaderLibrarySitesPort
  readonly lease: IdentityLease
  readonly capabilityLease: ReaderCapabilityLease
  readonly activeId: string | null
  readonly setActiveId: (value: string | null) => void
  readonly view: MainViewRoute
  readonly pendingLinkTarget: MutableRefObject<string | null>
  readonly commitRoute: (
    route: ReaderRoute,
    targets?: ReaderRouteTargets,
    historyMode?: 'push' | 'replace' | 'none',
    addressLink?: boolean,
  ) => void
  readonly confirmDiscardNavigation: () => boolean | Promise<boolean>
  readonly confirmDiscardContentEdit: () => boolean
  readonly setMobilePane: (value: 'list' | 'detail') => void
  readonly setMobileNavOpen: Dispatch<SetStateAction<boolean>>
  readonly flash: Flash
  readonly dismissToast: () => void
}

function metadataProjectionFrom(value: Partial<LinkResponse>): MetadataProjection | null {
  const revision = value.metadata_revision
  const title = value.title
  const summary = value.summary
  const tags = value.tags
  if (
    typeof revision !== 'number' ||
    !Number.isSafeInteger(revision) ||
    revision < 1 ||
    (title !== null && typeof title !== 'string') ||
    (summary !== null && typeof summary !== 'string') ||
    !Array.isArray(tags) ||
    !tags.every((tag) => typeof tag === 'string')
  ) {
    return null
  }
  return {
    metadataRevision: revision,
    title,
    summary,
    tags: [...tags],
  }
}

function sameMetadataProjection(
  left: MetadataProjection | undefined,
  right: MetadataProjection,
): boolean {
  return left?.metadataRevision === right.metadataRevision &&
    left.title === right.title &&
    left.summary === right.summary &&
    left.tags.length === right.tags.length &&
    left.tags.every((tag, index) => tag === right.tags[index])
}

function metadataPatchTouchesTuple(patch: Partial<LinkResponse>): boolean {
  return (
    Object.prototype.hasOwnProperty.call(patch, 'metadata_revision') ||
    Object.prototype.hasOwnProperty.call(patch, 'title') ||
    Object.prototype.hasOwnProperty.call(patch, 'summary') ||
    Object.prototype.hasOwnProperty.call(patch, 'tags')
  )
}

async function sha256Hex(text: string): Promise<string> {
  if (!globalThis.crypto?.subtle) throw new Error('Web Crypto is unavailable')
  const digest = await globalThis.crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(text),
  )
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')
}

/** 当前/最近已见链接的有界去重缓存；新数据优先，避免长期会话无界增长。 */
function mergeCorpus(
  current: LinkResponse[],
  incoming: LinkResponse[],
): LinkResponse[] {
  if (incoming.length === 0) return current
  const byID = new Map<string, LinkResponse>()
  incoming.forEach((link) => byID.set(link.id, link))
  current.forEach((link) => {
    if (!byID.has(link.id)) byID.set(link.id, link)
  })
  const merged = [...byID.values()].slice(0, CORPUS_LIMIT)
  // 结果与原来逐项同一引用时返回**原数组**。
  //
  // corpus 是 DetailPane / CommandPalette / BrowsePanel 的 prop，而这个函数
  // 每次都新建数组——于是列表每一次校验（哪怕后端一个字节都没变）都会换掉
  // corpus 的引用，把刚加上的那几层 memo 全部击穿。PF3 之后列表校验在数据
  // 未变时已经不再通知，但「同一批链接重新合并一次」的路径仍然存在。
  if (merged.length === current.length && merged.every((link, index) => link === current[index])) {
    return current
  }
  return merged
}

function pagerTitle(link: LinkResponse): string {
  return link.title?.trim() || link.domain?.trim() || link.url
}

/**
 * 按 content_revision 下界抬升一条链接的代次；已经不低于下界时原样返回，
 * 不制造多余的新对象（`active` 会流向已 memo 的 DetailPane）。
 */
function liftContentRevision(
  link: LinkResponse | undefined,
  floor: number | undefined,
): LinkResponse | undefined {
  if (!link || floor === undefined || (link.content_revision ?? 0) >= floor) return link
  return {
    ...link,
    content_revision: floor,
    content: undefined,
    content_document: undefined,
    content_format: undefined,
  }
}

export function contentRevisionOrUndefined(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
    ? value
    : undefined
}

export function useActiveResourceController({
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
}: UseActiveResourceControllerOptions) {
  const [selection, setSelection] = useState<Selection>({ type: 'smart', id: 'all', name: '全部链接' })
  const [activeFallback, setActiveFallback] = useState<LinkResponse | null>(null)
  const [activeDetail, setActiveDetail] = useState<LinkResponse | null>(null)
  const [summarySourceIdentity, setSummarySourceIdentity] =
    useState<SummarySourceIdentity | null>(null)
  const [summaryProjectionEpoch, setSummaryProjectionEpoch] = useState(0)
  const [metadataProjectionEpoch, setMetadataProjectionEpoch] = useState(0)
  const [corpus, setCorpus] = useState<LinkResponse[]>([])
  const [detailRequest, setDetailRequest] = useState<DetailRequestState | null>(null)
  const [revisionFloor, setRevisionFloor] = useState<Map<string, number>>(loadRevisionFloors)
  const detailRequestSeq = useRef(0)
  const ownedDetailRequest = useRef<OwnedDetailRequest | null>(null)
  const flashedDetailError = useRef<number | null>(null)
  const automaticOpenRef = useRef<string | null>(null)
  const activeRef = useRef<LinkResponse | undefined>(undefined)
  const metadataProjectionRef = useRef(new Map<string, MetadataProjection>())

  const list = useLinks(client, selection)
  const { patchLink } = list
  const tagsData = useTags(client)
  const domainData = useDomainSummaries(client)

  const recordMetadataProjection = useCallback((id: string, value: Partial<LinkResponse>) => {
    const next = metadataProjectionFrom(value)
    if (!next) return
    const current = metadataProjectionRef.current.get(id)
    if (current && current.metadataRevision > next.metadataRevision) return
    if (sameMetadataProjection(current, next)) return
    metadataProjectionRef.current.set(id, next)
    setMetadataProjectionEpoch((epoch) => epoch + 1)
  }, [])

  const protectMetadataLink = useCallback((link: LinkResponse): LinkResponse => {
    const projection = metadataProjectionRef.current.get(link.id)
    if (!projection || link.metadata_revision >= projection.metadataRevision) return link
    return {
      ...link,
      title: projection.title,
      summary: projection.summary,
      tags: [...projection.tags],
      metadata_revision: projection.metadataRevision,
    }
  }, [])

  const protectMetadataPatch = useCallback((id: string, patch: Partial<LinkResponse>) => {
    recordMetadataProjection(id, patch)
    const projection = metadataProjectionRef.current.get(id)
    if (!projection || !metadataPatchTouchesTuple(patch)) return patch
    if (
      typeof patch.metadata_revision === 'number' &&
      patch.metadata_revision >= projection.metadataRevision
    ) {
      return patch
    }
    return {
      ...patch,
      title: projection.title,
      summary: projection.summary,
      tags: [...projection.tags],
      metadata_revision: projection.metadataRevision,
    }
  }, [recordMetadataProjection])

  const acceptMetadataLink = useCallback((link: LinkResponse): LinkResponse => {
    recordMetadataProjection(link.id, link)
    return protectMetadataLink(link)
  }, [protectMetadataLink, recordMetadataProjection])

  // The projection map is ref-backed so writes can fence an in-flight response
  // synchronously. Its epoch deliberately re-runs this overlay after a write.
  const protectedListLinks = useMemo(
    () => list.links.map(protectMetadataLink),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [list.links, metadataProjectionEpoch, protectMetadataLink],
  )

  useEffect(() => {
    for (const link of list.links) recordMetadataProjection(link.id, link)
  }, [list.links, recordMetadataProjection])

  const knownLinksRef = useRef<{ list: LinkResponse[]; corpus: LinkResponse[] }>({
    list: [],
    corpus: [],
  })
  knownLinksRef.current = { list: protectedListLinks, corpus }

  useEffect(() => {
    setCorpus((current) => mergeCorpus(current, protectedListLinks))
  }, [protectedListLinks])

  const noteContentRevision = useCallback((id: string, revision: number) => {
    setRevisionFloor((current) => {
      const next = new Map(current)
      return noteRevisionFloor(next, id, revision) ? next : current
    })
  }, [])

  useEffect(() => {
    const onStorage = (event: StorageEvent) => {
      // key 为 null 表示 storage 被整体清空（clear()），也要跟进。
      if (event.key !== null && event.key !== revisionFloorStorageKey()) return
      // 按 max 并进来，不是整表替换：本页可能持有盘上没有的更高值（落盘失败，
      // 或本页那条被另一页挤出上限），替换会让下界下降。
      setRevisionFloor((current) => mergeRevisionFloors(current, loadRevisionFloors()))
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  // Mail 原则：详情 active 仅随 activeId 变；筛选 selection 变化不动 active。
  //
  // Keep the server projection's reported revision separate from the UI floor.
  // The lifted object is useful fallback metadata for rendering and commands,
  // but it is not an authoritative detail for that newer revision.
  const activeProjection = useMemo(
    () =>
      (activeDetail?.id === activeId ? activeDetail : undefined) ??
      protectedListLinks.find((link) => link.id === activeId) ??
      (activeFallback?.id === activeId ? activeFallback : undefined) ??
      corpus.find((link) => link.id === activeId),
    [protectedListLinks, activeFallback, activeDetail, corpus, activeId],
  )
  const activeRevisionFloor = activeId ? revisionFloor.get(activeId) : undefined
  const active = useMemo(
    () => liftContentRevision(activeProjection, activeRevisionFloor),
    [activeProjection, activeRevisionFloor],
  )
  // onConvertToSite 需要「点下去那一刻的当前文章」，但它必须是稳定引用
  // （DetailPane 已 memo）。把 active 走 ref 读，回调就不必把 active 列进依赖。
  activeRef.current = active
  const activeSummarySourceHash =
    active &&
    summarySourceIdentity?.linkId === active.id &&
    summarySourceIdentity.source === (active.summary ?? null)
      ? summarySourceIdentity.hash
      : null

  const onSummaryBlockText = useCallback(
    (linkId: string, source: string | null, renderedText: string | null) => {
      if (renderedText === null) {
        setSummarySourceIdentity({ linkId, source, hash: null })
        return
      }
      void sha256Hex(renderedText).then(
        (hash) => {
          const current = activeRef.current
          if (current?.id !== linkId || (current.summary ?? null) !== source) return
          setSummarySourceIdentity((previous) =>
            previous?.linkId === linkId &&
            previous.source === source &&
            previous.hash === hash
              ? previous
              : { linkId, source, hash },
          )
        },
        () => {
          const current = activeRef.current
          if (current?.id !== linkId || (current.summary ?? null) !== source) return
          setSummarySourceIdentity({ linkId, source, hash: null })
        },
      )
    },
    [],
  )
  const resetSummarySourceHash = useCallback((linkId: string, source: string | null) => {
    setSummarySourceIdentity({ linkId, source, hash: null })
    // The canonical text may be unchanged, or getLink may fail. Force the
    // already-mounted DOM projection to report again instead of relying on
    // a source/text mutation to invalidate DetailPane's deduplication key.
    setSummaryProjectionEpoch((epoch) => epoch + 1)
  }, [])

  const loadSavedBody = useCallback(
    async (context: DocumentCommandContext) => {
      const res = await loadLinkContent(
        client,
        context.id.linkId,
        context.id.contentRevision,
      )
      if (!res.ok && res.error.kind === 'unauthorized') {
        flash('鉴权失败，请检查连接配置', 'alert')
      }
      return res
    },
    [client, flash],
  )
  const savedArticle = useSavedArticleDocument({
    lease,
    detail: activeProjection,
    revisionFloor: activeRevisionFloor,
    loadBody: loadSavedBody,
  })
  const savedDocument = savedArticle.document

  // `openLink` starts before React can install the controller for the newly
  // selected article. Bind the request to that controller once it exists and
  // retain the exact generation context for the terminal error transition.
  // A revision advance or identity change aborts that context, so a late
  // failure cannot replace a newer detail resource with an error.
  useLayoutEffect(() => {
    if (!detailRequest) {
      ownedDetailRequest.current = null
      return
    }

    let owned = ownedDetailRequest.current
    if (
      owned &&
      (owned.id !== detailRequest.id || owned.sequence !== detailRequest.sequence)
    ) {
      ownedDetailRequest.current = null
      owned = null
    }

    const controller = savedArticle.controller
    if (!controller || controller.getSnapshot().id.linkId !== detailRequest.id) return
    // The only normal controller replacement for the same link/request is an
    // identity-lease replacement. Never rebind an old request to that owner.
    if (owned && owned.controller !== controller) return

    if (!owned) {
      const context = controller.captureContext()
      if (!controller.beginDetailLoad(context)) return
      owned = {
        id: detailRequest.id,
        sequence: detailRequest.sequence,
        controller,
        context,
      }
      ownedDetailRequest.current = owned
    }

    if (detailRequest.status === 'error') {
      controller.failDetail(detailRequest.error, owned.context)
    }
  }, [detailRequest, savedArticle.controller])

  const documentDetail =
    savedDocument?.detail.status === 'ready' && savedDocument.detail.data.id === active?.id
      ? savedDocument.detail.data
      : null
  const documentFallback =
    savedDocument && active && savedDocument.id.linkId === active.id
      ? liftContentRevision(active, savedDocument.id.contentRevision)
      : active
  const renderedActive = documentDetail ?? documentFallback
  const aiContentContext = (() => {
    if (!savedDocument || !renderedActive) {
      return renderedActive?.content_document ?? renderedActive?.content ?? renderedActive?.summary ?? null
    }
    if (
      savedDocument.id.linkId === renderedActive.id &&
      savedDocument.body.status === 'ready' &&
      savedDocument.body.revision === savedDocument.id.contentRevision &&
      (renderedActive.content_revision === undefined ||
        renderedActive.content_revision === savedDocument.id.contentRevision)
    ) {
      return savedDocument.body.data.content_document ?? savedDocument.body.data.content
    }
    return renderedActive.content_document ?? renderedActive.content ?? renderedActive.summary ?? null
  })()
  const documentOwnsDetailRequest = Boolean(
    detailRequest && savedDocument?.id.linkId === detailRequest.id,
  )
  const detailLoading = detailRequest?.status === 'loading' && detailRequest.id === activeId
    ? documentOwnsDetailRequest
      ? savedDocument?.detail.status === 'loading'
      : true
    : false

  useEffect(() => {
    if (
      detailRequest?.status !== 'error' ||
      flashedDetailError.current === detailRequest.sequence
    ) {
      return
    }

    const activeCanOwnDocument =
      active?.id === detailRequest.id &&
      contentRevisionOrUndefined(active.content_revision) !== undefined
    let error = detailRequest.error
    if (activeCanOwnDocument) {
      if (
        savedDocument?.id.linkId !== detailRequest.id ||
        savedDocument.detail.status !== 'error'
      ) {
        return
      }
      error = savedDocument.detail.error
    }

    flashedDetailError.current = detailRequest.sequence
    flash('加载链接详情失败：' + error.message, 'alert')
  }, [active, detailRequest, flash, savedDocument])

  activeRef.current = renderedActive

  const summaryBlock = useMemo<SourceBlockId | null>(() => {
    if (!renderedActive?.summary || !activeSummarySourceHash) return null
    return {
      namespace: lease.context.physicalNamespace,
      linkId: renderedActive.id,
      blockKind: 'summary',
      sourceHash: activeSummarySourceHash,
    }
  }, [activeSummarySourceHash, lease.context.physicalNamespace, renderedActive])

  useEffect(() => {
    const latest = protectedListLinks.find((link) => link.id === activeId)
    if (latest) {
      setActiveFallback(latest)
      setActiveDetail((current) => {
        if (current?.id !== latest.id) return current
        const merged = { ...current, ...latest }
        const advancesContentRevision =
          typeof latest.content_revision === 'number' &&
          latest.content_revision > (current.content_revision ?? 0)
        if (latest.content === undefined) {
          merged.content = advancesContentRevision ? undefined : current.content
        }
        if (latest.content_document === undefined) {
          merged.content_document = advancesContentRevision
            ? undefined
            : current.content_document
        }
        if (latest.content_format === undefined) {
          merged.content_format = advancesContentRevision
            ? undefined
            : current.content_format
        }
        // PF6 起列表如实汇报 has_content 与两项计数（后端落成了列，
        // has_content 还是生成列），因此这里不再需要「把详情端的真值保住、
        // 别被列表刷新冲掉」那段补丁——它当初存在的唯一理由是列表在撒谎。
        //
        // content_revision 同样不在这里护：陈旧列表确实会把它拖回去，但护在这里
        // 只能盖住「当前正开着这一篇」这一种路径，切走再切回就漏。统一由上面的
        // revisionFloor 在 active 汇合点抬升。
        //
        // has_content 也会被同一段窗口拖回旧值。那是可自愈的短暂闪烁，而且
        // **不能**照代次那样保住：revision 是单调身份，has_content 是当前正文
        // 是否存在的权威状态。RF5A 已让 requeue/site-complete 等 clear 路径也推进
        // revision，但若把 true 变成粘性值，服务端返回的正文删除仍会被本地旧值
        // 遮住，DetailPane 也就无法及时撤下旧正文。
        return merged
      })
    }
  }, [protectedListLinks, activeId])

  const patchKnownLink = useCallback(
    (id: string, patch: Partial<LinkResponse>) => {
      const protectedPatch = protectMetadataPatch(id, patch)
      patchLink(id, protectedPatch)
      setActiveFallback((current) =>
        current?.id === id
          ? protectMetadataLink({ ...current, ...protectedPatch })
          : current,
      )
      setActiveDetail((current) =>
        current?.id === id
          ? protectMetadataLink({ ...current, ...protectedPatch })
          : current,
      )
      setCorpus((current) =>
        current.map((link) => (
          link.id === id
            ? protectMetadataLink({ ...link, ...protectedPatch })
            : link
        )),
      )
    },
    [patchLink, protectMetadataLink, protectMetadataPatch],
  )

  const refreshMetadataViews = useCallback((id: string) => {
    invalidateLibrary()
    invalidateLinkProjection(id)
    invalidateReaderRelatedTags()
    invalidateReaderActivity()
    void Promise.allSettled([
      Promise.resolve(list.reload()),
      Promise.resolve(tagsData.reload()),
      Promise.resolve(domainData.reload()),
    ])
  }, [domainData, list, tagsData])

  const onSaveLinkMetadata = useCallback(
    async (
      id: string,
      revision: number,
      request: ReaderLinkMetadataRequest,
    ): Promise<MetadataEditOutcome> => {
      const result = await client.patchLinkMetadata(id, revision, request)
      if (!client.isIdentityCurrent()) {
        return {
          status: 'error',
          error: { kind: 'identity-mismatch', message: 'Reader identity changed' },
        }
      }

      if (result.ok) {
        if (result.data.link_id !== id) {
          return {
            status: 'error',
            error: { kind: 'other', message: '链接信息保存响应与当前链接不一致' },
          }
        }

        const metadataRevision = result.data.metadata_revision
        patchKnownLink(id, {
          title: request.title,
          summary: request.summary,
          tags: [...request.tags],
          metadata_revision: metadataRevision,
        })
        refreshMetadataViews(id)

        // PATCH returns only a revision. Re-read the normalized tuple in the
        // background, but never let a response from before this write lower the
        // local metadata projection.
        void client.getLink(id).then((refreshed) => {
          if (
            !client.isIdentityCurrent() ||
            !refreshed.ok ||
            refreshed.data.id !== id ||
            refreshed.data.metadata_revision < metadataRevision
          ) {
            return
          }
          patchKnownLink(id, {
            title: refreshed.data.title,
            summary: refreshed.data.summary,
            tags: [...refreshed.data.tags],
            metadata_revision: refreshed.data.metadata_revision,
          })
        })

        return { status: 'saved', metadataRevision }
      }

      if (result.error.errorCode !== 'metadata_revision_conflict') {
        return { status: 'error', error: result.error }
      }

      refreshMetadataViews(id)
      const refreshed = await client.getLink(id)
      if (!client.isIdentityCurrent()) {
        return {
          status: 'error',
          error: { kind: 'identity-mismatch', message: 'Reader identity changed' },
        }
      }
      if (!refreshed.ok) {
        return {
          status: 'error',
          error: {
            ...refreshed.error,
            message: `链接信息已变化，但无法读取最新版本：${refreshed.error.message}`,
          },
        }
      }
      if (
        refreshed.data.id !== id ||
        refreshed.data.metadata_revision <= revision
      ) {
        return {
          status: 'error',
          error: {
            kind: 'other',
            status: result.error.status,
            errorCode: result.error.errorCode,
            message: '链接信息已变化，但读取的最新版本无效，请刷新后重试',
          },
        }
      }

      patchKnownLink(id, {
        title: refreshed.data.title,
        summary: refreshed.data.summary,
        tags: [...refreshed.data.tags],
        metadata_revision: refreshed.data.metadata_revision,
      })
      return {
        status: 'conflict',
        metadataRevision: refreshed.data.metadata_revision,
        error: result.error,
      }
    },
    [client, patchKnownLink, refreshMetadataViews],
  )

  useEffect(() => {
    const linkId = savedDocument?.id.linkId
    const documentRevision = savedDocument?.id.contentRevision
    if (
      !linkId ||
      documentRevision === undefined ||
      activeProjection?.id !== linkId ||
      documentRevision <= (activeProjection.content_revision ?? 0)
    ) {
      return
    }

    noteContentRevision(linkId, documentRevision)
    let cancelled = false
    void client.getLink(linkId).then((result) => {
      if (cancelled || !client.isIdentityCurrent() || !result.ok) return
      const responseRevision = contentRevisionOrUndefined(result.data.content_revision)
      if (
        result.data.id !== linkId ||
        responseRevision === undefined ||
        responseRevision < documentRevision
      ) {
        return
      }
      patchKnownLink(linkId, result.data)
    })
    return () => {
      cancelled = true
    }
  }, [
    activeProjection?.content_revision,
    activeProjection?.id,
    client,
    noteContentRevision,
    patchKnownLink,
    savedDocument?.id.contentRevision,
    savedDocument?.id.linkId,
  ])

  const {
    counts,
    tags: tagStatList,
    domains: domainStatList,
    tagsAvailable,
    domainsAvailable,
  } = useSidebarData(
    corpus,
    tagsData.tags,
    domainData.summaries,
    domainData.total,
    {
      links: list.links,
      total: list.authoritativeTotal,
      complete: list.corpusComplete,
    },
  )
  const annotationCount = useAnnotatedLinkCount(client)
  const sidebarCounts = useMemo(() => ({
    ...counts,
    annotated: annotationCount,
  }), [annotationCount, counts])

  const openLink = useCallback(
    (id: string, candidate?: LinkResponse, revealMobileDetail = true, options: OpenLinkOptions = {}) => {
      const normalizedID = id.trim()
      if (!normalizedID) return
      const commitOpen = () => {
        if (!client.isIdentityCurrent() || !capabilityLease.isCurrent()) return false
        const historyMode = options.history ?? 'push'
        const addressLink = options.address ?? historyMode === 'push'
        commitRoute(
          { kind: 'library', id: 'reading' },
          { linkId: normalizedID },
          historyMode,
          addressLink,
        )
        // The route commit owns both the address and the state update. The
        // detail loader below owns the same target, so it must not be replayed
        // by the location-target effect on the next render.
        pendingLinkTarget.current = null
        // Every navigation invalidates an earlier detail request, including the
        // fast path that can render a settled list projection synchronously.
        const requestSeq = ++detailRequestSeq.current
        const knownLinks = knownLinksRef.current
        const candidateLink =
          candidate ??
          knownLinks.list.find((item) => item.id === normalizedID) ??
          knownLinks.corpus.find((item) => item.id === normalizedID)
        const link = candidateLink ? acceptMetadataLink(candidateLink) : undefined
        if (link) {
          setActiveFallback(link)
          setCorpus((current) => mergeCorpus(current, [link]))
        }
        setActiveDetail(null)
        if (revealMobileDetail) setMobilePane('detail')
        setMobileNavOpen(false)

        // PF6：列表如今如实汇报 has_content 与两项计数，LinkResponse 的其余字段
        // 列表投影也都覆盖了。因此**手上已有这条链接的列表数据时，不必再发一次
        // 详情请求**——直接用它渲染，冷启动少一个 API、点击已加载的链接少一个。
        //
        // 仍然发请求的两种情况：这条链接不在当前列表里（从 ⌘K 或站点页跳进来），
        // 或者它还在解析中（pending/processing 的字段会变，值得一次权威读取）。
        const known = link ?? knownLinks.list.find((item) => item.id === normalizedID)
        const settled = known && known.status !== 'pending' && known.status !== 'processing'
        if (known && settled) {
          setActiveDetail(acceptMetadataLink(known))
          if (!known.has_content) {
            setDetailRequest(null)
            return true
          }

          // content_source is intentionally absent from list projections. Hydrate
          // only the detail metadata here; the body endpoint remains deferred until
          // the user expands the saved-original section.
          setDetailRequest({ id: normalizedID, sequence: requestSeq, status: 'loading' })
          void client.getLink(normalizedID).then((res) => {
            if (!client.isIdentityCurrent() || requestSeq !== detailRequestSeq.current) return
            if (res.ok) {
              const accepted = acceptMetadataLink(res.data)
              setDetailRequest(null)
              setActiveDetail(accepted)
              patchKnownLink(normalizedID, accepted)
              return
            }
            setDetailRequest({
              id: normalizedID,
              sequence: requestSeq,
              status: 'error',
              error: res.error,
            })
          })
          return true
        }

        setDetailRequest({ id: normalizedID, sequence: requestSeq, status: 'loading' })
        void client.getLink(normalizedID).then((res) => {
          if (!client.isIdentityCurrent()) return
          if (requestSeq !== detailRequestSeq.current) return
          if (res.ok) {
            const accepted = acceptMetadataLink(res.data)
            setDetailRequest(null)
            setActiveDetail(accepted)
            patchKnownLink(normalizedID, accepted)
            return
          }
          setDetailRequest({
            id: normalizedID,
            sequence: requestSeq,
            status: 'error',
            error: res.error,
          })
        })
        return true
      }

      if (normalizedID === activeId || options.guard === false) return commitOpen()
      const allowed = confirmDiscardNavigation()
      if (typeof (allowed as Promise<boolean>)?.then === 'function') {
        return Promise.resolve(allowed).then((ready) => ready ? commitOpen() : false)
      }
      return allowed ? commitOpen() : false
    },
    [
      acceptMetadataLink,
      activeId,
      capabilityLease,
      client,
      commitRoute,
      confirmDiscardNavigation,
      patchKnownLink,
      pendingLinkTarget,
      setMobileNavOpen,
      setMobilePane,
    ],
  )

  useEffect(() => {
    const target = pendingLinkTarget.current
    if (!target || list.loading) return
    const candidate = protectedListLinks.find((link) => link.id === target)
    openLink(target, candidate, false, { history: 'none', address: true, guard: false })
  }, [pendingLinkTarget, protectedListLinks, list.loading, openLink])

  useEffect(() => {
    if (activeId) {
      automaticOpenRef.current = null
      return
    }
    if (view !== 'reading' || list.loading || protectedListLinks.length === 0) return
    const first = protectedListLinks[0]
    if (automaticOpenRef.current === first.id) return
    automaticOpenRef.current = first.id
    openLink(first.id, first, false, { history: 'none', address: false, guard: false })
  }, [activeId, protectedListLinks, list.loading, openLink, view])

  const clearActiveResource = useCallback(() => {
    setActiveId(null)
    setActiveDetail(null)
    setActiveFallback(null)
  }, [setActiveId])

  const getActiveLink = useCallback(() => activeRef.current, [])

  const onDeleteLink = useCallback(() => {
    const target = activeRef.current
    if (!target) return
    if (!confirmDiscardContentEdit()) return
    const { id, title, url } = target
    void (async () => {
      const result = await client.deleteLink(id)
      if (!result.ok) {
        flash(`删除失败：${result.error.message}`, 'alert')
        return
      }
      invalidateLibrary()
      invalidateLink(id)
      clearActiveResource()
      void list.reload()
      flash(`已删除「${title || url}」`, 'trash', {
        label: '撤销',
        onAction: () => {
          dismissToast()
          void (async () => {
            const restored = await client.restoreLink(id)
            if (!restored.ok) {
              flash(`撤销失败：${restored.error.message}`, 'alert')
              return
            }
            invalidateLibrary()
            invalidateLink(id)
            void list.reload()
            flash('已恢复', 'check')
          })()
        },
      })
    })()
  }, [clearActiveResource, client, confirmDiscardContentEdit, dismissToast, flash, list])

  const activeListIndex = activeId
    ? protectedListLinks.findIndex((link) => link.id === activeId)
    : -1
  const previousLink = activeListIndex > 0 ? protectedListLinks[activeListIndex - 1] : null
  const nextLink =
    activeListIndex >= 0 && activeListIndex < protectedListLinks.length - 1
      ? protectedListLinks[activeListIndex + 1]
      : null

  const prefetchTargets = useMemo<PrefetchTarget[]>(() => {
    const targets: PrefetchTarget[] = []
    for (const candidate of [nextLink, previousLink]) {
      if (!candidate) continue
      const key = translationsKey(candidate.id, candidate.content_revision)
      targets.push({
        key,
        load: () =>
          resourceStore.fetch(key, () =>
            client.getTranslations(candidate.id),
          ),
      })
    }
    return targets
  }, [client, nextLink, previousLink])
  usePrefetch(prefetchTargets)

  const previousPager = useMemo(
    () =>
      previousLink
        ? { title: pagerTitle(previousLink), onSelect: () => openLink(previousLink.id, previousLink) }
        : null,
    [previousLink, openLink],
  )

  const nextPager = useMemo(
    () =>
      nextLink
        ? { title: pagerTitle(nextLink), onSelect: () => openLink(nextLink.id, nextLink) }
        : null,
    [nextLink, openLink],
  )

  return {
    selection,
    setSelection,
    list,
    tagsData,
    domainData,
    reloadLinks: list.reload,
    reloadTags: tagsData.reload,
    reloadDomains: domainData.reload,
    protectedListLinks,
    corpus,
    activeProjection,
    active,
    renderedActive,
    aiContentContext,
    savedArticle,
    savedDocument,
    captureSavedDocumentContext: savedArticle.captureContext,
    loadSavedDocumentBody: savedArticle.loadBody,
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
  }
}
