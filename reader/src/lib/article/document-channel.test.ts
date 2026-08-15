import { afterEach, describe, expect, it, vi } from 'vitest'

import { IdentityLease } from '../identity'
import { ownedStorageKeyForLease } from '../storage-ownership'
import {
  ANNOTATION_CHANNEL_NAME,
  AnnotationDocumentChannel,
  type AnnotationChangeHint,
} from './document-channel'

class FakeBroadcastChannel {
  static readonly instances = new Set<FakeBroadcastChannel>()

  readonly listeners = new Set<(event: MessageEvent<unknown>) => void>()
  closed = false

  constructor(readonly name: string) {
    FakeBroadcastChannel.instances.add(this)
  }

  addEventListener(_type: 'message', listener: EventListener): void {
    this.listeners.add(listener as (event: MessageEvent<unknown>) => void)
  }

  removeEventListener(_type: 'message', listener: EventListener): void {
    this.listeners.delete(listener as (event: MessageEvent<unknown>) => void)
  }

  postMessage(value: unknown): void {
    for (const instance of FakeBroadcastChannel.instances) {
      if (instance === this || instance.name !== this.name || instance.closed) continue
      for (const listener of instance.listeners) {
        listener(new MessageEvent('message', { data: value }))
      }
    }
  }

  close(): void {
    this.closed = true
    FakeBroadcastChannel.instances.delete(this)
  }
}

function lease(namespace = 'physical-A'): IdentityLease {
  return new IdentityLease({
    serverClientDataNamespace: `server:${namespace}`,
    physicalNamespace: namespace,
    localEpoch: 1,
  })
}

afterEach(() => {
  vi.unstubAllGlobals()
  FakeBroadcastChannel.instances.clear()
})

describe('AnnotationDocumentChannel', () => {
  it('broadcasts only a validated same-namespace durable reread hint', () => {
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    const receiver = vi.fn<(hint: AnnotationChangeHint) => void>()
    const first = new AnnotationDocumentChannel(lease(), () => undefined)
    const second = new AnnotationDocumentChannel(lease(), receiver)
    const foreign = new AnnotationDocumentChannel(lease('physical-B'), receiver)

    expect(first.publish({
      linkId: 'L1',
      documentRevision: 7,
      annotationStoreVersion: 3,
    })).toBe(true)
    expect(receiver).toHaveBeenCalledTimes(1)
    expect(receiver).toHaveBeenCalledWith({
      kind: 'annotation-change',
      namespace: 'physical-A',
      linkId: 'L1',
      documentRevision: 7,
      annotationStoreVersion: 3,
    })

    first.dispose()
    second.dispose()
    foreign.dispose()
  })

  it('uses a different namespaced storage wakeup value when BroadcastChannel is unavailable', () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const sender = new AnnotationDocumentChannel(owner, () => undefined)
    const receiver = vi.fn<(hint: AnnotationChangeHint) => void>()
    const otherPage = new AnnotationDocumentChannel(owner, receiver)
    const key = ownedStorageKeyForLease('annotationWakeup', owner)
    expect(key).not.toBeNull()

    expect(sender.publish({
      linkId: 'L1',
      documentRevision: 7,
      annotationStoreVersion: 1,
    })).toBe(true)
    const firstValue = localStorage.getItem(key as string)
    expect(sender.publish({
      linkId: 'L1',
      documentRevision: 7,
      annotationStoreVersion: 2,
    })).toBe(true)
    const secondValue = localStorage.getItem(key as string)
    expect(secondValue).not.toBe(firstValue)

    window.dispatchEvent(new StorageEvent('storage', {
      key,
      newValue: JSON.stringify({
        kind: 'annotation-change',
        namespace: 'physical-A',
        linkId: 'L1',
        documentRevision: 7,
        annotationStoreVersion: 2,
        items: [{ id: 'untrusted' }],
      }),
    }))
    expect(receiver).toHaveBeenCalledWith({
      kind: 'annotation-change',
      namespace: 'physical-A',
      linkId: 'L1',
      documentRevision: 7,
      annotationStoreVersion: 2,
    })

    sender.dispose()
    otherPage.dispose()
  })

  it('ignores malformed, foreign, revoked, and disposed hints', () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const owner = lease()
    const listener = vi.fn()
    const channel = new AnnotationDocumentChannel(owner, listener)
    const key = ownedStorageKeyForLease('annotationWakeup', owner)

    window.dispatchEvent(new StorageEvent('storage', {
      key,
      newValue: JSON.stringify({
        kind: 'annotation-change',
        namespace: 'physical-B',
        linkId: 'L1',
        documentRevision: 7,
        annotationStoreVersion: 1,
      }),
    }))
    window.dispatchEvent(new StorageEvent('storage', {
      key,
      newValue: JSON.stringify({
        kind: 'annotation-change',
        namespace: 'physical-A',
        linkId: 'L\0other',
        documentRevision: 7,
        annotationStoreVersion: 1,
      }),
    }))
    window.dispatchEvent(new StorageEvent('storage', { key, newValue: '{' }))
    expect(listener).not.toHaveBeenCalled()

    owner.revoke()
    expect(channel.publish({
      linkId: 'L1',
      documentRevision: 7,
      annotationStoreVersion: 2,
    })).toBe(false)
    channel.dispose()
    window.dispatchEvent(new StorageEvent('storage', {
      key,
      newValue: JSON.stringify({
        kind: 'annotation-change',
        namespace: 'physical-A',
        linkId: 'L1',
        documentRevision: 7,
        annotationStoreVersion: 3,
      }),
    }))
    expect(listener).not.toHaveBeenCalled()
  })

  it('rejects ambiguous or invalid outbound identities', () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const channel = new AnnotationDocumentChannel(lease(), () => undefined)
    expect(channel.publish({
      linkId: 'L\0other',
      documentRevision: 7,
      annotationStoreVersion: 1,
    })).toBe(false)
    expect(channel.publish({
      linkId: 'L1',
      documentRevision: -1,
      annotationStoreVersion: 1,
    })).toBe(false)
    channel.dispose()
  })

  it('uses the stable production channel name', () => {
    expect(ANNOTATION_CHANNEL_NAME).toBe('webtag-reader-annotations-v1')
  })
})
