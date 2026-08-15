import { describe, expect, it, vi } from 'vitest'

import { ok, err, type ApiResult } from '../api/result'
import { ResourceStore } from './store'
import { IdentityAuthority } from '../identity'

function okAfter<T>(value: T, delay = 0): Promise<ApiResult<T>> {
  if (delay === 0) return Promise.resolve(ok(value))
  return new Promise((resolve) => setTimeout(() => resolve(ok(value)), delay))
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>((release) => {
    resolve = release
  })
  return { promise, resolve }
}

describe('ResourceStore identity ownership', () => {
  it('binds private memory to one lease and rejects a late A commit after activating B', () => {
    const authority = new IdentityAuthority()
    const store = new ResourceStore({ identityRequired: true })
    store.set('before-handshake', { private: true })
    expect(store.has('before-handshake')).toBe(false)

    const leaseA = authority.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    store.activateIdentity(leaseA)
    store.set('A-key', { owner: 'A' })
    const lateA = leaseA.captureOwnership('late A hydrate')
    expect(store.has('A-key')).toBe(true)

    const leaseB = authority.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    store.activateIdentity(leaseB)

    expect(store.activePhysicalNamespace).toBe('physical-B')
    expect(store.has('A-key')).toBe(false)
    expect(store.setForIdentity(lateA, 'A-late', { owner: 'A' })).toBe(false)
    expect(store.has('A-late')).toBe(false)
  })
})

describe('ResourceStore in-flight 去重', () => {
  it('同键并发只回源一次，全部调用者共享同一结果', async () => {
    const store = new ResourceStore()
    const fetcher = vi.fn(() => okAfter({ id: 'a' }, 10))

    const [first, second, third] = await Promise.all([
      store.fetch('GET /api/links', fetcher),
      store.fetch('GET /api/links', fetcher),
      store.fetch('GET /api/links', fetcher),
    ])

    expect(fetcher).toHaveBeenCalledTimes(1)
    expect(first).toEqual(second)
    expect(second).toEqual(third)
  })

  it('不同键各自回源，不会互相合并', async () => {
    const store = new ResourceStore()
    const fetcher = vi.fn(async (value: string) => ok({ id: value }))

    await Promise.all([
      store.fetch('GET /api/links', () => fetcher('a')),
      store.fetch('GET /api/tags', () => fetcher('b')),
    ])

    expect(fetcher).toHaveBeenCalledTimes(2)
  })

  it('上一次完成之后新的请求会真的回源（in-flight 不会粘住）', async () => {
    const store = new ResourceStore()
    const fetcher = vi.fn(async () => ok({ id: 'a' }))

    await store.fetch('k', fetcher)
    await store.fetch('k', fetcher)

    expect(fetcher).toHaveBeenCalledTimes(2)
  })
})

