import { expect, test, type Page } from '@playwright/test'

import {
  bootstrapReaderPage,
  configureReaderConnection,
  WP26_NAMESPACE,
  Wp26BackendFixture,
} from './wp26-fixtures'

const NOTE_ID = 'note-capture-1'

async function openNotes(page: Page, backend: Wp26BackendFixture): Promise<void> {
  await backend.install(page)
  await configureReaderConnection(page)
  await bootstrapReaderPage(page, '?view=notes')
  await expect(page.getByRole('heading', { level: 1, name: '笔记' })).toBeVisible()
  await expect(page.getByRole('textbox', { name: '笔记内容' })).toBeVisible()
}

async function selectNoteText(page: Page, start: number, end: number): Promise<void> {
  const paragraph = page.locator('[data-hl-block="note"] p').first()
  await expect(paragraph).toBeVisible()
  await paragraph.evaluate((element, rangeOffsets) => {
    const block = element.closest('[data-hl-block="note"]')
    const textNode = document.createTreeWalker(element, NodeFilter.SHOW_TEXT).nextNode()
    if (!block) throw new Error('note preview block is missing')
    if (!textNode) throw new Error('note preview paragraph is missing')
    const range = document.createRange()
    range.setStart(textNode, rangeOffsets.start)
    range.setEnd(textNode, rangeOffsets.end)
    const selection = window.getSelection()
    selection?.removeAllRanges()
    selection?.addRange(range)
    block.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }))
  }, { start, end })
}

test('Notes Markdown commands and Edit/Preview preserve the editor viewport', async ({ page }) => {
  const backend = new Wp26BackendFixture()
  const source = [
    '# Round trip heading',
    '',
    ...Array.from({ length: 60 }, (_, index) => `Paragraph ${index} keeps the editor scrollable.`),
  ].join('\n')
  const note = backend.notes.get(NOTE_ID)
  if (!note) throw new Error('WP26 Note fixture is missing')
  note.published_content = source
  await openNotes(page, backend)

  let textarea = page.getByRole('textbox', { name: '笔记内容' })
  const draft = `draft-prefix\n${source}`
  await textarea.fill(draft)
  await textarea.evaluate((element: HTMLTextAreaElement) => {
    element.setSelectionRange(7, 19)
    element.scrollTop = 180
    element.dispatchEvent(new Event('scroll', { bubbles: true }))
  })

  await page.getByRole('button', { name: '预览' }).click()
  await expect(page.getByRole('article', { name: '笔记预览' })).toContainText('draft-prefix')
  await page.getByRole('button', { name: '编辑' }).click()

  textarea = page.getByRole('textbox', { name: '笔记内容' })
  await expect(textarea).toHaveValue(draft)
  await expect.poll(() => textarea.evaluate((element: HTMLTextAreaElement) => ({
    start: element.selectionStart,
    end: element.selectionEnd,
  }))).toEqual({ start: 7, end: 19 })
  await expect.poll(() => textarea.evaluate(
    (element: HTMLTextAreaElement) => Math.abs(element.scrollTop - 180),
  )).toBeLessThanOrEqual(1)

  await textarea.fill('- [x] done')
  await textarea.press('End')
  await textarea.press('Enter')
  await expect(textarea).toHaveValue('- [x] done\n- [ ] ')

  await textarea.fill('word')
  await textarea.evaluate((element: HTMLTextAreaElement) => element.setSelectionRange(0, 4))
  await textarea.press('Control+b')
  await expect(textarea).toHaveValue('**word**')
  await textarea.press('End')
  await textarea.press('!')
  await expect(textarea).toHaveValue('**word**!')
  await textarea.press('Control+z')
  await expect(textarea).toHaveValue('**word**')
  await textarea.press('Control+z')
  await expect(textarea).toHaveValue('word')
  await textarea.press('Control+Shift+z')
  await expect(textarea).toHaveValue('**word**')

  await textarea.fill('/code')
  await expect(page.getByRole('listbox', { name: 'Markdown 命令' })).toBeVisible()
  await textarea.press('Enter')
  await expect(textarea).toHaveValue('```\n\n```')
  await expect.poll(() => textarea.evaluate((element: HTMLTextAreaElement) => element.selectionStart)).toBe(4)
})

