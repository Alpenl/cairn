package handler

import (
	"reflect"
	"strings"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
)

// init 让 gin 默认的 validator 在 FieldError 上使用 JSON tag 名而非 Go 字段名。
//
// 背景：bindJSONWithLimit 把 validator.ValidationErrors 第一条转成
// client-safe 短消息（firstValidationMessage），消息里要带"哪个字段失败了"。
// 默认 fe.Field() 返回 Go struct field 名（PascalCase，如 "URL"、"Kind"）；
// 但客户端拿到 JSON envelope 后只认得 JSON tag（小写 / snake_case，如 "url"、
// "kind"）。注册 RegisterTagNameFunc 让两边对齐，错误消息直接可被前端按字段
// 定位，不需要二次映射。
//
// 这是 wave 10 API MED M5 的配套改造：validator tag 化无意义，除非客户端
// 能拿到稳定的字段名。
func init() {
	v, ok := binding.Validator.Engine().(*validator.Validate)
	if !ok {
		return
	}
	v.RegisterTagNameFunc(func(field reflect.StructField) string {
		// `json:"url,omitempty"` → "url"；无 json tag 或 tag 是 "-" 则
		// fallback 到 Go 字段名（保持现状）。
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			return field.Name
		}
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			tag = tag[:comma]
		}
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return field.Name
		}
		return tag
	})
}
