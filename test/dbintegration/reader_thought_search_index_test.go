package dbintegration

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/repository"
)

// TestReaderThoughtSearchPostgresSubstringContract freezes the *literal* match
// semantics of thought search before any index work can quietly turn it into a
// lexeme search.
//
// The existing snapshot/cursor contract test already pins source, quote,
// tombstone authority and pagination, but every one of its probes is a whole
// stored value. That leaves the parts a tsvector rewrite would break completely
// unguarded: a match that starts in the middle of a word, a CJK fragment that no
// tokenizer would ever emit as a lexeme, case folding, and the fact that a user
// typed `%` / `_` reaches PostgreSQL as a LIKE wildcard today.
//
// Every case here holds for the pre-trigram query too — that is the point. If a
// later change makes any of them fail, search results changed for users.
func TestReaderThoughtSearchPostgresSubstringContract(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	updatedAt := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	snapshot := func(body, source, quote string) map[string]any {
		return map[string]any{
			"body":       body,
			"source":     source,
			"quote":      map[string]string{"exact": quote},
			"updated_at": updatedAt.Format(time.RFC3339Nano),
		}
	}

	// Active projections. Mid-word, mixed case and CJK on purpose.
	seedReaderThoughtSearchFixture(t, pool, "active-infix", "leading noise ZWISCHENraum trailing noise", "user", "unrelated active quote", false, updatedAt, nil)
	seedReaderThoughtSearchFixture(t, pool, "active-cjk", "前缀内容 罕见词元集合 后缀内容", "user", "无关引文", false, updatedAt, nil)
	seedReaderThoughtSearchFixture(t, pool, "active-source-infix", "unrelated active body", "wechat-Article-Importer-7", "unrelated source quote", false, updatedAt, nil)
	seedReaderThoughtSearchFixture(t, pool, "active-quote-infix", "unrelated quoted body", "user", "被引用的原文片段", false, updatedAt, nil)
	seedReaderThoughtSearchFixture(t, pool, "active-wildcard", "wild card token", "user", "unrelated wildcard quote", false, updatedAt, nil)

	// Tombstone projections. The mutable row must never be what matched, so its
	// live columns carry sentinels that no probe below can hit.
	seedReaderThoughtSearchFixture(t, pool, "tombstone-infix", "mutable infix body", "mutable-infix-source", "mutable infix quote", false, updatedAt,
		snapshot("frozen noise NACHBARschaft frozen tail", "frozen-source", "frozen quote"))
	seedReaderThoughtSearchFixture(t, pool, "tombstone-cjk", "mutable cjk body", "mutable-cjk-source", "mutable cjk quote", false, updatedAt,
		snapshot("冻结前缀 稀有短语集合 冻结后缀", "frozen-cjk-source", "冻结引文"))
	seedReaderThoughtSearchFixture(t, pool, "tombstone-source-infix", "mutable tombstone source body", "mutable-source-marker", "mutable tombstone source quote", false, updatedAt,
		snapshot("frozen tombstone source body", "telegram-Channel-Importer-9", "frozen tombstone source quote"))
	seedReaderThoughtSearchFixture(t, pool, "tombstone-quote-infix", "mutable tombstone quote body", "mutable-quote-marker", "mutable tombstone quote", false, updatedAt,
		snapshot("frozen tombstone quote body", "frozen-quote-source", "被冻结的引文片段"))

	for _, probe := range []struct {
		name   string
		query  string
		wantID string
	}{
		// A match that begins inside a word. No lexeme index can serve this.
		{"active body infix", "zwischenr", "active-infix"},
		// Trimming plus case folding, frozen together with the infix.
		{"active body infix is case folded and trimmed", "  ZWISCHENRAUM  ", "active-infix"},
		// Two CJK characters lifted out of the middle of a four-character run.
		{"active body cjk fragment", "见词元", "active-cjk"},
		{"active source infix", "article-import", "active-source-infix"},
		{"active quote infix", "用的原文", "active-quote-infix"},
		{"tombstone snapshot body infix", "nachbarsch", "tombstone-infix"},
		{"tombstone snapshot body cjk fragment", "有短语", "tombstone-cjk"},
		{"tombstone snapshot source infix", "channel-import", "tombstone-source-infix"},
		{"tombstone snapshot quote infix", "冻结的引文", "tombstone-quote-infix"},
		// `%` and `_` are LIKE metacharacters that reach PostgreSQL unescaped
		// today. Whether that is desirable is a product question; silently
		// changing it while touching indexes is not.
		{"percent stays a wildcard", "wild%token", "active-wildcard"},
		{"underscore stays a single-character wildcard", "wild_card", "active-wildcard"},
	} {
		items, total, next, err := repo.SearchThoughts(ctx, probe.query, "", 20)
		if err != nil {
			t.Fatalf("%s: SearchThoughts(%q): %v", probe.name, probe.query, err)
		}
		if total != 1 || len(items) != 1 || items[0].ID != probe.wantID || next != "" {
			t.Fatalf("%s: SearchThoughts(%q) = items=%#v total=%d next=%q, want only %q",
				probe.name, probe.query, items, total, next, probe.wantID)
		}
	}

	// `quote` is matched as its raw JSONB text rendering, so the structural key
	// name is part of the searchable text for every thought that carries a
	// quote. Freeze it: it is the load-bearing detail behind `quote::text`, and
	// an "obvious cleanup" to `quote->>'exact'` would change results.
	items, total, _, err := repo.SearchThoughts(ctx, "exact", "", 100)
	if err != nil {
		t.Fatalf("SearchThoughts(\"exact\"): %v", err)
	}
	if total != 9 || len(items) != 9 {
		t.Fatalf("SearchThoughts(\"exact\") = %d items total=%d, want the raw JSONB key to match all 9 fixtures", len(items), total)
	}

	// A query that only a lexeme index could satisfy must stay unmatched: the
	// contract is substring containment, never stemming or word normalisation.
	for _, query := range []string{"zwischenraume", "nachbar schaft", "importers"} {
		items, total, _, err := repo.SearchThoughts(ctx, query, "", 20)
		if err != nil {
			t.Fatalf("SearchThoughts(%q): %v", query, err)
		}
		if total != 0 || len(items) != 0 {
			t.Fatalf("SearchThoughts(%q) = items=%#v total=%d, want substring-only semantics", query, items, total)
		}
	}
}

