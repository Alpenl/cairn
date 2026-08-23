import type { ApiError } from '@webtag/api'
import { describe, expect, it } from 'vitest'

import type { ReaderThoughtAckResponse } from '../api/types'
import type {
  ThoughtHistoryOutboxRecord,
  ThoughtMaterializedRecord,
  ThoughtOutboxRecord,
  ThoughtSyncStateRecord,
  ThoughtVersionKey,
} from './thought-types'
import {
  applyThoughtAcknowledgementState,
  applyThoughtHistoryOutboxPushFailure,
  applyThoughtOutboxPushFailure,
  applyThoughtPullFailureState,
  advanceThoughtPullPageState,
  completeThoughtPullState,
  failureCode,
  planThoughtHistoryAcknowledgements,
  planThoughtOutboxAcknowledgements,
  resetThoughtPullCursorState,
  stateRetryAt,
} from './thought-sync-transitions'

const namespace = 'physical-A'
const target = { kind: 'saved-content', contentRevision: 7 } as const
const targetKey = 'saved-content:7'

function versionKey(
  opId: string,
  logicalClock = 3,
  deviceId = 'device-A',
): ThoughtVersionKey {
  return { logicalClock, deviceId, opId }
}

function wireKey(key: ThoughtVersionKey): ReaderThoughtAckResponse['submitted_key'] {
  return {
    logical_clock: key.logicalClock,
    device_id: key.deviceId,
    op_id: key.opId,
  }
}

function ack(
  record: ThoughtOutboxRecord | ThoughtHistoryOutboxRecord,
  disposition: ReaderThoughtAckResponse['disposition'] = 'applied',
  sequence = 10,
  winnerClock = record.logicalClock,
): ReaderThoughtAckResponse {
  return {
    contract_version: 1,
    op_id: record.opId,
    sequence,
    disposition,
    submitted_key: wireKey(versionKey(record.opId, record.logicalClock, record.deviceId)),
    current_winner_key: wireKey(versionKey(record.opId, winnerClock, record.deviceId)),
  }
}

function state(overrides: Partial<ThoughtSyncStateRecord> = {}): ThoughtSyncStateRecord {
  return {
    namespace,
    cursor: 'cursor-1',
    deviceId: 'device-A',
    tabToken: 'tab-A',
    logicalClockFloor: 1,
    updatedAt: 1,
    ...overrides,
  }
}

function outboxRecord(overrides: Partial<ThoughtOutboxRecord> = {}): ThoughtOutboxRecord {
  return {
    key: [namespace, 1],
    namespace,
    sequence: 1,
    opId: 'op-a',
    deviceId: 'device-A',
    contractVersion: 1,
    logicalClock: 3,
    operationKind: 'update',
    annotationId: 'thought-a',
    hostKind: 'link',
    hostId: 'link-a',
    linkId: 'link-a',
    target,
    targetKey,
    annotation: null,
    patch: { note: 'body', updatedAt: 2 },
    createdAt: 1,
    attemptCount: 0,
    ...overrides,
  }
}

function historyRecord(overrides: Partial<ThoughtHistoryOutboxRecord> = {}): ThoughtHistoryOutboxRecord {
  const winnerKey = versionKey('server-op', 9, 'server-device')
  return {
    key: [namespace, 'history-op'],
    namespace,
    opId: 'history-op',
    deviceId: 'device-A',
    contractVersion: 1,
    logicalClock: 4,
    action: 'reattach',
    annotationId: 'thought-a',
    hostKind: 'link',
    hostId: 'link-a',
    target,
    targetKey,
    expectedLastSequence: 3,
    expectedHostRevision: 7,
    snapshot: {
      body: 'old body',
      quote: null,
      source: 'self',
      target: { kind: target.kind, host_id: 'link-a', version: { content_revision: 7 } },
      winnerKey,
    },
    createdAt: 1,
    attemptCount: 0,
    ...overrides,
  }
}

