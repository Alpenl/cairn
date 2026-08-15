import { useEffect, useMemo, useState } from 'react'

import { Icon } from './Icon'
import type { ReaderClient } from '../lib/api/client'
import type { ReaderCapabilityLease } from '../lib/capabilities'
import type { LibraryReviewResponse } from '../lib/api/types'

const labels: Record<LibraryReviewResponse['kind'], string> = {
  classification_uncertain: '分类待确认',
  migration_suggestion: '迁移建议',
  note_conflict: '备注冲突',
  merge_conflict: '合并冲突',
}

export function ReviewView({
  client,
  capabilityLease,
  reviewID,
  onToast,
}: {
  client: ReaderClient
  capabilityLease: ReaderCapabilityLease
  reviewID?: string
  onToast: (message: string, icon?: import('./Icon').IconName) => void
}) {
  const [items, setItems] = useState<LibraryReviewResponse[]>([])
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState<string | null>(null)
  const selected = useMemo(
    () => reviewID ? items.find((item) => item.id === reviewID) : undefined,
    [items, reviewID],
  )

  useEffect(() => {
    if (!capabilityLease.isCurrent('siteAdvanced')) return
    void client.getLibraryReviews({ status: 'pending', limit: 100 }).then((result) => {
      if (!client.isIdentityCurrent() || !capabilityLease.isCurrent('siteAdvanced')) return
      if (result.ok) setItems(result.data)
      else onToast(`加载审核项失败：${result.error.message}`, 'alert')
      setLoading(false)
    })
  }, [capabilityLease, client, onToast])

  const resolve = async (
    item: LibraryReviewResponse,
    resolution: 'applied' | 'dismissed',
    action?: 'keep_reading' | 'move_to_site',
  ) => {
    const operationLease = capabilityLease
    if (!operationLease.isCurrent('siteAdvanced')) return
    setBusy(item.id)
    const result = await client.resolveLibraryReview(item.id, {
      expected_revision: item.revision,
      resolution,
      action,
      ...(action === 'move_to_site' ? { confirm_destructive: true } : {}),
    })
    if (!client.isIdentityCurrent() || !operationLease.isCurrent('siteAdvanced')) return
    setBusy(null)
    if (!result.ok) {
      onToast(
        result.error.errorCode === 'revision_conflict'
          ? '审核项已被其他窗口处理，请刷新'
          : `处理失败：${result.error.message}`,
        'alert',
      )
      return
    }
    setItems((current) => current.filter((currentItem) => currentItem.id !== item.id))
    onToast(
      action === 'move_to_site'
        ? '已移到网站'
        : action === 'keep_reading'
          ? '已保留在阅读'
          : resolution === 'dismissed'
            ? '已忽略审核项'
            : '审核项已标记为已应用',
      'check',
    )
  }

  const visible = selected ? [selected] : items
  return (
    <section className="review-view">
      <header className="review-heading">
        <div>
          <h1>待审核</h1>
          <span>{items.length} 项需要处理</span>
        </div>
      </header>
      {loading ? (
        <div className="sites-empty">正在加载审核项</div>
      ) : visible.length === 0 ? (
        <div className="sites-empty">当前没有待审核项</div>
      ) : (
        <div className="review-list">
          {visible.map((item) => (
            <article key={item.id} className="review-card">
              <header>
                <span>{labels[item.kind]}</span>
                <time>{new Date(item.created_at).toLocaleString()}</time>
              </header>
              <pre>{JSON.stringify(item.payload, null, 2)}</pre>
              <footer>
                {item.kind === 'migration_suggestion' ? (
                  <>
                    <button
                      type="button"
                      onClick={() => void resolve(item, 'dismissed')}
                      disabled={busy === item.id}
                    >
                      忽略建议
                    </button>
                    <button
                      type="button"
                      onClick={() => void resolve(item, 'applied', 'keep_reading')}
                      disabled={busy === item.id}
                    >
                      保留在阅读
                    </button>
                    <button
                      type="button"
                      onClick={() => {
                        if (window.confirm('移到网站会删除该阅读内容的已保存原文和译文。继续吗？')) {
                          void resolve(item, 'applied', 'move_to_site')
                        }
                      }}
                      disabled={busy === item.id}
                    >
                      {busy === item.id ? <Icon name="loader" size={14} /> : '移到网站'}
                    </button>
                  </>
                ) : (
                  <>
                    <button
                      type="button"
                      onClick={() => void resolve(item, 'dismissed')}
                      disabled={busy === item.id}
                    >
                      忽略
                    </button>
                    <button
                      type="button"
                      onClick={() => void resolve(item, 'applied')}
                      disabled={busy === item.id}
                    >
                      {busy === item.id ? <Icon name="loader" size={14} /> : '标记已应用'}
                    </button>
                  </>
                )}
              </footer>
            </article>
          ))}
        </div>
      )}
    </section>
  )
}
