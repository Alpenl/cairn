/**
 * Browser-side v2 archive selection and integrity validation.
 *
 * The archive is deliberately verified from its received bytes. Re-encoding a
 * parsed object would hide whitespace/key-order corruption and would not match
 * the server's checksum, which covers the original prefix before manifest.
 */

import { isLinkResponse } from '@webtag/api'
import { isRecord } from '../records'
import { isValidThoughtIdentifier } from '../user-data/thought-types'

/** The largest archive the browser is allowed to buffer and validate. */
export const ARCHIVE_V2_MAX_BYTES = 64 * 1024 * 1024

/** Stable error codes surfaced by the Reader download adapter. */
export const ARCHIVE_V2_TOO_LARGE_ERROR_CODE = 'archive_too_large_for_browser'
export const ARCHIVE_V2_VALIDATION_ERROR_CODE = 'archive_validation_failed'

/** The verified download is JSON even when a server sends an imprecise header. */
export const ARCHIVE_V2_MIME_TYPE = 'application/json'

/**
 * Base data is always included. Thoughts and notes independently add their
 * privacy groups to the archive.
 */
export interface ArchiveV2Selection {
  readonly includeThoughts?: boolean
  readonly includeNotes?: boolean
}

/** The explicit default; callers must still encode it as a query parameter. */
export const fullArchiveV2Selection = Object.freeze({
  includeThoughts: true,
  includeNotes: true,
}) satisfies Readonly<ArchiveV2Selection>

export interface ArchiveV2ValidationOptions {
  readonly clientDataNamespace: string
  readonly selection?: ArchiveV2Selection
}

/** A validation failure safe to expose through ApiError.errorCode. */
export class ArchiveV2ValidationError extends Error {
  readonly errorCode: string

  constructor(message: string, errorCode = ARCHIVE_V2_VALIDATION_ERROR_CODE) {
    super(message)
    this.name = 'ArchiveV2ValidationError'
    this.errorCode = errorCode
  }
}

const TOP_LEVEL_ARRAY_SECTIONS = [
  'links',
  'sites',
  'site_entries',
  'site_tags',
  'site_identities',
] as const

const READER_BASE_SECTIONS = [
  'feed_folders',
  'feed_subscriptions',
  'feed_items',
  'feed_saves',
  'inbox',
  'todos',
  'engagement',
  'feed_hides',
] as const

const READER_THOUGHT_SECTIONS = [
  'thoughts',
  'thought_ops',
  'thought_supersession_events',
  'thought_tombstones',
] as const

const READER_NOTE_SECTIONS = ['notes', 'note_history'] as const

const ARCHIVE_TOP_LEVEL_KEYS_WITHOUT_READER = [
  'schema_version',
  'exported_at',
  'generator_version',
  ...TOP_LEVEL_ARRAY_SECTIONS,
  'manifest',
] as const

const ARCHIVE_TOP_LEVEL_KEYS_WITH_READER = [
  ...ARCHIVE_TOP_LEVEL_KEYS_WITHOUT_READER.slice(0, -1),
  'reader',
  'manifest',
] as const

const MANIFEST_KEYS = [
  'client_data_namespace',
  'sections',
  'counts',
  'checksum_sha256',
] as const

const SHA256_HEX_PATTERN = /^[a-f0-9]{64}$/
const CLIENT_DATA_NAMESPACE_PATTERN = /^[A-Za-z0-9_-]{43}$/
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
const RFC3339_PATTERN = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/

const HOST_KINDS = new Set(['link', 'inbox', 'note'])
const THOUGHT_OPERATION_KINDS = new Set(['add', 'update', 'delete'])
const INBOX_STATUSES = new Set(['pending', 'confirmed', 'discarded'])
const TODO_ORIGIN_KINDS = new Set(['standalone', 'thought', 'note'])
const TODO_ORIGIN_HOST_KINDS = new Set(['thought', 'note'])
type NormalizedArchiveV2Selection = Readonly<{
  includeThoughts: boolean
  includeNotes: boolean
}>

type ReaderArchiveSection =
  | (typeof READER_BASE_SECTIONS)[number]
  | (typeof READER_THOUGHT_SECTIONS)[number]
  | (typeof READER_NOTE_SECTIONS)[number]

type ArchiveRowValidator = (value: unknown, name: string) => void

interface TopLevelMember {
  readonly key: string
  /** The comma that begins this member, or null for the first member. */
  readonly separatorOffset: number | null
}

function invalidArchive(message: string): never {
  throw new ArchiveV2ValidationError(message)
}

function archiveTooLarge(): never {
  throw new ArchiveV2ValidationError(
    '归档超过浏览器可验证的 64 MiB 上限',
    ARCHIVE_V2_TOO_LARGE_ERROR_CODE,
  )
}

/** Uint8Array values may originate from another browser realm. */
export function isArchiveV2ByteArray(value: unknown): value is Uint8Array {
  return Object.prototype.toString.call(value) === '[object Uint8Array]'
}

function hasExactKeys(value: Record<string, unknown>, expected: readonly string[]): boolean {
  const keys = Object.keys(value)
  return (
    keys.length === expected.length &&
    expected.every((key) => Object.prototype.hasOwnProperty.call(value, key))
  )
}

function requireExactRecord(
  value: unknown,
  name: string,
  expected: readonly string[],
): Record<string, unknown> {
  if (!isRecord(value) || !hasExactKeys(value, expected)) {
    invalidArchive(`归档记录 ${name} 不符合固定字段合同`)
  }
  return value
}

function requireRecord(value: unknown, name: string): Record<string, unknown> {
  if (!isRecord(value)) invalidArchive(`归档字段 ${name} 必须是对象`)
  return value
}

function requireString(value: unknown, name: string): string {
  if (typeof value !== 'string') invalidArchive(`归档字段 ${name} 必须是字符串`)
  return value
}

