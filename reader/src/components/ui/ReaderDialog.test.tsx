import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import { useRef, useState } from 'react'
import { ReaderDialog } from './ReaderDialog'

function backdrop(): HTMLElement {
  const element = document.querySelector<HTMLElement>('.reader-dialog-backdrop')
  if (!element) throw new Error('backdrop 未渲染')
  return element
}

describe('ReaderDialog 关闭规则', () => {
  it('点击 Dialog 内部不关闭，点击 backdrop 才关闭', () => {
    const onClose = vi.fn()
    render(
      <ReaderDialog title="标题" titleId="t" onClose={onClose}>
        <p>正文</p>
      </ReaderDialog>,
    )

    fireEvent.mouseDown(screen.getByRole('dialog'))
    fireEvent.mouseDown(screen.getByText('正文'))
    expect(onClose).not.toHaveBeenCalled()

    fireEvent.mouseDown(backdrop())
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('dismissOnBackdrop=false 时 backdrop 不关闭，关闭按钮仍然可用', () => {
    const onClose = vi.fn()
    render(
      <ReaderDialog title="标题" titleId="t" dismissOnBackdrop={false} onClose={onClose}>
        <p>正文</p>
      </ReaderDialog>,
    )

    fireEvent.mouseDown(backdrop())
    expect(onClose).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: '关闭' }))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('dismissOnEscape=false 时 Escape 不关闭', () => {
    const onClose = vi.fn()
    render(
      <ReaderDialog title="标题" titleId="t" dismissOnEscape={false} onClose={onClose}>
        <p>正文</p>
      </ReaderDialog>,
    )

    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).not.toHaveBeenCalled()
  })

  it('默认 Escape 关闭', () => {
    const onClose = vi.fn()
    render(
      <ReaderDialog title="标题" titleId="t" onClose={onClose}>
        <p>正文</p>
      </ReaderDialog>,
    )

    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('busy=true 时关闭按钮、Escape 和 backdrop 三条路径全部失效', () => {
    const onClose = vi.fn()
    render(
      <ReaderDialog title="标题" titleId="t" busy onClose={onClose}>
        <p>正文</p>
      </ReaderDialog>,
    )

    const close = screen.getByRole('button', { name: '关闭' })
    expect(close).toBeDisabled()
    fireEvent.click(close)
    fireEvent.keyDown(document, { key: 'Escape' })
    fireEvent.mouseDown(backdrop())

    expect(onClose).not.toHaveBeenCalled()
  })
})

describe('ReaderDialog 无障碍外壳', () => {
  it('把标题接到 aria-labelledby 上，并按 size 给出宽度档位', () => {
    render(
      <ReaderDialog title="移到阅读" titleId="move-title" size="compact" className="extra" onClose={vi.fn()}>
        <p>正文</p>
      </ReaderDialog>,
    )

    const dialog = screen.getByRole('dialog')
    expect(dialog).toHaveAttribute('aria-modal', 'true')
    expect(dialog).toHaveAttribute('aria-labelledby', 'move-title')
    expect(dialog).toHaveClass('reader-dialog', 'reader-dialog-compact', 'extra')
    expect(screen.getByRole('heading', { name: '移到阅读' })).toHaveAttribute('id', 'move-title')
  })
})

describe('ReaderDialog 焦点管理', () => {
  it('打开后聚焦 initialFocusRef', () => {
    function Harness() {
      const inputRef = useRef<HTMLInputElement>(null)
      return (
        <ReaderDialog title="添加链接" titleId="t" initialFocusRef={inputRef} onClose={vi.fn()}>
          <input ref={inputRef} aria-label="网址" />
        </ReaderDialog>
      )
    }
    render(<Harness />)

    expect(document.activeElement).toBe(screen.getByLabelText('网址'))
  })

  it('没有指定 initialFocusRef 时聚焦首个可交互元素', () => {
    render(
      <ReaderDialog title="标题" titleId="t" onClose={vi.fn()}>
        <input aria-label="网址" />
      </ReaderDialog>,
    )

    expect(document.activeElement).toBe(screen.getByRole('button', { name: '关闭' }))
  })

  it('关闭按钮被 busy 禁用时，首个可交互元素落到内容里', () => {
    render(
      <ReaderDialog title="标题" titleId="t" busy onClose={vi.fn()}>
        <input aria-label="网址" />
      </ReaderDialog>,
    )

    expect(document.activeElement).toBe(screen.getByLabelText('网址'))
  })

  it('Tab 从最后一个控件回到第一个，Shift+Tab 从第一个回到最后一个', () => {
    render(
      <ReaderDialog title="标题" titleId="t" onClose={vi.fn()}>
        <button type="button">取消</button>
        <button type="button">确认</button>
      </ReaderDialog>,
    )
    const close = screen.getByRole('button', { name: '关闭' })
    const confirm = screen.getByRole('button', { name: '确认' })

    confirm.focus()
    fireEvent.keyDown(confirm, { key: 'Tab' })
    expect(document.activeElement).toBe(close)

    fireEvent.keyDown(close, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(confirm)
  })

  it('焦点在中间控件时 Tab 不被劫持，交给浏览器默认顺序', () => {
    render(
      <ReaderDialog title="标题" titleId="t" onClose={vi.fn()}>
        <button type="button">取消</button>
        <button type="button">确认</button>
      </ReaderDialog>,
    )
    const cancel = screen.getByRole('button', { name: '取消' })

    cancel.focus()
    expect(fireEvent.keyDown(cancel, { key: 'Tab' })).toBe(true)
    expect(document.activeElement).toBe(cancel)
  })

  it('关闭后把焦点还给打开它的元素', () => {
    function Harness() {
      const [open, setOpen] = useState(false)
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>打开</button>
          {open && (
            <ReaderDialog title="标题" titleId="t" onClose={() => setOpen(false)}>
              <p>正文</p>
            </ReaderDialog>
          )}
        </>
      )
    }
    render(<Harness />)
    const trigger = screen.getByRole('button', { name: '打开' })
    trigger.focus()
    fireEvent.click(trigger)

    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(document.activeElement).toBe(screen.getByRole('button', { name: '关闭' }))

    fireEvent.click(screen.getByRole('button', { name: '关闭' }))

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(document.activeElement).toBe(trigger)
  })
})
