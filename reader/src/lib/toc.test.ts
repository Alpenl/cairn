import { describe, expect, it } from 'vitest'

import { collectHeadingsFromDOM, rehypeHeadingIds, type TocHeading } from './toc'
import type { HastNode } from './hast'

function el(tagName: string, children: HastNode[]): HastNode {
  return { type: 'element', tagName, children }
}

function text(value: string): HastNode {
  return { type: 'text', value }
}

function run(tree: HastNode, prefix = 'toc'): TocHeading[] {
  let collected: TocHeading[] = []
  rehypeHeadingIds({ prefix, collect: (headings) => { collected = headings } })(tree)
  return collected
}

describe('rehypeHeadingIds', () => {
  it('按文档顺序编号并收集前三级标题', () => {
    const tree = el('root', [
      el('h1', [text('总览')]),
      el('p', [text('正文')]),
      el('h2', [text('细节')]),
      el('h3', [text('更细')]),
    ])

    const headings = run(tree)

    expect(headings).toEqual([
      { id: 'toc-h0', level: 1, text: '总览' },
      { id: 'toc-h1', level: 2, text: '细节' },
      { id: 'toc-h2', level: 3, text: '更细' },
    ])
    expect(tree.children?.map((child) => child.properties?.id)).toEqual([
      'toc-h0',
      undefined,
      'toc-h1',
      'toc-h2',
    ])
  })

  it('四级及以下有 id 但不进目录，编号仍连续', () => {
    const tree = el('root', [el('h1', [text('一')]), el('h4', [text('四')]), el('h2', [text('二')])])

    const headings = run(tree)

    expect(headings.map((h) => h.id)).toEqual(['toc-h0', 'toc-h2'])
    expect(tree.children?.[1].properties?.id).toBe('toc-h1')
    expect(tree.children?.[1].properties?.dataTocHeading).toBeUndefined()
    expect(tree.children?.[0].properties?.dataTocHeading).toBe('')
  })

  it('标题内的行内元素拼成纯文本，空标题不进目录', () => {
    const tree = el('root', [
      el('h2', [text('带 '), el('code', [text('code')]), text(' 的标题')]),
      el('h2', [el('img', [])]),
    ])

    const headings = run(tree)

    expect(headings).toEqual([{ id: 'toc-h0', level: 2, text: '带 code 的标题' }])
    expect(tree.children?.[1].properties?.id).toBe('toc-h1')
  })

  it('嵌套在引用块里的标题一样会被编号（与渲染结果对齐）', () => {
    const tree = el('root', [
      el('blockquote', [el('h2', [text('引用里的标题')])]),
      el('h2', [text('正常标题')]),
    ])

    expect(run(tree).map((h) => h.text)).toEqual(['引用里的标题', '正常标题'])
  })

  it('保留标题上已有的其它属性', () => {
    const heading: HastNode = { type: 'element', tagName: 'h2', properties: { className: ['x'] }, children: [text('标题')] }

    run(el('root', [heading]))

    expect(heading.properties).toEqual({
      className: ['x'],
      id: 'toc-h0',
      dataTocHeading: '',
      tabIndex: -1,
    })
  })

  it('不同前缀产出不同锚点，避免同页多块正文撞 id', () => {
    expect(run(el('root', [el('h1', [text('a')])]), 'trans')[0].id).toBe('trans-h0')
  })
})

// 两种正文来源必须写出同一套锚点属性，否则下游的高亮 / 跳转 / 焦点交接要分叉。
describe('collectHeadingsFromDOM', () => {
  function mount(html: string): HTMLElement {
    const root = document.createElement('div')
    root.innerHTML = html
    return root
  }

  it('给 DOM 正文补锚点并收集前三级标题', () => {
    const root = mount('<h1>一级</h1><p>正文</p><h2>二级</h2><h3>三级</h3>')

    const headings = collectHeadingsFromDOM(root, 'feed-toc')

    expect(headings).toEqual([
      { id: 'feed-toc-h0', level: 1, text: '一级' },
      { id: 'feed-toc-h1', level: 2, text: '二级' },
      { id: 'feed-toc-h2', level: 3, text: '三级' },
    ])
    const h1 = root.querySelector('h1') as HTMLElement
    expect(h1.id).toBe('feed-toc-h0')
    expect(h1.hasAttribute('data-toc-heading')).toBe(true)
    expect(h1.tabIndex).toBe(-1)
  })

  it('四级以下与空标题有 id 但不进目录、不参与高亮', () => {
    const root = mount('<h1>一级</h1><h4>四级</h4><h2>  </h2>')

    const headings = collectHeadingsFromDOM(root, 'feed-toc')

    expect(headings.map((h) => h.id)).toEqual(['feed-toc-h0'])
    expect((root.querySelector('h4') as HTMLElement).id).toBe('feed-toc-h1')
    expect(root.querySelector('h4')?.hasAttribute('data-toc-heading')).toBe(false)
    expect(root.querySelector('h2')?.hasAttribute('data-toc-heading')).toBe(false)
  })

  it('与 markdown 那条路写出的锚点属性一致', () => {
    const root = mount('<h2>标题</h2>')
    collectHeadingsFromDOM(root, 'toc')
    const fromDOM = root.querySelector('h2') as HTMLElement

    const tree = el('root', [el('h2', [text('标题')])])
    run(tree)
    const fromMarkdown = tree.children?.[0].properties ?? {}

    expect(fromDOM.id).toBe(fromMarkdown.id)
    expect(fromDOM.hasAttribute('data-toc-heading')).toBe(fromMarkdown.dataTocHeading === '')
    expect(fromDOM.tabIndex).toBe(fromMarkdown.tabIndex)
  })
})
