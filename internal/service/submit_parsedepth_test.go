package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestSubmitPropagatesParseDepthToSourceMetadata(t *testing.T) {
	t.Parallel()

	linkStore := &repotest.ObservableLinkStore{
		CreateResult: &model.Link{
			ID:        uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			URL:       "https://example.com/special",
			Status:    model.LinkStatusPending,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		},
	}
	jobStore := &repotest.ObservableJobStore{
		CreateResult: &model.ParseJob{
			ID:        uuid.MustParse("22222222-2222-2222-2222-222222222222"),
			LinkID:    linkStore.CreateResult.ID,
			Status:    model.JobStatusPending,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		},
	}
	service := newTestSubmitService(linkStore, jobStore, &submitFakeQueue{}, &submitFakeLocker{})

	deep := "deep"
	if _, err := service.Submit(context.Background(), dto.LinkCreateRequest{
		URL:        "https://example.com/special",
		ParseDepth: &deep,
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if len(linkStore.CreateCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(linkStore.CreateCalls))
	}
	got := linkStore.CreateCalls[0].SourceMetadata
	if got == nil {
		t.Fatalf("SourceMetadata is nil; want map containing parse_depth=deep")
	}
	if v, _ := got["parse_depth"].(string); v != "deep" {
		t.Fatalf("SourceMetadata[parse_depth] = %v, want \"deep\"", got["parse_depth"])
	}
}

func TestSubmitOmitsSourceMetadataWhenParseDepthEmpty(t *testing.T) {
	t.Parallel()

	linkStore := &repotest.ObservableLinkStore{
		CreateResult: &model.Link{
			ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), URL: "https://example.com/x",
			Status: model.LinkStatusPending,
		},
	}
	jobStore := &repotest.ObservableJobStore{
		CreateResult: &model.ParseJob{
			ID:     uuid.MustParse("22222222-2222-2222-2222-222222222222"),
			LinkID: linkStore.CreateResult.ID, Status: model.JobStatusPending,
		},
	}
	service := newTestSubmitService(linkStore, jobStore, &submitFakeQueue{}, &submitFakeLocker{})

	if _, err := service.Submit(context.Background(), dto.LinkCreateRequest{
		URL: "https://example.com/x",
		// No ParseDepth set
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	got := linkStore.CreateCalls[0].SourceMetadata
	if got != nil {
		t.Fatalf("SourceMetadata = %#v, want nil (no parse_depth means use deployment default)", got)
	}
}

func TestSubmitRejectsInvalidParseDepth(t *testing.T) {
	t.Parallel()

	service := newTestSubmitService(
		&repotest.ObservableLinkStore{},
		&repotest.ObservableJobStore{},
		&submitFakeQueue{},
		&submitFakeLocker{},
	)

	bogus := "ultra-deep"
	_, err := service.Submit(context.Background(), dto.LinkCreateRequest{
		URL:        "https://example.com/x",
		ParseDepth: &bogus,
	})
	if err == nil {
		t.Fatal("expected validation error for invalid parse_depth")
	}
}

func TestBatchRejectsInvalidParseDepthPerItem(t *testing.T) {
	t.Parallel()

	// Configure the fake stores so the surviving items can complete their
	// Create round-trip — otherwise the underlying BaseLinkStore panics
	// when items[0] / items[2] flow through to SubmitBatch.Create.
	linkStore := &repotest.ObservableLinkStore{
		CreateFunc: func(_ context.Context, p repository.CreateLinkParams) (*model.Link, error) {
			return &model.Link{
				ID:     uuid.New(),
				URL:    p.URL,
				Status: model.LinkStatusPending,
			}, nil
		},
	}
	jobStore := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{ID: uuid.New(), LinkID: linkID, Status: model.JobStatusPending}, nil
		},
	}
	service := newTestSubmitService(linkStore, jobStore, &submitFakeQueue{}, &submitFakeLocker{})

	good := "light"
	bad := "ultra-deep"
	req := dto.BatchCreateRequest{
		Items: []dto.LinkCreateRequest{
			{URL: "https://example.com/ok1", ParseDepth: &good},
			{URL: "https://example.com/bad", ParseDepth: &bad}, // should error this slot
			{URL: "https://example.com/ok2"},                   // empty → default
		},
	}

	resp, err := service.Batch(context.Background(), req)
	if err != nil {
		t.Fatalf("Batch should return per-item errors, not a top-level error: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("Results len = %d, want 3", len(resp.Results))
	}
	if resp.Results[1].Error == "" {
		t.Errorf("item[1] should have validation error for bad parse_depth, got %#v", resp.Results[1])
	}
	if resp.Results[0].Error != "" {
		t.Errorf("item[0] (good light) unexpected error: %q", resp.Results[0].Error)
	}
	if resp.Results[2].Error != "" {
		t.Errorf("item[2] (empty depth) unexpected error: %q", resp.Results[2].Error)
	}

	// Verify SourceMetadata was actually attached to the Create calls
	// for the two valid items.
	if len(linkStore.CreateCalls) != 2 {
		t.Fatalf("expected 2 Create calls (bad item dropped pre-dedupe); got %d", len(linkStore.CreateCalls))
	}
	gotMeta0 := linkStore.CreateCalls[0].SourceMetadata
	if v, _ := gotMeta0["parse_depth"].(string); v != "light" {
		t.Errorf("CreateCalls[0] parse_depth = %v, want \"light\"", gotMeta0["parse_depth"])
	}
	gotMeta1 := linkStore.CreateCalls[1].SourceMetadata
	if gotMeta1 != nil {
		t.Errorf("CreateCalls[1] SourceMetadata = %#v, want nil (no parse_depth specified)", gotMeta1)
	}
}
