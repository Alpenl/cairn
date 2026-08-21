package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/readertext"
	"webtag/internal/repository"
)

type readerVNextChainStore struct {
	repository.ReaderVNextStore

	inbox             model.ReaderInbox
	linkID            uuid.UUID
	thoughts          map[string]model.ReaderThought
	thoughtOps        []model.ReaderThoughtOp
	note              model.ReaderNote
	todos             []model.ReaderTodo
	engagement        model.ReaderEngagement
	feed              *model.ReaderFeedPage
	feedbackKey       string
	feedback          string
	bulkConfirmations []model.ReaderInboxBulkConfirmation
	now               time.Time
}

func newReaderVNextChainStore() *readerVNextChainStore {
	now := time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC)
	inboxID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	linkID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	return &readerVNextChainStore{
		inbox: model.ReaderInbox{
			ID: inboxID, URL: "https://capture.example.test/article", SourceKind: "browser_capture",
			Body: "Captured body from the browser extension.", Status: "pending", MetadataRevision: 1,
			CreatedAt: now, UpdatedAt: now,
		},
		linkID:     linkID,
		thoughts:   make(map[string]model.ReaderThought),
		engagement: model.ReaderEngagement{LinkID: linkID, UpdatedAt: now},
		feed: &model.ReaderFeedPage{
			Mode: "recommended",
			Items: []model.ReaderFeedItem{{
				Key: "link:" + linkID.String(), Source: "reading", LinkID: &linkID,
				Title: "Captured article", Summary: "Captured summary", URL: "https://capture.example.test/article",
				CreatedAt: now,
			}},
		},
		now: now,
	}
}

func stringPointer(value string) *string {
	return &value
}

func (s *readerVNextChainStore) CreateInbox(_ context.Context, item model.ReaderInbox) (*model.ReaderInbox, error) {
	s.inbox.URL, s.inbox.SourceKind, s.inbox.Title, s.inbox.Body = item.URL, item.SourceKind, item.Title, item.Body
	s.inbox.Summary, s.inbox.Tags, s.inbox.SuggestedTags = item.Summary, item.Tags, item.SuggestedTags
	s.inbox.Status, s.inbox.UpdatedAt = "pending", s.now
	return cloneReaderInbox(s.inbox), nil
}

func (s *readerVNextChainStore) CreateInboxProposal(ctx context.Context, command CreateInboxProposalCommand) (InboxProposalResult, error) {
	item, err := s.CreateInbox(ctx, command.Inbox)
	return InboxProposalResult{Inbox: item}, err
}

func (s *readerVNextChainStore) EnsureInboxProposal(_ context.Context, command EnsureInboxProposalCommand) (InboxProposalResult, error) {
	if command.InboxID != s.inbox.ID {
		return InboxProposalResult{}, repository.ErrNotFound
	}
	return InboxProposalResult{Inbox: cloneReaderInbox(s.inbox)}, nil
}

func (s *readerVNextChainStore) ListInbox(_ context.Context, partition model.ReaderInboxPartition, _ string, _ int) ([]model.ReaderInboxListItem, int, int, string, error) {
	if s.inbox.Status != "pending" {
		return []model.ReaderInboxListItem{}, 0, 0, "", nil
	}
	if partition != model.ReaderInboxPartitionActive || s.inbox.Expired {
		return []model.ReaderInboxListItem{}, 0, 1, "", nil
	}
	return []model.ReaderInboxListItem{readerInboxListItemFixture(*cloneReaderInbox(s.inbox))}, 1, 0, "", nil
}

// readerInboxListItemFixture mirrors what the repository projection selects:
// the card fields only, never the body or note the chain fixture carries.
func readerInboxListItemFixture(item model.ReaderInbox) model.ReaderInboxListItem {
	preview := item.Body
	if item.Summary != nil && *item.Summary != "" {
		preview = *item.Summary
	} else if item.Note != "" {
		preview = item.Note
	}
	return model.ReaderInboxListItem{
		ID:               item.ID,
		URL:              item.URL,
		SourceKind:       item.SourceKind,
		Title:            item.Title,
		Preview:          preview,
		Tags:             append([]string(nil), item.Tags...),
		Status:           item.Status,
		MetadataRevision: item.MetadataRevision,
		Expired:          item.Expired,
		UpdatedAt:        item.UpdatedAt,
	}
}

