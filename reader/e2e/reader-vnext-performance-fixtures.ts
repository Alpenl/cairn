// Browser-side half of the Reader vNext scale baseline (issue #40 phase 0).
//
// The PostgreSQL half of the baseline measures what the server pays. This half
// measures what the *browser* pays for the same responses: transfer size,
// decode size, and how long each surface takes to become usable once the
// payload lands. Neither number is interesting without the other — a 10 MiB
// inbox response is a server problem and a client problem, and phase 1.1 has to
// move both.
//
// The route handlers below therefore reproduce the response *shape and size* of
// the PostgreSQL fixture, not its exact bytes. Row counts come from the tracked
// manifest, and the spec checks its probe list against the manifest's declared
// API paths, so a path cannot be dropped from one half of the baseline while
// the other half still claims to cover it.
//
// What is *not* linked: the page sizes and heavy-body constants below mirror
// the Go fixture by hand. They are repository defaults (30-row first screens,
// 1 MiB inbox bodies) rather than derived values, and nothing enforces the
// correspondence — if the server-side page size changes, this file has to be
// changed with it.
//
// Everything is served through page.route rather than e2e/server.mjs. That is
// the established pattern for a full Reader backend in this suite (see
// wp26-fixtures.ts) and it keeps a 10 MiB fixture out of the shared dev server
// that every other spec boots.
import { readFileSync } from 'node:fs'
import type { Page, Route } from '@playwright/test'

export const READER_SCALE_NAMESPACE = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

/** Frozen instant for every generated timestamp. Mirrors the Go fixture's base. */
const NOW = '2026-06-01T00:00:00Z'

/**
 * Heavy inbox bodies are 1 MiB and sit at the top of the first screen, exactly
 * as the PostgreSQL fixture orders them. This is the payload pathology the
 * baseline exists to record, so it is reproduced rather than approximated.
 */
const HEAVY_INBOX_ITEMS = 10
const HEAVY_INBOX_BODY = 1 << 20
const INBOX_BODY = 8 << 10
const INBOX_NOTE = 2 << 10

/** First-screen page sizes, matching the repository defaults the server uses. */
const INBOX_PAGE = 30
const TODOS_PAGE = 30
const FEED_PAGE = 30
const SEARCH_PAGE = 20
const ACTIVITY_PAGE = 100
const CONTINUE_READING = 3
const RECENT_THOUGHTS = 5

export interface PerformanceFixtureManifest {
  schema_version: number
  fixture_id: string
  seed: string
  search_token: string
  data_scale: Record<string, number>
  browser_journey?: {
    steps?: string[]
    api_paths?: string[]
  }
}

/**
 * Reads the tracked manifest named by READER_PERF_FIXTURE_MANIFEST. The spec
 * skips when the variable is absent, so this never runs in an ordinary suite.
 */
export function readFixtureManifest(): PerformanceFixtureManifest {
  const path = process.env.READER_PERF_FIXTURE_MANIFEST
  if (!path) throw new Error('READER_PERF_FIXTURE_MANIFEST is required')
  return JSON.parse(readFileSync(path, 'utf-8')) as PerformanceFixtureManifest
}

// ------------------------------------------------------------ text filler

/**
 * Builds a 1 KiB deterministic chunk from a label, then repeats it. Repetition
 * rather than per-character generation keeps a 1 MiB body cheap to build; the
 * payload's job is to have the right size and to be incompressible-ish, not to
 * be unique at every offset.
 */
function chunkFor(label: string): string {
  let hash = 2166136261
  const parts: string[] = []
  for (let index = 0; index < 128; index += 1) {
    hash ^= label.charCodeAt(index % label.length) + index
    hash = Math.imul(hash, 16777619) >>> 0
    parts.push(hash.toString(16).padStart(8, '0'))
  }
  return parts.join('')
}

const chunkCache = new Map<string, string>()

function scaleText(size: number, label: string): string {
  if (size <= 0) return ''
  let chunk = chunkCache.get(label)
  if (chunk === undefined) {
    chunk = chunkFor(label)
    chunkCache.set(label, chunk)
  }
  return chunk.repeat(Math.ceil(size / chunk.length)).slice(0, size)
}

// -------------------------------------------------------------- generators

function makeTodo(index: number, projected: boolean): Record<string, unknown> {
  return {
    id: `todo-${String(index).padStart(5, '0')}`,
    text: `${projected ? 'projected' : 'standalone'} task ${index} ${scaleText(40, `todo-${index}`)}`,
    due_at: index % 3 === 0 ? NOW : null,
    done: index % 4 === 0,
    origin_kind: projected ? 'note' : 'standalone',
    origin_host_kind: projected ? 'note' : null,
    origin_host_id: projected ? `note-${String(index % 2000).padStart(4, '0')}` : null,
    origin_ref: projected ? { block_ref: `task:${index.toString(16)}`, occurrence: 1 } : null,
    host_revision: projected ? 3 : 0,
    completed_at: index % 4 === 0 ? NOW : null,
    created_at: NOW,
    updated_at: NOW,
    expired: false,
  }
}

