import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Annotation } from '../annotations'
import { IdentityLease } from '../identity'
import { ownedDatabaseName } from '../storage-ownership'
import {
  commitAnnotationOperation,
  enumerateAnnotatedLinkIds,
  listAnnotatedLinks,
  readAnnotationSnapshot,
} from './annotation-store'
import { listAnnotationOperationsForTest } from '../../test/annotation-operations'
import type {
  AnnotationOperationInput,
  AnnotationTarget,
  LegacyStaleAnnotationAddDraft,
  SavedContentAnnotationAddDraft,
  SummaryAnnotationAddDraft,
} from './annotation-types'
import {
  ANNOTATED_LINKS_STORE,
  ANNOTATION_LINK_STATE_STORE,
  ANNOTATION_MATERIALIZED_STORE,
  resetUserDataDatabaseHandle,
} from './idb'

const CONTENT_REVISION_7 = {
  kind: 'saved-content',
  contentRevision: 7,
} as const satisfies AnnotationTarget
const CONTENT_REVISION_8 = {
  kind: 'saved-content',
  contentRevision: 8,
} as const satisfies AnnotationTarget
const SUMMARY_A = {
  kind: 'summary',
  sourceHash: 'a'.repeat(64),
} as const satisfies AnnotationTarget
const LEGACY_CONTENT = {
  kind: 'legacy-stale',
  sourceKey: 'unverified-content-v1',
} as const satisfies AnnotationTarget

function identity(namespace: string): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `server-${namespace}`,
    physicalNamespace: namespace,
    localEpoch: 1,
  })
}

function contentAnnotation(
  id: string,
  revision = 7,
  overrides: Partial<Annotation> = {},
): Annotation {
  return {
    id,
    blockKey: 'content-document',
    start: 0,
    end: 4,
    text: 'text',
    note: '',
    source: 'self',
    createdAt: 100,
    updatedAt: 100,
    sourceContentRevision: revision,
    ...overrides,
  }
}

function contentDraft(
  id: string,
  _revision = 7,
  overrides: Partial<SavedContentAnnotationAddDraft> = {},
): SavedContentAnnotationAddDraft {
  return {
    id,
    blockKey: 'content-document',
    start: 0,
    end: 4,
    text: 'text',
    note: '',
    source: 'self',
    createdAt: 100,
    updatedAt: 100,
    ...overrides,
  }
}

function summaryDraft(id: string): SummaryAnnotationAddDraft {
  return {
    id,
    start: 1,
    end: 5,
    text: 'note',
    note: '',
    source: 'ai',
    createdAt: 101,
    updatedAt: 101,
  }
}

function legacyDraft(id: string): LegacyStaleAnnotationAddDraft {
  return {
    ...contentDraft(id),
    blockKey: 'content-document',
  }
}

async function deleteUserDataDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('database delete failed'))
    request.onblocked = () => reject(new Error('database delete was blocked'))
  })
}

afterEach(async () => {
  vi.restoreAllMocks()
  await deleteUserDataDatabase()
})

