import { expect, test, type Page } from '@playwright/test'

const HARNESS_PATH = '/__test__/rf5b-annotations-harness'
const ANNOTATION_BARRIER_PATH = '/__test__/annotation-barrier'
const SAVED_DOCUMENT = {
  namespace: 'rf5b-browser-namespace',
  linkId: 'rf5b-browser-link',
  contentRevision: 7,
} as const

type HarnessCommitResult = Awaited<
  ReturnType<Window['rf5bAnnotationsHarness']['commit']>
>

interface BarrierRequestOptions {
  readonly name: string
  readonly participants: number
  readonly party?: string
  readonly timeoutMs?: number
}

interface BarrierResponse {
  readonly status: number
  readonly body: unknown
}

interface BarrierState {
  readonly participants: number
  readonly parties: readonly string[]
}

async function requestBarrier(
  page: Page,
  options: BarrierRequestOptions,
): Promise<BarrierResponse> {
  const query = new URLSearchParams({
    name: options.name,
    participants: String(options.participants),
  })
  if (options.party !== undefined) query.set('party', options.party)
  if (options.timeoutMs !== undefined) {
    query.set('timeoutMs', String(options.timeoutMs))
  }
  const response = await page.request.get(`${ANNOTATION_BARRIER_PATH}?${query}`)
  return { status: response.status(), body: await response.json() }
}

async function readBarrierState(
  page: Page,
  name: string,
): Promise<BarrierState | null> {
  const response = await page.request.get('/__test__/state')
  expect(response.ok()).toBe(true)
  const body = await response.json() as {
    annotationBarriers: Record<string, BarrierState>
  }
  return body.annotationBarriers[name] ?? null
}

async function resetServer(page: Page): Promise<void> {
  const response = await page.request.get('/__test__/reset')
  expect(response.ok()).toBe(true)
}

async function loadHarness(page: Page): Promise<void> {
  await page.goto(HARNESS_PATH)
  await page.waitForFunction(() => Boolean(window.rf5bAnnotationsHarness))
}

async function resetAndInstall(page: Page): Promise<void> {
  await loadHarness(page)
  await page.evaluate(async (savedDocument) => {
    await window.rf5bAnnotationsHarness.reset()
    window.rf5bAnnotationsHarness.install(savedDocument)
  }, SAVED_DOCUMENT)
}

async function install(page: Page): Promise<void> {
  await loadHarness(page)
  await page.evaluate((savedDocument) => {
    window.rf5bAnnotationsHarness.install(savedDocument)
  }, SAVED_DOCUMENT)
}

function committedSequence(result: HarnessCommitResult): number {
  if (!result.ok || result.value.status === 'op-id-conflict') {
    throw new Error(`annotation commit failed: ${JSON.stringify(result)}`)
  }
  expect(result.value.status).toBe('committed')
  return result.value.sequence
}

function browserLinks(prefix: string) {
  return Array.from({ length: 150 }, (_, index) => ({
    id: `${prefix}-${String(index + 1).padStart(3, '0')}`,
    createdAt: new Date(Date.UTC(2026, 0, 150 - index)).toISOString(),
  }))
}

test('annotation barrier requires a unique party for every participant', async ({ page }) => {
  await resetServer(page)
  const name = 'rf5b-barrier-unique-party'

  await expect(requestBarrier(page, { name, participants: 2 })).resolves.toEqual({
    status: 400,
    body: { error: 'invalid annotation barrier' },
  })

  const first = requestBarrier(page, {
    name,
    participants: 2,
    party: 'party-a',
    timeoutMs: 1_000,
  })
  await expect.poll(() => readBarrierState(page, name)).toEqual({
    participants: 2,
    parties: ['party-a'],
  })

  await expect(requestBarrier(page, {
    name,
    participants: 2,
    party: 'party-a',
    timeoutMs: 1_000,
  })).resolves.toEqual({
    status: 409,
    body: { error: 'annotation barrier party already arrived' },
  })
  await expect.poll(() => readBarrierState(page, name)).toEqual({
    participants: 2,
    parties: ['party-a'],
  })

  const second = requestBarrier(page, {
    name,
    participants: 2,
    party: 'party-b',
    timeoutMs: 1_000,
  })
  await expect(Promise.all([first, second])).resolves.toEqual([
    {
      status: 200,
      body: { name, participants: 2, party: 'party-a', arrival: 1 },
    },
    {
      status: 200,
      body: { name, participants: 2, party: 'party-b', arrival: 2 },
    },
  ])
  await expect.poll(() => readBarrierState(page, name)).toBeNull()
})

