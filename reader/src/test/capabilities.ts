import type { CapabilitiesResponse } from '../lib/api/types'
import {
  deriveReaderCapabilityPolicy,
  ReaderCapabilityLease,
  type ReaderCapabilityPolicy,
} from '../lib/capabilities'

export const ENABLED_READER_CAPABILITIES: CapabilitiesResponse = {
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

export function enabledReaderCapabilityPolicy(
  overrides: Partial<ReaderCapabilityPolicy> = {},
): ReaderCapabilityPolicy {
  return {
    ...deriveReaderCapabilityPolicy(ENABLED_READER_CAPABILITIES),
    ...overrides,
  }
}

export function enabledReaderCapabilityLease(
  overrides: Partial<ReaderCapabilityPolicy> = {},
): ReaderCapabilityLease {
  return new ReaderCapabilityLease(enabledReaderCapabilityPolicy(overrides))
}
