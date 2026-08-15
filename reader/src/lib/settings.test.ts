import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  clearConnection,
  completeLegacySessionUpgrade,
  confirmLegacyBearerCompatibility,
  getConnection,
  hasConnection,
  initializeConnection,
  needsLegacySessionUpgrade,
  saveConnection,
  useConnection,
} from './settings'

const KEY = 'webtag:reader:conn:v2'

interface StoredConnection {
  baseURL: string
  mode: 'session' | 'installation-token'
  installationToken: string
  revision: string
}

function storedConnection(): StoredConnection {
  return JSON.parse(localStorage.getItem(KEY) ?? '{}') as StoredConnection
}

afterEach(() => {
  localStorage.clear()
  // Clear the module-level credential after every test as well as durable state.
  getConnection()
})

describe('settings 连接配置', () => {
  it('无配置时返回空且 hasConnection=false', () => {
    expect(getConnection()).toEqual({
      baseURL: '',
      mode: 'installation-token',
      installationToken: '',
    })
    expect(hasConnection()).toBe(false)
  })

  it('保存后裁掉末尾斜杠并 trim，且持久行带唯一 revision', async () => {
    const saved = await saveConnection({
      baseURL: '  http://localhost:8080/  ',
      mode: 'installation-token',
      installationToken: '  tk  ',
    })
    expect(saved).toEqual({
      baseURL: 'http://localhost:8080',
      mode: 'installation-token',
      installationToken: 'tk',
    })
    expect(storedConnection()).toMatchObject({
      baseURL: 'http://localhost:8080',
      mode: 'installation-token',
      installationToken: '',
      revision: expect.any(String),
    })
    expect(getConnection()).toEqual(saved)
    expect(hasConnection()).toBe(true)
  })

  it('clear 后恢复空配置', async () => {
    await saveConnection({
      baseURL: 'http://x',
      mode: 'installation-token',
      installationToken: 'y',
    })
    await clearConnection()
    expect(getConnection()).toEqual({
      baseURL: '',
      mode: 'installation-token',
      installationToken: '',
    })
    expect(localStorage.getItem(KEY)).toBeNull()
  })

  it('损坏的存储值容错为空配置', () => {
    localStorage.setItem(KEY, '{not json')
    expect(getConnection()).toEqual({
      baseURL: '',
      mode: 'installation-token',
      installationToken: '',
    })
  })

  it('会话模式绝不把安装令牌写进 localStorage', async () => {
    await saveConnection({
      baseURL: 'http://localhost:8080',
      mode: 'session',
      installationToken: 'installation-secret',
    })

    expect(getConnection()).toEqual({
      baseURL: 'http://localhost:8080',
      mode: 'session',
      installationToken: '',
    })
    expect(storedConnection()).toMatchObject({
      mode: 'session',
      installationToken: '',
      revision: expect.any(String),
    })
    expect(localStorage.getItem(KEY) ?? '').not.toContain('installation-secret')
  })

  it('拒绝未知连接配置形状', () => {
    localStorage.setItem(
      KEY,
      JSON.stringify({ baseURL: 'http://old', mode: 'unknown', credential: 'legacy' }),
    )
    expect(hasConnection()).toBe(false)
  })

  it('读取侧清理 session 模式下残留的安装令牌', async () => {
    localStorage.setItem(
      KEY,
      JSON.stringify({
        baseURL: 'http://x',
        mode: 'session',
        installationToken: 'leftover',
      }),
    )

    const snapshot = await initializeConnection()
    expect(snapshot.connection).toEqual({
      baseURL: 'http://x',
      mode: 'session',
      installationToken: '',
    })
    expect(localStorage.getItem(KEY) ?? '').not.toContain('leftover')
  })

  it('迁移旧 Bearer 配置时只在当前页面内存保留 token 并立即清理原始存储', async () => {
    localStorage.setItem(
      KEY,
      JSON.stringify({
        baseURL: 'http://legacy',
        mode: 'installation-token',
        installationToken: 'legacy-secret',
      }),
    )

    const migrated = (await initializeConnection()).connection
    expect(migrated).toEqual({
      baseURL: 'http://legacy',
      mode: 'installation-token',
      installationToken: 'legacy-secret',
    })
    expect(needsLegacySessionUpgrade(migrated)).toBe(true)
    expect(storedConnection()).toMatchObject({
      baseURL: 'http://legacy',
      mode: 'installation-token',
      installationToken: '',
      revision: expect.any(String),
    })
  })

  it('只把仍为当前 revision 的旧 token 升级为持久 session 模式', async () => {
    localStorage.setItem(
      KEY,
      JSON.stringify({
        baseURL: 'http://legacy/',
        mode: 'installation-token',
        installationToken: 'legacy-secret',
      }),
    )
    const migrated = (await initializeConnection()).connection

    await expect(
      completeLegacySessionUpgrade({ ...migrated, baseURL: 'http://other' }),
    ).resolves.toBe(false)
    expect(needsLegacySessionUpgrade(migrated)).toBe(true)
    await expect(completeLegacySessionUpgrade(migrated)).resolves.toBe(true)
    expect(getConnection()).toEqual({
      baseURL: 'http://legacy',
      mode: 'session',
      installationToken: '',
    })
    expect(needsLegacySessionUpgrade(migrated)).toBe(false)
    expect(storedConnection()).toMatchObject({
      baseURL: 'http://legacy',
      mode: 'session',
      installationToken: '',
      revision: expect.any(String),
    })
  })

  it('不会让旧升级覆盖另一个 tab 保存的替代连接', async () => {
    localStorage.setItem(
      KEY,
      JSON.stringify({
        baseURL: 'http://legacy',
        mode: 'installation-token',
        installationToken: 'legacy-secret',
      }),
    )
    const migrated = (await initializeConnection()).connection
    const replacement = await saveConnection({
      baseURL: 'http://replacement',
      mode: 'session',
      installationToken: '',
    })
    const replacementRevision = storedConnection().revision

    await expect(completeLegacySessionUpgrade(migrated)).resolves.toBe(false)
    expect(getConnection()).toEqual(replacement)
    expect(storedConnection()).toMatchObject({
      baseURL: 'http://replacement',
      mode: 'session',
      revision: replacementRevision,
    })
  })

  it('明确兼容后保留页面内 Bearer 但不再重复自动 exchange', async () => {
    localStorage.setItem(
      KEY,
      JSON.stringify({
        baseURL: 'http://legacy',
        mode: 'installation-token',
        installationToken: 'legacy-secret',
      }),
    )
    const migrated = (await initializeConnection()).connection

    await expect(confirmLegacyBearerCompatibility(migrated)).resolves.toBe(true)
    expect(needsLegacySessionUpgrade(migrated)).toBe(false)
    expect(getConnection()).toEqual(migrated)
    expect(localStorage.getItem(KEY) ?? '').not.toContain('legacy-secret')
  })

  it('新建的已协商 Bearer 连接不是旧配置自动升级候选', async () => {
    const saved = await saveConnection({
      baseURL: 'http://legacy',
      mode: 'installation-token',
      installationToken: 'compat-secret',
    })

    expect(needsLegacySessionUpgrade(saved)).toBe(false)
  })

  it('setItem 抛错时公开 error 状态并清空可用连接', async () => {
    const { result } = renderHook(() => useConnection())
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('quota denied', 'QuotaExceededError')
    })

    await act(async () => {
      await expect(saveConnection({
        baseURL: 'http://unwritable',
        mode: 'session',
        installationToken: '',
      })).rejects.toThrow('无法写入连接存储')
    })
    expect(result.current[0].baseURL).toBe('')
    expect(result.current[3]).toEqual({ phase: 'error', message: '无法写入连接存储' })
  })

  it('setItem 静默丢弃写入时通过 readback 校验失败关闭连接', async () => {
    const { result } = renderHook(() => useConnection())
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => undefined)

    await act(async () => {
      await expect(saveConnection({
        baseURL: 'http://discarded',
        mode: 'session',
        installationToken: '',
      })).rejects.toThrow('连接存储写入后校验失败')
    })
    expect(result.current[0].baseURL).toBe('')
    expect(result.current[3].phase).toBe('error')
  })

  it('旧 token 清理写失败时不把凭证暴露到内存并保留可重试的原始行', async () => {
    localStorage.setItem(
      KEY,
      JSON.stringify({
        baseURL: 'http://legacy',
        mode: 'installation-token',
        installationToken: 'legacy-secret',
      }),
    )
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('storage denied', 'SecurityError')
    })

    const snapshot = await initializeConnection()
    expect(snapshot.connection.baseURL).toBe('')
    expect(snapshot.storage.phase).toBe('error')
    expect(localStorage.getItem(KEY)).toContain('legacy-secret')
    expect(needsLegacySessionUpgrade({
      baseURL: 'http://legacy',
      mode: 'installation-token',
      installationToken: 'legacy-secret',
    })).toBe(false)
  })

  it('storage 事件重新读取替代行并让 hook 切换连接', async () => {
    await saveConnection({
      baseURL: 'http://before',
      mode: 'session',
      installationToken: '',
    })
    const { result } = renderHook(() => useConnection())
    const replacement = JSON.stringify({
      baseURL: 'http://after',
      mode: 'session',
      installationToken: '',
      revision: 'external-tab-revision',
    })

    localStorage.setItem(KEY, replacement)
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', {
        key: KEY,
        newValue: replacement,
        storageArea: localStorage,
      }))
    })

    await waitFor(() => expect(result.current[0]).toEqual({
      baseURL: 'http://after',
      mode: 'session',
      installationToken: '',
    }))
    expect(result.current[3].phase).toBe('ready')
  })
})