test('annotation barrier timeout and reset fail waiters and clean state for reuse', async ({
  page,
}) => {
  await resetServer(page)
  const name = 'rf5b-barrier-lifecycle'
  const timedOut = [
    requestBarrier(page, {
      name,
      participants: 3,
      party: 'timeout-a',
      timeoutMs: 750,
    }),
    requestBarrier(page, {
      name,
      participants: 3,
      party: 'timeout-b',
      timeoutMs: 750,
    }),
  ]
  await expect.poll(() => readBarrierState(page, name)).toEqual({
    participants: 3,
    parties: ['timeout-a', 'timeout-b'],
  })
  await expect(Promise.all(timedOut)).resolves.toEqual([
    { status: 504, body: { error: 'annotation barrier timed out' } },
    { status: 504, body: { error: 'annotation barrier timed out' } },
  ])
  await expect.poll(() => readBarrierState(page, name)).toBeNull()

  await expect(Promise.all([
    requestBarrier(page, {
      name,
      participants: 2,
      party: 'reuse-a',
      timeoutMs: 1_000,
    }),
    requestBarrier(page, {
      name,
      participants: 2,
      party: 'reuse-b',
      timeoutMs: 1_000,
    }),
  ])).resolves.toEqual([
    {
      status: 200,
      body: { name, participants: 2, party: 'reuse-a', arrival: 1 },
    },
    {
      status: 200,
      body: { name, participants: 2, party: 'reuse-b', arrival: 2 },
    },
  ])

  const resetWaiter = requestBarrier(page, {
    name,
    participants: 2,
    party: 'reset-a',
    timeoutMs: 1_000,
  })
  await expect.poll(() => readBarrierState(page, name)).toEqual({
    participants: 2,
    parties: ['reset-a'],
  })
  await resetServer(page)
  await expect(resetWaiter).resolves.toEqual({
    status: 503,
    body: { error: 'annotation barrier reset' },
  })
  await expect.poll(() => readBarrierState(page, name)).toBeNull()
})

test('two real pages preserve distinct writes started from the same durable projection', async ({
  context,
  page,
}) => {
  await resetAndInstall(page)
  const pageB = await context.newPage()
  await install(pageB)

  const initial = await Promise.all([
    page.evaluate(() => window.rf5bAnnotationsHarness.read()),
    pageB.evaluate(() => window.rf5bAnnotationsHarness.read()),
  ])
  expect(initial.map((snapshot) => snapshot.annotationStoreVersion)).toEqual([0, 0])
  expect(initial.map((snapshot) => snapshot.annotations)).toEqual([[], []])

  const commits = await Promise.all([
    page.evaluate(() => window.rf5bAnnotationsHarness.commitAfterBarrier(
      'rf5b-distinct-from-empty',
      2,
      {
        kind: 'add',
        opId: 'distinct-op-a',
        annotationId: 'distinct-a',
        note: 'from page A',
        stamp: 1,
      },
    )),
    pageB.evaluate(() => window.rf5bAnnotationsHarness.commitAfterBarrier(
      'rf5b-distinct-from-empty',
      2,
      {
        kind: 'add',
        opId: 'distinct-op-b',
        annotationId: 'distinct-b',
        note: 'from page B',
        stamp: 2,
      },
    )),
  ])
  const sequences = commits.map(committedSequence).sort((left, right) => left - right)
  expect(new Set(sequences).size).toBe(2)

  const finalSnapshots = await Promise.all([
    page.evaluate(() => window.rf5bAnnotationsHarness.read()),
    pageB.evaluate(() => window.rf5bAnnotationsHarness.read()),
  ])
  for (const snapshot of finalSnapshots) {
    expect(snapshot.annotationStoreVersion).toBe(sequences[1])
    expect(snapshot.annotations.map((annotation) => annotation.id).sort()).toEqual([
      'distinct-a',
      'distinct-b',
    ])
  }
})

