package service

import (
	"context"
	"errors"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
	"unicode/utf8"

	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/security"
	"webtag/internal/urlidentity"
)

const (
	maxLinkURLLength         = 2048
	maxLinkDescriptionLength = 4096

	// defaultRefreshCooldown is the minimum wall-clock interval between
	// successful Refresh calls for the same link. Prevents thrash from
	// clients that loop POST /api/links/:id/refresh and repeatedly re-run
	// the analyzer pipeline.
	// 60s is a tradeoff: short enough that a human user clicking
	// "refresh" twice in a row sees the second one work after waiting,
	// long enough that a buggy script burns at most one job per minute.
	defaultRefreshCooldown = 60 * time.Second
)

const (
	captureDestinationLibrary = "library"
	captureDestinationInbox   = "inbox"
	captureDestinationSite    = "site"
)

// URLLocker 按 URL 串行化执行 fn；用于保证同一链接的并发提交 / 刷新不会
// 同时进入「查 → 写」临界区。
type URLLocker interface {
	WithURL(context.Context, string, func(context.Context) error) error
	WithURLs(context.Context, []string, func(context.Context) error) error
}

// SubmitService handles the URL-keyed write surface: Submit (single URL) and
// Refresh (re-queue an existing link by id). Multi-modal /api/ingest is handled by IngestService —
// both share the *linkSubmitter core (createNewLink / requeueExisting
// / submitExisting / requireExisting).
type SubmitService struct {
	core            *linkSubmitter
	refreshCooldown time.Duration
	// now is injected so cooldown tests can drive the clock.
	now func() time.Time
}

// SubmitServiceOptions wires operator-tuned SubmitService behavior.
// A non-positive RefreshCooldown falls back to defaultRefreshCooldown.
type SubmitServiceOptions struct {
	RefreshCooldown time.Duration
	// InboxWriter resolves existing Inbox captures for the default destination.
	InboxWriter InboxCaptureWriter
	// InboxProposalCommands atomically creates or repairs the proposal attempt and River row.
	InboxProposalCommands InboxProposalCommands
}

// normalizeCaptureDestination 解析采集目的地。缺省值由调用方给出，因为不同入口
// 的语义不同：用户主动收藏（单条、批量、ingest）缺省进收件箱——收藏是「先收着」，
// 抓取质量、标题、归类都还没确认，直接进库会把未经确认的内容混进阅读库；而订阅源
// 的条目分析（AnalyzeRSS）缺省仍是 library，它由订阅刷新驱动、结果关联回 feed
// item，不是一次收藏动作，进收件箱会把它淹没。
func normalizeCaptureDestination(raw, fallback string) (string, error) {
	destination := strings.ToLower(strings.TrimSpace(raw))
	if destination == "" {
		destination = fallback
	}
	switch destination {
	case captureDestinationLibrary, captureDestinationInbox:
		return destination, nil
	default:
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_destination", "destination must be library or inbox")
	}
}

// validateURL 校验提交链接的合法性并返回它的**规范化身份**：必须是 http/https、
// 能解析出主机，且主机不在 SSRF 黑名单内。任何失败都返回 422 httperr，调用方
// 直接透传即可。
//
// Wave 9 MED 迁移：把这条热路径上的所有 httperr.New 改成 NewWithCode，
// 让前端能按 slug 分支处理（"是格式不合法还是 SSRF 拒绝？"），不再依
// 赖 message 字面量做正则。
//
// 规范化本身住在 internal/urlidentity，不在这里内联：links / ingest /
// Inbox / Extension / Reader / 订阅保存七个入口都经由本函数取身份，规则只有一
// 份实现，入口无从各自发明变体。返回值是查重、任务复用和建记录用的唯一 key；
// 原始 URL 由调用方另行保留作展示与来源证据。
func validateURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeURLRequired, "url is required")
	}
	if len(rawURL) > maxLinkURLLength {
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeURLTooLong, "url exceeds length limit")
	}

	normalized, err := urlidentity.Normalize(rawURL)
	if err != nil {
		return "", urlIdentityError(err)
	}
	// SSRF 判定跑在规范化之后：大小写、尾点和 www 折叠都可能被用来把一个内网
	// 主机写成黑名单匹配不到的样子，规范化前判定等于给绕过留门。
	host, err := normalizedHost(normalized)
	if err != nil {
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidURL, "invalid url")
	}
	if security.IsUnsafeHost(host) {
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeUnsafeURLTarget, "unsafe url target")
	}
	return normalized, nil
}

// urlIdentityError maps the normalisation contract's failure modes onto the
// public 422 codes clients already branch on.
func urlIdentityError(err error) error {
	switch {
	case errors.Is(err, urlidentity.ErrEmpty):
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeURLRequired, "url is required")
	case errors.Is(err, urlidentity.ErrUnsupportedScheme):
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeUnsupportedURLScheme, "unsupported url scheme")
	default:
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidURL, "invalid url")
	}
}

// originalURLMetadataKey names the source-evidence field. It is deliberately
// not a lookup key: nothing may query by it.
const originalURLMetadataKey = "original_url"

// withOriginalURL keeps the URL exactly as the client wrote it whenever
// normalisation rewrote it. links.url also keeps the trimmed display spelling;
// this metadata remains explicit source evidence for import and audit paths.
//
// Already-canonical input adds nothing, so metadata stays nil for the common
// case and rows that never needed source metadata keep telling "no metadata"
// apart from "empty JSONB".
func withOriginalURL(metadata map[string]any, submitted, normalized string) map[string]any {
	trimmed := strings.TrimSpace(submitted)
	if trimmed == "" || trimmed == normalized {
		return metadata
	}
	if metadata == nil {
		metadata = make(map[string]any, 1)
	}
	metadata[originalURLMetadataKey] = trimmed
	return metadata
}

func normalizedHost(normalized string) (string, error) {
	parsed, err := neturl.Parse(normalized)
	if err != nil {
		return "", err
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return "", errors.New("normalized url has no host")
	}
	return host, nil
}

func validateLinkDescription(description *string) error {
	if description == nil || utf8.RuneCountInString(*description) <= maxLinkDescriptionLength {
		return nil
	}
	return httperr.NewWithCode(
		http.StatusUnprocessableEntity,
		httperr.CodeDescriptionTooLong,
		"description exceeds length limit",
	)
}

func normalizeRequestedLibraryKind(raw string) (model.RequestedLibraryKind, error) {
	switch model.RequestedLibraryKind(strings.ToLower(strings.TrimSpace(raw))) {
	case "", model.RequestedLibraryKindAuto:
		return model.RequestedLibraryKindAuto, nil
	case model.RequestedLibraryKindReading:
		return model.RequestedLibraryKindReading, nil
	case model.RequestedLibraryKindSite:
		return model.RequestedLibraryKindSite, nil
	default:
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidRequestedLibraryKind, "requested_library_kind must be auto, reading, or site")
	}
}

type noopURLLocker struct{}

func (noopURLLocker) WithURL(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

func (noopURLLocker) WithURLs(ctx context.Context, _ []string, fn func(context.Context) error) error {
	return fn(ctx)
}
