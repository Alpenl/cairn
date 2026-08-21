// Package service 是 WebTag 的业务编排层，把仓储 / 抓取 / 分析 / 队列等
// 底层组件拼装成上层 HTTP 处理器所需的业务能力。包内主要分四大业务流：
//
//   - ingest（ingest.go / ingest_normalize*.go）: 多模态 /api/ingest 入口，
//     负责把 URL / 文本 / 图片 / browser_capture 各种来源规范化成统一的入库参数。
//   - pipeline（pipeline*.go）: 单条链接的「抓取 → 分析 → 落库」主流程，
//     由队列 worker 驱动，是写入侧最复杂的一环。
//   - submit（submit*.go / link_submitter.go）: 用户单条 / 批量 / 刷新提交链接的入口，
//     与 ingest 共享同一个 *linkSubmitter 核心。
//   - read（read*.go）: 链接 / 标签 / 树状导航 / 任务详情等查询读取面。
//
// 此外 diagnostics 提供错误归类。
//
// 子包（可独立演进，与父包同层）：
//   - service/urlmeta：URL 结构分析（ClassifyURL / AncestorURLs），纯函数，
//     不依赖本包任何东西。
//   - service/urllock：按 URL 串行化的进程内锁实现。
//     接口定义留在消费侧（submit.go 的 URLLocker），实现搬出去，因此本包不
//     import 它——装配层负责把实现注入。
//   - service/linktranslation：全文 / 选段翻译的 service + River worker。
//     与本包零代码依赖（双向都是），是第一个被切出去的完整业务流。
//   - service/analyzer、service/translator：既有子包。
//
// 本包仍然偏大（117 个文件，其中 60 个非测试）。上面三个子包的共同特征是
// 「与其余部分零双向引用」，其余部分不能照做。
//
// 不能照做的原因是**共享面太宽**：read / pipeline / site / library / submit /
// ingest 六块共用提交入口、URL 解析、错误分类等一批未导出符号，按
// 目录切开会立刻要么成环、要么把主包整个拖进子包。要真切，得先把这些共享原语
// 下沉成叶子包，并在消费侧定义接口反转方向——那是一次独立的设计工作。
//
// 这里**不再给具体的引用计数**。此前写过一张「哪几对是双向」的表用来论证，
// 而它是用符号名归属的正则统计出来的：Go 里方法名跨类型重名极其普遍
// （Get / List / Run / Create…），这种统计会把不存在的边算进去，也会漏掉真实
// 的边。用 go/types 复核时得到的是第三组数字，三组互不一致。一个复现不出的
// 数字比没有数字更糟——它看起来像证据。
//
// 要重新论证，请用 go/types 的 TypesInfo.Uses 做真实符号解析，并把方法**接收者
// 类型**的声明位置作为归属，而不是按文件名前缀分组。
package service

import (
	"context"
	"log/slog"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
	"webtag/internal/service/analyzer"
	"webtag/internal/service/urlmeta"
	"webtag/internal/siteidentity"
)

// ContentFetcher 抽象「按 URL 抓取页面内容」的能力，pipeline 依赖此接口
// 而非具体实现，便于测试桩替换。
type ContentFetcher interface {
	Fetch(context.Context, string) (fetcher.Content, error)
}

type parseTagStore interface {
	ListDistinct(context.Context) ([]string, error)
}

// ParseLinkStore is the link state surface used by the parse state machine.
type ParseLinkStore interface {
	GetParseInputByID(context.Context, uuid.UUID) (*repository.LinkParseInput, error)
	repository.ParseStateStore
}

// AncestorLinkLookup resolves already-saved URL ancestors for parent linking.
type AncestorLinkLookup interface {
	LookupByURLs(context.Context, []string) (map[string]*model.Link, error)
}

