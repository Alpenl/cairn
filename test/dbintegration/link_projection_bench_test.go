package dbintegration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

const (
	rf9MeasurementEnv = "WEBTAG_LINK_PROJECTION_MEASURE"

	// rf9BaselineColumns freezes the pre-RF9 point-lookup payload. Keep it
	// independent from production constants: the benchmark must remain a
	// historical baseline even after the legacy full-row methods are retired.
	rf9BaselineColumns = "id, url, source_kind, source_key, input_title, input_text, input_html, input_images, source_metadata, title, summary, tags, fetcher_type, is_low_confidence, low_confidence_reason, status, error_msg, description, domain, content_type, library_kind, library_kind_source, library_kind_locked, predicted_library_kind, classification_confidence, classification_reason, classification_explanation, classifier_version, content_revision, content_source, has_content, content_cjk_chars, content_words, first_collected_at, last_recollected_at, payload_purge_due_at, payload_purged_at, path_depth, parent_path, parent_id, created_at, updated_at"
	rf9DetailColumns   = "id, url, title, summary, tags, fetcher_type, is_low_confidence, low_confidence_reason, status, error_msg, description, domain, content_type, library_kind, library_kind_source, library_kind_locked, predicted_library_kind, classification_confidence, classification_reason, classification_explanation, classifier_version, content_revision, content_source, has_content, content_cjk_chars, content_words, path_depth, parent_path, parent_id, created_at, updated_at"
	rf9ParseColumns    = "id, url, source_kind, source_key, input_title, input_text, input_html, input_images, source_metadata, description, status, library_kind, library_kind_locked, content_revision, updated_at"
	rf9LifecycleCols   = "id, url, status, library_kind, library_kind_source, library_kind_locked, classification_reason, content_revision, has_content"
	rf9SubmitColumns   = "id, url, source_key, status, library_kind"

	rf9WarmupReads     = 25
	rf9WarmIterations  = 200
	rf9ColdWarmupReads = 2
	rf9ColdIterations  = 100
)

var rf9ProjectionSink uuid.UUID

type rf9ProjectionSpec struct {
	name    string
	columns string
}

var rf9ProjectionSpecs = []rf9ProjectionSpec{
	{name: "baseline", columns: rf9BaselineColumns},
	{name: "detail", columns: rf9DetailColumns},
	{name: "parse", columns: rf9ParseColumns},
	{name: "lifecycle", columns: rf9LifecycleCols},
	{name: "submit", columns: rf9SubmitColumns},
}

type rf9Fixture struct {
	name string
	id   uuid.UUID
}

type rf9FixturePayload struct {
	inputTitle     string
	inputText      string
	inputHTML      string
	inputImages    []string
	metadata       map[string]any
	title          string
	summary        string
	tags           []string
	description    string
	explanation    string
	classification string
}

func TestLinkProjectionFixtureContract(t *testing.T) {
	pool := StartPostgres(t)
	fixtures := seedRF9ProjectionFixtures(t, pool)
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			var baselineBytes int64
			for _, spec := range rf9ProjectionSpecs {
				if err := readRF9Projection(ctx, repo, spec.name, fixture.id); err != nil {
					t.Fatalf("read %s projection: %v", spec.name, err)
				}
				rowBytes, encodedBytes := measureRF9ServerBytes(t, pool, fixture.id, spec.columns)
				t.Logf("fixture=%s projection=%s row_bytes=%d encoded_bytes=%d", fixture.name, spec.name, rowBytes, encodedBytes)
				if rowBytes <= 0 || encodedBytes <= 0 {
					t.Fatalf("%s byte measurements = %d/%d, want positive", spec.name, rowBytes, encodedBytes)
				}
				if spec.name == "baseline" {
					baselineBytes = rowBytes
					continue
				}
				if rowBytes >= baselineBytes {
					t.Fatalf("%s row bytes = %d, want below frozen baseline %d", spec.name, rowBytes, baselineBytes)
				}
			}
		})
	}
}

