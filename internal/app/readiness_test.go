package app

import (
	"context"
	"errors"
	"testing"
)

func TestDatabaseReadinessReadyDelegatesToPoolPing(t *testing.T) {
	t.Parallel()

	pingErr := errors.New("ping failed")
	readiness := NewDatabaseReadiness(readinessFakePool{pingErr: pingErr})

	err := readiness.Ready(context.Background())
	if !errors.Is(err, pingErr) {
		t.Fatalf("Ready() error = %v, want %v", err, pingErr)
	}
}

type readinessFakePool struct {
	pingErr error
}

func (p readinessFakePool) Ping(context.Context) error {
	return p.pingErr
}

// TestAggregatedReadinessAllChecksPassReturnsNil 锁定：所有 check 返回 nil
// 时整体 nil，且 FailedNamesFromReadinessError 返回 nil。
func TestAggregatedReadinessAllChecksPassReturnsNil(t *testing.T) {
	t.Parallel()

	agg := NewAggregatedReadiness(
		ReadinessCheck{Name: "database", Check: func(context.Context) error { return nil }},
		ReadinessCheck{Name: "queue", Check: func(context.Context) error { return nil }},
	)

	if err := agg.Ready(context.Background()); err != nil {
		t.Fatalf("Ready() error = %v, want nil", err)
	}
}

// TestAggregatedReadinessReportsFailedNames 锁定：任一 check 失败时返回
// 的 error 能通过 FailedNamesFromReadinessError 解出失败项名字（按 Name
// 字典序排序），并且包装的 cause 仍可通过 errors.Is 还原。
func TestAggregatedReadinessReportsFailedNames(t *testing.T) {
	t.Parallel()

	dbErr := errors.New("db down")
	queueErr := errors.New("queue not seeded")
	agg := NewAggregatedReadiness(
		ReadinessCheck{Name: "queue", Check: func(context.Context) error { return queueErr }},
		ReadinessCheck{Name: "database", Check: func(context.Context) error { return dbErr }},
	)

	err := agg.Ready(context.Background())
	if err == nil {
		t.Fatal("Ready() error = nil, want failure")
	}
	failed := FailedNamesFromReadinessError(err)
	if len(failed) != 2 || failed[0] != "database" || failed[1] != "queue" {
		t.Fatalf("FailedNamesFromReadinessError = %#v, want sorted [database queue]", failed)
	}
	// 锁定：第一项失败的 cause 透传，便于结构化日志保留细节。
	if !errors.Is(err, queueErr) {
		t.Fatalf("errors.Is(err, queueErr) = false; err = %v", err)
	}
}

// TestAggregatedReadinessSinglePartialFailureLeavesPassingChecksAlone
// 锁定：聚合探针不会因为某项失败而短路后续 check —— 所有 check 都跑过，
// 即使某些已经失败，让 failed 列表完整。这对运维同时看到"DB 慢 + queue
// 也没 seed 完"两个信号至关重要。
func TestAggregatedReadinessSinglePartialFailureLeavesPassingChecksAlone(t *testing.T) {
	t.Parallel()

	var dbCalled, queueCalled bool
	agg := NewAggregatedReadiness(
		ReadinessCheck{Name: "database", Check: func(context.Context) error {
			dbCalled = true
			return errors.New("db failed")
		}},
		ReadinessCheck{Name: "queue", Check: func(context.Context) error {
			queueCalled = true
			return nil
		}},
	)

	err := agg.Ready(context.Background())
	if err == nil {
		t.Fatal("Ready() error = nil, want failure")
	}
	if !dbCalled || !queueCalled {
		t.Fatalf("calls: db=%v queue=%v, want both true (no short-circuit)", dbCalled, queueCalled)
	}
	failed := FailedNamesFromReadinessError(err)
	if len(failed) != 1 || failed[0] != "database" {
		t.Fatalf("failed = %#v, want [database]", failed)
	}
}

// TestAggregatedReadinessNilOrEmptyReturnsNil 锁定边界：nil 接收者与
// 空 check 列表都视为"全通过"，方便测试构造退化运行时。
func TestAggregatedReadinessNilOrEmptyReturnsNil(t *testing.T) {
	t.Parallel()

	var nilAgg *AggregatedReadiness
	if err := nilAgg.Ready(context.Background()); err != nil {
		t.Fatalf("nil receiver Ready() = %v, want nil", err)
	}
	empty := NewAggregatedReadiness()
	if err := empty.Ready(context.Background()); err != nil {
		t.Fatalf("empty Ready() = %v, want nil", err)
	}
}

// TestFailedNamesFromReadinessErrorReturnsNilForUnrelatedError 锁定：
// 不是聚合探针生成的 error（如旧的 DatabaseReadiness 直接返回 ping err）
// 走原路径，不会被错误地包装。
func TestFailedNamesFromReadinessErrorReturnsNilForUnrelatedError(t *testing.T) {
	t.Parallel()

	if got := FailedNamesFromReadinessError(errors.New("plain")); got != nil {
		t.Fatalf("FailedNamesFromReadinessError(plain) = %#v, want nil", got)
	}
	if got := FailedNamesFromReadinessError(nil); got != nil {
		t.Fatalf("FailedNamesFromReadinessError(nil) = %#v, want nil", got)
	}
}