test('two browser tabs create one local-first recovery candidate for one immutable event', async ({
  context,
  page,
}) => {
  await resetAndInstall(page)
  const pageB = await context.newPage()
  await install(pageB)
  await page.evaluate(() => window.rf5bAnnotationsHarness.seedSupersessionRecovery())

  const recoveries = await Promise.all([
    page.evaluate(() => window.rf5bAnnotationsHarness.recoverSupersession()),
    pageB.evaluate(() => window.rf5bAnnotationsHarness.recoverSupersession()),
  ])
  const statuses = recoveries.map((result) => result.ok ? result.value.status : 'failed').sort()
  expect(statuses).toEqual(['committed', 'duplicate'])

  const outbox = await page.evaluate(() => window.rf5bAnnotationsHarness.thoughtOutboxKeys())
  expect(outbox.filter((item) => item.opId === 'supersession-recovery:7')).toHaveLength(1)
})

test('same-ID add, update, and delete resolve by the returned global commit sequence', async ({
  context,
  page,
}) => {
  await resetAndInstall(page)
  const pageB = await context.newPage()
  const pageC = await context.newPage()
  await Promise.all([install(pageB), install(pageC)])

  const base = await page.evaluate(() => window.rf5bAnnotationsHarness.commit({
    kind: 'add',
    opId: 'same-id-base',
    annotationId: 'shared-id',
    note: 'base projection',
    stamp: 10,
  }))
  committedSequence(base)

  const staleProjections = await Promise.all([
    page.evaluate(() => window.rf5bAnnotationsHarness.read()),
    pageB.evaluate(() => window.rf5bAnnotationsHarness.read()),
    pageC.evaluate(() => window.rf5bAnnotationsHarness.read()),
  ])
  for (const snapshot of staleProjections) {
    expect(snapshot.annotations).toEqual([
      expect.objectContaining({ id: 'shared-id', note: 'base projection' }),
    ])
  }

  const contenders = [
    {
      kind: 'add' as const,
      result: page.evaluate(() => window.rf5bAnnotationsHarness.commitAfterBarrier(
        'rf5b-same-id-three-way',
        3,
        {
          kind: 'add',
          opId: 'same-id-add',
          annotationId: 'shared-id',
          note: 'add won',
          stamp: 20,
        },
      )),
      expectedNote: 'add won',
    },
    {
      kind: 'update' as const,
      result: pageB.evaluate(() => window.rf5bAnnotationsHarness.commitAfterBarrier(
        'rf5b-same-id-three-way',
        3,
        {
          kind: 'update',
          opId: 'same-id-update',
          annotationId: 'shared-id',
          note: 'update won',
          stamp: 30,
        },
      )),
      expectedNote: 'update won',
    },
    {
      kind: 'delete' as const,
      result: pageC.evaluate(() => window.rf5bAnnotationsHarness.commitAfterBarrier(
        'rf5b-same-id-three-way',
        3,
        {
          kind: 'delete',
          opId: 'same-id-delete',
          annotationId: 'shared-id',
        },
      )),
      expectedNote: null,
    },
  ]
  const settled = await Promise.all(contenders.map(async (contender) => ({
    ...contender,
    sequence: committedSequence(await contender.result),
  })))
  expect(new Set(settled.map((contender) => contender.sequence)).size).toBe(3)
  const winner = settled.reduce((left, right) =>
    left.sequence > right.sequence ? left : right)

  const finalSnapshot = await page.evaluate(() => window.rf5bAnnotationsHarness.read())
  expect(finalSnapshot.annotationStoreVersion).toBe(winner.sequence)
  if (winner.kind === 'delete') {
    expect(finalSnapshot.annotations).toEqual([])
  } else {
    expect(finalSnapshot.annotations).toEqual([
      expect.objectContaining({ id: 'shared-id', note: winner.expectedNote }),
    ])
  }

  const history = await page.evaluate(() => window.rf5bAnnotationsHarness.operationKinds())
  expect(history.at(-1)).toEqual({ kind: winner.kind, sequence: winner.sequence })
})