// TestLinkProjectionMeasurements is opt-in because its two cache-eviction
// relations are each larger than shared_buffers. Normal dbintegration still
// executes TestLinkProjectionFixtureContract; this test is the reproducible
// BenchmarkLinkProjection provides reproducible narrow and full projection measurements.
func TestLinkProjectionMeasurements(t *testing.T) {
	if os.Getenv(rf9MeasurementEnv) != "1" {
		t.Skipf("set %s=1 to run cache and latency measurements", rf9MeasurementEnv)
	}

	pool := StartPostgres(t)
	fixtures := seedRF9ProjectionFixtures(t, pool)
	ctx := t.Context()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire measurement connection: %v", err)
	}
	t.Cleanup(conn.Release)
	repo := repository.NewPGXLinkRepository(conn)

	var version, sharedBuffers, blockSize string
	if err := conn.QueryRow(ctx, `SELECT version(), current_setting('shared_buffers'), current_setting('block_size')`).Scan(&version, &sharedBuffers, &blockSize); err != nil {
		t.Fatalf("read PostgreSQL environment: %v", err)
	}
	t.Logf("RF9_ENV postgres=%q shared_buffers=%s block_size=%s warmup=%d warm_iterations=%d cold_warmup=%d cold_iterations=%d concurrency=1",
		version, sharedBuffers, blockSize, rf9WarmupReads, rf9WarmIterations, rf9ColdWarmupReads, rf9ColdIterations)

	evictor := newRF9SharedBufferEvictor(t, conn)
	for _, fixture := range fixtures {
		for _, spec := range rf9ProjectionSpecs {
			rowBytes, encodedBytes := measureRF9ServerBytes(t, pool, fixture.id, spec.columns)

			warmDurations := measureRF9WarmLatency(t, ctx, repo, spec.name, fixture.id)
			warmIO := measureRF9IO(t, ctx, conn, repo, spec.name, fixture.id, nil)

			for range rf9ColdWarmupReads {
				if err := evictor.evict(ctx); err != nil {
					t.Fatalf("cold warmup evict %s/%s: %v", fixture.name, spec.name, err)
				}
				if err := readRF9Projection(ctx, repo, spec.name, fixture.id); err != nil {
					t.Fatalf("cold warmup read %s/%s: %v", fixture.name, spec.name, err)
				}
			}
			coldDurations := make([]time.Duration, 0, rf9ColdIterations)
			for range rf9ColdIterations {
				if err := evictor.evict(ctx); err != nil {
					t.Fatalf("cold evict %s/%s: %v", fixture.name, spec.name, err)
				}
				started := time.Now()
				if err := readRF9Projection(ctx, repo, spec.name, fixture.id); err != nil {
					t.Fatalf("cold read %s/%s: %v", fixture.name, spec.name, err)
				}
				coldDurations = append(coldDurations, time.Since(started))
			}
			coldIO := measureRF9IO(t, ctx, conn, repo, spec.name, fixture.id, evictor.evict)

			warmP50, warmP95 := rf9Percentiles(warmDurations)
			coldP50, coldP95 := rf9Percentiles(coldDurations)
			t.Logf("RF9_RESULT fixture=%s projection=%s row_bytes=%d encoded_bytes=%d warm_p50_us=%.3f warm_p95_us=%.3f cold_p50_us=%.3f cold_p95_us=%.3f",
				fixture.name, spec.name, rowBytes, encodedBytes,
				float64(warmP50.Microseconds())+float64(warmP50.Nanoseconds()%int64(time.Microsecond))/1e3,
				float64(warmP95.Microseconds())+float64(warmP95.Nanoseconds()%int64(time.Microsecond))/1e3,
				float64(coldP50.Microseconds())+float64(coldP50.Nanoseconds()%int64(time.Microsecond))/1e3,
				float64(coldP95.Microseconds())+float64(coldP95.Nanoseconds()%int64(time.Microsecond))/1e3)
			t.Logf("RF9_IO fixture=%s projection=%s cache=warm heap_read=%d heap_hit=%d index_read=%d index_hit=%d toast_read=%d toast_hit=%d",
				fixture.name, spec.name, warmIO.heapRead, warmIO.heapHit, warmIO.indexRead, warmIO.indexHit, warmIO.toastRead, warmIO.toastHit)
			t.Logf("RF9_IO fixture=%s projection=%s cache=cold heap_read=%d heap_hit=%d index_read=%d index_hit=%d toast_read=%d toast_hit=%d",
				fixture.name, spec.name, coldIO.heapRead, coldIO.heapHit, coldIO.indexRead, coldIO.indexHit, coldIO.toastRead, coldIO.toastHit)
		}
	}
}

