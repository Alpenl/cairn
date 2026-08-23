/**
 * PF2 的量化验收（第三半）：证明 MainView 的无关状态变化不再让 DetailPane
 * 整棵重渲染。
 *
 * 与 MarkdownView.memo.test.tsx 是两道独立的闸，不能互相替代：
 *   · MarkdownView 那道挡的是「整篇 markdown 重新 parse」。
 *   · 这一道挡的是 DetailPane 自身近千行的渲染工作——阅读时长计算、
 *     savedContent 派生、工具栏与元信息组装、划线列表排序。即便 markdown
 *     不再重 parse，这些也会随每次父级 state 变化白跑一遍。
 *
 * 计数点选在 Icon 上：DetailPane 在任何非空态下都会渲染多个 Icon，
 * 因此「Icon 渲染次数增量为 0」等价于「DetailPane 函数体没有被重新执行」。
 */
import { useState } from 'react'
import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'

import { DetailPane, type DetailPaneProps } from './DetailPane'
import { makeLink } from '../test/fixtures'
import { isContentAnchored, NO_ANNOTATIONS } from '../lib/annotation-domain'
import type { SavedArticleDocument } from '../lib/article/document'

const probe = vi.hoisted(() => ({ icons: 0 }))

vi.mock('./Icon', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./Icon')>()
  return {
    ...actual,
    Icon: (props: Parameters<typeof actual.Icon>[0]) => {
      probe.icons += 1
      return <actual.Icon {...props} />
    },
  }
})

// 全部 prop 走模块级稳定引用。这正是生产代码里 MainView 现在做的事
// （onPickTag / onToggleFocus / onConvertToSite / previous / next 都已提升为
// useCallback / useMemo），此处如实复刻，否则测的就不是 memo 而是内联 prop。
const stableLink = makeLink({ content_revision: 1 })
const stableDocument: SavedArticleDocument = {
  id: { namespace: 'test-namespace', linkId: stableLink.id, contentRevision: 1 },
  generation: 1,
  detail: { status: 'ready', revision: 1, data: stableLink },
  body: { status: 'idle' },
  annotations: { status: 'idle' },
  translations: { status: 'idle' },
}
const stable: DetailPaneProps = {
  l: stableLink,
  document: stableDocument,
  captureDocumentContext: () => null,
  onBack: vi.fn(),
  chatOpen: false,
  onChat: vi.fn(),
  onToast: vi.fn(),
  curTag: null,
  onPickTag: vi.fn(),
  corpus: [],
  annotationsEnabled: true,
  aiEnabled: true,
  relatedTagsEnabled: true,
  engagementEnabled: true,
  anns: NO_ANNOTATIONS,
  onAddAnn: vi.fn(async (annotation) => {
    if (isContentAnchored(annotation.blockKey)) {
      return {
        id: 'an1',
        blockKey: annotation.blockKey,
        target: { kind: 'saved-content' as const, contentRevision: 1 },
      }
    }
    return annotation.blockKey === 'summary'
      ? {
          id: 'an1',
          blockKey: 'summary' as const,
          target: { kind: 'summary' as const, sourceHash: 'a'.repeat(64) },
        }
      : null
  }),
  onRemoveAnn: vi.fn(async () => true),
  onOpenNote: vi.fn(),
  onAskAI: vi.fn(),
  onSaveContent: vi.fn(),
  onReplaceContent: vi.fn(),
  onLoadContent: vi.fn(async () => null),
  savingContent: null,
  translations: [],
  translationsLoading: false,
  onTranslateSelection: vi.fn(async () => null),
  onTranslateFull: vi.fn(),
  focusMode: false,
  onToggleFocus: vi.fn(),
}

function Harness() {
  // 模拟 MainView 上与当前文章无关的状态：toast 计时器、chatOpen、
  // mobileNavOpen、翻译轮询触发的重渲染……
  const [toastTick, setToastTick] = useState(0)
  return (
    <div>
      <button onClick={() => setToastTick((value) => value + 1)}>flash toast</button>
      <span data-testid="toast-tick">{toastTick}</span>
      <DetailPane {...stable} />
    </div>
  )
}

describe('DetailPane memo（PF2 量化验收）', () => {
  it('父组件无关状态连续变化 10 次，DetailPane 渲染次数增量为 0', () => {
    probe.icons = 0
    render(<Harness />)

    const afterMount = probe.icons
    expect(afterMount).toBeGreaterThan(0) // 挂载确实渲染了 Icon，计数点有效

    for (let i = 0; i < 10; i += 1) {
      fireEvent.click(screen.getByText('flash toast'))
    }

    expect(screen.getByTestId('toast-tick')).toHaveTextContent('10')
    // 改造前：这里会是 afterMount * 10（每次父渲染整棵重渲染一遍）。
    expect(probe.icons - afterMount).toBe(0)
  })

  it('对照组：换一条链接时仍然重渲染（memo 没有把真更新挡掉）', () => {
    probe.icons = 0
    const { rerender } = render(<DetailPane {...stable} />)
    const afterMount = probe.icons

    rerender(<DetailPane {...stable} l={makeLink({ id: 'L2', title: '另一篇文章' })} />)

    expect(probe.icons - afterMount).toBeGreaterThan(0)
    expect(screen.getByText('另一篇文章')).toBeInTheDocument()
  })
})
