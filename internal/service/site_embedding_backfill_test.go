package service

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/repository"
)

type siteEmbeddingBackfillFake struct {
	mu     sync.Mutex
	rows   []repository.SiteEmbeddingCandidate
	models map[uuid.UUID]string
}

func (f *siteEmbeddingBackfillFake) ListSitesNeedingEmbedding(_ context.Context, model string, after uuid.UUID, limit int) ([]repository.SiteEmbeddingCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]repository.SiteEmbeddingCandidate, 0, limit)
	for _, row := range f.rows {
		if row.ID == after {
			continue
		}
		if after != uuid.Nil && row.ID.String() < after.String() {
			continue
		}
		if f.models[row.ID] == model {
			continue
		}
		out = append(out, row)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
func (f *siteEmbeddingBackfillFake) UpdateSiteEmbedding(_ context.Context, id uuid.UUID, revision int64, _ []float32, model string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.ID == id && row.Revision != revision {
			return false, nil
		}
	}
	f.models[id] = model
	return true, nil
}

type recordingSiteEmbedder struct{ inputs [][]string }

func (e *recordingSiteEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	e.inputs = append(e.inputs, append([]string(nil), inputs...))
	out := make([][]float32, len(inputs))
	for i := range out {
		out[i] = []float32{1, 2}
	}
	return out, nil
}
func (*recordingSiteEmbedder) Model() string { return "site-model" }
func (*recordingSiteEmbedder) Enabled() bool { return true }

func TestSiteEmbeddingBackfillUsesProfileOnlyDocument(t *testing.T) {
	t.Parallel()
	store := &siteEmbeddingBackfillFake{rows: []repository.SiteEmbeddingCandidate{{ID: uuid.New(), Revision: 3, Name: "Example", Intro: "Useful APIs", DisplayHost: "example.com", Tags: []string{"api"}, Entries: []repository.SiteEmbeddingEntryCandidate{{Name: "Docs", Purpose: "Read the API"}}}}, models: map[uuid.UUID]string{}}
	embedder := &recordingSiteEmbedder{}
	filled, failed, err := NewSiteEmbeddingBackfiller(SiteEmbeddingBackfillOptions{Store: store, Embedder: embedder, BatchSize: 8}).Run(context.Background())
	if err != nil || filled != 1 || failed != 0 {
		t.Fatalf("Run() = %d/%d/%v", filled, failed, err)
	}
	if len(embedder.inputs) != 1 || len(embedder.inputs[0]) != 1 {
		t.Fatalf("embed inputs = %#v", embedder.inputs)
	}
	input := embedder.inputs[0][0]
	for _, expected := range []string{"site: Example", "intro: Useful APIs", "host: example.com", "tag: api", "entry: Docs", "purpose: Read the API"} {
		if !containsSiteEmbedding(input, expected) {
			t.Fatalf("input %q misses %q", input, expected)
		}
	}
	if containsSiteEmbedding(input, "DO-NOT-EMBED-CAPTURED-BODY") {
		t.Fatalf("input includes a body sentinel: %q", input)
	}
}

func TestSiteEmbeddingBackfillDoesNotCountStaleRevisionAsFilled(t *testing.T) {
	t.Parallel()
	candidate := repository.SiteEmbeddingCandidate{ID: uuid.New(), Revision: 4, Name: "Before"}
	embedder := &recordingSiteEmbedder{}
	// Simulate a profile mutation after the candidate snapshot but before CAS.
	filled, failed, err := NewSiteEmbeddingBackfiller(SiteEmbeddingBackfillOptions{
		Store: &staleSiteEmbeddingStore{candidate: candidate, currentRevision: 5}, Embedder: embedder, BatchSize: 8,
	}).Run(context.Background())
	if err != nil || filled != 0 || failed != 0 {
		t.Fatalf("Run() = %d/%d/%v, want stale CAS skip", filled, failed, err)
	}
}

type staleSiteEmbeddingStore struct {
	candidate       repository.SiteEmbeddingCandidate
	currentRevision int64
}

func (s *staleSiteEmbeddingStore) ListSitesNeedingEmbedding(_ context.Context, _ string, after uuid.UUID, _ int) ([]repository.SiteEmbeddingCandidate, error) {
	if after != uuid.Nil {
		return nil, nil
	}
	return []repository.SiteEmbeddingCandidate{s.candidate}, nil
}

func (s *staleSiteEmbeddingStore) UpdateSiteEmbedding(_ context.Context, _ uuid.UUID, revision int64, _ []float32, _ string) (bool, error) {
	return revision == s.currentRevision, nil
}

func containsSiteEmbedding(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
