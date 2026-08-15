package app

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"webtag/internal/database"
)

type recordingRuntimeBackground struct {
	name     string
	events   *[]string
	startErr error
	stopErr  error
}

type cleanupContextObservation struct {
	name string
	ctx  context.Context
	err  error
}

type cleanupContextRuntimeBackground struct {
	name         string
	startErr     error
	cancelStart  context.CancelFunc
	observations *[]cleanupContextObservation
}

type cleanupContextRuntimePersistence struct {
	observations *[]cleanupContextObservation
}

func (p *cleanupContextRuntimePersistence) AdmitOwner(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func (p *cleanupContextRuntimePersistence) CloseAdmission() {}

func (p *cleanupContextRuntimePersistence) Drain(ctx context.Context) error {
	*p.observations = append(*p.observations, cleanupContextObservation{name: "drain", ctx: ctx, err: ctx.Err()})
	return nil
}

func (p *cleanupContextRuntimePersistence) Close(ctx context.Context) error {
	*p.observations = append(*p.observations, cleanupContextObservation{name: "pool", ctx: ctx, err: ctx.Err()})
	return nil
}

func (b *cleanupContextRuntimeBackground) Start(context.Context) error {
	if b.cancelStart != nil {
		b.cancelStart()
	}
	return b.startErr
}

func (b *cleanupContextRuntimeBackground) Stop(ctx context.Context) error {
	*b.observations = append(*b.observations, cleanupContextObservation{name: b.name, ctx: ctx, err: ctx.Err()})
	return nil
}

type deadlineHeldRuntimeBackground struct {
	release  chan struct{}
	loopDone chan struct{}
}

func newDeadlineHeldRuntimeBackground() *deadlineHeldRuntimeBackground {
	return &deadlineHeldRuntimeBackground{
		release:  make(chan struct{}),
		loopDone: make(chan struct{}),
	}
}

func (b *deadlineHeldRuntimeBackground) Start(context.Context) error {
	go func() {
		defer close(b.loopDone)
		<-b.release
	}()
	return nil
}

func (b *deadlineHeldRuntimeBackground) Stop(ctx context.Context) error {
	select {
	case <-b.loopDone:
		return nil
	case <-ctx.Done():
		// Exercise the lifecycle's deadline barrier independently of whether an
		// individual controller remembers to surface ctx.Err itself.
		return nil
	}
}

type runtimeOwnerTestKey struct{}

type recordingRuntimePersistence struct {
	events   *[]string
	drainErr error
	closeErr error
}

type deadlineBlockingRuntimePersistence struct {
}

func (p *deadlineBlockingRuntimePersistence) AdmitOwner(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func (p *deadlineBlockingRuntimePersistence) CloseAdmission() {}

func (p *deadlineBlockingRuntimePersistence) Drain(context.Context) error { return nil }

func (p *deadlineBlockingRuntimePersistence) Close(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (p *recordingRuntimePersistence) AdmitOwner(ctx context.Context) (context.Context, func(), error) {
	*p.events = append(*p.events, "admit owner")
	ownerCtx := context.WithValue(ctx, runtimeOwnerTestKey{}, true)
	return ownerCtx, func() { *p.events = append(*p.events, "revoke owner") }, nil
}

func (p *recordingRuntimePersistence) CloseAdmission() {
	*p.events = append(*p.events, "close admission")
}

func (p *recordingRuntimePersistence) Drain(context.Context) error {
	*p.events = append(*p.events, "drain persistence")
	return p.drainErr
}

func (p *recordingRuntimePersistence) Close(ctx context.Context) error {
	*p.events = append(*p.events, "close pool")
	return errors.Join(p.closeErr, ctx.Err())
}

func (b *recordingRuntimeBackground) Start(context.Context) error {
	*b.events = append(*b.events, "start "+b.name)
	return b.startErr
}

func TestRuntimeLifecycleClosesAdmissionThenStopsAndDrainsInOrder(t *testing.T) {
	t.Parallel()

	var events []string
	persistence := &recordingRuntimePersistence{events: &events}
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		cleanupTimeout: time.Second,
		persistence:    persistence,
		tracerShutdown: func(context.Context) error {
			events = append(events, "shutdown tracer")
			return nil
		},
		backgrounds: []namedRuntimeBackground{
			{name: "recorder", background: &recordingRuntimeBackground{name: "recorder", events: &events}},
			{name: "queue", background: &recordingRuntimeBackground{name: "queue", events: &events}},
			{name: "cleaner", background: &recordingRuntimeBackground{name: "cleaner", events: &events}},
		},
	})

	if err := lifecycle.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := lifecycle.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	want := []string{
		"admit owner",
		"start recorder",
		"start queue",
		"start cleaner",
		"close admission",
		"stop cleaner",
		"stop queue",
		"stop recorder",
		"revoke owner",
		"drain persistence",
		"close pool",
		"shutdown tracer",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events = %v, want %v", events, want)
	}
}

func TestRuntimeLifecycleCloseBeforeStartStopsConstructedResourcesInReverseOrder(t *testing.T) {
	t.Parallel()

	var events []string
	persistence := &recordingRuntimePersistence{events: &events}
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		persistence: persistence,
		tracerShutdown: func(context.Context) error {
			events = append(events, "shutdown tracer")
			return nil
		},
		backgrounds: []namedRuntimeBackground{
			{name: "recorder", background: &recordingRuntimeBackground{name: "recorder", events: &events}},
			{name: "queue", background: &recordingRuntimeBackground{name: "queue", events: &events}},
		},
	})

	if err := lifecycle.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	want := []string{
		"close admission",
		"stop queue",
		"stop recorder",
		"drain persistence",
		"close pool",
		"shutdown tracer",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("close-before-start events = %v, want %v", events, want)
	}
}

