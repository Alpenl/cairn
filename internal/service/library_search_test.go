package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
	"webtag/internal/representation"
)

type librarySearchLinksFake struct {
	repotest.BaseLinkStore
	items  []model.Link
	total  int
	filter *repository.ListLinksFilter
	calls  int
}

func (f *librarySearchLinksFake) ListDone(_ context.Context, filter repository.ListLinksFilter) ([]model.Link, int, error) {
	f.calls++
	f.filter = &filter
	return f.items, f.total, nil
}

type librarySearchSitesFake struct {
	items         []repository.SiteSearchMatch
	total         int
	query         string
	limit         int
	calls         int
	semanticCalls int
	vector        []float32
	model         string
}

type librarySearchReaderFake struct {
	thoughts     []model.ReaderThoughtSearch
	thoughtTotal int
	notes        []model.ReaderNoteSearch
	noteTotal    int
	thoughtQuery string
	thoughtAfter string
	noteQuery    string
	thoughtLimit int
	noteLimit    int
	thoughtNext  string
}

func (f *librarySearchReaderFake) SearchThoughts(_ context.Context, query, after string, limit int) ([]model.ReaderThoughtSearch, int, string, error) {
	f.thoughtQuery, f.thoughtAfter, f.thoughtLimit = query, after, limit
	return f.thoughts, f.thoughtTotal, f.thoughtNext, nil
}

func (f *librarySearchReaderFake) SearchPublishedNotes(_ context.Context, query string, limit int) ([]model.ReaderNoteSearch, int, error) {
	f.noteQuery, f.noteLimit = query, limit
	return f.notes, f.noteTotal, nil
}

func (f *librarySearchSitesFake) SearchSitesSemantic(_ context.Context, query string, vector []float32, model string, limit int) ([]repository.SiteSearchMatch, int, error) {
	f.semanticCalls++
	f.query, f.limit, f.vector, f.model = query, limit, vector, model
	return f.items, f.total, nil
}

func (f *librarySearchSitesFake) SearchSites(_ context.Context, query string, limit int) ([]repository.SiteSearchMatch, int, error) {
	f.calls++
	f.query, f.limit = query, limit
	return f.items, f.total, nil
}

func TestLibrarySearchUsesOneQueryVectorForBothGroups(t *testing.T) {
	t.Parallel()
	links, sites := &librarySearchLinksFake{}, &librarySearchSitesFake{}
	embedder := &stubQueryEmbedder{enabled: true, vec: []float32{0.1, 0.2}}
	if _, err := NewLibrarySearchService(links, sites, embedder).Search(context.Background(), "tools", 10, 10, 20, ""); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if embedder.calls != 1 || links.filter == nil || len(links.filter.QueryEmbedding) != 2 || links.filter.EmbeddingModel != "test-model" {
		t.Fatalf("reading semantic filter = %#v, embed calls = %d", links.filter, embedder.calls)
	}
	if sites.semanticCalls != 1 || len(sites.vector) != 2 || sites.model != "test-model" {
		t.Fatalf("site semantic call = calls:%d vector:%v model:%q", sites.semanticCalls, sites.vector, sites.model)
	}
}

func TestLibrarySearchReturnsReadingAndSiteGroups(t *testing.T) {
	t.Parallel()
	linkID, siteID, entryID := uuid.New(), uuid.New(), uuid.New()
	links := &librarySearchLinksFake{items: []model.Link{{ID: linkID, URL: "https://example.com/read", Status: model.LinkStatusDone}}, total: 12}
	sites := &librarySearchSitesFake{items: []repository.SiteSearchMatch{{SiteID: siteID, SiteName: "Example", MatchedEntries: []repository.SiteSearchEntry{{ID: entryID, Name: "Documentation", URL: "https://example.com/docs"}}}}, total: 3}

	got, err := NewLibrarySearchService(links, sites).Search(context.Background(), "  docs  ", 7, 8, 20, "")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if links.filter == nil || links.filter.Query == nil || *links.filter.Query != "docs" || links.filter.LibraryKind == nil || *links.filter.LibraryKind != model.LibraryKindReading || links.filter.Limit != 7 {
		t.Fatalf("reading filter = %#v, want trimmed reading-only query with limit", links.filter)
	}
	if sites.query != "docs" || sites.limit != 8 {
		t.Fatalf("site search = query %q limit %d, want docs / 8", sites.query, sites.limit)
	}
	if got.Reading.TotalHint != 12 || len(got.Reading.Items) != 1 || got.Reading.Items[0].ID != linkID.String() {
		t.Fatalf("reading group = %#v", got.Reading)
	}
	if got.Sites.TotalHint != 3 || len(got.Sites.Items) != 1 || got.Sites.Items[0].ID != siteID.String() || len(got.Sites.Items[0].MatchedEntries) != 1 || got.Sites.Items[0].MatchedEntries[0].ID != entryID.String() {
		t.Fatalf("site group = %#v", got.Sites)
	}
}

