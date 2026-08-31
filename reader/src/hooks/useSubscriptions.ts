import { useCallback, useMemo } from 'react'
import type { ApiError } from '@webtag/api'
import type { FeedSubscription, SubscriptionsResponse } from '../lib/api/types'
import type { ReaderSubscriptionsFeedPort } from '../lib/reader-api-ports'
import type { IdentityOwnership } from '../lib/identity'
import { SUBSCRIPTIONS_CACHE_KEY } from '../lib/cache/keys'
import { resourceStore } from '../lib/cache/store'
import { useIdentityBoundOperationGate } from './identityBoundOperation'
import { useFinalIdentityCachedResource } from './useIdentityCachedResource'
import { useIdentityPolling } from './useIdentityPolling'

const EMPTY_SUBSCRIPTIONS: SubscriptionsResponse = {
  folders: [],
  subscriptions: [],
  counts: { all: 0, unread: 0, starred: 0, later: 0 },
}

export { SUBSCRIPTIONS_CACHE_KEY } from '../lib/cache/keys'

type SubscriptionsClient = Pick<
  ReaderSubscriptionsFeedPort,
  'identityLease' | 'isIdentityCurrent' | 'captureIdentity' | 'getSubscriptions'
>

/**
 * RSS navigation data with retained-data refresh errors and a 60-second poll.
 *
 * PF3 起走共享缓存：SubsView 卸载重挂不再全量重拉，60 秒轮询在数据未变时
 * 也不再产生任何重渲染。
 */
export function useSubscriptions(client: SubscriptionsClient) {
  const {
    canFetch,
    resource,
  } = useFinalIdentityCachedResource<SubscriptionsResponse, SubscriptionsClient>(
    client,
    SUBSCRIPTIONS_CACHE_KEY,
    ({ client, signal }) => client.getSubscriptions(undefined, { signal }),
  )
  const operationGate = useIdentityBoundOperationGate<'reload'>(client, [SUBSCRIPTIONS_CACHE_KEY])

  const reload = useCallback(
    async (quiet = false): Promise<boolean> => {
      const operation = operationGate.begin('reload', 'reload subscriptions')
      if (!operation) return false
      const result = await resource.reload({ silent: quiet })
      return Boolean(result?.ok && operationGate.isCurrent(operation))
    },
    [operationGate, resource],
  )

  useIdentityPolling(client, {
    enabled: canFetch,
    intervalMs: 60_000,
    logicalKey: 'poll subscriptions',
    ownerKey: SUBSCRIPTIONS_CACHE_KEY,
    onTick: () => { void reload(true) },
  })

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
      loading: canFetch && resource.loading,
      error: resource.error as ApiError | null,
      reload,
      patchSubscription,
      patchSubscriptionForIdentity,
    }),
    [
      resource.data,
      canFetch,
      resource.loading,
      resource.error,
      reload,
      patchSubscription,
      patchSubscriptionForIdentity,
    ],
  )
}