// TestReaderThoughtSearchUsesTrigramIndexes proves the rewritten query actually
// reaches both trigram indexes instead of scanning reader_thoughts and
// reader_thought_tombstones end to end.
//
// The evidence is the index's own scan counter rather than an EXPLAIN of a
// hand-copied query string: the counter can only move if the statement the
// repository really issued used that index, so it cannot drift away from the
// production SQL the way a duplicated plan probe would.
func TestReaderThoughtSearchUsesTrigramIndexes(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	const scale = 8000
	updatedAt := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO reader_thoughts (id,host_kind,host_id,target,quote,body,source,deleted,last_sequence,created_at,updated_at)
		SELECT 'bulk-'||g,'note','host-bulk-'||g,'{"kind":"note"}'::jsonb,
			jsonb_build_object('exact','bulk quotation number '||g),
			'bulk thought body with enough prose that a sequential scan has real pages to walk '||g,
			'user',false,1,$1,$1
		FROM generate_series(1,$2) g`, updatedAt, scale); err != nil {
		t.Fatalf("seed bulk thoughts: %v", err)
	}
	// Tombstoned halves of the corpus: their live rows must not carry the probe
	// token, so a hit can only come through the snapshot index.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot,created_at)
		SELECT 'bulk-'||g,'note','host-bulk-'||g,'note_deleted',
			jsonb_build_object(
				'snapshot_version',1,'id','bulk-'||g,'host_kind','note','host_id','host-bulk-'||g,
				'link_id',NULL,'type','thought',
				'body','frozen bulk body with enough prose that a sequential scan has real pages to walk '||g,
				'target',jsonb_build_object('kind','note','host_id','host-bulk-'||g),
				'quote',jsonb_build_object('exact','frozen bulk quotation '||g),
				'source','user','created_at',$1::text,'updated_at',$1::text,
				'original_host_snapshot',jsonb_build_object('content','frozen host'),
				'original_host_identity',jsonb_build_object('kind','note','id','host-bulk-'||g),
				'frozen_at',$1::text),
			$2
		FROM generate_series(1,$3) g`,
		updatedAt.Format(time.RFC3339Nano), updatedAt, scale/2); err != nil {
		t.Fatalf("seed bulk tombstones: %v", err)
	}
	// One active needle and one tombstoned needle, both carrying a token that
	// exists nowhere else in the corpus.
	seedReaderThoughtSearchFixture(t, pool, "needle-active", "corpus prose around trigramneedle inside a word", "user", "needle quote", false, updatedAt, nil)
	seedReaderThoughtSearchFixture(t, pool, "needle-tombstone", "mutable needle body", "mutable-needle-source", "mutable needle quote", false, updatedAt,
		map[string]any{
			"body":       "frozen corpus prose around trigramneedle inside a word",
			"source":     "frozen-needle-source",
			"quote":      map[string]string{"exact": "frozen needle quote"},
			"updated_at": updatedAt.Format(time.RFC3339Nano),
		})
	if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) reader_thoughts, reader_thought_tombstones`); err != nil {
		t.Fatalf("vacuum analyze search corpus: %v", err)
	}

	const activeIndex = "idx_reader_thoughts_search_trgm"
	const tombstoneIndex = "idx_reader_thought_tombstones_search_trgm"
	before := readIndexScanCounts(t, pool, activeIndex, tombstoneIndex)

	items, total, next, err := repo.SearchThoughts(ctx, "trigramneedle", "", 20)
	if err != nil {
		t.Fatalf("SearchThoughts on scale corpus: %v", err)
	}
	if total != 2 || len(items) != 2 || next != "" {
		t.Fatalf("SearchThoughts = items=%#v total=%d next=%q, want both needles", items, total, next)
	}
	found := map[string]string{}
	for _, item := range items {
		found[item.ID] = item.LifecycleStatus
	}
	if found["needle-active"] != "active" || found["needle-tombstone"] != "tombstone" {
		t.Fatalf("SearchThoughts lifecycle statuses = %#v, want one active and one tombstone needle", found)
	}

	after := awaitIndexScanProgress(t, pool, before, activeIndex, tombstoneIndex)
	for _, name := range []string{activeIndex, tombstoneIndex} {
		if after[name] <= before[name] {
			t.Fatalf("%s scan count stayed at %d: thought search still walks the whole table (all counters: %v)",
				name, before[name], after)
		}
	}
	t.Logf("trigram index scans: %s %d -> %d, %s %d -> %d",
		activeIndex, before[activeIndex], after[activeIndex],
		tombstoneIndex, before[tombstoneIndex], after[tombstoneIndex])
}

// readIndexScanCounts snapshots pg_stat_user_indexes for the named indexes.
func readIndexScanCounts(t *testing.T, pool *pgxpool.Pool, names ...string) map[string]int64 {
	t.Helper()
	// The cumulative statistics view is snapshotted per transaction; without an
	// explicit clear a second read inside the same session can return the first
	// read's values forever.
	if _, err := pool.Exec(t.Context(), `SELECT pg_stat_clear_snapshot()`); err != nil {
		t.Fatalf("clear stat snapshot: %v", err)
	}
	counts := make(map[string]int64, len(names))
	rows, err := pool.Query(t.Context(),
		`SELECT indexrelname, idx_scan FROM pg_stat_user_indexes WHERE indexrelname = ANY($1)`, names)
	if err != nil {
		t.Fatalf("read index scan counts: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var scans int64
		if err := rows.Scan(&name, &scans); err != nil {
			t.Fatalf("scan index scan counts: %v", err)
		}
		counts[name] = scans
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index scan counts: %v", err)
	}
	for _, name := range names {
		if _, ok := counts[name]; !ok {
			t.Fatalf("index %q does not exist; the migration did not create it", name)
		}
	}
	return counts
}

// awaitIndexScanProgress polls until every counter has moved or the backend's
// statistics flush interval has clearly elapsed. Backends report cumulative
// statistics asynchronously, so a single read right after the query can legally
// still show the old value.
func awaitIndexScanProgress(t *testing.T, pool *pgxpool.Pool, before map[string]int64, names ...string) map[string]int64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var latest map[string]int64
	for {
		latest = readIndexScanCounts(t, pool, names...)
		progressed := true
		for _, name := range names {
			if latest[name] <= before[name] {
				progressed = false
			}
		}
		if progressed || time.Now().After(deadline) {
			return latest
		}
		time.Sleep(200 * time.Millisecond)
	}
}
