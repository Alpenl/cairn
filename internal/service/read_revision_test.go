package service

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/representation"
)

type blockingReadRevisionStore struct {
	base         representation.VersionBase
	calls        atomic.Int64
	firstEntered chan struct{}
	releaseFirst chan struct{}
	once         sync.Once
}

func (s *blockingReadRevisionStore) Current(context.Context, representation.ComponentSet) (representation.VersionBase, error) {
	call := s.calls.Add(1)
	if call == 1 {
		s.once.Do(func() { close(s.firstEntered) })
		<-s.releaseFirst
	}
	return s.base, nil
}

func TestReadRevisionServiceCoalescesOnlyOverlappingReads(t *testing.T) {
	t.Parallel()

	store := &blockingReadRevisionStore{
		base: representation.VersionBase{
			RepresentationNamespace: uuid.MustParse("b30c0c90-ea52-4b72-82de-acdebd8bcc21"),
			Components: []representation.Component{{
				Name:     representation.LibraryComponent,
				Revision: 17,
			}},
		},
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	service := NewReadRevisionService(store)
	ctx := context.Background()
	components, err := representation.NewComponentSet(representation.LibraryComponent)
	if err != nil {
		t.Fatalf("NewComponentSet() error = %v", err)
	}

	const readers = 32
	start := make(chan struct{})
	results := make(chan representation.VersionBase, readers)
	errs := make(chan error, readers)
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(readers)
	done.Add(readers)
	for range readers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			base, err := service.Current(ctx, components)
			results <- base
			errs <- err
		}()
	}

	ready.Wait()
	close(start)
	select {
	case <-store.firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first revision read did not reach the store")
	}
	// Let the remaining callers join the in-flight read before it completes.
	time.Sleep(50 * time.Millisecond)
	close(store.releaseFirst)
	done.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Current() error = %v", err)
		}
	}
	for got := range results {
		if got.RepresentationNamespace != store.base.RepresentationNamespace || !slices.Equal(got.Components, store.base.Components) {
			t.Fatalf("Current() = %#v, want %#v", got, store.base)
		}
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("overlapping store reads = %d, want 1", got)
	}

	if _, err := service.Current(ctx, components); err != nil {
		t.Fatalf("Current() after completed batch error = %v", err)
	}
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("store reads after completed batch = %d, want 2", got)
	}
}

type componentReadRevisionStore struct {
	entered sync.WaitGroup
	release chan struct{}
}

func (s *componentReadRevisionStore) Current(_ context.Context, components representation.ComponentSet) (representation.VersionBase, error) {
	s.entered.Done()
	<-s.release
	base := representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("b30c0c90-ea52-4b72-82de-acdebd8bcc21"),
	}
	if components.Key() == string(representation.LibraryComponent) {
		base.Components = []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: 29,
		}}
	}
	return base, nil
}

func TestReadRevisionServiceDoesNotMergeDifferentComponentSets(t *testing.T) {
	t.Parallel()

	store := &componentReadRevisionStore{release: make(chan struct{})}
	store.entered.Add(2)
	service := NewReadRevisionService(store)
	ctx := context.Background()
	identityComponents, err := representation.NewComponentSet()
	if err != nil {
		t.Fatalf("NewComponentSet(identity) error = %v", err)
	}
	libraryComponents, err := representation.NewComponentSet(representation.LibraryComponent)
	if err != nil {
		t.Fatalf("NewComponentSet(library) error = %v", err)
	}

	results := make(chan representation.VersionBase, 2)
	errs := make(chan error, 2)
	var done sync.WaitGroup
	done.Add(2)
	for _, components := range []representation.ComponentSet{identityComponents, libraryComponents} {
		components := components
		go func() {
			defer done.Done()
			base, readErr := service.Current(ctx, components)
			results <- base
			errs <- readErr
		}()
	}

	entered := make(chan struct{})
	go func() {
		store.entered.Wait()
		close(entered)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(store.release)
		done.Wait()
		t.Fatal("identity and library component reads did not independently enter the store")
	}
	close(store.release)
	done.Wait()
	close(results)
	close(errs)

	for readErr := range errs {
		if readErr != nil {
			t.Fatalf("Current() error = %v", readErr)
		}
	}
	sawIdentity, sawLibrary := false, false
	for base := range results {
		switch {
		case len(base.Components) == 0:
			sawIdentity = true
		case len(base.Components) == 1 &&
			base.Components[0].Name == representation.LibraryComponent &&
			base.Components[0].Revision == 29:
			sawLibrary = true
		default:
			t.Fatalf("unexpected version base %#v", base)
		}
	}
	if !sawIdentity || !sawLibrary {
		t.Fatalf("results saw identity=%v library=%v, want both", sawIdentity, sawLibrary)
	}
}
