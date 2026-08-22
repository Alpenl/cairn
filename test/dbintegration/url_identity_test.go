package dbintegration

import (
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/service/urllock"
)

var urlIdentityVariants = []string{
	"https://identity.example.com/docs/guide?a=1&b=2",
	"HTTPS://Identity.Example.COM/docs/guide?a=1&b=2",
	"https://www.identity.example.com/docs/guide?a=1&b=2",
	"https://identity.example.com.:443/docs/guide?a=1&b=2",
	"https://identity.example.com//docs//guide/?b=2&a=1",
	"https://identity.example.com/docs/tmp/../guide?a=1&b=2#section",
	"https://identity.example.com/docs/./guide?utm_source=news&b=2&a=1&fbclid=x",
}

const urlIdentityCanonicalForm = "https://identity.example.com/docs/guide?a=1&b=2"

func newURLIdentitySubmitService(t *testing.T, pool *pgxpool.Pool) (*service.SubmitService, *countingSubmitQueue) {
	t.Helper()
	links := repository.NewPGXLinkRepository(pool)
	queue := &countingSubmitQueue{}
	submit, _ := service.NewLinkServices(
		links,
		dbLinkCommands(pool, links, queue),
		urllock.NewInProcessURLLocker(),
		service.SubmitServiceOptions{},
	)
	return submit, queue
}

func TestURLIdentitySerialVariantsReuseOneRecord(t *testing.T) {
	pool := StartPostgres(t)
	submit, _ := newURLIdentitySubmitService(t, pool)
	submittedDisplayURL := "  " + urlIdentityVariants[4] + "  "
	first, err := submit.Submit(t.Context(), dto.LinkCreateRequest{URL: submittedDisplayURL, Destination: "library"})
	if err != nil || first.LinkID == "" {
		t.Fatalf("first Submit() = %+v, %v", first, err)
	}
	for _, variant := range urlIdentityVariants {
		result, submitErr := submit.Submit(t.Context(), dto.LinkCreateRequest{URL: variant, Destination: "library"})
		if submitErr != nil {
			t.Fatalf("Submit(%q): %v", variant, submitErr)
		}
		if result.LinkID != first.LinkID {
			t.Fatalf("Submit(%q) = %+v, want link %s", variant, result, first.LinkID)
		}
	}
	if got := rawCountLinks(t, pool); got != 1 {
		t.Fatalf("links = %d, want 1", got)
	}
	linkID := uuid.MustParse(first.LinkID)
	if got := rawLinkIdentity(t, pool, linkID); got.URL != strings.TrimSpace(submittedDisplayURL) || got.SourceKey != urlIdentityCanonicalForm {
		t.Fatalf("stored identity = %+v, want display %q canonical %q", got, strings.TrimSpace(submittedDisplayURL), urlIdentityCanonicalForm)
	}
}

func TestURLIdentityConcurrentVariantsCollapseOntoOneRecord(t *testing.T) {
	pool := StartPostgres(t)
	submit, _ := newURLIdentitySubmitService(t, pool)
	start := make(chan struct{})
	responses := make([]dto.SubmitResponse, len(urlIdentityVariants))
	errorsByIndex := make([]error, len(urlIdentityVariants))
	var group sync.WaitGroup
	for index, variant := range urlIdentityVariants {
		group.Add(1)
		go func(index int, variant string) {
			defer group.Done()
			<-start
			responses[index], errorsByIndex[index] = submit.Submit(t.Context(), dto.LinkCreateRequest{URL: variant, Destination: "library"})
		}(index, variant)
	}
	close(start)
	group.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("Submit(%q): %v", urlIdentityVariants[index], err)
		}
		if responses[index].LinkID != responses[0].LinkID {
			t.Fatalf("response %d link = %s, want %s", index, responses[index].LinkID, responses[0].LinkID)
		}
	}
	if rawCountLinks(t, pool) != 1 {
		t.Fatalf("concurrent variants wrote links = %d, want 1", rawCountLinks(t, pool))
	}
}

func TestURLIdentityMultimodalIngestAndSubmitShareOneRecord(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	_, ingest := service.NewLinkServices(
		links,
		dbLinkCommands(pool, links, &countingSubmitQueue{}),
		urllock.NewInProcessURLLocker(),
		service.SubmitServiceOptions{},
	)
	displayURL := "  " + urlIdentityVariants[6] + "  "
	created, err := ingest.Ingest(t.Context(), dto.IngestRequest{Destination: "library", Sources: []dto.IngestSource{
		{Kind: "url", URL: displayURL},
		{Kind: "text", Text: "Selected passage"},
	}})
	if err != nil || created.LinkID == "" {
		t.Fatalf("Ingest() = %+v, %v", created, err)
	}
	identity := rawLinkIdentity(t, pool, uuid.MustParse(created.LinkID))
	if identity.URL != strings.TrimSpace(displayURL) || identity.SourceKey != urlIdentityCanonicalForm {
		t.Fatalf("ingest identity = %+v", identity)
	}
	submit, _ := newURLIdentitySubmitService(t, pool)
	reused, err := submit.Submit(t.Context(), dto.LinkCreateRequest{URL: urlIdentityVariants[2], Destination: "library"})
	if err != nil || reused.LinkID != created.LinkID {
		t.Fatalf("Submit() after Ingest() = %+v, %v", reused, err)
	}
}

