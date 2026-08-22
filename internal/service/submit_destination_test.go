package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

type inboxCaptureWriterFake struct {
	items       map[string]model.ReaderInbox
	created     []model.ReaderInbox
	ensureCalls []EnsureInboxProposalCommand
}

func (f *inboxCaptureWriterFake) CreateInboxProposal(_ context.Context, command CreateInboxProposalCommand) (InboxProposalResult, error) {
	item := command.Inbox
	if item.ID == uuid.Nil {
		item.ID = uuid.New()
	}
	if item.Status == "" {
		item.Status = "pending"
	}
	item.ProposalStatus = "pending"
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
	return InboxProposalResult{Inbox: &copy}, nil
}

func (f *inboxCaptureWriterFake) EnsureInboxProposal(_ context.Context, command EnsureInboxProposalCommand) (InboxProposalResult, error) {
	f.ensureCalls = append(f.ensureCalls, command)
	for _, item := range f.items {
		if item.ID == command.InboxID {
			item.ProposalStatus = "pending"
			copy := item
			return InboxProposalResult{Inbox: &copy}, nil
		}
	}
	return InboxProposalResult{Inbox: &model.ReaderInbox{ID: command.InboxID, MetadataRevision: command.ExpectedMetadataRevision, ProposalStatus: "pending"}}, nil
}

func (f *inboxCaptureWriterFake) GetInboxByURL(_ context.Context, rawURL string) (*model.ReaderInbox, error) {
	item, ok := f.items[rawURL]
	if !ok || item.DeletedAt != nil {
		return nil, nil
	}
	copy := item
	return &copy, nil
}

func TestSubmitServiceInboxDestinationReturnsInboxIDWithoutLinkID(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
	inbox := &inboxCaptureWriterFake{}
	service, _ := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox, InboxProposalCommands: inbox})

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
	inboxID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	inbox := &inboxCaptureWriterFake{items: map[string]model.ReaderInbox{
		"https://example.com/inbox": {ID: inboxID, URL: "https://example.com/inbox", Status: "pending"},
	}}
	commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
	service, _ := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox, InboxProposalCommands: inbox})

	response, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "HTTPS://WWW.Example.com//inbox/?utm_source=again#frag", Destination: "inbox"})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if response.InboxID != inboxID.String() || len(inbox.created) != 0 || len(links.CreateCalls) != 0 {
		t.Fatalf("response = %+v, created inbox = %d, links = %d", response, len(inbox.created), len(links.CreateCalls))
	}
}

func TestSubmitServiceConfirmedInboxRetryDoesNotRestartProposal(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	inboxID := uuid.MustParse("88888888-8888-8888-8888-888888888889")
	inbox := &inboxCaptureWriterFake{items: map[string]model.ReaderInbox{
		"https://example.com/confirmed": {
			ID: inboxID, URL: "https://example.com/confirmed", Status: "confirmed", ProposalStatus: "idle",
		},
	}}
	commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
	service, _ := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{
		InboxWriter: inbox, InboxProposalCommands: inbox,
	})

	response, err := service.Submit(context.Background(), dto.LinkCreateRequest{
		URL: "https://example.com/confirmed", Destination: "inbox",
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if response.InboxID != inboxID.String() || response.Status != "confirmed" || len(inbox.ensureCalls) != 0 {
		t.Fatalf("response = %+v, ensure calls = %+v", response, inbox.ensureCalls)
	}
}

func TestSubmitServiceInboxCaptureCreatesOneDurableProposalForCanonicalRetries(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
	inbox := &inboxCaptureWriterFake{}
	service, _ := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox, InboxProposalCommands: inbox})

	first, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "HTTPS://WWW.Example.com//proposal/?utm_source=save#frag", Destination: "inbox"})
	if err != nil {
		t.Fatalf("first Inbox Submit: %v", err)
	}
	second, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/proposal", Destination: "inbox"})
	if err != nil {
		t.Fatalf("canonical retry Inbox Submit: %v", err)
	}
	if first.InboxID == "" || second.InboxID != first.InboxID || len(inbox.created) != 1 || len(inbox.ensureCalls) != 1 {
		t.Fatalf("first=%+v second=%+v created=%d ensured=%d", first, second, len(inbox.created), len(inbox.ensureCalls))
	}
	if inbox.created[0].Note != "" || inbox.created[0].Summary != nil {
		t.Fatalf("capture invented a user note or AI summary: %#v", inbox.created[0])
	}
}