function requireNonEmptyString(value: unknown, name: string): string {
  const text = requireString(value, name)
  if (text.trim() === '') invalidArchive(`归档字段 ${name} 必须是非空字符串`)
  return text
}

function requireStringLength(
  value: unknown,
  name: string,
  minimum: number,
  maximum: number,
): string {
  const text = requireString(value, name)
  const length = Array.from(text).length
  if (length < minimum || length > maximum) {
    invalidArchive(`归档字段 ${name} 的长度超出合同范围`)
  }
  return text
}

function requireNullableString(value: unknown, name: string): string | null {
  if (value === null) return null
  return requireString(value, name)
}

function requireBoolean(value: unknown, name: string): boolean {
  if (typeof value !== 'boolean') invalidArchive(`归档字段 ${name} 必须是布尔值`)
  return value
}

function requireSafeInteger(
  value: unknown,
  name: string,
  minimum = Number.MIN_SAFE_INTEGER,
): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < minimum) {
    invalidArchive(`归档字段 ${name} 必须是范围内的安全整数`)
  }
  return value
}

function requireFiniteNumber(
  value: unknown,
  name: string,
  minimum: number,
  maximum: number,
): number {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < minimum || value > maximum) {
    invalidArchive(`归档字段 ${name} 必须是范围内的有限数字`)
  }
  return value
}

function requireEnum(value: unknown, name: string, values: ReadonlySet<string>): string {
  const text = requireString(value, name)
  if (!values.has(text)) invalidArchive(`归档字段 ${name} 不在允许枚举中`)
  return text
}

function isLeapYear(year: number): boolean {
  return year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0)
}

function isRFC3339DateTime(value: unknown): value is string {
  if (typeof value !== 'string') return false
  const match = RFC3339_PATTERN.exec(value)
  if (!match) return false
  const [, yearText, monthText, dayText, hourText, minuteText, secondText, , zone] = match
  const year = Number(yearText)
  const month = Number(monthText)
  const day = Number(dayText)
  const hour = Number(hourText)
  const minute = Number(minuteText)
  const second = Number(secondText)
  const days = [31, isLeapYear(year) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]
  if (month < 1 || month > 12 || day < 1 || day > days[month - 1] || hour > 23 || minute > 59 || second > 59) {
    return false
  }
  if (zone !== 'Z') {
    const zoneHour = Number(zone.slice(1, 3))
    const zoneMinute = Number(zone.slice(4, 6))
    if (zoneHour > 23 || zoneMinute > 59) return false
  }
  return Number.isFinite(Date.parse(value))
}

function requireDateTime(value: unknown, name: string): string {
  if (!isRFC3339DateTime(value)) invalidArchive(`归档字段 ${name} 必须是 RFC3339 date-time`)
  return value
}

function requireNullableDateTime(value: unknown, name: string): string | null {
  if (value === null) return null
  return requireDateTime(value, name)
}

function isUUID(value: unknown): value is string {
  return typeof value === 'string' && UUID_PATTERN.test(value)
}

function requireUUID(value: unknown, name: string): string {
  if (!isUUID(value)) invalidArchive(`归档字段 ${name} 必须是规范 UUID`)
  return value
}

function requireNullableUUID(value: unknown, name: string): string | null {
  if (value === null) return null
  return requireUUID(value, name)
}

function requireThoughtIdentifier(value: unknown, name: string, maxBytes = 128): string {
  if (!isValidThoughtIdentifier(value, maxBytes)) {
    invalidArchive(`归档字段 ${name} 必须是生产 Thought 标识符`)
  }
  return value
}

function validateThoughtOperationHost(
  operationKind: string,
  hostKindValue: unknown,
  hostIDValue: unknown,
  name: string,
): void {
  const hostKind = requireThoughtIdentifier(hostKindValue, `${name}.host_kind`, 32)
  const hostID = requireThoughtIdentifier(hostIDValue, `${name}.host_id`, 256)
  if (operationKind === 'delete') {
    // materializeThought still parses link hosts, even when a delete bypasses
    // the live-host lock. Other delete hosts retain their bounded wire form.
    if (hostKind === 'link') requireUUID(hostID, `${name}.host_id`)
    return
  }
  requireEnum(hostKind, `${name}.host_kind`, HOST_KINDS)
  requireUUID(hostID, `${name}.host_id`)
}

function validateMaterializedThoughtHost(
  deleted: boolean,
  hostKindValue: unknown,
  hostIDValue: unknown,
  name: string,
): void {
  const hostKind = requireThoughtIdentifier(hostKindValue, `${name}.host_kind`, 32)
  const hostID = requireThoughtIdentifier(hostIDValue, `${name}.host_id`, 256)
  if (deleted) {
    if (hostKind === 'link') requireUUID(hostID, `${name}.host_id`)
    return
  }
  requireEnum(hostKind, `${name}.host_kind`, HOST_KINDS)
  requireUUID(hostID, `${name}.host_id`)
}

function requireStringArray(value: unknown, name: string): readonly string[] {
  if (!Array.isArray(value) || !value.every((entry) => typeof entry === 'string')) {
    invalidArchive(`归档字段 ${name} 必须是字符串数组`)
  }
  return value
}

function requireObjectArray(value: unknown, name: string): readonly unknown[] {
  if (!Array.isArray(value)) invalidArchive(`归档字段 ${name} 必须是对象数组`)
  return value
}

function requireJSONRecord(value: unknown, name: string): Record<string, unknown> {
  return requireRecord(value, name)
}

function requireNullableJSONRecord(value: unknown, name: string): Record<string, unknown> | null {
  if (value === null) return null
  return requireJSONRecord(value, name)
}

function requireJSONArray(value: unknown, name: string): readonly unknown[] {
  if (!Array.isArray(value)) invalidArchive(`归档字段 ${name} 必须是数组`)
  return value
}

function validateSiteRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
	'id', 'site_key', 'name', 'intro', 'homepage_url', 'icon_url', 'user_note',
	'pinned', 'primary_entry_id',
	'revision', 'first_collected_at', 'last_collected_at',
    'created_at', 'updated_at',
  ])
  requireUUID(row.id, `${name}.id`)
  requireNonEmptyString(row.site_key, `${name}.site_key`)
  requireStringLength(row.name, `${name}.name`, 1, 256)
  requireStringLength(row.intro, `${name}.intro`, 0, 1000)
  const homepageURL = requireNullableString(row.homepage_url, `${name}.homepage_url`)
  if (homepageURL !== null) requireStringLength(homepageURL, `${name}.homepage_url`, 1, 2048)
  const iconURL = requireNullableString(row.icon_url, `${name}.icon_url`)
  if (iconURL !== null) requireStringLength(iconURL, `${name}.icon_url`, 1, 2048)
  requireStringLength(row.user_note, `${name}.user_note`, 0, 10000)
  requireBoolean(row.pinned, `${name}.pinned`)
  requireNullableUUID(row.primary_entry_id, `${name}.primary_entry_id`)
  requireSafeInteger(row.revision, `${name}.revision`, 1)
  requireDateTime(row.first_collected_at, `${name}.first_collected_at`)
  requireDateTime(row.last_collected_at, `${name}.last_collected_at`)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function requireNullableEnum(value: unknown, name: string, values: ReadonlySet<string>): string | null {
  if (value === null) return null
  return requireEnum(value, name, values)
}

function validateSiteEntryRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
	'id', 'site_id', 'link_id', 'entry_name', 'purpose', 'normalized_url',
	'first_collected_at', 'last_recollected_at',
    'created_at', 'updated_at',
  ])
  requireUUID(row.id, `${name}.id`)
  requireUUID(row.site_id, `${name}.site_id`)
  requireUUID(row.link_id, `${name}.link_id`)
  requireStringLength(row.entry_name, `${name}.entry_name`, 1, 256)
  requireStringLength(row.purpose, `${name}.purpose`, 0, 1000)
  requireStringLength(row.normalized_url, `${name}.normalized_url`, 1, 2048)
  requireDateTime(row.first_collected_at, `${name}.first_collected_at`)
  requireNullableDateTime(row.last_recollected_at, `${name}.last_recollected_at`)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateSiteTagRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
	'site_id', 'tag', 'normalized_tag', 'created_at', 'updated_at',
  ])
  requireUUID(row.site_id, `${name}.site_id`)
  requireStringLength(row.tag, `${name}.tag`, 1, 128)
  requireStringLength(row.normalized_tag, `${name}.normalized_tag`, 1, 128)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateSiteIdentityRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
	'identity_key', 'site_id', 'created_at', 'updated_at',
  ])
  requireNonEmptyString(row.identity_key, `${name}.identity_key`)
  requireUUID(row.site_id, `${name}.site_id`)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateThoughtRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'contract_version', 'id', 'host_kind', 'host_id', 'link_id', 'target', 'quote',
    'body', 'source', 'deleted', 'last_sequence', 'winner_key', 'created_at', 'updated_at',
  ])
  if (row.contract_version !== 1) invalidArchive(`归档字段 ${name}.contract_version 必须为 1`)
  requireThoughtIdentifier(row.id, `${name}.id`, 256)
  const deleted = requireBoolean(row.deleted, `${name}.deleted`)
  validateMaterializedThoughtHost(deleted, row.host_kind, row.host_id, name)
  requireNullableUUID(row.link_id, `${name}.link_id`)
  requireJSONRecord(row.target, `${name}.target`)
  requireNullableJSONRecord(row.quote, `${name}.quote`)
  requireString(row.body, `${name}.body`)
  requireString(row.source, `${name}.source`)
  requireSafeInteger(row.last_sequence, `${name}.last_sequence`, 1)
  const winner = requireExactRecord(row.winner_key, `${name}.winner_key`, [
    'logical_clock', 'device_id', 'op_id',
  ])
  requireSafeInteger(winner.logical_clock, `${name}.winner_key.logical_clock`, 0)
  requireThoughtIdentifier(winner.device_id, `${name}.winner_key.device_id`)
  requireThoughtIdentifier(winner.op_id, `${name}.winner_key.op_id`)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateThoughtOperationRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'contract_version', 'sequence', 'op_id', 'device_id', 'logical_clock',
    'operation_kind', 'annotation_id', 'host_kind', 'host_id', 'target', 'payload',
    'recovery_of', 'expected_current_winner_key', 'created_at',
  ])
  if (row.contract_version !== 1) invalidArchive(`归档字段 ${name}.contract_version 必须为 1`)
  requireSafeInteger(row.sequence, `${name}.sequence`, 1)
  requireThoughtIdentifier(row.op_id, `${name}.op_id`)
  requireThoughtIdentifier(row.device_id, `${name}.device_id`)
  requireSafeInteger(row.logical_clock, `${name}.logical_clock`, 0)
  const operationKind = requireEnum(row.operation_kind, `${name}.operation_kind`, THOUGHT_OPERATION_KINDS)
  requireThoughtIdentifier(row.annotation_id, `${name}.annotation_id`, 256)
  validateThoughtOperationHost(operationKind, row.host_kind, row.host_id, name)
  requireJSONRecord(row.target, `${name}.target`)
  requireJSONRecord(row.payload, `${name}.payload`)
  requireNullableJSONRecord(row.recovery_of, `${name}.recovery_of`)
  requireNullableJSONRecord(row.expected_current_winner_key, `${name}.expected_current_winner_key`)
  requireDateTime(row.created_at, `${name}.created_at`)
}

function validateThoughtSupersessionEventRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'sequence', 'annotation_id', 'loser', 'winner_at_detection', 'created_at',
  ])
  requireSafeInteger(row.sequence, `${name}.sequence`, 1)
  requireThoughtIdentifier(row.annotation_id, `${name}.annotation_id`, 256)
  requireJSONRecord(row.loser, `${name}.loser`)
  requireJSONRecord(row.winner_at_detection, `${name}.winner_at_detection`)
  requireDateTime(row.created_at, `${name}.created_at`)
}

function validateThoughtTombstoneRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'thought_id', 'host_kind', 'host_id', 'reason', 'snapshot', 'created_at',
  ])
  requireThoughtIdentifier(row.thought_id, `${name}.thought_id`, 256)
  requireEnum(row.host_kind, `${name}.host_kind`, HOST_KINDS)
  requireNonEmptyString(row.host_id, `${name}.host_id`)
  requireNonEmptyString(row.reason, `${name}.reason`)
  requireJSONRecord(row.snapshot, `${name}.snapshot`)
  requireDateTime(row.created_at, `${name}.created_at`)
}

function validateNoteRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'id', 'title', 'published_content', 'published_revision', 'draft_content',
    'draft_revision', 'draft_updated_at', 'deleted_at', 'created_at', 'updated_at',
  ])
  requireUUID(row.id, `${name}.id`)
  requireString(row.title, `${name}.title`)
  requireString(row.published_content, `${name}.published_content`)
  requireSafeInteger(row.published_revision, `${name}.published_revision`, 0)
  requireNullableString(row.draft_content, `${name}.draft_content`)
  requireSafeInteger(row.draft_revision, `${name}.draft_revision`, 0)
  requireNullableDateTime(row.draft_updated_at, `${name}.draft_updated_at`)
  requireNullableDateTime(row.deleted_at, `${name}.deleted_at`)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateNoteHistoryRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'id', 'note_id', 'revision', 'title', 'content', 'reanchor_ops', 'created_at',
  ])
  requireSafeInteger(row.id, `${name}.id`, 1)
  requireUUID(row.note_id, `${name}.note_id`)
  requireSafeInteger(row.revision, `${name}.revision`, 1)
  requireString(row.title, `${name}.title`)
  requireString(row.content, `${name}.content`)
  requireJSONArray(row.reanchor_ops, `${name}.reanchor_ops`)
  requireDateTime(row.created_at, `${name}.created_at`)
}

function validateInboxRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'id', 'url', 'source_kind', 'title', 'body', 'summary', 'suggested_tags', 'tags',
		'status', 'metadata_revision', 'expires_at', 'deleted_at',
    'created_at', 'updated_at',
  ])
  requireUUID(row.id, `${name}.id`)
  requireNonEmptyString(row.url, `${name}.url`)
  requireNonEmptyString(row.source_kind, `${name}.source_kind`)
  requireNullableString(row.title, `${name}.title`)
  requireString(row.body, `${name}.body`)
  requireNullableString(row.summary, `${name}.summary`)
  requireStringArray(row.suggested_tags, `${name}.suggested_tags`)
  requireStringArray(row.tags, `${name}.tags`)
  requireEnum(row.status, `${name}.status`, INBOX_STATUSES)
  requireSafeInteger(row.metadata_revision, `${name}.metadata_revision`, 1)
	requireNullableDateTime(row.expires_at, `${name}.expires_at`)
  requireNullableDateTime(row.deleted_at, `${name}.deleted_at`)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateTodoRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'id', 'text', 'due_at', 'done', 'origin_kind', 'origin_host_kind', 'origin_host_id',
    'origin_ref', 'host_revision', 'completed_at', 'deleted_at', 'created_at', 'updated_at',
  ])
  requireUUID(row.id, `${name}.id`)
  requireString(row.text, `${name}.text`)
  requireNullableDateTime(row.due_at, `${name}.due_at`)
  requireBoolean(row.done, `${name}.done`)
  const originKind = requireEnum(row.origin_kind, `${name}.origin_kind`, TODO_ORIGIN_KINDS)
  const originHostKind = requireNullableEnum(
    row.origin_host_kind,
    `${name}.origin_host_kind`,
    TODO_ORIGIN_HOST_KINDS,
  )
  if (originKind === 'standalone') {
    if (originHostKind !== null || row.origin_host_id !== null) {
      invalidArchive(`归档字段 ${name} 的 standalone 来源不能携带宿主`)
    }
  } else {
    if (originHostKind !== originKind) {
      invalidArchive(`归档字段 ${name}.origin_host_kind 与 origin_kind 不一致`)
    }
    if (originHostKind === 'thought') {
      requireThoughtIdentifier(row.origin_host_id, `${name}.origin_host_id`, 256)
    } else {
      requireUUID(row.origin_host_id, `${name}.origin_host_id`)
    }
  }
  requireNullableJSONRecord(row.origin_ref, `${name}.origin_ref`)
  requireSafeInteger(row.host_revision, `${name}.host_revision`, 0)
  requireNullableDateTime(row.completed_at, `${name}.completed_at`)
  requireNullableDateTime(row.deleted_at, `${name}.deleted_at`)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateEngagementRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'link_id', 'read', 'progress', 'read_later', 'last_opened', 'updated_at',
  ])
  requireUUID(row.link_id, `${name}.link_id`)
  requireBoolean(row.read, `${name}.read`)
  requireFiniteNumber(row.progress, `${name}.progress`, 0, 1)
  requireBoolean(row.read_later, `${name}.read_later`)
  requireNullableDateTime(row.last_opened, `${name}.last_opened`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateFeedFolderRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, ['id', 'name', 'created_at', 'updated_at'])
  requireUUID(row.id, `${name}.id`)
  requireStringLength(row.name, `${name}.name`, 1, 128)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateFeedSubscriptionRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'id', 'folder_id', 'url', 'canonical_url', 'site_url', 'title', 'description',
    'active', 'created_at', 'updated_at',
  ])
  requireUUID(row.id, `${name}.id`)
  requireNullableUUID(row.folder_id, `${name}.folder_id`)
  requireStringLength(row.url, `${name}.url`, 1, 2048)
  const canonicalURL = requireNullableString(row.canonical_url, `${name}.canonical_url`)
  if (canonicalURL !== null) requireStringLength(canonicalURL, `${name}.canonical_url`, 1, 2048)
  const siteURL = requireNullableString(row.site_url, `${name}.site_url`)
  if (siteURL !== null) requireStringLength(siteURL, `${name}.site_url`, 1, 2048)
  requireString(row.title, `${name}.title`)
  requireNullableString(row.description, `${name}.description`)
  requireBoolean(row.active, `${name}.active`)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateFeedItemRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'id', 'subscription_id', 'external_id', 'url', 'title', 'author', 'summary',
    'content_text', 'content_html', 'published_at', 'read_at', 'starred',
    'read_later', 'link_id', 'created_at', 'updated_at',
  ])
  requireUUID(row.id, `${name}.id`)
  requireUUID(row.subscription_id, `${name}.subscription_id`)
  requireNonEmptyString(row.external_id, `${name}.external_id`)
  requireString(row.url, `${name}.url`)
  requireString(row.title, `${name}.title`)
  requireNullableString(row.author, `${name}.author`)
  requireNullableString(row.summary, `${name}.summary`)
  requireNullableString(row.content_text, `${name}.content_text`)
  requireNullableString(row.content_html, `${name}.content_html`)
  requireNullableDateTime(row.published_at, `${name}.published_at`)
  requireNullableDateTime(row.read_at, `${name}.read_at`)
  requireBoolean(row.starred, `${name}.starred`)
  requireBoolean(row.read_later, `${name}.read_later`)
  requireNullableUUID(row.link_id, `${name}.link_id`)
  requireDateTime(row.created_at, `${name}.created_at`)
  requireDateTime(row.updated_at, `${name}.updated_at`)
}

function validateFeedSaveRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, [
    'feed_item_id', 'link_id', 'created_at',
  ])
  requireUUID(row.feed_item_id, `${name}.feed_item_id`)
  requireUUID(row.link_id, `${name}.link_id`)
  requireDateTime(row.created_at, `${name}.created_at`)
}

function validateFeedHideRow(value: unknown, name: string): void {
  const row = requireExactRecord(value, name, ['item_key', 'created_at'])
  requireNonEmptyString(row.item_key, `${name}.item_key`)
  requireDateTime(row.created_at, `${name}.created_at`)
}

const READER_ROW_VALIDATORS: Readonly<Record<ReaderArchiveSection, ArchiveRowValidator>> = {
  feed_folders: validateFeedFolderRow,
  feed_subscriptions: validateFeedSubscriptionRow,
  feed_items: validateFeedItemRow,
  feed_saves: validateFeedSaveRow,
  inbox: validateInboxRow,
  todos: validateTodoRow,
  engagement: validateEngagementRow,
  feed_hides: validateFeedHideRow,
  thoughts: validateThoughtRow,
  thought_ops: validateThoughtOperationRow,
  thought_supersession_events: validateThoughtSupersessionEventRow,
  thought_tombstones: validateThoughtTombstoneRow,
  notes: validateNoteRow,
  note_history: validateNoteHistoryRow,
}

function normalizeSelection(selection: ArchiveV2Selection): NormalizedArchiveV2Selection {
  if (!isRecord(selection) || !hasExactKeysForOptionalSelection(selection)) {
    invalidArchive('归档分区选择无效')
  }
  const { includeThoughts, includeNotes } = selection
  if (
    (includeThoughts !== undefined && typeof includeThoughts !== 'boolean') ||
    (includeNotes !== undefined && typeof includeNotes !== 'boolean')
  ) {
    invalidArchive('归档分区选择无效')
  }
  return Object.freeze({
    includeThoughts: includeThoughts === true,
    includeNotes: includeNotes === true,
  })
}

function hasExactKeysForOptionalSelection(value: Record<string, unknown>): boolean {
  return Object.keys(value).every(
    (key) => key === 'includeThoughts' || key === 'includeNotes',
  )
}

function selectionTokens(selection: NormalizedArchiveV2Selection): readonly string[] {
  const tokens = ['base']
  if (selection.includeThoughts) tokens.push('thoughts')
  if (selection.includeNotes) tokens.push('notes')
  return tokens
}

function selectedReaderSections(selection: NormalizedArchiveV2Selection): readonly ReaderArchiveSection[] {
  const sections: ReaderArchiveSection[] = [...READER_BASE_SECTIONS]
  if (selection.includeThoughts) sections.push(...READER_THOUGHT_SECTIONS)
  if (selection.includeNotes) sections.push(...READER_NOTE_SECTIONS)
  return sections
}

/** Return the only query representation accepted by the v2 endpoint. */
export function archiveV2Sections(
  selection: ArchiveV2Selection = fullArchiveV2Selection,
): string {
  return selectionTokens(normalizeSelection(selection)).join(',')
}

function requireExactStringArray(
  value: unknown,
  expected: readonly string[],
  name: string,
): void {
  if (
    !Array.isArray(value) ||
    value.length !== expected.length ||
    value.some((entry, index) => entry !== expected[index])
  ) {
    invalidArchive(`归档字段 ${name} 与请求的分区不一致`)
  }
}

function validateTopLevelRows(
  archive: Record<string, unknown>,
  counts: Map<string, number>,
): void {
  const validators: Readonly<Record<(typeof TOP_LEVEL_ARRAY_SECTIONS)[number], ArchiveRowValidator>> = {
    links: (value, name) => {
      if (!isLinkResponse(value)) invalidArchive(`归档记录 ${name} 不符合完整 LinkResponse 合同`)
    },
    sites: validateSiteRow,
    site_entries: validateSiteEntryRow,
    site_tags: validateSiteTagRow,
    site_identities: validateSiteIdentityRow,
  }
  for (const section of TOP_LEVEL_ARRAY_SECTIONS) {
    const rows = requireObjectArray(archive[section], section)
    rows.forEach((row, index) => validators[section](row, `${section}[${index}]`))
    counts.set(section, rows.length)
  }
}

