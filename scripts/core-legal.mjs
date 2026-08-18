#!/usr/bin/env node

import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import {
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  realpathSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, dirname, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const defaultExpected = join(root, 'legal', 'core')
const legalName = /^(licen[cs]e|copying|notice)(\..*)?$/i

function fail(message) {
  console.error(`core legal materials: ${message}`)
  process.exit(1)
}

function run(command, args) {
  try {
    return execFileSync(command, args, {
      cwd: root,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'inherit'],
    })
  } catch (error) {
    fail(`${command} ${args.join(' ')} failed with exit code ${error.status ?? 'unknown'}`)
  }
}

function sha256(path) {
  return createHash('sha256').update(readFileSync(path)).digest('hex')
}

function write(path, content) {
  mkdirSync(dirname(path), { recursive: true })
  writeFileSync(path, content.endsWith('\n') ? content : `${content}\n`, { mode: 0o644 })
}

function directLegalFiles(directory) {
  if (!existsSync(directory)) return []
  return readdirSync(directory, { withFileTypes: true })
    .filter((entry) => entry.isFile() && legalName.test(entry.name))
    .map((entry) => join(directory, entry.name))
    .sort()
}

function moduleLegalFiles(modulePath) {
  const vendorRoot = join(root, 'vendor')
  let directory = join(vendorRoot, ...modulePath.split('/'))
  while (directory.startsWith(`${vendorRoot}${sep}`)) {
    const files = directLegalFiles(directory)
    if (files.length > 0) return files
    directory = dirname(directory)
  }
  fail(`Go module ${modulePath} has no LICENSE/COPYING/NOTICE in vendor`)
}

function goModules(command, tags = '') {
  const args = ['list', '-mod=vendor']
  if (tags) args.push(`-tags=${tags}`)
  args.push('-deps', '-f', '{{with .Module}}{{if and (not .Main) .Path}}{{.Path}}\t{{.Version}}{{end}}{{end}}', command)
  const modules = new Map()
  for (const line of run('go', args).split('\n')) {
    if (!line) continue
    const [path, version] = line.split('\t')
    if (!path || !version || version === '(devel)') fail(`invalid Go module identity: ${line}`)
    const previous = modules.get(path)
    if (previous && previous !== version) fail(`Go module ${path} resolved to both ${previous} and ${version}`)
    modules.set(path, version)
  }
  return [...modules].sort(([a], [b]) => a.localeCompare(b))
}

function renderGoBundle(binary, command, tags = '') {
  const modules = goModules(command, tags)
  if (modules.length === 0) fail(`${binary} production closure is empty`)
  const output = [
    'Cairn Core Go third-party legal materials',
    `Binary: ${binary}`,
    `Build package: ${command}`,
    `Build tags: ${tags || '(none)'}`,
    '',
    'Only modules in this binary\'s frozen production build closure are listed.',
    '',
    'MODULES',
  ]
  for (const [path, version] of modules) output.push(`${path} ${version}`)

  for (const [path, version] of modules) {
    output.push('', `===== ${path} ${version} =====`)
    for (const licensePath of moduleLegalFiles(path)) {
      output.push(`--- ${relative(root, licensePath)} ---`, readFileSync(licensePath, 'utf8').trimEnd())
    }
  }
  return `${output.join('\n')}\n`
}

function readerPackages() {
  if (!existsSync(join(root, 'node_modules'))) {
    fail('node_modules is missing; run pnpm install --frozen-lockfile before generating')
  }
  const raw = run('pnpm', ['--filter', 'webtag-reader', 'licenses', 'list', '--prod', '--json', '--long'])
  const groups = JSON.parse(raw)
  const packages = new Map()
  for (const [reportedLicense, entries] of Object.entries(groups)) {
    if (/unknown|unlicensed/i.test(reportedLicense)) fail(`Reader dependency has unknown license group ${reportedLicense}`)
    for (const entry of entries) {
      for (const packagePath of entry.paths ?? []) {
        const manifestPath = join(packagePath, 'package.json')
        if (!existsSync(manifestPath)) fail(`Reader dependency manifest is missing: ${manifestPath}`)
        const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'))
        const license = typeof manifest.license === 'string' ? manifest.license : reportedLicense
        if (!license || /unknown|unlicensed/i.test(license)) {
          fail(`Reader dependency ${manifest.name}@${manifest.version} has unknown license metadata`)
        }
        const files = directLegalFiles(packagePath)
        if (files.length === 0) fail(`Reader dependency ${manifest.name}@${manifest.version} has no legal text`)
        const key = `${manifest.name}@${manifest.version}`
        const canonical = realpathSync(packagePath)
        const previous = packages.get(key)
        if (!previous || canonical.localeCompare(previous.path) < 0) {
          packages.set(key, {
            name: manifest.name,
            version: manifest.version,
            license,
            homepage: manifest.homepage ?? entry.homepage ?? '',
            path: canonical,
            files,
          })
        }
      }
    }
  }
  return [...packages.values()].sort((a, b) => `${a.name}@${a.version}`.localeCompare(`${b.name}@${b.version}`))
}

