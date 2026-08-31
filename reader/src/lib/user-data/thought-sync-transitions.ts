import type { ApiError } from '@webtag/api'

import {
  ThoughtClockError,
  maximumThoughtClock,
  nextThoughtLogicalClock,
} from './thought-clock'
import {
  THOUGHT_CONTRACT_VERSION,
  type ThoughtHistoryOutboxRecord,
  type ThoughtMaterializedRecord,
  type ThoughtOutboxRecord,
  type ThoughtSyncStateRecord,
} from './thought-types'

export const MAX_THOUGHT_PULL_PAGES_PER_ROUND = 20
export const BASE_THOUGHT_RETRY_MS = 1000
const MAX_THOUGHT_RETRY_MS = 5 * 60 * 1000

export type ThoughtSyncTransitionPhase = 'offline' | 'syncing' | 'failed' | 'pending' | 'synced'

export interface ThoughtSyncTransitionRecord {
  readonly opId: string
  readonly logicalClock: number
  readonly deviceId: string
  readonly status?: 'pending' | 'blocked'
  readonly nextAttemptAt?: number
  readonly blockedReason?: string
}

export type ThoughtSyncTransitionState = Pick<
  ThoughtSyncStateRecord,
  | 'cursor'
  | 'pullRetryAt'
  | 'pullInProgress'
  | 'lastSuccessfulSyncAt'
  | 'lastError'
  | 'pullLastError'
  | 'lastErrorCode'
>

export interface ThoughtSyncTransitionSnapshot {
  readonly phase: ThoughtSyncTransitionPhase
  readonly pendingCount: number
  readonly blockedCount: number
  readonly retryAt?: number
  readonly lastSuccessfulSyncAt?: number
  readonly errorCode?: string
}

export type ThoughtClockAllocationTransitionFailure =
  | 'invalid-thought-clock'
  | 'thought-clock-exhausted'
  | 'invalid-thought-sync-state'

export type ThoughtClockAllocationTransition =
  | {
      readonly ok: true
      readonly deviceId: string
      readonly clocks: readonly number[]
      readonly state: ThoughtSyncStateRecord
    }
  | {
      readonly ok: false
      readonly reason: ThoughtClockAllocationTransitionFailure
    }

export interface ThoughtClockAllocationInput {
  readonly namespace: string
  readonly rawState: unknown
  readonly deviceId: string
  readonly tabToken: string
  readonly count: number
  readonly now: number
  readonly outbox: readonly Pick<ThoughtOutboxRecord, 'logicalClock'>[]
  readonly historyOutbox: readonly Pick<ThoughtHistoryOutboxRecord, 'logicalClock' | 'snapshot'>[]
  readonly materialized?: readonly Pick<ThoughtMaterializedRecord, 'winnerKey'>[]
  readonly observedClocks?: readonly number[]
}

export interface ThoughtVersionKeyTransition {
  readonly logical_clock: number
  readonly device_id: string
  readonly op_id: string
}

export interface ThoughtAckTransitionResponse {
  readonly contract_version: unknown
  readonly op_id: unknown
  readonly sequence: unknown
  readonly disposition: unknown
  readonly submitted_key: unknown
  readonly current_winner_key: unknown
}

export type ThoughtAckTransition =
  | {
      readonly ok: true
      readonly completedIDs: readonly string[]
      readonly dispositions: readonly ThoughtAckDisposition[]
      readonly lastAckSequence: number
      readonly logicalClockFloor: number
    }
  | {
      readonly ok: false
      readonly reason:
        | 'ack-count-mismatch'
        | 'ack-op-id-mismatch'
        | 'ack-contract-mismatch'
        | 'ack-disposition-mismatch'
        | 'ack-sequence-invalid'
        | 'ack-submitted-key-mismatch'
        | 'ack-winner-key-invalid'
    }

export interface ThoughtAckDisposition {
  readonly opId: string
  readonly disposition: 'applied' | 'superseded' | 'duplicate'
}

export type ThoughtHistoryAckRecordTransition =
  | {
      readonly action: 'delete'
      readonly key: ThoughtHistoryOutboxRecord['key']
      readonly opId: string
    }
  | {
      readonly action: 'put'
      readonly record: ThoughtHistoryOutboxRecord
      readonly opId: string
    }

