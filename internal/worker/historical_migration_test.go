package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

type migrationWorkerStoreFake struct {
	rows          []repository.HistoricalMigrationCandidate
	commitOutcome repository.HistoricalMigrationOutcome
	commitErr     error
}

type ownerBlockingMigrationStore struct {
	entered chan struct{}
}

func (s *ownerBlockingMigrationStore) ListHistoricalMigrationCandidates(ctx context.Context, _ repository.HistoricalMigrationCursor, _ int) ([]repository.HistoricalMigrationCandidate, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*ownerBlockingMigrationStore) CommitHistoricalMigrationAssessment(context.Context, repository.HistoricalMigrationAssessment) (repository.HistoricalMigrationOutcome, error) {
	panic("CommitHistoricalMigrationAssessment must not run when listing is blocked")
}

func (f *migrationWorkerStoreFake) ListHistoricalMigrationCandidates(_ context.Context, _ repository.HistoricalMigrationCursor, _ int) ([]repository.HistoricalMigrationCandidate, error) {
	out := f.rows
	f.rows = nil
	return out, nil
}
func (f *migrationWorkerStoreFake) CommitHistoricalMigrationAssessment(context.Context, repository.HistoricalMigrationAssessment) (repository.HistoricalMigrationOutcome, error) {
	if f.commitErr != nil {
		return repository.HistoricalMigrationOutcomeNoop, f.commitErr
	}
	if f.commitOutcome != "" {
		return f.commitOutcome, nil
	}
	return repository.HistoricalMigrationOutcomeAutoMigrated, nil
}

func TestHistoricalMigrationWorkerRunsDryRunWithoutMutatingStore(t *testing.T) {
	t.Parallel()
	store := &migrationWorkerStoreFake{rows: []repository.HistoricalMigrationCandidate{migrationWorkerCandidate()}}
	w, err := NewHistoricalMigrationWorker(HistoricalMigrationWorkerOptions{Runner: service.NewHistoricalMigrationRunner(store), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	report, err := w.RunOnce(context.Background())
	if err != nil || report.AutoMigrated != 0 || report.WouldAutoMigrate != 1 {
		t.Fatalf("RunOnce() = %#v, %v", report, err)
	}
}

// runIteration 的日志分支必须自己有守卫。上一版把 skipped / retained 挂在
// `err != nil` 那条分支下，而「候选全部因为 CAS 冲突被跳过」的 run 是**成功**
// 返回的——那条 Warn 因此永远不触发，一轮零推进的 run 在日志里与什么都没发生
// 长得一模一样。那个不可达分支是靠人眼审出来的，不该继续靠人眼守着。
func TestHistoricalMigrationWorkerLogsSkippedOnSuccessfulRun(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &migrationWorkerStoreFake{
		rows:          []repository.HistoricalMigrationCandidate{migrationWorkerCandidate()},
		commitOutcome: repository.HistoricalMigrationOutcomeNoop,
	}
	w := newLoggingMigrationWorker(t, store, &buf)

	w.runIteration(context.Background())

	line := buf.String()
	// 这一轮**没有出错**，所以它只可能来自成功路径那条日志。
	if !strings.Contains(line, `"level":"WARN"`) || !strings.Contains(line, `"skipped":1`) {
		t.Fatalf("runIteration 日志 = %q；成功但零推进的一轮必须自己发声（含 skipped 计数）", line)
	}
	if strings.Contains(line, `"error"`) {
		t.Fatalf("runIteration 日志 = %q；这一轮成功返回，不该带 error 字段", line)
	}
	// 「恰好一条」也要钉住：漏掉 Warn 那支的 return，同一轮会 Warn + Info 各打
	// 一条，而只用 Contains 断言的话两条都在、照样绿。
	if got := strings.Count(strings.TrimSpace(line), "\n") + 1; got != 1 {
		t.Fatalf("runIteration 打了 %d 条日志，want 1：%q", got, line)
	}
}

// 与上一条相对：绝大多数轮次无事可做，必须保持安静。否则每 15 分钟一条
// scanned=0 会把真正需要被看见的那两条淹掉。
func TestHistoricalMigrationWorkerStaysQuietWhenNothingScanned(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := newLoggingMigrationWorker(t, &migrationWorkerStoreFake{}, &buf)

	w.runIteration(context.Background())

	if buf.Len() != 0 {
		t.Fatalf("空转的一轮打了日志：%q", buf.String())
	}
}

// 有实际推进的一轮打 Info：既证明成功路径确实会发声，也钉住它不是 Warn。
func TestHistoricalMigrationWorkerLogsCompletedRunAtInfo(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &migrationWorkerStoreFake{rows: []repository.HistoricalMigrationCandidate{migrationWorkerCandidate()}}
	w := newLoggingMigrationWorker(t, store, &buf)

	w.runIteration(context.Background())

	line := buf.String()
	if !strings.Contains(line, `"level":"INFO"`) || !strings.Contains(line, `"scanned":1`) {
		t.Fatalf("runIteration 日志 = %q；有推进的一轮该在 Info 上报出计数", line)
	}
}

func newLoggingMigrationWorker(t *testing.T, store service.HistoricalMigrationStore, buf *bytes.Buffer) *HistoricalMigrationWorker {
	t.Helper()
	w, err := NewHistoricalMigrationWorker(HistoricalMigrationWorkerOptions{
		Runner: service.NewHistoricalMigrationRunner(store),
		Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func migrationWorkerCandidate() repository.HistoricalMigrationCandidate {
	return repository.HistoricalMigrationCandidate{
		ID:              uuid.New(),
		URL:             "https://example.com/",
		ContentType:     model.ContentTypeHomepage,
		ContentRevision: 1,
		CreatedAt:       time.Now().UTC(),
		LibraryKind:     model.LibraryKindReading,
		Source:          model.LibraryKindSourceMigration,
	}
}

func TestHistoricalMigrationWorkerRequiresRunner(t *testing.T) {
	t.Parallel()
	if _, err := NewHistoricalMigrationWorker(HistoricalMigrationWorkerOptions{}); err == nil {
		t.Fatal("nil runner must be rejected")
	}
}

func TestHistoricalMigrationWorkerUsesBoundedLinearLifecycle(t *testing.T) {
	t.Parallel()

	worker, err := NewHistoricalMigrationWorker(HistoricalMigrationWorkerOptions{
		Runner: service.NewHistoricalMigrationRunner(&migrationWorkerStoreFake{}), Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewHistoricalMigrationWorker() error = %v", err)
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := worker.Start(context.Background()); !errors.Is(err, ErrBackgroundStopped) {
		t.Fatalf("Start() after Stop error = %v, want ErrBackgroundStopped", err)
	}
}

func TestHistoricalMigrationWorkerOwnerCancellationInterruptsActiveStoreCall(t *testing.T) {
	t.Parallel()

	store := &ownerBlockingMigrationStore{entered: make(chan struct{}, 1)}
	worker, err := NewHistoricalMigrationWorker(HistoricalMigrationWorkerOptions{
		Runner:   service.NewHistoricalMigrationRunner(store),
		Interval: time.Hour,
		Timeout:  time.Hour,
	})
	if err != nil {
		t.Fatalf("NewHistoricalMigrationWorker() error = %v", err)
	}
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()

	if err := worker.Start(ownerCtx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("historical migration store call did not start")
	}

	cancelOwner()
	joinCtx, cancelJoin := context.WithTimeout(context.Background(), time.Second)
	defer cancelJoin()
	if err := worker.Stop(joinCtx); err != nil {
		t.Fatalf("Stop() after owner cancellation error = %v", err)
	}
}
