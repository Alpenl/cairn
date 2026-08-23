import { isValidLinkId } from '../lib/article/source-block'
import type { IdentityLease } from '../lib/identity'
import {
  annotationTargetKey,
  canonicalAnnotationTarget,
  type AnnotationTarget,
} from '../lib/annotation-domain'
import type { AnnotationOperationRecord } from '../lib/user-data/annotation-types'
import {
  ANNOTATION_OPS_STORE,
  ANNOTATION_OPS_TARGET_INDEX,
  openUserDataDatabase,
  type UserDataTransactionResult,
} from '../lib/user-data/idb'

type StoredOperationRecord = Omit<AnnotationOperationRecord, 'opId'> & {
  readonly opId: string
  readonly logicalOpId: string
}

/** Test-only raw operation-log inspection. Production reads use snapshot + tail replay. */
export async function listAnnotationOperationsForTest(
  lease: IdentityLease,
  linkId: string,
  requestedTarget: AnnotationTarget,
  afterSequence = 0,
): Promise<UserDataTransactionResult<readonly AnnotationOperationRecord[]>> {
  const target = canonicalAnnotationTarget(requestedTarget)
  const targetKey = target ? annotationTargetKey(target) : null
  if (
    !isValidLinkId(linkId) ||
    !targetKey ||
    !Number.isSafeInteger(afterSequence) ||
    afterSequence < 0
  ) {
    return { ok: false }
  }

  const operation = lease.capture('inspect annotation operations in test')
  if (!lease.isCurrent(operation)) return { ok: false }
  const database = await openUserDataDatabase()
  if (!database || !lease.isCurrent(operation)) return { ok: false }

  return new Promise((resolve) => {
    let settled = false
    let value: readonly AnnotationOperationRecord[] | null = null
    const finish = (result: UserDataTransactionResult<readonly AnnotationOperationRecord[]>) => {
      if (settled) return
      settled = true
      resolve(result)
    }

    let transaction: IDBTransaction
    try {
      transaction = database.transaction(ANNOTATION_OPS_STORE, 'readonly')
      const request = transaction.objectStore(ANNOTATION_OPS_STORE)
        .index(ANNOTATION_OPS_TARGET_INDEX)
        .getAll(IDBKeyRange.bound(
          [lease.context.physicalNamespace, linkId, targetKey, afterSequence],
          [lease.context.physicalNamespace, linkId, targetKey, Number.MAX_SAFE_INTEGER],
          afterSequence > 0,
        )) as IDBRequest<StoredOperationRecord[]>
      request.onerror = () => {
        try {
          transaction.abort()
        } catch {
          finish({ ok: false })
        }
      }
      request.onsuccess = () => {
        if (
          !lease.isCurrent(operation) ||
          !request.result.every((item) =>
            typeof item.logicalOpId === 'string' &&
            item.logicalOpId.length > 0 &&
            Number.isSafeInteger(item.sequence) &&
            item.sequence > afterSequence)
        ) {
          try {
            transaction.abort()
          } catch {
            finish({ ok: false })
          }
          return
        }
        value = [...request.result]
          .sort((left, right) => left.sequence - right.sequence)
          .map((item) => {
            const { logicalOpId, ...stored } = item
            return { ...stored, opId: logicalOpId } as AnnotationOperationRecord
          })
      }
      transaction.oncomplete = () => {
        finish(value !== null && lease.isCurrent(operation)
          ? { ok: true, value }
          : { ok: false })
      }
      transaction.onerror = () => finish({ ok: false })
      transaction.onabort = () => finish({ ok: false })
    } catch {
      finish({ ok: false })
    }
  })
}
