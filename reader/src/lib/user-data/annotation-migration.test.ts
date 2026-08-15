import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Annotation } from '../annotations'
import { AnnotationDocumentChannel } from '../article/document-channel'
import { IdentityLease } from '../identity'
import {
  ownedDatabaseName,
  ownedStorageKeyForLease,
  readOwnedStorageForLease,
} from '../storage-ownership'
import { migrateLegacyAnnotations } from './annotation-migration'
import {
  commitAnnotationOperation,
  listAnnotatedLinks,
  readAnnotationSnapshot,
} from './annotation-store'
import { listAnnotationOperationsForTest } from '../../test/annotation-operations'
import {
  ANNOTATED_LINKS_STORE,
  resetUserDataDatabaseHandle,
} from './idb'
import type { AnnotationTarget } from './annotation-types'

const HASH_A = 'a'.repeat(64)
const HASH_B = 'b'.repeat(64)
const activeLeases: IdentityLease[] = []

function identity(namespace = 'physical-A'): IdentityLease {
  const lease = new IdentityLease({
    serverClientDataNamespace: `server-${namespace}`,
    physicalNamespace: namespace,
    localEpoch: activeLeases.length + 1,
  })
  activeLeases.push(lease)
  return lease
}

function annotation(
  id: string,
  blockKey: string,
  overrides: Partial<Annotation> = {},
): Annotation {
  return {
    id,
    blockKey,
    start: 0,
    end: 4,
    text: 'text',
    note: '',
    source: 'self',
    createdAt: 1,
    updatedAt: 1,
    ...overrides,
  }
}

function storageKey(lease: IdentityLease, id: 'annotationsV1' | 'annotationsV2'): string {
  const key = ownedStorageKeyForLease(id, lease)
  if (!key) throw new Error(`missing ${id} storage key`)
  return key
}

function storeV2(
  lease: IdentityLease,
  envelope: Record<string, unknown>,
): string {
  const raw = JSON.stringify({ L1: envelope })
  localStorage.setItem(storageKey(lease, 'annotationsV2'), raw)
  return raw
}

async function snapshot(
  lease: IdentityLease,
  target: AnnotationTarget,
): Promise<readonly Annotation[]> {
  const result = await readAnnotationSnapshot(lease, 'L1', target)
  if (!result.ok) throw new Error('annotation snapshot read failed')
  return result.value.annotations
}

async function deleteDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('database delete failed'))
    request.onblocked = () => reject(new Error('database delete was blocked'))
  })
}

afterEach(async () => {
  for (const lease of activeLeases.splice(0)) lease.revoke()
  vi.restoreAllMocks()
  localStorage.clear()
  await deleteDatabase()
})

