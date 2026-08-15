import { describe, expect, it } from 'vitest'
import { makeLink } from '../../test/fixtures'
import {
  ArchiveV2ValidationError,
  archiveV2Sections,
  fullArchiveV2Selection,
  type ArchiveV2Selection,
  validateArchiveV2Bytes,
} from './archive-v2'

const NAMESPACE = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
const encoder = new TextEncoder()
const decoder = new TextDecoder()
const AT = '2026-08-11T00:00:00Z'
const LINK_ID = '11111111-1111-4111-8111-111111111111'
const SITE_ID = '22222222-2222-4222-8222-222222222222'
const ENTRY_ID = '33333333-3333-4333-8333-333333333333'
const CATEGORY_ID = '44444444-4444-4444-8444-444444444444'
const NOTE_ID = '55555555-5555-4555-8555-555555555555'
const INBOX_ID = '66666666-6666-4666-8666-666666666666'
const TODO_ID = '77777777-7777-4777-8777-777777777777'
const SNAPSHOT_ID = '88888888-8888-4888-8888-888888888888'
const RULE_ID = '99999999-9999-4999-8999-999999999999'
const FEED_FOLDER_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
const FEED_SUBSCRIPTION_ID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'
const FEED_ITEM_ID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc'

const TOP_LEVEL_SECTIONS = [
  'links',
  'sites',
  'site_entries',
  'site_tags',
  'site_identities',
  'classification_rules',
] as const

const READER_BASE_SECTIONS = [
  'feed_folders',
  'feed_subscriptions',
  'feed_items',
  'feed_saves',
  'inbox',
  'categories',
  'categorizables',
  'todos',
  'engagement',
  'feed_feedback',
  'feed_snapshots',
  'tag_activity',
  'domain_activity',
  'content_history',
] as const

const READER_THOUGHT_SECTIONS = [
  'thoughts',
  'thought_ops',
  'thought_supersession_events',
  'thought_tombstones',
] as const

const READER_NOTE_SECTIONS = ['notes', 'note_history'] as const

type ReaderSection =
  | (typeof READER_BASE_SECTIONS)[number]
  | (typeof READER_THOUGHT_SECTIONS)[number]
  | (typeof READER_NOTE_SECTIONS)[number]

type ArchiveFixture = Record<string, unknown> & { reader: Record<string, unknown> }
type ManifestFixture = {
  client_data_namespace: string
  sections: string[]
  counts: Record<string, number>
  checksum_sha256: string
}

function readerSections(selection: ArchiveV2Selection): ReaderSection[] {
  const sections: ReaderSection[] = [...READER_BASE_SECTIONS]
  if (selection.includeThoughts === true) sections.push(...READER_THOUGHT_SECTIONS)
  if (selection.includeNotes === true) sections.push(...READER_NOTE_SECTIONS)
  return sections
}

async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const input = new Uint8Array(bytes.byteLength)
  input.set(bytes)
  const digest = await globalThis.crypto.subtle.digest('SHA-256', input)
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')
}

function topLevelRow(section: (typeof TOP_LEVEL_SECTIONS)[number]): Record<string, unknown> {
  switch (section) {
    case 'links':
      return makeLink({ id: LINK_ID })
    case 'sites':
      return {
        id: SITE_ID,
        site_key: 'example.com',
        name: 'Example',
        name_source: 'auto',
        intro: 'A site',
        intro_source: 'auto',
        homepage_url: 'https://example.com',
        homepage_source: 'auto',
        icon_url: null,
        icon_source: null,
        user_note: '',
        pinned: false,
        primary_entry_id: ENTRY_ID,
        primary_source: 'auto',
        grouping_locked: false,
        needs_review: false,
        revision: 1,
        first_collected_at: AT,
        last_collected_at: AT,
        created_at: AT,
        updated_at: AT,
      }
    case 'site_entries':
      return {
        id: ENTRY_ID,
        site_id: SITE_ID,
        link_id: LINK_ID,
        entry_name: 'Home',
        entry_name_source: 'auto',
        purpose: '',
        purpose_source: 'auto',
        normalized_url: 'https://example.com/',
        first_collected_at: AT,
        last_recollected_at: null,
        created_at: AT,
        updated_at: AT,
      }
    case 'site_tags':
      return {
        site_id: SITE_ID,
        tag: 'web',
        normalized_tag: 'web',
        source: 'auto',
        concept_id: null,
        created_at: AT,
        updated_at: AT,
      }
    case 'site_identities':
      return {
        identity_key: 'host:example.com',
        site_id: SITE_ID,
        source: 'auto',
        locked: false,
        created_at: AT,
        updated_at: AT,
      }
    case 'classification_rules':
      return {
        id: RULE_ID,
        host: 'example.com',
        identity_adapter: null,
        path_prefix: null,
        target_kind: 'site',
        enabled: true,
        revision: 1,
        created_at: AT,
        updated_at: AT,
      }
  }
}

