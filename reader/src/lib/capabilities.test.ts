import { describe, expect, it } from 'vitest'
import type { CapabilitiesResponse } from './api/types'
import type { ReaderRoute } from './navigation/route'
import {
  deriveReaderCapabilityPolicy,
  firstAvailableReaderRoute,
  ReaderCapabilityLease,
  readerRouteIsAvailable,
  type ReaderCapabilityPolicy,
} from './capabilities'

const ENABLED: CapabilitiesResponse = {
  library_kinds: true,
  site_library: true,
  site_management: true,
  site_advanced_management: true,
  archive_versions: [2],
  reader_vnext: true,
  reader: {
    annotations: true,
    notes: true,
    inbox: true,
    todos: true,
    engagement: true,
    home: true,
    feed: true,
    ai: true,
    related_tags: true,
    activity: true,
    history: true,
    trash: true,
  },
}

describe('Reader capability policy', () => {
  it.each([
    ['annotations', 'annotations'],
    ['notes', 'notes'],
    ['inbox', 'inbox'],
    ['todos', 'todos'],
    ['engagement', 'engagement'],
    ['home', 'home'],
    ['feed', 'feed'],
    ['ai', 'ai'],
    ['related_tags', 'relatedTags'],
    ['activity', 'activity'],
    ['history', 'history'],
    ['trash', 'trash'],
  ] as const)('maps reader.%s only to %s', (wireKey, policyKey) => {
    const capabilities: CapabilitiesResponse = {
      ...ENABLED,
      reader: { ...ENABLED.reader, [wireKey]: false },
    }
    const policy = deriveReaderCapabilityPolicy(capabilities)
    expect(policy[policyKey]).toBe(false)
    for (const [key, value] of Object.entries(policy)) {
      if (key !== policyKey && !key.startsWith('site')) expect(value).toBe(true)
    }
  })

  it('fails closed for a missing, partial, or globally disabled snapshot', () => {
    expect(Object.values(deriveReaderCapabilityPolicy(undefined)).some(Boolean)).toBe(false)
    const partial = {
      ...ENABLED,
      reader: { ...ENABLED.reader, notes: undefined },
    } as unknown as CapabilitiesResponse
    expect(deriveReaderCapabilityPolicy(partial).notes).toBe(false)
    expect(deriveReaderCapabilityPolicy({ ...ENABLED, reader_vnext: false }).home).toBe(false)
  })

  it('keeps site read, write, and advanced management independent', () => {
    const readOnly = deriveReaderCapabilityPolicy({
      ...ENABLED,
      site_management: false,
    })
    expect(readOnly).toMatchObject({ siteRead: true, siteWrite: false, siteAdvanced: false })

    const writeOnly = deriveReaderCapabilityPolicy({
      ...ENABLED,
      site_advanced_management: false,
    })
    expect(writeOnly).toMatchObject({ siteRead: true, siteWrite: true, siteAdvanced: false })
  })

  it.each([
    ['Home', { kind: 'surface', id: 'home' }, 'home'],
    ['Feed', { kind: 'surface', id: 'feed' }, 'feed'],
    ['Inbox', { kind: 'library', id: 'pending' }, 'inbox'],
    ['Sites', { kind: 'library', id: 'sites' }, 'siteRead'],
    ['Notes', { kind: 'library', id: 'notes' }, 'notes'],
    ['TODO', { kind: 'tool', id: 'todo' }, 'todos'],
    ['Thought History', { kind: 'tool', id: 'history' }, 'history'],
  ] satisfies ReadonlyArray<readonly [string, ReaderRoute, keyof ReaderCapabilityPolicy]>)('%s route follows only %s', (_label, route, policyKey) => {
    const policy = deriveReaderCapabilityPolicy(ENABLED)
    expect(readerRouteIsAvailable(route, policy)).toBe(true)
    expect(readerRouteIsAvailable(route, { ...policy, [policyKey]: false })).toBe(false)
  })

  it.each([
    ['Reading', { kind: 'library', id: 'reading' }],
    ['Subscriptions', { kind: 'library', id: 'subs' }],
    ['Settings', { kind: 'tool', id: 'settings' }],
  ] satisfies ReadonlyArray<readonly [string, ReaderRoute]>)('keeps the canonical %s route available without a fine-grained gate', (_label, route) => {
    expect(readerRouteIsAvailable(route, deriveReaderCapabilityPolicy(undefined))).toBe(true)
  })

  it('chooses the first available canonical fallback', () => {
    const policy = deriveReaderCapabilityPolicy({
      ...ENABLED,
      reader: { ...ENABLED.reader, notes: false, home: false },
    })
    expect(firstAvailableReaderRoute(policy)).toEqual({ kind: 'surface', id: 'feed' })
  })

  it('allows mounted-owner replay but never reactivates an explicitly revoked generation', () => {
    const lease = new ReaderCapabilityLease(deriveReaderCapabilityPolicy(ENABLED))

    lease.deactivate()
    expect(lease.isCurrent('archiveDownload')).toBe(false)
    lease.activate()
    expect(lease.isCurrent('archiveDownload')).toBe(true)

    lease.revoke()
    lease.activate()
    expect(lease.isCurrent('archiveDownload')).toBe(false)
  })
})
