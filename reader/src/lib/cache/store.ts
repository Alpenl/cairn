/**
 * Reader 的请求缓存内核（PF3）。
 *
 * # 它解决的问题
 *
 * 改造前七个数据 hook 各自 `useState + useEffect` 直打 fetch，组件卸载即丢、
 * 刷新即丢。后果是：切到订阅页再切回来要重拉一遍；文章 A→B→A 每次都重新请求
 * 详情与译文；F5 之后要等 5 个 API 串完才有首屏。
 *
 * # 语义：stale-while-revalidate
 *
 *   · 有缓存 → **立即**用缓存渲染，`loading` 保持 false（不闪加载态）；
 *   · 无论有没有缓存，都发起一次后台校验；
 *   · 校验回来的数据与缓存**实际不同**时才通知订阅者（见 equal.ts）——
 *     绝大多数校验拿回来的是一样的东西，照样 setState 等于每 30 秒把整棵树
 *     白重渲染一次；
 *   · 组件卸载只解绑订阅者，缓存条目保留。
 *
 * # 为什么自研而不是 react-query / swr
 *
 * Reader 的运行时依赖只有 react / react-dom / react-markdown / remark-gfm 四个，
 * 这个约束是刻意维持的。所需语义（SWR + in-flight 去重 + 前缀失效 + LRU）
 * 加起来就是这个文件，不值得为它引入一棵依赖树。
 */
import type { ApiError, ApiResult } from '../api/result'
import type { IdentityLease, IdentityOwnership } from '../identity'
import { sameResource } from './equal'

/**
 * 交给 fetcher 的条件请求上下文。
 *
 * fetcher 负责把 ifNoneMatch 发出去、并在拿到 200 时回调 onETag。store 不认识
 * HTTP，只负责保管这一小段状态。
 */
export interface ConditionalContext {
  ifNoneMatch: string | null
  onETag: (tag: string | null) => void
  /** Optional caller cancellation propagated to the concrete HTTP request. */
  signal?: AbortSignal
}

/** 缓存条目。**不可变**：任何变化都换一个新对象，好让 useSyncExternalStore 直接比引用。 */
export interface ResourceSnapshot<T = unknown> {
  /** 上一次成功的数据。undefined 表示从未成功过。 */
  readonly data?: T
  /** 上一次失败的错误。成功后清空。 */
  readonly error: ApiError | null
  /** data 最后一次被写入 / 确认的时刻（毫秒）。 */
  readonly updatedAt: number
  /** 是否有请求在途。 */
  readonly revalidating: boolean
  /** 上一次 200 响应带回的 ETag，下一次校验作为 If-None-Match 发出。 */
  readonly etag?: string | null
  /** The newest representation generation consumers want to observe. */
  readonly desiredGeneration: number
  /** The newest generation for which a request has been started. */
  readonly attemptedGeneration: number
  /** The newest generation whose request reached a terminal result. */
  readonly settledGeneration: number
}

/** Backward-compatible name used by persistence and callers while RF4 lands. */
export type CacheEntry<T = unknown> = ResourceSnapshot<T>

const EMPTY_ENTRY: ResourceSnapshot = {
  error: null,
  updatedAt: 0,
  revalidating: false,
  desiredGeneration: 0,
  attemptedGeneration: -1,
  settledGeneration: -1,
}

/** 缓存条目数上限。超出按最久未访问淘汰。 */
export const DEFAULT_CAPACITY = 200

function cancelledResult<T>(): ApiResult<T> {
  return {
    ok: false,
    error: { kind: 'other', message: '资料库刷新已取消' },
  }
}

