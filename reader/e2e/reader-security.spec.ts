import { expect, test } from '@playwright/test'

test('untrusted images stay offline and Reader cache reset preserves sibling applications', async ({ page }) => {
  const imageProbeRequests: string[] = []
  const pageErrors: string[] = []
  page.on('pageerror', (error) => pageErrors.push(error.message))
  page.on('request', (request) => {
    if (request.resourceType() === 'image') {
      imageProbeRequests.push(request.url())
    }
  })

  await page.goto('/__test__/reader-security-harness?probe=/__test__/image-probe')
  await expect.poll(() => pageErrors).toEqual([])
  await expect(page.getByRole('img', { name: 'Markdown 1' })).toBeVisible()
  await expect(page.getByRole('img', { name: 'Markdown 9' })).toBeVisible()
  await expect(page.getByRole('img', { name: 'Feed loopback' })).toBeVisible()
  await expect(page.getByRole('img', { name: 'Feed srcset' })).toBeVisible()
  await expect(page.getByRole('img', { name: 'Feed picture' })).toBeVisible()
  await expect(page.locator('img')).toHaveCount(0)
  for (const placeholder of await page.getByRole('img').all()) await placeholder.click()
  await page.waitForTimeout(100)
  expect(imageProbeRequests).toEqual([])

  const seeded = await page.evaluate(async () => {
    const currentCache = 'webtag-reader-%2Freader%2F-precache-v2-browser'
    const otherReaderCache = 'webtag-reader-%2Fother-reader%2F-precache-v2-browser'
    const siblingCache = 'other-app-shell-v1'
    await Promise.all([
      caches.open(currentCache),
      caches.open(otherReaderCache),
      caches.open(siblingCache),
    ])
    const readerRegistration = await navigator.serviceWorker.register('/__test__/reader-sw.js', { scope: '/reader/' })
    const siblingRegistration = await navigator.serviceWorker.register('/__test__/other-sw.js', { scope: '/other-app/' })
    return {
      currentCache,
      otherReaderCache,
      siblingCache,
      readerScope: readerRegistration.scope,
      siblingScope: siblingRegistration.scope,
    }
  })

  try {
    await page.evaluate(() => window.readerSecurityHarness.resetApplicationCache())
    const remaining = await page.evaluate(async () => ({
      caches: await caches.keys(),
      scopes: (await navigator.serviceWorker.getRegistrations()).map((registration) => registration.scope),
    }))

    expect(remaining.caches).not.toContain(seeded.currentCache)
    expect(remaining.caches).toContain(seeded.otherReaderCache)
    expect(remaining.caches).toContain(seeded.siblingCache)
    expect(remaining.scopes).not.toContain(seeded.readerScope)
    expect(remaining.scopes).toContain(seeded.siblingScope)
  } finally {
    await page.evaluate(async () => {
      await Promise.allSettled((await navigator.serviceWorker.getRegistrations()).map((registration) => registration.unregister()))
      await Promise.allSettled((await caches.keys()).map((key) => caches.delete(key)))
    })
  }
})

test('Reader CSP blocks an accidental cross-origin image that bypasses sanitization', async ({ page, request, baseURL }) => {
  await request.post('/__test__/image-probe-reset')
  const port = Number(new URL(baseURL as string).port)
  const leak = `http://127.0.0.1:${port + 1}/pixel.png?surface=csp`
  const response = await page.goto(`/__test__/reader-security-harness?cspLeak=${encodeURIComponent(leak)}`)
  expect(response?.headers()['content-security-policy']).toContain("img-src 'self' data: blob:")
  await expect(page.getByRole('img', { name: 'CSP leak probe' })).toBeAttached()
  await page.waitForTimeout(100)

  const probe = await request.get('/__test__/image-probe-count')
  expect(await probe.json()).toEqual({ count: 0 })
})