test('100 interleaved two-page rounds remain durable after both writers close', async ({
  context,
  page,
}) => {
  test.setTimeout(120_000)
  await resetAndInstall(page)
  const pageB = await context.newPage()
  await install(pageB)

  const runWriter = (writer: Page, side: 'a' | 'b') => writer.evaluate(
    async ({ rounds, sideName }) => {
      const sequences: number[] = []
      for (let round = 0; round < rounds; round += 1) {
        const result = await window.rf5bAnnotationsHarness.commitAfterBarrier(
          `rf5b-100-rounds-${round}`,
          2,
          {
            kind: 'add',
            opId: `round-${round}-${sideName}`,
            annotationId: `annotation-${round}-${sideName}`,
            note: `round ${round} from ${sideName}`,
            stamp: round * 2 + (sideName === 'a' ? 1 : 2),
          },
        )
        if (!result.ok || result.value.status === 'op-id-conflict') {
          throw new Error(`round ${round} commit failed`)
        }
        sequences.push(result.value.sequence)
      }
      return sequences
    },
    { rounds: 100, sideName: side },
  )

  const writerSequences = await Promise.all([
    runWriter(page, 'a'),
    runWriter(pageB, 'b'),
  ])
  const allSequences = writerSequences.flat()
  expect(allSequences).toHaveLength(200)
  expect(new Set(allSequences).size).toBe(200)

  await Promise.all([
    page.evaluate(() => window.rf5bAnnotationsHarness.close()),
    pageB.evaluate(() => window.rf5bAnnotationsHarness.close()),
  ])
  await Promise.all([page.close(), pageB.close()])

  const pageC = await context.newPage()
  await install(pageC)
  const reopened = await pageC.evaluate(async () => ({
    snapshot: await window.rf5bAnnotationsHarness.read(),
    annotatedLinkIds: await window.rf5bAnnotationsHarness.annotatedLinkIds(),
    thoughtKeys: await window.rf5bAnnotationsHarness.thoughtOutboxKeys(),
  }))
  expect(reopened.snapshot.annotations).toHaveLength(200)
  expect(reopened.snapshot.annotationStoreVersion).toBe(Math.max(...allSequences))
  expect(reopened.annotatedLinkIds).toEqual([SAVED_DOCUMENT.linkId])
  expect(new Set(reopened.snapshot.annotations.map((annotation) => annotation.id)).size).toBe(200)
  expect(new Set(reopened.thoughtKeys.map((key) => key.deviceId)).size).toBe(1)
  expect(reopened.thoughtKeys.map((key) => key.logicalClock).sort((left, right) => left - right))
    .toEqual(Array.from({ length: 200 }, (_, index) => index + 1))
})

test('a page that drops BroadcastChannel converges on visibilitychange from IndexedDB', async ({
  context,
  page,
}) => {
  await resetAndInstall(page)
  const pageB = await context.newPage()
  await install(pageB)
  await Promise.all([
    page.evaluate(() => window.rf5bAnnotationsHarness.startProjection()),
    pageB.evaluate(() => window.rf5bAnnotationsHarness.startProjection()),
  ])
  const stale = await pageB.evaluate(() => {
    window.rf5bAnnotationsHarness.dropHintChannel()
    return window.rf5bAnnotationsHarness.projection()
  })

  const result = await page.evaluate(() => window.rf5bAnnotationsHarness.commit({
    kind: 'add',
    opId: 'missed-broadcast-op',
    annotationId: 'missed-broadcast',
    note: 'durable despite missed hint',
    stamp: 1,
  }))
  const sequence = committedSequence(result)
  expect(await pageB.evaluate(() => window.rf5bAnnotationsHarness.projection())).toEqual(stale)

  const converged = await pageB.evaluate(
    () => window.rf5bAnnotationsHarness.triggerVisibilityRefresh(),
  )
  expect(converged.error).toBeNull()
  expect(converged.hintCount).toBe(0)
  expect(converged.refreshCount).toBe(stale.refreshCount + 1)
  expect(converged.annotationStoreVersion).toBe(sequence)
  expect(converged.annotations).toEqual([
    expect.objectContaining({ id: 'missed-broadcast' }),
  ])
})

