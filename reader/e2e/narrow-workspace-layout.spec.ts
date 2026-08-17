import { expect, test, type Locator, type Page } from '@playwright/test'
import { bootstrapReaderPage, configureReaderConnection, Wp26BackendFixture } from './wp26-fixtures'

const VIEWPORT = { width: 390, height: 844 }

/**
 * `.rvx-scroll` keeps `padding: 18px 13px 30px` below 680px. A content column
 * that collapses onto a stranded `flex-basis: 0` therefore still measures 48px,
 * which is the height this suite exists to reject.
 */
const COLLAPSED_SCROLLER_HEIGHT = 48

test.use({ viewport: VIEWPORT })

interface NarrowSurface {
  /** Route query the Reader coordinator resolves to this surface. */
  readonly query: string
  /**
   * A structural anchor rendered inside the shell scroller, below the header.
   * Selectors stay class-based on purpose: surface copy is renamed
   * independently of this layout contract.
   */
  readonly content: string
}

/**
 * Every vNext surface that renders through SurfaceShell. `?view=reading`,
 * `?view=subs` and `?view=sites` are deliberately absent: they render the
 * legacy `.rss-workspace` chrome instead.
 */
const SURFACES: ReadonlyArray<NarrowSurface> = [
  { query: '?surface=home', content: '.rvx-hero-row' },
  { query: '?surface=feed', content: 'li.rvx-feed-card' },
  { query: '?tool=settings', content: '.rvx-settings-list' },
  { query: '?tool=todo', content: '.rvx-todo-item' },
  { query: '?tool=history&thought_view=live', content: '.rvx-history-item' },
  { query: '?view=notes', content: '.rvx-list-column' },
  { query: '?view=pending', content: '.inbox-workbench' },
]

interface SurfaceGeometry {
  /** Height of the shell scroller, the element that owns the surface body. */
  readonly scrollHeight: number
  /** Border-box height of the sampled content element. */
  readonly contentHeight: number
  /** How much of that element survives clipping by every scrollable ancestor. */
  readonly visibleContentHeight: number
}

/**
 * Measure the surface the way a reader sees it: the content box is intersected
 * with the client rect of every clipping ancestor, so a collapsed scroller
 * reports its content as hidden even though the nodes are in the DOM.
 */
async function measureSurface(content: Locator): Promise<SurfaceGeometry> {
  return content.evaluate((element) => {
    const scroller = document.querySelector('.rvx-scroll')
    if (!(scroller instanceof HTMLElement)) throw new Error('the shell scroller is missing')
    const box = element.getBoundingClientRect()
    let top = box.top
    let bottom = box.bottom
    for (let node = element.parentElement; node; node = node.parentElement) {
      if (getComputedStyle(node).overflowY === 'visible') continue
      const clip = node.getBoundingClientRect()
      top = Math.max(top, clip.top)
      bottom = Math.min(bottom, clip.bottom)
    }
    return {
      scrollHeight: scroller.getBoundingClientRect().height,
      contentHeight: box.height,
      visibleContentHeight: Math.max(0, Math.min(bottom, window.innerHeight) - Math.max(top, 0)),
    }
  })
}

/** Open a surface by URL and wait for the shell, without reading any label. */
async function openSurface(page: Page, surface: NarrowSurface): Promise<Locator> {
  const backend = new Wp26BackendFixture()
  await backend.install(page)
  await configureReaderConnection(page)
  await bootstrapReaderPage(page, surface.query)
  await expect(page.locator('.rvx-main .rvx-header h1')).toBeVisible()
  expect(new URL(page.url()).search).toBe(surface.query)
  const content = page.locator(surface.content).first()
  await expect(content).toBeVisible()
  return content
}

for (const surface of SURFACES) {
  test(`Reader vNext ${surface.query} keeps its content column usable at ${VIEWPORT.width}px`, async ({ page }) => {
    const content = await openSurface(page, surface)
    await content.scrollIntoViewIfNeeded()
    const geometry = await measureSurface(content)

    // The scroller must own most of the viewport below the shell header rather
    // than collapse onto its own padding.
    expect(geometry.scrollHeight).toBeGreaterThan(VIEWPORT.height * 0.55)
    // The sampled element outgrows a collapsed scroller, so clipping shows up.
    expect(geometry.contentHeight).toBeGreaterThan(COLLAPSED_SCROLLER_HEIGHT)
    // And once scrolled to, it is genuinely on screen instead of clipped away.
    expect(geometry.visibleContentHeight).toBeGreaterThanOrEqual(
      Math.min(geometry.contentHeight, VIEWPORT.height * 0.3) * 0.95,
    )
  })
}

test('Reader vNext narrow shell keeps the nav pinned and scrolls inside the surface scroller', async ({ page }) => {
  await openSurface(page, SURFACES[0])

  const shell = await page.evaluate(() => {
    const scroller = document.querySelector('.rvx-scroll')
    const nav = document.querySelector('.rvx-nav')
    if (!(scroller instanceof HTMLElement) || !(nav instanceof HTMLElement)) {
      throw new Error('the narrow shell did not render its scroller and nav')
    }
    return {
      documentScrolls: document.documentElement.scrollHeight > window.innerHeight,
      navHeight: nav.getBoundingClientRect().height,
      scrollerTop: scroller.getBoundingClientRect().top,
      scrollerBottom: scroller.getBoundingClientRect().bottom,
      viewportHeight: window.innerHeight,
    }
  })

  // The page itself never scrolls: the shell scroller owns the overflow.
  expect(shell.documentScrolls).toBe(false)
  expect(shell.navHeight).toBeGreaterThan(0)
  expect(shell.scrollerTop).toBeGreaterThan(shell.navHeight)
  expect(shell.scrollerBottom).toBeLessThanOrEqual(shell.viewportHeight + 1)
})
