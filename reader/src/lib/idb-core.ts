export type IDBExecutionResult<T> =
  | { readonly ok: true; readonly value: T }
  | { readonly ok: false }

export interface IDBTransactionOptions {
  readonly signal?: AbortSignal
  readonly isCurrent?: () => boolean
}

export interface IDBTransactionScope<T> {
  readonly transaction: IDBTransaction
  readonly setResult: (value: T) => void
  readonly abort: () => void
  readonly isCurrent: () => boolean
}

export function abortIDBTransaction(transaction: IDBTransaction | null | undefined): void {
  try {
    transaction?.abort()
  } catch {
    // A request failure, earlier abort, or committed transaction may have closed it.
  }
}

export function attachIDBTransactionAbortSignal(
  transaction: IDBTransaction,
  signal: AbortSignal | undefined,
): () => void {
  if (!signal) return () => undefined

  const abort = () => abortIDBTransaction(transaction)
  const detach = () => signal.removeEventListener('abort', abort)
  signal.addEventListener('abort', abort, { once: true })
  transaction.addEventListener('complete', detach, { once: true })
  transaction.addEventListener('abort', detach, { once: true })
  transaction.addEventListener('error', detach, { once: true })
  if (signal.aborted) abort()
  return detach
}

export function requestToResult<T>(
  transaction: IDBTransaction,
  request: IDBRequest<T>,
  onSuccess: (value: T) => void,
): void {
  request.onerror = () => abortIDBTransaction(transaction)
  request.onsuccess = () => {
    try {
      onSuccess(request.result)
    } catch {
      abortIDBTransaction(transaction)
    }
  }
}

type IDBRequestResults<T extends Record<string, unknown>> = {
  readonly [K in keyof T]: T[K]
}

export function collectIDBRequestResults<T extends Record<string, unknown>>(
  transaction: IDBTransaction,
  requests: { readonly [K in keyof T]: IDBRequest<T[K]> },
  onSuccess: (results: IDBRequestResults<T>) => void,
): void {
  const keys = Object.keys(requests) as Array<keyof T>
  if (keys.length === 0) {
    onSuccess({} as IDBRequestResults<T>)
    return
  }

  const results = {} as { [K in keyof T]: T[K] }
  let pending = keys.length
  const finish = () => {
    try {
      onSuccess(results)
    } catch {
      abortIDBTransaction(transaction)
    }
  }

  for (const key of keys) {
    const request = requests[key]
    request.onerror = () => abortIDBTransaction(transaction)
    request.onsuccess = () => {
      results[key] = request.result
      pending -= 1
      if (pending === 0) finish()
    }
  }
}

export function collectIDBRequestList<T>(
  transaction: IDBTransaction,
  requests: readonly IDBRequest<T>[],
  onSuccess: (results: readonly T[]) => void,
): void {
  if (requests.length === 0) {
    onSuccess([])
    return
  }

  const results = new Array<T>(requests.length)
  let pending = requests.length
  const finish = () => {
    try {
      onSuccess(results)
    } catch {
      abortIDBTransaction(transaction)
    }
  }

  requests.forEach((request, index) => {
    request.onerror = () => abortIDBTransaction(transaction)
    request.onsuccess = () => {
      results[index] = request.result
      pending -= 1
      if (pending === 0) finish()
    }
  })
}

export function transactionComplete(transaction: IDBTransaction): Promise<boolean> {
  return new Promise((resolve) => {
    let settled = false
    let hadError = false
    const finish = (completed: boolean) => {
      if (settled) return
      settled = true
      resolve(completed && !hadError)
    }

    transaction.oncomplete = () => finish(true)
    transaction.onerror = () => {
      hadError = true
    }
    transaction.onabort = () => finish(false)
  })
}

export function runIDBTransaction<T>(
  database: IDBDatabase | null,
  storeNames: readonly string[],
  mode: IDBTransactionMode,
  execute: (scope: IDBTransactionScope<T>) => void,
  options: IDBTransactionOptions = {},
): Promise<IDBExecutionResult<T>> {
  if (!database || options.signal?.aborted || options.isCurrent?.() === false) {
    return Promise.resolve({ ok: false })
  }

  return new Promise((resolve) => {
    let settled = false
    let transaction: IDBTransaction | null = null
    let detachAbort: () => void = () => undefined
    let hasValue = false
    let value: T | undefined

    const current = () => options.isCurrent?.() ?? true
    const finish = (result: IDBExecutionResult<T>) => {
      if (settled) return
      settled = true
      detachAbort()
      resolve(result)
    }

    try {
      transaction = database.transaction([...storeNames], mode)
      const completion = transactionComplete(transaction)
      detachAbort = attachIDBTransactionAbortSignal(transaction, options.signal)
      const scope: IDBTransactionScope<T> = {
        transaction,
        setResult: (next) => {
          value = next
          hasValue = true
        },
        abort: () => abortIDBTransaction(transaction),
        isCurrent: current,
      }

      if (options.signal?.aborted || !current()) {
        abortIDBTransaction(transaction)
      } else {
        execute(scope)
        if (!current()) abortIDBTransaction(transaction)
      }

      void completion.then((completed) => {
        if (!completed || !hasValue || !current()) {
          finish({ ok: false })
          return
        }
        finish({ ok: true, value: value as T })
      })
    } catch {
      abortIDBTransaction(transaction)
      finish({ ok: false })
    }
  })
}
