package eval

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"webtag/internal/fetcher"
	"webtag/internal/service/analyzer"
)

// stubFetcher returns a fixed Content per URL so the runner test
// does not touch the network. CanHandle is forced to true so the
// runner does not need to know about routing rules.
type stubFetcher struct {
	body map[string]fetcher.Content
	err  error
}

func (s stubFetcher) CanHandle(string) bool { return true }
func (s stubFetcher) Fetch(_ context.Context, url string) (fetcher.Content, error) {
	if s.err != nil {
		return fetcher.Content{}, s.err
	}
	if c, ok := s.body[url]; ok {
		return c, nil
	}
	return fetcher.Content{URL: url, Body: "default body"}, nil
}

func newAnalyzerServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAnalyzer(srv *httptest.Server) *analyzer.OpenAIAnalyzer {
	return analyzer.NewOpenAIAnalyzer(analyzer.OpenAIAnalyzerOptions{
		BaseURL:              srv.URL,
		APIKey:               "test",
		Model:                "gpt-4.1-mini",
		HTTPClient:           srv.Client(),
		EmptyResponseRetries: 1,
		RequestTimeout:       2 * time.Second,
	})
}

func TestRunWalksMatrixAndProducesCellsForEveryCombination(t *testing.T) {
	t.Parallel()

	srv := newAnalyzerServer(t, `{"choices":[{"message":{"content":"{\"summary\":\"x\",\"tags\":[\"RAG\",\"腾讯\",\"ima\"]}"}}]}`)

	cases := []Case{
		{ID: "c1", URL: "https://a/1", Expected: CaseExpected{MustInclude: []string{"RAG"}}},
		{ID: "c2", URL: "https://a/2", Expected: CaseExpected{MustInclude: []string{"RAG"}}},
	}
	cfg := RunConfig{
		Cases:    cases,
		Fetchers: []FetcherName{"f1", "f2"},
		Prompts:  []PromptVariant{{Name: "p1", Body: "system prompt 1"}},
		Models:   []string{"gpt-4.1-mini"},
		BuildFetcher: func(_ FetcherName) (fetcher.Fetcher, error) {
			return stubFetcher{}, nil
		},
		BuildAnalyzer:  func(_ string) *analyzer.OpenAIAnalyzer { return newAnalyzer(srv) },
		PerCallTimeout: 5 * time.Second,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	// 2 cases * 2 fetchers * 1 prompt * 1 model = 4 cells
	if len(res.Cells) != 4 {
		t.Fatalf("cells = %d, want 4", len(res.Cells))
	}
	for _, c := range res.Cells {
		if !c.AnalyzeOK {
			t.Errorf("cell %+v should be analyze ok", c)
		}
		if c.Rule.Normalised != 1.0 {
			t.Errorf("cell %+v should score 1.0 (got RAG match)", c)
		}
	}
}

func TestRunGrokFetcherUsesURLDirectWithoutLocalFetch(t *testing.T) {
	t.Parallel()

	var systemPrompt string
	var userPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode analyzer request: %v", err)
		}
		systemPrompt, _ = request.Messages[0]["content"].(string)
		userPrompt, _ = request.Messages[1]["content"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"accessible\":true,\"title\":\"Grok title\",\"summary\":\"自然摘要。\",\"tags\":[\"Grok\"]}"}}]}`))
	}))
	t.Cleanup(srv.Close)

	buildFetcherCalled := false
	res, err := Run(context.Background(), RunConfig{
		Cases:    []Case{{ID: "direct", URL: "https://example.com/post", ContentType: "article"}},
		Fetchers: []FetcherName{FetcherGrok},
		Prompts:  []PromptVariant{{Name: "production"}},
		Models:   []string{"grok-4.3-fast"},
		BuildFetcher: func(FetcherName) (fetcher.Fetcher, error) {
			buildFetcherCalled = true
			return stubFetcher{}, nil
		},
		BuildAnalyzer: func(string) *analyzer.OpenAIAnalyzer { return newAnalyzer(srv) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if buildFetcherCalled {
		t.Fatal("grok eval should not construct or call a local fetcher")
	}
	if len(res.Cells) != 1 || !res.Cells[0].AnalyzeOK || res.Cells[0].Title != "Grok title" {
		t.Fatalf("cells = %+v, want successful Grok-direct result", res.Cells)
	}
	if !strings.Contains(systemPrompt, `"accessible": true`) || !strings.Contains(userPrompt, "https://example.com/post") {
		t.Fatalf("request was not URL-direct: system=%q user=%q", systemPrompt, userPrompt)
	}
	if strings.Contains(userPrompt, "正文:") {
		t.Fatalf("URL-direct user prompt should not contain local body: %q", userPrompt)
	}
}

func TestRunGrokFetcherFallsBackToProductionRouterWhenURLIsInaccessible(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"accessible\":false}"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"本地抓取回退后得到自然摘要。\",\"tags\":[\"Go\"]}"}}]}`))
	}))
	t.Cleanup(srv.Close)

	var built FetcherName
	res, err := Run(context.Background(), RunConfig{
		Cases:    []Case{{ID: "fallback", URL: "https://example.com/post", ContentType: "article"}},
		Fetchers: []FetcherName{FetcherGrok},
		Prompts:  []PromptVariant{{Name: "production"}},
		Models:   []string{"grok-4.3-fast"},
		BuildFetcher: func(name FetcherName) (fetcher.Fetcher, error) {
			built = name
			return stubFetcher{body: map[string]fetcher.Content{
				"https://example.com/post": {
					URL: "https://example.com/post", Title: "Local title", Body: "Local source body", FetcherType: "basic",
				},
			}}, nil
		},
		BuildAnalyzer: func(string) *analyzer.OpenAIAnalyzer { return newAnalyzer(srv) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if built != FetcherRouter {
		t.Fatalf("fallback fetcher = %q, want production router", built)
	}
	if calls != 2 {
		t.Fatalf("analyzer calls = %d, want URL-direct plus local-body fallback", calls)
	}
	cell := res.Cells[0]
	if !cell.FetchOK || !cell.AnalyzeOK || cell.Title != "Local title" || cell.Summary == "" {
		t.Fatalf("fallback cell = %+v, want successful local result", cell)
	}
}

func TestRunGrokJudgeReceivesLocallyFetchedSourceBody(t *testing.T) {
	t.Parallel()

	srv := newAnalyzerServer(t, `{"choices":[{"message":{"content":"{\"accessible\":true,\"title\":\"Grok title\",\"summary\":\"自然摘要。\",\"tags\":[\"Go\"]}"}}]}`)
	judge := &stubJudge{verdict: JudgeVerdict{Score: 5, SummaryScore: 5, TagScore: 5}}
	res, err := Run(context.Background(), RunConfig{
		Cases:    []Case{{ID: "judge-source", URL: "https://example.com/post", ContentType: "article"}},
		Fetchers: []FetcherName{FetcherGrok},
		Prompts:  []PromptVariant{{Name: "production"}},
		Models:   []string{"grok-4.3-fast"},
		BuildFetcher: func(name FetcherName) (fetcher.Fetcher, error) {
			if name != FetcherRouter {
				t.Fatalf("judge source fetcher = %q, want router", name)
			}
			return stubFetcher{body: map[string]fetcher.Content{
				"https://example.com/post": {URL: "https://example.com/post", Title: "Local", Body: "full local source body"},
			}}, nil
		},
		BuildAnalyzer: func(string) *analyzer.OpenAIAnalyzer { return newAnalyzer(srv) },
		Judge:         judge,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(res.Cells) != 1 || !res.Cells[0].AnalyzeOK {
		t.Fatalf("cells = %+v, want success", res.Cells)
	}
	if len(judge.inputs) != 1 || judge.inputs[0].BodyPreview != "full local source body" {
		t.Fatalf("judge inputs = %+v, want fetched source body", judge.inputs)
	}
}

func TestRunPopulatesDeterministicSummaryQuality(t *testing.T) {
	t.Parallel()

	summary := "文章比较 REST 与 GraphQL API 的长期维护成本，指出熟悉、可预测的接口比炫技式抽象更容易被团队采用。" +
		"作者建议默认保持向后兼容，只在无法避免破坏性变化时引入版本，并通过幂等键让调用方安全重试。" +
		"最终选择应服务产品价值和真实用户，而不是追求形式上的纯粹。"
	analysisJSON, _ := json.Marshal(map[string]any{"summary": summary, "tags": []string{"API"}})
	responseJSON, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": string(analysisJSON)}}},
	})
	srv := newAnalyzerServer(t, string(responseJSON))

	res, err := Run(context.Background(), RunConfig{
		Cases: []Case{{
			ID: "quality", URL: "https://example.com/article", ContentType: "article",
			Expected: CaseExpected{Summary: TextExpected{MustInclude: []string{"向后兼容", "幂等"}}},
		}},
		Fetchers: []FetcherName{FetcherBasic},
		Prompts:  []PromptVariant{{Name: "production"}},
		Models:   []string{"grok-4.3-fast"},
		BuildFetcher: func(FetcherName) (fetcher.Fetcher, error) {
			return stubFetcher{}, nil
		},
		BuildAnalyzer: func(string) *analyzer.OpenAIAnalyzer { return newAnalyzer(srv) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	cell := res.Cells[0]
	if cell.SummaryProfile != "article" || cell.SummaryRule.Score != 1 {
		t.Fatalf("summary quality = profile:%q rule:%+v, want article score 1", cell.SummaryProfile, cell.SummaryRule)
	}
	if cell.SummaryRule.LengthRunes != len([]rune(summary)) {
		t.Fatalf("summary length = %d, want %d", cell.SummaryRule.LengthRunes, len([]rune(summary)))
	}
	if !cell.ContentContract.Configured || !cell.ContentContract.Passed {
		t.Fatalf("content contract = %+v, want configured pass", cell.ContentContract)
	}
}

func TestRunRecordsFetchErrorAndStillProducesCell(t *testing.T) {
	t.Parallel()

	srv := newAnalyzerServer(t, `{"choices":[{"message":{"content":"{\"summary\":\"x\",\"tags\":[\"RAG\",\"腾讯\",\"ima\"]}"}}]}`)
	cfg := RunConfig{
		Cases:    []Case{{ID: "c", URL: "https://x", Expected: CaseExpected{MustInclude: []string{"RAG"}}}},
		Fetchers: []FetcherName{"broken"},
		Prompts:  []PromptVariant{{Name: "p", Body: "x"}},
		Models:   []string{"m"},
		BuildFetcher: func(_ FetcherName) (fetcher.Fetcher, error) {
			return stubFetcher{err: errors.New("fetch down")}, nil
		},
		BuildAnalyzer: func(_ string) *analyzer.OpenAIAnalyzer { return newAnalyzer(srv) },
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if len(res.Cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(res.Cells))
	}
	c := res.Cells[0]
	if c.AnalyzeOK {
		t.Errorf("AnalyzeOK should be false when fetch fails")
	}
	if !strings.Contains(c.Err, "fetch") {
		t.Errorf("Err should mention fetch; got %q", c.Err)
	}
	if c.Rule.Normalised != 0 {
		t.Errorf("score should be 0 when no tags; got %f", c.Rule.Normalised)
	}
}

func TestRunRejectsMissingAxes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  RunConfig
	}{
		{"no cases", RunConfig{Fetchers: []FetcherName{"x"}, Prompts: []PromptVariant{{Name: "p"}}, Models: []string{"m"},
			BuildFetcher:  func(FetcherName) (fetcher.Fetcher, error) { return stubFetcher{}, nil },
			BuildAnalyzer: func(string) *analyzer.OpenAIAnalyzer { return nil }}},
		{"no fetchers", RunConfig{Cases: []Case{{ID: "a", URL: "x"}}, Prompts: []PromptVariant{{Name: "p"}}, Models: []string{"m"},
			BuildFetcher:  func(FetcherName) (fetcher.Fetcher, error) { return stubFetcher{}, nil },
			BuildAnalyzer: func(string) *analyzer.OpenAIAnalyzer { return nil }}},
		{"no builders", RunConfig{Cases: []Case{{ID: "a", URL: "x"}}, Fetchers: []FetcherName{"x"}, Prompts: []PromptVariant{{Name: "p"}}, Models: []string{"m"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Run(context.Background(), tc.cfg); err == nil {
				t.Fatal("expected error from missing axis")
			}
		})
	}
}

func TestLoadPromptVariantsReadsFiles(t *testing.T) {
	t.Parallel()
	p1 := writeTemp(t, "p1.txt", "system 1")
	p2 := writeTemp(t, "p2.txt", "system 2")
	got, err := LoadPromptVariants([]string{p1, "named=" + p2})
	if err != nil {
		t.Fatalf("LoadPromptVariants: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name == "" || got[1].Name != "named" {
		t.Fatalf("names = %+v", got)
	}
	if got[0].Body != "system 1" || got[1].Body != "system 2" {
		t.Fatalf("bodies = %+v", got)
	}
}

func TestLoadPromptVariantsUsesBuiltInProductionPrompt(t *testing.T) {
	t.Parallel()

	got, err := LoadPromptVariants([]string{"production"})
	if err != nil {
		t.Fatalf("LoadPromptVariants: %v", err)
	}
	if len(got) != 1 || got[0].Name != "production" || got[0].Body != "" {
		t.Fatalf("variants = %+v, want built-in production prompt with no override", got)
	}
}
