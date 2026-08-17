import type { Annotation } from '../annotations'
import { isRecord } from '../records'
import {
  annotationTargetKey,
  canonicalAnnotationTarget,
  type AnnotationTarget,
  type AnnotationUpdatePatch,
} from './annotation-types'
import { cloneTargetAnnotation, isSafeNonNegativeInteger } from './annotation-codec'
import { maximumThoughtClock, nextThoughtLogicalClock } from './thought-clock'
import {
  THOUGHT_CONTRACT_VERSION,
  type ThoughtOutboxRecord,
} from './thought-types'

/**
 * A v6 repair is a derived view over v4/v5 data. The source stores are never
 * modified, so this module deliberately uses only synchronous deterministic
 * operations: it is used inside an IndexedDB versionchange transaction.
 */
export const THOUGHT_REPAIR_SCHEMA_VERSION = 6 as const
export const UNATTRIBUTED_REPAIR_NAMESPACE = '__webtag-repair-unattributed__'
export const THOUGHT_REPAIR_SOURCE_SEAL_KEY = [
  '__webtag-repair-source-seal__',
  'v6',
] as const

export type ThoughtRepairReason =
  | 'missing_complete_base'
  | 'invalid_quote'
  | 'identity_mismatch'
  | 'conflicting_duplicate_op'
  | 'invalid_legacy_op'

export type ThoughtRepairSourceKind = 'v4-operation' | 'v5-outbox'

export interface ThoughtRepairSourceRecord {
  readonly key: readonly [namespace: string, sourceId: string]
  readonly namespace: string
  readonly sourceId: string
  /** Present only for a v5 queue row; used to suppress its immutable source copy. */
  readonly legacyOpId?: string
  readonly sourceKind: ThoughtRepairSourceKind
  readonly sourceChecksum: string
  /** Floor observed before v6 allocated any repair clocks. */
  readonly legacyClockFloor: number
}

export interface ThoughtRepairSourceSealRecord {
  readonly key: typeof THOUGHT_REPAIR_SOURCE_SEAL_KEY
  readonly recordKind: 'source-seal'
  readonly schemaVersion: typeof THOUGHT_REPAIR_SCHEMA_VERSION
  readonly sourceCount: number
  readonly sourceChecksum: string
}

export interface ThoughtRepairReadyRecord extends ThoughtOutboxRecord {
  readonly repair: true
}

export interface ThoughtRepairQuarantineRecord {
  readonly key: readonly [namespace: string, annotationId: string]
  readonly namespace: string
  readonly annotationId: string
  readonly reason: ThoughtRepairReason
  readonly source: readonly unknown[]
}

/**
 * `readyChecksum` describes the complete planned dispatch set, not only the
 * currently pending rows. A durable ack receipt may remove a ready row; the
 * union of ready rows and matching receipts must still equal this checksum.
 * This makes acknowledgement idempotent without ever rewriting a v4/v5 row.
 */
export interface ThoughtRepairManifest {
  readonly namespace: string
  readonly schemaVersion: typeof THOUGHT_REPAIR_SCHEMA_VERSION
  readonly sourceCount: number
  readonly readyCount: number
  readonly quarantineCount: number
  readonly sourceChecksum: string
  readonly readyChecksum: string
  readonly quarantineChecksum: string
  readonly legacyClockFloor: number
  readonly logicalClockFloor: number
  readonly complete: true
}

export interface ThoughtRepairAckRecord {
  readonly key: readonly [namespace: string, opId: string]
  readonly namespace: string
  readonly opId: string
  /** SHA-256 of the canonical v6 ready record acknowledged by the server. */
  readonly readyChecksum: string
}

export interface ThoughtRepairInputs {
  readonly v4Operations: readonly unknown[]
  readonly v5Outbox: readonly unknown[]
  readonly v4Materialized: readonly unknown[]
  readonly v5Materialized: readonly unknown[]
  readonly syncStates: readonly unknown[]
}

export interface ThoughtRepairPlan {
  readonly sources: readonly ThoughtRepairSourceRecord[]
  readonly ready: readonly ThoughtRepairReadyRecord[]
  readonly quarantine: readonly ThoughtRepairQuarantineRecord[]
  readonly manifests: readonly ThoughtRepairManifest[]
}

