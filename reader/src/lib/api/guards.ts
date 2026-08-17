/**
 * 响应体运行时类型守卫。
 *
 * client 已解析的 JSON 是 `unknown`，必须经守卫确认形状后才能当作 DTO 使用——
 * 绝不把缺字段的响应伪装成「空数据成功」，否则真实故障会被掩盖成数据丢失。
 * Required fields and enums follow the generated wire types. Optional nested
 * payloads are checked when present.
 */
import {
  isErrorResponse,
  isLinkContentResponse,
  isLinkResponse,
  isPaginatedLinksResponse as isPaginatedLinks,
  isSubmitResponse,
  isTagCountResponse,
} from '@webtag/api'
import { isRecord } from '../records'
import type {
	ConversionPreviewResponse,
	ConversionExecuteResponse,
	ClassificationRuleResponse,
	LibraryReviewResponse,
	GroupedSearchResponse,
	ReaderThoughtSearchResponse,
	ReaderNoteSearchResponse,
  CapabilitiesResponse,
  HealthResponse,
  ReaderCapabilitiesResponse,
  DomainTreeSummaryEnvelope,
  DiscoverFeedsResponse,
  FeedFolder,
  FeedItem,
  FeedItemAnalyzeResponse,
  FeedSubscription,
  OPMLImportResponse,
  PaginatedFeedItemsResponse,
	PaginatedSitesResponse,
	SiteDetailResponse,
	SiteEntryDeleteResponse,
	SiteListItemResponse,
	SiteMergePreviewResponse,
	SiteMergeExecuteResponse,
	SiteSplitPreviewResponse,
	SiteSplitExecuteResponse,
  SubscriptionsResponse,
  TagCountResponse,
  TranslationListResponse,
  TranslationResponse,
  ReaderThoughtAckResponse,
  ReaderThoughtResponse,
  ReaderThoughtsResponse,
  ReaderThoughtSupersessionOperationResponse,
  ReaderThoughtSupersessionEventsResponse,
  ReaderNoteResponse,
  ReaderNotesResponse,
  ReaderNoteHistoryResponse,
  ReaderHostLifecycleResponse,
  ReaderTrashItemResponse,
  ReaderTrashResponse,
  ReaderInboxJobResponse,
  ReaderInboxResponse,
  ReaderInboxListItemResponse,
  ReaderInboxResponsePage,
  ReaderInboxBulkItemResponse,
  ReaderInboxBulkResponse,
  ReaderInboxConfirmAIProposalsResponse,
  ReaderConfirmResponse,
  ReaderCategoryResponse,
  ReaderCategoriesResponse,
  ReaderTodoResponse,
  ReaderTodosResponse,
  ReaderEngagementResponse,
  ReaderFeedAction,
  ReaderFeedFeedbackResponse,
  ReaderFeedItemResponse,
  ReaderFeedResponse,
  ReaderFeedSectionResponse,
  ReaderFeedSourceResponse,
  ReaderHomeResponse,
  ReaderLinkMetadataResponse,
  ReaderContentHistoryResponse,
  ReaderContentHistoryRestoreResponse,
  ReaderRelatedTagsResponse,
  ReaderActivityResponse,
  ReaderTagActivityResponse,
  ReaderDomainActivityResponse,
  ReaderAIResponse,
} from './types'
import { isValidSourceHash } from '../article/source-block'
import { isValidThoughtIdentifier } from '../user-data/thought-types'

export {
  isErrorResponse,
  isLinkContentResponse,
  isLinkResponse,
  isPaginatedLinks,
  isSubmitResponse,
}

const READER_CAPABILITY_KEYS = [
  'annotations',
  'notes',
  'inbox',
  'todos',
  'engagement',
  'home',
  'feed',
  'ai',
  'semantic',
  'activity',
  'history',
  'trash',
] as const

const READER_RESPONSE_FRESHNESS_VALUES = {
  unknown: true,
  fresh: true,
  partial: true,
  stale: true,
} as const

const READER_FEED_ACTIONS = {
  confirm: true,
  discard: true,
  read: true,
  read_later: true,
  save: true,
  unsave: true,
  hide: true,
  not_interested: true,
  open: true,
  open_workspace: true,
} as const satisfies Record<ReaderFeedAction, true>

const READER_FEED_ITEM_TYPES = {
  reading: true,
  inbox: true,
  subscription: true,
} as const

const READER_FEED_REASON_CODES = {
  pending_confirmation: true,
  saved_library: true,
  subscription_recent: true,
  unread: true,
  read_later: true,
  chronological_fallback: true,
} as const

const READER_FEED_CAPABILITIES = {
  snapshot: true,
  cursor: true,
  dedupe: true,
  reason: true,
  source_filter: true,
  actions: true,
  inbox_batch: true,
} as const

const DISABLED_READER_CAPABILITIES: ReaderCapabilitiesResponse = {
  annotations: false,
  notes: false,
  inbox: false,
  todos: false,
  engagement: false,
  home: false,
  feed: false,
  ai: false,
  semantic: false,
  activity: false,
  history: false,
  trash: false,
}

function isBooleanRecord(value: unknown, keys: readonly string[]): value is Record<string, boolean> {
  return isRecord(value) && keys.every((key) => typeof value[key] === 'boolean')
}

export function isReaderCapabilitiesResponse(v: unknown): v is ReaderCapabilitiesResponse {
  return isBooleanRecord(v, READER_CAPABILITY_KEYS)
}

/**
 * Capabilities were added after the original Reader API. Older servers may
 * return a valid legacy envelope without the Reader fields, so normalize that
 * response to an explicit capability-off snapshot instead of treating a
 * missing field as support for the new surfaces.
 */
export function normalizeCapabilitiesResponse(value: unknown): CapabilitiesResponse {
  const source = isRecord(value) ? value : {}
  const reader = isReaderCapabilitiesResponse(source.reader)
    ? source.reader
    : DISABLED_READER_CAPABILITIES
  return {
    library_kinds: source.library_kinds === true,
    site_library: source.site_library === true,
    site_auto_classification: source.site_auto_classification === true,
    site_management: source.site_management === true,
    site_advanced_management: source.site_advanced_management === true,
    archive_versions: Array.isArray(source.archive_versions)
      ? source.archive_versions.filter(isInteger)
      : [],
    reader_vnext: source.reader_vnext === true && isReaderCapabilitiesResponse(source.reader),
    reader,
  }
}

export function isCapabilitiesResponse(v: unknown): v is CapabilitiesResponse {
  if (!isRecord(v) || typeof v.library_kinds !== 'boolean' ||
    typeof v.site_library !== 'boolean' ||
    typeof v.site_auto_classification !== 'boolean' ||
    typeof v.site_management !== 'boolean' ||
    typeof v.site_advanced_management !== 'boolean' ||
    !Array.isArray(v.archive_versions) || !v.archive_versions.every(isInteger) ||
    typeof v.reader_vnext !== 'boolean') return false
  return isReaderCapabilitiesResponse(v.reader)
}