function makeThought(index: number, token: string): Record<string, unknown> {
  const hostID = `link-${String(index).padStart(5, '0')}`
  return {
    contract_version: 1,
    id: `thought-${String(index).padStart(5, '0')}`,
    host_kind: 'link',
    host_id: hostID,
    link_id: hostID,
    target: { kind: 'saved-content', host_id: hostID, version: { content_revision: 1 } },
    quote: {
      exact: scaleText(120, `thought-quote-${index}`),
      prefix: '',
      suffix: '',
      start: 0,
      end: 120,
    },
    body: `Thought ${index} ${scaleText(180, `thought-body-${index}`)} ${token}`,
    source: 'user',
    deleted: false,
    last_sequence: index + 1,
    winner_key: {
      logical_clock: index + 1,
      device_id: 'device-baseline',
      op_id: `op-${index}`,
    },
    created_at: NOW,
    updated_at: NOW,
  }
}

function makeInboxItem(index: number): Record<string, unknown> {
  const heavy = index < HEAVY_INBOX_ITEMS
  return {
    id: `inbox-${String(index).padStart(4, '0')}`,
    url: `https://inbox-${index % 250}.example/capture/${index}`,
    source_kind: 'browser_capture',
    title: `Inbox ${index}`,
    body: scaleText(heavy ? HEAVY_INBOX_BODY : INBOX_BODY, `inbox-body-${index}`),
    note: scaleText(INBOX_NOTE, `inbox-note-${index}`),
    summary: `Summary ${scaleText(200, `inbox-summary-${index}`)}`,
    suggested_tags: [`tag-${index % 200}`, 'suggested'],
    proposal_status: 'completed',
    tags: [`tag-${(index * 3) % 200}`],
    status: 'pending',
    metadata_revision: 1,
    expires_at: NOW,
    expired: false,
    created_at: NOW,
    updated_at: NOW,
  }
}

function makeFeedItem(index: number): Record<string, unknown> {
  const linkID = `link-${String(index).padStart(5, '0')}`
  return {
    key: `link:${linkID}`,
    source: 'reading',
    resource_key: `link:${linkID}`,
    title: `Feed item ${index}`,
    summary: `Summary ${scaleText(200, `feed-summary-${index}`)}`,
    url: `https://feed-${index % 50}.example/entry/${index}`,
    link_id: linkID,
    inbox_id: null,
    feed_item_id: null,
    read: index % 3 === 0,
    read_later: index % 37 === 0,
    saved: false,
    event_at: NOW,
  }
}

async function fulfill(route: Route, status: number, body: unknown): Promise<void> {
  await route.fulfill({
    status,
    contentType: 'application/json',
    headers: { 'X-WebTag-Data-Namespace': READER_SCALE_NAMESPACE, 'Cache-Control': 'no-store' },
    body: body === null ? '' : JSON.stringify(body),
  })
}

// ---------------------------------------------------------------- backend

/**
 * ReaderScaleBackend serves the six measured Reader endpoints at fixture scale
 * plus the supporting endpoints a surface needs to render. Anything else under
 * /api/ gets a well-formed 404 and is recorded in `unhandled`, so a missing stub
 * shows up in the baseline artifact instead of silently degrading the journey
 * into a measurement of an error screen.
 */
export class ReaderScaleBackend {
  readonly unhandled: string[] = []
  private readonly token: string
  private readonly counts: Record<string, number>

  private readonly todos: Record<string, unknown>[]
  private readonly inbox: Record<string, unknown>[]
  private readonly feed: Record<string, unknown>[]
  private readonly thoughts: Record<string, unknown>[]
  private readonly searchHits: Record<string, unknown>[]

  constructor(manifest: PerformanceFixtureManifest) {
    this.token = manifest.search_token
    this.counts = manifest.data_scale

    // Home returns the whole TODO list, not a page — that is the behaviour
    // being recorded. Standalone rows plus the projections Home materialises on
    // read gives the same list length the PostgreSQL fixture produces.
    const standalone = this.counts.standalone_todo_count ?? 0
    const projected = this.counts.todo_projection_source_count ?? 0
    this.todos = [
      ...Array.from({ length: projected }, (_, index) => makeTodo(index, true)),
      ...Array.from({ length: standalone }, (_, index) => makeTodo(projected + index, false)),
    ]
    this.inbox = Array.from({ length: INBOX_PAGE }, (_, index) => makeInboxItem(index))
    this.feed = Array.from({ length: FEED_PAGE }, (_, index) => makeFeedItem(index))
    this.thoughts = Array.from({ length: RECENT_THOUGHTS }, (_, index) => makeThought(index, this.token))
    this.searchHits = Array.from({ length: SEARCH_PAGE }, (_, index) => makeThought(index, this.token))
  }

  async install(page: Page): Promise<void> {
    await page.route('**/api/**', (route) => this.handle(route))
  }

