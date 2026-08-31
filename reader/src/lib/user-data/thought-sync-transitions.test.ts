import { describe, expect, it } from 'vitest'

import type { ApiError } from '@webtag/api'

import type {
  ThoughtHistoryOutboxRecord,
  ThoughtMaterializedRecord,
  ThoughtOutboxRecord,
  ThoughtSyncStateRecord,
} from './thought-types'
import {
  MAX_THOUGHT_PULL_PAGES_PER_ROUND,
  advanceThoughtAckState,
  advanceThoughtPullState,
  allocateThoughtClockTransition,
  classifyThoughtAckTransition,
  classifyThoughtSyncSnapshot,
  completeThoughtPullState,
  failThoughtHistoryOutboxTransition,
  failThoughtOutboxTransition,
  failThoughtPushState,
  failThoughtPullState,
  fenceRevokedThoughtTransition,
  reduceThoughtPullCursor,
  resetThoughtPullState,
  transitionThoughtHistoryAckRecord,
  type ThoughtAckTransitionResponse,
  type ThoughtSyncTransitionRecord,
  type ThoughtSyncTransitionState,
} from './thought-sync-transitions'

const BASE_STATE: ThoughtSyncTransitionState = {
  cursor: '',
}

function outbox(
  opId: string,
  logicalClock: number,
  extra: Partial<ThoughtSyncTransitionRecord> = {},
): ThoughtSyncTransitionRecord {
  return {
    opId,
    logicalClock,
    deviceId: 'device-a',
    ...extra,
  }
}

function ack(
  opId: string,
  logicalClock: number,
  disposition: 'applied' | 'duplicate' | 'superseded' = 'applied',
  winnerClock = logicalClock,
): ThoughtAckTransitionResponse {
  return {
    contract_version: 1,
    op_id: opId,
    sequence: winnerClock + 10,
    disposition,
    submitted_key: {
      logical_clock: logicalClock,
      device_id: 'device-a',
      op_id: opId,
    },
    current_winner_key: {
      logical_clock: winnerClock,
      device_id: winnerClock === logicalClock ? 'device-a' : 'remote-device',
      op_id: winnerClock === logicalClock ? opId : 'remote-winner',
    },
  }
}

function syncState(extra: Partial<ThoughtSyncStateRecord> = {}): ThoughtSyncStateRecord {
  return {
    namespace: 'ns',
    cursor: 'cursor-1',
    deviceId: 'device-a',
    tabToken: 'tab-a',
    logicalClockFloor: 5,
    updatedAt: 1000,
    ...extra,
  }
}

function thoughtRecord(extra: Partial<ThoughtOutboxRecord> = {}): ThoughtOutboxRecord {
  return {
    key: ['ns', 1],
    namespace: 'ns',
    sequence: 1,
    opId: 'thought-op',
    deviceId: 'device-a',
    contractVersion: 1,
    logicalClock: 7,
    operationKind: 'delete',
    annotationId: 'thought-1',
    hostKind: 'link',
    hostId: 'L1',
    linkId: 'L1',
    target: { kind: 'saved-content', contentRevision: 7 },
    targetKey: 'saved-content:7',
    annotation: null,
    createdAt: 1000,
    attemptCount: 0,
    ...extra,
  }
}

function historyRecord(extra: Partial<ThoughtHistoryOutboxRecord> = {}): ThoughtHistoryOutboxRecord {
  return {
    key: ['ns', 'history-op'],
    namespace: 'ns',
    opId: 'history-op',
    deviceId: 'device-a',
    contractVersion: 1,
    logicalClock: 11,
    action: 'reattach',
    annotationId: 'thought-1',
    hostKind: 'note',
    hostId: 'N1',
    target: { kind: 'note', noteRevision: 4 },
    targetKey: 'note:4',
    expectedLastSequence: 20,
    expectedHostRevision: 4,
    snapshot: {
      body: 'frozen body',
      quote: { exact: 'text', start: 0, end: 4 },
      source: 'self',
      target: {
        kind: 'saved-content',
        host_id: 'L1',
        version: { content_revision: 7 },
      },
      winnerKey: {
        logicalClock: 13,
        deviceId: 'remote-device',
        opId: 'remote-winner',
      },
    },
    createdAt: 1000,
    attemptCount: 0,
    ...extra,
  }
}