export interface ThoughtOutboxFailureTransitionInput {
  readonly record: ThoughtOutboxRecord
  readonly error: ApiError
  readonly now: number
}

export interface ThoughtHistoryOutboxFailureTransitionInput {
  readonly record: ThoughtHistoryOutboxRecord
  readonly error: ApiError
  readonly now: number
}

export interface ThoughtPullCursorState {
  readonly pageIndex: number
  readonly cursor: string
  readonly pulled: number
}

export type ThoughtPullCursorTransition =
  | {
      readonly status: 'continue' | 'complete' | 'incomplete'
      readonly cursor: string
      readonly pulled: number
    }
  | {
      readonly status: 'stale'
      readonly cursor: string
      readonly pulled: number
    }

export type RevokedThoughtTransitionPhase = 'push' | 'pull' | 'later-page'

export type RevokedThoughtTransition =
  | {
      readonly status: 'stale'
      readonly phase: RevokedThoughtTransitionPhase
      readonly cursor: string
      readonly pushed: number
      readonly pulled: number
      readonly pending: number
    }
  | {
      readonly status: 'active'
      readonly phase: RevokedThoughtTransitionPhase
      readonly cursor: string
      readonly pushed: number
      readonly pulled: number
      readonly pending: number
    }

const STABLE_FAILURE_CODE = /^[a-z0-9][a-z0-9._-]{0,63}(?::[a-z0-9][a-z0-9._-]{0,63}){0,2}$/
const STABLE_ERROR_COMPONENT = /^[a-z0-9][a-z0-9._-]{0,63}$/

function isSafeNonNegativeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined
}

function optionalNumber(record: Record<string, unknown> | undefined, key: string): Record<string, number> {
  const value = record?.[key]
  return typeof value === 'number' ? { [key]: value } : {}
}

function optionalString(record: Record<string, unknown> | undefined, key: string): Record<string, string> {
  const value = record?.[key]
  return typeof value === 'string' ? { [key]: value } : {}
}

function optionalBoolean(record: Record<string, unknown> | undefined, key: string): Record<string, boolean> {
  const value = record?.[key]
  return typeof value === 'boolean' ? { [key]: value } : {}
}

type Mutable<T> = { -readonly [K in keyof T]: T[K] }

function withoutFields<T extends object, K extends keyof T>(
  record: T,
  keys: readonly K[],
): Omit<T, K> {
  const copy = { ...record } as Partial<Mutable<T>>
  for (const key of keys) delete copy[key]
  return copy as Omit<T, K>
}

export function storedThoughtFailureCode(value: unknown): string | undefined {
  return typeof value === 'string' && value.length <= 128 && STABLE_FAILURE_CODE.test(value)
    ? value
    : undefined
}

function stableThoughtErrorComponent(value: unknown): string | undefined {
  return typeof value === 'string' && STABLE_ERROR_COMPONENT.test(value) ? value : undefined
}

/**
 * Payload-free failure classification stored in durable sync state. Transport
 * messages can contain request context, so they never cross this interface.
 */
export function thoughtSyncFailureCode(error: ApiError): string {
  const serverCode = stableThoughtErrorComponent(error.errorCode)
  const status = Number.isSafeInteger(error.status) && (error.status as number) >= 100 &&
    (error.status as number) <= 599
    ? String(error.status)
    : undefined
  return [
    error.kind,
    serverCode,
    status,
  ]
    .filter((value): value is string => value !== undefined && value.length > 0)
    .join(':')
}

function thoughtRetryDelay(error: ApiError, attemptCount: number): number {
  if (
    error.retryAfterSeconds !== undefined &&
    Number.isFinite(error.retryAfterSeconds) &&
    error.retryAfterSeconds >= 0
  ) {
    return Math.ceil(error.retryAfterSeconds * 1000)
  }
  return Math.min(
    MAX_THOUGHT_RETRY_MS,
    BASE_THOUGHT_RETRY_MS * (2 ** Math.min(Math.max(attemptCount - 1, 0), 8)),
  )
}

