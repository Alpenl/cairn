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
)

type fakeLinkTranslationService struct {
	createReq  model.TranslationRequest
	createResp *model.LinkTranslation
	createErr  error
	listResp   model.TranslationList
	listErr    error
}

func (f *fakeLinkTranslationService) Create(_ context.Context, _ uuid.UUID, req model.TranslationRequest) (*model.LinkTranslation, error) {
	f.createReq = req
	return f.createResp, f.createErr
}

func (f *fakeLinkTranslationService) List(context.Context, uuid.UUID) (model.TranslationList, error) {
	return f.listResp, f.listErr
}

func translationTestRouter(svc LinkTranslationService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/links/:link_id/translations", createLinkTranslation(svc))
	router.GET("/api/links/:link_id/translations", listLinkTranslations(svc))
	return router
}

func TestCreateLinkTranslationReturnsAcceptedForPendingJob(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	linkID, translationID := uuid.New(), uuid.New()
	svc := &fakeLinkTranslationService{createResp: &model.LinkTranslation{
		ID: translationID, LinkID: linkID, Scope: model.TranslationScopeSelection, BlockKey: "summary",
		StartOffset: 0, EndOffset: 5, SourceText: "hello", SourceFormat: "plain",
		TargetLanguage: "zh-CN", Status: model.TranslationStatusPending, CreatedAt: now, UpdatedAt: now,
	}}
	body := []byte(`{"scope":"selection","block_key":"summary","start_offset":0,"end_offset":5,"source_text":"hello"}`)
	recorder := httptest.NewRecorder()
	translationTestRouter(svc).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/links/"+linkID.String()+"/translations", bytes.NewReader(body)))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s, want 202", recorder.Code, recorder.Body.String())
	}
	if svc.createReq.SourceText != "hello" || svc.createReq.Scope != model.TranslationScopeSelection {
		t.Fatalf("service request = %+v", svc.createReq)
	}
}

