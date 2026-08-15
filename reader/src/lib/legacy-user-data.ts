import {
  listLegacyStorageEntries,
  readOwnedStorageForLease,
  removeLegacyStorageEntries,
  writeOwnedStorageForLease,
  type OwnedStorageID,
} from './storage-ownership'
import { readerIdentity, type IdentityLease, type IdentityOperationContext } from './identity'
import {
  DECISION_STORE,
  LEGACY_ARCHIVE_FINGERPRINT_INDEX,
  LEGACY_ARCHIVE_NAMESPACE_INDEX,
  LEGACY_ARCHIVE_STORE,
  LEGACY_DEDUP_STORE,
  LEGACY_STORE,
  attachLeaseAbort,
  openUserDataDatabase as userDataDatabase,
} from './user-data/idb'

export { resetUserDataDatabaseHandle } from './user-data/idb'

const LEGACY_USER_ASSETS = ['annotationsV1', 'annotationsV2', 'pins'] as const satisfies readonly OwnedStorageID[]
const DISCARDABLE_LEGACY_STATE = [
  'revisionFloor',
  'feedSelection',
  'collapsedFeedFolders',
  'sidebarFold',
] as const satisfies readonly OwnedStorageID[]

export type LegacyUserAssetID = (typeof LEGACY_USER_ASSETS)[number]

export interface LegacyUnattributedRecord {
  readonly id: LegacyUserAssetID
  readonly legacyKey: string
  readonly value: string
  readonly quarantinedAt: number
  readonly quarantineOrder?: number
  /** Body-free fingerprints used to subtract snapshots already claimed by any identity. */
  readonly fingerprints?: readonly string[]
}

export interface LegacyQuarantineResult {
  readonly quarantined: number
  readonly discarded: number
}

export interface LegacyImportPrompt {
  readonly assetCount: number
  readonly sources: LegacyUserAssetID[]
}

export type LegacyImportResult =
  | { readonly status: 'imported'; readonly imported: number }
  | { readonly status: 'already-resolved' | 'no-data' | 'stale' | 'unavailable'; readonly imported: 0 }

interface MigrationDecision {
  readonly id: string
  readonly kind: 'ignored' | 'imported'
  readonly namespace: string
  readonly decidedAt: number
}

interface LegacyArchiveRecord extends LegacyUnattributedRecord {
  readonly archiveID?: number
  readonly importedIntoNamespace: string
  readonly importedAt: number
  readonly deliveryPending?: boolean
  readonly fingerprintVersion?: 1
}

interface LegacyDedupRecord {
  readonly id: LegacyUserAssetID
  readonly fingerprints: readonly string[]
}

interface PreparedLegacyRecord {
  readonly record: LegacyUnattributedRecord
  subtractSeen(seen: ReadonlySet<string>): LegacyUnattributedRecord | null
}

const RESOLUTION_ID = 'resolution'

function ignoredDecisionID(namespace: string): string {
  return `ignored:${namespace}`
}

function nextQuarantineOrder(): number {
  if (typeof performance !== 'undefined' && Number.isFinite(performance.timeOrigin)) {
    const offset = performance.now()
    if (Number.isFinite(offset)) return performance.timeOrigin + offset
  }
  return Date.now()
}

async function archiveFingerprintCoverage(database: IDBDatabase): Promise<boolean | null> {
  return new Promise((resolve) => {
    let settled = false
    const finish = (covered: boolean | null) => {
      if (settled) return
      settled = true
      resolve(covered)
    }
    try {
      const transaction = database.transaction(LEGACY_ARCHIVE_STORE, 'readonly')
      const archive = transaction.objectStore(LEGACY_ARCHIVE_STORE)
      const total = archive.count()
      const covered = archive.index(LEGACY_ARCHIVE_FINGERPRINT_INDEX).count(1)
      transaction.oncomplete = () => finish(total.result === covered.result)
      transaction.onerror = () => finish(null)
      transaction.onabort = () => finish(null)
    } catch {
      finish(null)
    }
  })
}

