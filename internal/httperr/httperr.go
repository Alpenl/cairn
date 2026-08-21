// Package httperr adapts transport-neutral application problems to HTTP.
// Handlers may also construct Error for failures created at the HTTP boundary.
package httperr

import (
	"errors"
	"net/http"

	"webtag/internal/problem"
)

// 错误 slug 常量：service 层构造 httperr.NewWithCode 时使用，避免字符串
// 字面量在源代码中散落。slug 命名约定：snake_case 名词或动宾短语，避免
// HTTP 状态码字面量（status 已经在 carrier 里）。
//
// 这里只登记 service 层会主动抛出的 slug；presentation 层（middleware）
// 自身路径上还有更多 slug（如 invalid_request_body、unauthorized）维持
// 在 middleware/errors.go 即可，保持就近原则。两侧字面值如果重叠（如
// link_not_found），必须保持一致——避免前端按 slug 分支时产生歧义。
const (
	// CodeLinkNotFound —— /api/links/:id 系列：链接不存在。404 路径。
	CodeLinkNotFound = problem.CodeLinkNotFound
	// CodeInvalidLinkID —— linkID UUID 解析失败。400 路径。
	CodeInvalidLinkID        = problem.CodeInvalidLinkID
	CodeInvalidSiteID        = problem.CodeInvalidSiteID
	CodeInvalidSiteView      = problem.CodeInvalidSiteView
	CodeInvalidRecentCutoff  = problem.CodeInvalidRecentCutoff
	CodeSiteNotFound         = problem.CodeSiteNotFound
	CodeSiteRevisionConflict = problem.CodeSiteRevisionConflict
	CodeSiteRevisionRequired = problem.CodeSiteRevisionRequired
	CodeSiteUpdateEmpty      = problem.CodeSiteUpdateEmpty
	CodeInvalidSiteUpdate    = problem.CodeInvalidSiteUpdate
	CodeInvalidSiteEntryID   = problem.CodeInvalidSiteEntryID
	CodeSiteEntryNotFound    = problem.CodeSiteEntryNotFound
	CodeSiteEntryUpdateEmpty = problem.CodeSiteEntryUpdateEmpty
	CodeSiteDeleteConfirm    = problem.CodeSiteDeleteConfirm
	// CodeCooldownActive —— refresh 触发 per-link 冷却窗口。429 路径。
	CodeCooldownActive = problem.CodeCooldownActive
	// CodeLinkNotReady —— 对未解析完成（非 done）的 link 做「保存原文」。409 路径。
	CodeLinkNotReady = problem.CodeLinkNotReady
	// CodeLinkContentUnavailable —— 已完成的非 URL ingest 没有可提升的文本原文。
	// 409 路径；此类来源不能通过远程重抓补齐。
	CodeLinkContentUnavailable = problem.CodeLinkContentUnavailable
	// CodeTranslationInvalidRequest means a selected-text anchor or scope is
	// malformed. The initial translation surface only supports zh-CN output.
	CodeTranslationInvalidRequest = problem.CodeTranslationInvalidRequest
	// CodeTranslationContentUnavailable means full translation was requested
	// before the original article had been saved.
	CodeTranslationContentUnavailable = problem.CodeTranslationContentUnavailable
	// CodeContentRevisionConflict means the saved-content generation observed
	// by a translation client is no longer current. 409 path.
	CodeContentRevisionConflict = problem.CodeContentRevisionConflict
	// CodeMetadataRevisionConflict identifies a stale Link metadata CAS
	// command. It is intentionally distinct from generic revision conflicts so
	// Reader can retain a metadata draft and load the current server tuple.
	CodeMetadataRevisionConflict = problem.CodeMetadataRevisionConflict
	// CodeMetadataFieldsRequired rejects a partial Link metadata replacement.
	// title, summary, and tags must be present even when title/summary are null.
	CodeMetadataFieldsRequired = problem.CodeMetadataFieldsRequired
	// CodeInvalidLinkMetadata rejects a complete tuple whose field values do
	// not satisfy the bounded Link metadata contract.
	CodeInvalidLinkMetadata = problem.CodeInvalidLinkMetadata
	// CodeSourceBlockConflict means a summary source hash is no longer current.
	// 409 path.
	CodeSourceBlockConflict = problem.CodeSourceBlockConflict
	// CodeInvalidCursor —— ?after= 游标 token 解析失败或与 ?page= 冲突。422 路径。
	CodeInvalidCursor = problem.CodeInvalidCursor
	// CodeInvalidArchiveSections rejects a non-canonical v2 archive selector
	// before the handler begins the streaming response. 422 path.
	CodeInvalidArchiveSections = problem.CodeInvalidArchiveSections
	// CodeInvalidCreatedRange rejects one-sided, malformed, equal, or reversed
	// created_at ranges on GET /api/links. Omitting both bounds remains valid.
	CodeInvalidCreatedRange = problem.CodeInvalidCreatedRange

	// 以下为 Wave 9 MED 迁移补的 422 slug 集合：把 submit / ingest
	// 三条 URL 校验路径上的 httperr.New(...) 改为 NewWithCode(...) 后，
	// 前端 / 上游可以按 slug 分支处理（"是 URL 不合法还是 unsafe SSRF？"
	// 的错误语义变成稳定代码，不再依赖 message 字面量做正则匹配。

	// CodeURLRequired —— validateURL 入参为空字符串。422 路径。
	CodeURLRequired = problem.CodeURLRequired
	// CodeInvalidURL —— validateURL 解析失败或 host 为空。422 路径。
	CodeInvalidURL = problem.CodeInvalidURL
	// CodeURLTooLong —— link URL 超过 2048 字符。
	CodeURLTooLong = problem.CodeURLTooLong
	// CodeDescriptionTooLong —— link description 超过 4096 字符。
	CodeDescriptionTooLong = problem.CodeDescriptionTooLong
	// CodeUnsupportedURLScheme —— validateURL 收到非 http/https scheme。422 路径。
	CodeUnsupportedURLScheme = problem.CodeUnsupportedURLScheme
	// CodeUnsafeURLTarget —— validateURL 命中 SSRF 黑名单。422 路径。
	CodeUnsafeURLTarget = problem.CodeUnsafeURLTarget

	// CodeIngestSourceRequired —— /api/ingest 缺 sources。422 路径。
	CodeIngestSourceRequired = problem.CodeIngestSourceRequired
	// CodeIngestSourceKindRequired —— /api/ingest source.kind 为空。422 路径。
	CodeIngestSourceKindRequired = problem.CodeIngestSourceKindRequired
	// CodeUnsupportedIngestSourceKind —— /api/ingest source.kind 不在 url/text/image/browser_capture 之列。422 路径。
	CodeUnsupportedIngestSourceKind = problem.CodeUnsupportedIngestSourceKind

	// 以下为后续 wave 继续补的 slug，覆盖 ingest normalize、read filters、
	// 各组路径，把所有 service 层关心错误分类
	// 的 422 / 4xx 调用点从 default_<status> 兜底改造成稳定 slug。

	// CodeIngestTextRequired —— /api/ingest text 源 trim 后为空。422 路径。
	CodeIngestTextRequired = problem.CodeIngestTextRequired
	// CodeIngestImageSourceRequired —— /api/ingest image 源 URL 为空或非
	// http(s)/data URL。422 路径。
	CodeIngestImageSourceRequired = problem.CodeIngestImageSourceRequired
	// CodeIngestImageDataURLTooLarge —— /api/ingest image 源 data:URL 体积
	// 超过 maxImageDataURLBytes。422 路径。
	CodeIngestImageDataURLTooLarge = problem.CodeIngestImageDataURLTooLarge
	// CodeIngestBrowserCaptureEmpty —— /api/ingest browser_capture 源所有
	// 字段都是空值。422 路径。
	CodeIngestBrowserCaptureEmpty = problem.CodeIngestBrowserCaptureEmpty
	// CodeIngestMetadataKeyCountExceeded —— metadata key 数超过上限。422 路径。
	CodeIngestMetadataKeyCountExceeded = problem.CodeIngestMetadataKeyCountExceeded
	// CodeIngestMetadataKeyLengthExceeded —— metadata 单个 key 长度超限。422 路径。
	CodeIngestMetadataKeyLengthExceeded = problem.CodeIngestMetadataKeyLengthExceeded
	// CodeIngestMetadataValueLengthExceeded —— metadata 字符串值长度超限。422 路径。
	CodeIngestMetadataValueLengthExceeded = problem.CodeIngestMetadataValueLengthExceeded

	// CodeTagFiltersExceedLimit —— ?tags= 解析后超出 maxListTagFilters。422 路径。
	CodeTagFiltersExceedLimit = problem.CodeTagFiltersExceedLimit
	// CodeTagFilterTooLong —— 单个 tag 过滤值超过 maxListTagFilterLen。422 路径。
	CodeTagFilterTooLong = problem.CodeTagFilterTooLong
	// CodeUnsupportedContentTypeFilter —— ?content_type= 不在白名单内。422 路径。
	CodeUnsupportedContentTypeFilter = problem.CodeUnsupportedContentTypeFilter
	// CodeUnsupportedLowConfidenceFilter —— ?low_confidence= 不是 true/false。422 路径。
	CodeUnsupportedLowConfidenceFilter = problem.CodeUnsupportedLowConfidenceFilter
	// CodeDomainFilterTooLong —— ?domain= 长度超过 maxListDomainLen。422 路径。
	CodeDomainFilterTooLong = problem.CodeDomainFilterTooLong
	// CodeQueryTooLong —— ?q= 搜索 query 长度超过 maxListQueryLen。422 路径。
	CodeQueryTooLong = problem.CodeQueryTooLong
	// CodeUnsupportedStatusFilter —— ?status= 含 pending/processing/failed/done
	// 之外的非法状态值。400 路径：浏览器扩展按此 slug 区分"客户端传错状态"
	// 与其它过滤错误。
	CodeUnsupportedStatusFilter = problem.CodeUnsupportedStatusFilter

	// CodeInvalidRequestedLibraryKind rejects values other than auto, reading,
	// and site at the capture boundary.
	CodeInvalidRequestedLibraryKind = problem.CodeInvalidRequestedLibraryKind
	// CodeLibraryKindNotFinal rejects a partition-dependent operation before
	// automatic classification has produced a final destination.
	CodeLibraryKindNotFinal = problem.CodeLibraryKindNotFinal
	// CodeConversionTargetUnchanged rejects a conversion to the link's
	// already-final collection. It is a conflict rather than malformed input.
	CodeConversionTargetUnchanged       = problem.CodeConversionTargetUnchanged
	CodeDestructiveConfirmationRequired = problem.CodeDestructiveConfirmationRequired
	CodeRevisionConflict                = problem.CodeRevisionConflict
	// CodeSiteOriginalContentForbidden prevents original-body storage for a
	// website entry; site profile text belongs to sites.intro instead.
	CodeSiteOriginalContentForbidden = problem.CodeSiteOriginalContentForbidden
	// CodeContentEmpty means the edited body is empty after canonicalization.
	CodeContentEmpty = problem.CodeContentEmpty
	// CodeContentTooLarge means the decoded UTF-8 body exceeds the saved-content limit.
	CodeContentTooLarge = problem.CodeContentTooLarge
)

