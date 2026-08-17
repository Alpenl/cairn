import type {
  CapabilitiesResponse,
  ErrorResponse,
  JobResponse,
  LinkContentResponse,
  LinkResponse,
  PaginatedLinksResponse,
  ReaderFeedAction,
  ReaderFeedFeedbackResponse,
  ReaderFeedItemResponse,
  ReaderFeedReasonCode,
  ReaderFeedResponse,
  ReaderFeedScoreSignal,
  ReaderFeedSectionResponse,
  ReaderFeedSourceResponse,
  ReaderCapabilitiesResponse,
  SubmitResponse,
  TagCountResponse,
  TranslationSourceIdentity,
} from './generated'

type RequiredKeys<T> = {
  [K in keyof T]-?: object extends Pick<T, K> ? never : K
}[keyof T]

type Validator = (value: unknown) => boolean

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isString(value: unknown): value is string {
  return typeof value === 'string'
}

function isNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value)
}

function isInteger(value: unknown): value is number {
  return isNumber(value) && Number.isInteger(value)
}

function isPositiveSafeInteger(value: unknown): value is number {
  return isNumber(value) && Number.isSafeInteger(value) && value >= 1
}

function isNullableString(value: unknown): value is string | null {
  return value === null || isString(value)
}

function isNullableInteger(value: unknown): value is number | null {
  return value === null || isInteger(value)
}

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every(isString)
}

function isEnumValue<T extends string>(
  values: Readonly<Record<T, true>>,
  value: unknown,
): value is T {
  return isString(value) && Object.prototype.hasOwnProperty.call(values, value)
}

function validatesRequiredFields(
  value: Record<string, unknown>,
  validators: Readonly<Record<string, Validator>>,
): boolean {
  return Object.entries(validators).every(([field, validate]) =>
    validate(value[field]),
  )
}

const LINK_STATUSES = {
  skeleton: true,
  pending: true,
  processing: true,
  done: true,
  failed: true,
} satisfies Record<LinkResponse['status'], true>

const PROCESSING_STATUSES = {
  pending: true,
  processing: true,
  done: true,
  failed: true,
} satisfies Record<JobResponse['status'] | SubmitResponse['status'], true>

const CAPTURE_DESTINATIONS = {
  library: true,
  inbox: true,
  site: true,
} satisfies Record<NonNullable<SubmitResponse['destination']>, true>

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
] as const satisfies readonly (keyof ReaderCapabilitiesResponse)[]

function isReaderCapabilitiesResponse(
  value: unknown,
): value is ReaderCapabilitiesResponse {
  return (
    isRecord(value) &&
    READER_CAPABILITY_KEYS.every((key) => typeof value[key] === 'boolean')
  )
}

const CONTENT_FORMATS = {
  plain: true,
  markdown: true,
  html: true,
} satisfies Record<LinkContentResponse['content_format'], true>

const CONTENT_TYPES = {
  article: true,
  listing: true,
  homepage: true,
  unknown: true,
} satisfies Record<NonNullable<LinkResponse['content_type']>, true>

const LIBRARY_KINDS = {
  reading: true,
  site: true,
} satisfies Record<NonNullable<LinkResponse['library_kind']>, true>

const LOW_CONFIDENCE_REASONS = {
  search_fallback: true,
  thin_content: true,
  fetch_quality: true,
  title_quality: true,
  unknown: true,
} satisfies Record<NonNullable<LinkResponse['low_confidence_reason']>, true>

const CONTENT_SOURCES = {
  fetched: true,
  user: true,
} satisfies Record<NonNullable<LinkResponse['content_source']>, true>

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
} satisfies Record<ReaderFeedAction, true>

const READER_FEED_ITEM_TYPES = {
  reading: true,
  inbox: true,
  subscription: true,
} satisfies Record<NonNullable<ReaderFeedItemResponse['item_type']>, true>

