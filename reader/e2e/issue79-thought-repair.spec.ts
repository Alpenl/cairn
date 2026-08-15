import { expect, test } from '@playwright/test'

test.describe('v6 legacy thought repair in Chromium', () => {
  test('upgrades compacted v4 update/delete tails without mutating the legacy log', async ({ page }) => {
    await page.goto('/__test__/thought-repair-harness')
    await page.waitForFunction(() => Boolean(window.thoughtRepairHarness))
    for (const kind of ['update', 'delete'] as const) {
      const result = await page.evaluate((operationKind) =>
        window.thoughtRepairHarness.migrateTail(operationKind), kind)
      expect(result.legacyAfter).toEqual(result.legacyBefore)
      expect(result.ready).toHaveLength(2)
      expect(result.ready.map((record) => (record as { operationKind: string }).operationKind)).toEqual([
        'add', kind,
      ])
      expect(result.wireOps.map((record) => (record as { operation_kind: string }).operation_kind)).toEqual([
        'add', kind,
      ])
      expect(result.completeBaseReachedServer).toBe(true)
      expect(result.serverBody).toBe(kind === 'update' ? 'updated' : null)
      expect(result.ready[0]).toMatchObject({
        annotationId: 'tail-a1',
        opId: 'v6-repair:10:browser-v4:7:tail-a1',
        repair: true,
      })
      expect(result.manifest).toEqual([expect.objectContaining({
        namespace: 'browser-v4', schemaVersion: 6, complete: true, readyCount: 2,
      })])
    }
  })

  test('wakes another tab without ever dispatching quarantine payload', async ({ page }) => {
    await page.goto('/__test__/thought-repair-harness')
    await page.waitForFunction(() => Boolean(window.thoughtRepairHarness))
    await page.evaluate(() => window.thoughtRepairHarness.startQuarantineLifecycle())
    await expect.poll(() => page.evaluate(() =>
      window.thoughtRepairHarness.lifecycleMetrics().pulls)).toBeGreaterThan(0)
    await page.waitForTimeout(100)
    const before = await page.evaluate(() => window.thoughtRepairHarness.lifecycleMetrics().pulls)

    const other = await page.context().newPage()
    try {
      await other.goto('/__test__/thought-repair-harness')
      await other.evaluate(() => {
        const channel = new BroadcastChannel('webtag-reader-thought-sync-v1')
        channel.postMessage({
          kind: 'thought-repair-quarantine',
          namespace: 'browser-v4', reasons: [{ reason: 'invalid_quote', count: 2 }],
        })
        channel.close()
      })
      await expect.poll(() => page.evaluate(() =>
        window.thoughtRepairHarness.lifecycleMetrics().pulls)).toBeGreaterThan(before)
    } finally {
      await other.close()
    }
    const metrics = await page.evaluate(() => window.thoughtRepairHarness.lifecycleMetrics())
    expect(metrics.pushedAnnotationIds).not.toContain('browser-bad')
  })
})
