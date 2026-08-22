// Package observability provides structured logging and request-scoped logger
// helpers shared by the application.
package observability

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// ParseLevel maps a LOG_LEVEL config string to an slog.Level. Unknown or
// empty inputs fall back to Info so a typo never silences the logger. Both
// "warn" and "warning" are accepted; matching is case-insensitive and
// whitespace-tolerant. Centralized here so cmd/webtag and internal/app
// share the same parser instead of carrying drifting copies.
func ParseLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewLogger returns a JSON-handler logger at the default Info level. Prefer
// NewLoggerWithLevel when the level should come from configuration so the
// operator can drop it to debug or raise it to warn/error without recompiling.
func NewLogger() *slog.Logger {
	return NewLoggerWithLevel(slog.LevelInfo)
}

// NewLoggerWithLevel returns a JSON-handler logger emitting at the requested
// minimum level.
func NewLoggerWithLevel(level slog.Level) *slog.Logger {
	return NewLoggerWithOptions(LoggerOptions{Level: level, Format: LogFormatJSON})
}

// LogFormat 是 logger 的输出格式选项。
type LogFormat string

const (
	// LogFormatJSON 输出 JSON Lines，每行一条结构化日志。生产环境默认值，
	// 便于 Loki / ELK / Splunk 等聚合系统索引字段。
	LogFormatJSON LogFormat = "json"
	// LogFormatText 输出人类可读的 key=value 文本，并启用 AddSource
	// 显示日志触发点的文件:行号。本地开发推荐，stack trace 看起来一目了然。
	LogFormatText LogFormat = "text"
)

// LoggerOptions 控制 NewLoggerWithOptions 的输出。零值（Format=""）
// 退化到 JSON 输出 + Info 级别，保持与旧代码完全一致的默认行为。
type LoggerOptions struct {
	Level  slog.Level
	Format LogFormat
	// Writer 默认 os.Stdout；测试可以注入 bytes.Buffer 验证输出。
	Writer io.Writer
}

// NewLoggerWithOptions uses an instance-local level and supports text / JSON
// handlers. Constructing another logger cannot change an existing logger's
// filtering behavior.
//
// 调用方一般通过 NewLoggerWithLevel 进入，只有需要切 format 时才直接用本函数。
func NewLoggerWithOptions(opts LoggerOptions) *slog.Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}

	switch opts.Format {
	case LogFormatText:
		// AddSource: true 在 text 模式下加 source=file:line，本地排
		// 错时直接定位到日志触发点。生产 JSON 模式默认不开，避免
		// runtime.Callers 的固定开销。
		return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
			Level:     opts.Level,
			AddSource: true,
		}))
	default:
		// 包括空字符串 / "json" / 任何未识别值都落到 JSON——这是
		// 生产默认，向后兼容所有现有部署。
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level: opts.Level,
		}))
	}
}

// ParseLogFormat 把环境变量字符串映射为 LogFormat。未知 / 空值落回
// JSON，与 LOG_LEVEL 的 fallback 策略一致——typo 不会让日志系统沉默
// 或换成开发者格式。
func ParseLogFormat(raw string) LogFormat {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "text", "txt":
		return LogFormatText
	default:
		return LogFormatJSON
	}
}

// WithRequestID 返回一个绑定了 request_id 字段的派生 logger；
// logger 为 nil 时直接返回 nil，requestID 为空时返回原 logger（不附加空字段）。
func WithRequestID(logger *slog.Logger, requestID string) *slog.Logger {
	if logger == nil {
		return nil
	}
	if requestID == "" {
		return logger
	}
	return logger.With("request_id", requestID)
}

// FromContext 从 ctx 中取出之前通过 ContextWithLogger 注入的 logger；不存在时返回 nil。
func FromContext(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return nil
	}
	logger, _ := ctx.Value(loggerContextKey{}).(*slog.Logger)
	return logger
}

// ContextWithLogger 把 logger 绑定到一个新的 context，使下游可通过 FromContext 取出。
// ctx 为 nil 时退化到 context.Background()。
func ContextWithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

type loggerContextKey struct{}
