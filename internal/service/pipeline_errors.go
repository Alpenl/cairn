package service

import "webtag/internal/errsafe"

// PipelineRunError wraps a fetch/analyze failure that the pipeline has
// already persisted to the link/job rows. The queue (or any other
// supervisor) detects this state via errors.Is(err, errsafe.ErrAlreadyPersisted)
// so it can suppress its own redundant persist step without importing
// the service package directly.
type PipelineRunError struct {
	Cause error
}

// Error 返回底层 Cause 的错误信息；Cause 为空时退化为「已持久化」哨兵描述。
func (e *PipelineRunError) Error() string {
	if e == nil || e.Cause == nil {
		return errsafe.ErrAlreadyPersisted.Error()
	}
	return e.Cause.Error()
}

// Unwrap 暴露底层错误以配合 errors.Is/As 沿原始错误链匹配具体分类。
func (e *PipelineRunError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is only declares membership for errsafe.ErrAlreadyPersisted — that single
// sentinel is the contract callers (e.g. the worker queue) rely on to know
// the pipeline already wrote the failure to its rows. Any other sentinel
// must be matched against the wrapped Cause via the standard Unwrap chain;
// do not extend this method to claim unrelated sentinels just because they
// happen to flow through here.
func (e *PipelineRunError) Is(target error) bool {
	return target == errsafe.ErrAlreadyPersisted
}