/**
 * `/health` is the liveness probe that also carries the build identity the
 * settings surface shows. All four identity fields are required by the spec,
 * so a response missing any of them is a different service answering on the
 * same origin, not a Cairn backend with unknown version.
 */
export function isHealthResponse(v: unknown): v is HealthResponse {
  return isRecord(v) &&
    v.status === 'ok' &&
    isString(v.version) &&
    isString(v.commit) &&
    isString(v.build_time)
}

function isString(v: unknown): v is string {
  return typeof v === 'string'
}

function isNumber(v: unknown): v is number {
  return typeof v === 'number' && Number.isFinite(v)
}

function isInteger(v: unknown): v is number {
  return isNumber(v) && Number.isInteger(v)
}

function isNonNegativeInteger(v: unknown): v is number {
  return isInteger(v) && v >= 0
}

function isPositiveInteger(v: unknown): v is number {
  return isInteger(v) && v > 0
}

function isSafePositiveInteger(v: unknown): v is number {
  return typeof v === 'number' && Number.isSafeInteger(v) && v > 0
}

const MAX_SAFE_LINK_METADATA_REVISION = '9007199254740991'

function isJSONWhitespace(char: string): boolean {
  return char === ' ' || char === '\n' || char === '\r' || char === '\t'
}

function isJSONDigit(char: string): boolean {
  return char >= '0' && char <= '9'
}

function isJSONHexDigit(char: string): boolean {
  return (
    (char >= '0' && char <= '9') ||
    (char >= 'a' && char <= 'f') ||
    (char >= 'A' && char <= 'F')
  )
}

function isCanonicalSafeLinkMetadataRevisionToken(token: string): boolean {
  if (!/^[1-9][0-9]*$/.test(token)) return false
  return (
    token.length < MAX_SAFE_LINK_METADATA_REVISION.length ||
    (token.length === MAX_SAFE_LINK_METADATA_REVISION.length && token <= MAX_SAFE_LINK_METADATA_REVISION)
  )
}

/**
 * Validates every raw `metadata_revision` property before JSON.parse can round
 * a fractional or out-of-range number into an apparently safe integer.
 */
export function hasCanonicalSafeLinkMetadataRevisionTokens(raw: string): boolean {
  let index = 0

  function skipWhitespace(): void {
    while (isJSONWhitespace(raw[index] ?? '')) index += 1
  }

  function parseStringToken(): string | null {
    if (raw[index] !== '"') return null
    const start = index
    index += 1
    while (index < raw.length) {
      const char = raw[index] ?? ''
      index += 1
      if (char === '"') return raw.slice(start, index)
      if (char === '\\') {
        if (index >= raw.length) return null
        const escape = raw[index] ?? ''
        index += 1
        if (escape === 'u') {
          for (let offset = 0; offset < 4; offset += 1) {
            if (!isJSONHexDigit(raw[index + offset] ?? '')) return null
          }
          index += 4
        } else if (!'"\\/bfnrt'.includes(escape)) {
          return null
        }
      } else if (char.charCodeAt(0) < 0x20) {
        return null
      }
    }
    return null
  }

  function parseNumberToken(): string | null {
    const start = index
    if (raw[index] === '-') index += 1
    if (raw[index] === '0') {
      index += 1
    } else {
      const first = raw[index] ?? ''
      if (first < '1' || first > '9') return null
      index += 1
      while (isJSONDigit(raw[index] ?? '')) index += 1
    }
    if (raw[index] === '.') {
      index += 1
      if (!isJSONDigit(raw[index] ?? '')) return null
      while (isJSONDigit(raw[index] ?? '')) index += 1
    }
    if (raw[index] === 'e' || raw[index] === 'E') {
      index += 1
      if (raw[index] === '+' || raw[index] === '-') index += 1
      if (!isJSONDigit(raw[index] ?? '')) return null
      while (isJSONDigit(raw[index] ?? '')) index += 1
    }
    return raw.slice(start, index)
  }

  function parseValue(): boolean {
    skipWhitespace()
    switch (raw[index]) {
      case '{':
        return parseObject()
      case '[':
        return parseArray()
      case '"':
        return parseStringToken() !== null
      case 't':
        return parseLiteral('true')
      case 'f':
        return parseLiteral('false')
      case 'n':
        return parseLiteral('null')
      default:
        return parseNumberToken() !== null
    }
  }

  function parseLiteral(literal: string): boolean {
    if (!raw.startsWith(literal, index)) return false
    index += literal.length
    return true
  }

  function parseArray(): boolean {
    index += 1
    skipWhitespace()
    if (raw[index] === ']') {
      index += 1
      return true
    }
    while (index < raw.length) {
      if (!parseValue()) return false
      skipWhitespace()
      if (raw[index] === ']') {
        index += 1
        return true
      }
      if (raw[index] !== ',') return false
      index += 1
      skipWhitespace()
    }
    return false
  }

  function parseObject(): boolean {
    index += 1
    skipWhitespace()
    if (raw[index] === '}') {
      index += 1
      return true
    }
    while (index < raw.length) {
      const propertyToken = parseStringToken()
      if (propertyToken === null) return false
      let propertyName: unknown
      try {
        propertyName = JSON.parse(propertyToken)
      } catch {
        return false
      }
      skipWhitespace()
      if (raw[index] !== ':') return false
      index += 1
      skipWhitespace()
      if (propertyName === 'metadata_revision') {
        const revisionToken = parseNumberToken()
        if (
          revisionToken === null ||
          !isCanonicalSafeLinkMetadataRevisionToken(revisionToken)
        ) return false
      } else if (!parseValue()) {
        return false
      }
      skipWhitespace()
      if (raw[index] === '}') {
        index += 1
        return true
      }
      if (raw[index] !== ',') return false
      index += 1
      skipWhitespace()
    }
    return false
  }

  if (!parseValue()) return false
  skipWhitespace()
  return index === raw.length
}

function isNullableString(v: unknown): boolean {
  return v === null || isString(v)
}

function isStringArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.every(isString)
}

function hasEnumValue(values: Record<string, true>, value: unknown): boolean {
  return (
    isString(value) && Object.prototype.hasOwnProperty.call(values, value)
  )
}

/** Conversion previews are revision-bound; every consequence field must be
 * present so the confirmation UI never hides a deletion behind a partial API
 * response. */
export function isConversionPreviewResponse(v: unknown): v is ConversionPreviewResponse {
	return isRecord(v) &&
		isString(v.link_id) &&
		(v.current_kind === 'reading' || v.current_kind === 'site') &&
		(v.target_kind === 'reading' || v.target_kind === 'site') &&
		isInteger(v.expected_content_revision) &&
		typeof v.destructive === 'boolean' &&
		typeof v.saved_original === 'boolean' &&
		isInteger(v.translation_count) &&
		typeof v.reparse_required === 'boolean' &&
		(v.annotation_policy === undefined || isString(v.annotation_policy))
}

