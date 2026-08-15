package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/model"
	"webtag/internal/service/linktranslation"
)

type fakeTranslationTerminalStore struct {
	mu sync.Mutex

	terminals []translationTerminalJob
	missing   []translationMissingAttempt

	latestErr   error
	terminalErr error
	missingErr  error
	inspectErr  error

	inspectStates map[uuid.UUID]translationTerminalProjectionState
	inspectFound  map[uuid.UUID]bool

	latestCalls     int
	terminalCursors []translationTerminalCursor
	missingCursors  []translationMissingCursor
	missingCutoffs  []time.Time
	latestNotify    chan struct{}
}

func (s *fakeTranslationTerminalStore) LatestTerminalCursor(
	ctx context.Context,
) (translationTerminalCursor, bool, error) {
	if err := ctx.Err(); err != nil {
		return translationTerminalCursor{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latestCalls++
	if s.latestNotify != nil {
		select {
		case s.latestNotify <- struct{}{}:
		default:
		}
	}
	if s.latestErr != nil {
		return translationTerminalCursor{}, false, s.latestErr
	}
	var latest translationTerminalCursor
	for _, job := range s.terminals {
		cursor := terminalCursorForJob(job)
		if terminalCursorZero(latest) || terminalCursorAfterCursor(cursor, latest) {
			latest = cursor
		}
	}
	if terminalCursorZero(latest) {
		return translationTerminalCursor{}, false, nil
	}
	return latest, true, nil
}

func (s *fakeTranslationTerminalStore) ListTerminalJobs(
	ctx context.Context,
	cursor translationTerminalCursor,
	limit int,
) ([]translationTerminalJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminalCursors = append(s.terminalCursors, cursor)
	if s.terminalErr != nil {
		return nil, s.terminalErr
	}
	jobs := make([]translationTerminalJob, 0, len(s.terminals))
	for _, job := range s.terminals {
		if terminalCursorAfter(job, cursor) {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		return terminalCursorAfter(jobs[j], terminalCursorForJob(jobs[i]))
	})
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return append([]translationTerminalJob(nil), jobs...), nil
}

func (s *fakeTranslationTerminalStore) ListMissingAttempts(
	ctx context.Context,
	before time.Time,
	cursor translationMissingCursor,
	limit int,
) ([]translationMissingAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missingCursors = append(s.missingCursors, cursor)
	s.missingCutoffs = append(s.missingCutoffs, before)
	if s.missingErr != nil {
		return nil, s.missingErr
	}
	attempts := make([]translationMissingAttempt, 0, len(s.missing))
	for _, item := range s.missing {
		if item.updatedAt.Before(before) && missingCursorAfter(item, cursor) {
			attempts = append(attempts, item)
		}
	}
	sort.Slice(attempts, func(i, j int) bool {
		if attempts[i].updatedAt.Equal(attempts[j].updatedAt) {
			return attempts[i].attempt.TranslationID.String() < attempts[j].attempt.TranslationID.String()
		}
		return attempts[i].updatedAt.Before(attempts[j].updatedAt)
	})
	if len(attempts) > limit {
		attempts = attempts[:limit]
	}
	return append([]translationMissingAttempt(nil), attempts...), nil
}

func (s *fakeTranslationTerminalStore) InspectTerminalProjection(
	ctx context.Context,
	attempt model.TranslationAttempt,
	_ bool,
) (translationTerminalProjectionState, bool, error) {
	if err := ctx.Err(); err != nil {
		return translationTerminalProjectionState{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inspectErr != nil {
		return translationTerminalProjectionState{}, false, s.inspectErr
	}
	state, exists := s.inspectStates[attempt.TranslationID]
	if s.inspectFound != nil {
		exists = s.inspectFound[attempt.TranslationID]
	}
	return state, exists, nil
}

func (s *fakeTranslationTerminalStore) addTerminal(job translationTerminalJob) {
	s.mu.Lock()
	s.terminals = append(s.terminals, job)
	s.mu.Unlock()
}

type translationProjectionCall struct {
	attempt model.TranslationAttempt
	code    string
	missing bool
}

type fakeTranslationTerminalProjector struct {
	mu sync.Mutex

	calls           []translationProjectionCall
	applyOnce       bool
	applied         map[int64]bool
	failures        map[int64]int
	forceNotApplied map[int64]bool
	hook            func(context.Context, translationProjectionCall) error
}

func (p *fakeTranslationTerminalProjector) Fail(
	ctx context.Context,
	attempt model.TranslationAttempt,
	message string,
) (bool, error) {
	return p.project(ctx, translationProjectionCall{attempt: attempt, code: message})
}

func (p *fakeTranslationTerminalProjector) FailMissing(
	ctx context.Context,
	attempt model.TranslationAttempt,
	message string,
) (bool, error) {
	return p.project(ctx, translationProjectionCall{attempt: attempt, code: message, missing: true})
}

func (p *fakeTranslationTerminalProjector) project(
	ctx context.Context,
	call translationProjectionCall,
) (bool, error) {
	p.mu.Lock()
	p.calls = append(p.calls, call)
	if remaining := p.failures[call.attempt.RiverJobID]; remaining > 0 {
		p.failures[call.attempt.RiverJobID] = remaining - 1
		p.mu.Unlock()
		return false, errors.New("projection unavailable")
	}
	forceNotApplied := p.forceNotApplied[call.attempt.RiverJobID]
	if p.applyOnce {
		if p.applied == nil {
			p.applied = make(map[int64]bool)
		}
		if p.applied[call.attempt.RiverJobID] {
			forceNotApplied = true
		} else if !forceNotApplied {
			p.applied[call.attempt.RiverJobID] = true
		}
	}
	hook := p.hook
	p.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, call); err != nil {
			return false, err
		}
	}
	return !forceNotApplied, nil
}

func (p *fakeTranslationTerminalProjector) snapshotCalls() []translationProjectionCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]translationProjectionCall(nil), p.calls...)
}