export interface FetchOptions<T = unknown> {
  /**
   * 静默校验：失败时**不写入 error**，保留已有数据与既有错误态。
   *
   * 后台刷新专用。没有它的话一次网络抖动会把用户正读着的列表换成错误块——
   * 用户什么都没做。这条语义是从 useLinks 里原样继承的，那里的注释记录了
   * 两次真实事故。
   */
  silent?: boolean
  /** 覆盖默认的相等判定（决定「数据变没变」）。 */
  equal?: (left: unknown, right: unknown) => boolean
  /**
   * 强制重新回源：绕过 in-flight 合并，并作废那个在途请求的结果。
   *
   * 用户主动触发的重取（点同步、错误重试）用它。语义差别是实打实的：一个卡住
   * 的请求正在途中时，用户点同步的期待是「再试一次」，而不是「陪着那次一起等」。
   * 后台校验则相反——合并才是对的，不该把定时器变成流量放大器。
   */
  force?: boolean
  /** Cancel this caller's refresh without replacing renderable cached state. */
  signal?: AbortSignal
  /** Generation selected by the revalidation coordinator. */
  generation?: number
  /**
   * 成功响应应落入的缓存键。省略时仍写入请求键。
   *
   * 某些端点会把比请求更新的权威版本带回（例如请求正文 revision 7，响应已经是
   * revision 8）。调用方只负责从响应推导不透明键；in-flight 合并、identity / epoch、
   * 请求键与目标键的失效代际校验仍全部由 store 持有。目标键与请求键不同时，成功
   * 数据只写目标键，请求键只结束本次 loading / settled 生命周期。
   *
   * resolver 抛异常或在运行时返回非字符串时 fail closed：响应仍返回调用方，但不写
   * 任何成功数据。
   */
  resolveCommitKey?: (data: T) => string
}

interface DesiredGenerationFence {
  readonly entries: ReadonlyMap<string, number>
  readonly invalidationFloors: ReadonlyMap<string, number>
}

/**
 * 请求缓存。
 *
 * key 是**不透明字符串**，store 自己只做前缀匹配（invalidate）与相等比较，从不
 * 解析它。约定是以 `<方法> <路径>` 开头，绝大多数键就是 method + 完整 URL；但
 * 「必须长得像真实 URL」不是约束，也不该是——失效是按前缀做的，键的形状因此
 * 决定了谁会被谁连坐。已保存原文就刻意用了 `GET content:/api/links/...` 这个
 * 合成命名空间，好让它躲开库级失效（见 invalidate.ts 的 CONTENT_CACHE_PREFIX）。
 */
export class ResourceStore {
  private readonly entries = new Map<string, CacheEntry>()
  private readonly listeners = new Map<string, Set<() => void>>()
  private readonly inflight = new Map<string, Map<number, Promise<ApiResult<unknown>>>>()
  /** 访问顺序，队尾最新。用于 LRU 淘汰。 */
  private readonly recency: string[] = []
  /** 全局变更观察者（持久化层用）。与按键订阅分开：它关心的是"哪个键变了"。 */
  private readonly observers = new Set<(key: string) => void>()
  /** 前缀失效观察者（持久化层用）：清理已被内存 LRU 淘汰、但仍在磁盘上的条目。 */
  private readonly invalidationObservers = new Set<(prefix: string) => void>()
  /** Local resource-generation floors created by invalidation commands. */
  private readonly invalidationFloors = new Map<string, number>()
  /** Durable namespace journal positions, deliberately separate from resource generations. */
  private readonly durableInvalidationGenerations = new Map<string, number>()
  private readonly capacity: number
  private readonly now: () => number
  private readonly identityRequired: boolean
  private activeIdentity: IdentityOwnership | null = null
  /** Also fences requests in identity-optional test/local stores across clear(). */
  private storeEpoch = 0

  constructor(options: {
    capacity?: number
    now?: () => number
    identityRequired?: boolean
  } = {}) {
    this.capacity = options.capacity ?? DEFAULT_CAPACITY
    this.now = options.now ?? (() => Date.now())
    this.identityRequired = options.identityRequired ?? false
  }

  get activePhysicalNamespace(): string | null {
    return this.canAccessIdentity()
      ? (this.activeIdentity?.operation.physicalNamespace ?? null)
      : null
  }

  /** Whether this exact lease, including its local epoch, owns the active cache partition. */
  isIdentityActive(lease: IdentityLease): boolean {
    const active = this.activeIdentity
    return Boolean(
      active &&
      active.lease === lease &&
      lease.isOwnershipCurrent(active),
    )
  }

