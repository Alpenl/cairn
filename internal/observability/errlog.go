// observability/errlog.go —— 错误日志脱敏包装。
//
// Wave 2 H5：直接把 error 塞进 slog 字段会原样输出 err.Error()。如果
// 错误链里含 URL 的 ?token=、postgres://user:pass@host、Bearer xxx 等
// 敏感 token，它们就会进入日志聚合系统（Loki / Splunk / ELK），形成
// 长尾的"凭据散落"风险。本文件提供 SafeError(err) 包装：
//
//   - 输出三个字段而非原始 err.Error()：
//     msg       errsafe.SafeMessage(err) 给出的 "<category>: <truncated>"
//     category  errsafe.ClassifyError(err) 的分类 label
//     chain     脱敏后的完整错误链文本（fmt.Sprintf("%v", err)）
//
//   - chain 依次过：sanitizePostgresDSN（去 user:pass@）→ sanitizeURL
//     （去 query string）→ sanitizeAuthHeader（Bearer / Basic / Cookie /
//     sk- / 裸 JWT），最后做 2KB 截断防长链 DoS。不影响错误形状，只挖
//     掉值。
//
// 调用方：
//
//	slog.Error("upstream call failed",
//	    "error", observability.SafeError(err),
//	    "link_id", linkID,
//	)
//
// slog 会调用 LogValuer.LogValue() 把 SafeError 展开成 group，渲染成
//
//	{"level":"ERROR","error":{"msg":"...","category":"network","chain":"..."}}
//
// 比原始 err 多 1 行栈，但永远不会泄露 secret。
package observability

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"

	"webtag/internal/errsafe"
)

// SafeError 把 err 包成 slog.LogValuer。nil err 返回的 SafeError 在写入
// 日志时会展开为 "none" 分类的空 group——比 panic 更友好，调用方不需要
// 在每个 slog.Error 现场加 nil 判断。
func SafeError(err error) slog.LogValuer {
	return safeErr{err: err}
}

// SafeURLHost returns the only URL identity allowed in ordinary logs. It
// deliberately drops userinfo, path, query, fragment, and port; link_id stays
// the primary correlation key. Invalid or non-HTTP(S) input produces an empty
// value rather than echoing attacker-controlled text.
func SafeURLHost(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

type safeErr struct {
	err error
}

// LogValue 渲染 slog 字段：msg / category / chain。chain 是脱敏后的
// 完整错误链字符串，msg 是 errsafe.SafeMessage 的截断摘要。两者都呈现
// 是为了在排查时既有索引友好的短摘要也有完整上下文。
func (s safeErr) LogValue() slog.Value {
	if s.err == nil {
		return slog.GroupValue(
			slog.String("msg", "none"),
			slog.String("category", "none"),
			slog.String("chain", ""),
		)
	}
	return slog.GroupValue(
		slog.String("msg", errsafe.SafeMessage(s.err)),
		slog.String("category", errsafe.ClassifyError(s.err)),
		slog.String("chain", sanitizeErrorChain(s.err)),
	)
}

// chainMaxBytes 是 chain 字段输出的硬上限。错误链 Unwrap 嵌套若过深
// （retry → http → ssrf → dns 等），单条日志可能膨胀到几 KB；构造性
// 输入甚至能触发 DoS 级别的日志撑爆。2KB 足以容纳常见 4-6 层错误链，
// 超过时截断并附 "... (N bytes truncated)" 提示。
const chainMaxBytes = 2048

// sanitizeErrorChain 把 fmt.Sprintf("%v", err) 的结果依次过几道脱敏。
// 顺序是有讲究的：先处理 postgres URI（连 user:pass 一起替换），再处理
// 通用 URL 的 query string，最后是 Bearer / Basic / Cookie / sk- / JWT。
// 如果反过来，URL 脱敏会先把 postgres://user:pass@host 里的 query string
// 去掉，但 user:pass 部分还在，反而失去 DSN-aware 替换的机会。
// 最后做长度截断，避免超长 chain 撑爆日志。
func sanitizeErrorChain(err error) string {
	raw := errsafe.RedactSecrets(fmt.Sprintf("%v", err))
	if len(raw) > chainMaxBytes {
		truncated := len(raw) - chainMaxBytes
		raw = raw[:chainMaxBytes] + fmt.Sprintf("... (%d bytes truncated)", truncated)
	}
	return raw
}

// 编译期断言：safeErr 实现了 slog.LogValuer。比 runtime panic 更早暴露
// "LogValue 签名飘了" 这种回归。
var _ slog.LogValuer = safeErr{}
