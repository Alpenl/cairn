package dbintegration

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/database"
)

type gatedPoolOperation struct {
	name string
	run  func(context.Context) error
}

func TestAcquisitionGateRiverQueueStopIsListenerBarrier(t *testing.T) {
	adminPool := StartPostgres(t)
	applicationName := "rf7_gate_" + uuid.NewString()
	dsn, err := url.Parse(DSN(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	query := dsn.Query()
	query.Set("application_name", applicationName)
	dsn.RawQuery = query.Encode()

	gate := database.NewAcquisitionGate()
	pool, err := database.Open(t.Context(), dsn.String(), database.Options{
		MaxConns:        8,
		AcquisitionGate: gate,
	})
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(pool.Close)
	ownerCtx, owner, err := gate.AdmitOwner(t.Context())
	if err != nil {
		t.Fatalf("AdmitOwner() error = %v", err)
	}
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Start(ownerCtx); err != nil {
		owner.Revoke()
		t.Fatalf("queue.Start() error = %v", err)
	}
	queueStopped := false
	t.Cleanup(func() {
		if !queueStopped {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = queue.Stop(stopCtx)
		}
		owner.Revoke()
	})
	waitForApplicationSessions(t, adminPool, applicationName, true)

	gate.CloseAdmission()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 40*time.Millisecond)
	drainErr := gate.Drain(drainCtx, pool)
	cancelDrain()
	if !errors.Is(drainErr, database.ErrShutdownDeadline) {
		t.Fatalf("Drain() before queue Stop error = %v, want ErrShutdownDeadline", drainErr)
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	if err := queue.Stop(stopCtx); err != nil {
		cancelStop()
		t.Fatalf("queue.Stop() after admission close error = %v", err)
	}
	cancelStop()
	queueStopped = true
	waitForApplicationSessions(t, adminPool, applicationName, false)

	owner.Revoke()
	finalDrainCtx, cancelFinalDrain := context.WithTimeout(context.Background(), time.Second)
	if err := gate.Drain(finalDrainCtx, pool); err != nil {
		cancelFinalDrain()
		t.Fatalf("Drain() after queue Stop and owner revoke error = %v", err)
	}
	cancelFinalDrain()
}

func TestAcquisitionGateCoversPgxpoolSurfaces(t *testing.T) {
	StartPostgres(t)
	gate := database.NewAcquisitionGate()
	pool, err := database.Open(t.Context(), DSN(t), database.Options{
		MaxConns:        8,
		AcquisitionGate: gate,
	})
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(pool.Close)

	type ownerValueKey struct{}
	ownerCtx, owner, err := gate.AdmitOwner(context.WithValue(t.Context(), ownerValueKey{}, "runtime-owner"))
	if err != nil {
		t.Fatalf("AdmitOwner() error = %v", err)
	}
	gate.CloseAdmission()

	operations := gatedPoolOperations(pool)
	for _, operation := range operations {
		operation := operation
		t.Run("owner/"+operation.name, func(t *testing.T) {
			if err := operation.run(ownerCtx); err != nil {
				t.Fatalf("owner operation error = %v", err)
			}
		})
		t.Run("unowned/"+operation.name, func(t *testing.T) {
			err := operation.run(context.Background())
			if !errors.Is(err, database.ErrPersistenceAdmissionClosed) {
				t.Fatalf("unowned operation error = %v, want ErrPersistenceAdmissionClosed", err)
			}
		})
	}

	owner.Revoke()
	for _, operation := range operations {
		err := operation.run(ownerCtx)
		if !errors.Is(err, database.ErrPersistenceAdmissionClosed) {
			t.Fatalf("revoked owner %s error = %v, want ErrPersistenceAdmissionClosed", operation.name, err)
		}
	}
}

func TestAcquisitionGateDrainWaitsForRealPgxpoolHoldingsThenCloseIsSynchronous(t *testing.T) {
	StartPostgres(t)
	gate := database.NewAcquisitionGate()
	pool, err := database.Open(t.Context(), DSN(t), database.Options{
		MaxConns:        8,
		AcquisitionGate: gate,
	})
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			pool.Close()
		}
	})

	ownerCtx, owner, err := gate.AdmitOwner(t.Context())
	if err != nil {
		t.Fatalf("AdmitOwner() error = %v", err)
	}
	conn, err := pool.Acquire(ownerCtx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	rows, err := pool.Query(ownerCtx, "SELECT generate_series(1, 2)")
	if err != nil {
		conn.Release()
		t.Fatalf("Query() error = %v", err)
	}
	row := pool.QueryRow(ownerCtx, "SELECT 1")
	batch := &pgx.Batch{}
	batch.Queue("SELECT 1")
	results := pool.SendBatch(ownerCtx, batch)
	tx, err := pool.Begin(ownerCtx)
	if err != nil {
		conn.Release()
		rows.Close()
		_ = results.Close()
		t.Fatalf("Begin() error = %v", err)
	}

	gate.CloseAdmission()
	owner.Revoke()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 40*time.Millisecond)
	drainErr := gate.Drain(drainCtx, pool)
	cancelDrain()
	if !errors.Is(drainErr, database.ErrShutdownDeadline) || !errors.Is(drainErr, context.DeadlineExceeded) {
		t.Fatalf("Drain() with holdings error = %v, want shutdown deadline", drainErr)
	}
	if acquired := pool.Stat().AcquiredConns(); acquired < 5 {
		t.Fatalf("AcquiredConns() with conn/rows/row/batch/tx = %d, want at least 5", acquired)
	}

	conn.Release()
	rows.Close()
	var rowValue int
	if err := row.Scan(&rowValue); err != nil {
		t.Fatalf("held QueryRow.Scan() error = %v", err)
	}
	if err := results.Close(); err != nil {
		t.Fatalf("held BatchResults.Close() error = %v", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("held Tx.Rollback() error = %v", err)
	}

	finalDrainCtx, cancelFinalDrain := context.WithTimeout(context.Background(), time.Second)
	if err := gate.Drain(finalDrainCtx, pool); err != nil {
		cancelFinalDrain()
		t.Fatalf("Drain() after releases error = %v", err)
	}
	cancelFinalDrain()

	closeStarted := time.Now()
	pool.Close()
	closed = true
	if elapsed := time.Since(closeStarted); elapsed > time.Second {
		t.Fatalf("synchronous pool.Close() after drain took %v, want <= 1s", elapsed)
	}
}