test('native storage events wake another page when BroadcastChannel is unavailable', async ({
  context,
  page,
}) => {
  await resetAndInstall(page)
  const pageB = await context.newPage()
  await install(pageB)
  await Promise.all([
    page.evaluate(async () => {
      window.rf5bAnnotationsHarness.disableBroadcastChannel()
      await window.rf5bAnnotationsHarness.startProjection()
    }),
    pageB.evaluate(async () => {
      window.rf5bAnnotationsHarness.disableBroadcastChannel()
      await window.rf5bAnnotationsHarness.startProjection()
    }),
  ])

  const result = await page.evaluate(() => window.rf5bAnnotationsHarness.commit({
    kind: 'add',
    opId: 'storage-fallback-op',
    annotationId: 'storage-fallback',
    note: 'native storage fallback',
    stamp: 1,
  }))
  const sequence = committedSequence(result)

  await expect.poll(() => pageB.evaluate(() => {
    const projection = window.rf5bAnnotationsHarness.projection()
    return {
      version: projection.annotationStoreVersion,
      ids: projection.annotations.map((annotation) => annotation.id),
      hintCount: projection.hintCount,
      error: projection.error,
    }
  })).toEqual({
    version: sequence,
    ids: ['storage-fallback'],
    hintCount: 1,
    error: null,
  })

  const wakeup = await page.evaluate(() => window.rf5bAnnotationsHarness.wakeup())
  expect(wakeup.key).toContain(`:namespace:${SAVED_DOCUMENT.namespace}`)
  expect(wakeup.value).not.toBeNull()
  expect(JSON.parse(wakeup.value ?? '{}')).toMatchObject({
    kind: 'annotation-change',
    namespace: SAVED_DOCUMENT.namespace,
    linkId: SAVED_DOCUMENT.linkId,
    documentRevision: SAVED_DOCUMENT.contentRevision,
    annotationStoreVersion: sequence,
  })
})

test('the production annotated view finds only the oldest of 150 candidates and isolates namespaces', async ({
  page,
}) => {
  await resetAndInstall(page)
  const links = browserLinks('rf5b-candidate')
  const oldest = links.at(-1)!

  const seeded = await page.evaluate(
    (linkId) => window.rf5bAnnotationsHarness.seedAnnotatedLinkIds([linkId]),
    oldest.id,
  )
  expect(seeded).toMatchObject({ status: 'committed', examined: 1, applied: 1 })
  expect(seeded.lastSequence).toBeGreaterThan(0)
  expect(await page.evaluate(
    () => window.rf5bAnnotationsHarness.annotatedLinkIds(),
  )).toEqual([oldest.id])

  await page.evaluate((candidates) => {
    window.rf5bAnnotationsHarness.startAnnotatedLinksProjection(candidates)
  }, links)
  await expect.poll(() => page.evaluate(
    () => window.rf5bAnnotationsHarness.annotatedLinksProjection(),
  )).toMatchObject({ loading: false, error: null })

  const initial = await page.evaluate(
    () => window.rf5bAnnotationsHarness.annotatedLinksProjection(),
  )
  expect(initial.visibleLinkIds).toEqual([oldest.id])
  expect(initial.pointReadLinkIds).toEqual([oldest.id])
  expect(initial.activePointReads).toBe(0)
  expect(initial.maximumConcurrentPointReads).toBeLessThanOrEqual(6)
  expect(initial.activePhysicalNamespace).toBe(SAVED_DOCUMENT.namespace)
  expect(initial.clientOwnsActiveCacheIdentity).toBe(true)

  const isolatedDocument = {
    ...SAVED_DOCUMENT,
    namespace: 'rf5b-browser-isolated-namespace',
  }
  await page.evaluate(({ candidates, savedDocument }) => {
    window.rf5bAnnotationsHarness.install(savedDocument)
    window.rf5bAnnotationsHarness.startAnnotatedLinksProjection(candidates)
  }, { candidates: links, savedDocument: isolatedDocument })
  await expect.poll(() => page.evaluate(
    () => window.rf5bAnnotationsHarness.annotatedLinksProjection(),
  )).toMatchObject({ loading: false, error: null })

  const isolated = await page.evaluate(async () => ({
    projection: window.rf5bAnnotationsHarness.annotatedLinksProjection(),
    indexedLinkIds: await window.rf5bAnnotationsHarness.annotatedLinkIds(),
  }))
  expect(isolated.indexedLinkIds).toEqual([])
  expect(isolated.projection).toMatchObject({
    visibleLinkIds: [],
    pointReadLinkIds: [],
    activePointReads: 0,
    maximumConcurrentPointReads: 0,
    activePhysicalNamespace: isolatedDocument.namespace,
    clientOwnsActiveCacheIdentity: true,
  })

  await page.evaluate(({ candidates, savedDocument }) => {
    window.rf5bAnnotationsHarness.install(savedDocument)
    window.rf5bAnnotationsHarness.startAnnotatedLinksProjection(candidates)
  }, { candidates: links, savedDocument: SAVED_DOCUMENT })
  await expect.poll(() => page.evaluate(
    () => window.rf5bAnnotationsHarness.annotatedLinksProjection(),
  )).toMatchObject({ loading: false, error: null })

  const returned = await page.evaluate(async () => ({
    projection: window.rf5bAnnotationsHarness.annotatedLinksProjection(),
    indexedLinkIds: await window.rf5bAnnotationsHarness.annotatedLinkIds(),
  }))
  expect(returned.indexedLinkIds).toEqual([oldest.id])
  expect(returned.projection.visibleLinkIds).toEqual([oldest.id])
  expect(returned.projection.pointReadLinkIds).toEqual([oldest.id])
  expect(returned.projection.activePhysicalNamespace).toBe(SAVED_DOCUMENT.namespace)
  expect(returned.projection.clientOwnsActiveCacheIdentity).toBe(true)
})

test('the production annotated view reads all 150 indexed links with at most six point reads', async ({
  page,
}) => {
  await resetAndInstall(page)
  const links = browserLinks('rf5b-indexed')
  const linkIds = links.map((link) => link.id)

  const seeded = await page.evaluate(
    (ids) => window.rf5bAnnotationsHarness.seedAnnotatedLinkIds(ids),
    linkIds,
  )
  expect(seeded).toMatchObject({ status: 'committed', examined: 150, applied: 150 })
  expect(seeded.lastSequence).toBeGreaterThanOrEqual(150)
  expect(await page.evaluate(
    () => window.rf5bAnnotationsHarness.annotatedLinkIds(),
  )).toEqual([...linkIds].sort())

  await page.evaluate((candidates) => {
    window.rf5bAnnotationsHarness.startAnnotatedLinksProjection(candidates)
  }, links)
  await expect.poll(() => page.evaluate(
    () => window.rf5bAnnotationsHarness.annotatedLinksProjection(),
  ), { timeout: 10_000 }).toMatchObject({ loading: false, error: null })

  const projection = await page.evaluate(
    () => window.rf5bAnnotationsHarness.annotatedLinksProjection(),
  )
  expect(projection.visibleLinkIds).toEqual(linkIds)
  expect(projection.pointReadLinkIds).toHaveLength(150)
  expect(new Set(projection.pointReadLinkIds)).toEqual(new Set(linkIds))
  expect(projection.activePointReads).toBe(0)
  expect(projection.maximumConcurrentPointReads).toBe(6)
  expect(projection.maximumConcurrentPointReads).toBeLessThanOrEqual(6)
  expect(projection.activePhysicalNamespace).toBe(SAVED_DOCUMENT.namespace)
  expect(projection.clientOwnsActiveCacheIdentity).toBe(true)
})

test('clearing the evictable HTTP cache leaves annotation data and its index intact', async ({
  context,
  page,
}) => {
  await resetAndInstall(page)
  const result = await page.evaluate(() => window.rf5bAnnotationsHarness.commit({
    kind: 'add',
    opId: 'cache-isolation-op',
    annotationId: 'cache-isolation',
    note: 'user data survives cache clear',
    stamp: 1,
  }))
  const sequence = committedSequence(result)

  expect(await page.evaluate(
    () => window.rf5bAnnotationsHarness.seedCache('GET /api/links/rf5b-cache-probe'),
  )).toBe(true)
  expect(await page.evaluate(() => window.rf5bAnnotationsHarness.cacheRecordCount())).toBe(1)
  await page.evaluate(() => window.rf5bAnnotationsHarness.clearCache())
  expect(await page.evaluate(() => window.rf5bAnnotationsHarness.cacheRecordCount())).toBe(0)

  await page.evaluate(() => window.rf5bAnnotationsHarness.close())
  await page.close()
  const reopenedPage = await context.newPage()
  await install(reopenedPage)
  const durable = await reopenedPage.evaluate(async () => ({
    snapshot: await window.rf5bAnnotationsHarness.read(),
    annotatedLinkIds: await window.rf5bAnnotationsHarness.annotatedLinkIds(),
    cacheRecordCount: await window.rf5bAnnotationsHarness.cacheRecordCount(),
  }))
  expect(durable.cacheRecordCount).toBe(0)
  expect(durable.snapshot.annotationStoreVersion).toBe(sequence)
  expect(durable.snapshot.annotations).toEqual([
    expect.objectContaining({ id: 'cache-isolation' }),
  ])
  expect(durable.annotatedLinkIds).toEqual([SAVED_DOCUMENT.linkId])
})
