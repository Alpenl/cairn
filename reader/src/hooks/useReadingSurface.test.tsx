import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { useRef } from 'react'

import { useReadingSurface } from './useReadingSurface'
import { plainSource } from '../lib/reading-surface'
import type { TocHeading } from '../lib/toc'

function Probe({
  id,
  capabilities,
}: {
  id: string
  capabilities: Parameters<typeof useReadingSurface>[0]['capabilities']
}) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const surface = useReadingSurface({
    source: plainSource('', { hostId: id, version: '1' }),
    capabilities,
    scrollRef,
    layoutKey: 'test',
  })
  return (
    <div ref={scrollRef}>
      <output data-testid={`${id}-focus`}>{String(surface.focusMode)}</output>
      <output data-testid={`${id}-size`}>{surface.readingPreference.size}</output>
      <output data-testid={`${id}-toc`}>{String(surface.toc.items.length)}</output>
      <output data-testid={`${id}-toc-first`}>{surface.toc.items[0]?.text ?? ''}</output>
      <output data-testid={`${id}-capabilities`}>
        {Array.from(surface.contract.capabilities).join(',')}
      </output>
      <button type="button" onClick={() => surface.setFocusMode(true)}>focus</button>
      <button type="button" onClick={() => surface.setReadingPreference({ size: 3 })}>large</button>
      <button
        type="button"
        onClick={() => surface.toc.onHeadings([
          { id: 'heading-1', level: 1, text: 'Heading 1' } satisfies TocHeading,
        ])}
      >
        headings
      </button>
    </div>
  )
}

describe('useReadingSurface contract', () => {
  it('shares focus and preferences between Reading, Subscription, and Notes consumers', () => {
    render(
      <>
        <Probe id="reading" capabilities={['focus', 'preferences']} />
        <Probe id="subscription" capabilities={['focus', 'preferences']} />
        <Probe id="notes" capabilities={['focus', 'preferences']} />
      </>,
    )

    fireEvent.click(screen.getByTestId('reading-focus').parentElement!.querySelector('button')!)
    expect(screen.getByTestId('reading-focus')).toHaveTextContent('true')
    expect(screen.getByTestId('subscription-focus')).toHaveTextContent('true')
    expect(screen.getByTestId('notes-focus')).toHaveTextContent('true')

    fireEvent.click(screen.getByTestId('notes-size').parentElement!.querySelectorAll('button')[1])
    expect(screen.getByTestId('reading-size')).toHaveTextContent('3')
    expect(screen.getByTestId('subscription-size')).toHaveTextContent('3')
    expect(screen.getByTestId('notes-size')).toHaveTextContent('3')
  })

  it('keeps a single storage owner across three mounted preference consumers', () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    render(
      <>
        <Probe id="reading-owner" capabilities={['preferences']} />
        <Probe id="subscription-owner" capabilities={['preferences']} />
        <Probe id="notes-owner" capabilities={['preferences']} />
      </>,
    )

    fireEvent.click(screen.getByTestId('notes-owner-size').parentElement!.querySelectorAll('button')[1])

    expect(setItem.mock.calls.filter(([key]) => key === 'webtag:reading-preference')).toHaveLength(1)
  })

  it('keeps disabled capabilities inert and does not expose stale toc state', () => {
    render(<Probe id="disabled" capabilities={[]} />)
    expect(screen.getByTestId('disabled-focus')).toHaveTextContent('false')
    expect(screen.getByTestId('disabled-toc')).toHaveTextContent('0')

    act(() => {
      fireEvent.click(screen.getByRole('button', { name: 'focus' }))
      fireEvent.click(screen.getByRole('button', { name: 'large' }))
    })
    expect(screen.getByTestId('disabled-focus')).toHaveTextContent('false')
    expect(screen.getByTestId('disabled-size')).toHaveTextContent('1')
  })

  it('clears toc state when a consumer loses the toc capability', () => {
    const { rerender } = render(<Probe id="toggle" capabilities={['toc']} />)

    fireEvent.click(screen.getByRole('button', { name: 'headings' }))
    expect(screen.getByTestId('toggle-toc')).toHaveTextContent('1')

    rerender(<Probe id="toggle" capabilities={[]} />)

    expect(screen.getByTestId('toggle-toc')).toHaveTextContent('0')

    rerender(<Probe id="toggle" capabilities={['toc']} />)
    expect(screen.getByTestId('toggle-toc')).toHaveTextContent('0')
    expect(screen.getByTestId('toggle-toc-first')).toHaveTextContent('')
  })

  it('does not hydrate or persist preferences while the capability is disabled', async () => {
    const persisted = JSON.stringify({ size: 3, lineHeight: 2 })
    localStorage.setItem('webtag:reading-preference', persisted)
    const { rerender } = render(<Probe id="preference-gate" capabilities={[]} />)

    expect(screen.getByTestId('preference-gate-size')).toHaveTextContent('1')
    expect(localStorage.getItem('webtag:reading-preference')).toBe(persisted)

    rerender(<Probe id="preference-gate" capabilities={['preferences']} />)
    await waitFor(() => expect(screen.getByTestId('preference-gate-size')).toHaveTextContent('3'))
  })

  it('keeps the declared capability snapshot available to consumers', () => {
    render(<Probe id="matrix" capabilities={['focus', 'progress', 'toc', 'back-to-top', 'pager']} />)

    expect(screen.getByTestId('matrix-capabilities')).toHaveTextContent(
      'focus,progress,toc,back-to-top,pager',
    )
  })
})
