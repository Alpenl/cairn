import type { IdentityLease, IdentityOperationContext, IdentityOwnership } from '../identity'

export interface NamespaceStorageOperation {
  readonly context: IdentityOperationContext
  readonly ownership: IdentityOwnership
  readonly signal: AbortSignal
  isCurrent(): boolean
  commit<T>(write: () => T): T | undefined
}

export class NamespaceStorageQueue {
  private readonly tails = new Map<string, Promise<void>>()

  enqueue<T>(
    lease: IdentityLease,
    logicalKey: string,
    task: (operation: NamespaceStorageOperation) => Promise<T> | T,
  ): Promise<T | undefined> {
    const ownership = lease.captureOwnership(logicalKey)
    const context = ownership.operation
    const namespace = context.physicalNamespace
    const previous = this.tails.get(namespace) ?? Promise.resolve()
    const run = previous.then(async () => {
      if (!lease.isOwnershipCurrent(ownership)) return undefined
      const operation: NamespaceStorageOperation = Object.freeze({
        context,
        ownership,
        signal: context.signal,
        isCurrent: () => lease.isOwnershipCurrent(ownership),
        commit: <Value>(write: () => Value): Value | undefined => {
          if (!lease.isOwnershipCurrent(ownership)) return undefined
          return write()
        },
      })
      return task(operation)
    })
    const tail = run.then(
      () => undefined,
      () => undefined,
    )
    this.tails.set(namespace, tail)
    void tail.then(() => {
      if (this.tails.get(namespace) === tail) this.tails.delete(namespace)
    })
    return run
  }
}

export const cacheStorageQueue = new NamespaceStorageQueue()