func TestRuntimeLifecyclePoolCloseDeadlineIsHardBarrierForTracer(t *testing.T) {
	t.Parallel()

	tracerCalled := false
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		persistence: &deadlineBlockingRuntimePersistence{},
		tracerShutdown: func(context.Context) error {
			tracerCalled = true
			return nil
		},
	})
	if err := lifecycle.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelClose()
	startedAt := time.Now()
	err := lifecycle.Close(closeCtx)
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Close() elapsed = %v, want bounded by caller deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, database.ErrShutdownDeadline) {
		t.Fatalf("Close() error = %v, want DeadlineExceeded and ErrShutdownDeadline", err)
	}
	if tracerCalled {
		t.Fatal("tracer shutdown ran after persistence close crossed its deadline")
	}
}

func TestRuntimeLifecyclePoolCloseErrorIsHardBarrierForTracer(t *testing.T) {
	t.Parallel()

	poolErr := errors.New("pool connection close failed")
	var events []string
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		persistence: &recordingRuntimePersistence{events: &events, closeErr: poolErr},
		tracerShutdown: func(context.Context) error {
			events = append(events, "shutdown tracer")
			return nil
		},
	})
	if err := lifecycle.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	err := lifecycle.Close(t.Context())
	if !errors.Is(err, poolErr) || !errors.Is(err, database.ErrShutdownDeadline) {
		t.Fatalf("Close() error = %v, want pool error and ErrShutdownDeadline", err)
	}
	if slices.Contains(events, "shutdown tracer") {
		t.Fatal("tracer shutdown ran after persistence close returned an error")
	}
}

func TestRuntimeLifecycleStopFailureRetainsOwnerAndDoesNotClosePersistenceOrTracer(t *testing.T) {
	t.Parallel()

	stopErr := errors.New("queue stop deadline")
	tracerErr := errors.New("tracer shutdown failed")
	var events []string
	persistence := &recordingRuntimePersistence{events: &events}
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		cleanupTimeout: time.Second,
		persistence:    persistence,
		tracerShutdown: func(context.Context) error {
			events = append(events, "shutdown tracer")
			return tracerErr
		},
		backgrounds: []namedRuntimeBackground{
			{name: "recorder", background: &recordingRuntimeBackground{name: "recorder", events: &events}},
			{name: "queue", background: &recordingRuntimeBackground{name: "queue", events: &events, stopErr: stopErr}},
			{name: "cleaner", background: &recordingRuntimeBackground{name: "cleaner", events: &events}},
		},
	})
	if err := lifecycle.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	err := lifecycle.Close(context.Background())
	if !errors.Is(err, stopErr) || !errors.Is(err, database.ErrShutdownDeadline) {
		t.Fatalf("Close() error = %v, want stop error and shutdown deadline", err)
	}
	if errors.Is(err, tracerErr) {
		t.Fatalf("Close() error = %v, tracer must not run after incomplete stop", err)
	}
	for _, forbidden := range []string{"revoke owner", "drain persistence", "close pool", "shutdown tracer"} {
		for _, event := range events {
			if event == forbidden {
				t.Fatalf("%s ran after incomplete background stop: %v", forbidden, events)
			}
		}
	}
	wantSuffix := []string{
		"close admission",
		"stop cleaner",
		"stop queue",
		"stop recorder",
	}
	if got := events[len(events)-len(wantSuffix):]; !reflect.DeepEqual(got, wantSuffix) {
		t.Fatalf("close events = %v, want suffix %v", got, wantSuffix)
	}
}

