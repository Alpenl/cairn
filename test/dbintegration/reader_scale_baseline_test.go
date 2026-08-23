// Reader vNext hot-path baseline — issue #40 phase 0.
//
// Two tests live here and they have deliberately different jobs.
//
//   - TestReaderScaleFixtureContract runs whenever dbintegration runs. It locks
//     only things that cannot drift on a busy machine: the fixture's row counts
//     and per-table digests, the number of SQL statements each hot path issues,
//     an upper bound on the serialised response, and the plan shape of the one
//     statement that decides each path's cost. No timing assertion appears
//     anywhere in it, because a microsecond threshold on a shared CI runner
//     fails for reasons that have nothing to do with the code under review.
//
//   - TestReaderScaleHotPathMeasurements is opt-in behind
//     WEBTAG_READER_PERF_MEASURE=1. It is the thing that produces the actual
//     p50/p95, payload sizes and EXPLAIN (ANALYZE, BUFFERS) output, and writes
//     them to the gitignored artifacts/ tree. It is slow by construction —
//     fixed warmup plus at least 100 warm samples across six paths — and must
//     never be wired into a default gate.
//
// Each test seeds its own database, but both build it from the same generator
// and drive the same readerScalePaths through the same tracing pool. A number
// recorded by the measurement run and a budget enforced by the contract run
// therefore describe the same work, not two similar-looking ones.
package dbintegration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/handler"
	"webtag/internal/model"
	"webtag/internal/repository"
)

const (
	// readerScaleMeasureEnv gates the slow measurement run. Same shape as the
	// existing WEBTAG_LINK_PROJECTION_MEASURE switch — a repository already has
	// one opt-in measurement convention and a second one would only create
	// two things to remember.
	readerScaleMeasureEnv = "WEBTAG_READER_PERF_MEASURE"
	// readerScaleWriteManifestEnv regenerates the tracked manifest from a live
	// fixture instead of asserting against it. This is how digests, query
	// budgets and byte ceilings are refreshed after an intentional change; it
	// must never be set in CI, or the contract test would assert nothing.
	readerScaleWriteManifestEnv = "WEBTAG_READER_PERF_WRITE_MANIFEST"
	// readerScalePlanFileEnv overrides the measurement artifact path. Declared
	// by the manifest as postgres_plan_artifact_env since the original wave-0
	// fixture, and kept rather than replaced.
	readerScalePlanFileEnv = "READER_PERF_POSTGRES_PLAN_FILE"
	// readerScaleSamplesEnv shrinks the sample count for a smoke run. The
	// manifest's warm_samples is the contract; anything lower is explicitly
	// marked as reduced in the artifact so a short run can never be mistaken
	// for a real baseline.
	readerScaleSamplesEnv = "WEBTAG_READER_PERF_SAMPLES"

	readerScaleManifestPath = "../fixtures/reader-vnext-performance/manifest.json"
	readerScaleArtifactDir  = "../../artifacts/reader-vnext-performance"

	readerScaleWarmupIterations = 10
	readerScaleWarmSamples      = 100
)

// ---------------------------------------------------------------- manifest

type readerScaleManifest struct {
	SchemaVersion           int                          `json:"schema_version"`
	FixtureID               string                       `json:"fixture_id"`
	FixtureStatus           string                       `json:"fixture_status"`
	Seed                    string                       `json:"seed"`
	ReaderJourney           string                       `json:"reader_journey"`
	PostgresPlanArtifactEnv string                       `json:"postgres_plan_artifact_env"`
	MeasurementEnv          string                       `json:"measurement_env"`
	RegenerateEnv           string                       `json:"regenerate_env"`
	SearchToken             string                       `json:"search_token"`
	WarmupIterations        int                          `json:"warmup_iterations"`
	WarmSamples             int                          `json:"warm_samples"`
	DataScale               ReaderScaleCounts            `json:"data_scale"`
	RowCounts               map[string]int               `json:"row_counts"`
	RowDigests              map[string]string            `json:"row_digests"`
	DeterministicLocks      readerScaleLocks             `json:"deterministic_locks"`
	BrowserJourney          readerScaleBrowserJourneyDoc `json:"browser_journey"`
}

type readerScaleLocks struct {
	QueryCounts          map[string]int      `json:"query_counts"`
	ResponseByteCeilings map[string]int      `json:"response_byte_ceilings"`
	PlanShapes           map[string][]string `json:"plan_shapes"`
}