export function isConversionExecuteResponse(v: unknown): v is ConversionExecuteResponse {
	if (!isRecord(v) || !isString(v.link_id) ||
		(v.library_kind !== 'reading' && v.library_kind !== 'site') ||
		!isInteger(v.content_revision) || !isString(v.status) ||
		typeof v.reparse_required !== 'boolean' || !isRecord(v.reader_target) || !isString(v.reader_target.view)) return false
	const target = v.reader_target
	return (target.view === 'sites' || target.view === 'processing' || target.view === 'reading' || target.view === 'failed' || target.view === 'review') &&
		(v.site_id === undefined || isString(v.site_id)) &&
		(v.site_revision === undefined || isInteger(v.site_revision)) &&
		(v.entry_id === undefined || isString(v.entry_id)) &&
		(v.parse_job_id === undefined || isString(v.parse_job_id))
}

export function isTranslationResponse(v: unknown): v is TranslationResponse {
  return (
    isRecord(v) &&
    isString(v.id) &&
    isString(v.link_id) &&
    (v.scope === 'selection' || v.scope === 'full') &&
    isString(v.block_key) &&
    isInteger(v.start_offset) &&
    isInteger(v.end_offset) &&
    isString(v.source_text) &&
    isNullableString(v.translated_text) &&
    (v.source_format === 'plain' || v.source_format === 'markdown') &&
    v.target_language === 'zh-CN' &&
    (v.status === 'pending' ||
      v.status === 'processing' ||
      v.status === 'done' ||
      v.status === 'failed') &&
    isNullableString(v.model) &&
    isNullableString(v.error_msg) &&
    (v.source_content_revision === null || isPositiveInteger(v.source_content_revision)) &&
    typeof v.stale === 'boolean' &&
    isString(v.created_at) &&
    isString(v.updated_at)
  )
}

export function isTranslationListResponse(
  v: unknown,
): v is TranslationListResponse {
  return (
    isRecord(v) &&
    isNonNegativeInteger(v.current_content_revision) &&
    (v.current_summary_source_hash === null ||
      isValidSourceHash(v.current_summary_source_hash)) &&
    Array.isArray(v.items) &&
    v.items.every(isTranslationResponse)
  )
}

export function isReaderThoughtSearchResponse(v: unknown): v is ReaderThoughtSearchResponse {
	return isRecord(v) && isString(v.id) && isString(v.host_kind) && isString(v.host_id) &&
		isOptionalNullableString(v.link_id) && isString(v.snippet) && isString(v.updated_at)
}

export function isReaderNoteSearchResponse(v: unknown): v is ReaderNoteSearchResponse {
	return isRecord(v) && isString(v.id) && isString(v.title) && isString(v.snippet) &&
		isInteger(v.published_revision) && isString(v.updated_at)
}

/** Grouped library search must retain independently valid reading and site groups. */
export function isGroupedSearchResponse(v: unknown): v is GroupedSearchResponse {
	if (!isRecord(v) || !isRecord(v.reading) || !isRecord(v.sites)) return false
	const reading = v.reading
	const sites = v.sites
	if (!(isInteger(reading.total_hint) && Array.isArray(reading.items) && reading.items.every(isLinkResponse) &&
		isInteger(sites.total_hint) && Array.isArray(sites.items) && sites.items.every((site) =>
			isRecord(site) && isString(site.id) && isString(site.name) && Array.isArray(site.matched_entries) &&
			site.matched_entries.every((entry) => isRecord(entry) && isString(entry.id) && isString(entry.name) && isString(entry.url)),
		))) return false
	if (v.thoughts !== undefined && (!isRecord(v.thoughts) || !isInteger(v.thoughts.total_hint) || !isOptionalString(v.thoughts.next_cursor) || !Array.isArray(v.thoughts.items) || !v.thoughts.items.every(isReaderThoughtSearchResponse))) return false
	if (v.notes !== undefined && (!isRecord(v.notes) || !isInteger(v.notes.total_hint) || !Array.isArray(v.notes.items) || !v.notes.items.every(isReaderNoteSearchResponse))) return false
	return true
}

export function isSiteListItem(v: unknown): v is SiteListItemResponse {
	return isRecord(v) && isString(v.id) && isString(v.name) && isString(v.intro) &&
		isString(v.display_host) && isStringArray(v.tags) && isInteger(v.entry_count) &&
		typeof v.pinned === 'boolean' && typeof v.needs_review === 'boolean' &&
		isInteger(v.revision) && isString(v.first_collected_at) && isString(v.last_collected_at)
}

export function isPaginatedSites(v: unknown): v is PaginatedSitesResponse {
	return isRecord(v) && Array.isArray(v.items) && v.items.every(isSiteListItem) &&
		isInteger(v.total) && isInteger(v.page) && isInteger(v.limit) &&
		(v.recent_cutoff === undefined || isString(v.recent_cutoff))
}

export function isSiteDetail(v: unknown): v is SiteDetailResponse {
	if (!isRecord(v)) return false
	const raw: Record<string, unknown> = v
	if (!isSiteListItem(raw)) return false
	const detail = raw as unknown as Record<string, unknown>
	return isString(detail['user_note']) && typeof detail['grouping_locked'] === 'boolean' &&
		Array.isArray(detail['tags_with_source']) && Array.isArray(detail['entries']) &&
		Array.isArray(detail['related_readings']) && detail['related_readings'].every((item) => isRecord(item) && isString(item.id) && isString(item.title) && isString(item.url) && isString(item.created_at))
}

export function isSiteEntryDeleteResponse(v: unknown): v is SiteEntryDeleteResponse {
	return isRecord(v) && typeof v.deleted_site === 'boolean'
}

export function isSiteMergePreviewResponse(v: unknown): v is SiteMergePreviewResponse {
	return isRecord(v) && isString(v.target_site_id) && isInteger(v.target_revision) &&
		Array.isArray(v.entries) && v.entries.every((entry) => isRecord(entry) && isString(entry.id) && isString(entry.site_id) && isString(entry.link_id) && isString(entry.name) && isString(entry.url) && typeof entry.duplicate === 'boolean') &&
		isStringArray(v.user_tags) && isStringArray(v.identity_keys) &&
		Array.isArray(v.field_conflicts) && v.field_conflicts.every((conflict) => isRecord(conflict) && isString(conflict.field) && isString(conflict.target_value) && isString(conflict.source_site_id) && isString(conflict.source_value)) &&
		typeof v.requires_resolution === 'boolean'
}

