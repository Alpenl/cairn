import { readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'

import ts from 'typescript'
import { afterEach, describe, expect, it } from 'vitest'

import { readerIdentity } from './identity'
import {
  readOwnedStorage,
  storageOwnershipRegistry,
  writeOwnedStorage,
} from './storage-ownership'

function productionSources(directory: string): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const target = path.join(directory, entry.name)
    if (entry.isDirectory() && entry.name === 'test') return []
    if (entry.isDirectory()) return productionSources(target)
    if (!/\.[cm]?[jt]sx?$/.test(entry.name) || entry.name.includes('.test.')) return []
    return [target]
  })
}

function isLocalStorageReceiver(node: ts.Expression): boolean {
  if (ts.isIdentifier(node)) return node.text === 'localStorage'
  return (
    ts.isPropertyAccessExpression(node) &&
    node.name.text === 'localStorage' &&
    ts.isIdentifier(node.expression) &&
    (node.expression.text === 'window' ||
      node.expression.text === 'globalThis' ||
      node.expression.text === 'self')
  )
}

function isIndexedDBReceiver(node: ts.Expression): boolean {
  if (ts.isIdentifier(node)) return node.text === 'indexedDB'
  return (
    ts.isPropertyAccessExpression(node) &&
    node.name.text === 'indexedDB' &&
    ts.isIdentifier(node.expression) &&
    (node.expression.text === 'window' ||
      node.expression.text === 'globalThis' ||
      node.expression.text === 'self')
  )
}

function persistentStorageBypassesInSource(source: string, relative: string): string[] {
  const sourceFile = ts.createSourceFile(
    relative,
    source,
    ts.ScriptTarget.Latest,
    true,
    relative.endsWith('.tsx') ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
  )
  const findings: string[] = []
  const visit = (node: ts.Node): void => {
    if (ts.isCallExpression(node) && ts.isPropertyAccessExpression(node.expression)) {
      const receiver = node.expression.expression
      const method = node.expression.name.text
      if (
        isLocalStorageReceiver(receiver) &&
        !relative.endsWith('lib/storage-ownership.ts')
      ) {
        findings.push(`${relative}: direct localStorage.${method}`)
      }
      if (isIndexedDBReceiver(receiver) && method === 'open') {
        const databaseName = node.arguments[0]
        if (
          !databaseName ||
          !ts.isCallExpression(databaseName) ||
          !ts.isIdentifier(databaseName.expression) ||
          databaseName.expression.text !== 'ownedDatabaseName'
        ) {
          findings.push(`${relative}: unregistered indexedDB.open`)
        }
      }
    }
    ts.forEachChild(node, visit)
  }
  visit(sourceFile)
  return findings
}

function persistentStorageBypasses(filename: string, sourceRoot: string): string[] {
  return persistentStorageBypassesInSource(
    readFileSync(filename, 'utf8'),
    path.relative(sourceRoot, filename),
  )
}

afterEach(() => {
  readerIdentity.clear()
  localStorage.clear()
})