export function isPermanentThoughtPushFailure(error: ApiError): boolean {
  if (error.kind === 'unauthorized') return true
  if (error.kind !== 'other' || error.status === undefined) return false
  return error.status >= 400 && error.status < 500 &&
    error.status !== 408 && error.status !== 425 && error.status !== 429
}

/** Only request-validation 4xx responses can safely identify a single poison op by splitting. */
export function isIsolatablePermanentThoughtPushFailure(error: ApiError): boolean {
  return error.kind === 'other' && error.status !== undefined &&
    error.status >= 400 && error.status < 500 &&
    error.status !== 408 && error.status !== 425 && error.status !== 429
}

export function blockedThoughtFailureCode(
  records: readonly Pick<ThoughtSyncTransitionRecord, 'status' | 'blockedReason'>[],
): string | undefined {
  const blocked = records.find((record) => record.status === 'blocked')
  if (!blocked) return undefined
  return storedThoughtFailureCode(blocked.blockedReason) ?? 'blocked-operation'
}

function earliestThoughtRetryAt(
  records: readonly Pick<ThoughtSyncTransitionRecord, 'status' | 'nextAttemptAt'>[],
): number | undefined {
  return records.reduce<number | undefined>((earliest, record) => {
    if (record.status === 'blocked') return earliest
    if (record.nextAttemptAt === undefined) return earliest
    return earliest === undefined ? record.nextAttemptAt : Math.min(earliest, record.nextAttemptAt)
  }, undefined)
}

export function thoughtRetryAt(
  records: readonly Pick<ThoughtSyncTransitionRecord, 'status' | 'nextAttemptAt'>[],
  state: Pick<ThoughtSyncTransitionState, 'pullRetryAt'>,
): number | undefined {
  const outboxRetryAt = earliestThoughtRetryAt(records)
  if (outboxRetryAt === undefined) return state.pullRetryAt
  if (state.pullRetryAt === undefined) return outboxRetryAt
  return Math.min(outboxRetryAt, state.pullRetryAt)
}

export function earliestThoughtRetryTime(
  outboxRetryAt: number | undefined,
  pullRetryAt: number | undefined,
): number | undefined {
  if (outboxRetryAt === undefined) return pullRetryAt
  if (pullRetryAt === undefined) return outboxRetryAt
  return Math.min(outboxRetryAt, pullRetryAt)
}

export function normalizeThoughtOutboxFailureMarkers<T extends Pick<
  ThoughtSyncTransitionRecord,
  'status' | 'blockedReason'
> & { readonly lastError?: string }>(record: T): T {
  const lastError = record.lastError === undefined
    ? undefined
    : storedThoughtFailureCode(record.lastError) ?? 'sync-failed'
  const blockedReason = record.status === 'blocked'
    ? storedThoughtFailureCode(record.blockedReason) ?? 'blocked-operation'
    : undefined
  if (lastError === record.lastError && blockedReason === record.blockedReason) return record
  const base = withoutFields(record, ['lastError', 'blockedReason'])
  return {
    ...base,
    ...(lastError === undefined ? {} : { lastError }),
    ...(blockedReason === undefined ? {} : { blockedReason }),
  } as T
}