test('Notes mobile controls stay usable and the slash menu does not cover the caret', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  const backend = new Wp26BackendFixture()
  await openNotes(page, backend)

  const textarea = page.getByRole('textbox', { name: '笔记内容' })
  await expect(page.getByRole('button', { name: '编辑' })).toBeVisible()
  await expect(page.getByRole('button', { name: '预览' })).toBeVisible()
  await expect(page.getByRole('button', { name: '专注模式' })).toBeVisible()
  await expect(page.getByRole('button', { name: '发布', exact: true })).toBeVisible()

  const slashSource = `${Array.from({ length: 50 }, (_, index) => `line ${index}`).join('\n')}\n/`
  await textarea.fill(slashSource)
  await textarea.evaluate((element: HTMLTextAreaElement) => {
    element.scrollTop = element.scrollHeight
    element.dispatchEvent(new Event('scroll', { bubbles: true }))
  })

  const menu = page.getByRole('listbox', { name: 'Markdown 命令' })
  await expect(menu).toBeVisible()
  const geometry = await menu.evaluate((element) => {
    const rect = element.getBoundingClientRect()
    return {
      placement: element.getAttribute('data-placement'),
      caretTop: Number(element.getAttribute('data-caret-top')),
      caretBottom: Number(element.getAttribute('data-caret-bottom')),
      left: rect.left,
      right: rect.right,
      top: rect.top,
      bottom: rect.bottom,
    }
  })
  expect(geometry.left).toBeGreaterThanOrEqual(8)
  expect(geometry.right).toBeLessThanOrEqual(382)
  expect(geometry.top).toBeGreaterThanOrEqual(8)
  expect(geometry.bottom).toBeLessThanOrEqual(836)
  if (geometry.placement === 'above') {
    expect(geometry.bottom).toBeLessThanOrEqual(geometry.caretTop)
  } else {
    expect(geometry.placement).toBe('below')
    expect(geometry.top).toBeGreaterThanOrEqual(geometry.caretBottom)
  }
})

test('Notes selection AI sends no unselected Note content and stays below mobile controls', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  const selectedText = 'VISIBLE'
  const sentinel = 'UNSELECTED_PRIVATE_SENTINEL'
  const fullNote = `${selectedText} ${sentinel}`
  const backend = new Wp26BackendFixture({ ai: true })
  const note = backend.notes.get(NOTE_ID)
  if (!note) throw new Error('WP26 Note fixture is missing')
  note.published_content = fullNote
  note.published_revision = 9

  const consoleMessages: string[] = []
  page.on('console', (message) => consoleMessages.push(message.text()))
  await backend.install(page)
  const aiRequests: Array<{
    readonly url: string
    readonly headers: Record<string, string>
    readonly body: string
  }> = []
  await page.route('**/api/ai', async (route) => {
    aiRequests.push({
      url: route.request().url(),
      headers: await route.request().allHeaders(),
      body: route.request().postData() ?? '',
    })
    await route.fulfill({
      status: 200,
      headers: {
        'Content-Type': 'application/json',
        'Cache-Control': 'no-store',
        'X-WebTag-Data-Namespace': WP26_NAMESPACE,
      },
      body: JSON.stringify({ enabled: true, answer: 'AI selection explanation' }),
    })
  })
  await configureReaderConnection(page)
  await bootstrapReaderPage(page, '?view=notes')
  await expect(page.getByRole('textbox', { name: '笔记内容' })).toBeVisible()

  await page.getByRole('button', { name: '预览' }).click()
  await selectNoteText(page, 0, selectedText.length)
  await page.getByRole('button', { name: '问 AI' }).click()

  await expect.poll(() => aiRequests.length).toBe(1)
  await expect(page.getByText('AI selection explanation')).toBeVisible()
  const captured = aiRequests[0]
  const body = JSON.parse(captured.body) as Record<string, unknown>
  expect(body).toMatchObject({ scope: 'selection', selected_text: selectedText })
  expect(body).not.toHaveProperty('link_id')
  expect(String(body.prompt)).toContain(
    'Selection source metadata: {"source_type":"note","host_id":"note-capture-1","version":{"note_revision":9},"range":{"start":0,"end":7}}',
  )

  for (const surface of [
    captured.url,
    JSON.stringify(captured.headers),
    captured.body,
    consoleMessages.join('\n'),
  ]) {
    expect(surface).not.toContain(sentinel)
    expect(surface).not.toContain(fullNote)
  }

  const toolbar = page.locator('.rvx-note-toolbar')
  const chat = page.locator('.notes-split > .chat')
  await expect(toolbar).toBeVisible()
  await expect(chat).toBeVisible()
  await expect.poll(() => chat.evaluate((element) => getComputedStyle(element).position)).toBe('static')
  const toolbarBox = await toolbar.boundingBox()
  const chatBox = await chat.boundingBox()
  expect(toolbarBox).not.toBeNull()
  expect(chatBox).not.toBeNull()
  expect(chatBox!.y).toBeGreaterThanOrEqual(toolbarBox!.y + toolbarBox!.height)
})
