/**
 * 已保存原文的读侧：订阅缓存的 `useCachedLinkContent` + 按需取的 `loadLinkContent`。
 *
 * 独立成模块而不是留在组件里，理由是可测性：这两者**必须算出同一个键**，而这条
 * 不变量原先只能靠组件测试间接观察——组件测试里的回调是测试自己写的闭包，钉不住
 * 真实实现。放在一起、直接对它们写用例，键算歪了当场就红。
 *
 * 键的形状、以及「为什么它不该被库级失效连坐」，见 ./keys.ts。
 */
import { useCallback, useSyncExternalStore } from 'react'

import type { ReaderClient } from '../api/client'
import { ok, type ApiResult } from '@webtag/api'
import type { LinkContentResponse } from '../api/types'
import { contentCacheKey } from './keys'
import { resourceStore } from './store'

type LinkContentClient = Pick<ReaderClient, 'captureIdentity' | 'getContent'>

const normalizedBodyCache = new WeakMap<object, LinkContentResponse>()

/** Old persisted body entries predate content_source; normalize only at cache boundaries. */
export function normalizeCachedLinkContent(
  value: LinkContentResponse | null,
): LinkContentResponse | null {
  if (!value || value.content_source === 'user') return value
  if (value.content_source === 'fetched') return value
  const previous = normalizedBodyCache.get(value)
  if (previous) return previous
  const normalized = { ...value, content_source: 'fetched' as const }
  normalizedBodyCache.set(value, normalized)
  return normalized
}

/**
 * 订阅缓存里的已保存原文。渲染正文的组件用这个，**不要用裸 peek**。
 *
 * 订阅在这里承担三件事，缺一件都会出事：
 *
 * 1. **免于 LRU 淘汰。** store 的淘汰只豁免「有订阅者」的键（store.ts 的
 *    evictIfNeeded），而 `peek()` 又刻意不刷新 recency——一条只被 peek 读的正文
 *    会越读越靠近淘汰队头。正文是最大最多的那批条目，落盘之后 hydrate 还会把
 *    历史正文一起灌回同一个 200 条上限里，读上几十篇就会越界。淘汰发生在原文
 *    正展开着的时候，界面会当场塌成**完全空白**——没有正文、没有加载态、也没有
 *    错误块，因为渲染分支只认「有正文 / 没正文」。
 * 2. **条目后到时能重渲染。** 裸 peek 是渲染期读一个可变单例，写进来的数据不会
 *    通知任何人。
 * 3. 顺带把「渲染期读外部可变状态」这件事收回 React 的并发安全模型里。
 *
 * revision 必须由**正在渲染那条链接的地方**传进来，不能在这里去链接列表反查：
 * 当前文章经常不在当前筛选的列表里（换个筛选它就掉出去了，而详情按 Mail 原则
 * 不跟着变）。反查落空会静默退化成 rev=0，读写各算各的键，缓存等于没有——这正是
 * 用户报的那个 bug 的原始形态。
 *
 * linkId 为 null（未选中文章）时不订阅、不读。
 */
export function useCachedLinkContent(
  linkId: string | null | undefined,
  revision: number | undefined,
): LinkContentResponse | null {
  const key = linkId ? contentCacheKey(linkId, revision) : null
  const subscribe = useCallback(
    (listener: () => void) => (key ? resourceStore.subscribe(key, listener) : () => {}),
    [key],
  )
  // peek 返回的 data 在条目未变时是同一个引用，`?? null` 也稳定——useSyncExternalStore
  // 要求 getSnapshot 幂等，这一点必须保持。
  const getSnapshot = useCallback(
    () => (key
      ? normalizeCachedLinkContent(resourceStore.peek<LinkContentResponse>(key).data ?? null)
      : null),
    [key],
  )
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

/**
 * 按需取已保存原文：命中缓存**就地返回，不发校验请求**。
 *
 * 这是全仓唯一一处显式跳过 SWR 校验的读。不这么做的话，`store.fetch` 有缓存也
 * 一定回源、返回的就是那趟网络的 promise，于是每次展开原文仍要等一个往返才
 * resolve——缓存省下了数据，却没省下等待，用户看到的还是「又在加载」。
 *
 * 代价要说清楚：这条键此后永不做后台校验。「不会读到过期正文」因此**不是**由这里
 * 保证的，而是由键本身保证——后端每次写正文都递增 content_revision，所以内容变了
 * 就是另一个键，旧内容读不到（见 invalidate.ts 的 contentCacheKey，含指向后端守卫
 * 测试的指针）。`invalidateLink` 只负责抹平「写完到列表校验拿到新 revision」之间
 * 那段本机窗口。
 *
 * 要把这个写法照抄到别的键之前，先确认那个键也有等价的保证——**别只看有没有显式
 * 失效**。显式失效只在执行写操作的那个客户端实例里跑，跨设备够不着；这条键敢跳过
 * 校验，靠的是后端递增 revision 让键自己分叉。这两者不是一回事，历史上正是把它们
 * 混为一谈才让另一台设备永久读到了替换前的正文。
 */
export async function loadLinkContent(
  client: LinkContentClient,
  linkId: string,
  revision: number | undefined,
): Promise<ApiResult<LinkContentResponse>> {
  const ownership = client.captureIdentity(
    `load saved content ${linkId}@${revision ?? 'unknown'}`,
  )
  if (!ownership) {
    return {
      ok: false,
      error: { kind: 'identity-mismatch', message: 'Reader identity is not active' },
    }
  }
  if (
    !ownership.lease.isOwnershipCurrent(ownership) ||
    !resourceStore.isIdentityActive(ownership.lease)
  ) {
    return {
      ok: false,
      error: { kind: 'identity-mismatch', message: 'Reader cache belongs to another identity' },
    }
  }

  const key = contentCacheKey(linkId, revision)
  const cached = normalizeCachedLinkContent(resourceStore.peek<LinkContentResponse>(key).data ?? null)
  if (cached) return ok(cached)

  const result = await resourceStore.fetch<LinkContentResponse>(
    key,
    async () => {
      const response = await client.getContent(linkId)
      if (!response.ok) return response
      if (
        response.data.link_id !== linkId ||
        !Number.isSafeInteger(response.data.content_revision) ||
        response.data.content_revision < 0
      ) {
        return {
          ok: false,
          error: { kind: 'other', message: '已保存原文响应缺少可信 revision' },
        }
      }
      return response
    },
    {
      resolveCommitKey: (response) => contentCacheKey(linkId, response.content_revision),
    },
  )

  if (
    !ownership.lease.isOwnershipCurrent(ownership) ||
    !resourceStore.isIdentityActive(ownership.lease)
  ) {
    return {
      ok: false,
      error: { kind: 'identity-mismatch', message: '已保存原文响应属于过期身份' },
    }
  }
  return result
}
