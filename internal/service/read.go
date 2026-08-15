package service

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// treeListVisibleSoftCap mirrors repository/tree_repo.go's LIMIT 5000
// so this layer can log a Warn when ListVisible returns exactly the
// cap — operators then know the response was truncated and consumers
// should narrow by domain or page through a different endpoint.
const treeListVisibleSoftCap = 5000

// listFilter caps protect /api/links from abuse: a 10k-tag query string
// otherwise rides straight to PG with a wasted budget. The numbers are
// generous compared to any sane UI use (typical filter is 1-3 tags) but
// short of the request-body / URL-length cliffs.
const (
	maxListTagFilters   = 32
	maxListTagFilterLen = 64
	maxListDomainLen    = 253 // RFC 1035 hostname cap
	// maxListQueryLen caps the ?q= hybrid-search query length. Generous for
	// any sane search (a phrase, not a document) but short of overflowing the
	// embedding input or the URL-length cliff. Over-length q returns 422.
	maxListQueryLen = 512
)

// allowedContentTypes mirrors the OpenAPI enum. Empty/missing is also a
// valid filter (means "no content_type filter"). Any other value is
// rejected with 422 to avoid the spec lying about what the API accepts.
var allowedContentTypes = map[string]struct{}{
	"article":  {},
	"listing":  {},
	"homepage": {},
	"unknown":  {},
}

// allowedListStatuses is the whitelist for the ?status= filter. skeleton
// is deliberately excluded — those are placeholder rows that have not
// received any input yet and are never something a client lists. Empty /
// missing ?status= includes every user-saved row: status is enrichment state,
// not proof that the capture exists. Any token outside this set is rejected
// with 400 so a typo surfaces loudly instead of returning an empty page.
var allowedListStatuses = map[string]struct{}{
	string(model.LinkStatusPending):    {},
	string(model.LinkStatusProcessing): {},
	string(model.LinkStatusFailed):     {},
	string(model.LinkStatusDone):       {},
}

var defaultVisibleStatuses = []string{
	string(model.LinkStatusPending),
	string(model.LinkStatusProcessing),
	string(model.LinkStatusFailed),
	string(model.LinkStatusDone),
}

// maxListStatusFilters caps the parsed ?status= set. There are only four
// legal values, so anything past that is duplicated input or abuse; the
// bound keeps a pathological query string from forcing repeated whitelist
// lookups.
const maxListStatusFilters = 8

// CacheInvalidator 抽象「清空内存缓存」的能力，pipeline 写完链接后调用
// 一下让标签聚合在下一次读时重新加载。生产环境由 *TagCache 实现。
//
// 带 ctx 是租户化的必然结果：缓存按租户分键之后，失效必须知道打掉谁的那一份。
// 无参版本在 MODE=multi 下要么打掉所有人（让 TTL 形同虚设），要么打错人。
type CacheInvalidator interface {
	Invalidate(context.Context)
}

// MultiCacheInvalidator 把一次失效扇出到多个缓存，nil 成员自动跳过。
//
// 存在的理由：链接的增 / 删 / 重新解析会同时改变标签聚合与域名聚合，
// 只失效其中一个会让侧栏的两组数字在整个 TTL 窗口里互相矛盾。
type MultiCacheInvalidator []CacheInvalidator

func (m MultiCacheInvalidator) Invalidate(ctx context.Context) {
	for _, invalidator := range m {
		if invalidator != nil {
			invalidator.Invalidate(ctx)
		}
	}
}

// ConceptDisplayLookup overrides link.tags with concept display
// names so two links tagged "RAG" and "检索增强生成" surface the
// same string (the surface form most commonly used across all links
// attached to that concept). Optional — nil keeps the raw LLM tags
// from links.tags, which is the pre-concept-layer behaviour.
type ConceptDisplayLookup interface {
	ListDisplayNamesByLinkIDs(ctx context.Context, linkIDs []uuid.UUID) (map[uuid.UUID][]string, error)
}

