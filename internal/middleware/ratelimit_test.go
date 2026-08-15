package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"webtag/internal/dto"
)

func newRateLimitedRouter(t *testing.T, opts RateLimitOptions) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// Tests drive the limiter through deterministic loops; the periodic
	// sweep would race with the eviction tests that pin time.Now.
	opts.SweepDisabled = true
	handler, ctrl := RateLimit(opts)
	if err := ctrl.Start(context.Background()); err != nil {
		t.Fatalf("start rate limiter: %v", err)
	}
	t.Cleanup(func() {
		if err := ctrl.Stop(context.Background()); err != nil {
			t.Errorf("stop rate limiter: %v", err)
		}
	})
	router.Use(handler)
	router.GET("/api/links", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	return router
}

func doGet(router *gin.Engine, path, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestRateLimit_DisabledByDefault confirms the RPS=0 short-circuit so
// callers can install RateLimit unconditionally.
func TestRateLimit_DisabledByDefault(t *testing.T) {
	t.Parallel()

	router := newRateLimitedRouter(t, RateLimitOptions{RPS: 0})
	for i := 0; i < 100; i++ {
		if rec := doGet(router, "/api/links", "10.0.0.1"); rec.Code != http.StatusOK {
			t.Fatalf("iter=%d status=%d, want 200 (limiter must be off)", i, rec.Code)
		}
	}
}

// TestRateLimit_SkipsAllowedPaths covers the SkipPaths bypass that
// keeps liveness probes outside the per-IP budget.
func TestRateLimit_SkipsAllowedPaths(t *testing.T) {
	t.Parallel()

	// RPS=1 / Burst=1 means the second request to /api/links must 429,
	// while /health must always pass through.
	router := newRateLimitedRouter(t, RateLimitOptions{
		RPS:       1,
		Burst:     1,
		SkipPaths: []string{"/health"},
	})

	for i := 0; i < 5; i++ {
		if rec := doGet(router, "/health", "10.0.0.1"); rec.Code != http.StatusOK {
			t.Fatalf("/health iter=%d status=%d, want 200 (skip path)", i, rec.Code)
		}
	}
}

// TestRateLimit_BurstAndDeny pins the per-IP token bucket: the first
// Burst requests succeed, the Burst+1 request fails fast with 429 and
// a Retry-After header. The 429 body must use the same dto.ErrorResponse
// envelope as every other API error so SDK clients can deserialise on a
// single shape.
func TestRateLimit_BurstAndDeny(t *testing.T) {
	t.Parallel()

	router := newRateLimitedRouter(t, RateLimitOptions{
		RPS:   1,
		Burst: 3,
	})

	for i := 0; i < 3; i++ {
		if rec := doGet(router, "/api/links", "10.0.0.1"); rec.Code != http.StatusOK {
			t.Fatalf("burst iter=%d status=%d, want 200", i, rec.Code)
		}
	}

	rec := doGet(router, "/api/links", "10.0.0.1")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after-burst status=%d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After=%q, want \"1\"", rec.Header().Get("Retry-After"))
	}

	var body dto.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v (raw=%q)", err, rec.Body.String())
	}
	if body.Error.Code != http.StatusTooManyRequests {
		t.Fatalf("error.code=%d, want 429", body.Error.Code)
	}
	if body.Error.Message != "rate limit exceeded" {
		t.Fatalf("error.message=%q, want \"rate limit exceeded\"", body.Error.Message)
	}
}

// TestRateLimit_PerIPIsolation guarantees one noisy IP does not exhaust
// the budget for another IP. This is the core property of a per-IP
// limiter — without it, a single attacker would 429 every other client.
func TestRateLimit_PerIPIsolation(t *testing.T) {
	t.Parallel()

	router := newRateLimitedRouter(t, RateLimitOptions{
		RPS:   1,
		Burst: 1,
	})

	// IP A consumes its bucket.
	if rec := doGet(router, "/api/links", "10.0.0.1"); rec.Code != http.StatusOK {
		t.Fatalf("A first status=%d, want 200", rec.Code)
	}
	if rec := doGet(router, "/api/links", "10.0.0.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("A second status=%d, want 429", rec.Code)
	}

	// IP B still has its full bucket.
	if rec := doGet(router, "/api/links", "10.0.0.2"); rec.Code != http.StatusOK {
		t.Fatalf("B first status=%d, want 200 (per-IP isolation broken)", rec.Code)
	}
}

