package app

import (
	"context"
	"errors"
	"sort"
)

type readinessPinger interface {
	Ping(context.Context) error
}

// DatabaseReadiness 是 /ready 的就绪探针实现：通过对 pgxpool 执行
// Ping 来验证后端数据库可用。
type DatabaseReadiness struct {
	pool readinessPinger
}

// NewDatabaseReadiness 用给定的连接池构造就绪探针。pool 为 nil 时
// Ready 始终返回 nil，方便单测构造无依赖的运行时。
func NewDatabaseReadiness(pool readinessPinger) *DatabaseReadiness {
	return &DatabaseReadiness{pool: pool}
}

// Ready 实现 ReadinessChecker：对连接池执行一次 Ping，nil 接收者或
// 未注入 pool 时返回 nil。
func (r *DatabaseReadiness) Ready(ctx context.Context) error {
	if r == nil || r.pool == nil {
		return nil
	}
	return r.pool.Ping(ctx)
}

// ReadinessCheck 是一条被命名的就绪检查项：Name 用作 /ready degraded
// 响应中的 failed 列表元素；Check 返回 nil 表示通过，其他错误表示失败。
type ReadinessCheck struct {
	Name  string
	Check func(context.Context) error
}

// AggregatedReadiness 聚合多个命名探针，任意一项失败即整体失败。
// 由 NewRouterWithDependencies 的 /ready 路由消费：失败时把所有失败
// 项的 Name 列在 `failed` 字段里，方便运维一眼看出"是 DB 慢、还是
// queue 还在 seed"。
//
// WHY 不直接用现有的 ReadinessChecker：单一 Ready(ctx) error 接口
// 无法表达"哪一项失败了"；编排器和告警都需要 per-check 粒度才能定向
// 处置（例如 seed 中只是延迟摘流，而 DB 失败要触发 pager）。
type AggregatedReadiness struct {
	checks []ReadinessCheck
}

// NewAggregatedReadiness 构造聚合探针。nil / 空切片合法，等价于"所有
// 检查都通过"，方便测试构造退化运行时。
func NewAggregatedReadiness(checks ...ReadinessCheck) *AggregatedReadiness {
	cleaned := make([]ReadinessCheck, 0, len(checks))
	for _, c := range checks {
		if c.Check == nil {
			continue
		}
		cleaned = append(cleaned, c)
	}
	return &AggregatedReadiness{checks: cleaned}
}

// Ready 顺序跑所有 check：返回 nil 表示全部通过；任一失败时把所有
// 失败项以 errAggregateReadiness 包装的形式返回（携带 FailedNames，
// 供 /ready handler 写到响应体）。
//
// 顺序而不是并行的原因：当前每个 check 都是 sub-millisecond 量级
// （DB Ping 走连接池 / queue.Ready 是原子读），并行只会增加 goroutine
// 调度开销；如果未来有 IO-heavy 探针再换并行。
func (a *AggregatedReadiness) Ready(ctx context.Context) error {
	if a == nil || len(a.checks) == 0 {
		return nil
	}
	var failed []string
	var firstErr error
	for _, c := range a.checks {
		if err := c.Check(ctx); err != nil {
			failed = append(failed, c.Name)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if len(failed) == 0 {
		return nil
	}
	// 排序固定 failed 列表的输出顺序，让 /ready 响应在重复探测之间
	// 保持一致，便于运维直接 diff。
	sort.Strings(failed)
	return &aggregatedReadinessError{failed: failed, cause: firstErr}
}

// aggregatedReadinessError 实现 error + ReadinessFailureLister，让
// /ready handler 取得失败项列表写进 JSON 响应，同时保留底层 error
// 用于结构化日志（不进响应体，避免泄漏内部错误细节）。
type aggregatedReadinessError struct {
	failed []string
	cause  error
}

func (e *aggregatedReadinessError) Error() string {
	if e == nil {
		return ""
	}
	return "readiness check failed: " + joinFailedNames(e.failed)
}

func (e *aggregatedReadinessError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// FailedChecks 返回失败项名字列表（已排序）。返回切片是 e.failed 的
// 拷贝，调用方修改不会影响内部状态。
func (e *aggregatedReadinessError) FailedChecks() []string {
	if e == nil {
		return nil
	}
	out := make([]string, len(e.failed))
	copy(out, e.failed)
	return out
}

// FailedNamesFromReadinessError 从 Ready 返回的 error 中提取失败项
// 列表；如果 err 不是聚合探针生成的失败，返回 nil。/ready handler
// 用它决定是否在响应体里写 `failed` 字段。
func FailedNamesFromReadinessError(err error) []string {
	if err == nil {
		return nil
	}
	var aggErr *aggregatedReadinessError
	if errors.As(err, &aggErr) {
		return aggErr.FailedChecks()
	}
	return nil
}

func joinFailedNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	out := names[0]
	for _, n := range names[1:] {
		out += "," + n
	}
	return out
}
