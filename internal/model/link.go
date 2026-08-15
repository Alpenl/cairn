package model

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// LinkStatus 表示一条链接（links 表）的整体处理状态。
type LinkStatus string

// 链接的五种状态：skeleton 仅保留给历史占位行，新的解析流程不再自动创建；
// 其余四种与解析任务对齐。
const (
	LinkStatusSkeleton   LinkStatus = "skeleton"   // 历史占位行，保留兼容旧数据
	LinkStatusPending    LinkStatus = "pending"    // 已入库，等待解析
	LinkStatusProcessing LinkStatus = "processing" // 正在解析
	LinkStatusDone       LinkStatus = "done"       // 解析完成
	LinkStatusFailed     LinkStatus = "failed"     // 解析失败
)

// ContentType 标记一条链接的内容形态，用于前端分组和路由展示策略。
type ContentType string

// 已支持的内容形态枚举。
const (
	ContentTypeArticle  ContentType = "article"  // 文章页
	ContentTypeListing  ContentType = "listing"  // 列表 / 索引页
	ContentTypeHomepage ContentType = "homepage" // 首页 / 站点根
	ContentTypeUnknown  ContentType = "unknown"  // 无法判定
)

// LibraryKind is the user-facing collection partition. It intentionally stays
// independent from ContentType: an article can be reading content while a
// homepage can be a site entry.
type LibraryKind string

const (
	LibraryKindReading LibraryKind = "reading"
	LibraryKindSite    LibraryKind = "site"
)

// NormalizeOptionalLibraryKind canonicalizes the optional final collection
// partition used by scoped read APIs. The empty kind means the caller omitted
// the scope; every non-empty value must name a persisted final partition.
func NormalizeOptionalLibraryKind(raw string) (LibraryKind, bool) {
	kind := LibraryKind(strings.ToLower(strings.TrimSpace(raw)))
	switch kind {
	case "", LibraryKindReading, LibraryKindSite:
		return kind, true
	default:
		return "", false
	}
}

// RequestedLibraryKind records the capture-time collection intent. Auto must
// never be persisted as Link.LibraryKind, but it is persisted independently so
// a later refresh cannot mistake an automatic final partition for user intent.
type RequestedLibraryKind string

const (
	RequestedLibraryKindAuto    RequestedLibraryKind = "auto"
	RequestedLibraryKindReading RequestedLibraryKind = "reading"
	RequestedLibraryKindSite    RequestedLibraryKind = "site"
)

// RequestedLibraryKindSource records who selected the persisted capture
// intent. System hard rules (for example RSS -> reading) use auto even when the
// requested kind is concrete; only an explicit public reading/site selection
// uses user.
type RequestedLibraryKindSource string

const (
	RequestedLibraryKindSourceAuto RequestedLibraryKindSource = "auto"
	RequestedLibraryKindSourceUser RequestedLibraryKindSource = "user"
)

// LibraryKindSource records how the final collection partition was chosen.
type LibraryKindSource string

const (
	LibraryKindSourceAuto      LibraryKindSource = "auto"
	LibraryKindSourceUser      LibraryKindSource = "user"
	LibraryKindSourceMigration LibraryKindSource = "migration"
)

// ContentFormat declares how SavedContent.Document should be interpreted.
// Text is always the canonical plain-text projection regardless of format.
type ContentFormat string

const (
	ContentFormatPlain    ContentFormat = "plain"
	ContentFormatMarkdown ContentFormat = "markdown"
	ContentFormatHTML     ContentFormat = "html"
)

// ContentSource records who produced the currently saved original snapshot.
// It is intentionally separate from FetcherType: a user edit is still stored
// with the canonical content pipeline, but it did not come from a fetcher.
type ContentSource string

const (
	ContentSourceFetched ContentSource = "fetched"
	ContentSourceUser    ContentSource = "user"
)

// SavedContent is the on-demand original-content snapshot. Text remains the
// stable search/indexing projection; Document carries optional reading
// structure without overloading Text with markup.
type SavedContent struct {
	Text     string
	Document *string
	Format   ContentFormat
	Source   ContentSource
	// CJKChars / Words 是两项阅读计数（CJK 字符数、西文词数）。它们与正文
	// 写在同一条 UPDATE 里，因此不存在「计数与正文不同步」的窗口。
	//
	// 计数公式仍然只有 service.countReadingUnits 那一份——SQL 无法精确复刻
	// 前端那两条正则，硬凑一份近似的会让折叠态与展开态给出两个不同的数字。
	CJKChars int
	Words    int
	// Revision 是这份正文所属的 content_revision 代次，只在**读**方向有意义：
	// GetContent 填充它，写入方法忽略调用方设的值（代次由 SQL 自增决定）。
	//
	// 它必须一路传到 API 响应：Reader 用 (linkId, content_revision) 做正文缓存键，
	// 也用它判定划线 envelope 是否仍对应当前正文。保存原文后若不把新代次交给
	// 客户端，客户端会在下一次列表刷新前带着旧代次写划线，刷新后判定失配而
	// 静默丢弃——见 reader/src/lib/annotations.ts 的 isContentAnchored 与
	// useRevisionedAnnotations。
	Revision int64
}

// Link 是 links 表的领域模型，保存原始输入、解析产物以及状态 / 诊断字段。
type Link struct {
	ID uuid.UUID
	// HasContent / ContentCJKChars / ContentWords 是「已保存原文」的三项派生
	// 事实，PF6 起落成列并进入列表投影。此前列表恒报 has_content=false，
	// 而详情端为了这三个数字要把整篇正文读出来再扔掉。
	HasContent                 bool
	ContentCJKChars            int
	ContentWords               int
	URL                        string
	SourceKind                 string         // 来源类型：url / text / html / image 等
	SourceKey                  string         // 用于去重的唯一键（通常是规范化后的 URL 或哈希）
	InputTitle                 *string        // 调用方预提供的标题
	InputText                  *string        // 调用方预提供的纯文本正文
	InputHTML                  *string        // 调用方预提供的 HTML 片段
	InputImages                []string       // 关联图片 URL 列表
	SourceMetadata             map[string]any // 透传的来源元数据，例如 parse_depth
	Title                      *string        // 解析后的标题
	Summary                    *string        // 解析生成的摘要
	Tags                       []string       // 解析生成的原始标签（未经过 concept 规范化）
	FetcherType                *string        // 实际使用的抓取器类型
	IsLowConfidence            bool           // 是否被判定为低置信度结果
	LowConfidenceReason        *string        // 低置信度具体原因
	Status                     LinkStatus
	ErrorMsg                   *string // 失败时的简短错误描述
	Description                *string // 用户备注
	Domain                     *string // 链接所属域名
	ContentType                *string // 内容形态：article / listing / homepage / unknown
	RequestedLibraryKind       RequestedLibraryKind
	RequestedLibraryKindSource RequestedLibraryKindSource
	LibraryKind                *LibraryKind
	LibraryKindSource          *LibraryKindSource
	LibraryKindLocked          bool
	PredictedLibraryKind       *LibraryKind
	ClassificationConfidence   *float32
	ClassificationReason       *string
	ClassificationExplanation  *string
	ClassifierVersion          *string
	ContentRevision            int64
	MetadataRevision           int64
	ContentSource              ContentSource
	FirstCollectedAt           time.Time
	LastRecollectedAt          *time.Time
	PayloadPurgeDueAt          *time.Time
	PayloadPurgedAt            *time.Time
	PathDepth                  *int       // URL 路径深度
	ParentPath                 *string    // 父路径字符串
	ParentID                   *uuid.UUID // 父链接 ID（构成域名内树形结构）
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}
