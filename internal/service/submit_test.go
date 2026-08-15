package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/errsafe"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestSubmitServiceSubmitCreatesPendingLinkJobAndQueueEntryForNewURL(t *testing.T) {
	t.Parallel()

	linkStore := &repotest.ObservableLinkStore{
		CreateResult: &model.Link{
			ID:        uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			URL:       "https://example.com/new",
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
	queue := &submitFakeQueue{}
	locker := &submitFakeLocker{}
	service := newTestSubmitService(linkStore, jobStore, queue, locker)

	description := "note"
	submittedURL := "  HTTPS://WWW.Example.com//new/?utm_source=test#frag  "
	identityURL := "https://example.com/new"
	got, err := service.Submit(context.Background(), dto.LinkCreateRequest{
		URL:         submittedURL,
		Description: &description,
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	if len(locker.urls) != 1 || locker.urls[0] != identityURL {
		t.Fatalf("locker urls = %#v, want canonical identity %q once", locker.urls, identityURL)
	}

	if len(linkStore.CreateCalls) != 1 {
		t.Fatalf("create calls = %d, want 1", len(linkStore.CreateCalls))
	}

	createParams := linkStore.CreateCalls[0]
	if createParams.URL != strings.TrimSpace(submittedURL) {
		t.Fatalf("Create URL = %q, want trimmed display URL %q", createParams.URL, strings.TrimSpace(submittedURL))
	}
	if createParams.SourceKey != identityURL {
		t.Fatalf("Create SourceKey = %q, want canonical identity %q", createParams.SourceKey, identityURL)
	}
	if createParams.Status != model.LinkStatusPending {
		t.Fatalf("Create status = %q, want pending", createParams.Status)
	}
	if createParams.Description == nil || *createParams.Description != description {
		t.Fatalf("Create description = %#v, want %q", createParams.Description, description)
	}

	if len(queue.ids) != 1 || queue.ids[0] != linkStore.CreateResult.ID {
		t.Fatalf("queued ids = %#v, want [%s]", queue.ids, linkStore.CreateResult.ID)
	}

	wantJobID := jobStore.CreateResult.ID.String()
	want := dto.SubmitResponse{
		JobID:  &wantJobID,
		LinkID: linkStore.CreateResult.ID.String(),
		Status: string(model.LinkStatusPending),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Submit() = %#v, want %#v", got, want)
	}
}

func TestSubmitServiceSubmitRejectsInvalidURLAsClientError(t *testing.T) {
	t.Parallel()

	service := newTestSubmitService(&repotest.ObservableLinkStore{}, &repotest.ObservableJobStore{}, &submitFakeQueue{}, &submitFakeLocker{})

	_, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "   "})
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) {
		t.Fatalf("Submit() error = %v, want StatusError", err)
	}
	if statusErr.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", statusErr.HTTPStatus(), http.StatusUnprocessableEntity)
	}
}

func TestSubmitServiceSubmitRejectsUnsupportedOrUnsafeTargets(t *testing.T) {
	t.Parallel()

	service := newTestSubmitService(&repotest.ObservableLinkStore{}, &repotest.ObservableJobStore{}, &submitFakeQueue{}, &submitFakeLocker{})

	tests := []struct {
		name string
		url  string
	}{
		{name: "unsupported scheme", url: "ftp://example.com/file.txt"},
		{name: "localhost hostname", url: "http://localhost:8080/private"},
		{name: "loopback ipv4", url: "http://127.0.0.1:8080/private"},
		{name: "private ipv4", url: "http://10.0.0.8/admin"},
		{name: "link local ipv4", url: "http://169.254.169.254/latest/meta-data"},
		{name: "loopback ipv6", url: "http://[::1]/private"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: tt.url})
			var statusErr *httperr.Error
			if !errors.As(err, &statusErr) {
				t.Fatalf("Submit() error = %v, want StatusError", err)
			}
			if statusErr.HTTPStatus() != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d", statusErr.HTTPStatus(), http.StatusUnprocessableEntity)
			}
		})
	}
}

