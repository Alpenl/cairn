package dto

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"webtag/internal/httperr"
)

// LinkCreateRequest 是 POST /api/links 单条入库请求的 JSON 主体。
//
// validator binding tag 由 Wave 10 API MED M5 引入，作为请求体的早期门禁：
//   - URL：required + url 格式（RFC 3986）+ max=2048（实践上限，多数浏览器
//     与 CDN 的 URL 长度上限在 2K–8K，按保守默认取 2048，业务可调）
//   - Description / ParseDepth：可选字段，仅做长度上限保护，避免恶意客户端
//     用超长字符串撑爆 DB 列。具体语义（如 ParseDepth 的 oneof 校验）仍由
//     service.NormalizeParseDepth 等深度防御逻辑兜底。
type LinkCreateRequest struct {
	URL string `json:"url" binding:"required,url,max=2048"`
	// Destination selects the durable capture host. Empty means inbox: a
	// capture is "put this aside", and its fetch, title and classification
	// are all still unconfirmed. Clients that want the reading library
	// directly must say so explicitly. Deployments without Reader Inbox fall
	// back to library for the empty case only — an explicit inbox request
	// still fails loudly rather than being silently rerouted.
	Destination string  `json:"destination,omitempty" binding:"omitempty,oneof=library inbox"`
	Description *string `json:"description,omitempty" binding:"omitempty,max=4096"` // 用户备注，附在链接上的可选描述；按 wave 10 默认值设置，业务可调
	// RequestedLibraryKind is optional for backwards compatibility. Empty is
	// equivalent to auto; final library_kind is always assigned by the server.
	RequestedLibraryKind string `json:"requested_library_kind,omitempty" binding:"omitempty,max=16"`
	// ParseDepth, when set, overrides the pipeline's default fetch
	// strategy for this link. "light" forces the truncated tag-only
	// fetch path (~7x cheaper); "deep" (or "full") forces the full
	// body fetch chain. Empty / omitted = use whatever the deployment
	// default is (see FETCH_PREFER_LIGHT). Case-insensitive on input.
	//
	// 仅做长度上限：实际 oneof 由 NormalizeParseDepth 做（大小写归一 + 422 slug）。
	ParseDepth *string `json:"parse_depth,omitempty" binding:"omitempty,max=16"`
}

// BatchCreateRequest 是 POST /api/links/batch 批量入库请求的 JSON 主体，
// Items 长度上限为 100；超过将被请求校验拒绝。
//
// validator binding tag：
//   - Items 必须存在且 1–100 条；超过 100 是与 service.defaultBatchSubmitLimit
//     对齐的保守默认，按 wave 10 默认值设置，业务可调。
//   - 子项不在 binding 层递归校验：Batch 的公开契约是逐项 partial success，
//     坏 URL / parse_depth / 超长备注应落在对应 results[index].error，而不是让
//     第一个坏项把整批提前变成顶层 422。字段语义和长度由 service 逐项校验。
type BatchCreateRequest struct {
	Items []LinkCreateRequest `json:"items" binding:"required,min=1,max=100"`
}

// ValidParseDepths is the canonical allow-list of accepted parse_depth
// values, lower-cased. Empty string is also accepted (meaning "use
// deployment default") but is not enumerated here so the handler can
// short-circuit it without traversing the slice.
var ValidParseDepths = []string{"light", "deep", "full"}

// NormalizeParseDepth returns the canonical lower-cased trimmed value
// for parse_depth, or an httperr 422 if the value is non-empty but not
// one of the allow-list members. The empty string passes through as ""
// (deployment default).
func NormalizeParseDepth(raw *string) (string, error) {
	if raw == nil {
		return "", nil
	}
	v := strings.ToLower(strings.TrimSpace(*raw))
	if v == "" {
		return "", nil
	}
	for _, ok := range ValidParseDepths {
		if v == ok {
			return v, nil
		}
	}
	return "", httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeUnsupportedParseDepth,
		"parse_depth must be one of: light, deep, full")
}

