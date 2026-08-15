package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

type classificationRuleHandlerFake struct {
	rules          []model.LibraryClassificationRule
	createCalls    int
	create         repository.CreateClassificationRuleParams
	update         repository.UpdateClassificationRuleParams
	deleteID       string
	deleteRevision int64
}

func (f *classificationRuleHandlerFake) List(context.Context) ([]model.LibraryClassificationRule, error) {
	return f.rules, nil
}
func (f *classificationRuleHandlerFake) Create(_ context.Context, p repository.CreateClassificationRuleParams) (*model.LibraryClassificationRule, error) {
	f.createCalls++
	f.create = p
	return &model.LibraryClassificationRule{ID: uuid.New(), Host: p.Host, IdentityAdapter: p.IdentityAdapter, PathPrefix: p.PathPrefix, TargetKind: p.TargetKind, Enabled: p.Enabled, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}
func (f *classificationRuleHandlerFake) Update(_ context.Context, p repository.UpdateClassificationRuleParams) (*model.LibraryClassificationRule, error) {
	f.update = p
	return &model.LibraryClassificationRule{ID: p.ID, Host: "example.com", TargetKind: model.LibraryKindSite, Enabled: true, Revision: p.Revision + 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}
func (f *classificationRuleHandlerFake) Delete(_ context.Context, id string, revision int64) (bool, error) {
	f.deleteID, f.deleteRevision = id, revision
	return true, nil
}

func TestClassificationRuleRoutesCreateUpdateAndDeleteAcrossAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &classificationRuleHandlerFake{}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{ClassificationRules: svc}))

	create := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, "/api/library-classification-rules", strings.NewReader(`{"host":"example.com","target_kind":"site"}`))
	createRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(create, createRequest)
	if create.Code != http.StatusCreated || svc.create.Host != "example.com" || !svc.create.Enabled || svc.create.TargetKind != model.LibraryKindSite {
		t.Fatalf("create status=%d params=%#v body=%s", create.Code, svc.create, create.Body.String())
	}

	id := uuid.New().String()
	update := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/library-classification-rules/"+id, strings.NewReader(`{"host":"example.net","identity_adapter":null,"path_prefix":null,"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"4"`)
	router.ServeHTTP(update, req)
	if update.Code != http.StatusOK || svc.update.ID.String() != id || svc.update.Revision != 4 || svc.update.Host == nil || *svc.update.Host != "example.net" || svc.update.IdentityAdapter == nil || *svc.update.IdentityAdapter != nil || svc.update.PathPrefix == nil || *svc.update.PathPrefix != nil || svc.update.Enabled == nil || *svc.update.Enabled {
		t.Fatalf("update status=%d params=%#v body=%s", update.Code, svc.update, update.Body.String())
	}

	deleteRecorder := httptest.NewRecorder()
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/library-classification-rules/"+id, nil)
	deleteRequest.Header.Set("If-Match", "5")
	router.ServeHTTP(deleteRecorder, deleteRequest)
	if deleteRecorder.Code != http.StatusNoContent || svc.deleteID != id || svc.deleteRevision != 5 {
		t.Fatalf("delete status=%d id=%q revision=%d body=%s", deleteRecorder.Code, svc.deleteID, svc.deleteRevision, deleteRecorder.Body.String())
	}
}

func TestClassificationRuleUpdateRequiresValidRevisionAndID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{ClassificationRules: &classificationRuleHandlerFake{}}))
	for _, target := range []string{
		"/api/library-classification-rules/not-a-uuid",
		"/api/library-classification-rules/" + uuid.New().String(),
	} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, target, strings.NewReader(`{"enabled":true}`))
		req.Header.Set("Content-Type", "application/json")
		if strings.Contains(target, "not-a") {
			req.Header.Set("If-Match", "1")
		}
		router.ServeHTTP(recorder, req)
		want := http.StatusBadRequest
		if !strings.Contains(target, "not-a") {
			want = http.StatusPreconditionRequired
		}
		if recorder.Code != want {
			t.Fatalf("target=%s status=%d want=%d body=%s", target, recorder.Code, want, recorder.Body.String())
		}
	}
}

var _ ClassificationRuleService = (*classificationRuleHandlerFake)(nil)