func gatedPoolOperations(pool *pgxpool.Pool) []gatedPoolOperation {
	return []gatedPoolOperation{
		{name: "Acquire", run: func(ctx context.Context) error {
			conn, err := pool.Acquire(ctx)
			if err == nil {
				conn.Release()
			}
			return err
		}},
		{name: "AcquireFunc", run: func(ctx context.Context) error {
			return pool.AcquireFunc(ctx, func(*pgxpool.Conn) error { return nil })
		}},
		{name: "Exec", run: func(ctx context.Context) error {
			_, err := pool.Exec(ctx, "SELECT 1")
			return err
		}},
		{name: "Query", run: func(ctx context.Context) error {
			rows, err := pool.Query(ctx, "SELECT 1")
			if err != nil {
				return err
			}
			defer rows.Close()
			if !rows.Next() {
				return rows.Err()
			}
			var value int
			return rows.Scan(&value)
		}},
		{name: "QueryRow", run: func(ctx context.Context) error {
			var value int
			return pool.QueryRow(ctx, "SELECT 1").Scan(&value)
		}},
		{name: "SendBatch", run: func(ctx context.Context) error {
			batch := &pgx.Batch{}
			batch.Queue("SELECT 1")
			results := pool.SendBatch(ctx, batch)
			var value int
			scanErr := results.QueryRow().Scan(&value)
			return errors.Join(scanErr, results.Close())
		}},
		{name: "Begin", run: func(ctx context.Context) error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			return tx.Rollback(ctx)
		}},
		{name: "BeginTx", run: func(ctx context.Context) error {
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				return err
			}
			return tx.Rollback(ctx)
		}},
		{name: "CopyFrom", run: func(ctx context.Context) error {
			_, err := pool.CopyFrom(
				ctx,
				pgx.Identifier{"links"},
				[]string{"id"},
				pgx.CopyFromRows(nil),
			)
			return err
		}},
		{name: "Ping", run: pool.Ping},
	}
}

func waitForApplicationSessions(
	t *testing.T,
	adminPool *pgxpool.Pool,
	applicationName string,
	wantListener bool,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var listeners int
		if err := adminPool.QueryRow(t.Context(), `SELECT count(*)
			FROM pg_stat_activity
			WHERE application_name = $1 AND query LIKE 'LISTEN %'`, applicationName).Scan(&listeners); err != nil {
			t.Fatalf("query River listener sessions: %v", err)
		}
		if (listeners > 0) == wantListener {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("River listener sessions = %d, want listener present = %v", listeners, wantListener)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