export function isSiteMergeExecuteResponse(v: unknown): v is SiteMergeExecuteResponse {
	return isRecord(v) && isString(v.site_id) && isInteger(v.revision) && isInteger(v.moved_entries) && isInteger(v.deleted_duplicate_links)
}
function isSiteSplitRequest(v: unknown): boolean {
	return isRecord(v) && isInteger(v.expected_revision) && isStringArray(v.entry_ids) &&
		isString(v.name) && isString(v.primary_entry_id) &&
		(v.intro === undefined || isString(v.intro)) &&
		(v.homepage_url === undefined || isString(v.homepage_url)) &&
		(v.icon_url === undefined || isString(v.icon_url)) &&
		(v.user_note === undefined || isString(v.user_note)) &&
		(v.identity_keys_for_new_site === undefined || isStringArray(v.identity_keys_for_new_site))
}
export function isSiteSplitPreviewResponse(v: unknown): v is SiteSplitPreviewResponse {
	return isRecord(v) && isString(v.source_site_id) && isInteger(v.source_revision) && isSiteSplitRequest(v.payload) &&
		Array.isArray(v.entries) && v.entries.every((entry) => isRecord(entry) && isString(entry.id) && isString(entry.site_id) && isString(entry.link_id) && isString(entry.name) && isString(entry.url) && typeof entry.duplicate === 'boolean') &&
		Array.isArray(v.identities) && v.identities.every((identity) => isRecord(identity) && isString(identity.identity_key) && typeof identity.eligible_for_new_site === 'boolean' && (identity.owner === 'source' || identity.owner === 'new_site')) &&
		isStringArray(v.user_tags)
}
export function isSiteSplitExecuteResponse(v: unknown): v is SiteSplitExecuteResponse { return isRecord(v) && isString(v.source_site_id) && isInteger(v.source_revision) && isString(v.new_site_id) && isInteger(v.new_site_revision) && isInteger(v.moved_entries) }

export function isClassificationRuleResponse(v: unknown): v is ClassificationRuleResponse {
	return isRecord(v) && isString(v.id) && isString(v.host) &&
		isOptionalNullableString(v.identity_adapter) && isOptionalNullableString(v.path_prefix) &&
		(v.target_kind === 'reading' || v.target_kind === 'site') && typeof v.enabled === 'boolean' &&
		isInteger(v.revision) && isString(v.created_at) && isString(v.updated_at)
}

export function isClassificationRuleArray(v: unknown): v is ClassificationRuleResponse[] {
	return Array.isArray(v) && v.every(isClassificationRuleResponse)
}

export function isLibraryReviewResponse(v: unknown): v is LibraryReviewResponse {
	return isRecord(v) && isString(v.id) &&
		(v.kind === 'classification_uncertain' || v.kind === 'migration_suggestion' || v.kind === 'note_conflict' || v.kind === 'merge_conflict') &&
		isRecord(v.payload) && (v.status === 'pending' || v.status === 'applied' || v.status === 'dismissed') &&
		isInteger(v.revision) && isString(v.created_at) && isOptionalNullableString(v.link_id) && isOptionalNullableString(v.site_id) && isOptionalNullableString(v.resolved_at)
}

export function isLibraryReviewArray(v: unknown): v is LibraryReviewResponse[] { return Array.isArray(v) && v.every(isLibraryReviewResponse) }

/** 校验 TagCountResponse[]。 */
export function isTagCountArray(v: unknown): v is TagCountResponse[] {
  return Array.isArray(v) && v.every(isTagCountResponse)
}

/** 校验 GET /api/tree?view=domains 的聚合 envelope。 */
export function isDomainTreeSummaryEnvelope(
  v: unknown,
): v is DomainTreeSummaryEnvelope {
  return (
    isRecord(v) &&
    isInteger(v.total) &&
    (v.library_kind === undefined || v.library_kind === 'reading' || v.library_kind === 'site') &&
    Array.isArray(v.domains) &&
    v.domains.every(
      (d) => isRecord(d) && isString(d.domain) && isInteger(d.count),
    )
  )
}

function hasOwn(value: Record<string, unknown>, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(value, key)
}

function isReaderResponseMetadata(value: Record<string, unknown>): boolean {
  return (
    (value.freshness === undefined || hasEnumValue(READER_RESPONSE_FRESHNESS_VALUES, value.freshness)) &&
    (value.partial === undefined || typeof value.partial === 'boolean')
  )
}

function isOptionalDateTime(value: unknown): boolean {
  return value === undefined || value === null || isString(value)
}

function isThoughtIdentifier(value: unknown): value is string {
  return isValidThoughtIdentifier(value)
}

function isThoughtVersionKey(value: unknown): boolean {
  return isRecord(value) && Number.isSafeInteger(value.logical_clock) &&
    (value.logical_clock as number) >= 0 &&
    (value.logical_clock as number) <= Number.MAX_SAFE_INTEGER &&
    isThoughtIdentifier(value.device_id) && isThoughtIdentifier(value.op_id)
}

function isReaderThought(value: unknown): value is ReaderThoughtResponse {
  return (
    isRecord(value) &&
    value.contract_version === 1 &&
    isString(value.id) &&
    isString(value.host_kind) &&
    isString(value.host_id) &&
    hasOwn(value, 'target') &&
    isOptionalNullableString(value.link_id) &&
    isString(value.body) &&
    isString(value.source) &&
    typeof value.deleted === 'boolean' &&
    isInteger(value.last_sequence) &&
    isThoughtVersionKey(value.winner_key) &&
    isString(value.created_at) &&
    isString(value.updated_at) &&
    (value.lifecycle_status === undefined || value.lifecycle_status === 'active' || value.lifecycle_status === 'tombstone') &&
    isOptionalNullableString(value.lifecycle_reason) &&
    isOptionalDateTime(value.tombstoned_at) &&
    (value.original_host_snapshot === undefined || value.original_host_snapshot !== null)
  )
}

export function isReaderThoughtAckResponse(v: unknown): v is ReaderThoughtAckResponse {
  return isRecord(v) && v.contract_version === 1 && isThoughtIdentifier(v.op_id) &&
    isInteger(v.sequence) && v.sequence > 0 &&
    (v.disposition === 'applied' || v.disposition === 'superseded' ||
      v.disposition === 'duplicate') &&
    isThoughtVersionKey(v.submitted_key) && isThoughtVersionKey(v.current_winner_key)
}

export function isReaderThoughtResponse(v: unknown): v is ReaderThoughtResponse {
  return isReaderThought(v)
}

export function isReaderThoughtsResponse(v: unknown): v is ReaderThoughtsResponse {
  return (
    isRecord(v) &&
    v.contract_version === 1 &&
    Array.isArray(v.items) &&
    v.items.every(isReaderThought) &&
    isOptionalString(v.next_cursor)
  )
}

function isReaderThoughtTarget(value: unknown, hostID: string): boolean {
  if (!isRecord(value) || value.host_id !== hostID || !isRecord(value.version)) return false
  const version = value.version
  switch (value.kind) {
    case 'saved-content':
      return isSafePositiveInteger(version.content_revision)
    case 'summary':
      return isString(version.source_hash) && version.source_hash.length > 0
    case 'note':
      return isSafePositiveInteger(version.note_revision)
    case 'legacy-stale':
      return isString(version.source_key) && version.source_key.length > 0
    default:
      return false
  }
}