function readerRow(section: ReaderSection): Record<string, unknown> {
  switch (section) {
    case 'feed_folders':
      return { id: FEED_FOLDER_ID, name: 'Research', created_at: AT, updated_at: AT }
    case 'feed_subscriptions':
      return {
        id: FEED_SUBSCRIPTION_ID,
        folder_id: FEED_FOLDER_ID,
        url: 'https://example.com/feed.xml',
        canonical_url: 'https://example.com/feed.xml',
        site_url: 'https://example.com/',
        title: 'Example Feed',
        description: 'Feed description',
        active: false,
        created_at: AT,
        updated_at: AT,
      }
    case 'feed_items':
      return {
        id: FEED_ITEM_ID,
        subscription_id: FEED_SUBSCRIPTION_ID,
        external_id: 'entry-1',
        url: 'https://example.com/posts/one',
        title: 'Entry one',
        author: 'Author',
        summary: 'Summary',
        content_text: 'Complete body',
        content_html: '<p>Complete body</p>',
        published_at: AT,
        read_at: AT,
        starred: true,
        read_later: true,
        link_id: LINK_ID,
        created_at: AT,
        updated_at: AT,
      }
    case 'feed_saves':
      return { feed_item_id: FEED_ITEM_ID, link_id: LINK_ID, created_link: true, created_at: AT }
    case 'thoughts':
      return {
        contract_version: 1,
        id: 'thought-1',
        host_kind: 'link',
        host_id: LINK_ID,
        link_id: LINK_ID,
        target: { kind: 'saved-content', content_revision: 1 },
        quote: null,
        body: 'A private thought',
        source: 'user',
        deleted: false,
        last_sequence: 1,
        winner_key: { logical_clock: 7, device_id: 'device-1', op_id: 'thought-op-1' },
        created_at: AT,
        updated_at: AT,
      }
    case 'thought_ops':
      return {
        contract_version: 1,
        sequence: 1,
        op_id: 'thought-op-1',
        device_id: 'device-1',
        logical_clock: 7,
        operation_kind: 'add',
        annotation_id: 'thought-1',
        host_kind: 'link',
        host_id: LINK_ID,
        target: { kind: 'saved-content', content_revision: 1 },
        payload: { body: 'A private thought' },
        recovery_of: null,
        expected_current_winner_key: null,
        created_at: AT,
      }
    case 'thought_supersession_events':
      return {
        sequence: 1,
        annotation_id: 'thought-1',
        loser: { body: 'Superseded thought' },
        winner_at_detection: { body: 'A private thought' },
        created_at: AT,
      }
    case 'thought_tombstones':
      return {
        thought_id: 'thought-1',
        host_kind: 'link',
        host_id: LINK_ID,
        reason: 'link_deleted',
        snapshot: { body: 'A private thought' },
        created_at: AT,
      }
    case 'notes':
      return {
        id: NOTE_ID,
        title: 'Private note',
        published_content: 'Published body',
        published_revision: 1,
        draft_content: null,
        draft_revision: 1,
        draft_updated_at: null,
        deleted_at: null,
        created_at: AT,
        updated_at: AT,
      }
    case 'note_history':
      return {
        id: 1,
        note_id: NOTE_ID,
        revision: 1,
        title: 'Private note',
        content: 'Historic body',
        reanchor_ops: [],
        created_at: AT,
      }
    case 'inbox':
      return {
        id: INBOX_ID,
        url: 'https://example.com/inbox',
        source_kind: 'url',
        title: null,
        body: '',
        summary: null,
        suggested_tags: [],
        tags: [],
        status: 'pending',
        metadata_revision: 1,
        job_id: null,
        expires_at: null,
        expired_at: null,
        deleted_at: null,
        created_at: AT,
        updated_at: AT,
      }
    case 'categories':
      return { id: CATEGORY_ID, name: 'Ideas', created_at: AT }
    case 'categorizables':
      return { category_id: CATEGORY_ID, host_kind: 'link', host_id: LINK_ID }
    case 'todos':
      return {
        id: TODO_ID,
        text: 'Review export',
        due_at: null,
        done: false,
        origin_kind: 'standalone',
        origin_host_kind: null,
        origin_host_id: null,
        origin_ref: null,
        host_revision: 0,
        completed_at: null,
        deleted_at: null,
        created_at: AT,
        updated_at: AT,
      }
    case 'engagement':
      return {
        link_id: LINK_ID,
        read: false,
        progress: 0.5,
        read_later: false,
        last_opened: null,
        updated_at: AT,
      }
    case 'feed_feedback':
      return { item_key: 'feed:item', action: 'save', created_at: AT }
    case 'feed_snapshots':
      return { id: SNAPSHOT_ID, mode: 'recommended', items: [], created_at: AT }
    case 'tag_activity':
      return { tag: 'web', last_at: AT, last_link_id: LINK_ID }
    case 'domain_activity':
      return { domain: 'example.com', last_at: AT, last_link_id: LINK_ID }
    case 'content_history':
      return {
        id: 1,
        link_id: LINK_ID,
        revision: 1,
        content: null,
        content_document: null,
        content_format: 'plain',
        content_source: 'fetched',
        created_at: AT,
      }
  }
}

