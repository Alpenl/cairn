package handler

import (
	"context"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/service"
)

// Dependencies bundles the per-domain service surfaces RegisterRoutes
// needs. Splitting Links into write/read/ingest sub-fields lets each
// handler depend on the narrowest interface it actually uses (ISP) and
// removes the previous god-interface that forced every test to mock
// seven methods even when it only exercised one.
type Dependencies struct {
	ReaderCapabilities *dto.ReaderCapabilitiesResponse
	LinksWrite         LinkWriteService
	LinksRead          LinkReadService
	LinksContent       LinkContentService
	ConversionPreview  LinkConversionPreviewService
	ConversionExecute  LinkConversionExecuteService
	Translations       LinkTranslationService
	Ingest             IngestService
	Tags               TagService
	Tree               TreeService
	Sites              SiteReadService
	SiteManagement     SiteManagementService
	SiteMerge          SiteMergeService
	SiteSplit          SiteSplitService
	ArchiveV2          interface {
		Export(context.Context, io.Writer, service.ArchiveV2ExportOptions) error
	}
	LibrarySearch LibrarySearchService
	// Feeds is optional for isolated handler tests; production always wires it.
	Feeds FeedService
	// Reader is optional only for isolated handler tests; production always wires it.
	Reader ReaderService
}

// LinkWriteService is the per-link write surface. Production wiring binds it
// to *service.SubmitService.
type LinkWriteService interface {
	Submit(context.Context, dto.LinkCreateRequest) (dto.SubmitResponse, error)
	Refresh(context.Context, string) (dto.SubmitResponse, error)
}

// LinkContentService 是「保存原文」的写surface（POST /api/links/:id/content）。
// 与 LinkWriteService 分开：保存原文是与打标/提交解耦的独立动作。生产绑定到
// *service.ContentService。
type LinkContentService interface {
	Save(ctx context.Context, linkID string) (dto.LinkContentResponse, error)
	Replace(ctx context.Context, linkID string) (dto.LinkContentResponse, error)
	Edit(ctx context.Context, linkID string, request dto.ContentEditRequest) (dto.LinkContentResponse, error)
	// Get 只读已保存原文，绝不触发抓取。Reader 把原文做成折叠区后，进详情页
	// 不再随详情拖回整篇正文，展开时才来取。
	Get(ctx context.Context, linkID string) (dto.LinkContentResponse, error)
}

// LinkConversionPreviewService provides the non-mutating first step of the
// collection conversion flow. It remains optional until every deployment has
// the conversion writer required by the execute endpoint.
type LinkConversionPreviewService interface {
	Preview(context.Context, string, dto.ConversionPreviewRequest) (dto.ConversionPreviewResponse, error)
}

type LinkConversionExecuteService interface {
	Execute(context.Context, string, dto.ConversionExecuteRequest) (dto.ConversionExecuteResponse, error)
}

type LinkTranslationService interface {
	Create(context.Context, uuid.UUID, model.TranslationRequest) (*model.LinkTranslation, error)
	List(context.Context, uuid.UUID) (model.TranslationList, error)
}

// IngestService isolates the multimodal /api/ingest entrypoint. It is a
// separate interface (and, after Wave 12.3 M5, a separate service
// struct) because the normalize-and-create flow it owns is materially
// different from the URL-keyed Submit/Refresh/Batch path — keeping it
// distinct stops a future Ingest-only change from re-touching the
// shared LinkWriteService surface.
type IngestService interface {
	Ingest(context.Context, dto.IngestRequest) (dto.SubmitResponse, error)
}

// LinkReadService is the per-link read surface; Delete lives here too
// because it is paired with cache-invalidation that only the read-side
// service holds. Production wiring binds this to *service.LinkReadService.
type LinkReadService interface {
	List(context.Context, dto.ListLinksRequest) (dto.PaginatedLinksResponse, error)
	Get(context.Context, string) (dto.LinkResponse, error)
	// GetWithContent 让调用方声明「详情要不要带上已保存原文的正文」。
	// Reader 传 include_content=false：原文默认折叠，展开时才单独去取，
	// 打开一篇文章不再顺带把整篇正文拖过网络。
	GetWithContent(ctx context.Context, linkID string, includeContent bool) (dto.LinkResponse, error)
	Delete(context.Context, string) error
}

// TagService 是 /api/tags 的 handler 侧契约：返回带计数的标签清单。
// 生产实现是 *service.TagReadService。
type TagService interface {
	List(context.Context) ([]dto.TagCountResponse, error)
}

// TreeService 是 /api/tree 的 handler 侧契约：按域名汇总返回链接的
// 树形结构。生产实现是 *service.TreeReadService。
type TreeService interface {
	Get(context.Context, string) (dto.TreeResponse, error)
	ListDomains(context.Context) (dto.DomainTreeSummaryEnvelope, error)
	ListDomainsScoped(context.Context, string) (dto.DomainTreeSummaryEnvelope, error)
}

type LibrarySearchService interface {
	Search(context.Context, string, int, int, int, string) (dto.GroupedSearchResponse, error)
}

const (
	defaultMaxJSONBodyBytes int64 = 1 << 20

	// ingestMaxJSONBodyBytes remains larger than the default JSON body cap for
	// generic multi-source text/image clients. First-party browser_capture
	// sends a bounded readable-text snapshot only. Other endpoints keep the
	// tighter cap so an accidental body cannot trigger multi-megabyte reads.
	ingestMaxJSONBodyBytes int64 = 4 << 20

	// contentEditMaxJSONBodyBytes is deliberately larger than the service's
	// decoded-content limit. JSON escaping can expand a valid 2 MiB UTF-8
	// string substantially before the decoder hands it to the service.
	contentEditMaxJSONBodyBytes int64 = 8 << 20
)

