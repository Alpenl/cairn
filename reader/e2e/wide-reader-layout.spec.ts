import { expect, test } from '@playwright/test'
import { bootstrapReaderPage, configureReaderConnection, Wp26BackendFixture } from './wp26-fixtures'

const VIEWPORT = { width: 2048, height: 1000 }

test.use({ viewport: VIEWPORT })

test('wide reading detail keeps the article in the first grid row', async ({ page }) => {
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)
  await bootstrapReaderPage(page, '?view=reading')

  const article = page.locator('.reader-inner')
  const rail = page.locator('.reader-rail')
  await expect(article).toBeVisible()
  await expect(rail).toBeVisible()

  const offset = await article.evaluate((element) => {
    const scroller = element.closest('.reader-scroll')
    if (!(scroller instanceof HTMLElement)) throw new Error('reader scroller is missing')
    return Math.round(element.getBoundingClientRect().top - scroller.getBoundingClientRect().top)
  })

  expect(offset).toBeLessThanOrEqual(1)
})

test('narrow reading detail remains a single-column flow', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)
  await bootstrapReaderPage(page, '?view=reading')

  const article = page.locator('.reader-inner')
  if (!(await article.isVisible())) {
    await page.locator('.card').first().click()
  }
  await expect(article).toBeVisible()

  const layout = await article.evaluate((element) => {
    const scroller = element.closest('.reader-scroll')
    if (!(scroller instanceof HTMLElement)) throw new Error('reader scroller is missing')
    return {
      display: getComputedStyle(scroller).display,
      offset: Math.round(element.getBoundingClientRect().top - scroller.getBoundingClientRect().top),
      articleWidth: element.getBoundingClientRect().width,
      scrollerWidth: scroller.getBoundingClientRect().width,
    }
  })

  expect(layout.display).toBe('block')
  expect(layout.offset).toBeLessThanOrEqual(1)
  expect(layout.articleWidth).toBeLessThanOrEqual(layout.scrollerWidth)
})