function isReaderThoughtPayload(value: unknown, operationKind: unknown): boolean {
  if (!isRecord(value) || !isString(value.body) ||
    (value.source !== 'self' && value.source !== 'ai' && value.source !== 'user')) return false
  if (operationKind === 'delete' && value.quote === undefined) return true
  if (!isRecord(value.quote) || !isString(value.quote.exact)) return false
  return (value.quote.start === undefined || isNonNegativeInteger(value.quote.start)) &&
    (value.quote.end === undefined || isNonNegativeInteger(value.quote.end)) &&
    (value.quote.prefix === undefined || isString(value.quote.prefix)) &&
    (value.quote.suffix === undefined || isString(value.quote.suffix)) &&
    (value.quote.block_key === undefined || isString(value.quote.block_key))
}

function isReaderThoughtSupersessionOperation(
  value: unknown,
  annotationID: string,
): value is ReaderThoughtSupersessionOperationResponse {
  const logicalClock = isRecord(value) ? value.logical_clock : undefined
  if (!isRecord(value) || value.contract_version !== 1 ||
    !isSafePositiveInteger(value.sequence) || !isThoughtIdentifier(value.op_id) ||
    !isThoughtIdentifier(value.device_id) || typeof logicalClock !== 'number' ||
    !Number.isSafeInteger(logicalClock) ||
    logicalClock < 0 || logicalClock > Number.MAX_SAFE_INTEGER ||
    value.annotation_id !== annotationID || !isThoughtIdentifier(value.annotation_id) ||
    (value.operation_kind !== 'add' && value.operation_kind !== 'update' && value.operation_kind !== 'delete') ||
    (value.host_kind !== 'link' && value.host_kind !== 'note' && value.host_kind !== 'inbox') ||
    !isThoughtIdentifier(value.host_id) || !isReaderThoughtTarget(value.target, value.host_id) ||
    !isReaderThoughtPayload(value.payload, value.operation_kind) || !isString(value.created_at)) {
    return false
  }
  const hasRecoveryOf = value.recovery_of !== undefined
  const hasExpectedWinner = value.expected_current_winner_key !== undefined
  return hasRecoveryOf === hasExpectedWinner &&
    (!hasRecoveryOf || (isThoughtVersionKey(value.recovery_of) &&
      isThoughtVersionKey(value.expected_current_winner_key)))
}

function compareUTF8(left: string, right: string): number {
  const encoder = new TextEncoder()
  const leftBytes = encoder.encode(left)
  const rightBytes = encoder.encode(right)
  const length = Math.min(leftBytes.length, rightBytes.length)
  for (let index = 0; index < length; index += 1) {
    if (leftBytes[index] !== rightBytes[index]) return leftBytes[index] < rightBytes[index] ? -1 : 1
  }
  if (leftBytes.length === rightBytes.length) return 0
  return leftBytes.length < rightBytes.length ? -1 : 1
}

function compareReaderThoughtSupersessionOperations(
  left: ReaderThoughtSupersessionOperationResponse,
  right: ReaderThoughtSupersessionOperationResponse,
): number {
  if (left.logical_clock !== right.logical_clock) {
    return left.logical_clock < right.logical_clock ? -1 : 1
  }
  const device = compareUTF8(left.device_id, right.device_id)
  return device === 0 ? compareUTF8(left.op_id, right.op_id) : device
}

function isReaderThoughtSupersessionCursor(value: unknown): boolean {
  if (!isString(value) || value.length === 0 || value.length > 512 ||
    !/^[A-Za-z0-9_-]+$/.test(value)) return false
  try {
    const padding = '='.repeat((4 - value.length % 4) % 4)
    const decoded = atob(value.replace(/-/g, '+').replace(/_/g, '/') + padding)
    return /^[1-9][0-9]*\|event$/.test(decoded)
  } catch {
    return false
  }
}

export function isReaderThoughtSupersessionEventsResponse(
  value: unknown,
): value is ReaderThoughtSupersessionEventsResponse {
  return isRecord(value) && value.contract_version === 1 &&
    Array.isArray(value.items) &&
    (value.next_cursor === undefined || isReaderThoughtSupersessionCursor(value.next_cursor)) &&
    value.items.every((item) => {
      if (!isRecord(item) || !isSafePositiveInteger(item.sequence) ||
        !isThoughtIdentifier(item.annotation_id) ||
        !isReaderThoughtSupersessionOperation(item.loser, item.annotation_id) ||
        !isReaderThoughtSupersessionOperation(item.winner_at_detection, item.annotation_id)) {
        return false
      }
      return compareReaderThoughtSupersessionOperations(
        item.winner_at_detection,
        item.loser,
      ) > 0
    })
}

function isReaderNote(value: unknown): value is ReaderNoteResponse {
  return (
    isRecord(value) &&
    isString(value.id) &&
    isString(value.title) &&
    isString(value.published_content) &&
    isInteger(value.published_revision) &&
    isOptionalNullableString(value.draft_content) &&
    isInteger(value.draft_revision) &&
    isOptionalDateTime(value.draft_updated_at) &&
    isOptionalDateTime(value.deleted_at) &&
    isString(value.created_at) &&
    isString(value.updated_at) &&
    typeof value.dirty === 'boolean'
  )
}

export function isReaderNoteResponse(v: unknown): v is ReaderNoteResponse {
  return isReaderNote(v)
}

export function isReaderNotesResponse(v: unknown): v is ReaderNotesResponse {
  return (
    isRecord(v) &&
    Array.isArray(v.items) &&
    v.items.every(isReaderNote) &&
    isInteger(v.count) &&
    isOptionalString(v.next_cursor)
  )
}

export function isReaderNoteHistoryResponse(v: unknown): v is ReaderNoteHistoryResponse {
  return (
    isRecord(v) &&
    isInteger(v.id) &&
    isInteger(v.revision) &&
    isString(v.title) &&
    isString(v.content) &&
	Array.isArray(v.reanchor_ops) &&
    isString(v.created_at)
  )
}

function isReaderHostKind(value: unknown): boolean {
  return value === 'link' || value === 'inbox' || value === 'note'
}

export function isReaderHostLifecycleResponse(v: unknown): v is ReaderHostLifecycleResponse {
  return (
    isRecord(v) &&
    isReaderHostKind(v.host_kind) &&
    isString(v.host_id) &&
    (v.state === 'live' || v.state === 'trashed') &&
    typeof v.changed === 'boolean'
  )
}

function isReaderTrashItem(value: unknown): value is ReaderTrashItemResponse {
  return (
    isRecord(value) &&
    isReaderHostKind(value.host_kind) &&
    isString(value.host_id) &&
    isOptionalNullableString(value.title) &&
    isOptionalNullableString(value.url) &&
    isString(value.trashed_at)
  )
}

export function isReaderTrashResponse(v: unknown): v is ReaderTrashResponse {
  return (
    isRecord(v) &&
    Array.isArray(v.items) &&
    v.items.every(isReaderTrashItem) &&
    isInteger(v.count) && v.count >= 0 &&
    (v.next_cursor === undefined || isString(v.next_cursor))
  )
}

