import { expect, test, type Page } from '@playwright/test'

interface ControlState {
  identityWaiters: Partial<Record<'A' | 'B', number>>
  linkWaiters: Partial<Record<'A' | 'B', number>>
}

interface PersistedLink {
  namespace: string
  title: string
}

async function control<Result>(page: Page, path: string): Promise<Result> {
  return page.evaluate(async (endpoint) => {
    const response = await fetch(endpoint)
    if (!response.ok) throw new Error(`control request failed: ${response.status}`)
    return response.json()
  }, path) as Promise<Result>
}

async function triggerSync(page: Page): Promise<void> {
  await page.getByTitle('同步').click()
}

async function persistedLinks(page: Page): Promise<PersistedLink[]> {
  return page.evaluate(
    () =>
      new Promise<PersistedLink[]>((resolve) => {
        let database: IDBDatabase | null = null
        let settled = false
        const finish = (links: PersistedLink[]) => {
          if (settled) return
          settled = true
          database?.close()
          resolve(links)
        }
        const linksFromRows = (rows: unknown[]): PersistedLink[] =>
          rows.flatMap((row) => {
            if (!row || typeof row !== 'object') return []
            const record = row as {
              namespace?: unknown
              data?: { items?: Array<{ title?: unknown }> }
            }
            const title = record.data?.items?.[0]?.title
            return typeof record.namespace === 'string' && typeof title === 'string'
              ? [{ namespace: record.namespace, title }]
              : []
          })

        const open = indexedDB.open('webtag-reader-cache')
        open.onerror = () => finish([])
        open.onupgradeneeded = () => {
          open.transaction?.abort()
          finish([])
        }
        open.onsuccess = () => {
          database = open.result
          const hasPayloads = database.objectStoreNames.contains('cache_payload')
          const hasMetadata = database.objectStoreNames.contains('cache_meta')
          if (hasPayloads && hasMetadata) {
            const transaction = database.transaction(
              ['cache_payload', 'cache_meta'],
              'readonly',
            )
            const payloadRequest = transaction.objectStore('cache_payload').getAll()
            const metadataRequest = transaction.objectStore('cache_meta').getAll()
            let payloadRows: unknown[] = []
            let metadataRows: unknown[] = []
            payloadRequest.onsuccess = () => {
              payloadRows = payloadRequest.result as unknown[]
            }
            metadataRequest.onsuccess = () => {
              metadataRows = metadataRequest.result as unknown[]
            }
            transaction.onerror = () => finish([])
            transaction.onabort = () => finish([])
            transaction.oncomplete = () => {
              const payloadByKey = new Map<string, unknown>()
              for (const row of payloadRows) {
                if (!row || typeof row !== 'object') continue
                const payload = row as { key?: unknown; data?: unknown }
                if (typeof payload.key === 'string') {
                  payloadByKey.set(payload.key, payload.data)
                }
              }
              const joinedRows = metadataRows.flatMap((row) => {
                if (!row || typeof row !== 'object') return []
                const metadata = row as { key?: unknown; namespace?: unknown }
                if (typeof metadata.key !== 'string' || !payloadByKey.has(metadata.key)) {
                  return []
                }
                return [{
                  namespace: metadata.namespace,
                  data: payloadByKey.get(metadata.key),
                }]
              })
              finish(linksFromRows(joinedRows))
            }
            return
          }

          if (!database.objectStoreNames.contains('resources')) {
            finish([])
            return
          }
          const transaction = database.transaction('resources', 'readonly')
          const request = transaction.objectStore('resources').getAll()
          request.onerror = () => finish([])
          request.onsuccess = () => {
            finish(linksFromRows(request.result as unknown[]))
          }
        }
      }),
  )
}

test('persisted link inspection remains compatible with the legacy resources store', async ({
  page,
}) => {
  await page.goto('/__test__/blank')
  await page.evaluate(
    () =>
      new Promise<void>((resolve, reject) => {
        const open = indexedDB.open('webtag-reader-cache', 2)
        open.onupgradeneeded = () => {
          if (!open.result.objectStoreNames.contains('resources')) {
            open.result.createObjectStore('resources', { keyPath: 'key' })
          }
        }
        open.onerror = () => reject(open.error)
        open.onsuccess = () => {
          const database = open.result
          const transaction = database.transaction('resources', 'readwrite')
          transaction.objectStore('resources').put({
            key: 'legacy-link',
            namespace: 'physical-legacy',
            data: { items: [{ title: 'legacy cached link' }] },
          })
          transaction.onerror = () => reject(transaction.error)
          transaction.onabort = () => reject(transaction.error)
          transaction.oncomplete = () => {
            database.close()
            resolve()
          }
        }
      }),
  )

  await expect.poll(() => persistedLinks(page)).toEqual([
    { namespace: 'physical-legacy', title: 'legacy cached link' },
  ])
})

