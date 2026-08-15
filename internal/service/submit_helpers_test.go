package service

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

// repotest.ObservableLinkStore / ObservableJobStore replace the previous
// per-test spy fakes (submitFakeLinkStore / submitFakeJobStore). Both
// observables are mu-protected internally so the Batch concurrent path
// (errgroup-driven Submit fan-out) remains race-free.
//
// The submitFake* types kept here (commands / locker) carry
// their own bookkeeping because their surfaces are tiny and the
// observable abstraction is link/job-store specific.

type submitFakeQueue struct {
	mu          sync.Mutex
	ids         []uuid.UUID
	cancelled   []uuid.UUID
	callOrder   []string
	cancelError error
}

func (s *submitFakeQueue) enqueue(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, id)
	s.callOrder = append(s.callOrder, "enqueue")
}

func (s *submitFakeQueue) cancel(linkID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = append(s.cancelled, linkID)
	s.callOrder = append(s.callOrder, "cancel")
	return s.cancelError
}

type submitFakeLocker struct {
	mu   sync.Mutex
	urls []string
}

func (s *submitFakeLocker) WithURL(ctx context.Context, rawURL string, fn func(context.Context) error) error {
	s.mu.Lock()
	s.urls = append(s.urls, rawURL)
	s.mu.Unlock()
	return fn(ctx)
}

func (s *submitFakeLocker) WithURLs(ctx context.Context, rawURLs []string, fn func(context.Context) error) error {
	s.mu.Lock()
	s.urls = append(s.urls, rawURLs...)
	s.mu.Unlock()
	return fn(ctx)
}

// submitFakeSubmitter is the in-memory adapter for LinkSubmissionCommands.
// Tests cross the same application seam as production while retaining the
// observable link/job stores and queue ordering assertions.
type submitFakeSubmitter struct {
	mu              sync.Mutex
	links           *repotest.ObservableLinkStore
	jobs            *repotest.ObservableJobStore
	queue           *submitFakeQueue
	requeueCaptures []*LinkCapture
	intentUpdates   []UpdateLinkIntentCommand
}

func (s *submitFakeSubmitter) withQueue(queue *submitFakeQueue) *submitFakeSubmitter {
	s.queue = queue
	return s
}

func (s *submitFakeSubmitter) SubmitLink(ctx context.Context, command SubmitLinkCommand) (LinkSubmissionResult, error) {
	link, err := s.links.Create(ctx, repositoryCapture(command.Capture))
	if err != nil {
		return LinkSubmissionResult{}, err
	}
	job, err := s.jobs.Create(ctx, link.ID)
	if err != nil {
		return LinkSubmissionResult{}, err
	}
	if s.queue != nil {
		s.queue.enqueue(link.ID)
	}
	return LinkSubmissionResult{Link: link, Job: job, Inserted: true}, nil
}

func (s *submitFakeSubmitter) RequeueLink(ctx context.Context, command RequeueLinkCommand) (LinkSubmissionResult, error) {
	s.mu.Lock()
	if command.Capture == nil {
		s.requeueCaptures = append(s.requeueCaptures, nil)
	} else {
		captureCopy := *command.Capture
		s.requeueCaptures = append(s.requeueCaptures, &captureCopy)
	}
	s.mu.Unlock()
	if err := s.links.UpdateState(ctx, repository.UpdateLinkStateParams{
		ID: command.LinkID, Status: model.LinkStatusPending,
	}); err != nil {
		return LinkSubmissionResult{}, err
	}
	job, err := s.jobs.Create(ctx, command.LinkID)
	if err != nil {
		return LinkSubmissionResult{}, err
	}
	if s.queue != nil {
		if err := s.queue.cancel(command.LinkID); err != nil {
			return LinkSubmissionResult{}, err
		}
		s.queue.enqueue(command.LinkID)
	}
	return LinkSubmissionResult{Job: job, Inserted: true}, nil
}

func (s *submitFakeSubmitter) UpdateLinkIntent(ctx context.Context, command UpdateLinkIntentCommand) (UpdateLinkIntentResult, error) {
	s.mu.Lock()
	s.intentUpdates = append(s.intentUpdates, command)
	s.mu.Unlock()
	link, err := s.links.GetByID(ctx, command.LinkID)
	if err != nil || link == nil {
		return UpdateLinkIntentResult{}, err
	}
	if link.Status == model.LinkStatusPending || link.Status == model.LinkStatusProcessing {
		return UpdateLinkIntentResult{Status: link.Status}, nil
	}
	result, err := s.RequeueLink(ctx, RequeueLinkCommand{LinkID: command.LinkID})
	return UpdateLinkIntentResult{Status: model.LinkStatusPending, Job: result.Job}, err
}