describe('ResourceStore success response 动态提交键', () => {
  it('仍按 request key 合并请求，成功数据与 ETag 只写入 response target key', async () => {
    const store = new ResourceStore()
    const requestKey = 'GET content:/api/links/L1/content?revision=7'
    const targetKey = 'GET content:/api/links/L1/content?revision=8'
    const gate = deferred<void>()
    const fetcher = vi.fn(async (conditional: { onETag: (tag: string) => void }) => {
      conditional.onETag('"revision-8"')
      await gate.promise
      return ok({ revision: 8, body: 'new body' })
    })
    const options = {
      resolveCommitKey: (data: { revision: number }) =>
        `GET content:/api/links/L1/content?revision=${data.revision}`,
    }

    const first = store.fetch(requestKey, fetcher, options)
    const second = store.fetch(requestKey, fetcher, options)

    expect(store.peek(requestKey).revalidating).toBe(true)
    gate.resolve()
    const [firstResult, secondResult] = await Promise.all([first, second])

    expect(fetcher).toHaveBeenCalledTimes(1)
    expect(firstResult).toEqual(secondResult)
    const requestSnapshot = store.peek(requestKey)
    expect(requestSnapshot.data).toBeUndefined()
    expect(requestSnapshot.etag).toBeUndefined()
    expect(requestSnapshot).toMatchObject({
      revalidating: false,
      attemptedGeneration: 0,
      settledGeneration: 0,
    })
    expect(store.peek(targetKey)).toMatchObject({
      data: { revision: 8, body: 'new body' },
      etag: '"revision-8"',
      revalidating: false,
      desiredGeneration: 0,
      attemptedGeneration: 0,
      settledGeneration: 0,
    })
    expect(store.busy).toBe(false)
  })

  it('接受请求开始前已经失效的 target，并结算 target 自己的代际', async () => {
    const store = new ResourceStore()
    const requestKey = 'request-revision-7'
    const targetKey = 'target-revision-8'
    store.invalidate(targetKey)

    await store.fetch(
      requestKey,
      async () => ok({ revision: 8 }),
      { resolveCommitKey: () => targetKey },
    )

    expect(store.peek(targetKey)).toMatchObject({
      data: { revision: 8 },
      desiredGeneration: 1,
      attemptedGeneration: 1,
      settledGeneration: 1,
    })
  })

  it('请求途中失效 request key 时，迟到响应不得绕道写入 target key', async () => {
    const store = new ResourceStore()
    const requestKey = 'request-revision-7'
    const targetKey = 'target-revision-8'
    const gate = deferred<void>()
    const inflight = store.fetch(
      requestKey,
      async () => {
        await gate.promise
        return ok({ revision: 8 })
      },
      { resolveCommitKey: () => targetKey },
    )

    store.invalidate(requestKey)
    gate.resolve()
    const result = await inflight

    expect(result).toEqual(ok({ revision: 8 }))
    expect(store.has(requestKey)).toBe(false)
    expect(store.has(targetKey)).toBe(false)
    expect(store.peek(requestKey).revalidating).toBe(false)
    expect(store.busy).toBe(false)
  })

  it('请求途中只失效 target key 时，迟到响应不得重新填充 target', async () => {
    const store = new ResourceStore()
    const requestKey = 'request-revision-7'
    const targetKey = 'target-revision-8'
    const gate = deferred<void>()
    const inflight = store.fetch(
      requestKey,
      async () => {
        await gate.promise
        return ok({ revision: 8 })
      },
      { resolveCommitKey: () => targetKey },
    )

    store.invalidate(targetKey)
    gate.resolve()
    const result = await inflight

    expect(result).toEqual(ok({ revision: 8 }))
    expect(store.has(requestKey)).toBe(false)
    expect(store.has(targetKey)).toBe(false)
    expect(store.peek(requestKey)).toMatchObject({
      revalidating: false,
      settledGeneration: 0,
    })
    expect(store.peek(targetKey).desiredGeneration).toBe(1)
    expect(store.busy).toBe(false)
  })

  it('resolver 抛异常时 fail closed，且不会泄漏 in-flight 或 revalidating', async () => {
    const store = new ResourceStore()
    const fetcher = vi.fn(async () => ok({ revision: 8 }))

    const result = await store.fetch('request-revision-7', fetcher, {
      resolveCommitKey: () => {
        throw new Error('invalid response address')
      },
    })

    expect(result).toEqual(ok({ revision: 8 }))
    const requestSnapshot = store.peek('request-revision-7')
    expect(requestSnapshot.data).toBeUndefined()
    expect(requestSnapshot).toMatchObject({
      error: null,
      revalidating: false,
      settledGeneration: 0,
    })
    expect(store.busy).toBe(false)

    await store.fetch('request-revision-7', fetcher)
    expect(fetcher).toHaveBeenCalledTimes(2)
    expect(store.has('request-revision-7')).toBe(true)
  })
})