// readerScaleBrowserJourneyDoc declares the API paths the browser baseline is
// required to measure. The Playwright spec reads it and fails if its own probe
// list does not cover every declared path, so dropping a path from one half of
// the baseline cannot silently leave the other half claiming full coverage.
type readerScaleBrowserJourneyDoc struct {
	Steps    []string `json:"steps"`
	APIPaths []string `json:"api_paths"`
}

func readReaderScaleManifest(t testing.TB) readerScaleManifest {
	t.Helper()
	raw, err := os.ReadFile(readerScaleManifestPath)
	if err != nil {
		t.Fatalf("read fixture manifest: %v", err)
	}
	var manifest readerScaleManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode fixture manifest: %v", err)
	}
	return manifest
}

func writeReaderScaleManifest(t testing.TB, manifest readerScaleManifest) {
	t.Helper()
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode fixture manifest: %v", err)
	}
	if err := os.WriteFile(readerScaleManifestPath, append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("write fixture manifest: %v", err)
	}
	t.Logf("regenerated %s — review the diff before committing", readerScaleManifestPath)
}

// ------------------------------------------------------------------ tracer

// readerScaleTracer records every statement pgx sends on the pool. It is the
// mechanism behind both deterministic locks that do not need a clock: the
// statement count per request, and the SQL text to hand to EXPLAIN.
//
// Recording SQL from the driver rather than copying it out of the repository
// source matters: a copied constant drifts the moment production SQL changes,
// and the resulting plan assertion would keep passing against a query nobody
// runs any more.
type readerScaleTracer struct {
	enabled bool
	entries []readerScaleStatement
}

type readerScaleStatement struct {
	SQL  string
	Args []any
}

// nonWorkStatements are the statements pgx and the harness emit around a
// request rather than as part of it: transaction control, plus the session
// parameter writes the digest helper issues. They are real round trips, but
// counting them would make the budget change whenever an unrelated helper
// gained a transaction, which would say nothing about the path's cost.
var nonWorkStatements = []string{"begin", "commit", "rollback", "savepoint", "release", "set "}

