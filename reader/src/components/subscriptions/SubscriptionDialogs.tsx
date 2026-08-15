import { useEffect, useState, type FormEvent } from 'react'
import { Icon } from '../Icon'
import type { ApiResult } from '../../lib/api/result'
import type { DiscoveredFeed, DiscoverFeedsResponse, FeedFolder } from '../../lib/api/types'

function discoveredURL(feed: DiscoveredFeed): string {
  return feed.feed_url ?? feed.url ?? ''
}

interface AddSubscriptionDialogProps {
  open: boolean
  folders: FeedFolder[]
  onClose: () => void
  onDiscover: (url: string) => Promise<ApiResult<DiscoverFeedsResponse>>
  onSubscribe: (url: string, folderID: string | null) => Promise<ApiResult<true>>
}

export function AddSubscriptionDialog({
  open,
  folders,
  onClose,
  onDiscover,
  onSubscribe,
}: AddSubscriptionDialogProps) {
  const [url, setURL] = useState('')
  const [folderID, setFolderID] = useState('')
  const [feeds, setFeeds] = useState<DiscoveredFeed[]>([])
  const [discovering, setDiscovering] = useState(false)
  const [subscribingURL, setSubscribingURL] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!open) return
    setURL('')
    setFolderID('')
    setFeeds([])
    setError(null)
    setDiscovering(false)
    setSubscribingURL(null)
  }, [open])

  useEffect(() => {
    if (!open) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose, open])

  if (!open) return null

  const discover = async (event: FormEvent) => {
    event.preventDefault()
    const candidate = url.trim()
    if (!candidate) return
    setDiscovering(true)
    setError(null)
    const result = await onDiscover(candidate)
    setDiscovering(false)
    if (!result.ok) {
      setFeeds([])
      setError(result.error.message)
      return
    }
    setFeeds(result.data.feeds)
    if (result.data.feeds.length === 0) setError('这个网页没有发现 RSS 或 Atom 订阅源')
  }

  const subscribe = async (feedURL: string) => {
    setSubscribingURL(feedURL)
    setError(null)
    const result = await onSubscribe(feedURL, folderID || null)
    setSubscribingURL(null)
    if (!result.ok) {
      setError(result.error.message)
      return
    }
    onClose()
  }

  return (
    <div className="rss-dialog-overlay" role="presentation" onMouseDown={(event) => {
      if (event.target === event.currentTarget) onClose()
    }}>
      <section className="rss-dialog" role="dialog" aria-modal="true" aria-labelledby="add-feed-title">
        <header>
          <div>
            <h2 id="add-feed-title">添加订阅</h2>
            <p>粘贴 RSS、Atom 地址或网站首页</p>
          </div>
          <button type="button" onClick={onClose} aria-label="关闭添加订阅">
            <Icon name="close" size={16} />
          </button>
        </header>
        <form onSubmit={discover} className="rss-dialog-body">
          <label className="rss-field">
            <span>网站或订阅源地址</span>
            <div className="rss-url-input">
              <Icon name="globe" size={15} />
              <input
                type="url"
                required
                autoFocus
                value={url}
                onChange={(event) => {
                  setURL(event.target.value)
                  setFeeds([])
                }}
                placeholder="https://example.com/feed.xml"
              />
            </div>
          </label>
          <label className="rss-field">
            <span>文件夹</span>
            <select value={folderID} onChange={(event) => setFolderID(event.target.value)}>
              <option value="">未分组</option>
              {folders.map((folder) => (
                <option key={folder.id} value={folder.id}>{folder.name}</option>
              ))}
            </select>
          </label>
          {error && <div className="rss-dialog-error"><Icon name="alert" size={14} /> {error}</div>}
          {feeds.length > 0 && (
            <div className="rss-discovery-results" aria-live="polite">
              {feeds.map((feed) => {
                const feedURL = discoveredURL(feed)
                return (
                  <div className="rss-discovery-row" key={feedURL}>
                    <span className="rss-feed-mark"><Icon name="rss" size={13} /></span>
                    <div>
                      <strong>{feed.title?.trim() || 'RSS 订阅源'}</strong>
                      <span>{feedURL}</span>
                    </div>
                    <button
                      type="button"
                      className="rss-primary-button"
                      onClick={() => void subscribe(feedURL)}
                      disabled={subscribingURL !== null}
                    >
                      {subscribingURL === feedURL ? '订阅中' : '订阅'}
                    </button>
                  </div>
                )
              })}
            </div>
          )}
          <footer>
            <button type="button" className="rss-secondary-button" onClick={onClose}>取消</button>
            <button type="submit" className="rss-primary-button" disabled={discovering || !url.trim()}>
              {discovering ? <span className="spinning"><Icon name="loader" size={14} /></span> : <Icon name="search" size={14} />}
              {discovering ? '正在查找' : '查找订阅源'}
            </button>
          </footer>
        </form>
      </section>
    </div>
  )
}

interface FolderDialogProps {
  open: boolean
  folder?: FeedFolder
  onClose: () => void
  onSave: (name: string) => Promise<ApiResult<true>>
}

export function FolderDialog({ open, folder, onClose, onSave }: FolderDialogProps) {
  const [name, setName] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!open) return
    setName(folder?.name ?? '')
    setSaving(false)
    setError(null)
  }, [open, folder])

  useEffect(() => {
    if (!open) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose, open])

  if (!open) return null

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    const nextName = name.trim()
    if (!nextName) return
    setSaving(true)
    setError(null)
    const result = await onSave(nextName)
    setSaving(false)
    if (!result.ok) {
      setError(result.error.message)
      return
    }
    onClose()
  }

  return (
    <div className="rss-dialog-overlay" role="presentation" onMouseDown={(event) => {
      if (event.target === event.currentTarget) onClose()
    }}>
      <section className="rss-dialog compact" role="dialog" aria-modal="true" aria-labelledby="folder-dialog-title">
        <header>
          <div><h2 id="folder-dialog-title">{folder ? '重命名文件夹' : '新建文件夹'}</h2></div>
          <button type="button" onClick={onClose} aria-label="关闭文件夹对话框"><Icon name="close" size={16} /></button>
        </header>
        <form className="rss-dialog-body" onSubmit={submit}>
          <label className="rss-field">
            <span>名称</span>
            <input autoFocus value={name} onChange={(event) => setName(event.target.value)} maxLength={128} />
          </label>
          {error && <div className="rss-dialog-error"><Icon name="alert" size={14} /> {error}</div>}
          <footer>
            <button type="button" className="rss-secondary-button" onClick={onClose}>取消</button>
            <button type="submit" className="rss-primary-button" disabled={saving || !name.trim()}>
              {saving ? '保存中' : '保存'}
            </button>
          </footer>
        </form>
      </section>
    </div>
  )
}