// StatusCarrier is the normalized result consumed by handlers.
type StatusCarrier interface {
	error
	HTTPStatus() int
	HTTPMessage() string
}

// ErrorCoder exposes the stable client error code when one is available.
type ErrorCoder interface {
	HTTPErrorCode() string
}

// ConflictIdentity is the authoritative translation source identity observed
// when a conditional request is rejected. ContentRevision identifies saved
// content; SourceHash identifies the summary block. Nil pointers preserve
// which identity domain applies.
type ConflictIdentity struct {
	ContentRevision *int64
	BlockKey        string
	SourceHash      *string
}

// CurrentIdentityProvider exposes optional conflict metadata.
type CurrentIdentityProvider interface {
	HTTPCurrentIdentity() (ConflictIdentity, bool)
}

// Error is the canonical StatusCarrier implementation. Use New to
// construct one; the zero value is intentionally not useful.
type Error struct {
	status          int
	message         string
	retryAfter      int
	currentIdentity *ConflictIdentity
	// code 是机器可读的 error_code slug，由 NewWithCode 注入。零值（""）
	// 表示未指定，writeError 会回退到 default_<status>。slug 命名约定见
	// middleware.ErrCode* 常量集合。
	code string
}

// New builds a StatusCarrier with the supplied HTTP status code and
// client-facing message. The message is surfaced verbatim by the
// presentation layer, so callers must keep it free of internal details
// (table names, connection strings, raw upstream payloads).
func New(status int, message string) *Error {
	return &Error{status: status, message: message}
}

