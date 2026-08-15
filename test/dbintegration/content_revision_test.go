package dbintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/fetcher"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

type blockingContentFetcher struct {
	started chan struct{}
	release chan struct{}
	body    string
}

func (f *blockingContentFetcher) Fetch(ctx context.Context, _ string) (fetcher.Content, error) {
	close(f.started)
	select {
	case <-f.release:
		return fetcher.Content{Body: f.body, FetcherType: "test"}, nil
	case <-ctx.Done():
		return fetcher.Content{}, ctx.Err()
	}
}

func TestContentSaveCannotCrossCaptureRevision(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()

	inputA := "captured body A"
	inputB := "captured body B"
	link, _, err := repo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/content-revision",
		SourceKind: "browser_capture",
		SourceKey:  "https://example.com/content-revision",
		InputText:  &inputA,
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew(capture A): %v", err)
	}
	if err := repo.UpdateState(ctx, repository.UpdateLinkStateParams{ID: link.ID, Status: model.LinkStatusDone}); err != nil {
		t.Fatalf("mark capture A done: %v", err)
	}
	current, err := repo.GetByID(ctx, link.ID)
	if err != nil || current == nil {
		t.Fatalf("GetByID(capture A) = %#v, %v", current, err)
	}
	_, stored, err := repo.UpdateContentIfCurrent(ctx, link.ID, current.UpdatedAt, model.SavedContent{
		Text:   inputA,
		Format: model.ContentFormatPlain,
	})
	if err != nil || !stored {
		t.Fatalf("store capture A content = %v, %v; want true, nil", stored, err)
	}
	afterContent, err := repo.GetByID(ctx, link.ID)
	if err != nil || afterContent == nil {
		t.Fatalf("GetByID(after content save) = %#v, %v", afterContent, err)
	}
	if !afterContent.UpdatedAt.Equal(current.UpdatedAt) {
		t.Fatalf("content save changed parse revision from %v to %v", current.UpdatedAt, afterContent.UpdatedAt)
	}
	markdown := "# Capture A\n\nStructured body"
	_, replaced, err := repo.ReplaceContentIfCurrent(ctx, link.ID, current.UpdatedAt, model.SavedContent{
		Text:     "Capture A\n\nStructured body",
		Document: &markdown,
		Format:   model.ContentFormatMarkdown,
	})
	if err != nil || !replaced {
		t.Fatalf("replace capture A content = %v, %v; want true, nil", replaced, err)
	}
	structured, err := repo.GetContent(ctx, link.ID)
	if err != nil || structured == nil || structured.Document == nil {
		t.Fatalf("GetContent(structured) = %#v, %v", structured, err)
	}
	if structured.Format != model.ContentFormatMarkdown || *structured.Document != markdown {
		t.Fatalf("structured content = %#v, want markdown snapshot", structured)
	}

	if _, err := repo.RequeueExisting(ctx, link.ID, &repository.CreateLinkParams{
		URL:        link.URL,
		SourceKind: "browser_capture",
		SourceKey:  link.SourceKey,
		InputText:  &inputB,
		Status:     model.LinkStatusPending,
	}); err != nil {
		t.Fatalf("RequeueExistingTx(capture B): %v", err)
	}
	assertLinkRevisionState(t, pool, link.ID.String(), model.LinkStatusPending, inputB, nil)

	remote, _, err := repo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/remote-before-capture",
		SourceKind: "url",
		SourceKey:  "https://example.com/remote-before-capture",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew(remote): %v", err)
	}
	if err := repo.UpdateState(ctx, repository.UpdateLinkStateParams{ID: remote.ID, Status: model.LinkStatusDone}); err != nil {
		t.Fatalf("mark remote done: %v", err)
	}

	blocking := &blockingContentFetcher{
		started: make(chan struct{}),
		release: make(chan struct{}),
		body:    "stale remote body",
	}
	contentService := service.NewContentService(repo, blocking, nil)
	saveDone := make(chan error, 1)
	go func() {
		_, saveErr := contentService.Save(context.Background(), remote.ID.String())
		saveDone <- saveErr
	}()

	select {
	case <-blocking.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Content Save did not start fetching")
	}
	if _, err := repo.RequeueExisting(ctx, remote.ID, &repository.CreateLinkParams{
		URL:        remote.URL,
		SourceKind: "browser_capture",
		SourceKey:  remote.SourceKey,
		InputText:  &inputB,
		Status:     model.LinkStatusPending,
	}); err != nil {
		close(blocking.release)
		t.Fatalf("RequeueExistingTx(remote -> capture B): %v", err)
	}
	close(blocking.release)

	saveErr := <-saveDone
	var httpErr *httperr.Error
	if !errors.As(saveErr, &httpErr) || httpErr.HTTPStatus() != 409 || httpErr.HTTPErrorCode() != httperr.CodeLinkNotReady {
		t.Fatalf("Save() error = %v, want 409/%s", saveErr, httperr.CodeLinkNotReady)
	}
	assertLinkRevisionState(t, pool, remote.ID.String(), model.LinkStatusPending, inputB, nil)
}