async function migrateOwnedArchiveFingerprints(database: IDBDatabase): Promise<boolean> {
  const coverage = await archiveFingerprintCoverage(database)
  if (coverage !== false) return coverage === true

  const lease = readerIdentity.activeLease
  if (!lease) return false
  const operation = lease.capture('legacy archive fingerprint migration')
  const records = await readArchiveRecordsForNamespace(
    database,
    operation.physicalNamespace,
  )
  if (!records || !lease.isCurrent(operation)) return false
  const missing = records.filter(
    (record) => record.fingerprintVersion !== 1 || !record.fingerprints,
  )
  if (missing.length === 0) return false

  const prepared = await Promise.all(missing.map(async (record) => ({
    archiveID: record.archiveID,
    prepared: (await prepareLegacyRecord(record)).record,
  }))).catch(() => null)
  if (!prepared || !lease.isCurrent(operation)) return false
  const fingerprintsByAsset = new Map<LegacyUserAssetID, Set<string>>()
  for (const candidate of prepared) {
    const fingerprints = fingerprintsByAsset.get(candidate.prepared.id) ?? new Set<string>()
    for (const fingerprint of candidate.prepared.fingerprints ?? []) {
      fingerprints.add(fingerprint)
    }
    fingerprintsByAsset.set(candidate.prepared.id, fingerprints)
  }

  const migrated = await new Promise<boolean>((resolve) => {
    let settled = false
    const finish = (value: boolean) => {
      if (settled) return
      settled = true
      detachAbort()
      resolve(value && lease.isCurrent(operation))
    }
    let detachAbort: () => void = () => undefined
    try {
      const transaction = database.transaction(
        [LEGACY_ARCHIVE_STORE, LEGACY_DEDUP_STORE],
        'readwrite',
      )
      detachAbort = attachLeaseAbort(transaction, operation)
      const archive = transaction.objectStore(LEGACY_ARCHIVE_STORE)
      const dedup = transaction.objectStore(LEGACY_DEDUP_STORE)
      for (const candidate of prepared) {
        if (typeof candidate.archiveID !== 'number') {
          transaction.abort()
          break
        }
        const request = archive.get(candidate.archiveID)
        request.onsuccess = () => {
          const current = request.result as LegacyArchiveRecord | undefined
          if (
            !current ||
            current.importedIntoNamespace !== operation.physicalNamespace ||
            !lease.isCurrent(operation)
          ) {
            transaction.abort()
            return
          }
          const updated = {
            ...current,
            fingerprints: candidate.prepared.fingerprints,
            fingerprintVersion: 1,
          } satisfies LegacyArchiveRecord
          archive.put(updated)
        }
        request.onerror = () => transaction.abort()
      }
      for (const [id, fingerprints] of fingerprintsByAsset) {
        addFingerprints(dedup, id, [...fingerprints], transaction)
      }
      transaction.oncomplete = () => finish(true)
      transaction.onerror = () => finish(false)
      transaction.onabort = () => finish(false)
    } catch {
      finish(false)
    }
  })
  if (!migrated) return false
  return (await archiveFingerprintCoverage(database)) === true
}

async function preserveLegacyRecords(records: readonly PreparedLegacyRecord[]): Promise<boolean> {
  if (records.length === 0) return true
  const database = await userDataDatabase()
  if (!database) return false
  if (!(await migrateOwnedArchiveFingerprints(database))) return false

  return new Promise((resolve) => {
    let settled = false
    const finish = (saved: boolean) => {
      if (settled) return
      settled = true
      resolve(saved)
    }
    try {
      const transaction = database.transaction(
        [LEGACY_STORE, LEGACY_DEDUP_STORE, DECISION_STORE],
        'readwrite',
      )
      const pending = transaction.objectStore(LEGACY_STORE)
      const pendingRequest = pending.getAll()
      const dedupRequest = transaction.objectStore(LEGACY_DEDUP_STORE).getAll()
      let ready = 0
      const preserveWhenReady = () => {
        ready += 1
        if (ready !== 2) return
        const existing = new Map(
          (pendingRequest.result as LegacyUnattributedRecord[]).map((record) => [record.id, record]),
        )
        const seenByAsset = new Map(
          (dedupRequest.result as LegacyDedupRecord[]).map((record) => [
            record.id,
            new Set(record.fingerprints),
          ]),
        )
        let changed = false
        for (const captured of records) {
          const record = captured.subtractSeen(
            seenByAsset.get(captured.record.id) ?? new Set<string>(),
          )
          if (!record) continue
          const previous = existing.get(record.id)
          if (legacyRecordAlreadyRepresented(record, previous ? [previous] : [])) {
            continue
          }
          const merged = previous ? mergeLegacyRecords(previous, record) : record
          pending.put(merged)
          existing.set(record.id, merged)
          changed = true
        }
        if (changed) transaction.objectStore(DECISION_STORE).clear()
      }
      pendingRequest.onsuccess = preserveWhenReady
      dedupRequest.onsuccess = preserveWhenReady
      pendingRequest.onerror = () => transaction.abort()
      dedupRequest.onerror = () => transaction.abort()
      transaction.oncomplete = () => finish(true)
      transaction.onerror = () => finish(false)
      transaction.onabort = () => finish(false)
    } catch {
      finish(false)
    }
  })
}