test('real IndexedDB fences hydrate, prefix delete, and a started transaction', async ({ page }) => {
  await page.goto('/__test__/storage-harness')
  await page.waitForFunction(() => Boolean(window.rf2bStorageHarness))

  const result = await page.evaluate(() => window.rf2bStorageHarness.run())

  expect(result.prefixDelete).toEqual({
    settledBeforeSwitch: false,
    records: [
      'physical-A:GET /api/links?owner=A',
      'physical-B:GET /api/links?owner=B',
    ],
  })
  expect(result.hydrate).toEqual({
    settledBeforeSwitch: false,
    restored: 0,
    activeNamespace: 'physical-B',
    hasProbe: false,
  })
  expect(result.transactionAbort).toEqual({
    switchedAfterStart: true,
    records: [],
  })
  expect(result.legacyImportRace).toEqual({
    firstImportStatus: 'imported',
    lateQuarantined: 1,
    secondPrompt: false,
    secondImportStatus: 'already-resolved',
    aPinsBeforeSwitch: '{"tags":["single-claim"],"domains":[]}',
    bPins: null,
  })
})

test('two pages concurrently claim one legacy import for exactly one identity', async ({
  context,
  page,
}) => {
  const pageB = await context.newPage()
  await Promise.all([
    page.goto('/__test__/storage-harness'),
    pageB.goto('/__test__/storage-harness'),
  ])
  await Promise.all([
    page.waitForFunction(() => Boolean(window.rf2bStorageHarness)),
    pageB.waitForFunction(() => Boolean(window.rf2bStorageHarness)),
  ])
  await page.evaluate(() => window.rf2bStorageHarness.prepareLegacyImport())

  const imports = await Promise.all([
    page.evaluate(() =>
      window.rf2bStorageHarness.importLegacyInto('server-A', 'physical-A'),
    ),
    pageB.evaluate(() =>
      window.rf2bStorageHarness.importLegacyInto('server-B', 'physical-B'),
    ),
  ])

  expect(imports.map((result) => result.status).sort()).toEqual([
    'already-resolved',
    'imported',
  ])
  expect(new Set(imports.map((result) => result.harnessInstanceID)).size).toBe(2)
  expect(imports.filter((result) => result.pins !== null)).toEqual([
    expect.objectContaining({
      status: 'imported',
      pins: '{"tags":["two-page-claim"],"domains":[]}',
    }),
  ])

  await Promise.all([
    page.evaluate(() => window.rf2bStorageHarness.closeLegacyDatabase()),
    pageB.evaluate(() => window.rf2bStorageHarness.closeLegacyDatabase()),
  ])
  await page.evaluate(() => window.rf2bStorageHarness.cleanupLegacyImport())
})

test('a tab that misses the broadcast converges from the durable generation on remount', async ({
  context,
  page,
}) => {
  const pageB = await context.newPage()
  await Promise.all([
    page.goto('/__test__/storage-harness'),
    pageB.goto('/__test__/storage-harness'),
  ])
  await Promise.all([
    page.waitForFunction(() => Boolean(window.rf2bStorageHarness)),
    pageB.waitForFunction(() => Boolean(window.rf2bStorageHarness)),
  ])

  const key = 'GET /api/links?rf4a=missed-broadcast'
  await page.evaluate(() => window.rf2bStorageHarness.resetRevalidationStorage())
  await Promise.all([
    page.evaluate(() => window.rf2bStorageHarness.installRevalidationIdentity('physical-shared')),
    pageB.evaluate(() => window.rf2bStorageHarness.installRevalidationIdentity('physical-shared')),
  ])
  await page.evaluate(
    ([logicalKey]) => {
      window.rf2bStorageHarness.startRevalidationPersistence()
      window.rf2bStorageHarness.setRevalidationValue(logicalKey, 'A cached')
    },
    [key],
  )
  await pageB.evaluate(
    ([logicalKey]) => window.rf2bStorageHarness.setRevalidationValue(logicalKey, 'B stale memory'),
    [key],
  )
  await page.waitForTimeout(30)

  await page.evaluate(() => window.rf2bStorageHarness.invalidateAndWait('GET /api/links'))
  expect(
    await pageB.evaluate(
      ([logicalKey]) => window.rf2bStorageHarness.revalidationSnapshot(logicalKey),
      [key],
    ),
  ).toEqual({ hasData: true, desiredGeneration: 0 })

  await pageB.evaluate(() => window.rf2bStorageHarness.startRevalidationPersistence())
  await pageB.evaluate(() => window.rf2bStorageHarness.waitForStorageQueue())
  expect(
    await pageB.evaluate(
      ([logicalKey]) => window.rf2bStorageHarness.revalidationSnapshot(logicalKey),
      [key],
    ),
  ).toEqual({ hasData: false, desiredGeneration: 1 })

  await Promise.all([
    page.evaluate(() => window.rf2bStorageHarness.stopRevalidationPersistence()),
    pageB.evaluate(() => window.rf2bStorageHarness.stopRevalidationPersistence()),
  ])
})