func TestSubmitServiceSubmitAllowsPublicHTTPAndHTTPSURLs(t *testing.T) {
	t.Parallel()

	linkStore := &repotest.ObservableLinkStore{
		CreateResult: &model.Link{
			ID:        uuid.MustParse("12121212-1212-1212-1212-121212121212"),
			URL:       "https://example.com/new",
			Status:    model.LinkStatusPending,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		},
	}
	jobStore := &repotest.ObservableJobStore{
		CreateResult: &model.ParseJob{
			ID:        uuid.MustParse("34343434-3434-3434-3434-343434343434"),
			LinkID:    linkStore.CreateResult.ID,
			Status:    model.JobStatusPending,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		},
	}
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, jobStore, queue, &submitFakeLocker{})

	for _, rawURL := range []string{"https://example.com/new", "http://example.com/new"} {
		if _, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: rawURL}); err != nil {
			t.Fatalf("Submit(%q) error = %v, want success", rawURL, err)
		}
	}
}

func TestSubmitServiceSubmitReturnsExistingPendingJobWithoutReenqueue(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	jobID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	linkStore := &repotest.ObservableLinkStore{
		ByURL: map[string]*model.Link{
			"https://example.com/pending": {
				ID:        linkID,
				URL:       "https://example.com/pending",
				Status:    model.LinkStatusPending,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			},
		},
	}
	jobStore := &repotest.ObservableJobStore{
		LatestByLinkID: map[uuid.UUID]*model.ParseJob{
			linkID: {
				ID:        jobID,
				LinkID:    linkID,
				Status:    model.JobStatusPending,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			},
		},
	}
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, jobStore, queue, &submitFakeLocker{})

	got, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/pending"})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	if len(queue.ids) != 0 {
		t.Fatalf("queued ids = %#v, want none", queue.ids)
	}

	if len(jobStore.CreateCalls) != 0 {
		t.Fatalf("job create calls = %d, want 0", len(jobStore.CreateCalls))
	}

	wantJobID := jobID.String()
	want := dto.SubmitResponse{
		JobID:  &wantJobID,
		LinkID: linkID.String(),
		Status: string(model.LinkStatusPending),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Submit() = %#v, want %#v", got, want)
	}
}

func TestSubmitServiceSubmitReturnsExistingFailedJobWithoutRetry(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("45454545-4545-4545-4545-454545454545")
	jobID := uuid.MustParse("56565656-5656-5656-5656-565656565656")
	errorMessage := "upstream unavailable"
	linkStore := &repotest.ObservableLinkStore{
		ByURL: map[string]*model.Link{
			"https://example.com/failed-save": {
				ID:       linkID,
				URL:      "https://example.com/failed-save",
				Status:   model.LinkStatusFailed,
				ErrorMsg: &errorMessage,
			},
		},
	}
	jobStore := &repotest.ObservableJobStore{
		LatestByLinkID: map[uuid.UUID]*model.ParseJob{
			linkID: {ID: jobID, LinkID: linkID, Status: model.JobStatusFailed, ErrorMsg: &errorMessage},
		},
	}
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, jobStore, queue, &submitFakeLocker{})

	got, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/failed-save"})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	if len(queue.ids) != 0 {
		t.Fatalf("queued ids = %#v, want none; failed retries require Refresh", queue.ids)
	}
	if len(jobStore.CreateCalls) != 0 {
		t.Fatalf("job create calls = %d, want 0", len(jobStore.CreateCalls))
	}
	if len(linkStore.UpdateStateCalls) != 0 {
		t.Fatalf("link state updates = %d, want 0", len(linkStore.UpdateStateCalls))
	}

	wantJobID := jobID.String()
	want := dto.SubmitResponse{
		JobID:  &wantJobID,
		LinkID: linkID.String(),
		Status: string(model.LinkStatusFailed),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Submit() = %#v, want %#v", got, want)
	}
}