// NewWithCode 与 New 类似，但额外带一个机器可读的 error_code slug。
// 走 presentation 层时 writeError 会把 slug 透出到 ErrorDetail.error_code，
// 跳过 default_<status> 兜底。code 为空时退化等价于 New。
//
// 设计动机：Wave 6 给 handler 层加了 JSONErrorWithSlug，但 service 层抛出的
// httperr.Error 进入 handler.writeError 后只剩 status + message，slug 字段
// 走 default_<status> 兜底，前端拿不到稳定 token。NewWithCode 让 service
// 端在源头就把 slug 串到 carrier 里。
func NewWithCode(status int, code, message string) *Error {
	return &Error{status: status, message: message, code: code}
}

// NewWithCodeAndCurrentIdentity adds the current translation source identity
// to a coded error. It is used by source-CAS and rolling-schema conflicts so a
// client can refresh against structured data instead of parsing message text.
func NewWithCodeAndCurrentIdentity(status int, code, message string, identity ConflictIdentity) *Error {
	copy := cloneConflictIdentity(identity)
	return &Error{
		status:          status,
		message:         message,
		code:            code,
		currentIdentity: &copy,
	}
}

// NewWithRetryAfter is the rate-limit variant that carries a
// Retry-After hint (seconds). The presentation layer reads it via the
// RetryAfterSeconds method and serializes the header; clients then
// know how long to wait before retrying. retrySeconds <= 0 collapses
// to no header.
func NewWithRetryAfter(status int, message string, retrySeconds int) *Error {
	if retrySeconds < 0 {
		retrySeconds = 0
	}
	return &Error{status: status, message: message, retryAfter: retrySeconds}
}

