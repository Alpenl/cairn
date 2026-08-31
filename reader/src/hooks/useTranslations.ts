import { useCallback, useEffect, useMemo, useRef } from 'react'
import type {
  TranslationCreateRequest,
  TranslationListResponse,
  TranslationResponse,
} from '../lib/api/types'
import type { ReaderLibrarySitesPort } from '../lib/reader-api-ports'
import { err, type ApiError, type ApiResult } from '@webtag/api'
import { translationsKey } from '../lib/cache/keys'
import { resourceStore } from '../lib/cache/store'
import { useCachedResource } from '../lib/cache/useCachedResource'
import { isSavedContentTranslationSource } from '../lib/article/document'
import { usePolling } from './usePolling'

const TRANSLATION_POLL_MS = 1200

/**
 * 译文缓存键的前缀。
 *
 * 它**刻意不落在 `GET /api/links` 之下**。曾经是 `'GET /api/links/'`，而
 * `LINKS_CACHE_PREFIX` 是 `'GET /api/links'`（无尾随分隔符）——前者是后者的
 * 前缀，于是 `invalidateLibrary()` 会连坐把**全库每一篇的译文**一起打掉，而它
 * 在「添加链接」「刷新链接」「转为网站」这些日常动作上都会触发。
 *
 * 连坐的后果**当时**不是「多取一次」：条目被删之后 `useCachedResource` 会停在
 * 永久 loading，已译好的内容从界面消失，而 `hasActiveJobs` 从缓存推导，进行中的
 * 翻译任务轮询也一并停掉。（那条永久 loading 已由 `useCachedResource` 的「失效后
 * 自动重取」兜住，见那边；但独立命名空间仍然要留着——被打掉再自愈是一次白白的
 * 全库重取。）
 *
 * 命名空间的写法与 `CONTENT_CACHE_PREFIX` 对齐（`cache/keys.ts`），两者
 * 是同一条教训的两次应用。
 */
export {

  translationsKey,
} from '../lib/cache/keys'

const NO_TRANSLATIONS: TranslationResponse[] = []

export interface TranslationSourceIdentity {
  /** Null means the link has no saved-content generation. */
  contentRevision?: number | null
  /** Hash of the canonical rendered summary block. */
  summarySourceHash?: string | null
}

interface TranslationProjection {
  current: TranslationResponse[]
  stale: TranslationResponse[]
  currentContentRevision: number | null
}

function contentRevisionOrNull(value: unknown): number | null {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
    ? value
    : null
}

function projectTranslations(
  response: TranslationListResponse | undefined,
  expectedContentRevision?: number | null,
  expectedSummaryHash?: string | null,
): TranslationProjection {
  if (!response) {
    return {
      current: NO_TRANSLATIONS,
      stale: NO_TRANSLATIONS,
      currentContentRevision: null,
    }
  }
  const currentContentRevision = contentRevisionOrNull(
    response.current_content_revision,
  )
  const savedEnvelopeMatchesActive =
    expectedContentRevision === undefined ||
    (contentRevisionOrNull(expectedContentRevision) === currentContentRevision &&
      currentContentRevision !== null)
  const authoritativeSummaryHash = response.current_summary_source_hash
  const current: TranslationResponse[] = []
  const stale: TranslationResponse[] = []
  for (const item of response.items) {
    if (isSavedContentTranslationSource(item.scope, item.block_key)) {
      if (item.source_content_revision == null) {
        continue
      } else if (
        !savedEnvelopeMatchesActive ||
        currentContentRevision === null ||
        currentContentRevision === 0 ||
        item.stale ||
        item.source_content_revision !== currentContentRevision
      ) {
        stale.push(item)
      } else {
        current.push(item)
      }
      continue
    }
    // Summary is the only independently versioned source block exposed by Reader.
    if (item.block_key !== 'summary') {
      continue
    } else if (item.stale) {
      stale.push(item)
    } else if (
      typeof expectedSummaryHash !== 'string' ||
      authoritativeSummaryHash !== expectedSummaryHash
    ) {
      stale.push(item)
    } else {
      current.push(item)
    }
  }
  return { current, stale, currentContentRevision }
}

function mergeTranslation(
  items: TranslationResponse[],
  incoming: TranslationResponse,
): TranslationResponse[] {
  return [incoming, ...items.filter((item) => item.id !== incoming.id)]
}

export interface UseTranslationsResult {
  /** Safe for current-document rendering; saved rows are revision-verified. */
  items: TranslationResponse[]
  staleItems: TranslationResponse[]
  currentContentRevision: number | null
  loading: boolean
  error: ApiError | null
  hasActiveJobs: boolean
  reload: () => Promise<void>
  reloadAfterSourceChange: (
    expectedLinkId: string,
    options?: { reload?: boolean },
  ) => Promise<void>
  create: (
    request: TranslationCommandRequest,
  ) => Promise<ApiResult<TranslationResponse>>
}

