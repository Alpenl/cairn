package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
	"webtag/internal/urlidentity"
)

// This file is the entry-point contract: every surface that can turn a URL into
// a collected record must derive the same identity from the same input and must
// refuse the same inputs with the same code.
//
// It exists because the entry points were written years apart and each had its
// own idea of what counted as "the same page" — and one of them, Reader Inbox,
// had no idea at all and accepted whatever scheme the binding tag let through.
// A per-entry-point test would have kept passing through all of that; only an
// assertion that compares them against one another catches a surface drifting.

// entryPointVariants are seven spellings of one page. Each differs from the
// canonical only in a dimension the contract declares irrelevant.
var entryPointVariants = []string{
	"https://contract.example.com/docs/guide?a=1&b=2",
	"HTTPS://Contract.Example.COM/docs/guide?a=1&b=2",
	"https://www.contract.example.com/docs/guide?a=1&b=2",
	"https://contract.example.com.:443/docs/guide?a=1&b=2",
	"https://contract.example.com//docs//guide/?b=2&a=1",
	"https://contract.example.com/docs/tmp/../guide?a=1&b=2#frag",
	"https://contract.example.com/docs/./guide?utm_campaign=x&b=2&gclid=y&a=1",
}

const entryPointCanonical = "https://contract.example.com/docs/guide?a=1&b=2"

// urlEntryPoint names one way a URL can enter the system and reduces it to the
// identity that surface would persist.
type urlEntryPoint struct {
	name     string
	identity func(t *testing.T, raw string) (string, error)
}

func collectionEntryPoints() []urlEntryPoint {
	return []urlEntryPoint{
		{
			// POST /api/links, POST /api/links/batch, GET /api/links?url=,
			// and the RSS subscription save all take their identity from this
			// one function; asserting it once is asserting all four.
			name: "links/batch/url-lookup/rss",
			identity: func(_ *testing.T, raw string) (string, error) {
				return validateURL(raw)
			},
		},
		{
			name: "ingest url source",
			identity: func(_ *testing.T, raw string) (string, error) {
				normalized, err := normalizeIngestRequest(dto.IngestRequest{
					Sources: []dto.IngestSource{{Kind: "url", URL: raw}},
				})
				if err != nil {
					return "", err
				}
				return normalized.sourceKey, nil
			},
		},
		{
			name: "ingest browser capture",
			identity: func(_ *testing.T, raw string) (string, error) {
				normalized, err := normalizeIngestRequest(dto.IngestRequest{
					Sources: []dto.IngestSource{{Kind: "browser_capture", URL: raw, Title: "captured"}},
				})
				if err != nil {
					return "", err
				}
				return normalized.sourceKey, nil
			},
		},
		{
			name: "reader inbox ingest",
			identity: func(t *testing.T, raw string) (string, error) {
				store := &inboxIdentityStore{}
				service := NewReaderVNextService(store, nil)
				_, err := service.CreateInbox(context.Background(), dto.ReaderInboxCreateRequest{URL: raw})
				if err != nil {
					if store.created {
						t.Fatalf("CreateInbox(%q) wrote an inbox row before failing", raw)
					}
					return "", err
				}
				return store.identityKey, nil
			},
		},
	}
}

// inboxIdentityStore records exactly what CreateInbox tried to persist, so a
// rejected URL can be proven to have written nothing at all.
type inboxIdentityStore struct {
	repository.ReaderVNextStore
	created     bool
	url         string
	identityKey string
}

func (s *inboxIdentityStore) CreateInbox(_ context.Context, item model.ReaderInbox) (*model.ReaderInbox, error) {
	s.created = true
	s.url = item.URL
	s.identityKey = item.IdentityKey
	stored := item
	return &stored, nil
}

func TestCollectionEntryPointsAgreeOnOneURLIdentity(t *testing.T) {
	for _, entry := range collectionEntryPoints() {
		for _, variant := range entryPointVariants {
			t.Run(entry.name+"/"+variant, func(t *testing.T) {
				got, err := entry.identity(t, variant)
				if err != nil {
					t.Fatalf("%s identity(%q) error = %v", entry.name, variant, err)
				}
				if got != entryPointCanonical {
					t.Fatalf("%s identity(%q) = %q, want %q", entry.name, variant, got, entryPointCanonical)
				}
			})
		}
	}
}