func TestURLIdentityRejectedURLsWriteNothing(t *testing.T) {
	pool := StartPostgres(t)
	submit, queue := newURLIdentitySubmitService(t, pool)
	tests := []struct {
		name string
		url  string
		code string
	}{
		{"relative path", "/docs/guide", httperr.CodeInvalidURL},
		{"schemeless", "identity.example.com/docs", httperr.CodeInvalidURL},
		{"protocol relative", "//identity.example.com/docs", httperr.CodeInvalidURL},
		{"missing host", "https://", httperr.CodeInvalidURL},
		{"javascript scheme", "javascript:alert(1)", httperr.CodeInvalidURL},
		{"file scheme", "file:///etc/passwd", httperr.CodeInvalidURL},
		{"ftp scheme", "ftp://identity.example.com/a", httperr.CodeUnsupportedURLScheme},
		{"empty", "   ", httperr.CodeURLRequired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := submit.Submit(t.Context(), dto.LinkCreateRequest{URL: test.url})
			assertUnprocessableCode(t, err, test.code)
		})
	}
	if rawCountLinks(t, pool) != 0 {
		t.Fatalf("rejected URLs wrote links = %d", rawCountLinks(t, pool))
	}
	if calls := queue.enqueueTxCalls.Load(); calls != 0 {
		t.Fatalf("rejected URLs enqueued %d jobs", calls)
	}
}

func TestURLIdentityInboxConfirmReusesCanonicalRecord(t *testing.T) {
	pool := StartPostgres(t)
	submit, _ := newURLIdentitySubmitService(t, pool)
	saved, err := submit.Submit(t.Context(), dto.LinkCreateRequest{URL: urlIdentityVariants[0], Destination: "library"})
	if err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	inboxID := seedReaderVNextInbox(
		t,
		pool,
		"https://WWW.Identity.example.com//docs/guide/?b=2&a=1&utm_source=news#top",
		"Captured",
		"body",
		"summary",
	)
	reader := repository.NewPGXReaderVNextRepository(pool)
	linkID, err := reader.ConfirmInbox(t.Context(), inboxID, nil)
	if err != nil || linkID.String() != saved.LinkID {
		t.Fatalf("ConfirmInbox() = %s, %v; want %s", linkID, err, saved.LinkID)
	}
	if rawCountLinks(t, pool) != 1 {
		t.Fatalf("links after Inbox confirmation = %d, want 1", rawCountLinks(t, pool))
	}
}

func TestFeedURLIdentitySubscribeAndOPMLReuseCanonicalSubscription(t *testing.T) {
	pool := StartPostgres(t)
	feeds := repository.NewPGXFeedRepository(pool, pool)
	feedService := service.NewFeedService(service.FeedServiceOptions{
		Store: feeds, Locker: urllock.NewInProcessURLLocker(),
	})
	first, err := feedService.Subscribe(
		t.Context(), "https://WWW.Feeds.example.com//rss/?utm_source=old#latest", nil, false,
	)
	if err != nil {
		t.Fatalf("Subscribe(first): %v", err)
	}
	second, err := feedService.Subscribe(t.Context(), "https://feeds.example.com/rss/?gclid=new#top", nil, false)
	if err != nil || second.ID != first.ID {
		t.Fatalf("Subscribe(variant) = %+v, %v; want %s", second, err, first.ID)
	}
	opml := []byte(`<?xml version="1.0"?><opml version="2.0"><body><outline text="Again" type="rss" xmlUrl="https://www.feeds.example.com/rss/?utm_medium=mail#x"/></body></opml>`)
	if _, err := feedService.ImportOPML(t.Context(), opml); err != nil {
		t.Fatalf("ImportOPML(): %v", err)
	}
	var subscriptions int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM feed_subscriptions`).Scan(&subscriptions); err != nil {
		t.Fatalf("count feed subscriptions: %v", err)
	}
	if subscriptions != 1 {
		t.Fatalf("feed subscriptions = %d, want 1", subscriptions)
	}
}

func assertUnprocessableCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	carrier, ok := httperr.As(err)
	coder, coded := carrier.(httperr.ErrorCoder)
	if !ok || !coded || carrier.HTTPStatus() != 422 || coder.HTTPErrorCode() != wantCode {
		t.Fatalf("error = %v, want HTTP 422/%s", err, wantCode)
	}
}

type storedLinkIdentity struct {
	URL       string
	SourceKey string
}

func rawLinkIdentity(t *testing.T, pool *pgxpool.Pool, linkID uuid.UUID) storedLinkIdentity {
	t.Helper()
	var identity storedLinkIdentity
	if err := pool.QueryRow(t.Context(), `SELECT url,source_key FROM links WHERE id=$1`, linkID).
		Scan(&identity.URL, &identity.SourceKey); err != nil {
		t.Fatalf("read link identity: %v", err)
	}
	return identity
}