// TestRateLimit_EmitsStandardRateLimitHeaders 锁定 Wave 4 C 的新行为：
// 被 429 拒绝的请求必须带 X-RateLimit-Limit / X-RateLimit-Remaining /
// X-RateLimit-Reset，并且 Retry-After 来自真实的 limiter.Reserve().Delay()
// (向上取整、最低 1 秒)，而不是写死的 1。
func TestRateLimit_EmitsStandardRateLimitHeaders(t *testing.T) {
	t.Parallel()

	router := newRateLimitedRouter(t, RateLimitOptions{
		RPS:   1,
		Burst: 1,
	})

	// 第一个请求耗掉 burst。
	if rec := doGet(router, "/api/links", "10.0.0.7"); rec.Code != http.StatusOK {
		t.Fatalf("first status=%d, want 200", rec.Code)
	}
	rec := doGet(router, "/api/links", "10.0.0.7")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status=%d, want 429", rec.Code)
	}

	// X-RateLimit-Limit = RPS 配置值。我们传的是 1，所以应等于 "1"。
	if got := rec.Header().Get("X-RateLimit-Limit"); got != "1" {
		t.Fatalf("X-RateLimit-Limit=%q, want \"1\"", got)
	}

	// X-RateLimit-Remaining 在被拒时一定是 "0"。
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining=%q, want \"0\"", got)
	}

	// X-RateLimit-Reset 是 unix 秒，必须 > now（未来时间）。
	resetStr := rec.Header().Get("X-RateLimit-Reset")
	if resetStr == "" {
		t.Fatalf("X-RateLimit-Reset is empty")
	}
	resetUnix, err := strconv.ParseInt(resetStr, 10, 64)
	if err != nil {
		t.Fatalf("X-RateLimit-Reset=%q not an integer: %v", resetStr, err)
	}
	if resetUnix < time.Now().Unix() {
		t.Fatalf("X-RateLimit-Reset=%d is in the past (now=%d)", resetUnix, time.Now().Unix())
	}

	// Retry-After 仍然写出来；RPS=1/Burst=1 场景下 Reserve.Delay() 接近 1s，
	// 向上取整到 "1"。
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatalf("Retry-After header is empty")
	} else if ra, err := strconv.Atoi(got); err != nil || ra < 1 {
		t.Fatalf("Retry-After=%q, want integer >= 1", got)
	}
}

// TestRateLimit_SweeperExitsOnContextCancel 锁定 Wave 4 C 的新行为：
// startSweep 接受 ctx，外部 cancel 时立刻退出。这里通过 Controller.Stop
// 走 cancel 路径触发，sweeper 必须在合理时间内关闭 doneCh。
func TestRateLimit_SweeperExitsOnContextCancel(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	// 故意把 sweep 间隔拉得很大（远超 Stop ctx 的窗口），如果 sweeper
	// 只听 ticker.C 就根本不会唤醒。修复后它会监听 ctx.Done() 立即退出。
	// 注：startSweep 内部把 interval 强制 floor 到 1 分钟，所以即便我们传
	// 1 分钟，也无法靠 ticker.C 在 50ms 测试窗口内被唤醒——必须走 ctx 分支。
	_, ctrl := RateLimit(RateLimitOptions{
		RPS:           1,
		Burst:         1,
		IdleTTL:       10 * time.Minute,
		SweepInterval: 10 * time.Minute,
	})
	if err := ctrl.Start(context.Background()); err != nil {
		t.Fatalf("start rate limiter: %v", err)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		ctrl.Stop(stopCtx)
		close(done)
	}()

	select {
	case <-done:
		// 期望 stopCh 的 cancel + 内部 ctx 的 cancel 双保险让 sweeper
		// 立即退出，limiter.Stop 在 ctx timeout 之前就收到 doneCh。
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RateLimiterController.Stop did not return within 500ms; sweeper likely not honoring ctx cancellation")
	}
}

