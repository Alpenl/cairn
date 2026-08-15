import type { ComponentProps } from 'react'
import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { FeedSubscription, SubscriptionsResponse } from '../../lib/api/types'
import { readerIdentity } from '../../lib/identity'
import { ALL_FEEDS_SELECTION } from './model'
import { SubscriptionSidebar } from './SubscriptionSidebar'
import { enabledReaderCapabilityPolicy } from '../../test/capabilities'

const subscription: FeedSubscription = {
  id: 'feed-1',
  feed_url: 'https://example.com/atom.xml',
  title: '工程周刊',
  folder_id: 'folder-1',
  unread_count: 1,
  item_count: 1,
}

const data: SubscriptionsResponse = {
  folders: [{ id: 'folder-1', name: '技术' }],
  subscriptions: [subscription],
  counts: { all: 1, unread: 1, starred: 0, later: 0 },
}

beforeEach(() => {
  localStorage.clear()
})

type SidebarProps = ComponentProps<typeof SubscriptionSidebar>

function renderSidebar(overrides: Partial<SidebarProps> = {}) {
  const props: SidebarProps = {
    data,
    selection: ALL_FEEDS_SELECTION,
    loading: false,
    onSelect: vi.fn(),
    onView: vi.fn(),
    collapsed: false,
    onAddSubscription: vi.fn(),
    onAddFolder: vi.fn(),
    onEditFolder: vi.fn(),
    onDeleteFolder: vi.fn(),
    onMoveSubscription: vi.fn(),
    onRefreshSubscription: vi.fn(),
    onDeleteSubscription: vi.fn(),
    onBatchRefresh: vi.fn(),
    onBatchMove: vi.fn(),
    onBatchDelete: vi.fn(),
    onDeleteFailed: vi.fn(),
    onImportOPML: vi.fn(),
    onExportOPML: vi.fn(),
    capabilityPolicy: enabledReaderCapabilityPolicy(),
    ...overrides,
  }
  return render(<SubscriptionSidebar {...props} />)
}

describe('SubscriptionSidebar', () => {
  it('文件夹可折叠并重新展开其中的订阅源', () => {
    renderSidebar()

    expect(screen.getByRole('button', { name: /^工程周刊/ })).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '折叠文件夹 技术' }))
    expect(screen.queryByRole('button', { name: /^工程周刊/ })).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '展开文件夹 技术' }))
    expect(screen.getByRole('button', { name: /^工程周刊/ })).toBeInTheDocument()
  })

  it('按文件夹 ID 记住折叠状态并在重新挂载后恢复', () => {
    const first = renderSidebar()

    fireEvent.click(screen.getByRole('button', { name: '折叠文件夹 技术' }))
    expect(screen.queryByRole('button', { name: /^工程周刊/ })).not.toBeInTheDocument()
    first.unmount()

    renderSidebar()
    expect(screen.getByRole('button', { name: '展开文件夹 技术' })).toHaveAttribute(
      'aria-expanded',
      'false',
    )
    expect(screen.queryByRole('button', { name: /^工程周刊/ })).not.toBeInTheDocument()
  })

  it('does not expose A folder folds to B and restores them after returning to A', () => {
    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    const firstA = renderSidebar()
    fireEvent.click(screen.getByRole('button', { name: '折叠文件夹 技术' }))
    firstA.unmount()

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    const pageB = renderSidebar()
    expect(screen.getByRole('button', { name: /^工程周刊/ })).toBeInTheDocument()
    pageB.unmount()

    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    renderSidebar()
    expect(screen.queryByRole('button', { name: /^工程周刊/ })).not.toBeInTheDocument()
  })

  it('对连续失败的订阅源提供一键清理入口', async () => {
    const user = userEvent.setup()
    const onDeleteFailed = vi.fn()
    const brokenData: SubscriptionsResponse = {
      ...data,
      subscriptions: [
        {
          ...subscription,
          last_error: 'HTTP 410 Gone',
          failure_count: 3,
        },
      ],
    }

    renderSidebar({ data: brokenData, onDeleteFailed })

    await user.click(screen.getByRole('button', { name: '清理 1 个失效源' }))
    expect(onDeleteFailed).toHaveBeenCalledWith(brokenData.subscriptions)
  })

  it('提供可选择订阅源的批量管理模式', async () => {
    const user = userEvent.setup()
    const onBatchRefresh = vi.fn()
    const onBatchMove = vi.fn()
    const onBatchDelete = vi.fn()
    renderSidebar({ onBatchRefresh, onBatchMove, onBatchDelete })

    await user.click(screen.getByRole('button', { name: '批量管理' }))

    expect(screen.getByRole('checkbox', { name: '选择订阅源 工程周刊' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '全选订阅源' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '批量刷新' })).toBeInTheDocument()
    expect(screen.getByLabelText('批量移动')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '批量删除' })).toBeInTheDocument()

    await user.click(screen.getByRole('checkbox', { name: '选择订阅源 工程周刊' }))
    await user.click(screen.getByRole('button', { name: '批量刷新' }))
    expect(onBatchRefresh).toHaveBeenCalledWith([subscription])

    const moveMenu = screen.getByLabelText('批量移动').closest('details')
    expect(moveMenu).not.toBeNull()
    await user.click(screen.getByLabelText('批量移动'))
    await user.click(within(moveMenu as HTMLElement).getByRole('button', { name: '未分组' }))
    expect(onBatchMove).toHaveBeenCalledWith([subscription], null)

    await user.click(screen.getByRole('button', { name: '批量删除' }))
    expect(onBatchDelete).toHaveBeenCalledWith([subscription])
  })

  it('文件夹和订阅源三点菜单点击外部区域后关闭', async () => {
    const user = userEvent.setup()
    renderSidebar()

    const sourceSummary = screen.getByLabelText('管理订阅源 工程周刊')
    const sourceMenu = sourceSummary.closest('details')
    await user.click(sourceSummary)
    expect(sourceMenu).toHaveAttribute('open')
    await user.click(document.body)
    expect(sourceMenu).not.toHaveAttribute('open')

    const folderSummary = screen.getByLabelText('管理文件夹 技术')
    const folderMenu = folderSummary.closest('details')
    await user.click(folderSummary)
    expect(folderMenu).toHaveAttribute('open')
    await user.click(document.body)
    expect(folderMenu).not.toHaveAttribute('open')
  })
})
