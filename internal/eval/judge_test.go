package eval

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"webtag/internal/fetcher"
	"webtag/internal/service/analyzer"
)

type judgeRedirectRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn judgeRedirectRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newJudgeServer(t *testing.T, response string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status > 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newJudge(srv *httptest.Server) *HTTPJudge {
	return NewHTTPJudge(HTTPJudgeOptions{
		BaseURL:    srv.URL,
		APIKey:     "test",
		Model:      "gpt-4.1-mini",
		HTTPClient: srv.Client(),
	})
}

func TestHTTPJudgeReturnsParsedVerdict(t *testing.T) {
	t.Parallel()
	srv := newJudgeServer(t, `{"choices":[{"message":{"content":"{\"score\":4,\"summary_score\":5,\"tag_score\":4,\"reason\":\"摘要自然准确，标签有 1 个稍宽泛\"}"}}]}`, 200)
	j := newJudge(srv)

	v, err := j.JudgeCell(context.Background(), JudgeInput{
		URL: "https://x", Title: "y", BodyPreview: "z", Summary: "w", Tags: []string{"RAG"},
	})
	if err != nil {
		t.Fatalf("JudgeCell err = %v", err)
	}
	if v.Score != 4 {
		t.Fatalf("score = %v, want 4", v.Score)
	}
	if v.SummaryScore != 5 || v.TagScore != 4 {
		t.Fatalf("subscores = summary:%v tag:%v, want 5 and 4", v.SummaryScore, v.TagScore)
	}
	if !strings.Contains(v.Reason, "稍宽泛") {
		t.Fatalf("reason = %q", v.Reason)
	}
}

func TestHTTPJudgePinsNonStreamingResponse(t *testing.T) {
	t.Parallel()

	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"score\":5,\"summary_score\":5,\"tag_score\":5,\"reason\":\"自然准确\"}"}}]}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := newJudge(srv).JudgeCell(context.Background(), JudgeInput{Summary: "摘要。", Tags: []string{"Go"}}); err != nil {
		t.Fatalf("JudgeCell() error = %v", err)
	}
	stream, present := payload["stream"]
	if !present || stream != false {
		t.Fatalf("stream = %#v (present=%v), want explicit false for grok2api compatibility", stream, present)
	}
}