func (s *readerVNextChainStore) GetInbox(_ context.Context, id uuid.UUID) (*model.ReaderInbox, error) {
	if id != s.inbox.ID {
		return nil, repository.ErrNotFound
	}
	return cloneReaderInbox(s.inbox), nil
}

func (s *readerVNextChainStore) ConfirmInbox(_ context.Context, id uuid.UUID, _ *int64) (uuid.UUID, error) {
	if id != s.inbox.ID {
		return uuid.Nil, repository.ErrNotFound
	}
	s.inbox.Status = "confirmed"
	s.inbox.UpdatedAt = s.now
	return s.linkID, nil
}

func (s *readerVNextChainStore) BulkConfirmInbox(_ context.Context, confirmations []model.ReaderInboxBulkConfirmation) ([]model.ReaderInboxBulkResult, error) {
	s.bulkConfirmations = append([]model.ReaderInboxBulkConfirmation(nil), confirmations...)
	results := make([]model.ReaderInboxBulkResult, 0, len(confirmations))
	for _, confirmation := range confirmations {
		linkID := s.linkID
		results = append(results, model.ReaderInboxBulkResult{ID: confirmation.ID, Status: "confirmed", LinkID: &linkID})
	}
	return results, nil
}

func (s *readerVNextChainStore) AppendThoughtOps(_ context.Context, ops []model.ReaderThoughtOp) ([]model.ReaderThoughtAck, error) {
	acks := make([]model.ReaderThoughtAck, 0, len(ops))
	for _, op := range ops {
		s.thoughtOps = append(s.thoughtOps, op)
		var payload struct {
			Body   string          `json:"body"`
			Quote  json.RawMessage `json:"quote"`
			Source string          `json:"source"`
		}
		if err := json.Unmarshal(op.Payload, &payload); err != nil {
			return nil, err
		}
		sequence := int64(len(s.thoughtOps) + 1)
		if payload.Source == "" {
			payload.Source = "user"
		}
		winnerKey := model.ReaderThoughtVersionKey{
			LogicalClock: op.LogicalClock,
			DeviceID:     op.DeviceID,
			OpID:         op.OpID,
		}
		s.thoughts[op.AnnotationID] = model.ReaderThought{
			ID: op.AnnotationID, HostKind: op.HostKind, HostID: op.HostID, LinkID: &s.linkID,
			Target: op.Target, Quote: payload.Quote, Body: payload.Body, Source: payload.Source,
			LastSequence: sequence, WinnerKey: winnerKey, CreatedAt: s.now, UpdatedAt: s.now, LifecycleStatus: "active",
		}
		acks = append(acks, model.ReaderThoughtAck{
			OpID: op.OpID, Sequence: sequence, Disposition: "applied",
			SubmittedKey: winnerKey, WinnerKey: winnerKey,
		})
	}
	return acks, nil
}

func (s *readerVNextChainStore) ListThoughtsSince(context.Context, string, int) ([]model.ReaderThought, string, error) {
	items := make([]model.ReaderThought, 0, len(s.thoughts))
	for _, thought := range s.thoughts {
		items = append(items, thought)
	}
	return items, "", nil
}

func (s *readerVNextChainStore) ListThoughts(context.Context, string, string, int) ([]model.ReaderThought, string, error) {
	return s.ListThoughtsSince(context.Background(), "", 200)
}