  private async handle(route: Route): Promise<void> {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname

    // The `**/api/**` glob is broader than it looks: under the Vite dev server
    // the Reader's own source modules are served from /reader/src/lib/api/*,
    // and swallowing those means the application never boots and every surface
    // measurement times out against a blank page. Anything that is not a real
    // API request goes back to the dev server untouched.
    if (!path.startsWith('/api/')) {
      await route.continue()
      return
    }

    if (path === '/api/session') {
      await fulfill(route, 200, {
        client_data_namespace: READER_SCALE_NAMESPACE,
        representation_contract: 'v3',
      })
      return
    }
    if (path === '/api/capabilities') {
      await fulfill(route, 200, {
        library_kinds: true,
        site_library: true,
        site_management: false,
        site_advanced_management: false,
        archive_versions: [],
        reader_vnext: true,
        reader: {
          annotations: true,
          notes: true,
          inbox: true,
          todos: true,
          engagement: true,
          home: true,
          feed: true,
          ai: false,
          related_tags: false,
          activity: true,
          history: true,
          trash: true,
        },
      })
      return
    }

    if (path === '/api/home') {
      await fulfill(route, 200, {
        today: '2026-06-01',
        summary: 'reader vnext scale baseline',
        counts: {
          pending: this.counts.inbox_item_count ?? 0,
          inbox: this.counts.inbox_item_count ?? 0,
          inbox_expired: 0,
          reading: this.counts.link_count ?? 0,
          sites: this.counts.site_count ?? 0,
          subs: this.counts.feed_subscription_count ?? 0,
          notes: this.counts.note_count ?? 0,
          todos: this.todos.length,
          thoughts: this.counts.thought_count ?? 0,
          unread: this.counts.feed_item_count ?? 0,
        },
        continue_reading: this.feed.slice(0, CONTINUE_READING),
        recent_thoughts: this.thoughts,
        todos: this.todos,
        stale: false,
      })
      return
    }
    if (path === '/api/todos' && request.method() === 'GET') {
      await fulfill(route, 200, { items: this.todos.slice(0, TODOS_PAGE) })
      return
    }
    if (path === '/api/inbox' && request.method() === 'GET') {
      const expired = url.searchParams.get('partition') === 'expired'
      await fulfill(route, 200, {
        items: expired ? [] : this.inbox,
        active_count: this.counts.inbox_item_count ?? 0,
        expired_count: 0,
      })
      return
    }
    // The thought sync controller polls this on every surface. It is not one of
    // the six measured paths, so it answers with an empty, already-caught-up
    // page: leaving it unstubbed would make every surface pay a failed request
    // and pollute the journey timings with retry backoff.
    if (
      (path === '/api/annotations/sync' || path === '/api/annotations/conflicts') &&
      request.method() === 'GET'
    ) {
      await fulfill(route, 200, { contract_version: 1, items: [] })
      return
    }
    if (path === '/api/annotations' && request.method() === 'GET') {
      const query = (url.searchParams.get('q') ?? '').trim()
      await fulfill(route, 200, {
        contract_version: 1,
        items: query ? this.searchHits : this.thoughts,
      })
      return
    }
    if (path === '/api/reader-feed' && request.method() === 'GET') {
      const mode = url.searchParams.get('mode') === 'chronological' ? 'chronological' : 'recommended'
      await fulfill(route, 200, {
        items: this.feed,
        mode,
      })
      return
    }
    if (path === '/api/reader/activity' && request.method() === 'GET') {
      const half = ACTIVITY_PAGE / 2
      await fulfill(route, 200, {
        kind: 'all',
        tags: Array.from({ length: half }, (_, index) => ({ tag: `tag-${index}`, last_at: NOW })),
        domains: Array.from({ length: half }, (_, index) => ({ domain: `host-${index}.example`, last_at: NOW })),
      })
      return
    }

    // Supporting endpoints. They are deliberately small: the journey measures
    // the six hot paths, and padding the others would only add noise to the
    // recorded transfer totals.
    if (path === '/api/notes' && request.method() === 'GET') {
      await fulfill(route, 200, { items: [], count: this.counts.note_count ?? 0 })
      return
    }
    if (path === '/api/links') {
      await fulfill(route, 200, { items: [], total: this.counts.link_count ?? 0, page: 1, limit: 30 })
      return
    }
    if (path === '/api/subscriptions') {
      await fulfill(route, 200, { items: [], total: this.counts.feed_subscription_count ?? 0 })
      return
    }
    if (path === '/api/feed-folders') {
      await fulfill(route, 200, { items: [] })
      return
    }
    if (path === '/api/tags') {
      await fulfill(route, 200, [])
      return
    }
    if (path === '/api/tree') {
      await fulfill(route, 200, { nodes: [], total: 0 })
      return
    }
    if (path === '/api/sites') {
      await fulfill(route, 200, { items: [], total: this.counts.site_count ?? 0, page: 1, limit: 30 })
      return
    }

    this.unhandled.push(`${request.method()} ${path}`)
    await fulfill(route, 404, { error: { code: 404, message: 'not stubbed by the scale fixture' } })
  }
}

export async function configureScaleConnection(page: Page): Promise<void> {
  await page.addInitScript(() => {
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({ baseURL: window.location.origin, mode: 'installation-token', installationToken: '' }),
    )
  })
}
