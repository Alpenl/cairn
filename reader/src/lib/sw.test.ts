/**
 * PF10：Service Worker 的注销通道。
 *
 * 这是整个专项里唯一「出错后远程修不回来」的东西——一个坏版本的 SW 会把用户
 * 钉死在旧代码上，而修复本身也要靠那个坏 SW 放行才能送达。因此注销通道必须
 * **不依赖 SW 本身**能正常工作，且必须有测试守住。
 */
import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it, vi } from 'vitest'

import { readerIdentity } from './identity'
import {
  getLegacyImportPrompt,
  quarantineLegacyData,
  resetUserDataDatabaseHandle,
} from './legacy-user-data'
import { ownedDatabaseName } from './storage-ownership'
import { registerServiceWorker, resetApplicationCache } from './sw'
import { readerCacheIDForBase, readerScopeURL } from './sw-ownership'

async function deleteUserDataDatabase(): Promise<void> {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => resolve()
    request.onblocked = () => resolve()
  })
}

afterEach(async () => {
  vi.unstubAllGlobals()
  await deleteUserDataDatabase()
})

describe('registerServiceWorker 的注册时机', () => {
  function stubNavigator() {
    const register = vi.fn(async () => ({
      waiting: null,
      installing: null,
      addEventListener: vi.fn(),
    }))
    vi.stubGlobal('navigator', { ...navigator, serviceWorker: { register } })
    return register
  }

  // ⚠️ 守住「SW 从不注册」这个 bug 的**只有这一条**。下面那条守的是 else
  // 分支，把修复改回无条件 addEventListener 它照样绿（隔离跑已验证）——
  // 别把它当成第二道回归防线。
  it('文档已经 load 完之后才调用，仍然要注册', () => {
    // 调用方在 load 之后才调到这里时的时序。旧实现无条件
    // addEventListener('load')，于是 SW 一次都没注册成功过。
    vi.spyOn(document, 'readyState', 'get').mockReturnValue('complete')
    const register = stubNavigator()

    // 不 await：complete 分支必须**同步**就把 register 调出去。
    registerServiceWorker(() => {})

    expect(register).toHaveBeenCalledTimes(1)
  })

  it('文档还在加载时挂到 load 上，等 load 再注册（守 else 分支，非本 bug 的防线）', async () => {
    vi.spyOn(document, 'readyState', 'get').mockReturnValue('loading')
    const register = stubNavigator()

    registerServiceWorker(() => {})
    expect(register).not.toHaveBeenCalled()

    window.dispatchEvent(new Event('load'))
    await Promise.resolve()

    expect(register).toHaveBeenCalledTimes(1)
  })
})

describe('resetApplicationCache', () => {
  it('只注销当前 Reader scope 并删除当前 base 的新旧 shell cache', async () => {
    const readerUnregister = vi.fn(async () => true)
    const otherUnregister = vi.fn(async () => true)
    const cacheDelete = vi.fn(async () => true)
    const origin = window.location.origin
    const scope = readerScopeURL('/reader/', origin)
    const cacheID = readerCacheIDForBase('/reader/')
    vi.stubGlobal('navigator', {
      ...navigator,
      serviceWorker: { getRegistrations: async () => [
        { scope, unregister: readerUnregister },
        { scope: `${origin}/other-app/`, unregister: otherUnregister },
      ] },
    })
    vi.stubGlobal('caches', {
      keys: async () => [
        `${cacheID}-precache-v2-${scope}`,
        `workbox-precache-v2-${scope}`,
        `${readerCacheIDForBase('/other-reader/')}-precache-v2-${origin}/other-reader/`,
        'other-app-shell-v1',
      ],
      delete: cacheDelete,
    })

    await resetApplicationCache()

    expect(readerUnregister).toHaveBeenCalledTimes(1)
    expect(otherUnregister).not.toHaveBeenCalled()
    expect(cacheDelete).toHaveBeenCalledTimes(2)
    expect(cacheDelete).toHaveBeenCalledWith(`${cacheID}-precache-v2-${scope}`)
    expect(cacheDelete).toHaveBeenCalledWith(`workbox-precache-v2-${scope}`)
  })

  it('注销失败也要继续清缓存 —— 自救手段不能被自己卡住', async () => {
    const cacheDelete = vi.fn(async () => true)
    vi.stubGlobal('navigator', {
      ...navigator,
      serviceWorker: {
        getRegistrations: async () => {
          throw new Error('SW 坏了')
        },
      },
    })
    const cacheID = readerCacheIDForBase('/reader/')
    vi.stubGlobal('caches', { keys: async () => [`${cacheID}-precache-v2-current`], delete: cacheDelete })

    await expect(resetApplicationCache()).resolves.toBeUndefined()
    expect(cacheDelete).toHaveBeenCalledTimes(1)
  })

  it('浏览器不支持 SW / Cache Storage 时不抛异常', async () => {
    vi.stubGlobal('navigator', {})
    vi.stubGlobal('caches', undefined)

    await expect(resetApplicationCache()).resolves.toBeUndefined()
  })

  it('只清理可重建缓存，不删除隔离中的 installation user data', async () => {
    const lease = readerIdentity.activeLease
    if (!lease) throw new Error('test identity is not installed')
    localStorage.setItem('webtag:pins:v1', '{"tags":["legacy"],"domains":[]}')
    await quarantineLegacyData()

    expect(await getLegacyImportPrompt(lease)).toEqual({
      assetCount: 1,
      sources: ['pins'],
    })

    await resetApplicationCache()
    resetUserDataDatabaseHandle()

    expect(await getLegacyImportPrompt(lease)).toEqual({
      assetCount: 1,
      sources: ['pins'],
    })
  })
})
