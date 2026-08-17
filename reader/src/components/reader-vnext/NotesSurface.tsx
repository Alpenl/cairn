import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type CSSProperties } from 'react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { ReaderNoteHistoryResponse, ReaderNoteResponse, ReaderTrashItemResponse } from '../../lib/api/types'
import type { IdentityLease } from '../../lib/identity'
import type { ReaderCapabilityLease } from '../../lib/capabilities'
import { listRemoteThoughtsForHost } from '../../lib/user-data/thought-sync'
import type { ThoughtMaterializedRecord } from '../../lib/user-data/thought-types'
import { reanchorAnnotation, type ReanchorAnnotation, type ReanchorReason } from '../../lib/reanchor'
import type { ReaderRoute } from '../../lib/navigation/route'
import { Icon } from '../Icon'
import { ThoughtMarkdown } from '../ThoughtMarkdown'
import { ActionPopover, NOTE_SELECTION_ACTIONS, type PopoverAction } from '../ActionPopover'
import { NotePanel } from '../NotePanel'
import { ChatSidebar, type ChatDraft } from '../ChatSidebar'
import { ReadingTocControl } from '../detail/ReadingTocControl'
import {
  annotationLocator,
  annotationMatchesLocator,
  getSelectionInfo,
  type Annotation,
  type AnnotationLocator,
  type SelectionInfo,
} from '../../lib/annotations'
import { useNoteAnnotations } from '../../hooks/useNoteAnnotations'
import { useReadingSurface } from '../../hooks/useReadingSurface'
import {
  markdownSource,
  readingTextVersion,
  READING_LINE_HEIGHT_LABELS,
  READING_LINE_HEIGHTS,
  READING_SIZES,
} from '../../lib/reading-surface'
import { listChecklistBlocks } from '../../lib/thought-markdown/checklist'
import { SurfaceError, SurfaceLoading, SurfaceShell, formatRelativeDate, errorMessage } from './SurfaceShell'
import {
  NoteMarkdownEditor,
  type NoteEditorViewport,
  type NoteMarkdownEditorHandle,
} from './NoteMarkdownEditor'
import { NoteMarkdownPreview } from './NoteMarkdownPreview'
import { NoteWorkspaceTabs } from './NoteWorkspaceTabs'

export interface NotesSurfaceProps {
  readonly client: IdentityBoundReaderClient
  readonly lease: IdentityLease
  readonly capabilityLease: ReaderCapabilityLease
  readonly onNavigate: (route: ReaderRoute) => void
  readonly initialNoteID?: string
  readonly onDraftDirtyChange?: (dirty: boolean) => void
  /** Drives beforeunload only; lifecycle cleanup alone is not persistence. */
  readonly onPendingPersistenceChange?: (pending: boolean) => void
  /** Registers the single, awaitable leave barrier consumed by MainView. */
  readonly onPrepareToLeaveChange?: (prepare: (() => Promise<NotesLeaveResult>) | null) => void
  /** MainView owns creation so every entry point shares the navigation protocol. */
  readonly onCreateNote?: () => void
  readonly creatingNote?: boolean
  readonly annotationsEnabled?: boolean
  readonly aiEnabled?: boolean
  readonly trashEnabled?: boolean
}

export type NotesLeaveResult =
  | { readonly status: 'ready' }
  | {
      readonly status: 'blocked'
      readonly code: 'identity_lost' | 'note_delete_failed' | 'note_discard_failed' | 'note_save_failed'
    }

/** Matches the publish contract: only ASCII space, tab, CR and LF are blank. */
// eslint-disable-next-line react-refresh/only-export-components
export function isCanonicalEmptyNoteContent(value: string): boolean {
  return /^[ \t\r\n]*$/.test(value)
}

const NOTE_READING_CAPABILITIES = [
  'focus', 'preferences', 'toc', 'back-to-top', 'annotations', 'ai', 'editing',
] as const
const NOTE_READING_SLOTS = {
  toolbar: 'minimal',
  rail: 'toc-only',
  annotation: 'enabled',
} as const
const EMPTY_EDITOR_VIEWPORT: NoteEditorViewport = Object.freeze({
  selectionStart: 0,
  selectionEnd: 0,
  scrollTop: 0,
})

function historicalReanchorOp(
  thoughtID: string,
  reason: ReanchorReason,
): Record<string, unknown> {
  return {
    thought_id: thoughtID,
    status: 'historical',
    reason,
  }
}

// eslint-disable-next-line react-refresh/only-export-components
export function buildNoteReanchorOps(
  rows: readonly NoteReanchorRow[],
  previousSource: string,
  currentSource: string,
  previousRevision: number,
  nextRevision: number,
): Record<string, unknown>[] {
  const ops: Record<string, unknown>[] = []
  const seen = new Set<string>()
  for (const row of rows) {
    if (row.deleted || row.target.kind !== 'note' || seen.has(row.annotationId)) continue
    seen.add(row.annotationId)
    if (row.target.noteRevision !== previousRevision) {
      // The server validates that a successful operation starts at the
      // published revision. A stale materialized row cannot be safely mapped
      // from this source, so preserve the thought as historical instead of
      // making publish fail after the note snapshot has changed.
      ops.push(historicalReanchorOp(row.annotationId, 'missing-quote'))
      continue
    }
    const quote = row.quote && typeof row.quote === 'object' ? row.quote : null
    const exact = typeof quote?.exact === 'string' ? quote.exact : ''
    const start = typeof quote?.start === 'number' ? quote.start : -1
    const end = typeof quote?.end === 'number' ? quote.end : -1
    if (!exact || !Number.isSafeInteger(start) || !Number.isSafeInteger(end) || end <= start) {
      ops.push(historicalReanchorOp(row.annotationId, 'missing-quote'))
      continue
    }
    if (!quote) {
      ops.push(historicalReanchorOp(row.annotationId, 'missing-quote'))
      continue
    }
    const annotation: ReanchorAnnotation = {
      id: row.annotationId,
      blockKey: typeof quote.block_key === 'string' ? quote.block_key : 'note',
      range: { start, end },
      quote: {
        exact,
        prefix: typeof quote.prefix === 'string' ? quote.prefix : '',
        suffix: typeof quote.suffix === 'string' ? quote.suffix : '',
      },
      target: { kind: 'note', hostId: row.hostId, version: `note:${row.target.noteRevision}` },
      thought: row.body,
    }
    const result = reanchorAnnotation(annotation, previousSource, currentSource, {
      kind: 'note',
      hostId: row.hostId,
      version: `note:${nextRevision}`,
    })
    ops.push(result.status === 'historical'
      ? { ...historicalReanchorOp(row.annotationId, result.reason), quote: result.annotation.quote, range: result.annotation.range }
      : {
          thought_id: row.annotationId,
          status: result.status,
          reason: result.reason,
          target: { kind: 'note', host_id: row.hostId, version: { note_revision: nextRevision } },
          quote: result.annotation.quote,
          range: result.annotation.range,
        })
  }
  return ops
}

