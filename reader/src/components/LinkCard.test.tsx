import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { LinkCard } from './LinkCard'
import { makeLink } from '../test/fixtures'

describe('LinkCard 状态变体', () => {
  it('显示摘要与相对时间，不显示状态徽标', () => {
    const { container } = render(<LinkCard l={makeLink({ status: 'done', summary: '摘要内容' })} active={false} onSelect={() => {}} />)
    expect(screen.getByText('摘要内容')).toBeInTheDocument()
    expect(container.querySelector('.card-time')).toHaveTextContent('06-10')
    expect(container.querySelector('.st-badge')).toBeNull()
  })

  it.each([
    ['pending', { status: 'pending' as const }],
    ['processing', { status: 'processing' as const }],
    ['failed', { status: 'failed' as const, error_msg: '抓取超时' }],
    ['low confidence', { is_low_confidence: true, low_confidence_reason: 'thin_content' as const }],
  ])('%s 只按普通卡片内容渲染', (_, overrides) => {
    const { container } = render(<LinkCard l={makeLink(overrides)} active={false} onSelect={() => {}} />)
    expect(container.querySelector('.card-time')).toHaveTextContent('06-10')
    expect(screen.queryByText('排队等待解析…')).not.toBeInTheDocument()
    expect(screen.queryByText('正在抓取页面并生成摘要…')).not.toBeInTheDocument()
    expect(screen.queryByText('抓取超时')).not.toBeInTheDocument()
    expect(screen.queryByText('低置信')).not.toBeInTheDocument()
    expect(container.querySelector('.card-err')).toBeNull()
    expect(container.querySelector('.st-badge')).toBeNull()
  })

  it('无 title：以 mono URL 降级显示', () => {
    render(
      <LinkCard l={makeLink({ title: null, url: 'https://foo.bar/x' })} active={false} onSelect={() => {}} />,
    )
    const h = screen.getByText('foo.bar/x')
    expect(h).toHaveClass('card-url')
  })
})
