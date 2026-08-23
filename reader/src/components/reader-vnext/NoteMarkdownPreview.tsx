import type { CSSProperties, MouseEventHandler, RefObject } from 'react'

import type { Annotation, AnnotationLocator } from '../../lib/annotation-domain'
import type { TocHeading } from '../../lib/toc'
import { LazyMarkdownView as MarkdownView } from '../LazyMarkdownView'
import { ReaderToc } from '../ReaderToc'

export const NOTE_HEADING_ID_PREFIX = 'toc'

export interface NoteMarkdownPreviewProps {
  readonly text: string
  readonly annotations: Annotation[]
  readonly focusMode: boolean
  readonly scrollRef: RefObject<HTMLDivElement>
  readonly contentRef: RefObject<HTMLDivElement>
  readonly tocItems: TocHeading[]
  readonly activeTocId: string | null
  readonly style?: CSSProperties
  readonly onHeadings: (items: TocHeading[]) => void
  readonly onJumpToc: (id: string) => void
  readonly onScroll: () => void
  readonly onMouseUp: MouseEventHandler<HTMLDivElement>
  readonly onClickHighlight: (locator: AnnotationLocator, rect: DOMRect) => void
}

export function NoteMarkdownPreview({
  text,
  annotations,
  focusMode,
  scrollRef,
  contentRef,
  tocItems,
  activeTocId,
  style,
  onHeadings,
  onJumpToc,
  onScroll,
  onMouseUp,
  onClickHighlight,
}: NoteMarkdownPreviewProps) {
  return (
    <div
      ref={scrollRef}
      className={'rvx-note-preview-scroll' + (focusMode ? ' focused' : '')}
      style={style}
      onScroll={onScroll}
    >
      <ReaderToc
        items={tocItems}
        activeId={activeTocId}
        onJump={onJumpToc}
        focusMode={focusMode}
      />
      <article
        ref={contentRef}
        className="rvx-note-preview-document"
        aria-label="笔记预览"
        onMouseUp={onMouseUp}
      >
        <MarkdownView
          className="rvx-note-preview-markdown reader-flow"
          text={text}
          blockKey="note"
          anns={annotations}
          onClickHL={onClickHighlight}
          headingIdPrefix={NOTE_HEADING_ID_PREFIX}
          onHeadings={onHeadings}
        />
      </article>
    </div>
  )
}
