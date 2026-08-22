package service

import (
	"context"

	"github.com/google/uuid"

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
	// maxListQueryLen caps the ?q= search query length. It is generous for a
	// phrase while staying below practical URL-length cliffs. Over-length q
	// returns 422.
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

// allowedListStatuses is the whitelist for the ?status= filter. Empty or
// missing ?status= includes every saved row. Any token outside this set is
// rejected with 400 so a typo surfaces loudly instead of returning an empty page.
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

// LinkReadService 负责 /api/links 的列表、详情与删除。
//
// cursorKey 来自 config.Config.CursorSigningKey；所有游标 token 都带
// HMAC-SHA256 截短签名。
type linkReadStore interface {
	GetDetailByID(context.Context, uuid.UUID) (*repository.LinkDetailProjection, error)
	GetDetailByURL(context.Context, string) (*repository.LinkDetailProjection, error)
	ListDone(context.Context, repository.ListLinksFilter) ([]model.Link, int, error)
	GetLifecycleByID(context.Context, uuid.UUID) (*repository.LinkLifecycleProjection, error)
}

type LinkReadService struct {
	links     linkReadStore
	cursorKey []byte
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
	// CursorSigningKey is required by production configuration.
	CursorSigningKey string
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
	mutationLocker := opts.MutationLocker
	if mutationLocker == nil {
		mutationLocker = noopURLLocker{}
	}
	return &LinkReadService{
		links:          opts.Links,
		cursorKey:      []byte(opts.CursorSigningKey),
		contentReader:  opts.ContentReader,
		deleteCommands: opts.DeleteCommands,
		mutationLocker: mutationLocker,
	}
}
