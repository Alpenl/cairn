package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
	"webtag/internal/service/urllock"
)

func TestSubmitPromotesHistoricalSkeletonToPending(t *testing.T) {
	t.Parallel()

	const rawURL = "https://example.com/historical-skeleton"
	linkID := uuid.MustParse("10101010-1010-1010-1010-101010101010")
	jobID := uuid.MustParse("20202020-2020-2020-2020-202020202020")
	description := "keep this note"
	parseDepth := "deep"
	links := &repotest.ObservableLinkStore{
		ByURL: map[string]*model.Link{
			rawURL: {
				ID:         linkID,
				URL:        rawURL,
				SourceKind: "url",
				SourceKey:  rawURL,
				Status:     model.LinkStatusSkeleton,
				CreatedAt:  time.Now().UTC(),
				UpdatedAt:  time.Now().UTC(),
			},
		},
		UpdateStateFunc: func(context.Context, repository.UpdateLinkStateParams) error { return nil },
	}
	jobs := &repotest.ObservableJobStore{CreateResult: &model.ParseJob{
		ID: jobID, LinkID: linkID, Status: model.JobStatusPending,
	}}
	queue := &submitFakeQueue{}
	submitter := &submitFakeSubmitter{links: links, jobs: jobs}
	svc := newFakeSubmitService(links, submitter, jobs, queue, &submitFakeLocker{}, SubmitServiceOptions{})

	got, err := svc.Submit(context.Background(), dto.LinkCreateRequest{
		URL: rawURL, Description: &description, ParseDepth: &parseDepth,
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got.LinkID != linkID.String() || got.Status != string(model.LinkStatusPending) || got.JobID == nil || *got.JobID != jobID.String() {
		t.Fatalf("Submit() = %#v, want existing link with a fresh pending job", got)
	}
	if len(queue.ids) != 1 || queue.ids[0] != linkID {
		t.Fatalf("queued ids = %#v, want [%s]", queue.ids, linkID)
	}
	if len(submitter.requeueCaptures) != 1 || submitter.requeueCaptures[0] == nil {
		t.Fatalf("requeue captures = %#v, want the current submit input", submitter.requeueCaptures)
	}
	capture := submitter.requeueCaptures[0]
	if capture.URL != rawURL || capture.Description == nil || *capture.Description != description {
		t.Fatalf("promoted capture = %#v, want URL and note preserved", capture)
	}
	if gotDepth, _ := capture.SourceMetadata["parse_depth"].(string); gotDepth != parseDepth {
		t.Fatalf("promoted parse_depth = %q, want %q", gotDepth, parseDepth)
	}
}

func TestConcurrentBatchesPromoteHistoricalSkeletonOnce(t *testing.T) {
	const rawURL = "https://example.com/batch-skeleton"
	linkID := uuid.MustParse("30303030-3030-3030-3030-303030303030")
	current := model.Link{
		ID: linkID, URL: rawURL, SourceKind: "url", SourceKey: rawURL,
		Status: model.LinkStatusSkeleton, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}

	var stateMu sync.Mutex
	links := &repotest.ObservableLinkStore{}
	links.GetByURLFunc = func(context.Context, string) (*model.Link, error) {
		stateMu.Lock()
		copy := current
		stateMu.Unlock()
		return &copy, nil
	}
	links.UpdateStateFunc = func(_ context.Context, params repository.UpdateLinkStateParams) error {
		stateMu.Lock()
		current.Status = params.Status
		stateMu.Unlock()
		return nil
	}

	var jobMu sync.Mutex
	var latest *model.ParseJob
	createCount := 0
	jobs := &repotest.ObservableJobStore{}
	jobs.CreateFunc = func(_ context.Context, gotLinkID uuid.UUID) (*model.ParseJob, error) {
		jobMu.Lock()
		defer jobMu.Unlock()
		createCount++
		job := &model.ParseJob{ID: uuid.New(), LinkID: gotLinkID, Status: model.JobStatusPending}
		latest = job
		return job, nil
	}
	jobs.GetLatestByLinkIDFunc = func(context.Context, uuid.UUID) (*model.ParseJob, error) {
		jobMu.Lock()
		defer jobMu.Unlock()
		if latest == nil {
			return nil, nil
		}
		copy := *latest
		return &copy, nil
	}

	queue := &submitFakeQueue{}
	submitter := &submitFakeSubmitter{links: links, jobs: jobs}
	svc := newFakeSubmitService(links, submitter, jobs, queue, urllock.NewInProcessURLLocker(), SubmitServiceOptions{})
	req := dto.BatchCreateRequest{Items: []dto.LinkCreateRequest{{URL: rawURL}}}

	var wg sync.WaitGroup
	wg.Add(2)
	responses := make([]dto.BatchSubmitResponse, 2)
	errs := make([]error, 2)
	for i := range 2 {
		go func(index int) {
			defer wg.Done()
			responses[index], errs[index] = svc.Batch(context.Background(), req)
		}(i)
	}
	wg.Wait()

	for i := range responses {
		if errs[i] != nil {
			t.Fatalf("Batch[%d]() error = %v", i, errs[i])
		}
		if len(responses[i].Results) != 1 || responses[i].Results[0].Result == nil || responses[i].Results[0].Result.Status != string(model.LinkStatusPending) {
			t.Fatalf("Batch[%d]() = %#v, want one pending result", i, responses[i])
		}
	}
	jobMu.Lock()
	gotCreateCount := createCount
	jobMu.Unlock()
	if gotCreateCount != 1 {
		t.Fatalf("parse attempts created = %d, want 1", gotCreateCount)
	}
	if len(queue.ids) != 1 {
		t.Fatalf("queued jobs = %d, want 1", len(queue.ids))
	}
}

func TestConcurrentBatchesRepairOrphanPendingLinkOnce(t *testing.T) {
	const rawURL = "https://example.com/batch-orphan-pending"
	linkID := uuid.MustParse("60606060-6060-6060-6060-606060606060")
	current := model.Link{
		ID: linkID, URL: rawURL, SourceKind: "url", SourceKey: rawURL,
		Status: model.LinkStatusPending, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}

	var stateMu sync.Mutex
	links := &repotest.ObservableLinkStore{}
	links.GetByURLFunc = func(context.Context, string) (*model.Link, error) {
		stateMu.Lock()
		copy := current
		stateMu.Unlock()
		return &copy, nil
	}
	links.UpdateStateFunc = func(_ context.Context, params repository.UpdateLinkStateParams) error {
		stateMu.Lock()
		current.Status = params.Status
		stateMu.Unlock()
		return nil
	}

	var jobMu sync.Mutex
	var latest *model.ParseJob
	latestReads := 0
	firstTwoLatestReads := make(chan struct{})
	createCount := 0
	jobs := &repotest.ObservableJobStore{}
	jobs.GetLatestByLinkIDFunc = func(context.Context, uuid.UUID) (*model.ParseJob, error) {
		jobMu.Lock()
		latestReads++
		readNumber := latestReads
		var snapshot *model.ParseJob
		if latest != nil {
			copy := *latest
			snapshot = &copy
		}
		if readNumber == 2 {
			close(firstTwoLatestReads)
		}
		jobMu.Unlock()
		if readNumber == 1 {
			select {
			case <-firstTwoLatestReads:
			case <-time.After(100 * time.Millisecond):
			}
		}
		return snapshot, nil
	}
	jobs.CreateFunc = func(_ context.Context, gotLinkID uuid.UUID) (*model.ParseJob, error) {
		jobMu.Lock()
		defer jobMu.Unlock()
		createCount++
		job := &model.ParseJob{ID: uuid.New(), LinkID: gotLinkID, Status: model.JobStatusPending}
		latest = job
		return job, nil
	}

	queue := &submitFakeQueue{}
	submitter := &submitFakeSubmitter{links: links, jobs: jobs}
	svc := newFakeSubmitService(links, submitter, jobs, queue, urllock.NewInProcessURLLocker(), SubmitServiceOptions{})
	req := dto.BatchCreateRequest{Items: []dto.LinkCreateRequest{{URL: rawURL}}}

	var wg sync.WaitGroup
	wg.Add(2)
	responses := make([]dto.BatchSubmitResponse, 2)
	errs := make([]error, 2)
	for i := range 2 {
		go func(index int) {
			defer wg.Done()
			responses[index], errs[index] = svc.Batch(context.Background(), req)
		}(i)
	}
	wg.Wait()

	for i := range responses {
		if errs[i] != nil {
			t.Fatalf("Batch[%d]() error = %v", i, errs[i])
		}
		if len(responses[i].Results) != 1 || responses[i].Results[0].Result == nil || responses[i].Results[0].Result.JobID == nil {
			t.Fatalf("Batch[%d]() = %#v, want one repaired/reused attempt", i, responses[i])
		}
	}
	if *responses[0].Results[0].Result.JobID != *responses[1].Results[0].Result.JobID {
		t.Fatalf("concurrent responses returned different attempts: %#v vs %#v", responses[0], responses[1])
	}
	jobMu.Lock()
	gotCreateCount := createCount
	jobMu.Unlock()
	if gotCreateCount != 1 {
		t.Fatalf("parse attempts created = %d, want 1", gotCreateCount)
	}
	queue.mu.Lock()
	queued := len(queue.ids)
	queue.mu.Unlock()
	if queued != 1 {
		t.Fatalf("queued jobs = %d, want 1", queued)
	}
}

func TestURLIngestPromotesHistoricalSkeletonToPending(t *testing.T) {
	t.Parallel()

	const rawURL = "https://example.com/ingest-skeleton"
	linkID := uuid.MustParse("40404040-4040-4040-4040-404040404040")
	jobID := uuid.MustParse("50505050-5050-5050-5050-505050505050")
	links := &repotest.ObservableLinkStore{
		ByURL: map[string]*model.Link{rawURL: {
			ID: linkID, URL: rawURL, SourceKind: "url", SourceKey: rawURL,
			Status: model.LinkStatusSkeleton,
		}},
		UpdateStateFunc: func(context.Context, repository.UpdateLinkStateParams) error { return nil },
	}
	jobs := &repotest.ObservableJobStore{CreateResult: &model.ParseJob{
		ID: jobID, LinkID: linkID, Status: model.JobStatusPending,
	}}
	queue := &submitFakeQueue{}
	submitter := &submitFakeSubmitter{links: links, jobs: jobs}
	svc := newFakeIngestService(links, submitter, jobs, queue, &submitFakeLocker{})

	got, err := svc.Ingest(context.Background(), dto.IngestRequest{Sources: []dto.IngestSource{{Kind: "url", URL: rawURL}}})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if got.LinkID != linkID.String() || got.Status != string(model.LinkStatusPending) || got.JobID == nil || *got.JobID != jobID.String() {
		t.Fatalf("Ingest() = %#v, want existing link with a fresh pending job", got)
	}
	if len(queue.ids) != 1 || queue.ids[0] != linkID {
		t.Fatalf("queued ids = %#v, want [%s]", queue.ids, linkID)
	}
}