func TestLibrarySearchAddsPublishedReaderGroupsWhenStoreIsWired(t *testing.T) {
	t.Parallel()
	linkID, noteID := uuid.New(), uuid.New()
	links := &librarySearchLinksFake{}
	sites := &librarySearchSitesFake{}
	reader := &librarySearchReaderFake{
		thoughts:     []model.ReaderThoughtSearch{{ID: "thought-1", HostKind: "link", HostID: linkID.String(), LinkID: &linkID, Snippet: "matching thought"}},
		thoughtTotal: 4,
		thoughtNext:  "opaque-next",
		notes:        []model.ReaderNoteSearch{{ID: noteID, Title: "Published note", Snippet: "matching note", PublishedRevision: 2}},
		noteTotal:    3,
	}

	ctx := librarySearchIdentityContext(t, "30000000-0000-0000-0000-000000000003")
	got, err := NewLibrarySearchServiceWithMetrics(links, sites, nil, nil, reader).Search(ctx, "matching", 7, 8, 3, "")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if reader.thoughtQuery != "matching" || reader.thoughtAfter != "" || reader.noteQuery != "matching" || reader.thoughtLimit != 3 || reader.noteLimit != 7 {
		t.Fatalf("reader search calls = %#v", reader)
	}
	if got.Thoughts == nil || got.Thoughts.TotalHint != 4 || got.Thoughts.NextCursor == "" || got.Thoughts.NextCursor == "opaque-next" || len(got.Thoughts.Items) != 1 || got.Thoughts.Items[0].LinkID == nil || *got.Thoughts.Items[0].LinkID != linkID.String() {
		t.Fatalf("thought search group = %#v", got.Thoughts)
	}
	if got.Notes == nil || got.Notes.TotalHint != 3 || len(got.Notes.Items) != 1 || got.Notes.Items[0].ID != noteID.String() || got.Notes.Items[0].PublishedRevision != 2 {
		t.Fatalf("note search group = %#v", got.Notes)
	}
}

func TestLibrarySearchThoughtCursorRejectsTamperingAndCrossBinding(t *testing.T) {
	t.Parallel()
	const (
		query = "Matching Query"
		inner = "eyJ2IjoyLCJ1cGRhdGVkX2F0IjoiMjAyNi0wOC0xMVQwOTowMDowMFoiLCJ0aG91Z2h0X2lkIjoidGhvdWdodC0yMCIsInNjb3BlIjoic2NvcGUiLCJzbmFwc2hvdF9zZXF1ZW5jZSI6MTcsInNuYXBzaG90X2F0IjoiMjAyNi0wOC0xMVQwOTowMDowMVoiLCJ0b3RhbCI6MjF9"
	)
	installationA := librarySearchIdentityContext(t, "10000000-0000-0000-0000-000000000001")
	installationB := librarySearchIdentityContext(t, "20000000-0000-0000-0000-000000000002")
	reader := &librarySearchReaderFake{thoughtNext: inner}
	service := NewLibrarySearchServiceWithMetricsAndOptions(
		&librarySearchLinksFake{}, &librarySearchSitesFake{}, nil, nil,
		LibrarySearchServiceOptions{CursorSigningKey: "thought-search-cursor-key"},
		reader,
	)

	first, err := service.Search(installationA, query, 10, 10, 20, "")
	if err != nil || first.Thoughts == nil || first.Thoughts.NextCursor == "" {
		t.Fatalf("Search(first) = %#v, %v; want signed thought cursor", first, err)
	}
	cursor := first.Thoughts.NextCursor
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("thought cursor = %q, want payload.signature", cursor)
	}

	second, err := service.Search(installationA, "  matching query  ", 10, 10, 20, cursor)
	if err != nil || second.Thoughts == nil || reader.thoughtAfter != inner {
		t.Fatalf("Search(valid cursor) = %#v, %v; repository after=%q, want %q", second, err, reader.thoughtAfter, inner)
	}

	mutations := map[string]func(map[string]any){
		"installation namespace": func(payload map[string]any) { payload["client_data_namespace"] = strings.Repeat("A", 43) },
		"query scope":            func(payload map[string]any) { payload["query_scope"] = strings.Repeat("B", 43) },
	}
	for field, replacement := range map[string]any{
		"updated_at":        "2026-08-10T09:00:00Z",
		"thought_id":        "thought-19",
		"snapshot_sequence": float64(18),
		"snapshot_at":       "2026-08-11T09:00:02Z",
		"total":             float64(22),
	} {
		field := field
		replacement := replacement
		mutations[field] = func(payload map[string]any) {
			encoded, ok := payload["repository_cursor"].(string)
			if !ok {
				t.Fatalf("signed payload repository_cursor = %#v", payload["repository_cursor"])
			}
			decoded, decodeErr := base64.RawURLEncoding.DecodeString(encoded)
			if decodeErr != nil {
				t.Fatalf("decode repository cursor: %v", decodeErr)
			}
			var repositoryPayload map[string]any
			if unmarshalErr := json.Unmarshal(decoded, &repositoryPayload); unmarshalErr != nil {
				t.Fatalf("decode repository cursor payload: %v", unmarshalErr)
			}
			repositoryPayload[field] = replacement
			mutated, marshalErr := json.Marshal(repositoryPayload)
			if marshalErr != nil {
				t.Fatalf("encode tampered repository cursor: %v", marshalErr)
			}
			payload["repository_cursor"] = base64.RawURLEncoding.EncodeToString(mutated)
		}
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			assertInvalidThoughtCursor(t, service, installationA, query, mutateThoughtCursorPayload(t, cursor, mutate))
		})
	}

	assertInvalidThoughtCursor(t, service, installationA, "different query", cursor)
	assertInvalidThoughtCursor(t, service, installationA, " ", cursor)
	assertInvalidThoughtCursor(t, service, installationB, query, cursor)
	otherKeyService := NewLibrarySearchServiceWithMetricsAndOptions(
		&librarySearchLinksFake{}, &librarySearchSitesFake{}, nil, nil,
		LibrarySearchServiceOptions{CursorSigningKey: "different-thought-search-cursor-key"},
		&librarySearchReaderFake{},
	)
	assertInvalidThoughtCursor(t, otherKeyService, installationA, query, cursor)
	tamperedSignature := parts[1][:len(parts[1])-1] + "A"
	if strings.HasSuffix(parts[1], "A") {
		tamperedSignature = parts[1][:len(parts[1])-1] + "B"
	}
	assertInvalidThoughtCursor(t, service, installationA, query, parts[0]+"."+tamperedSignature)
	assertInvalidThoughtCursor(t, service, installationA, query, parts[0]+"."+parts[1][:len(parts[1])-1])
	assertInvalidThoughtCursor(t, service, installationA, query, "not-base64.not-base64")
	assertInvalidThoughtCursor(t, service, installationA, query, parts[0])
	assertInvalidThoughtCursor(t, service, installationA, query, inner)
}

