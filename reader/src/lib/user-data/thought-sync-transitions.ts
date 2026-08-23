import type { ApiError } from '@webtag/api'

import type { ReaderThoughtAckResponse } from '../api/types'
import { isRecord } from '../records'
import { isSafeNonNegativeInteger } from './annotation-codec'
import { maximumThoughtClock } from './thought-clock'
import type {
  ThoughtHistoryOutboxRecord,
  ThoughtMaterializedRecord,
  ThoughtOutboxRecord,
  ThoughtSyncStateRecord,
  ThoughtVersionKey,
} from './thought-types'
import {
  THOUGHT_CONTRACT_VERSION,
  isValidThoughtOutboxRecord,
  isValidThoughtVersionKey,
} from './thought-types'

const BASE_RETRY_MS = 1000
const MAX_RETRY_MS = 5 * 60 * 1000
const HISTORY_SUPERSEDED_ERROR = '历史想法操作已被较新的服务端版本覆盖。冻结候选已保留，请刷新后重试。'

export type ThoughtSyncOutboxRecord = ThoughtOutboxRecord | ThoughtHistoryOutboxRecord
type Mutable<T> = { -readonly [Key in keyof T]: T[Key] }

export interface PushFailureTransition<T extends ThoughtSyncOutboxRecord> {
  readonly record: T
  readonly terminal: boolean
  readonly retryAt?: number
  readonly code: string
}

export interface ThoughtOutboxEnqueueOperation {
  readonly kind: ThoughtOutboxRecord['operationKind']
  readonly opId: string
  readonly linkId: string
  readonly target: ThoughtOutboxRecord['target']
  readonly targetKey: string
  readonly annotationId: string
  readonly patch?: ThoughtOutboxRecord['patch']
}

export interface ThoughtOutboxRecoveryMetadata {
  readonly recoveryOf: ThoughtVersionKey
  readonly expectedCurrentWinnerKey: ThoughtVersionKey
  readonly hostKind: 'link' | 'note' | 'inbox'
}

export interface ThoughtOutboxEnqueueInput {
  readonly namespace: string
  readonly sequence: number
  readonly operation: ThoughtOutboxEnqueueOperation
  readonly annotation: ThoughtOutboxRecord['annotation']
  readonly fallbackAnnotation: ThoughtOutboxRecord['annotation']
  readonly checksum?: string
  readonly versionKey: ThoughtVersionKey
  readonly createdAt: number
  readonly recovery?: ThoughtOutboxRecoveryMetadata
}

export interface BaseAcknowledgementPlan {
  readonly observedClocks: readonly number[]
  readonly lastAckSequence: number
  readonly removedOpIds: readonly string[]
}

export interface DeleteThoughtOutboxAcknowledgement {
  readonly kind: 'delete'
  readonly record: ThoughtOutboxRecord
  readonly opId: string
}

export interface DeleteThoughtHistoryAcknowledgement {
  readonly kind: 'delete'
  readonly record: ThoughtHistoryOutboxRecord
  readonly opId: string
}

export interface BlockThoughtHistoryAcknowledgement {
  readonly kind: 'block-history-superseded'
  readonly record: ThoughtHistoryOutboxRecord
  readonly nextRecord: ThoughtHistoryOutboxRecord
  readonly opId: string
}

export interface ThoughtOutboxAcknowledgementPlan extends BaseAcknowledgementPlan {
  readonly actions: readonly DeleteThoughtOutboxAcknowledgement[]
}

export interface ThoughtHistoryAcknowledgementPlan extends BaseAcknowledgementPlan {
  readonly actions: readonly (DeleteThoughtHistoryAcknowledgement | BlockThoughtHistoryAcknowledgement)[]
}

export type ThoughtAcknowledgementPlanResult<T> =
  | { readonly ok: true; readonly value: T }
  | { readonly ok: false }

interface ValidatedAcknowledgements {
  readonly ackByID: ReadonlyMap<string, ReaderThoughtAckResponse>
  readonly observedClocks: readonly number[]
  readonly lastAckSequence: number
}

const STABLE_ERROR_COMPONENT = /^[a-z0-9][a-z0-9._-]{0,63}$/
const STABLE_FAILURE_CODE = /^[a-z0-9][a-z0-9._-]{0,63}(?::[a-z0-9][a-z0-9._-]{0,63}){0,2}$/

function stableErrorComponent(value: unknown): string | undefined {
  return typeof value === 'string' && STABLE_ERROR_COMPONENT.test(value) ? value : undefined
}

