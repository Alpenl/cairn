import {
  forwardRef,
  useCallback,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type ChangeEvent,
  type KeyboardEvent,
  type UIEvent,
} from 'react'

import {
  applySlashCommand,
  continueMarkdownList,
  filterSlashCommands,
  indentMarkdownLists,
  slashQueryAt,
  toggleMarkdownFormat,
  type MarkdownEdit,
  type SlashCommand,
  type SlashQuery,
} from '../../lib/note-markdown/editor'
import { Icon, type IconName } from '../Icon'

export interface NoteEditorViewport {
  readonly selectionStart: number
  readonly selectionEnd: number
  readonly scrollTop: number
}

export interface NoteMarkdownEditorHandle {
  captureViewport(): NoteEditorViewport
  focus(): void
}

export interface NoteMarkdownEditorProps {
  readonly documentKey: string
  readonly value: string
  readonly disabled?: boolean
  readonly initialViewport?: NoteEditorViewport
  readonly onValueChange: (value: string) => void
  readonly onViewportChange?: (viewport: NoteEditorViewport) => void
}

interface SlashMenuState {
  readonly query: SlashQuery
  readonly activeIndex: number
  readonly caretRect: DOMRect
}

interface EditorSnapshot {
  readonly text: string
  readonly viewport: NoteEditorViewport
}

interface CommandHistoryEntry {
  readonly before: EditorSnapshot
  readonly after: EditorSnapshot
}

export interface SlashMenuPosition {
  readonly left: number
  readonly top: number
  readonly width: number
  readonly maxHeight: number
  readonly placement: 'above' | 'below'
}

const DEFAULT_VIEWPORT: NoteEditorViewport = Object.freeze({
  selectionStart: 0,
  selectionEnd: 0,
  scrollTop: 0,
})

const SLASH_MENU_WIDTH = 264
const SLASH_MENU_ESTIMATED_HEIGHT = 306
const VIEWPORT_PADDING = 8
const CARET_GAP = 6
const COMMAND_HISTORY_LIMIT = 100

// eslint-disable-next-line react-refresh/only-export-components
export function positionSlashMenu(
  caretRect: Pick<DOMRect, 'left' | 'top' | 'bottom'>,
  viewportWidth: number,
  viewportHeight: number,
): SlashMenuPosition {
  const width = Math.min(SLASH_MENU_WIDTH, Math.max(0, viewportWidth - VIEWPORT_PADDING * 2))
  const left = Math.max(
    VIEWPORT_PADDING,
    Math.min(caretRect.left, viewportWidth - width - VIEWPORT_PADDING),
  )
  const below = Math.max(0, viewportHeight - caretRect.bottom - VIEWPORT_PADDING - CARET_GAP)
  const above = Math.max(0, caretRect.top - VIEWPORT_PADDING - CARET_GAP)
  const placement = below >= Math.min(132, SLASH_MENU_ESTIMATED_HEIGHT) || below >= above
    ? 'below'
    : 'above'
  const available = placement === 'below' ? below : above
  const maxHeight = Math.max(48, Math.min(SLASH_MENU_ESTIMATED_HEIGHT, available))
  const top = placement === 'below'
    ? caretRect.bottom + CARET_GAP
    : Math.max(VIEWPORT_PADDING, caretRect.top - maxHeight - CARET_GAP)
  return { left, top, width, maxHeight, placement }
}

function numericStyle(value: string, fallback: number): number {
  const parsed = Number.parseFloat(value)
  return Number.isFinite(parsed) ? parsed : fallback
}

