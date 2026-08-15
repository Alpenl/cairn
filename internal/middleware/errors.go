package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/observability"
)

// recoveryStackLimit 限制注入到日志的 stack trace 长度。完整 stack 在
// 深栈 panic 时可达 50KB+，写到结构化日志里会撑大单条 entry，影响日
// 志后端的吞吐与展示。4KB 通常足以覆盖业务层 + 框架层栈帧；超出截断
// 比"日志被拒收"更友好。
const recoveryStackLimit = 4096

// 标准错误 slug 常量集合：用于 ErrorDetail.ErrorCode 字段。
//
// 设计意图：现有 ErrorDetail.Code 是 HTTP 状态码（int），机器可读性差 —— 同样
// 的 400 既可能是"请求体非法"也可能是"link_id 格式错误"，客户端没办法只看
// status 区分。Wave 6 之后把 slug 作为新字段 error_code 加入信封，给前端 /
// SDK 一组稳定的机器可读 token。
//
// 命名约定：snake_case，名词或动宾短语，避免 HTTP 状态码字面量（已经在
// status 里给出）。新增 slug 请在这里集中登记，避免散落字符串字面量。
//
// 与 httperr.Code* 的关系：service 层用 httperr.NewWithCode 时引用
// httperr.Code* 常量；presentation 层（本文件）的 ErrCode* 是 handler /
// middleware 自身直接调用 JSONErrorWithSlug 时使用。两侧若有重名（如
// link_not_found），**字面值必须保持一致**，避免前端按 slug 分支时产生歧义。
// 重叠常量已在 httperr/httperr.go 同步登记。
const (
	// ErrCodeInvalidRequestBody —— 请求体解码失败或结构化校验未通过。
	// 422 路径上由 bindJSONWithLimit 使用。
	ErrCodeInvalidRequestBody = "invalid_request_body"
	// ErrCodeBodyTooLarge —— body 超过路由或全局上限。413 路径。
	ErrCodeBodyTooLarge = "body_too_large"
	// ErrCodeBodyUnreadable —— body 读取过程中失败（非 MaxBytes 触发）。400 路径。
	ErrCodeBodyUnreadable = "body_unreadable"
	// ErrCodeIdempotencyInProgress marks a duplicate keyed write whose owner
	// did not finish inside the bounded replay wait. 425 path.
	ErrCodeIdempotencyInProgress = "idempotency_in_progress"
	// ErrCodeIdempotencyUnavailable marks an acquire/wait storage failure.
	// Keyed writes fail closed with 503 because executing would risk a duplicate.
	ErrCodeIdempotencyUnavailable = "idempotency_unavailable"
	// ErrCodeIdempotencyResultUnknown marks an expired in-flight claim whose
	// business result may have committed. The same key is permanently blocked.
	ErrCodeIdempotencyResultUnknown = "idempotency_result_unknown"
	// ErrCodeLinkNotFound —— /api/links/:id 系列：链接不存在。404 路径。
	ErrCodeLinkNotFound = "link_not_found"
	// ErrCodeJobNotFound —— /api/jobs/:id：解析任务不存在。404 路径。
	ErrCodeJobNotFound = "job_not_found"
	// ErrCodeCooldownActive —— refresh 触发 per-link 冷却窗口。429 路径。
	ErrCodeCooldownActive = "cooldown_active"
	// ErrCodeRateLimitExceeded —— 全局 / per-IP 限流被打中。429 路径。
	ErrCodeRateLimitExceeded = "rate_limit_exceeded"
	// ErrCodeInvalidCursor —— ?after= 游标 token 解析失败或与 ?page= 冲突。422 路径。
	ErrCodeInvalidCursor = "invalid_cursor"
	// ErrCodeProposalAlreadyDecided —— concept-merge 已决策；并发审核冲突。409 路径。
	ErrCodeProposalAlreadyDecided = "proposal_already_decided"
	// ErrCodeInvalidProposalID —— concept-merge :id 不是合法 UUID。400 路径。
	ErrCodeInvalidProposalID = "invalid_proposal_id"
	// ErrCodeUnauthorized —— 通用鉴权失败（admin Bearer / metrics token）。401 路径。
	ErrCodeUnauthorized = "unauthorized"
	// ErrCodeInternalError —— 未被业务层显式分类的 500 兜底。
	ErrCodeInternalError = "internal_error"
)