type fakeTranslationLegacyResolver struct {
	mu          sync.Mutex
	resolutions map[uuid.UUID]model.TranslationAttemptResolution
	err         error
	calls       int
}

func (r *fakeTranslationLegacyResolver) ProveCurrentLegacyAttempt(
	_ context.Context,
	translationID uuid.UUID,
	_ int64,
	_ int64,
) (model.TranslationAttemptResolution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return model.TranslationAttemptResolution{}, r.err
	}
	if resolution, ok := r.resolutions[translationID]; ok {
		return resolution, nil
	}
	return model.TranslationAttemptResolution{
		Rejection: model.TranslationAttemptRejectionTranslationNotFound,
	}, nil
}

func newTestTranslationTerminalReconciler(
	store translationTerminalStore,
	projector TranslationTerminalProjector,
	legacy linktranslation.LegacyAttemptResolver,
	batch int,
) *TranslationTerminalReconciler {
	if legacy == nil {
		legacy = &fakeTranslationLegacyResolver{}
	}
	return newTranslationTerminalReconciler(
		store,
		projector,
		legacy,
		time.Hour,
		batch,
		2*time.Second,
		time.Hour,
		nil,
		nil,
	)
}

func v2TranslationTerminalJob(
	t *testing.T,
	riverJobID int64,
	finalizedAt time.Time,
	state rivertype.JobState,
	translationID uuid.UUID,
	generation int64,
) translationTerminalJob {
	t.Helper()
	encoded, err := json.Marshal(linktranslation.JobArgs{
		TranslationID:     translationID,
		AttemptGeneration: generation,
		SourceHash:        strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return translationTerminalJob{
		riverJobID: riverJobID, kind: model.TranslationJobKindV2,
		state: state, finalizedAt: finalizedAt, encodedArgs: encoded,
	}
}

func missingTranslationAttempt(
	translationID uuid.UUID,
	riverJobID int64,
	updatedAt time.Time,
) translationMissingAttempt {
	return translationMissingAttempt{
		attempt: model.TranslationAttempt{
			TranslationID: translationID, AttemptGeneration: 1,
			RiverJobID: riverJobID, SourceHash: strings.Repeat("b", 64),
		},
		updatedAt: updatedAt,
	}
}

func TestTranslationTerminalReconcilerMapsTerminalStatesToStableCodes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		state rivertype.JobState
		code  TranslationTerminalCode
	}{
		{state: rivertype.JobStateCancelled, code: TranslationTerminalJobCancelled},
		{state: rivertype.JobStateDiscarded, code: TranslationTerminalJobDiscarded},
		{state: rivertype.JobStateCompleted, code: TranslationTerminalProjectionMissing},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			t.Parallel()
			projector := &fakeTranslationTerminalProjector{}
			reconciler := newTestTranslationTerminalReconciler(
				&fakeTranslationTerminalStore{}, projector, nil, 10,
			)
			job := v2TranslationTerminalJob(t, 11, time.Now(), tc.state, uuid.New(), 3)
			var result TranslationTerminalReconcileResult
			if err := reconciler.reconcileTerminalJob(context.Background(), job, true, &result); err != nil {
				t.Fatalf("reconcileTerminalJob() error = %v", err)
			}
			calls := projector.snapshotCalls()
			if len(calls) != 1 || calls[0].code != string(tc.code) || calls[0].missing {
				t.Fatalf("projection calls = %+v", calls)
			}
			if result.Reconciled != 1 || result.Invalid != 0 {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestTranslationTerminalReconcilerRejectsInvalidV2Identity(t *testing.T) {
	t.Parallel()

	projector := &fakeTranslationTerminalProjector{}
	reconciler := newTestTranslationTerminalReconciler(
		&fakeTranslationTerminalStore{}, projector, nil, 10,
	)
	job := translationTerminalJob{
		riverJobID: 21, kind: model.TranslationJobKindV2,
		state: rivertype.JobStateDiscarded, finalizedAt: time.Now(),
		encodedArgs: []byte(`{"translation_id":"` + uuid.NewString() + `","attempt_generation":1}`),
	}
	var result TranslationTerminalReconcileResult
	if err := reconciler.reconcileTerminalJob(context.Background(), job, true, &result); err != nil {
		t.Fatalf("reconcileTerminalJob() error = %v", err)
	}
	if result.Invalid != 1 || len(projector.snapshotCalls()) != 0 {
		t.Fatalf("result=%+v calls=%+v", result, projector.snapshotCalls())
	}
}

func TestTranslationTerminalReconcilerResolvesLegacyAttempt(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const riverJobID int64 = 31
	attempt := model.TranslationAttempt{
		TranslationID: translationID, AttemptGeneration: 0, RiverJobID: riverJobID,
	}
	resolver := &fakeTranslationLegacyResolver{resolutions: map[uuid.UUID]model.TranslationAttemptResolution{
		translationID: {Attempt: attempt},
	}}
	encoded, err := json.Marshal(linktranslation.LegacyJobArgs{TranslationID: translationID})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	projector := &fakeTranslationTerminalProjector{}
	reconciler := newTestTranslationTerminalReconciler(
		&fakeTranslationTerminalStore{}, projector, resolver, 10,
	)
	job := translationTerminalJob{
		riverJobID: riverJobID, kind: model.TranslationJobKindLegacy,
		state: rivertype.JobStateCancelled, finalizedAt: time.Now(), encodedArgs: encoded,
	}
	var result TranslationTerminalReconcileResult
	if err := reconciler.reconcileTerminalJob(context.Background(), job, true, &result); err != nil {
		t.Fatalf("reconcileTerminalJob() error = %v", err)
	}
	calls := projector.snapshotCalls()
	if len(calls) != 1 || calls[0].attempt != attempt || resolver.calls != 1 {
		t.Fatalf("calls=%+v resolver_calls=%d", calls, resolver.calls)
	}
}

func TestTranslationTerminalReconcilerPaginatesEqualTimestampsByRiverID(t *testing.T) {
	t.Parallel()

	finalizedAt := time.Now().Add(-time.Hour)
	store := &fakeTranslationTerminalStore{terminals: []translationTerminalJob{
		v2TranslationTerminalJob(t, 40, finalizedAt, rivertype.JobStateCompleted, uuid.New(), 1),
		v2TranslationTerminalJob(t, 41, finalizedAt, rivertype.JobStateDiscarded, uuid.New(), 1),
		v2TranslationTerminalJob(t, 42, finalizedAt, rivertype.JobStateCancelled, uuid.New(), 1),
	}}
	projector := &fakeTranslationTerminalProjector{applyOnce: true}
	reconciler := newTestTranslationTerminalReconciler(store, projector, nil, 1)
	result, err := reconciler.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Scanned != 3 || result.Reconciled != 3 || len(projector.snapshotCalls()) != 3 {
		t.Fatalf("result=%+v calls=%+v", result, projector.snapshotCalls())
	}
	store.mu.Lock()
	cursors := append([]translationTerminalCursor(nil), store.terminalCursors...)
	store.mu.Unlock()
	if len(cursors) != 3 || cursors[0].riverJobID != 0 || cursors[1].riverJobID != 40 || cursors[2].riverJobID != 41 {
		t.Fatalf("terminal cursors = %+v", cursors)
	}
}

func TestTranslationTerminalReconcilerWrapFindsLateCommitBehindCursor(t *testing.T) {
	t.Parallel()

	base := time.Now().Add(-2 * time.Hour)
	first := v2TranslationTerminalJob(t, 52, base.Add(time.Minute), rivertype.JobStateDiscarded, uuid.New(), 1)
	store := &fakeTranslationTerminalStore{terminals: []translationTerminalJob{first}}
	projector := &fakeTranslationTerminalProjector{applyOnce: true}
	reconciler := newTestTranslationTerminalReconciler(store, projector, nil, 10)
	if _, err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce() error = %v", err)
	}
	late := v2TranslationTerminalJob(t, 51, base, rivertype.JobStateCancelled, uuid.New(), 1)
	store.addTerminal(late)
	if _, err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	projector.mu.Lock()
	lateApplied := projector.applied[late.riverJobID]
	projector.mu.Unlock()
	if !lateApplied {
		t.Fatalf("late terminal job %d was not projected; calls=%+v", late.riverJobID, projector.snapshotCalls())
	}
}

func TestTranslationTerminalReconcilerRetriesFailureWithoutStarvingLaterJobs(t *testing.T) {
	t.Parallel()

	base := time.Now().Add(-time.Hour)
	store := &fakeTranslationTerminalStore{terminals: []translationTerminalJob{
		v2TranslationTerminalJob(t, 61, base, rivertype.JobStateDiscarded, uuid.New(), 1),
		v2TranslationTerminalJob(t, 62, base.Add(time.Second), rivertype.JobStateDiscarded, uuid.New(), 1),
	}}
	projector := &fakeTranslationTerminalProjector{
		applyOnce: true, failures: map[int64]int{61: 1},
	}
	reconciler := newTestTranslationTerminalReconciler(store, projector, nil, 2)
	first, err := reconciler.RunOnce(context.Background())
	if err == nil || first.Failed != 1 || first.Reconciled != 1 {
		t.Fatalf("first RunOnce() = %+v, %v", first, err)
	}
	if len(reconciler.terminalRetries) != 1 || reconciler.terminalRetries[0].riverJobID != 61 {
		t.Fatalf("terminal retries = %+v", reconciler.terminalRetries)
	}
	second, err := reconciler.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	projector.mu.Lock()
	firstApplied, secondApplied := projector.applied[61], projector.applied[62]
	projector.mu.Unlock()
	if !firstApplied || !secondApplied || second.Reconciled < 1 || len(reconciler.terminalRetries) != 0 {
		t.Fatalf("second result=%+v applied=%v/%v retries=%+v", second, firstApplied, secondApplied, reconciler.terminalRetries)
	}
}

func TestTranslationTerminalReconcilerScansMissingAttemptsWithGlobalKeyset(t *testing.T) {
	t.Parallel()

	base := time.Now().Add(-3 * time.Hour)
	items := make([]translationMissingAttempt, 0, 5)
	for index := 0; index < 5; index++ {
		items = append(items, missingTranslationAttempt(uuid.New(), int64(70+index), base.Add(time.Duration(index)*time.Second)))
	}
	store := &fakeTranslationTerminalStore{missing: items}
	projector := &fakeTranslationTerminalProjector{applyOnce: true}
	reconciler := newTestTranslationTerminalReconciler(store, projector, nil, 2)
	result, err := reconciler.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Scanned != 5 || result.Missing != 5 || result.Reconciled != 5 {
		t.Fatalf("result = %+v", result)
	}
	store.mu.Lock()
	cursors := append([]translationMissingCursor(nil), store.missingCursors...)
	cutoffs := append([]time.Time(nil), store.missingCutoffs...)
	store.mu.Unlock()
	if len(cursors) != 3 || !cursors[0].updatedAt.IsZero() ||
		cursors[1].translationID != items[1].attempt.TranslationID ||
		cursors[2].translationID != items[3].attempt.TranslationID {
		t.Fatalf("missing cursors = %+v", cursors)
	}
	if len(cutoffs) != 3 || !cutoffs[0].Equal(cutoffs[1]) || !cutoffs[1].Equal(cutoffs[2]) {
		t.Fatalf("missing cutoffs changed within cycle: %+v", cutoffs)
	}
}

type nonIncreasingTerminalStore struct {
	job   translationTerminalJob
	end   translationTerminalCursor
	calls int
}

func (s *nonIncreasingTerminalStore) LatestTerminalCursor(context.Context) (translationTerminalCursor, bool, error) {
	return s.end, true, nil
}

func (s *nonIncreasingTerminalStore) ListTerminalJobs(context.Context, translationTerminalCursor, int) ([]translationTerminalJob, error) {
	s.calls++
	return []translationTerminalJob{s.job}, nil
}

func (*nonIncreasingTerminalStore) ListMissingAttempts(context.Context, time.Time, translationMissingCursor, int) ([]translationMissingAttempt, error) {
	return nil, nil
}

func TestTranslationTerminalReconcilerFailsClosedOnNonIncreasingTerminalCursor(t *testing.T) {
	t.Parallel()

	job := v2TranslationTerminalJob(t, 81, time.Now().Add(-time.Hour), rivertype.JobStateDiscarded, uuid.New(), 1)
	store := &nonIncreasingTerminalStore{
		job: job,
		end: translationTerminalCursor{finalizedAt: job.finalizedAt.Add(time.Second), riverJobID: 82},
	}
	reconciler := newTestTranslationTerminalReconciler(store, &fakeTranslationTerminalProjector{}, nil, 1)
	_, err := reconciler.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "non-increasing cursor") {
		t.Fatalf("RunOnce() error = %v", err)
	}
}

type nonIncreasingMissingStore struct {
	item translationMissingAttempt
}

func (*nonIncreasingMissingStore) LatestTerminalCursor(context.Context) (translationTerminalCursor, bool, error) {
	return translationTerminalCursor{}, false, nil
}

func (*nonIncreasingMissingStore) ListTerminalJobs(context.Context, translationTerminalCursor, int) ([]translationTerminalJob, error) {
	return nil, nil
}

func (s *nonIncreasingMissingStore) ListMissingAttempts(context.Context, time.Time, translationMissingCursor, int) ([]translationMissingAttempt, error) {
	return []translationMissingAttempt{s.item}, nil
}

func TestTranslationTerminalReconcilerFailsClosedOnNonIncreasingMissingCursor(t *testing.T) {
	t.Parallel()

	store := &nonIncreasingMissingStore{item: missingTranslationAttempt(uuid.New(), 91, time.Now().Add(-3*time.Hour))}
	reconciler := newTestTranslationTerminalReconciler(store, &fakeTranslationTerminalProjector{}, nil, 1)
	_, err := reconciler.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "non-increasing cursor") {
		t.Fatalf("RunOnce() error = %v", err)
	}
}

