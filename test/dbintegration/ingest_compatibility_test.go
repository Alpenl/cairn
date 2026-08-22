package dbintegration

import (
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/service/urllock"
)

func TestIngestService_RealDB_PrefersCanonicalSourceKeyOverLegacyURLDuplicate(t *testing.T) {
	pool := StartPostgres(t)
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire compatibility-test connection: %v", err)
	}
	defer conn.Release()
	for _, setting := range []string{
		"SET enable_indexscan = off",
		"SET enable_indexonlyscan = off",
		"SET enable_bitmapscan = off",
	} {
		if _, err := conn.Exec(t.Context(), setting); err != nil {
			t.Fatalf("apply %q: %v", setting, err)
		}
	}

	links := repository.NewPGXLinkRepository(conn)
	queue := &countingSubmitQueue{}
	commands := dbLinkCommands(pool, links, queue)
	_, ingest := service.NewLinkServices(
		links,
		commands,
		urllock.NewInProcessURLLocker(),
		service.SubmitServiceOptions{},
	)
	ctx := t.Context()
	const rawURL = "https://compat.example.com/legacy-capture-and-submit"

	legacyTitle := "Legacy capture"
	legacyBody := "Legacy captured body"
	legacy, err := links.Create(ctx, repository.CreateLinkParams{
		URL:        rawURL,
		SourceKind: "browser_capture",
		SourceKey:  "ingest:legacy-browser-capture",
		InputTitle: &legacyTitle,
		InputText:  &legacyBody,
		Status:     model.LinkStatusDone,
	})
	if err != nil {
		t.Fatalf("create legacy browser capture: %v", err)
	}
	canonical, err := links.Create(ctx, repository.CreateLinkParams{
		URL:        rawURL,
		SourceKind: "url",
		SourceKey:  rawURL,
		Status:     model.LinkStatusDone,
	})
	if err != nil {
		t.Fatalf("create canonical Submit row: %v", err)
	}
	urlOnly, err := ingest.Ingest(ctx, dto.IngestRequest{Destination: "library", Sources: []dto.IngestSource{{
		Kind: "url",
		URL:  rawURL,
	}}})
	if err != nil {
		t.Fatalf("URL-only Ingest() error = %v", err)
	}
	if urlOnly.LinkID != canonical.ID.String() || urlOnly.Status != string(model.LinkStatusDone) {
		t.Fatalf("URL-only Ingest() = %#v, want canonical done link %s", urlOnly, canonical.ID)
	}
	if got := queue.enqueueTxCalls.Load(); got != 0 {
		t.Fatalf("queue calls after URL-only reuse = %d, want 0", got)
	}

	newTitle := "Current capture"
	newBody := "Current captured body"
	rich, err := ingest.Ingest(ctx, dto.IngestRequest{Destination: "library", Sources: []dto.IngestSource{{
		Kind:  "browser_capture",
		URL:   rawURL,
		Title: newTitle,
		Text:  newBody,
	}}})
	if err != nil {
		t.Fatalf("rich Ingest() error = %v", err)
	}
	if rich.LinkID != canonical.ID.String() || rich.Status != string(model.LinkStatusPending) {
		t.Fatalf("rich Ingest() = %#v, want canonical link %s requeued", rich, canonical.ID)
	}
	if got := queue.enqueueTxCalls.Load(); got != 1 {
		t.Fatalf("queue calls after rich capture = %d, want 1", got)
	}

	canonicalAfter, err := links.GetByID(ctx, canonical.ID)
	if err != nil {
		t.Fatalf("read canonical link: %v", err)
	}
	if canonicalAfter == nil || canonicalAfter.SourceKind != "browser_capture" || canonicalAfter.SourceKey != rawURL || canonicalAfter.InputTitle == nil || *canonicalAfter.InputTitle != newTitle || canonicalAfter.InputText == nil || *canonicalAfter.InputText != newBody {
		t.Fatalf("canonical link after capture = %#v, want current browser capture with canonical source key", canonicalAfter)
	}
	legacyAfter, err := links.GetByID(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("read legacy link: %v", err)
	}
	if legacyAfter == nil || legacyAfter.SourceKey != "ingest:legacy-browser-capture" || legacyAfter.InputText == nil || *legacyAfter.InputText != legacyBody {
		t.Fatalf("legacy link was unexpectedly rewritten: %#v", legacyAfter)
	}
	if got := rawCountLinks(t, pool); got != 2 {
		t.Fatalf("links after compatibility path = %d, want both unmigrated rows preserved", got)
	}
}