func (b *recordingRuntimeBackground) Stop(context.Context) error {
	*b.events = append(*b.events, "stop "+b.name)
	return b.stopErr
}

func TestRuntimeLifecycleStartRollsBackSuccessfulBackgroundsInReverseOrder(t *testing.T) {
	names := []string{"recorder", "queue", "cleaner", "migration"}
	for failAt := range names {
		failAt := failAt
		t.Run(names[failAt], func(t *testing.T) {
			t.Parallel()

			wantErr := errors.New(names[failAt] + " start failed")
			var events []string
			backgrounds := make([]namedRuntimeBackground, 0, len(names))
			for index, name := range names {
				background := &recordingRuntimeBackground{name: name, events: &events}
				if index == failAt {
					background.startErr = wantErr
				}
				backgrounds = append(backgrounds, namedRuntimeBackground{name: name, background: background})
			}
			lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
				cleanupTimeout: time.Second,
				backgrounds:    backgrounds,
			})

			err := lifecycle.Start(context.Background())
			if !errors.Is(err, wantErr) {
				t.Fatalf("Start() error = %v, want %v", err, wantErr)
			}
			wantEvents := make([]string, 0, 2*failAt+1)
			for index := 0; index <= failAt; index++ {
				wantEvents = append(wantEvents, "start "+names[index])
			}
			for index := failAt - 1; index >= 0; index-- {
				wantEvents = append(wantEvents, "stop "+names[index])
			}
			if !reflect.DeepEqual(events, wantEvents) {
				t.Fatalf("lifecycle events = %v, want %v", events, wantEvents)
			}
		})
	}
}

func TestRuntimeFailedStartUsesOneDetachedSharedCleanupDeadline(t *testing.T) {
	t.Parallel()

	type contextKey string
	const key contextKey = "runtime-start-id"
	startCtx, cancelStart := context.WithCancel(context.WithValue(context.Background(), key, "rf7"))
	defer cancelStart()

	startErr := errors.New("queue start failed")
	var observations []cleanupContextObservation
	first := &cleanupContextRuntimeBackground{name: "recorder", observations: &observations}
	second := &cleanupContextRuntimeBackground{
		name:         "queue",
		startErr:     startErr,
		cancelStart:  cancelStart,
		observations: &observations,
	}
	third := &cleanupContextRuntimeBackground{name: "cleaner", observations: &observations}
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		persistence: &cleanupContextRuntimePersistence{observations: &observations},
		backgrounds: []namedRuntimeBackground{
			{name: first.name, background: first},
			{name: second.name, background: second},
			{name: third.name, background: third},
		},
		tracerShutdown: func(ctx context.Context) error {
			observations = append(observations, cleanupContextObservation{name: "tracer", ctx: ctx, err: ctx.Err()})
			return nil
		},
	})
	runtime := &Runtime{start: lifecycle.Start, close: lifecycle.Close}

	if err := runtime.Start(startCtx); !errors.Is(err, startErr) {
		t.Fatalf("Start() error = %v, want %v", err, startErr)
	}
	if len(observations) != 6 {
		t.Fatalf("cleanup observations = %d, want rollback, suffix, persistence, and tracer", len(observations))
	}
	if got := []string{
		observations[0].name,
		observations[1].name,
		observations[2].name,
		observations[3].name,
		observations[4].name,
		observations[5].name,
	}; !reflect.DeepEqual(got, []string{"recorder", "cleaner", "queue", "drain", "pool", "tracer"}) {
		t.Fatalf("cleanup order = %v, want [recorder cleaner queue drain pool tracer]", got)
	}

	var sharedDeadline time.Time
	for index, observation := range observations {
		if observation.err != nil {
			t.Fatalf("cleanup %d inherited triggering cancellation: %v", index, observation.err)
		}
		if got := observation.ctx.Value(key); got != "rf7" {
			t.Fatalf("cleanup %d context value = %v, want rf7", index, got)
		}
		deadline, ok := observation.ctx.Deadline()
		if !ok {
			t.Fatalf("cleanup %d has no deadline", index)
		}
		if index == 0 {
			sharedDeadline = deadline
			continue
		}
		if !deadline.Equal(sharedDeadline) {
			t.Fatalf("cleanup deadlines differ: first %v, cleanup %d %v", sharedDeadline, index, deadline)
		}
	}
}