export async function quarantineLegacyData(): Promise<LegacyQuarantineResult> {
  const quarantinedAt = Date.now()
  const quarantineOrder = nextQuarantineOrder()
  const entries = LEGACY_USER_ASSETS.flatMap((id) =>
    listLegacyStorageEntries(id).map((entry) => ({ id, entry })),
  )
  const records = await Promise.all(entries.map(({ id, entry }) =>
    prepareLegacyRecord({
      id,
      legacyKey: entry.key,
      value: entry.value,
      quarantinedAt,
      quarantineOrder,
    }),
  )).catch(() => null)

  let quarantined = 0
  if (records && await preserveLegacyRecords(records)) {
    quarantined = removeLegacyStorageEntries(entries.map(({ entry }) => entry))
  }

  let discarded = 0
  for (const id of DISCARDABLE_LEGACY_STATE) {
    discarded += removeLegacyStorageEntries(listLegacyStorageEntries(id))
  }
  return { quarantined, discarded }
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function parseRecord(raw: string | null): Record<string, unknown> | null {
  if (!raw) return null
  try {
    const value: unknown = JSON.parse(raw)
    return isPlainRecord(value) ? value : null
  } catch {
    return null
  }
}

function itemIdentity(item: unknown, index: number): string {
  if (isPlainRecord(item) && typeof item.id === 'string') return `id:${item.id}`
  try {
    return `value:${JSON.stringify(item)}`
  } catch {
    return `index:${index}`
  }
}

function mergeItems(legacy: unknown[], current: unknown[]): unknown[] {
  const merged = new Map<string, unknown>()
  for (const [index, item] of [...legacy, ...current].entries()) {
    merged.set(itemIdentity(item, index), item)
  }
  return [...merged.values()]
}

function mergeAnnotationV2Items(
  legacy: unknown[],
  current: unknown[],
  legacyEnvelope: Record<string, unknown>,
  currentEnvelope: Record<string, unknown>,
  quarantined: boolean,
): unknown[] {
  const merged = new Map<string, unknown>()
  legacy.forEach((item, index) => {
    merged.set(
      annotationV2ItemIdentity(item, index, legacyEnvelope, quarantined),
      item,
    )
  })
  current.forEach((item, index) => {
    merged.set(
      annotationV2ItemIdentity(item, index, currentEnvelope, quarantined),
      item,
    )
  })
  return [...merged.values()]
}

function mergeAnnotationV1(currentRaw: string | null, legacyRaw: string): string {
  const current = parseRecord(currentRaw)
  const legacy = parseRecord(legacyRaw)
  if (!current) return legacyRaw
  if (!legacy) return currentRaw ?? legacyRaw

  const merged: Record<string, unknown> = { ...legacy }
  for (const [linkID, value] of Object.entries(current)) {
    const legacyValue = merged[linkID]
    merged[linkID] = Array.isArray(legacyValue) && Array.isArray(value)
      ? mergeItems(legacyValue, value)
      : value
  }
  return JSON.stringify(merged)
}

function mergeAnnotationV2(currentRaw: string | null, legacyRaw: string): string {
  const current = parseRecord(currentRaw)
  const legacy = parseRecord(legacyRaw)
  if (!current) return legacyRaw
  if (!legacy) return currentRaw ?? legacyRaw

  const merged: Record<string, unknown> = { ...legacy }
  for (const [linkID, currentEnvelope] of Object.entries(current)) {
    const legacyEnvelope = merged[linkID]
    if (
      isPlainRecord(currentEnvelope) &&
      isPlainRecord(legacyEnvelope) &&
      Array.isArray(currentEnvelope.items) &&
      Array.isArray(legacyEnvelope.items)
    ) {
      const currentQuarantined = Array.isArray(currentEnvelope.quarantinedItems)
        ? currentEnvelope.quarantinedItems
        : []
      const legacyQuarantined = Array.isArray(legacyEnvelope.quarantinedItems)
        ? legacyEnvelope.quarantinedItems
        : []
      const sameEnvelopeSource = (
        currentEnvelope.contentRevision === legacyEnvelope.contentRevision &&
        currentEnvelope.summarySourceHash === legacyEnvelope.summarySourceHash
      )
      const legacyCurrent: unknown[] = []
      const legacyStale: unknown[] = []
      for (const item of legacyEnvelope.items) {
        const bound = bindAnnotationV2ItemSource(item, legacyEnvelope)
        const matchesCurrent = annotationV2ItemMatchesEnvelope(bound, currentEnvelope)
        if (matchesCurrent === true || (matchesCurrent === null && sameEnvelopeSource)) {
          legacyCurrent.push(bound)
        } else if (matchesCurrent === false) {
          legacyStale.push(bound)
        }
      }
      merged[linkID] = {
        ...currentEnvelope,
        items: mergeAnnotationV2Items(
          legacyCurrent,
          currentEnvelope.items,
          legacyEnvelope,
          currentEnvelope,
          false,
        ),
        ...(currentQuarantined.length > 0 ||
          legacyStale.length > 0 ||
          legacyQuarantined.length > 0
          ? {
              quarantinedItems: mergeAnnotationV2Items(
                [...legacyStale, ...legacyQuarantined],
                currentQuarantined,
                legacyEnvelope,
                currentEnvelope,
                true,
              ),
            }
          : {}),
      }
    } else {
      merged[linkID] = currentEnvelope
    }
  }
  return JSON.stringify(merged)
}

function stringList(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === 'string')
    : []
}

function mergePins(currentRaw: string | null, legacyRaw: string): string {
  const current = parseRecord(currentRaw)
  const legacy = parseRecord(legacyRaw)
  if (!current) return legacyRaw
  if (!legacy) return currentRaw ?? legacyRaw
  return JSON.stringify({
    tags: [...new Set([...stringList(current.tags), ...stringList(legacy.tags)])],
    domains: [...new Set([...stringList(current.domains), ...stringList(legacy.domains)])],
  })
}

type LegacyAssetMerge = (currentRaw: string | null, legacyRaw: string) => string

