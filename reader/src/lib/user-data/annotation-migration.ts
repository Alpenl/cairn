import type { Annotation } from '../annotations'
import { AnnotationDocumentChannel } from '../article/document-channel'
import { isValidLinkId, isValidSourceHash } from '../article/source-block'
import type { IdentityLease } from '../identity'
import {
  readOwnedStorageForLease,
  type OwnedStorageID,
} from '../storage-ownership'
import {
  annotationSignatureFields,
  bindAnnotationToTarget,
  decodeAnnotationWire,
  isSafeNonNegativeInteger,
  isSavedContentAnnotationBlockKey,
} from './annotation-codec'
import {
  compactAnnotationOperations,
  importAnnotationOperations,
  type AnnotationImportOperation,
} from './annotation-store'
import {
  annotationTargetKey,
  type AnnotationTarget,
  type LegacyStaleAnnotationAddDraft,
  type NoteAnnotationAddDraft,
  type SavedContentAnnotationAddDraft,
  type SummaryAnnotationAddDraft,
} from './annotation-types'

const MIGRATION_DOMAIN = 'webtag:legacy-annotation-localstorage:v1'

type LegacyAnnotationStorageID = Extract<OwnedStorageID, 'annotationsV1' | 'annotationsV2'>

export interface LegacyAnnotationMigrationResult {
  readonly status: 'migrated' | 'nothing-to-do' | 'unavailable'
  readonly examined: number
  readonly applied: number
  readonly removedBackups: number
}

