import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import test from 'node:test'

import { classifyChangedPaths, formatGitHubOutputs, surfaces } from './ci-path-filter.mjs'

test('selects only the delivery surfaces touched by a focused change', () => {
  assert.deepEqual(classifyChangedPaths(['internal/service/reader_inbox.go']), {
    go: true,
    lint: true,
    reader: false,
    extension: false,
    android: false,
    ios: false,
    database: true,
  })
  assert.deepEqual(classifyChangedPaths(['reader/src/main.tsx']), {
    go: false,
    lint: false,
    reader: true,
    extension: false,
    android: false,
    ios: false,
    database: false,
  })
})

test('runs every surface when the workflow dispatcher changes', () => {
  const result = classifyChangedPaths(['.github/workflows/ci.yml'])
  assert.deepEqual(result, Object.fromEntries(surfaces.map((surface) => [surface, true])))
})

test('runs every surface when the path classifier changes', () => {
  for (const path of ['scripts/ci-path-filter.mjs', 'scripts/ci-path-filter.test.mjs']) {
    assert.deepEqual(
      classifyChangedPaths([path]),
      Object.fromEntries(surfaces.map((surface) => [surface, true])),
    )
  }
})

test('handles a large cross-platform pull request without losing early matches', () => {
  const paths = [
    'internal/service/reader_inbox.go',
    'reader/src/main.tsx',
    'extension/src/background.ts',
    'mobile/android/app/src/main/App.kt',
    'mobile/ios/WebTagShare/App/App.swift',
    ...Array.from({ length: 5000 }, (_, index) => `vendor/example/dependency/file-${index}.go`),
  ]
  const input = Buffer.from(`${paths.join('\0')}\0`)
  const run = spawnSync(process.execPath, ['scripts/ci-path-filter.mjs'], { input, encoding: 'utf8' })

  assert.equal(run.status, 0, run.stderr)
  assert.equal(
    run.stdout.trim(),
    formatGitHubOutputs(Object.fromEntries(surfaces.map((surface) => [surface, true]))),
  )
})

test('leaves every surface off for local-only project documentation', () => {
  assert.deepEqual(
    classifyChangedPaths(['README.md', 'docs/开发/测试与持续集成.md']),
    Object.fromEntries(surfaces.map((surface) => [surface, false])),
  )
})
