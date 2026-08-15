import { describe, expect, it } from 'vitest'

import {
  planThoughtRepair,
  sha256UTF8,
  stableRepairID,
  type ThoughtRepairInputs,
} from './thought-repair'

const NAMESPACE = 'physical-A'
const TARGET = { kind: 'saved-content', contentRevision: 7 } as const
const TARGET_KEY = 'saved-content:7'

function annotation(id = 'a1', note = 'base note') {
  return {
    id,
    blockKey: 'content-document',
    start: 2,
    end: 6,
    text: 'quoted',
    note,
    source: 'self' as const,
    createdAt: 10,
    updatedAt: 10,
    sourceContentRevision: 7,
  }
}

function v4(
  kind: 'add' | 'update' | 'delete',
  sequence: number,
  overrides: Record<string, unknown> = {},
) {
  return {
    sequence,
    opId: `op-${sequence}`,
    namespace: NAMESPACE,
    linkId: 'link-1',
    target: TARGET,
    targetKey: TARGET_KEY,
    annotationId: 'a1',
    kind,
    ...(kind === 'add' ? { annotation: annotation() } : {}),
    ...(kind === 'update' ? { patch: { note: 'tail note', source: 'ai', updatedAt: 11 } } : {}),
    ...overrides,
  }
}

function v5(
  kind: 'add' | 'update' | 'delete',
  sequence: number,
  overrides: Record<string, unknown> = {},
) {
  const raw = v4(kind, sequence, overrides)
  const { kind: operationKind, ...rest } = raw
  return {
    key: [NAMESPACE, sequence],
    deviceId: '',
    contractVersion: 0,
    logicalClock: 0,
    createdAt: 0,
    attemptCount: 0,
    ...rest,
    operationKind,
  }
}

function materialized(note = 'base note', overrides: Record<string, unknown> = {}) {
  const value = annotation('a1', note)
  return {
    key: [NAMESPACE, 'link-1', TARGET_KEY, 'a1'],
    namespace: NAMESPACE,
    linkId: 'link-1',
    target: TARGET,
    targetKey: TARGET_KEY,
    annotationId: 'a1',
    sequence: 1,
    annotation: value,
    fallbackAnnotation: value,
    ...overrides,
  }
}

function inputs(overrides: Partial<ThoughtRepairInputs> = {}): ThoughtRepairInputs {
  return {
    v4Operations: [],
    v5Outbox: [],
    v4Materialized: [],
    v5Materialized: [],
    syncStates: [],
    ...overrides,
  }
}