  activateIdentity(lease: IdentityLease): boolean {
    const identity = lease.captureOwnership('ResourceStore identity')
    if (!lease.isOwnershipCurrent(identity)) return false
    if (this.activeIdentity?.lease === lease && this.canAccessIdentity()) return true
    this.clear()
    this.activeIdentity = identity
    return true
  }

  deactivateIdentity(expectedLease?: IdentityLease): void {
    if (expectedLease && this.activeIdentity?.lease !== expectedLease) return
    this.clear()
    this.activeIdentity = null
  }

  private canAccessIdentity(): boolean {
    if (!this.identityRequired) return true
    const active = this.activeIdentity
    return Boolean(active && active.lease.isOwnershipCurrent(active))
  }

  private captureIdentity(logicalKey: string): IdentityOwnership | null {
    if (!this.identityRequired) return null
    const active = this.activeIdentity
    if (!active || !active.lease.isOwnershipCurrent(active)) return null
    const identity = active.lease.captureOwnership(logicalKey)
    return active.lease.isOwnershipCurrent(identity) ? identity : null
  }

  private acceptsIdentity(
    identity: IdentityOwnership | null,
  ): boolean {
    if (!this.identityRequired) return true
    return Boolean(identity && this.acceptsExplicitIdentity(identity))
  }

  private acceptsExplicitIdentity(identity: IdentityOwnership): boolean {
    const active = this.activeIdentity
    return Boolean(
      active &&
      active.lease === identity.lease &&
      identity.lease.isOwnershipCurrent(identity) &&
      active.lease.isOwnershipCurrent(active),
    )
  }

  private acceptsRequest(
    identity: IdentityOwnership | null,
    requestEpoch: number,
    key: string,
    generation: number,
  ): boolean {
    return (
      requestEpoch === this.storeEpoch &&
      this.acceptsIdentity(identity) &&
      this.peek(key).desiredGeneration === generation
    )
  }

  /** 当前是否有请求在途（任意键）。预取据此让路给用户主动发起的请求。 */
  get busy(): boolean {
    return this.canAccessIdentity() && this.inflight.size > 0
  }

  /** 读当前快照。永不返回 undefined，便于 useSyncExternalStore 拿到稳定引用。 */
  peek<T>(key: string): CacheEntry<T> {
    if (!this.canAccessIdentity()) return EMPTY_ENTRY as CacheEntry<T>
    const existing = this.entries.get(key) as CacheEntry<T> | undefined
    if (existing) return existing
    let desiredGeneration = 0
    for (const [prefix, generation] of this.invalidationFloors) {
      if (key.startsWith(prefix)) desiredGeneration = Math.max(desiredGeneration, generation)
    }
    if (desiredGeneration === 0) return EMPTY_ENTRY as CacheEntry<T>
    const snapshot: CacheEntry<T> = {
      ...(EMPTY_ENTRY as CacheEntry<T>),
      desiredGeneration,
    }
    this.entries.set(key, snapshot)
    return snapshot
  }

  /** 是否已有可渲染的数据（决定首屏要不要显示 loading）。 */
  has(key: string): boolean {
    if (!this.canAccessIdentity()) return false
    return this.entries.get(key)?.data !== undefined
  }

  /**
   * 订阅**任意键**的变更。返回取消函数。
   *
   * 与 subscribe(key) 的区别：那个是渲染用的按键订阅，这个是给持久化层用的
   * 全局观察者——它需要知道"哪个键刚变了"才能决定往磁盘上写什么。
   */
  onChange(observer: (key: string) => void): () => void {
    this.observers.add(observer)
    return () => {
      this.observers.delete(observer)
    }
  }

  onInvalidate(observer: (prefix: string) => void): () => void {
    this.invalidationObservers.add(observer)
    return () => {
      this.invalidationObservers.delete(observer)
    }
  }

  subscribe(key: string, listener: () => void): () => void {
    let bucket = this.listeners.get(key)
    if (!bucket) {
      bucket = new Set()
      this.listeners.set(key, bucket)
    }
    bucket.add(listener)
    return () => {
      // 卸载只解绑订阅者，**不清缓存**——切回来能秒开全靠这一点。
      bucket.delete(listener)
      if (bucket.size === 0) this.listeners.delete(key)
    }
  }