interface LegacyOperation {
  readonly raw: unknown
  readonly sourceKind: ThoughtRepairSourceKind
  readonly namespace: string
  readonly sourceOpId: string
  readonly opId: string | null
  readonly annotationId: string | null
  readonly sequence: number | null
  readonly operationKind: 'add' | 'update' | 'delete' | null
  readonly linkId: string | null
  readonly hostKind: string | null
  readonly hostId: string | null
  readonly target: AnnotationTarget | null
  readonly targetKey: string | null
  readonly annotation: Annotation | null
  readonly annotationInvalid: boolean
  readonly patch: AnnotationUpdatePatch | undefined
  readonly createdAt: number
  readonly attemptCount: number
  readonly contractVersion: 0 | 1 | null
  readonly deviceId: string | null
  readonly logicalClock: number | null
  readonly checksum: string | undefined
}

interface CandidateBase {
  readonly annotation: Annotation
  readonly target: AnnotationTarget
  readonly targetKey: string
  readonly linkId: string
  readonly hostKind: string
  readonly hostId: string
}

interface PlannedEntry {
  readonly namespace: string
  readonly legacySequence: number
  readonly annotationId: string
  readonly opId: string
  readonly source: LegacyOperation | null
  readonly operationKind: 'add' | 'update' | 'delete'
  readonly linkId: string
  readonly hostKind: string
  readonly hostId: string
  readonly target: AnnotationTarget
  readonly targetKey: string
  readonly annotation: Annotation | null
  readonly patch?: AnnotationUpdatePatch
  readonly createdAt: number
  readonly preserveVersionKey: boolean
  readonly synthetic: boolean
}

function nonEmpty(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0
}

function validChecksum(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{64}$/.test(value)
}

function validIdentifier(value: unknown): value is string {
  return nonEmpty(value) && value === value.trim() && !value.includes('\0') &&
    new TextEncoder().encode(value).length <= 128
}

/** Stable JSON with lexicographically sorted UTF-8-safe property names. */
export function canonicalRepairJSON(value: unknown): string {
  if (value === null) return 'null'
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
    return JSON.stringify(value)
  }
  if (Array.isArray(value)) return `[${value.map(canonicalRepairJSON).join(',')}]`
  if (isRecord(value)) {
    return `{${Object.keys(value).sort().map((key) =>
      `${JSON.stringify(key)}:${canonicalRepairJSON(value[key])}`).join(',')}}`
  }
  // IndexedDB structured-clone values are JSON-like. Giving unexpected values
  // an explicit representation keeps the failure deterministic and quarantined.
  return JSON.stringify(String(value))
}

// The synchronous SHA-256 below is intentional: WebCrypto is asynchronous and
// cannot keep a versionchange transaction alive. It implements FIPS 180-4 over
// the UTF-8 bytes of canonicalRepairJSON, not a non-cryptographic substitute.
const SHA256_INITIAL = [
  0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
  0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
]
const SHA256_K = [
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]

function rightRotate(value: number, amount: number): number {
  return (value >>> amount) | (value << (32 - amount))
}

export function sha256UTF8(value: string): string {
  const input = new TextEncoder().encode(value)
  const bitLength = input.length * 8
  const paddedLength = Math.ceil((input.length + 9) / 64) * 64
  const bytes = new Uint8Array(paddedLength)
  bytes.set(input)
  bytes[input.length] = 0x80
  const high = Math.floor(bitLength / 0x1_0000_0000)
  const low = bitLength >>> 0
  const view = new DataView(bytes.buffer)
  view.setUint32(bytes.length - 8, high, false)
  view.setUint32(bytes.length - 4, low, false)
  const hash = [...SHA256_INITIAL]
  const words = new Uint32Array(64)
  for (let offset = 0; offset < bytes.length; offset += 64) {
    for (let index = 0; index < 16; index += 1) words[index] = view.getUint32(offset + index * 4, false)
    for (let index = 16; index < 64; index += 1) {
      const a = words[index - 15]
      const b = words[index - 2]
      words[index] = (((rightRotate(a, 7) ^ rightRotate(a, 18) ^ (a >>> 3)) + words[index - 16] +
        (rightRotate(b, 17) ^ rightRotate(b, 19) ^ (b >>> 10)) + words[index - 7]) >>> 0)
    }
    let [a, b, c, d, e, f, g, h] = hash
    for (let index = 0; index < 64; index += 1) {
      const s1 = rightRotate(e, 6) ^ rightRotate(e, 11) ^ rightRotate(e, 25)
      const choice = (e & f) ^ (~e & g)
      const first = (h + s1 + choice + SHA256_K[index] + words[index]) >>> 0
      const s0 = rightRotate(a, 2) ^ rightRotate(a, 13) ^ rightRotate(a, 22)
      const majority = (a & b) ^ (a & c) ^ (b & c)
      const second = (s0 + majority) >>> 0
      h = g; g = f; f = e; e = (d + first) >>> 0
      d = c; c = b; b = a; a = (first + second) >>> 0
    }
    hash[0] = (hash[0] + a) >>> 0; hash[1] = (hash[1] + b) >>> 0
    hash[2] = (hash[2] + c) >>> 0; hash[3] = (hash[3] + d) >>> 0
    hash[4] = (hash[4] + e) >>> 0; hash[5] = (hash[5] + f) >>> 0
    hash[6] = (hash[6] + g) >>> 0; hash[7] = (hash[7] + h) >>> 0
  }
  return hash.map((word) => word.toString(16).padStart(8, '0')).join('')
}

