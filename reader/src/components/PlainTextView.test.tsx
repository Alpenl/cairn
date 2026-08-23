import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { Annotation } from '../lib/annotation-domain'
import { PlainTextView } from './PlainTextView'

function annotation(contentRevision: number, start: number, end: number): Annotation {
  return {
    id: 'shared',
    blockKey: 'content',
    start,
    end,
    text: '',
    note: '',
    source: 'self',
    createdAt: contentRevision,
    updatedAt: contentRevision,
    sourceContentRevision: contentRevision,
  }
}

describe('PlainTextView', () => {
  it('同 ID、同 block 的不同 saved revisions 由 mark target key 精确定位', () => {
    const onClickHL = vi.fn()
    render(
      <PlainTextView
        blockKey="content"
        text="old new"
        anns={[annotation(7, 0, 3), annotation(8, 4, 7)]}
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
})