  /**
   * 取数据。同键并发请求合并为一次网络往返。
   *
   * 返回的是本次（或被复用的那次）请求的结果，调用方据此做 toast 之类的
   * 一次性反馈；渲染仍应走 subscribe + peek。
   */
  async fetch<T>(
    key: string,
    fetcher: (conditional: ConditionalContext) => Promise<ApiResult<T>>,
    options: FetchOptions<T> = {},
  ): Promise<ApiResult<T>> {
    if (options.signal?.aborted) return cancelledResult()

    const identity = this.captureIdentity(`ResourceStore fetch ${key}`)
    const requestEpoch = this.storeEpoch
    if (this.identityRequired && !identity) {
      return {
        ok: false,
        error: { kind: 'identity-mismatch', message: 'Reader identity is not active' },
      }
    }

    let snapshot = this.peek(key)
    let generation = options.generation ?? snapshot.desiredGeneration
    if (options.force) {
      generation = snapshot.desiredGeneration + 1
      snapshot = this.publishGeneration(key, generation)
    } else if (generation < snapshot.desiredGeneration) {
      generation = snapshot.desiredGeneration
    }

    const generations = this.inflight.get(key)
    const existing = generations?.get(generation)
    if (existing) {
      return this.observeWithSignal(existing as Promise<ApiResult<T>>, options.signal)
    }

    // 已有数据时，revalidating 的翻转对消费者不可见（loading 恒 false，
    // data / error 都没动），却会产生一次通知 → 一次重渲染 → 下游 memo 全部
    // 重算。后台校验每 30 秒一次、订阅每 60 秒一次、译文轮询 1.2 秒一次，
    // 这条通知是纯浪费。因此只在「还没有数据可渲染」时才广播它——那种情况下
    // 它决定了 loading。
    const before = this.peek(key)
    this.publish(
      key,
      {
        ...before,
        attemptedGeneration: Math.max(before.attemptedGeneration, generation),
        revalidating: generation === before.desiredGeneration,
      },
      before.data === undefined || before.attemptedGeneration < generation,
    )
    const targetGenerationFence = options.resolveCommitKey
      ? this.captureDesiredGenerationFence()
      : null

    let responseETag: string | null | undefined
    const conditional: ConditionalContext = {
      // force 是用户主动触发的重取（点同步、错误重试）。这时**不发**
      // If-None-Match：服务端的读版本号自带 1 秒缓存，写完立刻点同步有可能
      // 撞上那个窗口拿到 304 —— 表现为「点了同步没反应」。用户主动重取必须
      // 永远有一条不经协商的逃生舱。
      ifNoneMatch: options.force ? null : (this.peek(key).etag ?? null),
      onETag: (tag) => {
        if (!this.acceptsRequest(identity, requestEpoch, key, generation)) return
        responseETag = tag
      },
      signal: options.signal,
    }

    let resolveRun!: (result: ApiResult<T>) => void
    const run = new Promise<ApiResult<T>>((resolve) => {
      resolveRun = resolve
    })
    const byGeneration = this.inflight.get(key) ?? new Map<number, Promise<ApiResult<unknown>>>()
    byGeneration.set(generation, run)
    this.inflight.set(key, byGeneration)

    let settled = false
    const cleanup = () => options.signal?.removeEventListener('abort', cancel)
    const finish = (result: ApiResult<T>, shouldCommit: boolean) => {
      if (settled) return
      settled = true
      cleanup()
      this.clearInflight(key, generation, run)
      if (shouldCommit) {
        this.commit(
          key,
          result,
          options,
          generation,
          identity,
          requestEpoch,
          targetGenerationFence,
          responseETag,
        )
      } else {
        this.settleCancelledRequest(key, generation, identity, requestEpoch)
      }
      resolveRun(result)
    }
    const cancel = () => finish(cancelledResult(), false)

    options.signal?.addEventListener('abort', cancel, { once: true })
    if (options.signal?.aborted) {
      cancel()
      return run
    }

    void this.invoke(() => fetcher(conditional)).then((result) => finish(result, true))
    return run
  }