// defaultErrorCodeForStatus 给未显式提供 slug 的旧调用点一个保底值，形如
// "default_500"。保留旧 JSONError(c, status, message) 调用语法不必逐个改动，
// 同时让前端依然能拿到一个非空 error_code（"default_xxx" 一看就知道是兜底
// 而非业务定义的稳定 slug，便于后续治理）。
func defaultErrorCodeForStatus(status int) string {
	return "default_" + strconv.Itoa(status)
}

// JSONError 中断当前请求并返回统一的 JSON 错误响应（dto.ErrorResponse 结构），
// 让所有 API 错误共享同一份外层信封，方便客户端按统一形状反序列化。
//
// 兼容老签名：未提供 slug 时 error_code 字段走 default_<status> 兜底。新代码
// 应优先使用 JSONErrorWithSlug 把机器可读 slug 传进来。
func JSONError(c *gin.Context, code int, message string) {
	JSONErrorWithSlug(c, code, defaultErrorCodeForStatus(code), message)
}

// JSONErrorWithSlug 是 JSONError 的增强版：除了 HTTP 状态码和面向人的 message，
// 还接受一个机器可读的 error_code slug。slug 走 ErrorDetail.ErrorCode（JSON
// 字段名 error_code），与现有 code（HTTP status int）并存，向后兼容。
//
// slug 为空字符串时回退到 default_<status>，等同 JSONError 行为。
func JSONErrorWithSlug(c *gin.Context, code int, slug, message string) {
	JSONErrorWithSlugAndCurrentIdentity(c, code, slug, message, nil)
}

// JSONErrorWithSlugAndCurrentIdentity is the source-CAS conflict variant of
// JSONErrorWithSlug. Existing error responses pass nil and retain their exact
// wire shape; translation 409 responses attach structured current identity.
func JSONErrorWithSlugAndCurrentIdentity(c *gin.Context, code int, slug, message string, currentIdentity *dto.TranslationSourceIdentity) {
	if slug == "" {
		slug = defaultErrorCodeForStatus(code)
	}
	c.AbortWithStatusJSON(code, dto.ErrorResponse{
		Error: dto.ErrorDetail{
			Code:            code,
			ErrorCode:       slug,
			Message:         message,
			CurrentIdentity: currentIdentity,
		},
	})
}

// Recovery 返回一个 Gin 中间件，将 handler 中的 panic 转换为带 request_id 的
// 结构化错误日志并以 500 + 统一 JSON 错误信封作答，避免裸 panic 触发 Gin
// 默认 HTML 错误页。
//
// Wave 12.5 L2 起在日志里追加一个 "stack" 字段，内容是 debug.Stack() 截
// 断到 recoveryStackLimit 字节后的字符串。原因：之前 panic 日志只有
// recovered 的字面值（通常是 error.Error() 或 nil 解引用提示），但
// "在哪一行 panic" 完全没记录，事故排查只能猜或者复现 —— 直接把当前
// goroutine 的 stack 拍进日志，把推断变成查阅。
func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		if logger != nil {
			stack := debug.Stack()
			if len(stack) > recoveryStackLimit {
				stack = stack[:recoveryStackLimit]
			}
			observability.WithRequestID(logger, c.GetString(RequestIDContextKey)).Error("http panic recovered",
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"panic", recovered,
				"stack", string(stack),
			)
		}
		JSONErrorWithSlug(c, http.StatusInternalServerError, ErrCodeInternalError, "内部服务器错误")
	})
}
