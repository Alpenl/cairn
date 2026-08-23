/**
 * 详情底部划线与想法列表。
 *
 * 按 annOrder（summary 在前，历史遗留块靠后）+ start 排序。每项：来源色条 + 引文 +
 * 想法（Markdown/GFM 渲染，AI 笔记带标签）或「添加想法…」占位 + 删除按钮。
 * 点击 item 打开 NotePanel 编辑。
 */
import { Icon } from './Icon'
import { ThoughtMarkdown } from './ThoughtMarkdown'
import { annOrder, type Annotation } from '../lib/annotation-domain'

export interface AnnotationsListProps {
  anns: readonly Annotation[]
  onOpen: (annotation: Annotation) => void
  onDelete: (annotation: Annotation) => void | Promise<unknown>
}

export function AnnotationsList({ anns, onOpen, onDelete }: AnnotationsListProps) {
  if (!anns.length) return null
  const sorted = [...anns].sort((a, b) => annOrder(a) - annOrder(b) || a.start - b.start)
  return (
    <div className="ann-list reader-prose">
      <div className="sec-eyebrow">
        <Icon name="marker" size={13} /> 划线与想法 · {anns.length}
      </div>
      <div className="ann-items">
        {sorted.map((a) => (
          <div
            key={`${a.blockKey}\0${a.id}\0${a.sourceContentRevision ?? a.sourceSummaryHash ?? a.sourceNoteRevision ?? ''}`}
            className="ann-item"
            onClick={() => onOpen(a)}
          >
            <span className={'ann-bar' + (a.source === 'ai' ? ' ai' : '')} />
            <div className="ann-body">
              <div className="ann-quote">{a.text}</div>
              {a.note ? (
                <div className="ann-note">
                  {a.source === 'ai' && (
                    <span className="ann-ai-tag">
                      <Icon name="sparkles" size={10} /> AI
                    </span>
                  )}
                  <ThoughtMarkdown source={a.note} />
                </div>
              ) : (
                <div className="ann-empty">添加想法…</div>
              )}
            </div>
            <button
              className="ann-del"
              title="删除"
              onClick={(e) => {
                e.stopPropagation()
                void onDelete(a)
              }}
            >
              <Icon name="trash" size={14} />
            </button>
          </div>
        ))}
      </div>
    </div>
  )
}
