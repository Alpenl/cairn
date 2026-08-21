package dbintegration

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/urlidentity"
)

// TestReaderVNextPostgresCrossSurfaceChain is the executable database evidence
// for the Reader vNext paths that cross more than one repository surface. The
// browser and handler tests prove the HTTP wiring; this test keeps the
// transaction, trigger, and revision behavior honest against real
// PostgreSQL.
func TestReaderVNextPostgresCrossSurfaceChain(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	links := repository.NewPGXLinkRepository(pool)

	// capture -> inbox -> confirm -> read
	inboxID := seedReaderVNextInbox(t, pool, "https://reader-vnext.example/captured", "Captured article", "Captured article body", "Captured article summary")
	savedLinkFixtureID := seedReaderVNextSavedLink(t, pool, "https://reader-vnext.example/captured", "Captured article", "Captured article body", "Captured article summary")
	inboxItems, _, _, _, err := reader.ListInbox(ctx, model.ReaderInboxPartitionActive, "", 20)
	if err != nil {
		t.Fatalf("ListInbox before confirm: %v", err)
	}
	if len(inboxItems) != 1 || inboxItems[0].ID != inboxID || inboxItems[0].Status != "pending" {
		t.Fatalf("ListInbox before confirm = %#v, want the pending capture", inboxItems)
	}

	linkID, err := reader.ConfirmInbox(ctx, inboxID, nil)
	if err != nil {
		t.Fatalf("ConfirmInbox: %v", err)
	}
	confirmedLinkID, err := reader.ConfirmInbox(ctx, inboxID, nil)
	if err != nil {
		t.Fatalf("ConfirmInbox idempotent retry: %v", err)
	}
	if confirmedLinkID != linkID {
		t.Fatalf("ConfirmInbox retry returned %s, want stable link %s", confirmedLinkID, linkID)
	}
	if linkID != savedLinkFixtureID {
		t.Fatalf("ConfirmInbox returned %s, want the pre-seeded saved link %s", linkID, savedLinkFixtureID)
	}
	confirmedInbox, err := reader.GetInbox(ctx, inboxID)
	if err != nil {
		t.Fatalf("GetInbox after confirm: %v", err)
	}
	if confirmedInbox.Status != "confirmed" {
		t.Fatalf("confirmed inbox status = %q, want confirmed", confirmedInbox.Status)
	}
	remainingInbox, _, _, _, err := reader.ListInbox(ctx, model.ReaderInboxPartitionActive, "", 20)
	if err != nil {
		t.Fatalf("ListInbox after confirm: %v", err)
	}
	if len(remainingInbox) != 0 {
		t.Fatalf("ListInbox after confirm = %#v, want no pending rows", remainingInbox)
	}

	link, err := links.GetByID(ctx, linkID)
	if err != nil || link == nil {
		t.Fatalf("GetByID confirmed link = %#v, %v", link, err)
	}
	if link.Status != model.LinkStatusDone || link.ContentRevision != 1 {
		t.Fatalf("confirmed link state = status=%q content_revision=%d, want done/1", link.Status, link.ContentRevision)
	}
	content, err := links.GetContent(ctx, linkID)
	if err != nil || content == nil || content.Text != "Captured article body" {
		t.Fatalf("GetContent confirmed link = %#v, %v", content, err)
	}

	// thought -> sync -> search -> note -> TODO
	initialNoteContent := "# Captured\n\nOriginal quote\n\n- [ ] Re-read the captured article"
	note, err := reader.CreateNote(ctx, model.ReaderNote{Title: "Captured follow-up", PublishedContent: initialNoteContent})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	thoughtID := "thought-" + uuid.NewString()
	initialTarget := readerVNextJSON(t, map[string]any{
		"kind":    "note",
		"host_id": note.ID.String(),
		"version": map[string]any{"note_revision": note.PublishedRevision},
	})
	thoughtPayload := readerVNextJSON(t, map[string]any{
		"body":   "Remember this captured idea",
		"quote":  map[string]any{"exact": "Original quote"},
		"source": "user",
	})
	acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID:          "op-" + uuid.NewString(),
		DeviceID:      "reader-device-a",
		LogicalClock:  1,
		OperationKind: "add",
		AnnotationID:  thoughtID,
		HostKind:      "note",
		HostID:        note.ID.String(),
		Target:        initialTarget,
		Payload:       thoughtPayload,
	}})
	if err != nil {
		t.Fatalf("AppendThoughtOps: %v", err)
	}
	if len(acks) != 1 || acks[0].Sequence <= 0 {
		t.Fatalf("AppendThoughtOps acks = %#v, want one server sequence", acks)
	}

	thoughts, syncCursor, err := reader.ListThoughtsSince(ctx, "", 20)
	if err != nil {
		t.Fatalf("ListThoughtsSince: %v", err)
	}
	if len(thoughts) != 1 || thoughts[0].ID != thoughtID || thoughts[0].Body != "Remember this captured idea" || syncCursor == "" {
		t.Fatalf("ListThoughtsSince = items=%#v cursor=%q", thoughts, syncCursor)
	}
	searchThoughts, thoughtTotal, _, err := reader.SearchThoughts(ctx, "captured idea", "", 20)
	if err != nil {
		t.Fatalf("SearchThoughts: %v", err)
	}
	if thoughtTotal != 1 || len(searchThoughts) != 1 || searchThoughts[0].ID != thoughtID {
		t.Fatalf("SearchThoughts = items=%#v total=%d, want the durable thought", searchThoughts, thoughtTotal)
	}

	draftContent := "# Captured\n\nUpdated quote\n\n- [ ] Re-read the captured article"
	draft, err := reader.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{
		NoteID:                note.ID,
		Content:               draftContent,
		ExpectedDraftRevision: note.DraftRevision,
	})
	if err != nil {
		t.Fatalf("SaveNoteDraft: %v", err)
	}
	reanchorExact := "Updated quote"
	quoteStart := utf16Length(draftContent[:strings.Index(draftContent, reanchorExact)])
	reanchor := readerVNextJSON(t, map[string]any{
		"thought_id": thoughtID,
		"status":     "reanchored",
		"reason":     "unique-quote",
		"target": map[string]any{
			"kind":    "note",
			"host_id": note.ID.String(),
			"version": map[string]any{"note_revision": note.PublishedRevision + 1},
		},
		"quote": map[string]any{"exact": reanchorExact},
		"range": map[string]any{
			"start": quoteStart,
			"end":   quoteStart + utf16Length(reanchorExact),
		},
	})
	published, err := reader.PublishNote(ctx, model.ReaderNotePublishCommand{
		NoteID:                    note.ID,
		ExpectedDraftRevision:     draft.DraftRevision,
		ExpectedPublishedRevision: note.PublishedRevision,
		ReanchorOps:               []json.RawMessage{reanchor},
	})
	if err != nil {
		t.Fatalf("PublishNote with reanchor: %v", err)
	}
	if published.PublishedRevision != 2 || published.PublishedContent != draftContent || published.DraftContent != nil {
		t.Fatalf("published note = %#v, want revision 2 and no draft", published)
	}

	reattachedThought, err := reader.GetThought(ctx, thoughtID)
	if err != nil {
		t.Fatalf("GetThought after note publish: %v", err)
	}
	var reattachedTarget struct {
		Version struct {
			NoteRevision int64 `json:"note_revision"`
		} `json:"version"`
	}
	if err := json.Unmarshal(reattachedThought.Target, &reattachedTarget); err != nil {
		t.Fatalf("decode reattached thought target %s: %v", reattachedThought.Target, err)
	}
	if reattachedTarget.Version.NoteRevision != 2 {
		t.Fatalf("reattached thought target = %s, want note revision 2", reattachedThought.Target)
	}
	searchedNotes, noteTotal, err := reader.SearchPublishedNotes(ctx, "Updated quote", 20)
	if err != nil {
		t.Fatalf("SearchPublishedNotes: %v", err)
	}
	if noteTotal != 1 || len(searchedNotes) != 1 || searchedNotes[0].ID != note.ID {
		t.Fatalf("SearchPublishedNotes = items=%#v total=%d, want published note", searchedNotes, noteTotal)
	}
	noteHistory, err := reader.ListNoteHistory(ctx, note.ID, 20)
	if err != nil || len(noteHistory) != 1 || noteHistory[0].Revision != 2 {
		t.Fatalf("ListNoteHistory = %#v, %v; want published revision 2", noteHistory, err)
	}

	todos, err := reader.ListTodos(ctx, "", 200)
	if err != nil || len(todos.Items) != 1 || todos.Items[0].Text != "Re-read the captured article" || todos.Items[0].HostRevision != published.PublishedRevision {
		t.Fatalf("ListTodos before completion = %#v, %v", todos, err)
	}
	done := true
	completedTodo, err := reader.PatchTodo(ctx, model.ReaderTodoPatch{
		ID:                   todos.Items[0].ID,
		Done:                 &done,
		ExpectedHostRevision: &todos.Items[0].HostRevision,
	})
	if err != nil {
		t.Fatalf("PatchTodo projected note: %v", err)
	}
	if !completedTodo.Done || completedTodo.HostRevision != published.PublishedRevision+1 {
		t.Fatalf("completed projected TODO = %#v, want done at note revision %d", completedTodo, published.PublishedRevision+1)
	}
	updatedNote, err := reader.GetNote(ctx, note.ID)
	if err != nil || updatedNote.PublishedRevision != published.PublishedRevision+1 || !strings.Contains(updatedNote.PublishedContent, "[x] Re-read") {
		t.Fatalf("note after TODO completion = %#v, %v", updatedNote, err)
	}

	// Feed action -> shared engagement -> Home aggregate. Keep a second Inbox
	// item active and a third one expired so the mixed Feed has both durable
	// reading and active Inbox cards, while expired captures never leak into
	// the Feed.
	pendingInboxID := seedReaderVNextInbox(t, pool, "https://reader-vnext.example/pending", "Pending feed capture", "Pending feed body", "")
	expiredInboxID := seedReaderVNextInbox(t, pool, "https://reader-vnext.example/expired", "Expired feed capture", "Expired feed body", "")
	if _, err := pool.Exec(t.Context(), `UPDATE reader_inbox SET expires_at=NOW() - INTERVAL '1 second' WHERE id=$1`, expiredInboxID); err != nil {
		t.Fatalf("expire Feed Inbox fixture: %v", err)
	}
	feedPage, err := reader.ListFeedWithSources(ctx, "recommended", "", nil, 50)
	if err != nil {
		t.Fatalf("ListFeed mixed page: %v", err)
	}
	var feedLink, feedInbox, expiredFeedInbox bool
	for _, item := range feedPage.Items {
		if item.LinkID != nil && *item.LinkID == linkID && item.Source == "reading" {
			feedLink = true
		}
		if item.InboxID != nil && *item.InboxID == pendingInboxID && item.Source == "inbox" {
			feedInbox = true
		}
		if item.InboxID != nil && *item.InboxID == expiredInboxID {
			expiredFeedInbox = true
		}
	}
	if !feedLink || !feedInbox || expiredFeedInbox {
		t.Fatalf("mixed Feed page = %#v, want reading link and active Inbox only", feedPage)
	}
	if _, err := reader.FeedbackFeed(ctx, "link:"+linkID.String(), "hide"); err != nil {
		t.Fatalf("FeedbackFeed hide action: %v", err)
	}
	engagement, err := reader.GetEngagement(ctx, linkID)
	if err != nil || engagement == nil || engagement.ReadLater {
		t.Fatalf("GetEngagement after Feed hide = %#v, %v; hide must not set read_later", engagement, err)
	}
	read := true
	progress := float32(0.4)
	engagement, err = reader.PatchEngagement(ctx, model.ReaderEngagementPatch{
		LinkID:   linkID,
		Read:     &read,
		Progress: &progress,
	})
	if err != nil || engagement == nil || !engagement.Read || engagement.Progress != progress {
		t.Fatalf("PatchEngagement from Feed action = %#v, %v", engagement, err)
	}
	home, err := reader.LoadHomeAggregate(ctx)
	if err != nil {
		t.Fatalf("LoadHomeAggregate after Feed action: %v", err)
	}
	if home.Freshness != repository.ReaderHomeFreshnessFresh || home.Counts["pending"] != 1 || home.Counts["reading"] != 1 || home.Counts["notes"] != 1 {
		t.Fatalf("Home aggregate = freshness=%q counts=%#v", home.Freshness, home.Counts)
	}
	if len(home.ContinueReading) != 1 || home.ContinueReading[0].LinkID == nil || *home.ContinueReading[0].LinkID != linkID {
		t.Fatalf("Home continue reading = %#v, want Feed link %s", home.ContinueReading, linkID)
	}

}

