package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
)

func TestBuildTreeLinksChildrenToParents(t *testing.T) {
	t.Parallel()

	rootID := uuid.New()
	childID := uuid.New()
	createdAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)

	links := []model.Link{
		{
			ID:        childID,
			URL:       "https://example.com/articles/12345",
			Status:    model.LinkStatusDone,
			ParentID:  &rootID,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
		{
			ID:        rootID,
			URL:       "https://example.com/",
			Status:    model.LinkStatusDone,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
	}

	nodes, total := BuildTree(links)
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}

	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}

	if nodes[0].ID != rootID.String() {
		t.Fatalf("root ID = %q, want %q", nodes[0].ID, rootID.String())
	}

	if len(nodes[0].Children) != 1 {
		t.Fatalf("len(children) = %d, want 1", len(nodes[0].Children))
	}

	if nodes[0].Children[0].ID != childID.String() {
		t.Fatalf("child ID = %q, want %q", nodes[0].Children[0].ID, childID.String())
	}
}

func TestBuildTreeNormalizesMissingTagsToEmptyArray(t *testing.T) {
	t.Parallel()

	roots, total := BuildTree([]model.Link{{ID: uuid.New(), URL: "https://example.com", Tags: nil}})
	if total != 1 || len(roots) != 1 {
		t.Fatalf("BuildTree() = %d roots/%d total, want 1/1", len(roots), total)
	}
	if roots[0].Tags == nil {
		t.Fatal("BuildTree() tags = nil, want a non-nil empty array")
	}
}

func TestBuildTreeCarriesLowConfidenceFields(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	fetcherType := "basic+search"
	reason := "search_fallback"

	nodes, total := BuildTree([]model.Link{
		{
			ID:                  linkID,
			URL:                 "https://example.com/post",
			Status:              model.LinkStatusDone,
			FetcherType:         &fetcherType,
			IsLowConfidence:     true,
			LowConfidenceReason: &reason,
			CreatedAt:           now,
			UpdatedAt:           now,
		},
	})
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if !nodes[0].IsLowConfidence {
		t.Fatal("IsLowConfidence = false, want true")
	}
	if nodes[0].LowConfidenceReason == nil || *nodes[0].LowConfidenceReason != reason {
		t.Fatalf("LowConfidenceReason = %#v, want %q", nodes[0].LowConfidenceReason, reason)
	}
	if nodes[0].FetcherType == nil || *nodes[0].FetcherType != fetcherType {
		t.Fatalf("FetcherType = %#v, want %q", nodes[0].FetcherType, fetcherType)
	}
}

func TestBuildTreePromotesOrphansToRoots(t *testing.T) {
	t.Parallel()

	missingParentID := uuid.New()
	linkID := uuid.New()
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)

	links := []model.Link{
		{
			ID:        linkID,
			URL:       "https://example.com/orphan",
			Status:    model.LinkStatusDone,
			ParentID:  &missingParentID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}

	nodes, total := BuildTree(links)
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}

	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}

	if nodes[0].ID != linkID.String() {
		t.Fatalf("root ID = %q, want %q", nodes[0].ID, linkID.String())
	}
}

func TestBuildTreePreservesGrandchildren(t *testing.T) {
	t.Parallel()

	rootID := uuid.New()
	childID := uuid.New()
	grandchildID := uuid.New()
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)

	links := []model.Link{
		{
			ID:        rootID,
			URL:       "https://example.com/root",
			Status:    model.LinkStatusDone,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:        childID,
			URL:       "https://example.com/root/child",
			Status:    model.LinkStatusDone,
			ParentID:  &rootID,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:        grandchildID,
			URL:       "https://example.com/root/child/grandchild",
			Status:    model.LinkStatusDone,
			ParentID:  &childID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}

	nodes, _ := BuildTree(links)
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if len(nodes[0].Children) != 1 {
		t.Fatalf("len(root children) = %d, want 1", len(nodes[0].Children))
	}
	if len(nodes[0].Children[0].Children) != 1 {
		t.Fatalf("len(grandchildren) = %d, want 1", len(nodes[0].Children[0].Children))
	}
	if nodes[0].Children[0].Children[0].ID != grandchildID.String() {
		t.Fatalf("grandchild ID = %q, want %q", nodes[0].Children[0].Children[0].ID, grandchildID.String())
	}
}

