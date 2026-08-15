import { expect, test } from '@playwright/test'
import { bootstrapReaderPage, configureReaderConnection } from './wp26-fixtures'

test('website library loads the 61st site in fixed 30-item pages', async ({ page }) => {
  await configureReaderConnection(page)
  await bootstrapReaderPage(page, '?view=sites')

  await expect(page.getByText('Pagination site 001')).toBeVisible()
  await expect(page.getByText('Pagination site 061')).toHaveCount(0)

  await page.getByRole('button', { name: '加载更多网站' }).click()
  await expect(page.getByText('Pagination site 060')).toBeVisible()

  await page.getByRole('button', { name: '加载更多网站' }).click()
  await expect(page.getByText('Pagination site 061')).toBeVisible()
  await expect(page.getByRole('button', { name: '加载更多网站' })).toHaveCount(0)
})
