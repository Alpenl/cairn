// Package database – pgx query tracer for Prometheus metrics.
//
// QueryTracer implements the pgx.QueryTracer interface so that every SQL
// call routed through the pool is timed and observed into
// webtag_db_query_duration_seconds without touching business code.
//
// Usage – attach to the pgxpool.Config before creating the pool:
//
//	cfg.Tracer = database.NewQueryTracer(metrics)
package database

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"webtag/internal/observability"
)

// queryTracerKey is the context key used to pass the query start time
// from TraceQueryStart to TraceQueryEnd.
type queryTracerKey struct{}

// queryTracerData carries the method label and start time through the
// context so TraceQueryEnd can observe the correct duration.
type queryTracerData struct {
	method string
	start  time.Time
}

// QueryTracer implements pgx.QueryTracer.  A nil metrics pointer is safe:
// all methods become no-ops so the tracer can be registered unconditionally.
type QueryTracer struct {
	metrics *observability.Metrics
}

// NewQueryTracer returns a *QueryTracer wired to the given Metrics.
// Passing nil disables timing without removing the hook from pgx.
func NewQueryTracer(m *observability.Metrics) *QueryTracer {
	return &QueryTracer{metrics: m}
}

// TraceQueryStart records the start time.  It satisfies pgx.QueryTracer.
func (t *QueryTracer) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	if t.metrics == nil || t.metrics.DBQueryDuration == nil {
		return ctx
	}
	// Derive a human-friendly method label from the SQL verb.
	method := sqlVerb(data.SQL)
	return context.WithValue(ctx, queryTracerKey{}, queryTracerData{
		method: method,
		start:  time.Now(),
	})
}

// TraceQueryEnd observes the elapsed duration.  It satisfies pgx.QueryTracer.
func (t *QueryTracer) TraceQueryEnd(
	ctx context.Context,
	_ *pgx.Conn,
	_ pgx.TraceQueryEndData,
) {
	if t.metrics == nil || t.metrics.DBQueryDuration == nil {
		return
	}
	v, _ := ctx.Value(queryTracerKey{}).(queryTracerData)
	if v.start.IsZero() {
		return
	}
	elapsed := time.Since(v.start).Seconds()
	t.metrics.DBQueryDuration.WithLabelValues(v.method).Observe(elapsed)
}

// sqlVerb extracts the first word of a SQL statement as a short method
// label (SELECT, INSERT, UPDATE, DELETE, …).  Unknown / empty inputs
// return "other" to bound label cardinality.
//
// Leading whitespace (e.g. ORM-formatted SQL like "\n  SELECT ...") is
// skipped so the verb extracted is the first non-whitespace token.
func sqlVerb(sql string) string {
	start := 0
	for start < len(sql) && isASCIISpace(sql[start]) {
		start++
	}
	if start == len(sql) {
		return "other"
	}
	end := start
	for end < len(sql) && !isASCIISpace(sql[end]) {
		end++
	}
	verb := toUpperASCII(sql[start:end])
	if verb == "" {
		return "other"
	}
	return verb
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// toUpperASCII returns the ASCII-uppercased version of s, or "" if it
// contains non-ASCII characters (to keep label cardinality safe).
func toUpperASCII(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			out[i] = c - 32
		case (c >= 'A' && c <= 'Z') || c == '_':
			out[i] = c
		default:
			return "other"
		}
	}
	return string(out)
}