export function repairChecksum(value: unknown): string {
  return sha256UTF8(canonicalRepairJSON(value))
}

function canonicalSourceRows(sources: readonly ThoughtRepairSourceRecord[]): string[] {
  return sources.map(canonicalRepairJSON).sort()
}

export function thoughtRepairSourceSeal(
  sources: readonly ThoughtRepairSourceRecord[],
): ThoughtRepairSourceSealRecord {
  return {
    key: THOUGHT_REPAIR_SOURCE_SEAL_KEY,
    recordKind: 'source-seal',
    schemaVersion: THOUGHT_REPAIR_SCHEMA_VERSION,
    sourceCount: sources.length,
    sourceChecksum: repairChecksum(canonicalSourceRows(sources)),
  }
}

export function isThoughtRepairSourceSeal(
  value: unknown,
  sources: readonly ThoughtRepairSourceRecord[],
): value is ThoughtRepairSourceSealRecord {
  if (!isRecord(value) || value.recordKind !== 'source-seal' ||
    value.schemaVersion !== THOUGHT_REPAIR_SCHEMA_VERSION ||
    !Array.isArray(value.key) || value.key.length !== 2 ||
    value.key[0] !== THOUGHT_REPAIR_SOURCE_SEAL_KEY[0] ||
    value.key[1] !== THOUGHT_REPAIR_SOURCE_SEAL_KEY[1]) return false
  const expected = thoughtRepairSourceSeal(sources)
  return value.sourceCount === expected.sourceCount &&
    value.sourceChecksum === expected.sourceChecksum
}

function repairDeviceID(namespace: string): string {
  return `v6-repair-${sha256UTF8(namespace).slice(0, 32)}`
}

export function stableRepairID(namespace: string, annotationID: string): string {
  return `v6-repair:${namespace.length}:${namespace}:${annotationID.length}:${annotationID}`
}

function sourceNamespace(raw: unknown): string {
  return isRecord(raw) && validIdentifier(raw.namespace)
    ? raw.namespace
    : UNATTRIBUTED_REPAIR_NAMESPACE
}

function targetFrom(raw: Record<string, unknown>): AnnotationTarget | null {
  return isRecord(raw.target)
    ? canonicalAnnotationTarget(raw.target as unknown as AnnotationTarget)
    : null
}

function patchFrom(raw: Record<string, unknown>): AnnotationUpdatePatch | undefined {
  if (!isRecord(raw.patch) || !isSafeNonNegativeInteger(raw.patch.updatedAt) ||
    (raw.patch.note !== undefined && typeof raw.patch.note !== 'string') ||
    (raw.patch.source !== undefined && raw.patch.source !== 'self' && raw.patch.source !== 'ai')) return undefined
  return raw.patch as unknown as AnnotationUpdatePatch
}