function renderReaderBundle() {
  const packages = readerPackages()
  if (packages.length === 0) fail('Reader production dependency closure is empty')
  const output = [
    'Cairn embedded Reader third-party legal materials',
    '',
    'This list is generated from the frozen production dependency graph selected by',
    '`pnpm --filter webtag-reader licenses list --prod`. Build-only and test-only',
    'dependencies are intentionally excluded.',
    '',
    'PACKAGES',
  ]
  for (const item of packages) {
    output.push(`${item.name}@${item.version} | ${item.license}${item.homepage ? ` | ${item.homepage}` : ''}`)
  }
  for (const item of packages) {
    output.push('', `===== ${item.name}@${item.version} (${item.license}) =====`)
    for (const licensePath of item.files) {
      output.push(`--- ${basename(licensePath)} ---`, readFileSync(licensePath, 'utf8').trimEnd())
    }
  }
  return `${output.join('\n')}\n`
}

function generate(output) {
  rmSync(output, { recursive: true, force: true })
  const common = join(output, 'common')
  mkdirSync(common, { recursive: true })

  const repositoryLicense = readFileSync(join(root, 'LICENSE'), 'utf8')
  const mitStart = repositoryLicense.indexOf('MIT License\n')
  if (mitStart < 0) fail('repository LICENSE does not contain the Cairn MIT text')
  write(join(common, 'CAIRN_LICENSE.txt'), repositoryLicense.slice(mitStart))

  const dictionaryDir = join(root, 'internal', 'service', 'translator', 'dictionary')
  cpSync(join(dictionaryDir, 'LICENSE'), join(common, 'OPENCC_LICENSE.txt'))
  const dictionaries = ['TSCharacters.txt', 'TSPhrases.txt']
  const dictionaryLines = dictionaries.map((name) => `${sha256(join(dictionaryDir, name))}  ${name}`)
  write(
    join(common, 'OPENCC_SOURCE.txt'),
    [
      'Embedded OpenCC dictionary source',
      '',
      'Upstream: https://github.com/longbridgeapp/opencc',
      'Version: v0.3.13',
      'Configuration: official OpenCC t2s dictionaries',
      'License: Apache-2.0; see OPENCC_LICENSE.txt',
      '',
      'Embedded asset SHA-256:',
      ...dictionaryLines,
    ].join('\n'),
  )

  write(join(common, 'GO_WEBTAG_THIRD_PARTY.txt'), renderGoBundle('webtag', './cmd/webtag', 'nomsgpack,sonic'))
  write(join(common, 'GO_MIGRATE_THIRD_PARTY.txt'), renderGoBundle('migrate', './cmd/migrate'))
  write(join(common, 'READER_THIRD_PARTY.txt'), renderReaderBundle())
  write(
    join(common, 'DISTRIBUTION_BOUNDARY.txt'),
    [
      'Cairn Core application legal-material boundary',
      '',
      'This directory covers application content added by Cairn:',
      '- CAIRN_LICENSE.txt: both Core executables and first-party embedded Reader code.',
      '- OPENCC_LICENSE.txt and OPENCC_SOURCE.txt: embedded dictionary assets.',
      '- GO_WEBTAG_THIRD_PARTY.txt: modules linked into /app/webtag.',
      '- GO_MIGRATE_THIRD_PARTY.txt: modules linked into /app/migrate.',
      '- READER_THIRD_PARTY.txt: production packages compiled into the embedded Reader.',
      '',
      'It does not claim to enumerate the inherited Alpine base image or apk runtime',
      'packages. Their package database remains in the image, and release SBOM/Trivy',
      'evidence inventories that separate base/runtime layer at each child digest.',
    ].join('\n'),
  )

  for (const path of walkFiles(output)) {
    if (statSync(path).size === 0) fail(`generated an empty legal material: ${relative(output, path)}`)
  }
}

function walkFiles(directory) {
  if (!existsSync(directory)) return []
  const result = []
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name)
    if (entry.isDirectory()) result.push(...walkFiles(path))
    else if (entry.isFile()) result.push(path)
  }
  return result.sort()
}

function inventory(directory) {
  return new Map(walkFiles(directory).map((path) => [relative(directory, path), sha256(path)]))
}

function compare(expected, actual) {
  const expectedFiles = inventory(expected)
  const actualFiles = inventory(actual)
  const names = new Set([...expectedFiles.keys(), ...actualFiles.keys()])
  const changed = [...names]
    .sort()
    .filter((name) => expectedFiles.get(name) !== actualFiles.get(name))
  if (changed.length > 0) {
    console.error('Core legal materials are stale or incomplete:')
    for (const name of changed) console.error(`  ${name}`)
    return false
  }
  return true
}

const [mode = 'check', argument] = process.argv.slice(2)
if (!['generate', 'check'].includes(mode)) fail('usage: core-legal.mjs generate [output] | check [expected]')

if (mode === 'generate') {
  const output = resolve(argument ?? defaultExpected)
  generate(output)
  console.log(`generated Core legal materials in ${relative(root, output) || '.'}`)
} else {
  const expected = resolve(argument ?? defaultExpected)
  const temporary = mkdtempSync(join(tmpdir(), 'cairn-core-legal-'))
  try {
    const actual = join(temporary, 'core')
    generate(actual)
    if (!compare(expected, actual)) process.exit(1)
    console.log('Core legal materials match the frozen production closures')
  } finally {
    rmSync(temporary, { recursive: true, force: true })
  }
}
