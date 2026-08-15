/**
 * Persistent reading assistance rail for the reading-library detail view.
 *
 * RSS detail keeps using ReaderToc because it has a different data contract.
 * This component is deliberately presentational: progress, tags, headings,
 * and annotations remain owned by DetailPane and are passed in as snapshots.
 */
import { Icon } from './Icon'
import {
  annotationLocator,
  annotationLocatorTargetKey,
  type Annotation,
  type AnnotationLocator,
} from '../lib/annotations'
import { ArticleOutline } from './detail/ArticleOutline'
import { ReadingProgressSummary } from './detail/ReadingProgress'
import type { TocHeading } from '../lib/toc'

export interface ReaderRailProps {
  tags: string[]
  relatedTags: string[]
  currentTag: string | null
  onPickTag: (tag: string) => void
  progress: number
  progressEnabled?: boolean
  readMinutes: number
  tocItems: TocHeading[]
  activeTocId: string | null
  onJumpToc: (id: string) => void
  annotations: Annotation[]
  onOpenAnnotation: (locator: AnnotationLocator) => void
  editing?: boolean
}

function annotationKey(annotation: Annotation, locator: AnnotationLocator): string {
  return [
    annotation.id,
    annotation.blockKey,
    annotationLocatorTargetKey(locator) ?? 'unbound',
  ].join('\0')
}

export function ReaderRail({
  tags,
  relatedTags,
  currentTag,
  onPickTag,
  progress,
  progressEnabled = true,
  readMinutes,
  tocItems,
  activeTocId,
  onJumpToc,
  annotations,
  onOpenAnnotation,
  editing = false,
}: ReaderRailProps) {
  const railAnnotations = annotations
    .map((annotation) => ({ annotation, locator: annotationLocator(annotation) }))
    .filter((entry): entry is { annotation: Annotation; locator: AnnotationLocator } => Boolean(entry.locator))
    .slice(0, 6)

  return (
    <aside className="reader-rail" aria-label="阅读辅助栏">
      <section className="reader-rail-section reader-rail-tags" aria-labelledby="reader-rail-tags-title">
        <h2 id="reader-rail-tags-title" className="reader-rail-title">
          <Icon name="tag" size={13} /> 标签
        </h2>
        {tags.length > 0 || relatedTags.length > 0 ? (
          <div className="reader-rail-tag-list">
            {tags.map((tag) => (
              <button
                key={`tag:${tag}`}
                type="button"
                className={'mini-tag clickable' + (currentTag === tag ? ' cur' : '')}
                title={`查看标签 #${tag}`}
                onClick={() => onPickTag(tag)}
              >
                #{tag}
              </button>
            ))}
            {relatedTags.map((tag) => (
              <button
                key={`related:${tag}`}
                type="button"
                className={'mini-tag clickable rel' + (currentTag === tag ? ' cur' : '')}
                title={`查看相关标签 #${tag}`}
                onClick={() => onPickTag(tag)}
              >
                ≈ {tag}
              </button>
            ))}
          </div>
        ) : (
          <p className="reader-rail-empty">暂无标签</p>
        )}
      </section>

      {!editing && progressEnabled && <ReadingProgressSummary progress={progress} readMinutes={readMinutes} />}

      {!editing && (
        <ArticleOutline items={tocItems} activeId={activeTocId} onJump={onJumpToc} />
      )}

      {railAnnotations.length > 0 && (
        <section className="reader-rail-section reader-rail-annotations" aria-labelledby="reader-rail-annotations-title">
          <h2 id="reader-rail-annotations-title" className="reader-rail-title">
            <Icon name="marker" size={13} /> 划线
          </h2>
          <div className="reader-rail-annotation-list">
            {railAnnotations.map(({ annotation, locator }) => (
              <button
                key={annotationKey(annotation, locator)}
                type="button"
                className={'reader-rail-annotation' + (annotation.source === 'ai' ? ' ai' : '')}
                title={annotation.text}
                onClick={() => onOpenAnnotation(locator)}
              >
                <span className="reader-rail-annotation-quote">{annotation.text}</span>
                {annotation.note.trim() && <span className="reader-rail-annotation-note">有想法</span>}
              </button>
            ))}
          </div>
        </section>
      )}
    </aside>
  )
}