// IngestRequest 是多模态采集接口的请求体，允许一次提交多个不同来源（URL / 文本 / 图片等）。
//
// validator binding tag：Sources 必须存在且 1–64 条。browser_capture 通常只有
// 一个脱敏文本来源；较高上限是为显式多源 text/image 客户端保留。超过后在
// handler 层提前拒绝，避免进入 service 循环。
type IngestRequest struct {
	Sources []IngestSource `json:"sources" binding:"required,min=1,max=64,dive"`
	// Destination is optional; empty means inbox, matching the single-link
	// capture default. Deployments without Reader Inbox fall back to library
	// for the empty case only.
	Destination          string `json:"destination,omitempty" binding:"omitempty,oneof=library inbox site"`
	RequestedLibraryKind string `json:"requested_library_kind,omitempty" binding:"omitempty,max=16"`
}

// IngestSource 描述一条采集来源；Kind 决定其余字段的语义（例如 url / text / html / image）。
//
// validator binding tag：
//   - Kind：required + oneof（与 service.normalizeIngestRequest 的 switch
//     分支完全对齐：url / text / image / browser_capture）。前置校验避免无效
//     payload 进入 service 大循环。
//   - URL：不强制 url 格式校验，因为 kind=image 时允许 data:image/* base64 URL；
//     仅 max 上限保护。具体格式校验由 service.validateURL / validateImageLocator
//     做深度防御。
//   - Title / Text / HTML：仅做通用接口的长度上限。第一方 browser_capture 在
//     扩展侧把脱敏纯文本与正文结构快照分别限制为 512 KiB；HTML 不是整页 DOM，
//     且后端在转换为阅读文档时会再次执行安全过滤。
//   - ImageURLs：为通用多源客户端保留。browser_capture 路径忽略该字段；需要
//     图片分析时应提交显式 kind=image 来源。
//   - Metadata：validator 不递归 map[string]any（无法表达深度校验），完整规则
//     仍交给 service.validateIngestMetadata。
//
// 以上数值均按 wave 10 默认值设置，业务可调。
type IngestSource struct {
	Kind      string         `json:"kind" binding:"required,oneof=url text image browser_capture"`      // 来源类型
	URL       string         `json:"url,omitempty" binding:"omitempty,max=2048"`                        // Kind=url/image 时的目标地址
	Title     string         `json:"title,omitempty" binding:"omitempty,max=512"`                       // 调用方预先获取到的标题（可选）
	Text      string         `json:"text,omitempty" binding:"omitempty,max=4194304"`                    // 通用文本正文；第一方 browser_capture ≤512KiB
	HTML      string         `json:"html,omitempty" binding:"omitempty,max=4194304"`                    // HTML 结构；第一方 browser_capture 发送脱敏正文片段
	ImageURLs []string       `json:"image_urls,omitempty" binding:"omitempty,max=100,dive,max=1048576"` // 通用兼容字段；browser_capture 忽略
	Metadata  map[string]any `json:"metadata,omitempty"`                                                // 透传的自定义元数据，深度校验由 service 完成
}

// TranslationCreateRequest creates either a selected-text translation or a
// full saved-original translation. target_language is intentionally absent:
// the initial API only supports automatic source-language detection to zh-CN.
type TranslationCreateRequest struct {
	Scope                   string  `json:"scope" binding:"required,oneof=selection full"`
	BlockKey                string  `json:"block_key,omitempty" binding:"omitempty,max=64"`
	StartOffset             int     `json:"start_offset,omitempty" binding:"omitempty,min=0"`
	EndOffset               int     `json:"end_offset,omitempty" binding:"omitempty,min=0"`
	SourceText              string  `json:"source_text,omitempty" binding:"omitempty,max=16384"`
	ExpectedContentRevision *int64  `json:"expected_content_revision,omitempty" binding:"omitempty,min=1"`
	ExpectedSourceHash      *string `json:"expected_source_hash,omitempty" binding:"omitempty,min=1,max=128"`
	Force                   bool    `json:"force,omitempty"`
}

