import {
  buildFeedItemsQuery,
  buildLinksQuery,
  type ListLinksParams,
  type ReaderActivityKind,
} from '../api/client'
import type { ListFeedItemsParams, ListSitesParams } from '../api/types'
import type { IdentityContext } from '../identity'

export type LinksCacheSelection = Readonly<{
  type: 'smart' | 'tag' | 'domain'
  id: string
}>

export const LINKS_CACHE_PREFIX = 'GET /api/links'
export const ANNOTATED_LINKS_CACHE_KEY = `${LINKS_CACHE_PREFIX}#smart:annotated`

/**
 * 已保存原文缓存键的前缀。
 *
 * 它**刻意不落在 `GET /api/links` 之下**。曾经是 `GET /api/links/{id}/content`，
 * 于是 invalidateLibrary 的前缀 `GET /api/links` 会连带把**全库每一篇的正文**
 * 一起打掉——而 invalidateLibrary 在「添加链接」这种日常动作上就会触发。用户
 * 感受到的是：读过的文章过一会儿又要重新下载一遍。
 *
 * 列表 / 详情该被写操作失效，正文不该。
 */
export const CONTENT_CACHE_PREFIX = 'GET content:/api/links'
export const TRANSLATIONS_CACHE_PREFIX = 'GET translations:/api/links'

export const TAGS_CACHE_PREFIX = 'GET /api/tags'
export const TAGS_CACHE_KEY = `${TAGS_CACHE_PREFIX}?library_kind=reading`

export const DOMAIN_SUMMARIES_CACHE_PREFIX = 'GET /api/tree?view=domains'
export const DOMAIN_SUMMARIES_CACHE_KEY = `${DOMAIN_SUMMARIES_CACHE_PREFIX}&library_kind=reading`

export const FEED_ITEMS_CACHE_PREFIX = 'GET /api/feed-items'
export const SUBSCRIPTIONS_CACHE_KEY = 'GET /api/subscriptions'

export const SITES_CACHE_PREFIX = 'GET /api/sites'

export const READER_RELATED_TAGS_CACHE_PREFIX = 'GET /api/reader/related-tags'
export const READER_ACTIVITY_CACHE_PREFIX = 'GET /api/reader/activity'

export const PERSISTED_CACHE_PREFIXES = [
  LINKS_CACHE_PREFIX,
  CONTENT_CACHE_PREFIX,
  TRANSLATIONS_CACHE_PREFIX,
  TAGS_CACHE_PREFIX,
  DOMAIN_SUMMARIES_CACHE_PREFIX,
  SUBSCRIPTIONS_CACHE_KEY,
  SITES_CACHE_PREFIX,
] as const

function contentRevisionOrNull(value: unknown): number | null {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
    ? value
    : null
}

export function linkDetailCacheKey(linkId: string): string {
  return `${LINKS_CACHE_PREFIX}/${encodeURIComponent(linkId)}?include_content=false`
}

export function linksFirstPageCacheKey(
  selection: LinksCacheSelection,
  params: ListLinksParams,
): string {
  if (selection.type === 'smart' && selection.id === 'annotated') {
    return ANNOTATED_LINKS_CACHE_KEY
  }
  return `${LINKS_CACHE_PREFIX}${buildLinksQuery(params)}#${selection.type}:${selection.id}`
}

