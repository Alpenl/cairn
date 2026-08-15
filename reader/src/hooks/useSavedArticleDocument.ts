import {
  useCallback,
  useLayoutEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from 'react'

import type { ApiResult } from '../lib/api/result'
import type { LinkContentResponse, LinkResponse } from '../lib/api/types'
import {
  SavedArticleDocumentController,
  type DocumentCommandContext,
  type SavedArticleDocument,
} from '../lib/article/document'
import { useCachedLinkContent } from '../lib/cache/link-content'
import type { IdentityLease } from '../lib/identity'

export interface UseSavedArticleDocumentOptions {
  readonly lease: IdentityLease | null
  readonly detail: LinkResponse | null | undefined
  readonly revisionFloor?: number
  readonly loadBody: (
    context: DocumentCommandContext,
  ) => Promise<ApiResult<LinkContentResponse>>
}

export interface UseSavedArticleDocumentResult {
  readonly document: SavedArticleDocument | null
  readonly controller: SavedArticleDocumentController | null
  readonly captureContext: () => DocumentCommandContext | null
  readonly loadBody: () => Promise<ApiResult<LinkContentResponse> | null>
}

interface ControllerSlot {
  readonly key: string
  readonly controller: SavedArticleDocumentController
}

const subscribeToNothing = (): (() => void) => () => {}

function validRevision(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
}

function controllerKey(
  lease: IdentityLease | null,
  detail: LinkResponse | null | undefined,
): string | null {
  if (!lease || !detail?.id || !validRevision(detail.content_revision)) return null
  return `${lease.context.localEpoch}\0${lease.context.physicalNamespace}\0${detail.id}`
}

function detailWithBody(
  detail: LinkResponse,
  body: LinkContentResponse,
): LinkResponse {
  return {
    ...detail,
    has_content: true,
    content_revision: body.content_revision,
    content: body.content,
    content_document: body.content_document,
    content_format: body.content_format,
    content_source: body.content_source,
  }
}

/**
 * React owner for one saved-original document controller.
 *
 * Summary identity intentionally stays outside this hook. The controller is
 * retained across revisions of one link so advancing the revision aborts the
 * old generation atomically; changing link or lease disposes it entirely.
 */
export function useSavedArticleDocument({
  lease,
  detail,
  revisionFloor,
  loadBody: loadBodySource,
}: UseSavedArticleDocumentOptions): UseSavedArticleDocumentResult {
  const requestedKey = controllerKey(lease, detail)
  const latestDetail = useRef(detail)
  latestDetail.current = detail
  const [slot, setSlot] = useState<ControllerSlot | null>(null)

  useLayoutEffect(() => {
    if (!requestedKey || !lease) {
      setSlot(null)
      return
    }
    const seed = latestDetail.current
    if (!seed || !validRevision(seed.content_revision)) {
      setSlot(null)
      return
    }
    const controller = new SavedArticleDocumentController({ lease, detail: seed })
    setSlot({ key: requestedKey, controller })
    return () => controller.dispose()
  }, [lease, requestedKey])

  const controller = slot?.key === requestedKey ? slot.controller : null
  const detailFingerprint = detail ? JSON.stringify(detail) : ''
  const subscribe = useCallback(
    (listener: () => void) => controller?.subscribe(listener) ?? subscribeToNothing(),
    [controller],
  )
  const getSnapshot = useCallback(
    () => controller?.getSnapshot() ?? null,
    [controller],
  )
  const document = useSyncExternalStore(subscribe, getSnapshot, getSnapshot)

  useLayoutEffect(() => {
    const presentedDetail = latestDetail.current
    if (!controller || !presentedDetail || !validRevision(presentedDetail.content_revision)) return
    const current = controller.getSnapshot()
    if (current.detail.status === 'ready' && current.detail.data === presentedDetail) return
    controller.acceptDetail(presentedDetail, controller.captureContext())
  }, [controller, detailFingerprint])

  useLayoutEffect(() => {
    if (!controller || !validRevision(revisionFloor)) return
    controller.acceptRevisionFloor(revisionFloor)
  }, [controller, revisionFloor])

  const cachedBody = useCachedLinkContent(
    document?.id.linkId,
    document?.id.contentRevision,
  )
  useLayoutEffect(() => {
    if (!controller || !cachedBody) return
    controller.acceptBody(cachedBody, controller.captureContext())
  }, [cachedBody, controller])

  const captureContext = useCallback(
    () => controller?.captureContext() ?? null,
    [controller],
  )

  const loadBody = useCallback(async (): Promise<ApiResult<LinkContentResponse> | null> => {
    if (!controller) return null
    const result = await controller.loadBody(loadBodySource)
    const currentDetail = latestDetail.current
    if (
      result.ok &&
      currentDetail?.id === result.data.link_id &&
      currentDetail.content_revision === result.data.content_revision &&
      controller.getSnapshot().id.contentRevision === result.data.content_revision
    ) {
      // A body endpoint may reveal a newer revision before the list/detail
      // projection catches up. Never relabel the pre-request detail as that
      // newer revision: a newer detail may have arrived while this request was
      // pending, and replaying the captured metadata would restore stale title
      // or summary under the current document identity.
      controller.acceptDetail(
        detailWithBody(currentDetail, result.data),
        controller.captureContext(),
      )
    }
    return result
  }, [controller, loadBodySource])

  return { document, controller, captureContext, loadBody }
}
