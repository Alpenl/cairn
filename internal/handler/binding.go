package handler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"

	"webtag/internal/middleware"
	"webtag/internal/observability"
)

func bindJSONWithLimit(c *gin.Context, dst any, limit int64) bool {
	return bindJSONWithLimitAndCode(c, dst, limit, middleware.ErrCodeBodyTooLarge)
}

func bindJSONWithLimitAndCode(c *gin.Context, dst any, limit int64, tooLargeCode string) bool {
	if limit > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	}
	if err := c.ShouldBindJSON(dst); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			middleware.JSONErrorWithSlug(c, http.StatusRequestEntityTooLarge, tooLargeCode, "请求体过大")
			return false
		}
		// Validator/decoder errors can include struct paths, types, and
		// regex hints — log them server-side and reply with a fixed,
		// non-revealing message so we don't fingerprint the API surface.
		if logger := observability.FromContext(c.Request.Context()); logger != nil {
			// Wave 2 H5：JSON 解码错误经常带 struct path + 上游 URL，用
			// SafeError 一次性脱敏 query string / Bearer / DSN。
			logger.Warn("invalid JSON body",
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"route", c.FullPath(),
				"error", observability.SafeError(err),
			)
		}
		// Wave 10 API MED M5：DTO 加 validator binding tag 后，最常见的
		// 失败形态是 validator.ValidationErrors（字段缺失 / 超长 / 格式不
		// 合法等）。把第一条 FieldError 转成 client-safe 短消息，让前端
		// 能定位是哪个字段、什么规则不满足；其它解码失败（malformed JSON
		// 等）仍走原始 "request body invalid"，不泄露 struct 内部结构。
		message := firstValidationMessage(err)
		middleware.JSONErrorWithSlug(c, http.StatusUnprocessableEntity, middleware.ErrCodeInvalidRequestBody, message)
		return false
	}
	return true
}

// firstValidationMessage 把 ShouldBindJSON 返回的错误转成面向客户端的简短
// 描述。仅对 validator.ValidationErrors 做字段级描述（field + rule），其它
// 解码 / 类型错误统一回退到固定 "request body invalid"，避免把 struct path
// 或解析器内部细节透回客户端、被用来 fingerprint API surface。
func firstValidationMessage(err error) string {
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) || len(verrs) == 0 {
		return "request body invalid"
	}
	fe := verrs[0]
	field := fe.Field()
	if field == "" {
		field = fe.StructField()
	}
	tag := fe.Tag()
	switch tag {
	case "required":
		return fmt.Sprintf("field %q is required", field)
	case "url":
		return fmt.Sprintf("field %q must be a valid URL", field)
	case "max":
		return fmt.Sprintf("field %q exceeds max length/size %s", field, fe.Param())
	case "min":
		return fmt.Sprintf("field %q below min length/size %s", field, fe.Param())
	case "oneof":
		return fmt.Sprintf("field %q must be one of: %s", field, fe.Param())
	case "uuid", "uuid4":
		return fmt.Sprintf("field %q must be a valid UUID", field)
	default:
		return fmt.Sprintf("field %q failed validation: %s", field, tag)
	}
}
