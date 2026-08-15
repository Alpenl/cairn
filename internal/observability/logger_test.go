package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"webtag/internal/observability"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"  debug  ", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
		// Unknown inputs fall back to Info.
		{"", slog.LevelInfo},
		{"trace", slog.LevelInfo},
		{"verbose", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := observability.ParseLevel(tc.input)
			if got != tc.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestNewLogger_NotNil(t *testing.T) {
	t.Parallel()
	if observability.NewLogger() == nil {
		t.Error("NewLogger() returned nil")
	}
}

func TestNewLoggerWithLevel_NotNil(t *testing.T) {
	t.Parallel()
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if observability.NewLoggerWithLevel(level) == nil {
			t.Errorf("NewLoggerWithLevel(%v) returned nil", level)
		}
	}
}

// TestParseLogFormat 覆盖 LOG_FORMAT 解析的默认值与基本分支。空字符串
// 必须回退到 JSON——保证未配置该 env 的旧部署继续吐 JSON 日志，
// 不会被静悄悄换成 text 格式破坏 Loki / ELK 的字段索引。
func TestParseLogFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want observability.LogFormat
	}{
		{"", observability.LogFormatJSON},
		{"json", observability.LogFormatJSON},
		{"JSON", observability.LogFormatJSON},
		{"unknown", observability.LogFormatJSON},
		{"text", observability.LogFormatText},
		{"TEXT", observability.LogFormatText},
		{"txt", observability.LogFormatText},
	}
	for _, tc := range cases {
		if got := observability.ParseLogFormat(tc.in); got != tc.want {
			t.Errorf("ParseLogFormat(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestNewLoggerWithOptions_JSONShape 验证 JSON handler 输出能解析为合法
// JSON——保护下游聚合器的解析逻辑。
func TestNewLoggerWithOptions_JSONShape(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := observability.NewLoggerWithOptions(observability.LoggerOptions{
		Level:  slog.LevelDebug,
		Format: observability.LogFormatJSON,
		Writer: &buf,
	})
	logger.LogAttrs(context.Background(), slog.LevelInfo, "hello", slog.String("k", "v"))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("output not valid JSON: %q (%v)", buf.String(), err)
	}
	if line["msg"] != "hello" {
		t.Errorf("msg = %v, want \"hello\"", line["msg"])
	}
	if line["k"] != "v" {
		t.Errorf("k = %v, want \"v\"", line["k"])
	}
}

func TestNewLoggerWithOptionsLevelsAreInstanceLocal(t *testing.T) {
	t.Parallel()

	var debugBuf, errorBuf bytes.Buffer
	var debugLogger, errorLogger *slog.Logger
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		debugLogger = observability.NewLoggerWithOptions(observability.LoggerOptions{
			Level:  slog.LevelDebug,
			Format: observability.LogFormatJSON,
			Writer: &debugBuf,
		})
	}()
	go func() {
		defer wg.Done()
		errorLogger = observability.NewLoggerWithOptions(observability.LoggerOptions{
			Level:  slog.LevelError,
			Format: observability.LogFormatJSON,
			Writer: &errorBuf,
		})
	}()
	wg.Wait()

	debugLogger.Debug("debug-visible")
	errorLogger.Info("info-filtered")
	if !strings.Contains(debugBuf.String(), "debug-visible") {
		t.Fatalf("debug logger inherited another logger's level: %q", debugBuf.String())
	}
	if strings.Contains(errorBuf.String(), "info-filtered") {
		t.Fatalf("error logger inherited another logger's level: %q", errorBuf.String())
	}
}

// TestNewLoggerWithOptions_TextHasSource 验证 text 模式启用了 AddSource，
// 输出里能看到 source=file:line。Dev 模式下这是关键 affordance；CI 里
// 同时也是 "format=text 真的换了 handler" 的硬证据。
func TestNewLoggerWithOptions_TextHasSource(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := observability.NewLoggerWithOptions(observability.LoggerOptions{
		Level:  slog.LevelDebug,
		Format: observability.LogFormatText,
		Writer: &buf,
	})
	logger.LogAttrs(context.Background(), slog.LevelInfo, "hello")

	out := buf.String()
	if !strings.Contains(out, "msg=hello") {
		t.Errorf("text output missing msg field: %q", out)
	}
	if !strings.Contains(out, "source=") {
		t.Errorf("text output missing source field (AddSource not enabled?): %q", out)
	}
}

// TestWithRequestID 锁定三条 contract：
//  1. nil logger 直接返回 nil（middleware 在 logger 没装好时不应 panic）
//  2. 空 requestID 返回原 logger（不追加空字段，避免日志体积膨胀）
//  3. 非空 requestID 派生出带 request_id 字段的新 logger
func TestWithRequestID(t *testing.T) {
	t.Parallel()

	// 1) nil logger
	if got := observability.WithRequestID(nil, "req-1"); got != nil {
		t.Errorf("WithRequestID(nil, _) = %v, want nil", got)
	}

	// 2) 空 requestID → 同一个 logger 实例
	var buf bytes.Buffer
	base := observability.NewLoggerWithOptions(observability.LoggerOptions{
		Format: observability.LogFormatJSON,
		Level:  slog.LevelInfo,
		Writer: &buf,
	})
	if got := observability.WithRequestID(base, ""); got != base {
		t.Errorf("WithRequestID(base, \"\") returned a different logger; expected to pass through original")
	}

	// 3) 非空 requestID → 输出包含 request_id 字段
	derived := observability.WithRequestID(base, "req-abc")
	if derived == nil {
		t.Fatal("WithRequestID(base, req) returned nil")
	}
	buf.Reset()
	derived.Info("hello")
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON output: %v (%q)", err, buf.String())
	}
	if got["request_id"] != "req-abc" {
		t.Errorf("request_id = %v, want %q", got["request_id"], "req-abc")
	}
	// 原 logger 不应被污染（With 的契约是返回派生实例）。
	buf.Reset()
	base.Info("plain")
	var base2 map[string]any
	if err := json.Unmarshal(buf.Bytes(), &base2); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	if _, ok := base2["request_id"]; ok {
		t.Errorf("base logger leaked request_id from derived: %v", base2)
	}
}