func BenchmarkLinkProjectionReads(b *testing.B) {
	pool := StartPostgres(b)
	fixtures := seedRF9ProjectionFixtures(b, pool)
	ctx := b.Context()
	repo := repository.NewPGXLinkRepository(pool)

	for _, fixture := range fixtures {
		for _, spec := range rf9ProjectionSpecs {
			b.Run(fixture.name+"/"+spec.name, func(b *testing.B) {
				for range rf9WarmupReads {
					if err := readRF9Projection(ctx, repo, spec.name, fixture.id); err != nil {
						b.Fatalf("warmup read: %v", err)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if err := readRF9Projection(ctx, repo, spec.name, fixture.id); err != nil {
						b.Fatalf("benchmark read: %v", err)
					}
				}
			})
		}
	}
}

func seedRF9ProjectionFixtures(t testing.TB, pool *pgxpool.Pool) []rf9Fixture {
	t.Helper()
	repo := repository.NewPGXLinkRepository(pool)
	ctx := context.Background()
	fixtures := []struct {
		name    string
		payload rf9FixturePayload
	}{
		{name: "light", payload: lightRF9Payload()},
		{name: "heavy", payload: heavyRF9Payload()},
	}
	seeded := make([]rf9Fixture, 0, len(fixtures))
	for _, fixture := range fixtures {
		payload := fixture.payload
		rawURL := "https://rf9.example/" + fixture.name
		domain := "rf9.example"
		contentType := "article"
		pathDepth := 1
		parentPath := "/"
		predicted := model.LibraryKindReading
		link, err := repo.Create(ctx, repository.CreateLinkParams{
			URL:                  rawURL,
			SourceKind:           "browser_capture",
			SourceKey:            "rf9:" + fixture.name,
			InputTitle:           &payload.inputTitle,
			InputText:            &payload.inputText,
			InputHTML:            &payload.inputHTML,
			InputImages:          payload.inputImages,
			SourceMetadata:       payload.metadata,
			Description:          &payload.description,
			Status:               model.LinkStatusDone,
			Domain:               &domain,
			ContentType:          &contentType,
			PathDepth:            &pathDepth,
			ParentPath:           &parentPath,
			RequestedLibraryKind: model.RequestedLibraryKindReading,
			PredictedLibraryKind: &predicted,
		})
		if err != nil {
			t.Fatalf("seed %s link: %v", fixture.name, err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE links
			SET title = $2,
			    summary = $3,
			    tags = $4,
			    fetcher_type = 'browser_capture',
			    is_low_confidence = true,
			    low_confidence_reason = 'controlled_fixture',
			    classification_confidence = 0.91,
			    classification_reason = $5,
			    classification_explanation = $6,
			    classifier_version = 'rf9-fixture-v1',
			    content_revision = 7,
			    content_source = 'user',
			    content_cjk_chars = 321,
			    content_words = 654,
			    first_collected_at = TIMESTAMPTZ '2026-08-08 07:00:00+00',
			    last_recollected_at = TIMESTAMPTZ '2026-08-08 08:00:00+00',
			    payload_purge_due_at = TIMESTAMPTZ '2026-08-09 08:00:00+00',
			    created_at = TIMESTAMPTZ '2026-08-08 07:00:00+00',
			    updated_at = TIMESTAMPTZ '2026-08-08 08:00:00+00'
			WHERE id = $1`,
			link.ID, payload.title, payload.summary, payload.tags, payload.classification,
			payload.explanation,
		); err != nil {
			t.Fatalf("enrich %s link: %v", fixture.name, err)
		}
		seeded = append(seeded, rf9Fixture{name: fixture.name, id: link.ID})
	}
	return seeded
}

func lightRF9Payload() rf9FixturePayload {
	return rf9FixturePayload{
		inputTitle:     "Light capture",
		inputText:      "A short captured paragraph.",
		inputHTML:      "<p>A short captured paragraph.</p>",
		inputImages:    []string{"https://rf9.example/light.png"},
		metadata:       map[string]any{"parse_depth": "light"},
		title:          "Light article",
		summary:        "Short summary.",
		tags:           []string{"benchmark", "light"},
		description:    "Short note.",
		explanation:    "Controlled light fixture.",
		classification: "content_article",
	}
}

func heavyRF9Payload() rf9FixturePayload {
	metadata := make(map[string]any, 64)
	for i := range 64 {
		metadata[fmt.Sprintf("key_%02d", i)] = deterministicRF9Text(4096, fmt.Sprintf("metadata-%02d", i))
	}
	images := make([]string, 100)
	for i := range images {
		images[i] = fmt.Sprintf("https://assets.rf9.example/%03d.png?capture=%s", i, deterministicRF9Text(2000, fmt.Sprintf("image-%03d", i)))
	}
	tags := make([]string, 20)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag-%02d-%s", i, deterministicRF9Text(48, fmt.Sprintf("tag-%02d", i)))
	}
	return rf9FixturePayload{
		inputTitle:     deterministicRF9Text(512, "input-title"),
		inputText:      deterministicRF9Text(512*1024, "input-text"),
		inputHTML:      deterministicRF9Text(512*1024, "input-html"),
		inputImages:    images,
		metadata:       metadata,
		title:          deterministicRF9Text(512, "title"),
		summary:        deterministicRF9Text(4096, "summary"),
		tags:           tags,
		description:    deterministicRF9Text(4096, "description"),
		explanation:    deterministicRF9Text(4096, "explanation"),
		classification: deterministicRF9Text(256, "classification"),
	}
}

func deterministicRF9Text(size int, seed string) string {
	var out strings.Builder
	out.Grow(size)
	for block := 0; out.Len() < size; block++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%08d", seed, block)))
		out.WriteString(hex.EncodeToString(sum[:]))
	}
	return out.String()[:size]
}

func readRF9Projection(ctx context.Context, repo *repository.PGXLinkRepository, name string, id uuid.UUID) error {
	switch name {
	case "baseline":
		value, err := repo.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if value == nil {
			return repository.ErrNotFound
		}
		rf9ProjectionSink = value.ID
	case "detail":
		value, err := repo.GetDetailByID(ctx, id)
		if err != nil {
			return err
		}
		if value == nil {
			return repository.ErrNotFound
		}
		rf9ProjectionSink = value.ID
	case "parse":
		value, err := repo.GetParseInputByID(ctx, id)
		if err != nil {
			return err
		}
		if value == nil {
			return repository.ErrNotFound
		}
		rf9ProjectionSink = value.ID
	case "lifecycle":
		value, err := repo.GetLifecycleByID(ctx, id)
		if err != nil {
			return err
		}
		if value == nil {
			return repository.ErrNotFound
		}
		rf9ProjectionSink = value.ID
	case "submit":
		value, err := repo.GetSubmitLookupByID(ctx, id)
		if err != nil {
			return err
		}
		if value == nil {
			return repository.ErrNotFound
		}
		rf9ProjectionSink = value.ID
	default:
		return fmt.Errorf("unknown RF9 projection %q", name)
	}
	return nil
}

func measureRF9ServerBytes(t testing.TB, pool *pgxpool.Pool, id uuid.UUID, columns string) (int64, int64) {
	t.Helper()
	query := fmt.Sprintf(`SELECT pg_column_size(ROW(%[1]s)), octet_length(row_to_json(ROW(%[1]s))::text) FROM links WHERE id = $1`, columns)
	var rowBytes, encodedBytes int64
	if err := pool.QueryRow(context.Background(), query, id).Scan(&rowBytes, &encodedBytes); err != nil {
		t.Fatalf("measure server bytes: %v", err)
	}
	return rowBytes, encodedBytes
}

func measureRF9WarmLatency(t testing.TB, ctx context.Context, repo *repository.PGXLinkRepository, projection string, id uuid.UUID) []time.Duration {
	t.Helper()
	for range rf9WarmupReads {
		if err := readRF9Projection(ctx, repo, projection, id); err != nil {
			t.Fatalf("warmup %s projection: %v", projection, err)
		}
	}
	durations := make([]time.Duration, 0, rf9WarmIterations)
	for range rf9WarmIterations {
		started := time.Now()
		if err := readRF9Projection(ctx, repo, projection, id); err != nil {
			t.Fatalf("measure warm %s projection: %v", projection, err)
		}
		durations = append(durations, time.Since(started))
	}
	return durations
}

func rf9Percentiles(samples []time.Duration) (time.Duration, time.Duration) {
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	percentile := func(p float64) time.Duration {
		index := int(math.Ceil(p*float64(len(ordered)))) - 1
		if index < 0 {
			index = 0
		}
		return ordered[index]
	}
	return percentile(0.50), percentile(0.95)
}

type rf9IOStats struct {
	heapRead  int64
	heapHit   int64
	indexRead int64
	indexHit  int64
	toastRead int64
	toastHit  int64
}

func measureRF9IO(
	t testing.TB,
	ctx context.Context,
	conn *pgxpool.Conn,
	repo *repository.PGXLinkRepository,
	projection string,
	id uuid.UUID,
	prepare func(context.Context) error,
) rf9IOStats {
	t.Helper()
	if prepare != nil {
		if err := prepare(ctx); err != nil {
			t.Fatalf("prepare IO measurement: %v", err)
		}
	}
	before := readRF9IOStats(t, ctx, conn)
	if err := readRF9Projection(ctx, repo, projection, id); err != nil {
		t.Fatalf("read %s projection for IO: %v", projection, err)
	}
	after := readRF9IOStats(t, ctx, conn)
	return rf9IOStats{
		heapRead:  after.heapRead - before.heapRead,
		heapHit:   after.heapHit - before.heapHit,
		indexRead: after.indexRead - before.indexRead,
		indexHit:  after.indexHit - before.indexHit,
		toastRead: after.toastRead - before.toastRead,
		toastHit:  after.toastHit - before.toastHit,
	}
}

func readRF9IOStats(t testing.TB, ctx context.Context, conn *pgxpool.Conn) rf9IOStats {
	t.Helper()
	if _, err := conn.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("flush links IO counters: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
		t.Fatalf("clear statistics snapshot: %v", err)
	}
	var stats rf9IOStats
	if err := conn.QueryRow(ctx, `
		SELECT heap_blks_read, heap_blks_hit, idx_blks_read, idx_blks_hit,
		       COALESCE(toast_blks_read, 0), COALESCE(toast_blks_hit, 0)
		FROM pg_statio_user_tables
		WHERE relname = 'links'`,
	).Scan(&stats.heapRead, &stats.heapHit, &stats.indexRead, &stats.indexHit, &stats.toastRead, &stats.toastHit); err != nil {
		t.Fatalf("read links IO counters: %v", err)
	}
	return stats
}

type rf9SharedBufferEvictor struct {
	conn   *pgxpool.Conn
	tables [2]string
	next   int
}

func newRF9SharedBufferEvictor(t testing.TB, conn *pgxpool.Conn) *rf9SharedBufferEvictor {
	t.Helper()
	ctx := context.Background()
	for _, extension := range []string{"pg_buffercache", "pg_prewarm"} {
		if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+extension); err != nil {
			t.Fatalf("create %s extension: %v", extension, err)
		}
	}
	var sharedBytes int64
	if err := conn.QueryRow(ctx, `SELECT pg_size_bytes(current_setting('shared_buffers'))`).Scan(&sharedBytes); err != nil {
		t.Fatalf("read shared_buffers bytes: %v", err)
	}
	const payloadBytes = 3200
	rows := (sharedBytes*5/4 + payloadBytes - 1) / payloadBytes
	evictor := &rf9SharedBufferEvictor{
		conn:   conn,
		tables: [2]string{"rf9_buffer_evict_a", "rf9_buffer_evict_b"},
	}
	for index, table := range evictor.tables {
		if _, err := conn.Exec(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Fatalf("drop stale %s: %v", table, err)
		}
		if _, err := conn.Exec(ctx, "CREATE UNLOGGED TABLE "+table+" (id bigint PRIMARY KEY, payload bytea)"); err != nil {
			t.Fatalf("create %s: %v", table, err)
		}
		if _, err := conn.Exec(ctx, "ALTER TABLE "+table+" ALTER COLUMN payload SET STORAGE PLAIN"); err != nil {
			t.Fatalf("set %s payload storage: %v", table, err)
		}
		seed := fmt.Sprintf("rf9-evict-%d-", index)
		if _, err := conn.Exec(ctx,
			"INSERT INTO "+table+" (id, payload) SELECT g, decode(repeat(md5($1 || g::text), 200), 'hex') FROM generate_series(1, $2) AS g",
			seed, rows,
		); err != nil {
			t.Fatalf("populate %s: %v", table, err)
		}
		var relationBytes int64
		if err := conn.QueryRow(ctx, "SELECT pg_relation_size($1::regclass)", table).Scan(&relationBytes); err != nil {
			t.Fatalf("measure %s: %v", table, err)
		}
		if relationBytes <= sharedBytes {
			t.Fatalf("%s size=%d must exceed shared_buffers=%d", table, relationBytes, sharedBytes)
		}
	}
	t.Cleanup(func() {
		for _, table := range evictor.tables {
			_, _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
		}
	})
	return evictor
}

func (e *rf9SharedBufferEvictor) evict(ctx context.Context) error {
	for attempt := 0; attempt < 4; attempt++ {
		table := e.tables[e.next%len(e.tables)]
		e.next++
		if _, err := e.conn.Exec(ctx, "SELECT pg_prewarm($1::regclass, 'buffer')", table); err != nil {
			return fmt.Errorf("prewarm %s: %w", table, err)
		}
		resident, err := e.residentLinkBuffers(ctx)
		if err != nil {
			return err
		}
		if resident == 0 {
			return nil
		}
	}
	resident, err := e.residentLinkBuffers(ctx)
	if err != nil {
		return err
	}
	return fmt.Errorf("links heap/index/TOAST still has %d shared buffers after eviction", resident)
}

func (e *rf9SharedBufferEvictor) residentLinkBuffers(ctx context.Context) (int64, error) {
	var count int64
	err := e.conn.QueryRow(ctx, `
		WITH link_relations AS (
			SELECT 'links'::regclass::oid AS oid
			UNION
			SELECT reltoastrelid FROM pg_class WHERE oid = 'links'::regclass
			UNION
			SELECT indexrelid FROM pg_index WHERE indrelid = 'links'::regclass
			UNION
			SELECT indexrelid
			FROM pg_index
			WHERE indrelid = (SELECT reltoastrelid FROM pg_class WHERE oid = 'links'::regclass)
		), relation_files AS (
			SELECT pg_relation_filenode(oid) AS relfilenode FROM link_relations WHERE oid <> 0
		)
		SELECT count(*)
		FROM pg_buffercache AS buffers
		JOIN relation_files USING (relfilenode)
		WHERE buffers.reldatabase IN (0, (SELECT oid FROM pg_database WHERE datname = current_database()))`,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count resident link buffers: %w", err)
	}
	return count, nil
}
