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
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

type readerHandlerStub struct {
	ReaderThoughtRoutes
	ReaderNoteRoutes
	ReaderInboxRoutes
	ReaderTodoRoutes
	ReaderAggregateRoutes
	restoreInbox         func(context.Context, string) error
	restoreInboxID       string
	confirmBulk          func(context.Context, []string, map[string]int64) ([]model.ReaderInboxBulkResult, error)
	discardBulk          func(context.Context, []string) ([]model.ReaderInboxBulkResult, error)
	confirmAI            func(context.Context, string) (dto.ReaderInboxConfirmAIProposalsResponse, error)
	confirmBulkCalled    []string
	confirmBulkRevisions map[string]int64
	discardBulkCalled    []string
	confirmAIPartition   string
	confirmAICalls       int
	feedResponse         dto.ReaderFeedResponse
	feedMode             string
	feedAfter            string
	feedLimit            int
	feedSources          []string
	feedbackItemKey      string
	feedbackAction       string
	feedbackErr          error
	activityResponse     dto.ReaderActivityResponse
	activityKind         string
	activityAfter        string
	activityLimit        int
}

type readerTodoPatchHandlerStub struct {
	ReaderTodoRoutes
	err   error
	calls int
}

func (s *readerTodoPatchHandlerStub) PatchTodo(context.Context, string, dto.ReaderTodoPatchRequest) (dto.ReaderTodoResponse, error) {
	s.calls++
	return dto.ReaderTodoResponse{}, s.err
}

// readerTodoPatchPresenceStore makes the HTTP -> service command observable.
// The explicit-null case can only produce its public 422 if the service keeps
// the DTO's field-presence bit when it builds model.ReaderTodoPatch.
type readerTodoPatchPresenceStore struct {
	service.ReaderTodoStore
	commands []model.ReaderTodoPatch
	err      error
}

func (s *readerTodoPatchPresenceStore) PatchTodo(_ context.Context, command model.ReaderTodoPatch) (*model.ReaderTodo, error) {
	s.commands = append(s.commands, command)
	if s.err != nil {
		return nil, s.err
	}
	if command.ExpectedHostRevisionSet {
		return nil, repository.ErrReaderTodoHostRevisionNotApplicable
	}
	return &model.ReaderTodo{
		ID:         command.ID,
		Text:       "standalone TODO",
		Done:       command.Done != nil && *command.Done,
		OriginKind: "standalone",
	}, nil
}

type readerThoughtConflictHandlerStub struct {
	*readerHandlerStub
	response dto.ReaderThoughtConflictsResponse
}

func (s *readerThoughtConflictHandlerStub) ListThoughtConflicts(context.Context, string, int) (dto.ReaderThoughtConflictsResponse, error) {
	return s.response, nil
}

func (s *readerHandlerStub) RestoreInbox(ctx context.Context, id string) error {
	s.restoreInboxID = id
	if s.restoreInbox != nil {
		return s.restoreInbox(ctx, id)
	}
	return nil
}

func (s *readerHandlerStub) ConfirmInboxBulk(ctx context.Context, ids []string, expectedRevisions map[string]int64) ([]model.ReaderInboxBulkResult, error) {
	s.confirmBulkCalled = append([]string(nil), ids...)
	s.confirmBulkRevisions = expectedRevisions
	if s.confirmBulk != nil {
		return s.confirmBulk(ctx, ids, expectedRevisions)
	}
	return nil, nil
}

func (s *readerHandlerStub) DiscardInboxBulk(ctx context.Context, ids []string) ([]model.ReaderInboxBulkResult, error) {
	s.discardBulkCalled = append([]string(nil), ids...)
	if s.discardBulk != nil {
		return s.discardBulk(ctx, ids)
	}
	return nil, nil
}