const legacyAssetMergers = {
  annotationsV1: mergeAnnotationV1,
  annotationsV2: mergeAnnotationV2,
  pins: mergePins,
} satisfies Record<LegacyUserAssetID, LegacyAssetMerge>

interface AnnotationItemCollection {
  readonly key: string
  readonly items: unknown[]
  itemIdentity(item: unknown, index: number): string
}

type AnnotationCollections = (value: unknown) => readonly AnnotationItemCollection[] | null
type AnnotationEnvelope = (
  value: unknown,
  collections: ReadonlyMap<string, unknown[]>,
) => unknown

async function fingerprint(token: string): Promise<string> {
  if (!globalThis.crypto?.subtle) throw new Error('Web Crypto is unavailable')
  const digest = await globalThis.crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(`webtag-legacy-dedup-v1\0${token}`),
  )
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, '0'))
    .join('')
}

async function prepareAnnotationRecord(
  record: LegacyUnattributedRecord,
  collectionsFromValue: AnnotationCollections,
  envelopeWithCollections: AnnotationEnvelope,
): Promise<PreparedLegacyRecord> {
  const parsed = parseRecord(record.value)
  const rawFingerprint = await fingerprint(`${record.id}\0raw\0${record.value}`)
  if (!parsed) {
    const captured = { ...record, fingerprints: [rawFingerprint] }
    return {
      record: captured,
      subtractSeen: (seen) => seen.has(rawFingerprint) ? null : captured,
    }
  }

  const fingerprintsByLink = new Map<string, Map<string, string[]>>()
  for (const [linkID, value] of Object.entries(parsed)) {
    const collections = collectionsFromValue(value)
    if (!collections) continue
    const fingerprintsByCollection = new Map<string, string[]>()
    for (const collection of collections) {
      fingerprintsByCollection.set(
        collection.key,
        await Promise.all(collection.items.map((item, index) =>
          fingerprint(
            `${record.id}\0item\0${linkID}\0${collection.itemIdentity(item, index)}`,
          ),
        )),
      )
    }
    fingerprintsByLink.set(linkID, fingerprintsByCollection)
  }
  const fingerprints = [
    rawFingerprint,
    ...[...fingerprintsByLink.values()].flatMap((collections) => [
      ...collections.values(),
    ]).flat(),
  ]
  const captured = { ...record, fingerprints }
  return {
    record: captured,
    subtractSeen: (seen) => {
      if (seen.size === 0) return captured
      if (seen.has(rawFingerprint)) return null
      const delta: Record<string, unknown> = {}
      for (const [linkID, value] of Object.entries(parsed)) {
        const collections = collectionsFromValue(value)
        if (!collections) {
          delta[linkID] = value
          continue
        }
        const remaining = new Map<string, unknown[]>()
        let remainingCount = 0
        for (const collection of collections) {
          const itemFingerprints = fingerprintsByLink.get(linkID)?.get(collection.key) ?? []
          const unarchived = collection.items.filter(
            (_item, index) => !seen.has(itemFingerprints[index]),
          )
          remaining.set(collection.key, unarchived)
          remainingCount += unarchived.length
        }
        if (remainingCount > 0) {
          delta[linkID] = envelopeWithCollections(value, remaining)
        }
      }
      return Object.keys(delta).length > 0
        ? { ...captured, value: JSON.stringify(delta) }
        : null
    },
  }
}

function annotationV1Collections(value: unknown): readonly AnnotationItemCollection[] | null {
  return Array.isArray(value)
    ? [{ key: 'items', items: value, itemIdentity }]
    : null
}

function isContentAnnotationBlock(blockKey: unknown): blockKey is string {
  return typeof blockKey === 'string' && (
    blockKey === 'content' || blockKey.startsWith('content-')
  )
}

function validSourceContentRevision(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
}

function bindAnnotationV2ItemSource(
  item: unknown,
  envelope: Record<string, unknown>,
): unknown {
  if (!isPlainRecord(item)) return item
  if (
    isContentAnnotationBlock(item.blockKey) &&
    !validSourceContentRevision(item.sourceContentRevision) &&
    validSourceContentRevision(envelope.contentRevision)
  ) {
    return { ...item, sourceContentRevision: envelope.contentRevision }
  }
  if (
    item.blockKey === 'summary' &&
    typeof item.sourceSummaryHash !== 'string' &&
    typeof envelope.summarySourceHash === 'string'
  ) {
    return { ...item, sourceSummaryHash: envelope.summarySourceHash }
  }
  return item
}

function annotationV2ItemMatchesEnvelope(
  item: unknown,
  envelope: Record<string, unknown>,
): boolean | null {
  if (!isPlainRecord(item)) return null
  if (isContentAnnotationBlock(item.blockKey)) {
    return (
      validSourceContentRevision(item.sourceContentRevision) &&
      validSourceContentRevision(envelope.contentRevision) &&
      item.sourceContentRevision === envelope.contentRevision
    )
  }
  if (item.blockKey === 'summary') {
    if (envelope.summarySourceHash === undefined) return true
    return (
      typeof envelope.summarySourceHash === 'string' &&
      item.sourceSummaryHash === envelope.summarySourceHash
    )
  }
  return typeof item.blockKey === 'string' ? true : null
}

