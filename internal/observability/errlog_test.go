package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"webtag/internal/observability"
)

// TestSafeError_NilStaysSafe 验证 nil err 时 SafeError 渲染成 "none"
// 分类的 group，不 panic。生产路径上调用方习惯性地把可能为 nil 的 err
// 也传进来——这里要求兜底正确。
func TestSafeError_NilStaysSafe(t *testing.T) {
	t.Parallel()

	out := logToJSON(t, "boom", observability.SafeError(nil))
	got := out["error"].(map[string]any)
	if got["msg"] != "none" {
		t.Errorf("msg = %v, want \"none\"", got["msg"])
	}
	if got["category"] != "none" {
		t.Errorf("category = %v, want \"none\"", got["category"])
	}
	if got["chain"] != "" {
		t.Errorf("chain = %q, want empty string", got["chain"])
	}
}

// TestSafeError_RedactsOpenAIKey 覆盖 sk- 风格凭据。这是 OpenAI 风格
// API key 最常见的形态，错误链里出现 "openai: 401 unauthorized for
// sk-..." 时绝不能原样泄露。
func TestSafeError_RedactsOpenAIKey(t *testing.T) {
	t.Parallel()

	leaky := errors.New("openai chat completions: 401 unauthorized for key sk-abcdef0123456789ABCDEF")
	out := logToJSON(t, "boom", observability.SafeError(leaky))
	fields := out["error"].(map[string]any)
	chain := fields["chain"].(string)
	msg := fields["msg"].(string)

	if strings.Contains(chain, "sk-abcdef0123456789ABCDEF") {
		t.Errorf("chain leaked openai key: %q", chain)
	}
	if strings.Contains(msg, "sk-abcdef0123456789ABCDEF") {
		t.Errorf("msg leaked openai key: %q", msg)
	}
	if !strings.Contains(chain, "sk-<redacted>") {
		t.Errorf("chain missing redaction marker: %q", chain)
	}
}

// TestSafeError_RedactsBearerToken 覆盖 "Bearer xxx" 形态。HTTP 401
// 响应体里上游服务常常会回显客户端发的 Authorization 头部分内容。
func TestSafeError_RedactsBearerToken(t *testing.T) {
	t.Parallel()

	leaky := errors.New(`upstream returned 401 with "Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig"`)
	out := logToJSON(t, "boom", observability.SafeError(leaky))
	chain := out["error"].(map[string]any)["chain"].(string)

	if strings.Contains(chain, "eyJhbGciOiJIUzI1NiJ9.payload.sig") {
		t.Errorf("chain leaked bearer token: %q", chain)
	}
	if !strings.Contains(chain, "Bearer <redacted>") {
		t.Errorf("chain missing redaction marker: %q", chain)
	}
}

// TestSafeError_RedactsPostgresDSN 覆盖 postgres://user:pass@host 形态。
// pgx 在连接失败时常常把整个 DSN 写进错误里，这是凭据泄漏的高频点位。
func TestSafeError_RedactsPostgresDSN(t *testing.T) {
	t.Parallel()

	leaky := fmt.Errorf("dial postgres://webtag:s3cr3t-pw@db.internal:5432/webtag: connection refused")
	out := logToJSON(t, "boom", observability.SafeError(leaky))
	chain := out["error"].(map[string]any)["chain"].(string)

	if strings.Contains(chain, "webtag:s3cr3t-pw") {
		t.Errorf("chain leaked DSN credentials: %q", chain)
	}
	if !strings.Contains(chain, "postgres://<redacted>@db.internal:5432/webtag") {
		t.Errorf("chain missing DSN redaction marker: %q", chain)
	}
	// host / port / dbname 保留——它们对运维有用，不算敏感。
	if !strings.Contains(chain, "db.internal:5432/webtag") {
		t.Errorf("chain unnecessarily redacted host/db: %q", chain)
	}
}

