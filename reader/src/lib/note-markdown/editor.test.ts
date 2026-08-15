import { describe, expect, it } from 'vitest'

import {
  NOTE_SLASH_COMMANDS,
  applySlashCommand,
  continueMarkdownList,
  filterSlashCommands,
  indentMarkdownLists,
  slashQueryAt,
  toggleMarkdownFormat,
} from './editor'

describe('note markdown list transforms', () => {
  it.each([
    ['unordered dash', '- first', 7, '- first\n- ', 10],
    ['unordered star', '* first', 7, '* first\n* ', 10],
    ['ordered increment', '9. ninth', 8, '9. ninth\n10. ', 13],
    ['ordered parenthesis', '3) third', 8, '3) third\n4) ', 12],
    ['unchecked task', '- [ ] next', 10, '- [ ] next\n- [ ] ', 17],
    ['checked task becomes unchecked', '- [x] done', 10, '- [x] done\n- [ ] ', 17],
    ['uppercase checked task becomes unchecked', '- [X] done', 10, '- [X] done\n- [ ] ', 17],
    ['nested marker preserves indent', '  - child', 9, '  - child\n  - ', 14],
  ])('%s', (_name, text, caret, expectedText, expectedCaret) => {
    expect(continueMarkdownList(text, { start: caret, end: caret })).toEqual({
      text: expectedText,
      start: expectedCaret,
      end: expectedCaret,
      handled: true,
    })
  })

  it.each([
    ['unordered', 'before\n- ', 9, 'before\n', 7],
    ['ordered', 'before\n12.   ', 13, 'before\n', 7],
    ['task', 'before\n  - [x] ', 15, 'before\n', 7],
  ])('removes an empty %s marker and exits the list', (_name, text, caret, expectedText, expectedCaret) => {
    expect(continueMarkdownList(text, { start: caret, end: caret })).toEqual({
      text: expectedText,
      start: expectedCaret,
      end: expectedCaret,
      handled: true,
    })
  })

  it('splits a non-empty list item at the caret and keeps the suffix after the new marker', () => {
    expect(continueMarkdownList('- before after', { start: 8, end: 8 })).toEqual({
      text: '- before\n- after',
      start: 11,
      end: 11,
      handled: true,
    })
  })

  it.each([
    ['range selection', '- item', { start: 2, end: 4 }],
    ['plain line', 'plain', { start: 5, end: 5 }],
    ['inside marker', '- item', { start: 1, end: 1 }],
  ])('leaves %s to the browser', (_name, text, selection) => {
    expect(continueMarkdownList(text, selection)).toMatchObject({ text, ...selection, handled: false })
  })

  it('indents every selected list line by two ASCII spaces and maps partial boundaries', () => {
    const text = '- one\n1. two\n- [ ] three\nafter'
    expect(indentMarkdownLists(text, { start: 2, end: 25 }, 'indent')).toEqual({
      text: '  - one\n  1. two\n  - [ ] three\nafter',
      start: 4,
      end: 31,
      handled: true,
    })
  })

  it('does not include the next line when selectionEnd is exactly its boundary', () => {
    const text = '- one\n- two\nplain'
    expect(indentMarkdownLists(text, { start: 0, end: 6 }, 'indent')).toEqual({
      text: '  - one\n- two\nplain',
      start: 2,
      end: 8,
      handled: true,
    })
  })

  it('outdents at most two ASCII spaces per selected line and maps the caret', () => {
    const text = '    - one\n - two\n- three'
    expect(indentMarkdownLists(text, { start: 6, end: text.length }, 'outdent')).toEqual({
      text: '  - one\n- two\n- three',
      start: 4,
      end: 21,
      handled: true,
    })
  })

  it('keeps Tab in its browser-default path when any selected line is not a list', () => {
    const text = '- one\nplain'
    expect(indentMarkdownLists(text, { start: 0, end: text.length }, 'indent')).toEqual({
      text,
      start: 0,
      end: text.length,
      handled: false,
    })
  })

  it('handles root outdent without manufacturing a text change', () => {
    expect(indentMarkdownLists('- root', { start: 3, end: 3 }, 'outdent')).toEqual({
      text: '- root', start: 3, end: 3, handled: true,
    })
  })
})

