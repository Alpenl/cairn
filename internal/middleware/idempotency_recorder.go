package middleware

import (
	"bytes"
	"net/http"

	"github.com/gin-gonic/gin"
)

// idempotencyRecorder 是 gin.ResponseWriter 的薄 wrap：每次 Write 同时
// 落到原始 writer（让客户端看到响应）和内部 buffer（供后续 put 到缓存）。
// status 在 WriteHeader 时捕获；如果 handler 直接 Write 而没 WriteHeader，
// gin 内部会用 200 作为默认值，这里在 Write 路径上也兜底一次。
type idempotencyRecorder struct {
	gin.ResponseWriter
	body        *bytes.Buffer
	status      int
	contentType string
}

func (r *idempotencyRecorder) WriteHeader(code int) {
	r.status = code
	// 在 WriteHeader 调用时锁定 Content-Type；之后 handler 改 header 也
	// 不会影响已经发出的响应头。
	if r.contentType == "" {
		r.contentType = r.ResponseWriter.Header().Get("Content-Type")
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *idempotencyRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		// 没显式调过 WriteHeader：取 gin Writer 当前 Status（一般是 200）。
		r.status = r.Status()
		if r.status == 0 {
			r.status = http.StatusOK
		}
	}
	if r.contentType == "" {
		r.contentType = r.ResponseWriter.Header().Get("Content-Type")
	}
	// 先 buffer，再走真实 writer。两个写都需要成功才视为成功；buffer
	// 写不会失败（bytes.Buffer.Write 返回 nil error），所以只需要看真实
	// writer 的返回。
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (r *idempotencyRecorder) WriteString(s string) (int, error) {
	return r.Write([]byte(s))
}
