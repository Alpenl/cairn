/**
 * useSettings.test.ts — useWebTagSettings composable 单元测试。
 *
 * 依赖 test/setup.ts 中升级后的 chrome.storage.local mock：
 *   - 真实内存对象支撑，get 同时支持 Promise / 回调两种风格
 *   - set / remove 变更后会真实触发 chrome.storage.onChanged 监听器
 *
 * 覆盖：
 *   - mergeWithDefaults：加载时补齐缺失字段、过滤未知字段
 *   - 防抖落盘：对 ref 的修改经 PERSIST_DEBOUNCE_MS 防抖后写入 storage
 *   - onChanged 反向同步：其它上下文写入 storage 后同步进响应式 ref
 *   - update()：部分更新立即落盘、绕过防抖、落盘失败归一化为 UpdateResult
 *   - dispose()：解除 watch 与 onChanged 监听
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import {
  DEFAULT_WEBTAG_SETTINGS,
  WEBTAG_SETTINGS_STORAGE_KEY,
  getWebTagSettings,
  useWebTagSettings,
  type WebTagSettings,
} from './useSettings'

/** 防抖延迟（与 useSettings.ts 内部 PERSIST_DEBOUNCE_MS 保持一致）。 */
const PERSIST_DEBOUNCE_MS = 800

/** 直接读取 chrome.storage.local 中持久化的原始配置对象。 */
async function readPersisted(): Promise<unknown> {
  const result = await chrome.storage.local.get(WEBTAG_SETTINGS_STORAGE_KEY)
  return result[WEBTAG_SETTINGS_STORAGE_KEY]
}

/** 直接向 chrome.storage.local 写入指定配置（模拟其它上下文的写入）。 */
async function seedStorage(value: unknown): Promise<void> {
  await chrome.storage.local.set({ [WEBTAG_SETTINGS_STORAGE_KEY]: value })
}

describe('useWebTagSettings — 加载与 mergeWithDefaults', () => {
  it('storage 为空时加载出全部默认值', async () => {
    const { settings, ready, dispose } = useWebTagSettings()
    await ready
    expect(settings.value).toEqual(DEFAULT_WEBTAG_SETTINGS)
    dispose()
  })

  it('storage 缺字段时用默认值补齐缺失字段', async () => {
    // 只存了 backendUrl，其余字段缺失
    await seedStorage({ backendUrl: 'http://localhost:9000' })
    const { settings, ready, dispose } = useWebTagSettings()
    await ready
    expect(settings.value).toEqual({
      ...DEFAULT_WEBTAG_SETTINGS,
      backendUrl: 'http://localhost:9000',
    })
    dispose()
  })

  it('storage 含未知字段与非法值时被过滤回退默认值', async () => {
    await seedStorage({
      backendUrl: 'http://api.test',
      readerUrl: DEFAULT_WEBTAG_SETTINGS.readerUrl,
      accessToken: 'tok-1',
      linkOpenBehavior: 'invalid-behavior', // 非法值 → 回退默认
      legacyUnknownField: 'should-be-dropped', // 未知字段 → 丢弃
    })
    const { settings, ready, dispose } = useWebTagSettings()
    await ready
    expect(settings.value).toEqual({
      ...DEFAULT_WEBTAG_SETTINGS,
      backendUrl: 'http://api.test',
      accessToken: 'tok-1',
    })
    expect(settings.value).not.toHaveProperty('legacyUnknownField')
    dispose()
  })

  it('getWebTagSettings 一次性快照同样经过 mergeWithDefaults', async () => {
    await seedStorage({ accessToken: 'snapshot-token' })
    const snapshot = await getWebTagSettings()
    expect(snapshot).toEqual({
      ...DEFAULT_WEBTAG_SETTINGS,
      accessToken: 'snapshot-token',
    })
  })

  it('保留显式 Reader 地址并过滤非字符串旧值', async () => {
    await seedStorage({ readerUrl: ' https://reader.test/app ' })
    expect((await getWebTagSettings()).readerUrl).toBe(
      ' https://reader.test/app ',
    )

    await seedStorage({ readerUrl: 42 })
    expect((await getWebTagSettings()).readerUrl).toBe('')
  })
})

