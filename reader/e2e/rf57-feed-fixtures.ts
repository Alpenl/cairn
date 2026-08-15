import type { Page, Route } from '@playwright/test'
import { Wp26BackendFixture, WP26_NAMESPACE } from './wp26-fixtures'

const NOW = '2026-08-10T08:00:00Z'
const PAGE_SIZE = 12

const READING_ACTIONS = ['read', 'read_later', 'save', 'unsave', 'open', 'open_workspace']

export type Rf57Mode = 'recommended' | 'chronological'
export type Rf57Source = 'inbox' | 'reading' | 'subscription'

interface Rf57FeedItem {
  readonly key: string
  readonly source: Rf57Source
  readonly item_type: Rf57Source
  readonly actions: readonly string[]
  readonly title: string
  readonly summary: string
  readonly url: string
  readonly link_id: string
  readonly inbox_id: null
  readonly feed_item_id: null
  readonly read: boolean
  readonly read_later: boolean
  readonly saved: boolean
  readonly score: number
  readonly score_contributions: {
    readonly pending_confirmation: number
    readonly saved_library: number
    readonly subscription_recent: number
    readonly unread: number
    readonly read_later: number
    readonly chronological_fallback: number
  }
  readonly enabled_score_signals: readonly string[]
  readonly reason_code: string
  readonly reason_params: Record<string, unknown>
  readonly reason_contribution: number
  readonly reason_text: string
  readonly published_at: null
  readonly event_at: string
  readonly created_at: string
}

function tuple(mode: Rf57Mode, filter: readonly Rf57Source[]): string {
  return `${mode}-${[...filter].sort().join('+') || 'none'}`
}

/** Link id, stable per (mode, filter, index) so a resumed snapshot keeps its keys. */
export function rf57LinkID(mode: Rf57Mode, filter: readonly Rf57Source[], index: number): string {
  return `rf57-${tuple(mode, filter)}-${index}`
}

/** The value the surface puts in `data-feed-item-key` for that card. */
export function rf57CardKey(mode: Rf57Mode, filter: readonly Rf57Source[], index: number): string {
  return `reading:${rf57LinkID(mode, filter, index)}`
}

export function rf57ItemTitle(mode: Rf57Mode, filter: readonly Rf57Source[], index: number): string {
  return `RF57 ${tuple(mode, filter)} 第 ${index} 条`
}

function itemFor(mode: Rf57Mode, filter: readonly Rf57Source[], index: number, tall: boolean): Rf57FeedItem {
  const id = rf57LinkID(mode, filter, index)
  return {
    key: `reading:${id}`,
    source: 'reading',
    item_type: 'reading',
    actions: READING_ACTIONS,
    title: rf57ItemTitle(mode, filter, index),
    summary: tall
      ? `第 ${index} 条内容的摘要。${'这条摘要后来变长了，把它下面的所有卡片一起推下去。'.repeat(12)}`
      : `第 ${index} 条内容的摘要，用来把卡片撑到可以滚动的高度。`,
    url: `https://rf57.example.test/${id}`,
    link_id: id,
    inbox_id: null,
    feed_item_id: null,
    read: false,
    read_later: false,
    saved: false,
    score: 20,
    score_contributions: {
      pending_confirmation: 0,
      saved_library: 0,
      subscription_recent: 0,
      unread: 20,
      read_later: 0,
      chronological_fallback: 0,
    },
    enabled_score_signals: ['unread'],
    reason_code: 'unread',
    reason_params: { read: false },
    reason_contribution: 20,
    reason_text: '来自最近的阅读记录',
    published_at: null,
    event_at: NOW,
    created_at: NOW,
  }
}

function normalizeSources(values: readonly string[]): Rf57Source[] {
  const selected = new Set<string>()
  for (const value of values) {
    for (const part of value.split(',')) selected.add(part.trim().toLowerCase())
  }
  const order: Rf57Source[] = ['inbox', 'reading', 'subscription']
  const filtered = order.filter((source) => selected.has(source))
  return filtered.length > 0 ? filtered : order
}