interface PreparedAnnotation {
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly annotation: Annotation
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function isPositiveRevision(value: unknown): value is number {
  return isSafeNonNegativeInteger(value) && value > 0
}

function staleTarget(
  storageID: LegacyAnnotationStorageID,
  linkId: string,
  collection: string,
  annotation: Annotation,
): AnnotationTarget {
  return {
    kind: 'legacy-stale',
    sourceKey: JSON.stringify([
      MIGRATION_DOMAIN,
      storageID,
      linkId,
      collection,
      annotation.blockKey,
      annotation.id,
    ]),
  }
}

function annotationDraftBase(annotation: Annotation): SummaryAnnotationAddDraft {
  return {
    id: annotation.id,
    start: annotation.start,
    end: annotation.end,
    text: annotation.text,
    note: annotation.note,
    source: annotation.source,
    createdAt: annotation.createdAt,
    updatedAt: annotation.updatedAt,
    ...(annotation.quote === undefined ? {} : { quote: annotation.quote }),
  }
}

function importOperation(
  item: PreparedAnnotation,
  opId: string,
): AnnotationImportOperation {
  const common = { kind: 'add' as const, opId, linkId: item.linkId }
  switch (item.target.kind) {
    case 'saved-content': {
      if (!isSavedContentAnnotationBlockKey(item.annotation.blockKey)) {
        throw new Error('saved-content migration target requires a content block')
      }
      const draft: SavedContentAnnotationAddDraft = {
        ...annotationDraftBase(item.annotation),
        blockKey: item.annotation.blockKey,
      }
      return { ...common, target: item.target, draft }
    }
    case 'summary': {
      const draft = annotationDraftBase(item.annotation)
      return { ...common, target: item.target, draft }
    }
    case 'note': {
      const draft: NoteAnnotationAddDraft = {
        ...annotationDraftBase(item.annotation),
        blockKey: item.annotation.blockKey,
      }
      return { ...common, target: item.target, draft }
    }
    case 'legacy-stale': {
      const draft: LegacyStaleAnnotationAddDraft = {
        ...annotationDraftBase(item.annotation),
        blockKey: item.annotation.blockKey,
      }
      return { ...common, target: item.target, draft }
    }
  }
}

function v2Target(
  storageID: LegacyAnnotationStorageID,
  linkId: string,
  collection: 'items' | 'quarantinedItems',
  annotation: Annotation,
  envelopeRevision: number,
  envelopeSummaryHash: string | null,
): AnnotationTarget {
  const active = collection === 'items'
  if (isPositiveRevision(annotation.sourceNoteRevision)) {
    return { kind: 'note', noteRevision: annotation.sourceNoteRevision }
  }
  if (isSavedContentAnnotationBlockKey(annotation.blockKey)) {
    const revision = isPositiveRevision(annotation.sourceContentRevision)
      ? annotation.sourceContentRevision
      : active && isPositiveRevision(envelopeRevision)
        ? envelopeRevision
        : null
    if (revision !== null) return { kind: 'saved-content', contentRevision: revision }
  } else if (annotation.blockKey === 'summary') {
    const sourceHash = annotation.sourceSummaryHash === undefined
      ? active && isValidSourceHash(envelopeSummaryHash)
        ? envelopeSummaryHash
        : null
      : isValidSourceHash(annotation.sourceSummaryHash)
        ? annotation.sourceSummaryHash
        : null
    if (sourceHash !== null) return { kind: 'summary', sourceHash }
  }
  return staleTarget(storageID, linkId, collection, annotation)
}

function parseV1(value: unknown): PreparedAnnotation[] | null {
  if (!isRecord(value)) return null
  const prepared: PreparedAnnotation[] = []
  for (const [linkId, candidate] of Object.entries(value)) {
    if (!isValidLinkId(linkId) || !Array.isArray(candidate)) return null
    for (const rawAnnotation of candidate) {
      const annotation = decodeAnnotationWire(rawAnnotation)
      if (!annotation) return null
      const target = staleTarget('annotationsV1', linkId, 'items', annotation)
      const bound = bindAnnotationToTarget(annotation, target)
      if (!bound) return null
      prepared.push({ linkId, target, annotation: bound })
    }
  }
  return prepared
}

function parseV2(value: unknown): PreparedAnnotation[] | null {
  if (!isRecord(value)) return null
  const prepared: PreparedAnnotation[] = []
  for (const [linkId, candidate] of Object.entries(value)) {
    if (!isValidLinkId(linkId) || !isRecord(candidate)) return null
    if (!isSafeNonNegativeInteger(candidate.contentRevision) || !Array.isArray(candidate.items)) {
      return null
    }
    if (
      candidate.summarySourceHash !== undefined &&
      candidate.summarySourceHash !== null &&
      !isValidSourceHash(candidate.summarySourceHash)
    ) {
      return null
    }
    if (candidate.quarantinedItems !== undefined && !Array.isArray(candidate.quarantinedItems)) {
      return null
    }
    const summaryHash = isValidSourceHash(candidate.summarySourceHash)
      ? candidate.summarySourceHash
      : null
    const collections = [
      ['items', candidate.items],
      ['quarantinedItems', candidate.quarantinedItems ?? []],
    ] as const
    for (const [collection, items] of collections) {
      for (const rawAnnotation of items) {
        const annotation = decodeAnnotationWire(rawAnnotation)
        if (!annotation) return null
        const target = v2Target(
          'annotationsV2',
          linkId,
          collection,
          annotation,
          candidate.contentRevision,
          summaryHash,
        )
        const bound = bindAnnotationToTarget(annotation, target)
        if (!bound) return null
        prepared.push({ linkId, target, annotation: bound })
      }
    }
  }
  return prepared
}

function deduplicate(prepared: readonly PreparedAnnotation[]): PreparedAnnotation[] {
  const byIdentity = new Map<string, PreparedAnnotation>()
  for (const item of prepared) {
    const targetKey = annotationTargetKey(item.target)
    if (!targetKey) continue
    const key = JSON.stringify([item.linkId, targetKey, item.annotation.id])
    const current = byIdentity.get(key)
    if (
      !current ||
      item.annotation.updatedAt > current.annotation.updatedAt ||
      (
        item.annotation.updatedAt === current.annotation.updatedAt &&
        JSON.stringify(annotationSignatureFields(item.annotation)) >
          JSON.stringify(annotationSignatureFields(current.annotation))
      )
    ) {
      byIdentity.set(key, item)
    }
  }
  return [...byIdentity.entries()]
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([, item]) => item)
}

async function fingerprint(storageID: LegacyAnnotationStorageID, raw: string): Promise<string | null> {
  try {
    const bytes = new TextEncoder().encode(`${MIGRATION_DOMAIN}\0${storageID}\0${raw}`)
    const digest = await crypto.subtle.digest('SHA-256', bytes)
    return Array.from(new Uint8Array(digest), (byte) =>
      byte.toString(16).padStart(2, '0')).join('')
  } catch {
    return null
  }
}