function textareaCaretRect(textarea: HTMLTextAreaElement, offset: number): DOMRect {
  const textareaRect = textarea.getBoundingClientRect()
  const computed = window.getComputedStyle(textarea)
  const mirror = document.createElement('div')
  const marker = document.createElement('span')
  const properties = [
    'borderTopWidth', 'borderRightWidth', 'borderBottomWidth', 'borderLeftWidth',
    'paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft',
    'fontFamily', 'fontSize', 'fontStyle', 'fontWeight', 'fontVariant',
    'lineHeight', 'letterSpacing', 'textTransform', 'textIndent', 'textAlign',
    'wordSpacing', 'tabSize', 'wordBreak', 'overflowWrap',
  ] as const

  mirror.style.position = 'fixed'
  mirror.style.visibility = 'hidden'
  mirror.style.pointerEvents = 'none'
  mirror.style.whiteSpace = 'pre-wrap'
  mirror.style.boxSizing = 'border-box'
  mirror.style.overflow = 'hidden'
  mirror.style.left = `${textareaRect.left - textarea.scrollLeft}px`
  mirror.style.top = `${textareaRect.top - textarea.scrollTop}px`
  mirror.style.width = `${textarea.offsetWidth}px`
  for (const property of properties) mirror.style[property] = computed[property]

  mirror.append(document.createTextNode(textarea.value.slice(0, offset)))
  marker.textContent = '\u200b'
  mirror.append(marker)
  document.body.append(mirror)
  const measured = marker.getBoundingClientRect()
  mirror.remove()

  const lineHeight = numericStyle(computed.lineHeight, numericStyle(computed.fontSize, 14) * 1.4)
  const fallbackLeft = textareaRect.left + numericStyle(computed.paddingLeft, 0)
  const fallbackTop = textareaRect.top + numericStyle(computed.paddingTop, 0)
  const left = measured.left || fallbackLeft
  const top = measured.top || fallbackTop
  return new DOMRect(left, top, Math.max(1, measured.width), measured.height || lineHeight)
}

function viewportOf(textarea: HTMLTextAreaElement | null): NoteEditorViewport {
  if (!textarea) return DEFAULT_VIEWPORT
  return {
    selectionStart: textarea.selectionStart,
    selectionEnd: textarea.selectionEnd,
    scrollTop: textarea.scrollTop,
  }
}

function commandIcon(command: SlashCommand): IconName {
  switch (command.id) {
    case 'h1':
    case 'h2':
    case 'h3':
      return 'hash'
    case 'unordered-list':
    case 'ordered-list':
      return 'stack'
    case 'task-list':
      return 'check'
    case 'quote':
      return 'doc'
    case 'code-block':
      return 'code'
    case 'divider':
      return 'more'
  }
}

function commandHint(command: SlashCommand): string {
  return command.replacement.includes('\n') ? '``` ... ```' : command.replacement
}

