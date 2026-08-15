package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

type recordingInvalidator struct {
	mu       sync.Mutex
	count    int
	contexts []context.Context
}

func (r *recordingInvalidator) Invalidate(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
	r.contexts = append(r.contexts, ctx)
}

func (r *recordingInvalidator) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// TestInvalidateAggregatesOnWriteFiresOnlyForSuccessfulWrites 锁定触发条件：
// 只在写方法 + 2xx 时失效。
func TestInvalidateAggregatesOnWriteFiresOnlyForSuccessfulWrites(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name   string
		method string
		status int
		want   int
	}{
		{name: "POST 201 失效", method: http.MethodPost, status: http.StatusCreated, want: 1},
		{name: "PUT 200 失效", method: http.MethodPut, status: http.StatusOK, want: 1},
		{name: "PATCH 200 失效", method: http.MethodPatch, status: http.StatusOK, want: 1},
		{name: "DELETE 204 失效", method: http.MethodDelete, status: http.StatusNoContent, want: 1},
		{name: "GET 不失效", method: http.MethodGet, status: http.StatusOK, want: 0},
		{name: "POST 422 不失效", method: http.MethodPost, status: http.StatusUnprocessableEntity, want: 0},
		{name: "POST 401 不失效", method: http.MethodPost, status: http.StatusUnauthorized, want: 0},
		{name: "POST 500 不失效", method: http.MethodPost, status: http.StatusInternalServerError, want: 0},
		{name: "POST 429 不失效", method: http.MethodPost, status: http.StatusTooManyRequests, want: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			invalidator := &recordingInvalidator{}
			router := gin.New()
			router.Use(InvalidateAggregatesOnWrite(invalidator))
			router.Handle(tc.method, "/api/links", func(c *gin.Context) {
				c.Status(tc.status)
			})

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(tc.method, "/api/links", nil))

			if got := invalidator.calls(); got != tc.want {
				t.Fatalf("失效次数 = %d, want %d（status=%d）", got, tc.want, rec.Code)
			}
		})
	}
}

// TestInvalidateAggregatesOnWriteUsesFinalRequestContext locks the ordering
// contract: invalidation runs after the handler and observes its final request
// context rather than a context captured before c.Next().
func TestInvalidateAggregatesOnWriteUsesFinalRequestContext(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	type markerKey struct{}
	const marker = "handler-complete"
	invalidator := &recordingInvalidator{}

	router := gin.New()
	router.Use(InvalidateAggregatesOnWrite(invalidator))
	router.POST("/api/links", func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), markerKey{}, marker))
		c.Status(http.StatusCreated)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/links", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if invalidator.calls() != 1 {
		t.Fatalf("失效次数 = %d, want 1", invalidator.calls())
	}
	if got := invalidator.contexts[0].Value(markerKey{}); got != marker {
		t.Fatalf("invalidation context marker = %v, want %q", got, marker)
	}
}

// TestInvalidateAggregatesOnWriteNilIsNoop 锁定 nil 装配（测试路径）不 panic。
func TestInvalidateAggregatesOnWriteNilIsNoop(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(InvalidateAggregatesOnWrite(nil))
	router.POST("/api/links", func(c *gin.Context) { c.Status(http.StatusCreated) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/links", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
}
