import { expect, test } from '@playwright/test'
import {
  bootstrapReaderPage,
  configureReaderConnection,
  Wp26BackendFixture,
} from './wp26-fixtures'

test('Reader creates one empty note, cleans it on immediate leave, and keeps the list clean after reload', async ({ page }) => {
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)
  await bootstrapReaderPage(page, '?view=notes')

  await expect(page.getByRole('heading', { name: '笔记' })).toBeVisible()
  await page.getByRole('button', { name: '新建笔记' }).click()
  await expect(page).toHaveURL(/\?view=notes&note_id=note-capture-2$/)
  await expect(page.getByRole('textbox', { name: '笔记内容' })).toBeFocused()

  await page.getByRole('tab', { name: '阅读' }).click()
  await expect(page).toHaveURL(/\?view=reading$/)
  await expect.poll(() => backend.notes.has('note-capture-2')).toBe(false)

  await page.getByRole('button', { name: '折叠侧栏' }).click()
  await page.getByRole('tab', { name: '笔记' }).click()
  await expect(page).toHaveURL(/\?view=notes$/)
  await page.reload()
  await expect(page.getByRole('heading', { name: '笔记' })).toBeVisible()
  expect(backend.notes.has('note-capture-2')).toBe(false)
  expect(backend.calls.filter((call) => call.method === 'POST' && call.path === '/api/notes')).toHaveLength(1)
  expect(backend.calls.filter((call) => call.method === 'DELETE' && call.path === '/api/notes/note-capture-2')).toHaveLength(1)
})