func (s *readerVNextChainStore) SearchThoughts(context.Context, string, string, int) ([]model.ReaderThoughtSearch, int, string, error) {
	items := make([]model.ReaderThoughtSearch, 0, len(s.thoughts))
	for _, thought := range s.thoughts {
		items = append(items, model.ReaderThoughtSearch{ID: thought.ID, HostKind: thought.HostKind, HostID: thought.HostID, LinkID: thought.LinkID, Snippet: thought.Body, UpdatedAt: thought.UpdatedAt})
	}
	return items, len(items), "", nil
}

func (s *readerVNextChainStore) CreateNote(_ context.Context, note model.ReaderNote) (*model.ReaderNote, error) {
	s.note = note
	s.note.ID = uuid.MustParse("00000000-0000-0000-0000-000000000003")
	s.note.PublishedRevision = 1
	s.note.DraftRevision = 1
	s.note.CreatedAt, s.note.UpdatedAt = s.now, s.now
	return cloneReaderNote(s.note), nil
}

func (s *readerVNextChainStore) SaveNoteDraft(_ context.Context, command model.ReaderNoteDraftCommand) (*model.ReaderNote, error) {
	if command.NoteID != s.note.ID {
		return nil, repository.ErrNotFound
	}
	s.note.DraftContent = stringPointer(command.Content)
	s.note.DraftRevision++
	s.note.DraftUpdatedAt = &s.now
	s.note.UpdatedAt = s.now
	return cloneReaderNote(s.note), nil
}

func (s *readerVNextChainStore) PublishNote(_ context.Context, command model.ReaderNotePublishCommand) (*model.ReaderNote, error) {
	if command.NoteID != s.note.ID {
		return nil, repository.ErrNotFound
	}
	if s.note.DraftContent != nil {
		s.note.PublishedContent = *s.note.DraftContent
	}
	s.note.PublishedRevision++
	s.note.DraftContent = nil
	s.note.UpdatedAt = s.now
	// The real repository replaces the note's projections inside the publish
	// transaction. The double does the same so the chain exercises a Todos read
	// that only pages stored rows, which is what the service now does.
	hostKind, hostID := "note", s.note.ID.String()
	s.todos = s.todos[:0]
	for _, block := range readertext.List(s.note.PublishedContent) {
		originRef, err := json.Marshal(map[string]any{
			"block_ref": block.BlockRef, "text": block.Text, "occurrence": block.Occurrence,
			"source_kind": "note", "source_id": hostID,
		})
		if err != nil {
			return nil, err
		}
		s.todos = append(s.todos, model.ReaderTodo{
			ID:             uuid.MustParse("00000000-0000-0000-0000-000000000005"),
			Text:           block.Text,
			Done:           block.Done,
			OriginKind:     "note",
			OriginHostKind: &hostKind,
			OriginHostID:   &hostID,
			OriginRef:      originRef,
			HostRevision:   s.note.PublishedRevision,
			CreatedAt:      s.now,
			UpdatedAt:      s.now,
		})
	}
	return cloneReaderNote(s.note), nil
}

func (s *readerVNextChainStore) ListNotes(context.Context, string, int) ([]model.ReaderNote, int, string, error) {
	if s.note.ID == uuid.Nil {
		return []model.ReaderNote{}, 0, "", nil
	}
	return []model.ReaderNote{*cloneReaderNote(s.note)}, 1, "", nil
}

func (s *readerVNextChainStore) SearchPublishedNotes(context.Context, string, int) ([]model.ReaderNoteSearch, int, error) {
	if s.note.ID == uuid.Nil || s.note.PublishedContent == "" {
		return []model.ReaderNoteSearch{}, 0, nil
	}
	return []model.ReaderNoteSearch{{ID: s.note.ID, Title: s.note.Title, Snippet: s.note.PublishedContent, PublishedRevision: s.note.PublishedRevision, UpdatedAt: s.note.UpdatedAt}}, 1, nil
}

func (s *readerVNextChainStore) ListTodos(context.Context, string, int) (model.ReaderTodoPage, error) {
	return model.ReaderTodoPage{Items: append([]model.ReaderTodo(nil), s.todos...)}, nil
}

