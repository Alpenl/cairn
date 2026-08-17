import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { LinkCard } from './LinkCard'
import { makeLink } from '../test/fixtures'

describe('LinkCard 状态变体', () => {
  it('显示摘要与相对时间，不显示状态徽标', () => {
    const { container } = render(<LinkCard l={makeLink({ status: 'done', summary: '摘要内容' })} active={false} onSelect={() => {}} />)
    expect(screen.getByText('摘要内容')).toBeInTheDocument()
    expect(container.querySelector('.reader-preview-card-time')).toHaveTextContent('06-10')
    expect(container.querySelector('.st-badge')).toBeNull()
  })

  it.each([
    ['pending', { status: 'pending' as const }],
    ['processing', { status: 'processing' as const }],
    ['failed', { status: 'failed' as const, error_msg: '抓取超时' }],
    ['low confidence', { is_low_confidence: true, low_confidence_reason: 'thin_content' as const }],
  ])('%s 只按普通卡片内容渲染', (_, overrides) => {
    const { container } = render(<LinkCard l={makeLink(overrides)} active={false} onSelect={() => {}} />)
    expect(container.querySelector('.reader-preview-card-time')).toHaveTextContent('06-10')
    expect(screen.queryByText('排队等待解析…')).not.toBeInTheDocument()
    expect(screen.queryByText('正在抓取页面并生成摘要…')).not.toBeInTheDocument()
    expect(screen.queryByText('抓取超时')).not.toBeInTheDocument()
    expect(screen.queryByText('低置信')).not.toBeInTheDocument()
    expect(container.querySelector('.card-err')).toBeNull()
    expect(container.querySelector('.st-badge')).toBeNull()
  })

  it('无 title：以 mono URL 降级显示，并作为打开按钮的无障碍名称', () => {
    render(
      <LinkCard l={makeLink({ title: null, url: 'https://foo.bar/x' })} active={false} onSelect={() => {}} />,
    )
    const h = screen.getByText('foo.bar/x')
    expect(h).toHaveClass('card-url')
    expect(screen.getByRole('button', { name: '打开 foo.bar/x' })).toBeInTheDocument()
  })

  it('选中态映射到 is-selected 与 aria-current，业务 class 仍在根节点', () => {
    const { container } = render(
      <LinkCard l={makeLink({ title: '选中的一篇' })} active onSelect={() => {}} />,
    )
    const root = container.querySelector('.reader-preview-card')
    expect(root).toHaveClass('card', 'is-selected')
    expect(root).toHaveAttribute('aria-current', 'true')
  })
})

describe('LinkCard 打开行为', () => {
  it('点击、Enter 和 Space 都用链接 id 触发选中', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    const link = makeLink({ id: 'L9', title: '被打开的一篇' })
    render(<LinkCard l={link} active={false} onSelect={onSelect} />)

    // openLabel 成了 main button 的 aria-label，标题文本不再是无障碍名称。
    const open = screen.getByRole('button', { name: '打开 被打开的一篇' })
    await user.click(open)
    expect(onSelect).toHaveBeenCalledWith(link.id)

    open.focus()
    await user.keyboard('{Enter}')
    await user.keyboard(' ')
    expect(onSelect).toHaveBeenCalledTimes(3)
    expect(onSelect).toHaveBeenLastCalledWith(link.id)
  })
})
