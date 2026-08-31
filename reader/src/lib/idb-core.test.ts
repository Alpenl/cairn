import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it } from 'vitest'

import {
  collectIDBRequestList,
  collectIDBRequestResults,
  runIDBTransaction,
} from './idb-core'

const DATABASE_NAME = 'webtag-reader-idb-core-test'
const STORE_NAME = 'items'

async function openDatabase(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DATABASE_NAME, 1)
    request.onupgradeneeded = () => {
      request.result.createObjectStore(STORE_NAME)
    }
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
}

async function deleteDatabase(): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(DATABASE_NAME)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('database delete failed'))
    request.onblocked = () => reject(new Error('database delete was blocked'))
  })
}

async function seed(values: ReadonlyArray<readonly [string, string]>): Promise<void> {
  const database = await openDatabase()
  await new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(STORE_NAME, 'readwrite')
    const store = transaction.objectStore(STORE_NAME)
    for (const [key, value] of values) store.put(value, key)
    transaction.oncomplete = () => resolve()
    transaction.onerror = () => reject(transaction.error)
    transaction.onabort = () => reject(transaction.error)
  })
  database.close()
}

async function readValue(key: string): Promise<string | undefined> {
  const database = await openDatabase()
  return new Promise((resolve, reject) => {
    const transaction = database.transaction(STORE_NAME, 'readonly')
    const request = transaction.objectStore(STORE_NAME).get(key) as IDBRequest<string | undefined>
    request.onsuccess = () => {
      database.close()
      resolve(request.result)
    }
    request.onerror = () => {
      database.close()
      reject(request.error)
    }
  })
}

afterEach(async () => {
  await deleteDatabase()
})

describe('IDB request collection helpers', () => {
  it('collects keyed and ordered request results inside a transaction', async () => {
    await seed([
      ['alpha', 'A'],
      ['beta', 'B'],
    ])
    const database = await openDatabase()

    const result = await runIDBTransaction<{
      keyed: { readonly alpha: string | undefined; readonly beta: string | undefined }
      ordered: readonly (string | undefined)[]
    }>(
      database,
      [STORE_NAME],
      'readonly',
      ({ transaction, setResult }) => {
        const store = transaction.objectStore(STORE_NAME)
        collectIDBRequestResults<{
          alpha: string | undefined
          beta: string | undefined
        }>(
          transaction,
          {
            alpha: store.get('alpha') as IDBRequest<string | undefined>,
            beta: store.get('beta') as IDBRequest<string | undefined>,
          },
          (keyed) => {
            collectIDBRequestList<string | undefined>(
              transaction,
              [
                store.get('alpha') as IDBRequest<string | undefined>,
                store.get('beta') as IDBRequest<string | undefined>,
              ],
              (ordered) => setResult({ keyed, ordered }),
            )
          },
        )
      },
    )

    database.close()
    expect(result).toEqual({
      ok: true,
      value: {
        keyed: { alpha: 'A', beta: 'B' },
        ordered: ['A', 'B'],
      },
    })
  })

  it('aborts the transaction when a collected callback throws', async () => {
    await seed([['alpha', 'A']])
    const database = await openDatabase()

    const result = await runIDBTransaction<boolean>(
      database,
      [STORE_NAME],
      'readwrite',
      ({ transaction, setResult }) => {
        const store = transaction.objectStore(STORE_NAME)
        store.put('replacement', 'alpha')
        collectIDBRequestResults<{ alpha: string | undefined }>(
          transaction,
          { alpha: store.get('alpha') as IDBRequest<string | undefined> },
          () => {
            setResult(true)
            throw new Error('injected collection failure')
          },
        )
      },
    )

    database.close()
    expect(result).toEqual({ ok: false })
    await expect(readValue('alpha')).resolves.toBe('A')
  })
})
