package database

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAcquisitionGateKeepsExistingOwnerAdmittedAfterClosing(t *testing.T) {
	t.Parallel()

	gate := NewAcquisitionGate()
	ownerCtx, owner, err := gate.AdmitOwner(context.Background())
	if err != nil {
		t.Fatalf("AdmitOwner() error = %v", err)
	}
	gate.CloseAdmission()

	if err := gate.Check(ownerCtx); err != nil {
		t.Fatalf("existing owner Check() error = %v", err)
	}
	if err := gate.Check(context.Background()); !errors.Is(err, ErrPersistenceAdmissionClosed) {
		t.Fatalf("unowned Check() error = %v, want ErrPersistenceAdmissionClosed", err)
	}
	if _, _, err := gate.AdmitOwner(context.Background()); !errors.Is(err, ErrPersistenceAdmissionClosed) {
		t.Fatalf("AdmitOwner() after close error = %v, want ErrPersistenceAdmissionClosed", err)
	}

	owner.Revoke()
	if err := gate.Check(ownerCtx); !errors.Is(err, ErrPersistenceAdmissionClosed) {
		t.Fatalf("revoked owner Check() error = %v, want ErrPersistenceAdmissionClosed", err)
	}
}

func TestAcquisitionGateDrainWaitsForOwnersAndPoolCounts(t *testing.T) {
	t.Parallel()

	gate := NewAcquisitionGate()
	_, owner, err := gate.AdmitOwner(context.Background())
	if err != nil {
		t.Fatalf("AdmitOwner() error = %v", err)
	}
	gate.CloseAdmission()
	var acquired atomic.Int32
	var constructing atomic.Int32
	acquired.Store(1)
	constructing.Store(1)

	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		drained <- gate.drain(ctx, func() acquisitionCounts {
			return acquisitionCounts{acquired: acquired.Load(), constructing: constructing.Load()}
		})
	}()

	select {
	case err := <-drained:
		t.Fatalf("drain returned before owner/count release: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	owner.Revoke()
	select {
	case err := <-drained:
		t.Fatalf("drain returned while pool counts were non-zero: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	acquired.Store(0)
	select {
	case err := <-drained:
		t.Fatalf("drain returned while a connection was still constructing: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	constructing.Store(0)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("drain() error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("drain did not observe owner and pool counts reaching zero")
	}
}

func TestAcquisitionGateDrainReportsShutdownDeadline(t *testing.T) {
	t.Parallel()

	gate := NewAcquisitionGate()
	_, owner, err := gate.AdmitOwner(context.Background())
	if err != nil {
		t.Fatalf("AdmitOwner() error = %v", err)
	}
	defer owner.Revoke()
	gate.CloseAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = gate.drain(ctx, func() acquisitionCounts {
		return acquisitionCounts{acquired: 1}
	})
	if !errors.Is(err, ErrShutdownDeadline) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain() error = %v, want ErrShutdownDeadline and context deadline exceeded", err)
	}
}

func TestInstallAcquisitionGateComposesPgxpoolHooks(t *testing.T) {
	t.Parallel()

	cfg, err := pgxpool.ParseConfig("postgres://webtag:secret@localhost:5432/webtag")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	var beforeConnectCalls, prepareCalls, shouldPingCalls int
	cfg.BeforeConnect = func(context.Context, *pgx.ConnConfig) error {
		beforeConnectCalls++
		return nil
	}
	cfg.PrepareConn = func(context.Context, *pgx.Conn) (bool, error) {
		prepareCalls++
		return true, nil
	}
	cfg.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool {
		shouldPingCalls++
		return true
	}

	gate := NewAcquisitionGate()
	ownerCtx, owner, err := gate.AdmitOwner(context.Background())
	if err != nil {
		t.Fatalf("AdmitOwner() error = %v", err)
	}
	defer owner.Revoke()
	installAcquisitionGate(cfg, gate)
	gate.CloseAdmission()

	if err := cfg.BeforeConnect(context.Background(), cfg.ConnConfig); !errors.Is(err, ErrPersistenceAdmissionClosed) {
		t.Fatalf("unowned BeforeConnect() error = %v, want ErrPersistenceAdmissionClosed", err)
	}
	if ok, err := cfg.PrepareConn(context.Background(), nil); !ok || !errors.Is(err, ErrPersistenceAdmissionClosed) {
		t.Fatalf("unowned PrepareConn() = (%v, %v), want (true, ErrPersistenceAdmissionClosed)", ok, err)
	}
	if cfg.ShouldPing(context.Background(), pgxpool.ShouldPingParams{IdleDuration: 2 * time.Second}) {
		t.Fatal("unowned ShouldPing() = true after admission closed")
	}
	if beforeConnectCalls != 0 || prepareCalls != 0 || shouldPingCalls != 0 {
		t.Fatalf("prior hook calls for rejected acquisition = %d/%d/%d, want 0/0/0", beforeConnectCalls, prepareCalls, shouldPingCalls)
	}

	if err := cfg.BeforeConnect(ownerCtx, cfg.ConnConfig); err != nil {
		t.Fatalf("owner BeforeConnect() error = %v", err)
	}
	if ok, err := cfg.PrepareConn(ownerCtx, nil); !ok || err != nil {
		t.Fatalf("owner PrepareConn() = (%v, %v), want (true, nil)", ok, err)
	}
	if !cfg.ShouldPing(ownerCtx, pgxpool.ShouldPingParams{IdleDuration: 2 * time.Second}) {
		t.Fatal("owner ShouldPing() = false, want prior hook result")
	}
	if beforeConnectCalls != 1 || prepareCalls != 1 || shouldPingCalls != 1 {
		t.Fatalf("prior owner hook calls = %d/%d/%d, want 1/1/1", beforeConnectCalls, prepareCalls, shouldPingCalls)
	}
}

func TestAcquisitionOwnerRevokeIsIdempotentAndCannotCrossGates(t *testing.T) {
	t.Parallel()

	firstGate := NewAcquisitionGate()
	ownerCtx, owner, err := firstGate.AdmitOwner(context.Background())
	if err != nil {
		t.Fatalf("AdmitOwner() error = %v", err)
	}
	owner.Revoke()
	owner.Revoke()

	secondGate := NewAcquisitionGate()
	secondGate.CloseAdmission()
	if err := secondGate.Check(ownerCtx); !errors.Is(err, ErrPersistenceAdmissionClosed) {
		t.Fatalf("foreign owner Check() error = %v, want ErrPersistenceAdmissionClosed", err)
	}
}
