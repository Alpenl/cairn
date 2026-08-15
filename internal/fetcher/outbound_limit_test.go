package fetcher

import (
	"context"
	"testing"
	"time"
)

func TestNewOutboundLimiterInvalidArgs(t *testing.T) {
	cases := []struct {
		name   string
		events int
		window time.Duration
	}{
		{"zero events", 0, time.Second},
		{"negative events", -1, time.Second},
		{"zero window", 1, 0},
		{"negative window", 1, -time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewOutboundLimiter(tc.events, tc.window); got != nil {
				t.Errorf("NewOutboundLimiter(%d, %v) = %p, want nil", tc.events, tc.window, got)
			}
		})
	}
}

func TestOutboundLimiterNilSafe(t *testing.T) {
	var l *OutboundLimiter
	if err := l.Wait(context.Background(), "example.com"); err != nil {
		t.Fatalf("nil limiter Wait returned %v, want nil", err)
	}
}

func TestOutboundLimiterEmptyHostSkips(t *testing.T) {
	// Burst=1, refill rate slow; empty host should bypass limiter entirely.
	l := NewOutboundLimiter(1, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, ""); err != nil {
		t.Fatalf("empty host Wait returned %v, want nil (bypass)", err)
	}
}

func TestOutboundLimiterBucketsPerHost(t *testing.T) {
	// 1 event per very-long window means the second Wait on the SAME host
	// can't be served before ctx expires; a Wait on a DIFFERENT host can.
	l := NewOutboundLimiter(1, time.Hour)

	// Consume the only token for host A.
	if err := l.Wait(context.Background(), "host-a.example"); err != nil {
		t.Fatalf("first Wait host-a: %v", err)
	}

	// Second Wait on host A within a short ctx must fail because the
	// next token isn't due for almost an hour.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, "host-a.example"); err == nil {
		t.Fatal("second Wait on host-a unexpectedly succeeded; want ctx-deadline error")
	}

	// Different host has its own bucket, so it still has a token ready.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := l.Wait(ctx2, "host-b.example"); err != nil {
		t.Fatalf("Wait host-b should not be limited by host-a: %v", err)
	}
}

func TestOutboundLimiterCtxCancelled(t *testing.T) {
	l := NewOutboundLimiter(1, time.Hour)
	// Consume the only token.
	if err := l.Wait(context.Background(), "h"); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	if err := l.Wait(ctx, "h"); err == nil {
		t.Fatal("Wait with cancelled ctx returned nil, want error")
	}
}
