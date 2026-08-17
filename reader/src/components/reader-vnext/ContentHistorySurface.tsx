import { useCallback, useEffect, useRef, useState } from 'react'
import type { IdentityBoundReaderClient } from '../../lib/api/client'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import type { ReaderContentHistoryResponse } from '../../lib/api/types'
import type { ReaderRoute } from '../../lib/navigation/route'
import { Icon } from '../Icon'
import { readerErrorMessage, formatRelativeDate } from '../../lib/reader-surface'
import { SurfaceError, SurfaceLoading, SurfaceShell } from './SurfaceShell'

export interface ContentHistorySurfaceProps {
  readonly client: IdentityBoundReaderClient
  readonly linkID: string
  readonly expectedContentRevision: number
  readonly onNavigate: (route: ReaderRoute) => void
  readonly onRestored: (revision: number) => void
  readonly capabilityPolicy: ReaderCapabilityPolicy
}

function snapshotText(item: ReaderContentHistoryResponse): string {
  return item.content_document || item.content || ''
}

function snapshotPreview(item: ReaderContentHistoryResponse): string {
  const text = snapshotText(item).replace(/\s+/g, ' ').trim()
  if (!text) return '没有可预览的正文快照。'
  return text.length > 360 ? `${text.slice(0, 360)}…` : text
}

function validRevision(value: number): boolean {
  return Number.isInteger(value) && value >= 0
}

