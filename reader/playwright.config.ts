import { existsSync } from 'node:fs'
import { defineConfig } from '@playwright/test'

const port = Number(process.env.READER_E2E_PORT ?? 4179)
const baseURL = `http://127.0.0.1:${port}`
const browserName = process.env.READER_E2E_BROWSER === 'firefox' ? 'firefox' : 'chromium'
const configuredBrowser = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
const executablePath =
  browserName === 'chromium'
    ? configuredBrowser ?? (existsSync('/usr/bin/google-chrome') ? '/usr/bin/google-chrome' : undefined)
    : undefined

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
