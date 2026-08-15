package middleware

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// RateLimitOptions tunes the per-IP token bucket installed by
// RateLimit. RPS <= 0 short-circuits the middleware to a no-op so the
// caller can stage rollouts via env-var flips without redeploying;
// production configurations should set both RPS and Burst.
//
// SkipPaths is a small allow-list (typically /health, /ready, /metrics)
// that bypasses the limiter entirely. We exact-match against the
// request URL path because the allow-list is small and well-known —
// prefix matching invites accidental over-skipping.
//
// IdleTTL bounds the per-IP map: an entry untouched for IdleTTL is
// dropped by the background sweep loop (interval = IdleTTL/4, floored
// at 1 minute) and opportunistically when MaxBuckets fills. The default
// (10 minutes) keeps the map small under normal traffic while still
// giving long-tail clients a coherent budget.
//
// SweepInterval overrides the derived sweep cadence; tests pin it to a
// fixed duration. SweepDisabled lets tests skip the background goroutine
// when they only exercise the cold-path eviction.
//
// Logger 是可选项，用于让 sweep goroutine 在被外部 ctx cancel 时落一行
// 告警。生产 wire 时建议传入 app 级 slog；测试和 RPS<=0 短路场景留 nil
// 即可（沿用 nil 等价 io.Discard 的惯例）。
type RateLimitOptions struct {
	RPS           float64
	Burst         int
	IdleTTL       time.Duration
	MaxBuckets    int
	SkipPaths     []string
	Now           func() time.Time
	SweepInterval time.Duration
	SweepDisabled bool
	Logger        *slog.Logger
}

type rateBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type ipRateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*rateBucket
	rps        rate.Limit
	burst      int
	idleTTL    time.Duration
	maxBuckets int
	overflow   *rateBucket
	nextSweep  time.Time
	now        func() time.Time
	logger     *slog.Logger

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func newIPRateLimiter(opts RateLimitOptions) *ipRateLimiter {
	if opts.IdleTTL <= 0 {
		opts.IdleTTL = 10 * time.Minute
	}
	if opts.MaxBuckets <= 0 {
		opts.MaxBuckets = 4096
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &ipRateLimiter{
		buckets:    make(map[string]*rateBucket),
		rps:        rate.Limit(opts.RPS),
		burst:      opts.Burst,
		idleTTL:    opts.IdleTTL,
		maxBuckets: opts.MaxBuckets,
		overflow:   &rateBucket{limiter: rate.NewLimiter(rate.Limit(opts.RPS), opts.Burst)},
		now:        opts.Now,
		logger:     opts.Logger,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// startSweep launches the background TTL sweep. interval is clamped to
// at least one minute so a misconfigured IdleTTL cannot turn into a busy
// loop. The goroutine exits when stopCh is closed or ctx is cancelled,
// signalling doneCh either way.
//
// WHY ctx: 此前 sweeper 只监听 r.stopCh，依赖 RateLimiterController.Stop
// 主动关停。如果配置中 SweepInterval 远大于 Stop 自己的 ctx timeout，
// Stop 会等不到 doneCh 退出，进程关停时只能丢这个 goroutine 给 runtime。
// 把 ctx 接进来后，外部传入的 lifecycle ctx 一旦 Done，sweeper 立刻收尾，
// 同时 logger 警告一行让运维知道 sweep 是被强制收的，而不是自然到点退出。
func (r *ipRateLimiter) startSweep(ctx context.Context, interval time.Duration) {
	if interval < time.Minute {
		interval = time.Minute
	}
	go func() {
		defer close(r.doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stopCh:
				return
			case <-ctx.Done():
				// 外部 ctx 主动 cancel：通常意味着进程在 graceful shutdown
				// 中断（信号到达 / 总超时到点），用 Warn 让日志能区分
				// "正常 Stop" 与 "ctx-driven 提前收"。
				if r.logger != nil {
					r.logger.Warn("ratelimit sweeper terminated by ctx cancellation",
						"err", ctx.Err().Error(),
					)
				}
				return
			case <-ticker.C:
				now := r.now()
				r.mu.Lock()
				r.sweepLocked(now)
				r.mu.Unlock()
			}
		}
	}()
}

// Stop terminates the background sweep goroutine and waits for it to
// exit. Idempotent.
func (r *ipRateLimiter) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() { close(r.stopCh) })
	if r.doneCh == nil {
		return nil
	}
	select {
	case <-r.doneCh:
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// allow consults (and lazily creates) the per-IP bucket. The lock is
// held across map mutation and Allow() because *rate.Limiter is itself
// safe for concurrent use, but the map insert/sweep must be serialised
// — a finer-grained sync.Map would buy little under realistic IP
// cardinalities and complicate eviction.
func (r *ipRateLimiter) allow(ip string) bool {
	now := r.now()
	ip = rateLimitClientKey(ip)
	r.mu.Lock()
	defer r.mu.Unlock()

	bucket, ok := r.buckets[ip]
	if !ok {
		if len(r.buckets) >= r.maxBuckets && !now.Before(r.nextSweep) {
			r.sweepLocked(now)
			sweepInterval := r.idleTTL / 4
			if sweepInterval <= 0 {
				sweepInterval = time.Minute
			}
			r.nextSweep = now.Add(sweepInterval)
		}
		if len(r.buckets) >= r.maxBuckets {
			// All dedicated buckets are active. New identities share one fixed
			// overflow budget: the map remains capped, rotating source addresses
			// cannot regain burst, and the full path stays O(1) between sweeps.
			bucket = r.overflow
		} else {
			bucket = &rateBucket{limiter: rate.NewLimiter(r.rps, r.burst)}
			r.buckets[ip] = bucket
		}
	}
	bucket.lastSeen = now
	return bucket.limiter.Allow()
}

// reserveDelay 在被拒后估算 "下一次能放行还要等多久"。
//
// 用 *rate.Limiter.Reserve() 比手算 1/RPS 准——它考虑了当前桶的真实剩余
// 状态（刚被打空 vs. 即将续 token）。我们立刻 Cancel() 该 Reservation，
// 不真正把它消费掉，免得这条 429 请求又占了下一个 token 的位置。
//
// 调用方持有 r.mu，所以可以安全 touch bucket.
func (r *ipRateLimiter) reserveDelay(ip string) time.Duration {
	ip = rateLimitClientKey(ip)
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket, ok := r.buckets[ip]
	if !ok {
		bucket = r.overflow
	}
	if bucket == nil || bucket.limiter == nil {
		return 0
	}
	res := bucket.limiter.Reserve()
	if !res.OK() {
		return 0
	}
	d := res.Delay()
	// Cancel 把刚才 reserve 的 token 退回去：429 响应不应"扣"客户端配额。
	res.Cancel()
	return d
}

// rateLimitClientKey prevents address spelling from becoming cardinality.
// IPv4-mapped IPv6 is the same client as IPv4, IPv6 is budgeted per /64, and
// malformed sources share one fixed key.
func rateLimitClientKey(raw string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return "invalid"
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	addr = addr.WithZone("")
	return netip.PrefixFrom(addr, 64).Masked().String()
}

func (r *ipRateLimiter) sweepLocked(now time.Time) {
	for ip, bucket := range r.buckets {
		if now.Sub(bucket.lastSeen) >= r.idleTTL {
			delete(r.buckets, ip)
		}
	}
}

// RateLimit returns a gin middleware that drops requests above the
// configured per-IP token rate with HTTP 429 + Retry-After: 1. When
// RPS <= 0 the returned middleware is a no-op so callers can bind it
// unconditionally and let RATE_LIMIT_RPS=0 disable enforcement.
//
// The returned controller is constructor-only. Production wiring calls Start
// after acquisition succeeds and Stop during graceful shutdown. When
// SweepDisabled is set, Start/Stop still enforce the linear lifecycle but do
// not spawn a goroutine, matching fixtures that drive eviction manually.
func RateLimit(opts RateLimitOptions) (gin.HandlerFunc, *RateLimiterController) {
	return buildRateLimit(opts)
}

func buildRateLimit(opts RateLimitOptions) (gin.HandlerFunc, *RateLimiterController) {
	if opts.RPS <= 0 {
		// 即便走短路也返回一个 controller 占位，Stop 是 no-op。和原行为一致。
		return func(c *gin.Context) { c.Next() }, &RateLimiterController{}
	}
	if opts.Burst <= 0 {
		opts.Burst = int(opts.RPS)
		if opts.Burst < 1 {
			opts.Burst = 1
		}
	}

	skip := make(map[string]struct{}, len(opts.SkipPaths))
	for _, path := range opts.SkipPaths {
		skip[path] = struct{}{}
	}

	limiter := newIPRateLimiter(opts)
	interval := opts.SweepInterval
	if interval <= 0 {
		interval = limiter.idleTTL / 4
	}

	rpsStr := strconv.FormatFloat(opts.RPS, 'f', -1, 64)

	handler := func(c *gin.Context) {
		if _, skipPath := skip[c.Request.URL.Path]; skipPath {
			c.Next()
			return
		}

		ip := c.ClientIP()
		if !limiter.allow(ip) {
			// 用 Reserve().Delay() 计算真实的可用时间，向上取整到秒。
			// 客户端如果按这个值退避就不会再撞下一个 429。最低 1 秒，
			// 避免我们把"立刻就能重试"的暗示发出去——HTTP 语义里
			// Retry-After 是 *至少* 等多久。
			delay := limiter.reserveDelay(ip)
			retryAfterSec := int(math.Ceil(delay.Seconds()))
			if retryAfterSec < 1 {
				retryAfterSec = 1
			}
			now := time.Now()
			resetTs := now.Add(time.Duration(retryAfterSec) * time.Second).Unix()

			c.Header("Retry-After", strconv.Itoa(retryAfterSec))
			// 标准化的限流头部：客户端 SDK / 仪表盘可读取这三项做退避和
			// 可视化。X-RateLimit-Limit 是配置的 RPS；
			// X-RateLimit-Remaining 在被拒时一定是 0；
			// X-RateLimit-Reset 是 (粗略) 桶下次可用的 Unix 秒。
			c.Header("X-RateLimit-Limit", rpsStr)
			c.Header("X-RateLimit-Remaining", "0")
			c.Header("X-RateLimit-Reset", strconv.FormatInt(resetTs, 10))
			JSONErrorWithSlug(c, http.StatusTooManyRequests, ErrCodeRateLimitExceeded, "rate limit exceeded")
			return
		}
		c.Next()
	}
	return handler, &RateLimiterController{
		limiter:       limiter,
		sweepDisabled: opts.SweepDisabled,
		sweepInterval: interval,
	}
}

var (
	// ErrRateLimiterAlreadyStarted reports a repeated Start call.
	ErrRateLimiterAlreadyStarted = errors.New("rate limiter already started")
	// ErrRateLimiterStopped reports an attempt to restart a stopped controller.
	ErrRateLimiterStopped = errors.New("rate limiter stopped")
)

type rateLimiterState uint8

const (
	rateLimiterConstructed rateLimiterState = iota
	rateLimiterStarting
	rateLimiterStarted
	rateLimiterStopping
	rateLimiterStopped
)

// RateLimiterController owns the lifecycle of the limiter's background
// sweep. Construction never starts a goroutine; Runtime calls Start after all
// dependencies have been acquired, then Stop during graceful shutdown.
type RateLimiterController struct {
	limiter       *ipRateLimiter
	sweepDisabled bool
	sweepInterval time.Duration
	sweeperCancel context.CancelFunc

	lifecycleMu sync.Mutex
	state       rateLimiterState
	startDone   chan struct{}
	stopDone    chan struct{}
	stopErr     error
}

// Start launches the sweeper exactly once. A context that is already canceled
// leaves the controller constructed so the caller may retry with its runtime
// owner context.
func (c *RateLimiterController) Start(ctx context.Context) error {
	if c == nil {
		return nil
	}

	c.lifecycleMu.Lock()
	switch c.state {
	case rateLimiterStarting, rateLimiterStarted:
		c.lifecycleMu.Unlock()
		return ErrRateLimiterAlreadyStarted
	case rateLimiterStopping, rateLimiterStopped:
		c.lifecycleMu.Unlock()
		return ErrRateLimiterStopped
	}
	if err := ctx.Err(); err != nil {
		c.lifecycleMu.Unlock()
		return err
	}
	c.state = rateLimiterStarting
	c.startDone = make(chan struct{})
	c.lifecycleMu.Unlock()

	c.start(ctx)

	c.lifecycleMu.Lock()
	c.state = rateLimiterStarted
	close(c.startDone)
	c.lifecycleMu.Unlock()
	return nil
}

func (c *RateLimiterController) start(ctx context.Context) {
	if c.limiter == nil || c.sweepDisabled {
		return
	}
	runCtx, runCancel := context.WithCancel(ctx)
	c.sweeperCancel = runCancel
	c.limiter.startSweep(runCtx, c.sweepInterval)
}

// Stop signals owned sweepers to exit and waits up to ctx for completion.
// Stop-before-Start and repeated Stop calls are idempotent.
func (c *RateLimiterController) Stop(ctx context.Context) error {
	if c == nil {
		return nil
	}

	for {
		c.lifecycleMu.Lock()
		switch c.state {
		case rateLimiterConstructed:
			c.state = rateLimiterStopped
			c.lifecycleMu.Unlock()
			return nil
		case rateLimiterStarting:
			startDone := c.startDone
			c.lifecycleMu.Unlock()
			select {
			case <-startDone:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		case rateLimiterStarted:
			c.state = rateLimiterStopping
			c.stopDone = make(chan struct{})
			c.lifecycleMu.Unlock()

			err := c.stop(ctx)

			c.lifecycleMu.Lock()
			c.stopErr = err
			c.state = rateLimiterStopped
			close(c.stopDone)
			c.lifecycleMu.Unlock()
			return err
		case rateLimiterStopping:
			stopDone := c.stopDone
			c.lifecycleMu.Unlock()
			select {
			case <-stopDone:
				c.lifecycleMu.Lock()
				err := c.stopErr
				c.lifecycleMu.Unlock()
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		case rateLimiterStopped:
			err := c.stopErr
			c.lifecycleMu.Unlock()
			return err
		}
	}
}

func (c *RateLimiterController) stop(ctx context.Context) error {
	if c.sweeperCancel != nil {
		c.sweeperCancel()
	}
	if c.limiter == nil || c.sweepDisabled {
		return nil
	}
	return c.limiter.Stop(ctx)
}
