import type { IdentityBoundReaderClient, ReaderClient } from './api/client'
import type { IdentityLease, IdentityOwnership } from './identity'

export interface ReaderIdentityPort {
  readonly identityLease: IdentityLease
  isIdentityCurrent(): boolean
  captureIdentity(logicalKey: string): IdentityOwnership | null
}

export type ReaderLibrarySitesPort = ReaderIdentityPort & Pick<
  IdentityBoundReaderClient,
  | 'getLinks'
  | 'submitLink'
  | 'searchLibrary'
  | 'getLink'
  | 'getContent'
  | 'editContent'
  | 'getTags'
  | 'getDomainSummaries'
  | 'refreshLink'
  | 'saveContent'
  | 'replaceContent'
  | 'getTranslations'
  | 'createTranslation'
  | 'getSites'
  | 'getSite'
  | 'updateSite'
  | 'updateSiteEntry'
  | 'setPrimarySiteEntry'
  | 'deleteSiteEntry'
  | 'deleteSite'
  | 'previewSiteMerge'
  | 'executeSiteMerge'
  | 'previewSiteSplit'
  | 'executeSiteSplit'
  | 'previewLinkConversion'
  | 'convertLink'
  | 'patchLinkMetadata'
  | 'getRelatedTags'
  | 'getReaderActivity'
  | 'getEngagement'
  | 'patchEngagement'
  | 'deleteLink'
  | 'restoreLink'
>

export type ReaderSubscriptionsFeedPort = ReaderIdentityPort & Pick<
  IdentityBoundReaderClient,
  | 'getSubscriptions'
  | 'discoverFeeds'
  | 'createSubscription'
  | 'updateSubscription'
  | 'deleteSubscription'
  | 'refreshSubscription'
  | 'refreshSubscriptions'
  | 'getFeedItems'
  | 'getFeedItem'
  | 'updateFeedItem'
  | 'markFeedItemsRead'
  | 'analyzeFeedItem'
  | 'createFeedFolder'
  | 'updateFeedFolder'
  | 'deleteFeedFolder'
  | 'exportSubscriptionsOPML'
  | 'importSubscriptionsOPML'
  | 'getReaderFeed'
  | 'sendReaderFeedFeedback'
>

export type ReaderThoughtsNotesPort = ReaderIdentityPort & Pick<
  IdentityBoundReaderClient,
  | 'pushThoughtOps'
  | 'listThoughts'
  | 'syncThoughts'
  | 'listThoughtSupersessions'
  | 'listThoughtHistory'
  | 'getThought'
  | 'createNote'
  | 'listNotes'
  | 'getNote'
  | 'saveNoteDraft'
  | 'discardNoteDraft'
  | 'publishNote'
  | 'deleteNote'
  | 'restoreNote'
  | 'listNoteHistory'
  | 'restoreNoteRevision'
>

export type ReaderInboxTodosPort = ReaderIdentityPort & Pick<
  IdentityBoundReaderClient,
  | 'createInbox'
  | 'listInbox'
  | 'getInbox'
  | 'patchInbox'
  | 'confirmInbox'
  | 'restoreInbox'
  | 'confirmInboxBulk'
  | 'confirmAIProposals'
  | 'discardInboxBulk'
  | 'discardInbox'
  | 'resummarizeInbox'
  | 'createTodo'
  | 'listTodos'
  | 'patchTodo'
  | 'deleteTodo'
>

export type ReaderHomePort = ReaderIdentityPort &
  Pick<IdentityBoundReaderClient, 'getHome'> &
  Pick<ReaderInboxTodosPort, 'patchTodo'>

export type ReaderSessionArchivePort = ReaderIdentityPort &
  Pick<
    ReaderClient,
    | 'loginWithMutationStatus'
    | 'logout'
    | 'getIdentity'
    | 'getHealth'
    | 'getCapabilities'
    | 'testConnection'
  > &
  Pick<IdentityBoundReaderClient, 'downloadArchiveV2' | 'listTrash' | 'purgeHost'>

export type ReaderHealthPort = Pick<ReaderSessionArchivePort, 'getHealth'>

export type ReaderTrashPort = ReaderIdentityPort &
  Pick<ReaderSessionArchivePort, 'listTrash' | 'purgeHost'> &
  Pick<ReaderLibrarySitesPort, 'restoreLink'> &
  Pick<ReaderThoughtsNotesPort, 'restoreNote'> &
  Pick<ReaderInboxTodosPort, 'restoreInbox'>

export type ReaderAmbientClientPort = ReaderIdentityPort &
  Pick<
    ReaderLibrarySitesPort,
    'getRelatedTags' | 'getReaderActivity' | 'getEngagement' | 'patchEngagement'
  >

export type ReaderCommandSearchPort = Pick<ReaderIdentityPort, 'isIdentityCurrent'> & {
  readonly searchLibrary?: IdentityBoundReaderClient['searchLibrary']
}

export type ReaderAIPort = Pick<ReaderIdentityPort, 'isIdentityCurrent'> & {
  readonly completeReaderAI?: IdentityBoundReaderClient['completeReaderAI']
}
