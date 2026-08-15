package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service/analyzer"
)

// minimalPipelineLinkStore 仅用于装配测试——只验证 NewParsePipeline 接受哪些
// 依赖，pipeline 从不被 Run。因此它的终态 completer 无条件成功，没有复刻生产
// 校验。
//
// 不要拿它跑真实解析路径：那会重新打开「fake 比生产宽松」的口子（参见
// repotest.ObservableLinkStore 上的同名约束）。执行路径一律用
// repotest.ObservableLinkStore。
type minimalPipelineLinkStore struct{}

func (minimalPipelineLinkStore) GetParseInputByID(context.Context, uuid.UUID) (*repository.LinkParseInput, error) {
	return nil, nil
}

func (minimalPipelineLinkStore) MarkParseProcessing(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

func (minimalPipelineLinkStore) MarkParseFailed(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

// 终态 completer 与 Links 由同一类型实现，与 deps_services.go 的生产装配一致。
func (minimalPipelineLinkStore) CompleteReadingParse(context.Context, repository.CompleteReadingParseParams, uuid.UUID) (repository.CompleteReadingParseResult, error) {
	return repository.CompleteReadingParseResult{MetadataRevision: 1, MetadataApplied: true}, nil
}

func (minimalPipelineLinkStore) CompleteSiteParse(context.Context, repository.CompleteSiteParseParams, uuid.UUID) (repository.SiteAggregateResult, error) {
	return repository.SiteAggregateResult{}, nil
}

type minimalPipelineJobStore struct{}

func (minimalPipelineJobStore) GetByID(context.Context, uuid.UUID) (*model.ParseJob, error) {
	return nil, nil
}

type minimalAncestorLinkLookup struct{}

func (minimalAncestorLinkLookup) LookupByURLs(context.Context, []string) (map[string]*model.Link, error) {
	return nil, nil
}

type minimalTagStore struct{}

func (minimalTagStore) ListDistinct(context.Context) ([]string, error) {
	return nil, nil
}

func (minimalTagStore) ListCounts(context.Context) ([]repository.TagCount, error) {
	return nil, nil
}

type minimalContentFetcher struct{}

func (minimalContentFetcher) Fetch(context.Context, string) (fetcher.Content, error) {
	return fetcher.Content{}, nil
}

type minimalAnalyzer struct{}

func (minimalAnalyzer) Analyze(context.Context, analyzer.AnalyzeRequest) (analyzer.AnalysisResult, error) {
	return analyzer.AnalysisResult{}, nil
}

// minimalPipelineOptions 是 NewParsePipeline 的真实最小依赖集：少任何一项都会在
// 装配期 panic。Tree 不在其列——ensureParent 对 nil tree 有显式容忍，它是真可选。
func minimalPipelineOptions() ParsePipelineOptions {
	store := minimalPipelineLinkStore{}
	return ParsePipelineOptions{
		Links:            store,
		Jobs:             minimalPipelineJobStore{},
		Tags:             minimalTagStore{},
		Fetcher:          minimalContentFetcher{},
		Analyzer:         minimalAnalyzer{},
		ReadingCompleter: store,
		SiteCompleter:    store,
	}
}

func TestNewParsePipelineAcceptsExactRepositoryCapabilities(t *testing.T) {
	t.Parallel()

	opts := minimalPipelineOptions()
	opts.Tree = minimalAncestorLinkLookup{}
	pipeline := NewParsePipeline(opts)
	if pipeline == nil {
		t.Fatal("NewParsePipeline() returned nil")
	}

	// 断言依赖确实被装进了 pipeline，而不只是「构造函数返回了非 nil」——后者对
	// 任何实现都成立（构造函数无条件返回新对象），测试名承诺的 "accepts exact
	// repository capabilities" 因此不会被验证到。可复现的变异：删掉
	// NewParsePipeline 里某一行 `tags: opts.Tags` 之类的赋值，旧版断言仍全绿，
	// 下面的循环会报出该依赖未装配。
	for _, dep := range []struct {
		name string
		got  any
	}{
		{"links", pipeline.links},
		{"jobs", pipeline.jobs},
		{"tags", pipeline.tags},
		{"fetcher", pipeline.fetcher},
		{"analyzer", pipeline.analyzer},
		{"siteCompleter", pipeline.siteCompleter},
		{"readingCompleter", pipeline.readingCompleter},
		{"tree", pipeline.tree},
	} {
		if dep.got == nil {
			t.Errorf("依赖 %s 已传入 options 但未装配到 pipeline", dep.name)
		}
	}

	// Tree 是真可选：ensureParent 显式容忍 nil，缺它不应导致装配失败。
	withoutTree := NewParsePipeline(minimalPipelineOptions())
	if withoutTree == nil {
		t.Fatal("NewParsePipeline() 在无 Tree 时返回 nil；Tree 应为可选依赖")
	}
	if withoutTree.tree != nil {
		t.Fatal("未传入 Tree，pipeline.tree 却非 nil")
	}
}

// TestNewParsePipelineRejectsMissingTerminalCompleters 锁死阶段1的核心约束：
// 终态 completer 缺失必须在装配期崩溃，而不是在运行期静默降级到另一条落库
// 路径。历史缺陷正源于此——ReadingCompleter 为 nil 时 persist 会走 legacy
// CompleteParse，使测试与生产写入不同的表。
func TestNewParsePipelineRejectsMissingTerminalCompleters(t *testing.T) {
	t.Parallel()

	base := minimalPipelineOptions()

	for _, tc := range []struct {
		name    string
		mutate  func(*ParsePipelineOptions)
		wantMsg string
	}{
		{"missing links", func(o *ParsePipelineOptions) { o.Links = nil }, "Links"},
		{"missing jobs", func(o *ParsePipelineOptions) { o.Jobs = nil }, "Jobs"},
		{"missing tags", func(o *ParsePipelineOptions) { o.Tags = nil }, "Tags"},
		{"missing fetcher", func(o *ParsePipelineOptions) { o.Fetcher = nil }, "Fetcher"},
		{"missing analyzer", func(o *ParsePipelineOptions) { o.Analyzer = nil }, "Analyzer"},
		{"missing reading completer", func(o *ParsePipelineOptions) { o.ReadingCompleter = nil }, "ReadingCompleter"},
		{"missing site completer", func(o *ParsePipelineOptions) { o.SiteCompleter = nil }, "SiteCompleter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := base
			tc.mutate(&opts)
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatalf("NewParsePipeline() 未 panic，%s 缺失应导致装配失败", tc.wantMsg)
				}
				if msg, ok := recovered.(string); !ok || !strings.Contains(msg, tc.wantMsg) {
					t.Fatalf("panic = %v, 应包含 %q", recovered, tc.wantMsg)
				}
			}()
			NewParsePipeline(opts)
		})
	}
}