test('two stale tab projections cannot jointly admit more than the global cache quota', async ({
  context,
  page,
}) => {
  const pageB = await context.newPage()
  await Promise.all([
    page.goto('/__test__/storage-harness'),
    pageB.goto('/__test__/storage-harness'),
  ])
  await Promise.all([
    page.waitForFunction(() => Boolean(window.rf2bStorageHarness)),
    pageB.waitForFunction(() => Boolean(window.rf2bStorageHarness)),
  ])

  await page.evaluate(() => window.rf2bStorageHarness.resetRevalidationStorage())
  await Promise.all([
    page.evaluate(() => window.rf2bStorageHarness.installRevalidationIdentity('quota-A')),
    pageB.evaluate(() => window.rf2bStorageHarness.installRevalidationIdentity('quota-B')),
  ])
  await Promise.all([
    page.evaluate(() => window.rf2bStorageHarness.startRevalidationPersistence()),
    pageB.evaluate(() => window.rf2bStorageHarness.startRevalidationPersistence()),
  ])
  await Promise.all([
    page.evaluate(() => window.rf2bStorageHarness.waitForStorageQueue()),
    pageB.evaluate(() => window.rf2bStorageHarness.waitForStorageQueue()),
  ])
  expect((await page.evaluate(() => window.rf2bStorageHarness.cacheQuotaSnapshot())).knownBytes)
    .toBe(0)
  expect((await pageB.evaluate(() => window.rf2bStorageHarness.cacheQuotaSnapshot())).knownBytes)
    .toBe(0)

  const thirtyMiB = 30 * 1024 * 1024
  await Promise.all([
    page.evaluate(
      ([bytes]) => window.rf2bStorageHarness.setRevalidationPayloadBytes(
        'GET /api/links?quota=A',
        bytes,
      ),
      [thirtyMiB],
    ),
    pageB.evaluate(
      ([bytes]) => window.rf2bStorageHarness.setRevalidationPayloadBytes(
        'GET /api/links?quota=B',
        bytes,
      ),
      [thirtyMiB],
    ),
  ])
  await page.waitForTimeout(250)
  await Promise.all([
    page.evaluate(() => window.rf2bStorageHarness.waitForStorageQueue()),
    pageB.evaluate(() => window.rf2bStorageHarness.waitForStorageQueue()),
  ])

  const quota = await page.evaluate(() => window.rf2bStorageHarness.cacheQuotaSnapshot())
  expect(quota.durableBytes).toBeLessThanOrEqual(quota.maxBytes)
  expect(quota.records).toBe(1)
  await expect.poll(async () => {
    const snapshot = await page.evaluate(() => window.rf2bStorageHarness.cacheQuotaSnapshot())
    return snapshot.knownBytes === snapshot.durableBytes
  }).toBe(true)
  await expect.poll(async () => {
    const snapshot = await pageB.evaluate(() => window.rf2bStorageHarness.cacheQuotaSnapshot())
    return snapshot.knownBytes === snapshot.durableBytes
  }).toBe(true)

  await Promise.all([
    page.evaluate(() => window.rf2bStorageHarness.stopRevalidationPersistence()),
    pageB.evaluate(() => window.rf2bStorageHarness.stopRevalidationPersistence()),
  ])
})