  /** A signal on a coalesced observer cancels only that observer, not the shared request. */
  private observeWithSignal<T>(
    request: Promise<ApiResult<T>>,
    signal?: AbortSignal,
  ): Promise<ApiResult<T>> {
    if (!signal) return request
    if (signal.aborted) return Promise.resolve(cancelledResult())
    return new Promise((resolve) => {
      let settled = false
      const finish = (result: ApiResult<T>) => {
        if (settled) return
        settled = true
        signal.removeEventListener('abort', cancel)
        resolve(result)
      }
      const cancel = () => finish(cancelledResult())
      signal.addEventListener('abort', cancel, { once: true })
      void request.then(finish)
    })
  }

  /**
   * 调用 fetcher 并把「同步抛出」「返回非 Promise」两种违约收敛成失败结果。
   *
   * 必要性：调用方传进来的 fetcher 常常是测试里的 mock，用尽 mockResolvedValueOnce
   * 之后会返回 undefined，`undefined.then` 会同步抛出并把这个键的 in-flight
   * 永久卡住——后续所有读都挂在一个永不 settle 的 promise 上。
   */
  private async invoke<T>(fetcher: () => Promise<ApiResult<T>>): Promise<ApiResult<T>> {
    try {
      const result = await fetcher()
      if (!result || typeof result !== 'object' || !('ok' in result)) {
        return { ok: false, error: { kind: 'other', message: 'fetcher 未返回 ApiResult' } }
      }
      return result
    } catch (thrown) {
      return {
        ok: false,
        error: {
          kind: 'other',
          message: thrown instanceof Error ? thrown.message : String(thrown),
        },
      }
    }
  }

  /** 把请求结果并入缓存，仅在「对界面而言真的变了」时通知订阅者。 */
  private commit<T>(
    requestKey: string,
    result: ApiResult<T>,
    options: FetchOptions<T>,
    generation: number,
    identity: IdentityOwnership | null,
    requestEpoch: number,
    targetGenerationFence: DesiredGenerationFence | null,
    responseETag: string | null | undefined,
  ): void {
    // request-key fence 始终先于动态键解析。否则请求途中失效 request key 后，
    // 一份失效前响应仍可能绕道写进尚未失效的 target key。
    if (!this.acceptsRequest(identity, requestEpoch, requestKey, generation)) return

    if (!result.ok || !options.resolveCommitKey) {
      this.commitToKey(requestKey, result, options, generation, responseETag)
      return
    }
    if (!targetGenerationFence) return

    let targetKey: string
    try {
      const resolved: unknown = options.resolveCommitKey(result.data)
      if (typeof resolved !== 'string') {
        this.settleRedirectedRequest(requestKey, generation)
        return
      }
      targetKey = resolved
    } catch {
      // 缓存地址解析属于写入边界。解析失败不能污染任一 data entry，但网络响应
      // 本身仍应返回调用方；同时必须把 request key 的 loading 生命周期收尾。
      this.settleRedirectedRequest(requestKey, generation)
      return
    }

    if (targetKey === requestKey) {
      this.commitToKey(requestKey, result, options, generation, responseETag)
      return
    }

    this.settleRedirectedRequest(requestKey, generation)
    // target 的起点代际独立于 request：它可能在请求开始前就已被精准失效。
    // 只要请求期间 target 的 desiredGeneration 又前进，就拒绝迟到响应。
    if (!this.acceptsTarget(identity, requestEpoch, targetKey, targetGenerationFence)) return
    this.commitToKey(
      targetKey,
      result,
      options,
      this.peek(targetKey).desiredGeneration,
      responseETag,
    )
  }

  private captureDesiredGenerationFence(): DesiredGenerationFence {
    return {
      entries: new Map(
        [...this.entries].map(([key, entry]) => [key, entry.desiredGeneration]),
      ),
      invalidationFloors: new Map(this.invalidationFloors),
    }
  }

  private acceptsTarget(
    identity: IdentityOwnership | null,
    requestEpoch: number,
    key: string,
    fence: DesiredGenerationFence,
  ): boolean {
    if (requestEpoch !== this.storeEpoch || !this.acceptsIdentity(identity)) return false

    let expectedGeneration = fence.entries.get(key)
    if (expectedGeneration === undefined) {
      expectedGeneration = 0
      for (const [prefix, generation] of fence.invalidationFloors) {
        if (key.startsWith(prefix)) {
          expectedGeneration = Math.max(expectedGeneration, generation)
        }
      }
    }
    return this.peek(key).desiredGeneration === expectedGeneration
  }

