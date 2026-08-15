import { describe, expect, it } from 'vitest'

import { readerIdentity } from '../../lib/identity'
import {
  ALL_FEEDS_SELECTION,
  loadFeedSelection,
  saveFeedSelection,
  type FeedSelection,
} from './model'

describe('feed selection identity ownership', () => {
  it('does not expose A selection to B and restores it after returning to A', () => {
    const selectedA: FeedSelection = {
      kind: 'subscription',
      id: 'feed-A',
      name: 'A feed',
    }
    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    saveFeedSelection(selectedA)

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(loadFeedSelection()).toEqual(ALL_FEEDS_SELECTION)

    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect(loadFeedSelection()).toEqual(selectedA)
  })
})
