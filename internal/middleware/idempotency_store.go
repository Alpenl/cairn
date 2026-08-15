package middleware

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"webtag/internal/observability"
)

// IdempotencyClaim uniquely identifies one processing generation. Completion
// and release operations must match both fields so a delayed owner can never
// overwrite or delete a claim that has since been reacquired.
type IdempotencyClaim struct {
	OwnerToken string
	Generation int64
}

// StoredResponse is the persisted idempotent response the middleware replays
// on a cache hit. It mirrors the columns of the idempotency_keys table but is
// declared here (not imported from repository) to keep the middleware layer
// free of a repository dependency — the wiring layer adapts the repo to
// IdempotencyStore.
type StoredResponse struct {
	Claim       IdempotencyClaim
	Status      int
	Body        []byte
	ContentType string
	// InFlight is true while the owner is still running its handler or
	// finalizing the response. A concurrent caller waits for completion and
	// never enters the handler for the same generation. Once an in-flight
	// claim expires, its business result is unknown and the claim is retained
	// indefinitely so the same key cannot execute again.
	InFlight bool
	// ExpiresAt is the processing deadline for an in-flight claim and the
	// replay-retention cutoff for a completed response.
	ExpiresAt time.Time
}

// IdempotencyStore is the persistence contract the PG-backed idempotency cache
// needs (Phase 13 / v4.0 M2). Acquire atomically creates or reclaims an expired
// completed generation, but never reclaims an in-flight result; Get powers
// bounded duplicate waits; Store/Delete must match the returned
// owner/generation; PurgeExpired removes only expired completed responses.
//
// 多副本一致性由 PG 主键和 owner/generation CAS 保证：副本 A 抢到 key，
// 副本 B 命中 live processing 时只能等待/回放；延迟的旧 owner 不能覆盖新代。
type IdempotencyStore interface {
	Acquire(ctx context.Context, key, ownerToken string, now, expiresAt time.Time) (bool, *StoredResponse, error)
	Get(ctx context.Context, key string) (*StoredResponse, error)
	Store(ctx context.Context, key string, claim IdempotencyClaim, status int, body []byte, contentType string, expiresAt time.Time) error
	Delete(ctx context.Context, key string, claim IdempotencyClaim) error
	PurgeExpired(ctx context.Context, now time.Time) (int64, error)
}

// idempotencyPurgeInterval 是 TTL 清理后台任务的轮询间隔。沿用旧 LRU 清扫的
// 5min 折中：够频繁能把已完成且过期的响应及时清掉，又不至于把 DB 写在空扫
// 上。结果未知的 in-flight 行不参与清理。
const (
	idempotencyPurgeInterval = 5 * time.Minute

	// DefaultIdempotencyWaitTimeout bounds how long a duplicate request waits
	// for the current owner before receiving a retryable 425 response.
	DefaultIdempotencyWaitTimeout = time.Second
	// DefaultIdempotencyWaitPollInterval keeps duplicate-request polling
	// responsive without turning one in-flight claim into a database hot loop.
	DefaultIdempotencyWaitPollInterval = 25 * time.Millisecond
	// DefaultIdempotencyFinalizeTimeout bounds server-owned Store/Delete work
	// after the client request context has been cancelled.
	DefaultIdempotencyFinalizeTimeout = 5 * time.Second
)

var (
	// ErrPGIdempotencyStoreRequired reports an invalid cache configuration.
	ErrPGIdempotencyStoreRequired = errors.New("pg idempotency cache store is required")
	// ErrPGIdempotencyAlreadyStarted reports a repeated Start call.
	ErrPGIdempotencyAlreadyStarted = errors.New("pg idempotency cache already started")
	// ErrPGIdempotencyStopped reports an attempt to restart a stopped cache.
	ErrPGIdempotencyStopped = errors.New("pg idempotency cache stopped")
)

type pgIdempotencyState uint8

const (
	pgIdempotencyConstructed pgIdempotencyState = iota
	pgIdempotencyStarted
	pgIdempotencyStopped
)

type idempotencyPurgeTicker interface {
	C() <-chan time.Time
	Stop()
}

type realIdempotencyPurgeTicker struct {
	*time.Ticker
}

func (t realIdempotencyPurgeTicker) C() <-chan time.Time { return t.Ticker.C }

// PGIdempotencyCache 是基于 IdempotencyStore 的幂等缓存：把抢占 / 回放 / 录制
// 委托给 PG store，并跑一个后台 goroutine 周期清理过期键（替代旧 LRU 的
// sweepLoop）。构造函数只分配资源；后台清理的所有权由 Start 传入的 context
// 定义，Stop 使用调用方的 shutdown context 有界等待清理退出。
type PGIdempotencyCache struct {
	store            IdempotencyStore
	ttl              time.Duration
	logger           *slog.Logger
	waitTimeout      time.Duration
	waitPollInterval time.Duration
	finalizeTimeout  time.Duration

	// now 允许测试注入 fake clock；生产用 time.Now。所有 TTL 判定经由 now()。
	now           func() time.Time
	newOwnerToken func() string

	newTicker func(time.Duration) idempotencyPurgeTicker

	lifecycleMu sync.Mutex
	state       pgIdempotencyState
	runCancel   context.CancelFunc
	stop        chan struct{}
	done        chan struct{}
	doneOnce    sync.Once
}

