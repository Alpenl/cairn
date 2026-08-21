package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
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