function validateReaderRows(
  reader: Record<string, unknown>,
  selection: NormalizedArchiveV2Selection,
  counts: Map<string, number>,
  archive: Record<string, unknown>,
): void {
  const sections = selectedReaderSections(selection)
  const expectedKeys = ['schema_version', 'thought_contract_version', ...sections]
  if (!hasExactKeys(reader, expectedKeys)) {
    invalidArchive('归档 reader 分区与请求的分区不一致')
  }
  if (reader.schema_version !== 2 || reader.thought_contract_version !== 1) {
    invalidArchive('归档 reader schema 不符合 v2 合同')
  }
  for (const section of sections) {
    const rows = requireObjectArray(reader[section], `reader.${section}`)
    const validator = READER_ROW_VALIDATORS[section]
    rows.forEach((row, index) => validator(row, `reader.${section}[${index}]`))
    counts.set(`reader.${section}`, rows.length)
  }
  validateFeedRelationships(reader, archive)
}

function collectUniqueIDs(rows: readonly unknown[], field: string, name: string): Set<string> {
  const ids = new Set<string>()
  rows.forEach((value, index) => {
    const row = requireRecord(value, `${name}[${index}]`)
    const id = requireUUID(row[field], `${name}[${index}].${field}`)
    if (ids.has(id)) invalidArchive(`归档 ${name} 包含重复标识 ${id}`)
    ids.add(id)
  })
  return ids
}

function validateFeedRelationships(reader: Record<string, unknown>, archive: Record<string, unknown>): void {
  const folders = requireObjectArray(reader.feed_folders, 'reader.feed_folders')
  const subscriptions = requireObjectArray(reader.feed_subscriptions, 'reader.feed_subscriptions')
  const items = requireObjectArray(reader.feed_items, 'reader.feed_items')
  const saves = requireObjectArray(reader.feed_saves, 'reader.feed_saves')
  const links = requireObjectArray(archive.links, 'links')
  const folderIDs = collectUniqueIDs(folders, 'id', 'reader.feed_folders')
  const subscriptionIDs = collectUniqueIDs(subscriptions, 'id', 'reader.feed_subscriptions')
  const itemIDs = collectUniqueIDs(items, 'id', 'reader.feed_items')
  const linkIDs = collectUniqueIDs(links, 'id', 'links')
  const savedItemIDs = new Set<string>()

  subscriptions.forEach((value, index) => {
    const row = requireRecord(value, `reader.feed_subscriptions[${index}]`)
    const folderID = row.folder_id
    if (folderID !== null && !folderIDs.has(requireUUID(folderID, `reader.feed_subscriptions[${index}].folder_id`))) {
      invalidArchive(`归档 reader.feed_subscriptions[${index}].folder_id 引用不存在`)
    }
  })
  items.forEach((value, index) => {
    const row = requireRecord(value, `reader.feed_items[${index}]`)
    if (!subscriptionIDs.has(requireUUID(row.subscription_id, `reader.feed_items[${index}].subscription_id`))) {
      invalidArchive(`归档 reader.feed_items[${index}].subscription_id 引用不存在`)
    }
    const linkID = row.link_id
    if (linkID !== null && !linkIDs.has(requireUUID(linkID, `reader.feed_items[${index}].link_id`))) {
      invalidArchive(`归档 reader.feed_items[${index}].link_id 引用不存在`)
    }
  })
  saves.forEach((value, index) => {
    const row = requireRecord(value, `reader.feed_saves[${index}]`)
    const itemID = requireUUID(row.feed_item_id, `reader.feed_saves[${index}].feed_item_id`)
    if (!itemIDs.has(itemID)) invalidArchive(`归档 reader.feed_saves[${index}].feed_item_id 引用不存在`)
    if (savedItemIDs.has(itemID)) invalidArchive(`归档 reader.feed_saves 包含重复 feed_item_id ${itemID}`)
    savedItemIDs.add(itemID)
    const linkID = requireUUID(row.link_id, `reader.feed_saves[${index}].link_id`)
    if (!linkIDs.has(linkID)) invalidArchive(`归档 reader.feed_saves[${index}].link_id 引用不存在`)
  })
}

function validateArchiveShape(
  value: unknown,
  selection: NormalizedArchiveV2Selection,
  expectedNamespace: string,
): string {
  if (!isRecord(value)) invalidArchive('归档顶层结构不符合 v2 合同')
  const hasReader = Object.prototype.hasOwnProperty.call(value, 'reader')
  const expectedTopLevelKeys = hasReader
    ? ARCHIVE_TOP_LEVEL_KEYS_WITH_READER
    : ARCHIVE_TOP_LEVEL_KEYS_WITHOUT_READER
  if (!hasExactKeys(value, expectedTopLevelKeys)) {
    invalidArchive('归档顶层结构不符合 v2 合同')
  }
  if ((selection.includeThoughts || selection.includeNotes) && !hasReader) {
    invalidArchive('所选的归档分区缺少 reader 数据')
  }
  if (value.schema_version !== 2) invalidArchive('归档 schema_version 必须为 2')
  requireDateTime(value.exported_at, 'exported_at')
  if (value.generator_version !== 'webtag') {
    invalidArchive('归档 generator_version 必须为 webtag')
  }

  const actualCounts = new Map<string, number>()
  validateTopLevelRows(value, actualCounts)
  if (hasReader) validateReaderRows(requireRecord(value.reader, 'reader'), selection, actualCounts, value)

  const manifest = requireExactRecord(value.manifest, 'manifest', MANIFEST_KEYS)
  if (!CLIENT_DATA_NAMESPACE_PATTERN.test(expectedNamespace)) {
    invalidArchive('归档校验身份命名空间格式无效')
  }
  if (
    typeof manifest.client_data_namespace !== 'string' ||
    !CLIENT_DATA_NAMESPACE_PATTERN.test(manifest.client_data_namespace) ||
    manifest.client_data_namespace !== expectedNamespace
  ) {
    invalidArchive('归档 manifest 身份命名空间不匹配')
  }
  requireExactStringArray(manifest.sections, selectionTokens(selection), 'manifest.sections')
  const manifestCounts = requireRecord(manifest.counts, 'manifest.counts')
  const countKeys = [...actualCounts.keys()]
  if (!hasExactKeys(manifestCounts, countKeys)) {
    invalidArchive('归档 manifest.counts 分区不完整或包含未知分区')
  }
  for (const [section, actualCount] of actualCounts) {
    const declaredCount = manifestCounts[section]
    if (
      typeof declaredCount !== 'number' ||
      !Number.isSafeInteger(declaredCount) ||
      declaredCount < 0 ||
      declaredCount !== actualCount
    ) {
      invalidArchive(`归档 manifest.counts.${section} 与实际内容不一致`)
    }
  }
  if (typeof manifest.checksum_sha256 !== 'string' || !SHA256_HEX_PATTERN.test(manifest.checksum_sha256)) {
    invalidArchive('归档 manifest.checksum_sha256 格式无效')
  }
  return manifest.checksum_sha256
}

