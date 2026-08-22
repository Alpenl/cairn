package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
)

type readerVNextChainHandler struct {
	readerHandlerStub

	inbox            dto.ReaderInboxResponse
	inboxCalls       int
	confirmCalls     int
	thought          dto.ReaderThoughtResponse
	thoughtCalls     int
	note             dto.ReaderNoteResponse
	noteDraftCalls   int
	notePublishCalls int
	todo             dto.ReaderTodoResponse
	feed             dto.ReaderFeedResponse
	feedbackKey      string
	feedbackAction   string
	engagement       dto.ReaderEngagementResponse
	home             dto.ReaderHomeResponse
}

func newReaderVNextChainHandler() *readerVNextChainHandler {
	now := time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC)
	linkID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	inboxID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	noteID := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	todoID := uuid.MustParse("00000000-0000-0000-0000-000000000004")
	linkText, inboxText, noteText := linkID.String(), inboxID.String(), noteID.String()
	return &readerVNextChainHandler{
		inbox: dto.ReaderInboxResponse{
			ID: inboxText, URL: "https://capture.example.test/article", SourceKind: "browser_capture",
			Body: "Captured body", SuggestedTags: []string{"capture"}, Tags: []string{"capture"},
			Status: "pending", MetadataRevision: 1, CreatedAt: now, UpdatedAt: now,
		},
		thought: dto.ReaderThoughtResponse{
			ID: "thought-1", HostKind: "link", HostID: linkText, LinkID: &linkText,
			Target: json.RawMessage(`{"kind":"saved-content","host_id":"` + linkText + `","version":{"content_revision":3}}`),
			Body:   "A synced idea becomes searchable.", Source: "user", LastSequence: 2, CreatedAt: now, UpdatedAt: now,
		},
		note: dto.ReaderNoteResponse{
			ID: noteText, Title: "Captured idea note", PublishedContent: "", PublishedRevision: 1,
			DraftRevision: 1, CreatedAt: now, UpdatedAt: now,
		},
		todo: dto.ReaderTodoResponse{
			ID: todoID.String(), Text: "Follow up on the captured idea", OriginKind: "note",
			OriginHostKind: stringPointerForHandler("note"), OriginHostID: &noteText,
			OriginRef: json.RawMessage(`{"block_ref":"task:follow-up"}`), HostRevision: 2,
			CreatedAt: now, UpdatedAt: now,
		},
		feed: dto.ReaderFeedResponse{
			Items: []dto.ReaderFeedItemResponse{{
				Key: "link:" + linkText, Source: "reading", ResourceKey: "link:" + linkText,
				Title: "Captured article", Summary: "Captured summary",
				URL: "https://capture.example.test/article", LinkID: &linkText, Read: false, ReadLater: false,
				EventAt: now,
			}},
			Mode: "recommended",
		},
		engagement: dto.ReaderEngagementResponse{LinkID: linkText, Progress: 0, UpdatedAt: now},
		home: dto.ReaderHomeResponse{
			Today: "2026-08-10", Summary: "继续整理捕获内容", Counts: map[string]int{"pending": 0, "reading": 1, "notes": 1, "todos": 1},
			ContinueReading: []dto.ReaderFeedItemResponse{{Key: "link:" + linkText, Source: "reading", ResourceKey: "link:" + linkText, Title: "Captured article", Summary: "Captured summary", URL: "https://capture.example.test/article", LinkID: &linkText, EventAt: now}},
			RecentThoughts:  []dto.ReaderThoughtResponse{}, Todos: []dto.ReaderTodoResponse{}, Freshness: string(model.ReaderHomeFreshnessFresh),
		},
	}
}

func stringPointerForHandler(value string) *string {
	return &value
}

func (s *readerVNextChainHandler) CreateInbox(_ context.Context, request dto.ReaderInboxCreateRequest) (dto.ReaderInboxResponse, error) {
	s.inboxCalls++
	s.inbox.URL, s.inbox.SourceKind, s.inbox.Body, s.inbox.Tags = request.URL, request.SourceKind, request.Body, request.Tags
	s.inbox.SuggestedTags = append([]string(nil), request.Tags...)
	return s.inbox, nil
}

