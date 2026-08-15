package worker

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/service"
)

type fakeParseTerminalStore struct {
	mu                sync.Mutex
	candidates        []parseTerminalCandidate
	active            map[int64]bool
	missingAttempts   []parseMissingAttempt
	terminalErr       error
	missingAttemptErr error
	listed            chan struct{}
	terminalCursors   []int64
	missingCursors    []parseMissingCursor
}

func (s *fakeParseTerminalStore) ListMismatches(
	_ context.Context,
	afterID int64,
	limit int,
) ([]parseTerminalCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminalCursors = append(s.terminalCursors, afterID)
	if s.listed != nil {
		select {
		case s.listed <- struct{}{}:
		default:
		}
	}
	if s.terminalErr != nil {
		return nil, s.terminalErr
	}
	items := append([]parseTerminalCandidate(nil), s.candidates...)
	sort.Slice(items, func(i, j int) bool { return items[i].riverJobID < items[j].riverJobID })
	out := make([]parseTerminalCandidate, 0, limit)
	for _, candidate := range items {
		if candidate.riverJobID <= afterID {
			continue
		}
		if s.active != nil && !s.active[candidate.riverJobID] {
			continue
		}
		out = append(out, candidate)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeParseTerminalStore) ListMissingAttempts(
	_ context.Context,
	_ time.Time,
	cursor parseMissingCursor,
	limit int,
) ([]parseMissingAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missingCursors = append(s.missingCursors, cursor)
	if s.missingAttemptErr != nil {
		return nil, s.missingAttemptErr
	}
	items := append([]parseMissingAttempt(nil), s.missingAttempts...)
	sort.Slice(items, func(i, j int) bool {
		if !items[i].updatedAt.Equal(items[j].updatedAt) {
			return items[i].updatedAt.Before(items[j].updatedAt)
		}
		return bytesAfterUUID(items[i].parseJobID, items[j].parseJobID)
	})
	out := make([]parseMissingAttempt, 0, limit)
	for _, item := range items {
		if !parseMissingCursorAfter(item, cursor) {
			continue
		}
		out = append(out, item)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeParseTerminalStore) resolve(riverJobID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		s.active[riverJobID] = false
	}
}

func (s *fakeParseTerminalStore) resolveMissing(parseJobID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, item := range s.missingAttempts {
		if item.parseJobID == parseJobID {
			s.missingAttempts = append(s.missingAttempts[:index], s.missingAttempts[index+1:]...)
			return
		}
	}
}

type fakeParseTerminalProjector struct {
	mu             sync.Mutex
	calls          map[uuid.UUID]int
	failuresByLink map[uuid.UUID]int
	riverByLink    map[uuid.UUID]int64
	store          *fakeParseTerminalStore
	causes         map[uuid.UUID]error
}

func (p *fakeParseTerminalProjector) RecordDiscard(
	_ context.Context,
	linkID, parseJobID uuid.UUID,
	cause error,
) error {
	p.mu.Lock()
	if p.calls == nil {
		p.calls = make(map[uuid.UUID]int)
	}
	p.calls[linkID]++
	if p.causes == nil {
		p.causes = make(map[uuid.UUID]error)
	}
	p.causes[linkID] = cause
	if p.failuresByLink[linkID] > 0 {
		p.failuresByLink[linkID]--
		p.mu.Unlock()
		return errors.New("projection database unavailable")
	}
	riverJobID := p.riverByLink[linkID]
	p.mu.Unlock()
	if p.store != nil {
		p.store.resolve(riverJobID)
		p.store.resolveMissing(parseJobID)
	}
	return nil
}

func (p *fakeParseTerminalProjector) callCount(linkID uuid.UUID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[linkID]
}

func (p *fakeParseTerminalProjector) causeFor(linkID uuid.UUID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.causes[linkID]
}

func terminalCandidate(riverJobID int64, state rivertype.JobState) parseTerminalCandidate {
	return parseTerminalCandidate{
		riverJobID: riverJobID,
		state:      state,
		linkID:     uuid.New(),
		parseJobID: uuid.New(),
	}
}

func missingParseAttempt(updatedAt time.Time) parseMissingAttempt {
	return parseMissingAttempt{linkID: uuid.New(), parseJobID: uuid.New(), updatedAt: updatedAt}
}

func TestParseTerminalReconcilerRetriesProjectionFailureOnNextPass(t *testing.T) {
	t.Parallel()
	candidate := terminalCandidate(41, rivertype.JobStateDiscarded)
	store := &fakeParseTerminalStore{
		candidates: []parseTerminalCandidate{candidate},
		active:     map[int64]bool{41: true},
	}
	projector := &fakeParseTerminalProjector{
		failuresByLink: map[uuid.UUID]int{candidate.linkID: 1},
		riverByLink:    map[uuid.UUID]int64{candidate.linkID: 41},
		store:          store,
	}
	reconciler := newParseTerminalReconciler(store, projector, time.Hour, 10, nil)

	first, err := reconciler.RunOnce(context.Background())
	if err == nil || first.Scanned != 1 || first.Reconciled != 0 || first.Failed != 1 {
		t.Fatalf("first RunOnce() = %+v, %v, want retained projection failure", first, err)
	}
	second, err := reconciler.RunOnce(context.Background())
	if err != nil || second.Scanned != 1 || second.Reconciled != 1 || second.Failed != 0 {
		t.Fatalf("second RunOnce() = %+v, %v, want successful retry", second, err)
	}
	third, err := reconciler.RunOnce(context.Background())
	if err != nil || third.Scanned != 0 || projector.callCount(candidate.linkID) != 2 {
		t.Fatalf("third RunOnce() = %+v, %v calls=%d, want settled no-op", third, err, projector.callCount(candidate.linkID))
	}
}

func TestParseTerminalReconcilerFailureDoesNotStarveLaterJobs(t *testing.T) {
	t.Parallel()
	first := terminalCandidate(1, rivertype.JobStateDiscarded)
	second := terminalCandidate(2, rivertype.JobStateCompleted)
	store := &fakeParseTerminalStore{
		candidates: []parseTerminalCandidate{first, second},
		active:     map[int64]bool{1: true, 2: true},
	}
	projector := &fakeParseTerminalProjector{
		failuresByLink: map[uuid.UUID]int{first.linkID: 10},
		riverByLink: map[uuid.UUID]int64{
			first.linkID: 1, second.linkID: 2,
		},
		store: store,
	}
	reconciler := newParseTerminalReconciler(store, projector, time.Hour, 1, nil)

	result, err := reconciler.RunOnce(context.Background())
	if err == nil || result.Scanned != 2 || result.Reconciled != 1 || result.Failed != 1 {
		t.Fatalf("RunOnce() = %+v, %v, want later job reconciled after first failure", result, err)
	}
	next, err := reconciler.RunOnce(context.Background())
	if err == nil || next.Scanned != 1 || projector.callCount(second.linkID) != 1 {
		t.Fatalf("next RunOnce() = %+v, %v, want only failed mismatch retried", next, err)
	}
}

func TestParseTerminalReconcilerProjectsMissingAttempt(t *testing.T) {
	t.Parallel()
	item := missingParseAttempt(time.Now().Add(-8 * time.Hour))
	store := &fakeParseTerminalStore{active: map[int64]bool{}, missingAttempts: []parseMissingAttempt{item}}
	projector := &fakeParseTerminalProjector{store: store}
	reconciler := newParseTerminalReconciler(store, projector, time.Hour, 10, nil)

	result, err := reconciler.RunOnce(context.Background())
	if err != nil || result.Scanned != 1 || result.Reconciled != 1 || result.Missing != 1 || result.Failed != 0 {
		t.Fatalf("RunOnce() = %+v, %v, want one successful missing recovery", result, err)
	}
	if cause := projector.causeFor(item.linkID); !errors.Is(cause, service.ErrParseJobMissing) {
		t.Fatalf("projection cause = %v, want ErrParseJobMissing", cause)
	}
	next, err := reconciler.RunOnce(context.Background())
	if err != nil || next.Scanned != 0 || projector.callCount(item.linkID) != 1 {
		t.Fatalf("second RunOnce() = %+v, %v, want settled no-op", next, err)
	}
}

func TestParseTerminalReconcilerMissingFailureDoesNotStarveLaterAttempts(t *testing.T) {
	t.Parallel()
	at := time.Now().Add(-8 * time.Hour)
	first := missingParseAttempt(at)
	second := missingParseAttempt(at.Add(time.Second))
	store := &fakeParseTerminalStore{active: map[int64]bool{}, missingAttempts: []parseMissingAttempt{first, second}}
	projector := &fakeParseTerminalProjector{
		store:          store,
		failuresByLink: map[uuid.UUID]int{first.linkID: 1},
	}
	reconciler := newParseTerminalReconciler(store, projector, time.Hour, 1, nil)

	result, err := reconciler.RunOnce(context.Background())
	if err == nil || result.Scanned != 2 || result.Reconciled != 1 || result.Missing != 1 || result.Failed != 1 {
		t.Fatalf("first RunOnce() = %+v, %v, want later missing attempt reconciled", result, err)
	}
	result, err = reconciler.RunOnce(context.Background())
	if err != nil || result.Scanned != 1 || result.Reconciled != 1 || result.Missing != 1 {
		t.Fatalf("second RunOnce() = %+v, %v, want failed attempt retried", result, err)
	}
}

func TestParseTerminalReconcilerRejectsInvalidAttemptIdentities(t *testing.T) {
	t.Parallel()
	terminal := terminalCandidate(1, rivertype.JobStateDiscarded)
	terminal.linkID = uuid.Nil
	missing := missingParseAttempt(time.Now().Add(-8 * time.Hour))
	missing.parseJobID = uuid.Nil
	store := &fakeParseTerminalStore{
		candidates:      []parseTerminalCandidate{terminal},
		active:          map[int64]bool{1: true},
		missingAttempts: []parseMissingAttempt{missing},
	}
	reconciler := newParseTerminalReconciler(store, &fakeParseTerminalProjector{}, time.Hour, 10, nil)

	result, err := reconciler.RunOnce(context.Background())
	if err == nil || result.Invalid != 2 || result.Scanned != 0 || result.Reconciled != 0 {
		t.Fatalf("RunOnce() = %+v, %v, want two invalid identities rejected", result, err)
	}
}

func TestParseTerminalReconcilerDiscoveryErrorsFailClosed(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("discovery unavailable")
	for _, testCase := range []struct {
		name  string
		store *fakeParseTerminalStore
	}{
		{name: "terminal", store: &fakeParseTerminalStore{terminalErr: wantErr}},
		{name: "missing", store: &fakeParseTerminalStore{missingAttemptErr: wantErr}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reconciler := newParseTerminalReconciler(testCase.store, &fakeParseTerminalProjector{}, time.Hour, 10, nil)
			result, err := reconciler.RunOnce(context.Background())
			if !errors.Is(err, wantErr) || result.Reconciled != 0 {
				t.Fatalf("RunOnce() = %+v, %v, want fail-closed discovery error", result, err)
			}
		})
	}
}

