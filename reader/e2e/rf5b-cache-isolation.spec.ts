import { expect, test, type Page } from '@playwright/test'

import { CACHE_SCHEMA_VERSION } from '../src/lib/cache/idb'
import {
  ANNOTATED_LINKS_STORE,
  ANNOTATION_IMPORTS_STORE,
  ANNOTATION_LINK_STATE_STORE,
  ANNOTATION_MATERIALIZED_STORE,
  ANNOTATION_OPS_STORE,
  LEGACY_ARCHIVE_STORE,
  LEGACY_DEDUP_STORE,
  LEGACY_PENDING_STORE,
  MIGRATION_DECISION_STORE,
  THOUGHT_HISTORY_OUTBOX_STORE,
  THOUGHT_MATERIALIZED_STORE,
  THOUGHT_OUTBOX_STORE,
  THOUGHT_REPAIR_ACK_STORE,
  THOUGHT_REPAIR_MANIFEST_STORE,
  THOUGHT_REPAIR_QUARANTINE_STORE,
  THOUGHT_REPAIR_READY_STORE,
  THOUGHT_REPAIR_SOURCE_STORE,
  THOUGHT_SUPERSESSION_EVENTS_STORE,
  THOUGHT_SUPERSESSION_STATE_STORE,
  THOUGHT_SYNC_STATE_STORE,
  USER_DATA_DATABASE_VERSION,
} from '../src/lib/user-data/idb'

const HARNESS_PATH = '/__test__/storage-harness'
const NAMESPACE = 'rf5b-cache-isolation'
const LINK_ID = 'durable-user-data-link'
const TARGET = {
  kind: 'saved-content',
  contentRevision: 7,
} as const
const TARGET_KEY = 'saved-content:7'
const ANNOTATION = {
  id: 'durable-annotation',
  blockKey: 'content-document',
  start: 0,
  end: 7,
  text: 'durable',
  note: 'must survive cache maintenance',
  source: 'self',
  createdAt: 100,
  updatedAt: 100,
  sourceContentRevision: 7,
} as const

type UserDataSnapshot = Awaited<
  ReturnType<Window['rf2bStorageHarness']['readCacheIsolationUserData']>
>

async function loadHarness(page: Page): Promise<void> {
  await page.goto(HARNESS_PATH)
  await page.waitForFunction(() => Boolean(window.rf2bStorageHarness))
}

function persistedKey(logicalKey: string): string {
  return `${NAMESPACE.length}:${NAMESPACE}:${logicalKey}`
}

function expectUserDataIntact(snapshot: UserDataSnapshot): void {
  expect(snapshot).toEqual({
    schema: {
      version: USER_DATA_DATABASE_VERSION,
      stores: [
        LEGACY_PENDING_STORE,
        LEGACY_ARCHIVE_STORE,
        LEGACY_DEDUP_STORE,
        MIGRATION_DECISION_STORE,
        ANNOTATION_OPS_STORE,
        ANNOTATION_MATERIALIZED_STORE,
        ANNOTATION_LINK_STATE_STORE,
        ANNOTATED_LINKS_STORE,
        ANNOTATION_IMPORTS_STORE,
        THOUGHT_OUTBOX_STORE,
        THOUGHT_HISTORY_OUTBOX_STORE,
        THOUGHT_SYNC_STATE_STORE,
        THOUGHT_MATERIALIZED_STORE,
        // v6 的可恢复 thought repair（#79）新增的五个 store。这份清单刻意写死而不是
        // 从 schema 反查——反查会让断言恒真，schema 悄悄多一个 store 也照样通过。
        // 加 store 必须同步改这里，正是这道断言的用途。
        THOUGHT_REPAIR_READY_STORE,
        THOUGHT_REPAIR_QUARANTINE_STORE,
        THOUGHT_REPAIR_MANIFEST_STORE,
        THOUGHT_REPAIR_SOURCE_STORE,
        THOUGHT_REPAIR_ACK_STORE,
        THOUGHT_SUPERSESSION_EVENTS_STORE,
        THOUGHT_SUPERSESSION_STATE_STORE,
      ].sort(),
    },
    annotationSnapshot: {
      ok: true,
      value: {
        namespace: NAMESPACE,
        linkId: LINK_ID,
        target: TARGET,
        annotationStoreVersion: 1,
        annotations: [ANNOTATION],
      },
    },
    annotatedLinks: {
      ok: true,
      value: [{
        key: [NAMESPACE, LINK_ID, TARGET_KEY],
        namespace: NAMESPACE,
        linkId: LINK_ID,
        target: TARGET,
        targetKey: TARGET_KEY,
        annotationCount: 1,
        annotationStoreVersion: 1,
      }],
    },
    annotatedLinkIds: {
      ok: true,
      value: [LINK_ID],
    },
    operations: [{
      sequence: 1,
      opId: `${NAMESPACE.length}:${NAMESPACE}:durable-operation`,
      logicalOpId: 'durable-operation',
      namespace: NAMESPACE,
      linkId: LINK_ID,
      targetKey: TARGET_KEY,
      annotationId: ANNOTATION.id,
      kind: 'add',
    }],
    indexRecords: [{
      key: [NAMESPACE, LINK_ID, TARGET_KEY],
      namespace: NAMESPACE,
      linkId: LINK_ID,
      targetKey: TARGET_KEY,
      annotationCount: 1,
      annotationStoreVersion: 1,
    }],
    legacyPending: {
      id: 'pins',
      legacyKey: 'webtag:pins:v1',
      value: '{"tags":["quarantined"],"domains":[]}',
      quarantinedAt: 101,
    },
    legacyArchive: [{
      archiveID: 1,
      id: 'annotationsV1',
      legacyKey: 'webtag:annotations:v1',
      value: '{"archived-link":[{"id":"archived"}]}',
      quarantinedAt: 102,
      importedIntoNamespace: NAMESPACE,
      importedAt: 103,
      fingerprintVersion: 1,
      fingerprints: ['archive-fingerprint'],
    }],
  })
}

const rolledBackCache = {
  version: 2,
  stores: ['cache_control', 'resources'],
  records: [{
    key: persistedKey('GET /api/links?legacy-cache'),
    namespace: NAMESPACE,
    logicalKey: 'GET /api/links?legacy-cache',
    schema: CACHE_SCHEMA_VERSION,
    data: { cache: 'legacy' },
    updatedAt: 1,
    size: 10,
    generation: 0,
  }],
} as const

const quotaCache = {
  version: 3,
  stores: ['cache_control', 'cache_meta', 'cache_payload', 'resources'],
  records: [{
    key: persistedKey('GET /api/links?incoming-cache'),
    namespace: NAMESPACE,
    logicalKey: 'GET /api/links?incoming-cache',
    schema: CACHE_SCHEMA_VERSION,
    data: { cache: 'incoming' },
    updatedAt: 2,
    size: 60,
    generation: 0,
  }],
} as const

test('a real cache upgrade abort rolls back without touching durable user data', async ({
  context,
  page,
}) => {
  await loadHarness(page)
  const seeded = await page.evaluate(async () => {
    await window.rf2bStorageHarness.resetCacheIsolationDatabases()
    return window.rf2bStorageHarness.seedCacheIsolationUserData()
  })
  expectUserDataIntact(seeded)

  const failedUpgrade = await page.evaluate(() =>
    window.rf2bStorageHarness.runCacheIsolationUpgradeFailure())
  expect(failedUpgrade).toEqual({
    injectedFailures: 1,
    attemptedRecords: [],
    prototypeRestored: true,
    rolledBackCache,
  })

  await page.close()
  const reopened = await context.newPage()
  await loadHarness(reopened)
  expectUserDataIntact(await reopened.evaluate(() =>
    window.rf2bStorageHarness.readCacheIsolationUserData()))
  expect(await reopened.evaluate(() =>
    window.rf2bStorageHarness.readCacheIsolationCache())).toEqual(rolledBackCache)
})

test('real quota eviction leaves annotation and recovery stores intact after reopening', async ({
  context,
  page,
}) => {
  await loadHarness(page)
  const seeded = await page.evaluate(async () => {
    await window.rf2bStorageHarness.resetCacheIsolationDatabases()
    return window.rf2bStorageHarness.seedCacheIsolationUserData()
  })
  expectUserDataIntact(seeded)

  const eviction = await page.evaluate(() =>
    window.rf2bStorageHarness.runCacheIsolationQuotaEviction())
  expect(eviction).toEqual({
    oldStored: true,
    admission: { stored: true, mutated: true, totalBytes: 60 },
    cache: quotaCache,
  })

  await page.close()
  const reopened = await context.newPage()
  await loadHarness(reopened)
  expectUserDataIntact(await reopened.evaluate(() =>
    window.rf2bStorageHarness.readCacheIsolationUserData()))
  expect(await reopened.evaluate(() =>
    window.rf2bStorageHarness.readCacheIsolationCache())).toEqual(quotaCache)
})
