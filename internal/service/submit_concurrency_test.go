package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestSubmitServiceRefreshReReadsWorkerCompletionInsideLock(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("71111111-1111-1111-1111-111111111111")
	oldJobID := uuid.MustParse("72222222-2222-2222-2222-222222222222")
	newJobID := uuid.MustParse("73333333-3333-3333-3333-333333333333")
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	initial := &model.Link{
		ID: linkID, URL: "https://example.com/worker-completed", Status: model.LinkStatusPending,
	}
	current := &model.Link{
		ID: linkID, URL: initial.URL, Status: model.LinkStatusDone,
	}
	var reads atomic.Int32
	links := &repotest.ObservableLinkStore{
		GetByIDFunc: func(context.Context, uuid.UUID) (*model.Link, error) {
			if reads.Add(1) == 1 {
				return initial, nil
			}
			return current, nil
		},
		UpdateStateFunc: func(_ context.Context, params repository.UpdateLinkStateParams) error {
			current.Status = params.Status
			return nil
		},
	}
	jobs := &repotest.ObservableJobStore{
		LatestByLinkID: map[uuid.UUID]*model.ParseJob{
			linkID: {
				ID: oldJobID, LinkID: linkID, Status: model.JobStatusDone,
				CreatedAt: now.Add(-2 * time.Minute),
			},
		},
		CreateFunc: func(context.Context, uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{ID: newJobID, LinkID: linkID, Status: model.JobStatusPending}, nil
		},
	}
	queue := &submitFakeQueue{}
	service := newFakeSubmitService(
		links,
		&submitFakeSubmitter{links: links, jobs: jobs},
		jobs,
		queue,
		&submitFakeLocker{},
		SubmitServiceOptions{RefreshCooldown: time.Minute},
	)
	service.now = func() time.Time { return now }

	got, err := service.Refresh(context.Background(), linkID.String())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	wantJobID := newJobID.String()
	want := dto.SubmitResponse{JobID: &wantJobID, LinkID: linkID.String(), Status: string(model.LinkStatusPending)}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Refresh() = %#v, want new attempt %#v", got, want)
	}
	if gotReads := reads.Load(); gotReads != 2 {
		t.Fatalf("link reads = %d, want initial lookup plus in-lock refresh", gotReads)
	}
	if len(queue.ids) != 1 || queue.ids[0] != linkID {
		t.Fatalf("queued links = %#v, want completed link requeued once", queue.ids)
	}
}

func TestSubmitServiceRefreshReusesAttemptCreatedWhileWaitingForLock(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("74444444-4444-4444-4444-444444444444")
	jobID := uuid.MustParse("75555555-5555-5555-5555-555555555555")
	now := time.Date(2026, 7, 11, 13, 0, 0, 0, time.UTC)
	initial := &model.Link{
		ID: linkID, URL: "https://example.com/concurrent-refresh", Status: model.LinkStatusDone,
	}
	current := &model.Link{
		ID: linkID, URL: initial.URL, Status: model.LinkStatusPending,
	}
	var reads atomic.Int32
	links := &repotest.ObservableLinkStore{
		GetByIDFunc: func(context.Context, uuid.UUID) (*model.Link, error) {
			if reads.Add(1) == 1 {
				return initial, nil
			}
			return current, nil
		},
	}
	jobs := &repotest.ObservableJobStore{
		LatestByLinkID: map[uuid.UUID]*model.ParseJob{
			linkID: {ID: jobID, LinkID: linkID, Status: model.JobStatusPending, CreatedAt: now},
		},
	}
	queue := &submitFakeQueue{}
	service := newFakeSubmitService(
		links,
		&submitFakeSubmitter{links: links, jobs: jobs},
		jobs,
		queue,
		&submitFakeLocker{},
		SubmitServiceOptions{RefreshCooldown: time.Minute},
	)
	service.now = func() time.Time { return now }

	got, err := service.Refresh(context.Background(), linkID.String())
	if err != nil {
		t.Fatalf("Refresh() error = %v, want in-flight attempt reuse", err)
	}
	wantJobID := jobID.String()
	want := dto.SubmitResponse{JobID: &wantJobID, LinkID: linkID.String(), Status: string(model.LinkStatusPending)}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Refresh() = %#v, want %#v", got, want)
	}
	if gotReads := reads.Load(); gotReads != 2 {
		t.Fatalf("link reads = %d, want initial lookup plus in-lock refresh", gotReads)
	}
	if len(queue.ids) != 0 {
		t.Fatalf("queued links = %#v, want no duplicate attempt", queue.ids)
	}
}