function snapshotID(mode: Rf57Mode, filter: readonly Rf57Source[]): string {
  return `rf57-snapshot-${tuple(mode, filter)}`
}

function cursorFor(mode: Rf57Mode, filter: readonly Rf57Source[]): string {
  return `rf57-cursor-${tuple(mode, filter)}`
}

/**
 * Two pages of Feed cards per (mode, source filter) pair, plus links for the
 * cards so opening a card and coming back exercises the real detail roundtrip.
 */
export class Rf57BackendFixture extends Wp26BackendFixture {
  readonly feedRequests: string[] = []
  /** Cards the server has stopped returning, by index. */
  readonly droppedIndexes = new Set<number>()
  /** Cards whose summary grew, pushing everything below them down. */
  readonly grownIndexes = new Set<number>()
  /** Retires the persisted snapshot once, the way retention expiry does. */
  expireSnapshotOnce = false
  private snapshotGeneration = 1

  override async handle(route: Route): Promise<void> {
    const request = route.request()
    const url = new URL(request.url())

    if (url.pathname === '/api/reader-feed' && request.method() === 'GET') {
      this.feedRequests.push(`${url.pathname}${url.search}`)
      const mode: Rf57Mode = url.searchParams.get('mode') === 'chronological' ? 'chronological' : 'recommended'
      const filter = normalizeSources(url.searchParams.getAll('source'))
      if (this.expireSnapshotOnce && url.searchParams.get('snapshot_id') !== null) {
        this.expireSnapshotOnce = false
        this.snapshotGeneration += 1
        await route.fulfill({
          status: 410,
          headers: { 'Content-Type': 'application/json', 'X-WebTag-Data-Namespace': WP26_NAMESPACE },
          body: JSON.stringify({ error: { code: 410, message: 'snapshot expired' } }),
        })
        return
      }
      const secondPage = url.searchParams.get('after') !== null
      const start = secondPage ? PAGE_SIZE : 0
      const items = Array.from({ length: PAGE_SIZE }, (_, offset) => start + offset)
        .filter((index) => !this.droppedIndexes.has(index))
        .map((index) => itemFor(mode, filter, index, this.grownIndexes.has(index)))
      await this.fulfillJSON(route, {
        items,
        snapshot_id: `${snapshotID(mode, filter)}-g${this.snapshotGeneration}`,
        mode,
        ...(secondPage ? {} : { next_cursor: cursorFor(mode, filter) }),
        capabilities: ['snapshot', 'cursor', 'dedupe', 'reason', 'source_filter', 'actions'],
        sources: [
          { id: 'inbox', label: '收件箱', enabled: filter.includes('inbox'), count: 0, capabilities: [] },
          { id: 'reading', label: '收藏', enabled: filter.includes('reading'), count: PAGE_SIZE, capabilities: ['read'] },
          { id: 'subscription', label: '订阅', enabled: filter.includes('subscription'), count: PAGE_SIZE, capabilities: ['read'] },
        ],
      })
      return
    }

    const linkID = url.pathname.startsWith('/api/links/')
      ? decodeURIComponent(url.pathname.slice('/api/links/'.length).split('/')[0])
      : null
    if (linkID?.startsWith('rf57-') && request.method() === 'GET') {
      await this.fulfillJSON(route, url.pathname.endsWith('/content')
        ? { link_id: linkID, content: '', content_format: 'plain', content_source: 'fetched', content_revision: 1 }
        : linkResponse(linkID))
      return
    }

    await super.handle(route)
  }

  private async fulfillJSON(route: Route, body: unknown): Promise<void> {
    await route.fulfill({
      status: 200,
      headers: {
        'Content-Type': 'application/json',
        'Cache-Control': 'no-store',
        'X-WebTag-Data-Namespace': WP26_NAMESPACE,
      },
      body: JSON.stringify(body),
    })
  }
}

