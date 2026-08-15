import { expect, test } from '@playwright/test'

const HISTORY_INDEX_KEY = '__webtag_reader_history_index'

async function currentReaderHistoryIndex(page: import('@playwright/test').Page): Promise<number> {
  return page.evaluate((key) => {
    const value = history.state?.[key]
    if (typeof value !== 'number' || !Number.isSafeInteger(value)) {
      throw new Error(`missing Reader history index: ${String(value)}`)
    }
    return value
  }, HISTORY_INDEX_KEY)
}

test('Reader guard restores dirty Back/Forward cancellation without rewriting the target entry', async ({ page }) => {
  await page.goto('/__test__/navigation-guard-harness', { waitUntil: 'domcontentloaded' })

  await expect(page.getByTestId('surface')).toHaveText('reading')
  expect(await currentReaderHistoryIndex(page)).toBe(0)

  await page.getByRole('button', { name: 'commit-history' }).click()
  await expect(page).toHaveURL(/\?tool=history$/)
  expect(await currentReaderHistoryIndex(page)).toBe(1)

  await page.goBack()
  await expect(page).toHaveURL(/\?view=reading$/)
  await expect(page.getByTestId('surface')).toHaveText('reading')
  expect(await currentReaderHistoryIndex(page)).toBe(0)

  await page.getByRole('button', { name: 'make-dirty' }).click()
  await expect(page.getByTestId('draft')).toHaveText('dirty')

  page.once('dialog', (dialog) => void dialog.dismiss())
  await page.goForward()

  await expect(page).toHaveURL(/\?view=reading$/)
  await expect(page.getByTestId('surface')).toHaveText('reading')
  await expect(page.getByTestId('draft')).toHaveText('dirty')
  expect(await currentReaderHistoryIndex(page)).toBe(0)

  page.once('dialog', (dialog) => void dialog.accept())
  await page.goForward()
  await expect(page).toHaveURL(/\?tool=history$/)
  await expect(page.getByTestId('surface')).toHaveText('history')
  expect(await currentReaderHistoryIndex(page)).toBe(1)
})
