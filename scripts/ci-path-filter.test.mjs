import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import { classifyChangedPaths, formatGitHubOutputs, surfaces } from './ci-path-filter.mjs'

const everySurface = [...surfaces]
const criticalPathInventory = [
  ['.dockerignore', ['go', 'lint', 'delivery']],
  ['.env.example', ['go', 'delivery']],
  ['Dockerfile', ['go', 'delivery']],
  ['Makefile', ['go', 'lint', 'delivery']],
  ['.github/rulesets/main-policy.json', ['delivery']],
  ['.github/workflows/ci.yml', everySurface],
  ['.github/workflows/delivery.yml', ['lint', 'delivery']],
  ['.github/workflows/release-core.yml', ['lint', 'delivery']],
  ['deploy/Caddyfile', ['delivery']],
  ['deploy/caddy/cairn-deploy.caddy', ['delivery']],
  ['deploy/cairn-updater.env.example', ['delivery']],
  ['deploy/systemd/cairn-updater.service', ['delivery']],
  ['deploy/systemd/webtag.service', ['delivery']],
  ['legal/core/common/CAIRN_LICENSE.txt', ['delivery']],
  ['legal/core/common/DISTRIBUTION_BOUNDARY.txt', ['delivery']],
  ['legal/core/common/GO_MIGRATE_THIRD_PARTY.txt', ['delivery']],
  ['legal/core/common/GO_WEBTAG_THIRD_PARTY.txt', ['delivery']],
  ['legal/core/common/NOTICE', ['delivery']],
  ['legal/core/common/OPENCC_LICENSE.txt', ['delivery']],
  ['legal/core/common/OPENCC_SOURCE.txt', ['delivery']],
  ['legal/core/common/READER_THIRD_PARTY.txt', ['delivery']],
  ['scripts/cairn-install.sh', ['delivery']],
  ['scripts/cairn-install.test.sh', ['delivery']],
  ['scripts/ci-path-filter.mjs', everySurface],
  ['scripts/ci-path-filter.test.mjs', everySurface],
  ['scripts/ci-run-diagnose.mjs', ['delivery']],
  ['scripts/ci-run-diagnose.test.mjs', ['delivery']],
  ['scripts/container_smoke.sh', ['go', 'delivery']],
  ['scripts/core-legal.mjs', ['delivery']],
  ['scripts/core-legal.test.sh', ['delivery']],
  ['scripts/core-release-build.sh', ['delivery']],
  ['scripts/core-release-build.test.sh', ['delivery']],
  ['scripts/core-release-manifest.sh', ['delivery']],
  ['scripts/core-release-manifest.test.sh', ['delivery']],
  ['scripts/core-release-promote.sh', ['delivery']],
  ['scripts/core-release-promote.test.sh', ['delivery']],
  ['scripts/core-release-series.sh', ['delivery']],
  ['scripts/core-release-verify.sh', ['delivery']],
  ['scripts/core-release-verify.test.sh', ['delivery']],
  ['scripts/core_release_workflow_test.go', ['delivery']],
  ['scripts/db_migrate_smoke.sh', ['go', 'database', 'delivery']],
  ['scripts/deploy-contracts.test.sh', ['delivery']],
  ['scripts/migrate-dbintegration.sh', ['database', 'delivery']],
  ['scripts/reader-vnext-release.sh', ['delivery']],
  ['scripts/reader-vnext-release.test.sh', ['delivery']],
  ['scripts/validate-main-ruleset.mjs', ['delivery']],
  ['scripts/validate-main-ruleset.test.mjs', ['delivery']],
  ['scripts/verify-action-pins.sh', ['lint']],
]

const criticalPathSet = new Set(criticalPathInventory.map(([path]) => path))
const criticalExactPaths = new Set([
  '.dockerignore',
  '.env.example',
  'Dockerfile',
  'Makefile',
  '.github/workflows/ci.yml',
  '.github/workflows/delivery.yml',
  '.github/workflows/release-core.yml',
  'scripts/core_release_workflow_test.go',
])
const criticalScriptPattern =
  /^scripts\/(?:ci-path-filter(?:\.test)?|ci-run-diagnose(?:\.test)?|validate-main-ruleset(?:\.test)?|verify-action-pins|deploy-contracts\.test|cairn-install(?:\.test)?|migrate-dbintegration|container_smoke|db_migrate_smoke|reader-vnext-release(?:\.test)?|core-(?:legal|release-.+)(?:\.test)?)\.(?:mjs|sh)$/

