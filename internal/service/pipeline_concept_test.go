package service

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// recordingConceptAttacher captures every ResolveAndAttachBatch call
// so pipeline tests can assert which tag set ran through the resolver
// and exactly how many calls fired (the pipeline contract collapses
// N tags into one call).
type recordingConceptAttacher struct {
	mu    sync.Mutex
	calls []struct {
		linkID                   uuid.UUID
		expectedMetadataRevision int64
		tags                     []string
	}
	err error
}

func (r *recordingConceptAttacher) ResolveAndAttachBatch(_ context.Context, linkID uuid.UUID, expectedMetadataRevision int64, tags []string) ([]uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Copy the slice so a caller mutating its input after the call
	// cannot retroactively change what the test sees.
	copied := append([]string(nil), tags...)
	r.calls = append(r.calls, struct {
		linkID                   uuid.UUID
		expectedMetadataRevision int64
		tags                     []string
	}{linkID, expectedMetadataRevision, copied})
	if r.err != nil {
		return nil, r.err
	}
	ids := make([]uuid.UUID, len(tags))
	for i := range ids {
		ids[i] = uuid.New()
	}
	return ids, nil
}

func TestAttachConceptsCallsResolverOnceForAllTags(t *testing.T) {
	t.Parallel()

	attacher := &recordingConceptAttacher{}
	p := &ParsePipeline{conceptAttacher: attacher}
	linkID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	p.attachConcepts(context.Background(), linkID, 7, []string{"RAG", "腾讯", "WeKnora"})

	if len(attacher.calls) != 1 {
		t.Fatalf("batch calls = %d, want 1 (pipeline must collapse N tags into one call)", len(attacher.calls))
	}
	got := attacher.calls[0]
	if got.linkID != linkID {
		t.Errorf("call linkID = %v, want %v", got.linkID, linkID)
	}
	if got.expectedMetadataRevision != 7 {
		t.Errorf("call expected metadata revision = %d, want 7", got.expectedMetadataRevision)
	}
	want := []string{"RAG", "腾讯", "WeKnora"}
	if len(got.tags) != len(want) {
		t.Fatalf("tag count = %d, want %d", len(got.tags), len(want))
	}
	for i := range want {
		if got.tags[i] != want[i] {
			t.Errorf("tags[%d] = %q, want %q", i, got.tags[i], want[i])
		}
	}
}

func TestAttachConceptsSwallowsBatchError(t *testing.T) {
	t.Parallel()

	attacher := &recordingConceptAttacher{err: context.DeadlineExceeded}
	p := &ParsePipeline{conceptAttacher: attacher}

	// Must not panic and must not return (no return at all — best
	// effort), but the attacher still gets the one batch call.
	p.attachConcepts(context.Background(), uuid.New(), 3, []string{"a", "b"})

	if len(attacher.calls) != 1 {
		t.Errorf("batch calls = %d, want 1 even on error", len(attacher.calls))
	}
}

func TestAttachConceptsNoopOnEmptyTagSlice(t *testing.T) {
	t.Parallel()

	attacher := &recordingConceptAttacher{}
	p := &ParsePipeline{conceptAttacher: attacher}

	p.attachConcepts(context.Background(), uuid.New(), 1, nil)
	if len(attacher.calls) != 0 {
		t.Errorf("nil tags should produce 0 calls; got %d", len(attacher.calls))
	}

	p.attachConcepts(context.Background(), uuid.New(), 1, []string{})
	if len(attacher.calls) != 0 {
		t.Errorf("empty tags should produce 0 calls; got %d", len(attacher.calls))
	}
}