func TestTranslationTerminalReconcilerRejectsMissingProjectionWithActiveReplacement(t *testing.T) {
	t.Parallel()

	item := missingTranslationAttempt(uuid.New(), 101, time.Now().Add(-3*time.Hour))
	currentJobID := item.attempt.RiverJobID
	store := &fakeTranslationTerminalStore{
		missing: []translationMissingAttempt{item},
		inspectStates: map[uuid.UUID]translationTerminalProjectionState{
			item.attempt.TranslationID: {
				currentRiverJobID:     &currentJobID,
				attemptGeneration:     item.attempt.AttemptGeneration,
				sourceHash:            item.attempt.SourceHash,
				sourceContentRevision: item.attempt.SourceContentRevision,
				status:                model.TranslationStatusPending,
				activeReplacement:     true,
			},
		},
	}
	projector := &fakeTranslationTerminalProjector{
		forceNotApplied: map[int64]bool{item.attempt.RiverJobID: true},
	}
	reconciler := newTestTranslationTerminalReconciler(store, projector, nil, 10)
	result, err := reconciler.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Stale != 1 || result.Missing != 0 || result.Reconciled != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestTranslationTerminalReconcilerRecognizesAlreadyProjectedHistory(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	job := v2TranslationTerminalJob(t, 111, time.Now().Add(-time.Hour), rivertype.JobStateCompleted, translationID, 2)
	store := &fakeTranslationTerminalStore{
		terminals: []translationTerminalJob{job},
		inspectStates: map[uuid.UUID]translationTerminalProjectionState{
			translationID: {
				attemptGeneration: 2,
				sourceHash:        strings.Repeat("a", 64),
				status:            model.TranslationStatusDone,
			},
		},
	}
	projector := &fakeTranslationTerminalProjector{forceNotApplied: map[int64]bool{111: true}}
	reconciler := newTestTranslationTerminalReconciler(store, projector, nil, 10)
	result, err := reconciler.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Stale != 0 || result.Reconciled != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestTranslationTerminalReconcilerMissingCycleResumesAfterCancellation(t *testing.T) {
	t.Parallel()

	base := time.Now().Add(-3 * time.Hour)
	first := missingTranslationAttempt(uuid.New(), 121, base)
	second := missingTranslationAttempt(uuid.New(), 122, base.Add(time.Second))
	store := &fakeTranslationTerminalStore{missing: []translationMissingAttempt{first, second}}
	ctx, cancel := context.WithCancel(context.Background())
	projector := &fakeTranslationTerminalProjector{applyOnce: true}
	projector.hook = func(_ context.Context, call translationProjectionCall) error {
		if call.attempt.RiverJobID == first.attempt.RiverJobID {
			cancel()
			return context.Canceled
		}
		return nil
	}
	reconciler := newTestTranslationTerminalReconciler(store, projector, nil, 1)
	if _, err := reconciler.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first RunOnce() error = %v, want context.Canceled", err)
	}
	if !reconciler.missingCycleActive || reconciler.missingCursor.translationID != first.attempt.TranslationID {
		t.Fatalf("missing cycle state = active:%v cursor:%+v", reconciler.missingCycleActive, reconciler.missingCursor)
	}
	projector.hook = nil
	if _, err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if _, err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatalf("third RunOnce() error = %v", err)
	}
	projector.mu.Lock()
	firstApplied, secondApplied := projector.applied[121], projector.applied[122]
	projector.mu.Unlock()
	if !firstApplied || !secondApplied {
		t.Fatalf("missing attempts applied=%v/%v calls=%+v", firstApplied, secondApplied, projector.snapshotCalls())
	}
}