export function allocateThoughtClockTransition(
  input: ThoughtClockAllocationInput,
): ThoughtClockAllocationTransition {
  if (!Number.isSafeInteger(input.count) || input.count < 0) {
    return { ok: false, reason: 'invalid-thought-clock' }
  }
  const rawState = asRecord(input.rawState)
  if (rawState && rawState.namespace !== input.namespace) {
    return { ok: false, reason: 'invalid-thought-sync-state' }
  }
  try {
    const priorFloor = rawState?.deviceId === input.deviceId &&
      typeof rawState.logicalClockFloor === 'number'
      ? rawState.logicalClockFloor
      : 0
    let floor = maximumThoughtClock([
      priorFloor,
      ...(input.materialized ?? []).map((record) => record.winnerKey.logicalClock),
      ...input.outbox.map((record) => record.logicalClock),
      ...input.historyOutbox.map((record) => record.logicalClock),
      ...input.historyOutbox.map((record) => record.snapshot.winnerKey.logicalClock),
      ...(input.observedClocks ?? []),
    ])
    const clocks: number[] = []
    for (let index = 0; index < input.count; index += 1) {
      floor = nextThoughtLogicalClock([floor])
      clocks.push(floor)
    }
    return {
      ok: true,
      deviceId: input.deviceId,
      clocks,
      state: {
        namespace: input.namespace,
        cursor: typeof rawState?.cursor === 'string' ? rawState.cursor : '',
        deviceId: input.deviceId,
        tabToken: typeof rawState?.tabToken === 'string' && rawState.tabToken.length > 0
          ? rawState.tabToken
          : input.tabToken,
        logicalClockFloor: floor,
        updatedAt: input.now,
        ...optionalNumber(rawState, 'lastAckSequence'),
        ...optionalNumber(rawState, 'pullAttemptCount'),
        ...optionalNumber(rawState, 'pullRetryAt'),
        ...optionalString(rawState, 'pullLastError'),
        ...optionalNumber(rawState, 'retryAt'),
        ...optionalString(rawState, 'lastError'),
        ...optionalString(rawState, 'lastErrorCode'),
        ...optionalNumber(rawState, 'lastSuccessfulSyncAt'),
        ...optionalBoolean(rawState, 'resyncRequired'),
        ...optionalNumber(rawState, 'lastServerSequence'),
        ...optionalNumber(rawState, 'retentionCutoff'),
      },
    }
  } catch (error) {
    return {
      ok: false,
      reason: error instanceof ThoughtClockError
        ? error.code
        : 'invalid-thought-clock',
    }
  }
}

export function classifyThoughtSyncSnapshot(input: {
  readonly records: readonly ThoughtSyncTransitionRecord[]
  readonly state: ThoughtSyncTransitionState
  readonly syncing: boolean
  readonly offline: boolean
}): ThoughtSyncTransitionSnapshot {
  const pendingCount = input.records.length
  const blockedCount = input.records.filter((record) => record.status === 'blocked').length
  const retryAt = thoughtRetryAt(input.records, input.state)
  const errorCode = storedThoughtFailureCode(input.state.lastErrorCode) ??
    blockedThoughtFailureCode(input.records) ??
    (input.state.lastError || input.state.pullLastError ? 'sync-failed' : undefined)
  let phase: ThoughtSyncTransitionPhase
  if (input.offline) phase = 'offline'
  else if (input.syncing) phase = 'syncing'
  else if (blockedCount > 0 || errorCode !== undefined) phase = 'failed'
  else if (input.state.pullInProgress) phase = 'pending'
  else if (pendingCount > 0) phase = 'pending'
  else if (input.state.lastSuccessfulSyncAt !== undefined) phase = 'synced'
  else phase = 'syncing'
  return Object.freeze({
    phase,
    pendingCount,
    blockedCount,
    ...(retryAt === undefined ? {} : { retryAt }),
    ...(input.state.lastSuccessfulSyncAt === undefined
      ? {}
      : { lastSuccessfulSyncAt: input.state.lastSuccessfulSyncAt }),
    ...(errorCode === undefined ? {} : { errorCode }),
  })
}

function versionKeyFromTransition(value: unknown): ThoughtVersionKeyTransition | null {
  if (!value || typeof value !== 'object') return null
  const candidate = value as Record<string, unknown>
  return isSafeNonNegativeInteger(candidate.logical_clock) &&
    candidate.logical_clock > 0 &&
    typeof candidate.device_id === 'string' &&
    candidate.device_id.length > 0 &&
    typeof candidate.op_id === 'string' &&
    candidate.op_id.length > 0
    ? {
        logical_clock: candidate.logical_clock,
        device_id: candidate.device_id,
        op_id: candidate.op_id,
      }
    : null
}

