import { createRoot } from 'react-dom/client'
import type { IdentityBoundReaderClient } from '../src/lib/api/client'
import { err, ok } from '../src/lib/api/result'
import type { ReaderNoteHistoryResponse, ReaderNoteResponse } from '../src/lib/api/types'
import { IdentityLease } from '../src/lib/identity'
import { NotesSurface } from '../src/components/reader-vnext/NotesSurface'
import { enabledReaderCapabilityLease } from '../src/test/capabilities'

type Scenario = 'title' | 'empty' | 'noop' | 'dirty-restore' | 'clean-restore'

type Issue83State = {
  note: ReaderNoteResponse
  publishCalls: number
  history: ReaderNoteHistoryResponse[]
}

type Issue83Client = IdentityBoundReaderClient & {
  issue83State(): Issue83State
}

const historySource = '# Historical H1\n\nFull immutable Markdown body for restore.'

function currentScenario(): Scenario {
  const value = new URL(window.location.href).searchParams.get('scenario')
  if (value === 'empty' || value === 'noop' || value === 'dirty-restore' || value === 'clean-restore') return value
  return 'title'
}

function makeNote(overrides: Partial<ReaderNoteResponse> = {}): ReaderNoteResponse {
  return {
    id: 'issue83-note',
    title: 'Initial title',
    published_content: 'Initial published body',
    published_revision: 2,
    draft_content: null,
    draft_revision: 2,
    draft_updated_at: null,
    deleted_at: null,
    created_at: '2026-08-11T00:00:00Z',
    updated_at: '2026-08-11T00:00:00Z',
    dirty: false,
    ...overrides,
  }
}

function titleFrom(source: string): string {
  const heading = source.match(/^#\s+(.+)$/m)?.[1]?.trim()
  return heading || source.trim().split(/\s+/)[0] || '未命名笔记'
}

function asciiEmpty(source: string): boolean {
  return /^[ \t\r\n]*$/.test(source)
}

function makeHistory(): ReaderNoteHistoryResponse[] {
  return [{
    id: 1,
    revision: 1,
    title: 'Historical H1',
    content: historySource,
    reanchor_ops: [],
    created_at: '2026-08-10T00:00:00Z',
  }]
}

function makeClient(scenario: Scenario): Issue83Client {
  let note = makeNote(scenario === 'noop'
    ? { draft_content: 'Initial published body', dirty: false }
    : scenario === 'dirty-restore'
      ? { draft_content: 'Unpublished draft must survive', dirty: true }
      : {})
  const history = makeHistory()
  let publishCalls = 0

  const client = {
    listNotes: async () => ok({ items: [note], count: 1 }),
    getNote: async () => ok(note),
    listNoteHistory: async () => ok(history),
    saveNoteDraft: async (_id: string, request: { content: string }) => {
      note = makeNote({
        ...note,
        draft_content: request.content,
        draft_revision: note.draft_revision + 1,
        dirty: request.content !== note.published_content,
      })
      return ok(note)
    },
    publishNote: async (_id: string) => {
      publishCalls += 1
      const content = note.draft_content ?? note.published_content
      if (asciiEmpty(content)) {
        return err({ kind: 'other', status: 422, errorCode: 'note_content_empty', message: 'note content must not be empty' })
      }
      if (content === note.published_content) return ok(note)
      note = makeNote({
        ...note,
        title: titleFrom(content),
        published_content: content,
        published_revision: note.published_revision + 1,
        draft_content: null,
        draft_revision: note.draft_revision + 1,
        dirty: false,
      })
      history.unshift({ id: history.length + 1, revision: note.published_revision, title: note.title, content, reanchor_ops: [], created_at: '2026-08-11T00:00:00Z' })
      return ok(note)
    },
    restoreNoteRevision: async () => {
      if (note.dirty) {
        return err({ kind: 'other', status: 409, errorCode: 'note_draft_dirty', message: 'note draft has unpublished changes' })
      }
      note = makeNote({
        ...note,
        title: 'Historical H1',
        published_content: historySource,
        published_revision: note.published_revision + 1,
        draft_content: null,
        draft_revision: note.draft_revision + 1,
        dirty: false,
      })
      history.unshift({ id: history.length + 1, revision: note.published_revision, title: note.title, content: historySource, reanchor_ops: [], created_at: '2026-08-11T00:00:01Z' })
      return ok(note)
    },
    discardNoteDraft: async () => ok(true),
    createNote: async () => ok(note),
    deleteNote: async () => ok({ host_kind: 'note' as const, host_id: note.id, state: 'trashed' as const, changed: true }),
    restoreNote: async () => ok({ host_kind: 'note' as const, host_id: note.id, state: 'live' as const, changed: true }),
    listTrash: async () => ok({ items: [], count: 0 }),
    purgeHost: async () => ok(true),
    isIdentityCurrent: () => true,
    issue83State: () => ({ note, publishCalls, history: [...history] }),
  } as unknown as Issue83Client
  return client
}

const scenario = currentScenario()
const client = makeClient(scenario)
const lease = new IdentityLease({
  serverClientDataNamespace: 'issue83-browser-server',
  physicalNamespace: 'issue83-browser-physical',
  localEpoch: 1,
})

declare global {
  interface Window {
    issue83NotesHarness: { state(): ReturnType<typeof client.issue83State> }
  }
}

window.issue83NotesHarness = { state: () => client.issue83State() }
const mount = document.createElement('main')
document.body.append(mount)
createRoot(mount).render(<NotesSurface client={client} capabilityLease={enabledReaderCapabilityLease()} lease={lease} onNavigate={() => undefined} />)
