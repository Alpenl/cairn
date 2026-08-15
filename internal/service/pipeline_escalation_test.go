package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
	analyzerpkg "webtag/internal/service/analyzer"
)

// longBody returns a body comfortably above escalationMinBodyChars so
// isThinContent treats it as NOT thin on the rune-count leg.
func longBody() string {
	return strings.Repeat("x", escalationMinBodyChars+50)
}

func TestIsThinContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content fetcher.Content
		want    bool
	}{
		{"+thin suffix is thin even with long body", fetcher.Content{FetcherType: "light+thin", Body: longBody()}, true},
		{"thin suffix case-insensitive", fetcher.Content{FetcherType: "BASIC+THIN", Body: longBody()}, true},
		{"empty body is thin", fetcher.Content{FetcherType: "light", Body: ""}, true},
		{"short body is thin", fetcher.Content{FetcherType: "light", Body: "tiny"}, true},
		{"long body without thin suffix is not thin", fetcher.Content{FetcherType: "basic", Body: longBody()}, false},
		{"search_fallback alone is not thin (long body)", fetcher.Content{FetcherType: "basic+search", Body: longBody()}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isThinContent(tc.content); got != tc.want {
				t.Errorf("isThinContent(%+v) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

func TestIsExplicitLight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		link *model.Link
		want bool
	}{
		{"nil link", nil, false},
		{"no metadata", &model.Link{}, false},
		{"no parse_depth key", &model.Link{SourceMetadata: map[string]any{"other": "x"}}, false},
		{"explicit light", &model.Link{SourceMetadata: map[string]any{"parse_depth": "light"}}, true},
		{"explicit light mixed case + space", &model.Link{SourceMetadata: map[string]any{"parse_depth": "  LIGHT  "}}, true},
		{"explicit deep", &model.Link{SourceMetadata: map[string]any{"parse_depth": "deep"}}, false},
		{"non-string parse_depth", &model.Link{SourceMetadata: map[string]any{"parse_depth": 1}}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isExplicitLight(tc.link); got != tc.want {
				t.Errorf("isExplicitLight = %v, want %v", got, tc.want)
			}
		})
	}
}

// escalationFetcher records Fetch (deep) calls and returns a scripted deep
// result. It only implements ContentFetcher — escalateIfThin re-fetches via
// p.fetcher.Fetch, never FetchLight.
type escalationFetcher struct {
	deepCalls   int
	deepContent fetcher.Content
	deepErr     error
}

func (f *escalationFetcher) Fetch(context.Context, string) (fetcher.Content, error) {
	f.deepCalls++
	return f.deepContent, f.deepErr
}

// newEscalationPipeline 直接构造结构体而非走 NewParsePipeline：这些用例只驱动
// escalateIfThin，不触达仓储与终态写入。参见 pipelineWithFetcher 的同款说明。
func newEscalationPipeline(f ContentFetcher, m *observability.Metrics) *ParsePipeline {
	return &ParsePipeline{fetcher: f, metrics: m}
}

func thinLight() fetcher.Content {
	return fetcher.Content{URL: "https://example.com/", Title: "t", Body: "tiny", FetcherType: "light+thin"}
}

func deepGood() fetcher.Content {
	return fetcher.Content{URL: "https://example.com/", Title: "t", Body: longBody(), FetcherType: "basic"}
}

// TestEscalateIfThinReplacesThinLightWithDeep: a light+thin result on the
// PreferLight-by-default path is re-fetched once; the deep content replaces it
// and the recovered-outcome counter ticks.
func TestEscalateIfThinReplacesThinLightWithDeep(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()
	f := &escalationFetcher{deepContent: deepGood()}
	p := newEscalationPipeline(f, m)
	link := &model.Link{ID: uuid.New(), URL: "https://example.com/"}

	got := p.escalateIfThin(context.Background(), parseInputForTest(link), thinLight(), true)

	if f.deepCalls != 1 {
		t.Fatalf("deep re-fetch calls = %d, want 1", f.deepCalls)
	}
	if got.FetcherType != "basic" || got.Body != longBody() {
		t.Fatalf("escalated content = %+v, want deep content", got)
	}
	if v := testutil.ToFloat64(m.FetchEscalationTotal.WithLabelValues(escalationOutcomeRecovered)); v != 1 {
		t.Fatalf("recovered escalation counter = %v, want 1", v)
	}
}

func TestEscalationLogDoesNotExposePrivateURL(t *testing.T) {
	t.Parallel()

	const privateURL = "https://reader:secret@example.com/private/account/path?token=review-secret#fragment"
	var output bytes.Buffer
	f := &escalationFetcher{deepContent: deepGood()}
	p := newEscalationPipeline(f, nil)
	p.logger = slog.New(slog.NewJSONHandler(&output, nil))
	link := &model.Link{ID: uuid.New(), URL: privateURL}

	p.escalateIfThin(context.Background(), parseInputForTest(link), thinLight(), true)

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if record["url_host"] != "example.com" || record["link_id"] != link.ID.String() {
		t.Fatalf("safe correlation fields = %#v", record)
	}
	encoded := output.String()
	for _, secret := range []string{"reader:secret", "/private/account/path", "review-secret", "#fragment", privateURL} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("log leaked %q: %s", secret, encoded)
		}
	}
}