func (s *readerHandlerStub) ConfirmAIProposals(ctx context.Context, partition string) (dto.ReaderInboxConfirmAIProposalsResponse, error) {
	s.confirmAICalls++
	s.confirmAIPartition = partition
	if s.confirmAI != nil {
		return s.confirmAI(ctx, partition)
	}
	return dto.ReaderInboxConfirmAIProposalsResponse{Atomic: true, Items: []dto.ReaderInboxBulkItemResponse{}}, nil
}

func (s *readerHandlerStub) FeedWithSources(_ context.Context, mode, after string, sources []string, limit int) (dto.ReaderFeedResponse, error) {
	s.feedMode = mode
	s.feedAfter = after
	s.feedLimit = limit
	s.feedSources = append([]string(nil), sources...)
	return s.feedResponse, nil
}

func (s *readerHandlerStub) FeedbackFeed(_ context.Context, itemKey, action string) (dto.ReaderFeedFeedbackResponse, error) {
	s.feedbackItemKey = itemKey
	s.feedbackAction = action
	response := dto.ReaderFeedFeedbackResponse{ItemKey: itemKey, Action: action}
	if action == "save" {
		linkID := uuid.NewString()
		response.LinkID = &linkID
	}
	return response, s.feedbackErr
}

func (s *readerHandlerStub) Activity(_ context.Context, kind, after string, limit int) (dto.ReaderActivityResponse, error) {
	s.activityKind = kind
	s.activityAfter = after
	s.activityLimit = limit
	return s.activityResponse, nil
}

type readerActivityHandlerStore struct {
	service.ReaderLibraryStore
	items []model.ReaderActivity
}

func (s *readerActivityHandlerStore) ListActivity(_ context.Context, query model.ReaderActivityQuery) (model.ReaderActivityPage, error) {
	start := 0
	if query.After != nil {
		start = 1
	}
	end := start + query.Limit
	if end > len(s.items) {
		end = len(s.items)
	}
	return model.ReaderActivityPage{
		Items:   append([]model.ReaderActivity(nil), s.items[start:end]...),
		HasMore: end < len(s.items),
	}, nil
}

func TestReaderActivityPassesKindCursorAndLimit(t *testing.T) {
	stub := &readerHandlerStub{activityResponse: dto.ReaderActivityResponse{
		Kind: "tag", Tags: []dto.ReaderTagActivityResponse{}, Domains: []dto.ReaderDomainActivityResponse{},
	}}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/reader/activity?kind=tag&after=opaque-cursor&limit=37", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("activity status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if stub.activityKind != "tag" || stub.activityAfter != "opaque-cursor" || stub.activityLimit != 37 {
		t.Fatalf("activity query = kind %q after %q limit %d", stub.activityKind, stub.activityAfter, stub.activityLimit)
	}
}