describe('note markdown emphasis transforms', () => {
  it.each([
    ['bold selection', 'hello', { start: 0, end: 5 }, 'bold', '**hello**', { start: 2, end: 7 }],
    ['italic selection', 'hello', { start: 0, end: 5 }, 'italic', '*hello*', { start: 1, end: 6 }],
    ['bold empty', 'ab', { start: 1, end: 1 }, 'bold', 'a****b', { start: 3, end: 3 }],
    ['italic empty', 'ab', { start: 1, end: 1 }, 'italic', 'a**b', { start: 2, end: 2 }],
    ['bold multiline', 'one\ntwo', { start: 0, end: 7 }, 'bold', '**one\ntwo**', { start: 2, end: 9 }],
    ['italic emoji', 'A😀B', { start: 1, end: 3 }, 'italic', 'A*😀*B', { start: 2, end: 4 }],
  ] as const)('%s', (_name, text, selection, format, expectedText, expectedSelection) => {
    expect(toggleMarkdownFormat(text, selection, format)).toEqual({
      text: expectedText,
      ...expectedSelection,
      handled: true,
    })
  })

  it.each([
    ['selected bold wrapper', '**hello**', { start: 0, end: 9 }, 'bold', 'hello', { start: 0, end: 5 }],
    ['surrounding bold wrapper', '**hello**', { start: 2, end: 7 }, 'bold', 'hello', { start: 0, end: 5 }],
    ['selected italic wrapper', '*hello*', { start: 0, end: 7 }, 'italic', 'hello', { start: 0, end: 5 }],
    ['surrounding italic wrapper', '*hello*', { start: 1, end: 6 }, 'italic', 'hello', { start: 0, end: 5 }],
  ] as const)('toggles off a %s', (_name, text, selection, format, expectedText, expectedSelection) => {
    expect(toggleMarkdownFormat(text, selection, format)).toEqual({
      text: expectedText,
      ...expectedSelection,
      handled: true,
    })
  })

  it('supports consecutive bold and italic commands without confusing bold for italic', () => {
    const bold = toggleMarkdownFormat('hello', { start: 0, end: 5 }, 'bold')
    const italic = toggleMarkdownFormat(bold.text, bold, 'italic')
    expect(italic).toEqual({ text: '***hello***', start: 3, end: 8, handled: true })
    expect(toggleMarkdownFormat(italic.text, italic, 'italic')).toEqual({
      text: '**hello**', start: 2, end: 7, handled: true,
    })
  })
})

describe('note markdown slash commands', () => {
  it('exposes exactly the frozen nine commands in order', () => {
    expect(NOTE_SLASH_COMMANDS.map((command) => command.id)).toEqual([
      'h1', 'h2', 'h3', 'unordered-list', 'ordered-list', 'task-list', 'quote', 'code-block', 'divider',
    ])
  })

  it.each(NOTE_SLASH_COMMANDS)('applies $id and places the caret exactly', (command) => {
    const query = slashQueryAt('/query', { start: 6, end: 6 })
    expect(query).not.toBeNull()
    const result = applySlashCommand('/query', query!, command)
    expect(result.text).toBe(command.replacement)
    expect(result.start).toBe(command.caretOffset ?? command.replacement.length)
    expect(result.end).toBe(result.start)
  })

  it('preserves optional leading ASCII spaces while replacing only the query', () => {
    const query = slashQueryAt('before\n  /h2', { start: 12, end: 12 })
    expect(query).toEqual({ query: 'h2', start: 9, end: 12 })
    expect(applySlashCommand('before\n  /h2', query!, NOTE_SLASH_COMMANDS[1])).toEqual({
      text: 'before\n  ## ', start: 12, end: 12, handled: true,
    })
  })

  it('puts the code caret between the two fences', () => {
    const query = slashQueryAt('/code', { start: 5, end: 5 })!
    const command = NOTE_SLASH_COMMANDS.find((item) => item.id === 'code-block')!
    expect(applySlashCommand('/code', query, command)).toEqual({
      text: '```\n\n```', start: 4, end: 4, handled: true,
    })
  })

  it.each([
    ['not at logical line start', 'text /h1', 8],
    ['tab is not an ASCII leading space', '\t/h1', 4],
    ['text follows the caret', '/h1 suffix', 3],
    ['selection is not collapsed', '/h1', 0, 3],
  ])('does not open for %s', (_name, text, start, end = start) => {
    expect(slashQueryAt(text, { start, end })).toBeNull()
  })

  it('filters by id, English alias, and Chinese label without changing command order', () => {
    expect(filterSlashCommands('heading').map((command) => command.id)).toEqual(['h1', 'h2', 'h3'])
    expect(filterSlashCommands('todo').map((command) => command.id)).toEqual(['task-list'])
    expect(filterSlashCommands('引用').map((command) => command.id)).toEqual(['quote'])
    expect(filterSlashCommands('missing')).toEqual([])
  })
})