// TestEscalateIfThinStillThinCountsStillThin: when the deep re-fetch is also
// thin the deep content still replaces the light one but the still_thin outcome
// is recorded.
func TestEscalateIfThinStillThinCountsStillThin(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()
	deepThin := fetcher.Content{URL: "https://example.com/", Body: "still tiny", FetcherType: "basic+thin"}
	f := &escalationFetcher{deepContent: deepThin}
	p := newEscalationPipeline(f, m)
	link := &model.Link{ID: uuid.New(), URL: "https://example.com/"}

	got := p.escalateIfThin(context.Background(), parseInputForTest(link), thinLight(), true)

	if f.deepCalls != 1 {
		t.Fatalf("deep re-fetch calls = %d, want 1", f.deepCalls)
	}
	if got.FetcherType != "basic+thin" {
		t.Fatalf("escalated content type = %q, want deep result kept", got.FetcherType)
	}
	if v := testutil.ToFloat64(m.FetchEscalationTotal.WithLabelValues(escalationOutcomeStillThin)); v != 1 {
		t.Fatalf("still_thin escalation counter = %v, want 1", v)
	}
}

// TestEscalateIfThinSkipsExplicitLight: a link that explicitly asked for light
// is never escalated even when its result is thin.
func TestEscalateIfThinSkipsExplicitLight(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()
	f := &escalationFetcher{deepContent: deepGood()}
	p := newEscalationPipeline(f, m)
	link := &model.Link{
		ID:             uuid.New(),
		URL:            "https://example.com/",
		SourceMetadata: map[string]any{"parse_depth": "light"},
	}

	got := p.escalateIfThin(context.Background(), parseInputForTest(link), thinLight(), true)

	if f.deepCalls != 0 {
		t.Fatalf("explicit-light link must not re-fetch; deep calls = %d", f.deepCalls)
	}
	if got.FetcherType != "light+thin" {
		t.Fatalf("explicit-light content must be untouched; got %q", got.FetcherType)
	}
	if v := testutil.ToFloat64(m.FetchEscalationTotal.WithLabelValues(escalationOutcomeRecovered)); v != 0 {
		t.Fatalf("escalation counter must stay 0 for explicit light; got %v", v)
	}
}

// TestEscalateIfThinSkipsWhenNotLightPath: a deep run (tookLight=false, as the
// caller sets for ingest and deep fetches) is never escalated regardless of
// thinness.
func TestEscalateIfThinSkipsWhenNotLightPath(t *testing.T) {
	t.Parallel()
	f := &escalationFetcher{deepContent: deepGood()}
	p := newEscalationPipeline(f, nil)
	link := &model.Link{ID: uuid.New(), URL: "https://example.com/"}

	got := p.escalateIfThin(context.Background(), parseInputForTest(link), thinLight(), false)

	if f.deepCalls != 0 {
		t.Fatalf("non-light run must not re-fetch; deep calls = %d", f.deepCalls)
	}
	if got.FetcherType != "light+thin" {
		t.Fatalf("non-light content must be untouched; got %q", got.FetcherType)
	}
}

