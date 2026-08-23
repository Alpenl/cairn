import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { Sidebar, type SidebarProps } from './Sidebar'
import type { Selection } from '../hooks/useLinks'
import { ownedStorageKey } from '../lib/storage-ownership'
import { readerIdentity } from '../lib/identity'
import { resourceStore } from '../lib/cache/store'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import { ok } from '@webtag/api'
import { enabledReaderCapabilityPolicy } from '../test/capabilities'

function baseProps(over: Partial<SidebarProps> = {}): SidebarProps {
  return {
    sel: { type: 'smart', id: 'all', name: '全部链接' } as Selection,
    onSelect: vi.fn(),
    view: 'reading',
    onView: vi.fn(),
    pins: { tags: [], domains: [] },
    onTogglePin: vi.fn(),
    onBrowse: vi.fn(),
    tags: [
      { tag: 'LLM', count: 5, lastAt: '2026-02-01T00:00:00Z' },
      { tag: 'RAG', count: 3, lastAt: '2026-01-01T00:00:00Z' },
      { tag: 'Agent', count: 1, lastAt: '2026-03-01T00:00:00Z' },
    ],
    domains: [{ domain: 'example.com', count: 4, lastAt: '2026-02-01T00:00:00Z' }],
    counts: { all: 9 },
    capabilityPolicy: enabledReaderCapabilityPolicy(),
    ...over,
  }
}

afterEach(() => {
  resourceStore.deactivateIdentity()
})