export function ContentHistorySurface({
  client,
  linkID,
  expectedContentRevision,
  onNavigate,
  onRestored,
  capabilityPolicy,
}: ContentHistorySurfaceProps) {
  const [items, setItems] = useState<ReaderContentHistoryResponse[]>([])
  const [loading, setLoading] = useState(true)
  const [busyID, setBusyID] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [currentRevision, setCurrentRevision] = useState(expectedContentRevision)
  const requestID = useRef(0)
  const restoreRequestID = useRef(0)
  const currentRevisionRef = useRef(expectedContentRevision)
  const expectedRevisionRef = useRef(expectedContentRevision)
  const dataClientRef = useRef(client)
  const dataLinkRef = useRef(linkID)

  currentRevisionRef.current = currentRevision
  expectedRevisionRef.current = expectedContentRevision

  const identityCurrent = client.isIdentityCurrent()
  const dataOwnerCurrent = dataClientRef.current === client && dataLinkRef.current === linkID
  const visibleItems = identityCurrent && dataOwnerCurrent ? items : []
  const visibleError = identityCurrent && dataOwnerCurrent ? error : null
  const visibleLoading = identityCurrent && dataOwnerCurrent ? loading : true

  useEffect(() => {
    const nextRevision = Math.max(currentRevisionRef.current, expectedContentRevision)
    currentRevisionRef.current = nextRevision
    setCurrentRevision(nextRevision)
  }, [expectedContentRevision])

  useEffect(() => {
    dataClientRef.current = client
    dataLinkRef.current = linkID
    requestID.current += 1
    restoreRequestID.current += 1
    const nextRevision = expectedRevisionRef.current
    currentRevisionRef.current = nextRevision
    setItems([])
    setLoading(true)
    setBusyID(null)
    setError(null)
    setCurrentRevision(nextRevision)

    return () => {
      requestID.current += 1
      restoreRequestID.current += 1
    }
  }, [client, linkID])

  useEffect(() => {
    if (currentRevision > 0) return
    let cancelled = false
    void client.getLink(linkID).then((result) => {
      if (cancelled || !client.isIdentityCurrent()) return
      if (result.ok && typeof result.data.content_revision === 'number' && validRevision(result.data.content_revision)) {
        const nextRevision = Math.max(currentRevisionRef.current, result.data.content_revision)
        currentRevisionRef.current = nextRevision
        setCurrentRevision(nextRevision)
      } else if (!result.ok) {
        setError(readerErrorMessage(result.error))
      }
    }).catch((requestError: unknown) => {
      if (!cancelled && client.isIdentityCurrent()) {
        setError(requestError instanceof Error && requestError.message ? requestError.message : '读取当前正文版本失败，请稍后重试。')
      }
    })
    return () => { cancelled = true }
  }, [client, currentRevision, linkID])

  const load = useCallback(async () => {
    const request = ++requestID.current
    setLoading(true)
    setError(null)
    try {
      const result = await client.listContentHistory(linkID)
      if (request !== requestID.current || !client.isIdentityCurrent()) return
      if (result.ok) setItems(result.data)
      else setError(readerErrorMessage(result.error))
    } catch (requestError: unknown) {
      if (request === requestID.current && client.isIdentityCurrent()) {
        setError(requestError instanceof Error && requestError.message ? requestError.message : '正文历史加载失败，请稍后重试。')
      }
    } finally {
      if (request === requestID.current && client.isIdentityCurrent()) setLoading(false)
    }
  }, [client, linkID])

  useEffect(() => {
    void load()
  }, [load])

  const restore = useCallback(async (item: ReaderContentHistoryResponse) => {
    const expectedRevision = currentRevisionRef.current
    if (busyID !== null || expectedRevision <= 0 || item.revision === expectedRevision) return
    const request = ++restoreRequestID.current
    setBusyID(item.id)
    setError(null)
    try {
      const result = await client.restoreContentHistory(linkID, item.id, {
        expected_content_revision: expectedRevision,
      })
      if (request !== restoreRequestID.current || !client.isIdentityCurrent()) return
      if (!result.ok) {
        setError(readerErrorMessage(result.error))
        return
      }
      if (result.data.link_id !== linkID) {
        setError('恢复响应的链接不匹配，请刷新后重试。')
        return
      }
      const responseRevision = result.data.content_revision
      if (!validRevision(responseRevision) || responseRevision <= currentRevisionRef.current) {
        setError('恢复响应的正文版本没有前进，请刷新后重试。')
        return
      }
      currentRevisionRef.current = responseRevision
      setCurrentRevision((previous) => Math.max(previous, responseRevision))
      onRestored(responseRevision)
      await load()
    } catch (requestError: unknown) {
      if (request === restoreRequestID.current && client.isIdentityCurrent()) {
        setError(requestError instanceof Error && requestError.message ? requestError.message : '正文恢复失败，请稍后重试。')
      }
    } finally {
      if (request === restoreRequestID.current && client.isIdentityCurrent()) setBusyID(null)
    }
  }, [busyID, client, linkID, load, onRestored])

  return (
    <SurfaceShell
      title="正文历史"
      subtitle={`链接 ${linkID}`}
      onNavigate={onNavigate}
      capabilityPolicy={capabilityPolicy}
      onBack={() => onNavigate({ kind: 'library', id: 'reading' })}
      actions={(
        <button className="rvx-button secondary" type="button" disabled={visibleLoading || busyID !== null} onClick={() => void load()}>
          <Icon name="refresh" size={15} />刷新
        </button>
      )}
    >
      {visibleError && <SurfaceError message={visibleError} onRetry={() => void load()} />}
      {visibleLoading && visibleItems.length === 0 ? <SurfaceLoading /> : visibleItems.length === 0 ? (
        <div className="rvx-empty">
          <Icon name="clock" size={24} />
          <h2>没有正文历史</h2>
          <p>保存正文的新版本后，历史快照会显示在这里。</p>
        </div>
      ) : (
        <section className="rvx-history-panel rvx-content-history" aria-label="正文历史版本">
          <div className="rvx-history-current">
            当前正文版本 <strong>{currentRevision > 0 ? currentRevision : '读取中'}</strong>
          </div>
          <ul>
            {visibleItems.map((item) => {
              const current = currentRevision > 0 && item.revision === currentRevision
              const busy = busyID === item.id
              return (
                <li key={item.id}>
                  <div className="rvx-history-copy">
                    <strong>版本 {item.revision}</strong>
                    <small>{formatRelativeDate(item.created_at)} · {item.content_source} · {item.content_format}</small>
                    <p>{snapshotPreview(item)}</p>
                  </div>
                  <div className="rvx-history-actions">
                    <details>
                      <summary>查看</summary>
                      <pre>{snapshotText(item) || '没有正文内容'}</pre>
                    </details>
                    <button
                      className="rvx-button secondary"
                      type="button"
                      disabled={current || currentRevision <= 0 || busyID !== null}
                      onClick={() => void restore(item)}
                    >
                      <Icon name="refresh" size={14} />{busy ? '恢复中…' : current ? '当前版本' : '恢复'}
                    </button>
                  </div>
                </li>
              )
            })}
          </ul>
        </section>
      )}
    </SurfaceShell>
  )
}
