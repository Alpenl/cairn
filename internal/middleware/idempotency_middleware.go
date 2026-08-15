package middleware

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/observability"
)

// idempotentMethods 是中间件介入的 HTTP method 集合。RFC 7231 把 GET /
// HEAD / OPTIONS 视作天然幂等，重试它们不会产生新资源；这里聚焦在
// "写"类方法 —— POST / PUT / PATCH / DELETE —— 客户端层面没有任何
// 内建的去重保证，是真正会被网络抖动重试坑出重复资源的方法。
var idempotentMethods = map[string]struct{}{
	http.MethodPost:   {},
	http.MethodPut:    {},
	http.MethodPatch:  {},
	http.MethodDelete: {},
}

// Idempotency 返回一个 Gin 中间件：当请求方法是 POST/PUT/PATCH/DELETE
// 且携带 Idempotency-Key 头时，按 (method, path, key) 做响应级别
// 的缓存命中-回放，存储下沉到 PG（Phase 13 / v4.0 M2），多副本就绪。
// 生产路由只在公开 API 鉴权之后挂载本中间件；admin 与探针不参与。
//
// 流程（PG Acquire/Get/Store/Delete）：
//  1. method 不在 idempotentMethods 中 / 无 Idempotency-Key 头 → 直接 Next。
//  2. Acquire(key)：
//     - 抢占成功（本请求是第一个或接管已完成且过期的 generation）→ 包装
//     ResponseWriter 录制，跑 handler，结束后以 owner/generation CAS 做
//     Store（2xx/确定性 4xx）、Delete（明确未提交的瞬态
//     409/425/429），或保留 in-flight 证据（5xx）。
//     - 抢占失败且既存行已完成（in_flight=false）+ 未过期 → 回放既存响应，
//     不调 handler。
//     - 抢占失败且既存行仍 in_flight → 有界轮询 Get；完成即回放，超时
//     返回 425 + Retry-After；若 processing TTL 已过，则结果未知，返回稳定
//     503 且永久禁止同 key 再进入业务 handler。
//  3. Acquire/Get 出错（DB 抖动）→ 503 fail-closed，避免结果未知时重复写。
//  4. 首 owner 的 Store/Delete 使用脱离客户端取消、但有 deadline 的服务端
//     context；断连不会丢失已提交业务的幂等收尾。
//
// 为什么 5xx 不能释放 claim：HTTP 状态无法证明业务事务没有提交。handler
// 可能先完成写入，再因后续读取/编码失败返回 500；删除 claim 会让同 key 重试
// 再次执行副作用。只有明确表示前置条件未满足的 409/425/429 才允许释放。
// 400/422 等确定性客户端错误则直接存储并回放。
func Idempotency(cache *PGIdempotencyCache) gin.HandlerFunc {
	if cache == nil {
		// 防御性：未提供 cache 时退化为 no-op，避免 nil 解引用 panic。
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}
		if _, ok := idempotentMethods[c.Request.Method]; !ok {
			c.Next()
			return
		}
		key := c.GetHeader(IdempotencyHeader)
		if key == "" {
			c.Next()
			return
		}

		// 缓存 key：method + path + idempotency-key。单实例只存在一个数据域。
		// query string 不纳入：语义等价的 query 差异不应产生不同缓存项。
		cacheKey := c.Request.Method + ":" + c.Request.URL.Path + ":" + key
		cache.serveKeyedWrite(c, cacheKey)
	}
}

func (c *PGIdempotencyCache) serveKeyedWrite(ginContext *gin.Context, cacheKey string) {
	now := c.now()
	ownerToken := c.newOwnerToken()
	if ownerToken == "" {
		if c.logger != nil {
			c.logger.Error("idempotency owner token generator returned an empty token")
		}
		writeIdempotencyUnavailable(ginContext)
		return
	}
	acquired, existing, err := c.store.Acquire(ginContext.Request.Context(), cacheKey, ownerToken, now, now.Add(c.ttl))
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("idempotency acquire failed, rejecting keyed write", "error", observability.SafeError(err))
		}
		writeIdempotencyUnavailable(ginContext)
		return
	}
	if acquired {
		c.serveOwnedClaim(ginContext, cacheKey, ownerToken, existing)
		return
	}
	c.serveExistingClaim(ginContext, cacheKey, existing)
}