describe('legacy annotation operation-log migration', () => {
  it('binds active envelopes but requires per-item identity for quarantined records', async () => {
    const lease = identity()
    storeV2(lease, {
      contentRevision: 7,
      summarySourceHash: HASH_A,
      items: [
        annotation('active-body', 'content-document'),
        annotation('active-summary', 'summary'),
        annotation('retired-dr', 'dr'),
      ],
      quarantinedItems: [
        annotation('old-body', 'content', { sourceContentRevision: 6 }),
        annotation('old-summary', 'summary', { sourceSummaryHash: HASH_B }),
        annotation('unverified-body', 'content'),
      ],
    })

    await expect(migrateLegacyAnnotations(lease)).resolves.toEqual({
      status: 'migrated',
      examined: 6,
      applied: 6,
      removedBackups: 0,
    })
    expect(readOwnedStorageForLease('annotationsV2', lease)).not.toBeNull()
    expect(await snapshot(lease, { kind: 'saved-content', contentRevision: 7 }))
      .toEqual([expect.objectContaining({
        id: 'active-body',
        sourceContentRevision: 7,
      })])
    expect(await snapshot(lease, { kind: 'summary', sourceHash: HASH_A }))
      .toEqual([expect.objectContaining({
        id: 'active-summary',
        sourceSummaryHash: HASH_A,
      })])
    expect(await snapshot(lease, { kind: 'saved-content', contentRevision: 6 }))
      .toEqual([expect.objectContaining({ id: 'old-body' })])
    expect(await snapshot(lease, { kind: 'summary', sourceHash: HASH_B }))
      .toEqual([expect.objectContaining({ id: 'old-summary' })])

    const indexed = await listAnnotatedLinks(lease)
    if (!indexed.ok) throw new Error('annotated index read failed')
    const stale = indexed.value.filter((record) => record.target.kind === 'legacy-stale')
    expect(stale).toHaveLength(2)
    const staleIDs = (
      await Promise.all(stale.map((record) => snapshot(lease, record.target)))
    ).flatMap((items) => items.map((item) => item.id)).sort()
    expect(staleIDs).toEqual(['retired-dr', 'unverified-body'])
  })

  it('publishes one durable hint for every imported document target', async () => {
    const lease = identity()
    storeV2(lease, {
      contentRevision: 7,
      summarySourceHash: HASH_A,
      items: [
        annotation('body-7', 'content'),
        annotation('summary-a', 'summary'),
      ],
      quarantinedItems: [
        annotation('body-6', 'content', { sourceContentRevision: 6 }),
        annotation('summary-b', 'summary', { sourceSummaryHash: HASH_B }),
      ],
    })
    const publish = vi.spyOn(AnnotationDocumentChannel.prototype, 'publish')

    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({
      status: 'migrated',
      applied: 4,
    })

    expect(publish).toHaveBeenCalledTimes(3)
    const hints = publish.mock.calls.map(([hint]) => hint)
    expect(hints).toEqual(expect.arrayContaining([
      expect.objectContaining({ linkId: 'L1', documentRevision: 0 }),
      expect.objectContaining({ linkId: 'L1', documentRevision: 6 }),
      expect.objectContaining({ linkId: 'L1', documentRevision: 7 }),
    ]))
    expect(hints.every((hint) => hint.annotationStoreVersion > 0)).toBe(true)
  })

  it('keeps every v1 annotation stale even when it contains revision-like fields', async () => {
    const lease = identity()
    localStorage.setItem(storageKey(lease, 'annotationsV1'), JSON.stringify({
      L1: [annotation('v1-body', 'content', { sourceContentRevision: 99 })],
    }))

    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({
      status: 'migrated',
      examined: 1,
      applied: 1,
      removedBackups: 0,
    })
    expect(await snapshot(lease, { kind: 'saved-content', contentRevision: 99 })).toEqual([])
    const indexed = await listAnnotatedLinks(lease)
    if (!indexed.ok) throw new Error('annotated index read failed')
    expect(indexed.value).toEqual([
      expect.objectContaining({ target: expect.objectContaining({ kind: 'legacy-stale' }) }),
    ])
    const staleItems = await snapshot(lease, indexed.value[0].target)
    expect(staleItems).toHaveLength(1)
    expect(staleItems[0]).not.toHaveProperty('sourceContentRevision')
    expect(staleItems[0]).not.toHaveProperty('sourceSummaryHash')
  })

  it('never rebinds an explicitly invalid item hash through the envelope fallback', async () => {
    const lease = identity()
    storeV2(lease, {
      contentRevision: 7,
      summarySourceHash: HASH_A,
      items: [annotation('invalid-item-hash', 'summary', {
        sourceSummaryHash: 'A'.repeat(64),
      })],
    })

    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({
      status: 'migrated',
      examined: 1,
      applied: 1,
    })
    expect(await snapshot(lease, { kind: 'summary', sourceHash: HASH_A })).toEqual([])
    const indexed = await listAnnotatedLinks(lease)
    if (!indexed.ok) throw new Error('annotated index read failed')
    expect(indexed.value).toEqual([
      expect.objectContaining({ target: expect.objectContaining({ kind: 'legacy-stale' }) }),
    ])
  })

  it('rolls back every operation, projection, index, and marker when import fails', async () => {
    const lease = identity()
    const raw = storeV2(lease, {
      contentRevision: 7,
      items: [annotation('must-rollback', 'content')],
    })
    const originalPut = IDBObjectStore.prototype.put
    vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (
      this: IDBObjectStore,
      ...args
    ) {
      if (this.name === ANNOTATED_LINKS_STORE) {
        throw new Error('injected annotated index failure')
      }
      return originalPut.apply(this, args as Parameters<IDBObjectStore['put']>)
    })

    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({
      status: 'unavailable',
      applied: 0,
      removedBackups: 0,
    })
    expect(readOwnedStorageForLease('annotationsV2', lease)).toBe(raw)
    expect(await snapshot(lease, { kind: 'saved-content', contentRevision: 7 })).toEqual([])
    await expect(listAnnotationOperationsForTest(
      lease,
      'L1',
      { kind: 'saved-content', contentRevision: 7 },
    )).resolves.toEqual({ ok: true, value: [] })
    await expect(listAnnotatedLinks(lease)).resolves.toEqual({ ok: true, value: [] })
  })

  it('uses the committed marker to replay a retained recovery backup idempotently', async () => {
    const lease = identity()
    storeV2(lease, {
      contentRevision: 7,
      items: [annotation('once', 'content')],
    })

    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({
      status: 'migrated',
      applied: 1,
      removedBackups: 0,
    })
    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({
      status: 'migrated',
      applied: 0,
      removedBackups: 0,
    })
    expect(readOwnedStorageForLease('annotationsV2', lease)).not.toBeNull()
    const operations = await listAnnotationOperationsForTest(
      lease,
      'L1',
      { kind: 'saved-content', contentRevision: 7 },
    )
    expect(operations).toMatchObject({ ok: true, value: [{ kind: 'add' }] })
  })

  it('never removes the legacy source because old tabs can write concurrently', async () => {
    const lease = identity()
    storeV2(lease, {
      contentRevision: 7,
      items: [annotation('first', 'content')],
    })
    const key = storageKey(lease, 'annotationsV2')
    const newerRaw = JSON.stringify({
      L1: {
        contentRevision: 7,
        items: [
          annotation('first', 'content'),
          annotation('second', 'content', { updatedAt: 2 }),
        ],
      },
    })
    const removeItem = vi.spyOn(Storage.prototype, 'removeItem')

    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({
      status: 'migrated',
      applied: 1,
      removedBackups: 0,
    })
    expect(removeItem).not.toHaveBeenCalledWith(key)
    expect(localStorage.getItem(key)).not.toBeNull()

    localStorage.setItem(key, newerRaw)
    expect(localStorage.getItem(key)).toBe(newerRaw)
    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({
      status: 'migrated',
      examined: 2,
      applied: 1,
      removedBackups: 0,
    })
    expect(localStorage.getItem(key)).toBe(newerRaw)
    expect((await snapshot(lease, { kind: 'saved-content', contentRevision: 7 }))
      .map((item) => item.id).sort()).toEqual(['first', 'second'])
  })

  it('lets a newer snapshot update import-owned data', async () => {
    const lease = identity()
    storeV2(lease, {
      contentRevision: 7,
      items: [annotation('shared', 'content', { note: 'old', updatedAt: 10 })],
    })
    await migrateLegacyAnnotations(lease)
    storeV2(lease, {
      contentRevision: 7,
      items: [annotation('shared', 'content', { note: 'new snapshot', updatedAt: 20 })],
    })

    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({ applied: 1 })
    expect(await snapshot(lease, { kind: 'saved-content', contentRevision: 7 }))
      .toEqual([expect.objectContaining({ note: 'new snapshot', updatedAt: 20 })])
  })

  it('never lets a later legacy snapshot overwrite an RF5B live edit', async () => {
    const lease = identity()
    storeV2(lease, {
      contentRevision: 7,
      items: [annotation('shared', 'content', { note: 'imported', updatedAt: 10 })],
    })
    await migrateLegacyAnnotations(lease)
    await commitAnnotationOperation(lease, {
      kind: 'update',
      opId: 'live-edit',
      linkId: 'L1',
      target: { kind: 'saved-content', contentRevision: 7 },
      annotationId: 'shared',
      patch: { note: 'live wins', updatedAt: 20 },
    })
    storeV2(lease, {
      contentRevision: 7,
      items: [annotation('shared', 'content', {
        note: 'old tab tries to overwrite',
        updatedAt: 30,
      })],
    })

    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({
      examined: 1,
      applied: 0,
      removedBackups: 0,
    })
    expect(await snapshot(lease, { kind: 'saved-content', contentRevision: 7 }))
      .toEqual([expect.objectContaining({ note: 'live wins', updatedAt: 20 })])
  })

  it('compacts a legacy import once one target exceeds the operation threshold', async () => {
    const lease = identity()
    storeV2(lease, {
      contentRevision: 7,
      items: Array.from({ length: 257 }, (_, index) =>
        annotation(`bulk-${index}`, 'content', { start: index, end: index + 1 })),
    })

    await expect(migrateLegacyAnnotations(lease)).resolves.toMatchObject({
      status: 'migrated',
      examined: 257,
      applied: 257,
      removedBackups: 0,
    })

    const target = { kind: 'saved-content', contentRevision: 7 } as const
    const durable = await readAnnotationSnapshot(lease, 'L1', target)
    expect(durable).toMatchObject({
      ok: true,
      value: {
        annotations: expect.arrayContaining([
          expect.objectContaining({ id: 'bulk-0' }),
          expect.objectContaining({ id: 'bulk-256' }),
        ]),
      },
    })
    if (!durable.ok) throw new Error('bulk migration snapshot must be readable')
    expect(durable.value.annotations).toHaveLength(257)
    await expect(listAnnotationOperationsForTest(lease, 'L1', target)).resolves.toEqual({
      ok: true,
      value: [],
    })
  })

  it('leaves malformed localStorage untouched for recovery', async () => {
    const lease = identity()
    const key = storageKey(lease, 'annotationsV2')
    localStorage.setItem(key, '{not-json')

    await expect(migrateLegacyAnnotations(lease)).resolves.toEqual({
      status: 'unavailable',
      examined: 0,
      applied: 0,
      removedBackups: 0,
    })
    expect(localStorage.getItem(key)).toBe('{not-json')
  })

  it.each([
    {
      label: 'NUL-delimited link ID',
      value: {
        'L1\0summary:forged': {
          contentRevision: 7,
          items: [annotation('ambiguous-link', 'content')],
        },
      },
    },
    {
      label: 'non-canonical summary hash',
      value: {
        L1: {
          contentRevision: 7,
          summarySourceHash: 'A'.repeat(64),
          items: [annotation('invalid-summary', 'summary')],
        },
      },
    },
  ])('retains and rejects a legacy snapshot with $label', async ({ value }) => {
    const lease = identity()
    const key = storageKey(lease, 'annotationsV2')
    const raw = JSON.stringify(value)
    localStorage.setItem(key, raw)

    await expect(migrateLegacyAnnotations(lease)).resolves.toEqual({
      status: 'unavailable',
      examined: 0,
      applied: 0,
      removedBackups: 0,
    })
    expect(localStorage.getItem(key)).toBe(raw)
    await expect(listAnnotatedLinks(lease)).resolves.toEqual({ ok: true, value: [] })
  })
})
