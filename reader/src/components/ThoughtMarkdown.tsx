import { useCallback, useMemo, useRef, type ChangeEvent } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { listChecklistBlocks, type ChecklistBlock } from '../lib/thought-markdown/checklist'
import { BlockedContentImage } from './BlockedContentImage'

export interface ThoughtMarkdownProps {
  readonly source: string
  readonly className?: string
  readonly onToggleTask?: (block: ChecklistBlock, done: boolean) => void
}

/**
 * Renders thought text as Markdown/GFM without creating an annotation host.
 * A thought can contain tasks, but it must never become a nested selection
 * surface for the article annotation system.
 */
export function ThoughtMarkdown({ source, className, onToggleTask }: ThoughtMarkdownProps) {
  const rootRef = useRef<HTMLDivElement>(null)
  const renderedTaskIndex = useRef(0)
  const tasks = useMemo(() => listChecklistBlocks(source), [source])

  const onChange = useCallback((event: ChangeEvent<HTMLDivElement>) => {
    if (!(event.target instanceof HTMLInputElement) || event.target.type !== 'checkbox') return
    const root = rootRef.current
    if (!root || !onToggleTask) return
    const blockRef = event.target.dataset.blockRef
    const occurrence = Number(event.target.dataset.occurrence)
    const task = blockRef && Number.isSafeInteger(occurrence)
      ? tasks.find((candidate) => candidate.blockRef === blockRef && candidate.occurrence === occurrence)
      : undefined
    if (task) onToggleTask(task, event.target.checked)
  }, [onToggleTask, tasks])

  const components = useMemo(() => ({
    img({ alt }: { alt?: string }) {
      return <BlockedContentImage alt={alt} />
    },
    input(props: { checked?: boolean; type?: string; disabled?: boolean }) {
      const task = tasks[renderedTaskIndex.current]
      renderedTaskIndex.current += 1
      return (
        <input
          {...props}
          type="checkbox"
          checked={task?.done ?? props.checked}
          disabled={!onToggleTask || !task}
          readOnly={!onToggleTask}
          data-block-ref={task?.blockRef}
          data-occurrence={task?.occurrence}
          onChange={() => undefined}
        />
      )
    },
  }), [onToggleTask, tasks])

  // react-markdown calls the input component in source order while rendering.
  // Resetting the cursor lets each source render bind its checkbox to the
  // stable blockRef/occurrence pair rather than to a line or DOM position.
  renderedTaskIndex.current = 0

  return (
    <div ref={rootRef} className={className} onChange={onChange}>
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={components}>
        {source}
      </ReactMarkdown>
    </div>
  )
}