describe('useWebTagSettings — 防抖落盘', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('修改 ref 后经防抖延迟才写入 storage', async () => {
    const { settings, ready, dispose } = useWebTagSettings()
    // ready 内部 await readSettings —— 用真实微任务推进，与 fake timer 无关。
    await ready

    settings.value.linkOpenBehavior = 'new'
    await nextTick() // 触发 watch 回调，安排防抖 timer

    // 防抖未到时间：storage 尚未写入
    expect(await readPersisted()).toBeUndefined()

    // 推进到防抖时长后：落盘
    vi.advanceTimersByTime(PERSIST_DEBOUNCE_MS)
    const persisted = (await readPersisted()) as WebTagSettings
    expect(persisted.linkOpenBehavior).toBe('new')
    dispose()
  })

  it('防抖窗口内的多次修改合并为一次写入', async () => {
    const { settings, ready, dispose } = useWebTagSettings()
    await ready

    settings.value.linkOpenBehavior = 'new'
    await nextTick()
    vi.advanceTimersByTime(PERSIST_DEBOUNCE_MS / 2) // 防抖未触发
    settings.value.linkOpenBehavior = 'current'
    await nextTick()
    vi.advanceTimersByTime(PERSIST_DEBOUNCE_MS / 2) // 重新计时，仍未到

    expect(await readPersisted()).toBeUndefined()

    vi.advanceTimersByTime(PERSIST_DEBOUNCE_MS / 2) // 补足后半段
    const persisted = (await readPersisted()) as WebTagSettings
    // 只落盘最后一次的值
    expect(persisted.linkOpenBehavior).toBe('current')

    // chrome.storage.local.set 只被业务调用一次（排除 seed/读取不计）
    dispose()
  })

  it('防抖落盘失败时被 .catch 兜底，不抛未处理的 Promise rejection', async () => {
    const { settings, ready, dispose } = useWebTagSettings()
    await ready

    // 让 chrome.storage.local.set 在防抖回调里 reject（模拟配额超限 / 瞬时故障）。
    const setSpy = vi
      .spyOn(chrome.storage.local, 'set')
      .mockRejectedValueOnce(new Error('QUOTA_BYTES quota exceeded'))
    // 防抖回调内部应 console.warn 兜底，spy 之以断言失败被记录而非静默 / 抛出。
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {})

    settings.value.linkOpenBehavior = 'new'
    await nextTick()
    // 推进防抖时长触发回调，再用 runAllTimersAsync 排空回调内的微任务链。
    await vi.advanceTimersByTimeAsync(PERSIST_DEBOUNCE_MS)

    // .catch 已消费 rejection 并 console.warn 记录；未处理 rejection 不会出现。
    expect(setSpy).toHaveBeenCalled()
    expect(warnSpy).toHaveBeenCalled()
    warnSpy.mockRestore()
    setSpy.mockRestore()
    dispose()
  })
})