func TestReaderActivityHandlerRejectsTamperedAndCrossBoundCursor(t *testing.T) {
	when := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	store := &readerActivityHandlerStore{items: []model.ReaderActivity{
		{Kind: "tag", Key: "alpha", NormalizedKey: "alpha", LastAt: when},
		{Kind: "tag", Key: "beta", NormalizedKey: "beta", LastAt: when},
	}}
	reader := service.NewReaderVNextService(readerServiceTestStores(store), nil, service.ReaderVNextServiceOptions{CursorSigningKey: "handler-activity-key"})
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(reader))

	firstResponse := httptest.NewRecorder()
	router.ServeHTTP(firstResponse, httptest.NewRequest(http.MethodGet, "/api/reader/activity?kind=tag&limit=1", nil))
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first activity status = %d; body=%s", firstResponse.Code, firstResponse.Body.String())
	}
	var first dto.ReaderActivityResponse
	if err := json.NewDecoder(firstResponse.Body).Decode(&first); err != nil || first.NextCursor == "" {
		t.Fatalf("decode first activity = %#v, %v", first, err)
	}

	requests := []struct {
		name string
		path string
	}{
		{name: "query kind", path: "/api/reader/activity?kind=domain&after=" + first.NextCursor},
		{name: "tamper", path: "/api/reader/activity?kind=tag&after=" + first.NextCursor[:len(first.NextCursor)-1] + "x"},
	}
	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			router.ServeHTTP(response, request)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("activity cursor status = %d, want 422; body=%s", response.Code, response.Body.String())
			}
			var body struct {
				Error struct {
					ErrorCode string `json:"error_code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil || body.Error.ErrorCode != httperr.CodeInvalidCursor {
				t.Fatalf("activity cursor error = %#v, %v", body, err)
			}
		})
	}
}

func TestReaderTodoPatchHandlerPreservesErrorContract(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
	}{
		{name: "not found", status: http.StatusNotFound, code: "reader_not_found"},
		{name: "projected revision conflict", status: http.StatusConflict, code: httperr.CodeRevisionConflict},
		{name: "standalone revision not applicable", status: http.StatusUnprocessableEntity, code: "todo_host_revision_not_applicable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &readerTodoPatchHandlerStub{err: httperr.NewWithCode(tt.status, tt.code, "stable TODO error")}
			router := gin.New()
			RegisterReaderRoutes(router, readerTestRoutes(stub))

			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPatch, "/api/todos/00000000-0000-0000-0000-000000000001", bytes.NewBufferString(`{"done":true,"expected_host_revision":0}`))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(response, request)

			if response.Code != tt.status {
				t.Fatalf("PATCH /api/todos/:id status = %d, want %d; body=%s", response.Code, tt.status, response.Body.String())
			}
			var body dto.ErrorResponse
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode error envelope: %v; body=%s", err, response.Body.String())
			}
			if body.Error.Code != tt.status || body.Error.ErrorCode != tt.code || body.Error.Message != "stable TODO error" {
				t.Fatalf("error envelope = %#v, want status/code/message %d/%q/%q", body.Error, tt.status, tt.code, "stable TODO error")
			}
			if stub.calls != 1 {
				t.Fatalf("PatchTodo calls = %d, want 1", stub.calls)
			}
		})
	}
}

func TestReaderTodoPatchHTTPPreservesExpectedHostRevisionPresence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	todoID := "00000000-0000-0000-0000-000000000073"
	negative := int64(-1)
	tests := []struct {
		name            string
		body            string
		wantRevisionSet bool
		wantRevision    *int64
		wantStatus      int
		wantCode        string
		storeErr        error
	}{
		{
			name:            "explicit null reaches model and returns not applicable",
			body:            `{"done":true,"expected_host_revision":null}`,
			wantRevisionSet: true,
			wantStatus:      http.StatusUnprocessableEntity,
			wantCode:        "todo_host_revision_not_applicable",
		},
		{
			name:            "negative revision on an unknown TODO stays not found",
			body:            `{"done":true,"expected_host_revision":-1}`,
			wantRevisionSet: true,
			wantRevision:    &negative,
			wantStatus:      http.StatusNotFound,
			wantCode:        "reader_not_found",
			storeErr:        repository.ErrNotFound,
		},
		{
			name:            "negative integer reaches model and returns not applicable",
			body:            `{"done":true,"expected_host_revision":-1}`,
			wantRevisionSet: true,
			wantRevision:    &negative,
			wantStatus:      http.StatusUnprocessableEntity,
			wantCode:        "todo_host_revision_not_applicable",
		},
		{
			name:            "omission reaches model as absent",
			body:            `{"done":true}`,
			wantRevisionSet: false,
			wantStatus:      http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &readerTodoPatchPresenceStore{err: tt.storeErr}
			router := gin.New()
			reader := service.NewReaderVNextService(readerServiceTestStores(store), nil)
			RegisterReaderRoutes(router, readerTestRoutes(reader))

			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPatch, "/api/todos/"+todoID, bytes.NewBufferString(tt.body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("PATCH /api/todos/:id status = %d, want %d; body=%s", response.Code, tt.wantStatus, response.Body.String())
			}
			if len(store.commands) != 1 {
				t.Fatalf("PatchTodo command count = %d, want 1", len(store.commands))
			}
			command := store.commands[0]
			if command.ID.String() != todoID {
				t.Fatalf("PatchTodo command ID = %q, want %q", command.ID, todoID)
			}
			if command.Done == nil || !*command.Done {
				t.Fatalf("PatchTodo command Done = %#v, want true", command.Done)
			}
			if command.ExpectedHostRevisionSet != tt.wantRevisionSet {
				t.Fatalf("PatchTodo command ExpectedHostRevisionSet = %t, want %t", command.ExpectedHostRevisionSet, tt.wantRevisionSet)
			}
			if tt.wantRevision == nil {
				if command.ExpectedHostRevision != nil {
					t.Fatalf("PatchTodo command ExpectedHostRevision = %d, want nil", *command.ExpectedHostRevision)
				}
			} else if command.ExpectedHostRevision == nil || *command.ExpectedHostRevision != *tt.wantRevision {
				t.Fatalf("PatchTodo command ExpectedHostRevision = %v, want %d", command.ExpectedHostRevision, *tt.wantRevision)
			}
			if tt.wantCode == "" {
				return
			}

			var envelope dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v; body=%s", err, response.Body.String())
			}
			if envelope.Error.Code != tt.wantStatus || envelope.Error.ErrorCode != tt.wantCode {
				t.Fatalf("error envelope = %#v, want status/code %d/%q", envelope.Error, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestRegisterReaderRoutesExposesThoughtConflictRecovery(t *testing.T) {
	reader := &readerThoughtConflictHandlerStub{
		readerHandlerStub: &readerHandlerStub{},
		response: dto.ReaderThoughtConflictsResponse{
			Items: []dto.ReaderThoughtConflictResponse{{
				Sequence:     7,
				AnnotationID: "thought-1",
			}},
		},
	}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(reader))

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/annotations/conflicts?limit=10", nil)
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/annotations/conflicts status = %d, want 200", response.Code)
	}
	var body dto.ReaderThoughtConflictsResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].Sequence != 7 || body.Items[0].AnnotationID != "thought-1" {
		t.Fatalf("conflict response = %#v", body)
	}
}

func TestReaderInboxRestoreReturnsOKAndPassesID(t *testing.T) {
	const inboxID = "00000000-0000-0000-0000-000000000001"
	stub := &readerHandlerStub{}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/inbox/"+inboxID+"/restore", nil)
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if stub.restoreInboxID != inboxID {
		t.Fatalf("restore inbox id = %q, want %q", stub.restoreInboxID, inboxID)
	}
}

func TestReaderInboxRestoreMapsStateConflict(t *testing.T) {
	stub := &readerHandlerStub{
		restoreInbox: func(context.Context, string) error {
			return httperr.NewWithCode(http.StatusConflict, "inbox_state_conflict", "the inbox item is already confirmed")
		},
	}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/inbox/00000000-0000-0000-0000-000000000001/restore", nil)
	router.ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("restore conflict status = %d, want 409; body=%s", response.Code, response.Body.String())
	}
	if response.Body.Len() == 0 {
		t.Fatal("restore conflict should include an error envelope")
	}
}

func TestReaderConfirmAIProposalsUsesStaticCollectionRouteAndMapsResponse(t *testing.T) {
	first, second, linkID := uuid.New(), uuid.New(), uuid.New()
	stub := &readerHandlerStub{confirmAI: func(_ context.Context, partition string) (dto.ReaderInboxConfirmAIProposalsResponse, error) {
		if partition != "expired" {
			t.Fatalf("partition = %q, want expired", partition)
		}
		return dto.ReaderInboxConfirmAIProposalsResponse{
			Atomic: true,
			Items: []dto.ReaderInboxBulkItemResponse{
				{InboxID: first.String(), Status: "confirmed", LinkID: stringPointerForReaderHandler(linkID.String())},
				{InboxID: second.String(), Status: "confirmed"},
			},
			RemainingCount: 3,
		}, nil
	}}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/inbox/confirm-ai-proposals", bytes.NewBufferString(`{"partition":"expired"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK || stub.confirmAICalls != 1 || stub.confirmAIPartition != "expired" {
		t.Fatalf("POST confirm-ai-proposals status/calls/partition = %d/%d/%q; body=%s", response.Code, stub.confirmAICalls, stub.confirmAIPartition, response.Body.String())
	}
	var body dto.ReaderInboxConfirmAIProposalsResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode confirmation response: %v", err)
	}
	if !body.Atomic || body.RemainingCount != 3 || len(body.Items) != 2 || body.Items[0].InboxID != first.String() || body.Items[0].LinkID == nil || *body.Items[0].LinkID != linkID.String() || body.Items[1].InboxID != second.String() {
		t.Fatalf("confirmation response = %#v", body)
	}
}

func TestReaderConfirmAIProposalsRejectsInvalidPartitionBeforeService(t *testing.T) {
	stub := &readerHandlerStub{}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/inbox/confirm-ai-proposals", bytes.NewBufferString(`{"partition":"other"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity || stub.confirmAICalls != 0 {
		t.Fatalf("invalid partition status/calls = %d/%d; body=%s", response.Code, stub.confirmAICalls, response.Body.String())
	}
}

