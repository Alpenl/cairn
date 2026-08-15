import { useEffect, useRef, useState, type FormEvent } from 'react'
import { Icon } from './Icon'
import type { ReaderClient } from '../lib/api/client'
import type { ReaderCapabilityLease } from '../lib/capabilities'

export interface AddLinkDialogProps {
  client: ReaderClient
  capabilityLease: ReaderCapabilityLease
  destination: 'inbox' | 'library'
  onClose: () => void
  onAdded: (target: { readonly kind: 'inbox' | 'library'; readonly id: string }) => void
  onToast: (message: string, icon?: import('./Icon').IconName) => void
}

export function AddLinkDialog({ client, capabilityLease, destination, onClose, onAdded, onToast }: AddLinkDialogProps) {
  const [url, setURL] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    inputRef.current?.focus()
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && !submitting) onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose, submitting])

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    const operationLease = capabilityLease
    if (!operationLease.isCurrent() || (destination === 'inbox' && !operationLease.isCurrent('inbox'))) return
    const value = url.trim()
    try {
      const parsed = new URL(value)
      if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') throw new Error()
    } catch {
      setError('请输入有效的 http 或 https 链接')
      return
    }
    setSubmitting(true)
    setError(null)
    const result = await client.submitLink({
      url: value,
      requested_library_kind: 'auto',
      destination,
    })
    if (!client.isIdentityCurrent() || !operationLease.isCurrent() || (destination === 'inbox' && !operationLease.isCurrent('inbox'))) return
    setSubmitting(false)
    if (!result.ok) {
      setError(result.error.message)
      return
    }
    if (destination === 'inbox') {
      const inboxID = result.data.inbox_id?.trim()
      if (!inboxID) {
        setError('服务端没有返回收件箱条目标识')
        return
      }
      onToast('链接已加入收件箱', 'inbox')
      onAdded({ kind: 'inbox', id: inboxID })
      return
    }
    const linkID = result.data.link_id?.trim()
    if (!linkID) {
      setError('服务端没有返回资料库链接身份')
      return
    }
    onToast('链接已加入资料库', 'check')
    onAdded({ kind: 'library', id: linkID })
  }

  return (
    <div className="add-link-overlay" role="presentation" onMouseDown={() => { if (!submitting) onClose() }}>
      <form className="add-link-dialog" onSubmit={(event) => void submit(event)} onMouseDown={(event) => event.stopPropagation()} aria-labelledby="add-link-title">
        <header>
          <h2 id="add-link-title">添加链接</h2>
          <button type="button" title="关闭" aria-label="关闭" onClick={onClose} disabled={submitting}>
            <Icon name="close" size={14} />
          </button>
        </header>
        <div className="add-link-body">
          <div className="add-link-row">
            <label className="add-link-input">
              <Icon name="link" size={14} />
              <input
                ref={inputRef}
                type="url"
                inputMode="url"
                value={url}
                onChange={(event) => { setURL(event.target.value); setError(null) }}
                placeholder="粘贴 https:// 链接，回车提交"
                disabled={submitting}
              />
            </label>
            <button className="add-link-submit" type="submit" disabled={submitting || !url.trim()}>
              {submitting ? '添加中…' : '添加'}
            </button>
          </div>
          {error && <p className="add-link-error" role="alert">{error}</p>}
        </div>
      </form>
    </div>
  )
}