function isTrackedCriticalPath(path) {
  return (
    criticalExactPaths.has(path) ||
    path.startsWith('.github/rulesets/') ||
    path.startsWith('deploy/') ||
    path.startsWith('legal/core/') ||
    criticalScriptPattern.test(path)
  )
}

test('selects only the delivery surfaces touched by a focused change', () => {
  assert.deepEqual(classifyChangedPaths(['internal/service/reader_inbox.go']), {
    go: true,
    lint: true,
    reader: false,
    extension: false,
    android: false,
    ios: false,
    database: true,
    delivery: false,
  })
  assert.deepEqual(classifyChangedPaths(['reader/src/main.tsx']), {
    go: false,
    lint: false,
    reader: true,
    extension: false,
    android: false,
    ios: false,
    database: false,
    delivery: false,
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

test('routes the declared critical-path inventory through real verifier surfaces', () => {
  for (const [path, requiredSurfaces] of criticalPathInventory) {
    const result = classifyChangedPaths([path])
    for (const surface of requiredSurfaces) {
      assert.equal(result[surface], true, `${path} must trigger ${surface}`)
    }
  }
})

test('covers every tracked critical release, policy, legal, and deploy path in the inventory', () => {
  const run = spawnSync(
    'git',
    [
      'ls-files',
      '.dockerignore',
      '.env.example',
      'Dockerfile',
      'Makefile',
      '.github/rulesets',
      '.github/workflows',
      'deploy',
      'legal/core',
      'scripts',
    ],
    { encoding: 'utf8' },
  )
  assert.equal(run.status, 0, run.stderr)

  const missing = run.stdout
    .trim()
    .split('\n')
    .filter(Boolean)
    .filter(isTrackedCriticalPath)
    .filter((path) => !criticalPathSet.has(path))
    .sort()

  assert.deepEqual(missing, [])
})

test('keeps the delivery job inside the aggregate gate', () => {
  const workflow = readFileSync(new URL('../.github/workflows/ci.yml', import.meta.url), 'utf8')
  const changesOutputs = workflow.match(/outputs:\n(?<outputs>(?: {6}\w+: .+\n)+)/)?.groups?.outputs ?? ''
  assert.match(changesOutputs, /^\s+delivery:/m)
  assert.match(workflow, /^\s+delivery:\n/m)

  const gateNeeds = workflow.match(/^\s+needs:\s+\[(?<jobs>[^\]]+)\]/m)?.groups?.jobs ?? ''
  for (const job of ['changes', 'commitlint', 'go', 'lint', 'reader', 'extension', 'android', 'ios', 'database', 'delivery']) {
    assert.match(gateNeeds, new RegExp(`\\b${job}\\b`), `gate must depend on ${job}`)
  }
})

test('runs real delivery verifiers instead of only routing tests', () => {
  const workflow = readFileSync(new URL('../.github/workflows/delivery.yml', import.meta.url), 'utf8')
  assert.match(workflow, /^\s+contracts:\n/m)
  assert.match(workflow, /^\s+core-release:\n/m)
  assert.match(workflow, /^\s+reader-bundle:\n/m)
  assert.match(workflow, /^\s+deploy:\n/m)
  assert.match(workflow, /make ci-contracts/)
  assert.match(workflow, /make core-release-test/)
  assert.match(workflow, /make reader-bundle-test/)
  assert.match(workflow, /make deploy-contracts deploy-permissions/)
})

test('handles a large cross-platform pull request without losing early matches', () => {
  const paths = [
    'internal/service/reader_inbox.go',
    'reader/src/main.tsx',
    'extension/src/background.ts',
    'mobile/android/app/src/main/App.kt',
    'mobile/ios/WebTagShare/App/App.swift',
    'deploy/Caddyfile',
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
