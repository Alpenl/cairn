package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakeIdempotencyStore is an in-memory IdempotencyStore for unit tests. It
// mirrors the PG store's Acquire/Store/Delete/PurgeExpired contract: Acquire
// inserts an in-flight placeholder on first sight (returns acquired=true),
// reclaims only completed expired rows, or returns the existing record on
// conflict; Store backfills the response and clears in-flight; Delete drops a
// key; PurgeExpired removes only completed expired rows.
// mu-protected so the concurrent "two replicas, same key" test is race-free.
type fakeIdempotencyStore struct {
	mu          sync.Mutex
	entries     map[string]*StoredResponse
	getObserved chan struct{}
}

func newFakeIdempotencyStore() *fakeIdempotencyStore {
	return &fakeIdempotencyStore{entries: make(map[string]*StoredResponse)}
}

func (s *fakeIdempotencyStore) Acquire(_ context.Context, key, ownerToken string, now, expiresAt time.Time) (bool, *StoredResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.entries[key]; ok {
		if !existing.InFlight && !existing.ExpiresAt.After(now) {
			existing.Claim.OwnerToken = ownerToken
			existing.Claim.Generation++
			existing.Status = 0
			existing.Body = nil
			existing.ContentType = ""
			existing.InFlight = true
			existing.ExpiresAt = expiresAt
			cp := *existing
			return true, &cp, nil
		}
		cp := *existing
		return false, &cp, nil
	}
	entry := &StoredResponse{
		Claim:     IdempotencyClaim{OwnerToken: ownerToken, Generation: 1},
		InFlight:  true,
		ExpiresAt: expiresAt,
	}
	s.entries[key] = entry
	cp := *entry
	return true, &cp, nil
}

func (s *fakeIdempotencyStore) Get(_ context.Context, key string) (*StoredResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getObserved != nil {
		select {
		case s.getObserved <- struct{}{}:
		default:
		}
	}
	entry := s.entries[key]
	if entry == nil {
		return nil, nil
	}
	cp := *entry
	cp.Body = append([]byte(nil), entry.Body...)
	return &cp, nil
}

type faultIdempotencyStore struct {
	*fakeIdempotencyStore
	acquireErr error
	getErr     error
	storeErr   error
	deleteErr  error
}

func (s *faultIdempotencyStore) Acquire(ctx context.Context, key, ownerToken string, now, expiresAt time.Time) (bool, *StoredResponse, error) {
	if s.acquireErr != nil {
		return false, nil, s.acquireErr
	}
	return s.fakeIdempotencyStore.Acquire(ctx, key, ownerToken, now, expiresAt)
}

func (s *faultIdempotencyStore) Get(ctx context.Context, key string) (*StoredResponse, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.fakeIdempotencyStore.Get(ctx, key)
}

func (s *faultIdempotencyStore) Store(ctx context.Context, key string, claim IdempotencyClaim, status int, body []byte, contentType string, expiresAt time.Time) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	return s.fakeIdempotencyStore.Store(ctx, key, claim, status, body, contentType, expiresAt)
}

func (s *faultIdempotencyStore) Delete(ctx context.Context, key string, claim IdempotencyClaim) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.fakeIdempotencyStore.Delete(ctx, key, claim)
}

type finalizeContextStore struct {
	*fakeIdempotencyStore
	finalizeContext chan finalizeContextObservation
}

type finalizeContextObservation struct {
	err         error
	hasDeadline bool
}

func (s *finalizeContextStore) Store(ctx context.Context, key string, claim IdempotencyClaim, status int, body []byte, contentType string, expiresAt time.Time) error {
	_, hasDeadline := ctx.Deadline()
	s.finalizeContext <- finalizeContextObservation{err: ctx.Err(), hasDeadline: hasDeadline}
	return s.fakeIdempotencyStore.Store(ctx, key, claim, status, body, contentType, expiresAt)
}

func (s *fakeIdempotencyStore) Store(_ context.Context, key string, claim IdempotencyClaim, status int, body []byte, contentType string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || entry.Claim != claim || !entry.InFlight {
		return errIdempotencyResultUnknown
	}
	entry.Status = status
	entry.Body = append([]byte(nil), body...)
	entry.ContentType = contentType
	entry.InFlight = false
	entry.ExpiresAt = expiresAt
	return nil
}