// ContentEditRequest replaces the saved original snapshot without fetching
// the remote page. expected_content_revision is the optimistic-concurrency
// token observed by the Reader.
type ContentEditRequest struct {
	Content                 string `json:"content"`
	ExpectedContentRevision int64  `json:"expected_content_revision" binding:"min=1"`
}

// SiteUpdateRequest is a partial, user-authored site profile update. Pointer
// fields preserve the difference between omitted fields and supplied values.
type SiteUpdateRequest struct {
	Name        *string              `json:"name,omitempty" binding:"omitempty,max=256"`
	Intro       *string              `json:"intro,omitempty" binding:"omitempty,max=1000"`
	HomepageURL *string              `json:"homepage_url,omitempty" binding:"omitempty,max=2048"`
	IconURL     *string              `json:"icon_url,omitempty" binding:"omitempty,max=2048"`
	UserNote    *string              `json:"user_note,omitempty" binding:"omitempty,max=10000"`
	Pinned      *bool                `json:"pinned,omitempty"`
	Tags        *SiteTagPatchRequest `json:"tags,omitempty"`
}

// SiteTagPatchRequest maintains user-owned site tags as a delta. An omitted
// tags field leaves tags untouched; add/remove are de-duplicated and validated
// by the service before they reach the transaction.
type SiteTagPatchRequest struct {
	Add    []string `json:"add,omitempty" binding:"omitempty,max=50,dive,min=1,max=128"`
	Remove []string `json:"remove,omitempty" binding:"omitempty,max=50,dive,min=1,max=128"`
}

// SiteEntryUpdateRequest is a partial user edit of one existing site entry.
// Revision lives in If-Match so profile and entry writes share one CAS model.
type SiteEntryUpdateRequest struct {
	Name    *string `json:"name,omitempty" binding:"omitempty,min=1,max=256"`
	Purpose *string `json:"purpose,omitempty" binding:"omitempty,max=1000"`
}

// ConversionPreviewRequest asks the server to describe the consequences of
// moving a completed link between the reading and site collections.
type ConversionPreviewRequest struct {
	TargetKind string `json:"target_kind" binding:"required,oneof=reading site"`
}

// ConversionExecuteRequest applies a previously inspected conversion. The
// source revision and, where applicable, the target site's revision make the
// destructive operation compare-and-swap rather than last-write-wins.
type ConversionExecuteRequest struct {
	TargetKind              string  `json:"target_kind" binding:"required,oneof=reading site"`
	ExpectedContentRevision int64   `json:"expected_content_revision" binding:"min=0"`
	ExpectedSiteRevision    *int64  `json:"expected_site_revision,omitempty" binding:"omitempty,min=0"`
	TargetSiteID            *string `json:"target_site_id,omitempty" binding:"omitempty,uuid"`
	ConfirmDestructive      bool    `json:"confirm_destructive,omitempty"`
	PreservedUserNote       *string `json:"preserved_user_note,omitempty" binding:"omitempty,max=10000"`
}

// ClassificationRuleCreateRequest is intentionally scope-oriented: shared
// platform validation happens server-side and never relies on UI affordances.
type ClassificationRuleCreateRequest struct {
	Host            string  `json:"host" binding:"required,max=253"`
	IdentityAdapter *string `json:"identity_adapter,omitempty" binding:"omitempty,max=64"`
	PathPrefix      *string `json:"path_prefix,omitempty" binding:"omitempty,max=2048"`
	TargetKind      string  `json:"target_kind" binding:"required,oneof=reading site"`
	Enabled         *bool   `json:"enabled,omitempty"`
}

// OptionalString preserves all three PATCH states for nullable scope fields:
// omitted, explicitly cleared with null, and supplied with a string. It is an
// input-only type; response DTOs continue to use ordinary *string fields.
type OptionalString struct {
	Set   bool
	Value *string
}