function annotationV2ItemIdentity(
  item: unknown,
  index: number,
  envelope: Record<string, unknown>,
  quarantined: boolean,
): string {
  const base = itemIdentity(item, index)
  if (!isPlainRecord(item)) return base

  if (isContentAnnotationBlock(item.blockKey)) {
    const revision = validSourceContentRevision(item.sourceContentRevision)
      ? item.sourceContentRevision
      : !quarantined && validSourceContentRevision(envelope.contentRevision)
        ? envelope.contentRevision
        : 'unverified'
    return `${base}\0content\0${revision}`
  }
  if (item.blockKey === 'summary') {
    const sourceHash = typeof item.sourceSummaryHash === 'string'
      ? item.sourceSummaryHash
      : !quarantined && typeof envelope.summarySourceHash === 'string'
        ? envelope.summarySourceHash
        : 'unverified'
    return `${base}\0summary\0${sourceHash}`
  }
  if (typeof item.blockKey === 'string') return `${base}\0stable\0${item.blockKey}`
  if (validSourceContentRevision(item.sourceContentRevision)) {
    return `${base}\0content\0${item.sourceContentRevision}`
  }
  if (typeof item.sourceSummaryHash === 'string') {
    return `${base}\0summary\0${item.sourceSummaryHash}`
  }
  return base
}

function annotationV2Collections(value: unknown): readonly AnnotationItemCollection[] | null {
  if (!isPlainRecord(value) || !Array.isArray(value.items)) return null
  const collections: AnnotationItemCollection[] = [{
    key: 'items',
    items: value.items,
    itemIdentity: (item, index) => annotationV2ItemIdentity(item, index, value, false),
  }]
  if (Array.isArray(value.quarantinedItems)) {
    collections.push({
      key: 'quarantinedItems',
      items: value.quarantinedItems,
      itemIdentity: (item, index) => annotationV2ItemIdentity(item, index, value, true),
    })
  }
  return collections
}

function prepareAnnotationV1(record: LegacyUnattributedRecord): Promise<PreparedLegacyRecord> {
  return prepareAnnotationRecord(
    record,
    annotationV1Collections,
    (_value, collections) => collections.get('items') ?? [],
  )
}

function prepareAnnotationV2(record: LegacyUnattributedRecord): Promise<PreparedLegacyRecord> {
  return prepareAnnotationRecord(
    record,
    annotationV2Collections,
    (value, collections) => {
      const envelope = value as Record<string, unknown>
      return {
        ...envelope,
        items: collections.get('items') ?? [],
        ...(Array.isArray(envelope.quarantinedItems)
          ? { quarantinedItems: collections.get('quarantinedItems') ?? [] }
          : {}),
      }
    },
  )
}

async function preparePins(record: LegacyUnattributedRecord): Promise<PreparedLegacyRecord> {
  const parsed = parseRecord(record.value)
  const rawFingerprint = await fingerprint(`${record.id}\0raw\0${record.value}`)
  if (!parsed) {
    const captured = { ...record, fingerprints: [rawFingerprint] }
    return {
      record: captured,
      subtractSeen: (seen) => seen.has(rawFingerprint) ? null : captured,
    }
  }

  const tags = stringList(parsed.tags)
  const domains = stringList(parsed.domains)
  const tagFingerprints = await Promise.all(
    tags.map((tag) => fingerprint(`${record.id}\0tag\0${tag}`)),
  )
  const domainFingerprints = await Promise.all(
    domains.map((domain) => fingerprint(`${record.id}\0domain\0${domain}`)),
  )
  const captured = {
    ...record,
    fingerprints: [rawFingerprint, ...tagFingerprints, ...domainFingerprints],
  }
  return {
    record: captured,
    subtractSeen: (seen) => {
      if (seen.size === 0) return captured
      if (seen.has(rawFingerprint)) return null
      const remainingTags = tags.filter((_tag, index) => !seen.has(tagFingerprints[index]))
      const remainingDomains = domains.filter(
        (_domain, index) => !seen.has(domainFingerprints[index]),
      )
      return remainingTags.length > 0 || remainingDomains.length > 0
        ? { ...captured, value: JSON.stringify({ tags: remainingTags, domains: remainingDomains }) }
        : null
    },
  }
}

type LegacyRecordPreparer = (
  record: LegacyUnattributedRecord,
) => Promise<PreparedLegacyRecord>

const legacyRecordPreparers = {
  annotationsV1: prepareAnnotationV1,
  annotationsV2: prepareAnnotationV2,
  pins: preparePins,
} satisfies Record<LegacyUserAssetID, LegacyRecordPreparer>

function prepareLegacyRecord(record: LegacyUnattributedRecord): Promise<PreparedLegacyRecord> {
  return legacyRecordPreparers[record.id](record)
}

