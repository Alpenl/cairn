/**
 * PF2 的量化验收：证明「父组件的无关状态变化不再触发整篇 markdown 重新 parse」。
 *
 * 计数点选在 react-markdown 本身而不是 MarkdownView 的渲染次数——它才是真正
 * 昂贵的那一步。react-markdown v10 的 Markdown() 在组件体里直接跑
 * createProcessor + parse + runSync（node_modules/react-markdown/lib/index.js:175-179），
 * 没有任何记忆化，所以「被渲染一次」就等于「整篇被重新 parse 一次」。
 */
import { useState } from 'react'
import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

// 从**生产实际渲染的那个组件**导入：PF9 之后 DetailPane 渲染的是
// LazyMarkdownView（懒加载边界），直接 import MarkdownView 会让这道闸守着
// 一个生产已经不直接渲染的组件——去掉 LazyMarkdownView 上的 memo 也不会变红。
import { LazyMarkdownView as MarkdownView } from './LazyMarkdownView'
import { NO_ANNOTATIONS, type Annotation } from '../lib/annotation-domain'

// vi.mock 会被提升到 import 之上，因此计数器必须走 vi.hoisted。
const probe = vi.hoisted(() => ({ parses: 0 }))

vi.mock('react-markdown', () => ({
  default: ({ children }: { children?: string }) => {
    probe.parses += 1
    return <div data-testid="md-output">{children}</div>
  },
}))

// 模块级稳定引用：这些 prop 若在父组件里内联，memo 会恒失效，
// 那样本测试就测不出 memo 的效果了（而是测出"内联 prop 会破坏 memo"）。
const stableOnClickHL = () => {}

function Harness({ text = '# 标题\n\n正文' }: { text?: string }) {
  const [tick, setTick] = useState(0)
  return (
    <div>
      {/* 模拟 MainView / DetailPane 上与文章无关的状态：toast、hover、轮询…… */}
      <button onClick={() => setTick((value) => value + 1)}>unrelated</button>
      <span data-testid="tick">{tick}</span>
      <MarkdownView
        blockKey="summary"
        text={text}
        anns={NO_ANNOTATIONS}
        onClickHL={stableOnClickHL}
      />
    </div>
  )
}

describe('MarkdownView memo（PF2 量化验收）', () => {
  it('父组件无关状态连续变化 10 次，markdown 重新 parse 次数增量为 0', async () => {
    probe.parses = 0
    render(<Harness />)

    // 懒加载边界：等 MarkdownView chunk 解析完再取基线。
    await screen.findByTestId('md-output')
    const afterMount = probe.parses
    expect(afterMount).toBe(1) // 首次挂载必然 parse 一次

    for (let i = 0; i < 10; i += 1) {
      fireEvent.click(screen.getByText('unrelated'))
    }

    // 父组件确实重渲染了 10 次——否则下面那条断言是空的。
    expect(screen.getByTestId('tick')).toHaveTextContent('10')
    // 改造前：这里会是 11（每次父渲染都重新 parse 整篇）。
    expect(probe.parses - afterMount).toBe(0)
  })

  it('对照组：文章内容真的变了时仍然重新 parse（memo 没有把真更新也挡掉）', async () => {
    probe.parses = 0
    const { rerender } = render(<Harness text="# 第一篇" />)
    await screen.findByTestId('md-output')
    const afterMount = probe.parses

    rerender(<Harness text="# 第二篇" />)
    await waitFor(() => expect(probe.parses - afterMount).toBe(1))
    expect(screen.getByTestId('md-output')).toHaveTextContent('第二篇')
  })

  it('对照组：本块划线变化时仍然重新 parse', async () => {
    const highlight: Annotation = {
      id: 'a1',
      blockKey: 'summary',
      start: 0,
      end: 2,
      text: '标题',
      note: '',
      source: 'self',
      createdAt: 0,
      updatedAt: 0,
    }

    probe.parses = 0
    const { rerender } = render(
      <MarkdownView
        blockKey="summary"
        text="# 标题"
        anns={NO_ANNOTATIONS}
        onClickHL={stableOnClickHL}
      />,
    )
    await screen.findByTestId('md-output')
    const afterMount = probe.parses

    rerender(
      <MarkdownView
        blockKey="summary"
        text="# 标题"
        anns={[highlight]}
        onClickHL={stableOnClickHL}
      />,
    )

    await waitFor(() => expect(probe.parses - afterMount).toBe(1))
  })
})
