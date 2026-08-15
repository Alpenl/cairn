import type { components, paths } from './generated'

type LinkMetadataOperation = paths['/api/links/{link_id}/metadata']['patch']
type LinkMetadataRequest = components['schemas']['ReaderLinkMetadataRequest']
type LinkMetadataRequestBody = NonNullable<LinkMetadataOperation['requestBody']>['content']['application/json']
type LinkMetadataResponseBody = LinkMetadataOperation['responses'][200]['content']['application/json']
type LinkMetadataSuccessHeaders = LinkMetadataOperation['responses'][200]['headers']
type LinkMetadataConflictBody = LinkMetadataOperation['responses'][409]['content']['application/json']
type LinkMetadataTooLargeBody = LinkMetadataOperation['responses'][413]['content']['application/json']
type LinkMetadataValidationBody = LinkMetadataOperation['responses'][422]['content']['application/json']
type LinkMetadataPreconditionBody = LinkMetadataOperation['responses'][428]['content']['application/json']
type LinkMetadataInternalErrorBody = LinkMetadataOperation['responses'][500]['content']['application/json']

const linkID = '00000000-0000-0000-0000-000000000001'
const dataNamespace = 'Aq1_7fYv4t0Lx2M8pR6sW9dE3hJ5kN7bC1uG4zQ'
const maxSafeMetadataRevision = 9007199254740991

// This pins the generated wire surface: metadata edits replace the complete
// tuple, use a quoted revision precondition, and return the next token.
export const readerLinkMetadataContractExamples = {
  request: {
    title: null,
    summary: 'A user-owned replacement summary.',
    tags: ['Research', 'reader'],
  } satisfies LinkMetadataRequest & LinkMetadataRequestBody,
  header: '"7"' satisfies LinkMetadataOperation['parameters']['header']['If-Match'],
  response: {
    link_id: linkID,
    metadata_revision: 8,
  } satisfies components['schemas']['ReaderLinkMetadataResponse'] & LinkMetadataResponseBody,
  successHeaders: {
    ETag: '"8"',
    'X-WebTag-Data-Namespace': dataNamespace,
  } satisfies LinkMetadataSuccessHeaders,
  maxSafeRevision: {
    header: '"9007199254740991"' satisfies LinkMetadataOperation['parameters']['header']['If-Match'],
    response: {
      link_id: linkID,
      metadata_revision: maxSafeMetadataRevision,
    } satisfies components['schemas']['ReaderLinkMetadataResponse'] & LinkMetadataResponseBody,
    successHeaders: {
      ETag: '"9007199254740991"',
      'X-WebTag-Data-Namespace': dataNamespace,
    } satisfies LinkMetadataSuccessHeaders,
  },
  conflict: {
    error: {
      code: 409,
      error_code: 'metadata_revision_conflict',
      message: 'link metadata revision is stale',
    },
  } satisfies LinkMetadataConflictBody,
  tooLarge: {
    error: {
      code: 413,
      // 服务端实际发出的 slug 是 body_too_large（internal/middleware/errors.go）。
      error_code: 'body_too_large',
      message: 'request body is too large',
    },
  } satisfies LinkMetadataTooLargeBody,
  validation: {
    error: {
      code: 422,
      error_code: 'invalid_if_match',
      message: 'If-Match must be a quoted positive revision',
    },
  } satisfies LinkMetadataValidationBody,
  precondition: {
    error: {
      code: 428,
      error_code: 'if_match_required',
      message: 'If-Match is required',
    },
  } satisfies LinkMetadataPreconditionBody,
  internalError: {
    error: {
      code: 500,
      error_code: 'internal_error',
      message: 'internal server error',
    },
  } satisfies LinkMetadataInternalErrorBody,
} as const

export const readerLinkMetadataResponseStatuses = {
  success: 200,
  conflict: 409,
  tooLarge: 413,
  validation: 422,
  precondition: 428,
  internalError: 500,
} satisfies {
  success: keyof LinkMetadataOperation['responses']
  conflict: keyof LinkMetadataOperation['responses']
  tooLarge: keyof LinkMetadataOperation['responses']
  validation: keyof LinkMetadataOperation['responses']
  precondition: keyof LinkMetadataOperation['responses']
  internalError: keyof LinkMetadataOperation['responses']
}

// @ts-expect-error Metadata PATCH is a complete replacement, not a partial update.
export const readerLinkMetadataMissingTags: LinkMetadataRequestBody = {
  title: null,
  summary: null,
}

export const readerLinkMetadataNullTags: LinkMetadataRequestBody = {
  title: null,
  summary: null,
  // @ts-expect-error tags must be an array even when the caller is clearing all tags.
  tags: null,
}