func TestTranslationTerminalReconcilerLogsInstallationIdentity(t *testing.T) {
	t.Parallel()

	item := missingTranslationAttempt(uuid.New(), 131, time.Now().Add(-3*time.Hour))
	store := &fakeTranslationTerminalStore{missing: []translationMissingAttempt{item}}
	projector := &fakeTranslationTerminalProjector{}
	var logs bytes.Buffer
	reconciler := newTranslationTerminalReconciler(
		store,
		projector,
		&fakeTranslationLegacyResolver{},
		time.Hour,
		10,
		2*time.Second,
		time.Hour,
		slog.New(slog.NewTextHandler(&logs, nil)),
		nil,
	)
	if _, err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, "translation_id="+item.attempt.TranslationID.String()) ||
		strings.Contains(got, "tenant_id") {
		t.Fatalf("log = %q", got)
	}
}

func TestTranslationTerminalReconcilerConstructionIsDormantAndLifecycleIsLinear(t *testing.T) {
	t.Parallel()

	notify := make(chan struct{}, 1)
	store := &fakeTranslationTerminalStore{latestNotify: notify}
	reconciler := newTranslationTerminalReconciler(
		store,
		&fakeTranslationTerminalProjector{},
		&fakeTranslationLegacyResolver{},
		time.Hour,
		10,
		2*time.Second,
		time.Hour,
		nil,
		nil,
	)
	select {
	case <-notify:
		t.Fatal("reconciler performed work before Start")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := reconciler.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-notify:
	case <-time.After(time.Second):
		t.Fatal("reconciler did not run after Start")
	}
	if err := reconciler.Start(ctx); !errors.Is(err, ErrBackgroundAlreadyStarted) {
		t.Fatalf("second Start() error = %v", err)
	}
	if err := reconciler.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := reconciler.Start(context.Background()); !errors.Is(err, ErrBackgroundStopped) {
		t.Fatalf("Start() after Stop error = %v", err)
	}
}

