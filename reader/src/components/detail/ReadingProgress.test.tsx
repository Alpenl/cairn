import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ReadingProgressSummary, ReadingProgressStrip } from './ReadingProgress'

describe('ReadingProgress', () => {
  it('clamps invalid values without exposing NaN to the UI', () => {
    const { container } = render(
      <>
        <ReadingProgressStrip progress={Number.NaN} />
        <ReadingProgressSummary progress={Number.NaN} readMinutes={Number.NaN} />
      </>,
    )

    expect(container.querySelector('.read-progress')).toHaveStyle({ width: '0%' })
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '0')
    expect(screen.getByText('已读 0% · 剩 0 分钟')).toBeInTheDocument()
    expect(container.textContent).not.toContain('NaN')
  })
})