type nonIncreasingParseStore struct {
	terminal []parseTerminalCandidate
	missing  []parseMissingAttempt
}

func (s *nonIncreasingParseStore) ListMismatches(context.Context, int64, int) ([]parseTerminalCandidate, error) {
	return append([]parseTerminalCandidate(nil), s.terminal...), nil
}

func (s *nonIncreasingParseStore) ListMissingAttempts(context.Context, time.Time, parseMissingCursor, int) ([]parseMissingAttempt, error) {
	return append([]parseMissingAttempt(nil), s.missing...), nil
}

func TestParseTerminalReconcilerFailsClosedOnNonIncreasingCursors(t *testing.T) {
	t.Parallel()
	terminal := terminalCandidate(1, rivertype.JobStateDiscarded)
	missing := missingParseAttempt(time.Now().Add(-8 * time.Hour))
	for _, testCase := range []struct {
		name  string
		store parseTerminalStore
	}{
		{name: "terminal", store: &nonIncreasingParseStore{terminal: []parseTerminalCandidate{terminal, terminal}}},
		{name: "missing", store: &nonIncreasingParseStore{missing: []parseMissingAttempt{missing, missing}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			projector := &fakeParseTerminalProjector{}
			reconciler := newParseTerminalReconciler(testCase.store, projector, time.Hour, 10, nil)
			result, err := reconciler.RunOnce(context.Background())
			if err == nil || !strings.Contains(err.Error(), "non-increasing cursor") {
				t.Fatalf("RunOnce() error = %v, want non-increasing cursor", err)
			}
			if result.Scanned != 1 || result.Reconciled != 1 {
				t.Fatalf("result = %+v, want exactly one projection before rejection", result)
			}
		})
	}
}

