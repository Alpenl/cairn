package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/repository"
)

type stubMergeService struct {
	mu           sync.Mutex
	listResult   []ConceptMergeView
	listErr      error
	approveErr   error
	rejectErr    error
	approveCalls []struct {
		id uuid.UUID
		by string
	}
	rejectCalls []struct {
		id uuid.UUID
		by string
	}
}

func (s *stubMergeService) ListPending(_ context.Context, _, _ int) ([]ConceptMergeView, error) {
	return s.listResult, s.listErr
}
func (s *stubMergeService) Approve(_ context.Context, id uuid.UUID, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approveCalls = append(s.approveCalls, struct {
		id uuid.UUID
		by string
	}{id, by})
	return s.approveErr
}
func (s *stubMergeService) Reject(_ context.Context, id uuid.UUID, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectCalls = append(s.rejectCalls, struct {
		id uuid.UUID
		by string
	}{id, by})
	return s.rejectErr
}

func newConceptMergeRouter(svc ConceptMergeService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterConceptMergeRoutes(r, svc)
	return r
}

func TestListConceptMergesReturnsJSONItems(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	svc := &stubMergeService{
		listResult: []ConceptMergeView{{
			ID: id, WinnerName: "RAG", LoserName: "检索增强生成",
			Score: 0.82, LLMReason: "same concept",
		}},
	}
	r := newConceptMergeRouter(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/concept-merges", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Items []ConceptMergeView `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].WinnerName != "RAG" {
		t.Fatalf("items = %+v", body.Items)
	}
}

func TestApproveConceptMergeCallsServiceWithActorHeader(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	svc := &stubMergeService{}
	r := newConceptMergeRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/concept-merges/"+id.String()+"/approve", nil)
	req.Header.Set("X-Admin-Actor", "ops@acme.io")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if len(svc.approveCalls) != 1 || svc.approveCalls[0].id != id {
		t.Fatalf("approveCalls = %+v", svc.approveCalls)
	}
	if svc.approveCalls[0].by != "ops@acme.io" {
		t.Fatalf("decided_by = %q, want X-Admin-Actor header value", svc.approveCalls[0].by)
	}
}

func TestApproveConceptMergeFallsBackToAdminWhenHeaderMissing(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	svc := &stubMergeService{}
	r := newConceptMergeRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/concept-merges/"+id.String()+"/approve", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if svc.approveCalls[0].by != "admin" {
		t.Fatalf("decided_by = %q, want fallback 'admin'", svc.approveCalls[0].by)
	}
}

func TestApproveConceptMergeMapsAlreadyDecidedTo409(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	svc := &stubMergeService{approveErr: repository.ErrConceptMergeAlreadyDecided}
	r := newConceptMergeRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/concept-merges/"+id.String()+"/approve", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestApproveConceptMergeMapsOrphanedProposalTo409(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	svc := &stubMergeService{approveErr: repository.ErrConceptMergeOrphaned}
	r := newConceptMergeRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/concept-merges/"+id.String()+"/approve", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "missing concept") {
		t.Fatalf("status/body = %d/%q, want explicit orphan conflict", w.Code, w.Body.String())
	}
}

func TestRejectConceptMergeCallsRejectPath(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	svc := &stubMergeService{}
	r := newConceptMergeRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/concept-merges/"+id.String()+"/reject", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if len(svc.rejectCalls) != 1 {
		t.Fatalf("rejectCalls = %d, want 1", len(svc.rejectCalls))
	}
}

func TestConceptMergeRejectsInvalidUUID(t *testing.T) {
	t.Parallel()
	svc := &stubMergeService{}
	r := newConceptMergeRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/concept-merges/not-a-uuid/approve", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid proposal id") {
		t.Fatalf("body = %q", w.Body.String())
	}
	if len(svc.approveCalls) != 0 {
		t.Fatalf("invalid UUID should not reach service")
	}
}

func TestConceptMergeRoutesSkippedWhenServiceNil(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterConceptMergeRoutes(r, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/concept-merges", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("nil-service route still mounted; status = %d", w.Code)
	}
}

func TestListConceptMergesPropagatesServiceError(t *testing.T) {
	t.Parallel()
	svc := &stubMergeService{listErr: errors.New("DB error")}
	r := newConceptMergeRouter(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/concept-merges", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code < 500 || w.Code >= 600 {
		t.Fatalf("unmapped service error should land as 5xx; got %d", w.Code)
	}
}