func seedReaderVNextInbox(t *testing.T, pool *pgxpool.Pool, rawURL, title, body, summary string) uuid.UUID {
	t.Helper()
	identityKey, err := urlidentity.Normalize(rawURL)
	if err != nil {
		t.Fatalf("normalize Reader Inbox URL %s: %v", rawURL, err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO reader_inbox (url,identity_key,source_kind,title,body,summary,suggested_tags,tags)
		VALUES ($1,$2,'browser_capture',$3,$4,NULLIF($5,''),ARRAY[]::text[],ARRAY[]::text[])
		RETURNING id`, rawURL, identityKey, title, body, summary).Scan(&id); err != nil {
		t.Fatalf("seed Reader Inbox %s: %v", rawURL, err)
	}
	return id
}

func seedReaderVNextSavedLink(t *testing.T, pool *pgxpool.Pool, rawURL, title, body, summary string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO links (
			url,source_kind,source_key,input_title,input_text,title,summary,tags,status,
			content,content_document,content_format,content_source,content_revision,
			library_kind,library_kind_locked,first_collected_at,created_at,updated_at)
		VALUES ($1,'browser_capture',$1,$3,$2,$3,$4,ARRAY[]::text[],'done',$2,$2,'markdown','user',1,'reading',false,NOW(),NOW(),NOW())
		RETURNING id`, rawURL, body, title, summary).Scan(&id); err != nil {
		t.Fatalf("seed Reader saved link %s: %v", rawURL, err)
	}
	return id
}

func seedReaderVNextNote(t *testing.T, pool *pgxpool.Pool, title, content string) model.ReaderNote {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO reader_notes (title,published_content,published_revision,draft_content,draft_revision)
		VALUES ($1,$2,1,NULL::text,0)
		RETURNING id`, title, content).Scan(&id); err != nil {
		t.Fatalf("seed Reader note %s: %v", title, err)
	}
	return model.ReaderNote{ID: id, Title: title, PublishedContent: content, PublishedRevision: 1}
}

func readerVNextJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal fixture: %v", err)
	}
	return encoded
}

func utf16Length(value string) int {
	return len(utf16.Encode([]rune(value)))
}
