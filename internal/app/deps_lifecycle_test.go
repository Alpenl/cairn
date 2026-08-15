package app

import (
	"context"
	"fmt"
	"testing"
)

type lifecycleQueueStub struct {
	starts int
	stops  int
}

func (s *lifecycleQueueStub) Start(context.Context) error {
	s.starts++
	return nil
}

func (s *lifecycleQueueStub) Stop(context.Context) error {
	s.stops++
	return nil
}

type lifecyclePollerStub struct {
	starts int
	stops  int
}

func (s *lifecyclePollerStub) Start(context.Context) error {
	s.starts++
	return nil
}

func (s *lifecyclePollerStub) Stop(context.Context) error {
	s.stops++
	return nil
}

func TestRuntimeStartStartsQueueAndBackgroundRepair(t *testing.T) {
	t.Parallel()

	queue := &lifecycleQueueStub{}
	poller := &lifecyclePollerStub{}
	reconciler := &lifecyclePollerStub{}
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		backgrounds: []namedRuntimeBackground{
			{name: "queue", background: queue},
			{name: "poller", background: poller},
			{name: "reconciler", background: reconciler},
		},
	})

	if err := lifecycle.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if queue.starts != 1 || poller.starts != 1 || reconciler.starts != 1 {
		t.Fatalf("queue/poller/reconciler starts = %d/%d/%d, want 1/1/1", queue.starts, poller.starts, reconciler.starts)
	}
	if err := lifecycle.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if queue.stops != 1 || poller.stops != 1 || reconciler.stops != 1 {
		t.Fatalf("queue/poller/reconciler stops = %d/%d/%d, want 1/1/1", queue.stops, poller.stops, reconciler.stops)
	}
}

type lifecycleOwnerContextKey struct{}

type lifecycleOwnerContextMarker struct {
	value int
}

type lifecycleOwnerContextPersistence struct {
	marker *lifecycleOwnerContextMarker
}

func (p *lifecycleOwnerContextPersistence) AdmitOwner(ctx context.Context) (context.Context, func(), error) {
	return context.WithValue(ctx, lifecycleOwnerContextKey{}, p.marker), func() {}, nil
}

func (*lifecycleOwnerContextPersistence) CloseAdmission()             {}
func (*lifecycleOwnerContextPersistence) Drain(context.Context) error { return nil }
func (*lifecycleOwnerContextPersistence) Close(context.Context) error { return nil }

type lifecycleOwnerContextBackground struct {
	name   string
	marker *lifecycleOwnerContextMarker
	starts int
}

func (b *lifecycleOwnerContextBackground) Start(ctx context.Context) error {
	if got := ctx.Value(lifecycleOwnerContextKey{}); got != b.marker {
		return fmt.Errorf("%s owner marker = %p, want %p", b.name, got, b.marker)
	}
	b.starts++
	return nil
}

func (*lifecycleOwnerContextBackground) Stop(context.Context) error { return nil }

func TestRuntimeLifecycleStartsEveryBackgroundWithAdmittedOwnerContext(t *testing.T) {
	t.Parallel()

	marker := &lifecycleOwnerContextMarker{value: 7}
	backgrounds := []*lifecycleOwnerContextBackground{
		{name: "queue", marker: marker},
		{name: "recorder", marker: marker},
		{name: "cleaner", marker: marker},
	}
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		persistence: &lifecycleOwnerContextPersistence{marker: marker},
		backgrounds: []namedRuntimeBackground{
			{name: backgrounds[0].name, background: backgrounds[0]},
			{name: backgrounds[1].name, background: backgrounds[1]},
			{name: backgrounds[2].name, background: backgrounds[2]},
		},
	})

	if err := lifecycle.Start(context.WithValue(
		t.Context(),
		lifecycleOwnerContextKey{},
		&lifecycleOwnerContextMarker{value: 9},
	)); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = lifecycle.Close(t.Context()) })
	for _, background := range backgrounds {
		if background.starts != 1 {
			t.Fatalf("%s starts = %d, want 1 with admitted owner context", background.name, background.starts)
		}
	}
}