// TestApproveConceptMergeTruncatesOverlongActorHeader 验证超长 X-Admin-Actor
// 被截断到 maxAdminActorLen 字节再落到 service.Approve 的 decided_by 参数。
// 防御上游反代 / SSO 误配出超长 token 直接撑大 audit 行的风险。
func TestApproveConceptMergeTruncatesOverlongActorHeader(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	svc := &stubMergeService{}
	r := newConceptMergeRouter(svc)

	// 构造 200 字节超长头部（远超 maxAdminActorLen=64），全部 ASCII 字符
	// 简化字节计数。
	longActor := strings.Repeat("a", 200)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/concept-merges/"+id.String()+"/approve", nil)
	req.Header.Set("X-Admin-Actor", longActor)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if len(svc.approveCalls) != 1 {
		t.Fatalf("approveCalls = %d, want 1", len(svc.approveCalls))
	}
	got := svc.approveCalls[0].by
	if len(got) != maxAdminActorLen {
		t.Fatalf("decided_by length = %d, want %d (truncated)", len(got), maxAdminActorLen)
	}
	if got != strings.Repeat("a", maxAdminActorLen) {
		t.Fatalf("decided_by = %q, want first %d bytes of input", got, maxAdminActorLen)
	}
}

// TestTruncateAdminActorBoundary 单测截断边界：等于上限保持原样、超出截断。
// TestParseIntQuery 锁定 listConceptMerges 等 admin endpoint 用的查询
// 参数兜底：empty / non-numeric / 越界都收敛到合理值，避免下游 SQL
// limit/offset 接到负数或巨大值。回归会让 /api/admin/concept-merges
// 这类管理面 endpoint 在用户传错参数时直接 500 或拉满 DB 资源。
//
// 当前覆盖率 27.3% 是因为之前只有单条路径用例间接覆盖；这里把全部 5
// 条分支独立 lock 住。
func TestParseIntQuery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string // 空表示不传 query；非空作为 ?n=<raw> 传入
		def  int
		min  int
		max  int
		want int
	}{
		{"empty_returns_default", "", 20, 1, 200, 20},
		{"non_numeric_returns_default", "abc", 20, 1, 200, 20},
		{"below_min_clamps_to_min", "0", 20, 1, 200, 1},
		{"negative_clamps_to_min", "-5", 20, 1, 200, 1},
		{"above_max_clamps_to_max", "9999", 20, 1, 200, 200},
		{"at_min_boundary_passes_through", "1", 20, 1, 200, 1},
		{"at_max_boundary_passes_through", "200", 20, 1, 200, 200},
		{"in_range_passes_through", "42", 20, 1, 200, 42},
		// 边界：大负数被 clamp。
		{"large_negative_clamps_to_min", "-9999", 20, 1, 200, 1},
		// 边界：空格被 strconv.Atoi 视为非法。
		{"whitespace_returns_default", "   ", 20, 1, 200, 20},
		// 边界：浮点也是 Atoi 错。
		{"float_returns_default", "3.14", 20, 1, 200, 20},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			urlStr := "/x"
			if tc.raw != "" {
				urlStr = "/x?n=" + url.QueryEscape(tc.raw)
			}
			c.Request = httptest.NewRequest(http.MethodGet, urlStr, nil)
			if got := parseIntQuery(c, "n", tc.def, tc.min, tc.max); got != tc.want {
				t.Errorf("parseIntQuery(raw=%q, def=%d, min=%d, max=%d) = %d, want %d",
					tc.raw, tc.def, tc.min, tc.max, got, tc.want)
			}
		})
	}
}

func TestTruncateAdminActorBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short", "ops@acme.io", "ops@acme.io"},
		{"exact-limit", strings.Repeat("x", maxAdminActorLen), strings.Repeat("x", maxAdminActorLen)},
		{"one-over", strings.Repeat("x", maxAdminActorLen+1), strings.Repeat("x", maxAdminActorLen)},
		{"way-over", strings.Repeat("y", 1000), strings.Repeat("y", maxAdminActorLen)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := truncateAdminActor(tc.in)
			if got != tc.want {
				t.Fatalf("truncateAdminActor(len=%d) = %q (len=%d), want %q (len=%d)",
					len(tc.in), got, len(got), tc.want, len(tc.want))
			}
		})
	}
}