function linkResponse(id: string): Record<string, unknown> {
  return {
    id,
    url: `https://rf57.example.test/${id}`,
    title: `RF57 详情 ${id}`,
    summary: 'RF57 详情摘要',
    description: null,
    tags: [],
    content_type: 'article',
    status: 'done',
    domain: 'rf57.example.test',
    path_depth: 1,
    parent_id: null,
    parent_path: null,
    created_at: NOW,
    updated_at: NOW,
    fetcher_type: 'stored',
    is_low_confidence: false,
    low_confidence_reason: null,
    error_category: null,
    error_msg: null,
    metadata_revision: 1,
    has_content: false,
  }
}

export interface FeedViewport {
  readonly scrollTop: number
  /** Anchored card's top relative to the scroller's own top, in CSS pixels. */
  readonly anchorOffset: number | null
  readonly firstVisibleKey: string | null
}

/** Stored Feed position keys, with the identity namespace stripped off. */
export async function readFeedAnchorKeys(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const keys: string[] = []
    for (let index = 0; index < sessionStorage.length; index += 1) {
      const key = sessionStorage.key(index) as string
      if (!key.endsWith(':scroll')) continue
      // webtag:reader:mixed-feed:v1:<namespace>:<mode>:<filter>:scroll
      keys.push(key.split(':').slice(5).join(':'))
    }
    return keys.sort()
  })
}

/** Reads the live geometry the surface is supposed to be restoring. */
export async function readFeedViewport(page: Page, anchorKey: string | null): Promise<FeedViewport> {
  return page.evaluate((key) => {
    const scroller = document.querySelector('.rvx-scroll')
    if (!(scroller instanceof HTMLElement)) throw new Error('feed scroller is not rendered')
    const viewportTop = scroller.getBoundingClientRect().top
    const cards = [...scroller.querySelectorAll<HTMLElement>('[data-feed-item-key]')]
    const visible = cards.filter((card) => card.getBoundingClientRect().bottom > viewportTop)
    const anchored = key === null ? null : cards.find((card) => card.getAttribute('data-feed-item-key') === key)
    return {
      scrollTop: scroller.scrollTop,
      anchorOffset: anchored ? anchored.getBoundingClientRect().top - viewportTop : null,
      firstVisibleKey: visible[0]?.getAttribute('data-feed-item-key') ?? null,
    }
  }, anchorKey)
}

/**
 * Toggles a source switch without moving the scroller.
 *
 * The switches live inside the Feed scroller, and both Playwright's click and
 * the browser's focus handling scroll their target into view first — which
 * would rewrite the very position this suite is asserting about. Dispatching
 * the click keeps the real React handler in the loop without that side effect.
 */
export async function toggleFeedSource(page: Page, label: string): Promise<void> {
  await page.evaluate((name) => {
    const control = [...document.querySelectorAll('button[role="switch"]')]
      .find((candidate) => candidate.getAttribute('aria-label') === name)
    if (!(control instanceof HTMLElement)) throw new Error(`source switch ${name} is not rendered`)
    control.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
  }, label)
}

/** Scrolls so the given card's top sits `offset` px below the viewport top. */
export async function scrollFeedToCard(page: Page, itemKey: string, offset: number): Promise<void> {
  await page.evaluate(({ key, targetOffset }) => {
    const scroller = document.querySelector('.rvx-scroll')
    if (!(scroller instanceof HTMLElement)) throw new Error('feed scroller is not rendered')
    const card = scroller.querySelector(`[data-feed-item-key="${CSS.escape(key)}"]`)
    if (!(card instanceof HTMLElement)) throw new Error(`feed card ${key} is not rendered`)
    const viewportTop = scroller.getBoundingClientRect().top
    scroller.scrollTop = scroller.scrollTop + (card.getBoundingClientRect().top - viewportTop) - targetOffset
  }, { key: itemKey, targetOffset: offset })
}
