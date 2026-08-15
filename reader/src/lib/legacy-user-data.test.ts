import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  getLegacyImportPrompt,
  importLegacyData,
  keepLegacyDataIsolated,
  quarantineLegacyData,
  resetUserDataDatabaseHandle,
  type LegacyUnattributedRecord,
} from './legacy-user-data'
import {
  ownedDatabaseName,
  ownedStorageKey,
  readOwnedStorageForLease,
  readOwnedStorage,
  writeOwnedStorage,
} from './storage-ownership'
import { IdentityLease, readerIdentity } from './identity'
import { USER_DATA_DATABASE_VERSION } from './user-data/idb'

async function deleteUserDataDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => resolve()
    request.onblocked = () => resolve()
  })
}

async function seedResolvedVersionOneBatch(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.open(ownedDatabaseName('userDataDatabase'), 1)
    request.onupgradeneeded = () => {
      const pending = request.result.createObjectStore('legacy-unattributed', { keyPath: 'id' })
      const decisions = request.result.createObjectStore('migration-decisions', { keyPath: 'id' })
      pending.put({
        id: 'pins',
        legacyKey: 'webtag:pins:v1',
        value: '{"tags":["already-claimed-by-A"],"domains":[]}',
        quarantinedAt: 1,
      })
      decisions.put({
        id: 'resolution',
        kind: 'imported',
        namespace: 'physical-A',
        decidedAt: 2,
      })
    }
    request.onsuccess = () => {
      request.result.close()
      resolve()
    }
    request.onerror = () => reject(request.error)
  })
}

async function seedResolvedVersionTwoArchives(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.open(ownedDatabaseName('userDataDatabase'), 2)
    request.onupgradeneeded = () => {
      request.result.createObjectStore('legacy-unattributed', { keyPath: 'id' })
      const archive = request.result.createObjectStore('legacy-import-archive', {
        keyPath: 'archiveID',
        autoIncrement: true,
      })
      const decisions = request.result.createObjectStore('migration-decisions', { keyPath: 'id' })
      archive.add({
        id: 'pins',
        legacyKey: 'webtag:pins:v1',
        value: '{"tags":["already-claimed-by-A-1"],"domains":[]}',
        quarantinedAt: 1,
        importedIntoNamespace: 'physical-A',
        importedAt: 2,
      })
      archive.add({
        id: 'pins',
        legacyKey: 'webtag:pins:v1',
        value: '{"tags":["already-claimed-by-A-2"],"domains":[]}',
        quarantinedAt: 3,
        importedIntoNamespace: 'physical-A',
        importedAt: 4,
      })
      decisions.put({
        id: 'resolution',
        kind: 'imported',
        namespace: 'physical-A',
        decidedAt: 2,
      })
    }
    request.onsuccess = () => {
      request.result.close()
      resolve()
    }
    request.onerror = () => reject(request.error)
  })
}

interface ArchiveInspection {
  readonly version: number
  readonly indexNames: string[]
  readonly records: Array<LegacyUnattributedRecord & {
    readonly importedIntoNamespace: string
    readonly fingerprintVersion?: number
  }>
}

async function inspectArchive(
  namespace: string,
): Promise<ArchiveInspection> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(ownedDatabaseName('userDataDatabase'))
    request.onerror = () => reject(request.error)
    request.onsuccess = () => {
      const database = request.result
      const transaction = database.transaction('legacy-import-archive', 'readonly')
      const archive = transaction.objectStore('legacy-import-archive')
      const records = archive.index('by-imported-namespace').getAll(namespace)
      transaction.oncomplete = () => {
        database.close()
        resolve({
          version: database.version,
          indexNames: Array.from(archive.indexNames),
          records: records.result as ArchiveInspection['records'],
        })
      }
      transaction.onerror = () => {
        database.close()
        reject(transaction.error)
      }
      transaction.onabort = transaction.onerror
    }
  })
}

async function listLegacyUnattributed(): Promise<LegacyUnattributedRecord[]> {
  return new Promise((resolve) => {
    const request = indexedDB.open(ownedDatabaseName('userDataDatabase'))
    request.onerror = () => resolve([])
    request.onsuccess = () => {
      const database = request.result
      const transaction = database.transaction(
        ['legacy-unattributed', 'legacy-import-archive'],
        'readonly',
      )
      const pending = transaction.objectStore('legacy-unattributed').getAll()
      const archive = transaction.objectStore('legacy-import-archive').getAll()
      transaction.oncomplete = () => {
        database.close()
        resolve([
          ...(pending.result as LegacyUnattributedRecord[]),
          ...(archive.result as LegacyUnattributedRecord[]),
        ])
      }
      transaction.onerror = () => {
        database.close()
        resolve([])
      }
      transaction.onabort = transaction.onerror
    }
  })
}

