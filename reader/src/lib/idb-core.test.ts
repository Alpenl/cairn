import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it } from 'vitest'

import { runIDBTransaction } from './idb-core'

const DATABASE_NAME = 'webtag-idb-core-test'
const STORE_NAME = 'items'

function openDatabase(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DATABASE_NAME, 1)
    request.onupgradeneeded = () => {
      request.result.createObjectStore(STORE_NAME, { keyPath: 'id' })
    }
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
}

function deleteDatabase(): Promise<void> {
  return new Promise((resolve) => {
    const request = indexedDB.deleteDatabase(DATABASE_NAME)
    request.onsuccess = () => resolve()
    request.onerror = () => resolve()
    request.onblocked = () => resolve()
  })
}

async function readRow(id: string): Promise<unknown> {
  const database = await openDatabase()
  try {
    const result = await runIDBTransaction<unknown>(
      database,
      STORE_NAME,
      'readonly',
      ({ transaction, request, setResult }) => {
        request(transaction.objectStore(STORE_NAME).get(id), setResult)
      },
    )
    return result.ok ? result.value : undefined
  } finally {
    database.close()
  }
}

afterEach(async () => {
  await deleteDatabase()
})

describe('runIDBTransaction', () => {
  it('commits a successful request result', async () => {
    const database = await openDatabase()
    try {
      const result = await runIDBTransaction<IDBValidKey>(
        database,
        STORE_NAME,
        'readwrite',
        ({ transaction, request, setResult }) => {
          request(transaction.objectStore(STORE_NAME).put({ id: 'ok', value: 1 }), setResult)
        },
      )

      expect(result).toEqual({ ok: true, value: 'ok' })
      expect(await readRow('ok')).toEqual({ id: 'ok', value: 1 })
    } finally {
      database.close()
    }
  })

  it('fails closed when a request errors', async () => {
    const database = await openDatabase()
    try {
      const result = await runIDBTransaction<IDBValidKey>(
        database,
        STORE_NAME,
        'readwrite',
        ({ transaction, request, setResult }) => {
          const store = transaction.objectStore(STORE_NAME)
          request(store.add({ id: 'duplicate', value: 1 }), () => undefined)
          request(store.add({ id: 'duplicate', value: 2 }), setResult)
        },
      )

      expect(result).toEqual({ ok: false })
      expect(await readRow('duplicate')).toBeUndefined()
    } finally {
      database.close()
    }
  })

  it('fails closed when a transaction aborts', async () => {
    const database = await openDatabase()
    try {
      const result = await runIDBTransaction<string>(
        database,
        STORE_NAME,
        'readwrite',
        ({ abort, setResult }) => {
          setResult('uncommitted')
          abort()
        },
      )

      expect(result).toEqual({ ok: false })
    } finally {
      database.close()
    }
  })

  it('fails closed on quota exceptions without exposing the exception', async () => {
    const database = await openDatabase()
    try {
      const result = await runIDBTransaction<string>(
        database,
        STORE_NAME,
        'readwrite',
        () => {
          throw new DOMException('quota exceeded', 'QuotaExceededError')
        },
      )

      expect(result).toEqual({ ok: false })
    } finally {
      database.close()
    }
  })

  it('aborts when a request success callback throws', async () => {
    const database = await openDatabase()
    try {
      const result = await runIDBTransaction<IDBValidKey>(
        database,
        STORE_NAME,
        'readwrite',
        ({ transaction, request }) => {
          request(transaction.objectStore(STORE_NAME).put({ id: 'throw', value: 1 }), () => {
            throw new Error('callback failed')
          })
        },
      )

      expect(result).toEqual({ ok: false })
      expect(await readRow('throw')).toBeUndefined()
    } finally {
      database.close()
    }
  })
})
