export type IDBOperationResult<T> =
  | { readonly ok: true; readonly value: T }
  | { readonly ok: false }

export interface IDBTransactionScope<T> {
  readonly transaction: IDBTransaction
  readonly setResult: (value: T) => void
  readonly abort: () => void
  readonly isCurrent: () => boolean
  readonly request: <TRequest>(
    request: IDBRequest<TRequest>,
    onSuccess: (result: TRequest) => void,
  ) => void
}

export interface RunIDBTransactionOptions {
  readonly signal?: AbortSignal
  readonly isCurrent?: () => boolean
  readonly requireResult?: boolean
}

export function abortIDBTransaction(transaction: IDBTransaction | null | undefined): void {
  try {
    transaction?.abort()
  } catch {
    // The transaction may already have committed or aborted.
  }
}

export function attachIDBAbortSignal(
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

export function handleIDBRequest<T>(
  transaction: IDBTransaction,
  request: IDBRequest<T>,
  onSuccess: (result: T) => void,
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

export function runIDBTransaction<T>(
  database: IDBDatabase | null,
  storeNames: string | readonly string[],
  mode: IDBTransactionMode,
  execute: (scope: IDBTransactionScope<T>) => void,
  options: RunIDBTransactionOptions = {},
): Promise<IDBOperationResult<T>> {
  if (!database || options.isCurrent?.() === false) return Promise.resolve({ ok: false })

  return new Promise((resolve) => {
    let settled = false
    let value: T | undefined
    let hasValue = false
    let transaction: IDBTransaction | null = null
    let detachAbort: () => void = () => undefined

    const finish = (result: IDBOperationResult<T>) => {
      if (settled) return
      settled = true
      detachAbort()
      resolve(result)
    }
    const isCurrent = () => options.isCurrent?.() !== false
    const abort = () => abortIDBTransaction(transaction)

    try {
      transaction = database.transaction(
        Array.isArray(storeNames) ? [...storeNames] : storeNames,
        mode,
      )
      detachAbort = attachIDBAbortSignal(transaction, options.signal)
      if (options.signal?.aborted || !isCurrent()) {
        abort()
        finish({ ok: false })
        return
      }

      execute({
        transaction,
        setResult: (next) => {
          value = next
          hasValue = true
        },
        abort,
        isCurrent,
        request: (request, onSuccess) => handleIDBRequest(transaction!, request, onSuccess),
      })

      transaction.oncomplete = () => {
        if (!isCurrent() || (options.requireResult !== false && !hasValue)) {
          finish({ ok: false })
          return
        }
        finish({ ok: true, value: value as T })
      }
      transaction.onerror = () => finish({ ok: false })
      transaction.onabort = () => finish({ ok: false })
    } catch {
      abort()
      finish({ ok: false })
    }
  })
}
