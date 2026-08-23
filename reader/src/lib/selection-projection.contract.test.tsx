import { existsSync, readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { render } from '@testing-library/react'
import { beforeAll, describe, expect, it, vi } from 'vitest'

import { MarkdownView } from '../components/MarkdownView'
import { PlainTextView } from '../components/PlainTextView'
import type { Annotation } from './annotation-domain'
import { getSelectionInfo } from './annotations'

interface SelectionProjectionFixture {
  name: string
  format: 'plain' | 'markdown'
  source: string
  projection: string
  selection: {
    start: number
    end: number
    text: string
    rejected_by_reader_minimum?: boolean
  }
}

function sharedFixturePath(): string {
  const candidates = [
    resolve(process.cwd(), '../test/fixtures/selection_projection.json'),
    resolve(process.cwd(), 'test/fixtures/selection_projection.json'),
  ]
  const fixture = candidates.find((candidate) => existsSync(candidate))
  if (!fixture) throw new Error(`shared selection fixture not found from ${process.cwd()}`)
  return fixture
}

const fixtures = JSON.parse(readFileSync(sharedFixturePath(), 'utf8')) as SelectionProjectionFixture[]

beforeAll(() => {
  if (!Range.prototype.getBoundingClientRect) {
    Range.prototype.getBoundingClientRect = () =>
      ({ left: 0, top: 0, width: 0, height: 0, right: 0, bottom: 0, x: 0, y: 0 }) as DOMRect
  }
})

function boundaryAt(root: Node, utf16Offset: number): { node: Text; offset: number } {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT)
  let consumed = 0
  let current = walker.nextNode() as Text | null
  while (current) {
    const next = consumed + current.data.length
    if (utf16Offset <= next) return { node: current, offset: utf16Offset - consumed }
    consumed = next
    current = walker.nextNode() as Text | null
  }
  throw new Error(`UTF-16 offset ${utf16Offset} exceeds rendered length ${consumed}`)
}

function selectUTF16Range(root: Node, start: number, end: number): void {
  const from = boundaryAt(root, start)
  const to = boundaryAt(root, end)
  const range = document.createRange()
  range.setStart(from.node, from.offset)
  range.setEnd(to.node, to.offset)
  const selection = window.getSelection()
  if (!selection) throw new Error('window selection is unavailable')
  selection.removeAllRanges()
  selection.addRange(range)
}

describe('shared canonical selection projection', () => {
  it.each(fixtures)('$name', (fixture) => {
    const blockKey = fixture.format === 'markdown' ? 'content-document' : 'content'
    const view =
      fixture.format === 'markdown' ? (
        <MarkdownView
          blockKey={blockKey}
          text={fixture.source}
          anns={[]}
          onClickHL={vi.fn()}
        />
      ) : (
        <PlainTextView blockKey={blockKey} text={fixture.source} anns={[]} onClickHL={vi.fn()} />
      )
    const { container } = render(view)
    const block = container.querySelector<HTMLElement>('[data-hl-block]')
    expect(block).not.toBeNull()
    expect(block?.textContent).toBe(fixture.projection)

    selectUTF16Range(block as HTMLElement, fixture.selection.start, fixture.selection.end)
    const info = getSelectionInfo(container)
    if (fixture.selection.rejected_by_reader_minimum) {
      expect(info).toBeNull()
      return
    }
    expect(info).not.toBeNull()
    expect(info).toMatchObject({
      start: fixture.selection.start,
      end: fixture.selection.end,
      text: fixture.selection.text,
    })

    const annotation: Annotation = {
      id: `fixture-${fixture.name}`,
      blockKey,
      start: fixture.selection.start,
      end: fixture.selection.end,
      text: fixture.selection.text,
      note: '',
      source: 'self',
      createdAt: 0,
      updatedAt: 0,
    }
    const highlighted =
      fixture.format === 'markdown' ? (
        <MarkdownView
          blockKey={blockKey}
          text={fixture.source}
          anns={[annotation]}
          onClickHL={vi.fn()}
        />
      ) : (
        <PlainTextView
          blockKey={blockKey}
          text={fixture.source}
          anns={[annotation]}
          onClickHL={vi.fn()}
        />
      )
    const marked = render(highlighted).container
    const markedText = Array.from(marked.querySelectorAll(`mark[data-ann="${annotation.id}"]`))
      .map((node) => node.textContent)
      .join('')
    expect(markedText).toBe(fixture.selection.text)
  })
})