func (o *OptionalString) UnmarshalJSON(data []byte) error {
	o.Set = true
	if bytes.Equal(data, []byte("null")) {
		o.Value = nil
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	o.Value = &value
	return nil
}

// ClassificationRuleUpdateRequest is intentionally narrower than a generic
// JSON merge patch. Updating a scope requires host, adapter, and prefix
// together, so a shared-platform rule can never be widened by an omitted
// field. OptionalString keeps explicit null available for changing a scoped
// rule into a host-wide rule on a non-shared host.
type ClassificationRuleUpdateRequest struct {
	Host            *string        `json:"host,omitempty" binding:"omitempty,max=253"`
	IdentityAdapter OptionalString `json:"identity_adapter,omitempty"`
	PathPrefix      OptionalString `json:"path_prefix,omitempty"`
	TargetKind      *string        `json:"target_kind,omitempty" binding:"omitempty,oneof=reading site"`
	Enabled         *bool          `json:"enabled,omitempty"`
}

// LibraryReviewResolveRequest resolves one pending review with the revision
// returned by the list API. Action-specific object writes are deliberately
// delegated to their owning workflows; this endpoint owns queue lifecycle.
type LibraryReviewResolveRequest struct {
	ExpectedRevision   int64  `json:"expected_revision" binding:"required,min=1"`
	Resolution         string `json:"resolution" binding:"required,oneof=applied dismissed"`
	Action             string `json:"action,omitempty" binding:"omitempty,oneof=keep_reading move_to_site"`
	ConfirmDestructive bool   `json:"confirm_destructive,omitempty"`
}

type SiteRevisionRef struct {
	SiteID   string `json:"site_id" binding:"required,uuid"`
	Revision int64  `json:"revision" binding:"required,min=1"`
}

// SiteMergePreviewRequest names one stable target and one or more source
// aggregates. Revisions make the preview describe a concrete snapshot rather
// than a best-effort suggestion that an execute call could silently race.
type SiteMergePreviewRequest struct {
	TargetSiteID   string            `json:"target_site_id" binding:"required,uuid"`
	TargetRevision int64             `json:"target_revision" binding:"required,min=1"`
	Sources        []SiteRevisionRef `json:"sources" binding:"required,min=1,max=20,dive"`
}

type SiteMergeFieldResolutionRequest struct {
	Field  string `json:"field" binding:"required,oneof=name intro homepage_url icon_url user_note"`
	Choice string `json:"choice" binding:"required,oneof=target source"`
	// SourceSiteID identifies the specific preview conflict. It is required
	// even for "target": several source sites may conflict on one field.
	SourceSiteID string `json:"source_site_id" binding:"required,uuid"`
}

// SiteMergeExecuteRequest repeats the revision-bound preview target. Every
// reported user-field conflict must appear exactly once in Resolutions; this
// makes an intentional keep-target decision as explicit as taking a source.
type SiteMergeExecuteRequest struct {
	TargetSiteID   string                            `json:"target_site_id" binding:"required,uuid"`
	TargetRevision int64                             `json:"target_revision" binding:"required,min=1"`
	Sources        []SiteRevisionRef                 `json:"sources" binding:"required,min=1,max=20,dive"`
	Resolutions    []SiteMergeFieldResolutionRequest `json:"resolutions,omitempty" binding:"omitempty,max=100,dive"`
}

// SiteSplitRequest is the complete revision-bound split proposal consumed by
// both preview and execute. An empty identity list leaves every binding on the
// source site; the one-item maximum makes ownership unambiguous.
type SiteSplitRequest struct {
	ExpectedRevision       int64    `json:"expected_revision" binding:"required,min=1"`
	EntryIDs               []string `json:"entry_ids" binding:"required,min=1,max=100,dive,uuid"`
	Name                   string   `json:"name" binding:"required,min=1,max=256"`
	Intro                  *string  `json:"intro,omitempty" binding:"omitempty,max=1000"`
	HomepageURL            *string  `json:"homepage_url,omitempty" binding:"omitempty,max=2048"`
	IconURL                *string  `json:"icon_url,omitempty" binding:"omitempty,max=2048"`
	UserNote               *string  `json:"user_note,omitempty" binding:"omitempty,max=10000"`
	PrimaryEntryID         string   `json:"primary_entry_id" binding:"required,uuid"`
	IdentityKeysForNewSite []string `json:"identity_keys_for_new_site,omitempty" binding:"omitempty,max=1,dive,min=1,max=512"`
}
