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

// The submitFake* types keep only the business-row and queue observations that
// remain part of the durable command contract.

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
// observable Link store and queue ordering assertions.
type submitFakeSubmitter struct {
	mu                 sync.Mutex
	links              *repotest.ObservableLinkStore
	queue              *submitFakeQueue
	requeueCaptures    []*LinkCapture
	libraryKindUpdates []SetLinkLibraryKindCommand
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
	if s.queue != nil {
		s.queue.enqueue(link.ID)
	}
	return LinkSubmissionResult{Link: link, Enqueued: true}, nil
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
	if s.queue != nil {
		if err := s.queue.cancel(command.LinkID); err != nil {
			return LinkSubmissionResult{}, err
		}
		s.queue.enqueue(command.LinkID)
	}
	return LinkSubmissionResult{Enqueued: true}, nil
}

func (s *submitFakeSubmitter) SetLinkLibraryKind(ctx context.Context, command SetLinkLibraryKindCommand) (SetLinkLibraryKindResult, error) {
	s.mu.Lock()
	s.libraryKindUpdates = append(s.libraryKindUpdates, command)
	s.mu.Unlock()
	link, err := s.links.GetByID(ctx, command.LinkID)
	if err != nil || link == nil {
		return SetLinkLibraryKindResult{}, err
	}
	if link.Status == model.LinkStatusPending || link.Status == model.LinkStatusProcessing {
		return SetLinkLibraryKindResult{Status: link.Status}, nil
	}
	_, err = s.RequeueLink(ctx, RequeueLinkCommand{LinkID: command.LinkID})
	return SetLinkLibraryKindResult{Status: model.LinkStatusPending}, err
}

func repositoryCapture(capture LinkCapture) repository.CreateLinkParams {
	return repository.CreateLinkParams{
		URL: capture.URL, SourceKind: capture.SourceKind, SourceKey: capture.SourceKey,
		InputTitle: capture.InputTitle, InputText: capture.InputText, InputHTML: capture.InputHTML,
		InputImages: capture.InputImages, SourceMetadata: capture.SourceMetadata,
		Description: capture.Description, Status: capture.Status, Domain: capture.Domain,
		ContentType: capture.ContentType, PathDepth: capture.PathDepth, ParentPath: capture.ParentPath,
		ParentID: capture.ParentID, RequestedLibraryKind: capture.RequestedLibraryKind,
		UserSelectedLibraryKind: capture.UserSelectedLibraryKind,
	}
}

// newTestSubmitService wires the Observable test surface into the
// SubmitService constructor. Production code passes the same
// *PGXLinkRepository twice; tests pass the Observable link store as reader
// plus a thin in-memory durable command adapter.
func newTestSubmitService(links *repotest.ObservableLinkStore, queue *submitFakeQueue, locker URLLocker) *SubmitService {
	commands := (&submitFakeSubmitter{links: links}).withQueue(queue)
	submit, _ := NewLinkServices(links, commands, locker, SubmitServiceOptions{})
	return submit
}

// newTestIngestService is the IngestService counterpart used by the
// /api/ingest tests after Wave 12.3 M5 split Ingest off SubmitService.
// It accepts the same Link observable and queue fake.
func newTestIngestService(links *repotest.ObservableLinkStore, queue *submitFakeQueue, locker URLLocker) *IngestService {
	commands := (&submitFakeSubmitter{links: links}).withQueue(queue)
	_, ingest := NewLinkServices(links, commands, locker, SubmitServiceOptions{})
	return ingest
}

func newFakeSubmitService(
	links *repotest.ObservableLinkStore,
	commands *submitFakeSubmitter,
	queue *submitFakeQueue,
	locker URLLocker,
	opts SubmitServiceOptions,
) *SubmitService {
	submit, _ := NewLinkServices(links, commands.withQueue(queue), locker, opts)
	return submit
}

func newFakeIngestService(
	links *repotest.ObservableLinkStore,
	commands *submitFakeSubmitter,
	queue *submitFakeQueue,
	locker URLLocker,
) *IngestService {
	_, ingest := NewLinkServices(links, commands.withQueue(queue), locker, SubmitServiceOptions{})
	return ingest
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

func submitResponseEqual(a, b dto.SubmitResponse) bool {
	return a == b
}