test('two tabs recover from an origin-wide HttpOnly cookie replacement without cross-identity IO', async ({
  context,
  page,
}) => {
  await page.clock.install()
  await page.goto('/__test__/blank')
  await control(page, '/__test__/reset')
  await page.evaluate(() => {
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({ baseURL: window.location.origin, mode: 'session', installationToken: '' }),
    )
  })

  const pageB = await context.newPage()
  await Promise.all([page.goto('/reader/'), pageB.goto('/reader/')])
  await Promise.all([
    expect(page.getByText('A private v1', { exact: true }).first()).toBeVisible(),
    expect(pageB.getByText('A private v1', { exact: true }).first()).toBeVisible(),
  ])
  await expect.poll(async () => (await persistedLinks(page)).map((record) => record.title)).toContain(
    'A private v1',
  )

  // Drain both tabs' initial 500 ms persistence timers, then freeze subsequent batches.
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 1_000))
  await Promise.all([
    page.addScriptTag({ type: 'module', url: '/reader/e2e/storage-harness.ts' }),
    pageB.addScriptTag({ type: 'module', url: '/reader/e2e/storage-harness.ts' }),
  ])
  await Promise.all([
    page.waitForFunction(() => Boolean(window.rf2bStorageHarness)),
    pageB.waitForFunction(() => Boolean(window.rf2bStorageHarness)),
  ])
  await Promise.all([
    page.evaluate(() => window.rf2bStorageHarness.blockActiveCacheQueue()),
    pageB.evaluate(() => window.rf2bStorageHarness.blockActiveCacheQueue()),
  ])

  try {
    await control(pageB, '/__test__/payload?identity=A&title=A%20private%20late')
    await control(pageB, '/__test__/block-links?identity=A')
    await triggerSync(page)
    await expect.poll(async () => {
      const current = await control<ControlState>(pageB, '/__test__/state')
      return current.linkWaiters.A ?? 0
    }).toBe(1)
    await triggerSync(pageB)

    await control(pageB, '/__test__/block-identity?identity=B')
    await control(pageB, '/__test__/session?identity=B')
    const replacedCookie = (await context.cookies())
      .find((cookie) => cookie.name === 'webtag_session')
    expect(replacedCookie).toMatchObject({ value: 'B', httpOnly: true })
    expect(await page.evaluate(() => document.cookie)).not.toContain('webtag_session')

    await control(pageB, '/__test__/release-links?identity=A')
    await Promise.all([
      expect(page.getByText('A private late', { exact: true }).first()).toBeVisible(),
      expect(pageB.getByText('A private late', { exact: true }).first()).toBeVisible(),
    ])

    const beforeFlush = (await persistedLinks(page)).map((record) => record.title)
    expect(beforeFlush).toContain('A private v1')
    expect(beforeFlush).not.toContain('A private late')

    // Both 500 ms persistence callbacks now queue behind the two active A barriers.
    await page.clock.runFor(501)
    const whileBlocked = (await persistedLinks(page)).map((record) => record.title)
    expect(whileBlocked).toContain('A private v1')
    expect(whileBlocked).not.toContain('A private late')

    await Promise.all([triggerSync(page), triggerSync(pageB)])
    await Promise.all([
      page.evaluate(() => window.rf2bStorageHarness.waitForBlockedLeaseRevocation()),
      pageB.evaluate(() => window.rf2bStorageHarness.waitForBlockedLeaseRevocation()),
    ])
    await Promise.all([
      expect(page.locator('.sidebar')).toHaveCount(0),
      expect(pageB.locator('.sidebar')).toHaveCount(0),
    ])
    await expect.poll(async () => {
      const current = await control<ControlState>(pageB, '/__test__/state')
      return current.identityWaiters.B ?? 0
    }).toBeGreaterThanOrEqual(1)

    await Promise.all([
      page.evaluate(() => window.rf2bStorageHarness.releaseActiveCacheQueueAndDrain()),
      pageB.evaluate(() => window.rf2bStorageHarness.releaseActiveCacheQueueAndDrain()),
    ])
    const afterRevokedDrain = (await persistedLinks(page)).map((record) => record.title)
    expect(afterRevokedDrain).toContain('A private v1')
    expect(afterRevokedDrain).not.toContain('A private late')

    await page.clock.resume()
    await control(pageB, '/__test__/release-identity?identity=B')
    await Promise.all([
      expect(page.getByText('B private v1', { exact: true }).first()).toBeVisible(),
      expect(pageB.getByText('B private v1', { exact: true }).first()).toBeVisible(),
    ])
    await expect.poll(async () => (await persistedLinks(page)).map((record) => record.title))
      .toContain('B private v1')
    const partitioned = await persistedLinks(page)
    const namespaceA = partitioned.find((record) => record.title === 'A private v1')?.namespace
    const namespaceB = partitioned.find((record) => record.title === 'B private v1')?.namespace
    expect(namespaceA).toBeTruthy()
    expect(namespaceB).toBeTruthy()
    expect(namespaceA).not.toBe(namespaceB)

    await control(pageB, '/__test__/block-links-after-one?identity=A')
    await control(pageB, '/__test__/session?identity=A')
    await triggerSync(page)
    await expect(page.getByText('A private v1', { exact: true }).first()).toBeVisible()
    await expect(page.getByText('B private v1', { exact: true })).toHaveCount(0)
    await expect.poll(async () => {
      const current = await control<ControlState>(pageB, '/__test__/state')
      return current.linkWaiters.A ?? 0
    }).toBe(1)
    expect((await persistedLinks(page)).map((record) => record.title)).toContain('A private v1')

    await control(pageB, '/__test__/release-links?identity=A')
    await expect(page.getByText('A private late', { exact: true }).first()).toBeVisible()
  } finally {
    await Promise.allSettled([
      page.evaluate(() => window.rf2bStorageHarness.releaseActiveCacheQueueAndDrain()),
      pageB.evaluate(() => window.rf2bStorageHarness.releaseActiveCacheQueueAndDrain()),
      page.clock.resume(),
    ])
  }
})