// TestBuildTreeTruncatesDeepChain pins the M-2 fix: a parent_id chain
// longer than maxTreeDepth must render the cap-depth node with
// Truncated=true and no children, instead of recursing the encoder
// into a 5000-deep nested object.
func TestBuildTreeTruncatesDeepChain(t *testing.T) {
	t.Parallel()

	const chainLen = maxTreeDepth + 5
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	links := make([]model.Link, 0, chainLen)
	prev := uuid.UUID{}
	for i := 0; i < chainLen; i++ {
		id := uuid.New()
		link := model.Link{
			ID:        id,
			URL:       "https://example.com/chain/" + id.String(),
			Status:    model.LinkStatusDone,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if i > 0 {
			parent := prev
			link.ParentID = &parent
		}
		links = append(links, link)
		prev = id
	}

	nodes, total := BuildTree(links)
	if total != chainLen {
		t.Fatalf("total = %d, want %d", total, chainLen)
	}
	if len(nodes) != 1 {
		t.Fatalf("len(roots) = %d, want 1 (single chain root)", len(nodes))
	}

	// Walk the response and make sure depth caps out — the deepest
	// node must have Truncated=true and zero children.
	depth := 0
	cur := nodes[0]
	for len(cur.Children) > 0 {
		cur = cur.Children[0]
		depth++
	}
	if depth >= chainLen-1 {
		t.Fatalf("response not truncated: depth = %d, full chain = %d", depth, chainLen-1)
	}
	if !cur.Truncated {
		t.Fatalf("deepest rendered node should be Truncated=true; got %#v", cur)
	}
	if depth != maxTreeDepth {
		t.Fatalf("response depth = %d, want maxTreeDepth %d", depth, maxTreeDepth)
	}
}

// TestBuildTreeHandlesCycleWithLongTail mirrors a pathological dataset:
// a long acyclic prefix that feeds into a 3-node cycle. The cycle
// members should land in roots; the prefix should attach normally.
func TestBuildTreeHandlesCycleWithLongTail(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	cycleA := uuid.New()
	cycleB := uuid.New()
	cycleC := uuid.New()
	tailHead := uuid.New()
	tailMid := uuid.New()

	links := []model.Link{
		{ID: cycleA, URL: "https://example.com/A", Status: model.LinkStatusDone, ParentID: &cycleB, CreatedAt: now, UpdatedAt: now},
		{ID: cycleB, URL: "https://example.com/B", Status: model.LinkStatusDone, ParentID: &cycleC, CreatedAt: now, UpdatedAt: now},
		{ID: cycleC, URL: "https://example.com/C", Status: model.LinkStatusDone, ParentID: &cycleA, CreatedAt: now, UpdatedAt: now},
		// Tail: head -> mid -> A (cycle entry). head and mid are NOT
		// part of the cycle; they should attach to A normally.
		{ID: tailHead, URL: "https://example.com/Head", Status: model.LinkStatusDone, ParentID: &tailMid, CreatedAt: now, UpdatedAt: now},
		{ID: tailMid, URL: "https://example.com/Mid", Status: model.LinkStatusDone, ParentID: &cycleA, CreatedAt: now, UpdatedAt: now},
	}

	nodes, total := BuildTree(links)
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	// Three cycle members become roots. The tail attaches to A and so
	// does not surface as a separate root.
	if len(nodes) != 3 {
		t.Fatalf("len(roots) = %d, want 3 (cycle members promoted)", len(nodes))
	}
}

func TestBuildTreeTreatsCyclesAsRoots(t *testing.T) {
	t.Parallel()

	firstID := uuid.New()
	secondID := uuid.New()
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)

	links := []model.Link{
		{
			ID:        firstID,
			URL:       "https://example.com/first",
			Status:    model.LinkStatusDone,
			ParentID:  &secondID,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:        secondID,
			URL:       "https://example.com/second",
			Status:    model.LinkStatusDone,
			ParentID:  &firstID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}

	nodes, total := BuildTree(links)
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	if len(nodes) != 2 {
		t.Fatalf("len(nodes) = %d, want 2", len(nodes))
	}
}
