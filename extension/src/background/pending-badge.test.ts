import { describe, expect, it, vi } from 'vitest'
import {
  PENDING_BADGE_COLOR,
  PENDING_BADGE_STORAGE_KEY,
  PENDING_BADGE_STORAGE_PREFIX,
  PendingBadgeController,
  type PendingBadgeDeps,
} from './pending-badge'

type PendingResult =
  { ok: true; data: number | null } | { ok: false; error: unknown }

function makeFixture(
  initialCount?: unknown,
  storageKey = PENDING_BADGE_STORAGE_KEY,
): {
  deps: PendingBadgeDeps
  values: Record<string, unknown>
  texts: Array<{ text: string }>
  colors: Array<{ color: string }>
  pending: { value: PendingResult }
  buildClient: ReturnType<typeof vi.fn>
} {
  const values: Record<string, unknown> = {}
  if (initialCount !== undefined) {
    values[storageKey] = initialCount
  }
  const texts: Array<{ text: string }> = []
  const colors: Array<{ color: string }> = []
  const pending: { value: PendingResult } = {
    value: { ok: true, data: 0 },
  }
  const getReaderPendingCount = vi.fn(() => Promise.resolve(pending.value))
  const buildClient = vi.fn(() => Promise.resolve({ getReaderPendingCount }))

  const deps: PendingBadgeDeps = {
    buildClient,
    storage: {
      get: vi.fn(async () => ({ ...values })),
      set: vi.fn(async (items: Record<string, unknown>) => {
        Object.assign(values, items)
      }),
    },
    getStorageKey: vi.fn(async () => storageKey),
    setBadgeText: vi.fn((details: { text: string }) => {
      texts.push(details)
    }),
    setBadgeBackgroundColor: vi.fn((details: { color: string }) => {
      colors.push(details)
    }),
  }

  return { deps, values, texts, colors, pending, buildClient }
}

describe('PendingBadgeController', () => {
  it('跨 worker 重建先从 storage 恢复缓存数量', async () => {
    const storageKey = `${PENDING_BADGE_STORAGE_PREFIX}account-a`
    const first = makeFixture(7, storageKey)
    await new PendingBadgeController(first.deps).restore()

    const restarted = makeFixture(first.values[storageKey], storageKey)
    await new PendingBadgeController(restarted.deps).restore()

    expect(first.texts).toEqual([{ text: '7' }])
    expect(restarted.texts).toEqual([{ text: '7' }])
    expect(restarted.texts[0]).not.toHaveProperty('tabId')
  })

  it('不同身份使用不同 storage bucket，不共享待确认数量', async () => {
    const accountA = makeFixture(7, `${PENDING_BADGE_STORAGE_PREFIX}account-a`)
    const accountB = makeFixture(
      undefined,
      `${PENDING_BADGE_STORAGE_PREFIX}account-b`,
    )

    await new PendingBadgeController(accountA.deps).restore()
    await new PendingBadgeController(accountB.deps).restore()

    expect(accountA.texts).toEqual([{ text: '7' }])
    expect(accountB.texts).toEqual([{ text: '' }])
    expect(
      accountB.values[`${PENDING_BADGE_STORAGE_PREFIX}account-a`],
    ).toBeUndefined()
  })

  it('身份切换时先清除旧 badge，不短暂展示旧身份数量', async () => {
    const accountAKey = `${PENDING_BADGE_STORAGE_PREFIX}account-a`
    const accountBKey = `${PENDING_BADGE_STORAGE_PREFIX}account-b`
    const fixture = makeFixture(7, accountAKey)
    let currentKey = accountAKey
    fixture.deps.getStorageKey = vi.fn(async () => currentKey)
    const controller = new PendingBadgeController(fixture.deps)

    await controller.restore()
    currentKey = accountBKey
    await controller.restore()

    expect(fixture.texts[0]).toEqual({ text: '7' })
    expect(fixture.texts.at(-1)).toEqual({ text: '' })
  })

  it('成功刷新读取 counts.inbox 并设置全局数字 badge', async () => {
    const fixture = makeFixture()
    fixture.pending.value = { ok: true, data: 7 }
    const controller = new PendingBadgeController(fixture.deps)

    await controller.refresh()

    expect(fixture.values[PENDING_BADGE_STORAGE_KEY]).toBe(7)
    expect(fixture.texts).toEqual([{ text: '7' }])
    expect(fixture.colors).toEqual([{ color: PENDING_BADGE_COLOR }])
    expect(fixture.texts[0]).not.toHaveProperty('tabId')
  })

  it('数量为 0 时清空 badge', async () => {
    const fixture = makeFixture(4)
    fixture.pending.value = { ok: true, data: 0 }

    await new PendingBadgeController(fixture.deps).start()

    expect(fixture.texts).toEqual([{ text: '4' }, { text: '' }])
    expect(fixture.values[PENDING_BADGE_STORAGE_KEY]).toBe(0)
  })

  it('数量超过 99 时显示 99+', async () => {
    const fixture = makeFixture()
    fixture.pending.value = { ok: true, data: 100 }

    await new PendingBadgeController(fixture.deps).refresh()

    expect(fixture.texts).toEqual([{ text: '99+' }])
  })

  it('旧后端 404/null 清除旧 badge', async () => {
    const fixture = makeFixture(12)
    fixture.pending.value = { ok: true, data: null }

    await new PendingBadgeController(fixture.deps).start()

    expect(fixture.texts).toEqual([{ text: '12' }, { text: '' }])
    expect(fixture.values[PENDING_BADGE_STORAGE_KEY]).toBe(0)
  })

  it('网络/API 错误保留缓存 badge', async () => {
    const fixture = makeFixture(12)
    fixture.pending.value = {
      ok: false,
      error: { kind: 'network-unreachable' },
    }

    await new PendingBadgeController(fixture.deps).start()

    expect(fixture.texts).toEqual([{ text: '12' }])
    expect(fixture.values[PENDING_BADGE_STORAGE_KEY]).toBe(12)
  })

  it('构造客户端失败时保留缓存且 refresh 可再次执行', async () => {
    const fixture = makeFixture(5)
    fixture.buildClient
      .mockRejectedValueOnce(new Error('settings unavailable'))
      .mockImplementationOnce(() =>
        Promise.resolve({
          getReaderPendingCount: () => Promise.resolve({ ok: true, data: 6 }),
        }),
      )
    const controller = new PendingBadgeController(fixture.deps)

    await controller.refresh()
    await controller.refresh()

    expect(fixture.texts).toEqual([{ text: '6' }])
    expect(fixture.values[PENDING_BADGE_STORAGE_KEY]).toBe(6)
  })
})
