// link_backfill_test.go exercises the Phase 9 link content-vector backfill
// against a real pgvector-enabled Postgres: the scan query (status='done' AND
// (embedding IS NULL OR embedding_model IS DISTINCT FROM current), id-cursored)
// and the UPDATE … SET embedding/embedding_model. The pure-Go unit tests in
// internal/service/link_backfill_test.go cover the batch loop / fail-soft /
// cursor logic against an in-memory store; this is the only place the real SQL
// runs against pgvector.
package dbintegration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/repository"
	"webtag/internal/service"
)

// metadataMutatingBackfillEmbedder changes the source tuple after a backfill
// candidate has been listed but before its vector is written. It makes the
// metadata-revision CAS miss deterministic without racing test goroutines.
type metadataMutatingBackfillEmbedder struct {
	mutate func() error
}

func (e *metadataMutatingBackfillEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	if e.mutate != nil {
		if err := e.mutate(); err != nil {
			return nil, err
		}
		e.mutate = nil
	}
	vectors := make([][]float32, len(inputs))
	for i := range vectors {
		vectors[i] = e0()
	}
	return vectors, nil
}

func (*metadataMutatingBackfillEmbedder) Model() string { return gateTestModel }
func (*metadataMutatingBackfillEmbedder) Enabled() bool { return true }

// seedDoneLinkNoVector inserts a done link with title/summary/input_text but NO
// content vector (the state a pre-Phase-9 row or a parse-time embedding failure
// leaves behind). Returns its id.
func seedDoneLinkNoVector(t *testing.T, pool *pgxpool.Pool, title string) uuid.UUID {
	t.Helper()
	url := "https://example.com/" + title
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO links (url, source_key, status, title, summary, input_text, content_type, first_collected_at)
		 VALUES ($1, $1, 'done', $2, 'a summary', 'body text', 'article', NOW()) RETURNING id`,
		url, title,
	).Scan(&id); err != nil {
		t.Fatalf("seed done link %q: %v", title, err)
	}
	return id
}

func linkVectorModel(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (string, bool) {
	t.Helper()
	var (
		model  *string
		hasVec bool
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT embedding_model, embedding IS NOT NULL FROM links WHERE id = $1`, id).
		Scan(&model, &hasVec); err != nil {
		t.Fatalf("link vector/model for %v: %v", id, err)
	}
	if model == nil {
		return "", hasVec
	}
	return *model, hasVec
}

// TestLinkBackfillFillsMissingDoneLinks: 3 done links with no vector → the
// backfill embeds all 3 under the current model. A pending link (not done) is
// left untouched (the scan only sees done rows).
func TestLinkBackfillFillsMissingDoneLinks(t *testing.T) {
	pool := StartPostgres(t)
	store := repository.NewPGXLinkRepository(pool)

	a := seedDoneLinkNoVector(t, pool, "alpha")
	b := seedDoneLinkNoVector(t, pool, "beta")
	c := seedDoneLinkNoVector(t, pool, "gamma")

	// A pending link must not be scanned (only status='done' participates).
	var pending uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO links (url, source_key, status, title, content_type, first_collected_at)
		 VALUES ($1, $1, 'pending', 'pending row', 'article', NOW()) RETURNING id`,
		"https://example.com/pending",
	).Scan(&pending); err != nil {
		t.Fatalf("seed pending link: %v", err)
	}

	bf := service.NewLinkBackfiller(service.LinkBackfillOptions{
		Store:     store,
		Embedder:  scriptedEmbedder{vec: e0()},
		BatchSize: 2, // force the id cursor across batches
	})
	filled, failed, skipped, err := bf.Run(context.Background())
	if err != nil || filled != 3 || failed != 0 || skipped != 0 {
		t.Fatalf("backfill filled=%d failed=%d skipped=%d err=%v, want 3/0/0/nil", filled, failed, skipped, err)
	}

	for _, tc := range []struct {
		name string
		id   uuid.UUID
	}{{"alpha", a}, {"beta", b}, {"gamma", c}} {
		model, hasVec := linkVectorModel(t, pool, tc.id)
		if !hasVec || model != gateTestModel {
			t.Fatalf("%s after backfill: hasVec=%v model=%q, want true/%q", tc.name, hasVec, model, gateTestModel)
		}
	}
	if _, hasVec := linkVectorModel(t, pool, pending); hasVec {
		t.Fatal("pending link must not be embedded by the backfill")
	}

	// Idempotent: a second run finds nothing to do.
	if filled, failed, skipped, err := bf.Run(context.Background()); err != nil || filled != 0 || failed != 0 || skipped != 0 {
		t.Fatalf("second run filled=%d failed=%d skipped=%d err=%v, want 0/0/0/nil (idempotent)", filled, failed, skipped, err)
	}
}

// TestLinkBackfillCountsStaleMetadataCASMissAsSkipped proves the candidate
// metadata is rechecked at write time. The title changes after scanning and
// embedding, so the zero-row UPDATE must be visible as skipped, not as a
// successful vector fill or a failed write.
func TestLinkBackfillCountsStaleMetadataCASMissAsSkipped(t *testing.T) {
	pool := StartPostgres(t)
	store := repository.NewPGXLinkRepository(pool)
	linkID := seedDoneLinkNoVector(t, pool, "stale-metadata-cas")
	updatedTitle := "Metadata changed while embedding"

	embedder := &metadataMutatingBackfillEmbedder{
		mutate: func() error {
			_, err := pool.Exec(t.Context(), `UPDATE links SET title = $2 WHERE id = $1`, linkID, updatedTitle)
			return err
		},
	}
	backfiller := service.NewLinkBackfiller(service.LinkBackfillOptions{
		Store:         store,
		Embedder:      embedder,
		BatchSize:     8,
		BatchInterval: time.Nanosecond,
	})

	filled, failed, skipped, err := backfiller.Run(t.Context())
	if err != nil {
		t.Fatalf("backfill Run() error = %v", err)
	}
	if filled != 0 || failed != 0 || skipped != 1 {
		t.Fatalf("backfill Run() = filled=%d failed=%d skipped=%d, want 0/0/1", filled, failed, skipped)
	}

	var (
		title  string
		model  *string
		hasVec bool
	)
	if err := pool.QueryRow(t.Context(), `SELECT title, embedding_model, embedding IS NOT NULL FROM links WHERE id = $1`, linkID).Scan(&title, &model, &hasVec); err != nil {
		t.Fatalf("read stale backfill row: %v", err)
	}
	if title != updatedTitle || hasVec || model != nil {
		t.Fatalf("stale backfill row = title=%q hasVec=%v model=%v, want updated title with no vector/model", title, hasVec, model)
	}
}