func isNonWorkStatement(sql string) bool {
	normalized := strings.ToLower(strings.TrimSpace(sql))
	for _, prefix := range nonWorkStatements {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func (tracer *readerScaleTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if tracer.enabled && !isNonWorkStatement(data.SQL) {
		tracer.entries = append(tracer.entries, readerScaleStatement{SQL: data.SQL, Args: data.Args})
	}
	return ctx
}

func (tracer *readerScaleTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tracer *readerScaleTracer) start() {
	tracer.enabled = true
	tracer.entries = nil
}

func (tracer *readerScaleTracer) stop() []readerScaleStatement {
	tracer.enabled = false
	captured := tracer.entries
	tracer.entries = nil
	return captured
}

// newReaderScaleTracedPool opens a second pool against the shared container
// with a query tracer attached. MaxConns is 1 so a path's statements arrive in
// a single, ordered stream; with a larger pool the trace would interleave and
// the per-request statement count would stop being a stable number.
func newReaderScaleTracedPool(t *testing.T, tracer *readerScaleTracer) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(DSN(t))
	if err != nil {
		t.Fatalf("parse traced pool config: %v", err)
	}
	config.MaxConns = 1
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ------------------------------------------------------------------- paths

type readerScaleDeps struct {
	repo    *repository.PGXReaderVNextRepository
	library handler.ReaderLibraryRoutes
	todos   handler.ReaderTodoRoutes
	inbox   handler.ReaderInboxRoutes
}

// readerScalePath is one measured request-equivalent. run must perform exactly
// the work the HTTP handler would and return the value that would be
// serialised, so the recorded byte count is the response size and not an
// internal projection that happens to be smaller.
type readerScalePath struct {
	name string
	// note explains what the path costs today, so a later phase reading the
	// baseline artifact knows what it is supposed to have improved.
	note string
	run  func(ctx context.Context, deps readerScaleDeps) (any, error)
	// planSelector picks the single statement whose plan decides this path's
	// cost out of everything the path executed.
	planSelector string
}

func readerScalePaths() []readerScalePath {
	return []readerScalePath{
		{
			name:         "home",
			note:         "GET /api/home: one read-only repeatable-read snapshot over the stored projection; it no longer reconciles or writes",
			planSelector: "FROM reader_thoughts t",
			run: func(ctx context.Context, deps readerScaleDeps) (any, error) {
				return deps.library.Home(ctx)
			},
		},
		{
			name:         "todos",
			note:         "GET /api/todos: one keyset page straight off reader_todos; the read no longer parses bodies or reconciles",
			planSelector: "FROM reader_todos",
			run: func(ctx context.Context, deps readerScaleDeps) (any, error) {
				return deps.todos.ListTodos(ctx, "", 30)
			},
		},
		{
			name:         "inbox_first_screen",
			note:         "GET /api/inbox: first screen of the active partition; list rows carry a bounded preview, never the body or full note",
			planSelector: "FROM reader_inbox",
			run: func(ctx context.Context, deps readerScaleDeps) (any, error) {
				return deps.inbox.ListInbox(ctx, string(model.ReaderInboxPartitionActive), "", 30)
			},
		},
		{
			name:         "thought_search",
			note:         "GET /api/annotations?q=: %term% ILIKE over body, source, quote and tombstone snapshots",
			planSelector: "ILIKE",
			run: func(ctx context.Context, deps readerScaleDeps) (any, error) {
				items, total, next, err := deps.repo.SearchThoughts(ctx, ReaderScaleSearchToken, "", 20)
				if err != nil {
					return nil, err
				}
				return map[string]any{"items": items, "total": total, "next": next}, nil
			},
		},
		{
			name: "recommended_feed",
			note: "GET /api/reader-feed: builds a fresh recommended snapshot and writes the whole candidate set as one JSONB row",
			// The snapshot INSERT is the last statement but not the interesting
			// one — its plan is a bare Result. The first candidate query is:
			// every done reading link, left joined to engagement and feedback,
			// sorted, then cut to 1000. That is what the build actually pays
			// for, and it is the first statement mentioning feed feedback.
			planSelector: "reader_feed_hides",
			run: func(ctx context.Context, deps readerScaleDeps) (any, error) {
				return deps.library.FeedWithSources(ctx, "recommended", "", nil, 30)
			},
		},
		{
			name:         "activity",
			note:         "GET /api/reader/activity: one read-only tag+domain aggregation over confirmed Reading links with keyset pagination",
			planSelector: "unnest(l.tags)",
			run: func(ctx context.Context, deps readerScaleDeps) (any, error) {
				return deps.repo.ListActivity(ctx, model.ReaderActivityQuery{
					Kind:  model.ReaderActivityKindAll,
					Limit: 100,
				})
			},
		},
	}
}

// ------------------------------------------------------------------ EXPLAIN

type readerScaleExplainNode struct {
	NodeType     string                   `json:"Node Type"`
	RelationName string                   `json:"Relation Name"`
	IndexName    string                   `json:"Index Name"`
	CTEName      string                   `json:"CTE Name"`
	Plans        []readerScaleExplainNode `json:"Plans"`
}

type readerScaleExplainRoot struct {
	Plan readerScaleExplainNode `json:"Plan"`
}

// readerScalePlanShape flattens a plan tree into a sorted, deduplicated set of
// "<node type> on <relation> using <index>" strings. Costs, row estimates and
// timings are deliberately discarded: they move with statistics and machine
// load, and locking them would produce exactly the flaky threshold gate issue
// #40 says not to build. What survives — which relations are touched and by
// which access method — is decided by the SQL text, so it is stable.
func readerScalePlanShape(raw []byte) ([]string, error) {
	var roots []readerScaleExplainRoot
	if err := json.Unmarshal(raw, &roots); err != nil {
		return nil, fmt.Errorf("decode explain json: %w", err)
	}
	shape := map[string]struct{}{}
	var walk func(node readerScaleExplainNode)
	walk = func(node readerScaleExplainNode) {
		entry := node.NodeType
		if node.RelationName != "" {
			entry += " on " + node.RelationName
		} else if node.CTEName != "" {
			entry += " on cte " + node.CTEName
		}
		if node.IndexName != "" {
			entry += " using " + node.IndexName
		}
		shape[entry] = struct{}{}
		for _, child := range node.Plans {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root.Plan)
	}
	out := make([]string, 0, len(shape))
	for entry := range shape {
		out = append(out, entry)
	}
	sort.Strings(out)
	return out, nil
}

// readerScaleExplain runs EXPLAIN against a captured statement. analyze=false
// never executes the statement, which is what the contract test needs: several
// of these statements are data-modifying CTEs and must not run twice.
//
// analyze=true does execute, so it is wrapped in a transaction that is always
// rolled back. Without that the ANALYZE pass of the TODO reconcile or the
// Activity rebuild would mutate the very fixture the next sample reads.
func readerScaleExplain(ctx context.Context, pool *pgxpool.Pool, statement readerScaleStatement, analyze bool) ([]byte, error) {
	prefix := "EXPLAIN (FORMAT JSON) "
	if analyze {
		prefix = "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin explain transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var raw []byte
	if err := tx.QueryRow(ctx, prefix+statement.SQL, statement.Args...).Scan(&raw); err != nil {
		return nil, fmt.Errorf("explain: %w", err)
	}
	return raw, nil
}

func selectReaderScaleStatement(statements []readerScaleStatement, selector string) (readerScaleStatement, error) {
	for _, statement := range statements {
		if strings.Contains(statement.SQL, selector) {
			return statement, nil
		}
	}
	return readerScaleStatement{}, fmt.Errorf("no captured statement contains %q (captured %d statements)", selector, len(statements))
}

// ------------------------------------------------------------ run helpers

// runReaderScalePathTraced executes one path with the tracer on and returns the
// serialised payload size alongside every statement it issued.
func runReaderScalePathTraced(t *testing.T, ctx context.Context, tracer *readerScaleTracer, path readerScalePath, deps readerScaleDeps) (int, []readerScaleStatement) {
	t.Helper()
	tracer.start()
	payload, err := path.run(ctx, deps)
	statements := tracer.stop()
	if err != nil {
		t.Fatalf("%s: run: %v", path.name, err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("%s: marshal payload: %v", path.name, err)
	}
	return len(encoded), statements
}

// settleReaderScalePath drives a path to its steady state. The first Home or
// Todos read inserts every TODO projection; later reads update them. Both
// branches happen to cost the same two statements per projection, but settling
// removes the need to rely on that and makes the locked number describe the
// ordinary case rather than a cold cache.
func settleReaderScalePath(t *testing.T, ctx context.Context, path readerScalePath, deps readerScaleDeps) {
	t.Helper()
	for range 2 {
		if _, err := path.run(ctx, deps); err != nil {
			t.Fatalf("%s: settle: %v", path.name, err)
		}
	}
}

func readerScaleByteCeiling(measured int) int {
	// 5% headroom plus 1 KiB, rounded up to a whole KiB. Derived from the
	// measured value so regeneration is deterministic; loose enough that a
	// harmless field rename does not fail the gate, tight enough that adding a
	// body back into a list response does.
	ceiling := measured + measured/20 + 1024
	const kib = 1024
	return ((ceiling + kib - 1) / kib) * kib
}

// --------------------------------------------------------- contract test

func TestReaderScaleFixtureContract(t *testing.T) {
	pool := StartPostgres(t)
	spec := SeedReaderScaleFixture(t, pool)

	counts := ReaderScaleRowCounts(t, pool)
	digests := ReaderScaleDigests(t, pool)

	tracer := &readerScaleTracer{}
	tracedPool := newReaderScaleTracedPool(t, tracer)
	repo := repository.NewPGXReaderVNextRepository(tracedPool)
	applications := postgresReaderApplications(t, pool, repo)
	deps := readerScaleDeps{repo: repo, library: handler.NewReaderLibraryRoutes(applications.Library), todos: handler.NewReaderTodoRoutes(applications.Todos), inbox: handler.NewReaderInboxRoutes(applications.Inbox)}
	ctx := t.Context()

	queryCounts := map[string]int{}
	responseBytes := map[string]int{}
	byteCeilings := map[string]int{}
	planShapes := map[string][]string{}

	for _, path := range readerScalePaths() {
		settleReaderScalePath(t, ctx, path, deps)

		firstBytes, statements := runReaderScalePathTraced(t, ctx, tracer, path, deps)
		secondBytes, secondStatements := runReaderScalePathTraced(t, ctx, tracer, path, deps)

		// Two consecutive identical traces is the proof that this path has a
		// steady state at all. A path that keeps changing its statement count
		// is not lockable, and saying so here is more useful than freezing
		// whichever number the first run happened to produce.
		if len(statements) != len(secondStatements) {
			t.Fatalf("%s: statement count is not stable across consecutive reads: %d then %d",
				path.name, len(statements), len(secondStatements))
		}
		// Exact equality is the wrong bar for bytes, and the reason is worth
		// stating because it looks like flakiness: every timestamp the payload
		// carries is written by NOW(), and RFC3339Nano trims trailing zeros, so
		// one timestamp can render anywhere from 20 to 30 characters. Home
		// re-stamps ~1000 TODO projections per read, which moves the serialised
		// response by a few KiB between two otherwise identical reads. A
		// proportional tolerance absorbs that; it is still two orders of
		// magnitude below any payload change worth catching.
		byteJitterTolerance := max(256, max(firstBytes, secondBytes)/50)
		if delta := firstBytes - secondBytes; delta > byteJitterTolerance || delta < -byteJitterTolerance {
			t.Fatalf("%s: response size is not stable across consecutive reads: %d then %d bytes (tolerance %d)",
				path.name, firstBytes, secondBytes, byteJitterTolerance)
		}

		queryCounts[path.name] = len(statements)
		responseBytes[path.name] = max(firstBytes, secondBytes)
		byteCeilings[path.name] = readerScaleByteCeiling(max(firstBytes, secondBytes))

		statement, err := selectReaderScaleStatement(statements, path.planSelector)
		if err != nil {
			t.Fatalf("%s: %v", path.name, err)
		}
		raw, err := readerScaleExplain(ctx, tracedPool, statement, false)
		if err != nil {
			t.Fatalf("%s: explain key statement: %v", path.name, err)
		}
		shape, err := readerScalePlanShape(raw)
		if err != nil {
			t.Fatalf("%s: %v", path.name, err)
		}
		planShapes[path.name] = shape
		t.Logf("%s: statements=%d response_bytes=%d plan=%v", path.name, len(statements), responseBytes[path.name], shape)
	}

	locks := readerScaleLocks{
		QueryCounts:          queryCounts,
		ResponseByteCeilings: byteCeilings,
		PlanShapes:           planShapes,
	}

	if os.Getenv(readerScaleWriteManifestEnv) == "1" {
		writeReaderScaleManifest(t, readerScaleManifest{
			SchemaVersion:           2,
			FixtureID:               ReaderScaleFixtureID,
			FixtureStatus:           "frozen",
			Seed:                    ReaderScaleSeed,
			ReaderJourney:           "reader-vnext-performance",
			PostgresPlanArtifactEnv: readerScalePlanFileEnv,
			MeasurementEnv:          readerScaleMeasureEnv,
			RegenerateEnv:           readerScaleWriteManifestEnv,
			SearchToken:             ReaderScaleSearchToken,
			WarmupIterations:        readerScaleWarmupIterations,
			WarmSamples:             readerScaleWarmSamples,
			DataScale:               spec.Counts,
			RowCounts:               counts,
			RowDigests:              digests,
			DeterministicLocks:      locks,
			BrowserJourney: readerScaleBrowserJourneyDoc{
				Steps:    []string{"home", "todos", "inbox", "thought_search", "recommended_feed", "activity"},
				APIPaths: []string{"/api/home", "/api/todos", "/api/inbox", "/api/annotations", "/api/reader-feed", "/api/reader/activity"},
			},
		})
		return
	}

	manifest := readReaderScaleManifest(t)
	if manifest.FixtureID != ReaderScaleFixtureID || manifest.Seed != ReaderScaleSeed {
		t.Fatalf("manifest identity = %s/%s, want %s/%s", manifest.FixtureID, manifest.Seed, ReaderScaleFixtureID, ReaderScaleSeed)
	}
	// The browser spec builds its search probe from the manifest token, not from
	// the Go constant. Without this check a hand-edited manifest would send the
	// browser looking for a term the fixture never seeded, and the browser
	// baseline would quietly measure an empty result set.
	if manifest.SearchToken != ReaderScaleSearchToken {
		t.Fatalf("manifest search_token = %q, want %q", manifest.SearchToken, ReaderScaleSearchToken)
	}
	if manifest.DataScale != spec.Counts {
		t.Fatalf("manifest data_scale = %+v, want %+v", manifest.DataScale, spec.Counts)
	}
	assertReaderScaleIntMap(t, "row_counts", manifest.RowCounts, counts)
	assertReaderScaleStringMap(t, "row_digests", manifest.RowDigests, digests)
	assertReaderScaleIntMap(t, "query_counts", manifest.DeterministicLocks.QueryCounts, queryCounts)

	// The ceiling is compared against the measured response, not against a
	// freshly derived ceiling. Comparing two derived ceilings would re-import
	// the timestamp jitter the tolerance above just excluded, and it would also
	// fail a payload that legitimately got *smaller* — which is the direction
	// phases 1 and 2 are supposed to move.
	for name, ceiling := range manifest.DeterministicLocks.ResponseByteCeilings {
		measured, ok := responseBytes[name]
		if !ok {
			t.Errorf("response_byte_ceilings: manifest records %q but no such path ran", name)
			continue
		}
		if measured > ceiling {
			t.Errorf("%s: response is %d bytes, above the recorded ceiling of %d", name, measured, ceiling)
		}
	}
	for name, recorded := range manifest.DeterministicLocks.PlanShapes {
		observed, ok := planShapes[name]
		if !ok {
			t.Errorf("plan_shapes: manifest records %q but no such path ran", name)
			continue
		}
		if !slices.Equal(recorded, observed) {
			t.Errorf("%s: plan shape changed\n  recorded: %v\n  observed: %v\nRegenerate with %s=1 only when the change is intended.",
				name, recorded, observed, readerScaleWriteManifestEnv)
		}
	}
}

func assertReaderScaleIntMap(t *testing.T, label string, recorded, observed map[string]int) {
	t.Helper()
	if len(recorded) != len(observed) {
		t.Errorf("%s: recorded %d entries, observed %d", label, len(recorded), len(observed))
	}
	keys := make([]string, 0, len(observed))
	for key := range observed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if recorded[key] != observed[key] {
			t.Errorf("%s[%s] = %d, recorded %d", label, key, observed[key], recorded[key])
		}
	}
}

func assertReaderScaleStringMap(t *testing.T, label string, recorded, observed map[string]string) {
	t.Helper()
	if len(recorded) != len(observed) {
		t.Errorf("%s: recorded %d entries, observed %d", label, len(recorded), len(observed))
	}
	keys := make([]string, 0, len(observed))
	for key := range observed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if recorded[key] != observed[key] {
			t.Errorf("%s[%s] = %s, recorded %s — the fixture is no longer reproducible", label, key, observed[key], recorded[key])
		}
	}
}

// ------------------------------------------------------ measurement test

type readerScaleSampleReport struct {
	Path          string   `json:"path"`
	Note          string   `json:"note"`
	Statements    int      `json:"statements"`
	ResponseBytes int      `json:"response_bytes"`
	Warmup        int      `json:"warmup_iterations"`
	Samples       int      `json:"warm_samples"`
	P50Micros     float64  `json:"p50_us"`
	P95Micros     float64  `json:"p95_us"`
	MinMicros     float64  `json:"min_us"`
	MaxMicros     float64  `json:"max_us"`
	KeySQL        string   `json:"key_sql"`
	PlanShape     []string `json:"plan_shape"`
	PlanJSON      any      `json:"explain_analyze_buffers"`
	PlanError     string   `json:"explain_error,omitempty"`
}

type readerScaleBaselineArtifact struct {
	SchemaVersion   int                       `json:"schema_version"`
	GeneratedAt     string                    `json:"generated_at"`
	FixtureID       string                    `json:"fixture_id"`
	Seed            string                    `json:"seed"`
	Reduced         bool                      `json:"reduced_run"`
	Environment     map[string]string         `json:"environment"`
	DataScale       ReaderScaleCounts         `json:"data_scale"`
	RowCounts       map[string]int            `json:"row_counts"`
	FixtureBuildSec float64                   `json:"fixture_build_seconds"`
	Paths           []readerScaleSampleReport `json:"paths"`
}

func readerScalePercentile(sorted []time.Duration, fraction float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted))*fraction+0.5) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func micros(duration time.Duration) float64 {
	return float64(duration.Nanoseconds()) / 1000
}