function jsonValuesEqual(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) return true
  if (Array.isArray(left) || Array.isArray(right)) {
    return (
      Array.isArray(left) &&
      Array.isArray(right) &&
      left.length === right.length &&
      left.every((value, index) => jsonValuesEqual(value, right[index]))
    )
  }
  if (!isPlainRecord(left) || !isPlainRecord(right)) return false
  const leftKeys = Object.keys(left).sort()
  const rightKeys = Object.keys(right).sort()
  return (
    leftKeys.length === rightKeys.length &&
    leftKeys.every(
      (key, index) => key === rightKeys[index] && jsonValuesEqual(left[key], right[key]),
    )
  )
}

function storageValuesEqual(left: string, right: string): boolean {
  if (left === right) return true
  try {
    return jsonValuesEqual(JSON.parse(left), JSON.parse(right))
  } catch {
    return false
  }
}

function legacyRecordAlreadyRepresented(
  incoming: LegacyUnattributedRecord,
  preserved: readonly LegacyUnattributedRecord[],
): boolean {
  return preserved.some((record) => {
    if (record.id !== incoming.id || record.legacyKey !== incoming.legacyKey) return false
    return storageValuesEqual(mergeLegacyRecords(record, incoming).value, record.value)
  })
}

function mergeLegacyRecords(
  left: LegacyUnattributedRecord,
  right: LegacyUnattributedRecord,
): LegacyUnattributedRecord {
  const leftIsNewer = left.quarantinedAt !== right.quarantinedAt
    ? left.quarantinedAt > right.quarantinedAt
    : (left.quarantineOrder ?? 0) > (right.quarantineOrder ?? 0)
  const [current, legacy] = leftIsNewer
    ? [left, right]
    : [right, left]
  const value = legacyAssetMergers[current.id](current.value, legacy.value)
  const fingerprints = [...new Set([
    ...(left.fingerprints ?? []),
    ...(right.fingerprints ?? []),
  ])]
  return fingerprints.length > 0
    ? { ...current, value, fingerprints }
    : { ...current, value }
}

function mergeLegacyAsset(
  record: LegacyUnattributedRecord,
  lease: IdentityLease,
): string {
  const current = readOwnedStorageForLease(record.id, lease)
  return legacyAssetMergers[record.id](current, record.value)
}

function pendingDeliveriesForNamespace(
  records: readonly LegacyArchiveRecord[],
  namespace: string,
): LegacyArchiveRecord[] {
  return records.filter(
    (record) => record.deliveryPending === true && record.importedIntoNamespace === namespace,
  )
}

function archiveRecordsForNamespace(
  archive: IDBObjectStore,
  namespace: string,
): IDBRequest<LegacyArchiveRecord[]> {
  return archive.index(LEGACY_ARCHIVE_NAMESPACE_INDEX).getAll(namespace) as IDBRequest<
    LegacyArchiveRecord[]
  >
}

function addFingerprints(
  dedup: IDBObjectStore,
  id: LegacyUserAssetID,
  incoming: readonly string[],
  transaction: IDBTransaction,
): void {
  if (incoming.length === 0) return
  const request = dedup.get(id)
  request.onsuccess = () => {
    const existing = request.result as LegacyDedupRecord | undefined
    dedup.put({
      id,
      fingerprints: [...new Set([...(existing?.fingerprints ?? []), ...incoming])],
    } satisfies LegacyDedupRecord)
  }
  request.onerror = () => transaction.abort()
}

function mergeDeliveryAssets(
  records: readonly LegacyUnattributedRecord[],
): LegacyUnattributedRecord[] {
  const merged = new Map<LegacyUserAssetID, LegacyUnattributedRecord>()
  for (const record of records) {
    const previous = merged.get(record.id)
    merged.set(record.id, previous ? mergeLegacyRecords(previous, record) : record)
  }
  return [...merged.values()]
}

function importPromptFor(records: readonly LegacyUnattributedRecord[]): LegacyImportPrompt {
  const assets = mergeDeliveryAssets(records)
  return {
    assetCount: assets.length,
    sources: assets.map((record) => record.id),
  }
}

async function readPendingLegacyDeliveries(
  database: IDBDatabase,
  namespace: string,
): Promise<LegacyArchiveRecord[] | null> {
  const records = await readArchiveRecordsForNamespace(database, namespace)
  return records ? pendingDeliveriesForNamespace(records, namespace) : null
}

async function readArchiveRecordsForNamespace(
  database: IDBDatabase,
  namespace: string,
): Promise<LegacyArchiveRecord[] | null> {
  return new Promise((resolve) => {
    let settled = false
    const finish = (records: LegacyArchiveRecord[] | null) => {
      if (settled) return
      settled = true
      resolve(records)
    }
    try {
      const transaction = database.transaction(LEGACY_ARCHIVE_STORE, 'readonly')
      const request = archiveRecordsForNamespace(
        transaction.objectStore(LEGACY_ARCHIVE_STORE),
        namespace,
      )
      transaction.oncomplete = () => finish(request.result)
      request.onerror = () => finish(null)
      transaction.onerror = () => finish(null)
      transaction.onabort = () => finish(null)
    } catch {
      finish(null)
    }
  })
}