// ParsePipelineOptions 聚合了构造 ParsePipeline 所需的全部依赖与开关，
// 通过 NewParsePipeline 注入。
//
// 字段按生命周期分三组，顺序即分组：
//
//  1. 必需依赖——缺失即装配失败（NewParsePipeline 直接 panic）。走构造函数就
//     不可能为 nil；persist 里对两个 completer 仍留了返回 error 的兜底，那只
//     服务于直接构造 collectionFinalizer 的单元测试，不是运行期降级路径。
//  2. 可选能力——nil 表示该能力未装配，流程跳过它继续跑。
//  3. 配置开关——纯值，不是依赖，零值即默认行为。
//
// 这个分组不是文档洁癖。历史上 ReadingCompleter 被当作第 2 组对待（nil 就走另
// 一条落库路径），实际却是第 1 组，结果是测试与生产长期跑在不同的写入路径上。
// 判据很简单：nil 会让流程走向**另一种结果**的，属于第 1 组；只是**少做一件
// 事**的，才属于第 2 组。
type ParsePipelineOptions struct {
	// ---- 1. 必需依赖 ----

	Links            ParseLinkStore
	Tags             parseTagStore
	Fetcher          ContentFetcher
	Analyzer         analyzer.Analyzer
	SiteCompleter    repository.SiteParseCompleter
	ReadingCompleter repository.ReadingParseCompleter

	// ---- 2. 可选能力 ----

	// Tree 解析已入库的 URL 祖先用于建父子关系。nil 时 ensureParent 直接返回
	// 无父节点——这是显式容忍的降级，故属于可选。
	Tree   AncestorLinkLookup
	Logger *slog.Logger

	// ---- 3. 配置开关（纯值，零值即默认行为）----

	// URLDirect 为 true 时启用混合 Grok 路由：X/Twitter 等难抓社交来源优先
	// 让分析器直连 URL；普通网页先由本地抓取器提供完整正文，抓取失败时再以
	// URLDirect 兜底。字段零值为 false；生产配置由 PARSE_MODE=grok_direct 开启。
	URLDirect bool
}

// ParsePipeline 是「单条链接解析」的状态机驱动者：从 pending → processing
// → done/failed，所有副作用（更新链接与任务状态）都在
// 这里串起来。队列 worker 对每个 link.ID 调一次 Run。
type ParsePipeline struct {
	links            ParseLinkStore
	tags             parseTagStore
	tree             AncestorLinkLookup
	fetcher          ContentFetcher
	analyzer         analyzer.Analyzer
	logger           *slog.Logger
	urlDirect        bool
	siteCompleter    repository.SiteParseCompleter
	readingCompleter repository.ReadingParseCompleter
}

// NewParsePipeline 根据 opts 构造一个 *ParsePipeline；调用方在 app/deps_services.go
// 完成全部依赖装配后调用一次即可。
//
// 必需依赖缺失时直接 panic（完整清单见 ParsePipelineOptions 第 1 组，共 7 项）：
// 这是装配错误而非运行时错误，必须在进程启动时立刻暴露。历史上 ReadingCompleter
// 为 nil 会让 persist 静默降级到 legacy CompleteParse 分支，导致测试与生产跑在
// 不同的落库路径上。
// 把「未装配」变成启动即崩，是杜绝该类偏差的唯一可靠手段。
func NewParsePipeline(opts ParsePipelineOptions) *ParsePipeline {
	mustProvide(opts.Links != nil, "Links")
	mustProvide(opts.Tags != nil, "Tags")
	mustProvide(opts.Fetcher != nil, "Fetcher")
	mustProvide(opts.Analyzer != nil, "Analyzer")
	mustProvide(opts.SiteCompleter != nil, "SiteCompleter")
	mustProvide(opts.ReadingCompleter != nil, "ReadingCompleter")

	// 部署提示：混合模式仅对 X/Twitter 及本地抓取失败的 URL 使用模型直连，
	// 因而兜底分析端点仍需具备联网抓取能力。
	if opts.URLDirect && opts.Logger != nil {
		opts.Logger.Info("PARSE_MODE=grok_direct：普通网页优先本地抓取，X/Twitter 与抓取失败场景使用联网 Grok 直连兜底")
	}
	return &ParsePipeline{
		links:            opts.Links,
		tags:             opts.Tags,
		tree:             opts.Tree,
		fetcher:          opts.Fetcher,
		analyzer:         opts.Analyzer,
		logger:           opts.Logger,
		urlDirect:        opts.URLDirect,
		siteCompleter:    opts.SiteCompleter,
		readingCompleter: opts.ReadingCompleter,
	}
}

