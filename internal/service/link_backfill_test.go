package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/repository"
)

// linkBackfillRow models one links row in the in-memory backfill store.
type linkBackfillRow struct {
	id       uuid.UUID
	title    string
	summary  string
	body     string
	model    string
	embedded bool
}

// fakeLinkBackfillStore is an in-memory LinkBackfillStore. Rows are scanned in
// insertion (id) order; ListLinksNeedingEmbedding returns those whose vector is
// missing or whose model != currentModel, mirroring the SQL WHERE clause.
type fakeLinkBackfillStore struct {
	mu             sync.Mutex
	rows           []*linkBackfillRow
	updateErrFor   map[uuid.UUID]error
	staleUpdateFor map[uuid.UUID]bool
	scanErr        error
}

func newFakeLinkBackfillStore() *fakeLinkBackfillStore {
	return &fakeLinkBackfillStore{
		updateErrFor:   map[uuid.UUID]error{},
		staleUpdateFor: map[uuid.UUID]bool{},
	}
}

// seed appends a row. embeddedModel == "" means "no vector yet". Title/summary/
// body are the columns the runner folds into the embed input; a row with all
// three blank exercises the "no usable text" skip.
func (s *fakeLinkBackfillStore) seed(title, summary, body, embeddedModel string) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.New()
	r := &linkBackfillRow{id: id, title: title, summary: summary, body: body}
	if embeddedModel != "" {
		r.model = embeddedModel
		r.embedded = true
	}
	s.rows = append(s.rows, r)
	return id
}

