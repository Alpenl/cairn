import { describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen } from '@testing-library/react'
import { CommandPalette } from './CommandPalette'
import type { ReaderClient } from '../lib/api/client'
import type { ApiResult } from '@webtag/api'
import type { GroupedSearchResponse } from '../lib/api/types'
import type { TagStat, DomainStat } from '../lib/stats'
import { makeLink } from '../test/fixtures'
import { enabledReaderCapabilityPolicy } from '../test/capabilities'

function fakeClient(): ReaderClient {
  return {
    searchLibrary: vi.fn(
      async (): Promise<ApiResult<GroupedSearchResponse>> => ({
        ok: true,
        data: { reading: { items: [], total_hint: 0 }, sites: { items: [], total_hint: 0 } },
      }),
    ),
    isIdentityCurrent: vi.fn(() => true),
  } as unknown as ReaderClient
}

const tagStats: TagStat[] = [
  { tag: 'LLM', count: 8, lastAt: '2026-06-01T00:00:00Z' },
  { tag: 'rust', count: 3, lastAt: '2026-05-01T00:00:00Z' },
]
const domainStats: DomainStat[] = [{ domain: 'arxiv.org', count: 5, lastAt: '2026-06-01T00:00:00Z' }]

function renderCP(props: Partial<React.ComponentProps<typeof CommandPalette>> = {}) {
  const onCommand = vi.fn()
  const onClose = vi.fn()
  const rendered = render(
    <CommandPalette
      open
      onClose={onClose}
      onCommand={onCommand}
      client={fakeClient()}
      corpus={[]}
      tagStats={tagStats}
      domainStats={domainStats}
      capabilityPolicy={enabledReaderCapabilityPolicy()}
      {...props}
    />,
  )
  return { onCommand, onClose, ...rendered }
}

