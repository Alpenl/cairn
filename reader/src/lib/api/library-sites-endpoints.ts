import {
  buildLinksQuery,
  type ApiResult,
  err,
  ok,
} from '@webtag/api'
import {
  buildReaderQuery,
  readerLimit,
  type ReaderEndpointTransport,
} from './endpoint-helpers'
import {
  isConversionExecuteResponse,
  isConversionPreviewResponse,
  isDomainTreeSummaryEnvelope,
  isGroupedSearchResponse,
  isLinkContentResponse,
  isLinkResponse,
  isPaginatedLinks,
  isPaginatedSites,
  isReaderActivityResponse,
  isReaderEngagementResponse,
  isReaderLinkMetadataResponse,
  isReaderRelatedTagsResponse,
  isSiteDetail,
  isSiteEntryDeleteResponse,
  isSiteMergeExecuteResponse,
  isSiteMergePreviewResponse,
  isSiteSplitExecuteResponse,
  isSiteSplitPreviewResponse,
  isSubmitResponse,
  isTagCountArray,
  isTranslationListResponse,
  isTranslationResponse,
} from './guards'
import { shapeMismatch, type ReaderReadOptions, type ReaderRequestOptions } from './transport'
import type {
  ContentEditRequest,
  ConversionExecuteRequest,
  ConversionExecuteResponse,
  ConversionPreviewRequest,
  ConversionPreviewResponse,
  DomainTreeSummaryEnvelope,
  GroupedSearchResponse,
  LinkContentResponse,
  LinkCreateRequest,
  LinkResponse,
  ListLinksParams,
  ListSitesParams,
  PaginatedLinksResponse,
  PaginatedSitesResponse,
  ReaderActivityResponse,
  ReaderEngagementRequest,
  ReaderEngagementResponse,
  ReaderLinkMetadataRequest,
  ReaderLinkMetadataResponse,
  ReaderRelatedTagsResponse,
  SiteDetailResponse,
  SiteEntryDeleteResponse,
  SiteEntryUpdateRequest,
  SiteMergeExecuteRequest,
  SiteMergeExecuteResponse,
  SiteMergePreviewRequest,
  SiteMergePreviewResponse,
  SiteSplitExecuteResponse,
  SiteSplitPreviewResponse,
  SiteSplitRequest,
  SiteUpdateRequest,
  SubmitResponse,
  TagCountResponse,
  TranslationCreateRequest,
  TranslationListResponse,
  TranslationResponse,
} from './types'
import type { ReaderActivityRequestOptions } from './client'

export async function getLinks(
  transport: ReaderEndpointTransport,
  params: ListLinksParams = {},
  options?: ReaderReadOptions,
): Promise<ApiResult<PaginatedLinksResponse>> {
  const r = await transport.send('GET', `/api/links${buildLinksQuery(params)}`, {
    readOptions: options,
    rawJSONContract: 'link-metadata-revision',
  })
  if (!r.ok) return r
  if (!isPaginatedLinks(r.data)) return shapeMismatch('PaginatedLinksResponse')
  return ok(r.data)
}

export async function submitLink(
  transport: ReaderEndpointTransport,
  request: LinkCreateRequest,
): Promise<ApiResult<SubmitResponse>> {
  const r = await transport.send('POST', '/api/links', { body: request })
  if (!r.ok) return r
  if (!isSubmitResponse(r.data)) return shapeMismatch('SubmitResponse')
  return ok(r.data)
}

export async function searchLibrary(
  transport: ReaderEndpointTransport,
  q: string,
  readingLimit = 10,
  siteLimit = 10,
  thoughtLimit = 20,
  thoughtAfter = '',
): Promise<ApiResult<GroupedSearchResponse>> {
  const query = new URLSearchParams({
    q: q.trim(),
    reading_limit: String(readingLimit),
    site_limit: String(siteLimit),
    thought_limit: String(thoughtLimit),
  })
  if (thoughtAfter.trim()) query.set('thought_after', thoughtAfter)
  const r = await transport.send('GET', `/api/search?${query}`, {
    rawJSONContract: 'link-metadata-revision',
  })
  if (!r.ok) return r
  return isGroupedSearchResponse(r.data) ? ok(r.data) : shapeMismatch('GroupedSearchResponse')
}

