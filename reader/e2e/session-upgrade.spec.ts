import { expect, test, type Page } from '@playwright/test'

interface UpgradeState {
  enabled: boolean
  bearerIdentity: 'A' | 'B'
  sessionIdentity: 'A' | 'B'
  bearerGets: number
  sessionGets: number
  exchanges: number
  deletes: number
  events: string[]
  cookieVerificationWaiters: number
}

async function control<Result>(page: Page, path: string): Promise<Result> {
  return page.evaluate(async (endpoint) => {
    const response = await fetch(endpoint)
    if (!response.ok) throw new Error(`control request failed: ${response.status}`)
    return response.json()
  }, path) as Promise<Result>
}

test.afterEach(async ({ request }) => {
  await request.get('/__test__/reset')
})

test('historical token upgrades to same-identity HttpOnly session and survives refresh', async ({
  context,
  page,
}) => {
  await page.goto('/__test__/blank')
  await control(page, '/__test__/reset')
  await control(page, '/__test__/legacy-session-upgrade?bearerIdentity=A&sessionIdentity=A')
  await context.clearCookies()
  await page.evaluate(() => {
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: window.location.origin,
        mode: 'installation-token',
        installationToken: 'legacy-secret',
      }),
    )
  })

  await page.goto('/reader/')
  await expect(page.getByText('A private v1', { exact: true }).first()).toBeVisible()

  const storedAfterUpgrade = await page.evaluate(() =>
    localStorage.getItem('webtag:reader:conn:v2'),
  )
  expect(JSON.parse(storedAfterUpgrade ?? '{}')).toMatchObject({
    baseURL: new URL(page.url()).origin,
    mode: 'session',
    installationToken: '',
    revision: expect.any(String),
  })
  expect(storedAfterUpgrade).not.toContain('legacy-secret')
  const sessionCookie = (await context.cookies()).find((cookie) => cookie.name === 'webtag_session')
  expect(sessionCookie).toMatchObject({ value: 'A', httpOnly: true })
  expect(await page.evaluate(() => document.cookie)).not.toContain('webtag_session')

  const beforeRefresh = await control<UpgradeState>(
    page,
    '/__test__/legacy-session-upgrade-state',
  )
  expect(beforeRefresh).toMatchObject({
    enabled: true,
    bearerIdentity: 'A',
    sessionIdentity: 'A',
    exchanges: 1,
  })
  expect(beforeRefresh.bearerGets).toBeGreaterThanOrEqual(1)
  expect(beforeRefresh.sessionGets).toBeGreaterThanOrEqual(2)

  await page.reload()
  await expect(page.getByText('A private v1', { exact: true }).first()).toBeVisible()

  const afterRefresh = await control<UpgradeState>(
    page,
    '/__test__/legacy-session-upgrade-state',
  )
  expect(afterRefresh.exchanges).toBe(1)
  expect(afterRefresh.bearerGets).toBe(beforeRefresh.bearerGets)
  expect(afterRefresh.sessionGets).toBeGreaterThan(beforeRefresh.sessionGets)
  expect(await page.evaluate(() => localStorage.getItem('webtag:reader:conn:v2'))).not.toContain(
    'legacy-secret',
  )
})

test('cross-tab replacement cancels a stale upgrade before its durable commit', async ({
  context,
  page,
}) => {
  await page.goto('/__test__/blank')
  await control(page, '/__test__/reset')
  await control(
    page,
    '/__test__/legacy-session-upgrade?bearerIdentity=A&sessionIdentity=A&blockCookieIdentity=1',
  )
  await context.clearCookies()
  await page.evaluate(() => {
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: window.location.origin,
        mode: 'installation-token',
        installationToken: 'legacy-secret',
      }),
    )
  })

  await page.goto('/reader/')
  await expect.poll(async () => (
    await control<UpgradeState>(page, '/__test__/legacy-session-upgrade-state')
  ).cookieVerificationWaiters).toBe(1)

  const replacementPage = await context.newPage()
  await replacementPage.goto('/__test__/connection-storage-harness')
  await expect(replacementPage.locator('body')).toHaveAttribute('data-ready', 'true')
  const replacementBaseURL = 'http://127.0.0.1:9'
  await replacementPage.evaluate(async (baseURL) => {
    await window.connectionStorageHarness.save({
      baseURL,
      mode: 'session',
      installationToken: '',
    })
  }, replacementBaseURL)

  const successorOutcome = await replacementPage.evaluate(async (baseURL) =>
    window.connectionStorageHarness.negotiateSuperseded(baseURL),
  new URL(page.url()).origin)
  expect(successorOutcome).toBe('error')

  const racedState = await control<UpgradeState>(
    replacementPage,
    '/__test__/legacy-session-upgrade-state',
  )
  expect(racedState.events).toEqual(['post', 'delete', 'post', 'delete'])
  expect(racedState).toMatchObject({ exchanges: 2, deletes: 2 })

  const durable = await replacementPage.evaluate(() =>
    JSON.parse(localStorage.getItem('webtag:reader:conn:v2') ?? '{}'),
  )
  expect(durable).toMatchObject({
    baseURL: replacementBaseURL,
    mode: 'session',
    installationToken: '',
    revision: expect.any(String),
  })
  expect(JSON.stringify(durable)).not.toContain('legacy-secret')
  expect((await context.cookies()).some((cookie) => cookie.name === 'webtag_session')).toBe(false)

  await control(replacementPage, '/__test__/release-legacy-cookie-identity')
  await replacementPage.close()
})