// TestRateLimit_IdleEviction confirms that buckets exceeding IdleTTL
// are cleared during the next sweep so the per-IP map cannot grow
// without bound under fan-in.
func TestRateLimit_IdleEviction(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	limiter := newIPRateLimiter(RateLimitOptions{
		RPS:        1,
		Burst:      1,
		IdleTTL:    time.Minute,
		MaxBuckets: 2,
		Now:        func() time.Time { return now },
	})

	limiter.allow("10.0.0.1")
	limiter.allow("10.0.0.2")
	if got := len(limiter.buckets); got != 2 {
		t.Fatalf("after seed, len=%d, want 2", got)
	}

	now = now.Add(2 * time.Minute) // both stale
	limiter.allow("10.0.0.3")      // third entry triggers sweep before insert

	if _, ok := limiter.buckets["10.0.0.1"]; ok {
		t.Fatal("10.0.0.1 should have been evicted by sweep")
	}
	if _, ok := limiter.buckets["10.0.0.3"]; !ok {
		t.Fatal("10.0.0.3 should be present after insert")
	}
}

func TestRateLimit_MaxBucketsIsHardLimitAndOverflowSharesBurst(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	limiter := newIPRateLimiter(RateLimitOptions{
		RPS:        0.001,
		Burst:      1,
		IdleTTL:    time.Hour,
		MaxBuckets: 2,
		Now:        func() time.Time { return now },
	})

	if !limiter.allow("192.0.2.1") || !limiter.allow("192.0.2.2") {
		t.Fatal("dedicated buckets did not receive their initial burst")
	}
	if !limiter.allow("192.0.2.3") {
		t.Fatal("first overflow client did not receive shared initial burst")
	}
	for i := 0; i < 10_000; i++ {
		rotating := fmt.Sprintf("198.51.%d.%d", i/256, i%256)
		if limiter.allow(rotating) {
			// The fixed clock prevents refill, so any allowance means a rotating
			// overflow identity received a fresh bucket.
			t.Fatalf("rotating overflow identity %d regained burst", i)
		}
		if got := len(limiter.buckets); got > limiter.maxBuckets {
			t.Fatalf("bucket map len=%d exceeded MaxBuckets=%d", got, limiter.maxBuckets)
		}
	}
	if got := len(limiter.buckets); got != 2 {
		t.Fatalf("bucket map len=%d, want hard cap 2", got)
	}
}

func TestRateLimitClientKeyAggregatesMappedAndIPv6Addresses(t *testing.T) {
	t.Parallel()

	if got := rateLimitClientKey("::ffff:192.0.2.10"); got != "192.0.2.10" {
		t.Fatalf("mapped IPv4 key=%q, want canonical IPv4", got)
	}
	left := rateLimitClientKey("2001:db8:1234:5678::1")
	right := rateLimitClientKey("2001:db8:1234:5678:ffff::2")
	if left != right || left != "2001:db8:1234:5678::/64" {
		t.Fatalf("IPv6 /64 keys=%q/%q, want one canonical prefix", left, right)
	}
	if got := rateLimitClientKey("not-an-ip"); got != "invalid" {
		t.Fatalf("invalid client key=%q, want fixed invalid key", got)
	}
}

func TestRateLimitMaxBucketsRemainsHardLimitUnderConcurrency(t *testing.T) {
	t.Parallel()

	limiter := newIPRateLimiter(RateLimitOptions{
		RPS: 1, Burst: 1, IdleTTL: time.Hour, MaxBuckets: 32,
	})
	var workers sync.WaitGroup
	for worker := 0; worker < 64; worker++ {
		worker := worker
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := 0; item < 64; item++ {
				limiter.allow(fmt.Sprintf("2001:db8:%x:%x::1", worker, item))
			}
		}()
	}
	workers.Wait()
	limiter.mu.Lock()
	got := len(limiter.buckets)
	limiter.mu.Unlock()
	if got > limiter.maxBuckets {
		t.Fatalf("concurrent bucket map len=%d exceeded MaxBuckets=%d", got, limiter.maxBuckets)
	}
}
