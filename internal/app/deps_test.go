package app

import (
	"context"
	"errors"
	"testing"

	"webtag/internal/config"
)

// TestBuildRuntimeReturnsErrorOnInvalidDatabaseURL covers the
// fail-fast path in BuildRuntime — a malformed DSN must surface as a
// constructor error before any other dependency is built. The path was
// previously at 0% coverage so a regression in error wrapping (e.g.
// returning a half-built Runtime alongside the error) would not have
// been caught.
func TestBuildRuntimeReturnsErrorOnInvalidDatabaseURL(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		DatabaseURL: "://broken-dsn",
	}

	runtime, err := BuildRuntime(context.Background(), cfg)
	if err == nil {
		t.Fatal("BuildRuntime returned nil error for malformed DSN")
	}
	if runtime != nil {
		t.Fatalf("BuildRuntime returned non-nil runtime alongside error: %+v", runtime)
	}
}

// TestRuntimeStartCloseAreNilSafe pins the defensive zero/nil guards
// on Runtime.Start and Runtime.Close so a caller wiring partial
// state never panics. These guards also protect a non-nil Runtime
// whose start/close closures were never installed.
func TestRuntimeStartCloseAreNilSafe(t *testing.T) {
	t.Parallel()

	var nilRuntime *Runtime
	if err := nilRuntime.Start(context.Background()); err != nil {
		t.Fatalf("nil Runtime Start = %v, want nil", err)
	}
	if err := nilRuntime.Close(context.Background()); err != nil {
		t.Fatalf("nil Runtime Close = %v, want nil", err)
	}

	emptyRuntime := &Runtime{}
	if err := emptyRuntime.Start(context.Background()); err != nil {
		t.Fatalf("empty Runtime Start = %v, want nil", err)
	}
	if err := emptyRuntime.Close(context.Background()); err != nil {
		t.Fatalf("empty Runtime Close = %v, want nil", err)
	}

	wantErr := errors.New("start failed")
	withFunc := &Runtime{
		start: func(context.Context) error { return wantErr },
		close: func(context.Context) error { return wantErr },
	}
	if err := withFunc.Start(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Start propagation = %v, want %v", err, wantErr)
	}
	if err := withFunc.Close(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Close propagation = %v, want %v", err, wantErr)
	}
}