export type TranslationCommandRequest = Omit<
  TranslationCreateRequest,
  'expected_content_revision' | 'expected_source_hash'
>

type TranslationsClient = Pick<
  ReaderLibrarySitesPort,
  'getTranslations' | 'createTranslation' | 'isIdentityCurrent'
>

function buildTranslationRequest(
  command: TranslationCommandRequest,
  identity: TranslationSourceIdentity | undefined,
): ApiResult<TranslationCreateRequest> {
  const request: TranslationCreateRequest = { ...command }

  const savedSource = isSavedContentTranslationSource(request.scope, request.block_key)
  if (savedSource) {
    if (
      typeof identity?.contentRevision !== 'number' ||
      !Number.isInteger(identity.contentRevision) ||
      identity.contentRevision <= 0
    ) {
      return err({ kind: 'other', message: '后端未返回正文版本，无法安全创建翻译' })
    }
    request.expected_content_revision = identity.contentRevision
  }

  if (request.block_key === 'summary') {
    if (!identity?.summarySourceHash) {
      return err({ kind: 'other', message: '摘要来源尚未验证，请刷新后重试' })
    }
    request.expected_source_hash = identity.summarySourceHash
  }

  if (
    request.scope === 'selection' &&
    request.block_key !== 'content' &&
    request.block_key !== 'content-document' &&
    request.block_key !== 'summary'
  ) {
    return err({ kind: 'other', message: '该内容块暂不支持翻译' })
  }
  return { ok: true, data: request }
}

/**
 * 某条链接的译文列表。
 *
 * PF3 起走共享缓存：文章 A→B→A 往返时译文不再重新请求；活跃任务的 1.2 秒
 * 轮询在数据未变时也不再产生任何重渲染（store 只在内容真的变了才通知）。
 */
