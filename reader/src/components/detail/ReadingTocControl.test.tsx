import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'

import { ReadingTocControl } from './ReadingTocControl'
import type { TocHeading } from '../../lib/toc'

const headings: TocHeading[] = [
  { id: 'h1', level: 1, text: '开头' },
  { id: 'h2', level: 2, text: '中段' },
  { id: 'h3', level: 3, text: '细节' },
]

const originalInnerWidth = window.innerWidth

afterEach(() => {
  window.innerWidth = originalInnerWidth
})

describe('ReadingTocControl', () => {
  it('在窄屏提供可达目录入口，并把跳转交给正文 owner', async () => {
    window.innerWidth = 390
    const onJump = vi.fn()

    render(<ReadingTocControl items={headings} activeId="h2" onJump={onJump} />)

    fireEvent.click(await screen.findByRole('button', { name: '正文目录' }))
    const menu = screen.getByRole('navigation', { name: '移动正文目录' })
    const items = screen.getAllByRole('button')
    expect(items).toHaveLength(4)
    expect(screen.getByRole('button', { name: '中段' })).toHaveAttribute('aria-current', 'true')

    fireEvent.click(screen.getByRole('button', { name: '细节' }))
    expect(onJump).toHaveBeenCalledWith('h3')
    expect(menu).not.toBeInTheDocument()
  })

  it('少于三条标题时不占用窄屏工具栏入口', () => {
    window.innerWidth = 390

    render(
      <ReadingTocControl
        items={headings.slice(0, 2)}
        activeId={null}
        onJump={vi.fn()}
      />,
    )

    expect(screen.queryByRole('button', { name: '正文目录' })).not.toBeInTheDocument()
  })
})
