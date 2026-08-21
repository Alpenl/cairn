import { createHash } from 'node:crypto'
import { expect, test, type Page } from '@playwright/test'

const NAMESPACE = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

type ArchiveSelection = 'base' | 'base,thoughts' | 'base,notes' | 'base,thoughts,notes'
type ArchiveMode =
  | { kind: 'success'; archive?: Buffer }
  | { kind: 'error'; status: 422 | 503; code: string; message: string }
  | { kind: 'invalid-body' }

type ReaderArchiveRows = Partial<Record<string, readonly Record<string, unknown>[]>>

const readerBaseSections = [
  'feed_folders',
  'feed_subscriptions',
  'feed_items',
  'feed_saves',
  'inbox',
  'todos',
  'engagement',
  'feed_hides',
]

function headers(extra: Record<string, string> = {}): Record<string, string> {
  return {
    'Content-Type': 'application/json',
    'X-WebTag-Data-Namespace': NAMESPACE,
    ...extra,
  }
}

function archiveSections(selection: ArchiveSelection): string[] {
  const sections = ['base']
  if (selection.includes('thoughts')) sections.push('thoughts')
  if (selection.includes('notes')) sections.push('notes')
  return sections
}

function selectedReaderSections(selection: ArchiveSelection): string[] {
  const sections = [...readerBaseSections]
  if (selection.includes('thoughts')) {
    sections.push('thoughts', 'thought_ops', 'thought_supersession_events', 'thought_tombstones')
  }
  if (selection.includes('notes')) sections.push('notes', 'note_history')
  return sections
}

function archivePayload(selection: ArchiveSelection, rows: ReaderArchiveRows = {}): Buffer {
  const reader: Record<string, unknown> = {
    schema_version: 2,
    thought_contract_version: 1,
  }
  for (const section of selectedReaderSections(selection)) reader[section] = rows[section] ?? []
  const archive = {
    schema_version: 2,
    exported_at: '2026-08-11T00:00:00Z',
    generator_version: 'webtag',
    links: [],
    sites: [],
    site_entries: [],
    site_tags: [],
    site_identities: [],
    reader,
  }
  const prefix = JSON.stringify(archive).slice(0, -1)
  const counts: Record<string, number> = {
    links: 0,
    sites: 0,
    site_entries: 0,
    site_tags: 0,
    site_identities: 0,
  }
  for (const section of selectedReaderSections(selection)) {
    const sectionRows = reader[section]
    counts[`reader.${section}`] = Array.isArray(sectionRows) ? sectionRows.length : 0
  }
  const manifest = {
    client_data_namespace: NAMESPACE,
    sections: archiveSections(selection),
    counts,
    checksum_sha256: createHash('sha256').update(prefix).digest('hex'),
  }
  return Buffer.from(`${prefix},"manifest":${JSON.stringify(manifest)}}`, 'utf8')
}

