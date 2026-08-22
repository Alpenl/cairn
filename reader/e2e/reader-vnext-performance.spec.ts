import { writeFileSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import {
  ReaderScaleBackend,
  configureScaleConnection,
  readFixtureManifest,
} from './reader-vnext-performance-fixtures'

// Reader vNext browser baseline (issue #40 phase 0).
//
// The previous version of this spec recorded one navigation timing entry
// against an empty backend. That number could not move when a Reader surface
// got slower, which made it a baseline in name only. This version drives the
// six hot paths against the scale fixture and records two different things,
// because they answer two different questions:
//
//   - Surface journeys: how long a cold load of Home / Todos / Inbox / Feed
//     takes to fetch its data and paint. This is the number a user feels.
//   - API timings: transfer size, decoded size, and JSON parse cost per
//     endpoint. This is the number phase 1.1 has to move, and it is measured
//     independently of rendering so a render change cannot mask a payload
//     change.
//
// Thought search and Activity have no URL-addressable surface of their own, so
// they appear in the API pass only. Saying that here is better than quietly
// dropping two of the six paths.
//
// No assertion in this file is a latency threshold. Playwright runs on shared
// hardware; a millisecond bound here would fail for reasons unrelated to the
// diff under review. The spec asserts that every measured path answered, and
// writes the numbers to the artifact for comparison against a recorded run.

const SURFACE_WARMUP = 1
const API_WARMUP = 3
const API_SAMPLES = 10

interface SurfaceStep {
  readonly name: string
  readonly query: string
  readonly apiPath: string
}

const SURFACE_STEPS: readonly SurfaceStep[] = [
  { name: 'home', query: '?surface=home', apiPath: '/api/home' },
  { name: 'todos', query: '?tool=todo', apiPath: '/api/todos' },
  { name: 'inbox', query: '?view=pending', apiPath: '/api/inbox' },
  { name: 'recommended_feed', query: '?surface=feed', apiPath: '/api/reader-feed' },
]

interface ApiProbe {
  readonly name: string
  readonly path: string
}

const API_PROBES: readonly ApiProbe[] = [
  { name: 'home', path: '/api/home' },
  { name: 'todos', path: '/api/todos?limit=30' },
  { name: 'inbox_first_screen', path: '/api/inbox?partition=active&limit=30' },
  { name: 'thought_search', path: '/api/annotations?q=__TOKEN__&limit=20' },
  { name: 'recommended_feed', path: '/api/reader-feed?mode=recommended&limit=30' },
  { name: 'activity', path: '/api/reader/activity?kind=all&limit=100' },
]

interface ApiSampleResult {
  name: string
  path: string
  status: number
  samples: number
  totalP50Ms: number
  totalP95Ms: number
  parseP50Ms: number
  transferSize: number | null
  encodedBodySize: number | null
  decodedBodySize: number | null
  responseChars: number
}

test.use({ trace: 'off' })

test('Reader vNext scale baseline journey', async ({ page }, testInfo) => {
  test.skip(
    !process.env.READER_PERF_FIXTURE_MANIFEST,
    'performance baseline requires the named harness inputs',
  )
  test.setTimeout(300_000)

  const fixture = readFixtureManifest()

  // The manifest declares which API paths the browser half of the baseline is
  // required to cover. Checking it here is what stops the two halves drifting:
  // a path added to the PostgreSQL baseline and forgotten here would otherwise
  // leave this spec passing while measuring five of six paths.
  const declaredPaths = fixture.browser_journey?.api_paths ?? []
  const probedPaths = new Set(API_PROBES.map((probe) => probe.path.split('?')[0]))
  for (const declared of declaredPaths) {
    expect(probedPaths, `manifest declares ${declared} as a measured path`).toContain(declared)
  }

  const backend = new ReaderScaleBackend(fixture)
  await backend.install(page)
  await configureScaleConnection(page)

  const tracePath = testInfo.outputPath('reader-vnext-performance-trace.zip')
  const metricsPath = testInfo.outputPath('reader-vnext-performance-metrics.json')
  const context = page.context()
  await context.tracing.start({ screenshots: true, snapshots: true })

  try {
    const journeyStartedAt = Date.now()

    // ------------------------------------------------- surface journeys
    const surfaces: Record<string, unknown>[] = []
    for (const step of SURFACE_STEPS) {
      for (let warm = 0; warm < SURFACE_WARMUP; warm += 1) {
        const warmPage = await context.newPage()
        try {
          await backend.install(warmPage)
          await configureScaleConnection(warmPage)
          await warmPage.goto(`/reader/${step.query}`, { waitUntil: 'domcontentloaded' })
        } finally {
          await warmPage.close()
        }
      }

      const surfacePage = await context.newPage()
      await backend.install(surfacePage)
      await configureScaleConnection(surfacePage)

      const responsePromise = surfacePage
        .waitForResponse(
          (response) => new URL(response.url()).pathname === step.apiPath,
          { timeout: 60_000 },
        )
        .catch(() => null)

      const startedAt = Date.now()
      try {
        await surfacePage.goto(`/reader/${step.query}`, { waitUntil: 'domcontentloaded' })
        const response = await responsePromise
        const dataReadyMs = Date.now() - startedAt

        // Two animation frames is the cheapest reliable "React has committed and
        // the browser has painted" signal. networkidle is not usable here: the
        // Reader keeps background sync traffic alive, so it would time out rather
        // than settle.
        await surfacePage.evaluate(
          () => new Promise<void>((resolve) => {
            requestAnimationFrame(() => requestAnimationFrame(() => resolve()))
          }),
        )
        const paintedMs = Date.now() - startedAt

        const navigation = await surfacePage.evaluate(() => {
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

        surfaces.push({
          name: step.name,
          route: step.query,
          apiPath: step.apiPath,
          apiObserved: response !== null,
          apiStatus: response?.status() ?? null,
          dataReadyMs,
          paintedMs,
          navigation,
        })

        // Without this the surface half asserts nothing: a broken fixture that
        // stops the app booting produces four 60-second timeouts, records them as
        // numbers, and passes green. That is not hypothetical — it is what a too
        // greedy route glob did to this spec while it was being written.
        expect(response, `${step.name} issued ${step.apiPath}`).not.toBeNull()
        expect(response?.status(), `${step.name} loaded its data`).toBe(200)
      } finally {
        await surfacePage.close()
      }
    }

    // ------------------------------------------------------ API timings
    //
    // Driven from page context so the measurement includes the browser's own
    // decode and JSON.parse cost, which is where a 10 MiB inbox response is
    // actually expensive on the client.
    const probes = API_PROBES.map((probe) => ({
      name: probe.name,
      path: probe.path.replace('__TOKEN__', encodeURIComponent(fixture.search_token)),
    }))
    await page.goto('/reader/?surface=home', { waitUntil: 'domcontentloaded' })

    const apiTimings: ApiSampleResult[] = await page.evaluate(
      async ({ probeList, warmup, samples }) => {
        function pick(sorted: number[], fraction: number): number {
          if (sorted.length === 0) return 0
          const index = Math.min(sorted.length - 1, Math.max(0, Math.round(sorted.length * fraction) - 1))
          return sorted[index]
        }

        const results: ApiSampleResult[] = []
        for (const probe of probeList) {
          for (let warm = 0; warm < warmup; warm += 1) {
            await fetch(probe.path, { headers: { accept: 'application/json' } })
          }

          const totals: number[] = []
          const parses: number[] = []
          let status = 0
          let responseChars = 0
          let transferSize: number | null = null
          let encodedBodySize: number | null = null
          let decodedBodySize: number | null = null

          for (let sample = 0; sample < samples; sample += 1) {
            performance.clearResourceTimings()
            const startedAt = performance.now()
            const response = await fetch(probe.path, { headers: { accept: 'application/json' } })
            const text = await response.text()
            const transportMs = performance.now() - startedAt

            const parseStartedAt = performance.now()
            JSON.parse(text)
            const parseMs = performance.now() - parseStartedAt

            status = response.status
            responseChars = text.length
            totals.push(transportMs + parseMs)
            parses.push(parseMs)

            const wanted = new URL(probe.path, location.origin).pathname
            const entry = performance
              .getEntriesByType('resource')
              .find((candidate) => new URL(candidate.name).pathname === wanted) as
              | PerformanceResourceTiming
              | undefined
            if (entry) {
              transferSize = entry.transferSize
              encodedBodySize = entry.encodedBodySize
              decodedBodySize = entry.decodedBodySize
            }
          }

          totals.sort((left, right) => left - right)
          parses.sort((left, right) => left - right)
          results.push({
            name: probe.name,
            path: probe.path,
            status,
            samples,
            totalP50Ms: pick(totals, 0.5),
            totalP95Ms: pick(totals, 0.95),
            parseP50Ms: pick(parses, 0.5),
            transferSize,
            encodedBodySize,
            decodedBodySize,
            responseChars,
          })
        }
        return results
      },
      { probeList: probes, warmup: API_WARMUP, samples: API_SAMPLES },
    )

    const metrics: Record<string, unknown> = {
      schema_version: 2,
      fixture_id: fixture.fixture_id,
      seed: fixture.seed,
      data_scale: fixture.data_scale,
      journeyDurationMs: Date.now() - journeyStartedAt,
      surfaceWarmup: SURFACE_WARMUP,
      apiWarmup: API_WARMUP,
      apiSamples: API_SAMPLES,
      surfaces,
      apiTimings,
      unhandledApiRequests: [...new Set(backend.unhandled)],
      browser: await page.evaluate(() => ({
        userAgent: navigator.userAgent,
        viewport: { width: window.innerWidth, height: window.innerHeight },
      })),
    }
    writeFileSync(metricsPath, `${JSON.stringify(metrics, null, 2)}\n`, 'utf-8')

    // The only hard assertions: every measured endpoint answered, and the
    // payload the browser decoded is the scale payload rather than an empty
    // fallback. Sizes and durations go to the artifact, not to a threshold.
    for (const timing of apiTimings) {
      expect(timing.status, `${timing.name} responded`).toBe(200)
      expect(timing.responseChars, `${timing.name} returned a body`).toBeGreaterThan(0)
    }
    const inbox = apiTimings.find((timing) => timing.name === 'inbox_first_screen')
    expect(inbox, 'inbox first screen was measured').toBeDefined()
    // The heavy-body inbox first screen is the payload issue #40 phase 1.1
    // removes. Asserting it is currently multi-megabyte keeps the fixture
    // honest: if this ever drops without the fixture changing, the browser
    // baseline stopped exercising the pathology it exists to record.
    expect(inbox?.responseChars ?? 0, 'inbox first screen carries the heavy bodies').toBeGreaterThan(4 * 1024 * 1024)
  } finally {
    await context.tracing.stop({ path: tracePath })
  }
})