describe('v6 thought repair planner', () => {
  it('uses real SHA-256 over canonical UTF-8 input', () => {
    expect(sha256UTF8('abc')).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad')
  })

  it.each([
    ['update', { patch: { note: 'tail note', source: 'ai', updatedAt: 11 } }],
    ['delete', {}],
  ] as const)('injects deterministic complete add before a compacted %s tail', (kind, extra) => {
    const plan = planThoughtRepair(inputs({
      v4Operations: [v4(kind, 8, extra)],
      v4Materialized: [materialized()],
    }))
    expect(plan.quarantine).toEqual([])
    expect(plan.ready.map((record) => [record.operationKind, record.opId, record.sequence])).toEqual([
      ['add', stableRepairID(NAMESPACE, 'a1'), 1],
      [kind, 'op-8', 2],
    ])
    expect(plan.ready[0]).toMatchObject({ annotation: annotation(), contractVersion: 1 })
    expect(Number(plan.ready[1].logicalClock)).toBeGreaterThan(Number(plan.ready[0].logicalClock))
  })

  it('makes v4 direct and already-v5 repair manifests byte-equivalent', () => {
    const v4Plan = planThoughtRepair(inputs({
      v4Operations: [v4('update', 8)],
      v4Materialized: [materialized()],
    }))
    const v5Plan = planThoughtRepair(inputs({
      v5Outbox: [v5('update', 8)],
      v4Materialized: [materialized()],
    }))
    expect(v5Plan.ready).toEqual(v4Plan.ready)
    expect(v5Plan.quarantine).toEqual(v4Plan.quarantine)
    expect(v5Plan.manifests).toEqual(v4Plan.manifests)
  })

  it('uses the first complete materialization candidate without combining fields', () => {
    const plan = planThoughtRepair(inputs({
      v4Operations: [v4('update', 3)],
      v5Materialized: [materialized('from v5')],
      v4Materialized: [materialized('from v4')],
    }))
    expect(plan.ready[0]?.annotation?.note).toBe('from v5')

    const malformed = materialized('from v5', {
      annotation: { ...annotation(), quote: { exact: 7 } },
    })
    const fallback = planThoughtRepair(inputs({
      v4Operations: [v4('update', 3)],
      v5Materialized: [malformed],
      v4Materialized: [materialized('from v4')],
    }))
    expect(fallback.ready[0]?.annotation?.note).toBe('from v4')
    expect(fallback.quarantine).toEqual([])

    const invalid = planThoughtRepair(inputs({
      v4Operations: [v4('update', 3)],
      v5Materialized: [malformed],
      v4Materialized: [malformed],
    }))
    expect(invalid.ready).toEqual([])
    expect(invalid.quarantine).toEqual([expect.objectContaining({ reason: 'invalid_quote' })])
  })

  it.each([
    ['missing base', inputs({ v4Operations: [v4('update', 2)] }), 'missing_complete_base'],
    ['same id divergent payload', inputs({
      v4Operations: [v4('add', 1), v4('add', 2, { opId: 'op-1', annotation: annotation('a1', 'different') })],
    }), 'conflicting_duplicate_op'],
    ['same id divergent version key', inputs({
      v5Outbox: [
        v5('add', 1, { opId: 'same-op', deviceId: 'device-a', contractVersion: 1, logicalClock: 5 }),
        v5('add', 2, { opId: 'same-op', deviceId: 'device-b', contractVersion: 1, logicalClock: 5 }),
      ],
    }), 'conflicting_duplicate_op'],
    ['identity mismatch', inputs({
      v4Operations: [v4('add', 1), v4('update', 2, { target: { kind: 'saved-content', contentRevision: 8 }, targetKey: 'saved-content:8' })],
    }), 'identity_mismatch'],
    ['bad quote', inputs({
      v4Operations: [v4('add', 1, { annotation: { ...annotation(), quote: { exact: 5 } } })],
    }), 'invalid_quote'],
  ] as const)('durably quarantines %s', (_caseName, fixture, reason) => {
    const plan = planThoughtRepair(fixture)
    expect(plan.ready).toEqual([])
    expect(plan.quarantine).toEqual([expect.objectContaining({ namespace: NAMESPACE, reason })])
    expect(plan.manifests[0]).toMatchObject({ complete: true, quarantineCount: 1 })
  })

  it('keeps an already legal complete v5 triple while assigning v6 dispatch sequence', () => {
    const plan = planThoughtRepair(inputs({
      v5Outbox: [v5('add', 7, {
        deviceId: 'device-a',
        contractVersion: 1,
        logicalClock: 42,
      })],
    }))
    expect(plan.ready).toEqual([expect.objectContaining({
      sequence: 1,
      opId: 'op-7',
      deviceId: 'device-a',
      logicalClock: 42,
    })])
  })

  it('keeps a complete add-update-delete chain without adding a synthetic base', () => {
    const plan = planThoughtRepair(inputs({
      v5Outbox: [
        v5('add', 1, { deviceId: 'device-a', contractVersion: 1, logicalClock: 11 }),
        v5('update', 2, { deviceId: 'device-a', contractVersion: 1, logicalClock: 12 }),
        v5('delete', 3, { deviceId: 'device-a', contractVersion: 1, logicalClock: 13 }),
      ],
    }))
    expect(plan.quarantine).toEqual([])
    expect(plan.ready.map((record) => ({
      opId: record.opId,
      kind: record.operationKind,
      deviceId: record.deviceId,
      clock: record.logicalClock,
      repair: record.repair,
    }))).toEqual([
      { opId: 'op-1', kind: 'add', deviceId: 'device-a', clock: 11, repair: true },
      { opId: 'op-2', kind: 'update', deviceId: 'device-a', clock: 12, repair: true },
      { opId: 'op-3', kind: 'delete', deviceId: 'device-a', clock: 13, repair: true },
    ])
  })

  it('isolates identical legacy sequences and op ids across namespaces', () => {
    const otherAnnotation = annotation('b1', 'namespace B')
    const plan = planThoughtRepair(inputs({
      v4Operations: [
        v4('update', 8),
        v4('update', 8, {
          namespace: 'physical-B', annotationId: 'b1', opId: 'op-8', linkId: 'link-b',
        }),
      ],
      v4Materialized: [
        materialized(),
        materialized('namespace B', {
          key: ['physical-B', 'link-b', TARGET_KEY, 'b1'], namespace: 'physical-B',
          linkId: 'link-b', annotationId: 'b1', annotation: otherAnnotation,
          fallbackAnnotation: otherAnnotation,
        }),
      ],
    }))
    expect(plan.quarantine).toEqual([])
    expect(plan.manifests.map((manifest) => manifest.namespace)).toEqual(['physical-A', 'physical-B'])
    expect(plan.ready.map((record) => [record.namespace, record.sequence, record.operationKind])).toEqual([
      ['physical-A', 1, 'add'], ['physical-A', 2, 'update'],
      ['physical-B', 1, 'add'], ['physical-B', 2, 'update'],
    ])
  })

  it('has identical canonical order, IDs, clocks, payloads and manifests across twenty runs', () => {
    const fixture = inputs({
      v4Operations: [
        v4('update', 8),
        v4('add', 4, { namespace: 'physical-B', annotationId: 'b1', opId: 'b-add', linkId: 'link-b', annotation: { ...annotation('b1'), sourceContentRevision: 7 } }),
        v4('delete', 9),
      ],
      v4Materialized: [materialized()],
    })
    const baseline = JSON.stringify(planThoughtRepair(fixture))
    for (let run = 0; run < 20; run += 1) expect(JSON.stringify(planThoughtRepair(fixture))).toBe(baseline)
  })
})
