import { useRef } from 'react'
import { render, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { MarkdownView } from '../MarkdownView'
import { NoteMarkdownPreview, NOTE_HEADING_ID_PREFIX } from './NoteMarkdownPreview'
import type { TocHeading } from '../../lib/toc'

const MARKDOWN = [
  '# Heading',
  '',
  '## Child',
  '',
  '### Third',
  '',
  '- [x] GFM task',
  '',
  '| A | B |',
  '| - | - |',
  '| 1 | 2 |',
  '',
  '[external](https://example.com)',
  '',
  '```ts',
  'const value = 1',
  '```',
  '',
  '<script>window.__unsafe = true</script>',
].join('\n')

function NotePreviewProbe({ onHeadings }: { readonly onHeadings: (items: TocHeading[]) => void }) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const contentRef = useRef<HTMLDivElement>(null)
  return (
    <NoteMarkdownPreview
      text={MARKDOWN}
      annotations={[]}
      focusMode={false}
      scrollRef={scrollRef}
      contentRef={contentRef}
      tocItems={[]}
      activeTocId={null}
      onHeadings={onHeadings}
      onJumpToc={() => {}}
      onScroll={() => {}}
      onMouseUp={() => {}}
      onClickHighlight={() => {}}
    />
  )
}

describe('Notes Preview renderer contract', () => {
  it('matches the formal reading Markdown HTML, heading IDs, and TOC tree exactly', async () => {
    const notesHeadings = vi.fn<(items: TocHeading[]) => void>()
    const readingHeadings = vi.fn<(items: TocHeading[]) => void>()
    const notes = render(<NotePreviewProbe onHeadings={notesHeadings} />)
    const reading = render(
      <MarkdownView
        text={MARKDOWN}
        blockKey="content-document"
        anns={[]}
        onClickHL={() => {}}
        headingIdPrefix={NOTE_HEADING_ID_PREFIX}
        onHeadings={readingHeadings}
      />,
    )

    await waitFor(() => {
      expect(notesHeadings).toHaveBeenCalled()
      expect(readingHeadings).toHaveBeenCalled()
    })
    const notesMarkdown = notes.container.querySelector('.md')
    const readingMarkdown = reading.container.querySelector('.md')
    expect(notesMarkdown?.innerHTML).toBe(readingMarkdown?.innerHTML)
    expect(Array.from(notesMarkdown?.querySelectorAll('h1,h2,h3') ?? [], (heading) => heading.id)).toEqual([
      'toc-h0', 'toc-h1', 'toc-h2',
    ])
    expect(notesHeadings.mock.calls.at(-1)?.[0]).toEqual(readingHeadings.mock.calls.at(-1)?.[0])
    expect(notesMarkdown?.querySelector('script')).toBeNull()
    expect(notesMarkdown?.querySelector('a')).toHaveAttribute('target', '_blank')
    expect(notesMarkdown?.querySelector('pre code')).toHaveTextContent('const value = 1')
    expect(notesMarkdown?.querySelector('table')).not.toBeNull()
  })
})