// TestEscalateIfThinSkipsWhenNotThin: a light run that produced a sufficient
// body is not escalated.
func TestEscalateIfThinSkipsWhenNotThin(t *testing.T) {
	t.Parallel()
	f := &escalationFetcher{deepContent: deepGood()}
	p := newEscalationPipeline(f, nil)
	link := &model.Link{ID: uuid.New(), URL: "https://example.com/"}
	goodLight := fetcher.Content{URL: "https://example.com/", Body: longBody(), FetcherType: "light"}

	got := p.escalateIfThin(context.Background(), parseInputForTest(link), goodLight, true)

	if f.deepCalls != 0 {
		t.Fatalf("non-thin light run must not re-fetch; deep calls = %d", f.deepCalls)
	}
	if got.FetcherType != "light" {
		t.Fatalf("non-thin content must be untouched; got %q", got.FetcherType)
	}
}

// TestEscalateIfThinKeepsLightOnDeepError: when the deep re-fetch errors the
// light content is preserved (parse must still proceed) and no counter ticks.
func TestEscalateIfThinKeepsLightOnDeepError(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()
	f := &escalationFetcher{deepErr: errors.New("upstream down")}
	p := newEscalationPipeline(f, m)
	link := &model.Link{ID: uuid.New(), URL: "https://example.com/"}

	got := p.escalateIfThin(context.Background(), parseInputForTest(link), thinLight(), true)

	if f.deepCalls != 1 {
		t.Fatalf("deep re-fetch attempted once even on error; deep calls = %d", f.deepCalls)
	}
	if got.FetcherType != "light+thin" {
		t.Fatalf("light content must survive a failed re-fetch; got %q", got.FetcherType)
	}
	if v := testutil.ToFloat64(m.FetchEscalationTotal.WithLabelValues(escalationOutcomeRecovered)); v != 0 {
		t.Fatalf("recovered counter must stay 0 on re-fetch error; got %v", v)
	}
	if v := testutil.ToFloat64(m.FetchEscalationTotal.WithLabelValues(escalationOutcomeStillThin)); v != 0 {
		t.Fatalf("still_thin counter must stay 0 on re-fetch error; got %v", v)
	}
}

func TestEscalationFailureLogDoesNotExposePrivateURL(t *testing.T) {
	t.Parallel()

	const privateURL = "https://reader:secret@example.com/private/account/path?token=review-secret#fragment"
	var output bytes.Buffer
	f := &escalationFetcher{deepErr: errors.New("upstream down")}
	p := newEscalationPipeline(f, nil)
	p.logger = slog.New(slog.NewJSONHandler(&output, nil))
	link := &model.Link{ID: uuid.New(), URL: privateURL}

	p.escalateIfThin(context.Background(), parseInputForTest(link), thinLight(), true)

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if record["url_host"] != "example.com" || record["link_id"] != link.ID.String() {
		t.Fatalf("safe correlation fields = %#v", record)
	}
	encoded := output.String()
	for _, secret := range []string{"reader:secret", "/private/account/path", "review-secret", "#fragment", privateURL} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("log leaked %q: %s", secret, encoded)
		}
	}
}

// dualEscalationFetcher implements both ContentFetcher and LightContentFetcher
// so a full pipeline.Run exercises the light path → escalation → deep path. It
// records both legs so the test can assert each fired exactly once.
type dualEscalationFetcher struct {
	lightCalls int
	deepCalls  int
	light      fetcher.Content
	deep       fetcher.Content
}

func (f *dualEscalationFetcher) FetchLight(_ context.Context, url string) (fetcher.Content, error) {
	f.lightCalls++
	c := f.light
	c.URL = url
	return c, nil
}

func (f *dualEscalationFetcher) Fetch(_ context.Context, url string) (fetcher.Content, error) {
	f.deepCalls++
	c := f.deep
	c.URL = url
	return c, nil
}