func (s *readerVNextChainHandler) ListInbox(_ context.Context, partition, _ string, _ int) (dto.ReaderInboxResponsePage, error) {
	if s.inbox.Status != "pending" {
		return dto.ReaderInboxResponsePage{Items: []dto.ReaderInboxListItemResponse{}, ActiveCount: 0, ExpiredCount: 0}, nil
	}
	if partition == "expired" {
		return dto.ReaderInboxResponsePage{Items: []dto.ReaderInboxListItemResponse{}, ActiveCount: 1, ExpiredCount: 0}, nil
	}
	return dto.ReaderInboxResponsePage{
		Items: []dto.ReaderInboxListItemResponse{{
			ID:               s.inbox.ID,
			URL:              s.inbox.URL,
			SourceKind:       s.inbox.SourceKind,
			Title:            s.inbox.Title,
			Preview:          s.inbox.Body,
			Tags:             s.inbox.Tags,
			Status:           s.inbox.Status,
			MetadataRevision: s.inbox.MetadataRevision,
			Expired:          s.inbox.Expired,
			UpdatedAt:        s.inbox.UpdatedAt,
		}},
		ActiveCount:  1,
		ExpiredCount: 0,
	}, nil
}

func (s *readerVNextChainHandler) ConfirmInbox(context.Context, string, int64) (map[string]string, error) {
	s.confirmCalls++
	s.inbox.Status = "confirmed"
	return map[string]string{"target_kind": "link", "link_id": *s.thought.LinkID, "status": "confirmed"}, nil
}

func (s *readerVNextChainHandler) PushThoughtOps(_ context.Context, request dto.ReaderThoughtOpsRequest) ([]dto.ReaderThoughtAckResponse, error) {
	s.thoughtCalls += len(request.Ops)
	return []dto.ReaderThoughtAckResponse{{OpID: request.Ops[0].OpID, Sequence: s.thought.LastSequence}}, nil
}

func (s *readerVNextChainHandler) SyncThoughts(context.Context, string, int) (dto.ReaderThoughtsResponse, error) {
	return dto.ReaderThoughtsResponse{Items: []dto.ReaderThoughtResponse{s.thought}}, nil
}

func (s *readerVNextChainHandler) CreateNote(_ context.Context, request dto.ReaderNoteCreateRequest) (dto.ReaderNoteResponse, error) {
	s.note.Title = request.Title
	return s.note, nil
}

func (s *readerVNextChainHandler) SaveNoteDraft(_ context.Context, _ string, request dto.ReaderNoteDraftRequest) (dto.ReaderNoteResponse, error) {
	s.noteDraftCalls++
	s.note.DraftContent = &request.Content
	s.note.DraftRevision = request.ExpectedDraftRevision + 1
	s.note.Dirty = true
	return s.note, nil
}

func (s *readerVNextChainHandler) PublishNote(_ context.Context, _ string, request dto.ReaderNotePublishRequest) (dto.ReaderNoteResponse, error) {
	s.notePublishCalls++
	s.note.PublishedContent = *s.note.DraftContent
	s.note.PublishedRevision = *request.ExpectedPublishedRevision + 1
	s.note.DraftContent = nil
	s.note.Dirty = false
	return s.note, nil
}

func (s *readerVNextChainHandler) ListTodos(context.Context, string, int) (dto.ReaderTodosResponse, error) {
	if s.note.PublishedContent != "" {
		s.home.Todos = []dto.ReaderTodoResponse{s.todo}
	}
	return dto.ReaderTodosResponse{Items: s.home.Todos}, nil
}

func (s *readerVNextChainHandler) PatchTodo(_ context.Context, _ string, request dto.ReaderTodoPatchRequest) (dto.ReaderTodoResponse, error) {
	if request.Done != nil {
		s.todo.Done = *request.Done
		completedAt := s.todo.UpdatedAt.Add(time.Minute)
		s.todo.CompletedAt = &completedAt
	}
	return s.todo, nil
}

func (s *readerVNextChainHandler) FeedWithSources(context.Context, string, string, []string, int) (dto.ReaderFeedResponse, error) {
	return s.feed, nil
}