func librarySearchIdentityContext(t *testing.T, installationID string) context.Context {
	t.Helper()
	identity, err := representation.NewClientIdentity(representation.VersionBase{RepresentationNamespace: uuid.MustParse(installationID)})
	if err != nil {
		t.Fatalf("NewClientIdentity(%q): %v", installationID, err)
	}
	return representation.WithClientIdentity(context.Background(), identity)
}

func mutateThoughtCursorPayload(t *testing.T, cursor string, mutate func(map[string]any)) string {
	t.Helper()
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 {
		t.Fatalf("thought cursor = %q, want payload.signature", cursor)
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode thought cursor payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("unmarshal thought cursor payload: %v", err)
	}
	mutate(payload)
	mutated, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal thought cursor payload: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(mutated) + "." + parts[1]
}

func assertInvalidThoughtCursor(t *testing.T, service *LibrarySearchService, ctx context.Context, query, cursor string) {
	t.Helper()
	_, err := service.Search(ctx, query, 10, 10, 20, cursor)
	var coder interface{ HTTPErrorCode() string }
	if !errors.As(err, &coder) || coder.HTTPErrorCode() != httperr.CodeInvalidCursor {
		t.Fatalf("Search(cursor=%q) error = %v, want %q", cursor, err, httperr.CodeInvalidCursor)
	}
}

func TestLibrarySearchEmptyQueryAvoidsStores(t *testing.T) {
	t.Parallel()
	links, sites := &librarySearchLinksFake{}, &librarySearchSitesFake{}
	got, err := NewLibrarySearchService(links, sites).Search(context.Background(), " \t", 10, 10, 20, "")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if links.calls != 0 || sites.calls != 0 || got.Reading.Items == nil || got.Sites.Items == nil {
		t.Fatalf("empty query result = %#v, calls links=%d sites=%d", got, links.calls, sites.calls)
	}
}

func TestLibrarySearchClampsLimitsAndRejectsLongQuery(t *testing.T) {
	t.Parallel()
	links, sites := &librarySearchLinksFake{}, &librarySearchSitesFake{}
	svc := NewLibrarySearchService(links, sites)
	if _, err := svc.Search(context.Background(), "find", 99, 0, 0, ""); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if links.filter == nil || links.filter.Limit != 50 || sites.limit != 10 {
		t.Fatalf("limits = links %#v sites %d, want 50 / 10", links.filter, sites.limit)
	}
	tooLong := strings.Repeat("x", maxListQueryLen+1)
	_, err := svc.Search(context.Background(), tooLong, 1, 1, 20, "")
	var httpErr *httperr.Error
	if !errors.As(err, &httpErr) || httpErr.HTTPStatus() != http.StatusUnprocessableEntity || httpErr.HTTPErrorCode() != httperr.CodeQueryTooLong {
		t.Fatalf("long query error = %v, want 422 query_too_long", err)
	}
	if links.calls != 1 || sites.calls != 1 {
		t.Fatalf("long query reached stores: links=%d sites=%d", links.calls, sites.calls)
	}
}