export const NoteMarkdownEditor = forwardRef<NoteMarkdownEditorHandle, NoteMarkdownEditorProps>(
  function NoteMarkdownEditor({
    documentKey,
    value,
    disabled = false,
    initialViewport = DEFAULT_VIEWPORT,
    onValueChange,
    onViewportChange,
  }, forwardedRef) {
    const textareaRef = useRef<HTMLTextAreaElement>(null)
    const pendingViewport = useRef<NoteEditorViewport | null>(null)
    const undoStack = useRef<CommandHistoryEntry[]>([])
    const redoStack = useRef<CommandHistoryEntry[]>([])
    const renderedDocumentKey = useRef(documentKey)
    const renderedValue = useRef(value)
    const requestedValue = useRef<string | null>(null)
    const dismissedSlashQuery = useRef<string | null>(null)
    const [slashMenu, setSlashMenu] = useState<SlashMenuState | null>(null)
    const commands = useMemo(
      () => slashMenu ? filterSlashCommands(slashMenu.query.query) : [],
      [slashMenu],
    )

    const reportViewport = useCallback(() => {
      onViewportChange?.(viewportOf(textareaRef.current))
    }, [onViewportChange])

    useImperativeHandle(forwardedRef, () => ({
      captureViewport: () => viewportOf(textareaRef.current),
      focus: () => textareaRef.current?.focus(),
    }), [])

    useLayoutEffect(() => {
      const textarea = textareaRef.current
      if (!textarea) return
      const start = Math.max(0, Math.min(textarea.value.length, initialViewport.selectionStart))
      const end = Math.max(start, Math.min(textarea.value.length, initialViewport.selectionEnd))
      textarea.setSelectionRange(start, end)
      textarea.scrollTop = initialViewport.scrollTop
      textarea.focus()
      reportViewport()
      // The editor is remounted when returning from Preview. Reapplying this
      // snapshot on ordinary controlled renders would move the user's caret.
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [])

    useLayoutEffect(() => {
      if (renderedDocumentKey.current !== documentKey) {
        renderedDocumentKey.current = documentKey
        renderedValue.current = value
        requestedValue.current = null
        pendingViewport.current = null
        undoStack.current = []
        redoStack.current = []
      } else if (renderedValue.current !== value) {
        if (requestedValue.current !== value) {
          undoStack.current = []
          redoStack.current = []
        }
        renderedValue.current = value
      }
      if (requestedValue.current === value) requestedValue.current = null

      const viewport = pendingViewport.current
      const textarea = textareaRef.current
      if (!viewport || !textarea) return
      pendingViewport.current = null
      textarea.setSelectionRange(viewport.selectionStart, viewport.selectionEnd)
      textarea.scrollTop = viewport.scrollTop
      reportViewport()
    }, [documentKey, reportViewport, value])

    useEffect(() => {
      if (disabled) setSlashMenu(null)
    }, [disabled])

    useEffect(() => {
      dismissedSlashQuery.current = null
      setSlashMenu(null)
    }, [documentKey])

    const syncSlashMenu = useCallback((nextValue = value) => {
      const textarea = textareaRef.current
      if (!textarea || disabled) {
        setSlashMenu(null)
        return
      }
      const query = slashQueryAt(nextValue, {
        start: textarea.selectionStart,
        end: textarea.selectionEnd,
      })
      if (!query) {
        dismissedSlashQuery.current = null
        setSlashMenu(null)
        return
      }
      const queryKey = `${query.start}:${query.end}:${query.query}`
      if (dismissedSlashQuery.current === queryKey) {
        setSlashMenu(null)
        return
      }
      dismissedSlashQuery.current = null
      const caretRect = textareaCaretRect(textarea, query.end)
      setSlashMenu((current) => ({
        query,
        caretRect,
        activeIndex: current &&
          current.query.start === query.start &&
          current.query.query === query.query
          ? current.activeIndex
          : 0,
      }))
    }, [disabled, value])

    const applySnapshot = useCallback((snapshot: EditorSnapshot) => {
      pendingViewport.current = snapshot.viewport
      dismissedSlashQuery.current = null
      setSlashMenu(null)
      if (snapshot.text !== value) {
        requestedValue.current = snapshot.text
        onValueChange(snapshot.text)
      } else {
        pendingViewport.current = null
        const textarea = textareaRef.current
        textarea?.setSelectionRange(
          snapshot.viewport.selectionStart,
          snapshot.viewport.selectionEnd,
        )
        if (textarea) textarea.scrollTop = snapshot.viewport.scrollTop
        reportViewport()
      }
    }, [onValueChange, reportViewport, value])

    const commit = useCallback((edit: MarkdownEdit) => {
      if (!edit.handled) return false
      const beforeViewport = viewportOf(textareaRef.current)
      const after: EditorSnapshot = {
        text: edit.text,
        viewport: {
          selectionStart: edit.start,
          selectionEnd: edit.end,
          scrollTop: beforeViewport.scrollTop,
        },
      }
      if (edit.text !== value) {
        undoStack.current = [
          ...undoStack.current,
          { before: { text: value, viewport: beforeViewport }, after },
        ].slice(-COMMAND_HISTORY_LIMIT)
        redoStack.current = []
      }
      applySnapshot(after)
      return true
    }, [applySnapshot, value])

    const replayCommandHistory = useCallback((direction: 'undo' | 'redo') => {
      const source = direction === 'undo' ? undoStack.current : redoStack.current
      const entry = source.at(-1)
      if (!entry) return false
      const expected = direction === 'undo' ? entry.after : entry.before
      if (expected.text !== value) return false

      source.pop()
      const destination = direction === 'undo' ? redoStack.current : undoStack.current
      destination.push(entry)
      applySnapshot(direction === 'undo' ? entry.before : entry.after)
      return true
    }, [applySnapshot, value])

    const chooseCommand = useCallback((command: SlashCommand) => {
      if (!slashMenu || disabled) return
      commit(applySlashCommand(value, slashMenu.query, command))
    }, [commit, disabled, slashMenu, value])

    const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
      const textarea = event.currentTarget
      const selection = { start: textarea.selectionStart, end: textarea.selectionEnd }

      if (slashMenu) {
        if (event.key === 'Escape') {
          event.preventDefault()
          dismissedSlashQuery.current = `${slashMenu.query.start}:${slashMenu.query.end}:${slashMenu.query.query}`
          setSlashMenu(null)
          return
        }
        if (commands.length > 0 && (event.key === 'ArrowDown' || event.key === 'ArrowUp')) {
          event.preventDefault()
          const delta = event.key === 'ArrowDown' ? 1 : -1
          setSlashMenu((current) => current ? {
            ...current,
            activeIndex: (current.activeIndex + delta + commands.length) % commands.length,
          } : null)
          return
        }
        if (commands.length > 0 && event.key === 'Enter') {
          event.preventDefault()
          chooseCommand(commands[Math.min(slashMenu.activeIndex, commands.length - 1)])
          return
        }
      }

      if ((event.metaKey || event.ctrlKey) && !event.altKey) {
        const key = event.key.toLocaleLowerCase()
        const historyDirection = key === 'z'
          ? (event.shiftKey ? 'redo' : 'undo')
          : key === 'y' && !event.shiftKey
            ? 'redo'
            : null
        if (historyDirection && replayCommandHistory(historyDirection)) {
          event.preventDefault()
          return
        }
        if (key === 'b' || key === 'i') {
          event.preventDefault()
          commit(toggleMarkdownFormat(value, selection, key === 'b' ? 'bold' : 'italic'))
          return
        }
      }

      if (event.key === 'Tab') {
        const edit = indentMarkdownLists(value, selection, event.shiftKey ? 'outdent' : 'indent')
        if (edit.handled) {
          event.preventDefault()
          commit(edit)
        }
        return
      }

      if (event.key === 'Enter') {
        const edit = continueMarkdownList(value, selection)
        if (edit.handled) {
          event.preventDefault()
          commit(edit)
        }
      }
    }

    const onChange = (event: ChangeEvent<HTMLTextAreaElement>) => {
      const nextValue = event.currentTarget.value
      dismissedSlashQuery.current = null
      requestedValue.current = nextValue
      redoStack.current = []
      onValueChange(nextValue)
      onViewportChange?.(viewportOf(event.currentTarget))
      syncSlashMenu(nextValue)
    }

    const onScroll = (event: UIEvent<HTMLTextAreaElement>) => {
      onViewportChange?.(viewportOf(event.currentTarget))
      if (slashMenu) syncSlashMenu()
    }

    const menuPosition = slashMenu
      ? positionSlashMenu(slashMenu.caretRect, window.innerWidth, window.innerHeight)
      : null
    const activeCommand = commands[Math.min(slashMenu?.activeIndex ?? 0, Math.max(0, commands.length - 1))]
    const menuID = 'note-markdown-slash-menu'

    return (
      <div className="rvx-note-editor-input">
        <textarea
          ref={textareaRef}
          className="rvx-note-textarea"
          value={value}
          disabled={disabled}
          spellCheck
          aria-label="笔记内容"
          aria-autocomplete="list"
          aria-controls={slashMenu ? menuID : undefined}
          aria-expanded={slashMenu ? true : undefined}
          aria-activedescendant={activeCommand ? `${menuID}-${activeCommand.id}` : undefined}
          onChange={onChange}
          onKeyDown={onKeyDown}
          onKeyUp={() => { reportViewport(); syncSlashMenu() }}
          onClick={() => { reportViewport(); syncSlashMenu() }}
          onSelect={() => { reportViewport(); syncSlashMenu() }}
          onScroll={onScroll}
        />
        {!disabled && slashMenu && menuPosition && (
          <div
            id={menuID}
            className="rvx-slash-menu"
            role="listbox"
            aria-label="Markdown 命令"
            data-placement={menuPosition.placement}
            data-caret-top={slashMenu.caretRect.top}
            data-caret-bottom={slashMenu.caretRect.bottom}
            style={{
              left: menuPosition.left,
              top: menuPosition.top,
              width: menuPosition.width,
              maxHeight: menuPosition.maxHeight,
            } as CSSProperties}
            onMouseDown={(event) => event.preventDefault()}
          >
            {commands.length === 0 ? (
              <p className="rvx-slash-empty">没有匹配的命令</p>
            ) : commands.map((command, index) => (
              <button
                id={`${menuID}-${command.id}`}
                key={command.id}
                type="button"
                role="option"
                aria-selected={index === slashMenu.activeIndex}
                className={index === slashMenu.activeIndex ? 'active' : ''}
                onMouseEnter={() => setSlashMenu((current) => current ? { ...current, activeIndex: index } : null)}
                onClick={() => chooseCommand(command)}
              >
                <Icon name={commandIcon(command)} size={15} />
                <span>{command.label}</span>
                <code>{commandHint(command)}</code>
              </button>
            ))}
          </div>
        )}
      </div>
    )
  },
)