async function markLegacyDeliveriesComplete(
  database: IDBDatabase,
  records: readonly LegacyArchiveRecord[],
  lease: IdentityLease,
  operation: IdentityOperationContext,
): Promise<boolean> {
  const archiveIDs = new Set(
    records.flatMap((record) => typeof record.archiveID === 'number' ? [record.archiveID] : []),
  )
  if (archiveIDs.size === 0) return false

  return new Promise((resolve) => {
    let settled = false
    const finish = (completed: boolean) => {
      if (settled) return
      settled = true
      detachAbort()
      resolve(completed && lease.isCurrent(operation))
    }
    let detachAbort: () => void = () => undefined
    try {
      const transaction = database.transaction(LEGACY_ARCHIVE_STORE, 'readwrite')
      detachAbort = attachLeaseAbort(transaction, operation)
      const archive = transaction.objectStore(LEGACY_ARCHIVE_STORE)
      for (const archiveID of archiveIDs) {
        const request = archive.get(archiveID)
        request.onsuccess = () => {
          const record = request.result as LegacyArchiveRecord | undefined
          if (!record) return
          if (!lease.isCurrent(operation)) {
            transaction.abort()
            return
          }
          if (
            record.deliveryPending === true &&
            record.importedIntoNamespace === operation.physicalNamespace
          ) {
            archive.put({ ...record, deliveryPending: false } satisfies LegacyArchiveRecord)
          }
        }
        request.onerror = () => transaction.abort()
      }
      if (!lease.isCurrent(operation)) {
        transaction.abort()
      }
      transaction.oncomplete = () => finish(true)
      transaction.onerror = () => finish(false)
      transaction.onabort = () => finish(false)
    } catch {
      finish(false)
    }
  })
}

async function deliverPendingLegacyImports(
  database: IDBDatabase,
  lease: IdentityLease,
  operation: IdentityOperationContext,
  claimedAssetCount: number,
): Promise<LegacyImportResult> {
  if (!lease.isCurrent(operation)) return { status: 'stale', imported: 0 }
  const records = await readPendingLegacyDeliveries(database, operation.physicalNamespace)
  if (!records) return { status: 'unavailable', imported: 0 }
  if (!lease.isCurrent(operation)) return { status: 'stale', imported: 0 }
  if (records.length === 0) {
    return claimedAssetCount > 0
      ? { status: 'imported', imported: claimedAssetCount }
      : { status: 'already-resolved', imported: 0 }
  }

  const assets = mergeDeliveryAssets(records)
  for (const record of assets) {
    if (!lease.isCurrent(operation)) return { status: 'stale', imported: 0 }
    const merged = mergeLegacyAsset(record, lease)
    if (!writeOwnedStorageForLease(record.id, merged, lease)) {
      return { status: 'unavailable', imported: 0 }
    }
  }
  if (!(await markLegacyDeliveriesComplete(database, records, lease, operation))) {
    return lease.isCurrent(operation)
      ? { status: 'unavailable', imported: 0 }
      : { status: 'stale', imported: 0 }
  }
  return { status: 'imported', imported: assets.length }
}

export async function getLegacyImportPrompt(
  lease: IdentityLease,
): Promise<LegacyImportPrompt | null> {
  const operation = lease.capture('legacy import prompt')
  const database = await userDataDatabase()
  if (!database || !lease.isCurrent(operation)) return null

  const snapshot = await new Promise<{
    records: LegacyUnattributedRecord[]
    deliveries: LegacyArchiveRecord[]
    resolved: boolean
    ignored: boolean
  } | null>((resolve) => {
    let settled = false
    const finish = (value: {
      records: LegacyUnattributedRecord[]
      deliveries: LegacyArchiveRecord[]
      resolved: boolean
      ignored: boolean
    } | null) => {
      if (settled) return
      settled = true
      resolve(value)
    }
    try {
      const transaction = database.transaction(
        [LEGACY_STORE, LEGACY_ARCHIVE_STORE, DECISION_STORE],
        'readonly',
      )
      const recordsRequest = transaction.objectStore(LEGACY_STORE).getAll()
      const archiveRequest = archiveRecordsForNamespace(
        transaction.objectStore(LEGACY_ARCHIVE_STORE),
        operation.physicalNamespace,
      )
      const decisions = transaction.objectStore(DECISION_STORE)
      const resolutionRequest = decisions.get(RESOLUTION_ID)
      const ignoredRequest = decisions.get(ignoredDecisionID(operation.physicalNamespace))
      transaction.oncomplete = () => finish({
        records: recordsRequest.result as LegacyUnattributedRecord[],
        deliveries: pendingDeliveriesForNamespace(
          archiveRequest.result,
          operation.physicalNamespace,
        ),
        resolved: resolutionRequest.result !== undefined,
        ignored: ignoredRequest.result !== undefined,
      })
      transaction.onerror = () => finish(null)
      transaction.onabort = () => finish(null)
    } catch {
      finish(null)
    }
  })
  if (!snapshot || !lease.isCurrent(operation)) return null
  if (snapshot.deliveries.length > 0) return importPromptFor(snapshot.deliveries)
  if (snapshot.resolved || snapshot.ignored) return null
  if (snapshot.records.length === 0) return null
  return importPromptFor(snapshot.records)
}