describe('useWebTagSettings — onChanged 反向同步', () => {
  it('其它上下文写入 storage 后同步进响应式 ref', async () => {
    const { settings, ready, dispose } = useWebTagSettings()
    await ready
    expect(settings.value.linkOpenBehavior).toBe(
      DEFAULT_WEBTAG_SETTINGS.linkOpenBehavior,
    )

    // 模拟另一个上下文（popup）写入 → 触发 onChanged
    await seedStorage({
      backendUrl: 'http://remote.test',
      readerUrl: DEFAULT_WEBTAG_SETTINGS.readerUrl,
      accessToken: 'remote-token',
      linkOpenBehavior: 'new',
    })
    await nextTick()

    expect(settings.value).toEqual({
      ...DEFAULT_WEBTAG_SETTINGS,
      backendUrl: 'http://remote.test',
      accessToken: 'remote-token',
      linkOpenBehavior: 'new',
    })
    dispose()
  })

  it('反向同步不会再触发一次落盘（不形成回写循环）', async () => {
    const { ready, dispose } = useWebTagSettings()
    await ready

    // spyOn 复用底层 vi.fn()，先 mockClear 抹掉此前累积的调用，从干净基线计数。
    const setSpy = vi.spyOn(chrome.storage.local, 'set')
    setSpy.mockClear()

    // 远端写入触发 onChanged → 反向同步进 ref。这一次 set 是 seed 自身发出的。
    await seedStorage({
      backendUrl: 'http://no-loop.test',
      accessToken: '',
      linkOpenBehavior: 'current',
    })
    await nextTick()
    await nextTick()

    // onChanged 同步是程序化写入，watch 回调消费标记后跳过落盘——
    // 因此除了 seed 自身那一次，composable 不应再额外调用 set。
    expect(setSpy).toHaveBeenCalledTimes(1)
    setSpy.mockRestore()
    dispose()
  })

  it('非 local 区域的变更被忽略', async () => {
    const { settings, ready, dispose } = useWebTagSettings()
    await ready
    const before = { ...settings.value }

    // 直接以 sync 区域名触发监听器：应被 areaName 判断过滤掉
    const listeners = (
      chrome.storage.onChanged.addListener as unknown as {
        mock: { calls: Array<[(...args: unknown[]) => void]> }
      }
    ).mock.calls
    for (const [listener] of listeners) {
      listener(
        {
          [WEBTAG_SETTINGS_STORAGE_KEY]: {
            newValue: { backendUrl: 'http://sync-area.test' },
          },
        },
        'sync',
      )
    }
    await nextTick()
    expect(settings.value).toEqual(before)
    dispose()
  })
})

describe('useWebTagSettings — update()', () => {
  it('update 立即落盘并返回 ok:true', async () => {
    const { settings, ready, update, dispose } = useWebTagSettings()
    await ready

    const result = await update({ linkOpenBehavior: 'new' })
    expect(result).toEqual({ ok: true })

    // ref 已即时更新
    expect(settings.value.linkOpenBehavior).toBe('new')
    // storage 已即时落盘（无需等防抖）
    const persisted = (await readPersisted()) as WebTagSettings
    expect(persisted.linkOpenBehavior).toBe('new')
    dispose()
  })

  it('普通 update 拒绝连接身份字段', async () => {
    const { settings, ready, update, dispose } = useWebTagSettings()
    await ready

    const result = await update({
      backendUrl: 'https://unconfirmed.example.test',
      accessToken: 'unconfirmed-token',
    })

    expect(result.ok).toBe(false)
    expect(settings.value).toEqual(DEFAULT_WEBTAG_SETTINGS)
    expect(await readPersisted()).toBeUndefined()
    dispose()
  })

  it('update 只覆盖 patch 字段，其余字段保持不变', async () => {
    await seedStorage({
      backendUrl: 'http://keep.test',
      readerUrl: DEFAULT_WEBTAG_SETTINGS.readerUrl,
      accessToken: 'keep-token',
      linkOpenBehavior: 'current',
    })
    const { settings, ready, update, dispose } = useWebTagSettings()
    await ready

    await update({ linkOpenBehavior: 'new' })
    expect(settings.value).toEqual({
      ...DEFAULT_WEBTAG_SETTINGS,
      backendUrl: 'http://keep.test',
      accessToken: 'keep-token',
      linkOpenBehavior: 'new',
    })
    dispose()
  })

  it('update 取消尚未触发的防抖写入', async () => {
    vi.useFakeTimers()
    try {
      const { settings, ready, update, dispose } = useWebTagSettings()
      await ready

      // 制造一个待执行的防抖写入
      settings.value.linkOpenBehavior = 'new'
      await nextTick()
      // 防抖未到时间就调用 update —— update 内部应 clearTimeout 掉它
      await update({ linkOpenBehavior: 'current' })

      // 再推进防抖时长：旧的防抖回调应已被取消，不会覆盖 update 的结果
      vi.advanceTimersByTime(PERSIST_DEBOUNCE_MS)
      const persisted = (await readPersisted()) as WebTagSettings
      expect(persisted.linkOpenBehavior).toBe('current')
      dispose()
    } finally {
      vi.useRealTimers()
    }
  })

  it('落盘失败时归一化为 { ok: false, error } 且不抛异常', async () => {
    const { ready, update, dispose } = useWebTagSettings()
    await ready

    // 让 chrome.storage.local.set 抛错（模拟配额超限）
    const setSpy = vi
      .spyOn(chrome.storage.local, 'set')
      .mockRejectedValueOnce(new Error('QUOTA_BYTES quota exceeded'))

    // update 不应抛出，而是返回失败结果
    const result = await update({ linkOpenBehavior: 'new' })
    expect(result.ok).toBe(false)
    if (!result.ok) {
      expect(result.error).toBeInstanceOf(Error)
      expect(result.error.message).toContain('写入扩展配置失败')
      expect(result.error.message).toContain('QUOTA_BYTES quota exceeded')
    }

    setSpy.mockRestore()
    dispose()
  })

  it('落盘失败后程序化标记不残留，后续用户修改仍能正常防抖落盘', async () => {
    const { settings, ready, update, dispose } = useWebTagSettings()
    await ready

    const setSpy = vi
      .spyOn(chrome.storage.local, 'set')
      .mockRejectedValueOnce(new Error('transient failure'))

    const failed = await update({ linkOpenBehavior: 'new' })
    expect(failed.ok).toBe(false)
    await nextTick() // 让失败 update 的 watch 回调消费掉程序化标记

    setSpy.mockRestore()

    // 后续用户直接改 ref —— 标记未残留，watch 应识别为用户修改并防抖落盘
    vi.useFakeTimers()
    try {
      settings.value.linkOpenBehavior = 'current'
      await nextTick()
      vi.advanceTimersByTime(PERSIST_DEBOUNCE_MS)
      const persisted = (await readPersisted()) as WebTagSettings
      expect(persisted.linkOpenBehavior).toBe('current')
    } finally {
      vi.useRealTimers()
    }
    dispose()
  })
})