// L3 contract: a done link no longer fabricates a parse_jobs row just to give
// the API a non-nil job_id. The response is now {status:"done", job_id:nil}
// when no real job exists, and the existing real job is reported verbatim
// when one does. This test covers the "no real job" branch.
func TestSubmitServiceSubmitDoneLinkWithoutJobReturnsNilJobID(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	linkStore := &repotest.ObservableLinkStore{
		ByURL: map[string]*model.Link{
			"https://example.com/done": {
				ID:        linkID,
				URL:       "https://example.com/done",
				Status:    model.LinkStatusDone,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			},
		},
	}
	jobStore := &repotest.ObservableJobStore{}
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, jobStore, queue, &submitFakeLocker{})

	got, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/done"})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	if len(queue.ids) != 0 {
		t.Fatalf("queued ids = %#v, want none", queue.ids)
	}
	if len(jobStore.CreateCalls) != 0 {
		t.Fatalf("job create calls = %d, want 0 (L3 removed the synthetic done job)", len(jobStore.CreateCalls))
	}
	if len(jobStore.UpdateStateCalls) != 0 {
		t.Fatalf("job update calls = %d, want 0", len(jobStore.UpdateStateCalls))
	}

	want := dto.SubmitResponse{
		JobID:  nil,
		LinkID: linkID.String(),
		Status: string(model.LinkStatusDone),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Submit() = %#v, want %#v", got, want)
	}
}

func TestSubmitServiceRefreshCreatesNewPendingJobAndClearsFailure(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	jobID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	errMsg := "previous failure"
	linkStore := &repotest.ObservableLinkStore{
		ByID: map[uuid.UUID]*model.Link{
			linkID: {
				ID:        linkID,
				URL:       "https://example.com/failed",
				Status:    model.LinkStatusFailed,
				ErrorMsg:  &errMsg,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			},
		},
		// Refresh on a failed link transitions it back to pending; the
		// observable now panics on unhooked writes so this hook makes the
		// "happy path persists" intent explicit. UpdateStateCalls is still
		// populated by the wrapper for the assertion below.
		UpdateStateFunc: func(context.Context, repository.UpdateLinkStateParams) error { return nil },
	}
	jobStore := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{
				ID:        jobID,
				LinkID:    linkID,
				Status:    model.JobStatusPending,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
	}
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, jobStore, queue, &submitFakeLocker{})

	got, err := service.Refresh(context.Background(), linkID.String())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	if len(linkStore.UpdateStateCalls) != 1 {
		t.Fatalf("update state calls = %d, want 1", len(linkStore.UpdateStateCalls))
	}

	update := linkStore.UpdateStateCalls[0]
	if update.ID != linkID || update.Status != model.LinkStatusPending || update.ErrorMsg != nil {
		t.Fatalf("UpdateState() = %#v, want pending with cleared error", update)
	}

	if len(queue.ids) != 1 || queue.ids[0] != linkID {
		t.Fatalf("queued ids = %#v, want [%s]", queue.ids, linkID)
	}
	if len(queue.cancelled) != 1 || queue.cancelled[0] != linkID {
		t.Fatalf("cancelled ids = %#v, want [%s]", queue.cancelled, linkID)
	}
	if got := strings.Join(queue.callOrder, ","); got != "cancel,enqueue" {
		t.Fatalf("queue call order = %q, want cancel,enqueue", got)
	}

	wantJobID := jobID.String()
	want := dto.SubmitResponse{
		JobID:  &wantJobID,
		LinkID: linkID.String(),
		Status: string(model.LinkStatusPending),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Refresh() = %#v, want %#v", got, want)
	}
}

