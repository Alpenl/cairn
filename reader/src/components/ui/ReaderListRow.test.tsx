import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ReactElement } from 'react'
import { ReaderListRow } from './ReaderListRow'

function row(): HTMLElement {
  const element = document.querySelector<HTMLElement>('.reader-list-row')
  if (!element) throw new Error('行未渲染')
  return element
}

function renderRow(ui: ReactElement) {
  return render(<ul>{ui}</ul>)
}

describe('ReaderListRow 结构', () => {
  it('始终渲染 li，并按 variant 给出密度 class', () => {
    const { unmount } = renderRow(<ReaderListRow variant="feed" title="标题" />)
    expect(row().tagName).toBe('LI')
    expect(row()).toHaveClass('reader-list-row', 'reader-list-row-feed')
    unmount()

    renderRow(<ReaderListRow variant="dense" title="标题" className="rvx-trash-row" />)
    expect(row()).toHaveClass('reader-list-row-dense', 'rvx-trash-row')
  })

  it('把 source、meta、summary、details、footer 和 actions 放进固定分区', () => {
    renderRow(
      <ReaderListRow
        variant="feed"
        source={<span className="rvx-source-chip">订阅</span>}
        meta={<span className="rvx-muted">分段：今天</span>}
        title="标题"
        summary="摘要内容"
        details={<p role="note">推荐原因</p>}
        footer={<time dateTime="2026-08-17">刚刚</time>}
        actions={<button type="button">保存</button>}
      />,
    )

    expect(document.querySelector('.reader-list-row-source')).toHaveTextContent('订阅')
    expect(document.querySelector('.reader-list-row-meta')).toHaveTextContent('分段：今天')
    expect(document.querySelector('.reader-list-row-summary')).toHaveTextContent('摘要内容')
    expect(document.querySelector('.reader-list-row-details')).toHaveTextContent('推荐原因')
    expect(document.querySelector('.reader-list-row-footer')).toHaveTextContent('刚刚')
    expect(screen.getByRole('button', { name: '保存' }).closest('.reader-list-row-actions')).not.toBeNull()
  })

  it('省略可选内容时不渲染空的分区节点', () => {
    renderRow(<ReaderListRow variant="compact" title="标题" />)

    expect(document.querySelector('.reader-list-row-meta')).toBeNull()
    expect(document.querySelector('.reader-list-row-summary')).toBeNull()
    expect(document.querySelector('.reader-list-row-details')).toBeNull()
    expect(document.querySelector('.reader-list-row-foot')).toBeNull()
  })

  it('把 dataAttributes 原样挂在 li 上，供滚动锚点定位', () => {
    renderRow(
      <ReaderListRow
        variant="feed"
        title="标题"
        dataAttributes={{ 'data-feed-item-key': 'item-1', 'data-resource-key': 'link:1' }}
      />,
    )

    expect(row()).toHaveAttribute('data-feed-item-key', 'item-1')
    expect(row()).toHaveAttribute('data-resource-key', 'link:1')
  })
})

describe('ReaderListRow 打开行为', () => {
  it('给了 onOpen 就用真实 button 承载点击与键盘打开', async () => {
    const user = userEvent.setup()
    const onOpen = vi.fn()
    renderRow(<ReaderListRow variant="feed" title="标题" onOpen={onOpen} />)
    const title = screen.getByRole('button', { name: '标题' })

    expect(title).toHaveClass('reader-list-row-title')
    await user.click(title)
    expect(onOpen).toHaveBeenCalledTimes(1)

    title.focus()
    await user.keyboard('{Enter}')
    expect(onOpen).toHaveBeenCalledTimes(2)

    await user.keyboard(' ')
    expect(onOpen).toHaveBeenCalledTimes(3)
  })

  it('没有 onOpen 时降级成 strong，不留一个点不动的按钮', () => {
    renderRow(<ReaderListRow variant="feed" title="标题" />)

    expect(screen.queryByRole('button')).toBeNull()
    const title = screen.getByText('标题')
    expect(title.tagName).toBe('STRONG')
    expect(title).toHaveClass('reader-list-row-title')
  })

  it('openLabel 覆盖打开按钮的无障碍名称', () => {
    renderRow(<ReaderListRow variant="dense" title="标题" openLabel="恢复 标题" onOpen={vi.fn()} />)

    expect(screen.getByRole('button', { name: '恢复 标题' })).toBeInTheDocument()
  })

  it('busy 时禁用打开按钮并标记行状态', async () => {
    const user = userEvent.setup()
    const onOpen = vi.fn()
    renderRow(<ReaderListRow variant="dense" title="标题" busy onOpen={onOpen} />)
    const title = screen.getByRole('button', { name: '标题' })

    expect(row()).toHaveClass('is-busy')
    expect(title).toBeDisabled()
    await user.click(title)
    expect(onOpen).not.toHaveBeenCalled()
  })
})