// A score signal owns one column of score_contributions; a reason code only
// explains the card. Every signal is a reason code, never the reverse:
// continue_reading is a Home reason that the ranking pass never produces.
const READER_FEED_SCORE_SIGNALS = {
  pending_confirmation: true,
  saved_library: true,
  subscription_recent: true,
  unread: true,
  read_later: true,
  chronological_fallback: true,
} satisfies Record<ReaderFeedScoreSignal, true>

const READER_FEED_REASON_CODES = {
  ...READER_FEED_SCORE_SIGNALS,
  continue_reading: true,
} satisfies Record<ReaderFeedReasonCode, true>

const READER_FEED_CAPABILITIES = {
  snapshot: true,
  cursor: true,
  dedupe: true,
  reason: true,
  source_filter: true,
  actions: true,
  inbox_batch: true,
} satisfies Record<NonNullable<ReaderFeedResponse['capabilities']>[number], true>

const LINK_FIELD_VALIDATORS = {
  id: isString,
  url: isString,
  title: isNullableString,
  summary: isNullableString,
  description: isNullableString,
  tags: isStringArray,
  content_type: (value) =>
    value === null || isEnumValue(CONTENT_TYPES, value),
  status: (value) => isEnumValue(LINK_STATUSES, value),
  domain: isNullableString,
  path_depth: isNullableInteger,
  parent_id: isNullableString,
  parent_path: isNullableString,
  fetcher_type: isNullableString,
  metadata_revision: isPositiveSafeInteger,
  has_content: (value: unknown) => typeof value === 'boolean',
  is_low_confidence: (value) => typeof value === 'boolean',
  low_confidence_reason: (value) =>
    value === null || isEnumValue(LOW_CONFIDENCE_REASONS, value),
  error_category: isNullableString,
  error_msg: isNullableString,
  created_at: isString,
  updated_at: isString,
} satisfies Record<RequiredKeys<LinkResponse>, Validator>

const LINK_CONTENT_FIELD_VALIDATORS = {
  link_id: isString,
  content: isString,
  content_format: (value) => isEnumValue(CONTENT_FORMATS, value),
  fetcher_type: isString,
  content_source: (value) => isEnumValue(CONTENT_SOURCES, value),
  content_revision: isInteger,
} satisfies Record<RequiredKeys<LinkContentResponse>, Validator>

function isTranslationSourceIdentity(
  value: unknown,
): value is TranslationSourceIdentity {
  return (
    isRecord(value) &&
    (value.content_revision === undefined ||
      isInteger(value.content_revision)) &&
    (value.block_key === undefined || isString(value.block_key)) &&
    (value.source_hash === undefined || isString(value.source_hash))
  )
}

export function isErrorResponse(value: unknown): value is ErrorResponse {
  if (!isRecord(value) || !isRecord(value.error)) return false
  return (
    isInteger(value.error.code) &&
    isString(value.error.message) &&
    (value.error.error_code === undefined ||
      isString(value.error.error_code)) &&
    (value.error.current_identity === undefined ||
      isTranslationSourceIdentity(value.error.current_identity))
  )
}

export function isCapabilitiesResponse(
  value: unknown,
): value is CapabilitiesResponse {
  return (
    isRecord(value) &&
    typeof value.library_kinds === 'boolean' &&
    typeof value.site_library === 'boolean' &&
    typeof value.site_auto_classification === 'boolean' &&
    typeof value.site_management === 'boolean' &&
    typeof value.site_advanced_management === 'boolean' &&
    Array.isArray(value.archive_versions) &&
    value.archive_versions.every(isInteger) &&
    typeof value.reader_vnext === 'boolean' &&
    isReaderCapabilitiesResponse(value.reader)
  )
}

export function isLinkResponse(value: unknown): value is LinkResponse {
  if (!isRecord(value)) return false
  return (
    validatesRequiredFields(value, LINK_FIELD_VALIDATORS) &&
    (value.library_kind === undefined ||
      isEnumValue(LIBRARY_KINDS, value.library_kind)) &&
    (value.content === undefined || isString(value.content)) &&
    (value.content_document === undefined ||
      isString(value.content_document)) &&
    (value.content_format === undefined ||
      isEnumValue(CONTENT_FORMATS, value.content_format)) &&
    (value.content_source === undefined ||
      isEnumValue(CONTENT_SOURCES, value.content_source)) &&
    (value.content_cjk_chars === undefined ||
      isInteger(value.content_cjk_chars)) &&
    (value.content_words === undefined || isInteger(value.content_words))
  )
}

