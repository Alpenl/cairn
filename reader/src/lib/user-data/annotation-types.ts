import type {
  Annotation,
  AnnotationQuote,
  AnnotationSource,
  AnnotationTarget,
  NoteAnnotationTarget,
  SavedContentAnnotationBlockKey,
  SavedContentAnnotationTarget,
  SummaryAnnotationTarget,
} from '../annotations-domain'

export {
  annotationTargetKey,
  canonicalAnnotationTarget,
} from '../annotations-domain'
export type {
  Annotation,
  AnnotationQuote,
  AnnotationSource,
  AnnotationTarget,
  NoteAnnotationTarget,
  SavedContentAnnotationBlockKey,
  SavedContentAnnotationTarget,
  SummaryAnnotationTarget,
} from '../annotations-domain'

export interface AnnotationUpdatePatch {
  readonly note?: string
  readonly source?: AnnotationSource
  readonly updatedAt: number
}

interface AnnotationOperationInputBase<TTarget extends AnnotationTarget = AnnotationTarget> {
  readonly opId: string
  readonly linkId: string
  readonly target: TTarget
}

interface AnnotationAddDraftBase {
  readonly id: string
  readonly start: number
  readonly end: number
  readonly text: string
  readonly note: string
  readonly source: AnnotationSource
  readonly createdAt: number
  readonly updatedAt: number
  /** TextQuoteSelector context retained for note re-anchoring. */
  readonly quote?: AnnotationQuote
  /** Source identity is derived from `target`, never supplied by a caller. */
  readonly sourceContentRevision?: never
  /** Source identity is derived from `target`, never supplied by a caller. */
  readonly sourceSummaryHash?: never
}

export interface SavedContentAnnotationAddDraft extends AnnotationAddDraftBase {
  readonly blockKey: SavedContentAnnotationBlockKey
}

export interface SummaryAnnotationAddDraft extends AnnotationAddDraftBase {
  /** Summary is the target's single canonical block; callers cannot choose another block. */
  readonly blockKey?: never
}

export interface NoteAnnotationAddDraft extends AnnotationAddDraftBase {
  /** Defaults to the note host block when an old caller omitted it. */
  readonly blockKey?: string
}

export type AnnotationAddDraft =
  | SavedContentAnnotationAddDraft
  | SummaryAnnotationAddDraft
  | NoteAnnotationAddDraft

export type AnnotationAddOperationInput =
  | (AnnotationOperationInputBase<SavedContentAnnotationTarget> & {
      readonly kind: 'add'
      readonly draft: SavedContentAnnotationAddDraft
    })
  | (AnnotationOperationInputBase<SummaryAnnotationTarget> & {
      readonly kind: 'add'
      readonly draft: SummaryAnnotationAddDraft
    })
  | (AnnotationOperationInputBase<NoteAnnotationTarget> & {
      readonly kind: 'add'
      readonly draft: NoteAnnotationAddDraft
    })

export interface AnnotationUpdateOperationInput extends AnnotationOperationInputBase {
  readonly kind: 'update'
  readonly annotationId: string
  readonly patch: AnnotationUpdatePatch
}

export interface AnnotationDeleteOperationInput extends AnnotationOperationInputBase {
  readonly kind: 'delete'
  readonly annotationId: string
}

export type AnnotationOperationInput =
  | AnnotationAddOperationInput
  | AnnotationUpdateOperationInput
  | AnnotationDeleteOperationInput

export interface AnnotationCommitOptions {
  /** Aborts the durable transaction when its SavedArticleDocument generation changes. */
  readonly signal?: AbortSignal
}

interface AnnotationOperationRecordBase {
  readonly sequence: number
  readonly opId: string
  readonly namespace: string
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly targetKey: string
  readonly annotationId: string
}

export interface AnnotationAddOperationRecord extends AnnotationOperationRecordBase {
  readonly kind: 'add'
  readonly annotation: Annotation
}

export interface AnnotationUpdateOperationRecord extends AnnotationOperationRecordBase {
  readonly kind: 'update'
  readonly patch: AnnotationUpdatePatch
}

export interface AnnotationDeleteOperationRecord extends AnnotationOperationRecordBase {
  readonly kind: 'delete'
}

export type AnnotationOperationRecord =
  | AnnotationAddOperationRecord
  | AnnotationUpdateOperationRecord
  | AnnotationDeleteOperationRecord

export type AnnotationMaterializedKey = [
  namespace: string,
  linkId: string,
  targetKey: string,
  annotationId: string,
]

export type AnnotationTargetStateKey = [
  namespace: string,
  linkId: string,
  targetKey: string,
]

export interface AnnotationMaterializedRecord {
  readonly key: AnnotationMaterializedKey
  readonly namespace: string
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly targetKey: string
  readonly annotationId: string
  readonly sequence: number
  readonly annotation: Annotation | null
  /** Last visible value retained behind a tombstone for sequence-ordered updates. */
  readonly fallbackAnnotation: Annotation | null
  /** Stable source owner while this row is still controlled by a legacy snapshot import. */
  readonly importedBy?: string
}

export interface AnnotationReplaySnapshotItem {
  readonly annotationId: string
  readonly sequence: number
  readonly annotation: Annotation | null
  readonly fallbackAnnotation: Annotation | null
}

export interface AnnotationOperationReceipt {
  /** Reserved `annotation_imports` key prefix; migration markers use other keys. */
  readonly key: [
    kind: 'operation-receipt',
    namespace: string,
    opId: string,
    linkId: string,
    targetKey: string,
  ]
  readonly kind: 'operation-receipt'
  readonly opId: string
  readonly namespace: string
  readonly linkId: string
  readonly targetKey: string
  readonly annotationId: string
  readonly operationKind: AnnotationOperationRecord['kind']
  readonly sequence: number
  readonly signature: string
}

export interface AnnotationLinkStateRecord {
  readonly key: AnnotationTargetStateKey
  readonly namespace: string
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly targetKey: string
  /** The last committed operation sequence for this source target. */
  readonly version: number
  readonly activeCount: number
  readonly compactedThroughSequence: number
  readonly snapshot: readonly AnnotationReplaySnapshotItem[]
}

export interface AnnotatedLinkRecord {
  readonly key: AnnotationTargetStateKey
  readonly namespace: string
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly targetKey: string
  readonly annotationCount: number
  readonly annotationStoreVersion: number
}

export interface AnnotationSnapshot {
  readonly namespace: string
  readonly linkId: string
  readonly target: AnnotationTarget
  readonly annotationStoreVersion: number
  readonly annotations: readonly Annotation[]
}

export type AnnotationCommitResult =
  | {
      readonly status: 'committed' | 'duplicate'
      readonly sequence: number
      readonly annotationStoreVersion: number
      readonly annotation: Annotation | null
    }
  | { readonly status: 'op-id-conflict' }

export interface AnnotationCompactionOptions {
  readonly threshold?: number
}

export interface AnnotationCompactionResult {
  readonly status: 'compacted' | 'skipped'
  readonly highWaterMark: number
  readonly operationsDeleted: number
  readonly annotationStoreVersion: number
}

export const DEFAULT_ANNOTATION_COMPACTION_THRESHOLD = 256

export function annotationMaterializedKey(
  namespace: string,
  linkId: string,
  targetKey: string,
  annotationId: string,
): AnnotationMaterializedKey {
  return [namespace, linkId, targetKey, annotationId]
}

export function annotationTargetStateKey(
  namespace: string,
  linkId: string,
  targetKey: string,
): AnnotationTargetStateKey {
  return [namespace, linkId, targetKey]
}
