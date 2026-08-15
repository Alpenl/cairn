package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

type inboxCaptureWriterFake struct {
	items   map[string]model.ReaderInbox
	created []model.ReaderInbox
	job     *model.ReaderInboxJob
	begins  int
}

func (f *inboxCaptureWriterFake) BeginInboxResummarizeJob(_ context.Context, inboxID uuid.UUID, revision int64) (*model.ReaderInboxJob, bool, error) {
	f.begins++
	if f.job != nil {
		return f.job, false, nil
	}
	f.job = &model.ReaderInboxJob{ID: uuid.New(), InboxID: inboxID, ExpectedMetadataRevision: revision, Status: "queued"}
	return f.job, true, nil
}

func (f *inboxCaptureWriterFake) FailInboxJob(_ context.Context, jobID uuid.UUID, _ string) error {
	if f.job != nil && f.job.ID == jobID {
		f.job.Status = "failed"
	}
	return nil
}

type inboxProposalSchedulerFake struct{ args []ReaderInboxSummaryJobArgs }

func (f *inboxProposalSchedulerFake) EnqueueReaderInboxSummary(_ context.Context, args ReaderInboxSummaryJobArgs) error {
	f.args = append(f.args, args)
	return nil
}

type inboxProposalCommandsFake struct {
	job         *model.ReaderInboxJob
	ensureCalls []EnsureInboxProposalCommand
}

func (f *inboxProposalCommandsFake) CreateInboxProposal(_ context.Context, command CreateInboxProposalCommand) (InboxProposalResult, error) {
	item := command.Inbox
	if item.ID == uuid.Nil {
		item.ID = uuid.New()
	}
	return InboxProposalResult{Inbox: &item, Job: f.job}, nil
}

func (f *inboxProposalCommandsFake) EnsureInboxProposal(_ context.Context, command EnsureInboxProposalCommand) (InboxProposalResult, error) {
	f.ensureCalls = append(f.ensureCalls, command)
	return InboxProposalResult{Job: f.job}, nil
}

func (f *inboxCaptureWriterFake) GetInboxByURL(_ context.Context, rawURL string) (*model.ReaderInbox, error) {
	item, ok := f.items[rawURL]
	if !ok || item.Status == "discarded" {
		return nil, nil
	}
	copy := item
	return &copy, nil
}

func (f *inboxCaptureWriterFake) CreateInbox(_ context.Context, item model.ReaderInbox) (*model.ReaderInbox, error) {
	if item.ID == uuid.Nil {
		item.ID = uuid.New()
	}
	if item.Status == "" {
		item.Status = "pending"
	}
	if f.items == nil {
		f.items = make(map[string]model.ReaderInbox)
	}
	identityKey := item.IdentityKey
	if identityKey == "" {
		identityKey = item.URL
	}
	f.items[identityKey] = item
	f.created = append(f.created, item)
	copy := item
	return &copy, nil
}

func TestSubmitServiceInboxDestinationReturnsInboxIDWithoutLinkID(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	jobs := &repotest.ObservableJobStore{}
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
	inbox := &inboxCaptureWriterFake{}
	service, _ := NewLinkServices(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox})

	submittedURL := "HTTPS://WWW.Example.com//inbox/?utm_source=save#frag"
	response, err := service.Submit(context.Background(), dto.LinkCreateRequest{
		URL:         submittedURL,
		Destination: "inbox",
		Description: stringPtrForDestinationTest("keep for later"),
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if response.InboxID == "" || response.LinkID != "" || response.Destination != captureDestinationInbox || response.Status != "pending" {
		t.Fatalf("response = %+v, want inbox-only identity", response)
	}
	if len(links.CreateCalls) != 0 || len(inbox.created) != 1 {
		t.Fatalf("link creates = %d, inbox creates = %d", len(links.CreateCalls), len(inbox.created))
	}
	if inbox.created[0].Summary == nil || *inbox.created[0].Summary != "keep for later" {
		t.Fatalf("inbox summary = %v, want submitted description", inbox.created[0].Summary)
	}
	if inbox.created[0].URL != submittedURL || inbox.created[0].IdentityKey != "https://example.com/inbox" {
		t.Fatalf("inbox URL = %q identity = %q, want display %q and canonical identity", inbox.created[0].URL, inbox.created[0].IdentityKey, submittedURL)
	}
}

func TestSubmitServiceInboxDestinationIsIdempotentByURL(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	jobs := &repotest.ObservableJobStore{}
	inboxID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	inbox := &inboxCaptureWriterFake{items: map[string]model.ReaderInbox{
		"https://example.com/inbox": {ID: inboxID, URL: "https://example.com/inbox", Status: "pending"},
	}}
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
	service, _ := NewLinkServices(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox})

	response, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "HTTPS://WWW.Example.com//inbox/?utm_source=again#frag", Destination: "inbox"})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if response.InboxID != inboxID.String() || len(inbox.created) != 0 || len(links.CreateCalls) != 0 {
		t.Fatalf("response = %+v, created inbox = %d, links = %d", response, len(inbox.created), len(links.CreateCalls))
	}
}