// TestSubmitServiceRefreshHonorsCooldown pins M-1: a Refresh on a
// terminal link inside the cooldown window must return 429 +
// Retry-After, and must NOT spawn a new parse_jobs row or update the
// link state. The cooldown only fires for terminal-state links —
// pending/processing follow-ups are still served from the in-flight
// job, so they're orthogonal to this guard.
func TestSubmitServiceRefreshHonorsCooldown(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	jobID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")

	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-15 * time.Second) // inside the 60s default
	linkStore := &repotest.ObservableLinkStore{
		ByID: map[uuid.UUID]*model.Link{
			linkID: {
				ID:        linkID,
				URL:       "https://example.com/done",
				Status:    model.LinkStatusDone,
				CreatedAt: now.Add(-time.Hour),
				UpdatedAt: recent,
			},
		},
	}
	jobStore := &repotest.ObservableJobStore{
		LatestByLinkID: map[uuid.UUID]*model.ParseJob{
			linkID: {
				ID:        jobID,
				LinkID:    linkID,
				Status:    model.JobStatusDone,
				CreatedAt: recent,
				UpdatedAt: recent,
			},
		},
	}
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, jobStore, queue, &submitFakeLocker{})
	service.now = func() time.Time { return now }

	_, err := service.Refresh(context.Background(), linkID.String())
	if err == nil {
		t.Fatal("Refresh() error = nil, want 429 cooldown error")
	}
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) {
		t.Fatalf("Refresh() error = %T, want *httperr.Error", err)
	}
	if statusErr.HTTPStatus() != 429 {
		t.Fatalf("status = %d, want 429", statusErr.HTTPStatus())
	}
	if statusErr.RetryAfterSeconds() <= 0 || statusErr.RetryAfterSeconds() > 60 {
		t.Fatalf("Retry-After = %d, want in (0, 60]", statusErr.RetryAfterSeconds())
	}
	if len(linkStore.UpdateStateCalls) != 0 {
		t.Fatal("cooldown should not write link state")
	}
	if len(queue.ids) != 0 {
		t.Fatal("cooldown should not enqueue")
	}
}

// TestSubmitServiceRefreshAllowsAfterCooldown is the happy-path
// counterpart: once the cooldown window has passed, Refresh proceeds
// normally.
func TestSubmitServiceRefreshAllowsAfterCooldown(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	jobID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	newJobID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	old := now.Add(-5 * time.Minute)
	linkStore := &repotest.ObservableLinkStore{
		ByID: map[uuid.UUID]*model.Link{
			linkID: {ID: linkID, URL: "https://example.com/old", Status: model.LinkStatusDone, CreatedAt: now.Add(-time.Hour), UpdatedAt: old},
		},
		UpdateStateFunc: func(context.Context, repository.UpdateLinkStateParams) error { return nil },
	}
	jobStore := &repotest.ObservableJobStore{
		LatestByLinkID: map[uuid.UUID]*model.ParseJob{
			linkID: {ID: jobID, LinkID: linkID, Status: model.JobStatusDone, CreatedAt: old, UpdatedAt: old},
		},
		CreateFunc: func(_ context.Context, id uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{ID: newJobID, LinkID: id, Status: model.JobStatusPending, CreatedAt: now, UpdatedAt: now}, nil
		},
	}
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, jobStore, queue, &submitFakeLocker{})
	service.now = func() time.Time { return now }

	resp, err := service.Refresh(context.Background(), linkID.String())
	if err != nil {
		t.Fatalf("Refresh() error = %v, want nil after cooldown", err)
	}
	if resp.JobID == nil || *resp.JobID != newJobID.String() {
		t.Fatalf("resp.JobID = %v, want %s", resp.JobID, newJobID)
	}
	if len(queue.ids) != 1 {
		t.Fatalf("queue ids = %v, want one enqueue", queue.ids)
	}
}

