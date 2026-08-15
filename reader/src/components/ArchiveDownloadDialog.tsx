import { useEffect, useLayoutEffect, useState, type FormEvent } from 'react'
import { archiveV2Sections, fullArchiveV2Selection, type ArchiveV2Selection } from '../lib/api/archive-v2'
import { Icon } from './Icon'

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

  useEffect(() => {
    if (!open) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && !downloading) onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [downloading, onClose, open])

  if (!open) return null

  const selection: ArchiveV2Selection = { includeThoughts, includeNotes }
  const selector = archiveV2Sections(selection)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (await onDownload(selection)) onClose()
  }

  return (
    <div
      className="site-dialog-backdrop"
      role="presentation"
      onMouseDown={() => { if (!downloading) onClose() }}
    >
      <form
        className="site-dialog compact archive-download-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="archive-download-title"
        onMouseDown={(event) => event.stopPropagation()}
        onSubmit={(event) => void submit(event)}
      >
        <header>
          <h2 id="archive-download-title">下载归档</h2>
          <button
            type="button"
            className="tb-btn"
            title="关闭"
            aria-label="关闭"
            onClick={onClose}
            disabled={downloading}
          >
            <Icon name="close" size={15} />
          </button>
        </header>
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
    </div>
  )
}