// mustProvide panics when a required ParsePipeline dependency is missing.
// 由 NewParsePipeline 在装配时调用，失败即启动失败。
func mustProvide(ok bool, field string) {
	if !ok {
		panic("service.NewParsePipeline: 必需依赖 " + field + " 未装配")
	}
}

const (
	lowConfidenceReasonSearchFallback = "search_fallback"
	lowConfidenceReasonThinContent    = "thin_content"
	lowConfidenceReasonFetchQuality   = "fetch_quality"
	lowConfidenceReasonTitleQuality   = "title_quality"
)

// Run 执行单条链接的完整解析流程：标记 processing → 抓取 → 取已有标签 →
// 调用 analyzer → 构建祖先树 → 落库 done/failed。任何阶段失败都会同步把
// links 行写成 failed，并返回 *PipelineRunError 让队列识别为
// 「已自行持久化」，避免重复写状态。
func (p *ParsePipeline) Run(ctx context.Context, attempt model.ParseAttempt) error {
	link, err := p.loadParseAttempt(ctx, attempt)
	if err != nil {
		return err
	}

	urlMeta := urlmeta.ClassifyURL(link.URL)
	requestedKind := requestedKindForLink(link)

	if err := p.markProcessing(ctx, attempt); err != nil {
		return err
	}

	// URL-direct is only valid for plain URL submissions. Ingest sources already
	// carry their canonical Input* payload; asking the analyzer to fetch their URL
	// would discard that saved content (especially for browser captures with a real
	// page URL).
	canUseURLDirect := p.urlDirect && !isParseInputIngestSource(link)
	urlDirectTried := false
	if canUseURLDirect && preferURLDirectFirst(link.URL) {
		urlDirectTried = true
		if handled, derr := p.runURLDirect(ctx, link, attempt, urlMeta, requestedKind); handled {
			return derr
		}
	}

	content, err := p.acquireContent(ctx, link)
	if err != nil {
		// Normal documents are more reliable when WebTag supplies the fetched
		// body to Grok. If that fetch fails, URL-direct remains the fail-soft
		// escape hatch instead of turning a recoverable page into a failed job.
		if canUseURLDirect && !urlDirectTried {
			if handled, derr := p.runURLDirect(ctx, link, attempt, urlMeta, requestedKind); handled {
				return derr
			}
		}
		return p.fail(ctx, attempt, err)
	}
	fetcherType := strings.TrimSpace(content.FetcherType)

	existingTags, err := p.loadExistingTags(ctx)
	if err != nil {
		p.logWarn(ctx, "existing tags unavailable; continuing without tag suggestions",
			"link_id", attempt.LinkID.String(),
			"parse_generation", attempt.Generation,
			"err", observability.SafeError(err),
		)
		existingTags = nil
	}

	result, err := p.analyzer.Analyze(ctx, analyzer.AnalyzeRequest{
		Content:              content,
		ExistingTags:         existingTags,
		ContentType:          string(urlMeta.ContentType),
		RequestedLibraryKind: requestedKind,
		UserDescription:      link.Description,
	})
	if err != nil {
		return p.fail(ctx, attempt, err)
	}
	return p.finalizeAnalysis(ctx, link, attempt, urlMeta, content, result, fetcherType)
}

func preferURLDirectFirst(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	return host == "x.com" || strings.HasSuffix(host, ".x.com") ||
		host == "twitter.com" || strings.HasSuffix(host, ".twitter.com")
}

// finalizeAnalysis hands the analyzed link to the collection finalizer.
//
// 直接收 *model.Link 而非把 id / url / tags 拆成三个形参：终态决策还需要它的
// library_kind 与 library_kind_locked（用户锁定必须压过自动分类），逐个加形参
// 会把签名推到 14 个。link 是同一个聚合，整体传入既短又不会再漏字段。
func (p *ParsePipeline) finalizeAnalysis(
	ctx context.Context,
	link *repository.LinkParseInput,
	attempt model.ParseAttempt,
	urlMeta urlmeta.URLMetadata,
	content fetcher.Content,
	result analyzer.AnalysisResult,
	fetcherType string,
) error {
	return p.collectionFinalizer().Finalize(ctx, collectionFinalizationRequest{
		LinkID:        link.ID,
		Attempt:       attempt,
		URL:           link.URL,
		CurrentKind:   link.LibraryKind,
		CurrentLocked: link.LibraryKindLocked,
		URLMeta:       urlMeta,
		Content:       content,
		Result:        result,
		FetcherType:   fetcherType,
	})
}