// TestContextWithLoggerAndFromContext 锁定 logger 在 context 里的注入/取出
// 是配对的，且都对 nil 安全：
//   - ContextWithLogger(nil, l) 应退化到 Background，不 panic
//   - FromContext(nil) 返回 nil
//   - FromContext(ctx 里没装) 返回 nil
//   - 注入后取出的就是同一个 *slog.Logger 指针
//
// 这条路径上承载的是 request-scoped logger（含 trace_id），整条链断会让
// handler 写日志时拿不到 request_id 字段，trace 关联失效。
func TestContextWithLoggerAndFromContext(t *testing.T) {
	t.Parallel()

	// 1) FromContext(nil) → nil
	if got := observability.FromContext(nil); got != nil { //nolint:staticcheck // reason: 显式传 nil 是测试的一部分
		t.Errorf("FromContext(nil) = %v, want nil", got)
	}

	// 2) context 里没装 → nil
	if got := observability.FromContext(context.Background()); got != nil {
		t.Errorf("FromContext(empty ctx) = %v, want nil", got)
	}

	// 3) 注入后取出指针相等
	var buf bytes.Buffer
	base := observability.NewLoggerWithOptions(observability.LoggerOptions{
		Format: observability.LogFormatJSON,
		Level:  slog.LevelInfo,
		Writer: &buf,
	})
	ctx := observability.ContextWithLogger(context.Background(), base)
	if got := observability.FromContext(ctx); got != base {
		t.Errorf("FromContext returned different logger from what ContextWithLogger stored")
	}

	// 4) ContextWithLogger(nil, _) 不 panic，退化到 Background 仍可取出
	ctx2 := observability.ContextWithLogger(nil, base) //nolint:staticcheck // reason: 显式传 nil 验证 nil-safe 兜底
	if got := observability.FromContext(ctx2); got != base {
		t.Errorf("FromContext on ctx built from nil-parent returned wrong logger")
	}
}