describe('installation-owned local storage', () => {
  it('registers every production localStorage and IndexedDB key with its ownership', () => {
    expect(storageOwnershipRegistry).toEqual({
      annotationsV1: {
        storage: 'localStorage',
        key: 'webtag:annotations:v1',
        keyKind: 'exact',
        ownership: 'installation-user-data',
      },
      annotationsV2: {
        storage: 'localStorage',
        key: 'webtag:annotations:v2',
        keyKind: 'exact',
        ownership: 'installation-user-data',
      },
      annotationWakeup: {
        storage: 'localStorage',
        key: 'webtag:reader:annotations:wakeup:v1',
        keyKind: 'exact',
        ownership: 'installation-user-data',
      },
      thoughtDevice: {
        storage: 'localStorage',
        key: 'webtag:reader:thought-device:v1',
        keyKind: 'exact',
        ownership: 'installation-user-data',
      },
      pins: {
        storage: 'localStorage',
        key: 'webtag:pins:v1',
        keyKind: 'exact',
        ownership: 'installation-user-data',
      },
      revisionFloor: {
        storage: 'localStorage',
        key: 'webtag:content-revision-floor',
        keyKind: 'exact',
        ownership: 'installation-cache',
      },
      feedSelection: {
        storage: 'localStorage',
        key: 'webtag:reader:feed-selection:v1',
        keyKind: 'exact',
        ownership: 'installation-user-data',
      },
      collapsedFeedFolders: {
        storage: 'localStorage',
        key: 'webtag:reader:collapsed-feed-folders:v1',
        keyKind: 'exact',
        ownership: 'installation-user-data',
      },
      sidebarFold: {
        storage: 'localStorage',
        key: 'webtag:sbfold:',
        keyKind: 'prefix',
        ownership: 'installation-user-data',
      },
      theme: {
        storage: 'localStorage',
        key: 'webtag:theme',
        keyKind: 'exact',
        ownership: 'device',
      },
      readingPreference: {
        storage: 'localStorage',
        key: 'webtag:reading-preference',
        keyKind: 'exact',
        ownership: 'device',
      },
      sidebarCollapsed: {
        storage: 'localStorage',
        key: 'webtag:sidebar-collapsed',
        keyKind: 'exact',
        ownership: 'device',
      },
      connection: {
        storage: 'localStorage',
        key: 'webtag:reader:conn:v2',
        keyKind: 'exact',
        ownership: 'connection',
      },
      cacheDatabase: {
        storage: 'indexedDB',
        key: 'webtag-reader-cache',
        keyKind: 'database',
        ownership: 'installation-cache',
      },
      userDataDatabase: {
        storage: 'indexedDB',
        key: 'webtag-reader-user-data',
        keyKind: 'database',
        ownership: 'installation-user-data',
      },
    })

    const sourceRoot = path.resolve(process.cwd(), 'src')
    const bypasses = productionSources(sourceRoot).flatMap((filename) =>
      persistentStorageBypasses(filename, sourceRoot),
    )
    expect(bypasses).toEqual([])
  })

  it('detects qualified localStorage calls that bypass the ownership adapter', () => {
    const bypasses = persistentStorageBypassesInSource(
      [
        "window.localStorage.setItem('window-key', 'value')",
        "globalThis.localStorage.removeItem('global-key')",
        "self.localStorage.setItem('worker-key', 'value')",
      ].join('\n'),
      'components/UnsafeStorage.ts',
    )

    expect(bypasses).toEqual([
      'components/UnsafeStorage.ts: direct localStorage.setItem',
      'components/UnsafeStorage.ts: direct localStorage.removeItem',
      'components/UnsafeStorage.ts: direct localStorage.setItem',
    ])
  })

  it('detects qualified IndexedDB opens that bypass the ownership adapter', () => {
    const bypasses = persistentStorageBypassesInSource(
      [
        "window.indexedDB.open('window-db')",
        "globalThis.indexedDB.open('global-db')",
        "self.indexedDB.open('worker-db')",
      ].join('\n'),
      'lib/unsafe-database.ts',
    )

    expect(bypasses).toEqual([
      'lib/unsafe-database.ts: unregistered indexedDB.open',
      'lib/unsafe-database.ts: unregistered indexedDB.open',
      'lib/unsafe-database.ts: unregistered indexedDB.open',
    ])
  })

  it('keeps the same-origin A and B partitions isolated while preserving A', () => {
    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    writeOwnedStorage('pins', 'pins-from-A')

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(readOwnedStorage('pins')).toBeNull()
    writeOwnedStorage('pins', 'pins-from-B')

    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    expect(readOwnedStorage('pins')).toBe('pins-from-A')
  })

  it('keeps device preferences and connection settings global across identity changes', () => {
    readerIdentity.install({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
    })
    writeOwnedStorage('theme', 'dark')
    writeOwnedStorage('connection', 'connection-A')

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    expect(readOwnedStorage('theme')).toBe('dark')
    expect(readOwnedStorage('connection')).toBe('connection-A')
  })
})