function materialized(
  annotationId: string,
  serverSequence: number,
  logicalClock = serverSequence,
): Pick<ThoughtMaterializedRecord, 'winnerKey' | 'serverSequence'> {
  return {
    serverSequence,
    winnerKey: {
      logicalClock,
      deviceId: 'remote-device',
      opId: `remote-${annotationId}`,
    },
  }
}

describe('thought sync pure transitions', () => {
  it('keeps offline recovery as offline while preserving retry and pending facts', () => {
    expect(classifyThoughtSyncSnapshot({
      records: [outbox('retry-after-offline', 1, { nextAttemptAt: 2000 })],
      state: { ...BASE_STATE, pullRetryAt: 5000 },
      syncing: false,
      offline: true,
    })).toEqual({
      phase: 'offline',
      pendingCount: 1,
      blockedCount: 0,
      retryAt: 2000,
    })
  })

  it('classifies remount from durable state without claiming synced before a real round', () => {
    expect(classifyThoughtSyncSnapshot({
      records: [],
      state: { ...BASE_STATE, cursor: 'cursor-after-remount' },
      syncing: false,
      offline: false,
    })).toEqual({
      phase: 'syncing',
      pendingCount: 0,
      blockedCount: 0,
    })
    expect(classifyThoughtSyncSnapshot({
      records: [outbox('pending-after-remount', 3)],
      state: { ...BASE_STATE, cursor: 'cursor-after-remount' },
      syncing: false,
      offline: false,
    })).toMatchObject({
      phase: 'pending',
      pendingCount: 1,
      blockedCount: 0,
    })
  })

  it('accepts duplicate ACKs but rejects submitted-key mismatches', () => {
    expect(classifyThoughtAckTransition(
      [outbox('duplicate-op', 7)],
      [ack('duplicate-op', 7, 'duplicate')],
    )).toEqual({
      ok: true,
      completedIDs: ['duplicate-op'],
      dispositions: [{ opId: 'duplicate-op', disposition: 'duplicate' }],
      lastAckSequence: 17,
      logicalClockFloor: 7,
    })

    const mismatched = ack('duplicate-op', 8, 'duplicate')
    expect(classifyThoughtAckTransition(
      [outbox('duplicate-op', 7)],
      [mismatched],
    )).toEqual({
      ok: false,
      reason: 'ack-submitted-key-mismatch',
    })
  })

  it('allocates enqueue clocks from ordinary, history, materialized, and observed clocks', () => {
    const allocation = allocateThoughtClockTransition({
      namespace: 'ns',
      rawState: {
        namespace: 'ns',
        cursor: 'cursor-before-enqueue',
        deviceId: 'device-a',
        tabToken: 'tab-existing',
        logicalClockFloor: 6,
        pullRetryAt: 7000,
        lastSuccessfulSyncAt: 8000,
      },
      deviceId: 'device-a',
      tabToken: 'tab-new',
      count: 2,
      now: 9000,
      outbox: [thoughtRecord({ logicalClock: 10 })],
      historyOutbox: [historyRecord({
        logicalClock: 12,
        snapshot: {
          ...historyRecord().snapshot,
          winnerKey: { logicalClock: 14, deviceId: 'remote-device', opId: 'remote-history' },
        },
      })],
      materialized: [materialized('server-thought', 15, 16)],
      observedClocks: [20],
    })

    expect(allocation).toEqual({
      ok: true,
      deviceId: 'device-a',
      clocks: [21, 22],
      state: expect.objectContaining({
        namespace: 'ns',
        cursor: 'cursor-before-enqueue',
        deviceId: 'device-a',
        tabToken: 'tab-existing',
        logicalClockFloor: 22,
        pullRetryAt: 7000,
        lastSuccessfulSyncAt: 8000,
        updatedAt: 9000,
      }),
    })
  })

  it('models retryable and terminal push failures for ordinary and history outboxes', () => {
    const retryable: ApiError = {
      kind: 'network-unreachable',
      message: 'offline transport text',
      retryAfterSeconds: 5,
    }
    const terminal: ApiError = {
      kind: 'other',
      status: 422,
      errorCode: 'invalid_thought_payload',
      message: 'private payload details',
    }

    const retried = failThoughtOutboxTransition({
      record: thoughtRecord({
        attemptCount: 1,
        status: 'blocked',
        blockedReason: 'previous-block',
        recoveryConflict: true,
      }),
      error: retryable,
      now: 1000,
    })
    expect(retried).toEqual(expect.objectContaining({
      attemptCount: 2,
      status: 'pending',
      nextAttemptAt: 6000,
      lastError: 'network-unreachable',
    }))
    expect(retried).not.toHaveProperty('blockedReason')
    expect(retried).not.toHaveProperty('recoveryConflict')

    expect(failThoughtOutboxTransition({
      record: thoughtRecord({ nextAttemptAt: 2000 }),
      error: terminal,
      now: 1000,
    })).toEqual(expect.objectContaining({
      attemptCount: 1,
      status: 'blocked',
      blockedReason: 'other:invalid_thought_payload:422',
      lastError: 'other:invalid_thought_payload:422',
    }))

    const blockedHistory = failThoughtHistoryOutboxTransition({
      record: historyRecord({ nextAttemptAt: 2000 }),
      error: terminal,
      now: 1000,
    })
    expect(blockedHistory).toEqual(expect.objectContaining({
      attemptCount: 1,
      status: 'blocked',
      blockedReason: 'other:invalid_thought_payload:422',
      lastError: 'other:invalid_thought_payload:422',
    }))
    expect(blockedHistory).not.toHaveProperty('nextAttemptAt')

    expect(failThoughtPushState({
      state: syncState({ pullRetryAt: 12_000 }),
      records: [retried, blockedHistory],
      error: terminal,
      now: 7000,
    })).toEqual({
      retryAt: 6000,
      state: expect.objectContaining({
        retryAt: 6000,
        lastError: 'other:invalid_thought_payload:422',
        lastErrorCode: 'other:invalid_thought_payload:422',
        updatedAt: 7000,
      }),
    })
  })

  it('blocks recovery CAS conflicts without storing transport payload text', () => {
    const failed = failThoughtOutboxTransition({
      record: thoughtRecord({
        recoveryOf: { logicalClock: 4, deviceId: 'loser-device', opId: 'loser-op' },
        expectedCurrentWinnerKey: { logicalClock: 9, deviceId: 'winner-device', opId: 'winner-op' },
      }),
      error: {
        kind: 'other',
        status: 409,
        errorCode: 'thought_recovery_conflict',
        message: 'private-recovery-cas-sentinel',
      },
      now: 1000,
    })

    expect(failed).toEqual(expect.objectContaining({
      status: 'blocked',
      blockedReason: 'thought_recovery_conflict',
      recoveryConflict: true,
      lastError: 'sync-failed',
    }))
    expect(JSON.stringify(failed)).not.toContain('private-recovery-cas-sentinel')
  })

  it('models history superseded ACKs and advances sync state from the ack summary', () => {
    const history = historyRecord({ logicalClock: 7 })
    const ackTransition = classifyThoughtAckTransition(
      [history],
      [ack('history-op', 7, 'superseded', 20)],
    )

    expect(ackTransition).toEqual({
      ok: true,
      completedIDs: ['history-op'],
      dispositions: [{ opId: 'history-op', disposition: 'superseded' }],
      lastAckSequence: 30,
      logicalClockFloor: 20,
    })
    if (!ackTransition.ok) throw new Error('expected ack transition')

    const historyAction = transitionThoughtHistoryAckRecord(
      history,
      ackTransition.dispositions[0]?.disposition ?? 'applied',
      'history superseded',
    )
    expect(historyAction).toEqual({
      action: 'put',
      opId: 'history-op',
      record: expect.objectContaining({
        status: 'blocked',
        blockedReason: 'superseded',
        lastError: 'history superseded',
      }),
    })

    expect(advanceThoughtAckState({
      state: syncState({ lastAckSequence: 2, lastErrorCode: 'network-unreachable' }),
      remaining: historyAction.action === 'put' ? [historyAction.record] : [],
      ack: ackTransition,
      now: 4000,
    })).toEqual(expect.objectContaining({
      logicalClockFloor: 20,
      lastAckSequence: 30,
      lastErrorCode: 'superseded',
      updatedAt: 4000,
    }))
  })

  it('ends the twentieth pull page as an incomplete transition with a resumable cursor', () => {
    const beforeLastPage = {
      pageIndex: MAX_THOUGHT_PULL_PAGES_PER_ROUND - 1,
      cursor: 'cursor-19',
      pulled: 1900,
    }
    expect(reduceThoughtPullCursor(beforeLastPage, {
      identityCurrent: true,
      nextCursor: 'cursor-20',
      stored: 100,
      advanced: true,
      hasMore: true,
    })).toEqual({
      status: 'incomplete',
      cursor: 'cursor-20',
      pulled: 2000,
    })
  })

  it('models pull pending, cursor reset, cursor advance, and replay completion', () => {
    const advanced = advanceThoughtPullState({
      state: syncState({ retentionCutoff: 3 }),
      cursor: 'cursor-2',
      materialized: [materialized('server-thought', 8, 9)],
      retentionCutoff: 5,
      pullInProgress: true,
      now: 2000,
    })
    expect(advanced).toEqual(expect.objectContaining({
      cursor: 'cursor-2',
      logicalClockFloor: 9,
      lastServerSequence: 8,
      retentionCutoff: 5,
      pullInProgress: true,
      updatedAt: 2000,
    }))

    const failed = failThoughtPullState({
      state: advanced,
      records: [outbox('retryable-local', 3, { nextAttemptAt: 5000 })],
      expectedCursor: 'cursor-2',
      error: {
        kind: 'rate-limited',
        status: 429,
        errorCode: 'thought_cooldown',
        retryAfterSeconds: 10,
        message: 'server cooldown',
      },
      now: 2000,
    })
    expect(failed).toEqual({
      retryAt: 5000,
      state: expect.objectContaining({
        pullAttemptCount: 1,
        pullRetryAt: 12000,
        retryAt: 5000,
        pullLastError: 'rate-limited:thought_cooldown:429',
        lastErrorCode: 'rate-limited:thought_cooldown:429',
      }),
    })

    const reset = resetThoughtPullState({
      state: failed.state,
      records: [outbox('retryable-local', 3, { nextAttemptAt: 5000 })],
      now: 3000,
    })
    expect(reset).toEqual(expect.objectContaining({
      cursor: '',
      pullInProgress: true,
      resyncRequired: true,
      retryAt: 5000,
      updatedAt: 3000,
    }))
    expect(reset).toHaveProperty('pullRetryAt', undefined)
    expect(reset).toHaveProperty('lastErrorCode', undefined)

    const complete = completeThoughtPullState({
      state: { ...reset, cursor: 'cursor-final' },
      records: [],
      now: 4000,
    })
    expect(complete).toEqual(expect.objectContaining({
      cursor: 'cursor-final',
      resyncRequired: false,
      lastSuccessfulSyncAt: 4000,
      updatedAt: 4000,
    }))
    expect(complete).not.toHaveProperty('pullInProgress')
    expect(complete).not.toHaveProperty('pullRetryAt')
  })

  it('fences revoked push, pull, and later-page transitions as stale', () => {
    for (const phase of ['push', 'pull', 'later-page'] as const) {
      expect(fenceRevokedThoughtTransition({
        phase,
        identityCurrent: false,
        cursor: `${phase}-cursor`,
        pushed: phase === 'push' ? 1 : 0,
        pulled: phase === 'pull' ? 1 : 0,
        pending: 2,
      })).toEqual({
        status: 'stale',
        phase,
        cursor: `${phase}-cursor`,
        pushed: phase === 'push' ? 1 : 0,
        pulled: phase === 'pull' ? 1 : 0,
        pending: 2,
      })
    }
  })
})
