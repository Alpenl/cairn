import { expect, test, type Page } from '@playwright/test'

const HARNESS_PATH = '/__test__/thought-sync-harness'

async function loadHarness(page: Page): Promise<void> {
  await page.goto(HARNESS_PATH)
  await page.waitForFunction(() => Boolean(window.thoughtSyncHarness))
}

async function install(page: Page, identity: 'A' | 'B'): Promise<void> {
  await page.evaluate((nextIdentity) => window.thoughtSyncHarness.install(nextIdentity), identity)
}

test('offline writes survive reload and recover through the namespace controller', async ({ page }) => {
  await loadHarness(page)
  await page.evaluate(async () => {
    await window.thoughtSyncHarness.reset()
    window.thoughtSyncHarness.setOnline(false)
    window.thoughtSyncHarness.install('A')
    await window.thoughtSyncHarness.commit('offline-write')
  })

  await expect.poll(() => page.evaluate(() => window.thoughtSyncHarness.snapshot())).toMatchObject({
    phase: 'offline',
    pendingCount: 1,
  })
  await expect.poll(() => page.evaluate(() => window.thoughtSyncHarness.outbox())).toEqual([
    { opId: 'offline-write', status: null },
  ])

  await page.reload()
  await page.waitForFunction(() => Boolean(window.thoughtSyncHarness))
  await install(page, 'A')
  await expect.poll(() => page.evaluate(() => window.thoughtSyncHarness.outbox())).toEqual([])
  await expect.poll(() => page.evaluate(() => window.thoughtSyncHarness.snapshot())).toMatchObject({
    phase: 'synced',
    pendingCount: 0,
  })
})

test('retryable and blocked operations remain observable without stalling later work', async ({ page }) => {
  await loadHarness(page)
  await page.evaluate(async () => {
    await window.thoughtSyncHarness.reset()
    window.thoughtSyncHarness.install('A')
    window.thoughtSyncHarness.setMode('retryable')
    await window.thoughtSyncHarness.commit('retryable-write')
  })
  await expect.poll(() => page.evaluate(() => window.thoughtSyncHarness.snapshot())).toMatchObject({
    phase: 'failed',
    pendingCount: 1,
    blockedCount: 0,
    errorCode: 'network-unreachable',
  })

  await page.evaluate(() => window.thoughtSyncHarness.setMode('ack'))
  await expect.poll(() => page.evaluate(() => window.thoughtSyncHarness.outbox()), { timeout: 5_000 }).toEqual([])

  await page.evaluate(async () => {
    window.thoughtSyncHarness.setMode('blocked')
    await window.thoughtSyncHarness.commit('blocked-write')
  })
  await expect.poll(() => page.evaluate(() => window.thoughtSyncHarness.snapshot())).toMatchObject({
    phase: 'failed',
    pendingCount: 1,
    blockedCount: 1,
    errorCode: 'other:invalid_thought_payload:422',
  })

  await page.evaluate(async () => {
    window.thoughtSyncHarness.setMode('ack')
    await window.thoughtSyncHarness.commit('after-blocked')
  })
  await expect.poll(() => page.evaluate(() => window.thoughtSyncHarness.outbox())).toEqual([
    { opId: 'blocked-write', status: 'blocked' },
  ])
})

test('same-namespace tabs wake each other and identity namespaces stay isolated', async ({ context, page }) => {
  await loadHarness(page)
  await page.evaluate(async () => {
    await window.thoughtSyncHarness.reset()
    window.thoughtSyncHarness.install('A')
  })
  const pageB = await context.newPage()
  await loadHarness(pageB)
  await install(pageB, 'A')
  const pullsBefore = await pageB.evaluate(() => window.thoughtSyncHarness.stats().pullCalls)

  await page.evaluate(async () => {
    await window.thoughtSyncHarness.commit('cross-tab-write')
  })
  await expect.poll(() => pageB.evaluate(() => window.thoughtSyncHarness.stats().pullCalls))
    .toBeGreaterThan(pullsBefore)

  await install(pageB, 'B')
  await expect.poll(() => pageB.evaluate(() => window.thoughtSyncHarness.outbox())).toEqual([])
  await pageB.close()
})
