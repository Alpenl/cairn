import { describe, expect, it } from 'vitest'

import {
  MAX_THOUGHT_PULL_PAGES_PER_ROUND,
  classifyThoughtAckTransition,
  classifyThoughtSyncSnapshot,
  fenceRevokedThoughtTransition,
  reduceThoughtPullCursor,
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
