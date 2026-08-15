import type { FeedView, ListFeedItemsParams } from '../../lib/api/types'
import { readOwnedStorage, writeOwnedStorage } from '../../lib/storage-ownership'

const FEED_VIEWS = new Set<FeedView>(['all', 'unread', 'starred', 'later'])

export type FeedSelection =
  | { kind: 'view'; id: FeedView; name: string }
  | { kind: 'folder'; id: string; name: string }
  | { kind: 'subscription'; id: string; name: string }

export const ALL_FEEDS_SELECTION: FeedSelection = {
  kind: 'view',
  id: 'all',
  name: '全部文章',
}

export function loadFeedSelection(): FeedSelection {
  try {
    const value = JSON.parse(readOwnedStorage('feedSelection') || 'null') as unknown
    if (!value || typeof value !== 'object') return ALL_FEEDS_SELECTION
    const candidate = value as Record<string, unknown>
    if (
      candidate.kind === 'view' &&
      typeof candidate.id === 'string' &&
      FEED_VIEWS.has(candidate.id as FeedView) &&
      typeof candidate.name === 'string'
    ) {
      return candidate as FeedSelection
    }
    if (
      (candidate.kind === 'folder' || candidate.kind === 'subscription') &&
      typeof candidate.id === 'string' &&
      candidate.id !== '' &&
      typeof candidate.name === 'string' &&
      candidate.name !== ''
    ) {
      return candidate as FeedSelection
    }
  } catch {
    // A damaged browser preference must not prevent the Reader from opening.
  }
  return ALL_FEEDS_SELECTION
}

export function saveFeedSelection(selection: FeedSelection): void {
  writeOwnedStorage('feedSelection', JSON.stringify(selection))
}

export function selectionFilters(selection: FeedSelection): ListFeedItemsParams {
  if (selection.kind === 'view') return { view: selection.id }
  if (selection.kind === 'folder') return { view: 'all', folder_id: selection.id }
  return { view: 'all', subscription_id: selection.id }
}