function legacyOperation(
  raw: unknown,
  sourceKind: ThoughtRepairSourceKind,
  ordinal: number,
): LegacyOperation {
  const record = isRecord(raw) ? raw : {}
  const namespace = sourceNamespace(raw)
  const isV5 = sourceKind === 'v5-outbox'
  const target = targetFrom(record)
  const targetKey = target ? annotationTargetKey(target) : null
  const operationKind = (isV5 ? record.operationKind : record.kind)
  const annotationCandidate = record.annotation
  const annotation = target ? cloneTargetAnnotation(annotationCandidate, target) : null
  const invalidAnnotation = annotationCandidate !== null && annotationCandidate !== undefined && annotation === null
  const rawOpID = isV5 ? record.opId : (nonEmpty(record.logicalOpId) ? record.logicalOpId : record.opId)
  const version = record.contractVersion === THOUGHT_CONTRACT_VERSION ? THOUGHT_CONTRACT_VERSION :
    record.contractVersion === 0 || record.contractVersion === undefined ? 0 : null
  const normalizedKind: 'add' | 'update' | 'delete' | null =
    operationKind === 'add' || operationKind === 'update' || operationKind === 'delete'
      ? operationKind : null
  const common = {
    opId: validIdentifier(rawOpID) ? rawOpID : null,
    annotationId: validIdentifier(record.annotationId) ? record.annotationId : null,
    sequence: isSafeNonNegativeInteger(record.sequence) && record.sequence > 0 ? record.sequence : null,
    operationKind: normalizedKind,
    linkId: validIdentifier(record.linkId) ? record.linkId : null,
    hostKind: validIdentifier(record.hostKind) ? record.hostKind :
      target ? (target.kind === 'note' ? 'note' : 'link') : null,
    hostId: validIdentifier(record.hostId) ? record.hostId :
      (validIdentifier(record.linkId) ? record.linkId : null),
    target,
    targetKey,
    annotation,
    patch: patchFrom(record),
    // v4 had no outbox timestamp and the early v5 bridge used 0. Prefer the
    // durable add payload timestamp in both forms so direct v4 and opened-v5
    // fixtures derive byte-identical v6 records.
    createdAt: isSafeNonNegativeInteger(record.createdAt) && record.createdAt > 0
      ? record.createdAt
      : annotation?.createdAt ?? 0,
  }
  const normalizedIdentity = common.opId === null
    ? `invalid:${ordinal}:${repairChecksum(raw)}`
    : `legacy:${common.opId}:${repairChecksum(common)}`
  return {
    raw,
    sourceKind,
    namespace,
    sourceOpId: normalizedIdentity,
    ...common,
    annotationInvalid: invalidAnnotation,
    attemptCount: isSafeNonNegativeInteger(record.attemptCount) ? record.attemptCount : 0,
    contractVersion: version,
    deviceId: validIdentifier(record.deviceId) ? record.deviceId : null,
    logicalClock: isSafeNonNegativeInteger(record.logicalClock) ? record.logicalClock : null,
    checksum: validChecksum(record.checksum) ? record.checksum : undefined,
  }
}

function sourceRecord(operation: LegacyOperation): ThoughtRepairSourceRecord {
  // Source op ids are independent from legacy op IDs, so malformed/colliding
  // rows remain addressable without making an outbox unique index authoritative.
  return {
    key: [operation.namespace, operation.sourceOpId],
    namespace: operation.namespace,
    sourceId: operation.sourceOpId,
    ...(operation.sourceKind === 'v5-outbox' && operation.opId !== null
      ? { legacyOpId: operation.opId }
      : {}),
    sourceKind: operation.sourceKind,
    sourceChecksum: repairChecksum(canonicalLegacySource(operation)),
    legacyClockFloor: 0,
  }
}

function sourceReceiptFingerprint(source: ThoughtRepairSourceRecord): string {
  return canonicalRepairJSON({
    key: source.key,
    namespace: source.namespace,
    sourceId: source.sourceId,
    legacyOpId: source.legacyOpId ?? null,
    sourceKind: source.sourceKind,
    sourceChecksum: source.sourceChecksum,
  })
}

function canonicalLegacySource(operation: LegacyOperation): Record<string, unknown> {
  return {
    opId: operation.opId,
    annotationId: operation.annotationId,
    sequence: operation.sequence,
    operationKind: operation.operationKind,
    linkId: operation.linkId,
    hostKind: operation.hostKind,
    hostId: operation.hostId,
    target: operation.target,
    targetKey: operation.targetKey,
    annotation: operation.annotation,
    patch: operation.patch ?? null,
    createdAt: operation.createdAt,
    versionKey: operation.contractVersion === THOUGHT_CONTRACT_VERSION
      ? {
          contractVersion: operation.contractVersion,
          deviceId: operation.deviceId,
          logicalClock: operation.logicalClock,
        }
      : null,
  }
}