func TestCreateLinkTranslationPreservesOptionalExpectedSourceIdentity(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	now := time.Now().UTC()
	tests := []struct {
		name           string
		body           string
		wantRevision   *int64
		wantSourceHash *string
	}{
		{
			name:         "saved content revision",
			body:         `{"scope":"full","expected_content_revision":7}`,
			wantRevision: func() *int64 { value := int64(7); return &value }(),
		},
		{
			name:           "summary source hash",
			body:           `{"scope":"selection","block_key":"summary","start_offset":0,"end_offset":5,"source_text":"hello","expected_source_hash":"sha256:current-summary"}`,
			wantSourceHash: func() *string { value := "sha256:current-summary"; return &value }(),
		},
		{
			name: "legacy client omission",
			body: `{"scope":"selection","block_key":"summary","start_offset":0,"end_offset":5,"source_text":"hello"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeLinkTranslationService{createResp: &model.LinkTranslation{
				ID: uuid.New(), LinkID: linkID, Scope: model.TranslationScopeSelection,
				TargetLanguage: model.TranslationTargetChinese, Status: model.TranslationStatusDone,
				CreatedAt: now, UpdatedAt: now,
			}}
			recorder := httptest.NewRecorder()
			translationTestRouter(svc).ServeHTTP(recorder, httptest.NewRequest(
				http.MethodPost,
				"/api/links/"+linkID.String()+"/translations",
				bytes.NewBufferString(tt.body),
			))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s, want 200", recorder.Code, recorder.Body.String())
			}
			if !equalInt64Pointers(svc.createReq.ExpectedContentRevision, tt.wantRevision) {
				t.Errorf("ExpectedContentRevision = %v, want %v", svc.createReq.ExpectedContentRevision, tt.wantRevision)
			}
			if !equalStringPointers(svc.createReq.ExpectedSourceHash, tt.wantSourceHash) {
				t.Errorf("ExpectedSourceHash = %v, want %v", svc.createReq.ExpectedSourceHash, tt.wantSourceHash)
			}
		})
	}
}

func equalInt64Pointers(got, want *int64) bool {
	return got == nil && want == nil || got != nil && want != nil && *got == *want
}

func equalStringPointers(got, want *string) bool {
	return got == nil && want == nil || got != nil && want != nil && *got == *want
}

func TestCreateLinkTranslationReturnsOKForCachedResult(t *testing.T) {
	t.Parallel()

	translated := "你好"
	now := time.Now().UTC()
	linkID, translationID := uuid.New(), uuid.New()
	svc := &fakeLinkTranslationService{createResp: &model.LinkTranslation{
		ID: translationID, LinkID: linkID, Scope: model.TranslationScopeSelection, BlockKey: "summary",
		StartOffset: 0, EndOffset: 5, SourceText: "hello", TranslatedText: &translated,
		SourceFormat: model.TranslationFormatPlain, TargetLanguage: "zh-CN", Status: model.TranslationStatusDone,
		CreatedAt: now, UpdatedAt: now,
	}}
	body := []byte(`{"scope":"selection","block_key":"summary","start_offset":0,"end_offset":5,"source_text":"hello"}`)
	recorder := httptest.NewRecorder()
	translationTestRouter(svc).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/links/"+linkID.String()+"/translations", bytes.NewReader(body)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", recorder.Code, recorder.Body.String())
	}
}

func TestCreateLinkTranslationReturnsNullableSourceContentRevision(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	now := time.Now().UTC()
	revision := int64(11)
	tests := []struct {
		name     string
		revision *int64
	}{
		{name: "verified saved content", revision: &revision},
		{name: "legacy unverified", revision: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeLinkTranslationService{createResp: &model.LinkTranslation{
				ID: uuid.New(), LinkID: linkID, Scope: model.TranslationScopeFull,
				SourceContentRevision: tt.revision,
				TargetLanguage:        model.TranslationTargetChinese, Status: model.TranslationStatusDone,
				CreatedAt: now, UpdatedAt: now,
			}}
			recorder := httptest.NewRecorder()
			translationTestRouter(svc).ServeHTTP(recorder, httptest.NewRequest(
				http.MethodPost,
				"/api/links/"+linkID.String()+"/translations",
				bytes.NewBufferString(`{"scope":"full"}`),
			))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s, want 200", recorder.Code, recorder.Body.String())
			}
			var response dto.TranslationResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if !equalInt64Pointers(response.SourceContentRevision, tt.revision) {
				t.Errorf("source_content_revision = %v, want %v", response.SourceContentRevision, tt.revision)
			}
			var raw map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode raw response: %v", err)
			}
			if _, ok := raw["source_content_revision"]; !ok {
				t.Fatalf("body = %s, source_content_revision must be explicit even for legacy rows", recorder.Body.String())
			}
		})
	}
}

func TestCreateLinkTranslationReturnsStableConflictIdentity(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	revision := int64(12)
	sourceHash := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	tests := []struct {
		name     string
		code     string
		wantCode string
		identity httperr.ConflictIdentity
	}{
		{
			name:     "saved content revision conflict",
			code:     httperr.CodeContentRevisionConflict,
			wantCode: "content_revision_conflict",
			identity: httperr.ConflictIdentity{ContentRevision: &revision, BlockKey: "content"},
		},
		{
			name:     "summary source block conflict",
			code:     httperr.CodeSourceBlockConflict,
			wantCode: "source_block_conflict",
			identity: httperr.ConflictIdentity{BlockKey: "summary", SourceHash: &sourceHash},
		},
		{
			name:     "rolling schema transition",
			code:     httperr.CodeTranslationSchemaTransition,
			wantCode: "translation_schema_transition",
			identity: httperr.ConflictIdentity{ContentRevision: &revision, BlockKey: "content"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeLinkTranslationService{createErr: httperr.NewWithCodeAndCurrentIdentity(
				http.StatusConflict,
				tt.code,
				"translation source changed",
				tt.identity,
			)}
			recorder := httptest.NewRecorder()
			translationTestRouter(svc).ServeHTTP(recorder, httptest.NewRequest(
				http.MethodPost,
				"/api/links/"+linkID.String()+"/translations",
				bytes.NewBufferString(`{"scope":"full","expected_content_revision":11}`),
			))

			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d body=%s, want 409", recorder.Code, recorder.Body.String())
			}
			var response dto.ErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Error.ErrorCode != tt.wantCode {
				t.Errorf("error_code = %q, want %q", response.Error.ErrorCode, tt.wantCode)
			}
			got := response.Error.CurrentIdentity
			if got == nil {
				t.Fatalf("current_identity is nil: %s", recorder.Body.String())
			}
			if !equalInt64Pointers(got.ContentRevision, tt.identity.ContentRevision) ||
				got.BlockKey != tt.identity.BlockKey ||
				!equalStringPointers(got.SourceHash, tt.identity.SourceHash) {
				t.Errorf("current_identity = %+v, want %+v", got, tt.identity)
			}
		})
	}
}

func TestListLinkTranslationsReturnsPersistentItems(t *testing.T) {
	t.Parallel()

	linkID, translationID := uuid.New(), uuid.New()
	summaryHash := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	svc := &fakeLinkTranslationService{listResp: model.TranslationList{
		CurrentContentRevision:   9,
		CurrentSummarySourceHash: &summaryHash,
		Items: []model.LinkTranslation{{
			ID: translationID, LinkID: linkID, Status: model.TranslationStatusDone, TargetLanguage: "zh-CN",
		}},
	}}
	recorder := httptest.NewRecorder()
	translationTestRouter(svc).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/links/"+linkID.String()+"/translations", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response dto.TranslationListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].ID != translationID.String() {
		t.Fatalf("response = %+v", response)
	}
	if response.CurrentContentRevision != 9 {
		t.Fatalf("current_content_revision = %d, want 9", response.CurrentContentRevision)
	}
	if response.CurrentSummarySourceHash == nil || *response.CurrentSummarySourceHash != summaryHash {
		t.Fatalf("current_summary_source_hash = %v, want %q", response.CurrentSummarySourceHash, summaryHash)
	}
}

func TestListLinkTranslationsReturnsExplicitNullSummaryIdentity(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	svc := &fakeLinkTranslationService{listResp: model.TranslationList{Items: []model.LinkTranslation{}}}
	recorder := httptest.NewRecorder()
	translationTestRouter(svc).ServeHTTP(recorder, httptest.NewRequest(
		http.MethodGet,
		"/api/links/"+linkID.String()+"/translations",
		nil,
	))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got, ok := raw["current_summary_source_hash"]; !ok || string(got) != "null" {
		t.Fatalf("body = %s, want explicit null current_summary_source_hash", recorder.Body.String())
	}
}
