package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/representation"
)

type blockingIdentityStore struct {
	identity     representation.ClientIdentity
	calls        atomic.Int64
	firstEntered chan struct{}
	releaseFirst chan struct{}
	once         sync.Once
}

func (s *blockingIdentityStore) Current(context.Context) (representation.ClientIdentity, error) {
	call := s.calls.Add(1)
	if call == 1 {
		s.once.Do(func() { close(s.firstEntered) })
		<-s.releaseFirst
	}
	return s.identity, nil
}

func TestInstallationIdentityServiceCoalescesOnlyOverlappingReads(t *testing.T) {
	t.Parallel()

	identity, err := representation.NewClientIdentity(uuid.MustParse("b30c0c90-ea52-4b72-82de-acdebd8bcc21"))
	if err != nil {
		t.Fatalf("NewClientIdentity: %v", err)
	}
	store := &blockingIdentityStore{
		identity:     identity,
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	service := NewInstallationIdentityService(store)

	const readers = 32
	start := make(chan struct{})
	results := make(chan representation.ClientIdentity, readers)
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
			got, readErr := service.Current(context.Background())
			results <- got
			errs <- readErr
		}()
	}
	ready.Wait()
	close(start)
	select {
	case <-store.firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first identity read did not reach the store")
	}
	time.Sleep(50 * time.Millisecond)
	close(store.releaseFirst)
	done.Wait()
	close(results)
	close(errs)

	for readErr := range errs {
		if readErr != nil {
			t.Fatalf("Current() error = %v", readErr)
		}
	}
	for got := range results {
		if got != identity {
			t.Fatalf("Current() = %#v, want %#v", got, identity)
		}
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("overlapping store reads = %d, want 1", got)
	}

	if _, err := service.Current(context.Background()); err != nil {
		t.Fatalf("Current() after completed batch error = %v", err)
	}
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("store reads after completed batch = %d, want 2", got)
	}
}

type fixedIdentityStore struct {
	identity representation.ClientIdentity
	err      error
}

func (s fixedIdentityStore) Current(context.Context) (representation.ClientIdentity, error) {
	return s.identity, s.err
}

func TestInstallationIdentityServiceRejectsInvalidStoreResults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		store InstallationIdentityStore
	}{
		{name: "nil store", store: nil},
		{name: "invalid identity", store: fixedIdentityStore{}},
		{name: "store error", store: fixedIdentityStore{err: errors.New("database unavailable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := NewInstallationIdentityService(tc.store)
			identity, err := service.Current(context.Background())
			if err == nil || identity.Valid() {
				t.Fatalf("Current() = %#v, error=%v; want failure", identity, err)
			}
		})
	}
}
