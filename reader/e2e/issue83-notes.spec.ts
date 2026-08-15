import { expect, test, type Page } from '@playwright/test'

const harness = (scenario: string) => `/__test__/issue83-notes-harness?scenario=${scenario}`

async function browserState(page: Page) {
  return page.evaluate(() => {
    const harness = (window as unknown as {
      issue83NotesHarness: { state(): unknown }
    }).issue83NotesHarness
    return harness.state()
  })
}

test('Issue 83 notes: title derives from the first H1 anywhere in published Markdown', async ({ page }) => {
  await page.goto(harness('title'))
  const editor = page.getByRole('textbox', { name: '笔记内容' })
  await expect(editor).toHaveValue('Initial published body')
  await editor.fill('ordinary prose before heading\n\n# Server H1 wins')
  await page.getByRole('button', { name: '发布', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Server H1 wins' })).toBeVisible()
})

test('Issue 83 notes: empty publish exposes the server 422 and no-op does not add a revision', async ({ page }) => {
  await page.goto(harness('empty'))
  const editor = page.getByRole('textbox', { name: '笔记内容' })
  await editor.fill(' \t\r\n')
  await page.getByRole('button', { name: '发布', exact: true }).click()
  await expect(page.getByText('note content must not be empty')).toBeVisible()

  await page.goto(harness('noop'))
  await page.getByRole('button', { name: '发布', exact: true }).click()
  await expect.poll(() => browserState(page)).toMatchObject({
    note: { published_revision: 2 },
    publishCalls: 1,
    history: [{ revision: 1 }],
  })
})

test('Issue 83 notes: history previews immutable Markdown and dirty restore is rejected', async ({ page }) => {
  await page.goto(harness('dirty-restore'))
  await page.getByRole('button', { name: '历史版本' }).click()
  await page.getByRole('button', { name: '预览', exact: true }).nth(1).click()
  await expect(page.getByRole('region', { name: '历史版本 v1 预览' })).toContainText('Full immutable Markdown body for restore.')
  await page.getByRole('button', { name: '恢复到此版本' }).click()
  await expect(page.getByText('内容已经被其他窗口更新，请刷新后重试。')).toBeVisible()
  await expect.poll(async () => (await browserState(page) as { note: unknown }).note).toMatchObject({
    published_revision: 2,
    draft_content: 'Unpublished draft must survive',
    dirty: true,
  })
})

test('Issue 83 notes: clean restore creates a new revision from immutable history', async ({ page }) => {
  await page.goto(harness('clean-restore'))
  await page.getByRole('button', { name: '历史版本' }).click()
  await page.getByRole('button', { name: '恢复到此版本' }).click()
  await expect(page.getByRole('heading', { name: 'Historical H1' })).toBeVisible()
  await expect(page.getByText('发布 v3 · 草稿 v3', { exact: true })).toBeVisible()
  await expect.poll(async () => (await browserState(page) as { note: unknown }).note).toMatchObject({
    published_content: '# Historical H1\n\nFull immutable Markdown body for restore.',
    published_revision: 3,
    draft_content: null,
    dirty: false,
  })
})