export async function keepLegacyDataIsolated(lease: IdentityLease): Promise<boolean> {
  const operation = lease.capture('keep legacy data isolated')
  const database = await userDataDatabase()
  if (!database || !lease.isCurrent(operation)) return false

  return new Promise((resolve) => {
    let settled = false
    const finish = (kept: boolean) => {
      if (settled) return
      settled = true
      detachAbort()
      resolve(kept && lease.isCurrent(operation))
    }
    let detachAbort: () => void = () => undefined
    try {
      const transaction = database.transaction(DECISION_STORE, 'readwrite')
      detachAbort = attachLeaseAbort(transaction, operation)
      transaction.objectStore(DECISION_STORE).put({
        id: ignoredDecisionID(operation.physicalNamespace),
        kind: 'ignored',
        namespace: operation.physicalNamespace,
        decidedAt: Date.now(),
      } satisfies MigrationDecision)
      transaction.oncomplete = () => finish(true)
      transaction.onerror = () => finish(false)
      transaction.onabort = () => finish(false)
    } catch {
      finish(false)
    }
  })
}

export async function importLegacyData(lease: IdentityLease): Promise<LegacyImportResult> {
  const operation = lease.capture('import legacy data')
  const database = await userDataDatabase()
  if (!database) return { status: 'unavailable', imported: 0 }
  if (!lease.isCurrent(operation)) return { status: 'stale', imported: 0 }

  return new Promise((resolve) => {
    let settled = false
    let result: LegacyImportResult = { status: 'unavailable', imported: 0 }
    let shouldDeliver = false
    const finish = (next: LegacyImportResult) => {
      if (settled) return
      settled = true
      detachAbort()
      resolve(next)
    }
    let detachAbort: () => void = () => undefined
    try {
      const transaction = database.transaction(
        [LEGACY_STORE, LEGACY_ARCHIVE_STORE, LEGACY_DEDUP_STORE, DECISION_STORE],
        'readwrite',
      )
      detachAbort = attachLeaseAbort(transaction, operation)
      const recordsRequest = transaction.objectStore(LEGACY_STORE).getAll()
      const decisions = transaction.objectStore(DECISION_STORE)
      const archive = transaction.objectStore(LEGACY_ARCHIVE_STORE)
      const dedup = transaction.objectStore(LEGACY_DEDUP_STORE)
      const archiveRequest = archiveRecordsForNamespace(
        archive,
        operation.physicalNamespace,
      )
      const resolutionRequest = decisions.get(RESOLUTION_ID)
      let ready = 0
      const importWhenReady = () => {
        ready += 1
        if (ready !== 3) return
        if (!lease.isCurrent(operation)) {
          result = { status: 'stale', imported: 0 }
          transaction.abort()
          return
        }
        const pendingDeliveries = pendingDeliveriesForNamespace(
          archiveRequest.result,
          operation.physicalNamespace,
        )
        if (pendingDeliveries.length > 0) {
          shouldDeliver = true
          result = {
            status: 'imported',
            imported: mergeDeliveryAssets(pendingDeliveries).length,
          }
          return
        }
        if (resolutionRequest.result !== undefined) {
          result = { status: 'already-resolved', imported: 0 }
          return
        }
        const records = recordsRequest.result as LegacyUnattributedRecord[]
        if (records.length === 0) {
          result = { status: 'no-data', imported: 0 }
          return
        }
        const importedAt = Date.now()
        for (const record of records) {
          archive.add({
            ...record,
            importedIntoNamespace: operation.physicalNamespace,
            importedAt,
            deliveryPending: true,
            fingerprintVersion: 1,
          } satisfies LegacyArchiveRecord)
          addFingerprints(dedup, record.id, record.fingerprints ?? [], transaction)
        }
        transaction.objectStore(LEGACY_STORE).clear()
        decisions.clear()
        decisions.put({
          id: RESOLUTION_ID,
          kind: 'imported',
          namespace: operation.physicalNamespace,
          decidedAt: Date.now(),
        } satisfies MigrationDecision)
        shouldDeliver = true
        result = { status: 'imported', imported: records.length }
      }
      recordsRequest.onsuccess = importWhenReady
      archiveRequest.onsuccess = importWhenReady
      resolutionRequest.onsuccess = importWhenReady
      recordsRequest.onerror = () => transaction.abort()
      archiveRequest.onerror = () => transaction.abort()
      resolutionRequest.onerror = () => transaction.abort()
      transaction.oncomplete = () => {
        if (!shouldDeliver || result.status !== 'imported') {
          finish(result)
          return
        }
        void deliverPendingLegacyImports(database, lease, operation, result.imported).then(
          finish,
          () => finish({ status: 'unavailable', imported: 0 }),
        )
      }
      transaction.onerror = () => finish(
        operation.signal.aborted
          ? { status: 'stale', imported: 0 }
          : { status: 'unavailable', imported: 0 },
      )
      transaction.onabort = transaction.onerror
    } catch {
      finish({ status: 'unavailable', imported: 0 })
    }
  })
}