export function useTranslations(
  client: TranslationsClient,
  linkId: string | null | undefined,
  sourceIdentity?: TranslationSourceIdentity,
): UseTranslationsResult {
  const expectedContentRevision = sourceIdentity?.contentRevision
  const expectedSummarySourceHash = sourceIdentity?.summarySourceHash
  const key = linkId ? translationsKey(linkId, expectedContentRevision) : null
  const resourceEnabled = key !== null
  // 当前链接的 ref。回调可能被调用方在切换文章之后才触发（它捕获的是**旧的**
  // linkId），只比闭包里那个值会让一次针对旧文章的重载打到已经切走的界面上。
  const currentLinkId = useRef(linkId)
  currentLinkId.current = linkId
  const sourceIdentityRef = useRef(sourceIdentity)
  sourceIdentityRef.current = sourceIdentity
  const resource = useCachedResource<TranslationListResponse>(
    key,
    () => client.getTranslations(linkId as string),
    { enabled: resourceEnabled },
  )

  const projection = useMemo(
    () =>
      projectTranslations(
        resource.data,
        expectedContentRevision,
        expectedSummarySourceHash,
      ),
    [expectedContentRevision, expectedSummarySourceHash, resource.data],
  )
  const hasActiveJobs = useMemo(
    () =>
      projection.current.some(
        (item) => item.status === 'pending' || item.status === 'processing',
      ),
    [projection],
  )

  // 有任务在跑时轮询。silent：轮询失败不该把译文区变成错误块。
  usePolling(
    useCallback(() => {
      void resource.reload({ silent: true })
    }, [resource]),
    hasActiveJobs && key !== null ? TRANSLATION_POLL_MS : null,
  )

  const reload = useCallback(async (): Promise<void> => {
    await resource.reload()
  }, [resource])

  const observedSourceIdentity = useRef({
    key,
    linkId,
    contentRevision: sourceIdentity?.contentRevision,
    summarySourceHash: sourceIdentity?.summarySourceHash,
  })
  useEffect(() => {
    const previous = observedSourceIdentity.current
    const current = {
      key,
      linkId,
      contentRevision: sourceIdentity?.contentRevision,
      summarySourceHash: sourceIdentity?.summarySourceHash,
    }
    observedSourceIdentity.current = current
    // A cache-generation change already schedules its initial request in
    // useCachedResource. Let that request own the new key instead of forcing a
    // duplicate fetch from this source-identity effect.
    if (previous.key !== key || previous.linkId !== linkId || !linkId) return
    if (
      previous.contentRevision === current.contentRevision &&
      previous.summarySourceHash === current.summarySourceHash
    ) {
      return
    }
    if (typeof current.summarySourceHash !== 'string') return
    // With no renderable entry, useCachedResource's generation effect owns the
    // first request. Force is only needed to fence an identity-era cache/inflight.
    if (!key || !resourceStore.has(key)) return
    const cached = resourceStore.peek<TranslationListResponse>(key).data
    if (cached?.current_summary_source_hash === current.summarySourceHash) return
    // Fence an in-flight request observed under the old source identity.
    void resource.reload({ silent: true, force: true })
  }, [key, linkId, resource, sourceIdentity?.contentRevision, sourceIdentity?.summarySourceHash])

  const create = useCallback(
    async (command: TranslationCommandRequest): Promise<ApiResult<TranslationResponse>> => {
      if (!linkId) {
        return err({ kind: 'other', message: '请先选择一篇内容' })
      }
      const observedIdentity = sourceIdentityRef.current
        ? { ...sourceIdentityRef.current }
        : undefined
      const built = buildTranslationRequest(command, observedIdentity)
      if (!built.ok) return built
      const request = built.data
      const response = await client.createTranslation(linkId, request)
      if (response.ok && client.isIdentityCurrent()) {
        const currentIdentity = sourceIdentityRef.current
        const savedSource = isSavedContentTranslationSource(request.scope, request.block_key)
        const summarySource = request.block_key === 'summary'
        const observedContentRevision = observedIdentity?.contentRevision
        const observedSummarySourceHash = observedIdentity?.summarySourceHash
        const sourceStillCurrent = savedSource
          ? typeof observedContentRevision === 'number' &&
            request.expected_content_revision === observedContentRevision &&
            currentIdentity?.contentRevision === observedContentRevision &&
            response.data.source_content_revision === observedContentRevision
          : summarySource
            ? typeof observedSummarySourceHash === 'string' &&
              currentIdentity?.summarySourceHash === observedSummarySourceHash &&
              request.expected_source_hash === observedSummarySourceHash
            : true
        if (!sourceStillCurrent) {
          // The successful response belongs to a source that stopped being
          // current while the request was in flight. Never patch it into the
          // active envelope; re-read the authoritative list so a concurrent
          // writer is still reflected after this continuation is fenced out.
          void resource.reload({ silent: true, force: true })
          return err({
            kind: 'identity-mismatch',
            message: '翻译来源已更新，请重新确认后再试',
          })
        }
        const cacheKey = key
        if (!cacheKey) return response
        const cached = resourceStore.peek<TranslationListResponse>(cacheKey).data
        const incomingRevision = response.data.source_content_revision
        const matchesEnvelope = incomingRevision === null
          ? request.block_key !== 'summary' ||
            (sourceIdentityRef.current?.summarySourceHash !== null &&
              sourceIdentityRef.current?.summarySourceHash !== undefined &&
              cached?.current_summary_source_hash ===
                sourceIdentityRef.current.summarySourceHash)
          : cached !== undefined && incomingRevision === cached.current_content_revision
        // A successful saved-content CAS proves the response's own revision,
        // but cannot rewrite a list envelope fetched for another generation.
        if (matchesEnvelope) {
          resourceStore.patch<TranslationListResponse>(cacheKey, (current) => ({
            ...current,
            items: mergeTranslation(current.items, response.data),
          }))
        }
        if (!cached || !matchesEnvelope) {
          void resource.reload({ silent: true })
        }
      }
      return response
    },
    [client, key, linkId, resource],
  )

  const reloadAfterSourceChange = useCallback(
    async (
      expectedLinkId: string,
      options: { reload?: boolean } = {},
    ): Promise<void> => {
      if (!client.isIdentityCurrent()) return
      // 两侧都要匹配：闭包捕获的 linkId 说明"这个回调属于哪篇文章"，
      // ref 说明"用户现在停在哪篇文章"。任一不符都说明这次重载已经过期。
      if (!expectedLinkId || linkId !== expectedLinkId) return
      if (currentLinkId.current !== expectedLinkId) return
      // 原文换了，全文译文相对新原文即为过期。先就地标记让界面立刻反映，
      // 再拉一次拿后端的权威状态。
      if (!key) return
      resourceStore.patch<TranslationListResponse>(key, (current) => ({
        ...current,
        items: current.items.map((item) =>
          item.scope === 'full' ? { ...item, stale: true } : item,
        ),
      }))
      if (options.reload === false) return
      await resource.reload()
    },
    [client, key, linkId, resource],
  )

  return {
    items: projection.current,
    staleItems: projection.stale,
    currentContentRevision: projection.currentContentRevision,
    loading: resourceEnabled && resource.loading,
    error: resource.error,
    hasActiveJobs,
    reload,
    reloadAfterSourceChange,
    create,
  }
}
