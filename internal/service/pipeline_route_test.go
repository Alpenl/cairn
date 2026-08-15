package service

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/concept"
)

func cands(names ...string) []concept.CandidateConcept {
	out := make([]concept.CandidateConcept, len(names))
	for i, n := range names {
		out[i] = concept.CandidateConcept{ConceptID: uuid.New(), Name: n}
	}
	return out
}

// TestRouteRetrievedTagsKeepsSelectionsTruncatesNewWords is the Phase 7
// new-word routing contract: every candidate hit survives; new words are
// capped at one (first wins, rest dropped); order is preserved.
func TestRouteRetrievedTagsKeepsSelectionsTruncatesNewWords(t *testing.T) {
	t.Parallel()

	candidates := cands("RAG", "检索增强生成", "WeKnora")

	cases := []struct {
		name        string
		tags        []string
		wantKept    []string
		wantDropped []string
	}{
		{
			name:        "all selections survive",
			tags:        []string{"RAG", "WeKnora"},
			wantKept:    []string{"RAG", "WeKnora"},
			wantDropped: nil,
		},
		{
			name:        "one new word allowed",
			tags:        []string{"RAG", "腾讯"},
			wantKept:    []string{"RAG", "腾讯"},
			wantDropped: nil,
		},
		{
			name:        "excess new words truncated to first",
			tags:        []string{"RAG", "腾讯", "字节", "阿里"},
			wantKept:    []string{"RAG", "腾讯"},
			wantDropped: []string{"字节", "阿里"},
		},
		{
			name:        "case-insensitive candidate match",
			tags:        []string{"rag", "WEKNORA"},
			wantKept:    []string{"rag", "WEKNORA"},
			wantDropped: nil,
		},
		{
			name:        "order preserved with interleaved new words",
			tags:        []string{"新词A", "RAG", "新词B"},
			wantKept:    []string{"新词A", "RAG"},
			wantDropped: []string{"新词B"},
		},
		{
			name:        "blank tags skipped",
			tags:        []string{"  ", "RAG", ""},
			wantKept:    []string{"RAG"},
			wantDropped: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kept, dropped := routeRetrievedTags(tc.tags, candidates)
			if !reflect.DeepEqual(kept, tc.wantKept) {
				t.Errorf("kept = %#v, want %#v", kept, tc.wantKept)
			}
			if !reflect.DeepEqual(dropped, tc.wantDropped) {
				t.Errorf("dropped = %#v, want %#v", dropped, tc.wantDropped)
			}
		})
	}
}

// TestRouteTagsPassthroughWhenNoCandidates verifies the free-generation
// path: with no candidate set, tags are returned unchanged (no truncation,
// since there is no "candidate vs new word" distinction to make).
func TestRouteTagsPassthroughWhenNoCandidates(t *testing.T) {
	t.Parallel()

	p := &ParsePipeline{}
	tags := []string{"a", "b", "c", "d", "e"}
	got := p.routeTags(t.Context(), uuid.New(), tags, nil)
	if !reflect.DeepEqual(got, tags) {
		t.Errorf("passthrough = %#v, want %#v", got, tags)
	}
}