func TestSubmitServiceRefreshRejectsInvalidIDAsClientError(t *testing.T) {
	t.Parallel()

	service := newTestSubmitService(&repotest.ObservableLinkStore{}, &repotest.ObservableJobStore{}, &submitFakeQueue{}, &submitFakeLocker{})

	_, err := service.Refresh(context.Background(), "not-a-uuid")
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) {
		t.Fatalf("Refresh() error = %v, want StatusError", err)
	}
	if statusErr.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", statusErr.HTTPStatus(), http.StatusBadRequest)
	}
}

func TestSubmitServiceBatchReturnsPerItemResponsesInOrder(t *testing.T) {
	t.Parallel()

	firstLinkID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	secondLinkID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	firstJobID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	secondJobID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	linkStore := &repotest.ObservableLinkStore{
		ByURL: map[string]*model.Link{
			"https://example.com/existing": {
				ID:        secondLinkID,
				URL:       "https://example.com/existing",
				Status:    model.LinkStatusProcessing,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			},
		},
		CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
			return &model.Link{
				ID:        firstLinkID,
				URL:       params.URL,
				Status:    params.Status,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
	}
	jobStore := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{
				ID:        firstJobID,
				LinkID:    linkID,
				Status:    model.JobStatusPending,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
		LatestByLinkID: map[uuid.UUID]*model.ParseJob{
			secondLinkID: {
				ID:        secondJobID,
				LinkID:    secondLinkID,
				Status:    model.JobStatusProcessing,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			},
		},
	}
	queue := &submitFakeQueue{}
	locker := &submitFakeLocker{}
	service := newTestSubmitService(linkStore, jobStore, queue, locker)

	newSubmittedURL := "  HTTPS://WWW.Example.com//new/?utm_source=batch#frag  "
	got, err := service.Batch(context.Background(), dto.BatchCreateRequest{
		Items: []dto.LinkCreateRequest{
			{URL: newSubmittedURL},
			{URL: "https://example.com/existing"},
		},
	})
	if err != nil {
		t.Fatalf("Batch() error = %v", err)
	}

	if len(got.Results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(got.Results))
	}
	if len(linkStore.CreateCalls) != 1 {
		t.Fatalf("create calls = %d, want 1", len(linkStore.CreateCalls))
	}
	if capture := linkStore.CreateCalls[0]; capture.URL != strings.TrimSpace(newSubmittedURL) || capture.SourceKey != "https://example.com/new" {
		t.Fatalf("batch capture = URL %q SourceKey %q, want display %q identity %q", capture.URL, capture.SourceKey, strings.TrimSpace(newSubmittedURL), "https://example.com/new")
	}
	if len(locker.urls) != 3 || locker.urls[0] != "https://example.com/new" || locker.urls[1] != "https://example.com/existing" || locker.urls[2] != "https://example.com/existing" {
		t.Fatalf("batch lock URLs = %#v, want canonical identities", locker.urls)
	}

	firstJobIDStr := firstJobID.String()
	secondJobIDStr := secondJobID.String()
	wantResults := []dto.BatchItemResponse{
		{
			Result: &dto.SubmitResponse{JobID: &firstJobIDStr, LinkID: firstLinkID.String(), Status: string(model.LinkStatusPending)},
		},
		{
			Result: &dto.SubmitResponse{JobID: &secondJobIDStr, LinkID: secondLinkID.String(), Status: string(model.LinkStatusProcessing)},
		},
	}
	for i, want := range wantResults {
		gotItem := got.Results[i]
		if gotItem.Error != "" {
			t.Fatalf("results[%d].Error = %q, want empty", i, gotItem.Error)
		}
		if gotItem.Result == nil {
			t.Fatalf("results[%d].Result = nil, want non-nil", i)
		}
		if !submitResponseEqual(*gotItem.Result, *want.Result) {
			t.Fatalf("results[%d].Result = %#v, want %#v", i, *gotItem.Result, *want.Result)
		}
	}
}