export function storedFailureCode(value: unknown): string | undefined {
  return typeof value === 'string' && value.length <= 128 && STABLE_FAILURE_CODE.test(value)
    ? value
    : undefined
}

function hostKindForEnqueue(
  operation: ThoughtOutboxEnqueueOperation,
  recovery: ThoughtOutboxRecoveryMetadata | undefined,
): ThoughtOutboxRecord['hostKind'] {
  if (recovery) return recovery.hostKind
  return operation.target.kind === 'note'
    ? 'note'
    : operation.target.kind === 'inbox'
      ? 'inbox'
      : 'link'
}

export function planThoughtOutboxEnqueue(input: ThoughtOutboxEnqueueInput): ThoughtOutboxRecord | null {
  const { namespace, operation, recovery, sequence, versionKey } = input
  if (
    !isSafeNonNegativeInteger(sequence) ||
    sequence === 0 ||
    !isSafeNonNegativeInteger(input.createdAt) ||
    !isValidThoughtVersionKey(versionKey) ||
    versionKey.opId !== operation.opId
  ) {
    return null
  }

  const record: ThoughtOutboxRecord = {
    key: [namespace, sequence],
    namespace,
    sequence,
    opId: operation.opId,
    deviceId: versionKey.deviceId,
    contractVersion: THOUGHT_CONTRACT_VERSION,
    logicalClock: versionKey.logicalClock,
    operationKind: operation.kind,
    annotationId: operation.annotationId,
    hostKind: hostKindForEnqueue(operation, recovery),
    hostId: operation.linkId,
    linkId: operation.linkId,
    target: operation.target,
    targetKey: operation.targetKey,
    annotation: input.annotation ?? input.fallbackAnnotation,
    ...(operation.kind === 'update' ? { patch: operation.patch } : {}),
    createdAt: input.createdAt,
    attemptCount: 0,
    ...(input.checksum === undefined ? {} : { checksum: input.checksum }),
    ...(recovery === undefined ? {} : {
      recoveryOf: recovery.recoveryOf,
      expectedCurrentWinnerKey: recovery.expectedCurrentWinnerKey,
    }),
  }
  return isValidThoughtOutboxRecord(record, namespace) ? record : null
}

/**
 * The UI and cross-tab controller need a durable failure classification, but
 * must never surface transport messages as protocol state: messages can be
 * server supplied and may contain request context. Keep this deliberately
 * small and composed only from the typed API classification.
 */