describe('Sidebar', () => {
  it('显示核心智能视图，并在注意项为零时隐藏空分组', () => {
    render(<Sidebar {...baseProps()} />)
    expect(screen.getByText('全部链接')).toBeInTheDocument()
    expect(screen.getByText('今天新增')).toBeInTheDocument()
    expect(screen.getByText('有划线')).toBeInTheDocument()
    expect(screen.queryByText('处理中')).not.toBeInTheDocument()
    expect(screen.queryByText('解析失败')).not.toBeInTheDocument()
    expect(screen.queryByText('低置信度')).not.toBeInTheDocument()
    expect(screen.getByText('9')).toBeInTheDocument()
  })

  it('即使旧状态计数误传入也不渲染状态行', () => {
    const legacyCounts = {
      all: 9,
      annotated: 2,
      inflight: 3,
      failed: 4,
      lowconf: 5,
    } as unknown as SidebarProps['counts']
    render(<Sidebar {...baseProps({ counts: legacyCounts })} />)
    expect(screen.queryByText('处理中')).not.toBeInTheDocument()
    expect(screen.queryByText('解析失败')).not.toBeInTheDocument()
    expect(screen.queryByText('低置信度')).not.toBeInTheDocument()
  })

  it('后端全库总数不可用时不显示伪数字', () => {
    render(<Sidebar {...baseProps({ counts: {} })} />)
    const allRow = screen.getByText('全部链接').closest('.sb-row')
    expect(allRow?.querySelector('.sb-count')).toBeNull()
  })

  it('钉选标签优先排在最前', () => {
    render(<Sidebar {...baseProps({ pins: { tags: ['Agent'], domains: [] } })} />)
    const tagNames = screen.getAllByText(/^(LLM|RAG|Agent)$/).map((n) => n.textContent)
    expect(tagNames[0]).toBe('Agent')
  })

  it('点钉选按钮触发 onTogglePin（不触发选择）', () => {
    const onTogglePin = vi.fn()
    const onSelect = vi.fn()
    render(<Sidebar {...baseProps({ onTogglePin, onSelect })} />)
    const llmRow = screen.getByText('LLM').closest('.sb-row') as HTMLElement
    const pin = llmRow.querySelector('.pin-btn') as HTMLElement
    fireEvent.click(pin)
    expect(onTogglePin).toHaveBeenCalledWith('tags', 'LLM')
    expect(onSelect).not.toHaveBeenCalled()
  })

  it('旧阅读侧栏的四模式和 surface/tool 入口都交给 canonical route owner', () => {
    const onNavigate = vi.fn()
    render(<Sidebar {...baseProps({ onNavigate })} />)

    fireEvent.click(screen.getByRole('button', { name: '今天' }))
    // 网站 lost its tab; the reading rail is where its route is reachable now.
    // SbRow renders a div, not a button (pre-existing in this rail), so this
    // one is reached by its label.
    fireEvent.click(screen.getByText('按站点'))
    fireEvent.click(screen.getByRole('button', { name: '设置' }))

    expect(onNavigate.mock.calls).toEqual([
      [{ kind: 'surface', id: 'home' }],
      [{ kind: 'library', id: 'sites' }],
      [{ kind: 'tool', id: 'settings' }],
    ])
  })

  it('折叠态持久化到 localStorage', () => {
    render(<Sidebar {...baseProps()} />)
    // 标签组默认展开，点击折叠 → 写入 0
    const label = screen.getByText('标签')
    fireEvent.click(label)
    expect(localStorage.getItem(ownedStorageKey('sidebarFold', 'tags') ?? '')).toBe('0')
  })

  it('does not expose A sidebar folds to B and restores them after returning to A', () => {
    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const firstA = render(<Sidebar {...baseProps()} />)
    fireEvent.click(screen.getByText('标签'))
    expect(screen.queryByText('LLM')).not.toBeInTheDocument()
    firstA.unmount()

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    const pageB = render(<Sidebar {...baseProps()} />)
    expect(screen.getByText('LLM')).toBeInTheDocument()
    pageB.unmount()

    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    render(<Sidebar {...baseProps()} />)
    expect(screen.queryByText('LLM')).not.toBeInTheDocument()
  })

  it('优先用 server activity 覆盖本地 lastAt，并驱动标签最近排序', async () => {
    const lease = readerIdentity.install({
      serverClientDataNamespace: 'sidebar-activity-server',
      physicalNamespace: 'sidebar-activity-physical',
    })
    resourceStore.activateIdentity(lease)
    const getReaderActivity = vi.fn(async (
      _limit: number,
      options?: { kind?: string },
    ) => options?.kind === 'domain'
      ? ok({
          kind: 'domain' as const,
          tags: [],
          domains: [{ domain: 'example.com', last_at: '2026-08-10T00:00:00Z' }],
          next_cursor: 'domain-page-2',
        })
      : ok({
          kind: 'tag' as const,
          tags: [
            { tag: 'RAG', last_at: '2026-08-10T00:00:00Z' },
            { tag: 'LLM', last_at: '2026-01-01T00:00:00Z' },
            { tag: 'Agent', last_at: '2026-02-01T00:00:00Z' },
          ],
          domains: [],
          next_cursor: 'tag-page-2',
        })) as IdentityBoundReaderClient['getReaderActivity']
    const readerClient = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
      getReaderActivity,
    } as unknown as IdentityBoundReaderClient

    render(<Sidebar {...baseProps({ readerClient })} />)
    await waitFor(() => {
      const names = screen.getAllByText(/^(LLM|RAG|Agent)$/).map((node) => node.textContent)
      expect(names[0]).toBe('RAG')
    })
    expect(getReaderActivity).toHaveBeenCalledWith(100, { kind: 'tag' })
    expect(getReaderActivity).toHaveBeenCalledWith(100, { kind: 'domain' })
    expect(getReaderActivity).toHaveBeenCalledTimes(2)
  })

  it('keeps local ordering and sends no activity request when activity is unavailable', () => {
    const lease = readerIdentity.install({
      serverClientDataNamespace: 'sidebar-activity-disabled-server',
      physicalNamespace: 'sidebar-activity-disabled-physical',
    })
    resourceStore.activateIdentity(lease)
    const getReaderActivity = vi.fn(async () => ok({
      kind: 'all' as const,
      tags: [{ tag: 'server-only', last_at: '2026-08-11T00:00:00Z' }],
      domains: [],
    }))
    const readerClient = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
      getReaderActivity,
    } as unknown as IdentityBoundReaderClient

    render(<Sidebar {...baseProps({
      readerClient,
      capabilityPolicy: enabledReaderCapabilityPolicy({ activity: false }),
    })} />)

    expect(screen.getByText('LLM')).toBeInTheDocument()
    expect(screen.queryByText('server-only')).not.toBeInTheDocument()
    expect(getReaderActivity).not.toHaveBeenCalled()
  })

  it('标签最近排序在时间相同时按 normalized key 稳定排序', () => {
    render(<Sidebar {...baseProps({
      tags: [
        { tag: 'zeta', count: 3, lastAt: '2026-03-01T00:00:00Z' },
        { tag: 'alpha', count: 3, lastAt: '2026-03-01T00:00:00Z' },
        { tag: 'many', count: 4, lastAt: '2026-03-01T00:00:00Z' },
      ],
    })} />)
    const tagGroup = screen.getByText('标签').closest('.sb-group')
    const names = Array.from(tagGroup?.querySelectorAll('.sb-row .sb-name') ?? [])
      .map((node) => node.textContent)
    expect(names).toEqual(['alpha', 'many', 'zeta'])
  })

  it('标签最近排序按时间值而非时区字符串比较', () => {
    render(<Sidebar {...baseProps({
      tags: [
        { tag: 'zeta', count: 3, lastAt: '2026-08-10T03:00:00+01:00' },
        { tag: 'alpha', count: 3, lastAt: '2026-08-10T02:00:00Z' },
      ],
    })} />)
    const tagGroup = screen.getByText('标签').closest('.sb-group')
    const names = Array.from(tagGroup?.querySelectorAll('.sb-row .sb-name') ?? [])
      .map((node) => node.textContent)
    expect(names).toEqual(['alpha', 'zeta'])
  })

  it('权威首屏存在时不把未返回标签的本地 created_at 混入 top-N', async () => {
    const lease = readerIdentity.install({
      serverClientDataNamespace: 'sidebar-no-mix-server',
      physicalNamespace: 'sidebar-no-mix-physical',
    })
    resourceStore.activateIdentity(lease)
    const getReaderActivity = vi.fn(async (
      _limit: number,
      options?: { kind?: string },
    ) => options?.kind === 'domain'
      ? ok({ kind: 'domain' as const, tags: [], domains: [] })
      : ok({
          kind: 'tag' as const,
          tags: [{ tag: 'RAG', last_at: '2026-01-01T00:00:00Z' }],
          domains: [],
        })) as IdentityBoundReaderClient['getReaderActivity']
    const readerClient = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
      getReaderActivity,
    } as unknown as IdentityBoundReaderClient

    render(<Sidebar {...baseProps({
      readerClient,
      tags: [
        { tag: 'LLM', count: 99, lastAt: '2026-12-01T00:00:00Z' },
        { tag: 'RAG', count: 1, lastAt: '2026-01-01T00:00:00Z' },
      ],
    })} />)

    await waitFor(() => {
      const tagGroup = screen.getByText('标签').closest('.sb-group')
      const names = Array.from(tagGroup?.querySelectorAll('.sb-row .sb-name') ?? [])
        .map((node) => node.textContent)
      expect(names).toEqual(['RAG'])
    })
  })
})