function materializedRecord(overrides: Partial<ThoughtMaterializedRecord> = {}): ThoughtMaterializedRecord {
  return {
    key: [namespace, 'thought-a'],
    namespace,
    annotationId: 'thought-a',
    contractVersion: 1,
    winnerKey: versionKey('remote-op', 12, 'remote-device'),
    hostKind: 'link',
    hostId: 'link-a',
    linkId: 'link-a',
    target,
    targetKey,
    quote: null,
    body: 'remote body',
    source: 'self',
    deleted: false,
    serverSequence: 6,
    createdAt: '2026-08-01T00:00:00.000Z',
    updatedAt: '2026-08-01T00:00:00.000Z',
    ...overrides,
  }
}

describe('thought sync transition helpers', () => {
  it('marks retryable, permanent, and recovery push failures without IDB state', () => {
    const retryable: ApiError = { kind: 'network-unreachable', message: 'offline' }
    const permanent: ApiError = {
      kind: 'other',
      status: 422,
      errorCode: 'invalid_payload',
      message: 'bad payload',
    }
    const recoveryConflict: ApiError = {
      kind: 'other',
      status: 409,
      errorCode: 'thought_recovery_conflict',
      message: 'conflict',
    }

    const retryableRecord = applyThoughtOutboxPushFailure(outboxRecord({
      attemptCount: 1,
      blockedReason: 'old-block',
      recoveryConflict: true,
    }), retryable, 1_000)
    const permanentRecord = applyThoughtHistoryOutboxPushFailure(historyRecord({
      attemptCount: 2,
      nextAttemptAt: 1_500,
    }), permanent, 1_000)
    const recoveryRecord = applyThoughtOutboxPushFailure(outboxRecord({
      recoveryOf: versionKey('loser', 1),
      expectedCurrentWinnerKey: versionKey('winner', 2),
    }), recoveryConflict, 1_000)

    expect(retryableRecord).toMatchObject({
      terminal: false,
      retryAt: 3_000,
      code: 'network-unreachable',
      record: {
        attemptCount: 2,
        status: 'pending',
        nextAttemptAt: 3_000,
        lastError: 'network-unreachable',
      },
    })
    expect(retryableRecord.record).not.toHaveProperty('blockedReason')
    expect(retryableRecord.record).not.toHaveProperty('recoveryConflict')
    expect(permanentRecord).toMatchObject({
      terminal: true,
      code: 'other:invalid_payload:422',
      record: {
        attemptCount: 3,
        status: 'blocked',
        blockedReason: 'other:invalid_payload:422',
        lastError: 'other:invalid_payload:422',
      },
    })
    expect(permanentRecord.record).not.toHaveProperty('nextAttemptAt')
    expect(recoveryRecord).toMatchObject({
      terminal: true,
      code: 'other:thought_recovery_conflict:409',
      record: {
        attemptCount: 1,
        status: 'blocked',
        blockedReason: 'thought_recovery_conflict',
        recoveryConflict: true,
        lastError: '恢复版本同步失败，请刷新后重试。',
      },
    })
  })

  it('plans standard acknowledgements and advances acknowledgement-owned state', () => {
    const record = outboxRecord({ logicalClock: 3 })
    const plan = planThoughtOutboxAcknowledgements([record], [ack(record, 'duplicate', 12, 9)])

    expect(plan.ok).toBe(true)
    if (!plan.ok) throw new Error('standard acknowledgement plan expected')

    const next = applyThoughtAcknowledgementState(
      state({ logicalClockFloor: 5, lastAckSequence: 7, lastError: 'old', lastErrorCode: 'old' }),
      [],
      plan.value,
      2_000,
    )

    expect(plan.value.actions).toEqual([{ kind: 'delete', record, opId: 'op-a' }])
    expect(plan.value.removedOpIds).toEqual(['op-a'])
    expect(next).toMatchObject({
      logicalClockFloor: 9,
      lastAckSequence: 12,
      updatedAt: 2_000,
    })
    expect(next.retryAt).toBeUndefined()
    expect(next.lastError).toBeUndefined()
    expect(next.lastErrorCode).toBeUndefined()
  })

  it('keeps superseded history acknowledgements as blocked durable evidence', () => {
    const record = historyRecord({ nextAttemptAt: 3_000 })
    const plan = planThoughtHistoryAcknowledgements([record], [ack(record, 'superseded', 8, 11)])

    expect(plan.ok).toBe(true)
    if (!plan.ok) throw new Error('history acknowledgement plan expected')

    expect(plan.value.removedOpIds).toEqual([])
    expect(plan.value.actions).toHaveLength(1)
    expect(plan.value.actions[0]).toMatchObject({
      kind: 'block-history-superseded',
      opId: 'history-op',
      nextRecord: {
        status: 'blocked',
        blockedReason: 'superseded',
        lastError: '历史想法操作已被较新的服务端版本覆盖。冻结候选已保留，请刷新后重试。',
      },
    })
    if (plan.value.actions[0].kind !== 'block-history-superseded') {
      throw new Error('superseded history action expected')
    }
    expect(plan.value.actions[0].nextRecord).not.toHaveProperty('nextAttemptAt')

    const next = applyThoughtAcknowledgementState(
      state({ lastError: 'old', lastErrorCode: 'old' }),
      [plan.value.actions[0].nextRecord],
      plan.value,
      2_000,
    )
    expect(next).toMatchObject({
      lastAckSequence: 8,
      lastErrorCode: 'superseded',
    })
    expect(next.lastError).toBeUndefined()
  })

  it('rejects acknowledgements that do not match the submitted operation key', () => {
    const record = outboxRecord()
    const mismatched = {
      ...ack(record),
      submitted_key: wireKey(versionKey('different-op', record.logicalClock, record.deviceId)),
    }

    expect(planThoughtOutboxAcknowledgements([record], [mismatched]).ok).toBe(false)
  })

  it('keeps pull retry, reset, complete, and page cursor state transitions pure', () => {
    const retryablePull: ApiError = {
      kind: 'other',
      status: 503,
      retryAfterSeconds: 10,
      message: 'retry later',
    }
    const pending = outboxRecord({ nextAttemptAt: 4_000 })
    const failed = applyThoughtPullFailureState(
      state({ pullAttemptCount: 1, pullRetryAt: 3_000 }),
      [pending],
      retryablePull,
      1_000,
    )
    const reset = resetThoughtPullCursorState(
      state({
        cursor: 'expired',
        pullAttemptCount: 2,
        pullRetryAt: 5_000,
        pullLastError: 'other:503',
        lastError: 'old',
        lastErrorCode: 'old',
      }),
      [pending],
      2_000,
    )
    const completed = completeThoughtPullState(
      state({
        pullInProgress: true,
        pullAttemptCount: 2,
        pullRetryAt: 5_000,
        pullLastError: 'other:503',
        resyncRequired: true,
      }),
      [],
      3_000,
    )
    const advanced = advanceThoughtPullPageState(
      state({
        logicalClockFloor: 5,
        lastServerSequence: 4,
        retentionCutoff: 1_000,
        pullInProgress: true,
      }),
      [materializedRecord({ serverSequence: 9, winnerKey: versionKey('remote-op', 12, 'remote-device') })],
      'cursor-2',
      2_000,
      true,
      4_000,
    )

    expect(failed).toMatchObject({
      pullAttemptCount: 2,
      pullRetryAt: 11_000,
      retryAt: 4_000,
      lastErrorCode: failureCode(retryablePull),
    })
    expect(stateRetryAt([pending], failed)).toBe(4_000)
    expect(reset).toMatchObject({
      cursor: '',
      pullInProgress: true,
      resyncRequired: true,
      retryAt: 4_000,
      updatedAt: 2_000,
    })
    expect(reset.pullAttemptCount).toBeUndefined()
    expect(reset.pullRetryAt).toBeUndefined()
    expect(reset.pullLastError).toBeUndefined()
    expect(reset.lastError).toBeUndefined()
    expect(reset.lastErrorCode).toBeUndefined()
    expect(completed).toMatchObject({
      resyncRequired: false,
      updatedAt: 3_000,
      lastSuccessfulSyncAt: 3_000,
    })
    expect(completed.pullInProgress).toBeUndefined()
    expect(completed.pullAttemptCount).toBeUndefined()
    expect(completed.pullRetryAt).toBeUndefined()
    expect(completed.pullLastError).toBeUndefined()
    expect(advanced).toMatchObject({
      cursor: 'cursor-2',
      logicalClockFloor: 12,
      lastServerSequence: 9,
      retentionCutoff: 2_000,
      pullInProgress: true,
      updatedAt: 4_000,
    })
  })
})