export function classifyThoughtAckTransition(
  records: readonly Pick<ThoughtSyncTransitionRecord, 'opId' | 'logicalClock' | 'deviceId'>[],
  acks: readonly ThoughtAckTransitionResponse[],
): ThoughtAckTransition {
  if (acks.length !== records.length) return { ok: false, reason: 'ack-count-mismatch' }
  const ackByID = new Map<unknown, ThoughtAckTransitionResponse>()
  for (const ack of acks) {
    if (ackByID.has(ack.op_id)) return { ok: false, reason: 'ack-op-id-mismatch' }
    ackByID.set(ack.op_id, ack)
  }
  let lastAckSequence = 0
  let logicalClockFloor = 0
  const completedIDs: string[] = []
  const dispositions: ThoughtAckDisposition[] = []
  for (const record of records) {
    const ack = ackByID.get(record.opId)
    if (!ack || ack.op_id !== record.opId) return { ok: false, reason: 'ack-op-id-mismatch' }
    if (ack.contract_version !== THOUGHT_CONTRACT_VERSION) {
      return { ok: false, reason: 'ack-contract-mismatch' }
    }
    if (ack.disposition !== 'applied' &&
      ack.disposition !== 'superseded' &&
      ack.disposition !== 'duplicate') {
      return { ok: false, reason: 'ack-disposition-mismatch' }
    }
    if (!isSafeNonNegativeInteger(ack.sequence) || ack.sequence <= 0) {
      return { ok: false, reason: 'ack-sequence-invalid' }
    }
    const submitted = versionKeyFromTransition(ack.submitted_key)
    if (!submitted ||
      submitted.logical_clock !== record.logicalClock ||
      submitted.device_id !== record.deviceId ||
      submitted.op_id !== record.opId) {
      return { ok: false, reason: 'ack-submitted-key-mismatch' }
    }
    const winner = versionKeyFromTransition(ack.current_winner_key)
    if (!winner) return { ok: false, reason: 'ack-winner-key-invalid' }
    lastAckSequence = Math.max(lastAckSequence, ack.sequence)
    logicalClockFloor = Math.max(logicalClockFloor, submitted.logical_clock, winner.logical_clock)
    completedIDs.push(record.opId)
    dispositions.push({ opId: record.opId, disposition: ack.disposition })
  }
  return {
    ok: true,
    completedIDs,
    dispositions,
    lastAckSequence,
    logicalClockFloor,
  }
}

function isRecoveryCASConflict(record: ThoughtOutboxRecord, error: ApiError): boolean {
  return record.recoveryOf !== undefined && record.expectedCurrentWinnerKey !== undefined &&
    error.kind === 'other' && error.status === 409 && error.errorCode === 'thought_recovery_conflict'
}

export function failThoughtOutboxTransition({
  record,
  error,
  now,
}: ThoughtOutboxFailureTransitionInput): ThoughtOutboxRecord {
  const attemptCount = record.attemptCount + 1
  const code = thoughtSyncFailureCode(error)
  if (isRecoveryCASConflict(record, error)) {
    const withoutRetry = withoutFields(record, ['nextAttemptAt'])
    return {
      ...withoutRetry,
      attemptCount,
      status: 'blocked',
      blockedReason: 'thought_recovery_conflict',
      recoveryConflict: true,
      lastError: 'sync-failed',
    }
  }
  if (isPermanentThoughtPushFailure(error)) {
    const withoutRetry = withoutFields(record, ['nextAttemptAt'])
    return {
      ...withoutRetry,
      attemptCount,
      status: 'blocked',
      blockedReason: code,
      lastError: code,
    }
  }
  const retryAt = now + thoughtRetryDelay(error, attemptCount)
  const withoutBlock = withoutFields(record, ['blockedReason', 'recoveryConflict'])
  return {
    ...withoutBlock,
    attemptCount,
    status: 'pending',
    nextAttemptAt: retryAt,
    lastError: code,
  }
}

export function failThoughtHistoryOutboxTransition({
  record,
  error,
  now,
}: ThoughtHistoryOutboxFailureTransitionInput): ThoughtHistoryOutboxRecord {
  const attemptCount = record.attemptCount + 1
  const code = thoughtSyncFailureCode(error)
  if (isPermanentThoughtPushFailure(error)) {
    const withoutRetry = withoutFields(record, ['nextAttemptAt'])
    return {
      ...withoutRetry,
      attemptCount,
      status: 'blocked',
      blockedReason: code,
      lastError: code,
    }
  }
  const retryAt = now + thoughtRetryDelay(error, attemptCount)
  const withoutBlock = withoutFields(record, ['blockedReason'])
  return {
    ...withoutBlock,
    attemptCount,
    status: 'pending',
    nextAttemptAt: retryAt,
    lastError: code,
  }
}

