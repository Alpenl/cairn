import type { IdentityBoundReaderClient } from '../src/lib/api/client'
import { IdentityAuthority, type IdentityLease } from '../src/lib/identity'
import {
  commitAnnotationOperation,
} from '../src/lib/user-data/annotation-store'
import {
  getThoughtSyncController,
  type ThoughtSyncController,
  type ThoughtSyncSnapshot,
} from '../src/lib/user-data/thought-sync'
import {
  THOUGHT_OUTBOX_NAMESPACE_INDEX,
  THOUGHT_OUTBOX_STORE,
  openUserDataDatabase,
  resetUserDataDatabaseHandle,
} from '../src/lib/user-data/idb'
import { ownedDatabaseName } from '../src/lib/storage-ownership'
import type { ThoughtOutboxRecord } from '../src/lib/user-data/thought-types'

type SyncMode = 'ack' | 'retryable' | 'blocked'

interface ThoughtSyncBrowserHarness {
  reset(): Promise<void>
  install(identity: 'A' | 'B'): void
  setOnline(online: boolean): void
  setMode(mode: SyncMode): void
  commit(opId: string): Promise<void>
  sync(): Promise<void>
  snapshot(): ThoughtSyncSnapshot
  outbox(): Promise<ReadonlyArray<{ readonly opId: string; readonly status: string | null }>>
  stats(): { readonly pushCalls: number; readonly pullCalls: number }
}

const authority = new IdentityAuthority()
let lease: IdentityLease | null = null
let controller: ThoughtSyncController | null = null
let stop: (() => void) | null = null
let mode: SyncMode = 'ack'
let pushCalls = 0
let pullCalls = 0

function client(): IdentityBoundReaderClient {
  return {
    pushThoughtOps: async (
      request: Parameters<IdentityBoundReaderClient['pushThoughtOps']>[0],
    ) => {
      pushCalls += 1
      if (mode === 'retryable') {
        return {
          ok: false,
          error: {
            kind: 'network-unreachable',
            message: 'retryable browser fixture failure',
            retryAfterSeconds: 1,
          },
        }
      }
      if (mode === 'blocked') {
        return {
          ok: false,
          error: {
            kind: 'other',
            status: 422,
            errorCode: 'invalid_thought_payload',
            message: 'permanent browser fixture failure',
          },
        }
      }
      return {
        ok: true,
        data: request.ops.map((operation, index) => ({
          contract_version: 1 as const,
          op_id: operation.op_id,
          sequence: pushCalls * 100 + index + 1,
          disposition: 'applied' as const,
          submitted_key: {
            logical_clock: operation.logical_clock,
            device_id: operation.device_id,
            op_id: operation.op_id,
          },
          current_winner_key: {
            logical_clock: operation.logical_clock,
            device_id: operation.device_id,
            op_id: operation.op_id,
          },
        })),
      }
    },
    syncThoughts: async () => {
      pullCalls += 1
      return { ok: true, data: { contract_version: 1 as const, items: [] } }
    },
  } as unknown as IdentityBoundReaderClient
}

function requireLease(): IdentityLease {
  if (!lease) throw new Error('thought sync browser harness is not installed')
  return lease
}

async function deleteDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('failed to delete browser harness database'))
    request.onblocked = () => reject(new Error('browser harness database deletion was blocked'))
  })
}

async function readOutbox(namespace: string): Promise<ReadonlyArray<ThoughtOutboxRecord>> {
  const database = await openUserDataDatabase()
  if (!database) return []
  return new Promise((resolve, reject) => {
    const transaction = database.transaction(THOUGHT_OUTBOX_STORE, 'readonly')
    const request = transaction.objectStore(THOUGHT_OUTBOX_STORE)
      .index(THOUGHT_OUTBOX_NAMESPACE_INDEX)
      .getAll(namespace) as IDBRequest<ThoughtOutboxRecord[]>
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
}

function install(identity: 'A' | 'B'): void {
  stop?.()
  authority.clear()
  lease = authority.install({
    serverClientDataNamespace: `thought-browser-${identity}`,
    physicalNamespace: `thought-browser-${identity}`,
  })
  controller = getThoughtSyncController(lease, client())
  stop = controller.start()
}

async function commit(opId: string): Promise<void> {
  const current = requireLease()
  const result = await commitAnnotationOperation(current, {
    kind: 'add',
    opId,
    linkId: 'browser-link',
    target: { kind: 'saved-content', contentRevision: 7 },
    draft: {
      id: `${opId}-annotation`,
      blockKey: 'content-document',
      start: 0,
      end: 4,
      text: 'test',
      note: 'browser thought payload',
      source: 'self',
      createdAt: 1,
      updatedAt: 1,
      quote: { exact: 'test', prefix: '', suffix: '' },
    },
  })
  if (!result.ok || result.value.status === 'op-id-conflict') {
    throw new Error('failed to commit browser Thought operation')
  }
  window.dispatchEvent(new Event('webtag:annotations-change'))
}

window.thoughtSyncHarness = {
  async reset() {
    stop?.()
    stop = null
    controller = null
    lease = null
    authority.clear()
    mode = 'ack'
    pushCalls = 0
    pullCalls = 0
    await deleteDatabase()
  },
  install,
  setOnline(online) {
    Object.defineProperty(window.navigator, 'onLine', { configurable: true, value: online })
    window.dispatchEvent(new Event(online ? 'online' : 'offline'))
  },
  setMode(nextMode) {
    mode = nextMode
  },
  async commit(opId) {
    await commit(opId)
  },
  async sync() {
    await controller?.sync()
  },
  snapshot() {
    if (!controller) throw new Error('thought sync browser harness is not installed')
    return controller.getSnapshot()
  },
  async outbox() {
    return (await readOutbox(requireLease().context.physicalNamespace)).map((record) => {
      if (typeof record.opId !== 'string') throw new Error('browser harness found an opaque op id')
      return { opId: record.opId, status: record.status ?? null }
    })
  },
  stats() {
    return { pushCalls, pullCalls }
  },
} satisfies ThoughtSyncBrowserHarness

declare global {
  interface Window {
    thoughtSyncHarness: ThoughtSyncBrowserHarness
  }
}