func stringPointerForReaderHandler(value string) *string {
	return &value
}

func TestReaderInboxBulkConfirmPreservesUniqueInputOrder(t *testing.T) {
	first := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	second := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	link := uuid.MustParse("00000000-0000-0000-0000-000000000101")
	stub := &readerHandlerStub{
		confirmBulk: func(_ context.Context, ids []string, expectedRevisions map[string]int64) ([]model.ReaderInboxBulkResult, error) {
			if len(ids) != 3 || ids[0] != first.String() || ids[1] != second.String() || ids[2] != first.String() {
				t.Fatalf("service ids = %#v, want original input order", ids)
			}
			if expectedRevisions[first.String()] != 3 || expectedRevisions[second.String()] != 7 {
				t.Fatalf("service expected revisions = %#v", expectedRevisions)
			}
			return []model.ReaderInboxBulkResult{
				{ID: first, Status: "confirmed", LinkID: &link},
				{ID: second, Status: "confirmed"},
			}, nil
		},
	}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	body := []byte(`{"inbox_ids":["` + first.String() + `","` + second.String() + `","` + first.String() + `"],"expected_revisions":{"` + first.String() + `":3,"` + second.String() + `":7}}`)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/inbox/bulk/confirm", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("bulk confirm status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var decoded dto.ReaderInboxBulkResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode bulk confirm response: %v", err)
	}
	if !decoded.Atomic || len(decoded.Items) != 2 {
		t.Fatalf("bulk confirm response = %#v, want atomic two-item response", decoded)
	}
	if decoded.Items[0].InboxID != first.String() || decoded.Items[1].InboxID != second.String() {
		t.Fatalf("response item order = %#v", decoded.Items)
	}
	if decoded.Items[0].LinkID == nil || *decoded.Items[0].LinkID != link.String() {
		t.Fatalf("first response link_id = %#v, want %s", decoded.Items[0].LinkID, link)
	}
}