function annotationFromThoughtMaterialized(raw: Record<string, unknown>, target: AnnotationTarget): Annotation | null {
  const direct = cloneTargetAnnotation(raw.annotation ?? raw.fallbackAnnotation, target)
  if (direct) return direct
  const quote = isRecord(raw.quote) ? raw.quote : null
  const timestamp = (value: unknown): number | null => {
    if (isSafeNonNegativeInteger(value)) return value
    if (typeof value !== 'string') return null
    const parsed = Date.parse(value)
    return Number.isFinite(parsed) && parsed >= 0 ? parsed : null
  }
  if (!quote || !validIdentifier(raw.annotationId) || typeof quote.exact !== 'string' ||
    !isSafeNonNegativeInteger(quote.start) || !isSafeNonNegativeInteger(quote.end) || quote.end < quote.start ||
    typeof quote.block_key !== 'string' || typeof raw.body !== 'string' ||
    (raw.source !== 'self' && raw.source !== 'ai' && raw.source !== 'user')) return null
  const createdAt = timestamp(raw.createdAt)
  const updatedAt = timestamp(raw.updatedAt)
  if (createdAt === null || updatedAt === null) return null
  const candidate: Record<string, unknown> = {
    id: raw.annotationId,
    blockKey: quote.block_key,
    start: quote.start,
    end: quote.end,
    text: quote.exact,
    note: raw.body,
    source: raw.source === 'user' ? 'self' : raw.source,
    createdAt,
    updatedAt,
  }
  if (target.kind === 'saved-content') candidate.sourceContentRevision = target.contentRevision
  if (target.kind === 'summary') candidate.sourceSummaryHash = target.sourceHash
  if (target.kind === 'note') candidate.sourceNoteRevision = target.noteRevision
  return cloneTargetAnnotation(candidate, target)
}

function baseFromMaterialized(
  rows: readonly unknown[],
  namespace: string,
  annotationId: string,
): { readonly base: CandidateBase | null; readonly malformed: boolean } {
  for (const raw of rows) {
    if (!isRecord(raw) || raw.namespace !== namespace || raw.annotationId !== annotationId ||
      !validIdentifier(raw.linkId)) continue
    const target = targetFrom(raw)
    if (!target) continue
    const targetKey = annotationTargetKey(target)
    if (!targetKey) return { base: null, malformed: true }
    const annotation = annotationFromThoughtMaterialized(raw, target)
    if (!annotation) return { base: null, malformed: true }
    return {
      base: {
        annotation,
        target,
        targetKey,
        linkId: raw.linkId,
        hostKind: validIdentifier(raw.hostKind) ? raw.hostKind : (target.kind === 'note' ? 'note' : 'link'),
        hostId: validIdentifier(raw.hostId) ? raw.hostId : raw.linkId,
      },
      malformed: false,
    }
  }
  return { base: null, malformed: false }
}

function validLegacyOperation(operation: LegacyOperation): boolean {
  return operation.namespace !== UNATTRIBUTED_REPAIR_NAMESPACE && operation.opId !== null &&
    operation.annotationId !== null && operation.sequence !== null && operation.operationKind !== null &&
    operation.linkId !== null && operation.hostKind !== null && operation.hostId !== null &&
    operation.target !== null && operation.targetKey !== null && operation.contractVersion !== null &&
    (operation.operationKind !== 'update' || operation.patch !== undefined) &&
    (operation.operationKind !== 'add' || operation.annotation !== null)
}

function samePayload(left: LegacyOperation, right: LegacyOperation): boolean {
  return canonicalRepairJSON({
    operationKind: left.operationKind,
    annotationId: left.annotationId,
    linkId: left.linkId,
    hostKind: left.hostKind,
    hostId: left.hostId,
    target: left.target,
    targetKey: left.targetKey,
    annotation: left.annotation,
    patch: left.patch,
    versionKey: left.contractVersion === THOUGHT_CONTRACT_VERSION
      ? [left.contractVersion, left.deviceId, left.logicalClock]
      : null,
  }) === canonicalRepairJSON({
    operationKind: right.operationKind,
    annotationId: right.annotationId,
    linkId: right.linkId,
    hostKind: right.hostKind,
    hostId: right.hostId,
    target: right.target,
    targetKey: right.targetKey,
    annotation: right.annotation,
    patch: right.patch,
    versionKey: right.contractVersion === THOUGHT_CONTRACT_VERSION
      ? [right.contractVersion, right.deviceId, right.logicalClock]
      : null,
  })
}

function sameIdentity(left: LegacyOperation, right: LegacyOperation): boolean {
  return left.linkId === right.linkId && left.hostKind === right.hostKind && left.hostId === right.hostId &&
    left.targetKey === right.targetKey && canonicalRepairJSON(left.target) === canonicalRepairJSON(right.target)
}

function isCompleteAdd(operation: LegacyOperation): operation is LegacyOperation & {
  readonly operationKind: 'add'
  readonly annotation: Annotation
  readonly target: AnnotationTarget
  readonly targetKey: string
  readonly annotationId: string
} {
  return operation.operationKind === 'add' && operation.annotation !== null && operation.target !== null &&
    operation.targetKey !== null && operation.annotationId !== null &&
    operation.annotation.id === operation.annotationId &&
    cloneTargetAnnotation(operation.annotation, operation.target) !== null
}

