import {
  THOUGHT_CONTRACT_VERSION,
  type ThoughtSyncStateRecord,
} from './thought-types'

export const MAX_THOUGHT_PULL_PAGES_PER_ROUND = 20

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

function isSafeNonNegativeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

export function storedThoughtFailureCode(value: unknown): string | undefined {
  return typeof value === 'string' && value.length <= 128 && STABLE_FAILURE_CODE.test(value)
    ? value
    : undefined
}

export function blockedThoughtFailureCode(
  records: readonly Pick<ThoughtSyncTransitionRecord, 'status' | 'blockedReason'>[],
): string | undefined {
  const blocked = records.find((record) => record.status === 'blocked')
  if (!blocked) return undefined
  return storedThoughtFailureCode(blocked.blockedReason) ?? 'blocked-operation'
}

export function earliestThoughtRetryAt(
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
  }
  return {
    ok: true,
    completedIDs,
    lastAckSequence,
    logicalClockFloor,
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