// NewWithCodeAndRetryAfter 同时承载 slug 与 Retry-After 提示，给冷却 / 限流
// 场景使用——既能带 slug 让前端按错误码分支处理，又能让客户端按
// Retry-After 退避。retrySeconds < 0 收敛到 0。
func NewWithCodeAndRetryAfter(status int, code, message string, retrySeconds int) *Error {
	if retrySeconds < 0 {
		retrySeconds = 0
	}
	return &Error{status: status, message: message, code: code, retryAfter: retrySeconds}
}

// HTTPErrorCode returns the machine-readable slug carried by this error,
// or "" when none was attached. presentation 层用空字符串作为"走默认"
// 的信号。nil 接收者同样回退到空字符串，与其他 nil-safe 方法一致。
func (e *Error) HTTPErrorCode() string {
	if e == nil {
		return ""
	}
	return e.code
}

// HTTPCurrentIdentity returns a defensive copy of optional source-conflict
// metadata. The boolean is false for every existing non-conflict error.
func (e *Error) HTTPCurrentIdentity() (ConflictIdentity, bool) {
	if e == nil || e.currentIdentity == nil {
		return ConflictIdentity{}, false
	}
	return cloneConflictIdentity(*e.currentIdentity), true
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

// RetryAfterSeconds is the optional Retry-After hint a 429 / 503 may
// carry. Returns 0 when no hint was attached. Presentation layer uses
// this to decide whether to write the Retry-After header.
func (e *Error) RetryAfterSeconds() int {
	if e == nil {
		return 0
	}
	return e.retryAfter
}

// Error makes *Error satisfy the standard error interface. Returns an
// empty string on a nil receiver so a typed-nil *Error wrapped in an
// `error` interface value never panics if a call site reaches it before
// the nil-check fires.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

// HTTPStatus returns the status code the presentation layer should map to.
// A nil receiver collapses to 500: the carrier surface is never useful when
// the value is nil, but a panic at the handler boundary would convert a
// programming error into an opaque 502 from the recovering middleware.
// Returning 500 keeps the failure self-describing.
func (e *Error) HTTPStatus() int {
	if e == nil {
		return 500
	}
	return e.status
}

// HTTPMessage returns the client-facing message the presentation layer
// should serialize. It is identical to Error() today; the split exists so
// adapters can render different copy than the Go-side error string in the
// future without breaking either contract. Mirrors Error() in returning
// an empty string on a nil receiver.
func (e *Error) HTTPMessage() string {
	if e == nil {
		return ""
	}
	return e.message
}

// As maps boundary errors and application problems to one HTTP carrier.
func As(err error) (StatusCarrier, bool) {
	if err == nil {
		return nil, false
	}
	var boundaryError *Error
	if errors.As(err, &boundaryError) && boundaryError != nil {
		return boundaryError, true
	}
	applicationError, ok := problem.As(err)
	if !ok {
		return nil, false
	}
	return problemStatus{applicationError}, true
}

type problemStatus struct {
	problem *problem.Error
}

func (e problemStatus) Error() string       { return e.problem.Error() }
func (e problemStatus) HTTPMessage() string { return e.problem.Message() }
func (e problemStatus) HTTPErrorCode() string {
	return e.problem.Code()
}
func (e problemStatus) RetryAfterSeconds() int {
	return e.problem.RetryAfterSeconds()
}
func (e problemStatus) HTTPCurrentIdentity() (ConflictIdentity, bool) {
	identity, ok := e.problem.CurrentIdentity()
	if !ok {
		return ConflictIdentity{}, false
	}
	return ConflictIdentity{
		ContentRevision: identity.ContentRevision,
		BlockKey:        identity.BlockKey,
		SourceHash:      identity.SourceHash,
	}, true
}
func (e problemStatus) HTTPStatus() int {
	switch e.problem.Kind() {
	case problem.Malformed:
		return http.StatusBadRequest
	case problem.Invalid:
		return http.StatusUnprocessableEntity
	case problem.NotFound:
		return http.StatusNotFound
	case problem.Conflict:
		return http.StatusConflict
	case problem.Precondition:
		return http.StatusPreconditionRequired
	case problem.TooLarge:
		return http.StatusRequestEntityTooLarge
	case problem.RateLimited:
		return http.StatusTooManyRequests
	case problem.Forbidden:
		return http.StatusForbidden
	case problem.Unavailable:
		return http.StatusServiceUnavailable
	case problem.Upstream:
		return http.StatusBadGateway
	case problem.Timeout:
		return http.StatusGatewayTimeout
	case problem.Canceled:
		return 499
	default:
		return http.StatusInternalServerError
	}
}