func assertLinkRevisionState(
	t *testing.T,
	pool *pgxpool.Pool,
	linkID string,
	wantStatus model.LinkStatus,
	wantInput string,
	wantContent *string,
) {
	t.Helper()
	var (
		status   string
		input    *string
		content  *string
		document *string
		format   string
	)
	if err := pool.QueryRow(t.Context(),
		`SELECT status, input_text, content, content_document, content_format FROM links WHERE id = $1`,
		linkID,
	).Scan(&status, &input, &content, &document, &format); err != nil {
		t.Fatalf("query link revision state: %v", err)
	}
	if status != string(wantStatus) || input == nil || *input != wantInput {
		t.Fatalf("link state = status %q input %#v, want %q/%q", status, input, wantStatus, wantInput)
	}
	if (content == nil) != (wantContent == nil) || content != nil && *content != *wantContent {
		t.Fatalf("content = %#v, want %#v", content, wantContent)
	}
	if wantContent == nil && (document != nil || format != "plain") {
		t.Fatalf("cleared content left document/format = %#v/%q, want nil/plain", document, format)
	}
}

// TestContentWritesBumpContentRevision pins the token Reader's saved-original
// cache is keyed by.
//
// Reader caches saved originals under (linkId, content_revision) and — since
// PF3/PF4 — serves a cache hit WITHOUT revalidating. That is only sound while
// content_revision actually moves when the stored text changes. It did not:
// the column used to move only on the paths that set content to NULL, so a
// replaced original kept the same key while every other visible column
// (updated_at, has_content) stayed identical too. A client holding the old
// text never learned it changed — permanently, and across refreshes, since the
// entry is persisted to IndexedDB.
//
// The assertions are strict equality on the delta rather than "changed",
// because a token that jumps by an arbitrary amount would also pass a
// "!= before" check while breaking the conversion CAS that compares exact
// values.
func TestContentWritesBumpContentRevision(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()

	link, _, err := repo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/content-revision-bump",
		SourceKind: "url",
		SourceKey:  "https://example.com/content-revision-bump",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}
	if err := repo.UpdateState(ctx, repository.UpdateLinkStateParams{ID: link.ID, Status: model.LinkStatusDone}); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	revisionNow := func(stage string) int64 {
		t.Helper()
		got, getErr := repo.GetByID(ctx, link.ID)
		if getErr != nil || got == nil {
			t.Fatalf("GetByID(%s) = %#v, %v", stage, got, getErr)
		}
		return got.ContentRevision
	}

	before := revisionNow("before save")
	parsedAt, err := repo.GetByID(ctx, link.ID)
	if err != nil || parsedAt == nil {
		t.Fatalf("GetByID(parsed) = %#v, %v", parsedAt, err)
	}

	savedRevision, stored, err := repo.UpdateContentIfCurrent(ctx, link.ID, parsedAt.UpdatedAt, model.SavedContent{
		Text:   "first saved original",
		Format: model.ContentFormatPlain,
	})
	if err != nil || !stored {
		t.Fatalf("UpdateContentIfCurrent = %v, %v; want true, nil", stored, err)
	}
	afterSave := revisionNow("after save")
	if afterSave != before+1 {
		t.Fatalf("content_revision after save = %d, want %d — Reader's content cache key never changes without this", afterSave, before+1)
	}
	// The value handed back to the caller must be the post-write generation, not
	// a stale read: it goes straight into the API response, and a client that
	// stores annotations against a wrong generation silently loses them on the
	// next list refresh.
	if savedRevision != afterSave {
		t.Fatalf("UpdateContentIfCurrent returned revision %d but the row is at %d", savedRevision, afterSave)
	}

	replacedRevision, replaced, err := repo.ReplaceContentIfCurrent(ctx, link.ID, parsedAt.UpdatedAt, model.SavedContent{
		Text:   "replaced original",
		Format: model.ContentFormatPlain,
	})
	if err != nil || !replaced {
		t.Fatalf("ReplaceContentIfCurrent = %v, %v; want true, nil", replaced, err)
	}
	afterReplace := revisionNow("after replace")
	if afterReplace != afterSave+1 {
		t.Fatalf("content_revision after replace = %d, want %d — replacing is exactly the case a cached copy goes stale invisibly", afterReplace, afterSave+1)
	}
	if replacedRevision != afterReplace {
		t.Fatalf("ReplaceContentIfCurrent returned revision %d but the row is at %d", replacedRevision, afterReplace)
	}
	// The read path must report the same generation the write just produced,
	// otherwise the idempotent "already saved" branch answers with 0.
	readBack, err := repo.GetContent(ctx, link.ID)
	if err != nil || readBack == nil {
		t.Fatalf("GetContent(after replace) = %#v, %v", readBack, err)
	}
	if readBack.Revision != afterReplace {
		t.Fatalf("GetContent revision = %d, want %d", readBack.Revision, afterReplace)
	}

	// The parse-source token must stay put: bumping it here would make every
	// caller holding an expectedUpdatedAt fail its CAS after an unrelated
	// content save. The two tokens track different things on purpose.
	final, err := repo.GetByID(ctx, link.ID)
	if err != nil || final == nil {
		t.Fatalf("GetByID(final) = %#v, %v", final, err)
	}
	if !final.UpdatedAt.Equal(parsedAt.UpdatedAt) {
		t.Fatalf("content writes moved updated_at from %v to %v; it identifies the parsed source revision, not the content", parsedAt.UpdatedAt, final.UpdatedAt)
	}

	// A LOSING CAS must not bump either. The bump lives inside the same
	// UPDATE as the CAS predicate, so this holds by construction — but only
	// as long as it stays there. Splitting the bump into its own statement
	// (a plausible "let's simplify this SQL" refactor) keeps every
	// success-path assertion above green while bumping on every failed
	// attempt: that would spuriously invalidate other clients' content cache
	// keys and shoot down live conversion previews.
	staleExpectation := parsedAt.UpdatedAt.Add(time.Second)
	lostRevision, lost, err := repo.ReplaceContentIfCurrent(ctx, link.ID, staleExpectation, model.SavedContent{
		Text:   "must not be written",
		Format: model.ContentFormatPlain,
	})
	if err != nil {
		t.Fatalf("ReplaceContentIfCurrent(stale) error = %v; a CAS miss is an expected outcome, not an error", err)
	}
	if lost {
		t.Fatal("ReplaceContentIfCurrent(stale) = true; the CAS predicate is not holding")
	}
	if lostRevision != 0 {
		t.Fatalf("ReplaceContentIfCurrent(stale) revision = %d; a losing write reports no generation", lostRevision)
	}
	if after := revisionNow("after losing CAS"); after != afterReplace {
		t.Fatalf("content_revision moved to %d on a LOSING write (was %d) — the bump escaped the CAS statement", after, afterReplace)
	}
}