  /** Redirected success 不把 data / ETag 写回 request key，只结束请求状态。 */
  private settleRedirectedRequest(key: string, generation: number): void {
    const current = this.peek(key)
    this.publish(
      key,
      {
        ...current,
        revalidating: false,
        settledGeneration: Math.max(current.settledGeneration, generation),
      },
      current.data === undefined,
    )
  }

  private settleCancelledRequest(
    key: string,
    generation: number,
    identity: IdentityOwnership | null,
    requestEpoch: number,
  ): void {
    if (!this.acceptsRequest(identity, requestEpoch, key, generation)) return
    const current = this.peek(key)
    this.publish(
      key,
      {
        ...current,
        revalidating: false,
        attemptedGeneration: Math.max(current.attemptedGeneration, generation),
        settledGeneration: Math.max(current.settledGeneration, generation),
      },
      current.data === undefined,
    )
  }

  private commitToKey<T>(
    key: string,
    result: ApiResult<T>,
    options: FetchOptions<T>,
    generation: number,
    responseETag: string | null | undefined,
  ): void {
    // 回源途中发生过失效 → 这份结果反映的是失效之前的状态，丢弃。
    // 不丢的话失效等于没发生（与服务端 SnapshotCache 同一条教训）。
    // 在 commit 时刻重新读取条目，避免用 fetch 开始前的陈旧快照覆盖并发状态。
    const current = this.peek(key)
    const equal = options.equal ?? sameResource
    const attemptedGeneration = Math.max(current.attemptedGeneration, generation)
    const settledGeneration = Math.max(current.settledGeneration, generation)

    if (!result.ok) {
      // 304 不是失败：服务端确认我们手里那份仍然有效。刷新 updatedAt、
      // 清掉可能残留的错误，数据与它的引用**原样保留**（下游 memo 不失效）。
      if (result.error.kind === 'not-modified') {
        this.publish(
          key,
          {
            ...current,
            error: null,
            updatedAt: this.now(),
            revalidating: false,
            etag: responseETag === undefined ? current.etag : responseETag,
            attemptedGeneration,
            settledGeneration,
          },
          current.error !== null,
        )
        return
      }
      // 静默校验失败：保留数据与既有错误态，只落 revalidating。
      if (options.silent) {
        this.publish(key, {
          ...current,
          revalidating: false,
          attemptedGeneration,
          settledGeneration,
        })
        return
      }
      this.publish(key, {
        ...current,
        error: result.error,
        revalidating: false,
        attemptedGeneration,
        settledGeneration,
      })
      return
    }

    // 成功必须清掉上一次的错误——包括静默成功。不清的后果是新数据到不了
    // 界面：调用方普遍按 `error ? 错误块 : 列表` 渲染，于是「手动刷新失败 →
    // 后台刷新成功」会停在错误块上直到用户手动重试。
    if (current.data !== undefined && equal(current.data, result.data)) {
      // 数据没变：保留**原来那个 data 引用**，避免下游 memo 全部失效。
      // 只有「之前有错误、这次成功了」才需要广播——那是可见的状态变化。
      this.publish(
        key,
        {
          ...current,
          error: null,
          updatedAt: this.now(),
          revalidating: false,
          etag: responseETag === undefined ? current.etag : responseETag,
          attemptedGeneration,
          settledGeneration,
        },
        current.error !== null,
      )
      return
    }
    // onETag 在请求期间只写局部变量，到双 fence 全部通过后才随实际 target 提交。
    // 未回调时沿用 target 现有 ETag；动态改键时不会把响应 ETag 留在 request key。
    this.publish(key, {
      data: result.data,
      error: null,
      updatedAt: this.now(),
      revalidating: false,
      etag: responseETag === undefined ? current.etag : responseETag,
      desiredGeneration: current.desiredGeneration,
      attemptedGeneration,
      settledGeneration,
    })
  }