/**
 * 已保存原文的缓存键。
 *
 * 键里带 content_revision，而后端会在同一 SQL/transaction 中为每次可见正文
 * 变化推进它：保存、替换 / 重新抓取、requeue/site-complete clear、转换与历史
 * 迁移都在此列。所以正文内容或存在性一变就是另一个键，旧内容不会被当成新代次
 * 命中——这是 `loadLinkContent` 敢命中即返回、不发校验的地基。
 *
 * `has_content` 闸门仍不能据此删除：revision 负责缓存身份，has_content 负责当前
 * 正文存在性；而 MainView 的 link/content state 在 RF5B 原子 document reducer
 * 落地前仍可能短暂来自不同响应。DetailPane 必须优先服从服务端的 false，不能让
 * link-scoped 的旧 `content` / `loadedContent` 穿过。
 *
 * ⚠ 这条不变量是**后端保证**的，不是前端能自证的。它由
 * `TestContentWritesBumpContentRevision` 与 RF5A 的
 * `TestSavedContentGenerationMatrix` 钉住。前者捕获保存/替换 CAS 的漏递增、错误步长、
 * 无关 updated_at 变化、**把递增从 CAS 语句里拆出去**以及 RETURNING 漂移；后者
 * 覆盖 clear、conversion、historical migration、失败回滚与幂等路径。谁要改这些
 * SQL，先看这两个真实 PostgreSQL 测试。
 *
 * 历史：这句话曾经是**假的**，而当时的注释照样这么写。后端那时只在置空 content 时
 * 才递增，于是「重新抓取」之后键不变、缓存不校验，另一台设备会永久停在替换前的
 * 正文上。别把这段历史删掉——它解释了为什么这里要指名一个后端测试。
 */
export function contentCacheKey(
  linkId: string,
  revision: number | undefined,
): string {
  return `${CONTENT_CACHE_PREFIX}/${encodeURIComponent(linkId)}?rev=${revision ?? 0}`
}

export function linkContentInvalidationPrefix(linkId: string): string {
  return `${CONTENT_CACHE_PREFIX}/${encodeURIComponent(linkId)}?`
}

export function translationsKey(
  linkId: string,
  contentRevision?: number | null,
): string {
  const revision = contentRevisionOrNull(contentRevision)
  return `${TRANSLATIONS_CACHE_PREFIX}/${encodeURIComponent(linkId)}/translations?rev=${revision ?? 'unverified'}`
}

export function linkTranslationsInvalidationPrefix(linkId: string): string {
  return `${TRANSLATIONS_CACHE_PREFIX}/${encodeURIComponent(linkId)}/`
}

export function feedItemsCacheKey(params: ListFeedItemsParams): string {
  return `${FEED_ITEMS_CACHE_PREFIX}${buildFeedItemsQuery(params)}`
}

export function sitesCacheKey(params: ListSitesParams): string {
  const query = new URLSearchParams()
  if (params.view) query.set('view', params.view)
  if (params.tags?.trim()) query.set('tags', params.tags.trim())
  if (params.recentCutoff?.trim()) query.set('recent_cutoff', params.recentCutoff.trim())
  if (params.page && params.page > 1) query.set('page', String(params.page))
  if (params.limit && params.limit > 0) query.set('limit', String(params.limit))
  return SITES_CACHE_PREFIX + (query.size ? `?${query}` : '')
}

export function sitesPageCacheKey(
  params: ListSitesParams,
  capabilityGeneration: number,
): string {
  return `${sitesCacheKey(params)}#capability=${capabilityGeneration}`
}

export function readerIdentityCacheSuffix(
  context: IdentityContext | null | undefined,
): string {
  if (!context) return 'unscoped'
  return [
    context.serverClientDataNamespace,
    context.physicalNamespace,
    String(context.localEpoch),
  ].map((part) => encodeURIComponent(part)).join(':')
}

export function identityScopedCacheKey(
  baseKey: string,
  context: IdentityContext | null | undefined,
): string {
  return `${baseKey}#${readerIdentityCacheSuffix(context)}`
}

export function readerRelatedTagsCacheKey(
  linkId: string,
  context: IdentityContext | null | undefined,
  limit: number,
): string {
  return identityScopedCacheKey(
    `${READER_RELATED_TAGS_CACHE_PREFIX}?link_id=${encodeURIComponent(linkId)}&limit=${limit}`,
    context,
  )
}

export function readerActivityCacheKey(
  kind: ReaderActivityKind,
  context: IdentityContext | null | undefined,
  limit: number,
): string {
  return identityScopedCacheKey(
    `${READER_ACTIVITY_CACHE_PREFIX}?kind=${kind}&limit=${limit}`,
    context,
  )
}