type NoteReanchorRow = Pick<ThoughtMaterializedRecord, 'annotationId' | 'deleted' | 'target' | 'quote' | 'hostId' | 'body'>

function localNoteReanchorRow(annotation: Annotation, noteID: string): NoteReanchorRow | null {
  if (annotation.sourceNoteRevision === undefined) return null
  return {
    annotationId: annotation.id,
    deleted: false,
    hostId: noteID,
    body: annotation.note,
    target: { kind: 'note', noteRevision: annotation.sourceNoteRevision },
    quote: {
      exact: annotation.text,
      prefix: annotation.quote?.prefix ?? '',
      suffix: annotation.quote?.suffix ?? '',
      start: annotation.start,
      end: annotation.end,
      block_key: annotation.blockKey,
    },
  }
}

function optionalRecord(value: unknown): Record<string, unknown> | null {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : null
}

function optionalProjectionText(record: Record<string, unknown>, keys: readonly string[]): string | null {
  for (const key of keys) {
    const value = record[key]
    if (typeof value === 'string' && value.trim()) return value.trim()
  }
  return null
}

function optionalProjectionCount(record: Record<string, unknown>, keys: readonly string[]): number | null {
  for (const key of keys) {
    const value = record[key]
    if (typeof value === 'number' && Number.isSafeInteger(value) && value >= 0) return value
  }
  return null
}