// SubmitBatch mirrors the production multi-row INSERT contract against
// the observable test stores. The production path uses ON CONFLICT
// (source_key) to either insert a fresh row or return the existing
// one; the fake models that by consulting the canonical SourceKey and
// then the legacy URL map under that same identity.
//
// When a URL already has a row in ByURL the fake returns it with
// Inserted=false and skips the job Create. Otherwise it falls through
// to the per-item Create/Create pair. This is what lets existing
// SubmitService tests preconfigure ByURL to assert the existing-link
// branch (TestSubmitServiceBatchReturnsPerItemResponsesInOrder relies
// on this).
func (s *submitFakeSubmitter) SubmitLinksBatch(ctx context.Context, command SubmitLinksBatchCommand) ([]LinkSubmissionResult, error) {
	if len(command.Captures) == 0 {
		return nil, nil
	}
	results := make([]LinkSubmissionResult, 0, len(command.Captures))
	for _, capture := range command.Captures {
		identityKey := capture.SourceKey
		if identityKey == "" {
			identityKey = capture.URL
		}
		existing, _ := s.links.GetBySourceKey(ctx, identityKey)
		if existing == nil {
			existing, _ = s.links.GetByURL(ctx, identityKey)
		}
		if existing != nil {
			linkCopy := *existing
			results = append(results, LinkSubmissionResult{
				Link:     &linkCopy,
				Inserted: false,
			})
			continue
		}
		link, err := s.links.Create(ctx, repositoryCapture(capture))
		if err != nil {
			return nil, err
		}
		job, err := s.jobs.Create(ctx, link.ID)
		if err != nil {
			return nil, err
		}
		results = append(results, LinkSubmissionResult{
			Link:     link,
			Job:      job,
			Inserted: true,
		})
		if s.queue != nil {
			s.queue.enqueue(link.ID)
		}
	}
	return results, nil
}

func repositoryCapture(capture LinkCapture) repository.CreateLinkParams {
	return repository.CreateLinkParams{
		URL: capture.URL, SourceKind: capture.SourceKind, SourceKey: capture.SourceKey,
		InputTitle: capture.InputTitle, InputText: capture.InputText, InputHTML: capture.InputHTML,
		InputImages: capture.InputImages, SourceMetadata: capture.SourceMetadata,
		Description: capture.Description, Status: capture.Status, Domain: capture.Domain,
		ContentType: capture.ContentType, PathDepth: capture.PathDepth, ParentPath: capture.ParentPath,
		ParentID: capture.ParentID, RequestedLibraryKind: capture.RequestedLibraryKind,
		RequestedLibraryKindSource: capture.RequestedLibraryKindSource,
		PredictedLibraryKind:       capture.PredictedLibraryKind,
	}
}

// newTestSubmitService wires the Observable test surface into the
// SubmitService constructor. Production code passes the same
// *PGXLinkRepository twice; tests pass the Observable link store as reader
// plus a thin adapter that fans the
// SubmitNew call out to both stores.
func newTestSubmitService(links *repotest.ObservableLinkStore, jobs *repotest.ObservableJobStore, queue *submitFakeQueue, locker URLLocker) *SubmitService {
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(queue)
	return NewSubmitService(links, jobs, commands, locker, SubmitServiceOptions{})
}

// newTestIngestService is the IngestService counterpart used by the
// /api/ingest tests after Wave 12.3 M5 split Ingest off SubmitService.
// It accepts the same Observable fakes so an ingest test can keep
// asserting against linkStore.CreateCalls / jobStore.CreateCalls without
// any rewrite — only the receiver type changed.
func newTestIngestService(links *repotest.ObservableLinkStore, jobs *repotest.ObservableJobStore, queue *submitFakeQueue, locker URLLocker) *IngestService {
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(queue)
	return NewIngestService(links, jobs, commands, locker)
}

func newFakeSubmitService(
	links *repotest.ObservableLinkStore,
	commands *submitFakeSubmitter,
	jobs *repotest.ObservableJobStore,
	queue *submitFakeQueue,
	locker URLLocker,
	opts SubmitServiceOptions,
) *SubmitService {
	return NewSubmitService(links, jobs, commands.withQueue(queue), locker, opts)
}

func newFakeIngestService(
	links *repotest.ObservableLinkStore,
	commands *submitFakeSubmitter,
	jobs *repotest.ObservableJobStore,
	queue *submitFakeQueue,
	locker URLLocker,
) *IngestService {
	return NewIngestService(links, jobs, commands.withQueue(queue), locker)
}

func assertStringFieldIfPresent(t *testing.T, params repository.CreateLinkParams, fieldName string, want string) {
	t.Helper()

	got := readStringField(params, fieldName)
	if got == "" {
		return
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", fieldName, got, want)
	}
}

func readStringField(params repository.CreateLinkParams, fieldName string) string {
	value := reflect.ValueOf(params)
	field := value.FieldByName(fieldName)
	if !field.IsValid() {
		return ""
	}
	switch field.Kind() {
	case reflect.String:
		return field.String()
	case reflect.Pointer:
		if field.IsNil() || field.Elem().Kind() != reflect.String {
			return ""
		}
		return field.Elem().String()
	default:
		return ""
	}
}

// submitResponseEqual compares two SubmitResponse values for equality with
// pointer-aware semantics on JobID. The L3 cleanup made JobID a *string so a
// raw == comparison would compare pointer identity instead of the underlying
// values; this helper dereferences when both sides are non-nil.
func submitResponseEqual(a, b dto.SubmitResponse) bool {
	if a.LinkID != b.LinkID || a.Status != b.Status {
		return false
	}
	switch {
	case a.JobID == nil && b.JobID == nil:
		return true
	case a.JobID == nil || b.JobID == nil:
		return false
	default:
		return *a.JobID == *b.JobID
	}
}