func TestRuntimeCloseDeadlineIsAHardBarrierForPermanentlyBlockedBackground(t *testing.T) {
	t.Parallel()

	background := newDeadlineHeldRuntimeBackground()
	defer func() {
		close(background.release)
		select {
		case <-background.loopDone:
		case <-time.After(time.Second):
			t.Fatal("test background did not exit after release")
		}
	}()

	var events []string
	persistence := &recordingRuntimePersistence{events: &events}
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		persistence: persistence,
		tracerShutdown: func(context.Context) error {
			events = append(events, "shutdown tracer")
			return nil
		},
		backgrounds: []namedRuntimeBackground{{name: "blocked", background: background}},
	})
	runtime := &Runtime{start: lifecycle.Start, close: lifecycle.Close}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelClose()
	startedAt := time.Now()
	err := runtime.Close(closeCtx)
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Close() elapsed = %v, want bounded by caller deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, database.ErrShutdownDeadline) {
		t.Fatalf("Close() error = %v, want DeadlineExceeded and ErrShutdownDeadline", err)
	}
	for _, forbidden := range []string{"revoke owner", "drain persistence", "close pool", "shutdown tracer"} {
		for _, event := range events {
			if event == forbidden {
				t.Fatalf("%s ran past blocked background deadline: %v", forbidden, events)
			}
		}
	}
}

func TestRuntimeLifecycleStartRollbackJoinsEveryStopError(t *testing.T) {
	t.Parallel()

	startErr := errors.New("third start failed")
	firstStopErr := errors.New("first stop failed")
	secondStopErr := errors.New("second stop failed")
	var events []string
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		cleanupTimeout: time.Second,
		backgrounds: []namedRuntimeBackground{
			{name: "first", background: &recordingRuntimeBackground{name: "first", events: &events, stopErr: firstStopErr}},
			{name: "second", background: &recordingRuntimeBackground{name: "second", events: &events, stopErr: secondStopErr}},
			{name: "third", background: &recordingRuntimeBackground{name: "third", events: &events, startErr: startErr}},
		},
	})

	err := lifecycle.Start(context.Background())
	for _, wantErr := range []error{startErr, firstStopErr, secondStopErr} {
		if !errors.Is(err, wantErr) {
			t.Fatalf("Start() error = %v, want joined %v", err, wantErr)
		}
	}
	if want := []string{"start first", "start second", "start third", "stop second", "stop first"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events = %v, want %v", events, want)
	}
}

func TestRuntimeLifecycleStartRollbackFailureRetainsPersistenceOwner(t *testing.T) {
	t.Parallel()

	startErr := errors.New("queue start failed")
	stopErr := errors.New("recorder stop failed")
	var events []string
	persistence := &recordingRuntimePersistence{events: &events}
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		cleanupTimeout: time.Second,
		persistence:    persistence,
		tracerShutdown: func(context.Context) error {
			events = append(events, "shutdown tracer")
			return nil
		},
		backgrounds: []namedRuntimeBackground{
			{name: "recorder", background: &recordingRuntimeBackground{name: "recorder", events: &events, stopErr: stopErr}},
			{name: "queue", background: &recordingRuntimeBackground{name: "queue", events: &events, startErr: startErr}},
		},
	})

	err := lifecycle.Start(context.Background())
	if !errors.Is(err, startErr) || !errors.Is(err, stopErr) {
		t.Fatalf("Start() error = %v, want start and rollback errors", err)
	}
	closeErr := lifecycle.Close(context.Background())
	if !errors.Is(closeErr, database.ErrShutdownDeadline) {
		t.Fatalf("Close() error = %v, want ErrShutdownDeadline", closeErr)
	}
	for _, forbidden := range []string{"revoke owner", "drain persistence", "close pool", "shutdown tracer"} {
		for _, event := range events {
			if event == forbidden {
				t.Fatalf("%s ran after failed start rollback: %v", forbidden, events)
			}
		}
	}
}

func TestRuntimeLifecycleDrainFailureDoesNotClosePoolOrTracer(t *testing.T) {
	t.Parallel()

	drainErr := errors.Join(database.ErrShutdownDeadline, context.DeadlineExceeded)
	var events []string
	persistence := &recordingRuntimePersistence{events: &events, drainErr: drainErr}
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		persistence: persistence,
		tracerShutdown: func(context.Context) error {
			events = append(events, "shutdown tracer")
			return nil
		},
	})

	err := lifecycle.Close(context.Background())
	if !errors.Is(err, database.ErrShutdownDeadline) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want drain deadline", err)
	}
	if want := []string{"close admission", "drain persistence"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("close events = %v, want %v", events, want)
	}
}