function compareText(left: string, right: string): number {
  const a = new TextEncoder().encode(left)
  const b = new TextEncoder().encode(right)
  for (let index = 0; index < Math.min(a.length, b.length); index += 1) {
    if (a[index] !== b[index]) return a[index] < b[index] ? -1 : 1
  }
  return a.length - b.length
}

function compareLegacy(left: LegacyOperation, right: LegacyOperation): number {
  return (left.sequence ?? Number.MAX_SAFE_INTEGER) - (right.sequence ?? Number.MAX_SAFE_INTEGER) ||
    compareText(left.opId ?? left.sourceOpId, right.opId ?? right.sourceOpId)
}

function compareDispatch(left: PlannedEntry, right: PlannedEntry): number {
  const sequence = left.legacySequence - right.legacySequence
  if (sequence !== 0) return sequence
  const annotation = compareText(left.annotationId, right.annotationId)
  if (annotation !== 0) return annotation
  // The synthetic base has the same legacy position as the first tail but is
  // a required prefix, independent of its deterministic lexical op id.
  if (left.synthetic !== right.synthetic) return left.synthetic ? -1 : 1
  return compareText(left.opId, right.opId)
}

function stateFloor(rows: readonly unknown[], namespace: string): number {
  const values: number[] = []
  for (const row of rows) {
    if (!isRecord(row) || row.namespace !== namespace) continue
    if (isSafeNonNegativeInteger(row.logicalClockFloor)) values.push(row.logicalClockFloor)
  }
  return maximumThoughtClock(values.length > 0 ? values : [0])
}

function quarantine(
  namespace: string,
  annotationId: string,
  reason: ThoughtRepairReason,
  source: readonly LegacyOperation[],
): ThoughtRepairQuarantineRecord {
  return {
    key: [namespace, annotationId],
    namespace,
    annotationId,
    reason,
    source: source.map(canonicalLegacySource),
  }
}

function invalidAnnotationID(operation: LegacyOperation): string {
  return `invalid:${repairChecksum(operation.raw).slice(0, 32)}`
}

