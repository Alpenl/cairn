import { expect, test, type Page } from '@playwright/test'
import { bootstrapReaderPage, configureReaderConnection } from './wp26-fixtures'
import {
  readFeedAnchorKeys,
  readFeedViewport,
  Rf57BackendFixture,
  rf57CardKey,
  rf57ItemTitle,
  scrollFeedToCard,
  toggleFeedSource,
  type Rf57Mode,
  type Rf57Source,
} from './rf57-feed-fixtures'

const ALL_SOURCES: Rf57Source[] = ['inbox', 'reading', 'subscription']
const WITHOUT_SUBSCRIPTIONS: Rf57Source[] = ['inbox', 'reading']

async function openFeed(page: Page): Promise<Rf57BackendFixture> {
  const backend = new Rf57BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)
  await bootstrapReaderPage(page, '?surface=feed')
  await expect(page.getByRole('heading', { level: 1, name: '混合 Feed' })).toBeVisible()
  return backend
}

async function expectCardVisible(page: Page, mode: Rf57Mode, filter: Rf57Source[], index: number): Promise<void> {
  await expect(page.getByText(rf57ItemTitle(mode, filter, index), { exact: true })).toBeVisible()
}

/** The card must sit `offset` px below the viewport top, within the 2px budget. */
async function expectAnchoredAt(
  page: Page,
  mode: Rf57Mode,
  filter: Rf57Source[],
  index: number,
  offset: number,
): Promise<void> {
  const key = rf57CardKey(mode, filter, index)
  await expect.poll(async () => {
    const viewport = await readFeedViewport(page, key)
    if (viewport.anchorOffset === null) return `card ${key} is not rendered`
    const drift = viewport.anchorOffset - offset
    return Math.abs(drift) <= 2 ? 'anchored' : `off by ${drift.toFixed(3)}px`
  }).toBe('anchored')
}

async function expectAtTop(page: Page, mode: Rf57Mode, filter: Rf57Source[]): Promise<void> {
  await expect.poll(async () => {
    const viewport = await readFeedViewport(page, null)
    return { scrollTop: viewport.scrollTop, firstVisibleKey: viewport.firstVisibleKey }
  }).toEqual({ scrollTop: 0, firstVisibleKey: rf57CardKey(mode, filter, 0) })
}

async function loadSecondPage(page: Page, mode: Rf57Mode, filter: Rf57Source[]): Promise<void> {
  await page.getByRole('button', { name: '加载更多' }).click()
  await expectCardVisible(page, mode, filter, 12)
}

test('RF57 Feed returns to the same card after a detail roundtrip, Back/Forward and a reload', async ({ page }) => {
  const backend = await openFeed(page)
  await expectCardVisible(page, 'recommended', ALL_SOURCES, 0)
  await loadSecondPage(page, 'recommended', ALL_SOURCES)
  const requestsAfterPaging = backend.feedRequests.length
  expect(backend.feedRequests.filter((request) => request.includes('after='))).toHaveLength(1)

  // A card from the second page, deliberately scrolled part-way into view.
  await scrollFeedToCard(page, rf57CardKey('recommended', ALL_SOURCES, 15), 40)
  await expectAnchoredAt(page, 'recommended', ALL_SOURCES, 15, 40)

  const card = page.locator('li.rvx-feed-card').filter({ hasText: rf57ItemTitle('recommended', ALL_SOURCES, 15) })
  await card.getByRole('button', { name: '打开' }).click()
  await expect(page.getByRole('heading', { level: 1, name: '混合 Feed' })).toHaveCount(0)

  await page.goBack()
  await expect(page.getByRole('heading', { level: 1, name: '混合 Feed' })).toBeVisible()
  // Both pages come back, otherwise the anchored card could not be on screen.
  await expectCardVisible(page, 'recommended', ALL_SOURCES, 0)
  await expectCardVisible(page, 'recommended', ALL_SOURCES, 12)
  await expectAnchoredAt(page, 'recommended', ALL_SOURCES, 15, 40)

  await page.goForward()
  await expect(page.getByRole('heading', { level: 1, name: '混合 Feed' })).toHaveCount(0)
  await page.goBack()
  await expect(page.getByRole('heading', { level: 1, name: '混合 Feed' })).toBeVisible()
  await expectAnchoredAt(page, 'recommended', ALL_SOURCES, 15, 40)

  await page.reload()
  await expect(page.getByRole('heading', { level: 1, name: '混合 Feed' })).toBeVisible()
  await expectAnchoredAt(page, 'recommended', ALL_SOURCES, 15, 40)

  expect(backend.feedRequests).toHaveLength(requestsAfterPaging)
  expect(backend.feedRequests.every((request) => !request.includes('snapshot_id'))).toBe(true)
})