func TestReaderInboxBulkDiscardMapsAtomicFailure(t *testing.T) {
	stub := &readerHandlerStub{
		discardBulk: func(context.Context, []string) ([]model.ReaderInboxBulkResult, error) {
			return nil, httperr.NewWithCode(http.StatusConflict, "inbox_state_conflict", "the inbox item is already confirmed")
		},
	}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	body := []byte(`{"inbox_ids":["00000000-0000-0000-0000-000000000001"]}`)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/inbox/bulk/discard", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("bulk discard status = %d, want 409; body=%s", response.Code, response.Body.String())
	}
	if response.Body.Len() == 0 {
		t.Fatal("bulk discard conflict should include an error envelope")
	}
}

func TestReaderInboxBulkRejectsEmptyEnvelopeBeforeService(t *testing.T) {
	stub := &readerHandlerStub{}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/inbox/bulk/confirm", bytes.NewReader([]byte(`{"inbox_ids":[]}`)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty bulk status = %d, want 422", response.Code)
	}
	if stub.confirmBulkCalled != nil {
		t.Fatalf("service called for invalid envelope: %#v", stub.confirmBulkCalled)
	}
}

func TestReaderFeedPassesRepeatedSourceQueryValues(t *testing.T) {
	stub := &readerHandlerStub{feedResponse: dto.ReaderFeedResponse{Mode: "recommended"}}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/reader-feed?mode=recommended&after=cursor-1&limit=17&source=reading&source=subscription", nil)
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("repeated source status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if stub.feedMode != "recommended" || stub.feedAfter != "cursor-1" || stub.feedLimit != 17 {
		t.Fatalf("feed request fields = mode=%q after=%q limit=%d", stub.feedMode, stub.feedAfter, stub.feedLimit)
	}
	if len(stub.feedSources) != 2 || stub.feedSources[0] != "reading" || stub.feedSources[1] != "subscription" {
		t.Fatalf("source query values = %#v, want repeated source values", stub.feedSources)
	}
}