const BYTE_QUOTE = 0x22
const BYTE_BACKSLASH = 0x5c
const BYTE_COMMA = 0x2c
const BYTE_COLON = 0x3a
const BYTE_OPEN_OBJECT = 0x7b
const BYTE_CLOSE_OBJECT = 0x7d
const BYTE_OPEN_ARRAY = 0x5b
const BYTE_CLOSE_ARRAY = 0x5d

function isJSONWhitespace(byte: number | undefined): boolean {
  return byte === 0x20 || byte === 0x09 || byte === 0x0a || byte === 0x0d
}

function skipJSONWhitespace(bytes: Uint8Array, offset: number): number {
  let index = offset
  while (isJSONWhitespace(bytes[index])) index += 1
  return index
}

function hexValue(byte: number | undefined): number {
  if (byte === undefined) return -1
  if (byte >= 0x30 && byte <= 0x39) return byte - 0x30
  if (byte >= 0x41 && byte <= 0x46) return byte - 0x41 + 10
  if (byte >= 0x61 && byte <= 0x66) return byte - 0x61 + 10
  return -1
}

function escapedCodeUnit(bytes: Uint8Array, offset: number): number {
  let value = 0
  for (let index = 0; index < 4; index += 1) {
    const digit = hexValue(bytes[offset + index])
    if (digit < 0) invalidArchive('归档 JSON 字符串包含无效 unicode 转义')
    value = value * 16 + digit
  }
  return value
}

function decodeJSONString(bytes: Uint8Array, start: number, end: number): string {
  try {
    const decoded = new TextDecoder('utf-8', { fatal: true }).decode(bytes.slice(start, end))
    const value: unknown = JSON.parse(decoded)
    if (typeof value !== 'string') invalidArchive('归档 JSON key 不是字符串')
    return value
  } catch (error) {
    if (error instanceof ArchiveV2ValidationError) throw error
    invalidArchive('归档 JSON 字符串无效')
  }
}

/**
 * Scan a JSON string token without relying on a textual spelling of any key.
 * JSON.parse accepts unpaired UTF-16 escape units, but an archive identifier
 * may never contain them, so reject malformed surrogate escapes at the byte
 * boundary before parsing any semantic object.
 */
function scanJSONString(bytes: Uint8Array, start: number): { readonly end: number; readonly value: string } {
  if (bytes[start] !== BYTE_QUOTE) invalidArchive('归档 JSON 预期字符串')
  let index = start + 1
  while (index < bytes.length) {
    const byte = bytes[index]
    if (byte === BYTE_QUOTE) {
      const end = index + 1
      return { end, value: decodeJSONString(bytes, start, end) }
    }
    if (byte === BYTE_BACKSLASH) {
      const escape = bytes[index + 1]
      if (escape === 0x75) {
        const unit = escapedCodeUnit(bytes, index + 2)
        if (unit >= 0xd800 && unit <= 0xdbff) {
          const nextSlash = index + 6
          if (bytes[nextSlash] !== BYTE_BACKSLASH || bytes[nextSlash + 1] !== 0x75) {
            invalidArchive('归档 JSON 字符串包含未配对的高位代理项')
          }
          const low = escapedCodeUnit(bytes, nextSlash + 2)
          if (low < 0xdc00 || low > 0xdfff) {
            invalidArchive('归档 JSON 字符串包含未配对的高位代理项')
          }
          index = nextSlash + 6
          continue
        }
        if (unit >= 0xdc00 && unit <= 0xdfff) {
          invalidArchive('归档 JSON 字符串包含未配对的低位代理项')
        }
        index += 6
        continue
      }
      if (
        escape !== BYTE_QUOTE && escape !== BYTE_BACKSLASH && escape !== 0x2f &&
        escape !== 0x62 && escape !== 0x66 && escape !== 0x6e && escape !== 0x72 && escape !== 0x74
      ) {
        invalidArchive('归档 JSON 字符串包含无效转义')
      }
      index += 2
      continue
    }
    if (byte === undefined || byte < 0x20) invalidArchive('归档 JSON 字符串包含控制字符')
    index += 1
  }
  invalidArchive('归档 JSON 字符串未结束')
}