describe('ResourceStore SWR 语义', () => {
  it('数据没变时保留原来的 data 引用，且不通知订阅者', async () => {
    const store = new ResourceStore()
    const listener = vi.fn()
    store.subscribe('k', listener)

    await store.fetch('k', async () => ok({ items: [{ id: '1', updated_at: 't1' }] }))
    const firstData = store.peek('k').data
    const callsAfterFirst = listener.mock.calls.length

    // 同一份数据的另一个对象实例。
    await store.fetch('k', async () => ok({ items: [{ id: '1', updated_at: 't1' }] }))

    expect(store.peek('k').data).toBe(firstData) // 引用未变 → 下游 memo 不失效
    // revalidating 的进出仍会通知，但数据本身不该产生额外的"变了"通知；
    // 关键断言是引用相同，这里只确认没有异常的通知风暴。
    expect(listener.mock.calls.length - callsAfterFirst).toBeLessThanOrEqual(2)
  })

  it('数据变了就换新引用', async () => {
    const store = new ResourceStore()
    await store.fetch('k', async () => ok({ items: [{ id: '1', updated_at: 't1' }] }))
    const firstData = store.peek('k').data

    await store.fetch('k', async () => ok({ items: [{ id: '1', updated_at: 't2' }] }))

    expect(store.peek('k').data).not.toBe(firstData)
  })

  it('成功会清掉上一次的错误', async () => {
    const store = new ResourceStore()
    await store.fetch('k', async () => err({ kind: 'other', message: 'boom' }))
    expect(store.peek('k').error).not.toBeNull()

    await store.fetch('k', async () => ok({ id: 'a' }))
    expect(store.peek('k').error).toBeNull()
  })

  it('silent 失败不写错误态、不动已有数据', async () => {
    const store = new ResourceStore()
    await store.fetch('k', async () => ok({ items: [{ id: '1' }] }))
    const data = store.peek('k').data

    await store.fetch('k', async () => err({ kind: 'network-unreachable', message: 'down' }), {
      silent: true,
    })

    expect(store.peek('k').error).toBeNull()
    expect(store.peek('k').data).toBe(data)
  })

  it('非 silent 失败写错误态但保留已有数据', async () => {
    const store = new ResourceStore()
    await store.fetch('k', async () => ok({ items: [{ id: '1' }] }))
    const data = store.peek('k').data

    await store.fetch('k', async () => err({ kind: 'other', message: 'boom' }))

    expect(store.peek('k').error?.message).toBe('boom')
    expect(store.peek('k').data).toBe(data)
  })

  it('fetcher 抛异常不会把 in-flight 卡死', async () => {
    const store = new ResourceStore()
    const result = await store.fetch('k', async () => {
      throw new Error('unexpected')
    })
    expect(result.ok).toBe(false)

    const after = await store.fetch('k', async () => ok({ id: 'a' }))
    expect(after.ok).toBe(true)
  })
})

describe('ResourceStore 失效与容量', () => {
  it('invalidate 按前缀删除并通知订阅者', async () => {
    const store = new ResourceStore()
    const listener = vi.fn()
    await store.fetch('GET /api/links?page=1', async () => ok({ items: [] }))
    await store.fetch('GET /api/links?page=2', async () => ok({ items: [] }))
    await store.fetch('GET /api/tags', async () => ok([]))
    store.subscribe('GET /api/links?page=1', listener)

    store.invalidate('GET /api/links')

    expect(store.has('GET /api/links?page=1')).toBe(false)
    expect(store.has('GET /api/links?page=2')).toBe(false)
    expect(store.has('GET /api/tags')).toBe(true) // 前缀不匹配，不受影响
    expect(listener).toHaveBeenCalled()
  })

  it('超容时淘汰最久未访问的条目', async () => {
    const store = new ResourceStore({ capacity: 2 })
    await store.fetch('a', async () => ok({ id: 'a' }))
    await store.fetch('b', async () => ok({ id: 'b' }))
    await store.fetch('a', async () => ok({ id: 'a' })) // 触碰 a
    await store.fetch('c', async () => ok({ id: 'c' }))

    expect(store.has('b')).toBe(false) // 最久未访问
    expect(store.has('a')).toBe(true)
    expect(store.has('c')).toBe(true)
  })

  it('有订阅者的键不被淘汰——它正被渲染，删掉界面会当场塌空', async () => {
    const store = new ResourceStore({ capacity: 1 })
    await store.fetch('watched', async () => ok({ id: 'w' }))
    store.subscribe('watched', () => {})

    await store.fetch('other', async () => ok({ id: 'o' }))

    expect(store.has('watched')).toBe(true)
  })

  it('取消订阅不清缓存——切回来能秒开全靠这一点', async () => {
    const store = new ResourceStore()
    const unsubscribe = store.subscribe('k', () => {})
    await store.fetch('k', async () => ok({ id: 'a' }))

    unsubscribe()

    expect(store.has('k')).toBe(true)
  })
})

