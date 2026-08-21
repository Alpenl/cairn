// Package problem defines failures that application code may return to an
// outer transport. It deliberately has no HTTP dependency.
package problem

import "errors"

// Kind describes how a caller may recover from a failure.
type Kind uint8

const (
	Internal Kind = iota
	Malformed
	Invalid
	NotFound
	Conflict
	Precondition
	TooLarge
	RateLimited
	Forbidden
	Unavailable
	Upstream
	Timeout
	Canceled
)

// Stable codes used by current clients.
const (
	CodeLinkNotFound                      = "link_not_found"
	CodeInvalidLinkID                     = "invalid_link_id"
	CodeInvalidSiteID                     = "invalid_site_id"
	CodeInvalidSiteView                   = "invalid_site_view"
	CodeInvalidRecentCutoff               = "invalid_site_recent_cutoff"
	CodeSiteNotFound                      = "site_not_found"
	CodeSiteRevisionConflict              = "site_revision_conflict"
	CodeSiteRevisionRequired              = "site_revision_required"
	CodeSiteUpdateEmpty                   = "site_update_empty"
	CodeInvalidSiteUpdate                 = "invalid_site_update"
	CodeInvalidSiteEntryID                = "invalid_site_entry_id"
	CodeSiteEntryNotFound                 = "site_entry_not_found"
	CodeSiteEntryUpdateEmpty              = "site_entry_update_empty"
	CodeSiteDeleteConfirm                 = "site_delete_confirmation_required"
	CodeCooldownActive                    = "cooldown_active"
	CodeLinkNotReady                      = "link_not_ready"
	CodeLinkContentUnavailable            = "link_content_unavailable"
	CodeTranslationInvalidRequest         = "translation_invalid_request"
	CodeTranslationContentUnavailable     = "translation_content_unavailable"
	CodeContentRevisionConflict           = "content_revision_conflict"
	CodeMetadataRevisionConflict          = "metadata_revision_conflict"
	CodeMetadataFieldsRequired            = "metadata_fields_required"
	CodeInvalidLinkMetadata               = "invalid_link_metadata"
	CodeSourceBlockConflict               = "source_block_conflict"
	CodeInvalidCursor                     = "invalid_cursor"
	CodeInvalidArchiveSections            = "invalid_archive_sections"
	CodeInvalidCreatedRange               = "invalid_created_range"
	CodeURLRequired                       = "url_required"
	CodeInvalidURL                        = "invalid_url"
	CodeURLTooLong                        = "url_too_long"
	CodeDescriptionTooLong                = "description_too_long"
	CodeUnsupportedURLScheme              = "unsupported_url_scheme"
	CodeUnsafeURLTarget                   = "unsafe_url_target"
	CodeIngestSourceRequired              = "ingest_source_required"
	CodeIngestSourceKindRequired          = "ingest_source_kind_required"
	CodeUnsupportedIngestSourceKind       = "unsupported_ingest_source_kind"
	CodeIngestTextRequired                = "ingest_text_required"
	CodeIngestImageSourceRequired         = "ingest_image_source_required"
	CodeIngestImageDataURLTooLarge        = "ingest_image_data_url_too_large"
	CodeIngestBrowserCaptureEmpty         = "ingest_browser_capture_empty"
	CodeIngestMetadataKeyCountExceeded    = "ingest_metadata_key_count_exceeded"
	CodeIngestMetadataKeyLengthExceeded   = "ingest_metadata_key_length_exceeded"
	CodeIngestMetadataValueLengthExceeded = "ingest_metadata_value_length_exceeded"
	CodeTagFiltersExceedLimit             = "tag_filters_exceed_limit"
	CodeTagFilterTooLong                  = "tag_filter_too_long"
	CodeUnsupportedContentTypeFilter      = "unsupported_content_type_filter"
	CodeUnsupportedLowConfidenceFilter    = "unsupported_low_confidence_filter"
	CodeDomainFilterTooLong               = "domain_filter_too_long"
	CodeQueryTooLong                      = "query_too_long"
	CodeUnsupportedStatusFilter           = "unsupported_status_filter"
	CodeInvalidRequestedLibraryKind       = "invalid_requested_library_kind"
	CodeLibraryKindNotFinal               = "library_kind_not_final"
	CodeConversionTargetUnchanged         = "conversion_target_unchanged"
	CodeDestructiveConfirmationRequired   = "destructive_confirmation_required"
	CodeRevisionConflict                  = "revision_conflict"
	CodeSiteOriginalContentForbidden      = "site_original_content_forbidden"
	CodeContentEmpty                      = "content_empty"
	CodeContentTooLarge                   = "content_too_large"
)

// ConflictIdentity is the current source identity returned with a rejected
// compare-and-swap operation.
type ConflictIdentity struct {
	ContentRevision *int64
	BlockKey        string
	SourceHash      *string
}

// Error is a transport-neutral application failure.
type Error struct {
	kind            Kind
	message         string
	code            string
	retryAfter      int
	currentIdentity *ConflictIdentity
	cause           error
}

func New(kind Kind, message string) *Error {
	return &Error{kind: kind, message: message}
}

func NewWithCode(kind Kind, code, message string) *Error {
	return &Error{kind: kind, code: code, message: message}
}

func NewWithRetryAfter(kind Kind, message string, seconds int) *Error {
	return &Error{kind: kind, message: message, retryAfter: max(0, seconds)}
}

func NewWithCodeAndRetryAfter(kind Kind, code, message string, seconds int) *Error {
	return &Error{kind: kind, code: code, message: message, retryAfter: max(0, seconds)}
}

func NewWithCodeAndCurrentIdentity(kind Kind, code, message string, identity ConflictIdentity) *Error {
	copy := cloneConflictIdentity(identity)
	return &Error{kind: kind, code: code, message: message, currentIdentity: &copy}
}

// Wrap keeps the original cause available to errors.Is and errors.As while
// exposing only the supplied client-safe message.
func Wrap(kind Kind, code, message string, cause error) *Error {
	return &Error{kind: kind, code: code, message: message, cause: cause}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *Error) Kind() Kind {
	if e == nil {
		return Internal
	}
	return e.kind
}

func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *Error) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *Error) RetryAfterSeconds() int {
	if e == nil {
		return 0
	}
	return e.retryAfter
}

func (e *Error) CurrentIdentity() (ConflictIdentity, bool) {
	if e == nil || e.currentIdentity == nil {
		return ConflictIdentity{}, false
	}
	return cloneConflictIdentity(*e.currentIdentity), true
}

func As(err error) (*Error, bool) {
	var target *Error
	if !errors.As(err, &target) || target == nil {
		return nil, false
	}
	return target, true
}

func cloneConflictIdentity(identity ConflictIdentity) ConflictIdentity {
	copy := identity
	if identity.ContentRevision != nil {
		value := *identity.ContentRevision
		copy.ContentRevision = &value
	}
	if identity.SourceHash != nil {
		value := *identity.SourceHash
		copy.SourceHash = &value
	}
	return copy
}
