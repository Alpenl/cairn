import 'fake-indexeddb/auto'

import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'

import { IdentityLease } from '../lib/identity'
import { readAnnotationSnapshot } from '../lib/user-data/annotation-store'
import { ownedDatabaseName } from '../lib/storage-ownership'
import { resetUserDataDatabaseHandle } from '../lib/user-data/idb'
import { useNoteAnnotations } from './useNoteAnnotations'

function lease(): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: 'server-note-test',
    physicalNamespace: 'physical-note-test',
    localEpoch: 1,
  })
}

async function clearDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('database delete failed'))
    request.onblocked = () => reject(new Error('database delete blocked'))
  })
}

afterEach(async () => {
  await clearDatabase()
})

describe('useNoteAnnotations', () => {
  it('binds annotations to note revision and commits through the durable store', async () => {
    const owner = lease()
    const result = renderHook(() => useNoteAnnotations(owner, 'N1', 4))
    await waitFor(() => expect(result.result.current.loading).toBe(false))

    await act(async () => {
      await expect(result.result.current.add({
        blockKey: 'note',
        start: 0,
        end: 4,
        text: 'body',
        quote: { exact: 'body', prefix: '', suffix: '' },
      })).resolves.toMatchObject({ status: 'committed' })
    })

    expect(result.result.current.anns).toHaveLength(1)
    expect(result.result.current.anns[0]).toMatchObject({
      blockKey: 'note',
      sourceNoteRevision: 4,
    })
    await expect(readAnnotationSnapshot(owner, 'N1', { kind: 'note', noteRevision: 4 })).resolves.toMatchObject({
      ok: true,
      value: { target: { kind: 'note', noteRevision: 4 }, annotations: [{ blockKey: 'note' }] },
    })
  })

  it('rejects edits from a different published note revision', async () => {
    const owner = lease()
    const result = renderHook(() => useNoteAnnotations(owner, 'N1', 4))
    await waitFor(() => expect(result.result.current.loading).toBe(false))
    const stale = {
      id: 'old', blockKey: 'note', start: 0, end: 2, text: 'ol', note: '', source: 'self' as const,
      createdAt: 1, updatedAt: 1, sourceNoteRevision: 3,
    }
    await expect(result.result.current.remove(stale)).resolves.toEqual({ status: 'stale' })
  })
})
