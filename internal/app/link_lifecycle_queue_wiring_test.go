package app

import (
	"reflect"
	"testing"

	"webtag/internal/config"
	"webtag/internal/repository"
	"webtag/internal/worker"
)

// This is an assembly contract, not a repository behavior test: Reader's
// lifecycle paths must share the production River queue constructed by app.
// Keeping it database-free makes an omitted binding fail immediately.
func TestRuntimeFeatureSharedBindsReaderLinkLifecycleQueue(t *testing.T) {
	t.Parallel()

	reader := repository.NewPGXReaderVNextRepository(nil)
	queue := &worker.RiverQueue{}
	layer := &persistenceLayer{reader: reader}

	_ = newRuntimeFeatureShared(config.Config{}, layer, queue)

	field := reflect.ValueOf(reader).Elem().FieldByName("linkLifecycleQueue")
	if !field.IsValid() {
		t.Fatal("Reader repository no longer exposes the lifecycle queue wiring field")
	}
	if field.IsNil() {
		t.Fatal("Reader lifecycle queue is nil: runtime assembly omitted the River queue binding")
	}
	if got, want := field.Elem().Pointer(), reflect.ValueOf(queue).Pointer(); got != want {
		t.Fatalf("Reader lifecycle queue pointer = %#x, want production River queue %#x", got, want)
	}
}