export function failThoughtPushState(input: {
  readonly state: ThoughtSyncStateRecord
  readonly records: readonly ThoughtSyncTransitionRecord[]
  readonly error: ApiError
  readonly now: number
}): { readonly state: ThoughtSyncStateRecord; readonly retryAt?: number } {
  const code = thoughtSyncFailureCode(input.error)
  const retryAt = thoughtRetryAt(input.records, input.state)
  return {
    retryAt,
    state: {
      ...input.state,
      ...(retryAt === undefined ? { retryAt: undefined } : { retryAt }),
      lastError: code,
      lastErrorCode: code,
      updatedAt: input.now,
    },
  }
}

export function transitionThoughtHistoryAckRecord(
  record: ThoughtHistoryOutboxRecord,
  disposition: ThoughtAckDisposition['disposition'],
  supersededError: string,
): ThoughtHistoryAckRecordTransition {
  if (disposition !== 'superseded') {
    return {
      action: 'delete',
      key: [record.key[0], record.key[1]],
      opId: record.opId,
    }
  }
  const withoutRetry = withoutFields(record, ['nextAttemptAt'])
  return {
    action: 'put',
    opId: record.opId,
    record: {
      ...withoutRetry,
      status: 'blocked',
      blockedReason: 'superseded',
      lastError: supersededError,
    },
  }
}

export function advanceThoughtAckState(input: {
  readonly state: ThoughtSyncStateRecord
  readonly remaining: readonly ThoughtSyncTransitionRecord[]
  readonly ack: Extract<ThoughtAckTransition, { readonly ok: true }>
  readonly now: number
}): ThoughtSyncStateRecord {
  const retryAt = thoughtRetryAt(input.remaining, input.state)
  const blockedCode = blockedThoughtFailureCode(input.remaining)
  const logicalClockFloor = maximumThoughtClock([
    input.state.logicalClockFloor ?? 0,
    input.ack.logicalClockFloor,
  ])
  return {
    ...input.state,
    logicalClockFloor,
    lastAckSequence: Math.max(input.state.lastAckSequence ?? 0, input.ack.lastAckSequence),
    ...(retryAt === undefined
      ? {
          retryAt: undefined,
          lastError: undefined,
          ...(blockedCode === undefined
            ? { lastErrorCode: undefined }
            : { lastErrorCode: blockedCode }),
        }
      : { retryAt }),
    updatedAt: input.now,
  }
}

export function advanceThoughtPullState(input: {
  readonly state: ThoughtSyncStateRecord
  readonly cursor: string
  readonly materialized: readonly Pick<ThoughtMaterializedRecord, 'winnerKey' | 'serverSequence'>[]
  readonly retentionCutoff?: number
  readonly pullInProgress: boolean
  readonly now: number
}): ThoughtSyncStateRecord {
  const lastServerSequence = input.materialized.reduce(
    (highest, record) => Math.max(highest, record.serverSequence),
    input.state.lastServerSequence ?? 0,
  )
  const nextRetentionCutoff = input.retentionCutoff === undefined
    ? input.state.retentionCutoff
    : Math.max(input.state.retentionCutoff ?? 0, input.retentionCutoff)
  const logicalClockFloor = maximumThoughtClock([
    input.state.logicalClockFloor ?? 0,
    ...input.materialized.map((record) => record.winnerKey.logicalClock),
  ])
  const stateWithoutPullInProgress = withoutFields(input.state, ['pullInProgress'])
  return {
    ...stateWithoutPullInProgress,
    cursor: input.cursor,
    logicalClockFloor,
    lastServerSequence,
    ...(input.pullInProgress ? { pullInProgress: true } : {}),
    ...(nextRetentionCutoff === undefined
      ? {}
      : { retentionCutoff: nextRetentionCutoff }),
    updatedAt: input.now,
  }
}