function isReaderInbox(value: unknown): value is ReaderInboxResponse {
  return (
    isRecord(value) &&
    isString(value.id) &&
    isString(value.url) &&
    isString(value.source_kind) &&
    isOptionalNullableString(value.title) &&
    isString(value.body) &&
    isString(value.note) &&
    isOptionalNullableString(value.summary) &&
    isStringArray(value.suggested_tags) &&
    isRecord(value.proposal_signals) &&
    (value.proposal_status === 'pending' || value.proposal_status === 'running' || value.proposal_status === 'completed' || value.proposal_status === 'failed') &&
    isStringArray(value.tags) &&
    isStringArray(value.category_ids) &&
    (value.status === 'pending' || value.status === 'confirmed' || value.status === 'discarded') &&
    isInteger(value.metadata_revision) &&
    isOptionalNullableString(value.job_id) &&
    isNullableString(value.expires_at) &&
    isNullableString(value.expired_at) &&
    typeof value.expired === 'boolean' &&
    value.expired === (value.expired_at !== null) &&
    isString(value.created_at) &&
    isString(value.updated_at)
  )
}

export function isReaderInboxResponse(v: unknown): v is ReaderInboxResponse {
  return isReaderInbox(v)
}

// The queue card contract is narrower than the detail record on purpose: a
// capture may hold a 4 MiB body and a 1 MiB note, and the list is read on every
// Inbox open. This guard therefore validates the card fields and nothing else —
// it must not start requiring body/note back, or the projection is undone.
function isReaderInboxListItem(value: unknown): value is ReaderInboxListItemResponse {
  return (
    isRecord(value) &&
    isString(value.id) &&
    isString(value.url) &&
    isString(value.source_kind) &&
    isOptionalNullableString(value.title) &&
    isString(value.preview) &&
    isStringArray(value.tags) &&
    (value.status === 'pending' || value.status === 'confirmed' || value.status === 'discarded') &&
    isInteger(value.metadata_revision) &&
    typeof value.expired === 'boolean' &&
    isString(value.updated_at)
  )
}

export function isReaderInboxListItemResponse(v: unknown): v is ReaderInboxListItemResponse {
  return isReaderInboxListItem(v)
}

export function isReaderInboxResponsePage(v: unknown): v is ReaderInboxResponsePage {
  return (
    isRecord(v) &&
    Array.isArray(v.items) &&
    v.items.every(isReaderInboxListItem) &&
    isOptionalString(v.next_cursor) &&
    isInteger(v.active_count) &&
    v.active_count >= 0 &&
    isInteger(v.expired_count) &&
    v.expired_count >= 0
  )
}

function isReaderInboxBulkItem(value: unknown): value is ReaderInboxBulkItemResponse {
  return (
    isRecord(value) &&
    isString(value.inbox_id) &&
    (value.status === 'confirmed' || value.status === 'discarded') &&
    isOptionalString(value.link_id)
  )
}

export function isReaderInboxBulkResponse(v: unknown): v is ReaderInboxBulkResponse {
  return isRecord(v) && v.atomic === true && Array.isArray(v.items) && v.items.every(isReaderInboxBulkItem)
}

export function isReaderInboxConfirmAIProposalsResponse(
  v: unknown,
): v is ReaderInboxConfirmAIProposalsResponse {
  return (
    isRecord(v) &&
    v.atomic === true &&
    Array.isArray(v.items) &&
    v.items.every(isReaderInboxBulkItem) &&
    isInteger(v.remaining_count) &&
    v.remaining_count >= 0
  )
}

export function isReaderInboxJobResponse(v: unknown): v is ReaderInboxJobResponse {
  return isRecord(v) && isString(v.inbox_id) && isString(v.status) && isString(v.job_id)
}

export function isReaderConfirmResponse(v: unknown): v is ReaderConfirmResponse {
  return isRecord(v) && v.target_kind === 'link' && isString(v.link_id) && v.status === 'confirmed'
}

function isReaderCategory(value: unknown): value is ReaderCategoryResponse {
  return isRecord(value) && isString(value.id) && isString(value.name) && isInteger(value.count) && isString(value.created_at)
}

export function isReaderCategoryResponse(v: unknown): v is ReaderCategoryResponse {
  return isReaderCategory(v)
}

export function isReaderCategoriesResponse(v: unknown): v is ReaderCategoriesResponse {
  return isRecord(v) && Array.isArray(v.items) && v.items.every(isReaderCategory)
}

function isReaderTodo(value: unknown): value is ReaderTodoResponse {
  return (
    isRecord(value) &&
    isString(value.id) &&
    isString(value.text) &&
    isOptionalDateTime(value.due_at) &&
    typeof value.done === 'boolean' &&
    isString(value.origin_kind) &&
    isOptionalNullableString(value.origin_host_kind) &&
    isOptionalNullableString(value.origin_host_id) &&
    hasOwn(value, 'origin_ref') &&
    isInteger(value.host_revision) &&
    value.host_revision >= 0 &&
    isOptionalDateTime(value.completed_at) &&
    isString(value.created_at) &&
    isString(value.updated_at) &&
    typeof value.expired === 'boolean' &&
    (
      value.source_href === undefined ||
      (
        isString(value.source_href) &&
        value.source_href.startsWith('/') &&
        !value.source_href.startsWith('//')
      )
    )
  )
}

export function isReaderTodoResponse(v: unknown): v is ReaderTodoResponse {
  return isReaderTodo(v)
}

export function isReaderTodosResponse(v: unknown): v is ReaderTodosResponse {
  return (
    isRecord(v) &&
    isReaderResponseMetadata(v) &&
    Array.isArray(v.items) &&
    v.items.every(isReaderTodo) &&
    (v.next_after === undefined || isString(v.next_after))
  )
}

export function isReaderEngagementResponse(v: unknown): v is ReaderEngagementResponse {
  return (
    isRecord(v) &&
    isString(v.link_id) &&
    typeof v.read === 'boolean' &&
    isNumber(v.progress) &&
    v.progress >= 0 &&
    v.progress <= 1 &&
    typeof v.read_later === 'boolean' &&
    isOptionalDateTime(v.last_opened) &&
    isString(v.updated_at)
  )
}

function isReaderFeedAction(value: unknown): value is ReaderFeedAction {
  return isString(value) && Object.prototype.hasOwnProperty.call(READER_FEED_ACTIONS, value)
}

function isReaderFeedReasonCode(value: unknown): value is keyof typeof READER_FEED_REASON_CODES {
  return isString(value) && Object.prototype.hasOwnProperty.call(READER_FEED_REASON_CODES, value)
}

function isReaderFeedScoreContributions(value: unknown): value is Record<keyof typeof READER_FEED_REASON_CODES, number> {
  return (
    isRecord(value) &&
    Object.keys(READER_FEED_REASON_CODES).every((code) => isInteger(value[code])) &&
    Object.keys(value).every((code) => Object.prototype.hasOwnProperty.call(READER_FEED_REASON_CODES, code))
  )
}