func TestReaderFeedPassesCSVSourcesQueryValue(t *testing.T) {
	stub := &readerHandlerStub{}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/reader-feed?sources=reading,subscription", nil)
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("CSV source status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if len(stub.feedSources) != 1 || stub.feedSources[0] != "reading,subscription" {
		t.Fatalf("CSV source query value = %#v, want one CSV value", stub.feedSources)
	}
}

func TestReaderFeedWritesLiveCursorEnvelope(t *testing.T) {
	linkID := uuid.New()
	linkIDText := linkID.String()
	now := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	stub := &readerHandlerStub{feedResponse: dto.ReaderFeedResponse{
		Items: []dto.ReaderFeedItemResponse{{
			Key:         "link:" + linkID.String(),
			Source:      "reading",
			ResourceKey: "link:" + linkID.String(),
			Title:       "Reading",
			Summary:     "Summary",
			URL:         "https://example.com/reading",
			LinkID:      &linkIDText,
			Read:        true,
			EventAt:     now,
		}},
		NextCursor: "cursor-2",
		Mode:       "chronological",
	}}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/reader-feed?mode=chronological", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("feed status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var body dto.ReaderFeedResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode feed response: %v", err)
	}
	if body.NextCursor != "cursor-2" || body.Mode != "chronological" || len(body.Items) != 1 {
		t.Fatalf("feed envelope = %#v", body)
	}
	item := body.Items[0]
	if item.Key != "link:"+linkID.String() || item.ResourceKey != "link:"+linkID.String() || item.LinkID == nil || *item.LinkID != linkID.String() || !item.Read || !item.EventAt.Equal(now) {
		t.Fatalf("feed item wire = %#v", item)
	}
}

func TestReaderFeedFeedbackPassesActionIdentityAndAction(t *testing.T) {
	feedItemID := uuid.New()
	stub := &readerHandlerStub{}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/reader-feed/feedback?item_key=subscription:"+feedItemID.String(), bytes.NewReader([]byte(`{"action":"save"}`)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("feedback status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var body dto.ReaderFeedFeedbackResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode feedback response: %v", err)
	}
	if body.ItemKey != "subscription:"+feedItemID.String() || body.Action != "save" || body.LinkID == nil {
		t.Fatalf("feedback response = %#v", body)
	}
	if stub.feedbackItemKey != "subscription:"+feedItemID.String() || stub.feedbackAction != "save" {
		t.Fatalf("feedback command = (%q, %q), want canonical subscription identity and save", stub.feedbackItemKey, stub.feedbackAction)
	}
}
