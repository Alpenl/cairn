import { Fragment, useEffect, useState, type RefObject } from 'react'

import { AnnotationsList } from '../AnnotationsList'
import { ArticlePager, type ArticlePagerTarget } from '../ArticlePager'
import { Icon, type IconName } from '../Icon'
import { LazyMarkdownView as MarkdownView } from '../LazyMarkdownView'
import { PlainTextView } from '../PlainTextView'
import { CONTENT_TYPE_LABEL, relDate } from '../../lib/meta'
import {
  NO_ANNOTATIONS,
  type Annotation,
  type AnnotationLocator,
} from '../../lib/annotations'
import type { ApiError } from '../../lib/api/result'
import type { LinkResponse, TranslationResponse } from '../../lib/api/types'
import type { TocHeading } from '../../lib/toc'

export interface SavedContentView {
  content: string
  document?: string
  format: 'plain' | 'markdown' | 'html'
  source: 'fetched' | 'user'
}

export interface ContentEditView {
  initial: string
  draft: string
  format: 'plain' | 'markdown'
  error: ApiError | null
  saving: boolean
}

export interface MetadataEditView {
  title: string
  summary: string
  tags: string
  error: ApiError | null
  conflict: boolean
  saving: boolean
}

export interface HistoricalAnnotationView {
  readonly status: 'historical'
  readonly reason: string
  readonly sourceContentRevision: number
  readonly annotation: Annotation
  readonly sourceKey?: string
}

export interface ArticleBodyProps {
  article: LinkResponse
  bodyRef: RefObject<HTMLDivElement>
  notesRef: RefObject<HTMLDivElement>
  editTextareaRef: RefObject<HTMLTextAreaElement>
  focusMode: boolean
  fetcherIcon: IconName
  fetcherLabel?: string
  readMinutes: number
  relatedTags: string[]
  currentTag: string | null
  annotations: Annotation[]
  /** Saved-content annotations that could not be proven against the current body. */
  historicalAnnotations?: readonly HistoricalAnnotationView[]
  canEditMetadata: boolean
  metadataEdit: MetadataEditView | null
  loadingDetail: boolean
  hasSavedContent: boolean
  savedContent: SavedContentView | null
  contentOpen: boolean
  contentLanguageView: 'source' | 'translation'
  contentView: 'structured' | 'plain'
  contentSource: string
  contentEdit: ContentEditView | null
  contentLoading: boolean
  contentFailed: boolean
  canEditContent: boolean
  savingContent: string | null
  translationsLoading: boolean
  fullTranslation?: TranslationResponse
  hasStaleFullTranslation: boolean
  selectionTranslations: TranslationResponse[]
  historicalTranslations: TranslationResponse[]
  previous?: ArticlePagerTarget | null
  next?: ArticlePagerTarget | null
  onPickTag: (tag: string) => void
  onEnterMetadataEdit: () => void
  onUpdateMetadataEdit: (patch: Partial<Pick<MetadataEditView, 'title' | 'summary' | 'tags'>>) => void
  onSaveMetadataEdit: () => void
  onCancelMetadataEdit: () => void
  onClickHighlight: (locator: AnnotationLocator) => void
  onHeadings: (items: TocHeading[]) => void
  onToggleContent: () => void
  onExpandContent: () => void
  onSetContentLanguageView: (view: 'source' | 'translation') => void
  onSetContentView: (view: 'structured' | 'plain') => void
  onSaveContentEdit: () => void
  onCancelContentEdit: () => void
  onUpdateEditDraft: (draft: string) => void
  onTranslateFull: (force: boolean) => void
  onEnterContentEdit: () => void
  onReplaceContent: (id: string) => void
  onLoadContent: () => void
  onSaveContent: (id: string) => void
  onCopyTranslation: (text: string) => void
  onOpenAnnotation: (annotation: Annotation) => void
  /** Opens a historical item through a recovery surface instead of current-target editing. */
  onOpenHistoricalAnnotation?: (annotation: Annotation) => void
  onRemoveAnnotation: (annotation: Annotation) => Promise<boolean>
}

