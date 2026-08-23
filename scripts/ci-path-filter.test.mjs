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

test('dispatches native workflows for platform and shared contract changes', () => {
  for (const path of [
    'mobile/android/app/build.gradle.kts',
    'mobile/shared/fixtures/share-payloads.json',
    'scripts/mobile-x1-check.py',
    'scripts/mobile-wire-smoke.py',
  ]) {
    assert.equal(classifyChangedPaths([path]).android, true, path)
  }
  for (const path of [
    'mobile/ios/WebTagShare/Shared/WebTagShareCore.swift',
    'mobile/shared/fixtures/share-payloads.json',
    'scripts/mobile-x1-check.py',
    'scripts/mobile-wire-smoke.py',
  ]) {
    assert.equal(classifyChangedPaths([path]).ios, true, path)
  }
})

test('dispatches substantive workflows for critical delivery files', () => {
  const cases = [
    {
      path: 'scripts/core-release-promote.sh',
      surfaces: ['go', 'lint'],
    },
    {
      path: 'scripts/cairn-install.sh',
      surfaces: ['go', 'lint'],
    },
    {
      path: 'scripts/migrate-dbintegration.sh',
      surfaces: ['go', 'lint', 'database'],
    },
    {
      path: 'legal/core/common/NOTICE',
      surfaces: ['go', 'lint'],
    },
    {
      path: '.dockerignore',
      surfaces: ['go', 'lint'],
    },
    {
      path: 'deploy/Caddyfile',
      surfaces: ['go', 'lint'],
    },
  ]

  for (const { path, surfaces: expectedSurfaces } of cases) {
    const result = classifyChangedPaths([path])
    assert.equal(
      Object.values(result).some(Boolean),
      true,
      `${path} must not be treated as a documentation-only or dispatcher-only change`,
    )
    for (const surface of expectedSurfaces) assert.equal(result[surface], true, `${path} must trigger ${surface}`)
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
