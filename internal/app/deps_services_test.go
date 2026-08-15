package app

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/config"
	"webtag/internal/service/urllock"
)

func TestBuildURLLockerUseAdvisoryLocksWhenPoolCanReserveCallbackConnections(t *testing.T) {
	t.Parallel()

	submit := buildURLLocker(config.Config{DB: config.DBConfig{MaxConns: 4}}, &pgxpool.Pool{})
	submitLocker, ok := submit.(*urllock.AdvisoryURLLocker)
	if !ok {
		t.Fatalf("submission locker = %T, want *urllock.AdvisoryURLLocker", submit)
	}
	if submitLocker == nil {
		t.Fatal("advisory locker must not be nil")
	}
}

func TestBuildURLLockerUsesInProcessLockWithSingleConnection(t *testing.T) {
	t.Parallel()

	submit := buildURLLocker(config.Config{DB: config.DBConfig{MaxConns: 1}}, &pgxpool.Pool{})
	submitLocker, ok := submit.(*urllock.InProcessURLLocker)
	if !ok {
		t.Fatalf("submission locker = %T, want *urllock.InProcessURLLocker", submit)
	}
	if submitLocker == nil {
		t.Fatal("in-process fallback locker must not be nil")
	}
}

func TestBuildURLLockerFallsBackToInProcessWithoutPool(t *testing.T) {
	t.Parallel()

	submit := buildURLLocker(config.Config{DB: config.DBConfig{MaxConns: 4}}, nil)
	submitLocker, ok := submit.(*urllock.InProcessURLLocker)
	if !ok {
		t.Fatalf("submission locker = %T, want *urllock.InProcessURLLocker", submit)
	}
	if submitLocker == nil {
		t.Fatal("in-process fallback locker must not be nil")
	}
}
