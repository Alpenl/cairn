package observability_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"webtag/internal/observability"
)

// TestNewMetrics_NoPanic verifies that NewMetrics does not panic and returns
// a non-nil value.
func TestNewMetrics_NoPanic(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
}

// TestNewMetrics_AllFieldsNonNil confirms that every exported metric field is
// wired up so callers do not need nil-guards in hot paths.
func TestNewMetrics_AllFieldsNonNil(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()

	if m.ParseRunsTotal == nil {
		t.Error("ParseRunsTotal is nil")
	}
	if m.ParseFailuresTotal == nil {
		t.Error("ParseFailuresTotal is nil")
	}
	if m.ParseLowConfidenceTotal == nil {
		t.Error("ParseLowConfidenceTotal is nil")
	}
	if m.EnsureParentPathDepth == nil {
		t.Error("EnsureParentPathDepth is nil")
	}
	if m.TreeResponseTruncatedTotal == nil {
		t.Error("TreeResponseTruncatedTotal is nil")
	}
	if m.HTTPRequestTotal == nil {
		t.Error("HTTPRequestTotal is nil")
	}
	if m.HTTPRequestDuration == nil {
		t.Error("HTTPRequestDuration is nil")
	}
	if m.DBQueryDuration == nil {
		t.Error("DBQueryDuration is nil")
	}
	if m.FetcherDuration == nil {
		t.Error("FetcherDuration is nil")
	}
	if m.PDFParseOutcomesTotal == nil {
		t.Error("PDFParseOutcomesTotal is nil")
	}
	if m.TranslationTerminalOutcomesTotal == nil {
		t.Error("TranslationTerminalOutcomesTotal is nil")
	}
	if m.ReaderContentHistoryDeletedTotal == nil {
		t.Error("ReaderContentHistoryDeletedTotal is nil")
	}
	if m.ReaderContentHistoryCleanupRunsTotal == nil {
		t.Error("ReaderContentHistoryCleanupRunsTotal is nil")
	}
	if m.ReaderInboxDispatchRepairsTotal == nil {
		t.Error("ReaderInboxDispatchRepairsTotal is nil")
	}
}

func TestReaderInboxDispatchRepairMetric(t *testing.T) {
	t.Parallel()
	metrics := observability.NewMetrics()
	metrics.ReaderInboxDispatchRepairsTotal.WithLabelValues("success").Add(4)
	metrics.ReaderInboxDispatchRepairsTotal.WithLabelValues("failure").Inc()

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		`webtag_reader_inbox_dispatch_repairs_total{outcome="success"} 4`,
		`webtag_reader_inbox_dispatch_repairs_total{outcome="failure"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
}

func TestPDFParseOutcomesMetricUsesOnlyBoundedResourceLabels(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()
	labels := [][2]string{
		{"success", "none"},
		{"limit", "pages"},
		{"crash", "none"},
		{"timeout", "wall_time"},
	}
	for _, label := range labels {
		metrics.PDFParseOutcomesTotal.WithLabelValues(label[0], label[1]).Inc()
		if got := testutil.ToFloat64(metrics.PDFParseOutcomesTotal.WithLabelValues(label[0], label[1])); got != 1 {
			t.Fatalf("PDF parse outcome %v = %v, want 1", label, got)
		}
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		"# HELP webtag_pdf_parse_outcomes_total",
		`webtag_pdf_parse_outcomes_total{limit="none",outcome="success"} 1`,
		`webtag_pdf_parse_outcomes_total{limit="pages",outcome="limit"} 1`,
		`webtag_pdf_parse_outcomes_total{limit="none",outcome="crash"} 1`,
		`webtag_pdf_parse_outcomes_total{limit="wall_time",outcome="timeout"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
}

// TestNewMetrics_CanRecord confirms that Inc/Observe do not panic.
func TestNewMetrics_CanRecord(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()

	// Counter
	m.ParseRunsTotal.WithLabelValues("ok", "basic", "text/html").Inc()

	// Histogram
	m.EnsureParentPathDepth.Observe(3)
	m.DBQueryDuration.WithLabelValues("SELECT").Observe(0.002)
	m.FetcherDuration.WithLabelValues("fetch", "example.com").Observe(0.1)
}

func TestTranslationTerminalOutcomesMetric(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()
	for _, outcome := range []string{
		"translation_job_cancelled",
		"translation_job_discarded",
		"terminal_projection_missing",
		"translation_job_missing",
		"rejected_malformed_args",
		"stale",
		"failed",
	} {
		metrics.TranslationTerminalOutcomesTotal.WithLabelValues(outcome).Inc()
		if got := testutil.ToFloat64(metrics.TranslationTerminalOutcomesTotal.WithLabelValues(outcome)); got != 1 {
			t.Fatalf("translation terminal outcome %q = %v, want 1", outcome, got)
		}
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		"# HELP webtag_translation_terminal_reconciler_outcomes_total",
		`webtag_translation_terminal_reconciler_outcomes_total{outcome="translation_job_cancelled"} 1`,
		`webtag_translation_terminal_reconciler_outcomes_total{outcome="rejected_malformed_args"} 1`,
		`webtag_translation_terminal_reconciler_outcomes_total{outcome="stale"} 1`,
		`webtag_translation_terminal_reconciler_outcomes_total{outcome="failed"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
}

func TestTranslationSourceOutcomesMetric(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()
	for _, outcome := range []string{
		"legacy_unverified_write",
		"verified_schedule",
		"content_revision_conflict",
		"source_block_conflict",
		"translation_schema_transition",
	} {
		metrics.TranslationSourceOutcomesTotal.WithLabelValues(outcome).Inc()
		if got := testutil.ToFloat64(metrics.TranslationSourceOutcomesTotal.WithLabelValues(outcome)); got != 1 {
			t.Fatalf("translation source outcome %q = %v, want 1", outcome, got)
		}
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		"# HELP webtag_translation_source_outcomes_total",
		`webtag_translation_source_outcomes_total{outcome="legacy_unverified_write"} 1`,
		`webtag_translation_source_outcomes_total{outcome="verified_schedule"} 1`,
		`webtag_translation_source_outcomes_total{outcome="content_revision_conflict"} 1`,
		`webtag_translation_source_outcomes_total{outcome="source_block_conflict"} 1`,
		`webtag_translation_source_outcomes_total{outcome="translation_schema_transition"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, body)
		}
	}
}