export async function getLink(
  transport: ReaderEndpointTransport,
  id: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<LinkResponse>> {
  const r = await transport.send('GET', `/api/links/${encodeURIComponent(id)}?include_content=false`, {
    signal: options.signal,
    rawJSONContract: 'link-metadata-revision',
  })
  if (!r.ok) return r
  if (!isLinkResponse(r.data)) return shapeMismatch('LinkResponse')
  return ok(r.data)
}

export async function getContent(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<LinkContentResponse>> {
  const r = await transport.send('GET', `/api/links/${encodeURIComponent(id)}/content`)
  if (!r.ok) return r
  if (!isLinkContentResponse(r.data)) return shapeMismatch('LinkContentResponse')
  return ok(r.data)
}

export async function editContent(
  transport: ReaderEndpointTransport,
  id: string,
  request: ContentEditRequest,
): Promise<ApiResult<LinkContentResponse>> {
  const r = await transport.send('PATCH', `/api/links/${encodeURIComponent(id)}/content`, {
    body: request,
  })
  if (!r.ok) return r
  if (!isLinkContentResponse(r.data)) return shapeMismatch('LinkContentResponse')
  return ok(r.data)
}

export async function getSites(
  transport: ReaderEndpointTransport,
  params: ListSitesParams = {},
): Promise<ApiResult<PaginatedSitesResponse>> {
  const query = new URLSearchParams()
  if (params.view) query.set('view', params.view)
  if (params.tags?.trim()) query.set('tags', params.tags.trim())
  if (params.recentCutoff?.trim()) query.set('recent_cutoff', params.recentCutoff.trim())
  if (params.page && params.page > 1) query.set('page', String(params.page))
  if (params.limit && params.limit > 0) query.set('limit', String(params.limit))
  const suffix = query.size ? `?${query}` : ''
  const r = await transport.send('GET', `/api/sites${suffix}`)
  if (!r.ok) return r
  return isPaginatedSites(r.data) ? ok(r.data) : shapeMismatch('PaginatedSitesResponse')
}

export async function getSite(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<SiteDetailResponse>> {
  const r = await transport.send('GET', `/api/sites/${encodeURIComponent(id)}`)
  if (!r.ok) return r
  return isSiteDetail(r.data) ? ok(r.data) : shapeMismatch('SiteDetailResponse')
}

export async function updateSite(
  transport: ReaderEndpointTransport,
  id: string,
  revision: number,
  patch: SiteUpdateRequest,
): Promise<ApiResult<SiteDetailResponse>> {
  const r = await transport.send('PATCH', `/api/sites/${encodeURIComponent(id)}`, {
    body: patch,
    headers: { 'If-Match': `"${revision}"` },
  })
  if (!r.ok) return r
  return isSiteDetail(r.data) ? ok(r.data) : shapeMismatch('SiteDetailResponse')
}

export async function updateSiteEntry(
  transport: ReaderEndpointTransport,
  siteID: string,
  entryID: string,
  revision: number,
  patch: SiteEntryUpdateRequest,
): Promise<ApiResult<SiteDetailResponse>> {
  const r = await transport.send('PATCH', `/api/sites/${encodeURIComponent(siteID)}/entries/${encodeURIComponent(entryID)}`, {
    body: patch,
    headers: { 'If-Match': `"${revision}"` },
  })
  if (!r.ok) return r
  return isSiteDetail(r.data) ? ok(r.data) : shapeMismatch('SiteDetailResponse')
}

export async function setPrimarySiteEntry(
  transport: ReaderEndpointTransport,
  siteID: string,
  entryID: string,
  revision: number,
): Promise<ApiResult<SiteDetailResponse>> {
  const r = await transport.send('POST', `/api/sites/${encodeURIComponent(siteID)}/entries/${encodeURIComponent(entryID)}/set-primary`, {
    headers: { 'If-Match': `"${revision}"` },
  })
  if (!r.ok) return r
  return isSiteDetail(r.data) ? ok(r.data) : shapeMismatch('SiteDetailResponse')
}

export async function deleteSiteEntry(
  transport: ReaderEndpointTransport,
  siteID: string,
  entryID: string,
  revision: number,
): Promise<ApiResult<SiteEntryDeleteResponse>> {
  const r = await transport.send('DELETE', `/api/sites/${encodeURIComponent(siteID)}/entries/${encodeURIComponent(entryID)}`, {
    headers: { 'If-Match': `"${revision}"` },
  })
  if (!r.ok) return r
  return isSiteEntryDeleteResponse(r.data) ? ok(r.data) : shapeMismatch('SiteEntryDeleteResponse')
}

export async function deleteSite(
  transport: ReaderEndpointTransport,
  id: string,
  revision: number,
  entryCount: number,
): Promise<ApiResult<void>> {
  const query = new URLSearchParams({ confirm_entry_count: String(entryCount) })
  const r = await transport.send('DELETE', `/api/sites/${encodeURIComponent(id)}?${query}`, {
    headers: { 'If-Match': `"${revision}"` },
  })
  if (!r.ok) return r
  return ok(undefined)
}

export async function previewSiteMerge(
  transport: ReaderEndpointTransport,
  request: SiteMergePreviewRequest,
): Promise<ApiResult<SiteMergePreviewResponse>> {
  const r = await transport.send('POST', '/api/sites/merge-preview', { body: request })
  if (!r.ok) return r
  return isSiteMergePreviewResponse(r.data) ? ok(r.data) : shapeMismatch('SiteMergePreviewResponse')
}

export async function executeSiteMerge(
  transport: ReaderEndpointTransport,
  request: SiteMergeExecuteRequest,
): Promise<ApiResult<SiteMergeExecuteResponse>> {
  const r = await transport.send('POST', '/api/sites/merge', { body: request })
  if (!r.ok) return r
  return isSiteMergeExecuteResponse(r.data) ? ok(r.data) : shapeMismatch('SiteMergeExecuteResponse')
}

export async function previewSiteSplit(
  transport: ReaderEndpointTransport,
  siteID: string,
  request: SiteSplitRequest,
): Promise<ApiResult<SiteSplitPreviewResponse>> {
  const r = await transport.send('POST', `/api/sites/${encodeURIComponent(siteID)}/split-preview`, {
    body: request,
  })
  if (!r.ok) return r
  return isSiteSplitPreviewResponse(r.data) ? ok(r.data) : shapeMismatch('SiteSplitPreviewResponse')
}

export async function executeSiteSplit(
  transport: ReaderEndpointTransport,
  siteID: string,
  request: SiteSplitRequest,
): Promise<ApiResult<SiteSplitExecuteResponse>> {
  const r = await transport.send('POST', `/api/sites/${encodeURIComponent(siteID)}/split`, {
    body: request,
  })
  if (!r.ok) return r
  return isSiteSplitExecuteResponse(r.data) ? ok(r.data) : shapeMismatch('SiteSplitExecuteResponse')
}

export async function previewLinkConversion(
  transport: ReaderEndpointTransport,
  id: string,
  request: ConversionPreviewRequest,
): Promise<ApiResult<ConversionPreviewResponse>> {
  const r = await transport.send('POST', `/api/links/${encodeURIComponent(id)}/conversion-preview`, {
    body: request,
  })
  if (!r.ok) return r
  return isConversionPreviewResponse(r.data) ? ok(r.data) : shapeMismatch('ConversionPreviewResponse')
}

export async function convertLink(
  transport: ReaderEndpointTransport,
  id: string,
  request: ConversionExecuteRequest,
): Promise<ApiResult<ConversionExecuteResponse>> {
  const r = await transport.send('POST', `/api/links/${encodeURIComponent(id)}/convert`, {
    body: request,
  })
  if (!r.ok) return r
  return isConversionExecuteResponse(r.data) ? ok(r.data) : shapeMismatch('ConversionExecuteResponse')
}

export async function getTags(
  transport: ReaderEndpointTransport,
  scope?: 'reading' | 'site' | 'all',
  options?: ReaderReadOptions,
): Promise<ApiResult<TagCountResponse[]>> {
  const query = scope ? `?library_kind=${encodeURIComponent(scope)}` : ''
  const r = await transport.send('GET', `/api/tags${query}`, { readOptions: options })
  if (!r.ok) return r
  if (!isTagCountArray(r.data)) return shapeMismatch('TagCountResponse[]')
  if (scope && r.data.some((tag) => {
    const readingCount = tag.reading_count
    const siteCount = tag.site_count
    if (typeof readingCount !== 'number' || typeof siteCount !== 'number') return true
    if (scope === 'reading') return tag.count !== readingCount
    if (scope === 'site') return tag.count !== siteCount
    return tag.count !== readingCount + siteCount
  })) {
    return shapeMismatch('scoped TagCountResponse[]')
  }
  return ok(r.data)
}

export async function getDomainSummaries(
  transport: ReaderEndpointTransport,
  scope?: 'reading' | 'site',
  options?: ReaderReadOptions,
): Promise<ApiResult<DomainTreeSummaryEnvelope>> {
  const query = scope ? `?view=domains&library_kind=${encodeURIComponent(scope)}` : '?view=domains'
  const r = await transport.send('GET', `/api/tree${query}`, { readOptions: options })
  if (!r.ok) return r
  if (!isDomainTreeSummaryEnvelope(r.data)) {
    return shapeMismatch('DomainTreeSummaryEnvelope')
  }
  if (scope && r.data.library_kind !== scope) {
    return shapeMismatch('scoped DomainTreeSummaryEnvelope')
  }
  return ok(r.data)
}

export async function refreshLink(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<SubmitResponse>> {
  const r = await transport.send('POST', `/api/links/${encodeURIComponent(id)}/refresh`)
  if (!r.ok) return r
  if (!isSubmitResponse(r.data)) return shapeMismatch('SubmitResponse')
  return ok(r.data)
}

export async function saveContent(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<LinkContentResponse>> {
  const r = await transport.send('POST', `/api/links/${encodeURIComponent(id)}/content`)
  if (!r.ok) return r
  if (!isLinkContentResponse(r.data)) {
    return shapeMismatch('LinkContentResponse')
  }
  return ok(r.data)
}

export async function replaceContent(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<LinkContentResponse>> {
  const r = await transport.send('PUT', `/api/links/${encodeURIComponent(id)}/content`)
  if (!r.ok) return r
  if (!isLinkContentResponse(r.data)) {
    return shapeMismatch('LinkContentResponse')
  }
  return ok(r.data)
}

export async function getTranslations(
  transport: ReaderEndpointTransport,
  id: string,
): Promise<ApiResult<TranslationListResponse>> {
  const r = await transport.send('GET', `/api/links/${encodeURIComponent(id)}/translations`)
  if (!r.ok) return r
  if (!isTranslationListResponse(r.data)) {
    return shapeMismatch('TranslationListResponse')
  }
  return ok(r.data)
}

export async function createTranslation(
  transport: ReaderEndpointTransport,
  id: string,
  request: TranslationCreateRequest,
): Promise<ApiResult<TranslationResponse>> {
  const r = await transport.send('POST', `/api/links/${encodeURIComponent(id)}/translations`, {
    body: request,
  })
  if (!r.ok) return r
  if (!isTranslationResponse(r.data)) {
    return shapeMismatch('TranslationResponse')
  }
  return ok(r.data)
}

export async function getEngagement(
  transport: ReaderEndpointTransport,
  linkID: string,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderEngagementResponse>> {
  const r = await transport.send('GET', `/api/engagement/${encodeURIComponent(linkID)}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderEngagementResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderEngagementResponse')
}

export async function patchEngagement(
  transport: ReaderEndpointTransport,
  linkID: string,
  request: ReaderEngagementRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderEngagementResponse>> {
  const r = await transport.send('PATCH', `/api/engagement/${encodeURIComponent(linkID)}`, {
    body: request,
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderEngagementResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderEngagementResponse')
}

export async function patchLinkMetadata(
  transport: ReaderEndpointTransport,
  linkID: string,
  revision: number,
  request: ReaderLinkMetadataRequest,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderLinkMetadataResponse>> {
  if (!Number.isSafeInteger(revision) || revision < 1) {
    return err({ kind: 'other', message: 'metadata revision must be a positive JavaScript-safe integer' })
  }
  const r = await transport.send('PATCH', `/api/links/${encodeURIComponent(linkID)}/metadata`, {
    body: request,
    headers: { 'If-Match': `"${revision}"` },
    signal: options.signal,
    rawJSONContract: 'link-metadata-revision',
  })
  if (!r.ok) return r
  return isReaderLinkMetadataResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderLinkMetadataResponse')
}

export async function getRelatedTags(
  transport: ReaderEndpointTransport,
  linkID?: string,
  limit = 12,
  options: ReaderRequestOptions = {},
): Promise<ApiResult<ReaderRelatedTagsResponse>> {
  const query = buildReaderQuery({ link_id: linkID, limit: readerLimit(limit, 12) })
  const r = await transport.send('GET', `/api/reader/related-tags${query}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderRelatedTagsResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderRelatedTagsResponse')
}

export async function getReaderActivity(
  transport: ReaderEndpointTransport,
  limit = 100,
  options: ReaderActivityRequestOptions = {},
): Promise<ApiResult<ReaderActivityResponse>> {
  const query = buildReaderQuery({
    kind: options.kind,
    after: options.after,
    limit: readerLimit(limit, 100),
  })
  const r = await transport.send('GET', `/api/reader/activity${query}`, {
    signal: options.signal,
  })
  if (!r.ok) return r
  return isReaderActivityResponse(r.data) ? ok(r.data) : shapeMismatch('ReaderActivityResponse')
}