// TestSubmitServiceBatchPartialSuccessReportsPerItemFailures locks in the H7
// contract: a per-item Submit failure (here the second item's URL is
// rejected by validateURL) is captured in BatchItemResponse.Error rather
// than aborting the batch. The per-item outcomes appear in the original
// request order (M4 dropped the explicit Index field; the array index
// itself is the contract).
func TestSubmitServiceBatchPartialSuccessReportsPerItemFailures(t *testing.T) {
	t.Parallel()

	firstLinkID := uuid.MustParse("11111111-aaaa-aaaa-aaaa-111111111111")
	thirdLinkID := uuid.MustParse("33333333-aaaa-aaaa-aaaa-333333333333")
	firstJobID := uuid.MustParse("11111111-bbbb-bbbb-bbbb-111111111111")
	thirdJobID := uuid.MustParse("33333333-bbbb-bbbb-bbbb-333333333333")

	// Hooks fire from concurrent Submit goroutines (Batch fans out via
	// errgroup), so the seq counters must be guarded. A plain int++ races
	// under -race even though the test only validates aggregate counts.
	var hookMu sync.Mutex
	createSeq := 0
	jobSeq := 0
	linkStore := &repotest.ObservableLinkStore{
		CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
			hookMu.Lock()
			createSeq++
			seq := createSeq
			hookMu.Unlock()
			id := firstLinkID
			if seq == 2 {
				id = thirdLinkID
			}
			return &model.Link{
				ID:        id,
				URL:       params.URL,
				Status:    params.Status,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
	}
	jobStore := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
			hookMu.Lock()
			jobSeq++
			seq := jobSeq
			hookMu.Unlock()
			id := firstJobID
			if seq == 2 {
				id = thirdJobID
			}
			return &model.ParseJob{
				ID:        id,
				LinkID:    linkID,
				Status:    model.JobStatusPending,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
	}
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, jobStore, queue, &submitFakeLocker{})

	got, err := service.Batch(context.Background(), dto.BatchCreateRequest{
		Items: []dto.LinkCreateRequest{
			{URL: "https://example.com/ok-first"},
			{URL: "ftp://example.com/bad-scheme"},
			{URL: "https://example.com/ok-third"},
		},
	})
	if err != nil {
		t.Fatalf("Batch() error = %v, want partial-success outcome (no envelope error)", err)
	}

	if len(got.Results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(got.Results))
	}
	successCount, failureCount := 0, 0
	for _, item := range got.Results {
		if item.Error == "" {
			successCount++
		} else {
			failureCount++
		}
	}
	if successCount != 2 || failureCount != 1 {
		t.Fatalf("Batch() success=%d failure=%d, want 2/1", successCount, failureCount)
	}

	for i, item := range got.Results {
		if i == 1 {
			// Failure entry: no result, error message present and sanitized.
			if item.Result != nil {
				t.Fatalf("results[1].Result = %#v, want nil for failed item", item.Result)
			}
			if item.Error == "" {
				t.Fatalf("results[1].Error = empty, want sanitized failure message")
			}
			continue
		}
		if item.Result == nil {
			t.Fatalf("results[%d].Result = nil, want non-nil for success item", i)
		}
		if item.Error != "" {
			t.Fatalf("results[%d].Error = %q, want empty for success item", i, item.Error)
		}
		if item.Result.Status != string(model.LinkStatusPending) {
			t.Fatalf("results[%d].Result.Status = %q, want pending", i, item.Result.Status)
		}
		if item.Result.JobID == nil {
			t.Fatalf("results[%d].Result.JobID = nil, want non-nil for pending item", i)
		}
	}
}