  /** 就地改写某个键的数据（乐观更新）。键不存在时忽略。 */
  patch<T>(key: string, updater: (current: T) => T): void {
    if (!this.canAccessIdentity()) return
    const current = this.entries.get(key)
    if (!current || current.data === undefined) return
    this.publish(key, { ...current, data: updater(current.data as T) })
  }

  patchForIdentity<T>(
    identity: IdentityOwnership,
    key: string,
    updater: (current: T) => T,
  ): boolean {
    if (!this.acceptsExplicitIdentity(identity)) return false
    const current = this.entries.get(key)
    if (!current || current.data === undefined) return false
    this.publish(key, { ...current, data: updater(current.data as T) })
    return true
  }

  /** 直接写入数据（用于把一次写操作的响应喂回缓存，省一次校验）。 */
  set<T>(key: string, data: T): void {
    if (!this.canAccessIdentity()) return
    const current = this.peek(key)
    this.publish(key, {
      data,
      error: null,
      updatedAt: this.now(),
      revalidating: false,
      desiredGeneration: current.desiredGeneration,
      attemptedGeneration: Math.max(current.attemptedGeneration, current.desiredGeneration),
      settledGeneration: Math.max(current.settledGeneration, current.desiredGeneration),
    })
  }

  setForIdentity<T>(
    identity: IdentityOwnership,
    key: string,
    data: T,
  ): boolean {
    if (!this.acceptsExplicitIdentity(identity)) return false
    const current = this.peek(key)
    this.publish(key, {
      data,
      error: null,
      updatedAt: this.now(),
      revalidating: false,
      desiredGeneration: current.desiredGeneration,
      attemptedGeneration: Math.max(current.attemptedGeneration, current.desiredGeneration),
      settledGeneration: Math.max(current.settledGeneration, current.desiredGeneration),
    })
    return true
  }

  /**
   * 按**前缀**失效。写操作之后调用，精准打掉受影响的键。
   *
   * 失效 = 删除条目并通知订阅者，下一次读必然回源。不做「标记为陈旧但仍渲染」
   * ——那会让用户在删除一条链接后还看着它。
   */
  invalidate(prefix: string): void {
    if (!this.canAccessIdentity()) return
    this.invalidateResourcePrefix(prefix)
    for (const observer of [...this.invalidationObservers]) observer(prefix)
  }

  private invalidateResourcePrefix(prefix: string): void {
    const prefixGeneration = (this.invalidationFloors.get(prefix) ?? 0) + 1
    this.invalidationFloors.set(prefix, prefixGeneration)
    const knownKeys = new Set([
      ...this.entries.keys(),
      ...this.listeners.keys(),
      ...this.inflight.keys(),
    ])
    const doomed = [...knownKeys].filter((key) => key.startsWith(prefix))
    for (const key of doomed) {
      const current = this.peek(key)
      const desiredGeneration = Math.max(current.desiredGeneration + 1, prefixGeneration)
      this.entries.set(key, {
        error: null,
        updatedAt: 0,
        revalidating: false,
        desiredGeneration,
        attemptedGeneration: current.attemptedGeneration,
        settledGeneration: current.settledGeneration,
      })
      this.dropRecency(key)
      this.notify(key)
    }
  }

  /** Record a journal position already represented by a local invalidation command. */
  acknowledgeDurableInvalidation(prefix: string, generation: number): boolean {
    if (!this.canAccessIdentity() || !Number.isSafeInteger(generation) || generation < 0) {
      return false
    }
    const current = this.durableInvalidationGenerations.get(prefix) ?? 0
    if (generation <= current) return false
    this.durableInvalidationGenerations.set(prefix, generation)
    return true
  }

  /** Apply a newer journal position discovered from IndexedDB in another context. */
  applyInvalidationGeneration(prefix: string, generation: number): void {
    if (!this.acknowledgeDurableInvalidation(prefix, generation)) return
    // Journal positions are namespace-global while resource generations are
    // per key. A new journal position therefore means "advance once", not
    // "assign this numeric value".
    this.invalidateResourcePrefix(prefix)
  }