type blockingParseTerminalStore struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingParseTerminalStore) ListMismatches(ctx context.Context, _ int64, _ int) ([]parseTerminalCandidate, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*blockingParseTerminalStore) ListMissingAttempts(context.Context, time.Time, parseMissingCursor, int) ([]parseMissingAttempt, error) {
	return nil, nil
}

type ownerBlockingParseTerminalStore struct {
	entered chan struct{}
}

func (s *ownerBlockingParseTerminalStore) ListMismatches(ctx context.Context, _ int64, _ int) ([]parseTerminalCandidate, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*ownerBlockingParseTerminalStore) ListMissingAttempts(context.Context, time.Time, parseMissingCursor, int) ([]parseMissingAttempt, error) {
	return nil, nil
}

type partitionedParseTerminalStore struct {
	item parseMissingAttempt
}

func (*partitionedParseTerminalStore) ListMismatches(ctx context.Context, _ int64, _ int) ([]parseTerminalCandidate, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *partitionedParseTerminalStore) ListMissingAttempts(
	ctx context.Context,
	_ time.Time,
	cursor parseMissingCursor,
	_ int,
) ([]parseMissingAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parseMissingCursorAfter(s.item, cursor) {
		return []parseMissingAttempt{s.item}, nil
	}
	return nil, nil
}

