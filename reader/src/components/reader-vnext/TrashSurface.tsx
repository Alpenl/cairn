/**
 * 回收站：链接 / 收件箱条目 / 笔记三类已删内容的统一入口。
 *
 * 三类共用一个列表，是因为后端本来就是这么建模的：/api/trash 对三张表的
 * deleted_at 做 UNION，分页游标编码 (trashed_at, host_kind, host_id) 三元组——
 * 跨类型的复合游标只有为混合列表才有意义。删除时什么都没搬家，只是置了
 * deleted_at，所以「回收站」是一个视图而不是一个仓库。
 *
 * 更要紧的是这样贴合真实问题：找回东西时人想的是「我刚才误删了什么」，
 * 不是「我要去笔记回收站」。按删除时间倒序的混合列表正是这个念头的形状。
 *
 * 两条不能松的界线：
 *   - 恢复的去向逐类不同（笔记回它所属文章、链接回阅读库、收件箱条目回收件箱），
 *     所以每行都写明归处；否则点下去东西跑哪儿全靠猜。
 *   - 删除可以没有确认（进回收站是可逆的），永久清除必须逐条确认——它才是那个
 *     不可逆的动作。也因此这里没有「全部清空」：一次点击同时销毁三类东西，
 *     其中很可能混着只是想暂时收起来的收件箱条目。
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Icon, type IconName } from '../Icon'
import { SurfaceShell, SurfaceLoading, formatRelativeDate, identityIsCurrent, isIdentityError, SURFACE_IDENTITY_ERROR } from './SurfaceShell'
import type { ReaderClient } from '../../lib/api/client'
import type { ReaderTrashItemResponse, ReaderHostKind } from '../../lib/api/types'
import type { ReaderRoute } from '../../lib/navigation/route'
import type { ReaderCapabilityPolicy } from '../../lib/capabilities'
import { invalidateLibrary, invalidateLink } from '../../lib/cache/invalidate'

const PAGE_SIZE = 50

type KindFilter = 'all' | ReaderHostKind

const FILTERS: ReadonlyArray<{ readonly id: KindFilter; readonly label: string }> = [
  { id: 'all', label: '全部' },
  { id: 'link', label: '链接' },
  { id: 'inbox', label: '收件箱' },
  { id: 'note', label: '笔记' },
]

/** 每类的展示标签、图标，以及「恢复后去哪」——后者是这个混合列表的关键信息。 */
const KIND_META: Record<ReaderHostKind, { readonly label: string; readonly icon: IconName; readonly restoresTo: string }> = {
  link: { label: '链接', icon: 'doc', restoresTo: '恢复到阅读库' },
  inbox: { label: '收件箱', icon: 'inbox', restoresTo: '恢复到收件箱' },
  note: { label: '笔记', icon: 'edit', restoresTo: '恢复到所属文章' },
}

export interface TrashSurfaceProps {
  readonly client: ReaderClient
  readonly onNavigate: (route: ReaderRoute) => void
  readonly capabilityPolicy: ReaderCapabilityPolicy
  readonly onToast: (msg: string, icon?: IconName) => void
}