func (c *PGIdempotencyCache) serveOwnedClaim(ginContext *gin.Context, cacheKey, ownerToken string, claimed *StoredResponse) {
	if claimed == nil || claimed.Claim.OwnerToken != ownerToken || claimed.Claim.Generation < 1 {
		if c.logger != nil {
			c.logger.Error("idempotency acquire returned an invalid claim")
		}
		writeIdempotencyUnavailable(ginContext)
		return
	}

	// 抢占成功：录制响应，跑 handler，结束后以服务端拥有的有界 context
	// 做 owner/generation CAS 收尾。客户端断连不会取消 Store/Delete。
	rec := &idempotencyRecorder{ResponseWriter: ginContext.Writer, body: &bytes.Buffer{}}
	ginContext.Writer = rec
	ginContext.Next()
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ginContext.Request.Context()), c.finalizeTimeout)
	defer cancel()
	c.recordOutcome(finalizeCtx, cacheKey, claimed.Claim, rec)
}

func (c *PGIdempotencyCache) serveExistingClaim(ginContext *gin.Context, cacheKey string, existing *StoredResponse) {
	// 既存已完成行直接回放。processing 行绝不进入业务 handler；等待
	// owner 完成，或返回明确的 425 让客户端稍后重试。processing 行一旦
	// 到期就代表业务结果无法确认，必须永久 fail closed。
	if existing == nil {
		writeIdempotencyUnavailable(ginContext)
		return
	}
	if existing.InFlight && !existing.ExpiresAt.After(c.now()) {
		writeIdempotencyResultUnknown(ginContext)
		return
	}
	if !existing.ExpiresAt.After(c.now()) {
		writeIdempotencyUnavailable(ginContext)
		return
	}
	if !existing.InFlight {
		replayStoredResponse(ginContext, existing)
		return
	}

	stored, waitErr := c.waitForOutcome(ginContext.Request.Context(), cacheKey)
	switch {
	case waitErr == nil && stored != nil:
		replayStoredResponse(ginContext, stored)
	case errors.Is(waitErr, errIdempotencyWaitTimeout),
		errors.Is(waitErr, errIdempotencyRecordMissing),
		errors.Is(waitErr, context.Canceled):
		writeIdempotencyInProgress(ginContext, c.waitTimeout)
	case errors.Is(waitErr, errIdempotencyResultUnknown):
		writeIdempotencyResultUnknown(ginContext)
	default:
		if c.logger != nil {
			c.logger.Warn("idempotency wait failed, rejecting keyed write", "error", observability.SafeError(waitErr))
		}
		writeIdempotencyUnavailable(ginContext)
	}
}

var (
	errIdempotencyWaitTimeout   = errors.New("idempotency wait timeout")
	errIdempotencyRecordMissing = errors.New("idempotency record missing")
	errIdempotencyResultUnknown = errors.New("idempotency result unknown")
)

func (c *PGIdempotencyCache) waitForOutcome(requestCtx context.Context, cacheKey string) (*StoredResponse, error) {
	waitCtx, cancel := context.WithTimeout(requestCtx, c.waitTimeout)
	defer cancel()
	ticker := time.NewTicker(c.waitPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, errIdempotencyWaitTimeout
			}
			return nil, waitCtx.Err()
		case <-ticker.C:
			stored, err := c.store.Get(waitCtx, cacheKey)
			if err != nil {
				if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
					return nil, errIdempotencyWaitTimeout
				}
				return nil, err
			}
			if stored == nil {
				// The current owner may have deliberately released a known-safe
				// transient result. The caller should retry acquisition rather than
				// treating an absent row as durable unknown evidence.
				return nil, errIdempotencyRecordMissing
			}
			if stored.InFlight && !stored.ExpiresAt.After(c.now()) {
				return nil, errIdempotencyResultUnknown
			}
			if !stored.InFlight && stored.ExpiresAt.After(c.now()) {
				return stored, nil
			}
		}
	}
}

func writeIdempotencyInProgress(c *gin.Context, waitTimeout time.Duration) {
	retryAfter := int(math.Ceil(waitTimeout.Seconds()))
	if retryAfter < 1 {
		retryAfter = 1
	}
	c.Header("Retry-After", strconv.Itoa(retryAfter))
	JSONErrorWithSlug(c, http.StatusTooEarly, ErrCodeIdempotencyInProgress, "request with this idempotency key is still processing")
}

