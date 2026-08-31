import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { BrowsePanel } from './BrowsePanel'
import type { Pins } from '../lib/meta'
import type { TagStat, DomainStat } from '../lib/stats'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import { IdentityLease } from '../lib/identity'
import { resourceStore } from '../lib/cache/store'
import { ok } from '@webtag/api'

const tags: TagStat[] = [
  { tag: 'rust', count: 2, lastAt: '2026-01-01T00:00:00Z' },
  { tag: 'LLM', count: 9, lastAt: '2026-06-01T00:00:00Z' },
  { tag: 'web', count: 5, lastAt: '2026-03-01T00:00:00Z' },
]
const domains: DomainStat[] = [{ domain: 'arxiv.org', count: 4, lastAt: '2026-05-01T00:00:00Z' }]
const noPins: Pins = { tags: [], domains: [] }

function tagNames(): string[] {
  return screen.getAllByText(/^(rust|LLM|web|alpha|zeta)$/).map((n) => n.textContent!)
}

describe('BrowsePanel', () => {
  afterEach(() => {
    resourceStore.deactivateIdentity()
  })

  function setup(pins: Pins = noPins, readerClient?: IdentityBoundReaderClient, tagData: TagStat[] = tags) {
    const onPick = vi.fn()
    const onTogglePin = vi.fn()
    render(
      <BrowsePanel
        kind="tags"
        onClose={() => {}}
        onPick={onPick}
        pins={pins}
        onTogglePin={onTogglePin}
        tags={tagData}
        domains={domains}
        readerClient={readerClient}
        activityEnabled
      />,
    )
    return { onPick, onTogglePin }
  }

  it('默认按常用（count 降序）排序', () => {
    setup()
    expect(tagNames()).toEqual(['LLM', 'web', 'rust'])
  })

  it('切「最近」按 lastAt 降序', () => {
    setup()
    fireEvent.click(screen.getByText('最近'))
    expect(tagNames()).toEqual(['LLM', 'web', 'rust'])
    expect(screen.getByText(/最近时间：本地近似/)).toBeInTheDocument()
  })

  it('切「A-Z」按名称排序', () => {
    setup()
    fireEvent.click(screen.getByText('A-Z'))
    expect(tagNames()).toEqual(['LLM', 'rust', 'web'])
  })

  it('钉选项排到最前（优先于排序）', () => {
    setup({ tags: ['rust'], domains: [] })
    // rust 被钉选 → 即便 count 最低也排第一。
    expect(tagNames()).toEqual(['rust', 'LLM', 'web'])
  })

  it('搜索过滤', () => {
    setup()
    fireEvent.change(screen.getByPlaceholderText('搜索标签…'), { target: { value: 'we' } })
    expect(tagNames()).toEqual(['web'])
  })

  it('点条目 onPick(tag, name)', () => {
    const { onPick } = setup()
    fireEvent.click(screen.getByText('LLM'))
    expect(onPick).toHaveBeenCalledWith('tag', 'LLM')
  })

  it('点钉选按钮 onTogglePin', () => {
    const { onTogglePin } = setup()
    const pins = screen.getAllByTitle('钉选到侧栏')
    fireEvent.click(pins[0])
    expect(onTogglePin).toHaveBeenCalledWith('tags', 'LLM')
  })

  it('切到域名 tab 显示域名', () => {
    setup()
    fireEvent.click(screen.getByText('域名'))
    expect(screen.getByText('arxiv.org')).toBeInTheDocument()
  })

  it('最近排序优先使用 server activity', async () => {
    const lease = new IdentityLease({
      serverClientDataNamespace: 'browse-activity-server',
      physicalNamespace: 'browse-activity-physical',
      localEpoch: 1,
    })
    resourceStore.activateIdentity(lease)
    const getReaderActivity = vi.fn(async () => ok({
      kind: 'tag' as const,
      tags: [
        { tag: 'rust', last_at: '2026-08-10T00:00:00Z' },
        { tag: 'LLM', last_at: '2026-01-01T00:00:00Z' },
        { tag: 'web', last_at: '2026-02-01T00:00:00Z' },
      ],
      domains: [],
    }))
    const readerClient = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
      getReaderActivity,
    } as unknown as IdentityBoundReaderClient

    setup(noPins, readerClient)
    fireEvent.click(screen.getByText('最近'))
    await waitFor(() => expect(tagNames()[0]).toBe('rust'))
  })

  it('最近排序在时间相同时按 normalized key 稳定排序', () => {
    setup(noPins, undefined, [
      { tag: 'zeta', count: 3, lastAt: '2026-03-01T00:00:00Z' },
      { tag: 'alpha', count: 3, lastAt: '2026-03-01T00:00:00Z' },
      { tag: 'many', count: 4, lastAt: '2026-03-01T00:00:00Z' },
    ])
    fireEvent.click(screen.getByText('最近'))
    expect(Array.from(document.querySelectorAll('.browse-item .cmdk-label')).map((node) => node.textContent))
      .toEqual(['alpha', 'many', 'zeta'])
  })

  it('最近排序按时间值而非时区字符串比较', () => {
    setup(noPins, undefined, [
      { tag: 'zeta', count: 3, lastAt: '2026-08-10T03:00:00+01:00' },
      { tag: 'alpha', count: 3, lastAt: '2026-08-10T02:00:00Z' },
    ])
    fireEvent.click(screen.getByText('最近'))
    expect(tagNames()).toEqual(['alpha', 'zeta'])
  })

  it('完整浏览逐页取得首屏之外的权威 lastAt', async () => {
    const lease = new IdentityLease({
      serverClientDataNamespace: 'browse-pages-server',
      physicalNamespace: 'browse-pages-physical',
      localEpoch: 2,
    })
    resourceStore.activateIdentity(lease)
    const getReaderActivity = vi.fn(async (
      _limit: number,
      options?: { kind?: string; after?: string },
    ) => options?.after
      ? ok({
          kind: 'tag' as const,
          tags: [{ tag: 'beyond-100', last_at: '2026-08-11T00:00:00Z' }],
          domains: [],
        })
      : ok({
          kind: 'tag' as const,
          tags: [{ tag: 'first-page', last_at: '2026-08-10T00:00:00Z' }],
          domains: [],
          next_cursor: 'tag-page-2',
        })) as IdentityBoundReaderClient['getReaderActivity']
    const readerClient = {
      identityLease: lease,
      isIdentityCurrent: vi.fn(() => true),
      getReaderActivity,
    } as unknown as IdentityBoundReaderClient

    setup(noPins, readerClient, [
      { tag: 'first-page', count: 20, lastAt: '2026-01-01T00:00:00Z' },
      { tag: 'beyond-100', count: 1, lastAt: '2026-01-01T00:00:00Z' },
    ])
    fireEvent.click(screen.getByText('最近'))

    await waitFor(() => {
      const names = Array.from(document.querySelectorAll('.browse-item .cmdk-label')).map((node) => node.textContent)
      expect(names).toEqual(['beyond-100', 'first-page'])
    })
    expect(getReaderActivity).toHaveBeenNthCalledWith(1, 100, { kind: 'tag' })
    expect(getReaderActivity).toHaveBeenNthCalledWith(2, 100, { kind: 'tag', after: 'tag-page-2' })
  })
})