// TestSafeError_RedactsURLQueryString 覆盖 ?token=xxx 形态。短链 /
// signed URL / webhook callback 都会把临时凭据塞进 query string。
func TestSafeError_RedactsURLQueryString(t *testing.T) {
	t.Parallel()

	leaky := errors.New("fetch failed for https://upstream.example.com/api?token=abc123&user=42: timeout")
	out := logToJSON(t, "boom", observability.SafeError(leaky))
	chain := out["error"].(map[string]any)["chain"].(string)

	if strings.Contains(chain, "token=abc123") {
		t.Errorf("chain leaked query string token: %q", chain)
	}
	if !strings.Contains(chain, "?<redacted>") {
		t.Errorf("chain missing query-string redaction marker: %q", chain)
	}
	if !strings.Contains(chain, "https://upstream.example.com/api") {
		t.Errorf("chain dropped URL path/host: %q", chain)
	}
}

func TestSafeURLHostDropsAllPrivateURLComponents(t *testing.T) {
	t.Parallel()

	raw := "https://reader:secret@Example.COM:8443/private/account/path?token=review-secret#fragment"
	if got := observability.SafeURLHost(raw); got != "example.com" {
		t.Fatalf("SafeURLHost() = %q, want %q", got, "example.com")
	}
	for _, raw := range []string{"not a URL", "file:///private/path", "https:///missing-host"} {
		if got := observability.SafeURLHost(raw); got != "" {
			t.Fatalf("SafeURLHost(%q) = %q, want empty", raw, got)
		}
	}
}

// TestSafeError_PreservesCategory 验证 category 字段直接复用 errsafe
// 的分类——dashboards / 告警依赖这个标签稳定。
func TestSafeError_PreservesCategory(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("timed out fetching upstream")
	out := logToJSON(t, "boom", observability.SafeError(wrapped))
	got := out["error"].(map[string]any)
	if got["category"] != "timeout" {
		t.Errorf("category = %v, want \"timeout\"", got["category"])
	}
}

// TestSafeError_NoRegressionOnPlainError 边界用例：错误链里不含任何
// 凭据时，脱敏不应当改写文本（除了 LogValue group 化）。
func TestSafeError_NoRegressionOnPlainError(t *testing.T) {
	t.Parallel()

	plain := errors.New("upstream returned 503 service unavailable")
	out := logToJSON(t, "boom", observability.SafeError(plain))
	chain := out["error"].(map[string]any)["chain"].(string)
	if chain != "upstream returned 503 service unavailable" {
		t.Errorf("chain unexpectedly rewritten: %q", chain)
	}
}

// TestSafeError_RedactsBasicAuth 覆盖 "Authorization: Basic <base64>" 形态。
// 反向代理 / 内部 service 之间常用 Basic auth，错误回显时整段 base64
// 必须脱敏（base64 解开就是 user:password）。
func TestSafeError_RedactsBasicAuth(t *testing.T) {
	t.Parallel()

	leaky := errors.New(`upstream returned "Authorization: Basic dXNlcjpwYXNzd29yZA==" but credentials rejected`)
	out := logToJSON(t, "boom", observability.SafeError(leaky))
	chain := out["error"].(map[string]any)["chain"].(string)

	if strings.Contains(chain, "dXNlcjpwYXNzd29yZA") {
		t.Errorf("chain leaked basic auth blob: %q", chain)
	}
	if !strings.Contains(chain, "Basic <redacted>") {
		t.Errorf("chain missing basic redaction marker: %q", chain)
	}
}

// TestSafeError_RedactsBareJWT 覆盖不带 Bearer 前缀的裸 JWT。某些上游
// 把 token 直接拼到 path 或 error body 里，没有 Bearer 关键字。
func TestSafeError_RedactsBareJWT(t *testing.T) {
	t.Parallel()

	leaky := errors.New("upstream rejected token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.abcdefXYZ0123456 with 401")
	out := logToJSON(t, "boom", observability.SafeError(leaky))
	chain := out["error"].(map[string]any)["chain"].(string)

	if strings.Contains(chain, "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.abcdefXYZ0123456") {
		t.Errorf("chain leaked bare jwt: %q", chain)
	}
	if !strings.Contains(chain, "<redacted-jwt>") {
		t.Errorf("chain missing jwt redaction marker: %q", chain)
	}
}

