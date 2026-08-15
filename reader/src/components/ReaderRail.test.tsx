import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ReaderRail } from './ReaderRail'
import { annotationLocator, type Annotation, type AnnotationLocator } from '../lib/annotations'
import type { TocHeading } from '../lib/toc'

function annotation(
  over: Partial<Annotation> & Pick<Annotation, 'id' | 'text'>,
): Annotation {
  return {
    blockKey: 'content',
    start: 0,
    end: over.text.length,
    note: '',
    source: 'self',
    createdAt: 1,
    updatedAt: 1,
    sourceContentRevision: 4,
    ...over,
  }
}

function railProps(over: Partial<React.ComponentProps<typeof ReaderRail>> = {}) {
  return {
    tags: ['LLM'],
    relatedTags: ['RAG'],
    currentTag: 'LLM',
    onPickTag: vi.fn(),
    progress: 42,
    readMinutes: 7,
    tocItems: [],
    activeTocId: null,
    onJumpToc: vi.fn(),
    annotations: [],
    onOpenAnnotation: vi.fn(),
    ...over,
  }
}

describe('ReaderRail 标签与进度', () => {
  it('渲染文章标签、相关标签，并把点击交给筛选回调', () => {
    const onPickTag = vi.fn()
    render(<ReaderRail {...railProps({ onPickTag })} />)

    expect(screen.getByRole('button', { name: '#LLM' })).toHaveClass('cur')
    fireEvent.click(screen.getByRole('button', { name: '≈ RAG' }))
    expect(onPickTag).toHaveBeenCalledWith('RAG')
  })

  it('显示剩余阅读时间，并把进度限制在 0 到 100', () => {
    const { rerender } = render(<ReaderRail {...railProps()} />)
    const progress = screen.getByRole('progressbar', { name: '阅读进度' })

    expect(progress).toHaveAttribute('aria-valuenow', '42')
    expect(screen.getByText('已读 42% · 剩 5 分钟')).toBeInTheDocument()

    rerender(<ReaderRail {...railProps({ progress: 180 })} />)
    expect(screen.getByRole('progressbar', { name: '阅读进度' })).toHaveAttribute('aria-valuenow', '100')
    expect(screen.getByText('已读完')).toBeInTheDocument()

    rerender(<ReaderRail {...railProps({ progress: -10 })} />)
    expect(screen.getByRole('progressbar', { name: '阅读进度' })).toHaveAttribute('aria-valuenow', '0')
  })
})

describe('ReaderRail 目录', () => {
  const tocItems: TocHeading[] = [
    { id: 'toc-1', level: 1, text: '开头' },
    { id: 'toc-2', level: 2, text: '中段' },
    { id: 'toc-3', level: 3, text: '末段' },
  ]

  it('至少有三个标题时显示目录并跳转', () => {
    const onJumpToc = vi.fn()
    render(<ReaderRail {...railProps({ tocItems, activeTocId: 'toc-1', onJumpToc })} />)

    const toc = screen.getByRole('navigation', { name: '正文目录' })
    expect(screen.getByRole('button', { name: '开头' })).toHaveAttribute('aria-current', 'true')
    fireEvent.click(screen.getByRole('button', { name: '中段' }))
    expect(onJumpToc).toHaveBeenCalledWith('toc-2')
    expect(toc).toBeInTheDocument()
  })

  it('少于三个标题时不显示目录', () => {
    render(
      <ReaderRail
        {...railProps({
          tocItems: tocItems.slice(0, 2),
        })}
      />,
    )
    expect(screen.queryByRole('navigation', { name: '正文目录' })).not.toBeInTheDocument()
  })
})

describe('ReaderRail 划线', () => {
  it('最多显示六条有效划线，标记 AI 和带笔记的条目', () => {
    const onOpenAnnotation = vi.fn<(locator: AnnotationLocator) => void>()
    const valid = Array.from({ length: 7 }, (_, index) =>
      annotation({ id: `ann-${index}`, text: `划线 ${index}` }),
    )
    valid[1] = annotation({
      id: 'ann-ai',
      text: 'AI 划线',
      source: 'ai',
      note: '需要回看',
    })
    const invalid = annotation({
      id: 'ann-unbound',
      text: '旧版划线',
      sourceContentRevision: undefined,
    })
    render(
      <ReaderRail
        {...railProps({
          tags: [],
          relatedTags: [],
          tocItems: [],
          annotations: [...valid, invalid],
          onOpenAnnotation,
        })}
      />,
    )

    const buttons = document.querySelectorAll('.reader-rail-annotation')
    expect(buttons).toHaveLength(6)
    expect(screen.getByText('AI 划线').closest('button')).toHaveClass('ai')
    expect(screen.getByText('有想法')).toBeInTheDocument()
    expect(screen.queryByText('划线 6')).not.toBeInTheDocument()
    expect(screen.queryByText('旧版划线')).not.toBeInTheDocument()

    fireEvent.click(screen.getByText('划线 0'))
    expect(onOpenAnnotation).toHaveBeenCalledWith(annotationLocator(valid[0]))
  })

  it('React key 包含 target identity，不合并不同 target 的同 ID 划线', () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => {})
    try {
      render(
        <ReaderRail
          {...railProps({
            tags: [],
            relatedTags: [],
            annotations: [
              annotation({ id: 'same-id', text: '正文划线' }),
              annotation({
                id: 'same-id',
                blockKey: 'summary',
                text: '摘要划线',
                sourceContentRevision: undefined,
                sourceSummaryHash: 'a'.repeat(64),
              }),
            ],
          })}
        />,
      )

      expect(document.querySelectorAll('.reader-rail-annotation')).toHaveLength(2)
      expect(error).not.toHaveBeenCalledWith(expect.stringContaining('same key'))
    } finally {
      error.mockRestore()
    }
  })
})
