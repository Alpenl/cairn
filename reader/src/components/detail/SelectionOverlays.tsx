import type { CSSProperties } from 'react'

import type { PopoverAction } from '../../lib/selection-actions'
import { ActionPopover } from '../ActionPopover'
import { Icon } from '../Icon'
import type { TranslationResponse } from '../../lib/api/types'
import type { SelectionInfo } from '../../lib/annotations'

export interface TranslationPopoverState {
  linkId: string
  translationId: string | null
  info: SelectionInfo
}

export interface SelectionOverlaysProps {
  actionSelection: SelectionInfo | null
  annotationActionsPending: boolean
  translation: TranslationPopoverState | null
  translationResult?: TranslationResponse
  onAction: (action: PopoverAction) => void
  onCloseTranslation: () => void
  onRetryTranslation: () => void
  onCopyTranslation: (text: string) => void
  actions?: readonly PopoverAction[]
}

function translationPopoverStyle(selection: TranslationPopoverState): CSSProperties {
  const popWidth = Math.min(360, window.innerWidth - 24)
  const centeredLeft = selection.info.rect.left + selection.info.rect.width / 2 - popWidth / 2
  const left = Math.max(12, Math.min(centeredLeft, window.innerWidth - popWidth - 12))
  const placeAbove =
    selection.info.rect.bottom + 250 > window.innerHeight &&
    selection.info.rect.top > 250

  return placeAbove
    ? { left, top: selection.info.rect.top - 10, transform: 'translateY(-100%)' }
    : { left, top: selection.info.rect.bottom + 10 }
}

export function SelectionOverlays({
  actionSelection,
  annotationActionsPending,
  translation,
  translationResult,
  onAction,
  onCloseTranslation,
  onRetryTranslation,
  onCopyTranslation,
  actions,
}: SelectionOverlaysProps) {
  return (
    <>
      {actionSelection && (
        <ActionPopover
          rect={actionSelection.rect}
          onAct={onAction}
          actions={actions}
          annotationActionsPending={annotationActionsPending}
        />
      )}
      {translation && (
        <div
          className="translation-pop"
          style={translationPopoverStyle(translation)}
          role="status"
          aria-live="polite"
        >
          <div className="translation-pop-head">
            <span><Icon name="translate" size={14} /> 中文翻译</span>
            <button
              type="button"
              className="translation-icon-btn"
              aria-label="关闭翻译"
              title="关闭"
              onClick={onCloseTranslation}
            >
              <Icon name="close" size={14} />
            </button>
          </div>
          {!translation.translationId ||
          !translationResult ||
          translationResult.status === 'pending' ||
          translationResult.status === 'processing' ? (
            <p className="translation-pop-status">
              <Icon
                name="loader"
                size={14}
                style={{ animation: 'spin 0.9s linear infinite' }}
              />
              正在翻译…
            </p>
          ) : translationResult.status === 'failed' ? (
            <div className="translation-pop-failed">
              <p role="alert">{translationResult.error_msg || '翻译失败，请重试'}</p>
              <button type="button" className="link-btn" onClick={onRetryTranslation}>
                重试
              </button>
            </div>
          ) : (
            <div className="translation-pop-result">
              <p lang="zh-CN">{translationResult.translated_text}</p>
              {translationResult.translated_text && (
                <button
                  type="button"
                  className="translation-icon-btn"
                  title="复制译文"
                  aria-label="复制译文"
                  onClick={() => onCopyTranslation(translationResult.translated_text as string)}
                >
                  <Icon name="copy" size={14} />
                </button>
              )}
            </div>
          )}
        </div>
      )}
    </>
  )
}