func (s *fakeLinkBackfillStore) ListLinksNeedingEmbedding(_ context.Context, currentModel string, afterID uuid.UUID, limit int) ([]repository.LinkBackfillCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scanErr != nil {
		return nil, s.scanErr
	}
	out := make([]repository.LinkBackfillCandidate, 0, limit)
	passedCursor := afterID == uuid.Nil
	for _, r := range s.rows {
		if !passedCursor {
			if r.id == afterID {
				passedCursor = true
			}
			continue
		}
		if r.embedded && r.model == currentModel {
			continue
		}
		out = append(out, repository.LinkBackfillCandidate{
			ID:               r.id,
			Title:            strPtrOrNil(r.title),
			Summary:          strPtrOrNil(r.summary),
			InputText:        strPtrOrNil(r.body),
			MetadataRevision: 1,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *fakeLinkBackfillStore) UpdateLinkEmbedding(_ context.Context, id uuid.UUID, _ int64, _ []float32, model string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.updateErrFor[id]; err != nil {
		return false, err
	}
	if s.staleUpdateFor[id] {
		return false, nil
	}
	for _, r := range s.rows {
		if r.id == id {
			r.embedded = true
			r.model = model
			return true, nil
		}
	}
	return false, nil
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// backfillTestEmbedder is a minimal RetrievalEmbedder for the runner tests.
type backfillTestEmbedder struct {
	enabled bool
	err     error
	model   string
}

func (e backfillTestEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}
func (e backfillTestEmbedder) Model() string {
	if e.model == "" {
		return "bf-model"
	}
	return e.model
}
func (e backfillTestEmbedder) Enabled() bool { return e.enabled }

// TestLinkBackfillFillsMissingAndStale: missing-vector + stale-model rows are
// re-embedded; an already-current row is skipped. Batch size 2 forces the
// cursor across batches.
func TestLinkBackfillFillsMissingAndStale(t *testing.T) {
	t.Parallel()
	store := newFakeLinkBackfillStore()
	store.seed("alpha", "s1", "b1", "")          // missing
	store.seed("beta", "s2", "b2", "")           // missing
	store.seed("gamma", "s3", "b3", "old-model") // stale
	store.seed("delta", "s4", "b4", "bf-model")  // current → skip

	b := NewLinkBackfiller(LinkBackfillOptions{
		Store:     store,
		Embedder:  backfillTestEmbedder{enabled: true},
		BatchSize: 2,
	})
	filled, failed, skipped, err := b.Run(context.Background())
	if err != nil || filled != 3 || failed != 0 || skipped != 0 {
		t.Fatalf("Run filled=%d failed=%d skipped=%d err=%v, want 3/0/0/nil", filled, failed, skipped, err)
	}

	// A second run is idempotent — everything now carries the current model.
	filled, failed, skipped, err = b.Run(context.Background())
	if err != nil || filled != 0 || failed != 0 || skipped != 0 {
		t.Fatalf("second run filled=%d failed=%d skipped=%d err=%v, want 0/0/0/nil", filled, failed, skipped, err)
	}
}

// TestLinkBackfillDisabledEmbedderNoOps: a disabled embedder makes Run a no-op.
func TestLinkBackfillDisabledEmbedderNoOps(t *testing.T) {
	t.Parallel()
	store := newFakeLinkBackfillStore()
	store.seed("a", "", "", "")
	b := NewLinkBackfiller(LinkBackfillOptions{Store: store, Embedder: backfillTestEmbedder{enabled: false}})
	filled, failed, skipped, err := b.Run(context.Background())
	if filled != 0 || failed != 0 || skipped != 0 || err != nil {
		t.Fatalf("disabled run = %d/%d/%d/%v, want 0/0/0/nil", filled, failed, skipped, err)
	}
}

// TestLinkBackfillSkipsBlankTextRow: a row with no title/summary/body is counted
// failed (re-tried later) and never makes an embed call.
func TestLinkBackfillSkipsBlankTextRow(t *testing.T) {
	t.Parallel()
	store := newFakeLinkBackfillStore()
	store.seed("", "", "", "")        // blank → skip
	store.seed("has", "text", "", "") // usable
	b := NewLinkBackfiller(LinkBackfillOptions{Store: store, Embedder: backfillTestEmbedder{enabled: true}, BatchSize: 8})

	filled, failed, skipped, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if filled != 1 || failed != 1 || skipped != 0 {
		t.Fatalf("filled=%d failed=%d skipped=%d, want 1/1/0 (one usable, one blank skipped)", filled, failed, skipped)
	}
}

// TestLinkBackfillEmbeddingFailureSkipsBatch: an embedding error fails the whole
// batch soft (all rows counted failed, none filled) and the run completes.
func TestLinkBackfillEmbeddingFailureSkipsBatch(t *testing.T) {
	t.Parallel()
	store := newFakeLinkBackfillStore()
	store.seed("a", "", "", "")
	store.seed("b", "", "", "")
	b := NewLinkBackfiller(LinkBackfillOptions{
		Store:     store,
		Embedder:  backfillTestEmbedder{enabled: true, err: errors.New("embed down")},
		BatchSize: 8,
	})
	filled, failed, skipped, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("Run err=%v (fail-soft must not surface)", err)
	}
	if filled != 0 || failed != 2 || skipped != 0 {
		t.Fatalf("filled=%d failed=%d skipped=%d, want 0/2/0", filled, failed, skipped)
	}
}

// TestLinkBackfillPerRowWriteFailure: a write error fails just that row; the
// other row in the batch still fills.
func TestLinkBackfillPerRowWriteFailure(t *testing.T) {
	t.Parallel()
	store := newFakeLinkBackfillStore()
	good := store.seed("good", "", "", "")
	bad := store.seed("bad", "", "", "")
	store.updateErrFor[bad] = errors.New("write failed")
	_ = good

	b := NewLinkBackfiller(LinkBackfillOptions{Store: store, Embedder: backfillTestEmbedder{enabled: true}, BatchSize: 8})
	filled, failed, skipped, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if filled != 1 || failed != 1 || skipped != 0 {
		t.Fatalf("filled=%d failed=%d skipped=%d, want 1/1/0", filled, failed, skipped)
	}
}

// TestLinkBackfillHardScanErrorStops: a scan error stops the run and is
// surfaced (not swallowed like per-batch failures).
func TestLinkBackfillHardScanErrorStops(t *testing.T) {
	t.Parallel()
	store := newFakeLinkBackfillStore()
	store.scanErr = errors.New("db down")
	b := NewLinkBackfiller(LinkBackfillOptions{Store: store, Embedder: backfillTestEmbedder{enabled: true}})
	if _, _, _, err := b.Run(context.Background()); err == nil {
		t.Fatal("hard scan error must be surfaced")
	}
}

// TestLinkBackfillCountsStaleMetadataCASAsSkipped proves a zero-row CAS miss
// is neither a write failure nor a successful fill. The candidate remains
// unembedded so a later run can scan its replacement metadata.
func TestLinkBackfillCountsStaleMetadataCASAsSkipped(t *testing.T) {
	t.Parallel()
	store := newFakeLinkBackfillStore()
	stale := store.seed("stale", "summary", "body", "")
	store.staleUpdateFor[stale] = true

	b := NewLinkBackfiller(LinkBackfillOptions{Store: store, Embedder: backfillTestEmbedder{enabled: true}, BatchSize: 8})
	filled, failed, skipped, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if filled != 0 || failed != 0 || skipped != 1 {
		t.Fatalf("Run() = filled=%d failed=%d skipped=%d, want 0/0/1", filled, failed, skipped)
	}
	if store.rows[0].embedded {
		t.Fatal("stale candidate received an embedding despite losing its metadata CAS")
	}
}