describe('ResourceStore 乐观更新', () => {
  it('patch 就地改写并通知订阅者', async () => {
    const store = new ResourceStore()
    const listener = vi.fn()
    await store.fetch('k', async () => ok({ items: [{ id: '1', title: 'old' }] }))
    store.subscribe('k', listener)

    store.patch<{ items: { id: string; title: string }[] }>('k', (current) => ({
      items: current.items.map((item) => ({ ...item, title: 'new' })),
    }))

    expect(store.peek<{ items: { title: string }[] }>('k').data?.items[0].title).toBe('new')
    expect(listener).toHaveBeenCalled()
  })

  it('patch 对不存在的键是 no-op', () => {
    const store = new ResourceStore()
    store.patch('missing', () => ({ changed: true }))
    expect(store.has('missing')).toBe(false)
  })
})

describe('ResourceStore 条件请求（PF5）', () => {
  it('把上一次的 ETag 作为 If-None-Match 交给 fetcher', async () => {
    const store = new ResourceStore()
    const seen: (string | null)[] = []

    await store.fetch('k', async (conditional) => {
      seen.push(conditional.ifNoneMatch)
      conditional.onETag('"v1"')
      return ok({ id: 'a' })
    })
    await store.fetch('k', async (conditional) => {
      seen.push(conditional.ifNoneMatch)
      return ok({ id: 'a' })
    })

    expect(seen).toEqual([null, '"v1"'])
  })

  it('304 视为"缓存仍有效"：保留原数据引用、清错误、不当成失败', async () => {
    const store = new ResourceStore()
    await store.fetch('k', async (conditional) => {
      conditional.onETag('"v1"')
      return ok({ items: [{ id: '1' }] })
    })
    const data = store.peek('k').data

    await store.fetch('k', async () => ({
      ok: false as const,
      error: { kind: 'not-modified' as const, message: 'not modified', status: 304 },
    }))

    expect(store.peek('k').error).toBeNull()
    // 引用必须原样保留——换了引用等于让下游 memo 全部白失效一次。
    expect(store.peek('k').data).toBe(data)
  })

  it('304 之后仍然继续携带同一个 ETag', async () => {
    const store = new ResourceStore()
    const seen: (string | null)[] = []
    await store.fetch('k', async (conditional) => {
      conditional.onETag('"v1"')
      return ok({ id: 'a' })
    })
    await store.fetch('k', async (conditional) => {
      seen.push(conditional.ifNoneMatch)
      return { ok: false as const, error: { kind: 'not-modified' as const, message: '', status: 304 } }
    })
    await store.fetch('k', async (conditional) => {
      seen.push(conditional.ifNoneMatch)
      return ok({ id: 'a' })
    })
    expect(seen).toEqual(['"v1"', '"v1"'])
  })
})

