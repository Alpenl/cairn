import { useLayoutEffect, useState, type FormEvent } from 'react'
import { archiveV2Sections, fullArchiveV2Selection, type ArchiveV2Selection } from '../lib/api/archive-v2'
import { ReaderDialog } from './ui/ReaderDialog'

export interface ArchiveDownloadDialogProps {
  readonly open: boolean
  readonly downloading: boolean
  readonly onClose: () => void
  readonly onDownload: (selection: ArchiveV2Selection) => Promise<boolean>
}

// The dialog is retained between openings so resetting from `open` is explicit
// rather than relying on a mount/unmount detail in its parent.
export function ArchiveDownloadDialog({
  open,
  downloading,
  onClose,
  onDownload,
}: ArchiveDownloadDialogProps) {
  const [includeThoughts, setIncludeThoughts] = useState(true)
  const [includeNotes, setIncludeNotes] = useState(true)

  useLayoutEffect(() => {
    if (!open) return
    setIncludeThoughts(fullArchiveV2Selection.includeThoughts)
    setIncludeNotes(fullArchiveV2Selection.includeNotes)
  }, [open])

  if (!open) return null

  const selection: ArchiveV2Selection = { includeThoughts, includeNotes }
  const selector = archiveV2Sections(selection)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (await onDownload(selection)) onClose()
  }

  return (
    // 下载中（downloading）映射到 busy：关闭按钮、Escape 和 backdrop 一起失效，
    // 与迁移前「!downloading 才关闭」等价。
    <ReaderDialog
      title="下载归档"
      titleId="archive-download-title"
      size="compact"
      className="site-dialog archive-download-dialog"
      busy={downloading}
      onClose={onClose}
    >
      <form onSubmit={(event) => void submit(event)}>
        <div className="archive-download-options" aria-label="归档内容">
          <label className="archive-download-option">
            <input
              type="checkbox"
              checked={includeThoughts}
              disabled={downloading}
              onChange={(event) => setIncludeThoughts(event.target.checked)}
            />
            <span>想法</span>
          </label>
          <label className="archive-download-option">
            <input
              type="checkbox"
              checked={includeNotes}
              disabled={downloading}
              onChange={(event) => setIncludeNotes(event.target.checked)}
            />
            <span>笔记</span>
          </label>
        </div>
        <footer>
          <button type="button" onClick={onClose} disabled={downloading}>取消</button>
          <button type="submit" disabled={downloading} data-archive-sections={selector}>
            {downloading ? '下载中' : '下载'}
          </button>
        </footer>
      </form>
    </ReaderDialog>
  )
}
