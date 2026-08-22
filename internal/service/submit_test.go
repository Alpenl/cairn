package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestSubmitServiceSubmitCreatesPendingLinkAndQueueEntryForNewURL(t *testing.T) {
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
	queue := &submitFakeQueue{}
	locker := &submitFakeLocker{}
	service := newTestSubmitService(linkStore, queue, locker)

	description := "note"
	submittedURL := "  HTTPS://WWW.Example.com//new/?utm_source=test#frag  "
	identityURL := "https://example.com/new"
	got, err := service.Submit(context.Background(), dto.LinkCreateRequest{
		URL:         submittedURL,
		Destination: captureDestinationLibrary,
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

	want := dto.SubmitResponse{
		LinkID: linkStore.CreateResult.ID.String(),
		Status: string(model.LinkStatusPending),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Submit() = %#v, want %#v", got, want)
	}
}

func TestSubmitServiceSubmitRejectsInvalidURLAsClientError(t *testing.T) {
	t.Parallel()

	service := newTestSubmitService(&repotest.ObservableLinkStore{}, &submitFakeQueue{}, &submitFakeLocker{})

	_, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "   "})
	var statusErr *problem.Error
	if !errors.As(err, &statusErr) {
		t.Fatalf("Submit() error = %v, want StatusError", err)
	}
	if problemHTTPStatus(statusErr) != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", problemHTTPStatus(statusErr), http.StatusUnprocessableEntity)
	}
}

func TestSubmitServiceSubmitRejectsUnsupportedOrUnsafeTargets(t *testing.T) {
	t.Parallel()

	service := newTestSubmitService(&repotest.ObservableLinkStore{}, &submitFakeQueue{}, &submitFakeLocker{})

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
			var statusErr *problem.Error
			if !errors.As(err, &statusErr) {
				t.Fatalf("Submit() error = %v, want StatusError", err)
			}
			if problemHTTPStatus(statusErr) != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d", problemHTTPStatus(statusErr), http.StatusUnprocessableEntity)
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
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, queue, &submitFakeLocker{})

	for _, rawURL := range []string{"https://example.com/new", "http://example.com/new"} {
		if _, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: rawURL, Destination: captureDestinationLibrary}); err != nil {
			t.Fatalf("Submit(%q) error = %v, want success", rawURL, err)
		}
	}
}

func TestSubmitServiceSubmitReturnsExistingPendingLinkWithoutReenqueue(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
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
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, queue, &submitFakeLocker{})

	got, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/pending", Destination: captureDestinationLibrary})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	if len(queue.ids) != 0 {
		t.Fatalf("queued ids = %#v, want none", queue.ids)
	}

	want := dto.SubmitResponse{
		LinkID: linkID.String(),
		Status: string(model.LinkStatusPending),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Submit() = %#v, want %#v", got, want)
	}
}

func TestSubmitServiceSubmitReturnsExistingFailedLinkWithoutRetry(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("45454545-4545-4545-4545-454545454545")
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
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, queue, &submitFakeLocker{})

	got, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/failed-save", Destination: captureDestinationLibrary})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	if len(queue.ids) != 0 {
		t.Fatalf("queued ids = %#v, want none; failed retries require Refresh", queue.ids)
	}
	if len(linkStore.UpdateStateCalls) != 0 {
		t.Fatalf("link state updates = %d, want 0", len(linkStore.UpdateStateCalls))
	}

	want := dto.SubmitResponse{
		LinkID: linkID.String(),
		Status: string(model.LinkStatusFailed),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Submit() = %#v, want %#v", got, want)
	}
}

func TestSubmitServiceSubmitReturnsExistingDoneLink(t *testing.T) {
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
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, queue, &submitFakeLocker{})

	got, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/done", Destination: captureDestinationLibrary})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	if len(queue.ids) != 0 {
		t.Fatalf("queued ids = %#v, want none", queue.ids)
	}
	want := dto.SubmitResponse{
		LinkID: linkID.String(),
		Status: string(model.LinkStatusDone),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Submit() = %#v, want %#v", got, want)
	}
}

func TestSubmitServiceRefreshRequeuesAndClearsFailure(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
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
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, queue, &submitFakeLocker{})

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

	want := dto.SubmitResponse{
		LinkID: linkID.String(),
		Status: string(model.LinkStatusPending),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Refresh() = %#v, want %#v", got, want)
	}
}

// A terminal link inside the cooldown window must return 429 with
// Retry-After and leave both link state and the queue untouched.
func TestSubmitServiceRefreshHonorsCooldown(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-15 * time.Second) // inside the 60s default
	linkStore := &repotest.ObservableLinkStore{
		ByID: map[uuid.UUID]*model.Link{
			linkID: {
				ID:               linkID,
				URL:              "https://example.com/done",
				Status:           model.LinkStatusDone,
				FirstCollectedAt: recent,
				CreatedAt:        now.Add(-time.Hour),
				UpdatedAt:        recent,
			},
		},
	}
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, queue, &submitFakeLocker{})
	service.now = func() time.Time { return now }

	_, err := service.Refresh(context.Background(), linkID.String())
	if err == nil {
		t.Fatal("Refresh() error = nil, want 429 cooldown error")
	}
	var statusErr *problem.Error
	if !errors.As(err, &statusErr) {
		t.Fatalf("Refresh() error = %T, want *problem.Error", err)
	}
	if problemHTTPStatus(statusErr) != 429 {
		t.Fatalf("status = %d, want 429", problemHTTPStatus(statusErr))
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

	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	old := now.Add(-5 * time.Minute)
	linkStore := &repotest.ObservableLinkStore{
		ByID: map[uuid.UUID]*model.Link{
			linkID: {
				ID:               linkID,
				URL:              "https://example.com/old",
				Status:           model.LinkStatusDone,
				FirstCollectedAt: old,
				CreatedAt:        now.Add(-time.Hour),
				UpdatedAt:        old,
			},
		},
		UpdateStateFunc: func(context.Context, repository.UpdateLinkStateParams) error { return nil },
	}
	queue := &submitFakeQueue{}
	service := newTestSubmitService(linkStore, queue, &submitFakeLocker{})
	service.now = func() time.Time { return now }

	resp, err := service.Refresh(context.Background(), linkID.String())
	if err != nil {
		t.Fatalf("Refresh() error = %v, want nil after cooldown", err)
	}
	if resp.LinkID != linkID.String() || resp.Status != string(model.LinkStatusPending) {
		t.Fatalf("Refresh() = %#v, want pending link %s", resp, linkID)
	}
	if len(queue.ids) != 1 {
		t.Fatalf("queue ids = %v, want one enqueue", queue.ids)
	}
}

func TestSubmitServiceRefreshRejectsInvalidIDAsClientError(t *testing.T) {
	t.Parallel()

	service := newTestSubmitService(&repotest.ObservableLinkStore{}, &submitFakeQueue{}, &submitFakeLocker{})

	_, err := service.Refresh(context.Background(), "not-a-uuid")
	var statusErr *problem.Error
	if !errors.As(err, &statusErr) {
		t.Fatalf("Refresh() error = %v, want StatusError", err)
	}
	if problemHTTPStatus(statusErr) != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", problemHTTPStatus(statusErr), http.StatusBadRequest)
	}
}