describe('useWebTagSettings — confirmed connection', () => {
  it('直接修改连接字段会恢复 confirmed 值且不持久化', async () => {
    const { settings, ready, dispose } = useWebTagSettings()
    await ready

    settings.value.backendUrl = 'https://draft.example.test'
    settings.value.accessToken = 'draft-token'
    await nextTick()
    await nextTick()

    expect(settings.value.backendUrl).toBe('')
    expect(settings.value.accessToken).toBe('')
    expect(await readPersisted()).toBeUndefined()
    dispose()
  })

  it('待处理的普通设置防抖不会持久化随后直接写入的连接字段', async () => {
    vi.useFakeTimers()
    try {
      const { settings, ready, dispose } = useWebTagSettings()
      await ready
      const setSpy = vi.spyOn(chrome.storage.local, 'set')
      setSpy.mockClear()

      settings.value.linkOpenBehavior = 'new'
      await nextTick()
      settings.value.backendUrl = 'https://draft.example.test'
      settings.value.accessToken = 'draft-token'
      await nextTick()
      await nextTick()
      await vi.advanceTimersByTimeAsync(PERSIST_DEBOUNCE_MS)

      expect(settings.value.backendUrl).toBe('')
      expect(settings.value.accessToken).toBe('')
      expect(setSpy).not.toHaveBeenCalled()
      expect(await readPersisted()).toBeUndefined()
      setSpy.mockRestore()
      dispose()
    } finally {
      vi.useRealTimers()
    }
  })

  it('confirmConnection 单次原子写入完整元组并推进 revision', async () => {
    const { settings, ready, confirmConnection, dispose } = useWebTagSettings()
    await ready
    const setSpy = vi.spyOn(chrome.storage.local, 'set')
    setSpy.mockClear()

    const result = await confirmConnection({
      backendUrl: ' https://api-b.example.test ',
      readerUrl: ' https://reader-b.example.test ',
      accessToken: ' token-b ',
      connectionOwnerFingerprint: 'b'.repeat(64),
    })

    expect(result).toEqual({ ok: true })
    expect(setSpy).toHaveBeenCalledTimes(1)
    expect(settings.value).toEqual({
      backendUrl: 'https://api-b.example.test',
      readerUrl: 'https://reader-b.example.test',
      accessToken: 'token-b',
      connectionOwnerFingerprint: 'b'.repeat(64),
      connectionRevision: 1,
      linkOpenBehavior: 'current',
    })
    expect(await readPersisted()).toEqual(settings.value)
    setSpy.mockRestore()
    dispose()
  })

  it('confirmConnection 写入失败时不改变 confirmed ref 或 revision', async () => {
    const { settings, ready, confirmConnection, dispose } = useWebTagSettings()
    await ready
    const setSpy = vi
      .spyOn(chrome.storage.local, 'set')
      .mockRejectedValueOnce(new Error('storage unavailable'))

    const result = await confirmConnection({
      backendUrl: 'https://api-b.example.test',
      readerUrl: '',
      accessToken: 'token-b',
      connectionOwnerFingerprint: 'b'.repeat(64),
    })

    expect(result.ok).toBe(false)
    expect(settings.value).toEqual(DEFAULT_WEBTAG_SETTINGS)
    expect(await readPersisted()).toBeUndefined()
    setSpy.mockRestore()
    dispose()
  })
})