// TestCollectionEntryPointsMatchTheNormalizationContract closes the loop
// between the entry points and the package that owns the rules: if someone
// changes a rule in urlidentity, every surface must move with it, and if
// someone reintroduces a bespoke variant in an entry point, this fails.
func TestCollectionEntryPointsMatchTheNormalizationContract(t *testing.T) {
	for _, variant := range entryPointVariants {
		want, err := urlidentity.Normalize(variant)
		if err != nil {
			t.Fatalf("urlidentity.Normalize(%q) error = %v", variant, err)
		}
		for _, entry := range collectionEntryPoints() {
			got, entryErr := entry.identity(t, variant)
			if entryErr != nil {
				t.Fatalf("%s identity(%q) error = %v", entry.name, variant, entryErr)
			}
			if got != want {
				t.Fatalf("%s identity(%q) = %q, want the contract's %q", entry.name, variant, got, want)
			}
		}
	}
}

// rejectedEntryPointURLs pairs each refused input with the public 422 code the
// caller may branch on. The dangerous schemes are the reason this list is
// asserted at every entry point rather than only at /api/links.
var rejectedEntryPointURLs = []struct {
	name string
	url  string
	code string
}{
	{"relative path", "/docs/guide", httperr.CodeInvalidURL},
	{"schemeless host", "contract.example.com/docs", httperr.CodeInvalidURL},
	{"protocol relative", "//contract.example.com/docs", httperr.CodeInvalidURL},
	{"scheme without host", "https://", httperr.CodeInvalidURL},
	{"javascript scheme", "javascript:alert(1)", httperr.CodeInvalidURL},
	{"data scheme", "data:text/html;base64,PHNjcmlwdD4=", httperr.CodeInvalidURL},
	{"file scheme", "file:///etc/passwd", httperr.CodeInvalidURL},
	{"mailto scheme", "mailto:a@example.com", httperr.CodeInvalidURL},
	{"javascript with authority", "javascript://contract.example.com/%0aalert(1)", httperr.CodeUnsupportedURLScheme},
	{"ftp scheme", "ftp://contract.example.com/a", httperr.CodeUnsupportedURLScheme},
	{"ws scheme", "ws://contract.example.com/a", httperr.CodeUnsupportedURLScheme},
	{"loopback target", "http://127.0.0.1/admin", httperr.CodeUnsafeURLTarget},
	{"link local metadata", "http://169.254.169.254/latest/meta-data", httperr.CodeUnsafeURLTarget},
	{"loopback behind host casing", "http://LOCALHOST./admin", httperr.CodeUnsafeURLTarget},
}

func TestCollectionEntryPointsRejectTheSameURLsWithTheSameCode(t *testing.T) {
	for _, entry := range collectionEntryPoints() {
		for _, tc := range rejectedEntryPointURLs {
			t.Run(entry.name+"/"+tc.name, func(t *testing.T) {
				got, err := entry.identity(t, tc.url)
				if err == nil {
					t.Fatalf("%s identity(%q) = %q, want a 422 rejection", entry.name, tc.url, got)
				}
				var statusErr *httperr.Error
				if !errors.As(err, &statusErr) {
					t.Fatalf("%s identity(%q) error = %v, want *httperr.Error", entry.name, tc.url, err)
				}
				if statusErr.HTTPStatus() != 422 {
					t.Fatalf("%s identity(%q) status = %d, want 422", entry.name, tc.url, statusErr.HTTPStatus())
				}
				if statusErr.HTTPErrorCode() != tc.code {
					t.Fatalf(
						"%s identity(%q) code = %q, want %q",
						entry.name, tc.url, statusErr.HTTPErrorCode(), tc.code,
					)
				}
			})
		}
	}
}

