package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"webtag/internal/representation"
)

const snapshotTestNamespace = "nOhfumfdD54ioX0N3Gbl8wu7uQDgY8znIEGISwot7CI"

func snapshotVersionContext(revision int64) context.Context {
	return representation.WithVersion(context.Background(), representation.RepresentationVersion{
		ClientDataNamespace: snapshotTestNamespace,
		Components: []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: revision,
		}},
	})
}

func TestSnapshotCacheReloadsBodyForNewRepresentationVersion(t *testing.T) {
	t.Parallel()

	cache := NewSnapshotCache(time.Hour, nil, func(value string) string { return value })
	loads := 0

	first, err := cache.Get(snapshotVersionContext(1), "tags:all", func(context.Context) (string, error) {
		loads++
		return "body-at-version-1", nil
	})
	if err != nil {
		t.Fatalf("Get(version 1) error = %v", err)
	}
	if first != "body-at-version-1" {
		t.Fatalf("Get(version 1) = %q", first)
	}

	second, err := cache.Get(snapshotVersionContext(2), "tags:all", func(context.Context) (string, error) {
		loads++
		return "body-at-version-2", nil
	})
	if err != nil {
		t.Fatalf("Get(version 2) error = %v", err)
	}
	if second != "body-at-version-2" {
		t.Fatalf("Get(version 2) = %q, want body-at-version-2", second)
	}
	if loads != 2 {
		t.Fatalf("loader calls across representation versions = %d, want 2", loads)
	}
}

func TestSnapshotCacheDoesNotServeStaleBodyToVersionedRequest(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()
	cache := NewSnapshotCache(time.Minute, func() time.Time { return now }, func(value string) string { return value })
	ctx := snapshotVersionContext(4)

	if _, err := cache.Get(ctx, "tags:site", func(context.Context) (string, error) {
		return "body-at-version-4", nil
	}); err != nil {
		t.Fatalf("initial Get() error = %v", err)
	}

	now = now.Add(2 * time.Minute)
	loadErr := errors.New("database unavailable")
	got, err := cache.Get(ctx, "tags:site", func(context.Context) (string, error) {
		return "", loadErr
	})
	if !errors.Is(err, loadErr) {
		t.Fatalf("Get() error = %v, want %v", err, loadErr)
	}
	if got != "" {
		t.Fatalf("Get() on versioned load failure = %q, want zero value", got)
	}
}
