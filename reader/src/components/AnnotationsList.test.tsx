import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import { AnnotationsList } from './AnnotationsList'
import type { Annotation } from '../lib/annotation-domain'

function mkAnn(over: Partial<Annotation> = {}): Annotation {
  return {
    id: 'a',
    blockKey: 'summary',
    start: 0,
    end: 4,
    text: 't',
    note: '',
    source: 'self',
    createdAt: 1,
    updatedAt: 1,
    ...over,
  }
}

describe('AnnotationsList', () => {
  it('空列表不渲染', () => {
    const { container } = render(<AnnotationsList anns={[]} onOpen={() => {}} onDelete={() => {}} />)
    expect(container.firstChild).toBeNull()
  })

  it('按 annOrder + start 排序：summary 在前，正文次之，历史遗留块靠后', () => {
    // 'dr' 是已下线的深度调研块，历史划线还可能留在 localStorage 里，排最后。
    const anns = [
      mkAnn({ id: 'old', blockKey: 'dr', text: 'OLD' }),
      mkAnn({ id: 'body', blockKey: 'content-document', text: 'BODY' }),
      mkAnn({ id: 's', blockKey: 'summary', text: 'SUM' }),
    ]
    render(<AnnotationsList anns={anns} onOpen={() => {}} onDelete={() => {}} />)
    const quotes = screen.getAllByText(/SUM|BODY|OLD/).map((n) => n.textContent)
    expect(quotes).toEqual(['SUM', 'BODY', 'OLD'])
  })

  it('AI 笔记带 AI 标签，thought Markdown 渲染加粗', () => {
    render(
      <AnnotationsList
        anns={[mkAnn({ source: 'ai', note: '看 **重点**' })]}
        onOpen={() => {}}
        onDelete={() => {}}
      />,
    )
    expect(screen.getByText('AI')).toBeInTheDocument()
    expect(screen.getByText('重点').tagName).toBe('STRONG')
  })

  it('用完整 Markdown/GFM 渲染想法，且不创建递归 annotation host', () => {
    const { container } = render(
      <AnnotationsList
        anns={[mkAnn({
          note: [
            '## 后续',
            '',
            'Use _emphasis_, ~~removed~~, and `code`.',
            '',
            '- [x] done',
            '- [ ] next',
            '',
            '[reference](https://example.com)',
          ].join('\n'),
        })]}
        onOpen={() => {}}
        onDelete={() => {}}
      />,
    )

    expect(screen.getByRole('heading', { name: '后续' })).toBeInTheDocument()
    expect(screen.getByText('emphasis').tagName).toBe('EM')
    expect(screen.getByText('removed').tagName).toBe('DEL')
    expect(screen.getByText('code').tagName).toBe('CODE')
    expect(screen.getByRole('link', { name: 'reference' })).toHaveAttribute('href', 'https://example.com')
    expect(screen.getAllByRole('checkbox')).toHaveLength(2)
    expect(container.querySelector('[data-hl-block]')).not.toBeInTheDocument()
  })

  it('点 item 触发 onOpen，点删除触发 onDelete 不冒泡', () => {
    const onOpen = vi.fn()
    const onDelete = vi.fn()
    render(<AnnotationsList anns={[mkAnn({ id: 'x', text: '引文' })]} onOpen={onOpen} onDelete={onDelete} />)
    fireEvent.click(screen.getByText('引文'))
    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({ id: 'x', blockKey: 'summary' }))
    fireEvent.click(screen.getByTitle('删除'))
    expect(onDelete).toHaveBeenCalledWith(expect.objectContaining({ id: 'x', blockKey: 'summary' }))
  })
})
