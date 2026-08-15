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

// 没有系统 Chrome 路径时退回 Playwright 的 'chrome' 通道（同样是 Google Chrome，
// 只是由 Playwright 定位）。不能退到 'chromium'：原生 dialog 关闭后，chromium
// 构建（含 chromium-headless-shell）不会把焦点还给触发它的按钮，
// document.activeElement 退回 <body>，依赖 dialog 后焦点的用例因此只在没有
// Google Chrome 的机器上失败。用 READER_E2E_IGNORE_SYSTEM_CHROME=1 可本地复现。
const channel = browserName === 'chromium' && !executablePath ? 'chrome' : undefined

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
