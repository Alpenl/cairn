import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import { err, ok, type ApiResult } from '@webtag/api'
import type { HealthResponse } from '../../lib/api/types'
import { READER_STARTUP_PREFERENCE_STORAGE_KEY } from '../../lib/navigation/route'
import { enabledReaderCapabilityPolicy } from '../../test/capabilities'
import { SettingsSurface } from './SettingsSurface'

function healthClient(result: ApiResult<HealthResponse>): IdentityBoundReaderClient {
  return { getHealth: vi.fn(async () => result) } as unknown as IdentityBoundReaderClient
}

/**
 * The startup-preference cases care about neither version nor GitHub. A probe
 * that never settles keeps the About panel in its loading state so those cases
 * assert on preferences only.
 */
function pendingHealthClient(): IdentityBoundReaderClient {
  return {
    getHealth: vi.fn(() => new Promise<never>(() => {})),
  } as unknown as IdentityBoundReaderClient
}

function releaseHealth(overrides: Partial<HealthResponse> = {}): HealthResponse {
  return {
    status: 'ok',
    version: '1.4.0',
    commit: '0123456789abcdef0123456789abcdef01234567',
    build_time: '2026-08-01T10:00:00Z',
    ...overrides,
  }
}

describe('SettingsSurface', () => {
  afterEach(() => {
    localStorage.clear()
    vi.unstubAllGlobals()
  })

  it('shows and persists the Reader startup preference', () => {
    localStorage.setItem(READER_STARTUP_PREFERENCE_STORAGE_KEY, 'reading')

    render(<SettingsSurface client={pendingHealthClient()} capabilityPolicy={enabledReaderCapabilityPolicy()} onNavigate={vi.fn()} onOpenConnectionSettings={vi.fn()} />)

    expect(screen.getByRole('button', { name: '直接阅读' })).toHaveAttribute('aria-pressed', 'true')

    fireEvent.click(screen.getByRole('button', { name: '总是今天' }))

    expect(localStorage.getItem(READER_STARTUP_PREFERENCE_STORAGE_KEY)).toBe('home')
    expect(screen.getByRole('button', { name: '总是今天' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('status')).toHaveTextContent('已保存。')
  })

  it('keeps connection settings and workspace navigation separate from startup preference changes', () => {
    const onNavigate = vi.fn()
    const onOpenConnectionSettings = vi.fn()

    render(<SettingsSurface client={pendingHealthClient()} capabilityPolicy={enabledReaderCapabilityPolicy()} onNavigate={onNavigate} onOpenConnectionSettings={onOpenConnectionSettings} />)

    fireEvent.click(screen.getByRole('button', { name: '记住上次位置' }))
    expect(onNavigate).not.toHaveBeenCalled()
    expect(onOpenConnectionSettings).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: /打开连接设置/ }))
    // TODO and the archived thoughts were demoted out of the rail; settings is
    // the explicit path that keeps them reachable.
    fireEvent.click(screen.getByRole('button', { name: /打开 TODO/ }))
    fireEvent.click(screen.getByRole('button', { name: /查看已归档/ }))

    expect(onOpenConnectionSettings).toHaveBeenCalledTimes(1)
    expect(onNavigate).toHaveBeenNthCalledWith(1, { kind: 'tool', id: 'todo' })
    expect(onNavigate).toHaveBeenNthCalledWith(2, { kind: 'tool', id: 'history' }, { thoughtView: 'history' })
  })

  it('hides an unavailable Home preference without rewriting the stored choice', () => {
    localStorage.setItem(READER_STARTUP_PREFERENCE_STORAGE_KEY, 'home')

    render(<SettingsSurface client={pendingHealthClient()} capabilityPolicy={enabledReaderCapabilityPolicy({ home: false })} onNavigate={vi.fn()} onOpenConnectionSettings={vi.fn()} />)

    expect(screen.queryByRole('button', { name: '总是首页' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '直接阅读' })).toHaveAttribute('aria-pressed', 'true')
    expect(localStorage.getItem(READER_STARTUP_PREFERENCE_STORAGE_KEY)).toBe('home')
  })
})

describe('SettingsSurface 关于 Core 版本', () => {
  afterEach(() => {
    localStorage.clear()
    vi.unstubAllGlobals()
  })

  function renderAbout(client: IdentityBoundReaderClient) {
    render(<SettingsSurface client={client} capabilityPolicy={enabledReaderCapabilityPolicy()} onNavigate={vi.fn()} onOpenConnectionSettings={vi.fn()} />)
  }

  it('shows the released version, short commit and build time reported by /health', async () => {
    renderAbout(healthClient(ok(releaseHealth())))

    expect(await screen.findByText(/版本 1\.4\.0/)).toHaveTextContent('commit 0123456')
    expect(screen.getByText(/构建于/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /查看这个版本的 Release/ })).toBeEnabled()
  })

  it('把部署交互接在关于旁边，默认锁定且不发任何部署请求', async () => {
    // 面板用的是默认的同源部署客户端；锁定态一个请求都不发，所以这里不需要
    // 桩掉 fetch 也不会有网络动作。
    renderAbout(healthClient(ok(releaseHealth())))

    expect(await screen.findByText(/版本 1\.4\.0/)).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '更新 Core' })).toBeInTheDocument()
    expect(screen.getByLabelText('部署令牌')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /检查更新/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /更新到/ })).not.toBeInTheDocument()
  })

  it('labels an uninjected development build and offers no release lookup', async () => {
    // internal/buildinfo falls back to 0.0.0 / unknown when ldflags were not
    // injected; there is no GitHub release to point at in that case.
    renderAbout(healthClient(ok(releaseHealth({ version: '0.0.0', commit: 'unknown', build_time: 'unknown' }))))

    expect(await screen.findByText(/开发构建/)).toHaveTextContent('commit unknown')
    expect(screen.getByText(/构建时间未注入/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /查看这个版本的 Release/ })).not.toBeInTheDocument()
  })

  it('keeps showing the current version when GitHub cannot be reached', async () => {
    const githubFetch = vi.fn(async () => { throw new TypeError('Failed to fetch') })
    vi.stubGlobal('fetch', githubFetch)
    renderAbout(healthClient(ok(releaseHealth())))

    fireEvent.click(await screen.findByRole('button', { name: /查看这个版本的 Release/ }))

    expect(await screen.findByText(/暂时连不上 GitHub/)).toBeInTheDocument()
    // The GitHub failure is confined to its own line.
    expect(screen.getByText(/版本 1\.4\.0/)).toHaveTextContent('commit 0123456')
    expect(githubFetch).toHaveBeenCalledTimes(1)
  })

  it('degrades to an explicit retry when the backend is unreachable', async () => {
    const getHealth = vi.fn()
      .mockResolvedValueOnce(err({ kind: 'network-unreachable', message: 'offline' }))
      .mockResolvedValueOnce(ok(releaseHealth()))
    renderAbout({ getHealth } as unknown as IdentityBoundReaderClient)

    expect(await screen.findByText(/后端不可达/)).toBeInTheDocument()
    // The rest of settings stays usable while the probe fails.
    expect(screen.getByRole('button', { name: /打开连接设置/ })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /查看这个版本的 Release/ })).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /重新读取/ }))

    expect(await screen.findByText(/版本 1\.4\.0/)).toHaveTextContent('commit 0123456')
    await waitFor(() => expect(getHealth).toHaveBeenCalledTimes(2))
  })
})