  /** Durable journal position to stamp on a persisted payload for this key. */
  durableGeneration(key: string): number {
    if (!this.canAccessIdentity()) return 0
    let generation = 0
    for (const [prefix, candidate] of this.durableInvalidationGenerations) {
      if (key.startsWith(prefix)) generation = Math.max(generation, candidate)
    }
    return generation
  }

  /** 清空全部缓存（登出 / 切换后端时用）。 */
  clear(): void {
    const keys = [...this.entries.keys()]
    // 在途请求必须一并作废并遗忘。只清 entries 的话，一个还没返回的请求会在
    // clear 之后把旧后端 / 旧会话的数据落进新的空缓存里；而它留在 inflight
    // 里还会让后续同键读复用一个永远不会带来正确数据的 promise。
    this.storeEpoch += 1
    this.inflight.clear()
    this.entries.clear()
    this.invalidationFloors.clear()
    this.durableInvalidationGenerations.clear()
    this.recency.length = 0
    for (const key of keys) this.notify(key)
  }

  /**
   * 清理 in-flight 记录，但**只在它仍然是自己那一次**时。
   *
   * force 重取会把旧请求从表里踢掉并放进新的；旧请求迟到 resolve 时若无条件
   * delete，删掉的是新的那一个 —— 此后同键并发读找不到 in-flight，会再发一次
   * 网络请求，去重在 force 之后静默失效。
   */
  private clearInflight(
    key: string,
    generation: number,
    own: Promise<ApiResult<unknown>>,
  ): void {
    const byGeneration = this.inflight.get(key)
    if (byGeneration?.get(generation) !== own) return
    byGeneration.delete(generation)
    if (byGeneration.size === 0) this.inflight.delete(key)
  }

  private publishGeneration(key: string, desiredGeneration: number): CacheEntry {
    const current = this.peek(key)
    if (desiredGeneration <= current.desiredGeneration) return current
    const next: CacheEntry = { ...current, desiredGeneration }
    this.publish(key, next)
    return next
  }

  private publish(key: string, entry: CacheEntry, notify = true): void {
    const previous = this.entries.get(key)
    this.entries.set(key, entry)
    this.touch(key)
    if (notify) {
      this.notify(key)
      return
    }
    // 不广播给渲染层，但**数据真的换了**时仍要告诉持久化层——否则磁盘上会
    // 留着旧快照。revalidating 之类的纯内部翻转不算数据变化。
    if (previous?.data !== entry.data) {
      for (const observer of [...this.observers]) observer(key)
    }
  }

  private notify(key: string): void {
    for (const observer of [...this.observers]) observer(key)
    const bucket = this.listeners.get(key)
    if (!bucket) return
    for (const listener of [...bucket]) listener()
  }

  private touch(key: string): void {
    this.dropRecency(key)
    this.recency.push(key)
    this.evictIfNeeded()
  }

  private dropRecency(key: string): void {
    const index = this.recency.indexOf(key)
    if (index >= 0) this.recency.splice(index, 1)
  }

  /**
   * 超容时淘汰最久未访问的条目。
   *
   * **有订阅者的键不淘汰**：它正被某个挂载中的组件渲染，删掉会让界面当场
   * 塌成空态。上限只是防止长会话无界增长，不该以牺牲正在看的内容为代价。
   */
  private evictIfNeeded(): void {
    let index = 0
    const dataEntryCount = () => {
      let count = 0
      for (const entry of this.entries.values()) {
        if (entry.data !== undefined) count += 1
      }
      return count
    }
    while (dataEntryCount() > this.capacity && index < this.recency.length) {
      const candidate = this.recency[index]
      if (this.listeners.has(candidate)) {
        index += 1
        continue
      }
      const current = this.entries.get(candidate)
      if (current) {
        this.entries.set(candidate, {
          error: null,
          updatedAt: 0,
          revalidating: false,
          desiredGeneration: current.desiredGeneration,
          attemptedGeneration: current.attemptedGeneration,
          settledGeneration: current.settledGeneration,
        })
      }
      this.recency.splice(index, 1)
    }
  }
}

/** Reader 全局共享的 store 实例。 */
export const resourceStore = new ResourceStore({ identityRequired: true })