func (s *readerVNextChainHandler) FeedbackFeed(_ context.Context, itemKey, action string) (dto.ReaderFeedFeedbackResponse, error) {
	s.feedbackKey, s.feedbackAction = itemKey, action
	return dto.ReaderFeedFeedbackResponse{ItemKey: itemKey, Action: action}, nil
}

func (s *readerVNextChainHandler) PatchEngagement(_ context.Context, _ string, request dto.ReaderEngagementRequest) (dto.ReaderEngagementResponse, error) {
	if request.Read != nil {
		s.engagement.Read = *request.Read
	}
	return s.engagement, nil
}

func (s *readerVNextChainHandler) Home(context.Context) (dto.ReaderHomeResponse, error) {
	s.home.RecentThoughts = []dto.ReaderThoughtResponse{s.thought}
	return s.home, nil
}

func serveReaderChainRequest(t *testing.T, router http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func decodeReaderChainJSON(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, response.Body.String())
	}
}

func TestRegisterReaderRoutesCrossSurfaceChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := newReaderVNextChainHandler()
	stub.feedResponse = stub.feed
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))
	inboxID := stub.inbox.ID
	noteID := stub.note.ID
	linkID := *stub.thought.LinkID

	response := serveReaderChainRequest(t, router, http.MethodPost, "/api/inbox", []byte(`{"url":"https://capture.example.test/article","source_kind":"browser_capture","body":"Captured body","tags":["capture"]}`))
	if response.Code != http.StatusCreated || stub.inboxCalls != 1 {
		t.Fatalf("create inbox status=%d calls=%d body=%s", response.Code, stub.inboxCalls, response.Body.String())
	}
	var inbox dto.ReaderInboxResponse
	decodeReaderChainJSON(t, response, &inbox)
	if inbox.ID != inboxID || inbox.Status != "pending" {
		t.Fatalf("created inbox = %#v", inbox)
	}

	response = serveReaderChainRequest(t, router, http.MethodGet, "/api/inbox", nil)
	var pending dto.ReaderInboxResponsePage
	decodeReaderChainJSON(t, response, &pending)
	if response.Code != http.StatusOK || len(pending.Items) != 1 || pending.Items[0].ID != inboxID {
		t.Fatalf("pending inbox status=%d page=%#v", response.Code, pending)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/inbox/"+inboxID+"/confirm", nil)
	request.Header.Set("If-Match", `"1"`)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var confirmation map[string]string
	decodeReaderChainJSON(t, response, &confirmation)
	if response.Code != http.StatusOK || confirmation["link_id"] != linkID || stub.confirmCalls != 1 || stub.inbox.Status != "confirmed" {
		t.Fatalf("confirmation status=%d response=%#v calls=%d inbox=%q", response.Code, confirmation, stub.confirmCalls, stub.inbox.Status)
	}

	thoughtBody := []byte(`{"ops":[{"contract_version":1,"op_id":"thought-op-1","device_id":"device-1","logical_clock":1,"operation_kind":"add","annotation_id":"thought-1","host_kind":"link","host_id":"` + linkID + `","target":{"kind":"saved-content","host_id":"` + linkID + `","version":{"content_revision":3}},"payload":{"body":"A synced idea becomes searchable.","quote":{"exact":"Captured body"}}}]}`)
	response = serveReaderChainRequest(t, router, http.MethodPost, "/api/annotations/ops", thoughtBody)
	if response.Code != http.StatusOK || stub.thoughtCalls != 1 {
		t.Fatalf("thought push status=%d calls=%d body=%s", response.Code, stub.thoughtCalls, response.Body.String())
	}
	response = serveReaderChainRequest(t, router, http.MethodGet, "/api/annotations/sync?after=0&limit=100", nil)
	var synced dto.ReaderThoughtsResponse
	decodeReaderChainJSON(t, response, &synced)
	if response.Code != http.StatusOK || len(synced.Items) != 1 || synced.Items[0].ID != stub.thought.ID {
		t.Fatalf("thought sync status=%d response=%#v", response.Code, synced)
	}

	response = serveReaderChainRequest(t, router, http.MethodPost, "/api/notes", []byte(`{"title":"Captured idea note"}`))
	var createdNote dto.ReaderNoteResponse
	decodeReaderChainJSON(t, response, &createdNote)
	if response.Code != http.StatusCreated || createdNote.ID != noteID {
		t.Fatalf("note create status=%d note=%#v", response.Code, createdNote)
	}
	draftBody := []byte(`{"content":"# Follow up\n\n- [ ] Re-read the captured article","expected_draft_revision":1}`)
	response = serveReaderChainRequest(t, router, http.MethodPatch, "/api/notes/"+noteID+"/draft", draftBody)
	var draft dto.ReaderNoteResponse
	decodeReaderChainJSON(t, response, &draft)
	if response.Code != http.StatusOK || !draft.Dirty || draft.DraftContent == nil || stub.noteDraftCalls != 1 {
		t.Fatalf("note draft status=%d note=%#v calls=%d", response.Code, draft, stub.noteDraftCalls)
	}
	publishBody := []byte(`{"expected_draft_revision":2,"expected_published_revision":1,"reanchor_ops":[{"thought_id":"thought-1","status":"reanchored"}]}`)
	response = serveReaderChainRequest(t, router, http.MethodPost, "/api/notes/"+noteID+"/publish", publishBody)
	var published dto.ReaderNoteResponse
	decodeReaderChainJSON(t, response, &published)
	if response.Code != http.StatusOK || published.Dirty || published.DraftContent != nil || published.PublishedRevision != 2 || stub.notePublishCalls != 1 {
		t.Fatalf("note publish status=%d note=%#v calls=%d", response.Code, published, stub.notePublishCalls)
	}

	response = serveReaderChainRequest(t, router, http.MethodGet, "/api/todos", nil)
	var todos dto.ReaderTodosResponse
	decodeReaderChainJSON(t, response, &todos)
	if response.Code != http.StatusOK || len(todos.Items) != 1 || todos.Items[0].OriginHostID == nil || *todos.Items[0].OriginHostID != noteID {
		t.Fatalf("todo projection status=%d todos=%#v", response.Code, todos)
	}
	response = serveReaderChainRequest(t, router, http.MethodPatch, "/api/todos/"+stub.todo.ID, []byte(`{"done":true}`))
	var completed dto.ReaderTodoResponse
	decodeReaderChainJSON(t, response, &completed)
	if response.Code != http.StatusOK || !completed.Done || completed.CompletedAt == nil {
		t.Fatalf("todo completion status=%d todo=%#v", response.Code, completed)
	}

	response = serveReaderChainRequest(t, router, http.MethodGet, "/api/reader-feed?mode=recommended", nil)
	var feed dto.ReaderFeedResponse
	decodeReaderChainJSON(t, response, &feed)
	if response.Code != http.StatusOK || len(feed.Items) != 1 || feed.Items[0].Key != "link:"+linkID {
		t.Fatalf("feed status=%d feed=%#v", response.Code, feed)
	}
	response = serveReaderChainRequest(t, router, http.MethodPost, "/api/reader-feed/feedback?item_key="+feed.Items[0].Key, []byte(`{"action":"hide"}`))
	var feedback dto.ReaderFeedFeedbackResponse
	decodeReaderChainJSON(t, response, &feedback)
	if response.Code != http.StatusOK || feedback.ItemKey != feed.Items[0].Key || feedback.Action != "hide" || stub.feedbackKey != feed.Items[0].Key || stub.feedbackAction != "hide" {
		t.Fatalf("feed action status=%d key=%q action=%q", response.Code, stub.feedbackKey, stub.feedbackAction)
	}
	response = serveReaderChainRequest(t, router, http.MethodPatch, "/api/engagement/"+linkID, []byte(`{"read":true}`))
	var engagement dto.ReaderEngagementResponse
	decodeReaderChainJSON(t, response, &engagement)
	if response.Code != http.StatusOK || !engagement.Read || engagement.LinkID != linkID {
		t.Fatalf("engagement status=%d response=%#v", response.Code, engagement)
	}

	response = serveReaderChainRequest(t, router, http.MethodGet, "/api/home", nil)
	var home dto.ReaderHomeResponse
	decodeReaderChainJSON(t, response, &home)
	if response.Code != http.StatusOK || len(home.RecentThoughts) != 1 || len(home.Todos) != 1 || home.ContinueReading[0].Key != feed.Items[0].Key {
		t.Fatalf("home status=%d response=%#v", response.Code, home)
	}
}