const apiRoutePrefix = "/api"

// RegisterRoutes wires the public API surface. It panics on nil required
// dependencies so a misconfigured runtime fails at boot rather than
// surfacing 500s at request time.
func RegisterRoutes(router gin.IRouter, deps Dependencies) { //nolint:gocyclo // 路由表：每条 route 一行，分支数等于端点数
	mustHaveServices(deps)

	prefix := apiRoutePrefix
	router.GET(prefix+"/capabilities", capabilities(deps.Reader != nil, deps.ReaderCapabilities))
	router.POST(prefix+"/ingest", ingestSubmission(deps.Ingest))

	links := router.Group(prefix + "/links")
	links.POST("", submitLink(deps.LinksWrite))
	links.POST("/:link_id/refresh", refreshLink(deps.LinksWrite))
	if deps.LinksContent != nil {
		links.GET("/:link_id/content", getLinkContent(deps.LinksContent))
		links.POST("/:link_id/content", saveLinkContent(deps.LinksContent))
		links.PUT("/:link_id/content", replaceLinkContent(deps.LinksContent))
		links.PATCH("/:link_id/content", editLinkContent(deps.LinksContent))
	}
	if deps.Translations != nil {
		links.POST("/:link_id/translations", createLinkTranslation(deps.Translations))
		links.GET("/:link_id/translations", listLinkTranslations(deps.Translations))
	}
	if deps.ConversionPreview != nil {
		links.POST("/:link_id/conversion-preview", conversionPreview(deps.ConversionPreview))
	}
	if deps.ConversionExecute != nil {
		links.POST("/:link_id/convert", convertLink(deps.ConversionExecute))
	}
	links.GET("", listLinks(deps.LinksRead))
	links.GET("/:link_id", getLink(deps.LinksRead))
	links.DELETE("/:link_id", deleteLink(deps.LinksRead))

	if deps.ArchiveV2 != nil {
		router.GET(prefix+"/export/v2", exportArchiveV2(deps.ArchiveV2))
	}

	tags := router.Group(prefix + "/tags")
	tags.GET("", listTags(deps.Tags))
	if deps.LibrarySearch != nil {
		router.GET(prefix+"/search", groupedSearch(deps.LibrarySearch))
	}
	tree := router.Group(prefix + "/tree")
	tree.GET("", getTree(deps.Tree))
	sites := router.Group(prefix + "/sites")
	if deps.Sites != nil {
		sites.GET("", listSites(deps.Sites))
		sites.GET("/:site_id", getSite(deps.Sites))
		if deps.SiteManagement != nil {
			sites.PATCH("/:site_id", updateSite(deps.SiteManagement))
			sites.PATCH("/:site_id/entries/:entry_id", updateSiteEntry(deps.SiteManagement))
			sites.POST("/:site_id/entries/:entry_id/set-primary", setSitePrimaryEntry(deps.SiteManagement))
			sites.DELETE("/:site_id/entries/:entry_id", deleteSiteEntry(deps.SiteManagement))
			sites.DELETE("/:site_id", deleteSite(deps.SiteManagement))
		} else {
			sites.PATCH("/:site_id", sitesUnavailable())
			sites.PATCH("/:site_id/entries/:entry_id", sitesUnavailable())
			sites.POST("/:site_id/entries/:entry_id/set-primary", sitesUnavailable())
			sites.DELETE("/:site_id/entries/:entry_id", sitesUnavailable())
			sites.DELETE("/:site_id", sitesUnavailable())
		}
	} else {
		sites.GET("", sitesUnavailable())
		sites.GET("/:site_id", sitesUnavailable())
		sites.PATCH("/:site_id", sitesUnavailable())
		sites.PATCH("/:site_id/entries/:entry_id", sitesUnavailable())
		sites.POST("/:site_id/entries/:entry_id/set-primary", sitesUnavailable())
		sites.DELETE("/:site_id/entries/:entry_id", sitesUnavailable())
		sites.DELETE("/:site_id", sitesUnavailable())
	}
	if deps.SiteMerge != nil {
		router.POST(prefix+"/sites/merge-preview", siteMergePreview(deps.SiteMerge))
		router.POST(prefix+"/sites/merge", siteMergeExecute(deps.SiteMerge))
	}
	if deps.SiteSplit != nil {
		router.POST(prefix+"/sites/:site_id/split-preview", siteSplitPreview(deps.SiteSplit))
		router.POST(prefix+"/sites/:site_id/split", siteSplitExecute(deps.SiteSplit))
	}
	RegisterFeedRoutes(router, deps.Feeds)
	RegisterReaderRoutes(router, deps.Reader)
}

// mustHaveServices fails fast at boot when any required service field
// is nil. This replaces the per-handler nil guards that used to emit a
// 500 + JSON body at request time — production wiring never passed nil
// here, so the per-request defense was test-only noise. Tests register
// stub services for every required field via withStubDeps.
func mustHaveServices(deps Dependencies) {
	missing := make([]string, 0, 6)
	if deps.LinksWrite == nil {
		missing = append(missing, "LinksWrite")
	}
	if deps.LinksRead == nil {
		missing = append(missing, "LinksRead")
	}
	if deps.Ingest == nil {
		missing = append(missing, "Ingest")
	}
	if deps.Tags == nil {
		missing = append(missing, "Tags")
	}
	if deps.Tree == nil {
		missing = append(missing, "Tree")
	}
	if len(missing) > 0 {
		panic("handler.RegisterRoutes: missing required services: " + strings.Join(missing, ", "))
	}
}