func TestSubmitServiceInboxRetryEnsuresProposalThroughDurableCommand(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	inboxID := uuid.New()
	inbox := &inboxCaptureWriterFake{items: map[string]model.ReaderInbox{
		"https://example.com/proposal": {
			ID: inboxID, URL: "https://example.com/proposal", Status: "pending",
			ProposalStatus: "pending", MetadataRevision: 4,
		},
	}}
	commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
	service, _ := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{
		InboxWriter: inbox, InboxProposalCommands: inbox,
	})

	response, err := service.Submit(context.Background(), dto.LinkCreateRequest{
		URL: "HTTPS://WWW.Example.com//proposal/?utm_source=retry", Destination: "inbox",
	})
	if err != nil {
		t.Fatalf("Inbox retry error = %v", err)
	}
	if response.InboxID != inboxID.String() {
		t.Fatalf("response = %+v", response)
	}
	if len(inbox.ensureCalls) != 1 || inbox.ensureCalls[0].InboxID != inboxID || inbox.ensureCalls[0].ExpectedMetadataRevision != 4 {
		t.Fatalf("ensure calls = %+v", inbox.ensureCalls)
	}
}

func TestIngestExplicitInboxDestinationUsesInboxWriter(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
	inbox := &inboxCaptureWriterFake{}
	_, service := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox, InboxProposalCommands: inbox})

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
	commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
	_, ingest := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{})

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
	if created.RequestedLibraryKind != model.RequestedLibraryKindSite || !created.UserSelectedLibraryKind {
		t.Fatalf("site selection = %q user-selected=%v, want site/true", created.RequestedLibraryKind, created.UserSelectedLibraryKind)
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
	commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
	inbox := &inboxCaptureWriterFake{}
	service, _ := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{
		InboxWriter: inbox, InboxProposalCommands: inbox,
	})

	resp, err := service.Submit(context.Background(), dto.LinkCreateRequest{URL: "https://example.com/default"})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if resp.Destination != captureDestinationInbox || resp.InboxID == "" || resp.LinkID != "" {
		t.Fatalf("default capture = %+v, want inbox", resp)
	}
}

// TestIngestInboxCaptureCarriesTheCapturedStructureIntoTheInbox pins the
// boundary the reading document used to die at. A browser capture arrives with
// both a flattened text projection and the sanitized HTML; the Inbox row is the
// only carrier between capture and confirmation, and confirmation writes the
// Library link's content columns directly without ever calling
// ContentService.Save. Whatever structure is dropped here is gone for good.
func TestIngestInboxCaptureCarriesTheCapturedStructureIntoTheInbox(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
	inbox := &inboxCaptureWriterFake{}
	_, service := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox, InboxProposalCommands: inbox})

	_, err := service.Ingest(context.Background(), dto.IngestRequest{
		Destination: "inbox",
		Sources: []dto.IngestSource{{
			Kind: "browser_capture", URL: "https://example.com/captured", Title: "Captured",
			Text: "Guide First paragraph One Two",
			HTML: `<article><h1>Guide</h1><p>First paragraph</p><ul><li>One</li><li>Two</li></ul>` +
				`<script>alert("must not survive")</script></article>`,
		}},
	})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if len(inbox.created) != 1 {
		t.Fatalf("inbox creates = %d, want 1", len(inbox.created))
	}
	created := inbox.created[0]
	if created.BodyFormat != model.ContentFormatMarkdown || created.BodyDocument == nil {
		t.Fatalf("created inbox format/document = %q/%#v, want a markdown document", created.BodyFormat, created.BodyDocument)
	}
	for _, want := range []string{"# Guide", "- One", "- Two"} {
		if !strings.Contains(*created.BodyDocument, want) {
			t.Errorf("body_document = %q, want %q", *created.BodyDocument, want)
		}
	}
	// The body stays the plain projection of that same document — the two are
	// two views of one capture, never two copies of one string.
	if created.Body == *created.BodyDocument || !strings.Contains(created.Body, "First paragraph") {
		t.Fatalf("body = %q, want the plain projection of the captured document", created.Body)
	}
	if strings.Contains(created.Body, "must not survive") || strings.Contains(*created.BodyDocument, "must not survive") {
		t.Fatalf("created inbox = %+v, must not carry executable capture markup", created)
	}
}

// TestIngestInboxCaptureWithoutHTMLStaysHonestlyPlain is the other half of the
// contract: a capture with no HTML has no structure to promise, so the Inbox
// row must not claim a document. Confirmation reads the format straight into
// links.content_format, and a plain body labelled markdown is exactly the lie
// that rendered captures as one wall of text.
func TestIngestInboxCaptureWithoutHTMLStaysHonestlyPlain(t *testing.T) {
	links := &repotest.ObservableLinkStore{}
	commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
	inbox := &inboxCaptureWriterFake{}
	_, service := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{InboxWriter: inbox, InboxProposalCommands: inbox})

	_, err := service.Ingest(context.Background(), dto.IngestRequest{
		Destination: "inbox",
		Sources: []dto.IngestSource{{
			Kind: "browser_capture", URL: "https://example.com/captured", Title: "Captured", Text: "just text",
		}},
	})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if len(inbox.created) != 1 {
		t.Fatalf("inbox creates = %d, want 1", len(inbox.created))
	}
	created := inbox.created[0]
	if created.Body != "just text" || created.BodyDocument != nil || created.BodyFormat != model.ContentFormatPlain {
		t.Fatalf("created inbox = %+v, want a plain body with no document", created)
	}
}