// runEscalationPipeline builds a full pipeline over the in-memory fakes (shared
// with pipeline_test.go) and runs a single PreferLight link through it.
func runEscalationPipeline(t *testing.T, f ContentFetcher, link *model.Link, metrics *observability.Metrics) *repository.UpdateLinkAnalysisParams {
	t.Helper()
	now := time.Now().UTC()
	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{link.ID: link})
	jobID := uuid.New()
	jobStore := newPipelineJobStore(map[uuid.UUID]*model.ParseJob{
		link.ID: {ID: jobID, LinkID: link.ID, Status: model.JobStatusPending, CreatedAt: now, UpdatedAt: now},
	})
	tagStore := &pipelineFakeTagStore{}
	treeStore := newPipelineTreeStore(map[string]*model.Link{})
	analyzer := pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		// Echo the analyzed body back so the test can prove which fetch leg's
		// content reached the analyzer.
		return analyzerpkg.AnalysisResult{Summary: "S:" + req.Content.Body, Tags: []string{"t"}}, nil
	})
	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Jobs:             jobStore,
		Tags:             tagStore,
		Tree:             treeStore,
		Fetcher:          f,
		Analyzer:         analyzer,
		Metrics:          metrics,
		PreferLight:      true,
	})
	_ = pipeline.Run(context.Background(), link.ID, jobID)
	if len(linkStore.UpdateAnalysisCalls) != 1 {
		t.Fatalf("analysis updates = %d, want 1", len(linkStore.UpdateAnalysisCalls))
	}
	return &linkStore.UpdateAnalysisCalls[0]
}

// TestRunEscalatesThinLightEndToEnd: a PreferLight link whose light fetch is
// thin gets re-fetched deep, and the deep content (not the light one) is what
// lands on the persisted link.
func TestRunEscalatesThinLightEndToEnd(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()
	f := &dualEscalationFetcher{
		light: fetcher.Content{Title: "t", Body: "thin", FetcherType: "light+thin"},
		deep:  fetcher.Content{Title: "t", Body: longBody(), FetcherType: "basic"},
	}
	link := &model.Link{ID: uuid.New(), URL: "https://example.com/a", Status: model.LinkStatusPending}

	update := runEscalationPipeline(t, f, link, m)

	if f.lightCalls != 1 || f.deepCalls != 1 {
		t.Fatalf("lightCalls=%d deepCalls=%d, want 1/1 (light then one escalation)", f.lightCalls, f.deepCalls)
	}
	if update.FetcherType == nil || *update.FetcherType != "basic" {
		t.Fatalf("persisted fetcher_type = %v, want basic (deep content)", update.FetcherType)
	}
	// Summary echoes the analyzed body — it must be the deep body, proving the
	// deep content replaced the light one before analysis.
	if update.Summary == nil || !strings.Contains(*update.Summary, longBody()) {
		t.Fatalf("analyzer consumed the wrong body; summary=%v", update.Summary)
	}
	if v := testutil.ToFloat64(m.FetchEscalationTotal.WithLabelValues(escalationOutcomeRecovered)); v != 1 {
		t.Fatalf("recovered escalation counter = %v, want 1", v)
	}
}

// TestRunDoesNotEscalateExplicitLight: a link that pinned parse_depth=light
// keeps its thin light content and never re-fetches.
func TestRunDoesNotEscalateExplicitLight(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()
	f := &dualEscalationFetcher{
		light: fetcher.Content{Title: "t", Body: "thin", FetcherType: "light+thin"},
		deep:  fetcher.Content{Title: "t", Body: longBody(), FetcherType: "basic"},
	}
	link := &model.Link{
		ID:             uuid.New(),
		URL:            "https://example.com/b",
		Status:         model.LinkStatusPending,
		SourceMetadata: map[string]any{"parse_depth": "light"},
	}

	update := runEscalationPipeline(t, f, link, m)

	if f.lightCalls != 1 || f.deepCalls != 0 {
		t.Fatalf("lightCalls=%d deepCalls=%d, want 1/0 (explicit light, no escalation)", f.lightCalls, f.deepCalls)
	}
	if update.FetcherType == nil || *update.FetcherType != "light+thin" {
		t.Fatalf("persisted fetcher_type = %v, want light+thin", update.FetcherType)
	}
	if v := testutil.ToFloat64(m.FetchEscalationTotal.WithLabelValues(escalationOutcomeRecovered)); v != 0 {
		t.Fatalf("escalation counter must stay 0 for explicit light; got %v", v)
	}
}