function isReaderFeedReasonParams(code: keyof typeof READER_FEED_REASON_CODES, value: unknown): boolean {
  if (!isRecord(value)) return false
  switch (code) {
    case 'pending_confirmation':
      return value.source === 'inbox' && Object.keys(value).length === 1
    case 'saved_library':
      return value.source === 'reading' && Object.keys(value).length === 1
    case 'subscription_recent':
      return value.source === 'subscription' && Object.keys(value).length === 1
    case 'unread':
      return value.read === false && Object.keys(value).length === 1
    case 'read_later':
      return value.read_later === true && Object.keys(value).length === 1
    case 'chronological_fallback':
      return isString(value.created_at) && Object.keys(value).length === 1
  }
}

function isUniqueReaderFeedArray(
  value: unknown,
  predicate: (value: unknown) => boolean,
): boolean {
  if (!Array.isArray(value)) return false
  const seen = new Set<unknown>()
  return value.every((item) => {
    if (!predicate(item) || seen.has(item)) return false
    seen.add(item)
    return true
  })
}

function isReaderFeedSection(value: unknown): value is ReaderFeedSectionResponse {
  return (
    isRecord(value) &&
    isString(value.id) &&
    isString(value.source) &&
    Object.prototype.hasOwnProperty.call(READER_FEED_ITEM_TYPES, value.source) &&
    isString(value.label) &&
    isNonNegativeInteger(value.count) &&
    isUniqueReaderFeedArray(value.capabilities, isReaderFeedAction)
  )
}

function isReaderFeedSource(value: unknown): value is ReaderFeedSourceResponse {
  return (
    isRecord(value) &&
    isString(value.id) &&
    Object.prototype.hasOwnProperty.call(READER_FEED_ITEM_TYPES, value.id) &&
    isString(value.label) &&
    typeof value.enabled === 'boolean' &&
    isNonNegativeInteger(value.count) &&
    isUniqueReaderFeedArray(value.capabilities, isReaderFeedAction)
  )
}

function isReaderFeedItem(value: unknown): value is ReaderFeedItemResponse {
  if (!isRecord(value)) return false
  if (
    !isString(value.key) ||
    !isString(value.source) ||
    !isString(value.title) ||
    !isString(value.summary) ||
    !isString(value.url) ||
    !isOptionalNullableString(value.link_id) ||
    !isOptionalNullableString(value.inbox_id) ||
    !isOptionalNullableString(value.feed_item_id) ||
    typeof value.read !== 'boolean' ||
    typeof value.read_later !== 'boolean' ||
    typeof value.saved !== 'boolean' ||
    !isInteger(value.score) ||
    !isReaderFeedScoreContributions(value.score_contributions) ||
    !isUniqueReaderFeedArray(value.enabled_score_signals, isReaderFeedReasonCode) ||
    !isReaderFeedReasonCode(value.reason_code) ||
    !isReaderFeedReasonParams(value.reason_code, value.reason_params) ||
    !isInteger(value.reason_contribution) ||
    value.reason_contribution < 0 ||
    !isString(value.reason_text) ||
    !isOptionalDateTime(value.published_at) ||
    !isString(value.event_at) ||
    !isString(value.created_at)
  ) {
    return false
  }
  if (
    value.score !== Object.values(value.score_contributions as Record<keyof typeof READER_FEED_REASON_CODES, number>).reduce((total, contribution) => total + contribution, 0) ||
    !(value.enabled_score_signals as (keyof typeof READER_FEED_REASON_CODES)[]).includes(value.reason_code) ||
    (value.score_contributions as Record<keyof typeof READER_FEED_REASON_CODES, number>)[value.reason_code] !== value.reason_contribution
  ) {
    return false
  }
  if (value.item_type !== undefined) {
    if (!isString(value.item_type) || !Object.prototype.hasOwnProperty.call(READER_FEED_ITEM_TYPES, value.item_type) || value.item_type !== value.source) {
      return false
    }
  }
  if (
    (value.resource_key !== undefined && !isString(value.resource_key)) ||
    (value.action_key !== undefined && !isString(value.action_key)) ||
    (value.dedupe_key !== undefined && !isString(value.dedupe_key)) ||
    (value.section_id !== undefined && !isString(value.section_id)) ||
    // `capabilities` belongs to the response envelope, never to an item.
    // Reject it here so a malformed envelope cannot hide an invalid value in
    // a card while the top-level capability collection is absent.
    value.capabilities !== undefined
  ) {
    return false
  }
  return value.actions === undefined || isUniqueReaderFeedArray(value.actions, isReaderFeedAction)
}

function isReaderFeedCapability(value: unknown): boolean {
  return isString(value) && Object.prototype.hasOwnProperty.call(READER_FEED_CAPABILITIES, value)
}

export function isReaderFeedItemResponse(v: unknown): v is ReaderFeedItemResponse {
  return isReaderFeedItem(v)
}

export function isReaderFeedFeedbackResponse(value: unknown): value is ReaderFeedFeedbackResponse {
  if (!isRecord(value) || !isString(value.item_key) || !isReaderFeedAction(value.action) || typeof value.saved !== 'boolean') return false
  if (value.association === undefined) return true
  return isRecord(value.association) && isString(value.association.feed_item_id) && isString(value.association.link_id) && typeof value.association.created_link === 'boolean'
}

export function isReaderFeedResponse(v: unknown): v is ReaderFeedResponse {
  return (
    isRecord(v) &&
    Array.isArray(v.items) &&
    v.items.every(isReaderFeedItem) &&
    isOptionalString(v.next_cursor) &&
    isString(v.snapshot_id) &&
    (v.mode === 'recommended' || v.mode === 'chronological') &&
    (v.capabilities === undefined || isUniqueReaderFeedArray(v.capabilities, isReaderFeedCapability)) &&
    (v.sections === undefined || (Array.isArray(v.sections) && v.sections.every(isReaderFeedSection))) &&
    (v.sources === undefined || (Array.isArray(v.sources) && v.sources.every(isReaderFeedSource)))
  )
}

export function isReaderHomeResponse(v: unknown): v is ReaderHomeResponse {
  return (
    isRecord(v) &&
    isReaderResponseMetadata(v) &&
    isString(v.today) &&
    isString(v.summary) &&
    isRecord(v.counts) &&
    Object.values(v.counts).every(isInteger) &&
    Array.isArray(v.continue_reading) &&
    v.continue_reading.every(isReaderFeedItem) &&
    Array.isArray(v.recent_thoughts) &&
    v.recent_thoughts.every(isReaderThought) &&
    Array.isArray(v.todos) &&
    v.todos.every(isReaderTodo) &&
    typeof v.stale === 'boolean' &&
    (v.freshness === undefined ||
      v.freshness === 'unknown' ||
      v.freshness === 'fresh' ||
      v.freshness === 'partial' ||
      v.freshness === 'stale') &&
    (v.partial === undefined || typeof v.partial === 'boolean')
  )
}

export function isReaderLinkMetadataResponse(v: unknown): v is ReaderLinkMetadataResponse {
  return isRecord(v) && isString(v.link_id) && isSafePositiveInteger(v.metadata_revision)
}

export function isReaderContentHistoryResponse(v: unknown): v is ReaderContentHistoryResponse {
  return (
    isRecord(v) &&
    isInteger(v.id) &&
    isInteger(v.revision) &&
    isOptionalNullableString(v.content) &&
    isOptionalNullableString(v.content_document) &&
    isString(v.content_format) &&
    isString(v.content_source) &&
    isString(v.created_at)
  )
}