func TestReaderScaleHotPathMeasurements(t *testing.T) {
	if os.Getenv(readerScaleMeasureEnv) != "1" {
		t.Skipf("set %s=1 to run the Reader hot-path baseline measurement", readerScaleMeasureEnv)
	}

	samples := readerScaleWarmSamples
	reduced := false
	if raw := strings.TrimSpace(os.Getenv(readerScaleSamplesEnv)); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("%s=%q is not a positive integer", readerScaleSamplesEnv, raw)
		}
		samples = parsed
		reduced = samples < readerScaleWarmSamples
	}

	pool := StartPostgres(t)
	buildStarted := time.Now()
	spec := SeedReaderScaleFixture(t, pool)
	buildSeconds := time.Since(buildStarted).Seconds()
	counts := ReaderScaleRowCounts(t, pool)

	tracer := &readerScaleTracer{}
	tracedPool := newReaderScaleTracedPool(t, tracer)
	repo := repository.NewPGXReaderVNextRepository(tracedPool)
	applications := postgresReaderApplications(t, pool, repo)
	deps := readerScaleDeps{repo: repo, library: handler.NewReaderLibraryRoutes(applications.Library), todos: handler.NewReaderTodoRoutes(applications.Todos), inbox: handler.NewReaderInboxRoutes(applications.Inbox)}
	ctx := t.Context()

	var version, sharedBuffers, blockSize string
	if err := tracedPool.QueryRow(ctx,
		`SELECT version(), current_setting('shared_buffers'), current_setting('block_size')`,
	).Scan(&version, &sharedBuffers, &blockSize); err != nil {
		t.Fatalf("read PostgreSQL environment: %v", err)
	}

	reports := make([]readerScaleSampleReport, 0, 6)
	for _, path := range readerScalePaths() {
		settleReaderScalePath(t, ctx, path, deps)
		responseBytes, statements := runReaderScalePathTraced(t, ctx, tracer, path, deps)

		for range readerScaleWarmupIterations {
			if _, err := path.run(ctx, deps); err != nil {
				t.Fatalf("%s: warmup: %v", path.name, err)
			}
		}
		durations := make([]time.Duration, 0, samples)
		for range samples {
			started := time.Now()
			if _, err := path.run(ctx, deps); err != nil {
				t.Fatalf("%s: sample: %v", path.name, err)
			}
			durations = append(durations, time.Since(started))
		}
		slices.Sort(durations)

		report := readerScaleSampleReport{
			Path:          path.name,
			Note:          path.note,
			Statements:    len(statements),
			ResponseBytes: responseBytes,
			Warmup:        readerScaleWarmupIterations,
			Samples:       samples,
			P50Micros:     micros(readerScalePercentile(durations, 0.50)),
			P95Micros:     micros(readerScalePercentile(durations, 0.95)),
			MinMicros:     micros(durations[0]),
			MaxMicros:     micros(durations[len(durations)-1]),
		}

		if statement, err := selectReaderScaleStatement(statements, path.planSelector); err != nil {
			report.PlanError = err.Error()
		} else {
			report.KeySQL = statement.SQL
			raw, err := readerScaleExplain(ctx, tracedPool, statement, true)
			if err != nil {
				report.PlanError = err.Error()
			} else {
				if shape, shapeErr := readerScalePlanShape(raw); shapeErr == nil {
					report.PlanShape = shape
				}
				var plan any
				if json.Unmarshal(raw, &plan) == nil {
					report.PlanJSON = plan
				}
			}
		}

		t.Logf("READER_PERF path=%s statements=%d response_bytes=%d p50_us=%.1f p95_us=%.1f plan=%v",
			report.Path, report.Statements, report.ResponseBytes, report.P50Micros, report.P95Micros, report.PlanShape)
		reports = append(reports, report)
	}

	artifact := readerScaleBaselineArtifact{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		FixtureID:     ReaderScaleFixtureID,
		Seed:          ReaderScaleSeed,
		Reduced:       reduced,
		Environment: map[string]string{
			"postgres":       version,
			"shared_buffers": sharedBuffers,
			"block_size":     blockSize,
			"go":             runtime.Version(),
			"goos":           runtime.GOOS,
			"goarch":         runtime.GOARCH,
		},
		DataScale:       spec.Counts,
		RowCounts:       counts,
		FixtureBuildSec: buildSeconds,
		Paths:           reports,
	}
	writeReaderScaleArtifact(t, artifact)
}

func writeReaderScaleArtifact(t *testing.T, artifact readerScaleBaselineArtifact) {
	t.Helper()
	target := strings.TrimSpace(os.Getenv(readerScalePlanFileEnv))
	if target == "" {
		target = filepath.Join(readerScaleArtifactDir, "postgres-baseline.json")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("create artifact directory: %v", err)
	}
	encoded, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		t.Fatalf("encode baseline artifact: %v", err)
	}
	if err := os.WriteFile(target, append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("write baseline artifact: %v", err)
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		absolute = target
	}
	t.Logf("READER_PERF artifact written to %s", absolute)
}