test('RF57 Feed keeps a separate position per mode and per source filter', async ({ page }) => {
  await openFeed(page)
  await expectCardVisible(page, 'recommended', ALL_SOURCES, 0)
  await scrollFeedToCard(page, rf57CardKey('recommended', ALL_SOURCES, 5), 30)
  await expectAnchoredAt(page, 'recommended', ALL_SOURCES, 5, 30)

  await page.getByRole('button', { name: '时间', exact: true }).click()
  await expectCardVisible(page, 'chronological', ALL_SOURCES, 0)
  await expectAtTop(page, 'chronological', ALL_SOURCES)

  await scrollFeedToCard(page, rf57CardKey('chronological', ALL_SOURCES, 8), 10)
  await page.getByRole('button', { name: '推荐', exact: true }).click()
  await expectCardVisible(page, 'recommended', ALL_SOURCES, 0)
  await expectAnchoredAt(page, 'recommended', ALL_SOURCES, 5, 30)

  await page.getByRole('button', { name: '时间', exact: true }).click()
  await expectCardVisible(page, 'chronological', ALL_SOURCES, 0)
  await expectAnchoredAt(page, 'chronological', ALL_SOURCES, 8, 10)

  // A filter change is its own key: first entry starts at the top.
  await toggleFeedSource(page, '订阅')
  await expectCardVisible(page, 'chronological', WITHOUT_SUBSCRIPTIONS, 0)
  await expectAtTop(page, 'chronological', WITHOUT_SUBSCRIPTIONS)

  await scrollFeedToCard(page, rf57CardKey('chronological', WITHOUT_SUBSCRIPTIONS, 3), 20)
  await toggleFeedSource(page, '订阅')
  await expectCardVisible(page, 'chronological', ALL_SOURCES, 0)
  await expectAnchoredAt(page, 'chronological', ALL_SOURCES, 8, 10)

  await toggleFeedSource(page, '订阅')
  await expectCardVisible(page, 'chronological', WITHOUT_SUBSCRIPTIONS, 0)
  await expectAnchoredAt(page, 'chronological', WITHOUT_SUBSCRIPTIONS, 3, 20)

  await page.getByRole('button', { name: '推荐', exact: true }).click()
  await expectCardVisible(page, 'recommended', WITHOUT_SUBSCRIPTIONS, 0)
  await expectAtTop(page, 'recommended', WITHOUT_SUBSCRIPTIONS)
})

test('RF57 explicit refresh drops only the refreshed key position', async ({ page }) => {
  await openFeed(page)
  await expectCardVisible(page, 'recommended', ALL_SOURCES, 0)
  await scrollFeedToCard(page, rf57CardKey('recommended', ALL_SOURCES, 5), 30)

  await page.getByRole('button', { name: '时间', exact: true }).click()
  await expectCardVisible(page, 'chronological', ALL_SOURCES, 0)
  await scrollFeedToCard(page, rf57CardKey('chronological', ALL_SOURCES, 8), 10)

  await page.getByRole('button', { name: '推荐', exact: true }).click()
  await expectCardVisible(page, 'recommended', ALL_SOURCES, 0)
  await expectAnchoredAt(page, 'recommended', ALL_SOURCES, 5, 30)

  expect(await readFeedAnchorKeys(page)).toEqual([
    'chronological:inbox,reading,subscription:scroll',
    'recommended:inbox,reading,subscription:scroll',
  ])

  await page.getByRole('button', { name: '刷新' }).click()
  await expectAtTop(page, 'recommended', ALL_SOURCES)
  expect(await readFeedAnchorKeys(page)).toEqual(['chronological:inbox,reading,subscription:scroll'])

  await page.getByRole('button', { name: '时间', exact: true }).click()
  await expectCardVisible(page, 'chronological', ALL_SOURCES, 0)
  await expectAnchoredAt(page, 'chronological', ALL_SOURCES, 8, 10)

  await page.getByRole('button', { name: '推荐', exact: true }).click()
  await expectCardVisible(page, 'recommended', ALL_SOURCES, 0)
  await expectAtTop(page, 'recommended', ALL_SOURCES)
})