// TestEmptyURLIsRejectedWhereTheURLIsTheIdentity keeps the empty-string case
// out of the shared table because one entry point treats it differently on
// purpose: a browser capture may legitimately carry no URL (a selection copied
// out of a page that never had one), and it then gets a synthetic
// content-derived identity instead. Spelling that exception out here is the
// point — a filtered-out row in the table above would have hidden it.
func TestEmptyURLIsRejectedWhereTheURLIsTheIdentity(t *testing.T) {
	for _, entry := range collectionEntryPoints() {
		if entry.name == "ingest browser capture" {
			continue
		}
		t.Run(entry.name, func(t *testing.T) {
			got, err := entry.identity(t, "   ")
			if err == nil {
				t.Fatalf("%s identity(blank) = %q, want a 422 rejection", entry.name, got)
			}
			var statusErr *httperr.Error
			if !errors.As(err, &statusErr) || statusErr.HTTPErrorCode() != httperr.CodeURLRequired {
				t.Fatalf("%s identity(blank) error = %v, want %q", entry.name, err, httperr.CodeURLRequired)
			}
		})
	}

	// The documented exception, asserted rather than assumed.
	normalized, err := normalizeIngestRequest(dto.IngestRequest{
		Sources: []dto.IngestSource{{Kind: "browser_capture", URL: "   ", Title: "a selection"}},
	})
	if err != nil {
		t.Fatalf("browser capture without a URL error = %v, want a synthetic identity", err)
	}
	if normalized.realURL != "" {
		t.Fatalf("browser capture without a URL has realURL = %q, want empty", normalized.realURL)
	}
	if !strings.HasPrefix(normalized.storedURL, "webtag://ingest/") {
		t.Fatalf("browser capture without a URL storedURL = %q, want a synthetic webtag://ingest locator", normalized.storedURL)
	}
}

// TestSSRFRejectionSurvivesNormalization pins the ordering inside validateURL.
// The unsafe-host check has to see the normalized host: a blocklist consulted
// before folding "LOCALHOST." or "www.localhost" would be a bypass with a
// perfectly ordinary-looking diff.
func TestSSRFRejectionSurvivesNormalization(t *testing.T) {
	for _, raw := range []string{
		"http://localhost/admin",
		"http://LOCALHOST/admin",
		"http://localhost./admin",
		"http://127.0.0.1:80/admin",
		"http://[::1]/admin",
		"http://169.254.169.254/latest/meta-data",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := validateURL(raw)
			var statusErr *httperr.Error
			if !errors.As(err, &statusErr) || statusErr.HTTPErrorCode() != httperr.CodeUnsafeURLTarget {
				t.Fatalf("validateURL(%q) error = %v, want %q", raw, err, httperr.CodeUnsafeURLTarget)
			}
		})
	}
}

// TestSubmittedURLIsKeptAsSourceEvidence covers the other half of the identity
// decision: the display URL and source evidence keep what the user submitted,
// while canonical identity lives in source_key.
func TestSubmittedURLIsKeptAsSourceEvidence(t *testing.T) {
	submitted := "HTTPS://WWW.Contract.Example.com/docs/guide/?b=2&a=1&utm_source=news#frag"
	metadata := withOriginalURL(nil, submitted, entryPointCanonical)
	if metadata[originalURLMetadataKey] != submitted {
		t.Fatalf("source metadata %v, want %s = %q", metadata, originalURLMetadataKey, submitted)
	}

	// Already-canonical input must not start populating source metadata on
	// every submission; rows that carry none still mean "none".
	if metadata := withOriginalURL(nil, entryPointCanonical, entryPointCanonical); metadata != nil {
		t.Fatalf("source metadata for canonical input = %v, want nil", metadata)
	}
	if metadata := withOriginalURL(nil, "  "+entryPointCanonical+"  ", entryPointCanonical); metadata != nil {
		t.Fatalf("source metadata for whitespace-padded canonical input = %v, want nil", metadata)
	}
}

// TestIngestKeepsTheSubmittedURLAsSourceEvidence checks the same guarantee on
// the multi-modal entry point, whose metadata is assembled separately.
func TestIngestKeepsTheSubmittedURLAsSourceEvidence(t *testing.T) {
	submitted := "HTTPS://WWW.Contract.Example.com/docs/guide/?b=2&a=1#frag"
	normalized, err := normalizeIngestRequest(dto.IngestRequest{
		Sources: []dto.IngestSource{{Kind: "url", URL: submitted}},
	})
	if err != nil {
		t.Fatalf("normalizeIngestRequest() error = %v", err)
	}
	if normalized.storedURL != submitted {
		t.Fatalf("storedURL = %q, want submitted display URL %q", normalized.storedURL, submitted)
	}
	if normalized.sourceKey != entryPointCanonical || normalized.realURL != entryPointCanonical || normalized.lockURL != entryPointCanonical {
		t.Fatalf("identity fields = sourceKey %q realURL %q lockURL %q, want %q", normalized.sourceKey, normalized.realURL, normalized.lockURL, entryPointCanonical)
	}
	if got := normalized.sourceMetadata[originalURLMetadataKey]; got != submitted {
		t.Fatalf("source metadata %v, want %s = %q", normalized.sourceMetadata, originalURLMetadataKey, submitted)
	}
}

