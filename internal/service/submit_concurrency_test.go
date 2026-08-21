package service

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestSubmitServiceRefreshReReadsWorkerCompletionInsideLock(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("71111111-1111-1111-1111-111111111111")
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
	queue := &submitFakeQueue{}
	service := newFakeSubmitService(
		links,
		&submitFakeSubmitter{links: links},
		queue,
		&submitFakeLocker{},
		SubmitServiceOptions{},
	)

	got, err := service.Refresh(context.Background(), linkID.String())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	want := dto.SubmitResponse{LinkID: linkID.String(), Status: string(model.LinkStatusPending)}
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

func TestSubmitServiceRefreshReusesPendingStateCreatedWhileWaitingForLock(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("74444444-4444-4444-4444-444444444444")
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
	queue := &submitFakeQueue{}
	service := newFakeSubmitService(
		links,
		&submitFakeSubmitter{links: links},
		queue,
		&submitFakeLocker{},
		SubmitServiceOptions{},
	)

	got, err := service.Refresh(context.Background(), linkID.String())
	if err != nil {
		t.Fatalf("Refresh() error = %v, want in-flight attempt reuse", err)
	}
	want := dto.SubmitResponse{LinkID: linkID.String(), Status: string(model.LinkStatusPending)}
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