export function failureCode(error: ApiError): string {
  const serverCode = stableErrorComponent(error.errorCode)
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

export function retryDelay(error: ApiError, attemptCount: number): number {
  if (
    error.retryAfterSeconds !== undefined &&
    Number.isFinite(error.retryAfterSeconds) &&
    error.retryAfterSeconds >= 0
  ) {
    // Retry-After is a server contract. Local exponential backoff remains
    // capped below, but never shortens an explicit service cooldown.
    return Math.ceil(error.retryAfterSeconds * 1000)
  }
  return Math.min(
    MAX_RETRY_MS,
    BASE_RETRY_MS * (2 ** Math.min(Math.max(attemptCount - 1, 0), 8)),
  )
}

export function isPermanentPushFailure(error: ApiError): boolean {
  if (error.kind === 'unauthorized') return true
  if (error.kind !== 'other' || error.status === undefined) return false
  return error.status >= 400 && error.status < 500 &&
    error.status !== 408 && error.status !== 425 && error.status !== 429
}

/** Only request-validation 4xx responses can safely identify a single poison op by splitting. */
export function isIsolatablePermanentPushFailure(error: ApiError): boolean {
  return error.kind === 'other' && error.status !== undefined &&
    error.status >= 400 && error.status < 500 &&
    error.status !== 408 && error.status !== 425 && error.status !== 429
}

export function blockedFailureCode(records: readonly ThoughtSyncOutboxRecord[]): string | undefined {
  const blocked = records.find((record) => record.status === 'blocked')
  if (!blocked) return undefined
  return storedFailureCode(blocked.blockedReason) ?? 'blocked-operation'
}

export function normalizeThoughtOutboxFailureMarkers(record: ThoughtOutboxRecord): ThoughtOutboxRecord {
  const lastError = record.lastError === undefined
    ? undefined
    : storedFailureCode(record.lastError) ?? 'sync-failed'
  const blockedReason = record.status === 'blocked'
    ? storedFailureCode(record.blockedReason) ?? 'blocked-operation'
    : undefined
  if (lastError === record.lastError && blockedReason === record.blockedReason) return record
  const normalized = { ...record }
  if (lastError === undefined) delete normalized.lastError
  else normalized.lastError = lastError
  if (blockedReason === undefined) delete normalized.blockedReason
  else normalized.blockedReason = blockedReason
  return normalized
}

export function earliestRetryAt(records: readonly ThoughtSyncOutboxRecord[]): number | undefined {
  return records.reduce<number | undefined>((earliest, record) => {
    if (record.status === 'blocked') return earliest
    if (record.nextAttemptAt === undefined) return earliest
    return earliest === undefined ? record.nextAttemptAt : Math.min(earliest, record.nextAttemptAt)
  }, undefined)
}

export function earliestRetryTime(
  outboxRetryAt: number | undefined,
  pullRetryAt: number | undefined,
): number | undefined {
  if (outboxRetryAt === undefined) return pullRetryAt
  if (pullRetryAt === undefined) return outboxRetryAt
  return Math.min(outboxRetryAt, pullRetryAt)
}

export function stateRetryAt(
  records: readonly ThoughtSyncOutboxRecord[],
  state: ThoughtSyncStateRecord,
): number | undefined {
  return earliestRetryTime(earliestRetryAt(records), state.pullRetryAt)
}

function isRecoveryCASConflict(record: ThoughtOutboxRecord, error: ApiError): boolean {
  return record.recoveryOf !== undefined && record.expectedCurrentWinnerKey !== undefined &&
    error.kind === 'other' && error.status === 409 && error.errorCode === 'thought_recovery_conflict'
}

function durablePushError(record: ThoughtOutboxRecord, error: ApiError): string {
  return record.recoveryOf === undefined
    ? error.message.slice(0, 512)
    : '恢复版本同步失败，请刷新后重试。'
}

function retryableFailureRecord<T extends ThoughtSyncOutboxRecord>(
  record: T,
  attemptCount: number,
  retryAt: number,
  code: string,
): T {
  const withoutBlock = { ...record } as Mutable<T>
  delete withoutBlock.blockedReason
  return {
    ...withoutBlock,
    attemptCount,
    status: 'pending',
    nextAttemptAt: retryAt,
    lastError: code,
  } as T
}

function terminalFailureRecord<T extends ThoughtSyncOutboxRecord>(
  record: T,
  attemptCount: number,
  code: string,
): T {
  const withoutRetry = { ...record } as Mutable<T>
  delete withoutRetry.nextAttemptAt
  return {
    ...withoutRetry,
    attemptCount,
    status: 'blocked',
    blockedReason: code,
    lastError: code,
  } as T
}

export function applyThoughtOutboxPushFailure(
  record: ThoughtOutboxRecord,
  error: ApiError,
  now: number,
): PushFailureTransition<ThoughtOutboxRecord> {
  const attemptCount = record.attemptCount + 1
  if (isRecoveryCASConflict(record, error)) {
    const withoutRetry = { ...record }
    delete withoutRetry.nextAttemptAt
    return {
      record: {
        ...withoutRetry,
        attemptCount,
        status: 'blocked',
        blockedReason: 'thought_recovery_conflict',
        recoveryConflict: true,
        lastError: durablePushError(record, error),
      },
      terminal: true,
      code: failureCode(error),
    }
  }
  const code = failureCode(error)
  if (isPermanentPushFailure(error)) {
    return {
      record: terminalFailureRecord(record, attemptCount, code),
      terminal: true,
      code,
    }
  }
  const retryAt = now + retryDelay(error, attemptCount)
  const withoutRecoveryConflict = { ...retryableFailureRecord(record, attemptCount, retryAt, code) }
  delete withoutRecoveryConflict.recoveryConflict
  return {
    record: withoutRecoveryConflict,
    terminal: false,
    retryAt,
    code,
  }
}

export function applyThoughtHistoryOutboxPushFailure(
  record: ThoughtHistoryOutboxRecord,
  error: ApiError,
  now: number,
): PushFailureTransition<ThoughtHistoryOutboxRecord> {
  const attemptCount = record.attemptCount + 1
  const code = failureCode(error)
  if (isPermanentPushFailure(error)) {
    return {
      record: terminalFailureRecord(record, attemptCount, code),
      terminal: true,
      code,
    }
  }
  const retryAt = now + retryDelay(error, attemptCount)
  return {
    record: retryableFailureRecord(record, attemptCount, retryAt, code),
    terminal: false,
    retryAt,
    code,
  }
}

export function applyThoughtPushFailureState(
  state: ThoughtSyncStateRecord,
  records: readonly ThoughtSyncOutboxRecord[],
  error: ApiError,
  now: number,
): ThoughtSyncStateRecord {
  const retryAt = stateRetryAt(records, state)
  const code = failureCode(error)
  return {
    ...state,
    ...(retryAt === undefined ? { retryAt: undefined } : { retryAt }),
    lastError: code,
    lastErrorCode: code,
    updatedAt: now,
  }
}

export function versionKeyFromWire(value: unknown): ThoughtVersionKey | null {
  if (!isRecord(value)) return null
  const key = {
    logicalClock: value.logical_clock,
    deviceId: value.device_id,
    opId: value.op_id,
  }
  return isValidThoughtVersionKey(key) ? key : null
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0
}

function validateThoughtAcknowledgements(
  records: readonly ThoughtSyncOutboxRecord[],
  acks: readonly ReaderThoughtAckResponse[],
): ThoughtAcknowledgementPlanResult<ValidatedAcknowledgements> {
  if (acks.length !== records.length || !acks.every((ack) => {
    const submitted = versionKeyFromWire(ack.submitted_key)
    const winner = versionKeyFromWire(ack.current_winner_key)
    const record = records.find((candidate) => candidate.opId === ack.op_id)
    return ack.contract_version === THOUGHT_CONTRACT_VERSION &&
      (ack.disposition === 'applied' || ack.disposition === 'superseded' ||
        ack.disposition === 'duplicate') &&
      isNonEmptyString(ack.op_id) && isSafeNonNegativeInteger(ack.sequence) && ack.sequence > 0 &&
      submitted !== null && winner !== null && record !== undefined &&
      submitted.logicalClock === record.logicalClock &&
      submitted.deviceId === record.deviceId && submitted.opId === record.opId
  })) {
    return { ok: false }
  }
  const ackByID = new Map(acks.map((ack) => [ack.op_id, ack]))
  if (ackByID.size !== records.length || records.some((record) => !ackByID.has(record.opId))) {
    return { ok: false }
  }
  const observedClocks: number[] = []
  let lastAckSequence = 0
  for (const record of records) {
    const ack = ackByID.get(record.opId)
    const submitted = versionKeyFromWire(ack?.submitted_key)
    const winner = versionKeyFromWire(ack?.current_winner_key)
    if (!ack || !submitted || !winner) return { ok: false }
    lastAckSequence = Math.max(lastAckSequence, ack.sequence)
    observedClocks.push(submitted.logicalClock, winner.logicalClock)
  }
  return { ok: true, value: { ackByID, observedClocks, lastAckSequence } }
}

export function planThoughtOutboxAcknowledgements(
  records: readonly ThoughtOutboxRecord[],
  acks: readonly ReaderThoughtAckResponse[],
): ThoughtAcknowledgementPlanResult<ThoughtOutboxAcknowledgementPlan> {
  const validated = validateThoughtAcknowledgements(records, acks)
  if (!validated.ok) return { ok: false }
  const actions = records.map((record): DeleteThoughtOutboxAcknowledgement => ({
    kind: 'delete',
    record,
    opId: record.opId,
  }))
  return {
    ok: true,
    value: {
      actions,
      observedClocks: validated.value.observedClocks,
      lastAckSequence: validated.value.lastAckSequence,
      removedOpIds: actions.map((action) => action.opId),
    },
  }
}

export function planThoughtHistoryAcknowledgements(
  records: readonly ThoughtHistoryOutboxRecord[],
  acks: readonly ReaderThoughtAckResponse[],
): ThoughtAcknowledgementPlanResult<ThoughtHistoryAcknowledgementPlan> {
  const validated = validateThoughtAcknowledgements(records, acks)
  if (!validated.ok) return { ok: false }
  const actions = records.map((record): DeleteThoughtHistoryAcknowledgement | BlockThoughtHistoryAcknowledgement => {
    const ack = validated.value.ackByID.get(record.opId)
    if (ack?.disposition !== 'superseded') {
      return { kind: 'delete', record, opId: record.opId }
    }
    const blocked = { ...record }
    delete blocked.nextAttemptAt
    return {
      kind: 'block-history-superseded',
      record,
      nextRecord: {
        ...blocked,
        status: 'blocked',
        blockedReason: 'superseded',
        lastError: HISTORY_SUPERSEDED_ERROR,
      },
      opId: record.opId,
    }
  })
  return {
    ok: true,
    value: {
      actions,
      observedClocks: validated.value.observedClocks,
      lastAckSequence: validated.value.lastAckSequence,
      removedOpIds: actions
        .filter((action): action is DeleteThoughtHistoryAcknowledgement => action.kind === 'delete')
        .map((action) => action.opId),
    },
  }
}

export function applyThoughtAcknowledgementState(
  state: ThoughtSyncStateRecord,
  remaining: readonly ThoughtSyncOutboxRecord[],
  plan: BaseAcknowledgementPlan,
  now: number,
): ThoughtSyncStateRecord {
  const retryAt = earliestRetryTime(earliestRetryAt(remaining), state.pullRetryAt)
  const blockedCode = blockedFailureCode(remaining)
  const logicalClockFloor = maximumThoughtClock([
    state.logicalClockFloor ?? 0,
    ...plan.observedClocks,
  ])
  return {
    ...state,
    logicalClockFloor,
    lastAckSequence: Math.max(state.lastAckSequence ?? 0, plan.lastAckSequence),
    ...(retryAt === undefined
      ? {
          retryAt: undefined,
          lastError: undefined,
          ...(blockedCode === undefined
            ? { lastErrorCode: undefined }
            : { lastErrorCode: blockedCode }),
        }
      : { retryAt }),
    updatedAt: now,
  }
}

export function applyThoughtPullFailureState(
  state: ThoughtSyncStateRecord,
  records: readonly ThoughtSyncOutboxRecord[],
  error: ApiError,
  now: number,
): ThoughtSyncStateRecord {
  const pullAttemptCount = (state.pullAttemptCount ?? 0) + 1
  const pullRetryAt = now + retryDelay(error, pullAttemptCount)
  const retryAt = earliestRetryTime(earliestRetryAt(records), pullRetryAt)
  const code = failureCode(error)
  return {
    ...state,
    pullAttemptCount,
    pullRetryAt,
    pullLastError: code,
    retryAt,
    lastErrorCode: code,
    updatedAt: now,
  }
}

export function resetThoughtPullCursorState(
  state: ThoughtSyncStateRecord,
  records: readonly ThoughtSyncOutboxRecord[],
  now: number,
): ThoughtSyncStateRecord {
  return {
    ...state,
    cursor: '',
    pullInProgress: true,
    resyncRequired: true,
    pullAttemptCount: undefined,
    pullRetryAt: undefined,
    pullLastError: undefined,
    retryAt: earliestRetryAt(records),
    updatedAt: now,
    lastError: undefined,
    lastErrorCode: undefined,
  }
}

export function completeThoughtPullState(
  state: ThoughtSyncStateRecord,
  records: readonly ThoughtSyncOutboxRecord[],
  now: number,
): ThoughtSyncStateRecord {
  const stateWithoutPullInProgress = { ...state }
  delete stateWithoutPullInProgress.pullInProgress
  const nextState = { ...stateWithoutPullInProgress, resyncRequired: false, updatedAt: now }
  delete nextState.pullAttemptCount
  delete nextState.pullRetryAt
  delete nextState.pullLastError
  const retryAt = earliestRetryAt(records)
  const blockedCode = blockedFailureCode(records)
  const fullySynced = records.length === 0
  return {
    ...nextState,
    ...(retryAt === undefined
      ? {
          retryAt: undefined,
          lastError: undefined,
          ...(blockedCode === undefined
            ? { lastErrorCode: undefined }
            : { lastErrorCode: blockedCode }),
          ...(fullySynced ? { lastSuccessfulSyncAt: now } : {}),
        }
      : { retryAt }),
  }
}

export function advanceThoughtPullPageState(
  state: ThoughtSyncStateRecord,
  materializedRecords: readonly ThoughtMaterializedRecord[],
  cursor: string,
  retentionCutoff: number | undefined,
  pullInProgress: boolean,
  now: number,
): ThoughtSyncStateRecord {
  const lastServerSequence = materializedRecords.reduce(
    (highest, record) => Math.max(highest, record.serverSequence),
    state.lastServerSequence ?? 0,
  )
  const nextRetentionCutoff = retentionCutoff === undefined
    ? state.retentionCutoff
    : Math.max(state.retentionCutoff ?? 0, retentionCutoff)
  const logicalClockFloor = maximumThoughtClock([
    state.logicalClockFloor ?? 0,
    ...materializedRecords.map((record) => record.winnerKey.logicalClock),
  ])
  const stateWithoutPullInProgress = { ...state }
  delete stateWithoutPullInProgress.pullInProgress
  return {
    ...stateWithoutPullInProgress,
    cursor,
    logicalClockFloor,
    lastServerSequence,
    ...(pullInProgress ? { pullInProgress: true } : {}),
    ...(nextRetentionCutoff === undefined
      ? {}
      : { retentionCutoff: nextRetentionCutoff }),
    updatedAt: now,
  }
}