async function makeArchiveBytes(
  selection: ArchiveV2Selection = fullArchiveV2Selection,
  options: {
    omitReader?: boolean
    mutateArchive?: (archive: ArchiveFixture) => void
    mutateManifest?: (manifest: ManifestFixture) => void
  } = {},
): Promise<Uint8Array> {
  const reader: Record<string, unknown> = {
    schema_version: 2,
    thought_contract_version: 1,
  }
  for (const section of readerSections(selection)) reader[section] = [readerRow(section)]

  const archive: ArchiveFixture = {
    schema_version: 2,
    exported_at: AT,
    generator_version: 'webtag',
    links: [topLevelRow('links')],
    sites: [topLevelRow('sites')],
    site_entries: [topLevelRow('site_entries')],
    site_tags: [topLevelRow('site_tags')],
    site_identities: [topLevelRow('site_identities')],
    classification_rules: [topLevelRow('classification_rules')],
    reader,
  }
  if (options.omitReader) delete (archive as Record<string, unknown>).reader
  options.mutateArchive?.(archive)

  const prefix = JSON.stringify(archive).slice(0, -1)
  const counts: Record<string, number> = {}
  for (const section of TOP_LEVEL_SECTIONS) {
    counts[section] = (archive[section] as unknown[]).length
  }
  if (!options.omitReader) {
    for (const section of readerSections(selection)) {
      counts[`reader.${section}`] = (reader[section] as unknown[]).length
    }
  }
  const manifest: ManifestFixture = {
    client_data_namespace: NAMESPACE,
    sections: archiveV2Sections(selection).split(','),
    counts,
    checksum_sha256: await sha256Hex(encoder.encode(prefix)),
  }
  options.mutateManifest?.(manifest)
  return encoder.encode(`${prefix},"manifest":${JSON.stringify(manifest)}}`)
}

function validationOptions(selection: ArchiveV2Selection = fullArchiveV2Selection) {
  return { clientDataNamespace: NAMESPACE, selection }
}

function archivePrefixAndManifest(bytes: Uint8Array): { prefix: string; manifest: ManifestFixture } {
  const raw = decoder.decode(bytes)
  const marker = raw.lastIndexOf(',"manifest":')
  if (marker < 0) throw new Error('test fixture is missing manifest')
  return {
    prefix: raw.slice(0, marker),
    manifest: JSON.parse(raw.slice(marker + ',"manifest":'.length, -1)) as ManifestFixture,
  }
}