async function installArchiveApp(page: Page, getMode: () => ArchiveMode, requests: string[]): Promise<void> {
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    if (!url.pathname.startsWith('/api/')) {
      await route.continue()
      return
    }
    if (url.pathname === '/api/export/v2') {
      const selector = url.searchParams.get('sections')
      requests.push(`${request.method()} ${url.pathname}?sections=${selector}`)
      const mode = getMode()
      if (mode.kind === 'error') {
        await route.fulfill({
          status: mode.status,
          headers: headers(),
          body: JSON.stringify({ error: { code: mode.status, error_code: mode.code, message: mode.message } }),
        })
        return
      }
      if (mode.kind === 'invalid-body') {
        await route.fulfill({ status: 200, headers: headers(), body: '{"schema_version":2}' })
        return
      }
      if (selector === null || !isArchiveSelection(selector)) {
        throw new Error(`unexpected archive selector ${selector}`)
      }
      await route.fulfill({
        status: 200,
        headers: headers({ 'Content-Disposition': 'attachment; filename="webtag-archive-v2.json"' }),
        body: mode.archive ?? archivePayload(selector),
      })
      return
    }
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
          site_management: true,
          archive_versions: [2],
          reader_vnext: false,
          reader: {
            annotations: false,
            notes: false,
            inbox: false,
            todos: false,
            engagement: false,
            home: false,
            feed: false,
            ai: false,
            related_tags: false,
            activity: false,
            history: false,
            trash: false,
          },
        }),
      })
      return
    }
    if (url.pathname === '/api/links') {
      await route.fulfill({
        status: 200,
        headers: headers(),
        body: JSON.stringify({ items: [], total: 0, page: 1, limit: 30 }),
      })
      return
    }
    if (url.pathname === '/api/tags') {
      await route.fulfill({ status: 200, headers: headers(), body: '[]' })
      return
    }
    if (url.pathname === '/api/tree') {
      await route.fulfill({
        status: 200,
        headers: headers(),
        body: JSON.stringify({ library_kind: 'reading', domains: [], total: 0 }),
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
  await expect(page.getByRole('button', { name: '下载归档' })).toBeVisible({ timeout: 25_000 })
}

function isArchiveSelection(value: string): value is ArchiveSelection {
  return value === 'base' || value === 'base,thoughts' || value === 'base,notes' || value === 'base,thoughts,notes'
}

async function openArchiveDialog(page: Page): Promise<void> {
  await page.getByRole('button', { name: '下载归档' }).click()
  await expect(page.getByRole('dialog', { name: '下载归档' })).toBeVisible()
}

async function chooseArchiveSelection(page: Page, selection: ArchiveSelection): Promise<void> {
  const dialog = page.getByRole('dialog', { name: '下载归档' })
  const thoughts = dialog.getByRole('checkbox', { name: '想法' })
  const notes = dialog.getByRole('checkbox', { name: '笔记' })
  const includeThoughts = selection.includes('thoughts')
  const includeNotes = selection.includes('notes')
  if (await thoughts.isChecked() !== includeThoughts) await thoughts.click()
  if (await notes.isChecked() !== includeNotes) await notes.click()
  await expect(dialog.getByRole('button', { name: '下载' })).toHaveAttribute('data-archive-sections', selection)
}

async function downloadText(page: Page): Promise<string> {
  const downloadPromise = page.waitForEvent('download')
  await page.getByRole('dialog', { name: '下载归档' }).getByRole('button', { name: '下载' }).click()
  const download = await downloadPromise
  const stream = await download.createReadStream()
  if (!stream) throw new Error('archive download stream was unavailable')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return Buffer.concat(chunks).toString('utf8')
}

test('v2 archive selector combinations create verified browser downloads', async ({ page }) => {
  const mode: ArchiveMode = { kind: 'success' }
  const requests: string[] = []
  await installArchiveApp(page, () => mode, requests)

  for (const selection of ['base', 'base,thoughts', 'base,notes', 'base,thoughts,notes'] as const) {
    await openArchiveDialog(page)
    await chooseArchiveSelection(page, selection)
    const text = await downloadText(page)
    const archive = JSON.parse(text) as { manifest: { sections: string[]; checksum_sha256: string } }
    expect(archive.manifest.sections).toEqual(archiveSections(selection))
    expect(archive.manifest.checksum_sha256).toMatch(/^[a-f0-9]{64}$/)
    await expect(page.getByText('归档已下载')).toBeVisible()
  }
  expect(requests).toEqual([
    'GET /api/export/v2?sections=base',
    'GET /api/export/v2?sections=base,thoughts',
    'GET /api/export/v2?sections=base,notes',
    'GET /api/export/v2?sections=base,thoughts,notes',
  ])
})

test('v2 archive downloads server-valid Thought IDs, source text, and delete hosts', async ({ page }) => {
  const annotationID = 'a'.repeat(129)
  const deletedHostID = 'purged-inbox:legacy-42'
  const mode: ArchiveMode = {
    kind: 'success',
    archive: archivePayload('base,thoughts', {
      thoughts: [
        {
          contract_version: 1,
          id: annotationID,
          host_kind: 'inbox',
          host_id: '66666666-6666-4666-8666-666666666666',
          link_id: null,
          target: {
            kind: 'inbox',
            host_id: '66666666-6666-4666-8666-666666666666',
            version: { metadata_revision: 1 },
          },
          quote: { exact: 'selected text' },
          body: 'Imported thought',
          source: 'reader-v0-import',
          deleted: false,
          last_sequence: 1,
          winner_key: { logical_clock: 1, device_id: 'device-1', op_id: 'op-1' },
          created_at: '2026-08-11T00:00:00Z',
          updated_at: '2026-08-11T00:00:00Z',
        },
        {
          contract_version: 1,
          id: 'deleted-thought',
          host_kind: 'inbox',
          host_id: deletedHostID,
          link_id: null,
          target: {
            kind: 'inbox',
            host_id: deletedHostID,
            version: { metadata_revision: 1 },
          },
          quote: null,
          body: '',
          source: 'user',
          deleted: true,
          last_sequence: 2,
          winner_key: { logical_clock: 2, device_id: 'device-1', op_id: 'op-2' },
          created_at: '2026-08-11T00:00:00Z',
          updated_at: '2026-08-11T00:00:00Z',
        },
      ],
      thought_ops: [
        {
          contract_version: 1,
          sequence: 1,
          op_id: 'op-1',
          device_id: 'device-1',
          logical_clock: 1,
          operation_kind: 'add',
          annotation_id: annotationID,
          host_kind: 'inbox',
          host_id: '66666666-6666-4666-8666-666666666666',
          target: {
            kind: 'inbox',
            host_id: '66666666-6666-4666-8666-666666666666',
            version: { metadata_revision: 1 },
          },
          payload: { body: 'Imported thought', quote: { exact: 'selected text' }, source: 'reader-v0-import' },
          recovery_of: null,
          expected_current_winner_key: null,
          created_at: '2026-08-11T00:00:00Z',
        },
        {
          contract_version: 1,
          sequence: 2,
          op_id: 'op-2',
          device_id: 'device-1',
          logical_clock: 2,
          operation_kind: 'delete',
          annotation_id: 'deleted-thought',
          host_kind: 'inbox',
          host_id: deletedHostID,
          target: {
            kind: 'inbox',
            host_id: deletedHostID,
            version: { metadata_revision: 1 },
          },
          payload: {},
          recovery_of: null,
          expected_current_winner_key: null,
          created_at: '2026-08-11T00:00:00Z',
        },
      ],
      thought_tombstones: [
        {
          thought_id: 'deleted-thought',
          host_kind: 'inbox',
          host_id: deletedHostID,
          reason: 'inbox_purged',
          snapshot: {},
          created_at: '2026-08-11T00:00:00Z',
        },
      ],
      thought_supersession_events: [
        {
          sequence: 7,
          annotation_id: annotationID,
          loser: { body: 'Superseded thought' },
          winner_at_detection: { body: 'Imported thought' },
          created_at: '2026-08-11T00:00:00Z',
        },
      ],
    }),
  }
  const requests: string[] = []
  await installArchiveApp(page, () => mode, requests)

  await openArchiveDialog(page)
  await chooseArchiveSelection(page, 'base,thoughts')
  const text = await downloadText(page)
  const archive = JSON.parse(text) as {
    reader: {
      thoughts: Array<{ id: string; source: string }>
      thought_ops: Array<{ annotation_id: string; host_id: string }>
      thought_supersession_events: Array<{ annotation_id: string; sequence: number }>
    }
  }
  expect(archive.reader.thoughts[0]).toMatchObject({ id: annotationID, source: 'reader-v0-import' })
  expect(archive.reader.thought_ops[0]?.annotation_id).toBe(annotationID)
  expect(archive.reader.thought_ops[1]?.host_id).toBe(deletedHostID)
  expect(archive.reader.thought_supersession_events[0]).toMatchObject({
    annotation_id: annotationID,
    sequence: 7,
  })
  await expect(page.getByText('归档已下载')).toBeVisible()
  expect(requests).toEqual(['GET /api/export/v2?sections=base,thoughts'])
})

test('v2 archive typed failures stay retryable and never trigger a browser download', async ({ page }) => {
  let mode: ArchiveMode = { kind: 'success' }
  const requests: string[] = []
  const downloads: string[] = []
  page.on('download', (download) => downloads.push(download.suggestedFilename()))
  await installArchiveApp(page, () => mode, requests)

  const failures: Array<{ mode: ArchiveMode; expected: string }> = [
    {
      mode: { kind: 'error', status: 422, code: 'invalid_archive_sections', message: 'selector rejected' },
      expected: '归档下载失败：selector rejected',
    },
    {
      mode: { kind: 'error', status: 503, code: 'archive_reader_unavailable', message: 'reader unavailable' },
      expected: '归档下载失败：reader unavailable',
    },
    {
      mode: { kind: 'invalid-body' },
      expected: '归档下载失败：归档必须只有一个最终 manifest 顶层字段',
    },
  ]

  for (const failure of failures) {
    mode = failure.mode
    await openArchiveDialog(page)
    await chooseArchiveSelection(page, 'base,thoughts,notes')
    await page.getByRole('dialog', { name: '下载归档' }).getByRole('button', { name: '下载' }).click()
    await expect(page.getByText(failure.expected)).toBeVisible()
    await expect(page.getByRole('dialog', { name: '下载归档' })).toBeVisible()
    expect(downloads).toEqual([])
    await page.getByRole('dialog', { name: '下载归档' }).getByRole('button', { name: '取消' }).click()
  }
  expect(requests).toEqual([
    'GET /api/export/v2?sections=base,thoughts,notes',
    'GET /api/export/v2?sections=base,thoughts,notes',
    'GET /api/export/v2?sections=base,thoughts,notes',
  ])
})
