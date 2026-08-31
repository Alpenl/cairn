import { useCallback, useEffect, useMemo } from 'react'
import type { ReaderClient } from '../lib/api/client'
import type { ApiError } from '@webtag/api'
import type { FeedSubscription, SubscriptionsResponse } from '../lib/api/types'
import type { IdentityOwnership } from '../lib/identity'
import { SUBSCRIPTIONS_CACHE_KEY } from '../lib/cache/keys'
import { resourceStore } from '../lib/cache/store'
import { useCachedResource } from '../lib/cache/useCachedResource'

const EMPTY_SUBSCRIPTIONS: SubscriptionsResponse = {
  folders: [],
  subscriptions: [],
  counts: { all: 0, unread: 0, starred: 0, later: 0 },
}

export { SUBSCRIPTIONS_CACHE_KEY } from '../lib/cache/keys'

/**
 * RSS navigation data with retained-data refresh errors and a 60-second poll.
 *
 * PF3 起走共享缓存：SubsView 卸载重挂不再全量重拉，60 秒轮询在数据未变时
 * 也不再产生任何重渲染。
 */
export function useSubscriptions(client: ReaderClient) {
  const resource = useCachedResource<SubscriptionsResponse>(
    SUBSCRIPTIONS_CACHE_KEY,
    (conditional) => client.getSubscriptions(undefined, conditional),
  )

  const reload = useCallback(
    async (quiet = false): Promise<boolean> => {
      const result = await resource.reload({ silent: quiet })
      return Boolean(result?.ok)
    },
    [resource],
  )

  useEffect(() => {
    const timer = window.setInterval(() => void reload(true), 60_000)
    return () => window.clearInterval(timer)
  }, [reload])

  const patchSubscription = useCallback((
    id: string,
    patch: Partial<FeedSubscription>,
  ) => {
    const update = (current: SubscriptionsResponse): SubscriptionsResponse => ({
      ...current,
      subscriptions: current.subscriptions.map((subscription) =>
        subscription.id === id ? { ...subscription, ...patch } : subscription,
      ),
    })
    resourceStore.patch(SUBSCRIPTIONS_CACHE_KEY, update)
  }, [])

  const patchSubscriptionForIdentity = useCallback((
    ownership: IdentityOwnership,
    id: string,
    patch: Partial<FeedSubscription>,
  ) => {
    resourceStore.patchForIdentity<SubscriptionsResponse>(
      ownership,
      SUBSCRIPTIONS_CACHE_KEY,
      (current) => ({
        ...current,
        subscriptions: current.subscriptions.map((subscription) =>
          subscription.id === id ? { ...subscription, ...patch } : subscription,
        ),
      }),
    )
  }, [])

  return useMemo(
    () => ({
      data: resource.data ?? EMPTY_SUBSCRIPTIONS,
      loading: resource.loading,
      error: resource.error as ApiError | null,
      reload,
      patchSubscription,
      patchSubscriptionForIdentity,
    }),
    [
      resource.data,
      resource.loading,
      resource.error,
      reload,
      patchSubscription,
      patchSubscriptionForIdentity,
    ],
  )
}