func (s *readerVNextChainStore) PatchTodo(_ context.Context, command model.ReaderTodoPatch) (*model.ReaderTodo, error) {
	for index := range s.todos {
		if s.todos[index].ID != command.ID {
			continue
		}
		if command.Done != nil {
			s.todos[index].Done = *command.Done
			if *command.Done {
				s.todos[index].CompletedAt = &s.now
			} else {
				s.todos[index].CompletedAt = nil
			}
		}
		s.todos[index].UpdatedAt = s.now
		return &s.todos[index], nil
	}
	return nil, repository.ErrNotFound
}

func (s *readerVNextChainStore) ListFeedWithSources(context.Context, string, string, []string, int) (*model.ReaderFeedPage, error) {
	return s.feed, nil
}

func (s *readerVNextChainStore) FeedbackFeed(_ context.Context, itemKey, action string) (model.ReaderFeedFeedback, error) {
	s.feedbackKey, s.feedback = itemKey, action
	return model.ReaderFeedFeedback{ItemKey: itemKey, Action: action}, nil
}

func (s *readerVNextChainStore) PatchEngagement(_ context.Context, patch model.ReaderEngagementPatch) (*model.ReaderEngagement, error) {
	if patch.Read != nil {
		s.engagement.Read = *patch.Read
	}
	if patch.ReadLater != nil {
		s.engagement.ReadLater = *patch.ReadLater
	}
	if patch.Progress != nil {
		s.engagement.Progress = *patch.Progress
	}
	s.engagement.UpdatedAt = s.now
	return &s.engagement, nil
}

func (s *readerVNextChainStore) LoadHomeAggregate(context.Context) (repository.ReaderHomeAggregate, error) {
	return repository.ReaderHomeAggregate{
		Freshness:       repository.ReaderHomeFreshnessFresh,
		Counts:          map[string]int{"pending": boolCount(s.inbox.Status == "pending"), "todos": len(s.todos), "reading": 1, "notes": 1},
		ContinueReading: s.feed.Items,
		RecentThoughts:  mapReaderThoughts(s.thoughts),
		Todos:           append([]model.ReaderTodo(nil), s.todos...),
	}, nil
}

func cloneReaderInbox(item model.ReaderInbox) *model.ReaderInbox {
	copy := item
	copy.Tags = append([]string(nil), item.Tags...)
	copy.SuggestedTags = append([]string(nil), item.SuggestedTags...)
	return &copy
}