func TestRSSCapturePreservesDisplayURLAndUsesCanonicalIdentity(t *testing.T) {
	links := &repotest.ObservableLinkStore{CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
		return &model.Link{ID: uuid.New(), URL: params.URL, SourceKey: params.SourceKey, Status: params.Status}, nil
	}}
	jobs := &repotest.ObservableJobStore{CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
		return &model.ParseJob{ID: uuid.New(), LinkID: linkID, Status: model.JobStatusPending}, nil
	}}
	locker := &submitFakeLocker{}
	ingest := newTestIngestService(links, jobs, &submitFakeQueue{}, locker)
	submitted := "  HTTPS://WWW.Contract.Example.com//docs/guide/?b=2&a=1&utm_source=rss#frag  "
	if _, err := ingest.AnalyzeRSS(context.Background(), RSSIngestRequest{
		URL: submitted, SubscriptionID: uuid.New(), ItemID: uuid.New(),
	}); err != nil {
		t.Fatalf("AnalyzeRSS() error = %v", err)
	}
	if len(links.CreateCalls) != 1 {
		t.Fatalf("RSS create calls = %d, want 1", len(links.CreateCalls))
	}
	capture := links.CreateCalls[0]
	if capture.URL != strings.TrimSpace(submitted) || capture.SourceKey != entryPointCanonical {
		t.Fatalf("RSS capture = URL %q SourceKey %q, want display %q identity %q", capture.URL, capture.SourceKey, strings.TrimSpace(submitted), entryPointCanonical)
	}
	if len(locker.urls) != 1 || locker.urls[0] != entryPointCanonical {
		t.Fatalf("RSS lock URLs = %#v, want canonical identity %q", locker.urls, entryPointCanonical)
	}
}

func TestReaderInboxPreservesDisplayURLAndStoresCanonicalIdentity(t *testing.T) {
	submitted := "  HTTPS://WWW.Contract.Example.com//docs/guide/?b=2&a=1&utm_source=news#frag  "
	store := &inboxIdentityStore{}
	reader := NewReaderVNextService(store, nil)
	if _, err := reader.CreateInbox(context.Background(), dto.ReaderInboxCreateRequest{URL: submitted}); err != nil {
		t.Fatalf("CreateInbox() error = %v", err)
	}
	if store.url != strings.TrimSpace(submitted) {
		t.Fatalf("inbox URL = %q, want trimmed display URL %q", store.url, strings.TrimSpace(submitted))
	}
	if store.identityKey != entryPointCanonical {
		t.Fatalf("inbox identity_key = %q, want %q", store.identityKey, entryPointCanonical)
	}
}

func TestBrowserCaptureDisplayURLFollowsTheIdentityOwner(t *testing.T) {
	generic := "HTTPS://generic.example.com/reference"
	captured := "HTTPS://WWW.Contract.Example.com//docs/guide/?b=2&a=1&utm_source=news#frag"
	normalized, err := normalizeIngestRequest(dto.IngestRequest{Sources: []dto.IngestSource{
		{Kind: "url", URL: generic},
		{Kind: "browser_capture", URL: captured, Title: "Captured"},
	}})
	if err != nil {
		t.Fatalf("normalizeIngestRequest() error = %v", err)
	}
	if normalized.storedURL != captured {
		t.Fatalf("storedURL = %q, want browser capture display URL %q", normalized.storedURL, captured)
	}
	if normalized.sourceKey != entryPointCanonical || normalized.realURL != entryPointCanonical || normalized.lockURL != entryPointCanonical {
		t.Fatalf("identity fields = sourceKey %q realURL %q lockURL %q, want %q", normalized.sourceKey, normalized.realURL, normalized.lockURL, entryPointCanonical)
	}
}