export function isTagCountResponse(
  value: unknown,
): value is TagCountResponse {
  return (
    isRecord(value) &&
    isString(value.tag) &&
    isInteger(value.count) &&
    (value.reading_count === undefined || isInteger(value.reading_count)) &&
    (value.site_count === undefined || isInteger(value.site_count))
  )
}

export function isJobResponse(value: unknown): value is JobResponse {
  return (
    isRecord(value) &&
    isString(value.id) &&
    isString(value.link_id) &&
    isEnumValue(PROCESSING_STATUSES, value.status) &&
    isNullableString(value.error_category) &&
    isNullableString(value.error_msg) &&
    (value.link === null || isLinkResponse(value.link))
  )
}

export function isSubmitResponse(value: unknown): value is SubmitResponse {
  return (
    isRecord(value) &&
    isEnumValue(PROCESSING_STATUSES, value.status) &&
    (value.link_id === undefined || isString(value.link_id)) &&
    (value.inbox_id === undefined || isString(value.inbox_id)) &&
    (value.destination === undefined ||
      isEnumValue(CAPTURE_DESTINATIONS, value.destination)) &&
    (value.job_id === undefined || isString(value.job_id)) &&
    (value.link_id !== undefined || value.inbox_id !== undefined)
  )
}

export function isLinkContentResponse(
  value: unknown,
): value is LinkContentResponse {
  return (
    isRecord(value) &&
    validatesRequiredFields(value, LINK_CONTENT_FIELD_VALIDATORS) &&
    (value.content_document === undefined ||
      isString(value.content_document))
  )
}

export function isPaginatedLinksResponse(
  value: unknown,
): value is PaginatedLinksResponse {
  return (
    isRecord(value) &&
    Array.isArray(value.items) &&
    value.items.every(isLinkResponse) &&
    isInteger(value.total) &&
    isInteger(value.page) &&
    isInteger(value.limit) &&
    (value.next_cursor === undefined || isString(value.next_cursor))
  )
}

function isReaderFeedAction(value: unknown): value is ReaderFeedAction {
  return isEnumValue(READER_FEED_ACTIONS, value)
}

function isReaderFeedReasonCode(value: unknown): value is ReaderFeedReasonCode {
  return isEnumValue(READER_FEED_REASON_CODES, value)
}

function isReaderFeedScoreSignal(value: unknown): value is ReaderFeedScoreSignal {
  return isEnumValue(READER_FEED_SCORE_SIGNALS, value)
}

function isReaderFeedScoreContributions(value: unknown): value is Record<ReaderFeedScoreSignal, number> {
  return (
    isRecord(value) &&
    Object.keys(READER_FEED_SCORE_SIGNALS).every((signal) => isInteger(value[signal])) &&
    Object.keys(value).every((signal) => Object.prototype.hasOwnProperty.call(READER_FEED_SCORE_SIGNALS, signal))
  )
}

function isReaderFeedReasonParams(code: ReaderFeedReasonCode, value: unknown): boolean {
  if (!isRecord(value)) return false
  switch (code) {
    case 'continue_reading':
      return Object.keys(value).length === 0
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
    isEnumValue(READER_FEED_ITEM_TYPES, value.source) &&
    isString(value.label) &&
    isInteger(value.count) &&
    value.count >= 0 &&
    isUniqueReaderFeedArray(value.capabilities, isReaderFeedAction)
  )
}

function isReaderFeedSource(value: unknown): value is ReaderFeedSourceResponse {
  return (
    isRecord(value) &&
    isEnumValue(READER_FEED_ITEM_TYPES, value.id) &&
    isString(value.label) &&
    typeof value.enabled === 'boolean' &&
    isInteger(value.count) &&
    value.count >= 0 &&
    isUniqueReaderFeedArray(value.capabilities, isReaderFeedAction)
  )
}