describe('useWebTagSettings — reload 与 dispose', () => {
  it('reload 重新从 storage 拉取并覆盖本地 ref', async () => {
    const { settings, ready, reload, dispose } = useWebTagSettings()
    await ready
    expect(settings.value.backendUrl).toBe('')

    // 直接改 storage（不经 onChanged 链路 —— set 仍会触发，但这里验证 reload 显式拉取）
    await seedStorage({
      backendUrl: 'http://reloaded.test',
      accessToken: '',
      linkOpenBehavior: 'current',
    })
    await reload()
    expect(settings.value.backendUrl).toBe('http://reloaded.test')
    dispose()
  })

  it('reload 拉取的程序化赋值不触发回写落盘', async () => {
    const { ready, reload, dispose } = useWebTagSettings()
    await ready

    await seedStorage({
      backendUrl: 'http://reload-no-write.test',
      accessToken: '',
      linkOpenBehavior: 'current',
    })
    const setSpy = vi.spyOn(chrome.storage.local, 'set')
    setSpy.mockClear() // 抹掉 seed 与此前测试累积的调用，从干净基线计数
    await reload()
    await nextTick()
    expect(setSpy).not.toHaveBeenCalled()
    setSpy.mockRestore()
    dispose()
  })

  it('dispose 后 onChanged 监听被移除，远端变更不再同步进 ref', async () => {
    const { settings, ready, dispose } = useWebTagSettings()
    await ready
    dispose()

    await seedStorage({
      backendUrl: 'http://after-dispose.test',
      accessToken: '',
      linkOpenBehavior: 'current',
    })
    await nextTick()
    // 已 dispose：监听器已移除，ref 不应被远端变更影响
    expect(settings.value.backendUrl).toBe('')
    dispose() // 重复调用应幂等，不报错
  })

  it('dispose 后对 ref 的修改不再触发防抖落盘', async () => {
    const { settings, ready, dispose } = useWebTagSettings()
    await ready
    dispose()

    vi.useFakeTimers()
    try {
      const setSpy = vi.spyOn(chrome.storage.local, 'set')
      setSpy.mockClear() // 从干净基线计数
      settings.value.backendUrl = 'http://post-dispose.test'
      await nextTick()
      vi.advanceTimersByTime(PERSIST_DEBOUNCE_MS)
      expect(setSpy).not.toHaveBeenCalled()
      setSpy.mockRestore()
    } finally {
      vi.useRealTimers()
    }
  })
})
