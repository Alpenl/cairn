import { useMemo } from 'react'

import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { LinkResponse } from '../../lib/api/types'
import { Icon } from '../Icon'
import {
  compareReaderActivityLastAtDesc,
  useReaderActivity,
} from '../../hooks/useReaderActivity'
import { useReaderRelatedTags } from '../../hooks/useReaderRelatedTags'

export interface ReaderSemanticTagsPanelProps {
  link: LinkResponse | null | undefined
  corpus: LinkResponse[]
  client?: IdentityBoundReaderClient
  onPickTag?: (tag: string) => void
}

function relatedTagsStatus(mode: 'semantic' | 'cooccurrence' | 'local'): string {
  switch (mode) {
    case 'semantic':
      return '语义'
    case 'cooccurrence':
      return '共现降级'
    case 'local':
      return '本地近似'
  }
}

/**
 * A small, independently mountable consumer for semantic related tags.
 * DetailPane can keep its existing compact tag rail while other Reader
 * surfaces use this panel when they need the source/degradation metadata.
 */
export function ReaderSemanticTagsPanel({
  link,
  corpus,
  client,
  onPickTag,
}: ReaderSemanticTagsPanelProps) {
  const state = useReaderRelatedTags(link, corpus, client)

  return (
    <section className="rvx-signals-panel" aria-label="相关标签">
      <div className="rvx-section-head">
        <h3>相关标签</h3>
        <div className="rvx-header-actions">
          <span data-testid="related-tags-mode">{relatedTagsStatus(state.mode)}</span>
          {state.error && (
            <button
              className="rvx-icon-button"
              type="button"
              aria-label="重试相关标签"
              title="重试相关标签"
              disabled={state.loading}
              onClick={() => void state.reload()}
            >
              <Icon name="refresh" size={15} />
            </button>
          )}
        </div>
      </div>
      {state.loading && state.tags.length === 0 ? (
        <p role="status">加载中</p>
      ) : state.tags.length === 0 ? (
        <p className="rvx-muted">暂无相关标签。</p>
      ) : (
        <ul className="rvx-signal-list">
          {state.tags.map((tag) => (
            <li key={tag}>
              <button type="button" onClick={() => onPickTag?.(tag)}>#{tag}</button>
            </li>
          ))}
        </ul>
      )}
      {state.model && <small data-testid="related-tags-model">{state.model}</small>}
      {state.degraded && (
        <small role="status" data-testid="related-tags-degraded">
          当前结果不是完整语义召回
        </small>
      )}
    </section>
  )
}

export interface ReaderActivityPanelProps {
  links: LinkResponse[]
  client?: IdentityBoundReaderClient
}

function activityRows(
  values: ReadonlyMap<string, string>,
): Array<{ name: string; lastAt: string }> {
  return [...values.entries()]
    .map(([name, lastAt]) => ({ name, lastAt }))
    .sort((left, right) => compareReaderActivityLastAtDesc(left.lastAt, right.lastAt) || left.name.localeCompare(right.name, 'zh'))
}

/** A standalone activity consumer with an explicit local fallback indicator. */
export function ReaderActivityPanel({ links, client }: ReaderActivityPanelProps) {
  const state = useReaderActivity(client, links)
  const tags = useMemo(() => activityRows(state.tagLastAt), [state.tagLastAt])
  const domains = useMemo(() => activityRows(state.domainLastAt), [state.domainLastAt])

  return (
    <section className="rvx-signals-panel" aria-label="最近活跃">
      <div className="rvx-section-head">
        <h3>最近活跃</h3>
        <div className="rvx-header-actions">
          <span data-testid="activity-source">{state.source === 'server' ? '事件投影' : '本地近似'}</span>
          {state.error && (
            <button
              className="rvx-icon-button"
              type="button"
              aria-label="重试最近活跃"
              title="重试最近活跃"
              disabled={state.loading}
              onClick={() => void state.reload()}
            >
              <Icon name="refresh" size={15} />
            </button>
          )}
        </div>
      </div>
      {state.loading && tags.length === 0 && domains.length === 0 ? (
        <p role="status">加载中</p>
      ) : tags.length === 0 && domains.length === 0 ? (
        <p className="rvx-muted">暂无活跃记录。</p>
      ) : (
        <>
          <ul className="rvx-signal-list" aria-label="活跃标签">
            {tags.map((item) => (
              <li key={`tag:${item.name}`}>
                <span>#{item.name}</span>
                <time dateTime={item.lastAt}>{item.lastAt}</time>
              </li>
            ))}
          </ul>
          <ul className="rvx-signal-list" aria-label="活跃域名">
            {domains.map((item) => (
              <li key={`domain:${item.name}`}>
                <span>{item.name}</span>
                <time dateTime={item.lastAt}>{item.lastAt}</time>
              </li>
            ))}
          </ul>
        </>
      )}
      {state.degraded && (
        <small role="status" data-testid="activity-degraded">
          服务端事件不可用，已使用当前链接时间近似
        </small>
      )}
    </section>
  )
}
