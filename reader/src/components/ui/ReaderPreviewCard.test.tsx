import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ReaderPreviewCard } from './ReaderPreviewCard'

function card(): HTMLElement {
  const element = document.querySelector<HTMLElement>('.reader-preview-card')
  if (!element) throw new Error('卡片未渲染')
  return element
}

describe('ReaderPreviewCard 结构', () => {
  it('默认渲染 article，as="li" 时渲染 li', () => {
    const { unmount } = render(
      <ReaderPreviewCard source="example.com" title="标题" openLabel="打开 标题" onOpen={vi.fn()} />,
    )
    expect(card().tagName).toBe('ARTICLE')
    unmount()

    render(<ReaderPreviewCard as="li" source="example.com" title="标题" openLabel="打开 标题" onOpen={vi.fn()} />)
    expect(card().tagName).toBe('LI')
  })

  it('把来源、时间、标题、摘要和补充信息放进同一个 main button', () => {
    render(
      <ReaderPreviewCard
        source="example.com"
        time="06-10"
        title="标题"
        summary="摘要内容"
        details={<span className="mini-tag">#标签</span>}
        openLabel="打开 标题"
        onOpen={vi.fn()}
      />,
    )

    const main = screen.getByRole('button', { name: '打开 标题' })
    expect(main).toHaveClass('reader-preview-card-main')
    expect(main).toHaveTextContent('example.com')
    expect(main).toHaveTextContent('06-10')
    expect(main).toHaveTextContent('标题')
    expect(main).toHaveTextContent('摘要内容')
    expect(main).toHaveTextContent('#标签')
  })

  it('省略可选内容时不渲染对应节点', () => {
    render(<ReaderPreviewCard source="example.com" title="标题" openLabel="打开 标题" onOpen={vi.fn()} />)

    expect(document.querySelector('.reader-preview-card-time')).toBeNull()
    expect(document.querySelector('.reader-preview-card-summary')).toBeNull()
    expect(document.querySelector('.reader-preview-card-details')).toBeNull()
    expect(document.querySelector('.reader-preview-card-leading')).toBeNull()
    expect(document.querySelector('.reader-preview-card-actions')).toBeNull()
  })

  it('leading 与 actions 是 main button 的兄弟节点，不会嵌进打开区域', () => {
    render(
      <ReaderPreviewCard
        leading={<input type="checkbox" aria-label="选择 标题" />}
        actions={<button type="button">稍后读</button>}
        source="example.com"
        title="标题"
        openLabel="打开 标题"
        onOpen={vi.fn()}
      />,
    )

    const main = screen.getByRole('button', { name: '打开 标题' })
    const checkbox = screen.getByRole('checkbox', { name: '选择 标题' })
    const later = screen.getByRole('button', { name: '稍后读' })
    expect(main.contains(checkbox)).toBe(false)
    expect(main.contains(later)).toBe(false)
    expect(checkbox.closest('.reader-preview-card-leading')).not.toBeNull()
    expect(later.closest('.reader-preview-card-actions')).not.toBeNull()
  })
})

describe('ReaderPreviewCard 打开行为', () => {
  it('点击、Enter 和 Space 都由真实 button 触发打开', async () => {
    const user = userEvent.setup()
    const onOpen = vi.fn()
    render(<ReaderPreviewCard source="example.com" title="标题" openLabel="打开 标题" onOpen={onOpen} />)
    const main = screen.getByRole('button', { name: '打开 标题' })

    await user.click(main)
    expect(onOpen).toHaveBeenCalledTimes(1)

    main.focus()
    await user.keyboard('{Enter}')
    expect(onOpen).toHaveBeenCalledTimes(2)

    await user.keyboard(' ')
    expect(onOpen).toHaveBeenCalledTimes(3)
  })

  it('点击行内操作不会触发打开', async () => {
    const user = userEvent.setup()
    const onOpen = vi.fn()
    const onLater = vi.fn()
    render(
      <ReaderPreviewCard
        actions={<button type="button" onClick={onLater}>稍后读</button>}
        source="example.com"
        title="标题"
        openLabel="打开 标题"
        onOpen={onOpen}
      />,
    )

    await user.click(screen.getByRole('button', { name: '稍后读' }))

    expect(onLater).toHaveBeenCalledTimes(1)
    expect(onOpen).not.toHaveBeenCalled()
  })
})

describe('ReaderPreviewCard 状态映射', () => {
  it('默认不带任何状态 class 与 aria-current', () => {
    render(<ReaderPreviewCard source="example.com" title="标题" openLabel="打开 标题" onOpen={vi.fn()} />)

    const root = card()
    expect(root).toHaveClass('reader-preview-card')
    expect(root).not.toHaveClass('is-selected')
    expect(root).not.toHaveClass('is-read')
    expect(root).not.toHaveClass('is-picked')
    expect(root).not.toHaveAttribute('aria-current')
  })

  it('selected 同时给出 is-selected 与 aria-current', () => {
    render(<ReaderPreviewCard selected source="example.com" title="标题" openLabel="打开 标题" onOpen={vi.fn()} />)

    expect(card()).toHaveClass('is-selected')
    expect(card()).toHaveAttribute('aria-current', 'true')
  })

  it('read 与 picked 各自映射到 class，且不影响 aria-current', () => {
    render(
      <ReaderPreviewCard
        read
        picked
        className="rss-item-card"
        source="example.com"
        title="标题"
        openLabel="打开 标题"
        onOpen={vi.fn()}
      />,
    )

    const root = card()
    expect(root).toHaveClass('is-read', 'is-picked', 'rss-item-card')
    expect(root).not.toHaveClass('is-selected')
    expect(root).not.toHaveAttribute('aria-current')
  })
})
