import { useState } from 'react'
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { NoteMarkdownEditor, positionSlashMenu } from './NoteMarkdownEditor'

function EditorHarness({
  documentKey = 'test-note',
  initial = '',
  disabled = false,
  onValueChange = () => undefined,
}: {
  readonly documentKey?: string
  readonly initial?: string
  readonly disabled?: boolean
  readonly onValueChange?: (value: string) => void
}) {
  const [value, setValue] = useState(initial)
  return (
    <NoteMarkdownEditor
      documentKey={documentKey}
      value={value}
      disabled={disabled}
      onValueChange={(next) => {
        onValueChange(next)
        setValue(next)
      }}
    />
  )
}

function input(value: string): HTMLTextAreaElement {
  const textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement
  fireEvent.change(textarea, { target: { value } })
  return textarea
}

describe('NoteMarkdownEditor', () => {
  it('routes one keyboard command through exactly one controlled value change', () => {
    const onValueChange = vi.fn()
    render(<EditorHarness initial="word" onValueChange={onValueChange} />)
    const textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement
    textarea.setSelectionRange(0, 4)

    fireEvent.keyDown(textarea, { key: 'b', ctrlKey: true })

    expect(textarea).toHaveValue('**word**')
    expect(textarea.selectionStart).toBe(2)
    expect(textarea.selectionEnd).toBe(6)
    expect(onValueChange).toHaveBeenCalledTimes(1)
  })

  it('undoes and redoes a controlled Markdown command with exact selection offsets', () => {
    const onValueChange = vi.fn()
    render(<EditorHarness initial="word" onValueChange={onValueChange} />)
    const textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement
    textarea.setSelectionRange(0, 4)

    fireEvent.keyDown(textarea, { key: 'b', ctrlKey: true })
    fireEvent.keyDown(textarea, { key: 'z', ctrlKey: true })

    expect(textarea).toHaveValue('word')
    expect(textarea.selectionStart).toBe(0)
    expect(textarea.selectionEnd).toBe(4)

    fireEvent.keyDown(textarea, { key: 'z', ctrlKey: true, shiftKey: true })

    expect(textarea).toHaveValue('**word**')
    expect(textarea.selectionStart).toBe(2)
    expect(textarea.selectionEnd).toBe(6)
    expect(onValueChange).toHaveBeenCalledTimes(3)
  })

  it('does not replay command history across documents with identical text', () => {
    const view = render(<EditorHarness documentKey="note-a" initial="word" />)
    let textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement
    textarea.setSelectionRange(0, 4)
    fireEvent.keyDown(textarea, { key: 'b', ctrlKey: true })
    expect(textarea).toHaveValue('**word**')

    view.rerender(<EditorHarness documentKey="note-b" initial="word" />)
    textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement

    expect(fireEvent.keyDown(textarea, { key: 'z', ctrlKey: true })).toBe(true)
    expect(textarea).toHaveValue('**word**')
  })

  it('continues a checked task as unchecked and keeps one logical change', () => {
    const onValueChange = vi.fn()
    render(<EditorHarness initial="- [x] done" onValueChange={onValueChange} />)
    const textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement
    textarea.setSelectionRange(textarea.value.length, textarea.value.length)

    fireEvent.keyDown(textarea, { key: 'Enter' })

    expect(textarea).toHaveValue('- [x] done\n- [ ] ')
    expect(onValueChange).toHaveBeenCalledOnce()
  })

  it('intercepts Tab only for list selections', () => {
    render(<EditorHarness initial="plain" />)
    let textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement
    textarea.setSelectionRange(0, 5)
    expect(fireEvent.keyDown(textarea, { key: 'Tab' })).toBe(true)
    expect(textarea).toHaveValue('plain')

    fireEvent.change(textarea, { target: { value: '- one\n- two' } })
    textarea = screen.getByRole('textbox', { name: '笔记内容' }) as HTMLTextAreaElement
    textarea.setSelectionRange(0, textarea.value.length)
    expect(fireEvent.keyDown(textarea, { key: 'Tab' })).toBe(false)
    expect(textarea).toHaveValue('  - one\n  - two')
  })

  it('supports Arrow keys and Enter in the slash menu', () => {
    render(<EditorHarness />)
    const textarea = input('/')
    expect(screen.getAllByRole('option')).toHaveLength(9)

    fireEvent.keyDown(textarea, { key: 'ArrowDown' })
    expect(screen.getByRole('option', { name: /二级标题/ })).toHaveAttribute('aria-selected', 'true')
    fireEvent.keyDown(textarea, { key: 'Enter' })

    expect(textarea).toHaveValue('## ')
    expect(screen.queryByRole('listbox', { name: 'Markdown 命令' })).not.toBeInTheDocument()
  })

  it('filters and applies a slash command by click', () => {
    render(<EditorHarness />)
    const textarea = input('/todo')
    expect(screen.getAllByRole('option')).toHaveLength(1)

    fireEvent.click(screen.getByRole('option', { name: /任务列表/ }))

    expect(textarea).toHaveValue('- [ ] ')
  })

  it('closes the slash menu on Escape without changing source', () => {
    render(<EditorHarness />)
    const textarea = input('  /')
    expect(screen.getByRole('listbox', { name: 'Markdown 命令' })).toBeInTheDocument()

    fireEvent.keyDown(textarea, { key: 'Escape' })

    expect(textarea).toHaveValue('  /')
    expect(screen.queryByRole('listbox', { name: 'Markdown 命令' })).not.toBeInTheDocument()
  })

  it('closes a pending slash menu while autosave disables the editor', () => {
    const view = render(<EditorHarness />)
    input('/')
    expect(screen.getByRole('listbox', { name: 'Markdown 命令' })).toBeInTheDocument()

    view.rerender(<EditorHarness disabled />)
    expect(screen.queryByRole('listbox', { name: 'Markdown 命令' })).not.toBeInTheDocument()

    view.rerender(<EditorHarness />)
    expect(screen.queryByRole('listbox', { name: 'Markdown 命令' })).not.toBeInTheDocument()
  })

  it('does not open a slash menu away from logical line start', () => {
    render(<EditorHarness />)
    input('text /h1')
    expect(screen.queryByRole('listbox', { name: 'Markdown 命令' })).not.toBeInTheDocument()
  })

  it('puts the code-block caret between fences', () => {
    render(<EditorHarness />)
    const textarea = input('/code')
    fireEvent.keyDown(textarea, { key: 'Enter' })
    expect(textarea).toHaveValue('```\n\n```')
    expect(textarea.selectionStart).toBe(4)
    expect(textarea.selectionEnd).toBe(4)
  })
})

describe('slash menu geometry', () => {
  it('places the menu wholly on one side of the caret on a mobile viewport', () => {
    const below = positionSlashMenu(
      { left: 360, top: 20, bottom: 38 },
      390,
      844,
    )
    expect(below.placement).toBe('below')
    expect(below.top).toBeGreaterThan(38)
    expect(below.left + below.width).toBeLessThanOrEqual(382)

    const above = positionSlashMenu(
      { left: 20, top: 780, bottom: 798 },
      390,
      844,
    )
    expect(above.placement).toBe('above')
    expect(above.top + above.maxHeight).toBeLessThan(780)
  })
})