/** Return the comma or root-object close token after one top-level value. */
function scanTopLevelValueBoundary(bytes: Uint8Array, start: number): number {
  let index = start
  let depth = 0
  while (index < bytes.length) {
    const byte = bytes[index]
    if (byte === BYTE_QUOTE) {
      index = scanJSONString(bytes, index).end
      continue
    }
    if (byte === BYTE_OPEN_OBJECT || byte === BYTE_OPEN_ARRAY) {
      depth += 1
      index += 1
      continue
    }
    if (byte === BYTE_CLOSE_OBJECT) {
      if (depth === 0) return index
      depth -= 1
      index += 1
      continue
    }
    if (byte === BYTE_CLOSE_ARRAY) {
      if (depth === 0) invalidArchive('归档 JSON 顶层对象意外结束')
      depth -= 1
      index += 1
      continue
    }
    if (byte === BYTE_COMMA && depth === 0) return index
    index += 1
  }
  invalidArchive('归档 JSON 顶层对象未结束')
}

/**
 * Finds top-level members by JSON tokens, not byte-string matching. This
 * catches escaped spellings such as `mani\\u0066est`, arbitrary whitespace,
 * and duplicate keys before JSON.parse could silently retain only the last.
 */
function scanTopLevelMembers(bytes: Uint8Array): readonly TopLevelMember[] {
  let index = skipJSONWhitespace(bytes, 0)
  if (bytes[index] !== BYTE_OPEN_OBJECT) invalidArchive('归档顶层必须是 JSON 对象')
  index = skipJSONWhitespace(bytes, index + 1)
  const members: TopLevelMember[] = []
  let separatorOffset: number | null = null

  if (bytes[index] === BYTE_CLOSE_OBJECT) {
    index = skipJSONWhitespace(bytes, index + 1)
    if (index !== bytes.length) invalidArchive('归档 JSON 在顶层对象后包含额外字节')
    return members
  }

  while (index < bytes.length) {
    const keyToken = scanJSONString(bytes, index)
    index = skipJSONWhitespace(bytes, keyToken.end)
    if (bytes[index] !== BYTE_COLON) invalidArchive('归档 JSON 顶层 key 后缺少冒号')
    index = skipJSONWhitespace(bytes, index + 1)
    if (index >= bytes.length || bytes[index] === BYTE_COMMA || bytes[index] === BYTE_CLOSE_OBJECT) {
      invalidArchive('归档 JSON 顶层 key 缺少值')
    }
    const boundary = scanTopLevelValueBoundary(bytes, index)
    members.push({ key: keyToken.value, separatorOffset })
    index = skipJSONWhitespace(bytes, boundary)
    if (bytes[index] === BYTE_COMMA) {
      separatorOffset = index
      index = skipJSONWhitespace(bytes, index + 1)
      if (bytes[index] === BYTE_CLOSE_OBJECT) invalidArchive('归档 JSON 顶层对象含有尾随逗号')
      continue
    }
    if (bytes[index] === BYTE_CLOSE_OBJECT) {
      index = skipJSONWhitespace(bytes, index + 1)
      if (index !== bytes.length) invalidArchive('归档 JSON 在顶层对象后包含额外字节')
      return members
    }
    invalidArchive('归档 JSON 顶层成员之间缺少逗号')
  }
  invalidArchive('归档 JSON 顶层对象未结束')
}

function findManifestPrefixEnd(bytes: Uint8Array): number {
  const members = scanTopLevelMembers(bytes)
  const seen = new Set<string>()
  for (const member of members) {
    if (seen.has(member.key)) invalidArchive(`归档包含重复的顶层 key ${member.key}`)
    seen.add(member.key)
  }
  const manifestMembers = members.filter((member) => member.key === 'manifest')
  const finalMember = members[members.length - 1]
  if (manifestMembers.length !== 1 || finalMember?.key !== 'manifest' || finalMember.separatorOffset === null) {
    invalidArchive('归档必须只有一个最终 manifest 顶层字段')
  }
  return finalMember.separatorOffset
}

async function sha256Hex(bytes: Uint8Array): Promise<string> {
  if (!globalThis.crypto?.subtle) {
    invalidArchive('浏览器不支持归档 SHA-256 校验')
  }
  let digest: ArrayBuffer
  try {
    // DOM's BufferSource excludes SharedArrayBuffer views. Copying here makes
    // the Web Crypto boundary explicit without altering any archive bytes.
    const digestInput = new Uint8Array(bytes.byteLength)
    digestInput.set(bytes)
    digest = await globalThis.crypto.subtle.digest('SHA-256', digestInput)
  } catch {
    invalidArchive('归档 SHA-256 校验失败')
  }
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')
}

/**
 * Validate an archive from its exact received bytes. It intentionally returns
 * no parsed archive: callers may only persist/download the raw bytes after
 * this succeeds.
 */
export async function validateArchiveV2Bytes(
  bytes: Uint8Array,
  options: ArchiveV2ValidationOptions,
): Promise<void> {
  if (!isArchiveV2ByteArray(bytes)) {
    invalidArchive('归档响应不是字节流')
  }
  if (bytes.byteLength > ARCHIVE_V2_MAX_BYTES) archiveTooLarge()
  if (!isRecord(options) || typeof options.clientDataNamespace !== 'string' || !CLIENT_DATA_NAMESPACE_PATTERN.test(options.clientDataNamespace)) {
    invalidArchive('归档校验缺少有效身份命名空间')
  }

  const selection = normalizeSelection(options.selection ?? fullArchiveV2Selection)
  let text: string
  try {
    text = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    invalidArchive('归档不是有效的 UTF-8')
  }

  const manifestPrefixEnd = findManifestPrefixEnd(bytes)
  let archive: unknown
  try {
    archive = JSON.parse(text)
  } catch {
    invalidArchive('归档不是完整的 JSON')
  }

  const expectedChecksum = validateArchiveShape(
    archive,
    selection,
    options.clientDataNamespace,
  )
  const actualChecksum = await sha256Hex(bytes.slice(0, manifestPrefixEnd))
  if (actualChecksum !== expectedChecksum) {
    invalidArchive('归档 manifest 校验和不匹配')
  }
}
