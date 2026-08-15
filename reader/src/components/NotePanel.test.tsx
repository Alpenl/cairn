import { describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen } from '@testing-library/react'
import { NotePanel } from './NotePanel'
import type { Annotation } from '../lib/annotations'

function mkAnn(over: Partial<Annotation> = {}): Annotation {
  return {
    id: 'a1',
    blockKey: 'summary',
    start: 0,
    end: 4,
    text: '划线引文',
    note: '',
    source: 'self',
    createdAt: Date.now(),
    updatedAt: Date.now(),
    sourceSummaryHash: 'a'.repeat(64),
    ...over,
  }
}

describe('NotePanel', () => {
  it('展示引文，textarea 初值为 note', () => {
    render(
      <NotePanel ann={mkAnn({ note: '想法' })} onSave={() => {}} onDelete={() => {}} onClose={() => {}} onAskAI={() => {}} />,
    )
    expect(screen.getByText('划线引文')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('写下你的想法…')).toHaveValue('想法')
  })

  it('⌘Enter 保存当前编辑值', () => {
    const onSave = vi.fn()
    render(<NotePanel ann={mkAnn()} onSave={onSave} onDelete={() => {}} onClose={() => {}} onAskAI={() => {}} />)
    const ta = screen.getByPlaceholderText('写下你的想法…')
    fireEvent.change(ta, { target: { value: '新想法' } })
    fireEvent.keyDown(ta, { key: 'Enter', metaKey: true })
    expect(onSave).toHaveBeenCalledWith('新想法')
  })

  it('问 AI 回传当前未保存草稿值（避免丢字）', () => {
    const onAskAI = vi.fn()
    render(<NotePanel ann={mkAnn()} onSave={() => {}} onDelete={() => {}} onClose={() => {}} onAskAI={onAskAI} />)
    fireEvent.change(screen.getByPlaceholderText('写下你的想法…'), { target: { value: '草稿未存' } })
    fireEvent.click(screen.getByText('问 AI'))
    expect(onAskAI).toHaveBeenCalledWith(
      {
        id: 'a1',
        blockKey: 'summary',
        target: { kind: 'summary', sourceHash: 'a'.repeat(64) },
      },
      '划线引文',
      '草稿未存',
    )
  })

  it('同 ID 与 note 切换到另一 source target 时重置本地草稿', () => {
    const view = (contentRevision: number) => (
      <NotePanel
        ann={mkAnn({
          id: 'shared',
          blockKey: 'content',
          note: 'target note',
          sourceSummaryHash: undefined,
          sourceContentRevision: contentRevision,
        })}
        onSave={() => {}}
        onDelete={() => {}}
        onClose={() => {}}
        onAskAI={() => {}}
      />
    )
    const { rerender } = render(view(7))
    const textarea = screen.getByPlaceholderText('写下你的想法…')
    fireEvent.change(textarea, { target: { value: 'draft for revision seven' } })

    rerender(view(8))

    expect(textarea).toHaveValue('target note')
  })

  it('AI 来源显示「AI 笔记 · 可编辑」', () => {
    render(<NotePanel ann={mkAnn({ source: 'ai' })} onSave={() => {}} onDelete={() => {}} onClose={() => {}} onAskAI={() => {}} />)
    expect(screen.getByText('AI 笔记 · 可编辑')).toBeInTheDocument()
  })

  it('已归档想法使用 legacy-stale target 且保持只读', () => {
    const onAskAI = vi.fn()
    const onSave = vi.fn()
    render(
      <NotePanel
        ann={mkAnn({ blockKey: 'content', sourceSummaryHash: undefined, sourceContentRevision: 7 })}
        locator={{
          id: 'a1',
          blockKey: 'content',
          target: { kind: 'legacy-stale', sourceKey: 'saved-content:7:ambiguous-quote' },
        }}
        readOnly
        onSave={onSave}
        onDelete={() => {}}
        onClose={() => {}}
        onAskAI={onAskAI}
      />,
    )

    expect(screen.getByText('已归档想法 · 只读')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('写下你的想法…')).toHaveAttribute('readonly')
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '问 AI' })).toBeDisabled()
    expect(screen.getByTitle('删除划线')).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: '问 AI' }))
    expect(onAskAI).not.toHaveBeenCalled()
    expect(onSave).not.toHaveBeenCalled()
  })

  it('durable 保存未完成时禁用重复操作', async () => {
    let resolve!: () => void
    const pending = new Promise<void>((done) => { resolve = done })
    const onSave = vi.fn(() => pending)
    render(
      <NotePanel
        ann={mkAnn()}
        onSave={onSave}
        onDelete={() => {}}
        onClose={() => {}}
        onAskAI={() => {}}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(onSave).toHaveBeenCalledTimes(1)

    await act(async () => resolve())
    expect(screen.getByRole('button', { name: '保存' })).not.toBeDisabled()
  })

  it('durable 操作 reject 后恢复交互且消费 rejection', async () => {
    let reject!: (error: Error) => void
    const pending = new Promise<void>((_resolve, fail) => { reject = fail })
    const onSave = vi.fn(() => pending)
    render(
      <NotePanel
        ann={mkAnn()}
        onSave={onSave}
        onDelete={() => {}}
        onClose={() => {}}
        onAskAI={() => {}}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled()

    await act(async () => {
      reject(new Error('durable save failed'))
      await pending.catch(() => undefined)
    })
    expect(screen.getByRole('button', { name: '保存' })).not.toBeDisabled()
  })
})
