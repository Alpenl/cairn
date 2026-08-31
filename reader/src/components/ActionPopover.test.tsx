import { describe, expect, it, vi } from 'vitest'
import { render } from '@testing-library/react'
import { NOTE_SELECTION_ACTIONS } from '../lib/selection-actions'
import { ActionPopover } from './ActionPopover'

describe('ActionPopover viewport positioning', () => {
  it('keeps the full action row inside a 390px viewport', () => {
    vi.stubGlobal('innerWidth', 390)
    const { container } = render(
      <ActionPopover
        rect={new DOMRect(20, 300, 280, 52)}
        onAct={() => {}}
      />,
    )

    const popover = container.querySelector<HTMLElement>('.sel-pop')
    expect(popover).not.toBeNull()
    expect(Number.parseFloat(popover?.style.left ?? '0')).toBeGreaterThanOrEqual(8)
    expect(Number.parseFloat(popover?.style.top ?? '0')).toBeGreaterThanOrEqual(8)

    vi.unstubAllGlobals()
  })

  it('opens below a selection near the top edge', () => {
    vi.stubGlobal('innerWidth', 390)
    vi.stubGlobal('innerHeight', 844)
    const rect = new DOMRect(24, 4, 240, 28)
    const { container } = render(<ActionPopover rect={rect} onAct={() => {}} />)

    const popover = container.querySelector<HTMLElement>('.sel-pop')
    expect(popover).not.toBeNull()
    expect(Number.parseFloat(popover?.style.top ?? '0')).toBeGreaterThanOrEqual(
      rect.bottom + 8,
    )

    vi.unstubAllGlobals()
  })

  it('keeps the Notes action order fixed while omitting translation', () => {
    const { container } = render(
      <ActionPopover
        rect={new DOMRect(20, 300, 180, 24)}
        actions={NOTE_SELECTION_ACTIONS}
        onAct={() => {}}
      />,
    )

    expect(Array.from(container.querySelectorAll('button'), (button) => button.textContent?.trim())).toEqual([
      '划线', '写想法', '问 AI', '复制',
    ])
    expect(container).not.toHaveTextContent('翻译')
  })
})
