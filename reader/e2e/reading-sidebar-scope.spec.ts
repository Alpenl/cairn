import { expect, test } from '@playwright/test'

const NAMESPACE = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

test.use({ viewport: { width: 1600, height: 900 } })

function headers(): Record<string, string> {
  return {
    'Content-Type': 'application/json',
    'X-WebTag-Data-Namespace': NAMESPACE,
  }
}

test('Reading sidebar requests only the reading aggregate partition', async ({ page }) => {
  const requests: string[] = []
  const pageErrors: string[] = []
  page.on('pageerror', (error) => pageErrors.push(error.message))
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    if (!url.pathname.startsWith('/api/')) {
      await route.continue()
      return
    }
    requests.push(`${request.method()} ${url.pathname}${url.search}`)

    if (url.pathname === '/api/session') {
      await route.fulfill({
        status: 200,
        headers: headers(),
        body: JSON.stringify({
          client_data_namespace: NAMESPACE,
          representation_contract: 'v3',
        }),
      })
      return
    }
    if (url.pathname === '/api/capabilities') {
      await route.fulfill({
        status: 200,
        headers: headers(),
        body: JSON.stringify({
          library_kinds: true,
          site_library: true,
          site_auto_classification: true,
          site_management: true,
          site_advanced_management: true,
          archive_versions: [1, 2],
          reader_vnext: true,
          reader: {
            annotations: false,
            notes: false,
            inbox: false,
            todos: false,
            engagement: false,
            home: false,
            feed: false,
            ai: false,
            semantic: false,
            activity: false,
            history: false,
          },
        }),
      })
      return
    }
    if (url.pathname === '/api/links') {
      await route.fulfill({
        status: 200,
        headers: headers(),
        body: JSON.stringify({
          items: [{
            id: 'reading-link',
            url: 'https://reading.example/article',
            title: 'Reading link',
            summary: 'Reading summary',
            description: null,
            tags: ['reading-tag'],
            content_type: 'article',
            status: 'done',
            domain: 'reading.example',
            path_depth: 1,
            parent_id: null,
            parent_path: null,
            created_at: '2026-08-10T00:00:00Z',
            updated_at: '2026-08-10T00:00:00Z',
            fetcher_type: 'http',
            is_low_confidence: false,
            low_confidence_reason: null,
            error_category: null,
            error_msg: null,
            metadata_revision: 1,
            has_content: false,
            library_kind: 'reading',
          }],
          total: 1,
          page: 1,
          limit: 30,
        }),
      })
      return
    }
    if (url.pathname === '/api/tags') {
      expect(url.searchParams.get('library_kind')).toBe('reading')
      await route.fulfill({
        status: 200,
        headers: headers(),
        body: JSON.stringify([
          { tag: 'reading-tag', count: 1, reading_count: 1, site_count: 0 },
        ]),
      })
      return
    }
    if (url.pathname === '/api/tree') {
      expect(url.searchParams.get('view')).toBe('domains')
      expect(url.searchParams.get('library_kind')).toBe('reading')
      await route.fulfill({
        status: 200,
        headers: headers(),
        body: JSON.stringify({
          library_kind: 'reading',
          domains: [{ domain: 'reading.example', count: 1 }],
          total: 1,
        }),
      })
      return
    }
    if (url.pathname === '/api/reader/activity') {
      await route.fulfill({ status: 200, headers: headers(), body: JSON.stringify({ tags: [], domains: [] }) })
      return
    }
    await route.fulfill({
      status: 404,
      headers: headers(),
      body: JSON.stringify({ error: { code: 404, message: 'not configured' } }),
    })
  })

  await page.addInitScript(() => {
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({ baseURL: window.location.origin, mode: 'installation-token', installationToken: '' }),
    )
  })
  await page.goto('/reader/?view=reading', { waitUntil: 'domcontentloaded' })

  await expect(page.locator('#root')).not.toBeEmpty()
  expect(pageErrors).toEqual([])

  const sidebar = page.locator('#primary-navigation')
  await expect(sidebar.getByText('reading-tag', { exact: true })).toBeVisible()
  await sidebar.getByText('域名', { exact: true }).click()
  await expect(sidebar.getByText('reading.example', { exact: true })).toBeVisible()
  await expect(sidebar.getByText('site-only', { exact: true })).toHaveCount(0)
  await expect(sidebar.getByText('site-only.example', { exact: true })).toHaveCount(0)

  expect(requests).toContain('GET /api/tags?library_kind=reading')
  expect(requests).toContain('GET /api/tree?view=domains&library_kind=reading')
})
