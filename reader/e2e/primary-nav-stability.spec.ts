import { expect, test, type Page } from '@playwright/test'
import { bootstrapReaderPage, configureReaderConnection, Wp26BackendFixture } from './wp26-fixtures'

/**
 * The primary navigation must not move when the route changes.
 *
 * Before the shared PrimaryNav, the nine entries were rendered by two different
 * components: `.rvx-nav` for the vNext surfaces and `.sidebar` variants for the
 * library views. Measured on production at 1440x900, 设置 sat at y=832 on 首页
 * and y=303 on 阅读 — a 529px jump, because the tools group used
 * `margin-top: auto` in one shell and plain flow in the other. Even the top
 * group slid 14-21px from the differing container padding.
 *
 * This suite pins the contract that replaced it: for every main route, every
 * primary entry keeps the same viewport position, so a reader can build muscle
 * memory and click twice in the same place.
 */

test.use({ viewport: { width: 1440, height: 900 } })

/** Every route reachable from the primary navigation itself. */
const ROUTES: ReadonlyArray<string> = [
  '?surface=home',
  '?view=pending',
  '?view=reading',
  '?view=sites',
  '?view=subs',
  '?view=notes',
  '?tool=todo',
  '?tool=settings',
  '?tool=history&thought_view=live',
]

/** Labels are read from the DOM, not asserted, so renames do not break this. */
interface NavGeometry {
  readonly labels: ReadonlyArray<string>
  readonly tops: ReadonlyArray<number>
  readonly lefts: ReadonlyArray<number>
}

async function measurePrimaryNav(page: Page): Promise<NavGeometry> {
  return page.evaluate(() => {
    const nav = document.querySelector('.wt-primary-nav')
    if (!(nav instanceof HTMLElement)) throw new Error('the primary navigation is missing')
    const entries = [...nav.querySelectorAll<HTMLElement>('.rvx-nav-action, .library-mode-row')]
    if (entries.length === 0) throw new Error('the primary navigation rendered no entries')
    return {
      // Strip the unread badge: 收件箱 carries a count that changes with fixtures.
      labels: entries.map((entry) => (entry.textContent ?? '').trim().replace(/\d+$/, '')),
      tops: entries.map((entry) => Math.round(entry.getBoundingClientRect().top)),
      lefts: entries.map((entry) => Math.round(entry.getBoundingClientRect().left)),
    }
  })
}

async function openRoute(page: Page, query: string): Promise<NavGeometry> {
  await bootstrapReaderPage(page, query)
  await expect(page.locator('.wt-primary-nav')).toBeVisible()
  // Settle the shell before measuring: a surface that is still resolving its
  // first paint would report a transient position.
  await page.waitForTimeout(150)
  return measurePrimaryNav(page)
}

test('every primary navigation entry keeps its position across all main routes', async ({ page }) => {
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)

  const baseline = await openRoute(page, ROUTES[0])
  // 今天 + 四个内容库 + 设置。降级掉的网站/TODO/想法仍是合法路由，只是不再
  // 占一个入口。
  expect(baseline.labels).toEqual(['今天', '收件箱', '阅读', '订阅', '笔记', '设置'])

  for (const query of ROUTES.slice(1)) {
    const geometry = await openRoute(page, query)

    expect(geometry.labels, `${query} renders the same entries`).toEqual(baseline.labels)
    for (const [index, top] of geometry.tops.entries()) {
      expect(
        Math.abs(top - baseline.tops[index]),
        `${query} moved "${geometry.labels[index]}" vertically (${baseline.tops[index]} -> ${top})`,
      ).toBeLessThanOrEqual(1)
      expect(
        Math.abs(geometry.lefts[index] - baseline.lefts[index]),
        `${query} moved "${geometry.labels[index]}" horizontally`,
      ).toBeLessThanOrEqual(1)
    }
  }
})

test('the tools group sits directly under the library modes instead of the viewport bottom', async ({ page }) => {
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)
  await openRoute(page, '?surface=home')

  const layout = await page.evaluate(() => {
    const nav = document.querySelector('.wt-primary-nav')
    const modes = document.querySelector('.wt-primary-nav .library-mode-nav')
    const tools = document.querySelector('.wt-primary-nav .wt-nav-tools')
    if (!(nav instanceof HTMLElement) || !(modes instanceof HTMLElement) || !(tools instanceof HTMLElement)) {
      throw new Error('the primary navigation did not render its groups')
    }
    return {
      gapBelowModes: tools.getBoundingClientRect().top - modes.getBoundingClientRect().bottom,
      navBottom: nav.getBoundingClientRect().bottom,
      viewportHeight: window.innerHeight,
    }
  })

  // The whole block stays in the upper part of the rail: a bottom-pinned tools
  // group would put its container's bottom edge at the viewport edge.
  expect(layout.gapBelowModes).toBeLessThan(40)
  expect(layout.navBottom).toBeLessThan(layout.viewportHeight * 0.6)
})

test('a long contextual panel scrolls under the pinned primary navigation', async ({ page }) => {
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)
  const before = await openRoute(page, '?view=subs')

  const scrolled = await page.evaluate(() => {
    const nav = document.querySelector('.wt-primary-nav')
    if (!(nav instanceof HTMLElement)) throw new Error('the primary navigation is missing')
    let rail: HTMLElement | null = nav.parentElement
    while (rail && rail.scrollHeight <= rail.clientHeight) rail = rail.parentElement
    if (!rail) return { scrolledBy: 0 }
    rail.scrollTop = rail.scrollHeight
    return { scrolledBy: rail.scrollTop }
  })

  const after = await measurePrimaryNav(page)
  for (const [index, top] of after.tops.entries()) {
    expect(
      Math.abs(top - before.tops[index]),
      `scrolling the rail by ${scrolled.scrolledBy}px moved "${after.labels[index]}"`,
    ).toBeLessThanOrEqual(1)
  }
})
