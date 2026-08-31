import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { TocHeading } from '../../lib/toc'
import { hasRenderableOutline } from '../../lib/outline-tree'
import { OutlineTree } from './OutlineTree'

const headings: TocHeading[] = [
  { id: 'h1', level: 1, text: '开头' },
  { id: 'h2', level: 2, text: '中段' },
  { id: 'h3', level: 3, text: '细节' },
]

describe('OutlineTree', () => {
  it('renders one keyboard-focusable navigation item per heading with shared active state and hierarchy', () => {
    render(
      <nav aria-label="正文目录">
        <OutlineTree items={headings} activeId="h2" onJump={vi.fn()} variant="reader" />
      </nav>,
    )

    const toc = screen.getByRole('navigation', { name: '正文目录' })
    const items = within(toc).getAllByRole('button')
    expect(items.map((item) => item.textContent)).toEqual(['开头', '中段', '细节'])
    expect(items.map((item) => item.getAttribute('style'))).toEqual([
      'padding-inline-start: 0;',
      'padding-inline-start: 11px;',
      'padding-inline-start: 22px;',
    ])
    expect(screen.getByRole('button', { name: '中段' })).toHaveAttribute('aria-current', 'true')
  })

  it('keeps untitled and deep headings navigable without exposing caller-side branches', () => {
    const onJump = vi.fn()
    const onAfterJump = vi.fn()
    render(
      <nav aria-label="移动正文目录">
        <OutlineTree
          items={[
            { id: 'blank', level: 6, text: '   ' },
            { id: 'deep', level: 9, text: '深层标题' },
            { id: 'normal', level: 1, text: '普通标题' },
          ]}
          activeId="blank"
          onJump={onJump}
          variant="menu"
          onAfterJump={onAfterJump}
        />
      </nav>,
    )

    const untitled = screen.getByRole('button', { name: '未命名标题' })
    expect(untitled).toHaveAttribute('title', '未命名标题')
    expect(untitled).toHaveAttribute('aria-current', 'true')
    expect(untitled).toHaveStyle({ paddingInlineStart: '57px' })
    expect(screen.getByRole('button', { name: '深层标题' })).toHaveStyle({ paddingInlineStart: '57px' })

    fireEvent.click(screen.getByRole('button', { name: '普通标题' }))
    expect(onJump).toHaveBeenCalledWith('normal')
    expect(onAfterJump).toHaveBeenCalledTimes(1)
  })

  it('uses the same minimum item threshold for rail, reader, and compact outlines', () => {
    expect(hasRenderableOutline(headings.slice(0, 2))).toBe(false)
    expect(hasRenderableOutline(headings)).toBe(true)
  })
})