func TestSubmitServiceInboxCaptureCreatesOneDurableProposalForCanonicalRetries(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	jobs := &repotest.ObservableJobStore{}
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
	inbox := &inboxCaptureWriterFake{}
	scheduler := &inboxProposalSchedulerFake{}
	service, _ := NewLinkServices(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox, InboxJobScheduler: scheduler})

	first, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "HTTPS://WWW.Example.com//proposal/?utm_source=save#frag", Destination: "inbox"})
	if err != nil {
		t.Fatalf("first Inbox Submit: %v", err)
	}
	second, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/proposal", Destination: "inbox"})
	if err != nil {
		t.Fatalf("canonical retry Inbox Submit: %v", err)
	}
	if first.InboxID == "" || first.JobID == nil || second.InboxID != first.InboxID || second.JobID != nil || len(inbox.created) != 1 || inbox.begins != 1 || len(scheduler.args) != 1 {
		t.Fatalf("first=%+v second=%+v created=%d begins=%d queued=%d", first, second, len(inbox.created), inbox.begins, len(scheduler.args))
	}
	if inbox.created[0].Note != "" || inbox.created[0].Summary != nil {
		t.Fatalf("capture invented a user note or AI summary: %#v", inbox.created[0])
	}
}

func TestSubmitServiceInboxRetryRepairsQueuedProposalThroughDurableCommand(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	jobs := &repotest.ObservableJobStore{}
	inboxID, jobID := uuid.New(), uuid.New()
	inbox := &inboxCaptureWriterFake{items: map[string]model.ReaderInbox{
		"https://example.com/proposal": {
			ID: inboxID, URL: "https://example.com/proposal", Status: "pending",
			ProposalStatus: "pending", MetadataRevision: 4,
		},
	}}
	proposalCommands := &inboxProposalCommandsFake{job: &model.ReaderInboxJob{
		ID: jobID, InboxID: inboxID, ExpectedMetadataRevision: 4, Status: "queued",
	}}
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
	service, _ := NewLinkServices(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{
		InboxWriter: inbox, InboxProposalCommands: proposalCommands,
	})

	response, err := service.Submit(context.Background(), dto.LinkCreateRequest{
		URL: "HTTPS://WWW.Example.com//proposal/?utm_source=retry", Destination: "inbox",
	})
	if err != nil {
		t.Fatalf("Inbox retry error = %v", err)
	}
	if response.InboxID != inboxID.String() || response.JobID == nil || *response.JobID != jobID.String() {
		t.Fatalf("response = %+v", response)
	}
	if len(proposalCommands.ensureCalls) != 1 || proposalCommands.ensureCalls[0].InboxID != inboxID || proposalCommands.ensureCalls[0].ExpectedMetadataRevision != 4 {
		t.Fatalf("ensure calls = %+v", proposalCommands.ensureCalls)
	}
	if inbox.begins != 0 {
		t.Fatalf("legacy non-transactional BeginInboxResummarizeJob calls = %d", inbox.begins)
	}
}

func TestIngestExplicitInboxDestinationUsesInboxWriter(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	jobs := &repotest.ObservableJobStore{}
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
	inbox := &inboxCaptureWriterFake{}
	_, service := NewLinkServices(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox})

	response, err := service.Ingest(context.Background(), dto.IngestRequest{
		Destination: "inbox",
		Sources:     []dto.IngestSource{{Kind: "browser_capture", URL: "https://example.com/captured", Title: "Captured", Text: "body"}},
	})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if response.InboxID == "" || response.LinkID != "" || len(links.CreateCalls) != 0 || len(inbox.created) != 1 {
		t.Fatalf("response = %+v, links = %d, inbox = %d", response, len(links.CreateCalls), len(inbox.created))
	}
	if inbox.created[0].Body != "body" || inbox.created[0].Title == nil || *inbox.created[0].Title != "Captured" {
		t.Fatalf("created inbox = %+v", inbox.created[0])
	}
}

func TestIngestExplicitSiteDestinationForcesSiteIntent(t *testing.T) {
	links := &repotest.ObservableLinkStore{
		CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
			return &model.Link{ID: uuid.New(), URL: params.URL, Status: model.LinkStatusPending}, nil
		},
	}
	jobs := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{ID: uuid.New(), LinkID: linkID, Status: model.JobStatusPending}, nil
		},
	}
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
	_, ingest := NewLinkServices(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{})

	response, err := ingest.Ingest(context.Background(), dto.IngestRequest{
		Destination: "site",
		Sources: []dto.IngestSource{{
			Kind: "browser_capture", URL: "https://example.com/site", Title: "Site", Text: "body",
		}},
	})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if response.LinkID == "" || response.InboxID != "" || response.Destination != captureDestinationSite {
		t.Fatalf("response = %+v, want explicit site link identity", response)
	}
	if len(links.CreateCalls) != 1 {
		t.Fatalf("link creates = %d, want 1", len(links.CreateCalls))
	}
	created := links.CreateCalls[0]
	if created.RequestedLibraryKind != model.RequestedLibraryKindSite || created.RequestedLibraryKindSource != model.RequestedLibraryKindSourceUser {
		t.Fatalf("site intent = %q/%q, want site/user", created.RequestedLibraryKind, created.RequestedLibraryKindSource)
	}
}