export function failThoughtPullState(input: {
  readonly state: ThoughtSyncStateRecord
  readonly records: readonly ThoughtSyncTransitionRecord[]
  readonly expectedCursor: string
  readonly error: ApiError
  readonly now: number
}): { readonly state: ThoughtSyncStateRecord; readonly retryAt?: number } {
  if (input.state.cursor !== input.expectedCursor) {
    return {
      state: input.state,
      retryAt: thoughtRetryAt(input.records, input.state),
    }
  }
  const pullAttemptCount = (input.state.pullAttemptCount ?? 0) + 1
  const pullRetryAt = input.now + thoughtRetryDelay(input.error, pullAttemptCount)
  const retryAt = earliestThoughtRetryTime(earliestThoughtRetryAt(input.records), pullRetryAt)
  const code = thoughtSyncFailureCode(input.error)
  return {
    retryAt,
    state: {
      ...input.state,
      pullAttemptCount,
      pullRetryAt,
      pullLastError: code,
      retryAt,
      lastErrorCode: code,
      updatedAt: input.now,
    },
  }
}

export function resetThoughtPullState(input: {
  readonly state: ThoughtSyncStateRecord
  readonly records: readonly ThoughtSyncTransitionRecord[]
  readonly now: number
}): ThoughtSyncStateRecord {
  return {
    ...input.state,
    cursor: '',
    pullInProgress: true,
    resyncRequired: true,
    pullAttemptCount: undefined,
    pullRetryAt: undefined,
    pullLastError: undefined,
    retryAt: earliestThoughtRetryAt(input.records),
    updatedAt: input.now,
    lastError: undefined,
    lastErrorCode: undefined,
  }
}

export function completeThoughtPullState(input: {
  readonly state: ThoughtSyncStateRecord
  readonly records: readonly ThoughtSyncTransitionRecord[]
  readonly now: number
}): ThoughtSyncStateRecord {
  const withoutPullFields = withoutFields(input.state, [
    'pullInProgress',
    'pullAttemptCount',
    'pullRetryAt',
    'pullLastError',
  ])
  const retryAt = earliestThoughtRetryAt(input.records)
  const blockedCode = blockedThoughtFailureCode(input.records)
  const fullySynced = input.records.length === 0
  return {
    ...withoutPullFields,
    resyncRequired: false,
    updatedAt: input.now,
    ...(retryAt === undefined
      ? {
          retryAt: undefined,
          lastError: undefined,
          ...(blockedCode === undefined
            ? { lastErrorCode: undefined }
            : { lastErrorCode: blockedCode }),
          ...(fullySynced ? { lastSuccessfulSyncAt: input.now } : {}),
        }
      : { retryAt }),
  }
}

export function reduceThoughtPullCursor(
  state: ThoughtPullCursorState,
  page: {
    readonly identityCurrent: boolean
    readonly nextCursor: string
    readonly stored: number
    readonly advanced: boolean
    readonly hasMore: boolean
  },
): ThoughtPullCursorTransition {
  if (!page.identityCurrent) return {
    status: 'stale',
    cursor: state.cursor,
    pulled: state.pulled,
  }
  const pulled = page.advanced ? state.pulled + page.stored : state.pulled
  if (!page.advanced) return {
    status: 'incomplete',
    cursor: page.nextCursor,
    pulled,
  }
  if (!page.hasMore) return {
    status: 'complete',
    cursor: page.nextCursor,
    pulled,
  }
  if (state.pageIndex + 1 >= MAX_THOUGHT_PULL_PAGES_PER_ROUND) {
    return {
      status: 'incomplete',
      cursor: page.nextCursor,
      pulled,
    }
  }
  return {
    status: 'continue',
    cursor: page.nextCursor,
    pulled,
  }
}

export function fenceRevokedThoughtTransition(input: {
  readonly phase: RevokedThoughtTransitionPhase
  readonly identityCurrent: boolean
  readonly cursor: string
  readonly pushed: number
  readonly pulled: number
  readonly pending: number
}): RevokedThoughtTransition {
  return {
    status: input.identityCurrent ? 'active' : 'stale',
    phase: input.phase,
    cursor: input.cursor,
    pushed: input.pushed,
    pulled: input.pulled,
    pending: input.pending,
  }
}