export function isReaderContentHistoryRestoreResponse(v: unknown): v is ReaderContentHistoryRestoreResponse {
  return isRecord(v) && isString(v.link_id) && isPositiveInteger(v.content_revision)
}

export function isReaderRelatedTagsResponse(v: unknown): v is ReaderRelatedTagsResponse {
  return isRecord(v) && isStringArray(v.items) && isString(v.model) && typeof v.degraded === 'boolean'
}

export function isReaderTagActivityResponse(v: unknown): v is ReaderTagActivityResponse {
  return isRecord(v) && isString(v.tag) && isString(v.last_at)
}

export function isReaderDomainActivityResponse(v: unknown): v is ReaderDomainActivityResponse {
  return isRecord(v) && isString(v.domain) && isString(v.last_at)
}

export function isReaderActivityResponse(v: unknown): v is ReaderActivityResponse {
  return (
    isRecord(v) &&
    (v.kind === 'all' || v.kind === 'tag' || v.kind === 'domain') &&
    Array.isArray(v.tags) &&
    v.tags.every(isReaderTagActivityResponse) &&
    Array.isArray(v.domains) &&
    v.domains.every(isReaderDomainActivityResponse) &&
    isOptionalString(v.next_cursor)
  )
}

export function isReaderAIResponse(v: unknown): v is ReaderAIResponse {
  return isRecord(v) && typeof v.enabled === 'boolean' && isString(v.answer) && isOptionalString(v.model)
}

const FEED_ANALYSIS_STATUSES = {
  none: true,
  pending: true,
  processing: true,
  done: true,
  failed: true,
} as const

function isOptionalString(v: unknown): boolean {
  return v === undefined || isString(v)
}

function isOptionalNullableString(v: unknown): boolean {
  return v === undefined || isNullableString(v)
}

function isOptionalBoolean(v: unknown): boolean {
  return v === undefined || typeof v === 'boolean'
}

function isOptionalInteger(v: unknown): boolean {
  return v === undefined || isInteger(v)
}

/** RSS folder guard: identity and display name are the stable core. */
export function isFeedFolder(v: unknown): v is FeedFolder {
  return (
    isRecord(v) &&
    isString(v.id) &&
    isString(v.name) &&
    isOptionalString(v.created_at) &&
    isOptionalString(v.updated_at)
  )
}

/** Accept `feed_url` canonically and the early `url` alias. */
export function isFeedSubscription(v: unknown): v is FeedSubscription {
  if (!isRecord(v) || !isString(v.id)) return false
  if (!isString(v.feed_url) && !isString(v.url)) return false
  return (
    isOptionalNullableString(v.site_url) &&
    isOptionalNullableString(v.title) &&
    isOptionalNullableString(v.name) &&
    isOptionalNullableString(v.description) &&
    isOptionalNullableString(v.folder_id) &&
    isOptionalInteger(v.unread_count) &&
    isOptionalInteger(v.item_count) &&
    isOptionalBoolean(v.active) &&
    isOptionalBoolean(v.refreshing) &&
    isOptionalNullableString(v.last_success_at) &&
    isOptionalNullableString(v.last_fetched_at) &&
    isOptionalNullableString(v.next_fetch_at) &&
    isOptionalNullableString(v.last_error) &&
    isOptionalNullableString(v.fetch_error) &&
    isOptionalInteger(v.failure_count) &&
    isOptionalString(v.created_at) &&
    isOptionalString(v.updated_at)
  )
}

export function isSubscriptionsResponse(v: unknown): v is SubscriptionsResponse {
  if (!isRecord(v) || !Array.isArray(v.folders) || !Array.isArray(v.subscriptions)) {
    return false
  }
  if (!v.folders.every(isFeedFolder) || !v.subscriptions.every(isFeedSubscription)) {
    return false
  }
  return (
    isRecord(v.counts) &&
    isInteger(v.counts.all) &&
    isInteger(v.counts.unread) &&
    isInteger(v.counts.starred) &&
    isInteger(v.counts.later)
  )
}

export function isOPMLImportResponse(v: unknown): v is OPMLImportResponse {
  return (
    isRecord(v) &&
    isInteger(v.imported) &&
    isInteger(v.folders) &&
    isInteger(v.skipped) &&
    isStringArray(v.errors)
  )
}

export function isDiscoverFeedsResponse(v: unknown): v is DiscoverFeedsResponse {
  return (
    isRecord(v) &&
    Array.isArray(v.feeds) &&
    v.feeds.every(
      (feed) =>
        isRecord(feed) &&
        (isString(feed.feed_url) || isString(feed.url)) &&
        isOptionalNullableString(feed.title) &&
        isOptionalNullableString(feed.type),
    )
  )
}

export function isFeedItem(v: unknown): v is FeedItem {
  if (
    !isRecord(v) ||
    !isString(v.id) ||
    !isString(v.subscription_id) ||
    !isString(v.title) ||
    !isString(v.url)
  ) {
    return false
  }
  const analysisStatus = v.analysis_status
  const linkStatus = v.link_status
  return (
    isOptionalNullableString(v.guid) &&
    isOptionalString(v.subscription_title) &&
    isOptionalNullableString(v.author) &&
    isOptionalNullableString(v.summary) &&
    isOptionalNullableString(v.content) &&
    isOptionalNullableString(v.content_html) &&
    isOptionalNullableString(v.published_at) &&
    isOptionalString(v.created_at) &&
    isOptionalString(v.updated_at) &&
    isOptionalNullableString(v.read_at) &&
    isOptionalNullableString(v.starred_at) &&
    isOptionalNullableString(v.read_later_at) &&
    isOptionalBoolean(v.read) &&
    isOptionalBoolean(v.starred) &&
    isOptionalBoolean(v.read_later) &&
    isOptionalNullableString(v.link_id) &&
    (analysisStatus === undefined || hasEnumValue(FEED_ANALYSIS_STATUSES, analysisStatus)) &&
    (linkStatus === undefined || hasEnumValue(FEED_ANALYSIS_STATUSES, linkStatus)) &&
    isOptionalNullableString(v.analysis_error)
  )
}

export function isPaginatedFeedItems(v: unknown): v is PaginatedFeedItemsResponse {
  return (
    isRecord(v) &&
    Array.isArray(v.items) &&
    v.items.every(isFeedItem) &&
    isInteger(v.total) &&
    isInteger(v.page) &&
    isInteger(v.limit)
  )
}

export function isFeedItemAnalyzeResponse(v: unknown): v is FeedItemAnalyzeResponse {
  if (!isRecord(v)) return false
  return (
    (v.item === undefined || isFeedItem(v.item)) &&
    isOptionalString(v.link_id) &&
    (v.status === undefined || hasEnumValue(FEED_ANALYSIS_STATUSES, v.status)) &&
    (v.item !== undefined || v.link_id !== undefined || v.status !== undefined)
  )
}