function markdownExcerpt(source: string): string {
  const blocks = source.split(/\r?\n(?:\s*\r?\n)+/).map((block) => block.trim()).filter(Boolean)
  for (const block of blocks) {
    if (/^#{1,6}\s+\S/.test(block) || /^```/.test(block) || /^~~~/.test(block)) continue
    const text = block
      .replace(/^>\s?/gm, '')
      .replace(/^\s*(?:[-+*]|\d+[.)])\s+\[[ xX]\]\s+/gm, '')
      .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
      .replace(/[*_`~]/g, '')
      .replace(/\s+/g, ' ')
      .trim()
    if (text) return text.slice(0, 180)
  }
  return ''
}

interface NoteCardProjection {
  readonly excerpt: string
  readonly unfinishedTodoCount: number
}

function noteCardProjection(note: ReaderNoteResponse): NoteCardProjection {
  const record = optionalRecord(note)
  const excerpt = record
    ? optionalProjectionText(record, ['first_paragraph', 'published_excerpt', 'excerpt'])
    : null
  const unfinishedTodoCount = record
    ? optionalProjectionCount(record, ['unfinished_todo_count', 'open_todo_count', 'todo_count'])
    : null
  return {
    excerpt: excerpt ?? markdownExcerpt(note.published_content),
    unfinishedTodoCount: unfinishedTodoCount ?? listChecklistBlocks(note.published_content).filter((block) => !block.done).length,
  }
}

export function NotesSurface({ client, lease, capabilityLease, onNavigate, initialNoteID, onDraftDirtyChange, onPendingPersistenceChange, onPrepareToLeaveChange, onCreateNote, creatingNote = false, annotationsEnabled = false, aiEnabled = false, trashEnabled = false }: NotesSurfaceProps) {
  const [items, setItems] = useState<ReaderNoteResponse[]>([])
  const [trashItems, setTrashItems] = useState<ReaderTrashItemResponse[]>([])
  const [view, setView] = useState<'active' | 'trash'>('active')
  const [totalCount, setTotalCount] = useState<number | null>(null)
  const [trashCount, setTrashCount] = useState<number | null>(null)
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const [selectedTrashID, setSelectedTrashID] = useState<string | null>(null)
  const [nextCursor, setNextCursor] = useState<string | undefined>()
  const [trashNextCursor, setTrashNextCursor] = useState<string | undefined>()
  const [trashDetail, setTrashDetail] = useState<ReaderNoteResponse | null>(null)
  const [content, setContent] = useState('')
  const [showDraft, setShowDraft] = useState(false)
  const [preview, setPreview] = useState(false)
  const [history, setHistory] = useState<ReaderNoteHistoryResponse[] | null>(null)
	const [historyPreview, setHistoryPreview] = useState<ReaderNoteHistoryResponse | null>(null)
  const [selection, setSelection] = useState<{ readonly info: SelectionInfo; readonly rect: DOMRect } | null>(null)
  const [selectedAnnotationID, setSelectedAnnotationID] = useState<string | null>(null)
  const [chatDraft, setChatDraft] = useState<ChatDraft | null>(null)
  const previewRef = useRef<HTMLDivElement>(null)
  const previewScrollRef = useRef<HTMLDivElement>(null)
  const editorRef = useRef<NoteMarkdownEditorHandle>(null)
  const editorViewport = useRef<NoteEditorViewport>(EMPTY_EDITOR_VIEWPORT)
  const chatNonce = useRef(0)
  const [loading, setLoading] = useState(true)
  const [trashLoading, setTrashLoading] = useState(false)
  const [trashDetailLoading, setTrashDetailLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [trashError, setTrashError] = useState<string | null>(null)
  const autosaveTimer = useRef<number | null>(null)
  const draftQueues = useRef(new Map<string, Promise<unknown>>())
  const draftSavePromises = useRef(new Map<string, { value: string; generation: number; promise: Promise<ReaderNoteResponse | null> }>())
  const draftRevisions = useRef(new Map<string, number>())
  const draftContents = useRef(new Map<string, string>())
  const draftGenerations = useRef(new Map<string, number>())
  const emptyDraftDiscards = useRef(new Map<string, Promise<boolean>>())
  const prepareToLeaveInFlight = useRef<Promise<NotesLeaveResult> | null>(null)
  const preparedLeaveValues = useRef(new Map<string, string>())
  const editorStateRef = useRef<{
    readonly note: ReaderNoteResponse | null
    readonly content: string
    readonly showDraft: boolean
    readonly dirty: boolean
  }>({ note: null, content: '', showDraft: false, dirty: false })
  const busyCount = useRef(0)

  const selected = useMemo(() => items.find((item) => item.id === selectedID) ?? items[0] ?? null, [items, selectedID])
  const noteAnnotations = useNoteAnnotations(lease, selected?.id ?? null, selected?.published_revision ?? null)
  const canAnnotatePreview = Boolean(
    annotationsEnabled && preview && selected && selected.published_revision > 0 && content === selected.published_content,
  )
  const visibleAnnotations = canAnnotatePreview ? noteAnnotations.anns : []
  const selectedAnnotation = noteAnnotations.anns.find((annotation) => annotation.id === selectedAnnotationID) ?? null
  const annotationPanelOpen = Boolean(selectedAnnotation && canAnnotatePreview && !chatDraft)
  const serverDraftContent = selected ? (selected.draft_content ?? selected.published_content) : ''
  const dirty = Boolean(selected && showDraft && content !== serverDraftContent)
  const needsLeavePreparation = Boolean(selected && (
    selected.published_revision === 0 || selected.dirty || dirty || saving
  ))
  const pendingPersistence = Boolean(selected && (selected.dirty || dirty || saving || draftQueues.current.has(selected.id)))

  useLayoutEffect(() => {
    onDraftDirtyChange?.(needsLeavePreparation)
  }, [needsLeavePreparation, onDraftDirtyChange])

  useLayoutEffect(() => {
    onPendingPersistenceChange?.(pendingPersistence)
  }, [onPendingPersistenceChange, pendingPersistence])

  useEffect(() => () => {
    onDraftDirtyChange?.(false)
    onPendingPersistenceChange?.(false)
  }, [onDraftDirtyChange, onPendingPersistenceChange])
  const readingSource = useMemo(() => markdownSource(
    content,
    {
      hostId: selected?.id ?? 'empty-note-reading-surface',
      version: selected && content === selected.published_content
        ? `note:${selected.published_revision}`
        : `draft:${readingTextVersion(content)}`,
    },
    'note',
  ), [content, selected])
  const readingCapabilities = useMemo(
    () => NOTE_READING_CAPABILITIES.filter((capability) => {
      if (capability === 'annotations') return annotationsEnabled
      if (capability === 'ai') return aiEnabled
      return true
    }),
    [aiEnabled, annotationsEnabled],
  )
  const readingSurface = useReadingSurface({
    source: readingSource,
    capabilities: readingCapabilities,
    slots: NOTE_READING_SLOTS,
    scrollRef: previewScrollRef,
    layoutKey: 'notes-preview',
  })
  const {
    focusMode,
    setFocusMode,
    readingPreference,
    setReadingPreference,
    toc,
  } = readingSurface
  const previewStyle = {
    '--reading-font-size': `${READING_SIZES[readingPreference.size]}px`,
    '--reading-line-height': READING_LINE_HEIGHTS[readingPreference.lineHeight],
  } as CSSProperties

  const beginBusy = useCallback(() => {
    busyCount.current += 1
    setSaving(true)
  }, [])

  const endBusy = useCallback(() => {
    busyCount.current = Math.max(0, busyCount.current - 1)
    setSaving(busyCount.current > 0)
  }, [])

  const applyNoteResponse = useCallback((note: ReaderNoteResponse) => {
    draftRevisions.current.set(note.id, note.draft_revision)
    draftContents.current.set(note.id, note.draft_content ?? note.published_content)
    setItems((current) => current.map((item) => item.id === note.id ? note : item))
  }, [])

  const clearAutosave = useCallback(() => {
    if (autosaveTimer.current === null) return
    window.clearTimeout(autosaveTimer.current)
    autosaveTimer.current = null
  }, [])

  const enqueueDraftOperation = useCallback(<T,>(
    noteID: string,
    operation: () => Promise<T>,
    trackBusy = true,
  ): Promise<T> => {
    if (trackBusy) beginBusy()
    const previous = draftQueues.current.get(noteID) ?? Promise.resolve()
    const next = previous
      .catch(() => undefined)
      .then(operation)
      .finally(() => {
        if (trackBusy) endBusy()
        if (draftQueues.current.get(noteID) === next) draftQueues.current.delete(noteID)
      })
    draftQueues.current.set(noteID, next)
    return next
  }, [beginBusy, endBusy])

  const saveDraftRequest = useCallback(async (
    noteID: string,
    value: string,
  ): Promise<ReaderNoteResponse | null> => {
    const expectedDraftRevision = draftRevisions.current.get(noteID) ?? 0
    const result = await client.saveNoteDraft(noteID, {
      content: value,
      expected_draft_revision: expectedDraftRevision,
    })
    if (!client.isIdentityCurrent()) return null
    if (!result.ok) {
      setError(errorMessage(result.error))
      return null
    }
    applyNoteResponse(result.data)
    return result.data
  }, [applyNoteResponse, client])

  const saveDraftValue = useCallback((
    noteID: string,
    value: string,
    generation = draftGenerations.current.get(noteID) ?? 0,
  ): Promise<ReaderNoteResponse | null> => {
    if (draftGenerations.current.get(noteID) !== generation) return Promise.resolve(null)
    const previous = draftSavePromises.current.get(noteID)
    if (previous && previous.value === value && previous.generation === generation) return previous.promise

    const promise = enqueueDraftOperation(noteID, async () => {
      if (draftGenerations.current.get(noteID) !== generation) return null
      return saveDraftRequest(noteID, value)
    })
    draftSavePromises.current.set(noteID, { value, generation, promise })
    void promise.catch(() => undefined).finally(() => {
      const current = draftSavePromises.current.get(noteID)
      if (current?.promise === promise) draftSavePromises.current.delete(noteID)
    })
    return promise
  }, [enqueueDraftOperation, saveDraftRequest])

  const discardEmptyDraft = useCallback((
    noteID: string,
    fallbackRevision: number,
    reportError = true,
  ): Promise<boolean> => {
    const existing = emptyDraftDiscards.current.get(noteID)
    if (existing) return existing
    clearAutosave()
    draftGenerations.current.set(noteID, (draftGenerations.current.get(noteID) ?? 0) + 1)
    const promise = enqueueDraftOperation(noteID, async () => {
      const expectedDraftRevision = draftRevisions.current.get(noteID) ?? fallbackRevision
      const result = await client.discardNoteDraft(noteID, expectedDraftRevision)
      if (!client.isIdentityCurrent()) return false
      if (!result.ok) {
        if (reportError) setError(errorMessage(result.error))
        return false
      }
      draftContents.current.set(noteID, '')
      if (reportError) {
        setItems((current) => current.map((item) => item.id === noteID
          ? { ...item, draft_content: null, draft_updated_at: null, dirty: false }
          : item))
      }
      return true
    }, reportError)
    emptyDraftDiscards.current.set(noteID, promise)
    void promise.catch(() => false).finally(() => {
      if (emptyDraftDiscards.current.get(noteID) === promise) emptyDraftDiscards.current.delete(noteID)
    })
    return promise
  }, [clearAutosave, client, enqueueDraftOperation])

  const load = useCallback(async (append = false, cursor?: string) => {
    setLoading(!append)
    setError(null)
    const result = await client.listNotes({ after: cursor, limit: 30 })
    if (!client.isIdentityCurrent()) return
    if (!result.ok) setError(errorMessage(result.error))
    else {
      setItems((current) => append ? [...current, ...result.data.items] : result.data.items)
      setTotalCount(result.data.count)
      setNextCursor(result.data.next_cursor)
      const requested = initialNoteID ? result.data.items.find((item) => item.id === initialNoteID) : undefined
      setSelectedID((current) => requested?.id ?? current ?? result.data.items[0]?.id ?? null)
    }
    setLoading(false)
  }, [client, initialNoteID])

  useEffect(() => { void load() }, [load])

  const loadTrash = useCallback(async (append = false, cursor?: string) => {
    setTrashLoading(!append)
    setTrashError(null)
    const result = await client.listTrash({ hostKind: 'note', after: cursor, limit: 50 })
    if (!client.isIdentityCurrent()) return
    if (!result.ok) setTrashError(errorMessage(result.error))
    else {
      setTrashItems((current) => append ? [...current, ...result.data.items] : result.data.items)
      setTrashCount(result.data.count)
      setTrashNextCursor(result.data.next_cursor)
      setSelectedTrashID((current) => current ?? result.data.items[0]?.host_id ?? null)
    }
    setTrashLoading(false)
  }, [client])

  const selectTrashNote = useCallback(async (item: ReaderTrashItemResponse) => {
    if (item.host_id === selectedTrashID && trashDetail?.id === item.host_id) return
    setSelectedTrashID(item.host_id)
    setTrashDetail(null)
    setTrashDetailLoading(true)
    const result = await client.getNote(item.host_id)
    if (!client.isIdentityCurrent()) return
    if (!result.ok) setTrashError(errorMessage(result.error))
    else if (!result.data.deleted_at) setTrashError('该笔记已不在回收站。')
    else setTrashDetail(result.data)
    setTrashDetailLoading(false)
  }, [client, selectedTrashID, trashDetail?.id])

  // Search can land on a note outside the first page. Hydrate that one item
  // directly so a published search result never degrades to an approximate
  // Notes landing page.
  useEffect(() => {
    if (!initialNoteID || loading || items.some((item) => item.id === initialNoteID)) return
    let active = true
    void client.getNote(initialNoteID).then((result) => {
      if (!active || !client.isIdentityCurrent() || !result.ok) return
      setItems((current) => current.some((item) => item.id === result.data.id) ? current : [result.data, ...current])
      setSelectedID(result.data.id)
    })
    return () => { active = false }
  }, [client, initialNoteID, items, loading])

  useEffect(() => {
    if (!initialNoteID || !items.some((item) => item.id === initialNoteID)) return
    setSelectedID((current) => current === initialNoteID ? current : initialNoteID)
  }, [initialNoteID, items])

  useEffect(() => {
    if (!selected) {
      setContent('')
      setShowDraft(false)
      return
    }
    draftRevisions.current.set(selected.id, selected.draft_revision)
    draftContents.current.set(selected.id, selected.draft_content ?? selected.published_content)
    if (!draftGenerations.current.has(selected.id)) draftGenerations.current.set(selected.id, 0)
    setShowDraft(selected.dirty ? false : true)
    setContent(selected.dirty ? selected.published_content : selected.draft_content ?? selected.published_content)
    setPreview(false)
    setHistory(null)
    setSelection(null)
    setSelectedAnnotationID(null)
    setChatDraft(null)
    editorViewport.current = EMPTY_EDITOR_VIEWPORT
    if (previewScrollRef.current) previewScrollRef.current.scrollTop = 0
  // Only reset the editor when changing notes. An autosave replaces the
  // selected response object too, but must leave the current draft visible.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected?.id])

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.key !== 'Escape' || !focusMode) return
      setFocusMode(false)
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [focusMode, setFocusMode])

  useEffect(() => {
    if (selected?.id && showDraft) editorRef.current?.focus()
  }, [selected?.id, showDraft])

  useLayoutEffect(() => {
    editorStateRef.current = { note: selected, content, showDraft, dirty }
  }, [content, dirty, selected, showDraft])

  useEffect(() => {
    if (selectedID) preparedLeaveValues.current.delete(selectedID)
  }, [content, selectedID])

  useEffect(() => () => {
    const current = editorStateRef.current
    const note = current.note
    if (!note) return
    const savedDraft = draftContents.current.get(note.id) ?? note.draft_content ?? note.published_content
    const draftValue = current.showDraft ? current.content : savedDraft
    if (!(note.dirty || current.dirty) || draftValue.trim() !== '') return
    void discardEmptyDraft(note.id, note.draft_revision, false).catch(() => undefined)
  }, [discardEmptyDraft])

  useEffect(() => {
    clearAutosave()
    if (!selected || !showDraft || selected.deleted_at || !dirty) return
    const noteID = selected.id
    const value = content
    const generation = draftGenerations.current.get(noteID) ?? 0
    autosaveTimer.current = window.setTimeout(() => {
      autosaveTimer.current = null
      void saveDraftValue(noteID, value, generation).catch(() => undefined)
    }, 1200)
    return clearAutosave
  }, [clearAutosave, content, dirty, saveDraftValue, selected, showDraft])

  const saveDraft = useCallback(async () => {
    if (!selected || !dirty) return
    clearAutosave()
    const generation = draftGenerations.current.get(selected.id) ?? 0
    await saveDraftValue(selected.id, content, generation)
  }, [clearAutosave, content, dirty, saveDraftValue, selected])

  const discardDraft = useCallback(async () => {
    if (!selected || (!selected.dirty && !dirty)) return
    const noteID = selected.id
    clearAutosave()
    const generation = (draftGenerations.current.get(noteID) ?? 0) + 1
    draftGenerations.current.set(noteID, generation)
    const hadQueuedOperation = draftQueues.current.has(noteID)
    if (!selected.dirty && !hadQueuedOperation) {
      setContent(selected.published_content)
      setShowDraft(false)
      return
    }
    await enqueueDraftOperation(noteID, async () => {
      const expectedDraftRevision = draftRevisions.current.get(noteID) ?? selected.draft_revision
      const result = await client.discardNoteDraft(noteID, expectedDraftRevision)
      if (!client.isIdentityCurrent()) return
      if (!result.ok) {
        setError(errorMessage(result.error))
        return
      }
      const refreshed = await client.getNote(noteID)
      if (!client.isIdentityCurrent()) return
      if (!refreshed.ok) {
        setError(errorMessage(refreshed.error))
        return
      }
      applyNoteResponse(refreshed.data)
      setContent(refreshed.data.published_content)
      setShowDraft(false)
    })
  }, [applyNoteResponse, clearAutosave, client, dirty, enqueueDraftOperation, selected])

  const prepareToLeave = useCallback((): Promise<NotesLeaveResult> => {
    if (prepareToLeaveInFlight.current) return prepareToLeaveInFlight.current
    const current = editorStateRef.current
    const note = current.note
    if (!note) return Promise.resolve({ status: 'ready' })

    const prepared = (async () => {
      const persisted = draftContents.current.get(note.id) ?? note.draft_content ?? note.published_content
      const value = current.showDraft ? current.content : persisted
      if (preparedLeaveValues.current.get(note.id) === value) return { status: 'ready' } as const
      const needsWork = note.published_revision === 0 || note.dirty || current.dirty || draftQueues.current.has(note.id)
      if (!needsWork) return { status: 'ready' } as const
      clearAutosave()

      if (isCanonicalEmptyNoteContent(value)) {
        if (note.published_revision === 0) {
          const result = await client.deleteNote(note.id)
          if (!client.isIdentityCurrent() || !result.ok) {
            if (client.isIdentityCurrent() && !result.ok) setError(errorMessage(result.error))
            return { status: 'blocked', code: client.isIdentityCurrent() ? 'note_delete_failed' : 'identity_lost' } as const
          }
          preparedLeaveValues.current.set(note.id, value)
          return { status: 'ready' } as const
        }
        const discarded = await discardEmptyDraft(note.id, note.draft_revision)
        if (discarded && client.isIdentityCurrent()) preparedLeaveValues.current.set(note.id, value)
        return discarded && client.isIdentityCurrent()
          ? { status: 'ready' } as const
          : { status: 'blocked', code: client.isIdentityCurrent() ? 'note_discard_failed' : 'identity_lost' } as const
      }

      const generation = draftGenerations.current.get(note.id) ?? 0
      const saved = await saveDraftValue(note.id, value, generation)
      if (!saved || !client.isIdentityCurrent()) {
        return { status: 'blocked', code: client.isIdentityCurrent() ? 'note_save_failed' : 'identity_lost' } as const
      }
      preparedLeaveValues.current.set(note.id, value)
      return { status: 'ready' } as const
    })().catch(() => ({ status: 'blocked', code: 'note_save_failed' } as const))
    prepareToLeaveInFlight.current = prepared
    void prepared.finally(() => {
      if (prepareToLeaveInFlight.current === prepared) prepareToLeaveInFlight.current = null
    })
    return prepared
  }, [clearAutosave, client, discardEmptyDraft, saveDraftValue])

  useEffect(() => {
    onPrepareToLeaveChange?.(prepareToLeave)
    return () => onPrepareToLeaveChange?.(null)
  }, [onPrepareToLeaveChange, prepareToLeave])

  const switchView = useCallback(async (next: 'active' | 'trash') => {
    if (next === 'trash' && !trashEnabled) return
    if (next === view) return
    if ((await prepareToLeave()).status !== 'ready') return
    setView(next)
    if (next === 'trash') void loadTrash()
  }, [loadTrash, prepareToLeave, trashEnabled, view])

  const selectNote = useCallback(async (nextID: string) => {
    if (nextID === selected?.id) return
    const current = selected
    if (current) {
      const currentDraft = showDraft
        ? content
        : draftContents.current.get(current.id) ?? current.draft_content ?? current.published_content
      const hasDraft = current.dirty || dirty
      if (hasDraft && currentDraft.trim() === '') {
        const discarded = await discardEmptyDraft(current.id, current.draft_revision)
        if (!discarded) return
      } else if (dirty) {
        if (!window.confirm('当前笔记草稿有未保存修改，确定切换？')) return
        clearAutosave()
        const generation = draftGenerations.current.get(current.id) ?? 0
        const saved = await saveDraftValue(current.id, content, generation)
        if (!saved) return
      }
    }
    setSelectedID(nextID)
  }, [clearAutosave, content, dirty, discardEmptyDraft, saveDraftValue, selected, showDraft])

  const publish = useCallback(async () => {
    if (!selected || !showDraft || selected.deleted_at) return
    const noteID = selected.id
    const publishContent = content
    clearAutosave()
    const generation = (draftGenerations.current.get(noteID) ?? 0) + 1
    draftGenerations.current.set(noteID, generation)
    await enqueueDraftOperation(noteID, async () => {
      const persistedContent = draftContents.current.get(noteID) ?? selected.draft_content ?? selected.published_content
      if (persistedContent !== publishContent) {
        const saved = await saveDraftRequest(noteID, publishContent)
        if (!saved) return
      }
      const expectedDraftRevision = draftRevisions.current.get(noteID) ?? selected.draft_revision
      const previousRevision = selected.published_revision
      const remoteThoughts = await listRemoteThoughtsForHost(lease, 'note', noteID)
      const localThoughts = noteAnnotations.anns
        .map((annotation) => localNoteReanchorRow(annotation, noteID))
        .filter((row): row is NoteReanchorRow => row !== null)
      const reanchorOps = buildNoteReanchorOps(
        [...localThoughts, ...(remoteThoughts.ok ? remoteThoughts.value : [])],
        selected.published_content,
        publishContent,
        previousRevision,
        previousRevision + 1,
      )
      const result = await client.publishNote(noteID, {
        expected_draft_revision: expectedDraftRevision,
        expected_published_revision: previousRevision,
        reanchor_ops: reanchorOps,
      })
      if (!client.isIdentityCurrent()) return
      if (!result.ok) {
        setError(errorMessage(result.error))
        return
      }
      applyNoteResponse(result.data)
      setShowDraft(false)
      setContent(result.data.published_content)
    })
  }, [applyNoteResponse, clearAutosave, client, content, enqueueDraftOperation, lease, noteAnnotations.anns, saveDraftRequest, selected, showDraft])

  const clearSelection = useCallback(() => {
    window.getSelection()?.removeAllRanges()
    setSelection(null)
  }, [])

  const onPreviewMouseUp = useCallback(() => {
    if (!canAnnotatePreview) {
      setSelection(null)
      return
    }
    const info = getSelectionInfo(previewRef.current, 1)
    setSelection(info ? { info, rect: info.rect } : null)
  }, [canAnnotatePreview])

  const onClickAnnotation = useCallback((locator: AnnotationLocator) => {
    const annotation = noteAnnotations.anns.find((candidate) => annotationMatchesLocator(candidate, locator))
    if (!annotation) return
    clearSelection()
    setChatDraft(null)
    setSelectedAnnotationID(annotation.id)
  }, [clearSelection, noteAnnotations.anns])

  const openNoteAISession = useCallback((
    annotation: AnnotationLocator,
    text: string,
    start: number,
    end: number,
  ) => {
    if (!aiEnabled || !annotationsEnabled || !selected || annotation.target.kind !== 'note') return
    chatNonce.current += 1
    setSelectedAnnotationID(null)
    setChatDraft({
      annotation,
      text,
      nonce: chatNonce.current,
      source: {
        type: 'note',
        hostId: selected.id,
        revision: annotation.target.noteRevision,
        start,
        end,
      },
    })
  }, [aiEnabled, annotationsEnabled, selected])

  const onSelectionAction = useCallback((action: PopoverAction) => {
    const current = selection
    if (!current) return
    if (action === 'copy') {
      void navigator.clipboard?.writeText(current.info.text)
      clearSelection()
      return
    }
    if (action === 'translate') {
      clearSelection()
      return
    }
    if (!annotationsEnabled || (action === 'ai' && !aiEnabled)) {
      clearSelection()
      return
    }
    const existing = noteAnnotations.anns.find((annotation) =>
      annotation.blockKey === current.info.blockKey &&
      current.info.start < annotation.end && current.info.end > annotation.start)
    if (existing) {
      clearSelection()
      const locator = annotationLocator(existing)
      if (action === 'ai' && locator?.target.kind === 'note') {
        openNoteAISession(locator, current.info.text, current.info.start, current.info.end)
        return
      }
      setSelectedAnnotationID(existing.id)
      return
    }
    if (!selected) {
      clearSelection()
      return
    }
    const noteRevision = selected.published_revision
    void noteAnnotations.add({
      blockKey: 'note',
      start: current.info.start,
      end: current.info.end,
      text: current.info.text,
      quote: current.info.quote,
      source: action === 'ai' ? 'ai' : 'self',
    }).then((result) => {
      if (result.status === 'committed' || result.status === 'duplicate') {
        if (action === 'note') setSelectedAnnotationID(result.annotationId)
        if (action === 'ai') {
          openNoteAISession({
            id: result.annotationId,
            blockKey: 'note',
            target: { kind: 'note', noteRevision },
          }, current.info.text, current.info.start, current.info.end)
        }
      } else {
        setError('划线保存失败，请重试。')
      }
      clearSelection()
    })
  }, [aiEnabled, annotationsEnabled, clearSelection, noteAnnotations, openNoteAISession, selected, selection])

  const adoptAIThought = useCallback((locator: AnnotationLocator, value: string) => {
    const annotation = noteAnnotations.anns.find((candidate) => annotationMatchesLocator(candidate, locator))
    if (!annotation) {
      setError('AI 会话对应的划线已经变化，请重新选择。')
      return
    }
    void noteAnnotations.update(annotation, { note: value.trim(), source: 'ai' }).then((result) => {
      if (result.status !== 'committed' && result.status !== 'duplicate') {
        setError('AI 想法保存失败，请重试。')
      }
    })
  }, [noteAnnotations])

  const remove = useCallback(async () => {
    if (!trashEnabled || !capabilityLease.isCurrent('trash') || !selected) return
    beginBusy()
    try {
      const result = await client.deleteNote(selected.id)
      if (!client.isIdentityCurrent()) return
      if (!result.ok) setError(errorMessage(result.error))
      else {
        setItems((current) => current.filter((item) => item.id !== selected.id))
        setTotalCount((current) => current === null ? null : Math.max(0, current - 1))
        setSelectedID(null)
      }
    } finally {
      endBusy()
    }
  }, [beginBusy, capabilityLease, client, endBusy, selected, trashEnabled])

  const restore = useCallback(async () => {
    if (!selected) return
    beginBusy()
    try {
      const result = await client.restoreNote(selected.id)
      if (!client.isIdentityCurrent()) return
      if (!result.ok) setError(errorMessage(result.error))
      else void load()
    } finally {
      endBusy()
    }
  }, [beginBusy, client, endBusy, load, selected])

  const restoreTrashNote = useCallback(async (item: ReaderTrashItemResponse) => {
    beginBusy()
    try {
      const result = await client.restoreNote(item.host_id)
      if (!client.isIdentityCurrent()) return
      if (!result.ok) {
        setTrashError(errorMessage(result.error))
        return
      }
      setTrashItems((current) => current.filter((candidate) => candidate.host_id !== item.host_id))
      setTrashCount((current) => current === null ? null : Math.max(0, current - 1))
      if (selectedTrashID === item.host_id) {
        setSelectedTrashID(null)
        setTrashDetail(null)
      }
      void load()
    } finally {
      endBusy()
    }
  }, [beginBusy, client, endBusy, load, selectedTrashID])

  const purgeTrashNote = useCallback(async (item: ReaderTrashItemResponse) => {
    if (!window.confirm(`永久删除“${item.title || '未命名笔记'}”？此操作不可恢复。`)) return
    beginBusy()
    try {
      const operationID = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random()}`
      const result = await client.purgeHost('note', item.host_id, operationID)
      if (!client.isIdentityCurrent()) return
      if (!result.ok) {
        setTrashError(errorMessage(result.error))
        return
      }
      setTrashItems((current) => current.filter((candidate) => candidate.host_id !== item.host_id))
      setTrashCount((current) => current === null ? null : Math.max(0, current - 1))
      if (selectedTrashID === item.host_id) {
        setSelectedTrashID(null)
        setTrashDetail(null)
      }
    } finally {
      endBusy()
    }
  }, [beginBusy, client, endBusy, selectedTrashID])

  const loadHistory = useCallback(async () => {
    if (!selected) return
    const result = await client.listNoteHistory(selected.id)
    if (!client.isIdentityCurrent()) return
    if (!result.ok) setError(errorMessage(result.error))
    else setHistory(result.data)
  }, [client, selected])

  const restoreRevision = useCallback(async (entry: ReaderNoteHistoryResponse) => {
    if (!selected) return
    const noteID = selected.id
    clearAutosave()
    const generation = (draftGenerations.current.get(noteID) ?? 0) + 1
    draftGenerations.current.set(noteID, generation)
    await enqueueDraftOperation(noteID, async () => {
		const remoteThoughts = await listRemoteThoughtsForHost(lease, 'note', noteID)
		const localThoughts = noteAnnotations.anns
			.map((annotation) => localNoteReanchorRow(annotation, noteID))
			.filter((row): row is NoteReanchorRow => row !== null)
		const reanchorOps = buildNoteReanchorOps(
			[...localThoughts, ...(remoteThoughts.ok ? remoteThoughts.value : [])],
			selected.published_content,
			entry.content,
			selected.published_revision,
			selected.published_revision + 1,
		)
		const result = await client.restoreNoteRevision(noteID, entry.revision, {
			expected_draft_revision: draftRevisions.current.get(noteID) ?? selected.draft_revision,
			expected_published_revision: selected.published_revision,
			reanchor_ops: reanchorOps,
		})
      if (!client.isIdentityCurrent()) return
      if (!result.ok) setError(errorMessage(result.error))
      else {
        applyNoteResponse(result.data)
        setContent(result.data.published_content)
        setShowDraft(false)
        setHistory(null)
      }
    })
  }, [applyNoteResponse, clearAutosave, client, enqueueDraftOperation, lease, noteAnnotations.anns, selected])

  const askAIFromPanel = useCallback(async (
    locator: AnnotationLocator,
    text: string,
    draftValue: string,
  ) => {
    if (!aiEnabled || !annotationsEnabled) return
    const annotation = selectedAnnotation
    if (!annotation || !annotationMatchesLocator(annotation, locator)) return
    if (draftValue.trim() !== (annotation.note || '')) {
      const saved = await noteAnnotations.update(annotation, { note: draftValue.trim() })
      if (saved.status !== 'committed' && saved.status !== 'duplicate') {
        setError('划线想法保存失败，请重试。')
        return
      }
    }
    openNoteAISession(locator, text, annotation.start, annotation.end)
  }, [aiEnabled, annotationsEnabled, noteAnnotations, openNoteAISession, selectedAnnotation])

  useEffect(() => {
    if (trashEnabled || view !== 'trash') return
    setView('active')
    setTrashItems([])
    setTrashDetail(null)
    setSelectedTrashID(null)
    setTrashError(null)
  }, [trashEnabled, view])

  const selectionActions = NOTE_SELECTION_ACTIONS.filter((action) => {
    if ((action === 'highlight' || action === 'note') && !annotationsEnabled) return false
    if (action === 'ai' && (!annotationsEnabled || !aiEnabled)) return false
    return true
  })

  const enterPreview = useCallback(() => {
    editorViewport.current = editorRef.current?.captureViewport() ?? editorViewport.current
    setSelection(null)
    setPreview(true)
  }, [])

  const enterEdit = useCallback(() => {
    clearSelection()
    setSelectedAnnotationID(null)
    setPreview(false)
  }, [clearSelection])

  return (
    <SurfaceShell
      title="笔记"
      subtitle={selected ? `${totalCount ?? items.length} 篇 · 当前发布版本 ${selected.published_revision}` : `${totalCount ?? items.length} 篇`}
      active="notes"
      onNavigate={onNavigate}
      capabilityPolicy={capabilityLease.policy}
      workspaceClassName={focusMode ? 'rvx-notes-focus-mode' : undefined}
      actions={<><NoteWorkspaceTabs active="notes" onNavigate={onNavigate} capabilityPolicy={capabilityLease.policy} /><div className="rvx-segmented" role="tablist" aria-label="笔记视图"><button type="button" className={view === 'active' ? 'active' : ''} onClick={() => void switchView('active')}>进行中</button>{trashEnabled && <button type="button" className={view === 'trash' ? 'active' : ''} onClick={() => void switchView('trash')}>回收站{trashCount === null ? '' : ` (${trashCount})`}</button>}</div>{view === 'active' && onCreateNote && <button className="rvx-icon-button" type="button" title="新建笔记" aria-label="新建笔记" disabled={saving || creatingNote} onClick={onCreateNote}><Icon name="plus" size={16} /></button>}</>}
    >
      {view === 'trash' ? <>
        {trashError && <SurfaceError message={trashError} onRetry={() => void loadTrash()} />}
        {trashLoading && trashItems.length === 0 ? <SurfaceLoading /> : trashItems.length === 0 ? <div className="rvx-empty"><Icon name="trash" size={24} /><h2>回收站为空</h2><p>删除的笔记会保留在这里，直到你永久清除它。</p></div> : (
          <div className="rvx-split notes-split">
            <aside className="rvx-list-column" aria-label="笔记回收站">
              <ul className="rvx-compact-list">
                {trashItems.map((item) => <li key={item.host_id}>
                  <button type="button" className={item.host_id === selectedTrashID ? 'active' : ''} onClick={() => void selectTrashNote(item)}>
                    <strong>{item.title || '未命名笔记'}</strong>
                    <small>删除于 {formatRelativeDate(item.trashed_at)}</small>
                  </button>
                </li>)}
              </ul>
              {trashNextCursor && <button className="rvx-load-more" type="button" disabled={trashLoading} onClick={() => void loadTrash(true, trashNextCursor)}>更多</button>}
            </aside>
            <section className="rvx-detail-column rvx-note-editor" aria-label="回收站笔记">
              {trashDetailLoading ? <SurfaceLoading /> : trashDetail ? <>
                <div className="rvx-detail-heading">
                  <div><span className="rvx-eyebrow">已删除 · 只读</span><h2>{trashDetail.title || '未命名笔记'}</h2><small>发布 v{trashDetail.published_revision} · 删除于 {formatRelativeDate(trashDetail.deleted_at ?? '')}</small></div>
                  <div className="rvx-action-row">
                    <button className="rvx-button secondary" type="button" disabled={saving} onClick={() => void restoreTrashNote({ host_id: trashDetail.id, host_kind: 'note', title: trashDetail.title, trashed_at: trashDetail.deleted_at ?? '' })}>恢复</button>
                    <button className="rvx-icon-button danger" type="button" disabled={saving} title="永久删除" aria-label={`永久删除 ${trashDetail.title || '未命名笔记'}`} onClick={() => void purgeTrashNote({ host_id: trashDetail.id, host_kind: 'note', title: trashDetail.title, trashed_at: trashDetail.deleted_at ?? '' })}><Icon name="trash" size={16} /></button>
                  </div>
                </div>
                <ThoughtMarkdown className="rvx-markdown-preview" source={trashDetail.draft_content ?? trashDetail.published_content} />
              </> : <div className="rvx-empty"><Icon name="edit" size={24} /><h2>选择一篇已删除笔记</h2><p>回收站内容只读。恢复后可在进行中列表继续编辑。</p></div>}
            </section>
          </div>
        )}
      </> : <>
      {error && <SurfaceError message={error} onRetry={() => void load()} />}
      {loading && items.length === 0 ? <SurfaceLoading /> : items.length === 0 ? <div className="rvx-empty"><Icon name="edit" size={24} /><h2>还没有笔记</h2><p>新建一篇笔记，把想法整理成发布态内容。</p></div> : (
        <div className={'rvx-split notes-split' + (annotationPanelOpen || chatDraft ? ' tool-open' : '')}>
          <aside className="rvx-list-column" aria-label="笔记列表">
            <ul className="rvx-compact-list">
              {items.map((item) => {
                const projection = noteCardProjection(item)
                return <li key={item.id}><button type="button" className={item.id === selected?.id ? 'active' : ''} onClick={() => { void selectNote(item.id) }}><strong>{item.title || '未命名笔记'}</strong><span className="rvx-muted">{projection.excerpt || '暂无首段'}</span><small>{item.dirty ? '有草稿' : `已发布 v${item.published_revision}`} · {projection.unfinishedTodoCount} 个未完成 TODO · {formatRelativeDate(item.updated_at)}</small></button></li>
              })}
            </ul>
            {nextCursor && <button className="rvx-load-more" type="button" onClick={() => void load(true, nextCursor)}>更多</button>}
          </aside>
          {selected && (
            <section className="rvx-detail-column rvx-note-editor" aria-label="笔记编辑器">
              <div className="rvx-detail-heading"><div><span className="rvx-eyebrow">{showDraft ? '草稿' : '已发布'}</span><h2>{selected.title || '未命名笔记'}</h2><small>发布 v{selected.published_revision} · 草稿 v{selected.draft_revision}</small></div><div className="rvx-action-row"><button className="rvx-icon-button" type="button" title="历史版本" aria-label="历史版本" onClick={() => void loadHistory()}><Icon name="clock" size={16} /></button>{trashEnabled && (selected.deleted_at ? <button className="rvx-button secondary" type="button" onClick={() => void restore()}>恢复</button> : <button className="rvx-icon-button danger" type="button" title="移入回收站" aria-label="移入回收站" onClick={() => void remove()}><Icon name="trash" size={16} /></button>)}</div></div>
              {selected.dirty && !showDraft && <div className="rvx-draft-banner" role="status"><span>检测到未发布草稿。</span><button className="rvx-link-button" type="button" onClick={() => { setContent(selected.draft_content ?? ''); setShowDraft(true) }}>恢复草稿</button><button className="rvx-link-button" type="button" onClick={() => { setContent(selected.published_content); setShowDraft(false) }}>只看已发布</button></div>}
              <div className="rvx-editor-toolbar">
                <div className="rvx-segmented" role="group" aria-label="笔记视图">
                  <button type="button" className={!preview ? 'active' : ''} aria-pressed={!preview} onClick={enterEdit}>编辑</button>
                  <button type="button" className={preview ? 'active' : ''} aria-pressed={preview} onClick={enterPreview}>预览</button>
                </div>
                <div className="rvx-note-toolbar-actions">
                  {preview && (
                    <>
                      <button
                        className="rvx-icon-button reading-font-button"
                        type="button"
                        aria-label="阅读字号"
                        title={`阅读字号 ${READING_SIZES[readingPreference.size]}px`}
                        onClick={() => setReadingPreference({ size: (readingPreference.size + 1) % READING_SIZES.length })}
                      >
                        Aa
                      </button>
                      <button
                        className="rvx-reading-line-button"
                        type="button"
                        aria-label={`行距 ${READING_LINE_HEIGHT_LABELS[readingPreference.lineHeight]}`}
                        title={`行距 ${READING_LINE_HEIGHT_LABELS[readingPreference.lineHeight]}`}
                        onClick={() => setReadingPreference({ lineHeight: (readingPreference.lineHeight + 1) % READING_LINE_HEIGHTS.length })}
                      >
                        {READING_LINE_HEIGHT_LABELS[readingPreference.lineHeight]}
                      </button>
                      <ReadingTocControl items={toc.items} activeId={toc.activeId} onJump={toc.jumpTo} />
                    </>
                  )}
                  <button
                    className={'rvx-icon-button' + (focusMode ? ' active' : '')}
                    type="button"
                    aria-label={focusMode ? '退出专注模式' : '专注模式'}
                    title={focusMode ? '退出专注模式' : '专注模式'}
                    onClick={() => setFocusMode(!focusMode)}
                  >
                    <Icon name={focusMode ? 'focus_exit' : 'focus'} size={16} />
                  </button>
                  <span className="rvx-muted" role="status">{saving ? '保存中…' : dirty ? '等待自动保存' : '已同步'}</span>
                </div>
              </div>
              {preview ? (
                <NoteMarkdownPreview
                  text={content}
                  annotations={visibleAnnotations as Annotation[]}
                  focusMode={focusMode}
                  scrollRef={previewScrollRef}
                  contentRef={previewRef}
                  tocItems={toc.items}
                  activeTocId={toc.activeId}
                  style={previewStyle}
                  onHeadings={toc.onHeadings}
                  onJumpToc={toc.jumpTo}
                  onScroll={toc.onScroll}
                  onMouseUp={onPreviewMouseUp}
                  onClickHighlight={onClickAnnotation}
                />
              ) : (
                <NoteMarkdownEditor
                  ref={editorRef}
                  documentKey={selected.id}
                  value={content}
                  disabled={!showDraft || saving}
                  initialViewport={editorViewport.current}
                  onValueChange={setContent}
                  onViewportChange={(viewport) => { editorViewport.current = viewport }}
                />
              )}
              {selection && <ActionPopover rect={selection.rect} actions={selectionActions} onAct={onSelectionAction} annotationActionsPending={saving || noteAnnotations.loading} />}
              <div className="rvx-editor-actions"><button className="rvx-button secondary" type="button" disabled={saving || !showDraft || !dirty} onClick={() => void saveDraft()}>立即保存</button>{(selected.dirty || dirty) && <button className="rvx-button secondary" type="button" disabled={saving} onClick={() => void discardDraft()}>丢弃草稿</button>}<button className="rvx-button primary" type="button" disabled={saving || !showDraft} onClick={() => void publish()}><Icon name="check" size={15} />发布</button></div>
              {history && <div className="rvx-history-panel"><div className="rvx-section-head"><h3>历史版本</h3><button className="rvx-icon-button" type="button" aria-label="关闭历史" title="关闭历史" onClick={() => { setHistory(null); setHistoryPreview(null) }}><Icon name="x" size={15} /></button></div>{history.length === 0 ? <p className="rvx-muted">还没有历史版本。</p> : <ul>{history.map((entry) => <li key={`${entry.id}-${entry.revision}`}><div><strong>v{entry.revision}</strong><small>{formatRelativeDate(entry.created_at)}</small><p>{entry.title}</p></div><div className="rvx-action-row"><button className="rvx-button secondary" type="button" onClick={() => setHistoryPreview(entry)}>预览</button><button className="rvx-button secondary" type="button" onClick={() => void restoreRevision(entry)}>恢复到此版本</button></div></li>)}</ul>}{historyPreview && <section className="rvx-history-preview" role="region" aria-label={`历史版本 v${historyPreview.revision} 预览`}><div className="rvx-section-head"><h3>v{historyPreview.revision}</h3><button className="rvx-icon-button" type="button" aria-label="关闭版本预览" title="关闭版本预览" onClick={() => setHistoryPreview(null)}><Icon name="x" size={15} /></button></div><ThoughtMarkdown className="rvx-markdown-preview" source={historyPreview.content} /></section>}</div>}
            </section>
          )}
          {annotationPanelOpen && selectedAnnotation && (
            <NotePanel
              ann={selectedAnnotation}
              onSave={(value) => noteAnnotations.update(selectedAnnotation, { note: value }).then((result) => { if (result.status !== 'committed' && result.status !== 'duplicate') setError('划线想法保存失败，请重试。') })}
              onDelete={() => noteAnnotations.remove(selectedAnnotation).then((result) => { if (result.status === 'committed' || result.status === 'duplicate') setSelectedAnnotationID(null); else setError('划线删除失败，请重试。') })}
              onClose={() => setSelectedAnnotationID(null)}
              onAskAI={askAIFromPanel}
            />
          )}
          {aiEnabled && annotationsEnabled && chatDraft && (
            <ChatSidebar
              client={client}
              link={null}
              draft={chatDraft}
              onAdopt={adoptAIThought}
              onClearDraft={() => setChatDraft(null)}
              onClose={() => setChatDraft(null)}
            />
          )}
        </div>
      )}
      </>}
    </SurfaceShell>
  )
}