func TestParseTerminalReconcilerReservesRoundBudgetForMissingRecovery(t *testing.T) {
	t.Parallel()
	item := missingParseAttempt(time.Now().Add(-8 * time.Hour))
	store := &partitionedParseTerminalStore{item: item}
	projector := &fakeParseTerminalProjector{}
	reconciler := newParseTerminalReconciler(store, projector, time.Hour, 10, nil)
	reconciler.roundTimeout = 200 * time.Millisecond

	result, err := reconciler.RunOnce(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunOnce() error = %v, want terminal child deadline", err)
	}
	if result.Reconciled != 1 || result.Missing != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want missing recovery to use reserved budget", result)
	}
}

func TestParseTerminalReconcilerRunGateHonorsContext(t *testing.T) {
	t.Parallel()
	store := &blockingParseTerminalStore{entered: make(chan struct{}, 1), release: make(chan struct{})}
	reconciler := newParseTerminalReconciler(store, &fakeParseTerminalProjector{}, time.Hour, 10, nil)
	firstDone := make(chan error, 1)
	go func() {
		_, err := reconciler.RunOnce(context.Background())
		firstDone <- err
	}()
	<-store.entered

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelWait()
	_, err := reconciler.RunOnce(waitCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second RunOnce() error = %v, want context deadline exceeded", err)
	}
	close(store.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first RunOnce() error = %v", err)
	}
}

func TestParseTerminalReconcilerRunOnceAppliesConfiguredRoundTimeout(t *testing.T) {
	t.Parallel()
	store := &ownerBlockingParseTerminalStore{entered: make(chan struct{}, 1)}
	reconciler := newParseTerminalReconciler(store, &fakeParseTerminalProjector{}, time.Hour, 10, nil)
	reconciler.roundTimeout = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := reconciler.RunOnce(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunOnce() error = %v, want configured deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunOnce(context.Background()) exceeded configured round timeout")
	}
}

func TestParseTerminalReconcilerStartRunsImmediatelyAndLifecycleIsLinear(t *testing.T) {
	t.Parallel()
	listed := make(chan struct{}, 1)
	store := &fakeParseTerminalStore{listed: listed, active: map[int64]bool{}}
	reconciler := newParseTerminalReconciler(store, &fakeParseTerminalProjector{}, time.Hour, 10, nil)

	if err := reconciler.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-listed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("startup reconciliation did not run immediately")
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
}

func TestParseTerminalReconcilerOwnerCancellationInterruptsActiveStoreCall(t *testing.T) {
	t.Parallel()
	store := &ownerBlockingParseTerminalStore{entered: make(chan struct{}, 1)}
	reconciler := newParseTerminalReconciler(store, &fakeParseTerminalProjector{}, time.Hour, 10, nil)
	reconciler.roundTimeout = time.Hour
	ownerCtx, cancelOwner := context.WithCancel(context.Background())

	if err := reconciler.Start(ownerCtx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("parse terminal store call did not start")
	}
	cancelOwner()
	joinCtx, cancelJoin := context.WithTimeout(context.Background(), time.Second)
	defer cancelJoin()
	if err := reconciler.Stop(joinCtx); err != nil {
		t.Fatalf("Stop() after owner cancellation error = %v", err)
	}
}