describe('archiveV2Sections', () => {
  it.each([
    [{}, 'base'],
    [{ includeThoughts: true }, 'base,thoughts'],
    [{ includeNotes: true }, 'base,notes'],
    [fullArchiveV2Selection, 'base,thoughts,notes'],
  ] as const)('canonicalizes %o as %s', (selection, expected) => {
    expect(archiveV2Sections(selection)).toBe(expected)
  })

  it('rejects a non-boolean or unknown selector field rather than sending a near miss', () => {
    expect(() => archiveV2Sections({ includeThoughts: 'yes' } as unknown as ArchiveV2Selection)).toThrow(
      ArchiveV2ValidationError,
    )
    expect(() => archiveV2Sections({ includeNotes: true, extra: true } as unknown as ArchiveV2Selection)).toThrow(
      ArchiveV2ValidationError,
    )
  })
})

describe('validateArchiveV2Bytes', () => {
  it.each([
    {},
    { includeThoughts: true },
    { includeNotes: true },
    fullArchiveV2Selection,
  ] as const)('accepts every selected concrete export projection for %o', async (selection) => {
    const bytes = await makeArchiveBytes(selection)
    await expect(validateArchiveV2Bytes(bytes, validationOptions(selection))).resolves.toBeUndefined()
  })

  it('accepts production Thought TODO origins and rejects inconsistent Todo host fields', async () => {
    const validThoughtTodo = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateArchive: (archive) => {
        const todo = (archive.reader.todos as Record<string, unknown>[])[0]!
        todo.origin_kind = 'thought'
        todo.origin_host_kind = 'thought'
        todo.origin_host_id = 'thought-1'
      },
    })
    await expect(validateArchiveV2Bytes(validThoughtTodo, validationOptions())).resolves.toBeUndefined()

    const malformedThoughtTodo = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateArchive: (archive) => {
        const todo = (archive.reader.todos as Record<string, unknown>[])[0]!
        todo.origin_kind = 'thought'
        todo.origin_host_kind = 'thought'
        todo.origin_host_id = ' thought-1'
      },
    })
    const mismatchedTodoHost = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateArchive: (archive) => {
        const todo = (archive.reader.todos as Record<string, unknown>[])[0]!
        todo.origin_kind = 'thought'
        todo.origin_host_kind = 'note'
        todo.origin_host_id = NOTE_ID
      },
    })
    for (const bytes of [malformedThoughtTodo, mismatchedTodoHost]) {
      await expect(validateArchiveV2Bytes(bytes, validationOptions())).rejects.toBeInstanceOf(
        ArchiveV2ValidationError,
      )
    }
  })

  it('accepts a 129-byte Thought ID and persisted non-enum source from the server', async () => {
    const thoughtID = 'a'.repeat(129)
    const bytes = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateArchive: (archive) => {
        const thought = (archive.reader.thoughts as Record<string, unknown>[])[0]!
        const operation = (archive.reader.thought_ops as Record<string, unknown>[])[0]!
        thought.id = thoughtID
        thought.source = 'imported-reader-v0'
        operation.annotation_id = thoughtID
      },
    })
    await expect(validateArchiveV2Bytes(bytes, validationOptions())).resolves.toBeUndefined()
  })

  it('accepts a server-valid delete with a non-UUID host and a persisted tombstone host', async () => {
    const hostID = 'purged-inbox:legacy-42'
    const bytes = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateArchive: (archive) => {
        const thought = (archive.reader.thoughts as Record<string, unknown>[])[0]!
        const operation = (archive.reader.thought_ops as Record<string, unknown>[])[0]!
        const tombstone = (archive.reader.thought_tombstones as Record<string, unknown>[])[0]!
        thought.id = 'deleted-thought'
        thought.host_kind = 'inbox'
        thought.host_id = hostID
        thought.link_id = null
        thought.deleted = true
        operation.operation_kind = 'delete'
        operation.annotation_id = 'deleted-thought'
        operation.host_kind = 'inbox'
        operation.host_id = hostID
        operation.payload = {}
        tombstone.host_kind = 'inbox'
        tombstone.host_id = hostID
      },
    })
    await expect(validateArchiveV2Bytes(bytes, validationOptions())).resolves.toBeUndefined()
  })

  it('accepts the base-only compatibility archive when the optional Reader exporter is absent', async () => {
    const bytes = await makeArchiveBytes({}, { omitReader: true })
    await expect(validateArchiveV2Bytes(bytes, validationOptions({}))).resolves.toBeUndefined()
  })

  it('requires Reader data whenever a selected privacy group depends on it', async () => {
    const bytes = await makeArchiveBytes({ includeThoughts: true }, { omitReader: true })
    await expect(validateArchiveV2Bytes(bytes, validationOptions({ includeThoughts: true }))).rejects.toBeInstanceOf(
      ArchiveV2ValidationError,
    )
  })

  it('keeps Feed base rows in every privacy selection and rejects dangling relationships', async () => {
    for (const selection of [
      {},
      { includeThoughts: true },
      { includeNotes: true },
      fullArchiveV2Selection,
    ]) {
      await expect(validateArchiveV2Bytes(
        await makeArchiveBytes(selection),
        validationOptions(selection),
      )).resolves.toBeUndefined()
    }

    const danglingID = 'dddddddd-dddd-4ddd-8ddd-dddddddddddd'
    const mutations: Array<(archive: ArchiveFixture) => void> = [
      (archive) => { (archive.reader.feed_subscriptions as Record<string, unknown>[])[0]!.folder_id = danglingID },
      (archive) => { (archive.reader.feed_items as Record<string, unknown>[])[0]!.subscription_id = danglingID },
      (archive) => { (archive.reader.feed_items as Record<string, unknown>[])[0]!.link_id = danglingID },
      (archive) => { (archive.reader.feed_saves as Record<string, unknown>[])[0]!.feed_item_id = danglingID },
      (archive) => { (archive.reader.feed_saves as Record<string, unknown>[])[0]!.link_id = danglingID },
    ]
    for (const mutateArchive of mutations) {
      const bytes = await makeArchiveBytes(fullArchiveV2Selection, { mutateArchive })
      await expect(validateArchiveV2Bytes(bytes, validationOptions())).rejects.toBeInstanceOf(
        ArchiveV2ValidationError,
      )
    }
  })

  it('accepts a raw-byte checksum boundary with whitespace and an escaped sole manifest key', async () => {
    const { prefix, manifest } = archivePrefixAndManifest(await makeArchiveBytes())
    const prefixWithWhitespace = `${prefix} \n\t`
    manifest.checksum_sha256 = await sha256Hex(encoder.encode(prefixWithWhitespace))
    const raw = `${prefixWithWhitespace}, \r\n "mani\\u0066est" \t : ${JSON.stringify(manifest)}}`
    await expect(validateArchiveV2Bytes(encoder.encode(raw), validationOptions())).resolves.toBeUndefined()
  })

  it('rejects invalid metadata and an invalid identity lease namespace', async () => {
    const invalidExportedAt = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateArchive: (archive) => { archive.exported_at = '2026-02-30T25:61:61Z' },
    })
    const wrongGenerator = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateArchive: (archive) => { archive.generator_version = 'WebTag build' },
    })
    const malformedManifestNamespace = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateManifest: (manifest) => { manifest.client_data_namespace = 'not-a-namespace' },
    })
    for (const bytes of [invalidExportedAt, wrongGenerator, malformedManifestNamespace]) {
      await expect(validateArchiveV2Bytes(bytes, validationOptions())).rejects.toBeInstanceOf(ArchiveV2ValidationError)
    }
    await expect(validateArchiveV2Bytes(await makeArchiveBytes(), {
      clientDataNamespace: 'invalid lease namespace',
    })).rejects.toBeInstanceOf(ArchiveV2ValidationError)
  })

  it('rejects malformed bytes, incomplete JSON, and trailing garbage', async () => {
    const valid = await makeArchiveBytes()
    const cases = [
      encoder.encode('<!doctype html><html></html>'),
      new Uint8Array(),
      valid.slice(0, -1),
      new Uint8Array([0xc3, 0x28]),
      appendBytes(valid, encoder.encode('unexpected')),
    ]
    for (const bytes of cases) {
      await expect(validateArchiveV2Bytes(bytes, validationOptions())).rejects.toBeInstanceOf(
        ArchiveV2ValidationError,
      )
    }
  })

  it('rejects every partial exported row rather than accepting an opaque object', async () => {
    const cases: Array<{ name: string; mutate: (archive: ArchiveFixture) => void }> = [
      ...TOP_LEVEL_SECTIONS.map((section) => ({
        name: section,
        mutate: (archive: ArchiveFixture) => { archive[section] = [{}] },
      })),
      ...readerSections(fullArchiveV2Selection).map((section) => ({
        name: `reader.${section}`,
        mutate: (archive: ArchiveFixture) => { archive.reader[section] = [{}] },
      })),
    ]
    for (const test of cases) {
      const bytes = await makeArchiveBytes(fullArchiveV2Selection, { mutateArchive: test.mutate })
      await expect(validateArchiveV2Bytes(bytes, validationOptions()), test.name).rejects.toBeInstanceOf(
        ArchiveV2ValidationError,
      )
    }
  })

  it('rejects malformed Thought IDs, device IDs, and operation IDs using the production validator', async () => {
    const cases: Array<{ name: string; mutate: (archive: ArchiveFixture) => void }> = [
      {
        name: 'whitespace thought id',
        mutate: (archive) => { (archive.reader.thoughts as Record<string, unknown>[])[0]!.id = ' thought-1' },
      },
      {
        name: 'nul thought id',
        mutate: (archive) => { (archive.reader.thoughts as Record<string, unknown>[])[0]!.id = 'thought\0one' },
      },
      {
        name: 'unpaired surrogate thought id',
        mutate: (archive) => { (archive.reader.thoughts as Record<string, unknown>[])[0]!.id = '\ud800' },
      },
      {
        name: 'overlong device id',
        mutate: (archive) => {
          const thought = (archive.reader.thoughts as Record<string, unknown>[])[0]!
          ;(thought.winner_key as Record<string, unknown>).device_id = 'd'.repeat(129)
        },
      },
      {
        name: 'whitespace operation id',
        mutate: (archive) => { (archive.reader.thought_ops as Record<string, unknown>[])[0]!.op_id = ' op-1' },
      },
      {
        name: 'overlong annotation id',
        mutate: (archive) => { (archive.reader.thought_ops as Record<string, unknown>[])[0]!.annotation_id = 'a'.repeat(257) },
      },
      {
        name: 'overlong thought id',
        mutate: (archive) => { (archive.reader.thoughts as Record<string, unknown>[])[0]!.id = 'a'.repeat(257) },
      },
      {
        name: 'overlong operation id',
        mutate: (archive) => { (archive.reader.thought_ops as Record<string, unknown>[])[0]!.op_id = 'o'.repeat(129) },
      },
    ]
    for (const test of cases) {
      const bytes = await makeArchiveBytes(fullArchiveV2Selection, { mutateArchive: test.mutate })
      await expect(validateArchiveV2Bytes(bytes, validationOptions()), test.name).rejects.toBeInstanceOf(
        ArchiveV2ValidationError,
      )
    }
  })

  it('rejects duplicate, escaped, whitespace-separated, and non-final top-level manifest keys', async () => {
    const { prefix, manifest } = archivePrefixAndManifest(await makeArchiveBytes())
    const encodedManifest = JSON.stringify(manifest)
    const cases = [
      `${prefix}, "manifest" : ${encodedManifest}, "mani\\u0066est" : ${encodedManifest}}`,
      `${prefix}, "manifest" : ${encodedManifest}, "links" : []}`,
      `${prefix}, "schema_version" : 2, "manifest" : ${encodedManifest}}`,
    ]
    for (const raw of cases) {
      await expect(validateArchiveV2Bytes(encoder.encode(raw), validationOptions())).rejects.toBeInstanceOf(
        ArchiveV2ValidationError,
      )
    }
  })

  it('rejects a manifest namespace, count, or checksum that does not match the body', async () => {
    const wrongNamespace = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateManifest: (manifest) => { manifest.client_data_namespace = 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB' },
    })
    const wrongCount = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateManifest: (manifest) => { manifest.counts.links = 2 },
    })
    const wrongChecksum = await makeArchiveBytes(fullArchiveV2Selection, {
      mutateManifest: (manifest) => { manifest.checksum_sha256 = '0'.repeat(64) },
    })
    for (const bytes of [wrongNamespace, wrongCount, wrongChecksum]) {
      await expect(validateArchiveV2Bytes(bytes, validationOptions())).rejects.toBeInstanceOf(
        ArchiveV2ValidationError,
      )
    }
  })
})

function appendBytes(left: Uint8Array, right: Uint8Array): Uint8Array {
  const combined = new Uint8Array(left.byteLength + right.byteLength)
  combined.set(left)
  combined.set(right, left.byteLength)
  return combined
}