async function migrateStorageImpl(
  lease: IdentityLease,
  storageID: LegacyAnnotationStorageID,
): Promise<LegacyAnnotationMigrationResult> {
  const raw = readOwnedStorageForLease(storageID, lease)
  if (raw === null) {
    return { status: 'nothing-to-do', examined: 0, applied: 0, removedBackups: 0 }
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return { status: 'unavailable', examined: 0, applied: 0, removedBackups: 0 }
  }
  const candidates = storageID === 'annotationsV2' ? parseV2(parsed) : parseV1(parsed)
  if (!candidates) {
    return { status: 'unavailable', examined: 0, applied: 0, removedBackups: 0 }
  }
  const sourceFingerprint = await fingerprint(storageID, raw)
  if (!sourceFingerprint) {
    return { status: 'unavailable', examined: 0, applied: 0, removedBackups: 0 }
  }
  const prepared = deduplicate(candidates)
  const operations = prepared.map((item, index) => importOperation(
    item,
    `legacy:${storageID}:${sourceFingerprint}:${index}`,
  ))
  const imported = await importAnnotationOperations(lease, {
    importId: `${MIGRATION_DOMAIN}:${storageID}`,
    sourceFingerprint,
    operations,
  })
  if (!imported.ok || imported.value.examined !== operations.length) {
    return {
      status: 'unavailable',
      examined: operations.length,
      applied: 0,
      removedBackups: 0,
    }
  }
  const compactionTargets = new Map<string, {
    readonly linkId: string
    readonly target: AnnotationTarget
  }>()
  for (const operation of operations) {
    const targetKey = annotationTargetKey(operation.target)
    if (!targetKey) continue
    compactionTargets.set(
      JSON.stringify([operation.linkId, targetKey]),
      { linkId: operation.linkId, target: operation.target },
    )
  }
  // Run this for duplicate markers too. A process can stop after the import
  // transaction commits but before the first compaction attempt is scheduled.
  await Promise.allSettled([...compactionTargets.values()].map(({ linkId, target }) =>
    compactAnnotationOperations(lease, linkId, target)))
  if (imported.value.applied > 0) {
    const documentTargets = new Map<string, {
      readonly linkId: string
      readonly documentRevision: number
    }>()
    for (const operation of operations) {
      const revision = operation.target.kind === 'saved-content'
        ? operation.target.contentRevision
        : 0
      documentTargets.set(
        JSON.stringify([operation.linkId, revision]),
        { linkId: operation.linkId, documentRevision: revision },
      )
    }
    const channel = new AnnotationDocumentChannel(lease, () => undefined)
    for (const { linkId, documentRevision } of documentTargets.values()) {
      channel.publish({
        linkId,
        documentRevision,
        annotationStoreVersion: imported.value.lastSequence,
      })
    }
    channel.dispose()
    window.dispatchEvent(new Event('webtag:annotations-change'))
  }
  // Web Storage has no compare-and-delete primitive. Even a value check
  // immediately before removeItem can erase a newer snapshot written by an
  // old tab between those two calls. Keep the legacy source as a recovery
  // copy; deterministic import IDs make retries idempotent and still admit
  // later writes from an old client.
  return {
    status: 'migrated',
    examined: imported.value.examined,
    applied: imported.value.applied,
    removedBackups: 0,
  }
}

async function migrateStorage(
  lease: IdentityLease,
  storageID: LegacyAnnotationStorageID,
): Promise<LegacyAnnotationMigrationResult> {
  try {
    return await migrateStorageImpl(lease, storageID)
  } catch {
    // The source remains the recovery copy. A later bootstrap can retry the
    // deterministic import marker without duplicating committed operations.
    return { status: 'unavailable', examined: 0, applied: 0, removedBackups: 0 }
  }
}

export async function migrateLegacyAnnotations(
  lease: IdentityLease,
): Promise<LegacyAnnotationMigrationResult> {
  // V2 carries stronger source identity and claims matching records before V1.
  const v2 = await migrateStorage(lease, 'annotationsV2')
  const v1 = await migrateStorage(lease, 'annotationsV1')
  const status = v2.status === 'unavailable' || v1.status === 'unavailable'
    ? 'unavailable'
    : v2.status === 'migrated' || v1.status === 'migrated'
      ? 'migrated'
      : 'nothing-to-do'
  return {
    status,
    examined: v2.examined + v1.examined,
    applied: v2.applied + v1.applied,
    removedBackups: v2.removedBackups + v1.removedBackups,
  }
}