func TestTranslationTerminalReconcilerReplicasApplyCurrentAttemptOnce(t *testing.T) {
	t.Parallel()

	job := v2TranslationTerminalJob(t, 141, time.Now().Add(-time.Hour), rivertype.JobStateDiscarded, uuid.New(), 1)
	store := &fakeTranslationTerminalStore{terminals: []translationTerminalJob{job}}
	projector := &fakeTranslationTerminalProjector{applyOnce: true}
	first := newTestTranslationTerminalReconciler(store, projector, nil, 10)
	second := newTestTranslationTerminalReconciler(store, projector, nil, 10)

	var wg sync.WaitGroup
	results := make(chan TranslationTerminalReconcileResult, 2)
	errs := make(chan error, 2)
	for _, reconciler := range []*TranslationTerminalReconciler{first, second} {
		wg.Add(1)
		go func(r *TranslationTerminalReconciler) {
			defer wg.Done()
			result, err := r.RunOnce(context.Background())
			results <- result
			errs <- err
		}(reconciler)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("RunOnce() error = %v", err)
		}
	}
	totalReconciled := 0
	for result := range results {
		totalReconciled += result.Reconciled
	}
	if totalReconciled != 1 {
		t.Fatalf("replicas reconciled %d times, want exactly once", totalReconciled)
	}
}

func TestTranslationMissingStoreUsesInstallationScopeAndExplicitBounds(t *testing.T) {
	t.Parallel()

	for name, query := range map[string]string{
		"missing": listTranslationMissingAttemptsSQL,
		"inspect": inspectTranslationTerminalProjectionSQL,
	} {
		if strings.Contains(strings.ToLower(query), "tenant_id") {
			t.Fatalf("%s query still contains tenant_id: %s", name, query)
		}
	}
	for _, required := range []string{
		"(t.updated_at, t.id) >", "t.updated_at < $1", "LIMIT $6",
		"current_river_job_id IS NOT NULL", "active_job.args->>'attempt_generation'",
		"active_job.args->>'source_hash'", "source_content_revision",
	} {
		if !strings.Contains(listTranslationMissingAttemptsSQL, required) {
			t.Errorf("missing query lacks %q: %s", required, listTranslationMissingAttemptsSQL)
		}
	}
}
