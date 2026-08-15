package concept

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// backfillRow models one concept row in the in-memory backfill store: its
// primary_name plus the current vector/model state. embedded reports whether
// the row already carries a current-model vector.
type backfillRow struct {
	id       uuid.UUID
	name     string
	vector   []float32
	model    string
	embedded bool
}

// fakeBackfillStore is an in-memory BackfillStore. Rows are scanned in
// insertion (id) order; ListConceptsNeedingEmbedding returns those whose
// vector is missing or whose model != currentModel, mirroring the SQL WHERE
// clause. updateErrFor forces UpdateConceptEmbedding to fail for a specific id
// so the per-row fail-soft path can be exercised.
type fakeBackfillStore struct {
	mu           sync.Mutex
	rows         []*backfillRow
	updateErrFor map[uuid.UUID]error
	scanErr      error
	scanCalls    int
}

func newFakeBackfillStore() *fakeBackfillStore {
	return &fakeBackfillStore{updateErrFor: map[uuid.UUID]error{}}
}

// seed appends a row. embeddedModel == "" means "no vector yet"; a non-empty
// value seeds an already-embedded row under that model (used to verify a
// stale-model row is re-scanned and a current-model row is skipped).
func (s *fakeBackfillStore) seed(name, embeddedModel string) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.New()
	r := &backfillRow{id: id, name: name}
	if embeddedModel != "" {
		r.model = embeddedModel
		r.vector = []float32{1, 0, 0}
		r.embedded = true
	}
	s.rows = append(s.rows, r)
	return id
}

func (s *fakeBackfillStore) ListConceptsNeedingEmbedding(_ context.Context, currentModel string, afterID uuid.UUID, limit int) ([]BackfillCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanCalls++
	if s.scanErr != nil {
		return nil, s.scanErr
	}
	out := make([]BackfillCandidate, 0, limit)
	passedCursor := afterID == uuid.Nil
	for _, r := range s.rows {
		if !passedCursor {
			if r.id == afterID {
				passedCursor = true
			}
			continue
		}
		needs := !r.embedded || r.model != currentModel
		if !needs {
			continue
		}
		out = append(out, BackfillCandidate{ID: r.id, PrimaryName: r.name})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *fakeBackfillStore) UpdateConceptEmbedding(_ context.Context, id uuid.UUID, embedding []float32, embeddingModel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.updateErrFor[id]; err != nil {
		return err
	}
	for _, r := range s.rows {
		if r.id == id {
			r.vector = append([]float32(nil), embedding...)
			r.model = embeddingModel
			r.embedded = true
			return nil
		}
	}
	return errors.New("update: id not found")
}

func (s *fakeBackfillStore) embeddedCount(model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.rows {
		if r.embedded && r.model == model {
			n++
		}
	}
	return n
}

// fastBackfiller builds a Backfiller with a tiny batch interval so tests do
// not wait out the 500ms default between batches.
func fastBackfiller(store BackfillStore, emb Embedder, batchSize int) *Backfiller {
	return NewBackfiller(BackfillOptions{
		Store:         store,
		Embedder:      emb,
		BatchSize:     batchSize,
		BatchInterval: time.Millisecond,
	})
}

// TestBackfillDisabledEmbedderNoOp: a disabled embedder must short-circuit the
// run without touching the store.
func TestBackfillDisabledEmbedderNoOp(t *testing.T) {
	store := newFakeBackfillStore()
	store.seed("RAG", "")
	emb := &stubEmbedder{model: "m1", vec: []float32{0.1, 0.2}, enabled: false}

	b := fastBackfiller(store, emb, 64)
	filled, failed, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if filled != 0 || failed != 0 {
		t.Fatalf("disabled embedder: filled=%d failed=%d, want 0/0", filled, failed)
	}
	if store.scanCalls != 0 {
		t.Fatalf("disabled embedder scanned the store %d times, want 0", store.scanCalls)
	}
}

// TestBackfillFillsMissingAndStaleAcrossBatches: 3 vector-less rows + 1
// stale-model row + 1 already-current row, batch size 2 → only the 4 needy
// rows get the current-model vector, spanning multiple batches via the id
// cursor.
func TestBackfillFillsMissingAndStaleAcrossBatches(t *testing.T) {
	store := newFakeBackfillStore()
	store.seed("alpha", "")                  // missing
	store.seed("beta", "")                   // missing
	store.seed("gamma", "")                  // missing
	store.seed("delta", "old-mod")           // stale model
	currentID := store.seed("epsilon", "m1") // already current → must be skipped

	emb := enabledEmbedder("m1") // returns a non-empty vec for every input
	b := fastBackfiller(store, emb, 2)

	filled, failed, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if filled != 4 || failed != 0 {
		t.Fatalf("filled=%d failed=%d, want 4/0", filled, failed)
	}
	if n := store.embeddedCount("m1"); n != 5 {
		t.Fatalf("current-model embedded rows = %d, want 5 (4 backfilled + 1 pre-existing)", n)
	}
	// The pre-existing current-model row must not have been re-embedded
	// (still present, still m1) — embeddedCount already covers it, but assert
	// it was never offered as a candidate by checking it kept its seed vector.
	store.mu.Lock()
	for _, r := range store.rows {
		if r.id == currentID && (len(r.vector) != 3 || r.vector[0] != 1) {
			store.mu.Unlock()
			t.Fatalf("pre-existing current row was re-embedded: vec=%v", r.vector)
		}
	}
	store.mu.Unlock()
}

// TestBackfillBatchEmbeddingFailureIsFailSoft: when a batch's Embed call
// errors, every row in that batch is counted failed and skipped, and the run
// still drains the remaining batches.
func TestBackfillBatchEmbeddingFailureIsFailSoft(t *testing.T) {
	store := newFakeBackfillStore()
	store.seed("a", "")
	store.seed("b", "")

	emb := &stubEmbedder{model: "m1", embedErr: errors.New("upstream 503"), enabled: true}
	b := fastBackfiller(store, emb, 2)

	filled, failed, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("Run should not surface a soft batch failure: %v", err)
	}
	if filled != 0 || failed != 2 {
		t.Fatalf("filled=%d failed=%d, want 0/2", filled, failed)
	}
	if n := store.embeddedCount("m1"); n != 0 {
		t.Fatalf("embedded rows = %d, want 0 (embedding failed)", n)
	}
}

