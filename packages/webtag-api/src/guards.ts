import type {
  CapabilitiesResponse,
  ErrorResponse,
  LinkContentResponse,
  LinkResponse,
  PaginatedLinksResponse,
  ReaderFeedFeedbackResponse,
  ReaderFeedItemResponse,
  ReaderFeedResponse,
  ReaderCapabilitiesResponse,
  ReaderInboxListItemResponse,
  ReaderInboxResponsePage,
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

function isNonNegativeInteger(value: unknown): value is number {
  return isInteger(value) && value >= 0
}

function isPositiveSafeInteger(value: unknown): value is number {
  return isNumber(value) && Number.isSafeInteger(value) && value >= 1
}

function isNullableString(value: unknown): value is string | null {
  return value === null || isString(value)
}

function isOptionalNullableString(value: unknown): value is string | null | undefined {
  return value === undefined || isNullableString(value)
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
} satisfies Record<SubmitResponse['status'], true>

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
  'related_tags',
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

const READER_FEED_FEEDBACK_ACTIONS = {
  save: true,
  unsave: true,
  hide: true,
} satisfies Record<ReaderFeedFeedbackResponse['action'], true>

const READER_FEED_ITEM_TYPES = {
  reading: true,
  inbox: true,
  subscription: true,
} satisfies Record<ReaderFeedItemResponse['source'], true>

const READER_INBOX_STATUSES = {
  pending: true,
  confirmed: true,
} satisfies Record<ReaderInboxListItemResponse['status'], true>

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

const READER_INBOX_LIST_ITEM_FIELD_VALIDATORS = {
  id: isString,
  url: isString,
  source_kind: isString,
  preview: isString,
  tags: isStringArray,
  status: (value) => isEnumValue(READER_INBOX_STATUSES, value),
  metadata_revision: isInteger,
  expired: (value) => typeof value === 'boolean',
  updated_at: isString,
} satisfies Record<RequiredKeys<ReaderInboxListItemResponse>, Validator>

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

export function isSubmitResponse(value: unknown): value is SubmitResponse {
  return (
    isRecord(value) &&
    isEnumValue(PROCESSING_STATUSES, value.status) &&
    (value.link_id === undefined || isString(value.link_id)) &&
    (value.inbox_id === undefined || isString(value.inbox_id)) &&
    (value.destination === undefined ||
      isEnumValue(CAPTURE_DESTINATIONS, value.destination)) &&
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

export function isReaderInboxListItemResponse(
  value: unknown,
): value is ReaderInboxListItemResponse {
  return (
    isRecord(value) &&
    validatesRequiredFields(value, READER_INBOX_LIST_ITEM_FIELD_VALIDATORS) &&
    isOptionalNullableString(value.title)
  )
}

export function isReaderInboxResponsePage(
  value: unknown,
): value is ReaderInboxResponsePage {
  return (
    isRecord(value) &&
    Array.isArray(value.items) &&
    value.items.every(isReaderInboxListItemResponse) &&
    (value.next_cursor === undefined || isString(value.next_cursor)) &&
    isNonNegativeInteger(value.active_count) &&
    isNonNegativeInteger(value.expired_count)
  )
}

export function isReaderFeedItemResponse(value: unknown): value is ReaderFeedItemResponse {
  if (
    !isRecord(value) ||
    !isString(value.key) ||
    !isEnumValue(READER_FEED_ITEM_TYPES, value.source) ||
    !isString(value.resource_key) ||
    !isString(value.title) ||
    !isString(value.summary) ||
    !isString(value.url) ||
    !isOptionalNullableString(value.link_id) ||
    !isOptionalNullableString(value.inbox_id) ||
    !isOptionalNullableString(value.feed_item_id) ||
    typeof value.read !== 'boolean' ||
    typeof value.read_later !== 'boolean' ||
    typeof value.saved !== 'boolean' ||
    !isString(value.event_at)
  ) {
    return false
  }
  switch (value.source) {
    case 'reading':
      return isString(value.link_id) && value.inbox_id == null && value.feed_item_id == null
    case 'inbox':
      return isString(value.inbox_id) && value.link_id == null && value.feed_item_id == null
    case 'subscription':
      return isString(value.feed_item_id) && value.inbox_id == null
  }
}

export function isReaderFeedFeedbackResponse(value: unknown): value is ReaderFeedFeedbackResponse {
  return isRecord(value) && isString(value.item_key) && isEnumValue(READER_FEED_FEEDBACK_ACTIONS, value.action) &&
    (value.link_id === undefined || isString(value.link_id))
}

export function isReaderFeedResponse(value: unknown): value is ReaderFeedResponse {
  return (
    isRecord(value) &&
    Array.isArray(value.items) &&
    value.items.every(isReaderFeedItemResponse) &&
    (value.next_cursor === undefined || isString(value.next_cursor)) &&
    (value.mode === 'recommended' || value.mode === 'chronological')
  )
}