function contentEditErrorMessage(error: ApiError): string {
  switch (error.errorCode) {
    case 'content_revision_conflict':
      return '正文已在别处更新，请取消后重新加载最新正文。当前草稿已保留。'
    case 'content_empty':
      return '正文不能为空，保存前请保留至少一段可读内容。'
    case 'content_too_large':
      return '正文超过 2 MiB UTF-8 限制，请删减后重试。'
    default:
      return error.message || '正文保存失败，请重试。'
  }
}

function metadataEditErrorMessage(edit: MetadataEditView): string {
  if (edit.conflict) {
    return '链接信息已在其他位置更新。已保留本地草稿并同步最新版本，请再次保存。'
  }
  return edit.error?.message || '链接信息保存失败，请重试。'
}

export function ArticleBody({
  article,
  bodyRef,
  notesRef,
  editTextareaRef,
  focusMode,
  fetcherIcon,
  fetcherLabel,
  readMinutes,
  relatedTags,
  currentTag,
  annotations,
  historicalAnnotations = [],
  canEditMetadata,
  metadataEdit,
  loadingDetail,
  hasSavedContent,
  savedContent,
  contentOpen,
  contentLanguageView,
  contentView,
  contentSource,
  contentEdit,
  contentLoading,
  contentFailed,
  canEditContent,
  savingContent,
  translationsLoading,
  fullTranslation,
  hasStaleFullTranslation,
  selectionTranslations,
  historicalTranslations,
  previous,
  next,
  onPickTag,
  onEnterMetadataEdit,
  onUpdateMetadataEdit,
  onSaveMetadataEdit,
  onCancelMetadataEdit,
  onClickHighlight,
  onHeadings,
  onToggleContent,
  onExpandContent,
  onSetContentLanguageView,
  onSetContentView,
  onSaveContentEdit,
  onCancelContentEdit,
  onUpdateEditDraft,
  onTranslateFull,
  onEnterContentEdit,
  onReplaceContent,
  onLoadContent,
  onSaveContent,
  onCopyTranslation,
  onOpenAnnotation,
  onOpenHistoricalAnnotation,
  onRemoveAnnotation,
}: ArticleBodyProps) {
  const [historicalOpen, setHistoricalOpen] = useState(false)
  useEffect(() => {
    setHistoricalOpen(false)
  }, [article.content_revision, article.id])

  return (
    <div className={'reader-inner' + (focusMode ? ' focused' : '')} ref={bodyRef}>
      <div className="art-source reader-prose">
        <span style={{ display: 'inline-flex' }}>
          <Icon name={fetcherIcon} size={14} sw={1.8} />
        </span>
        {article.domain || '—'}
        {article.content_type && (
          <Fragment>
            <span className="dotsep">·</span>
            {CONTENT_TYPE_LABEL[article.content_type] || article.content_type}
          </Fragment>
        )}
        {fetcherLabel && (
          <Fragment>
            <span className="dotsep">·</span>
            {fetcherLabel}解析
          </Fragment>
        )}
        <span className="dotsep">·</span>
        {relDate(article.created_at)}收藏
        <span className="dotsep">·</span>
        约 {readMinutes} 分钟
      </div>

      {metadataEdit ? (
        <div className="content-edit-surface reader-prose" role="group" aria-label="编辑链接信息">
          <label style={{ display: 'grid', gap: 6, marginBottom: 12 }}>
            <span className="summary-eyebrow">标题</span>
            <input
              aria-label="链接标题"
              value={metadataEdit.title}
              disabled={metadataEdit.saving}
              onChange={(event) => onUpdateMetadataEdit({ title: event.target.value })}
              style={{
                boxSizing: 'border-box',
                width: '100%',
                minHeight: 36,
                padding: '7px 10px',
                border: '0.5px solid var(--divider)',
                borderRadius: 6,
                background: 'var(--surface-2)',
                color: 'var(--text-primary)',
                font: 'inherit',
              }}
            />
          </label>
          <label style={{ display: 'grid', gap: 6, marginBottom: 12 }}>
            <span className="summary-eyebrow"><Icon name="sparkles" size={13} /> 摘要</span>
            <textarea
              className="content-edit-textarea"
              aria-label="链接摘要"
              aria-describedby={metadataEdit.error ? 'metadata-edit-error' : undefined}
              value={metadataEdit.summary}
              disabled={metadataEdit.saving}
              onChange={(event) => onUpdateMetadataEdit({ summary: event.target.value })}
              style={{ minHeight: 128, fontFamily: 'var(--font-reading)' }}
            />
          </label>
          <label style={{ display: 'grid', gap: 6 }}>
            <span className="summary-eyebrow">标签</span>
            <input
              aria-label="链接标签"
              placeholder="以逗号分隔"
              value={metadataEdit.tags}
              disabled={metadataEdit.saving}
              onChange={(event) => onUpdateMetadataEdit({ tags: event.target.value })}
              style={{
                boxSizing: 'border-box',
                width: '100%',
                minHeight: 36,
                padding: '7px 10px',
                border: '0.5px solid var(--divider)',
                borderRadius: 6,
                background: 'var(--surface-2)',
                color: 'var(--text-primary)',
                font: 'inherit',
              }}
            />
          </label>
          {metadataEdit.error && (
            <p id="metadata-edit-error" className="content-edit-error" role="alert">
              <Icon name="alert" size={13} />
              {metadataEditErrorMessage(metadataEdit)}
            </p>
          )}
          <div style={{ display: 'flex', gap: 8, marginTop: 14 }}>
            <button
              type="button"
              className="replace-content-btn translate-content-btn"
              onClick={onSaveMetadataEdit}
              disabled={metadataEdit.saving}
              aria-label="保存链接信息"
            >
              <Icon
                name={metadataEdit.saving ? 'loader' : 'check'}
                size={13}
                style={metadataEdit.saving ? { animation: 'spin 0.9s linear infinite' } : undefined}
              />
              <span>{metadataEdit.saving ? '保存中…' : '保存'}</span>
            </button>
            <button
              type="button"
              className="replace-content-btn"
              onClick={onCancelMetadataEdit}
              disabled={metadataEdit.saving}
              aria-label="取消编辑链接信息"
            >
              <Icon name="close" size={13} />
              <span>取消</span>
            </button>
          </div>
        </div>
      ) : (
        <Fragment>
          <div style={{ display: 'flex', alignItems: 'flex-start', gap: 8 }}>
            {article.title ? (
              <h1 className="art-title reader-prose" style={{ flex: '1 1 auto' }}>{article.title}</h1>
            ) : (
              <h1 className="art-title url-title reader-prose" style={{ flex: '1 1 auto' }}>
                {article.url.replace(/^https?:\/\//, '')}
              </h1>
            )}
            {canEditMetadata && (
              <button
                type="button"
                className="replace-content-btn icon-action-btn"
                onClick={onEnterMetadataEdit}
                title="编辑链接信息"
                aria-label="编辑链接信息"
              >
                <Icon name="pencil" size={13} />
              </button>
            )}
          </div>

          {(article.tags.length > 0 || relatedTags.length > 0) && (
            <div className="art-tags reader-prose">
              {article.tags.map((tag) => (
                <button
                  key={tag}
                  className={'mini-tag clickable' + (currentTag === tag ? ' cur' : '')}
                  title={`查看标签 #${tag}`}
                  onClick={() => onPickTag(tag)}
                >
                  #{tag}
                </button>
              ))}
              {relatedTags.length > 0 && <span className="rel-sep">相关</span>}
              {relatedTags.map((tag) => (
                <button
                  key={`related:${tag}`}
                  className={'mini-tag clickable rel' + (currentTag === tag ? ' cur' : '')}
                  title={`语义相近的标签 #${tag}`}
                  onClick={() => onPickTag(tag)}
                >
                  ≈ {tag}
                </button>
              ))}
            </div>
          )}

          {article.summary && (
            <Fragment>
              <div className="summary-eyebrow reader-prose">
                <Icon name="sparkles" size={13} /> AI 摘要
              </div>
              <MarkdownView
                className="summary-lead reader-prose"
                blockKey="summary"
                text={article.summary}
                anns={annotations}
                onClickHL={onClickHighlight}
              />
            </Fragment>
          )}
        </Fragment>
      )}

      {article.status === 'done' && (
        <Fragment>
          {loadingDetail && !hasSavedContent ? (
            <div className="reader-prose reader-prose-actions">
              <button className="save-content-btn" type="button" disabled>
                <Icon
                  name="loader"
                  size={13}
                  sw={1.8}
                  style={{ animation: 'spin 0.9s linear infinite' }}
                />
                读取原文中…
              </button>
            </div>
          ) : hasSavedContent ? (
            <div className="orig-content-block">
              <div className="orig-content-head reader-prose">
                {contentEdit ? (
                  <div className="summary-eyebrow content-edit-heading">
                    <Icon name="stack" size={13} />
                    原文
                    {contentSource === 'user' && (
                      <span className="content-source-badge">已编辑</span>
                    )}
                  </div>
                ) : (
                  <button
                    type="button"
                    className={'orig-content-toggle summary-eyebrow' + (contentOpen ? ' open' : '')}
                    aria-expanded={contentOpen}
                    aria-controls="orig-content-body"
                    onClick={onToggleContent}
                  >
                    <Icon name="chevron" size={13} sw={2.2} />
                    <Icon
                      name={contentLanguageView === 'translation' ? 'translate' : 'stack'}
                      size={13}
                    />
                    {contentLanguageView === 'translation' ? '中文译文' : '原文'}
                    {contentSource === 'user' && (
                      <span className="content-source-badge">已编辑</span>
                    )}
                    {!contentOpen && (
                      <span className="orig-content-hint">约 {readMinutes} 分钟 · 点击展开</span>
                    )}
                  </button>
                )}
                {contentOpen && (
                  <div className="orig-content-actions">
                    {contentEdit ? (
                      <>
                        <button
                          type="button"
                          className="replace-content-btn translate-content-btn"
                          onClick={onSaveContentEdit}
                          disabled={contentEdit.saving || contentEdit.draft === contentEdit.initial}
                        >
                          <Icon
                            name={contentEdit.saving ? 'loader' : 'check'}
                            size={13}
                            style={contentEdit.saving
                              ? { animation: 'spin 0.9s linear infinite' }
                              : undefined}
                          />
                          <span>{contentEdit.saving ? '保存中…' : '保存'}</span>
                        </button>
                        <button
                          type="button"
                          className="replace-content-btn"
                          onClick={onCancelContentEdit}
                          disabled={contentEdit.saving}
                        >
                          <Icon name="close" size={13} />
                          <span>取消</span>
                        </button>
                      </>
                    ) : (
                      <>
                        {fullTranslation?.status === 'done' && fullTranslation.translated_text && (
                          <div
                            className="content-view-switch language-view-switch"
                            role="group"
                            aria-label="阅读语言"
                          >
                            <button
                              type="button"
                              className={contentLanguageView === 'source' ? 'active' : ''}
                              onClick={() => onSetContentLanguageView('source')}
                            >
                              原文
                            </button>
                            <button
                              type="button"
                              className={contentLanguageView === 'translation' ? 'active' : ''}
                              onClick={() => onSetContentLanguageView('translation')}
                            >
                              中文译文
                            </button>
                          </div>
                        )}
                        {contentLanguageView === 'source' &&
                          savedContent?.format === 'markdown' &&
                          savedContent.document && (
                            <div
                              className="content-view-switch"
                              role="group"
                              aria-label="原文显示方式"
                            >
                              <button
                                type="button"
                                className={contentView === 'structured' ? 'active' : ''}
                                onClick={() => onSetContentView('structured')}
                              >
                                排版
                              </button>
                              <button
                                type="button"
                                className={contentView === 'plain' ? 'active' : ''}
                                onClick={() => onSetContentView('plain')}
                              >
                                文本
                              </button>
                            </div>
                          )}
                        {!fullTranslation && (
                          <button
                            type="button"
                            className="replace-content-btn translate-content-btn"
                            onClick={() => onTranslateFull(false)}
                            disabled={translationsLoading}
                            aria-label={translationsLoading
                              ? '读取译文中…'
                              : hasStaleFullTranslation
                                ? '更新全文翻译'
                                : '翻译全文'}
                          >
                            <Icon
                              name={translationsLoading ? 'loader' : 'translate'}
                              size={13}
                              style={translationsLoading
                                ? { animation: 'spin 0.9s linear infinite' }
                                : undefined}
                            />
                            <span>
                              {translationsLoading
                                ? '读取译文中…'
                                : hasStaleFullTranslation
                                  ? '更新翻译'
                                  : '翻译全文'}
                            </span>
                          </button>
                        )}
                        {(fullTranslation?.status === 'pending' ||
                          fullTranslation?.status === 'processing') && (
                          <button
                            type="button"
                            className="replace-content-btn translate-content-btn"
                            disabled
                            aria-label="全文翻译中…"
                          >
                            <Icon
                              name="loader"
                              size={13}
                              style={{ animation: 'spin 0.9s linear infinite' }}
                            />
                            <span>全文翻译中…</span>
                          </button>
                        )}
                        {fullTranslation?.status === 'failed' && (
                          <button
                            type="button"
                            className="replace-content-btn translate-content-btn translation-retry"
                            onClick={() => onTranslateFull(true)}
                            aria-label="重试全文翻译"
                          >
                            <Icon name="refresh" size={13} />
                            <span>重试全文翻译</span>
                          </button>
                        )}
                        {fullTranslation?.status === 'done' && (
                          <button
                            type="button"
                            className="replace-content-btn icon-action-btn"
                            title="重新翻译全文"
                            aria-label="重新翻译全文"
                            onClick={() => onTranslateFull(true)}
                          >
                            <Icon name="refresh" size={13} />
                          </button>
                        )}
                        {canEditContent && (
                          <button
                            type="button"
                            className="replace-content-btn"
                            onClick={onEnterContentEdit}
                            title="编辑已保存原文"
                            aria-label="编辑已保存原文"
                          >
                            <Icon name="pencil" size={13} />
                            <span>编辑</span>
                          </button>
                        )}
                        <button
                          type="button"
                          className="replace-content-btn"
                          title="重新抓取并替换原文"
                          onClick={() => onReplaceContent(article.id)}
                          disabled={savingContent === article.id}
                        >
                          <Icon
                            name={savingContent === article.id ? 'loader' : 'refresh'}
                            size={13}
                            style={savingContent === article.id
                              ? { animation: 'spin 0.9s linear infinite' }
                              : undefined}
                          />
                          <span>{savingContent === article.id ? '抓取中…' : '重新抓取'}</span>
                        </button>
                      </>
                    )}
                  </div>
                )}
              </div>
              {fullTranslation?.status === 'failed' && fullTranslation.error_msg && (
                <p className="translation-inline-error reader-prose" role="alert">
                  <Icon name="alert" size={13} /> {fullTranslation.error_msg}
                  {!contentOpen && (
                    <button type="button" className="link-btn" onClick={onExpandContent}>
                      展开重试
                    </button>
                  )}
                </p>
              )}
              <div id="orig-content-body" hidden={!contentOpen}>
                {contentOpen && (
                  <>
                    {contentEdit ? (
                      <div className="content-edit-surface">
                        <textarea
                          ref={editTextareaRef}
                          className="content-edit-textarea"
                          value={contentEdit.draft}
                          onChange={(event) => onUpdateEditDraft(event.target.value)}
                          aria-label="编辑已保存原文"
                          aria-describedby={contentEdit.error ? 'content-edit-error' : undefined}
                          spellCheck={contentEdit.format === 'markdown'}
                        />
                        {contentEdit.error && (
                          <p id="content-edit-error" className="content-edit-error" role="alert">
                            <Icon name="alert" size={13} />
                            {contentEditErrorMessage(contentEdit.error)}
                          </p>
                        )}
                      </div>
                    ) : contentLanguageView === 'translation' &&
                      fullTranslation?.status === 'done' &&
                      fullTranslation.translated_text ? (
                        fullTranslation.source_format === 'markdown' ? (
                          <MarkdownView
                            className="orig-content translated-content reader-flow"
                            blockKey="content-translation"
                            text={fullTranslation.translated_text}
                            anns={NO_ANNOTATIONS}
                            onClickHL={onClickHighlight}
                            headingIdPrefix="toc"
                            onHeadings={onHeadings}
                          />
                        ) : (
                          <PlainTextView
                            className="orig-content translated-content reader-prose"
                            blockKey="content-translation"
                            text={fullTranslation.translated_text}
                            anns={NO_ANNOTATIONS}
                            onClickHL={onClickHighlight}
                          />
                        )
                      ) : contentLoading ? (
                        <p className="inline-state reader-prose" aria-live="polite">
                          <Icon
                            name="loader"
                            size={14}
                            sw={2}
                            style={{ animation: 'spin 0.9s linear infinite' }}
                          />
                          读取原文中…
                        </p>
                      ) : contentFailed ? (
                        <p className="inline-state err reader-prose" role="alert">
                          <Icon name="alert" size={14} sw={2} />
                          原文读取失败
                          <button className="link-btn" onClick={onLoadContent}>
                            重试
                          </button>
                        </p>
                      ) : savedContent === null ? null : savedContent.format === 'markdown' &&
                        savedContent.document &&
                        contentView === 'structured' ? (
                          <MarkdownView
                            className="orig-content reader-flow"
                            blockKey="content-document"
                            text={savedContent.document}
                            anns={annotations}
                            onClickHL={onClickHighlight}
                            headingIdPrefix="toc"
                            onHeadings={onHeadings}
                          />
                        ) : (
                          <PlainTextView
                            className="orig-content reader-prose"
                            blockKey="content"
                            text={savedContent.content}
                            anns={annotations}
                            onClickHL={onClickHighlight}
                          />
                        )}
                  </>
                )}
              </div>
            </div>
          ) : (
            <div className="reader-prose reader-prose-actions">
              <button
                className="save-content-btn"
                onClick={() => onSaveContent(article.id)}
                disabled={savingContent === article.id}
              >
                <Icon name="globe" size={13} sw={1.8} />
                {savingContent === article.id ? '保存原文中…' : '保存原文'}
              </button>
            </div>
          )}
        </Fragment>
      )}

      {selectionTranslations.length > 0 && (
        <section
          className="selection-translations reader-prose"
          aria-labelledby="selection-translations-title"
        >
          <div className="summary-eyebrow" id="selection-translations-title">
            <Icon name="translate" size={13} /> 选段译文
            <span className="translation-count">{selectionTranslations.length}</span>
          </div>
          <div className="selection-translation-list">
            {selectionTranslations.map((item) => (
              <article className="selection-translation-item" key={item.id}>
                <p className="translation-source" lang="und">{item.source_text}</p>
                {item.status === 'done' && item.translated_text ? (
                  <div className="translation-result-row">
                    <p className="translation-result" lang="zh-CN">{item.translated_text}</p>
                    <button
                      type="button"
                      className="translation-icon-btn"
                      title="复制译文"
                      aria-label="复制译文"
                      onClick={() => onCopyTranslation(item.translated_text as string)}
                    >
                      <Icon name="copy" size={14} />
                    </button>
                  </div>
                ) : item.status === 'failed' ? (
                  <p className="translation-status error" role="alert">
                    <Icon name="alert" size={13} />{' '}
                    {item.error_msg || '翻译失败，请重新选择文本后重试'}
                  </p>
                ) : (
                  <p className="translation-status" aria-live="polite">
                    <Icon
                      name="loader"
                      size={13}
                      style={{ animation: 'spin 0.9s linear infinite' }}
                    />
                    正在翻译…
                  </p>
                )}
              </article>
            ))}
          </div>
        </section>
      )}

      {historicalTranslations.length > 0 && (
        <section
          className="selection-translations translation-history reader-prose"
          aria-labelledby="translation-history-title"
        >
          <div className="summary-eyebrow" id="translation-history-title">
            <Icon name="clock" size={13} /> 历史译文
            <span className="translation-count">{historicalTranslations.length}</span>
          </div>
          <div className="selection-translation-list">
            {historicalTranslations.map((item) => (
              <article
                className="selection-translation-item"
                data-translation-source-state="stale"
                key={item.id}
              >
				<p className="translation-history-state stale">已过期译文</p>
                <p className="translation-source" lang="und">{item.source_text}</p>
                {item.translated_text ? (
                  <p className="translation-result" lang="zh-CN">{item.translated_text}</p>
                ) : (
                  <p className="translation-status">历史任务未产出译文</p>
                )}
              </article>
            ))}
          </div>
        </section>
      )}

      {article.description && (
        <p className="art-note reader-prose">
          <Icon name="pencil" size={12} sw={1.8} /> {article.description}
        </p>
      )}

      <div ref={notesRef}>
        <AnnotationsList
          anns={annotations}
          onOpen={onOpenAnnotation}
          onDelete={onRemoveAnnotation}
        />
      </div>

      {historicalAnnotations.length > 0 && (
        <section
          className="selection-translations translation-history reader-prose"
          aria-labelledby="historical-annotations-title"
        >
          <button
            type="button"
            className="summary-eyebrow"
            aria-expanded={historicalOpen}
            aria-controls="historical-annotations-body"
            onClick={() => setHistoricalOpen((open) => !open)}
          >
            <Icon name="clock" size={13} />
            <span id="historical-annotations-title">已归档想法 ({historicalAnnotations.length})</span>
          </button>
          <div id="historical-annotations-body" hidden={!historicalOpen}>
            {historicalOpen && (
              <div className="selection-translation-list">
                {historicalAnnotations.map((item) => {
                  const label = `${item.annotation.text}${item.annotation.note ? `：${item.annotation.note}` : ''}`
                  const content = (
                    <>
                      <span className="translation-history-state">正文 v{item.sourceContentRevision} · {item.reason}</span>
                      <span className="translation-source">{item.annotation.text}</span>
                      {item.annotation.note && <span className="translation-result">{item.annotation.note}</span>}
                    </>
                  )
                  return onOpenHistoricalAnnotation ? (
                    <button
                      type="button"
                      className="selection-translation-item"
                      aria-label={label}
                      key={`${item.sourceKey ?? item.sourceContentRevision}:${item.annotation.id}`}
                      onClick={() => onOpenHistoricalAnnotation(item.annotation)}
                    >
                      {content}
                    </button>
                  ) : (
                    <article
                      className="selection-translation-item"
                      key={`${item.sourceKey ?? item.sourceContentRevision}:${item.annotation.id}`}
                    >
                      {content}
                    </article>
                  )
                })}
              </div>
            )}
          </div>
        </section>
      )}

      <ArticlePager previous={previous} next={next} />
    </div>
  )
}
