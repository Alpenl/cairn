import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'

import { MarkdownView } from './MarkdownView'
import type { Annotation } from '../lib/annotations'

function ann(over: Partial<Annotation>): Annotation {
  return {
    id: 'a1',
    blockKey: 'summary',
    start: 0,
    end: 3,
    text: '',
    note: '',
    source: 'self',
    createdAt: 0,
    updatedAt: 0,
    sourceSummaryHash: 'a'.repeat(64),
    ...over,
  }
}

describe('MarkdownView', () => {
  it('渲染完整 markdown：标题/列表/加粗/链接', () => {
    const md = '# 标题\n\n- 第一项\n- 第二项\n\n**加粗** 与 [链接](https://x.com)'
    const { container } = render(
      <MarkdownView blockKey="summary" text={md} anns={[]} onClickHL={vi.fn()} />,
    )
    expect(container.querySelector('h1')?.textContent).toBe('标题')
    expect(container.querySelectorAll('li')).toHaveLength(2)
    expect(container.querySelector('strong')?.textContent).toBe('加粗')
    const a = container.querySelector('a')
    expect(a?.getAttribute('href')).toBe('https://x.com')
    expect(a?.getAttribute('target')).toBe('_blank')
  })

  it('块内字符偏移注入 <mark>：包住正确的文本片段', () => {
    // 纯文本 "abcdefg"，划线 start=2,end=5 → 应包住 "cde"
    const { container } = render(
      <MarkdownView
        blockKey="summary"
        text="abcdefg"
        anns={[ann({ start: 2, end: 5 })]}
        onClickHL={vi.fn()}
      />,
    )
    const mark = container.querySelector('mark.hl')
    expect(mark).not.toBeNull()
    expect(mark?.textContent).toBe('cde')
    expect(mark?.getAttribute('data-ann')).toBe('a1')
  })

  it('点击划线 mark 触发 onClickHL(target-aware locator)', () => {
    const onClickHL = vi.fn()
    render(
      <MarkdownView
        blockKey="summary"
        text="hello world"
        anns={[ann({ id: 'x9', start: 0, end: 5, note: '笔记' })]}
        onClickHL={onClickHL}
      />,
    )
    fireEvent.click(screen.getByText('hello'))
    expect(onClickHL).toHaveBeenCalled()
    expect(onClickHL.mock.calls[0][0]).toMatchObject({
      id: 'x9',
      blockKey: 'summary',
      target: { kind: 'summary', sourceHash: 'a'.repeat(64) },
    })
  })

  it('同 ID、同 block 的不同 saved revisions 由 mark target key 精确定位', () => {
    const onClickHL = vi.fn()
    render(
      <MarkdownView
        blockKey="content"
        text="old new"
        anns={[
          ann({
            id: 'shared',
            blockKey: 'content',
            start: 0,
            end: 3,
            sourceSummaryHash: undefined,
            sourceContentRevision: 7,
          }),
          ann({
            id: 'shared',
            blockKey: 'content',
            start: 4,
            end: 7,
            sourceSummaryHash: undefined,
            sourceContentRevision: 8,
          }),
        ]}
        onClickHL={onClickHL}
      />,
    )

    fireEvent.click(screen.getByText('old'))
    fireEvent.click(screen.getByText('new'))

    expect(onClickHL.mock.calls.map(([locator]) => locator.target)).toEqual([
      { kind: 'saved-content', contentRevision: 7 },
      { kind: 'saved-content', contentRevision: 8 },
    ])
  })

  it('无划线时不注入 mark', () => {
    const { container } = render(
      <MarkdownView blockKey="summary" text="just text" anns={[]} onClickHL={vi.fn()} />,
    )
    expect(container.querySelector('mark')).toBeNull()
  })

  it('传入 headingIdPrefix 时给标题写锚点并回传大纲', () => {
    const onHeadings = vi.fn()
    const { container } = render(
      <MarkdownView
        blockKey="content-document"
        text={'# 一级\n\n段落\n\n## 二级\n\n#### 四级'}
        anns={[]}
        onClickHL={vi.fn()}
        headingIdPrefix="toc"
        onHeadings={onHeadings}
      />,
    )

    expect(container.querySelector('h1')).toHaveAttribute('id', 'toc-h0')
    expect(container.querySelector('h2')).toHaveAttribute('id', 'toc-h1')
    expect(container.querySelector('h1')).toHaveAttribute('data-toc-heading')
    // 四级标题有锚点但不进目录，也不参与滚动高亮。
    expect(container.querySelector('h4')).toHaveAttribute('id', 'toc-h2')
    expect(container.querySelector('h4')).not.toHaveAttribute('data-toc-heading')
    expect(onHeadings).toHaveBeenCalledTimes(1)
    expect(onHeadings.mock.calls[0][0]).toEqual([
      { id: 'toc-h0', level: 1, text: '一级' },
      { id: 'toc-h1', level: 2, text: '二级' },
    ])
  })

  it('不传 headingIdPrefix 时不写锚点（摘要正文保持原样）', () => {
    const { container } = render(
      <MarkdownView blockKey="summary" text="# 摘要标题" anns={[]} onClickHL={vi.fn()} />,
    )
    expect(container.querySelector('h1')).not.toHaveAttribute('id')
  })

  // 契约：本组件只报告「本次渲染的大纲」，不替调用方判重——它不知道大纲属于哪篇
  // 文章，两篇文章大纲文字相同时去重会让调用方收不到「换文章了」。判重在 useReaderToc。
  it('每次真实渲染都回传大纲，不自行去重', () => {
    const onHeadings = vi.fn()
    const view = (text: string) => (
      <MarkdownView
        blockKey="content-document"
        text={text}
        anns={[]}
        onClickHL={vi.fn()}
        headingIdPrefix="toc"
        onHeadings={onHeadings}
      />
    )
    const { rerender } = render(view('# 一级\n\n## 二级'))
    expect(onHeadings).toHaveBeenCalledTimes(1)

    // 正文没变但 props 是新引用（生产路径就是这样）→ 照样回传同一份大纲。
    rerender(view('# 一级\n\n## 二级'))
    expect(onHeadings).toHaveBeenCalledTimes(2)
    expect(onHeadings.mock.calls[1][0]).toEqual(onHeadings.mock.calls[0][0])

    rerender(view('# 换了标题'))
    expect(onHeadings).toHaveBeenCalledTimes(3)
    expect(onHeadings.mock.calls[2][0]).toEqual([{ id: 'toc-h0', level: 1, text: '换了标题' }])
  })

  it('replaces every Markdown image protocol with alt placeholders that cannot load', () => {
    const { container } = render(
      <MarkdownView
        blockKey="content"
        text={[
          '![Remote](https://example.com/cover.jpg?private=1)',
          '![Relative](/cover.jpg)',
          '![Protocol relative](//tracker.test/pixel)',
          '![Data](data:image/png;base64,AA==)',
        ].join('\n\n')}
        anns={[]}
        onClickHL={vi.fn()}
      />,
    )
    expect(container.querySelector('img')).toBeNull()
    expect(screen.getAllByRole('img')).toHaveLength(4)
    expect(screen.getByRole('img', { name: 'Remote' })).toHaveTextContent('Remote')
    expect(screen.getByRole('img', { name: 'Relative' })).toHaveTextContent('Relative')
    expect(screen.getByRole('img', { name: 'Protocol relative' })).toHaveTextContent('Protocol relative')
    expect(screen.getByRole('img', { name: 'Data' })).toHaveTextContent('Data')
    expect(container.innerHTML).not.toContain('example.com')
    expect(container.innerHTML).not.toContain('tracker.test')
    expect(container.innerHTML).not.toContain('data:image')
    expect(container.querySelectorAll('[data-blocked-content="image"]')).toHaveLength(4)
  })
})