describe('CommandPalette', () => {
  it('open=false 不渲染', () => {
    const { container } = render(
      <CommandPalette
        open={false}
        onClose={() => {}}
        onCommand={() => {}}
        client={fakeClient()}
        corpus={[]}
        tagStats={[]}
        domainStats={[]}
      />,
    )
    expect(container.firstChild).toBeNull()
  })

  it('默认展示命令组（视图等）', () => {
    renderCP()
    expect(screen.getByText('全部链接')).toBeInTheDocument()
    expect(screen.getByText('打开 AI 助手')).toBeInTheDocument()
  })

  it('only exposes one capability-gated new-note command for empty and matching queries', () => {
    const { onCommand } = renderCP({ canCreateNote: true })
    expect(screen.getAllByText('新建笔记')).toHaveLength(1)
    const input = screen.getByPlaceholderText(/搜索标题/)
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(onCommand).toHaveBeenCalledWith(expect.objectContaining({ id: 'create-note' }))

    fireEvent.change(input, { target: { value: '新建笔记' } })
    expect(screen.getAllByText('新建笔记')).toHaveLength(1)
  })

  it('does not expose a write command when note capability is false or unknown', () => {
    const first = renderCP()
    expect(screen.queryByText('新建笔记')).not.toBeInTheDocument()
    first.unmount()
    renderCP({ canCreateNote: false })
    expect(screen.queryByText('新建笔记')).not.toBeInTheDocument()
  })

  it('mouse execution invokes the same new-note command', () => {
    const { onCommand } = renderCP({ canCreateNote: true })
    fireEvent.click(screen.getByText('新建笔记'))
    expect(onCommand).toHaveBeenCalledWith(expect.objectContaining({ id: 'create-note' }))
  })

  it('在空查询和收件箱查询中都只暴露一个收件箱命令', () => {
    const { onCommand, unmount } = renderCP()
    expect(screen.getAllByText('收件箱')).toHaveLength(1)
    const input = screen.getByPlaceholderText(/搜索标题/)
    fireEvent.change(input, { target: { value: '收件箱' } })
    expect(screen.getAllByText('收件箱')).toHaveLength(1)
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(onCommand).toHaveBeenCalledWith(expect.objectContaining({ id: 'pending' }))

    unmount()
    const mouse = renderCP()
    fireEvent.change(screen.getByPlaceholderText(/搜索标题/), { target: { value: '收件箱' } })
    fireEvent.click(screen.getByText('收件箱'))
    expect(mouse.onCommand).toHaveBeenCalledWith(expect.objectContaining({ id: 'pending' }))
  })

  it('# 前缀只搜标签', () => {
    renderCP()
    fireEvent.change(screen.getByPlaceholderText(/搜索标题/), { target: { value: '#ll' } })
    expect(screen.getByText('LLM')).toBeInTheDocument()
    // 命令不应出现（# 模式只返回标签）。
    expect(screen.queryByText('打开 AI 助手')).toBeNull()
  })

  it('非 # 输入匹配标签 + 域名', () => {
    renderCP()
    fireEvent.change(screen.getByPlaceholderText(/搜索标题/), { target: { value: 'arxiv' } })
    expect(screen.getByText('arxiv.org')).toBeInTheDocument()
  })

  it('阅读搜索不从链接状态生成已退休的状态命令', () => {
    renderCP({
      corpus: [
        makeLink({ id: 'pending', title: '待读文章', status: 'pending' }),
        makeLink({ id: 'failed', title: '失败文章', status: 'failed', error_msg: '抓取超时' }),
        makeLink({
          id: 'low-confidence',
          title: '低置信文章',
          is_low_confidence: true,
          low_confidence_reason: 'thin_content',
        }),
      ],
    })
    const input = screen.getByPlaceholderText(/搜索标题/)

    expect(screen.queryByText('处理中')).not.toBeInTheDocument()
    expect(screen.queryByText('解析失败')).not.toBeInTheDocument()
    expect(screen.queryByText('低置信度')).not.toBeInTheDocument()

    fireEvent.change(input, { target: { value: '文章' } })
    expect(screen.getByText('待读文章')).toBeInTheDocument()
    expect(screen.getByText('失败文章')).toBeInTheDocument()
    expect(screen.getByText('低置信文章')).toBeInTheDocument()
    expect(screen.queryByText('处理中')).not.toBeInTheDocument()
    expect(screen.queryByText('解析失败')).not.toBeInTheDocument()
    expect(screen.queryByText('低置信度')).not.toBeInTheDocument()
  })

  it('后端网站命中显示网站分组', async () => {
    vi.useFakeTimers()
    const client = {
      searchLibrary: vi.fn(async (): Promise<ApiResult<GroupedSearchResponse>> => ({
        ok: true,
        data: {
          reading: { items: [], total_hint: 0 },
          sites: {
            total_hint: 1,
            items: [{ id: 'site-1', name: 'Example Docs', matched_entries: [{ id: 'entry-1', name: 'Guide', url: 'https://example.com/guide' }] }],
          },
        },
      })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    try {
      renderCP({ client })
      fireEvent.change(screen.getByPlaceholderText(/搜索标题/), { target: { value: 'docs' } })
      await act(async () => { await vi.advanceTimersByTimeAsync(300) })
      expect(screen.getByText('Example Docs')).toBeInTheDocument()
      expect(screen.getByText('网站')).toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })

  it('后端想法/笔记命中只展示发布态摘要', async () => {
    vi.useFakeTimers()
    const client = {
      searchLibrary: vi.fn(async (): Promise<ApiResult<GroupedSearchResponse>> => ({
        ok: true,
        data: {
          reading: { items: [], total_hint: 0 },
          sites: { items: [], total_hint: 0 },
          thoughts: {
            total_hint: 1,
            items: [{ id: 'thought-1', host_kind: 'link', host_id: 'link-1', link_id: 'link-1', snippet: '一个公开想法', updated_at: '2026-08-09T00:00:00Z' }],
          },
          notes: {
            total_hint: 1,
            items: [{ id: 'note-1', title: '一篇公开笔记', snippet: '发布内容摘要', published_revision: 3, updated_at: '2026-08-09T00:00:00Z' }],
          },
        },
      })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    try {
      renderCP({ client })
      fireEvent.change(screen.getByPlaceholderText(/搜索标题/), { target: { value: '公开' } })
      await act(async () => { await vi.advanceTimersByTimeAsync(300) })
      expect(screen.getByText('一个公开想法')).toBeInTheDocument()
      expect(screen.getByText('一篇公开笔记')).toBeInTheDocument()
      expect(screen.queryByText('draft_content')).toBeNull()
      expect(screen.getByText('想法')).toBeInTheDocument()
      expect(screen.getByText('笔记')).toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })

  it('不展示无法映射到 canonical host target 的想法', async () => {
    vi.useFakeTimers()
    const client = {
      searchLibrary: vi.fn(async (): Promise<ApiResult<GroupedSearchResponse>> => ({
        ok: true,
        data: {
          reading: { items: [], total_hint: 0 },
          sites: { items: [], total_hint: 0 },
          thoughts: {
            total_hint: 1,
            items: [{ id: 'thought-unknown', host_kind: 'unsupported', host_id: 'host-1', snippet: '不可回跳想法', updated_at: '2026-08-09T00:00:00Z' }],
          },
        },
      })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    try {
      renderCP({ client })
      fireEvent.change(screen.getByPlaceholderText(/搜索标题/), { target: { value: '不可' } })
      await act(async () => { await vi.advanceTimersByTimeAsync(300) })
      expect(screen.queryByText('不可回跳想法')).not.toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })

  it('filters thought results whose canonical host route is unavailable', async () => {
    vi.useFakeTimers()
    const client = {
      searchLibrary: vi.fn(async (): Promise<ApiResult<GroupedSearchResponse>> => ({
        ok: true,
        data: {
          reading: { items: [], total_hint: 0 },
          sites: { items: [], total_hint: 0 },
          thoughts: {
            total_hint: 3,
            items: [
              { id: 'thought-link', host_kind: 'link', host_id: 'L1', link_id: 'L1', snippet: '可用链接想法', updated_at: '2026-08-09T00:00:00Z' },
              { id: 'thought-note', host_kind: 'note', host_id: 'N1', link_id: null, snippet: '禁用笔记想法', updated_at: '2026-08-09T00:00:00Z' },
              { id: 'thought-inbox', host_kind: 'inbox', host_id: 'I1', link_id: null, snippet: '禁用待确认想法', updated_at: '2026-08-09T00:00:00Z' },
            ],
          },
        },
      })),
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    try {
      renderCP({
        client,
        capabilityPolicy: enabledReaderCapabilityPolicy({ notes: false, inbox: false }),
      })
      fireEvent.change(screen.getByPlaceholderText(/搜索标题/), { target: { value: '想法' } })
      await act(async () => { await vi.advanceTimersByTimeAsync(300) })

      expect(screen.getByText('可用链接想法')).toBeInTheDocument()
      expect(screen.queryByText('禁用笔记想法')).not.toBeInTheDocument()
      expect(screen.queryByText('禁用待确认想法')).not.toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })

  it('加载更多想法命令在面板内分页并按 ID 去重', async () => {
    vi.useFakeTimers()
    const first: ApiResult<GroupedSearchResponse> = {
      ok: true,
      data: {
        reading: { items: [], total_hint: 0 },
        sites: { items: [], total_hint: 0 },
        thoughts: {
          total_hint: 21,
          next_cursor: 'opaque-page-2',
          items: [
            { id: 'thought-1', host_kind: 'link', host_id: 'link-1', snippet: '第一页想法', updated_at: '2026-08-10T00:00:00Z' },
            { id: 'thought-2', host_kind: 'link', host_id: 'link-2', snippet: '第一页想法 2', updated_at: '2026-08-10T00:00:00Z' },
            { id: 'thought-3', host_kind: 'link', host_id: 'link-3', snippet: '第一页想法 3', updated_at: '2026-08-10T00:00:00Z' },
            { id: 'thought-4', host_kind: 'link', host_id: 'link-4', snippet: '第一页想法 4', updated_at: '2026-08-10T00:00:00Z' },
            { id: 'thought-5', host_kind: 'link', host_id: 'link-5', snippet: '第一页想法 5', updated_at: '2026-08-10T00:00:00Z' },
          ],
        },
      },
    }
    const second: ApiResult<GroupedSearchResponse> = {
      ok: true,
      data: {
        reading: { items: [], total_hint: 0 },
        sites: { items: [], total_hint: 0 },
        thoughts: {
          total_hint: 21,
          items: [
            { id: 'thought-5', host_kind: 'link', host_id: 'link-5', snippet: '重复边界想法', updated_at: '2026-08-10T00:00:00Z' },
            { id: 'thought-6', host_kind: 'link', host_id: 'link-6', snippet: '第二页想法', updated_at: '2026-08-09T00:00:00Z' },
          ],
        },
      },
    }
    const searchLibrary = vi.fn()
    searchLibrary.mockResolvedValueOnce(first).mockResolvedValueOnce(second)
    const client = { searchLibrary, isIdentityCurrent: vi.fn(() => true) } as unknown as ReaderClient
    try {
      const { onCommand, onClose } = renderCP({ client })
      fireEvent.change(screen.getByPlaceholderText(/搜索标题/), { target: { value: '分页' } })
      await act(async () => { await vi.advanceTimersByTimeAsync(300) })
      expect(screen.getByText('第一页想法')).toBeInTheDocument()
      expect(screen.getByText('更多想法')).toBeInTheDocument()

      await act(async () => {
        fireEvent.click(screen.getByText('更多想法'))
        await Promise.resolve()
      })
      expect(searchLibrary).toHaveBeenNthCalledWith(1, '分页', 50, 10, 20)
      expect(searchLibrary).toHaveBeenNthCalledWith(2, '分页', 50, 10, 20, 'opaque-page-2')
      expect(screen.getByText('第二页想法')).toBeInTheDocument()
      expect(screen.getAllByText('第一页想法')).toHaveLength(1)
      expect(onCommand).not.toHaveBeenCalled()
      expect(onClose).not.toHaveBeenCalled()
    } finally {
      vi.useRealTimers()
    }
  })

  it('键盘下 + 回车执行选中项', () => {
    const { onCommand } = renderCP()
    const input = screen.getByPlaceholderText(/搜索标题/)
    // 默认无搜索，第一项为「全部链接」(nav:all)。回车直接执行。
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(onCommand).toHaveBeenCalledWith(expect.objectContaining({ id: 'nav:all' }))
  })

  it('Esc 关闭', () => {
    const { onClose } = renderCP()
    fireEvent.keyDown(screen.getByPlaceholderText(/搜索标题/), { key: 'Escape' })
    expect(onClose).toHaveBeenCalled()
  })

  it('点标签项执行 tag: 命令', () => {
    const { onCommand } = renderCP()
    fireEvent.change(screen.getByPlaceholderText(/搜索标题/), { target: { value: '#LLM' } })
    fireEvent.click(screen.getByText('LLM'))
    expect(onCommand).toHaveBeenCalledWith(expect.objectContaining({ id: 'tag:LLM' }))
  })
})