func (s *fakeIdempotencyStore) Delete(_ context.Context, key string, claim IdempotencyClaim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	if entry == nil || entry.Claim != claim || !entry.InFlight {
		return errIdempotencyResultUnknown
	}
	delete(s.entries, key)
	return nil
}

func (s *fakeIdempotencyStore) PurgeExpired(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for k, v := range s.entries {
		if !v.InFlight && !v.ExpiresAt.After(now) {
			delete(s.entries, k)
			n++
		}
	}
	return n, nil
}

// newTestCache 构造并启动一个 PG 幂等缓存（背后用 in-memory fake store）+
// 可控时钟，给 TTL 相关测试用。Stop 在 t.Cleanup 挂上避免 goroutine leak。
func newTestCache(t *testing.T, ttl time.Duration, now func() time.Time) (*PGIdempotencyCache, *fakeIdempotencyStore) {
	t.Helper()
	store := newFakeIdempotencyStore()
	cache, err := NewPGIdempotencyCache(PGIdempotencyOptions{
		Store: store,
		TTL:   ttl,
		Now:   now,
	})
	if err != nil {
		t.Fatalf("NewPGIdempotencyCache() error = %v", err)
	}
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := cache.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
	return cache, store
}

func newTestCacheWithOptions(t *testing.T, opts PGIdempotencyOptions) *PGIdempotencyCache {
	t.Helper()
	cache, err := NewPGIdempotencyCache(opts)
	if err != nil {
		t.Fatalf("NewPGIdempotencyCache() error = %v", err)
	}
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := cache.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
	return cache
}

// newRouterWithIdempotency 把 Idempotency 中间件挂到一个空 engine 上，
// 返回 engine + 一个原子计数器（统计 handler 真正被调用的次数）。
func newRouterWithIdempotency(t *testing.T, cache *PGIdempotencyCache) (*gin.Engine, *atomic.Int64) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Idempotency(cache))

	var calls atomic.Int64
	handler := func(c *gin.Context) {
		n := calls.Add(1)
		c.Header("Content-Type", "application/json")
		c.JSON(http.StatusCreated, gin.H{"call": n})
	}
	router.POST("/api/links", handler)
	router.PUT("/api/links/:id", handler)
	router.PATCH("/api/links/:id", handler)
	router.DELETE("/api/links/:id", handler)
	router.GET("/api/links", handler)
	return router, &calls
}