func writeIdempotencyUnavailable(c *gin.Context) {
	JSONErrorWithSlug(c, http.StatusServiceUnavailable, ErrCodeIdempotencyUnavailable, "idempotency service unavailable")
}

func writeIdempotencyResultUnknown(c *gin.Context) {
	JSONErrorWithSlug(c, http.StatusServiceUnavailable, ErrCodeIdempotencyResultUnknown, "prior request result cannot be confirmed")
}

// recordOutcome 在 handler 跑完后，按录制到的状态码决定把响应落库供回放
// （2xx/确定性 4xx）、释放明确安全的瞬态 4xx，或保留 5xx 的未知结果证据。抽出独立
// 方法是为了把主中间件闭包的圈复杂度压到 lint 阈值内，同时也让「成功后做
// 什么」单点可读可测。
func (c *PGIdempotencyCache) recordOutcome(ctx context.Context, cacheKey string, claim IdempotencyClaim, rec *idempotencyRecorder) {
	status := rec.status
	if status == 0 {
		status = rec.Status()
		if status == 0 {
			status = http.StatusOK
		}
	}

	if !shouldCacheStatus(status) {
		// 5xx 不能证明业务副作用没有提交。保留 in-flight 行，让 live TTL 内
		// 的重试得到 425，过期后永久得到 result_unknown，绝不再次进 handler。
		if !shouldReleaseIdempotencyClaim(status) {
			return
		}

		// 409/425/429 是应用合同中明确的前置条件失败；以 claim CAS 释放，
		// 允许条件恢复后用同 key 串行重试。若删除结果未知，行仍 fail closed。
		if delErr := c.store.Delete(ctx, cacheKey, claim); delErr != nil && c.logger != nil {
			c.logger.Warn("idempotency delete uncached key failed", "error", observability.SafeError(delErr))
		}
		return
	}

	// body / content-type 做 deep copy：bytes.Buffer.Bytes() 的底层数组在后续
	// Write 会被复用，必须拷贝出来 own。
	bodyCopy := make([]byte, rec.body.Len())
	copy(bodyCopy, rec.body.Bytes())

	ct := rec.contentType
	if ct == "" {
		ct = rec.Header().Get("Content-Type")
	}

	if storeErr := c.store.Store(ctx, cacheKey, claim, status, bodyCopy, ct, c.now().Add(c.ttl)); storeErr != nil && c.logger != nil {
		// 响应已发回客户端，无法改写；保留 processing 行意味着同键重试会
		// 先 wait/425；processing TTL 到期后成为永久 fail-closed 的未知结果。
		c.logger.Warn("idempotency store response failed", "error", observability.SafeError(storeErr))
	}
}

// shouldCacheStatus 决定一个状态码是否值得缓存。
//   - 2xx：正常响应，是 idempotency-key 想保护的主要场景。
//   - 400/422 等确定性 4xx：重试不会改变结果，回放节省 handler 重复校验。
//   - 409/425/429：不缓存，并由明确的 release 规则允许串行重试。
//   - 5xx 与其它：不缓存响应，但保留 claim，因为副作用结果无法证明。
func shouldCacheStatus(status int) bool {
	if status >= 200 && status < 300 {
		return true
	}
	switch status {
	case http.StatusConflict, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	if status >= 400 && status < 500 {
		return true
	}
	return false
}

func shouldReleaseIdempotencyClaim(status int) bool {
	switch status {
	case http.StatusConflict, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// replayStoredResponse 把既存响应原样写回 ResponseWriter，并终止中间件链。
// 写入顺序：Content-Type 头 → WriteHeader(status) → Write(body)。一旦
// WriteHeader 触发，header 就 frozen 了，所以 Content-Type 必须先设。
func replayStoredResponse(c *gin.Context, stored *StoredResponse) {
	if stored.ContentType != "" {
		c.Writer.Header().Set("Content-Type", stored.ContentType)
	}
	// 标记响应来自幂等回放，方便客户端 / 运维排查。
	c.Writer.Header().Set("Idempotent-Replay", "true")
	c.Writer.WriteHeader(stored.Status)
	if len(stored.Body) > 0 {
		_, _ = c.Writer.Write(stored.Body)
	}
	c.Abort()
}
