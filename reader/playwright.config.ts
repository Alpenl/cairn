import { existsSync } from 'node:fs'
import { defineConfig } from '@playwright/test'

const port = Number(process.env.READER_E2E_PORT ?? 4179)
const baseURL = `http://127.0.0.1:${port}`
const browserName = process.env.READER_E2E_BROWSER === 'firefox' ? 'firefox' : 'chromium'
const configuredBrowser = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
// READER_E2E_IGNORE_SYSTEM_CHROME=1 可以在本地复现 CI 那台没有系统 Chrome 的
// runner，用来验证下面的 channel 回退分支是否真的生效。
const systemChrome =
  process.env.READER_E2E_IGNORE_SYSTEM_CHROME === '1'
    ? undefined
    : ['/usr/bin/google-chrome', '/usr/bin/google-chrome-stable'].find((path) => existsSync(path))
const executablePath =
  browserName === 'chromium' ? (configuredBrowser || systemChrome) : undefined

// 没有系统 Chrome 时用完整的 chromium 构建，而不是 Playwright 在 headless 下
// 默认启动的 chromium-headless-shell。用 READER_E2E_IGNORE_SYSTEM_CHROME=1
// 可以在有 Chrome 的机器上复现这条分支。
const channel = browserName === 'chromium' && !executablePath ? 'chromium' : undefined

export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.spec.ts',
  outputDir: process.env.READER_PERF_OUTPUT_DIR ?? 'test-results',
  fullyParallel: false,
  workers: 1,
  timeout: 30_000,
  expect: { timeout: 5_000 },
  use: {
    baseURL,
    browserName,
    channel,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    launchOptions: executablePath ? { executablePath } : undefined,
  },
  webServer: {
    command: 'node e2e/server.mjs',
    env: { READER_E2E_PORT: String(port) },
    url: `${baseURL}/__test__/health`,
    reuseExistingServer: false,
    timeout: 120_000,
  },
})
