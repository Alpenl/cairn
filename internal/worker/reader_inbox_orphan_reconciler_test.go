package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"webtag/internal/observability"
	"webtag/internal/service"
)

type inboxOrphanRepairerStub struct {
	mu      sync.Mutex
	calls   []int
	results []service.InboxProposalOrphanRepairResult
	errs    []error
	called  chan int
}

func (s *inboxOrphanRepairerStub) RepairInboxProposalOrphans(_ context.Context, limit int) (service.InboxProposalOrphanRepairResult, error) {
	s.mu.Lock()
	index := len(s.calls)
	s.calls = append(s.calls, limit)
	var result service.InboxProposalOrphanRepairResult
	var err error
	if index < len(s.results) {
		result = s.results[index]
	}
	if index < len(s.errs) {
		err = s.errs[index]
	}
	s.mu.Unlock()
	if s.called != nil {
		s.called <- index + 1
	}
	return result, err
}

func (s *inboxOrphanRepairerStub) callsSnapshot() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.calls...)
}

func TestReaderInboxOrphanReconcilerStartsImmediatelyAndRetriesPeriodically(t *testing.T) {
	t.Parallel()
	repairer := &inboxOrphanRepairerStub{
		errs:   []error{errors.New("temporary repair failure"), nil},
		called: make(chan int, 4),
	}
	reconciler, err := NewReaderInboxOrphanReconciler(ReaderInboxOrphanReconcilerOptions{
		Repairer:  repairer,
		Interval:  10 * time.Millisecond,
		BatchSize: 23,
	})
	if err != nil {
		t.Fatalf("NewReaderInboxOrphanReconciler() error = %v", err)
	}
	if err := reconciler.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	for want := 1; want <= 2; want++ {
		select {
		case got := <-repairer.called:
			if got != want {
				t.Fatalf("repair call sequence = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for repair call %d", want)
		}
	}
	if err := reconciler.Start(context.Background()); !errors.Is(err, ErrBackgroundAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrBackgroundAlreadyStarted", err)
	}
	if err := reconciler.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := reconciler.Start(context.Background()); !errors.Is(err, ErrBackgroundStopped) {
		t.Fatalf("Start() after Stop error = %v, want ErrBackgroundStopped", err)
	}
	for index, got := range repairer.callsSnapshot() {
		if got != 23 {
			t.Fatalf("repair call %d batch = %d, want 23", index, got)
		}
	}
}

func TestReaderInboxOrphanReconcilerForwardsBatchAndRecordsBoundedMetrics(t *testing.T) {
	t.Parallel()
	metrics := observability.NewMetrics()
	repairer := &inboxOrphanRepairerStub{
		results: []service.InboxProposalOrphanRepairResult{{Claimed: 3, Repaired: 3}, {Claimed: 1}},
		errs:    []error{nil, errors.New("insert failed")},
	}
	reconciler, err := NewReaderInboxOrphanReconciler(ReaderInboxOrphanReconcilerOptions{
		Repairer:  repairer,
		BatchSize: 37,
		Metrics:   metrics,
	})
	if err != nil {
		t.Fatalf("NewReaderInboxOrphanReconciler() error = %v", err)
	}
	if _, err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce() error = %v", err)
	}
	if _, err := reconciler.RunOnce(context.Background()); err == nil {
		t.Fatal("second RunOnce() error = nil, want injected failure")
	}
	if got := repairer.callsSnapshot(); len(got) != 2 || got[0] != 37 || got[1] != 37 {
		t.Fatalf("repair batches = %v, want [37 37]", got)
	}
	if got := testutil.ToFloat64(metrics.ReaderInboxDispatchRepairsTotal.WithLabelValues("success")); got != 3 {
		t.Fatalf("success repair metric = %v, want 3", got)
	}
	if got := testutil.ToFloat64(metrics.ReaderInboxDispatchRepairsTotal.WithLabelValues("failure")); got != 1 {
		t.Fatalf("failure repair metric = %v, want 1", got)
	}
}

type blockingInboxOrphanRepairer struct {
	entered chan struct{}
	mu      sync.Mutex
	calls   int
}

func (s *blockingInboxOrphanRepairer) RepairInboxProposalOrphans(ctx context.Context, _ int) (service.InboxProposalOrphanRepairResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return service.InboxProposalOrphanRepairResult{}, ctx.Err()
}

func TestReaderInboxOrphanReconcilerRunGatePreventsOverlappingPasses(t *testing.T) {
	t.Parallel()
	repairer := &blockingInboxOrphanRepairer{entered: make(chan struct{}, 1)}
	reconciler, err := NewReaderInboxOrphanReconciler(ReaderInboxOrphanReconcilerOptions{
		Repairer:     repairer,
		RoundTimeout: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewReaderInboxOrphanReconciler() error = %v", err)
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, runErr := reconciler.RunOnce(firstCtx)
		firstDone <- runErr
	}()
	select {
	case <-repairer.entered:
	case <-time.After(time.Second):
		t.Fatal("first RunOnce() did not enter repairer")
	}

	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelSecond()
	if _, err := reconciler.RunOnce(secondCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overlapping RunOnce() error = %v, want deadline while waiting for gate", err)
	}
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first RunOnce() error = %v, want context.Canceled", err)
	}
	repairer.mu.Lock()
	calls := repairer.calls
	repairer.mu.Unlock()
	if calls != 1 {
		t.Fatalf("repairer calls = %d, want exactly one non-overlapping call", calls)
	}
}