// TestSafeError_RedactsCookie 覆盖 "Cookie: session=..." 和 "Set-Cookie:" 头。
// Session token / CSRF token 经常进 Cookie，整段 redact 最安全。
func TestSafeError_RedactsCookie(t *testing.T) {
	t.Parallel()

	leaky := errors.New(`upstream replied with "Set-Cookie: session=abc123xyz; HttpOnly" then closed`)
	out := logToJSON(t, "boom", observability.SafeError(leaky))
	chain := out["error"].(map[string]any)["chain"].(string)

	if strings.Contains(chain, "session=abc123xyz") {
		t.Errorf("chain leaked cookie value: %q", chain)
	}
	if !strings.Contains(chain, "Set-Cookie: <redacted>") {
		t.Errorf("chain missing cookie redaction marker: %q", chain)
	}
}

// TestSafeError_RedactsCookieUnquoted 验证无引号包裹的 Cookie 头：
// regex 在 `;` 处终止，不会一路吃掉后续运维上下文。
func TestSafeError_RedactsCookieUnquoted(t *testing.T) {
	t.Parallel()

	leaky := errors.New("Set-Cookie: session=abc123xyz; HttpOnly then upstream closed")
	out := logToJSON(t, "boom", observability.SafeError(leaky))
	chain := out["error"].(map[string]any)["chain"].(string)

	if strings.Contains(chain, "session=abc123xyz") {
		t.Errorf("chain leaked cookie value: %q", chain)
	}
	if !strings.Contains(chain, "<redacted>") {
		t.Errorf("chain missing redaction marker: %q", chain)
	}
	// 后续非敏感运维上下文应保留
	if !strings.Contains(chain, "then upstream closed") {
		t.Errorf("chain overzealously dropped non-sensitive tail: %q", chain)
	}
}

// TestSafeError_TruncatesLongChain 验证 chain 字段在超长错误链下做了截断。
// 避免恶意构造或自然嵌套的 wrap 链撑爆日志聚合。
func TestSafeError_TruncatesLongChain(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("very long error message segment. ", 200) // ~6.4KB
	leaky := errors.New(huge)
	out := logToJSON(t, "boom", observability.SafeError(leaky))
	chain := out["error"].(map[string]any)["chain"].(string)

	// 截断后含明确标记
	if !strings.Contains(chain, "bytes truncated") {
		t.Errorf("chain missing truncation marker: len=%d", len(chain))
	}
	// 长度受控（< 4KB 留出截断 suffix + JSON 转义余地）
	if len(chain) > 4096 {
		t.Errorf("chain length %d exceeds reasonable bound", len(chain))
	}
}

// TestSafeError_PreservesNestedWrap 验证多层 fmt.Errorf %w 包装时，
// chain 字段输出完整链路，且所有层级的脱敏规则都生效。
func TestSafeError_PreservesNestedWrap(t *testing.T) {
	t.Parallel()

	base := errors.New("dial postgres://u:p@host:5432/db: refused")
	mid := fmt.Errorf("setup pool: %w", base)
	top := fmt.Errorf("bootstrap: %w", mid)

	out := logToJSON(t, "boom", observability.SafeError(top))
	chain := out["error"].(map[string]any)["chain"].(string)

	// 链路文本应包含每一层的前缀
	if !strings.Contains(chain, "bootstrap:") || !strings.Contains(chain, "setup pool:") {
		t.Errorf("chain missing wrap context: %q", chain)
	}
	// DSN 凭据应被替换，即便在嵌套深处
	if strings.Contains(chain, "u:p@host") {
		t.Errorf("chain leaked nested DSN credentials: %q", chain)
	}
}

// logToJSON 把一条 slog.Error 写入 buffer，解析回 map[string]any 供断言。
// 测试侧用 NewJSONHandler 而非全局 default logger 避免污染 stdout。
func logToJSON(t *testing.T, msg string, errField slog.LogValuer) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logger.LogAttrs(context.Background(), slog.LevelError, msg, slog.Any("error", errField))

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("failed to parse JSON log line %q: %v", buf.String(), err)
	}
	return out
}