func cloneReaderNote(note model.ReaderNote) *model.ReaderNote {
	copy := note
	if note.DraftContent != nil {
		copy.DraftContent = stringPointer(*note.DraftContent)
	}
	return &copy
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func mapReaderThoughts(thoughts map[string]model.ReaderThought) []model.ReaderThought {
	items := make([]model.ReaderThought, 0, len(thoughts))
	for _, thought := range thoughts {
		items = append(items, thought)
	}
	return items
}

func TestReaderVNextServiceCrossSurfaceChain(t *testing.T) {
	store := newReaderVNextChainStore()
	service := NewReaderVNextService(store, nil, ReaderVNextServiceOptions{InboxProposalCommands: store})
	service.now = func() time.Time { return store.now }
	ctx := context.Background()

	inbox, err := service.CreateInbox(ctx, dto.ReaderInboxCreateRequest{
		URL: "https://capture.example.test/article", SourceKind: "browser_capture", Title: stringPointer("Captured article"), Body: "Captured body from the browser extension.",
	})
	if err != nil {
		t.Fatalf("CreateInbox() error = %v", err)
	}
	if inbox.Status != "pending" {
		t.Fatalf("CreateInbox() status = %q, want pending", inbox.Status)
	}
	pending, err := service.ListInbox(ctx, "", "", 30)
	if err != nil || len(pending.Items) != 1 || pending.Items[0].ID != inbox.ID {
		t.Fatalf("ListInbox() = %#v, error = %v", pending, err)
	}
	if pending.ActiveCount != 1 || pending.ExpiredCount != 0 {
		t.Fatalf("ListInbox() partition counts = active %d expired %d, want 1/0", pending.ActiveCount, pending.ExpiredCount)
	}
	confirmed, err := service.ConfirmInbox(ctx, inbox.ID, inbox.MetadataRevision)
	if err != nil {
		t.Fatalf("ConfirmInbox() error = %v", err)
	}
	if confirmed["target_kind"] != "link" || confirmed["link_id"] != store.linkID.String() || store.inbox.Status != "confirmed" {
		t.Fatalf("ConfirmInbox() = %#v, inbox status = %q", confirmed, store.inbox.Status)
	}

	thoughtID := "thought-1"
	thoughtRequest := dto.ReaderThoughtOpsRequest{Ops: []dto.ReaderThoughtOpRequest{{
		ContractVersion: 1, OpID: "thought-op-1", DeviceID: "reader-device-1", LogicalClock: 2,
		OperationKind: "add", AnnotationID: thoughtID,
		HostKind: "link", HostID: store.linkID.String(),
		Target:  json.RawMessage(`{"kind":"saved-content","host_id":"` + store.linkID.String() + `","version":{"content_revision":3}}`),
		Payload: json.RawMessage(`{"body":"A synced idea becomes searchable.","quote":{"exact":"Captured body"}}`),
	}}}
	acks, err := service.PushThoughtOps(ctx, thoughtRequest)
	if err != nil || len(acks) != 1 || acks[0].Sequence != 2 ||
		acks[0].ContractVersion != 1 || acks[0].Disposition != "applied" ||
		acks[0].CurrentWinnerKey.LogicalClock != 2 {
		t.Fatalf("PushThoughtOps() = %#v, error = %v", acks, err)
	}
	synced, err := service.SyncThoughts(ctx, "0", 100)
	if err != nil || len(synced.Items) != 1 || synced.Items[0].ID != thoughtID {
		t.Fatalf("SyncThoughts() = %#v, error = %v", synced, err)
	}

	search, err := NewLibrarySearchService(&librarySearchLinksFake{}, &librarySearchSitesFake{}, store, LibrarySearchServiceOptions{}).Search(ctx, "searchable", 10, 10, 20, "")
	if err != nil || search.Thoughts == nil || len(search.Thoughts.Items) != 1 || search.Thoughts.Items[0].ID != thoughtID {
		t.Fatalf("Search() thought projection = %#v, error = %v", search.Thoughts, err)
	}

	note, err := service.CreateNote(ctx, dto.ReaderNoteCreateRequest{Title: "Captured idea note"})
	if err != nil {
		t.Fatalf("CreateNote() error = %v", err)
	}
	draft, err := service.SaveNoteDraft(ctx, note.ID, dto.ReaderNoteDraftRequest{Content: "# Follow up\n\n- [ ] Re-read the captured article", ExpectedDraftRevision: note.DraftRevision})
	if err != nil || !draft.Dirty || draft.DraftContent == nil {
		t.Fatalf("SaveNoteDraft() = %#v, error = %v", draft, err)
	}
	expectedDraftRevision, expectedPublishedRevision := draft.DraftRevision, note.PublishedRevision
	published, err := service.PublishNote(ctx, note.ID, dto.ReaderNotePublishRequest{
		ExpectedDraftRevision: &expectedDraftRevision, ExpectedPublishedRevision: &expectedPublishedRevision,
		ReanchorOps: []json.RawMessage{json.RawMessage(`{"thought_id":"` + thoughtID + `","status":"reanchored"}`)},
	})
	if err != nil || published.Dirty || published.DraftContent != nil || published.PublishedContent == "" {
		t.Fatalf("PublishNote() = %#v, error = %v", published, err)
	}

	todos, err := service.ListTodos(ctx, "", 200)
	if err != nil || len(todos.Items) != 1 || todos.Items[0].OriginKind != "note" || todos.Items[0].OriginHostID == nil || *todos.Items[0].OriginHostID != note.ID {
		t.Fatalf("ListTodos() projection = %#v, error = %v", todos, err)
	}
	done := true
	completed, err := service.PatchTodo(ctx, todos.Items[0].ID, dto.ReaderTodoPatchRequest{Done: &done})
	if err != nil || !completed.Done || completed.CompletedAt == nil {
		t.Fatalf("PatchTodo() = %#v, error = %v", completed, err)
	}

	feed, err := service.FeedWithSources(ctx, "recommended", "", nil, 30)
	if err != nil || len(feed.Items) != 1 || feed.Items[0].Key != "link:"+store.linkID.String() {
		t.Fatalf("Feed() = %#v, error = %v", feed, err)
	}
	if _, err := service.FeedbackFeed(ctx, feed.Items[0].Key, "hide"); err != nil || store.feedbackKey != feed.Items[0].Key || store.feedback != "hide" {
		t.Fatalf("FeedbackFeed() = (%q, %q), error = %v", store.feedbackKey, store.feedback, err)
	}
	read := true
	engagement, err := service.PatchEngagement(ctx, store.linkID.String(), dto.ReaderEngagementRequest{Read: &read})
	if err != nil || !engagement.Read || engagement.LinkID != store.linkID.String() {
		t.Fatalf("PatchEngagement() = %#v, error = %v", engagement, err)
	}

	home, err := service.Home(ctx)
	if err != nil || len(home.RecentThoughts) != 1 || len(home.Todos) != 1 || len(home.ContinueReading) != 1 || home.ContinueReading[0].Key != feed.Items[0].Key {
		t.Fatalf("Home() = %#v, error = %v", home, err)
	}
	if home.Todos[0].ID != completed.ID || home.Todos[0].Done != completed.Done {
		t.Fatalf("Home() TODO identity = %#v, want completed %s", home.Todos[0], completed.ID)
	}
}

func TestReaderVNextBulkConfirmCarriesEveryExpectedRevision(t *testing.T) {
	store := newReaderVNextChainStore()
	service := NewReaderVNextService(store, nil)
	first := store.inbox.ID
	second := uuid.MustParse("00000000-0000-0000-0000-000000000003")

	_, err := service.ConfirmInboxBulk(context.Background(), []string{first.String(), second.String(), first.String()}, map[string]int64{
		first.String():  4,
		second.String(): 9,
	})
	if err != nil {
		t.Fatalf("ConfirmInboxBulk() error = %v", err)
	}
	if len(store.bulkConfirmations) != 2 {
		t.Fatalf("confirmations = %#v, want two de-duplicated entries", store.bulkConfirmations)
	}
	if store.bulkConfirmations[0].ExpectedRevision == nil || *store.bulkConfirmations[0].ExpectedRevision != 4 {
		t.Fatalf("first expected revision = %#v, want 4", store.bulkConfirmations[0].ExpectedRevision)
	}
	if store.bulkConfirmations[1].ExpectedRevision == nil || *store.bulkConfirmations[1].ExpectedRevision != 9 {
		t.Fatalf("second expected revision = %#v, want 9", store.bulkConfirmations[1].ExpectedRevision)
	}
}

func TestReaderVNextBulkConfirmRejectsPartialExpectedRevisions(t *testing.T) {
	store := newReaderVNextChainStore()
	service := NewReaderVNextService(store, nil)
	first := store.inbox.ID
	second := uuid.MustParse("00000000-0000-0000-0000-000000000003")

	_, err := service.ConfirmInboxBulk(context.Background(), []string{first.String(), second.String()}, map[string]int64{first.String(): 4})
	assertReaderHTTPError(t, err, http.StatusUnprocessableEntity, "invalid_inbox_batch_revision")
	if store.bulkConfirmations != nil {
		t.Fatalf("store called for partial expected revisions: %#v", store.bulkConfirmations)
	}
}
