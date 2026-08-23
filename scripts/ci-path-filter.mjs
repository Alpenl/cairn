#!/usr/bin/env node

import { fileURLToPath } from 'node:url'

export const surfaces = ['go', 'lint', 'reader', 'extension', 'android', 'ios', 'database']

const patterns = {
  go: /^(cmd|internal|test)\/|^go\.(mod|sum)$|^vendor\/|^Makefile$|^Dockerfile$|^\.dockerignore$|^\.env\.example$|^deploy\/|^legal\/core\/|^scripts\/(version(\.test)?|container_smoke|db_migrate_smoke|db-dump-schema|migrate-dbintegration|cairn-install(\.test)?|core-(legal|release-(build|verify|manifest|promote))(\.test)?)\.(sh|mjs)$|^\.github\/workflows\/go-verify\.yml$/,
  lint: /^(cmd|internal|test)\/|^go\.(mod|sum)$|^vendor\/|^Makefile$|^Dockerfile$|^\.dockerignore$|^deploy\/|^legal\/core\/|^scripts\/(cairn-install(\.test)?|core-(legal|release-(build|verify|manifest|promote))(\.test)?|migrate-dbintegration|verify-action-pins)\.(sh|mjs)$|^\.golangci|^\.github\/workflows\//,
  reader: /^(reader|packages)\/|^internal\/app\/assets\/openapi\.json$|^pnpm-|^package\.json$|^\.github\/workflows\/reader-ci\.yml$/,
  extension: /^(extension|packages)\/|^internal\/app\/assets\/openapi\.json$|^pnpm-|^package\.json$|^\.github\/workflows\/extension-ci\.yml$/,
  android: /^mobile\/(android|shared)\/|^scripts\/mobile-|^\.github\/workflows\/mobile-android\.yml$/,
  ios: /^mobile\/(ios|shared)\/|^scripts\/mobile-|^\.github\/workflows\/mobile-ios\.yml$/,
  database: /^internal\/|^test\/dbintegration\/|\.sql$|^go\.(mod|sum)$|^scripts\/(db-dump-schema|db_migrate_smoke|migrate-dbintegration)\.sh$|^\.github\/workflows\/dbintegration\.yml$/,
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