afterEach(async () => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  await deleteUserDataDatabase()
})

describe('legacy-unattributed migration', () => {
  it('quarantines user assets without claiming them for the active identity', async () => {
    localStorage.setItem('webtag:annotations:v1', '{"legacy-v1":[]}')
    localStorage.setItem('webtag:annotations:v2', '{"legacy-v2":{"contentRevision":3,"items":[]}}')
    localStorage.setItem('webtag:pins:v1', '{"tags":["legacy"],"domains":[]}')
    localStorage.setItem('webtag:content-revision-floor', '{"legacy-link":9}')
    localStorage.setItem(
      'webtag:reader:feed-selection:v1',
      '{"kind":"subscription","id":"legacy-feed","name":"Legacy"}',
    )
    localStorage.setItem('webtag:reader:collapsed-feed-folders:v1', '["legacy-folder"]')
    localStorage.setItem('webtag:sbfold:tags', '0')
    localStorage.setItem('webtag:theme', 'dark')
    writeOwnedStorage('sidebarFold', '1', 'domains')
    const namespacedFoldKey = ownedStorageKey('sidebarFold', 'domains')

    const result = await quarantineLegacyData()

    expect(result).toEqual({ quarantined: 3, discarded: 4 })
    expect(await listLegacyUnattributed()).toEqual([
      expect.objectContaining({ id: 'annotationsV1', value: '{"legacy-v1":[]}' }),
      expect.objectContaining({
        id: 'annotationsV2',
        value: '{"legacy-v2":{"contentRevision":3,"items":[]}}',
      }),
      expect.objectContaining({
        id: 'pins',
        value: '{"tags":["legacy"],"domains":[]}',
      }),
    ])
    expect(readOwnedStorage('annotationsV1')).toBeNull()
    expect(readOwnedStorage('annotationsV2')).toBeNull()
    expect(readOwnedStorage('pins')).toBeNull()
    expect(localStorage.getItem('webtag:annotations:v1')).toBeNull()
    expect(localStorage.getItem('webtag:annotations:v2')).toBeNull()
    expect(localStorage.getItem('webtag:pins:v1')).toBeNull()
    expect(localStorage.getItem('webtag:content-revision-floor')).toBeNull()
    expect(localStorage.getItem('webtag:reader:feed-selection:v1')).toBeNull()
    expect(localStorage.getItem('webtag:reader:collapsed-feed-folders:v1')).toBeNull()
    expect(localStorage.getItem('webtag:sbfold:tags')).toBeNull()
    expect(localStorage.getItem('webtag:theme')).toBe('dark')
    expect(localStorage.getItem(namespacedFoldKey ?? '')).toBe('1')
  })

  it('imports only after the user explicitly selects a target identity', async () => {
    localStorage.setItem('webtag:annotations:v1', '{"legacy-link":[{"id":"legacy-v1"}]}')
    localStorage.setItem(
      'webtag:annotations:v2',
      '{"legacy-link":{"contentRevision":3,"items":[{"id":"legacy-v2"}]}}',
    )
    localStorage.setItem('webtag:pins:v1', '{"tags":["legacy"],"domains":["legacy.test"]}')
    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    await quarantineLegacyData()

    expect(await getLegacyImportPrompt(leaseB)).toEqual({
      assetCount: 3,
      sources: ['annotationsV1', 'annotationsV2', 'pins'],
    })
    expect(await keepLegacyDataIsolated(leaseB)).toBe(true)
    expect(await getLegacyImportPrompt(leaseB)).toBeNull()

    const leaseA = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    writeOwnedStorage('annotationsV1', '{"current-link":[{"id":"current-v1"}]}')
    writeOwnedStorage(
      'annotationsV2',
      '{"current-link":{"contentRevision":7,"items":[{"id":"current-v2"}]}}',
    )
    writeOwnedStorage('pins', '{"tags":["current"],"domains":[]}')

    expect(await getLegacyImportPrompt(leaseA)).toEqual({
      assetCount: 3,
      sources: ['annotationsV1', 'annotationsV2', 'pins'],
    })
    expect(await importLegacyData(leaseA)).toEqual({ status: 'imported', imported: 3 })
    expect(JSON.parse(readOwnedStorage('annotationsV1') ?? '{}')).toEqual({
      'current-link': [{ id: 'current-v1' }],
      'legacy-link': [{ id: 'legacy-v1' }],
    })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      'current-link': { contentRevision: 7, items: [{ id: 'current-v2' }] },
      'legacy-link': { contentRevision: 3, items: [{ id: 'legacy-v2' }] },
    })
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['current', 'legacy'],
      domains: ['legacy.test'],
    })
    expect(await listLegacyUnattributed()).toHaveLength(3)

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(readOwnedStorage('annotationsV1')).toBeNull()
    expect(readOwnedStorage('annotationsV2')).toBeNull()
    expect(readOwnedStorage('pins')).toBeNull()
    expect(await getLegacyImportPrompt(readerIdentity.activeLease!)).toBeNull()
  })

  it('keeps source-bound quarantined annotations without replacing newer namespaced items', async () => {
    const lease = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    writeOwnedStorage(
      'annotationsV2',
      JSON.stringify({
        link: {
          contentRevision: 7,
          summarySourceHash: 'summary-current',
          items: [{
            id: 'same-id',
            blockKey: 'content',
            note: 'newer namespaced note',
            sourceContentRevision: 7,
          }],
        },
      }),
    )
    localStorage.setItem(
      'webtag:annotations:v2',
      JSON.stringify({
        link: {
          contentRevision: 7,
          summarySourceHash: 'summary-current',
          items: [{
            id: 'same-id',
            blockKey: 'content',
            note: 'stale global note',
            sourceContentRevision: 7,
          }],
          quarantinedItems: [
            {
              id: 'same-id',
              blockKey: 'content',
              note: 'previous revision note',
              sourceContentRevision: 6,
            },
            {
              id: 'old-summary',
              blockKey: 'summary',
              note: 'previous summary note',
              sourceSummaryHash: 'summary-previous',
            },
          ],
        },
      }),
    )

    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })
    expect(await importLegacyData(lease)).toEqual({ status: 'imported', imported: 1 })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      link: {
        contentRevision: 7,
        summarySourceHash: 'summary-current',
        items: [{
          id: 'same-id',
          blockKey: 'content',
          note: 'newer namespaced note',
          sourceContentRevision: 7,
        }],
        quarantinedItems: [
          {
            id: 'same-id',
            blockKey: 'content',
            note: 'previous revision note',
            sourceContentRevision: 6,
          },
          {
            id: 'old-summary',
            blockKey: 'summary',
            note: 'previous summary note',
            sourceSummaryHash: 'summary-previous',
          },
        ],
      },
    })
  })

  it('captures a later quarantined source identity without replaying an earlier snapshot', async () => {
    const leaseA = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const firstSnapshot = {
      link: {
        contentRevision: 7,
        items: [{
          id: 'same-id',
          blockKey: 'content',
          note: 'current source',
          sourceContentRevision: 7,
        }],
        quarantinedItems: [{
          id: 'same-id',
          blockKey: 'content',
          note: 'first old source',
          sourceContentRevision: 6,
        }],
      },
    }
    localStorage.setItem('webtag:annotations:v2', JSON.stringify(firstSnapshot))
    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })
    expect(await importLegacyData(leaseA)).toEqual({ status: 'imported', imported: 1 })

    localStorage.setItem(
      'webtag:annotations:v2',
      JSON.stringify({
        link: {
          ...firstSnapshot.link,
          quarantinedItems: [
            ...firstSnapshot.link.quarantinedItems,
            {
              id: 'same-id',
              blockKey: 'content',
              note: 'second old source',
              sourceContentRevision: 5,
            },
          ],
        },
      }),
    )
    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })

    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(await getLegacyImportPrompt(leaseB)).toEqual({
      assetCount: 1,
      sources: ['annotationsV2'],
    })
    expect(await importLegacyData(leaseB)).toEqual({ status: 'imported', imported: 1 })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      link: {
        contentRevision: 7,
        items: [],
        quarantinedItems: [{
          id: 'same-id',
          blockKey: 'content',
          note: 'second old source',
          sourceContentRevision: 5,
        }],
      },
    })
  })

  it('merges quarantined annotations by source identity instead of annotation id alone', async () => {
    const lease = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    writeOwnedStorage(
      'annotationsV2',
      JSON.stringify({
        link: {
          contentRevision: 7,
          items: [{
            id: 'same-id',
            blockKey: 'content',
            note: 'newer namespaced note',
            sourceContentRevision: 7,
          }],
          quarantinedItems: [{
            id: 'same-id',
            blockKey: 'content',
            note: 'revision six',
            sourceContentRevision: 6,
          }],
        },
      }),
    )
    localStorage.setItem(
      'webtag:annotations:v2',
      JSON.stringify({
        link: {
          contentRevision: 7,
          items: [{
            id: 'same-id',
            blockKey: 'content',
            note: 'stale global note',
            sourceContentRevision: 7,
          }],
          quarantinedItems: [{
            id: 'same-id',
            blockKey: 'content',
            note: 'revision five',
            sourceContentRevision: 5,
          }],
        },
      }),
    )

    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })
    expect(await importLegacyData(lease)).toEqual({ status: 'imported', imported: 1 })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      link: {
        contentRevision: 7,
        items: [{
          id: 'same-id',
          blockKey: 'content',
          note: 'newer namespaced note',
          sourceContentRevision: 7,
        }],
        quarantinedItems: [
          {
            id: 'same-id',
            blockKey: 'content',
            note: 'revision five',
            sourceContentRevision: 5,
          },
          {
            id: 'same-id',
            blockKey: 'content',
            note: 'revision six',
            sourceContentRevision: 6,
          },
        ],
      },
    })
  })

  it('quarantines a stale global source instead of overwriting a newer namespaced annotation', async () => {
    const lease = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    writeOwnedStorage(
      'annotationsV2',
      JSON.stringify({
        link: {
          contentRevision: 8,
          items: [{
            id: 'same-id',
            blockKey: 'content',
            note: 'revision eight',
            sourceContentRevision: 8,
          }],
        },
      }),
    )
    localStorage.setItem(
      'webtag:annotations:v2',
      JSON.stringify({
        link: {
          contentRevision: 7,
          items: [{
            id: 'same-id',
            blockKey: 'content',
            note: 'stale revision seven',
          }],
          quarantinedItems: [{
            id: 'same-id',
            blockKey: 'content',
            note: 'stale revision six',
            sourceContentRevision: 6,
          }],
        },
      }),
    )

    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })
    expect(await importLegacyData(lease)).toEqual({ status: 'imported', imported: 1 })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      link: {
        contentRevision: 8,
        items: [{
          id: 'same-id',
          blockKey: 'content',
          note: 'revision eight',
          sourceContentRevision: 8,
        }],
        quarantinedItems: [
          {
            id: 'same-id',
            blockKey: 'content',
            note: 'stale revision seven',
            sourceContentRevision: 7,
          },
          {
            id: 'same-id',
            blockKey: 'content',
            note: 'stale revision six',
            sourceContentRevision: 6,
          },
        ],
      },
    })
  })

  it('serializes concurrent tab imports so only one identity can claim the quarantine', async () => {
    localStorage.setItem('webtag:pins:v1', '{"tags":["legacy"],"domains":[]}')
    await quarantineLegacyData()
    const leaseA = new IdentityLease({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: 1,
    })
    const leaseB = new IdentityLease({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
      localEpoch: 1,
    })

    const results = await Promise.all([importLegacyData(leaseA), importLegacyData(leaseB)])

    expect(results.map((result) => result.status).sort()).toEqual([
      'already-resolved',
      'imported',
    ])
    const importedValues = [
      readOwnedStorageForLease('pins', leaseA),
      readOwnedStorageForLease('pins', leaseB),
    ]
    expect(importedValues.filter((value) => value !== null)).toHaveLength(1)
    expect(await getLegacyImportPrompt(leaseA)).toBeNull()
    expect(await getLegacyImportPrompt(leaseB)).toBeNull()
  })

  it('does not write namespaced data when the durable claim transaction aborts', async () => {
    localStorage.setItem('webtag:pins:v1', '{"tags":["legacy"],"domains":[]}')
    await quarantineLegacyData()
    const leaseA = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const originalPut = IDBObjectStore.prototype.put
    const putSpy = vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (
      this: IDBObjectStore,
      value: unknown,
      key?: IDBValidKey,
    ) {
      const request = key === undefined
        ? originalPut.call(this, value)
        : originalPut.call(this, value, key)
      if (
        this.name === 'migration-decisions' &&
        typeof value === 'object' &&
        value !== null &&
        'id' in value &&
        value.id === 'resolution'
      ) {
        this.transaction.abort()
      }
      return request
    })

    expect(await importLegacyData(leaseA)).toEqual({ status: 'unavailable', imported: 0 })
    expect(readOwnedStorageForLease('pins', leaseA)).toBeNull()

    putSpy.mockRestore()
    expect(await getLegacyImportPrompt(leaseA)).toEqual({
      assetCount: 1,
      sources: ['pins'],
    })
  })

  it('keeps a failed delivery claimed by its selected identity until retry succeeds', async () => {
    localStorage.setItem('webtag:pins:v1', '{"tags":["legacy"],"domains":[]}')
    await quarantineLegacyData()
    const leaseA = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const namespacedAKey = ownedStorageKey('pins')
    const originalSetItem = Storage.prototype.setItem
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (
      this: Storage,
      key,
      value,
    ) {
      if (key === namespacedAKey) throw new DOMException('quota exceeded', 'QuotaExceededError')
      originalSetItem.call(this, key, value)
    })

    expect(await importLegacyData(leaseA)).toEqual({ status: 'unavailable', imported: 0 })
    expect(readOwnedStorageForLease('pins', leaseA)).toBeNull()

    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(await getLegacyImportPrompt(leaseB)).toBeNull()
    expect(await importLegacyData(leaseB)).toEqual({ status: 'already-resolved', imported: 0 })
    expect(readOwnedStorageForLease('pins', leaseB)).toBeNull()

    setItemSpy.mockRestore()
    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect(await getLegacyImportPrompt(readerIdentity.activeLease!)).toEqual({
      assetCount: 1,
      sources: ['pins'],
    })
    expect(await importLegacyData(readerIdentity.activeLease!)).toEqual({
      status: 'imported',
      imported: 1,
    })
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['legacy'],
      domains: [],
    })
  })

  it('bounds identity-scoped archive reads before another owner body is deserialized', async () => {
    localStorage.setItem('webtag:pins:v1', '{"tags":["claimed-by-A"],"domains":[]}')
    await quarantineLegacyData()
    const leaseA = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const namespacedAKey = ownedStorageKey('pins')
    const originalSetItem = Storage.prototype.setItem
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (
      this: Storage,
      key,
      value,
    ) {
      if (key === namespacedAKey) throw new DOMException('quota exceeded', 'QuotaExceededError')
      originalSetItem.call(this, key, value)
    })
    expect(await importLegacyData(leaseA)).toEqual({ status: 'unavailable', imported: 0 })
    setItemSpy.mockRestore()

    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    const objectStoreGetAll = vi.spyOn(IDBObjectStore.prototype, 'getAll')
    const indexGetAll = vi.spyOn(IDBIndex.prototype, 'getAll')

    expect(await getLegacyImportPrompt(leaseB)).toBeNull()
    expect(await importLegacyData(leaseB)).toEqual({ status: 'already-resolved', imported: 0 })
    localStorage.setItem(
      'webtag:pins:v1',
      '{"tags":["claimed-by-A","written-after-claim"],"domains":[]}',
    )
    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })

    expect(
      (objectStoreGetAll.mock.contexts as IDBObjectStore[]).filter(
        (store) => store.name === 'legacy-import-archive',
      ),
    ).toHaveLength(0)
    expect(indexGetAll).toHaveBeenCalled()
    expect(indexGetAll.mock.calls.map(([query]) => query)).toEqual(
      indexGetAll.mock.calls.map(() => 'physical-B'),
    )
  })

  it('does not reopen a batch captured before another tab finishes importing it', async () => {
    const legacyKey = 'webtag:pins:v1'
    const legacyValue = '{"tags":["single-claim"],"domains":[]}'
    localStorage.setItem(legacyKey, legacyValue)
    await quarantineLegacyData()

    // Simulate a second tab that captured the same still-visible global before
    // the first tab's import transaction commits.
    localStorage.setItem(legacyKey, legacyValue)
    const leaseA = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const startedImports: Array<ReturnType<typeof importLegacyData>> = []
    const originalGetItem = Storage.prototype.getItem
    const getItemSpy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(function (
      this: Storage,
      key,
    ) {
      const value = originalGetItem.call(this, key)
      if (key === legacyKey && startedImports.length === 0) {
        startedImports.push(importLegacyData(leaseA))
      }
      return value
    })

    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })
    getItemSpy.mockRestore()
    expect(startedImports).toHaveLength(1)
    expect(await startedImports[0]).toEqual({ status: 'imported', imported: 1 })
    expect(JSON.parse(readOwnedStorageForLease('pins', leaseA) ?? '{}')).toEqual({
      tags: ['single-claim'],
      domains: [],
    })

    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(await getLegacyImportPrompt(leaseB)).toBeNull()
    expect(await importLegacyData(leaseB)).toEqual({ status: 'already-resolved', imported: 0 })
    expect(readOwnedStorageForLease('pins', leaseB)).toBeNull()
  })

  it('opens a delta-only pending batch when an old Reader writes after an earlier import', async () => {
    const leaseA = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    localStorage.setItem(
      'webtag:annotations:v1',
      '{"link":[{"id":"claimed-v1","note":"claimed by A"}]}',
    )
    localStorage.setItem(
      'webtag:annotations:v2',
      '{"link":{"contentRevision":3,"items":[{"id":"claimed-v2","note":"claimed by A"}]}}',
    )
    localStorage.setItem(
      'webtag:pins:v1',
      '{"tags":["claimed-by-A"],"domains":["claimed.example"]}',
    )
    await quarantineLegacyData()
    expect(await importLegacyData(leaseA)).toEqual({ status: 'imported', imported: 3 })
    expect(JSON.parse(readOwnedStorage('annotationsV1') ?? '{}')).toEqual({
      link: [{ id: 'claimed-v1', note: 'claimed by A' }],
    })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      link: {
        contentRevision: 3,
        items: [{ id: 'claimed-v2', note: 'claimed by A' }],
      },
    })
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['claimed-by-A'],
      domains: ['claimed.example'],
    })

    localStorage.setItem(
      'webtag:annotations:v1',
      '{"link":[{"id":"claimed-v1","note":"stale global copy"},{"id":"late-v1"}]}',
    )
    localStorage.setItem(
      'webtag:annotations:v2',
      '{"link":{"contentRevision":3,"items":[{"id":"claimed-v2","note":"stale global copy"},{"id":"late-v2"}]}}',
    )
    localStorage.setItem(
      'webtag:pins:v1',
      '{"tags":["claimed-by-A","written-after-first-import"],"domains":["claimed.example","late.example"]}',
    )
    expect(await quarantineLegacyData()).toEqual({ quarantined: 3, discarded: 0 })

    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(await getLegacyImportPrompt(leaseB)).toEqual({
      assetCount: 3,
      sources: ['annotationsV1', 'annotationsV2', 'pins'],
    })
    expect(await importLegacyData(leaseB)).toEqual({ status: 'imported', imported: 3 })
    expect(JSON.parse(readOwnedStorage('annotationsV1') ?? '{}')).toEqual({
      link: [{ id: 'late-v1' }],
    })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      link: { contentRevision: 3, items: [{ id: 'late-v2' }] },
    })
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['written-after-first-import'],
      domains: ['late.example'],
    })

    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect(JSON.parse(readOwnedStorage('annotationsV1') ?? '{}')).toEqual({
      link: [{ id: 'claimed-v1', note: 'claimed by A' }],
    })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      link: {
        contentRevision: 3,
        items: [{ id: 'claimed-v2', note: 'claimed by A' }],
      },
    })
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['claimed-by-A'],
      domains: ['claimed.example'],
    })
    expect(localStorage.getItem('webtag:pins:v1')).toBeNull()
  })

  it('preserves an old-Reader write that replaces a global while quarantine is saving', async () => {
    const legacyKey = 'webtag:pins:v1'
    const capturedValue = '{"tags":["captured"],"domains":["captured.example"]}'
    const replacementValue = '{"tags":["replacement"],"domains":["replacement.example"]}'
    localStorage.setItem(legacyKey, capturedValue)

    const originalGetItem = Storage.prototype.getItem
    let pinReads = 0
    const getItemSpy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(function (
      this: Storage,
      key,
    ) {
      const value = originalGetItem.call(this, key)
      if (key === legacyKey && ++pinReads === 2) {
        Storage.prototype.setItem.call(this, legacyKey, replacementValue)
        return replacementValue
      }
      return value
    })

    expect(await quarantineLegacyData()).toEqual({ quarantined: 0, discarded: 0 })
    expect(localStorage.getItem(legacyKey)).toBe(replacementValue)

    getItemSpy.mockRestore()
    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })
    expect(localStorage.getItem(legacyKey)).toBeNull()

    const lease = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect(await importLegacyData(lease)).toEqual({ status: 'imported', imported: 1 })
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['replacement', 'captured'],
      domains: ['replacement.example', 'captured.example'],
    })
  })

  it('keeps the later-captured annotation snapshot when an older transaction arrives last', async () => {
    let captureTime = 200
    vi.spyOn(Date, 'now').mockImplementation(() => captureTime)
    localStorage.setItem(
      'webtag:annotations:v2',
      '{"link":{"contentRevision":5,"items":[{"id":"newer-revision"}]}}',
    )
    await quarantineLegacyData()

    captureTime = 100
    localStorage.setItem(
      'webtag:annotations:v2',
      '{"link":{"contentRevision":4,"items":[{"id":"delayed-older-revision"}]}}',
    )
    await quarantineLegacyData()

    const lease = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect(await importLegacyData(lease)).toEqual({ status: 'imported', imported: 1 })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      link: { contentRevision: 5, items: [{ id: 'newer-revision' }] },
    })
  })

  it('compares high-resolution capture times from pages with different time origins', async () => {
    vi.spyOn(Date, 'now').mockReturnValue(100)
    let captureOrigin = 2_000
    let captureOffset = 100
    vi.stubGlobal('performance', {
      get timeOrigin() {
        return captureOrigin
      },
      now: () => captureOffset,
    })
    localStorage.setItem(
      'webtag:annotations:v2',
      '{"link":{"contentRevision":5,"items":[{"id":"newer-same-millisecond"}]}}',
    )
    await quarantineLegacyData()

    captureOrigin = 1_000
    captureOffset = 900
    localStorage.setItem(
      'webtag:annotations:v2',
      '{"link":{"contentRevision":4,"items":[{"id":"delayed-older-same-millisecond"}]}}',
    )
    await quarantineLegacyData()

    const lease = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect(await importLegacyData(lease)).toEqual({ status: 'imported', imported: 1 })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      link: { contentRevision: 5, items: [{ id: 'newer-same-millisecond' }] },
    })
  })

  it('merges repeated old-Reader writes into the current pending batch and reopens ignored prompts', async () => {
    const lease = readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    localStorage.setItem(
      'webtag:annotations:v1',
      '{"link":[{"id":"shared","note":"older"},{"id":"older-only","note":"keep"}]}',
    )
    localStorage.setItem(
      'webtag:annotations:v2',
      '{"same":{"contentRevision":3,"items":[{"id":"shared","note":"older"},{"id":"older-only"}]},"changed":{"contentRevision":4,"items":[{"id":"old-revision"}]}}',
    )
    localStorage.setItem(
      'webtag:pins:v1',
      '{"tags":["older","shared"],"domains":["older.example"]}',
    )
    await quarantineLegacyData()
    expect(await keepLegacyDataIsolated(lease)).toBe(true)
    expect(await getLegacyImportPrompt(lease)).toBeNull()

    localStorage.setItem(
      'webtag:annotations:v1',
      '{"link":[{"id":"shared","note":"newer"},{"id":"newer-only","note":"add"}]}',
    )
    localStorage.setItem(
      'webtag:annotations:v2',
      '{"same":{"contentRevision":3,"items":[{"id":"shared","note":"newer"},{"id":"newer-only"}]},"changed":{"contentRevision":5,"items":[{"id":"new-revision"}]}}',
    )
    localStorage.setItem(
      'webtag:pins:v1',
      '{"tags":["shared","newer"],"domains":["newer.example"]}',
    )
    expect(await quarantineLegacyData()).toEqual({ quarantined: 3, discarded: 0 })
    expect(await getLegacyImportPrompt(lease)).toEqual({
      assetCount: 3,
      sources: ['annotationsV1', 'annotationsV2', 'pins'],
    })
    expect(await importLegacyData(lease)).toEqual({ status: 'imported', imported: 3 })

    expect(JSON.parse(readOwnedStorage('annotationsV1') ?? '{}')).toEqual({
      link: [
        { id: 'shared', note: 'newer' },
        { id: 'older-only', note: 'keep' },
        { id: 'newer-only', note: 'add' },
      ],
    })
    expect(JSON.parse(readOwnedStorage('annotationsV2') ?? '{}')).toEqual({
      same: {
        contentRevision: 3,
        items: [
          { id: 'shared', note: 'newer' },
          { id: 'older-only' },
          { id: 'newer-only' },
        ],
      },
      changed: { contentRevision: 5, items: [{ id: 'new-revision' }] },
    })
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['shared', 'newer', 'older'],
      domains: ['newer.example', 'older.example'],
    })
  })

  it('archives a resolved v1 batch during upgrade before accepting later globals', async () => {
    await seedResolvedVersionOneBatch()
    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })

    expect(await getLegacyImportPrompt(leaseB)).toBeNull()
    expect(await listLegacyUnattributed()).toEqual([
      expect.objectContaining({
        id: 'pins',
        value: '{"tags":["already-claimed-by-A"],"domains":[]}',
      }),
    ])

    localStorage.setItem(
      'webtag:pins:v1',
      '{"tags":["written-after-upgrade"],"domains":[]}',
    )
    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })
    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(await getLegacyImportPrompt(readerIdentity.activeLease!)).toEqual({
      assetCount: 1,
      sources: ['pins'],
    })
    expect(await importLegacyData(readerIdentity.activeLease!)).toEqual({
      status: 'imported',
      imported: 1,
    })
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['written-after-upgrade'],
      domains: [],
    })
    expect(await listLegacyUnattributed()).toHaveLength(2)
  })

  it('waits for the archive owner to build body-free dedup metadata before exposing a delta', async () => {
    await seedResolvedVersionOneBatch()
    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    const legacyKey = 'webtag:pins:v1'
    const cumulative = '{"tags":["already-claimed-by-A","written-after-upgrade"],"domains":[]}'
    localStorage.setItem(legacyKey, cumulative)

    expect(await quarantineLegacyData()).toEqual({ quarantined: 0, discarded: 0 })
    expect(localStorage.getItem(legacyKey)).toBe(cumulative)
    expect(await getLegacyImportPrompt(leaseB)).toBeNull()

    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })
    expect(localStorage.getItem(legacyKey)).toBeNull()

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(await getLegacyImportPrompt(readerIdentity.activeLease!)).toEqual({
      assetCount: 1,
      sources: ['pins'],
    })
    expect(await importLegacyData(readerIdentity.activeLease!)).toEqual({
      status: 'imported',
      imported: 1,
    })
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['written-after-upgrade'],
      domains: [],
    })
  })

  it('aggregates same-asset v2 archive fingerprints before exposing a delta', async () => {
    await seedResolvedVersionTwoArchives()
    const objectStoreGetAll = vi.spyOn(IDBObjectStore.prototype, 'getAll')
    const indexGetAll = vi.spyOn(IDBIndex.prototype, 'getAll')
    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })

    expect(await getLegacyImportPrompt(leaseB)).toBeNull()
    const legacyKey = 'webtag:pins:v1'
    const cumulative = '{"tags":["already-claimed-by-A-1","already-claimed-by-A-2","written-after-upgrade"],"domains":[]}'
    localStorage.setItem(legacyKey, cumulative)
    expect(await quarantineLegacyData()).toEqual({ quarantined: 0, discarded: 0 })
    expect(localStorage.getItem(legacyKey)).toBe(cumulative)
    expect(await getLegacyImportPrompt(leaseB)).toBeNull()
    expect(
      (objectStoreGetAll.mock.contexts as IDBObjectStore[]).filter(
        (store) => store.name === 'legacy-import-archive',
      ),
    ).toHaveLength(0)
    expect(indexGetAll.mock.calls.map(([query]) => query)).toEqual([
      'physical-B',
      'physical-B',
      'physical-B',
    ])

    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect(await quarantineLegacyData()).toEqual({ quarantined: 1, discarded: 0 })
    expect(localStorage.getItem(legacyKey)).toBeNull()
    const archive = await inspectArchive('physical-A')
    expect(archive).toEqual({
      version: USER_DATA_DATABASE_VERSION,
      indexNames: ['by-fingerprint-version', 'by-imported-namespace'],
      records: [
        expect.objectContaining({
          id: 'pins',
          value: '{"tags":["already-claimed-by-A-1"],"domains":[]}',
          importedIntoNamespace: 'physical-A',
          fingerprintVersion: 1,
          fingerprints: expect.any(Array),
        }),
        expect.objectContaining({
          id: 'pins',
          value: '{"tags":["already-claimed-by-A-2"],"domains":[]}',
          importedIntoNamespace: 'physical-A',
          fingerprintVersion: 1,
          fingerprints: expect.any(Array),
        }),
      ],
    })

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(await getLegacyImportPrompt(readerIdentity.activeLease!)).toEqual({
      assetCount: 1,
      sources: ['pins'],
    })
    expect(await importLegacyData(readerIdentity.activeLease!)).toEqual({
      status: 'imported',
      imported: 1,
    })
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['written-after-upgrade'],
      domains: [],
    })
    expect(
      (objectStoreGetAll.mock.contexts as IDBObjectStore[]).filter(
        (store) => store.name === 'legacy-import-archive',
      ),
    ).toHaveLength(0)
    expect(indexGetAll.mock.calls.map(([query]) => query)).toEqual([
      'physical-B',
      'physical-B',
      'physical-B',
      'physical-A',
      'physical-A',
      'physical-B',
      'physical-B',
      'physical-B',
    ])
  })
})