// LinkReadService 负责 /api/links 读取面：列表 / 详情 / 删除，以及把
// concept 显示名等可选信息缝合到响应里。
//
// cursorKey 来自 config.Config.CursorSigningKey；非空时游标 token 会带
// HMAC-SHA256 截短签名，杜绝伪造（Wave 9 MED M5）。零值（nil）保持向
// 后兼容的明文 base64 格式。
type linkReadStore interface {
	repository.LinkDetailReader
	repository.LinkLifecycleReader
}

type LinkReadService struct {
	links          linkReadStore
	tagInvalidator CacheInvalidator
	conceptDisplay ConceptDisplayLookup
	cursorKey      []byte
	// queryEmbedder vectorizes the ?q= search query (Phase 9). nil or
	// disabled (EMBEDDING_MODEL unset) → the q= path degrades to pure ILIKE.
	queryEmbedder RetrievalEmbedder
	// querySF collapses concurrent identical ?q= vectorizations into one
	// upstream embedding call (thundering herd on a popular search term).
	// Key = model + query；与 tag_cache 的 singleflight 同模式。
	querySF singleflight.Group
	// logger surfaces the q= vectorization-failure WARN. nil-safe (log calls
	// degrade to no-ops).
	logger *slog.Logger
	// conceptExport streams the installation's canonical vocabulary for
	// GET /api/export/concepts. nil makes ExportConcepts emit an empty array.
	conceptExport ConceptExporter
	// contentReader 取详情页「已保存原文」（GET /api/links/:id）。nil → 详情不带
	// content 字段。独立于列表路径（列表不读 content，避免大文本开销）。
	contentReader  LinkContentReader
	deleteCommands LinkDeletionCommands
	mutationLocker URLLocker
}

// LinkContentReader 读取某条 link 已保存的原文（「保存原文」功能）。
type LinkContentReader interface {
	GetContent(ctx context.Context, id uuid.UUID) (*model.SavedContent, error)
}

// LinkReadServiceOptions wires the complete read-side service in one step.
type LinkReadServiceOptions struct {
	// Links is required because every public operation reads or deletes links.
	Links linkReadStore
	// TagInvalidator is optional; nil skips cache invalidation after delete.
	TagInvalidator CacheInvalidator
	// ConceptDisplay is optional; nil preserves the stored surface tags.
	ConceptDisplay ConceptDisplayLookup
	// CursorSigningKey is optional; empty keeps the legacy unsigned cursor.
	CursorSigningKey string
	// QueryEmbedder is optional; nil degrades search to ILIKE.
	QueryEmbedder RetrievalEmbedder
	// Logger is optional; nil suppresses fail-soft vectorization warnings.
	Logger *slog.Logger
	// ConceptExporter is optional; nil exports an empty concept array.
	ConceptExporter ConceptExporter
	// ContentReader is optional; nil omits saved source content from details.
	ContentReader LinkContentReader
	// DeleteCommands owns link deletion and durable job cancellation in one
	// transaction. It is required by Delete but not by read-only methods.
	DeleteCommands LinkDeletionCommands
	// MutationLocker must be the same URL locker used by submit/requeue and Deep
	// Research trigger so those lifecycle mutations cannot interleave.
	MutationLocker URLLocker
}

// NewLinkReadService constructs a fully initialized read-side link surface.
func NewLinkReadService(opts LinkReadServiceOptions) *LinkReadService {
	if opts.Links == nil {
		panic("service.NewLinkReadService: Links is required")
	}
	var cursorKey []byte
	if opts.CursorSigningKey != "" {
		cursorKey = []byte(opts.CursorSigningKey)
	}
	mutationLocker := opts.MutationLocker
	if mutationLocker == nil {
		mutationLocker = noopURLLocker{}
	}
	return &LinkReadService{
		links:          opts.Links,
		tagInvalidator: opts.TagInvalidator,
		conceptDisplay: opts.ConceptDisplay,
		cursorKey:      cursorKey,
		queryEmbedder:  opts.QueryEmbedder,
		logger:         opts.Logger,
		conceptExport:  opts.ConceptExporter,
		contentReader:  opts.ContentReader,
		deleteCommands: opts.DeleteCommands,
		mutationLocker: mutationLocker,
	}
}