// TestNewMetrics_Handler_NotNil verifies the handler is reachable.
func TestNewMetrics_Handler_NotNil(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()
	if m.Handler() == nil {
		t.Error("Handler() returned nil")
	}
}

// TestRegisterPgxpoolGauges verifies RegisterPgxpoolGauges does not panic and
// the gauges are non-nil after registration.
func TestRegisterPgxpoolGauges(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()

	stat := func() observability.PgxpoolStat {
		return observability.PgxpoolStat{
			AcquireCount: 42,
			IdleConns:    2,
			TotalConns:   5,
		}
	}

	// Must not panic.
	m.RegisterPgxpoolGauges(stat)

	if m.PgxpoolAcquireCount == nil {
		t.Error("PgxpoolAcquireCount is nil after RegisterPgxpoolGauges")
	}
	if m.PgxpoolIdleConns == nil {
		t.Error("PgxpoolIdleConns is nil after RegisterPgxpoolGauges")
	}
	if m.PgxpoolTotalConns == nil {
		t.Error("PgxpoolTotalConns is nil after RegisterPgxpoolGauges")
	}

	// Verify the gauge values are plumbed by inspecting the handler output.
	// We check indirectly: the gauge was registered without panic and fields are non-nil.
	// A direct metric value read would require prometheus/client_model internals;
	// we validate via the HTTP handler output in integration tests instead.
}

// TestRegisterPgxpoolGauges_Idempotent ensures a second call is a no-op and
// does not panic (e.g. duplicate-registration would panic via promauto).
func TestRegisterPgxpoolGauges_Idempotent(t *testing.T) {
	t.Parallel()
	m := observability.NewMetrics()
	stat := func() observability.PgxpoolStat { return observability.PgxpoolStat{} }

	// First call registers the gauges.
	m.RegisterPgxpoolGauges(stat)
	// Second call must be a no-op (not panic).
	m.RegisterPgxpoolGauges(stat)
}

// TestRegisterPgxpoolGauges_NilSafe 锁住 RegisterPgxpoolGauges 的两条
// nil 兜底分支：
//   - 接收者 m 为 nil 时直接 return，不能 panic（deps.go 在 metrics 创建
//     失败时会传 nil）
//   - stat 函数为 nil 时也 return（拒绝注册一个会 panic 的 GaugeFunc）
//
// 这两条分支之前没有覆盖；缺一条会导致启动期 panic 把 server 拖垮。
//
// 两条用 subtest 隔离 recover，避免一边意外 panic 被另一边的 defer 吞掉
// 导致错误归因模糊。
func TestRegisterPgxpoolGauges_NilSafe(t *testing.T) {
	t.Parallel()

	t.Run("nil_receiver", func(t *testing.T) {
		t.Parallel()
		var nilMetrics *observability.Metrics
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("RegisterPgxpoolGauges on nil receiver panicked: %v", r)
			}
		}()
		nilMetrics.RegisterPgxpoolGauges(func() observability.PgxpoolStat { return observability.PgxpoolStat{} })
	})

	t.Run("nil_stat", func(t *testing.T) {
		t.Parallel()
		m := observability.NewMetrics()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("RegisterPgxpoolGauges with nil stat panicked: %v", r)
			}
		}()
		m.RegisterPgxpoolGauges(nil)
		if m.PgxpoolAcquireCount != nil {
			t.Error("nil stat should not have registered any gauge")
		}
	})
}

// TestHandler_NilReceiver 锁定 (*Metrics)(nil).Handler() 回退到全局
// promhttp.Handler()，方便 deps.go 在 metrics 失败时仍能暴露
// /metrics endpoint 而不需要额外 nil 判断。
//
// 不只断言 != nil（那样未来代码改成 `return brokenHandler{}` 也能通过），
// 真起一次 ServeHTTP 看 body 是否符合 Prometheus exposition 格式——
// 全局默认 registry 里至少有 go_ / process_ 前缀的内置指标，借此证明
// 拿到的是个真能 serve 的 prom handler。
func TestHandler_NilReceiver(t *testing.T) {
	t.Parallel()

	var nilMetrics *observability.Metrics
	h := nilMetrics.Handler()
	if h == nil {
		t.Fatal("(*Metrics)(nil).Handler() = nil, want global promhttp.Handler() fallback")
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("handler status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	// 不强行要求 go_ 前缀（不同 Go 版本暴露的内置指标集合略有差异），
	// 而是要求至少看到 Prometheus 文本格式的 # HELP 行，证明输出是
	// prom-shape 而不是任意 200 响应。
	if !strings.Contains(body, "# HELP ") {
		t.Errorf("body does not look like Prometheus exposition format (missing '# HELP '): %q", truncateForError(body))
	}
}

// truncateForError 把过长的响应体裁短，避免测试失败时把 KB 级输出全
// 灌到失败日志里。
func truncateForError(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
