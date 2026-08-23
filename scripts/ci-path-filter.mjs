#!/usr/bin/env node

import { fileURLToPath } from 'node:url'

export const surfaces = ['go', 'lint', 'reader', 'extension', 'android', 'ios', 'database', 'delivery']

const patterns = {
  go: /^(cmd|internal|test)\/|^go\.(mod|sum)$|^vendor\/|^Makefile$|^Dockerfile$|^\.dockerignore$|^\.env\.example$|^scripts\/(version(\.test)?|container_smoke|db_migrate_smoke|db-dump-schema)\.sh$|^\.github\/workflows\/go-verify\.yml$/,
  lint: /^(cmd|internal|test)\/|^go\.(mod|sum)$|^vendor\/|^Makefile$|^Dockerfile$|^\.dockerignore$|^\.golangci|^\.github\/workflows\/|^scripts\/verify-action-pins\.sh$/,
  reader: /^(reader|packages)\/|^internal\/app\/assets\/openapi\.json$|^pnpm-|^package\.json$|^\.github\/workflows\/reader-ci\.yml$/,
  extension: /^(extension|packages)\/|^internal\/app\/assets\/openapi\.json$|^pnpm-|^package\.json$|^\.github\/workflows\/extension-ci\.yml$/,
  android: /^mobile\/(android|shared)\/|^scripts\/mobile-|^\.github\/workflows\/mobile-android\.yml$/,
  ios: /^mobile\/(ios|shared)\/|^scripts\/mobile-|^\.github\/workflows\/mobile-ios\.yml$/,
  database: /^internal\/|^test\/dbintegration\/|\.sql$|^go\.(mod|sum)$|^scripts\/(db-dump-schema|db_migrate_smoke|migrate-dbintegration)\.sh$|^\.github\/workflows\/dbintegration\.yml$/,
  delivery:
    /^(?:Dockerfile$|\.dockerignore$|\.env\.example$|Makefile$|(?:deploy|legal\/core|\.github\/rulesets)\/|\.github\/workflows\/(?:ci|delivery|release-core)\.yml$|scripts\/(?:ci-path-filter(?:\.test)?|ci-run-diagnose(?:\.test)?|validate-main-ruleset(?:\.test)?|deploy-contracts\.test|cairn-install(?:\.test)?|migrate-dbintegration|container_smoke|db_migrate_smoke|reader-vnext-release(?:\.test)?|core-(?:legal|release-(?:series|build|verify|manifest|promote))(?:\.test)?)\.(?:mjs|sh)$|scripts\/core_release_workflow_test\.go$)/,
}

const dispatcherPattern =
  /^(?:\.github\/workflows\/(?:ci|labeler)\.yml|scripts\/ci-path-filter(?:\.test)?\.mjs)$/

export function classifyChangedPaths(paths) {
  const changed = paths.filter(Boolean)
  const runAll = changed.some((path) => dispatcherPattern.test(path))
  return Object.fromEntries(
    surfaces.map((surface) => [surface, runAll || changed.some((path) => patterns[surface].test(path))]),
  )
}

export function formatGitHubOutputs(result) {
  return surfaces.map((surface) => `${surface}=${result[surface] ? 'true' : 'false'}`).join('\n')
}

async function main() {
  const chunks = []
  for await (const chunk of process.stdin) chunks.push(chunk)
  const input = Buffer.concat(chunks)
  const paths = input.toString('utf8').split('\0')
  process.stdout.write(`${formatGitHubOutputs(classifyChangedPaths(paths))}\n`)
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) await main()