// TestIdempotencyReplaysOnKeyHit 锁定核心行为：同一个 (method, path, key)
// 的第二次请求不调 handler，直接回放已完成的 status + body。
func TestIdempotencyReplaysOnKeyHit(t *testing.T) {
	cache, _ := newTestCache(t, time.Hour, time.Now)
	router, calls := newRouterWithIdempotency(t, cache)

	doPOST := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{"url":"x"}`))
		req.Header.Set(IdempotencyHeader, "test-key-1")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	first := doPOST()
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}
	firstBody := first.Body.String()

	second := doPOST()
	if second.Code != http.StatusCreated {
		t.Fatalf("second status = %d, want 201", second.Code)
	}
	if second.Body.String() != firstBody {
		t.Fatalf("replay body = %q, want %q", second.Body.String(), firstBody)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times, want 1 (second served from cache)", got)
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay response missing Idempotent-Replay header")
	}
}

// TestIdempotencyMissCallsHandler 不带 key 的请求每次都调 handler（无缓存）。
func TestIdempotencyMissCallsHandler(t *testing.T) {
	cache, _ := newTestCache(t, time.Hour, time.Now)
	router, calls := newRouterWithIdempotency(t, cache)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("handler called %d times, want 3 (no key = no dedupe)", got)
	}
}

// TestIdempotencyIsolatesByMethodAndPath 同一 key 在不同 (method, path) 上
// 不互相串响应——缓存键含 method + path。
func TestIdempotencyIsolatesByMethodAndPath(t *testing.T) {
	cache, _ := newTestCache(t, time.Hour, time.Now)
	router, calls := newRouterWithIdempotency(t, cache)

	send := func(method, path string) {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "shared-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
	}
	send(http.MethodPost, "/api/links")
	send(http.MethodPut, "/api/links/1")
	send(http.MethodPatch, "/api/links/1")
	if got := calls.Load(); got != 3 {
		t.Fatalf("handler called %d times, want 3 (different method/path not deduped)", got)
	}
}

// TestIdempotencySkipsNonIdempotentMethods GET 不被中间件介入。
func TestIdempotencySkipsNonIdempotentMethods(t *testing.T) {
	cache, _ := newTestCache(t, time.Hour, time.Now)
	router, calls := newRouterWithIdempotency(t, cache)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
		req.Header.Set(IdempotencyHeader, "get-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("GET handler called %d times, want 2 (GET not deduped)", got)
	}
}

// TestIdempotencyKeeps5xxResultUnknown covers a handler that may have committed
// its side effect before returning 500. The same key must never execute again.
func TestIdempotencyKeeps5xxResultUnknown(t *testing.T) {
	var nowNanos atomic.Int64
	nowNanos.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	cache, store := newTestCache(t, time.Minute, clock)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Idempotency(cache))
	var committedSideEffects atomic.Int64
	router.POST("/fail", func(c *gin.Context) {
		committedSideEffects.Add(1)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "boom"})
	})

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/fail", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "fail-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if first := send(); first.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want 500", first.Code)
	}
	if liveRetry := send(); liveRetry.Code != http.StatusTooEarly {
		t.Fatalf("live retry status = %d, want 425", liveRetry.Code)
	}
	nowNanos.Add(int64(2 * time.Minute))
	unknownRetry := send()
	assertIdempotencyError(t, unknownRetry, http.StatusServiceUnavailable, ErrCodeIdempotencyResultUnknown)
	if got := committedSideEffects.Load(); got != 1 {
		t.Fatalf("committed side effects = %d, want exactly 1", got)
	}
	store.mu.Lock()
	entry := store.entries["POST:/fail:fail-key"]
	store.mu.Unlock()
	if entry == nil || !entry.InFlight {
		t.Fatalf("5xx claim = %#v, want durable in-flight evidence", entry)
	}
}

// A handler can commit its side effect and then panic before the idempotency
// middleware observes a response. Recovery converts that panic to 500, but the
// owned claim must remain durable so the same key can never enter the handler
// again, including after its live TTL expires.
func TestIdempotencyKeepsPanickedResultUnknown(t *testing.T) {
	var nowNanos atomic.Int64
	nowNanos.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	cache, store := newTestCache(t, time.Minute, clock)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) {
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	router.Use(Idempotency(cache))
	var committedSideEffects atomic.Int64
	router.POST("/panic", func(_ *gin.Context) {
		committedSideEffects.Add(1)
		panic("after commit")
	})

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/panic", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "panic-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if first := send(); first.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want 500", first.Code)
	}
	if liveRetry := send(); liveRetry.Code != http.StatusTooEarly {
		t.Fatalf("live retry status = %d, want 425", liveRetry.Code)
	}
	nowNanos.Add(int64(2 * time.Minute))
	assertIdempotencyError(t, send(), http.StatusServiceUnavailable, ErrCodeIdempotencyResultUnknown)
	if got := committedSideEffects.Load(); got != 1 {
		t.Fatalf("committed side effects = %d, want exactly 1", got)
	}
	store.mu.Lock()
	entry := store.entries["POST:/panic:panic-key"]
	store.mu.Unlock()
	if entry == nil || !entry.InFlight {
		t.Fatalf("panic claim = %#v, want durable in-flight evidence", entry)
	}
}

func TestIdempotencyCachesImplicitOK(t *testing.T) {
	cache, _ := newTestCache(t, time.Hour, time.Now)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Idempotency(cache))
	var calls atomic.Int64
	router.POST("/implicit-ok", func(_ *gin.Context) {
		calls.Add(1)
	})

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/implicit-ok", nil)
		req.Header.Set(IdempotencyHeader, "implicit-ok-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	first := send()
	second := send()
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d/%d, want 200/200", first.Code, second.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("implicit 200 replay missing Idempotent-Replay header")
	}
}

func TestIdempotencyDoesNotCacheTransientClientErrors(t *testing.T) {
	t.Parallel()

	for _, transientStatus := range []int{
		http.StatusConflict,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
	} {
		transientStatus := transientStatus
		t.Run(http.StatusText(transientStatus), func(t *testing.T) {
			t.Parallel()

			cache, store := newTestCache(t, time.Hour, time.Now)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(Idempotency(cache))
			var calls atomic.Int64
			router.POST("/retry", func(c *gin.Context) {
				if calls.Add(1) == 1 {
					c.Header("Retry-After", "30")
					c.JSON(transientStatus, gin.H{"error": "retry later"})
					return
				}
				c.JSON(http.StatusCreated, gin.H{"ok": true})
			})

			send := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, "/retry", strings.NewReader(`{}`))
				req.Header.Set(IdempotencyHeader, "transient-key")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec
			}

			first := send()
			if first.Code != transientStatus || first.Header().Get("Retry-After") != "30" {
				t.Fatalf("first response = %d Retry-After=%q, want %d and 30", first.Code, first.Header().Get("Retry-After"), transientStatus)
			}
			second := send()
			if second.Code != http.StatusCreated {
				t.Fatalf("second response = %d, want 201 after retry condition recovered", second.Code)
			}
			if second.Header().Get("Retry-After") != "" {
				t.Fatalf("second Retry-After = %q, want no stale transient header", second.Header().Get("Retry-After"))
			}
			if second.Header().Get("Idempotent-Replay") != "" {
				t.Fatal("second response unexpectedly replayed the transient result")
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("handler calls = %d, want 2", got)
			}

			store.mu.Lock()
			entry := store.entries["POST:/retry:transient-key"]
			store.mu.Unlock()
			if entry == nil || entry.InFlight || entry.Status != http.StatusCreated {
				t.Fatalf("stored response = %#v, want completed 201 from the retry", entry)
			}
		})
	}
}

// TestIdempotencyCachesDeterministic4xx locks the client-error cases whose
// outcome cannot change without changing the request body.
func TestIdempotencyCachesDeterministic4xx(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			cache, _ := newTestCache(t, time.Hour, time.Now)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(Idempotency(cache))
			var calls atomic.Int64
			router.POST("/bad", func(c *gin.Context) {
				calls.Add(1)
				c.JSON(status, gin.H{"error": "invalid"})
			})

			send := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, "/bad", strings.NewReader(`{}`))
				req.Header.Set(IdempotencyHeader, "bad-key")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec
			}
			first := send()
			second := send()
			if first.Code != status || second.Code != status {
				t.Fatalf("statuses = %d/%d, want %d/%d", first.Code, second.Code, status, status)
			}
			if second.Header().Get("Idempotent-Replay") != "true" {
				t.Fatal("second deterministic client error was not replayed")
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("handler calls = %d, want 1", got)
			}
		})
	}
}

// TestIdempotencyTTLExpiry 过期后视为未命中，handler 再次被调。
func TestIdempotencyTTLExpiry(t *testing.T) {
	var nowVal atomic.Int64
	nowVal.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, nowVal.Load()) }

	cache, _ := newTestCache(t, time.Minute, clock)
	router, calls := newRouterWithIdempotency(t, cache)

	send := func() {
		req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "ttl-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
	}
	send()
	// 推进时钟越过 TTL（store 里的 entry 仍存在但 ExpiresAt 已过 → 视为未命中
	// 放行）。
	nowVal.Add(int64(2 * time.Minute))
	send()
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler called %d times, want 2 (expired key re-runs)", got)
	}
}

// TestIdempotencyNilCacheIsNoOp nil cache → 中间件退化为透传。
func TestIdempotencyNilCacheIsNoOp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Idempotency(nil))
	var calls atomic.Int64
	router.POST("/x", func(c *gin.Context) {
		calls.Add(1)
		c.Status(http.StatusOK)
	})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "k")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("nil cache handler called %d times, want 2 (no-op pass-through)", got)
	}
}

// TestIdempotencyStopIsIdempotent 多次 Stop 安全。
func TestIdempotencyStopIsIdempotent(t *testing.T) {
	store := newFakeIdempotencyStore()
	cache, err := NewPGIdempotencyCache(PGIdempotencyOptions{Store: store, TTL: time.Hour})
	if err != nil {
		t.Fatalf("NewPGIdempotencyCache() error = %v", err)
	}
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := cache.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	// 第二次 Stop 不应 panic / 阻塞。
	done := make(chan struct{})
	go func() {
		if err := cache.Stop(context.Background()); err != nil {
			t.Errorf("second Stop() error = %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second Stop blocked")
	}
}

// TestIdempotencyReplayPreservesBody 回放保留完整 body 字节（含非 ASCII）。
func TestIdempotencyReplayPreservesBody(t *testing.T) {
	cache, _ := newTestCache(t, time.Hour, time.Now)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Idempotency(cache))
	payload := `{"msg":"héllo 世界","n":42}`
	router.POST("/echo", func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		c.String(http.StatusCreated, payload)
	})

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "body-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	first := send()
	second := send()
	if first.Body.String() != payload {
		t.Fatalf("first body = %q, want %q", first.Body.String(), payload)
	}
	if second.Body.String() != payload {
		t.Fatalf("replay body = %q, want %q", second.Body.String(), payload)
	}
	if ct := second.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("replay content-type = %q, want application/json", ct)
	}
}

func TestIdempotencyConcurrentRequestWaitsAndReplays(t *testing.T) {
	store := newFakeIdempotencyStore()
	store.getObserved = make(chan struct{}, 1)
	cache := newTestCacheWithOptions(t, PGIdempotencyOptions{
		Store:            store,
		TTL:              time.Hour,
		WaitTimeout:      2 * time.Second,
		WaitPollInterval: time.Millisecond,
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Idempotency(cache))
	var calls atomic.Int64
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	router.POST("/todos", func(c *gin.Context) {
		calls.Add(1)
		close(handlerEntered)
		<-releaseHandler
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.String(http.StatusCreated, `{"id":"todo-1"}`)
	})

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/todos", strings.NewReader(`{"text":"one"}`))
		req.Header.Set(IdempotencyHeader, "concurrent-create")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- send() }()
	<-handlerEntered
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- send() }()
	select {
	case <-store.getObserved:
	case <-time.After(time.Second):
		t.Fatal("duplicate request did not enter the bounded wait path")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls while duplicate waits = %d, want 1", got)
	}
	close(releaseHandler)

	first := <-firstDone
	second := <-secondDone
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("statuses = %d/%d, want 201/201", first.Code, second.Code)
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("replay body = %q, want %q", second.Body.String(), first.Body.String())
	}
	if second.Header().Get("Content-Type") != first.Header().Get("Content-Type") {
		t.Fatalf("replay content type = %q, want %q", second.Header().Get("Content-Type"), first.Header().Get("Content-Type"))
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("waiter response is missing Idempotent-Replay: true")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want exactly 1", got)
	}
}

func TestIdempotencyWaiterCanRetryAfterExplicitTransientRelease(t *testing.T) {
	store := newFakeIdempotencyStore()
	store.getObserved = make(chan struct{}, 1)
	cache := newTestCacheWithOptions(t, PGIdempotencyOptions{
		Store:            store,
		TTL:              time.Hour,
		WaitTimeout:      time.Second,
		WaitPollInterval: time.Millisecond,
	})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Idempotency(cache))
	var calls atomic.Int64
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	router.POST("/retry", func(c *gin.Context) {
		if calls.Add(1) == 1 {
			close(handlerEntered)
			<-releaseHandler
			c.JSON(http.StatusConflict, gin.H{"error": "temporary"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"ok": true})
	})
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/retry", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "non-cacheable-race")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- send() }()
	<-handlerEntered
	waiterDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { waiterDone <- send() }()
	select {
	case <-store.getObserved:
	case <-time.After(time.Second):
		t.Fatal("duplicate request did not enter the wait path")
	}
	close(releaseHandler)
	if first := <-firstDone; first.Code != http.StatusConflict {
		t.Fatalf("first status = %d, want 409", first.Code)
	}
	waiter := <-waiterDone
	assertIdempotencyError(t, waiter, http.StatusTooEarly, ErrCodeIdempotencyInProgress)
	if waiter.Header().Get("Retry-After") == "" {
		t.Fatal("retryable deleted result is missing Retry-After")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls before serial retry = %d, want 1", got)
	}

	retry := send()
	if retry.Code != http.StatusCreated {
		t.Fatalf("serial retry status = %d, want 201", retry.Code)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after serial retry = %d, want 2", got)
	}
}

func TestIdempotencyWaitTimeoutReturnsStable425(t *testing.T) {
	store := newFakeIdempotencyStore()
	now := time.Now()
	if acquired, _, err := store.Acquire(context.Background(), "POST:/todos:busy", "owner-a", now, now.Add(time.Hour)); err != nil || !acquired {
		t.Fatalf("seed Acquire() = acquired %v, error %v", acquired, err)
	}
	cache := newTestCacheWithOptions(t, PGIdempotencyOptions{
		Store:            store,
		TTL:              time.Hour,
		WaitTimeout:      20 * time.Millisecond,
		WaitPollInterval: time.Millisecond,
	})
	router, calls := newRouterWithIdempotency(t, cache)

	req := httptest.NewRequest(http.MethodPost, "/todos", strings.NewReader(`{}`))
	req.Header.Set(IdempotencyHeader, "busy")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assertIdempotencyError(t, rec, http.StatusTooEarly, ErrCodeIdempotencyInProgress)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("425 response is missing Retry-After")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0 while the owner is processing", got)
	}
}

func TestIdempotencyStorageFailuresFailClosed(t *testing.T) {
	storageErr := errors.New("database unavailable")
	t.Run("acquire", func(t *testing.T) {
		store := &faultIdempotencyStore{fakeIdempotencyStore: newFakeIdempotencyStore(), acquireErr: storageErr}
		cache := newTestCacheWithOptions(t, PGIdempotencyOptions{Store: store})
		router, calls := newRouterWithIdempotency(t, cache)
		req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "acquire-failure")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		assertIdempotencyError(t, rec, http.StatusServiceUnavailable, ErrCodeIdempotencyUnavailable)
		if got := calls.Load(); got != 0 {
			t.Fatalf("handler calls = %d, want 0", got)
		}
	})

	t.Run("wait", func(t *testing.T) {
		base := newFakeIdempotencyStore()
		now := time.Now()
		if acquired, _, err := base.Acquire(context.Background(), "POST:/api/links:wait-failure", "owner-a", now, now.Add(time.Hour)); err != nil || !acquired {
			t.Fatalf("seed Acquire() = acquired %v, error %v", acquired, err)
		}
		store := &faultIdempotencyStore{fakeIdempotencyStore: base, getErr: storageErr}
		cache := newTestCacheWithOptions(t, PGIdempotencyOptions{
			Store:            store,
			WaitTimeout:      time.Second,
			WaitPollInterval: time.Millisecond,
		})
		router, calls := newRouterWithIdempotency(t, cache)
		req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "wait-failure")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		assertIdempotencyError(t, rec, http.StatusServiceUnavailable, ErrCodeIdempotencyUnavailable)
		if got := calls.Load(); got != 0 {
			t.Fatalf("handler calls = %d, want 0", got)
		}
	})
}

func TestIdempotencyFinalizeFailureRemainsFailClosedAfterTTL(t *testing.T) {
	base := newFakeIdempotencyStore()
	store := &faultIdempotencyStore{
		fakeIdempotencyStore: base,
		storeErr:             errors.New("store result unknown"),
	}
	var nowNanos atomic.Int64
	nowNanos.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	cache := newTestCacheWithOptions(t, PGIdempotencyOptions{
		Store: store,
		TTL:   time.Minute,
		Now:   clock,
	})
	router, calls := newRouterWithIdempotency(t, cache)

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "unknown-after-finalize")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	first := send()
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}
	before, err := base.Get(context.Background(), "POST:/api/links:unknown-after-finalize")
	if err != nil || before == nil || !before.InFlight {
		t.Fatalf("claim after failed finalize = %#v, error %v; want in-flight evidence", before, err)
	}

	nowNanos.Add(int64(2 * time.Minute))
	if purged, purgeErr := base.PurgeExpired(context.Background(), clock()); purgeErr != nil || purged != 0 {
		t.Fatalf("PurgeExpired() = (%d, %v), want (0, nil) for unknown result", purged, purgeErr)
	}
	second := send()
	assertIdempotencyError(t, second, http.StatusServiceUnavailable, ErrCodeIdempotencyResultUnknown)
	if retryAfter := second.Header().Get("Retry-After"); retryAfter != "" {
		t.Fatalf("unknown-result Retry-After = %q, want empty", retryAfter)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1 after result became unknown", got)
	}
	after, err := base.Get(context.Background(), "POST:/api/links:unknown-after-finalize")
	if err != nil || after == nil {
		t.Fatalf("claim after retry = %#v, error %v", after, err)
	}
	if after.Claim != before.Claim {
		t.Fatalf("claim changed from %+v to %+v; unknown result must not be reclaimed", before.Claim, after.Claim)
	}
	if !after.InFlight {
		t.Fatal("unknown result no longer in-flight; durable fail-closed evidence was lost")
	}
}

func TestIdempotencyTransientReleaseFailureRemainsFailClosedAfterTTL(t *testing.T) {
	base := newFakeIdempotencyStore()
	store := &faultIdempotencyStore{
		fakeIdempotencyStore: base,
		deleteErr:            errors.New("delete result unknown"),
	}
	var nowNanos atomic.Int64
	nowNanos.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	cache := newTestCacheWithOptions(t, PGIdempotencyOptions{
		Store: store,
		TTL:   time.Minute,
		Now:   clock,
	})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Idempotency(cache))
	var calls atomic.Int64
	router.POST("/fail", func(c *gin.Context) {
		calls.Add(1)
		c.JSON(http.StatusConflict, gin.H{"error": "retry later"})
	})

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/fail", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "unknown-after-delete")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if first := send(); first.Code != http.StatusConflict {
		t.Fatalf("first status = %d, want 409", first.Code)
	}
	nowNanos.Add(int64(2 * time.Minute))
	second := send()
	assertIdempotencyError(t, second, http.StatusServiceUnavailable, ErrCodeIdempotencyResultUnknown)
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1 after delete result became unknown", got)
	}
}

func TestIdempotencyLateFinalizeStartsFullReplayTTL(t *testing.T) {
	start := time.Now()
	var nowNanos atomic.Int64
	nowNanos.Store(start.UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	const ttl = time.Minute
	cache, store := newTestCache(t, ttl, clock)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Idempotency(cache))
	var calls atomic.Int64
	router.POST("/slow", func(c *gin.Context) {
		calls.Add(1)
		nowNanos.Add(int64(2 * ttl))
		c.JSON(http.StatusCreated, gin.H{"id": "committed"})
	})

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/slow", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "slow-finalize")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	first := send()
	second := send()
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("statuses = %d/%d, want 201/201", first.Code, second.Code)
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("late finalized response was not replayed")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
	stored, err := store.Get(context.Background(), "POST:/slow:slow-finalize")
	if err != nil || stored == nil {
		t.Fatalf("stored response = %#v, error %v", stored, err)
	}
	wantExpiry := clock().Add(ttl)
	if !stored.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("completion expiry = %s, want %s", stored.ExpiresAt, wantExpiry)
	}
}

func TestIdempotencyWaiterSeesResultBecomeUnknown(t *testing.T) {
	store := newFakeIdempotencyStore()
	store.getObserved = make(chan struct{}, 1)
	start := time.Now()
	var nowNanos atomic.Int64
	nowNanos.Store(start.UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	if acquired, _, err := store.Acquire(context.Background(), "POST:/api/links:expires-while-waiting", "owner-a", start, start.Add(time.Minute)); err != nil || !acquired {
		t.Fatalf("seed Acquire() = acquired %v, error %v", acquired, err)
	}
	cache := newTestCacheWithOptions(t, PGIdempotencyOptions{
		Store:            store,
		TTL:              time.Minute,
		Now:              clock,
		WaitTimeout:      time.Second,
		WaitPollInterval: time.Millisecond,
	})
	router, calls := newRouterWithIdempotency(t, cache)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyHeader, "expires-while-waiting")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		done <- rec
	}()

	select {
	case <-store.getObserved:
	case <-time.After(time.Second):
		t.Fatal("duplicate request did not enter the wait path")
	}
	nowNanos.Add(int64(2 * time.Minute))
	select {
	case rec := <-done:
		assertIdempotencyError(t, rec, http.StatusServiceUnavailable, ErrCodeIdempotencyResultUnknown)
		if rec.Header().Get("Retry-After") != "" {
			t.Fatalf("unknown-result Retry-After = %q, want empty", rec.Header().Get("Retry-After"))
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not return after result became unknown")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0", got)
	}
}

func TestIdempotencyFinalizesAfterClientCancellation(t *testing.T) {
	store := &finalizeContextStore{
		fakeIdempotencyStore: newFakeIdempotencyStore(),
		finalizeContext:      make(chan finalizeContextObservation, 1),
	}
	cache := newTestCacheWithOptions(t, PGIdempotencyOptions{Store: store, FinalizeTimeout: time.Second})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Idempotency(cache))
	var calls atomic.Int64
	var cancelClient context.CancelFunc
	router.POST("/todos", func(c *gin.Context) {
		calls.Add(1)
		c.JSON(http.StatusCreated, gin.H{"id": "todo-1"})
		cancelClient()
	})

	clientCtx, cancel := context.WithCancel(context.Background())
	cancelClient = cancel
	req := httptest.NewRequest(http.MethodPost, "/todos", strings.NewReader(`{}`)).WithContext(clientCtx)
	req.Header.Set(IdempotencyHeader, "cancel-after-commit")
	first := httptest.NewRecorder()
	router.ServeHTTP(first, req)

	observation := <-store.finalizeContext
	if observation.err != nil {
		t.Fatalf("server-owned finalize context error = %v, want nil", observation.err)
	}
	if !observation.hasDeadline {
		t.Fatal("server-owned finalize context has no deadline")
	}

	retry := httptest.NewRequest(http.MethodPost, "/todos", strings.NewReader(`{}`))
	retry.Header.Set(IdempotencyHeader, "cancel-after-commit")
	second := httptest.NewRecorder()
	router.ServeHTTP(second, retry)
	if second.Code != first.Code || second.Body.String() != first.Body.String() {
		t.Fatalf("retry response = %d %q, want replay %d %q", second.Code, second.Body.String(), first.Code, first.Body.String())
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("retry after client cancellation was not replayed")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
}

func TestIdempotencyClaimGenerationRejectsStaleOwnerABA(t *testing.T) {
	store := newFakeIdempotencyStore()
	now := time.Now()
	_, first, err := store.Acquire(context.Background(), "key", "reused-owner", now, now.Add(time.Second))
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	if err := store.Store(context.Background(), "key", first.Claim, http.StatusCreated, []byte("first"), "text/plain", now.Add(time.Second)); err != nil {
		t.Fatalf("first Store() error = %v", err)
	}
	acquired, second, err := store.Acquire(context.Background(), "key", "reused-owner", now.Add(2*time.Second), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("second Acquire() did not reclaim the expired completed response")
	}
	if second.Claim.Generation != first.Claim.Generation+1 {
		t.Fatalf("generation = %d, want %d", second.Claim.Generation, first.Claim.Generation+1)
	}
	if err := store.Store(context.Background(), "key", first.Claim, http.StatusCreated, []byte("stale"), "text/plain", now.Add(time.Hour)); err == nil {
		t.Fatal("stale owner Store() succeeded")
	}
	if err := store.Delete(context.Background(), "key", first.Claim); err == nil {
		t.Fatal("stale owner Delete() succeeded")
	}
	if err := store.Store(context.Background(), "key", second.Claim, http.StatusCreated, []byte("current"), "text/plain", now.Add(time.Hour)); err != nil {
		t.Fatalf("current owner Store() error = %v", err)
	}
	stored, err := store.Get(context.Background(), "key")
	if err != nil || stored == nil || string(stored.Body) != "current" || stored.InFlight {
		t.Fatalf("stored response = %#v, error %v", stored, err)
	}
}

func assertIdempotencyError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	var payload struct {
		Error struct {
			ErrorCode string `json:"error_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if payload.Error.ErrorCode != wantCode {
		t.Fatalf("error_code = %q, want %q", payload.Error.ErrorCode, wantCode)
	}
}
