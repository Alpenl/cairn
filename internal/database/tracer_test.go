package database

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"webtag/internal/observability"
)

func TestSqlVerb(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", "other"},
		{"select", "SELECT id FROM links", "SELECT"},
		{"lowercase", "select id from links", "SELECT"},
		{"insert", "INSERT INTO links (url) VALUES ($1)", "INSERT"},
		{"leading space", "  SELECT 1", "SELECT"},
		{"leading newline", "\n\tSELECT 1", "SELECT"},
		{"only whitespace", "   \t\n", "other"},
		{"non-ascii", "SELÉCT 1", "other"},
		{"single word no whitespace", "VACUUM", "VACUUM"},
		{"underscore allowed", "WITH_RECURSIVE foo AS (SELECT 1)", "WITH_RECURSIVE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sqlVerb(tc.in)
			if got != tc.want {
				t.Errorf("sqlVerb(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewQueryTracerNilSafe(t *testing.T) {
	tr := NewQueryTracer(nil)
	if tr == nil {
		t.Fatal("NewQueryTracer(nil) returned nil tracer")
	}
	// Both ends must be no-op when metrics is nil; in particular they
	// must not panic on a nil deref or nil context value.
	ctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
}

func TestQueryTracerObservesDuration(t *testing.T) {
	metrics := observability.NewMetrics()
	tr := NewQueryTracer(metrics)

	ctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT id FROM links"})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})

	// Verify the histogram observed exactly one sample under method="SELECT".
	count := histogramSampleCount(t, metrics.DBQueryDuration, "SELECT")
	if count != 1 {
		t.Errorf("DBQueryDuration{method=SELECT} sample count = %d, want 1", count)
	}
}

// TestQueryTracerSkipsWhenMetricNil exercises the path where metrics is
// non-nil but DBQueryDuration was not registered (defensive guard).
func TestQueryTracerSkipsWhenMetricNil(t *testing.T) {
	metrics := &observability.Metrics{}
	tr := NewQueryTracer(metrics)
	ctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	// No panic, no observation — that is success.
}

// TestQueryTracerEndWithoutStart guards against a TraceQueryEnd called
// with a context that did not pass through TraceQueryStart (could
// happen if the tracer was attached mid-query, or after a panic
// re-entry). Must be a silent no-op, not a panic.
func TestQueryTracerEndWithoutStart(t *testing.T) {
	metrics := observability.NewMetrics()
	tr := NewQueryTracer(metrics)
	// Bare context — never went through Start.
	tr.TraceQueryEnd(context.Background(), nil, pgx.TraceQueryEndData{})
	if got := histogramSampleCount(t, metrics.DBQueryDuration, "SELECT"); got != 0 {
		t.Errorf("expected no samples, got %d", got)
	}
}

func histogramSampleCount(t *testing.T, vec *prometheus.HistogramVec, label string) uint64 {
	t.Helper()
	h, err := vec.GetMetricWithLabelValues(label)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", label, err)
	}
	m := &dto.Metric{}
	if err := h.(prometheus.Histogram).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	if m.Histogram == nil {
		return 0
	}
	return m.Histogram.GetSampleCount()
}