export function TrashSurface({ client, onNavigate, capabilityPolicy, onToast }: TrashSurfaceProps) {
  const [filter, setFilter] = useState<KindFilter>('all')
  const [items, setItems] = useState<ReaderTrashItemResponse[]>([])
  const [count, setCount] = useState<number | null>(null)
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [busyID, setBusyID] = useState<string | null>(null)
  // 过滤器切换会让上一页请求的结果变得陈旧；只认最后一次请求的回包。
  const requestSeq = useRef(0)

  const load = useCallback(async (append: boolean, after?: string, kind: KindFilter = filter) => {
    if (!identityIsCurrent(client)) {
      setError(SURFACE_IDENTITY_ERROR)
      return
    }
    const seq = ++requestSeq.current
    setLoading(true)
    const result = await client.listTrash({
      hostKind: kind === 'all' ? undefined : kind,
      after,
      limit: PAGE_SIZE,
    })
    if (seq !== requestSeq.current) return
    setLoading(false)
    if (!result.ok) {
      setError(isIdentityError(result.error) ? SURFACE_IDENTITY_ERROR : result.error.message)
      return
    }
    setError(null)
    setCount(result.data.count)
    setCursor(result.data.next_cursor || undefined)
    setItems((current) => append ? [...current, ...result.data.items] : result.data.items)
  }, [client, filter])

  useEffect(() => {
    void load(false, undefined, filter)
  }, [filter, load])

  const switchFilter = useCallback((next: KindFilter) => {
    if (next === filter) return
    setItems([])
    setCount(null)
    setCursor(undefined)
    setFilter(next)
  }, [filter])

  /** 恢复后把该行摘掉。链接还要失效库缓存，否则阅读库里看不到它回来。 */
  const restore = useCallback(async (item: ReaderTrashItemResponse) => {
    setBusyID(item.host_id)
    const result = item.host_kind === 'link'
      ? await client.restoreLink(item.host_id)
      : item.host_kind === 'note'
        ? await client.restoreNote(item.host_id)
        : await client.restoreInbox(item.host_id)
    setBusyID(null)
    if (!result.ok) {
      onToast(`恢复失败：${result.error.message}`, 'alert')
      return
    }
    if (item.host_kind === 'link') {
      invalidateLibrary()
      invalidateLink(item.host_id)
    }
    setItems((current) => current.filter((row) => row.host_id !== item.host_id))
    setCount((current) => current === null ? null : Math.max(0, current - 1))
    onToast(`已${KIND_META[item.host_kind].restoresTo}`, 'check')
  }, [client, onToast])

  /** 永久清除。这一步不可逆，所以逐条确认，并且没有批量入口。 */
  const purge = useCallback(async (item: ReaderTrashItemResponse) => {
    const label = item.title || item.url || '这一项'
    if (!window.confirm(`永久删除“${label}”？此操作不可恢复。`)) return
    setBusyID(item.host_id)
    const result = await client.purgeHost(item.host_kind, item.host_id, crypto.randomUUID())
    setBusyID(null)
    if (!result.ok) {
      onToast(`清除失败：${result.error.message}`, 'alert')
      return
    }
    if (item.host_kind === 'link') invalidateLink(item.host_id)
    setItems((current) => current.filter((row) => row.host_id !== item.host_id))
    setCount((current) => current === null ? null : Math.max(0, current - 1))
    onToast('已永久删除', 'trash')
  }, [client, onToast])

  const subtitle = useMemo(() => {
    if (error) return undefined
    if (count === null) return undefined
    return filter === 'all' ? `${count} 项` : `${FILTERS.find((f) => f.id === filter)?.label} ${count} 项`
  }, [count, error, filter])

  return (
    <SurfaceShell
      title="回收站"
      subtitle={subtitle}
      activeRoute={{ kind: 'tool', id: 'trash' }}
      onNavigate={onNavigate}
      capabilityPolicy={capabilityPolicy}
      actions={
        <div className="rvx-segmented" role="tablist" aria-label="回收站类型">
          {FILTERS.map((entry) => (
            <button
              key={entry.id}
              type="button"
              role="tab"
              aria-selected={filter === entry.id}
              className={filter === entry.id ? 'active' : ''}
              onClick={() => switchFilter(entry.id)}
            >
              {entry.label}
            </button>
          ))}
        </div>
      }
    >
      {error ? (
        <div className="rvx-empty">
          <Icon name="alert" size={24} />
          <h2>无法读取回收站</h2>
          <p>{error}</p>
          <button className="rvx-button secondary" type="button" onClick={() => void load(false)}>重试</button>
        </div>
      ) : loading && items.length === 0 ? (
        <SurfaceLoading />
      ) : items.length === 0 ? (
        <div className="rvx-empty">
          <Icon name="trash" size={24} />
          <h2>回收站为空</h2>
          <p>删除的链接、收件箱条目和笔记都会先留在这里，直到你永久清除它。</p>
        </div>
      ) : (
        <>
          <ul className="rvx-trash-list">
            {items.map((item) => {
              const meta = KIND_META[item.host_kind]
              const busy = busyID === item.host_id
              return (
                <li key={`${item.host_kind}:${item.host_id}`} className="rvx-trash-row">
                  <span className="rvx-trash-kind" title={meta.label}>
                    <Icon name={meta.icon} size={15} />
                    {meta.label}
                  </span>
                  <span className="rvx-trash-main">
                    <strong>{item.title || item.url || '未命名'}</strong>
                    <small>
                      删除于 {formatRelativeDate(item.trashed_at)} · {meta.restoresTo}
                    </small>
                  </span>
                  <span className="rvx-action-row">
                    <button
                      className="rvx-button secondary"
                      type="button"
                      disabled={busy}
                      onClick={() => void restore(item)}
                    >
                      恢复
                    </button>
                    <button
                      className="rvx-icon-button danger"
                      type="button"
                      disabled={busy}
                      title="永久删除"
                      aria-label={`永久删除 ${item.title || item.url || '这一项'}`}
                      onClick={() => void purge(item)}
                    >
                      <Icon name="trash" size={16} />
                    </button>
                  </span>
                </li>
              )
            })}
          </ul>
          {cursor && (
            <button
              className="rvx-load-more"
              type="button"
              disabled={loading}
              onClick={() => void load(true, cursor)}
            >
              更多
            </button>
          )}
        </>
      )}
    </SurfaceShell>
  )
}