func TestHTTPJudgeClampsOutOfRangeScore(t *testing.T) {
	t.Parallel()
	srv := newJudgeServer(t, `{"choices":[{"message":{"content":"{\"score\":7,\"reason\":\"模型瞎打\"}"}}]}`, 200)
	j := newJudge(srv)
	v, err := j.JudgeCell(context.Background(), JudgeInput{Tags: []string{"x"}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if v.Score != 5 {
		t.Fatalf("clamp: score = %v, want 5", v.Score)
	}
}

func TestHTTPJudgeStripsMarkdownFence(t *testing.T) {
	t.Parallel()
	srv := newJudgeServer(t, "{\"choices\":[{\"message\":{\"content\":\"```json\\n{\\\"score\\\":3,\\\"reason\\\":\\\"一般\\\"}\\n```\"}}]}", 200)
	j := newJudge(srv)
	v, err := j.JudgeCell(context.Background(), JudgeInput{Tags: []string{"x"}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if v.Score != 3 {
		t.Fatalf("score = %v, want 3", v.Score)
	}
}

func TestHTTPJudgeStripsMarkdownBoldWrapper(t *testing.T) {
	t.Parallel()
	srv := newJudgeServer(t, `{"choices":[{"message":{"content":"**{\"score\":5,\"summary_score\":5,\"tag_score\":4,\"reason\":\"自然准确\"}**"}}]}`, 200)
	v, err := newJudge(srv).JudgeCell(context.Background(), JudgeInput{Summary: "摘要。", Tags: []string{"Go"}})
	if err != nil {
		t.Fatalf("JudgeCell() error = %v", err)
	}
	if v.Score != 5 || v.SummaryScore != 5 || v.TagScore != 4 {
		t.Fatalf("verdict = %+v, want 5/5/4", v)
	}
}

func TestHTTPJudgeSurfacesHTTPErrors(t *testing.T) {
	t.Parallel()
	srv := newJudgeServer(t, `{"error":"bad request"}`, 400)
	j := newJudge(srv)
	if _, err := j.JudgeCell(context.Background(), JudgeInput{Tags: []string{"x"}}); err == nil {
		t.Fatal("400 should surface as error")
	}
}

func TestHTTPJudgeSanitizesHTTPSDowngradeRedirect(t *testing.T) {
	t.Parallel()

	transportCalls := 0
	client := fetcher.NewHTTPClientWithOptions(fetcher.HTTPClientOptions{
		AllowUnsafeTargets: true,
		Client: &http.Client{
			Transport: judgeRedirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				transportCalls++
				if transportCalls > 1 {
					t.Fatal("hostile redirect target reached transport")
				}
				header := make(http.Header)
				header.Set("Location", "http://hostile-judge.example/chat/completions?token=judge-redirect-secret")
				return &http.Response{
					StatusCode: http.StatusTemporaryRedirect,
					Header:     header,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}),
		},
	}).Raw()
	judge := NewHTTPJudge(HTTPJudgeOptions{
		BaseURL:    "https://judge-provider.example",
		APIKey:     "fictional-judge-key",
		Model:      "judge-test",
		HTTPClient: client,
	})

	_, err := judge.JudgeCell(context.Background(), JudgeInput{
		Summary: "fictional private summary",
		Tags:    []string{"private-tag"},
	})
	if err == nil || !errors.Is(err, fetcher.ErrUnsafeRedirect) || !fetcher.IsUnsafeTargetError(err) {
		t.Fatalf("JudgeCell() error = %v, want typed unsafe redirect", err)
	}
	if transportCalls != 1 {
		t.Fatalf("transport calls = %d, want one TLS request and zero downstream requests", transportCalls)
	}
	for _, forbidden := range []string{
		"hostile-judge.example",
		"judge-redirect-secret",
		"fictional-judge-key",
		"fictional private summary",
	} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("JudgeCell() error leaked %q: %v", forbidden, err)
		}
	}
}

func TestHTTPJudgeRefusesEmptyConfig(t *testing.T) {
	t.Parallel()
	j := NewHTTPJudge(HTTPJudgeOptions{})
	if _, err := j.JudgeCell(context.Background(), JudgeInput{}); err == nil {
		t.Fatal("missing BaseURL/APIKey/Model should error")
	}
}

func TestHTTPJudgeAcceptsStringScoreFromChattyModel(t *testing.T) {
	t.Parallel()
	srv := newJudgeServer(t, `{"choices":[{"message":{"content":"{\"score\":\"4\",\"reason\":\"ok\"}"}}]}`, 200)
	j := newJudge(srv)
	v, err := j.JudgeCell(context.Background(), JudgeInput{Tags: []string{"x"}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if v.Score != 4 {
		t.Fatalf("score = %v, want 4 (string coercion)", v.Score)
	}
}

func TestHTTPJudgeAcceptsVisionStyleArrayContent(t *testing.T) {
	t.Parallel()
	// Some servers return content as [{type:text,text:...}].
	srv := newJudgeServer(t, `{"choices":[{"message":{"content":[{"type":"text","text":"{\"score\":5,\"reason\":\"perfect\"}"}]}}]}`, 200)
	j := newJudge(srv)
	v, err := j.JudgeCell(context.Background(), JudgeInput{Tags: []string{"x"}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if v.Score != 5 {
		t.Fatalf("array-style content not parsed; score = %v", v.Score)
	}
}

// stubJudge lets runner tests verify the per-cell wiring without
// hitting an HTTP server.
type stubJudge struct {
	verdict JudgeVerdict
	err     error
	calls   int
	inputs  []JudgeInput
}

func (s *stubJudge) JudgeCell(_ context.Context, in JudgeInput) (JudgeVerdict, error) {
	s.calls++
	s.inputs = append(s.inputs, in)
	if s.err != nil {
		return JudgeVerdict{}, s.err
	}
	return s.verdict, nil
}

func TestRunPopulatesJudgeFields(t *testing.T) {
	t.Parallel()
	srv := newAnalyzerServer(t, `{"choices":[{"message":{"content":"{\"summary\":\"x\",\"tags\":[\"RAG\",\"腾讯\",\"ima\"]}"}}]}`)
	judge := &stubJudge{verdict: JudgeVerdict{Score: 4.2, SummaryScore: 4.5, TagScore: 4, Reason: "good output"}}
	cfg := RunConfig{
		Cases:    []Case{{ID: "c", URL: "https://x", Expected: CaseExpected{MustInclude: []string{"RAG"}}}},
		Fetchers: []FetcherName{"f"},
		Prompts:  []PromptVariant{{Name: "p", Body: "x"}},
		Models:   []string{"m"},
		BuildFetcher: func(_ FetcherName) (fetcher.Fetcher, error) {
			return stubFetcher{}, nil
		},
		BuildAnalyzer: func(_ string) *analyzer.OpenAIAnalyzer { return newAnalyzer(srv) },
		Judge:         judge,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if judge.calls != 1 {
		t.Fatalf("judge.calls = %d, want 1", judge.calls)
	}
	if res.Cells[0].JudgeScore != 4.2 || res.Cells[0].JudgeSummaryScore != 4.5 || res.Cells[0].JudgeTagScore != 4 || res.Cells[0].JudgeReason != "good output" {
		t.Fatalf("cell = %+v", res.Cells[0])
	}
}

func TestRunSkipsJudgeOnFetchError(t *testing.T) {
	t.Parallel()
	srv := newAnalyzerServer(t, `{"choices":[{"message":{"content":"{\"summary\":\"x\",\"tags\":[\"RAG\",\"腾讯\",\"ima\"]}"}}]}`)
	judge := &stubJudge{verdict: JudgeVerdict{Score: 5}}
	cfg := RunConfig{
		Cases:    []Case{{ID: "c", URL: "https://x"}},
		Fetchers: []FetcherName{"f"},
		Prompts:  []PromptVariant{{Name: "p", Body: "x"}},
		Models:   []string{"m"},
		BuildFetcher: func(_ FetcherName) (fetcher.Fetcher, error) {
			return stubFetcher{err: errFake}, nil
		},
		BuildAnalyzer: func(_ string) *analyzer.OpenAIAnalyzer { return newAnalyzer(srv) },
		Judge:         judge,
	}
	res, _ := Run(context.Background(), cfg)
	if judge.calls != 0 {
		t.Fatalf("judge should not be called when fetch fails; got %d", judge.calls)
	}
	if res.Cells[0].JudgeScore != 0 {
		t.Fatalf("JudgeScore = %v, want 0 (skipped)", res.Cells[0].JudgeScore)
	}
}

var errFake = &fakeErr{"fake fetch error"}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }
