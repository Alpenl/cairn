package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type resourceBackground struct {
	name     string
	events   *[]string
	startErr error
	stopErr  error
}

func (b *resourceBackground) Start(context.Context) error {
	*b.events = append(*b.events, "start "+b.name)
	return b.startErr
}

func (b *resourceBackground) Stop(context.Context) error {
	*b.events = append(*b.events, "stop "+b.name)
	return b.stopErr
}

func TestRuntimeResourcesStartAndStopBackgroundsInReverseOrder(t *testing.T) {
	t.Parallel()

	var events []string
	resources := newRuntimeResources([]namedRuntimeBackground{
		{name: "queue", background: &resourceBackground{name: "queue", events: &events}},
		{name: "scheduler", background: &resourceBackground{name: "scheduler", events: &events}},
	}, nil)

	if err := resources.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := resources.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	want := []string{"start queue", "start scheduler", "stop scheduler", "stop queue"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRuntimeResourcesNamesStartAndStopFailures(t *testing.T) {
	t.Parallel()

	startErr := errors.New("start failed")
	stopErr := errors.New("stop failed")
	var events []string
	resources := newRuntimeResources([]namedRuntimeBackground{
		{name: "queue", background: &resourceBackground{name: "queue", events: &events, stopErr: stopErr}},
		{name: "scheduler", background: &resourceBackground{name: "scheduler", events: &events, startErr: startErr}},
	}, nil)

	if err := resources.Start(t.Context()); !errors.Is(err, startErr) {
		t.Fatalf("Start() error = %v, want %v", err, startErr)
	}
	if err := resources.Close(t.Context()); !errors.Is(err, stopErr) {
		t.Fatalf("Close() error = %v, want %v", err, stopErr)
	}
}

type deadlineBlockingBackground struct {
	name     string
	events   *[]string
	deadline *time.Time
}

func (b *deadlineBlockingBackground) Start(context.Context) error { return nil }

func (b *deadlineBlockingBackground) Stop(ctx context.Context) error {
	*b.events = append(*b.events, "stop "+b.name)
	if deadline, ok := ctx.Deadline(); ok && b.deadline != nil {
		*b.deadline = deadline
	}
	<-ctx.Done()
	return ctx.Err()
}

type deadlinePersistenceCloser struct {
	events   *[]string
	deadline *time.Time
	err      error
}

func (p *deadlinePersistenceCloser) Close(ctx context.Context) error {
	*p.events = append(*p.events, "close persistence")
	if deadline, ok := ctx.Deadline(); ok && p.deadline != nil {
		*p.deadline = deadline
	}
	if p.err != nil {
		return p.err
	}
	return ctx.Err()
}

func TestRuntimeResourcesUseOneDeadlineAcrossBackgroundsAndPersistence(t *testing.T) {
	t.Parallel()

	var (
		events              []string
		backgroundDeadline  time.Time
		persistenceDeadline time.Time
	)
	resources := newRuntimeResources([]namedRuntimeBackground{
		{
			name: "queue",
			background: &deadlineBlockingBackground{
				name:     "queue",
				events:   &events,
				deadline: &backgroundDeadline,
			},
		},
	}, &deadlinePersistenceCloser{
		events:   &events,
		deadline: &persistenceDeadline,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	err := resources.Close(ctx)
	elapsed := time.Since(startedAt)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want context deadline exceeded", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("Close() elapsed = %v, want bounded by caller deadline", elapsed)
	}
	if !reflect.DeepEqual(events, []string{"stop queue", "close persistence"}) {
		t.Fatalf("events = %v, want stop queue then close persistence", events)
	}
	if backgroundDeadline.IsZero() || persistenceDeadline.IsZero() {
		t.Fatalf("missing observed deadlines: background=%v persistence=%v",
			backgroundDeadline, persistenceDeadline)
	}
	if !backgroundDeadline.Equal(persistenceDeadline) {
		t.Fatalf("deadlines differ: background=%v persistence=%v",
			backgroundDeadline, persistenceDeadline)
	}
}
