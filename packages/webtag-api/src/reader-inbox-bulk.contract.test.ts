import type { components, paths } from './generated'

type BulkRequest = components['schemas']['ReaderInboxBulkRequest']
type BulkResponse = components['schemas']['ReaderInboxBulkResponse']
type AIProposalRequest = components['schemas']['ReaderInboxConfirmAIProposalsRequest']
type AIProposalResponse = components['schemas']['ReaderInboxConfirmAIProposalsResponse']
type BulkOperation = paths['/api/inbox/bulk/confirm']['post']
type DiscardOperation = paths['/api/inbox/bulk/discard']['post']
type AIProposalOperation = paths['/api/inbox/confirm-ai-proposals']['post']
type BulkRequestBody = NonNullable<BulkOperation['requestBody']>['content']['application/json']
type BulkResponseBody = NonNullable<BulkOperation['responses'][200]['content']>['application/json']
type DiscardRequestBody = NonNullable<DiscardOperation['requestBody']>['content']['application/json']
type AIProposalRequestBody = NonNullable<AIProposalOperation['requestBody']>['content']['application/json']
type AIProposalResponseBody = NonNullable<AIProposalOperation['responses'][200]['content']>['application/json']

// This is a compile-time contract test: both bulk operations must continue to
// share the generated request/response shapes as the OpenAPI document evolves.
export const readerInboxBulkContractExamples = {
  confirmRequest: {
    inbox_ids: ['00000000-0000-0000-0000-000000000001'],
    expected_revisions: {
      '00000000-0000-0000-0000-000000000001': 3,
    },
  } satisfies BulkRequest & BulkRequestBody,
  discardRequest: {
    inbox_ids: ['00000000-0000-0000-0000-000000000002'],
  } satisfies BulkRequest & DiscardRequestBody,
  response: {
    atomic: true,
    items: [
      {
        inbox_id: '00000000-0000-0000-0000-000000000001',
        status: 'confirmed',
        link_id: '00000000-0000-0000-0000-000000000101',
      },
    ],
  } satisfies BulkResponse & BulkResponseBody,
  confirmAIProposalsRequest: {
    partition: 'active',
  } satisfies AIProposalRequest & AIProposalRequestBody,
  confirmAIProposalsResponse: {
    atomic: true,
    items: [{
      inbox_id: '00000000-0000-0000-0000-000000000003',
      status: 'confirmed',
      link_id: '00000000-0000-0000-0000-000000000102',
    }],
    remaining_count: 101,
  } satisfies AIProposalResponse & AIProposalResponseBody,
}