func TestSubmitServiceBatchRejectsOversizedFieldsPerItem(t *testing.T) {
	t.Parallel()

	tooLongDescription := strings.Repeat("x", maxLinkDescriptionLength+1)
	linkStore := &repotest.ObservableLinkStore{
		CreateFunc: func(context.Context, repository.CreateLinkParams) (*model.Link, error) {
			t.Fatal("invalid batch items must not reach the repository")
			return nil, nil
		},
	}
	service := newTestSubmitService(
		linkStore,
		&repotest.ObservableJobStore{},
		&submitFakeQueue{},
		&submitFakeLocker{},
	)

	got, err := service.Batch(context.Background(), dto.BatchCreateRequest{Items: []dto.LinkCreateRequest{
		{URL: "https://example.com/" + strings.Repeat("a", maxLinkURLLength)},
		{URL: "https://example.com/description", Description: &tooLongDescription},
	}})
	if err != nil {
		t.Fatalf("Batch() error = %v, want per-item errors", err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(got.Results))
	}
	if got.Results[0].Error != "url exceeds length limit" {
		t.Fatalf("URL error = %q, want length error", got.Results[0].Error)
	}
	if got.Results[1].Error != "description exceeds length limit" {
		t.Fatalf("description error = %q, want length error", got.Results[1].Error)
	}
}

func TestSubmitServiceBatchRejectsEmptyAndOversizedRequests(t *testing.T) {
	t.Parallel()

	service := newTestSubmitService(&repotest.ObservableLinkStore{}, &repotest.ObservableJobStore{}, &submitFakeQueue{}, &submitFakeLocker{})

	tests := []struct {
		name string
		req  dto.BatchCreateRequest
		want string
	}{
		{
			name: "empty",
			req:  dto.BatchCreateRequest{},
			want: "batch items are required",
		},
		{
			name: "too many",
			req: dto.BatchCreateRequest{
				Items: make([]dto.LinkCreateRequest, defaultBatchSubmitLimit+1),
			},
			want: "batch items exceed limit",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := service.Batch(context.Background(), tt.req)
			var statusErr *httperr.Error
			if !errors.As(err, &statusErr) {
				t.Fatalf("Batch() error = %v, want StatusError", err)
			}
			if statusErr.HTTPStatus() != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d", statusErr.HTTPStatus(), http.StatusUnprocessableEntity)
			}
			if statusErr.HTTPMessage() != tt.want {
				t.Fatalf("message = %q, want %q", statusErr.HTTPMessage(), tt.want)
			}
		})
	}
}

// TestBatchItemErrorMessageCollapsesUnknownCategoryToGenericString locks in
// the privacy guarantee for /api/links/batch responses: errors that
// errsafe.ClassifyError cannot match against a sentinel (the "unknown"
// bucket — typically raw repository / pgx / driver text) must NOT surface
// their message text to API clients, because that text may embed
// connection strings, table names, or row data. They collapse to a
// fixed "submission failed" string instead.
//
// StatusError messages stay as-is (they are author-controlled validation
// copy), and classified errors keep their "<category>: <summary>" shape
// so callers can still distinguish e.g. timeout from network.
func TestBatchItemErrorMessageCollapsesUnknownCategoryToGenericString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil returns empty",
			err:  nil,
			want: "",
		},
		{
			name: "StatusError surfaces author-controlled message",
			err:  httperr.New(http.StatusUnprocessableEntity, "url is required"),
			want: "url is required",
		},
		{
			name: "unknown category collapses to generic submission failed",
			// Crafted to miss every Classify substring keyword (timeout,
			// connection refused, dial tcp, status=, decode, parse, …) so
			// it falls into the residual "unknown" bucket. Stand-in for the
			// kind of opaque pgx / driver text we don't want surfaced.
			err:  errors.New("ERROR 42P01 relation jobs_internal does not exist (SQLSTATE)"),
			want: "submission failed",
		},
		{
			name: "classified error keeps category prefix and summary",
			err:  fmt.Errorf("upstream returned 502: %w", errsafe.ErrUpstreamHTTP),
			want: "upstream_http: upstream returned 502: upstream http error",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := batchItemErrorMessage(tt.err)
			if got != tt.want {
				t.Fatalf("batchItemErrorMessage(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
