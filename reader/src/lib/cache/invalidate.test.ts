/**
 * 失效范围的守卫。
 *
 * 用户报的那条——「原文好像没缓存，切走再回来又要重新请求」——有两半。一半是
 * 读路径不肯用缓存；另一半在这里：**写路径把缓存扫掉了**。已保存原文曾经挂在
 * `GET /api/links/{id}/content` 这个键上，而 invalidateLibrary 是按 `GET /api/links`
 * 前缀失效的，于是「添加一条链接」这种日常动作会把全库每一篇读过的正文一起清空。
 */
import { beforeEach, describe, expect, it } from 'vitest'

import {
  ANNOTATED_LINKS_CACHE_KEY,
  linkDetailCacheKey,
} from '../../hooks/useAnnotatedLinks'
import { translationsKey } from '../../hooks/useTranslations'
import { ok } from '@webtag/api'
import {
  contentCacheKey,
  invalidateLibrary,
  invalidateLink,
  invalidateLinkContent,
  invalidateLinkProjection,
} from './invalidate'
import { resourceStore } from './store'

const LINKS_KEY = 'GET /api/links?limit=30'

/** 把一篇正文塞进缓存，返回它的键。 */
async function cacheContent(id: string, revision = 1): Promise<string> {
  const key = contentCacheKey(id, revision)
  await resourceStore.fetch(key, async () => ok({ link_id: id, content: `${id} 的正文` }))
  return key
}

/**
 * 把一条链接的译文塞进缓存，返回它的键。
 *
 * 键走 translationsKey 而不是在这里拼字符串——测试自己拼的话，生产端改了键的
 * 形状，测试会继续对着一个不存在的键做断言，全绿而守卫已经失效。
 */
async function cacheTranslations(id: string): Promise<string> {
  const key = translationsKey(id)
  await resourceStore.fetch(key, async () => ok({ items: [] }))
  return key
}

beforeEach(() => {
  resourceStore.clear()
})

describe('失效范围：库级写操作不碰已保存原文', () => {
  it('invalidateLibrary 打掉列表，但保留每一篇的正文', async () => {
    await resourceStore.fetch(LINKS_KEY, async () => ok({ items: [], total: 0 }))
    const a = await cacheContent('LA')
    const b = await cacheContent('LB')

    invalidateLibrary()

    expect(resourceStore.has(LINKS_KEY)).toBe(false)
    expect(resourceStore.has(a)).toBe(true)
    expect(resourceStore.has(b)).toBe(true)
  })

  it('invalidateLink 只回收这一条的正文，不牵连别的文章', async () => {
    const a = await cacheContent('LA')
    const b = await cacheContent('LB')

    invalidateLink('LA')

    expect(resourceStore.has(a)).toBe(false)
    expect(resourceStore.has(b)).toBe(true)
  })

  it('invalidateLink 打掉这一条的译文，且同样不牵连前缀相近的另一条', async () => {
    // 译文那一行原先零覆盖——整行删掉全仓 408 条测试照样全绿。而它是保存 / 替换
    // 正文、重新解析之后**唯一**打掉译文缓存的地方：静默失效的后果是用户拿旧译文
    // 对着换过的原文。（写这条时 invalidate 还不触发重取、连自愈通路都没有；
    // 自愈现已补上，但「该失效的没被失效」它救不了。）
    const mine = await cacheTranslations('L1')
    const other = await cacheTranslations('L12')

    invalidateLink('L1')

    expect(resourceStore.has(mine)).toBe(false)
    expect(resourceStore.has(other)).toBe(true)
  })

  it('invalidateLinkProjection 精确清除 point cache 并唤醒 annotated 投影', () => {
    const mine = linkDetailCacheKey('L1')
    const other = linkDetailCacheKey('L12')
    resourceStore.set(mine, { id: 'L1' })
    resourceStore.set(other, { id: 'L12' })
    let wakeups = 0
    const unsubscribe = resourceStore.subscribe(
      ANNOTATED_LINKS_CACHE_KEY,
      () => { wakeups += 1 },
    )

    try {
      invalidateLinkProjection('L1')
    } finally {
      unsubscribe()
    }

    expect(resourceStore.has(mine)).toBe(false)
    expect(resourceStore.has(other)).toBe(true)
    expect(wakeups).toBe(1)
  })

  it('invalidateLinkContent 只回收正文并保留 revision 分区的译文', async () => {
    const content = await cacheContent('L1', 7)
    const translations = await cacheTranslations('L1')

    invalidateLinkContent('L1')

    expect(resourceStore.has(content)).toBe(false)
    expect(resourceStore.has(translations)).toBe(true)
  })

  it('invalidateLink 不牵连 id 以它为前缀的另一条', async () => {
    // 上面那条用 LA / LB，两者互不为前缀，所以它抓不到「少一个尾随分隔符」这类
    // 缺陷。线上 id 是定长 UUID，同样天然互不为前缀——正确性因此挂在调用方的数据
    // 形状上，换个 id 方案就静默失效。这条直接钉住失效函数自身。
    const short = await cacheContent('L1')
    const longer = await cacheContent('L12')

    invalidateLink('L1')

    expect(resourceStore.has(short)).toBe(false)
    expect(resourceStore.has(longer)).toBe(true)
  })

  it('正文键与链接详情键彼此不构成前缀关系', () => {
    // 这条断言是上面两条的根据：只要正文键落在 `GET /api/links` 之下，
    // 任何按库前缀的失效都会连坐。
    expect(contentCacheKey('LA', 1).startsWith('GET /api/links')).toBe(false)
  })

  it('invalidateLibrary 打掉列表，但保留每一篇的译文', async () => {
    // 译文曾经住在 `GET /api/links/{id}/translations`，而 LINKS_CACHE_PREFIX 是
    // `GET /api/links`（无尾随分隔符）——于是「添加一条链接」就会清空全库译文。
    // 而译文条目被删的后果**当时**不是「多取一次」：它会让 useCachedResource 停在
    // 永久 loading，已译好的内容从界面消失，hasActiveJobs 从缓存推导，进行中的
    // 翻译任务轮询也一并停掉。那条永久 loading 现已由自愈兜住，但这条守卫仍然
    // 必要——被打掉再自愈是一次白白的全库重取。
    await resourceStore.fetch(LINKS_KEY, async () => ok({ items: [], total: 0 }))
    const a = await cacheTranslations('LA')
    const b = await cacheTranslations('LB')

    invalidateLibrary()

    expect(resourceStore.has(LINKS_KEY)).toBe(false)
    expect(resourceStore.has(a)).toBe(true)
    expect(resourceStore.has(b)).toBe(true)
  })

  it('译文键与链接列表键彼此不构成前缀关系', () => {
    expect(translationsKey('LA').startsWith('GET /api/links')).toBe(false)
  })
})