describe('transactional annotation operation store', () => {
  it('keeps namespace, link, and every source target isolated and discoverable', async () => {
    const leaseA = identity('physical-A')
    const leaseB = identity('physical-B')

    await expect(commitAnnotationOperation(leaseA, {
      kind: 'add',
      opId: 'A-content-7',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('same-annotation-id'),
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
    await expect(commitAnnotationOperation(leaseA, {
      kind: 'add',
      opId: 'A-content-8',
      linkId: 'L1',
      target: CONTENT_REVISION_8,
      draft: contentDraft('same-annotation-id', 8),
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
    await expect(commitAnnotationOperation(leaseA, {
      kind: 'add',
      opId: 'A-content-7-L2',
      linkId: 'L2',
      target: CONTENT_REVISION_7,
      draft: contentDraft('same-annotation-id'),
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
    await expect(commitAnnotationOperation(leaseA, {
      kind: 'add',
      opId: 'A-summary',
      linkId: 'L1',
      target: SUMMARY_A,
      draft: summaryDraft('same-annotation-id'),
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
    await expect(commitAnnotationOperation(leaseA, {
      kind: 'add',
      opId: 'A-legacy',
      linkId: 'old-link',
      target: LEGACY_CONTENT,
      draft: legacyDraft('same-annotation-id'),
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
    await expect(commitAnnotationOperation(leaseB, {
      kind: 'add',
      opId: 'B-content-7',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('same-annotation-id'),
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })

    await expect(readAnnotationSnapshot(leaseA, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({
        ok: true,
        value: {
          annotations: [{ id: 'same-annotation-id' }],
          annotationStoreVersion: 1,
        },
      })
    await expect(readAnnotationSnapshot(leaseA, 'L1', CONTENT_REVISION_8))
      .resolves.toMatchObject({
        ok: true,
        value: { annotations: [{ id: 'same-annotation-id' }] },
      })
    await expect(readAnnotationSnapshot(leaseB, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({
        ok: true,
        value: { annotations: [{ id: 'same-annotation-id' }] },
      })

    const rowsA = await listAnnotatedLinks(leaseA)
    expect(rowsA).toMatchObject({ ok: true })
    if (rowsA.ok) {
      expect(rowsA.value.map((row) => [row.linkId, row.target.kind])).toEqual([
        ['L1', 'saved-content'],
        ['L1', 'saved-content'],
        ['L1', 'summary'],
        ['L2', 'saved-content'],
        ['old-link', 'legacy-stale'],
      ])
      expect(rowsA.value.every((row) => row.namespace === 'physical-A')).toBe(true)
    }
    await expect(enumerateAnnotatedLinkIds(leaseA)).resolves.toEqual({
      ok: true,
      value: ['L1', 'L2', 'old-link'],
    })
    await expect(enumerateAnnotatedLinkIds(leaseB)).resolves.toEqual({
      ok: true,
      value: ['L1'],
    })
  })

  it('makes an exact duplicate opId idempotent and rejects a colliding payload', async () => {
    const lease = identity('physical-A')
    const operation = {
      kind: 'add',
      opId: 'stable-operation-id',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('a1'),
    } as const

    const first = await commitAnnotationOperation(lease, operation)
    const duplicate = await commitAnnotationOperation(lease, operation)
    const collision = await commitAnnotationOperation(lease, {
      ...operation,
      draft: contentDraft('different-id'),
    })

    expect(first).toMatchObject({
      ok: true,
      value: { status: 'committed', sequence: 1, annotationStoreVersion: 1 },
    })
    expect(duplicate).toEqual({
      ok: true,
      value: {
        status: 'duplicate',
        sequence: 1,
        annotationStoreVersion: 1,
        annotation: contentAnnotation('a1'),
      },
    })
    expect(collision).toEqual({ ok: true, value: { status: 'op-id-conflict' } })
    await expect(listAnnotationOperationsForTest(lease, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({ ok: true, value: [{ opId: 'stable-operation-id' }] })
    await expect(readAnnotationSnapshot(lease, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({ ok: true, value: { annotations: [{ id: 'a1' }] } })
  })

  it.each([
    { storeName: ANNOTATION_MATERIALIZED_STORE, method: 'put', seeded: false },
    { storeName: ANNOTATION_LINK_STATE_STORE, method: 'put', seeded: false },
    { storeName: ANNOTATED_LINKS_STORE, method: 'put', seeded: false },
    { storeName: ANNOTATED_LINKS_STORE, method: 'delete', seeded: true },
  ] as const)(
    'rolls back every durable surface when $storeName.$method fails',
    async ({ storeName, method, seeded }) => {
      const lease = identity('physical-A')
      if (seeded) {
        await commitAnnotationOperation(lease, {
          kind: 'add',
          opId: 'atomic-seed',
          linkId: 'L1',
          target: CONTENT_REVISION_7,
          draft: contentDraft('a1'),
        })
      }
      if (method === 'put') {
        const originalPut = IDBObjectStore.prototype.put
        vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (
          this: IDBObjectStore,
          value: unknown,
          key?: IDBValidKey,
        ) {
          if (this.name === storeName) throw new Error(`injected ${storeName}.put failure`)
          return key === undefined
            ? originalPut.call(this, value)
            : originalPut.call(this, value, key)
        })
      } else {
        const originalDelete = IDBObjectStore.prototype.delete
        vi.spyOn(IDBObjectStore.prototype, 'delete').mockImplementation(function (
          this: IDBObjectStore,
          query: IDBValidKey | IDBKeyRange,
        ) {
          if (this.name === storeName) {
            throw new Error(`injected ${storeName}.delete failure`)
          }
          return originalDelete.call(this, query)
        })
      }

      const failingOperation: AnnotationOperationInput = seeded
        ? {
            kind: 'delete',
            opId: 'atomic-failing-operation',
            linkId: 'L1',
            target: CONTENT_REVISION_7,
            annotationId: 'a1',
          }
        : {
            kind: 'add',
            opId: 'atomic-failing-operation',
            linkId: 'L1',
            target: CONTENT_REVISION_7,
            draft: contentDraft('a1'),
          }
      await expect(commitAnnotationOperation(lease, failingOperation))
        .resolves.toEqual({ ok: false })
      await expect(listAnnotationOperationsForTest(lease, 'L1', CONTENT_REVISION_7))
        .resolves.toMatchObject({
          ok: true,
          value: seeded ? [{ opId: 'atomic-seed' }] : [],
        })
      await expect(readAnnotationSnapshot(lease, 'L1', CONTENT_REVISION_7))
        .resolves.toMatchObject({
          ok: true,
          value: {
            annotationStoreVersion: seeded ? 1 : 0,
            annotations: seeded ? [{ id: 'a1' }] : [],
          },
        })
      await expect(listAnnotatedLinks(lease)).resolves.toMatchObject({
        ok: true,
        value: seeded ? [{ linkId: 'L1', annotationCount: 1 }] : [],
      })
    })

  it('serializes stale projections so concurrent distinct adds are both retained', async () => {
    const lease = identity('physical-A')
    const beforeA = await readAnnotationSnapshot(lease, 'L1', CONTENT_REVISION_7)
    const beforeB = await readAnnotationSnapshot(lease, 'L1', CONTENT_REVISION_7)
    expect(beforeA).toMatchObject({ ok: true, value: { annotationStoreVersion: 0 } })
    expect(beforeB).toMatchObject({ ok: true, value: { annotationStoreVersion: 0 } })

    const [left, right] = await Promise.all([
      commitAnnotationOperation(lease, {
        kind: 'add',
        opId: 'left-add',
        linkId: 'L1',
        target: CONTENT_REVISION_7,
        draft: contentDraft('left'),
      }),
      commitAnnotationOperation(lease, {
        kind: 'add',
        opId: 'right-add',
        linkId: 'L1',
        target: CONTENT_REVISION_7,
        draft: contentDraft('right', 7, { createdAt: 101, updatedAt: 101 }),
      }),
    ])

    expect(left).toMatchObject({ ok: true, value: { status: 'committed' } })
    expect(right).toMatchObject({ ok: true, value: { status: 'committed' } })
    if (!left.ok || !right.ok || left.value.status !== 'committed' ||
      right.value.status !== 'committed') {
      throw new Error('both concurrent operations must commit')
    }
    expect(new Set([left.value.sequence, right.value.sequence]).size).toBe(2)
    await expect(readAnnotationSnapshot(lease, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({
        ok: true,
        value: {
          annotationStoreVersion: Math.max(left.value.sequence, right.value.sequence),
          annotations: [{ id: 'left' }, { id: 'right' }],
        },
      })
  })

  it('aborts before durable commit when the document generation signal changes', async () => {
    const lease = identity('physical-A')
    const generation = new AbortController()
    const pending = commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'old-document-selection',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('must-not-commit'),
    }, { signal: generation.signal })

    generation.abort()

    await expect(pending).resolves.toEqual({ ok: false })
    await expect(readAnnotationSnapshot(lease, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({ ok: true, value: { annotations: [] } })
    await expect(listAnnotationOperationsForTest(lease, 'L1', CONTENT_REVISION_7))
      .resolves.toEqual({ ok: true, value: [] })
  })

  it('resolves same-ID add, update, and delete by the global committed sequence', async () => {
    const lease = identity('physical-A')
    await commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'seed',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('shared', 7, { note: 'seed', updatedAt: 9_999 }),
    })

    const [updated, deleted, replaced] = await Promise.all([
      commitAnnotationOperation(lease, {
        kind: 'update',
        opId: 'concurrent-update',
        linkId: 'L1',
        target: CONTENT_REVISION_7,
        annotationId: 'shared',
        patch: { note: 'old projection update', updatedAt: 2 },
      }),
      commitAnnotationOperation(lease, {
        kind: 'delete',
        opId: 'concurrent-delete',
        linkId: 'L1',
        target: CONTENT_REVISION_7,
        annotationId: 'shared',
      }),
      commitAnnotationOperation(lease, {
        kind: 'add',
        opId: 'concurrent-replacement',
        linkId: 'L1',
        target: CONTENT_REVISION_7,
        draft: contentDraft('shared', 7, {
          note: 'highest sequence replacement',
          createdAt: 1,
          updatedAt: 1,
        }),
      }),
    ])

    expect(updated).toMatchObject({ ok: true, value: { status: 'committed' } })
    expect(deleted).toMatchObject({ ok: true, value: { status: 'committed' } })
    expect(replaced).toMatchObject({ ok: true, value: { status: 'committed' } })
    if (!updated.ok || !deleted.ok || !replaced.ok ||
      updated.value.status !== 'committed' || deleted.value.status !== 'committed' ||
      replaced.value.status !== 'committed') {
      throw new Error('all same-ID operations must commit')
    }
    const firstLatest = [
      { sequence: updated.value.sequence, annotation: {
        id: 'shared',
        note: 'old projection update',
        updatedAt: 2,
      } },
      { sequence: deleted.value.sequence, annotation: null },
      { sequence: replaced.value.sequence, annotation: {
        id: 'shared',
        note: 'highest sequence replacement',
        updatedAt: 1,
      } },
    ].reduce((latest, candidate) =>
      candidate.sequence > latest.sequence ? candidate : latest)
    await expect(readAnnotationSnapshot(lease, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({
        ok: true,
        value: {
          annotationStoreVersion: firstLatest.sequence,
          annotations: firstLatest.annotation ? [firstLatest.annotation] : [],
        },
      })

    const [finalAdd, finalUpdate, finalDelete] = await Promise.all([
      commitAnnotationOperation(lease, {
        kind: 'add',
        opId: 'final-add',
        linkId: 'L1',
        target: CONTENT_REVISION_7,
        draft: contentDraft('shared', 7, { note: 'resurrected' }),
      }),
      commitAnnotationOperation(lease, {
        kind: 'update',
        opId: 'final-update',
        linkId: 'L1',
        target: CONTENT_REVISION_7,
        annotationId: 'shared',
        patch: { note: 'last note', updatedAt: 0 },
      }),
      commitAnnotationOperation(lease, {
        kind: 'delete',
        opId: 'final-delete',
        linkId: 'L1',
        target: CONTENT_REVISION_7,
        annotationId: 'shared',
      }),
    ])
    expect(finalAdd).toMatchObject({ ok: true, value: { status: 'committed' } })
    expect(finalUpdate).toMatchObject({ ok: true, value: { status: 'committed' } })
    expect(finalDelete).toMatchObject({ ok: true, value: { status: 'committed' } })
    if (!finalAdd.ok || !finalUpdate.ok || !finalDelete.ok ||
      finalAdd.value.status !== 'committed' || finalUpdate.value.status !== 'committed' ||
      finalDelete.value.status !== 'committed') {
      throw new Error('the final same-ID operations must commit')
    }
    const finalLatest = [
      { sequence: finalAdd.value.sequence, annotation: {
        id: 'shared',
        note: 'resurrected',
        updatedAt: 100,
      } },
      { sequence: finalUpdate.value.sequence, annotation: {
        id: 'shared',
        note: 'last note',
        updatedAt: 0,
      } },
      { sequence: finalDelete.value.sequence, annotation: null },
    ].reduce((latest, candidate) =>
      candidate.sequence > latest.sequence ? candidate : latest)
    await expect(readAnnotationSnapshot(lease, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({
        ok: true,
        value: {
          annotationStoreVersion: finalLatest.sequence,
          annotations: finalLatest.annotation ? [finalLatest.annotation] : [],
        },
      })
  })

  it('uses one global commit sequence across namespaces without exposing their rows', async () => {
    const leaseA = identity('physical-A')
    const leaseB = identity('physical-B')
    const first = await commitAnnotationOperation(leaseA, {
      kind: 'add',
      opId: 'same-logical-op-id',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('a'),
    })
    const second = await commitAnnotationOperation(leaseB, {
      kind: 'add',
      opId: 'same-logical-op-id',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('b'),
    })

    expect(first).toMatchObject({ ok: true, value: { sequence: 1 } })
    expect(second).toMatchObject({ ok: true, value: { sequence: 2 } })
    await expect(listAnnotationOperationsForTest(leaseA, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({
        ok: true,
        value: [{ opId: 'same-logical-op-id', sequence: 1 }],
      })
    await expect(listAnnotationOperationsForTest(leaseB, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({
        ok: true,
        value: [{ opId: 'same-logical-op-id', sequence: 2 }],
      })
  })

  it('lets an update committed after a delete win and remain replayable', async () => {
    const lease = identity('physical-A')
    await commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'update-delete-seed',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('shared', 7, { note: 'before delete' }),
    })

    const deleted = await commitAnnotationOperation(lease, {
      kind: 'delete',
      opId: 'delete-before-update',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      annotationId: 'shared',
    })
    const updated = await commitAnnotationOperation(lease, {
      kind: 'update',
      opId: 'update-after-delete',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      annotationId: 'shared',
      patch: { note: 'higher sequence update', updatedAt: 1 },
    })

    expect(deleted).toMatchObject({ ok: true, value: { status: 'committed' } })
    expect(updated).toMatchObject({ ok: true, value: { status: 'committed' } })
    if (!deleted.ok || !updated.ok || deleted.value.status !== 'committed' ||
      updated.value.status !== 'committed') {
      throw new Error('delete and update must both commit')
    }
    expect(deleted.value.sequence).toBeLessThan(updated.value.sequence)
    await expect(readAnnotationSnapshot(lease, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({
        ok: true,
        value: {
          annotationStoreVersion: updated.value.sequence,
          annotations: [{ id: 'shared', note: 'higher sequence update', updatedAt: 1 }],
        },
      })
    await expect(listAnnotationOperationsForTest(lease, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({
        ok: true,
        value: [
          { kind: 'add' },
          { kind: 'delete' },
          { kind: 'update', patch: { note: 'higher sequence update' } },
        ],
      })
  })

  it('derives saved-content source identity exclusively from the target', async () => {
    const lease = identity('physical-A')

    await expect(commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'target-owned-revision',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('derived'),
    })).resolves.toMatchObject({ ok: true, value: { status: 'committed' } })
    await expect(readAnnotationSnapshot(lease, 'L1', CONTENT_REVISION_7))
      .resolves.toMatchObject({
        ok: true,
        value: {
          annotations: [{
            id: 'derived',
            blockKey: 'content-document',
            sourceContentRevision: 7,
          }],
        },
      })
  })

  it('rejects ambiguous link IDs and non-canonical summary hashes at every API boundary', async () => {
    const lease = identity('physical-A')
    const ambiguousLinkId = 'L1\0summary:forged'

    await expect(commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'ambiguous-link',
      linkId: ambiguousLinkId,
      target: CONTENT_REVISION_7,
      draft: contentDraft('ambiguous'),
    })).resolves.toEqual({ ok: false })
    await expect(readAnnotationSnapshot(lease, ambiguousLinkId, CONTENT_REVISION_7))
      .resolves.toEqual({ ok: false })
    await expect(listAnnotationOperationsForTest(lease, ambiguousLinkId, CONTENT_REVISION_7))
      .resolves.toEqual({ ok: false })

    await expect(commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'invalid-summary-hash',
      linkId: 'L1',
      target: { kind: 'summary', sourceHash: 'A'.repeat(64) },
      draft: summaryDraft('invalid-summary'),
    })).resolves.toEqual({ ok: false })
    await expect(enumerateAnnotatedLinkIds(lease)).resolves.toEqual({ ok: true, value: [] })
  })

  it('removes the annotated-link row only when the target loses its last active item', async () => {
    const lease = identity('physical-A')
    await commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'add-one',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('one'),
    })
    await commitAnnotationOperation(lease, {
      kind: 'add',
      opId: 'add-two',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      draft: contentDraft('two'),
    })
    await commitAnnotationOperation(lease, {
      kind: 'delete',
      opId: 'delete-one',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      annotationId: 'one',
    })
    await expect(listAnnotatedLinks(lease)).resolves.toMatchObject({
      ok: true,
      value: [{ linkId: 'L1', annotationCount: 1 }],
    })

    await commitAnnotationOperation(lease, {
      kind: 'delete',
      opId: 'delete-two',
      linkId: 'L1',
      target: CONTENT_REVISION_7,
      annotationId: 'two',
    })
    await expect(listAnnotatedLinks(lease)).resolves.toEqual({ ok: true, value: [] })
  })

  it('enumerates all 150 annotated links without a recent-list limit', async () => {
    const lease = identity('physical-A')
    const linkIds = Array.from({ length: 150 }, (_, index) =>
      `L${String(index + 1).padStart(3, '0')}`)
    const writes = await Promise.all(linkIds.map((linkId, index) =>
      commitAnnotationOperation(lease, {
        kind: 'add',
        opId: `annotated-link-${index}`,
        linkId,
        target: CONTENT_REVISION_7,
        draft: contentDraft(`annotation-${index}`, 7, {
          createdAt: index,
          updatedAt: index,
        }),
      })))

    expect(writes.every((result) => result.ok && result.value.status === 'committed'))
      .toBe(true)
    await expect(enumerateAnnotatedLinkIds(lease)).resolves.toEqual({
      ok: true,
      value: linkIds,
    })
  })
})