// TestBackfillPerRowWriteFailureIsFailSoft: a single row whose write fails is
// counted failed; its batch-mates still fill.
func TestBackfillPerRowWriteFailureIsFailSoft(t *testing.T) {
	store := newFakeBackfillStore()
	good := store.seed("good", "")
	bad := store.seed("bad", "")
	store.updateErrFor[bad] = errors.New("write conflict")

	emb := enabledEmbedder("m1")
	b := fastBackfiller(store, emb, 8)

	filled, failed, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if filled != 1 || failed != 1 {
		t.Fatalf("filled=%d failed=%d, want 1/1", filled, failed)
	}
	if n := store.embeddedCount("m1"); n != 1 {
		t.Fatalf("embedded rows = %d, want 1 (good only)", n)
	}
	// The good row must carry the current model.
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, r := range store.rows {
		if r.id == good && r.model != "m1" {
			t.Fatalf("good row not embedded under current model: %q", r.model)
		}
	}
}

// TestBackfillContextCancellationStopsRun: a context cancelled mid-scan makes
// Run return the cancellation error promptly without draining everything.
func TestBackfillContextCancellationStopsRun(t *testing.T) {
	store := newFakeBackfillStore()
	for i := 0; i < 10; i++ {
		store.seed("c", "")
	}
	emb := enabledEmbedder("m1")
	b := fastBackfiller(store, emb, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run starts

	_, _, err := b.Run(ctx)
	if !ErrIsCancel(err) {
		t.Fatalf("Run err = %v, want a context-cancellation error", err)
	}
}

// TestBackfillHardScanErrorSurfaces: a hard scan error (not a soft batch
// failure) aborts the run and is returned to the caller.
func TestBackfillHardScanErrorSurfaces(t *testing.T) {
	store := newFakeBackfillStore()
	store.scanErr = errors.New("db down")
	emb := enabledEmbedder("m1")
	b := fastBackfiller(store, emb, 4)

	_, _, err := b.Run(context.Background())
	if err == nil {
		t.Fatal("Run should surface a hard scan error")
	}
}

// TestNewBackfillerAppliesDefaults: a zero-value batch size / interval falls
// back to the Spec defaults (64 / 500ms).
func TestNewBackfillerAppliesDefaults(t *testing.T) {
	b := NewBackfiller(BackfillOptions{Store: newFakeBackfillStore(), Embedder: enabledEmbedder("m1")})
	if b.batchSize != defaultBackfillBatchSize {
		t.Errorf("batchSize = %d, want %d", b.batchSize, defaultBackfillBatchSize)
	}
	if b.batchInterval != defaultBackfillBatchInterval {
		t.Errorf("batchInterval = %v, want %v", b.batchInterval, defaultBackfillBatchInterval)
	}
}
