import { readFileSync, writeFileSync } from 'node:fs'
import { expect, test } from '@playwright/test'

interface PerformanceFixtureManifest {
  fixture_id: string
  seed: string
  data_scale: Record<string, number>
}

function readFixtureManifest(): PerformanceFixtureManifest {
  const path = process.env.READER_PERF_FIXTURE_MANIFEST
  if (!path) throw new Error('READER_PERF_FIXTURE_MANIFEST is required')
  return JSON.parse(readFileSync(path, 'utf-8')) as PerformanceFixtureManifest
}

test.use({ trace: 'off' })

// ponytail: records one navigation timing and asserts nothing. Add p95 walls
// for Home / Inbox / Feed / Todos before treating this file as evidence.
test('Reader vNext named baseline journey', async ({ page }, testInfo) => {
  test.skip(
    !process.env.READER_PERF_FIXTURE_MANIFEST,
    'performance baseline requires the named harness inputs',
  )
  const fixture = readFixtureManifest()
  const tracePath = testInfo.outputPath('reader-vnext-performance-trace.zip')
  const metricsPath = testInfo.outputPath('reader-vnext-performance-metrics.json')
  const context = page.context()
  await context.tracing.start({ screenshots: true, snapshots: true })

  let metrics: Record<string, unknown> | undefined
  try {
    const journeyStartedAt = Date.now()
    await page.goto('/reader/?view=reading', { waitUntil: 'domcontentloaded' })
    await page.locator('body').waitFor()
    const readerNavigation = await page.evaluate(() => {
      const entry = performance.getEntriesByType('navigation')[0]
      if (!(entry instanceof PerformanceNavigationTiming)) {
        return { domContentLoadedMs: null, loadEventMs: null, transferSize: null }
      }
      return {
        domContentLoadedMs: entry.domContentLoadedEventEnd,
        loadEventMs: entry.loadEventEnd,
        transferSize: entry.transferSize,
      }
    })

    await page.goto('/__test__/blank', { waitUntil: 'domcontentloaded' })
    await expect(page.locator('body')).toContainText('browser test')
    const blankNavigation = await page.evaluate(() => {
      const entry = performance.getEntriesByType('navigation')[0]
      return entry instanceof PerformanceNavigationTiming
        ? { domContentLoadedMs: entry.domContentLoadedEventEnd, loadEventMs: entry.loadEventEnd }
        : { domContentLoadedMs: null, loadEventMs: null }
    })

    metrics = {
      schema_version: 1,
      fixture_id: fixture.fixture_id,
      seed: fixture.seed,
      data_scale: fixture.data_scale,
      journeyDurationMs: Date.now() - journeyStartedAt,
      readerNavigation,
      blankNavigation,
      browser: await page.evaluate(() => ({
        userAgent: navigator.userAgent,
        viewport: { width: window.innerWidth, height: window.innerHeight },
      })),
    }
    writeFileSync(metricsPath, `${JSON.stringify(metrics, null, 2)}\n`, 'utf-8')
  } finally {
    await context.tracing.stop({ path: tracePath })
  }

  expect(metrics).toBeDefined()
})