export function isReaderFeedItemResponse(value: unknown): value is ReaderFeedItemResponse {
  if (!isRecord(value)) return false
  if (
    !isString(value.key) ||
    !isString(value.source) ||
    !isString(value.title) ||
    !isString(value.summary) ||
    !isString(value.url) ||
    !isNullableString(value.link_id) ||
    !isNullableString(value.inbox_id) ||
    !isNullableString(value.feed_item_id) ||
    typeof value.read !== 'boolean' ||
    typeof value.read_later !== 'boolean' ||
	    !isInteger(value.score) ||
    !isReaderFeedScoreContributions(value.score_contributions) ||
    !isUniqueReaderFeedArray(value.enabled_score_signals, isReaderFeedScoreSignal) ||
    !isReaderFeedReasonCode(value.reason_code) ||
    !isReaderFeedReasonParams(value.reason_code, value.reason_params) ||
	    !isInteger(value.reason_contribution) ||
	    value.reason_contribution < 0 ||
	    typeof value.saved !== 'boolean' ||
    !isString(value.reason_text) ||
    (value.published_at !== undefined && value.published_at !== null && !isString(value.published_at)) ||
    !isString(value.event_at) ||
    !isString(value.created_at)
  ) {
    return false
  }
  const contributions = value.score_contributions as Record<ReaderFeedScoreSignal, number>
  if (value.score !== Object.values(contributions).reduce((total, contribution) => total + contribution, 0)) {
    return false
  }
  if (isReaderFeedScoreSignal(value.reason_code)) {
    // A scored card must name a signal that was actually enabled, and must
    // report exactly the contribution the ranking pass wrote for it.
    if (
      !(value.enabled_score_signals as ReaderFeedScoreSignal[]).includes(value.reason_code) ||
      contributions[value.reason_code] !== value.reason_contribution
    ) {
      return false
    }
  } else if (
    // A reason outside the ranking pass owns no contributions column, so it must
    // claim no contribution either.
    value.reason_contribution !== 0 ||
    Object.prototype.hasOwnProperty.call(contributions, value.reason_code)
  ) {
    return false
  }
  if (value.item_type !== undefined) {
    if (!isEnumValue(READER_FEED_ITEM_TYPES, value.item_type) || value.item_type !== value.source) {
      return false
    }
  }
  if (
    (value.resource_key !== undefined && !isString(value.resource_key)) ||
    (value.action_key !== undefined && !isString(value.action_key)) ||
    (value.dedupe_key !== undefined && !isString(value.dedupe_key)) ||
    (value.section_id !== undefined && !isString(value.section_id))
  ) {
    return false
  }
  return value.actions === undefined || isUniqueReaderFeedArray(value.actions, isReaderFeedAction)
}

export function isReaderFeedFeedbackResponse(value: unknown): value is ReaderFeedFeedbackResponse {
  if (!isRecord(value) || !isString(value.item_key) || !isEnumValue(READER_FEED_ACTIONS, value.action) || typeof value.saved !== 'boolean') return false
  if (value.association === undefined) return true
  return isRecord(value.association) && isString(value.association.feed_item_id) && isString(value.association.link_id) && typeof value.association.created_link === 'boolean'
}

export function isReaderFeedResponse(value: unknown): value is ReaderFeedResponse {
  if (
    !isRecord(value) ||
    !Array.isArray(value.items) ||
    !value.items.every(isReaderFeedItemResponse) ||
    (value.next_cursor !== undefined && !isString(value.next_cursor)) ||
    !isString(value.snapshot_id) ||
    (value.mode !== 'recommended' && value.mode !== 'chronological')
  ) {
    return false
  }
  return (
    (value.capabilities === undefined ||
      isUniqueReaderFeedArray(value.capabilities, (item) => isEnumValue(READER_FEED_CAPABILITIES, item))) &&
    (value.sections === undefined ||
      (Array.isArray(value.sections) && value.sections.every(isReaderFeedSection))) &&
    (value.sources === undefined ||
      (Array.isArray(value.sources) && value.sources.every(isReaderFeedSource)))
  )
}