/** Builds every v6 derived row without mutating one legacy input object. */
export function planThoughtRepair(inputs: ThoughtRepairInputs): ThoughtRepairPlan {
  // v5 is an immutable, complete copy of the v4 operation tail. When it is
  // present it is authoritative; mixing both logs would manufacture duplicate
  // operations and makes a reopened v5 database diverge from direct v4.
  const operations = inputs.v5Outbox.length > 0
    ? inputs.v5Outbox.map((raw, index) => legacyOperation(raw, 'v5-outbox', index))
    : inputs.v4Operations.map((raw, index) => legacyOperation(raw, 'v4-operation', index))
  const sourceMap = new Map<string, ThoughtRepairSourceRecord>()
  for (const operation of operations) {
    const source = sourceRecord(operation)
    const key = `${source.namespace}\0${source.sourceId}`
    // Exact same op/payload is intentionally deduped. A divergent duplicate
    // has a distinct payload hash and remains visible for quarantine.
    if (!sourceMap.has(key)) sourceMap.set(key, source)
  }
  const sources = [...sourceMap.values()].sort((left, right) =>
    compareText(left.namespace, right.namespace) || compareText(left.sourceId, right.sourceId))
  const allNamespaces = new Set(operations.map((operation) => operation.namespace))
  const ready: ThoughtRepairReadyRecord[] = []
  const quarantineRows: ThoughtRepairQuarantineRecord[] = []
  const manifests: ThoughtRepairManifest[] = []
  const legacyFloors = new Map<string, number>()

  for (const namespace of [...allNamespaces].sort(compareText)) {
    const namespaceOperations = operations.filter((operation) => operation.namespace === namespace)
    const groups = new Map<string, LegacyOperation[]>()
    for (const operation of namespaceOperations) {
      const annotationId = operation.annotationId ?? invalidAnnotationID(operation)
      const group = groups.get(annotationId) ?? []
      group.push(operation)
      groups.set(annotationId, group)
    }
    const entries: PlannedEntry[] = []
    const namespaceQuarantine: ThoughtRepairQuarantineRecord[] = []
    const legacyClockFloor = maximumThoughtClock([
      stateFloor(inputs.syncStates, namespace),
      ...namespaceOperations.flatMap((operation) =>
        operation.contractVersion === THOUGHT_CONTRACT_VERSION && operation.logicalClock !== null
          ? [operation.logicalClock] : []),
    ])
    legacyFloors.set(namespace, legacyClockFloor)
    let floor = legacyClockFloor
    for (const [annotationId, original] of [...groups.entries()].sort(([a], [b]) => compareText(a, b))) {
      const sorted = [...original].sort(compareLegacy)
      let reason: ThoughtRepairReason | null = null
      if (namespace === UNATTRIBUTED_REPAIR_NAMESPACE || sorted.some((operation) => !validLegacyOperation(operation))) {
        reason = sorted.some((operation) => operation.annotationInvalid) ? 'invalid_quote' : 'invalid_legacy_op'
      }
      const deduped: LegacyOperation[] = []
      const seen = new Map<string, LegacyOperation>()
      if (!reason) for (const operation of sorted) {
        const prior = seen.get(operation.opId!)
        if (prior && !samePayload(prior, operation)) { reason = 'conflicting_duplicate_op'; break }
        if (!prior) { seen.set(operation.opId!, operation); deduped.push(operation) }
      }
      const first = deduped[0]
      if (!reason && first && deduped.some((operation) => !sameIdentity(first, operation))) {
        reason = 'identity_mismatch'
      }
      const completeFirst = !reason && first !== undefined && isCompleteAdd(first)
      let synthetic: PlannedEntry | null = null
      if (!reason && !completeFirst && first) {
        const v5 = baseFromMaterialized(inputs.v5Materialized, namespace, annotationId)
        const v4 = v5.base ? { base: null, malformed: false } :
          baseFromMaterialized(inputs.v4Materialized, namespace, annotationId)
        let base = v5.base ?? v4.base
        const malformed = v5.malformed || v4.malformed
        if (!base) {
          const add = deduped.find(isCompleteAdd)
          if (add) {
            base = {
              annotation: add.annotation,
              target: add.target,
              targetKey: add.targetKey,
              linkId: add.linkId!,
              hostKind: add.hostKind!,
              hostId: add.hostId!,
            }
          }
        }
        if (!base) reason = malformed ? 'invalid_quote' : 'missing_complete_base'
        else if (!sameIdentity(first, {
          ...first,
          linkId: base.linkId,
          hostKind: base.hostKind,
          hostId: base.hostId,
          target: base.target,
          targetKey: base.targetKey,
        })) reason = 'identity_mismatch'
        else {
          synthetic = {
            namespace,
            legacySequence: first.sequence!,
            annotationId,
            opId: stableRepairID(namespace, annotationId),
            source: null,
            operationKind: 'add',
            linkId: base.linkId,
            hostKind: base.hostKind,
            hostId: base.hostId,
            target: base.target,
            targetKey: base.targetKey,
            annotation: base.annotation,
            createdAt: base.annotation.createdAt,
            preserveVersionKey: false,
            synthetic: true,
          }
        }
      }
      if (reason || !first) {
        namespaceQuarantine.push(quarantine(namespace, annotationId, reason ?? 'invalid_legacy_op', sorted))
        continue
      }
      const repairChain = synthetic !== null || deduped.some((operation) =>
        operation.sourceKind === 'v4-operation' || operation.contractVersion !== THOUGHT_CONTRACT_VERSION ||
        operation.deviceId === null || operation.logicalClock === null)
      if (synthetic) entries.push(synthetic)
      for (const operation of deduped) {
        entries.push({
          namespace,
          legacySequence: operation.sequence!,
          annotationId,
          opId: operation.opId!,
          source: operation,
          operationKind: operation.operationKind!,
          linkId: operation.linkId!,
          hostKind: operation.hostKind!,
          hostId: operation.hostId!,
          target: operation.target!,
          targetKey: operation.targetKey!,
          annotation: operation.annotation,
          ...(operation.patch === undefined ? {} : { patch: operation.patch }),
          createdAt: operation.createdAt,
          preserveVersionKey: !repairChain && operation.contractVersion === THOUGHT_CONTRACT_VERSION &&
            operation.deviceId !== null && operation.logicalClock !== null,
          synthetic: false,
        })
      }
    }
    const namespaceReady: ThoughtRepairReadyRecord[] = []
    for (const [index, entry] of entries.sort(compareDispatch).entries()) {
      let deviceId: string
      let logicalClock: number
      if (entry.preserveVersionKey && entry.source?.deviceId && entry.source.logicalClock !== null) {
        deviceId = entry.source.deviceId
        logicalClock = entry.source.logicalClock
      } else {
        floor = nextThoughtLogicalClock([floor])
        deviceId = repairDeviceID(namespace)
        logicalClock = floor
      }
      const source = entry.source
      const record: ThoughtRepairReadyRecord = {
        key: [namespace, index + 1],
        namespace,
        sequence: index + 1,
        opId: entry.opId,
        deviceId,
        contractVersion: THOUGHT_CONTRACT_VERSION,
        logicalClock,
        operationKind: entry.operationKind,
        annotationId: entry.annotationId,
        hostKind: entry.hostKind,
        hostId: entry.hostId,
        linkId: entry.linkId,
        target: entry.target,
        targetKey: entry.targetKey,
        annotation: entry.annotation,
        ...(entry.patch === undefined ? {} : { patch: entry.patch }),
        createdAt: entry.createdAt,
        attemptCount: source?.attemptCount ?? 0,
        ...(source?.checksum === undefined ? {} : { checksum: source.checksum }),
        repair: true,
      }
      namespaceReady.push(record)
    }
    ready.push(...namespaceReady)
    quarantineRows.push(...namespaceQuarantine)
    const namespaceSources = sources.filter((source) => source.namespace === namespace)
    manifests.push({
      namespace,
      schemaVersion: THOUGHT_REPAIR_SCHEMA_VERSION,
      sourceCount: namespaceSources.length,
      readyCount: namespaceReady.length,
      quarantineCount: namespaceQuarantine.length,
      sourceChecksum: repairChecksum(namespaceSources.map((source) => ({
        namespace: source.namespace,
        sourceId: source.sourceId,
        sourceChecksum: source.sourceChecksum,
      }))),
      readyChecksum: repairChecksum(namespaceReady),
      quarantineChecksum: repairChecksum(namespaceQuarantine),
      legacyClockFloor,
      logicalClockFloor: floor,
      complete: true,
    })
  }
  return {
    sources: sources.map((source) => ({
      ...source,
      legacyClockFloor: legacyFloors.get(source.namespace) ?? 0,
    })),
    ready,
    quarantine: quarantineRows,
    manifests,
  }
}