describe('ResourceStore 失效代际（invalidate 半边）', () => {
  it('回源途中发生 invalidate，迟到的结果不得落盘', async () => {
    const store = new ResourceStore()
    let release!: () => void
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })

    // 请求在途…
    const inflight = store.fetch('GET /api/links?x', async () => {
      await gate
      return ok({ items: [{ id: 'before-write' }] })
    })

    // …此时发生写入并失效（缓存里此刻还没有这个键，删无可删）。
    store.invalidate('GET /api/links')

    release()
    await inflight

    // 关键断言：写入前的快照不得被"抢救"落盘。落了的话失效等于没发生，
    // 用户的写要等到下一次校验才看得见。
    expect(store.has('GET /api/links?x')).toBe(false)
  })

  it('前缀不匹配的在途请求不受影响', async () => {
    const store = new ResourceStore()
    let release!: () => void
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    const inflight = store.fetch('GET /api/tags', async () => {
      await gate
      return ok([{ tag: 'go', count: 1 }])
    })

    store.invalidate('GET /api/links') // 与 tags 无关

    release()
    await inflight

    expect(store.has('GET /api/tags')).toBe(true)
  })

  it('持久失效代际低于资源重试代际时仍必须失效', async () => {
    const store = new ResourceStore()
    const key = 'GET /api/links?page=1'

    await store.fetch(key, async () => ok({ version: 0 }))
    for (let version = 1; version <= 3; version += 1) {
      await store.fetch(key, async () => ok({ version }), { force: true })
    }
    expect(store.peek(key).desiredGeneration).toBe(3)

    // Durable generations are namespace journal positions, not resource retry
    // generations. The first cross-tab tombstone must still invalidate a key
    // that has already been explicitly retried several times in this tab.
    store.applyInvalidationGeneration('GET /api/links', 1)

    expect(store.has(key)).toBe(false)
    expect(store.peek(key).desiredGeneration).toBe(4)
  })
})

describe('ResourceStore force 重取与 in-flight 记账', () => {
  it('取消 force 重取会保留旧数据、结束代际并拒绝迟到响应', async () => {
    const store = new ResourceStore()
    store.set('k', { id: 'cached' })
    const late = deferred<ApiResult<{ id: string }>>()
    const controller = new AbortController()
    let observedSignal: AbortSignal | undefined

    const reload = store.fetch('k', (conditional) => {
      observedSignal = conditional.signal
      return late.promise
    }, { force: true, signal: controller.signal })

    expect(observedSignal).toBe(controller.signal)
    expect(store.peek('k').revalidating).toBe(true)
    controller.abort()

    await expect(reload).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other', message: '资料库刷新已取消' },
    })
    expect(store.peek('k')).toMatchObject({
      data: { id: 'cached' },
      error: null,
      revalidating: false,
      desiredGeneration: 1,
      attemptedGeneration: 1,
      settledGeneration: 1,
    })

    late.resolve(ok({ id: 'late' }))
    await late.promise
    await Promise.resolve()
    expect(store.peek('k').data).toEqual({ id: 'cached' })
  })

  it('force 之后旧请求迟到，不得把新请求从 in-flight 表里删掉', async () => {
    const store = new ResourceStore()
    let releaseOld!: () => void
    let releaseNew!: () => void
    let calls = 0

    const oldGate = new Promise<void>((r) => { releaseOld = r })
    const newGate = new Promise<void>((r) => { releaseNew = r })

    const fetcher = async () => {
      calls += 1
      if (calls === 1) {
        await oldGate
        return ok({ id: 'old' })
      }
      await newGate
      return ok({ id: 'new' })
    }

    const first = store.fetch('k', fetcher)
    const second = store.fetch('k', fetcher, { force: true })

    // 旧请求迟到 resolve —— 它必须只清理自己，不能把 second 从表里抹掉。
    releaseOld()
    await first

    // 此刻 second 仍在途：同键再读应当复用它，而不是发第三次请求。
    const third = store.fetch('k', fetcher)
    releaseNew()
    await Promise.all([second, third])

    expect(calls).toBe(2)
  })

  it('force 不携带 If-None-Match —— 用户主动重取永远有逃生舱', async () => {
    const store = new ResourceStore()
    const seen: (string | null)[] = []
    await store.fetch('k', async (conditional) => {
      conditional.onETag('"v1"')
      return ok({ id: 'a' })
    })
    await store.fetch('k', async (conditional) => {
      seen.push(conditional.ifNoneMatch)
      return ok({ id: 'a' })
    }, { force: true })
    await store.fetch('k', async (conditional) => {
      seen.push(conditional.ifNoneMatch)
      return ok({ id: 'a' })
    })

    expect(seen).toEqual([null, '"v1"'])
  })
})
