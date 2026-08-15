import type { IdentityLease } from '../identity'
import { isValidLinkId, isValidSourceIdentityComponent } from './source-block'
import {
  ownedStorageKeyForLease,
  writeOwnedStorageForLease,
} from '../storage-ownership'

export const ANNOTATION_CHANNEL_NAME = 'webtag-reader-annotations-v1'

export interface AnnotationChangeHint {
  readonly kind: 'annotation-change'
  readonly namespace: string
  readonly linkId: string
  readonly documentRevision: number
  readonly annotationStoreVersion: number
}

export type AnnotationChangeHintInput = Omit<
  AnnotationChangeHint,
  'kind' | 'namespace'
>

type HintListener = (hint: AnnotationChangeHint) => void

let wakeupSequence = 0

function isNonNegativeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
}

function parseHint(value: unknown): AnnotationChangeHint | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null
  const candidate = value as Record<string, unknown>
  if (
    candidate.kind !== 'annotation-change' ||
    !isValidSourceIdentityComponent(candidate.namespace) ||
    !isValidLinkId(candidate.linkId) ||
    !isNonNegativeInteger(candidate.documentRevision) ||
    !isNonNegativeInteger(candidate.annotationStoreVersion)
  ) {
    return null
  }
  return {
    kind: 'annotation-change',
    namespace: candidate.namespace,
    linkId: candidate.linkId,
    documentRevision: candidate.documentRevision,
    annotationStoreVersion: candidate.annotationStoreVersion,
  }
}

function parseSerializedHint(value: string | null): AnnotationChangeHint | null {
  if (value === null) return null
  try {
    return parseHint(JSON.parse(value))
  } catch {
    return null
  }
}

export class AnnotationDocumentChannel {
  private readonly namespace: string
  private readonly wakeupKey: string | null
  private broadcast: BroadcastChannel | null = null
  private disposed = false

  constructor(
    private readonly lease: IdentityLease,
    private readonly listener: HintListener,
  ) {
    this.namespace = lease.context.physicalNamespace
    this.wakeupKey = ownedStorageKeyForLease('annotationWakeup', lease)
    try {
      if (typeof BroadcastChannel !== 'undefined') {
        this.broadcast = new BroadcastChannel(ANNOTATION_CHANNEL_NAME)
        this.broadcast.addEventListener('message', this.onBroadcast)
      }
    } catch {
      this.broadcast = null
    }
    if (typeof window !== 'undefined') {
      window.addEventListener('storage', this.onStorage)
    }
  }

  private readonly onBroadcast = (event: MessageEvent<unknown>): void => {
    this.deliver(parseHint(event.data))
  }

  private readonly onStorage = (event: StorageEvent): void => {
    if (event.key !== this.wakeupKey) return
    this.deliver(parseSerializedHint(event.newValue))
  }

  private deliver(hint: AnnotationChangeHint | null): void {
    if (
      this.disposed ||
      !hint ||
      hint.namespace !== this.namespace ||
      !this.lease.isCurrent(this.lease.capture('receive annotation change hint'))
    ) {
      return
    }
    this.listener(hint)
  }

  publish(input: AnnotationChangeHintInput): boolean {
    const operation = this.lease.capture('publish annotation change hint')
    if (
      this.disposed ||
      !this.lease.isCurrent(operation) ||
      !isValidLinkId(input.linkId) ||
      !isNonNegativeInteger(input.documentRevision) ||
      !isNonNegativeInteger(input.annotationStoreVersion)
    ) {
      return false
    }
    const hint: AnnotationChangeHint = {
      kind: 'annotation-change',
      namespace: this.namespace,
      linkId: input.linkId,
      documentRevision: input.documentRevision,
      annotationStoreVersion: input.annotationStoreVersion,
    }

    if (this.broadcast) {
      try {
        this.broadcast.postMessage(hint)
        return true
      } catch {
        this.broadcast.removeEventListener('message', this.onBroadcast)
        this.broadcast.close()
        this.broadcast = null
      }
    }

    wakeupSequence += 1
    return writeOwnedStorageForLease(
      'annotationWakeup',
      JSON.stringify({
        ...hint,
        nonce: `${Date.now()}:${wakeupSequence}`,
      }),
      this.lease,
    )
  }

  dispose(): void {
    if (this.disposed) return
    this.disposed = true
    if (typeof window !== 'undefined') {
      window.removeEventListener('storage', this.onStorage)
    }
    if (this.broadcast) {
      this.broadcast.removeEventListener('message', this.onBroadcast)
      this.broadcast.close()
      this.broadcast = null
    }
  }
}
