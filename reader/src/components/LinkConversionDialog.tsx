import { type FormEvent, useEffect, useState } from 'react'
import { Icon } from './Icon'
import { ReaderDialog } from './ui/ReaderDialog'
import type { ReaderClient } from '../lib/api/client'
import type { ReaderCapabilityLease } from '../lib/capabilities'
import type { ConversionPreviewResponse, LinkResponse } from '../lib/api/types'

export function LinkConversionDialog({ client, capabilityLease, link, initialNote, onClose, onConverted, onToast }: {
  client: ReaderClient
  capabilityLease: ReaderCapabilityLease
  link: LinkResponse
  initialNote: string
  onClose: () => void
  onConverted: () => void
  onToast: (message: string, icon?: import('./Icon').IconName) => void
}) {
  const [preview, setPreview] = useState<ConversionPreviewResponse | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [mode, setMode] = useState<'new' | 'matching'>('new')
  const [note, setNote] = useState(initialNote)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    let active = true
    if (!capabilityLease.isCurrent('siteWrite')) return () => { active = false }
    void client.previewLinkConversion(link.id, { target_kind: 'site' }).then((result) => {
      if (!active || !client.isIdentityCurrent() || !capabilityLease.isCurrent('siteWrite')) return
      if (!result.ok) { setError(result.error.message); return }
      setPreview(result.data)
      if (result.data.matching_site) setMode('matching')
    })
    return () => { active = false }
  }, [capabilityLease, client, link.id])

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    const operationLease = capabilityLease
    if (!preview || !operationLease.isCurrent('siteWrite')) return
    setSubmitting(true)
    const matching = mode === 'matching' ? preview.matching_site : undefined
    const result = await client.convertLink(link.id, {
      target_kind: 'site', expected_content_revision: preview.expected_content_revision,
      target_site_id: matching?.id, expected_site_revision: matching?.revision,
      confirm_destructive: true, preserved_user_note: note.trim() || undefined,
    })
    if (!client.isIdentityCurrent() || !operationLease.isCurrent('siteWrite')) return
    setSubmitting(false)
    if (!result.ok) { setError(result.error.errorCode === 'revision_conflict' ? '内容或网站已变化，请关闭后重新预览。' : result.error.message); return }
    onToast('已移到网站收藏', 'check')
    onConverted()
  }

  // 关闭规则原样保留：backdrop 可关（转换中除外），Escape 一直不关闭——迁移前这个
  // Dialog 就没有 Escape 监听，这里不借迁移偷偷改行为。
  return <ReaderDialog
    title="移到网站收藏"
    titleId="conversion-title"
    className="site-dialog conversion-dialog"
    busy={submitting}
    dismissOnEscape={false}
    onClose={onClose}
  >
    <form onSubmit={(event) => void submit(event)}>
      {!preview && !error && <p className="conversion-loading"><Icon name="loader" size={15} /> 正在检查转换影响</p>}
      {error && <p className="conversion-error" role="alert">{error}</p>}
      {preview && <>
        <p className="conversion-warning">此操作会移除阅读正文相关资产，原链接将作为网站入口保留。</p>
        <ul className="conversion-assets"><li>已保存原文：{preview.saved_original ? '将删除' : '无'}</li><li>译文：{preview.translation_count} 条将删除</li></ul>
        {preview.matching_site && <fieldset className="conversion-target"><legend>目标网站</legend><label><input type="radio" checked={mode === 'matching'} onChange={() => setMode('matching')} /> 附加到“{preview.matching_site.name}”</label><label><input type="radio" checked={mode === 'new'} onChange={() => setMode('new')} /> 新建网站卡片</label></fieldset>}
        <label>保留为网站备注<textarea value={note} onChange={(event) => setNote(event.target.value)} maxLength={10000} placeholder="从当前设备的划线笔记中提取，可编辑或留空" /></label>
        <p className="conversion-policy">确认后，当前 revision 的批注会保留在本机但不再渲染到阅读正文。</p>
      </>}
      <footer><button type="button" onClick={onClose} disabled={submitting}>取消</button><button type="submit" disabled={!preview || submitting}>{submitting ? '转换中' : '确认转换'}</button></footer>
    </form>
  </ReaderDialog>
}