func TestIngestService_RealDB_NoteOnlyRecaptureInvalidatesUserContent(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	queue := &countingSubmitQueue{}
	commands := dbLinkCommands(pool, links, queue)
	_, ingest := service.NewLinkServices(
		links,
		commands,
		urllock.NewInProcessURLLocker(),
		service.SubmitServiceOptions{},
	)
	ctx := t.Context()

	const (
		rawURL  = "https://capture.example.com/note-only-recapture"
		title   = "Captured article"
		body    = "The captured source body stays byte-for-byte identical."
		oldNote = "Read this before the meeting"
		newNote = "Read this after the meeting"
	)
	request := func(note string) dto.IngestRequest {
		return dto.IngestRequest{
			Destination:          "library",
			RequestedLibraryKind: string(model.RequestedLibraryKindReading),
			Sources: []dto.IngestSource{{
				Kind:     "browser_capture",
				URL:      rawURL,
				Title:    title,
				Text:     body,
				Metadata: map[string]any{"note": note},
			}},
		}
	}

	created, err := ingest.Ingest(ctx, request(oldNote))
	if err != nil {
		t.Fatalf("initial Ingest() error = %v", err)
	}
	linkID, err := uuid.Parse(created.LinkID)
	if err != nil {
		t.Fatalf("initial Ingest() link_id = %q: %v", created.LinkID, err)
	}
	if created.Status != string(model.LinkStatusPending) {
		t.Fatalf("initial Ingest() = %#v, want pending link", created)
	}
	if err := links.UpdateState(ctx, repository.UpdateLinkStateParams{ID: linkID, Status: model.LinkStatusDone}); err != nil {
		t.Fatalf("mark initial capture done: %v", err)
	}

	parsed, err := links.GetByID(ctx, linkID)
	if err != nil || parsed == nil {
		t.Fatalf("GetByID(initial capture) = %#v, %v", parsed, err)
	}
	savedRevision, stored, err := links.UpdateContentIfCurrent(ctx, linkID, parsed.UpdatedAt, model.SavedContent{
		Text:     "Fetched saved body",
		Format:   model.ContentFormatPlain,
		CJKChars: 3,
		Words:    7,
	})
	if err != nil || !stored {
		t.Fatalf("UpdateContentIfCurrent() = revision %d, stored %v, error %v", savedRevision, stored, err)
	}
	editedDocument := "# User-edited body\n\nThis document must be invalidated by the new capture revision."
	editedRevision, edited, err := links.EditContentIfRevision(ctx, linkID, savedRevision, model.SavedContent{
		Text:     "User-edited saved body",
		Document: &editedDocument,
		Format:   model.ContentFormatMarkdown,
		CJKChars: 5,
		Words:    11,
	})
	if err != nil || !edited {
		t.Fatalf("EditContentIfRevision() = revision %d, edited %v, error %v", editedRevision, edited, err)
	}
	editedContent, err := links.GetContent(ctx, linkID)
	if err != nil || editedContent == nil {
		t.Fatalf("GetContent(user edit) = %#v, %v", editedContent, err)
	}
	if editedContent.Source != model.ContentSourceUser || editedContent.Revision != editedRevision ||
		editedContent.Text != "User-edited saved body" || editedContent.Document == nil ||
		*editedContent.Document != editedDocument || editedContent.Format != model.ContentFormatMarkdown {
		t.Fatalf("saved user edit before recapture = %#v, want user-authored markdown at revision %d", editedContent, editedRevision)
	}

	recaptured, err := ingest.Ingest(ctx, request(newNote))
	if err != nil {
		t.Fatalf("note-only recapture Ingest() error = %v", err)
	}
	if recaptured.LinkID != linkID.String() || recaptured.Status != string(model.LinkStatusPending) {
		t.Fatalf("note-only recapture Ingest() = %#v, want same pending link", recaptured)
	}
	if got := queue.enqueueTxCalls.Load(); got != 2 {
		t.Fatalf("queue calls after initial capture and note-only recapture = %d, want 2", got)
	}

	var (
		status          string
		description     string
		inputTitle      string
		inputText       string
		content         *string
		document        *string
		format          string
		source          string
		contentCJK      int
		contentWords    int
		contentRevision int64
	)
	if err := pool.QueryRow(ctx, `
		SELECT status, description, input_title, input_text,
		       content, content_document, content_format, content_source,
		       content_cjk_chars, content_words, content_revision
		  FROM links
		 WHERE id = $1`, linkID).Scan(
		&status, &description, &inputTitle, &inputText,
		&content, &document, &format, &source,
		&contentCJK, &contentWords, &contentRevision,
	); err != nil {
		t.Fatalf("query note-only recapture state: %v", err)
	}
	if status != string(model.LinkStatusPending) || description != newNote {
		t.Fatalf("recaptured status/note = %q/%q, want pending/%q", status, description, newNote)
	}
	if inputTitle != title || inputText != body {
		t.Fatalf("recaptured title/body = %q/%q, want unchanged %q/%q", inputTitle, inputText, title, body)
	}
	if content != nil || document != nil || format != string(model.ContentFormatPlain) {
		t.Fatalf("recaptured content/document/format = %#v/%#v/%q, want nil/nil/plain", content, document, format)
	}
	if source != string(model.ContentSourceFetched) || contentCJK != 0 || contentWords != 0 {
		t.Fatalf("recaptured source/counts = %q/%d/%d, want fetched/0/0", source, contentCJK, contentWords)
	}
	if contentRevision != editedRevision+1 {
		t.Fatalf("recaptured content_revision = %d, want exactly %d", contentRevision, editedRevision+1)
	}
}