// runURLDirect 执行 grok 直连抓取路径：分析器自抓 URL 并返回 summary+tags+title。
// 返回 handled=true 表示已产出终态（落库 done，或落库出错需上抛）；handled=false
// 表示需回退本地抓取器——模型报 accessible=false（登录墙/反爬/404）或分析调用本身
// 出错（网络/上游）。回退是设计内的正常分支，不计失败指标。
//
// 直连路径不经本地抓取，因此合成一个最小 Content（URL + 模型抓到的标题 +
// 用摘要充当正文信号，避免被误判
// thin 低置信），fetcher_type 记为 "grok"。
func (p *ParsePipeline) runURLDirect(
	ctx context.Context,
	link *repository.LinkParseInput,
	attempt model.ParseAttempt,
	urlMeta urlmeta.URLMetadata,
	persistRequestedKind model.RequestedLibraryKind,
) (handled bool, err error) {
	existingTags, tagErr := p.loadExistingTags(ctx)
	if tagErr != nil {
		p.logWarn(ctx, "existing tags unavailable for url-direct analysis; continuing without tag suggestions",
			"link_id", link.ID.String(),
			"parse_generation", attempt.Generation,
			"err", observability.SafeError(tagErr),
		)
		existingTags = nil
	}
	result, aerr := p.analyzer.Analyze(ctx, analyzer.AnalyzeRequest{
		URLDirect:            true,
		Content:              fetcher.Content{URL: link.URL},
		ExistingTags:         existingTags,
		ContentType:          string(urlMeta.ContentType),
		RequestedLibraryKind: persistRequestedKind,
		UserDescription:      link.Description,
	})
	if aerr != nil {
		if fetcher.IsUnsafeTargetError(aerr) {
			return true, p.fail(ctx, attempt, aerr)
		}
		if p.logger != nil {
			p.logger.Warn("url-direct analyze failed; falling back to local fetcher",
				"link_id", link.ID.String(), "err", observability.SafeError(aerr))
		}
		return false, nil
	}
	if !result.Accessible {
		if p.logger != nil {
			p.logger.Info("url-direct: model could not access URL; falling back to local fetcher",
				"link_id", link.ID.String())
		}
		return false, nil
	}
	// 可达但模型没回标题：直连结果不够好（标题缺失会被 evaluateLowConfidence 误判
	// title_quality 低置信），回退本地抓取器拿真实标题，而非落一条无标题的 link。
	if strings.TrimSpace(result.Title) == "" {
		if p.logger != nil {
			p.logger.Info("url-direct: accessible but empty title; falling back to local fetcher",
				"link_id", link.ID.String())
		}
		return false, nil
	}

	content := fetcher.Content{
		URL:         link.URL,
		Title:       result.Title,
		Body:        result.Summary,
		FetcherType: "grok",
	}
	return true, p.finalizeAnalysis(ctx, link, attempt, urlMeta, content, result, "grok")
}

func requestedKindForLink(link *repository.LinkParseInput) model.RequestedLibraryKind {
	if link == nil || !link.LibraryKindLocked || link.LibraryKind == nil {
		return model.RequestedLibraryKindAuto
	}
	return model.RequestedLibraryKind(*link.LibraryKind)
}

func siteAggregateParams(linkID uuid.UUID, rawURL string, result analyzer.AnalysisResult) (repository.AggregateSiteParams, error) {
	identity, err := siteidentity.FromURL(rawURL)
	if err != nil {
		return repository.AggregateSiteParams{}, err
	}
	return repository.AggregateSiteParams{LinkID: linkID, IdentityKey: identity.Key, NormalizedURL: identity.NormalizedURL, Name: result.SiteName, Intro: result.SiteIntro, EntryName: result.EntryName, Purpose: result.EntryPurpose}, nil
}