func TestIngestExplicitSiteDestinationRejectsReadingIntent(t *testing.T) {
	_, err := normalizeIngestRequest(dto.IngestRequest{
		Destination:          "site",
		RequestedLibraryKind: "reading",
		Sources:              []dto.IngestSource{{Kind: "url", URL: "https://example.com/site"}},
	}, captureDestinationLibrary)
	if err == nil {
		t.Fatal("site destination with reading intent should fail")
	}
}

func TestSubmitServiceBatchSupportsLibraryAndInboxDestinations(t *testing.T) {
	links := &repotest.ObservableLinkStore{
		CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
			return &model.Link{ID: uuid.New(), URL: params.URL, Status: model.LinkStatusPending}, nil
		},
	}
	jobs := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{ID: uuid.New(), LinkID: linkID, Status: model.JobStatusPending}, nil
		},
	}
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
	inbox := &inboxCaptureWriterFake{}
	service, _ := NewLinkServices(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox})

	response, err := service.Batch(context.Background(), dto.BatchCreateRequest{Items: []dto.LinkCreateRequest{
		{URL: "https://example.com/library", Destination: "library"},
		{URL: "https://example.com/inbox", Destination: "inbox"},
		{URL: "https://example.com/inbox", Destination: "inbox"},
	}})
	if err != nil {
		t.Fatalf("Batch() error = %v", err)
	}
	if len(response.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(response.Results))
	}
	if response.Results[0].Result == nil || response.Results[0].Result.LinkID == "" || response.Results[0].Result.InboxID != "" {
		t.Fatalf("library result = %+v", response.Results[0])
	}
	for i := 1; i < 3; i++ {
		result := response.Results[i].Result
		if result == nil || result.InboxID == "" || result.LinkID != "" || result.Destination != captureDestinationInbox {
			t.Fatalf("inbox result %d = %+v", i, response.Results[i])
		}
	}
	if len(links.CreateCalls) != 1 || len(inbox.created) != 1 {
		t.Fatalf("link creates = %d, inbox creates = %d", len(links.CreateCalls), len(inbox.created))
	}
}

func TestNormalizeCaptureDestinationRejectsUnknownValue(t *testing.T) {
	if _, err := normalizeCaptureDestination("archive", captureDestinationLibrary); err == nil {
		t.Fatal("unknown destination should fail")
	}
}

func TestNormalizeCaptureDestinationRejectsSiteOutsideIngest(t *testing.T) {
	if _, err := normalizeCaptureDestination(captureDestinationSite, captureDestinationLibrary); err == nil {
		t.Fatal("link submit destination must not accept site")
	}
}

func stringPtrForDestinationTest(value string) *string { return &value }

// 收藏的缺省目的地是收件箱：客户端（尤其是 Android 分享入口）不传 destination
// 时，条目必须落进收件箱而不是直接进阅读库。
func TestSubmitDefaultsToInboxWhenInboxAvailable(t *testing.T) {
	links := &repotest.ObservableLinkStore{
		CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
			return &model.Link{ID: uuid.New(), URL: params.URL, Status: model.LinkStatusPending}, nil
		},
	}
	jobs := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{ID: uuid.New(), LinkID: linkID, Status: model.JobStatusPending}, nil
		},
	}
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
	service, _ := NewLinkServices(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{
		InboxWriter: &inboxCaptureWriterFake{},
	})

	resp, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/default"})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if resp.Destination != captureDestinationInbox || resp.InboxID == "" || resp.LinkID != "" {
		t.Fatalf("default capture = %+v, want inbox", resp)
	}
}

// 收件箱是可选特性。未启用 Reader Inbox 的部署里 InboxWriter 为 nil，缺省必须
// 退回阅读库——否则「默认进收件箱」会把这类部署的每一次收藏都变成 503。
func TestSubmitDefaultsToLibraryWhenInboxUnavailable(t *testing.T) {
	links := &repotest.ObservableLinkStore{
		CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
			return &model.Link{ID: uuid.New(), URL: params.URL, Status: model.LinkStatusPending}, nil
		},
	}
	jobs := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{ID: uuid.New(), LinkID: linkID, Status: model.JobStatusPending}, nil
		},
	}
	commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
	service, _ := NewLinkServices(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{})

	resp, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/default"})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if resp.LinkID == "" || resp.InboxID != "" {
		t.Fatalf("fallback capture = %+v, want library", resp)
	}

	// 显式点名 inbox 仍必须硬失败，不被静默改道。
	if _, err := service.Submit(context.Background(), dto.LinkCreateRequest{
		URL:         "https://example.com/explicit",
		Destination: "inbox",
	}); err == nil {
		t.Fatal("explicit inbox destination should fail when inbox is unavailable")
	}
}
