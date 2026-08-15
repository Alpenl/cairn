// pipeline_escalation_test.go exercises the Phase 10 low-confidence escalation
// ladder end to end against a real Postgres. The pure-Go unit tests in
// internal/service cover the branch logic (thin判定 / explicit-light / metrics)
// with in-memory fakes; this file proves the full pipeline.Run path: a
// PreferLight link whose light fetch is thin gets re-fetched through the deep
// Fetch, and the DEEP content (not the thin light one) is what persists to the
// links row. It also locks the "explicit parse_depth=light is never escalated"
// contract against the real schema.
package dbintegration

import (
	"context"
	"strings"
	"testing"

	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/service/analyzer"
)

// escalationDualFetcher implements both ContentFetcher and LightContentFetcher.
// FetchLight returns a thin (+thin) body; Fetch returns a full deep body. It
// records each leg so the test can assert light fired once and deep fired at
// most once (the ladder escalates a link at most once).
type escalationDualFetcher struct {
	lightCalls int
	deepCalls  int
}

func (f *escalationDualFetcher) FetchLight(_ context.Context, url string) (fetcher.Content, error) {
	f.lightCalls++
	return fetcher.Content{URL: url, Title: "Thin title", Body: "tiny", FetcherType: "light+thin"}, nil
}

func (f *escalationDualFetcher) Fetch(_ context.Context, url string) (fetcher.Content, error) {
	f.deepCalls++
	return fetcher.Content{URL: url, Title: "Deep title", Body: strings.Repeat("d", 500), FetcherType: "basic"}, nil
}

// escalationStubAnalyzer echoes the analyzed body into the summary so the test
// can prove which fetch leg's content reached analysis.
type escalationStubAnalyzer struct{}

func (escalationStubAnalyzer) Analyze(_ context.Context, req analyzer.AnalyzeRequest) (analyzer.AnalysisResult, error) {
	return analyzer.AnalysisResult{Summary: "body=" + req.Content.Body, Tags: []string{"t"}}, nil
}

// TestPipelineEscalatesThinLightToDeepEndToEnd: PreferLight + thin light →
// one deep re-fetch; the persisted link carries the deep fetcher_type and the
// deep body reaches the analyzer.
func TestPipelineEscalatesThinLightToDeepEndToEnd(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()

	links := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	tags := repository.NewPGXTagRepository(pool)
	tree := repository.NewPGXTreeRepository(pool)

	link, job, err := links.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/escalate-me",
		SourceKind: "url",
		SourceKey:  "escalate-me",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}

	f := &escalationDualFetcher{}
	pipeline := service.NewParsePipeline(service.ParsePipelineOptions{
		Links: links,
		Jobs:  jobs,
		Tags:  tags,
		Tree:  tree,
		// 生产装的是 link repo 本身（deps_services.go），这里用的正是同一个
		// 真 repo——补齐后与生产同构，而不是塞个 fake 进来。
		SiteCompleter:    links,
		ReadingCompleter: links,
		Fetcher:          f,
		Analyzer:         escalationStubAnalyzer{},
		PreferLight:      true,
	})

	if err := pipeline.Run(ctx, link.ID, job.ID); err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}

	if f.lightCalls != 1 {
		t.Errorf("light fetch calls = %d, want 1", f.lightCalls)
	}
	if f.deepCalls != 1 {
		t.Errorf("deep re-fetch calls = %d, want 1 (escalated exactly once)", f.deepCalls)
	}

	got, err := links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != model.LinkStatusDone {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if got.FetcherType == nil || *got.FetcherType != "basic" {
		t.Fatalf("persisted fetcher_type = %v, want basic (deep content replaced light)", got.FetcherType)
	}
	// The deep body (500 'd's) must be the one the analyzer summarized.
	if got.Summary == nil || !strings.Contains(*got.Summary, strings.Repeat("d", 500)) {
		t.Fatalf("analyzer consumed the wrong body; summary=%v", got.Summary)
	}
	// Deep body cleared the thin floor → the link is no longer low confidence.
	if got.IsLowConfidence {
		t.Errorf("link should not be low confidence after recovering a full deep body")
	}
}

// TestPipelineDoesNotEscalateExplicitLight: a link that pinned
// parse_depth=light keeps its thin light content — no deep re-fetch — even
// though the result is thin.
func TestPipelineDoesNotEscalateExplicitLight(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()

	links := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	tags := repository.NewPGXTagRepository(pool)
	tree := repository.NewPGXTreeRepository(pool)

	link, job, err := links.SubmitNew(ctx, repository.CreateLinkParams{
		URL:            "https://example.com/explicit-light",
		SourceKind:     "url",
		SourceKey:      "explicit-light",
		Status:         model.LinkStatusPending,
		SourceMetadata: map[string]any{"parse_depth": "light"},
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}

	f := &escalationDualFetcher{}
	pipeline := service.NewParsePipeline(service.ParsePipelineOptions{
		Links: links,
		Jobs:  jobs,
		Tags:  tags,
		Tree:  tree,
		// 生产装的是 link repo 本身（deps_services.go），这里用的正是同一个
		// 真 repo——补齐后与生产同构，而不是塞个 fake 进来。
		SiteCompleter:    links,
		ReadingCompleter: links,
		Fetcher:          f,
		Analyzer:         escalationStubAnalyzer{},
		PreferLight:      true,
	})

	if err := pipeline.Run(ctx, link.ID, job.ID); err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}

	if f.lightCalls != 1 {
		t.Errorf("light fetch calls = %d, want 1", f.lightCalls)
	}
	if f.deepCalls != 0 {
		t.Errorf("deep re-fetch calls = %d, want 0 (explicit light must not escalate)", f.deepCalls)
	}

	got, err := links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.FetcherType == nil || *got.FetcherType != "light+thin" {
		t.Fatalf("persisted fetcher_type = %v, want light+thin (no escalation)", got.FetcherType)
	}
}