// PGIdempotencyOptions 收拢 PGIdempotencyCache 的可注入参数。
type PGIdempotencyOptions struct {
	Store            IdempotencyStore
	TTL              time.Duration
	WaitTimeout      time.Duration
	WaitPollInterval time.Duration
	FinalizeTimeout  time.Duration
	Logger           *slog.Logger
	Now              func() time.Time

	// newTicker is an internal seam for deterministic lifecycle tests.
	newTicker func(time.Duration) idempotencyPurgeTicker
	// newOwnerToken is an internal seam for deterministic claim tests.
	newOwnerToken func() string
}

// NewPGIdempotencyCache 只构造 PG 幂等缓存，不启动后台 goroutine。Store 必填；
// TTL<=0 时用 DefaultIdempotencyTTL；Now==nil 用 time.Now。调用方在 Runtime
// 启动/停机时分别调用 Start/Stop。
func NewPGIdempotencyCache(opts PGIdempotencyOptions) (*PGIdempotencyCache, error) {
	if opts.Store == nil {
		return nil, ErrPGIdempotencyStoreRequired
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultIdempotencyTTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	waitTimeout := opts.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = DefaultIdempotencyWaitTimeout
	}
	waitPollInterval := opts.WaitPollInterval
	if waitPollInterval <= 0 {
		waitPollInterval = DefaultIdempotencyWaitPollInterval
	}
	finalizeTimeout := opts.FinalizeTimeout
	if finalizeTimeout <= 0 {
		finalizeTimeout = DefaultIdempotencyFinalizeTimeout
	}
	newOwnerToken := opts.newOwnerToken
	if newOwnerToken == nil {
		newOwnerToken = uuid.NewString
	}
	newTicker := opts.newTicker
	if newTicker == nil {
		newTicker = func(interval time.Duration) idempotencyPurgeTicker {
			return realIdempotencyPurgeTicker{Ticker: time.NewTicker(interval)}
		}
	}
	c := &PGIdempotencyCache{
		store:            opts.Store,
		ttl:              ttl,
		waitTimeout:      waitTimeout,
		waitPollInterval: waitPollInterval,
		finalizeTimeout:  finalizeTimeout,
		logger:           opts.Logger,
		now:              now,
		newOwnerToken:    newOwnerToken,
		newTicker:        newTicker,
		stop:             make(chan struct{}),
		done:             make(chan struct{}),
	}
	return c, nil
}

// Start 启动唯一的 TTL 清理 goroutine。已取消的 owner context 不会启动；成功
// 启动后不能重复 Start，Stop 后也不能重启。
func (c *PGIdempotencyCache) Start(ctx context.Context) error {
	if c == nil {
		return nil
	}

	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	switch c.state {
	case pgIdempotencyStarted:
		return ErrPGIdempotencyAlreadyStarted
	case pgIdempotencyStopped:
		return ErrPGIdempotencyStopped
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	runCtx, runCancel := context.WithCancel(ctx)
	ticker := c.newTicker(idempotencyPurgeInterval)
	c.runCancel = runCancel
	c.state = pgIdempotencyStarted
	go c.purgeLoop(runCtx, runCancel, ticker)
	return nil
}

// Stop 请求后台清理退出并等待完成。空闲 loop 会立即退出；若 PurgeExpired 正在
// 执行，则允许它在 ctx deadline 前完成，deadline/cancellation 到达时取消 owner
// context。幂等，多次调用共享同一个 done barrier。
func (c *PGIdempotencyCache) Stop(ctx context.Context) error {
	if c == nil {
		return nil
	}

	c.lifecycleMu.Lock()
	runCancel := c.runCancel
	switch c.state {
	case pgIdempotencyConstructed:
		c.state = pgIdempotencyStopped
		close(c.stop)
		c.doneOnce.Do(func() { close(c.done) })
		c.lifecycleMu.Unlock()
		return nil
	case pgIdempotencyStarted:
		c.state = pgIdempotencyStopped
		close(c.stop)
	}
	c.lifecycleMu.Unlock()

	select {
	case <-c.done:
		return nil
	default:
	}

	stopCancellation := context.AfterFunc(ctx, runCancel)
	defer stopCancellation()

	select {
	case <-c.done:
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		runCancel()
		// IdempotencyStore implementations must honor their context. Joining here
		// guarantees Stop never leaves an unmanaged purge goroutine behind.
		<-c.done
		return ctx.Err()
	}
}

// purgeLoop 周期调 store.PurgeExpired 清掉已完成且过期的响应。结果未知的
// in-flight 行由 store 永久保留。owner cancellation 或 Stop signal 都会退出；
// ticker 的创建和 goroutine 本身都只发生在 Start 之后。
func (c *PGIdempotencyCache) purgeLoop(ctx context.Context, runCancel context.CancelFunc, ticker idempotencyPurgeTicker) {
	defer func() {
		ticker.Stop()
		runCancel()
		c.lifecycleMu.Lock()
		c.state = pgIdempotencyStopped
		c.lifecycleMu.Unlock()
		c.doneOnce.Do(func() { close(c.done) })
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stop:
			return
		case <-ticker.C():
			if _, err := c.store.PurgeExpired(ctx, c.now()); err != nil && ctx.Err() == nil && c.logger != nil {
				c.logger.Warn("idempotency purge expired keys failed", "error", observability.SafeError(err))
			}
		}
	}
}