/**
 * After schema v6 is installed, normal annotation operations continue to be
 * appended to the old operational stores. Their sequence must never make them
 * look like a second legacy migration. This selector retains only rows that
 * match the source receipt written atomically during the original versionchange.
 */
export function repairInputsForKnownSources(
  inputs: ThoughtRepairInputs,
  known: readonly unknown[],
): ThoughtRepairInputs {
  const fingerprints = new Set(known.filter(isRecord).map((value) => sourceReceiptFingerprint(
    value as unknown as ThoughtRepairSourceRecord,
  )))
  const retain = (rows: readonly unknown[], sourceKind: ThoughtRepairSourceKind): unknown[] =>
    rows.filter((raw, index) => fingerprints.has(sourceReceiptFingerprint(sourceRecord(
      legacyOperation(raw, sourceKind, index),
    ))))
  const floorByNamespace = new Map<string, number>()
  for (const source of known) {
    if (!isRecord(source) || !validIdentifier(source.namespace) ||
      !isSafeNonNegativeInteger(source.legacyClockFloor)) continue
    const prior = floorByNamespace.get(source.namespace) ?? 0
    floorByNamespace.set(source.namespace, Math.max(prior, source.legacyClockFloor))
  }
  return {
    ...inputs,
    v4Operations: retain(inputs.v4Operations, 'v4-operation'),
    v5Outbox: retain(inputs.v5Outbox, 'v5-outbox'),
    syncStates: [...floorByNamespace.entries()].map(([namespace, logicalClockFloor]) => ({
      namespace,
      logicalClockFloor,
    })),
  }
}

export function isThoughtRepairAck(
  value: unknown,
  expected: ThoughtRepairReadyRecord,
): value is ThoughtRepairAckRecord {
  return isRecord(value) && Array.isArray(value.key) && value.key.length === 2 &&
    value.key[0] === expected.namespace && value.key[1] === expected.opId &&
    value.namespace === expected.namespace && value.opId === expected.opId &&
    value.readyChecksum === repairReadyChecksum(expected)
}

/** Retry bookkeeping is mutable; an acknowledgement binds only the immutable wire operation. */
export function repairReadyChecksum(record: ThoughtRepairReadyRecord): string {
  return repairChecksum({
    key: record.key,
    namespace: record.namespace,
    sequence: record.sequence,
    opId: record.opId,
    deviceId: record.deviceId,
    contractVersion: record.contractVersion,
    logicalClock: record.logicalClock,
    operationKind: record.operationKind,
    annotationId: record.annotationId,
    hostKind: record.hostKind,
    hostId: record.hostId,
    linkId: record.linkId,
    target: record.target,
    targetKey: record.targetKey,
    annotation: record.annotation,
    patch: record.patch ?? null,
    createdAt: record.createdAt,
    checksum: record.checksum ?? null,
    repair: true,
  })
}
