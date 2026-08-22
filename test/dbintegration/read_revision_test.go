package dbintegration

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/repository"
)

func TestInstallationIdentityStateIsSingleton(t *testing.T) {
	pool := StartPostgres(t)

	var stateRows int
	var namespace uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT count(*),
		(SELECT representation_namespace FROM installation_state WHERE singleton)
		FROM installation_state`).Scan(&stateRows, &namespace); err != nil {
		t.Fatalf("read installation identity state: %v", err)
	}
	if stateRows != 1 {
		t.Fatalf("installation_state rows = %d, want 1", stateRows)
	}
	if namespace == uuid.Nil {
		t.Fatal("installation representation namespace is nil")
	}

	if _, err := pool.Exec(t.Context(), `INSERT INTO installation_state (singleton) VALUES (false)`); err == nil {
		t.Fatal("installation_state accepted a non-singleton row")
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO installation_state (singleton) VALUES (true)`); err == nil {
		t.Fatal("installation_state accepted a second singleton row")
	}
}

func TestInstallationIdentityRepositoryReadsNamespace(t *testing.T) {
	pool := StartPostgres(t)
	repository := repository.NewPGXInstallationIdentityRepository(pool)

	var want uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT representation_namespace FROM installation_state WHERE singleton`).Scan(&want); err != nil {
		t.Fatalf("read expected namespace: %v", err)
	}
	identity, err := repository.Current(t.Context())
	if err != nil {
		t.Fatalf("Current(): %v", err)
	}
	if identity.RepresentationNamespace != want || !identity.Valid() {
		t.Fatalf("Current() = %#v, want valid namespace %s", identity, want)
	}
}

func TestInstallationIdentityRepositoryFailsClosedWithoutState(t *testing.T) {
	pool := StartPostgres(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin missing-state probe: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()

	if _, err := tx.Exec(t.Context(), `DELETE FROM installation_state`); err != nil {
		t.Fatalf("delete installation state in probe transaction: %v", err)
	}
	_, err = repository.NewPGXInstallationIdentityRepository(tx).Current(t.Context())
	if err == nil || !strings.Contains(err.Error(), "installation state is missing") {
		t.Fatalf("Current() missing-state error=%v, want actionable fail-closed error", err)
	}
}

func TestInstallationIdentityRepositoryAcceptsTransactionalQuerier(t *testing.T) {
	pool := StartPostgres(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()

	transactionalNamespace := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if _, err := tx.Exec(t.Context(), `UPDATE installation_state SET representation_namespace=$1 WHERE singleton`, transactionalNamespace); err != nil {
		t.Fatalf("update transaction-local namespace: %v", err)
	}
	inside, err := repository.NewPGXInstallationIdentityRepository(tx).Current(t.Context())
	if err != nil {
		t.Fatalf("read transaction-local identity: %v", err)
	}
	outside, err := repository.NewPGXInstallationIdentityRepository(pool).Current(t.Context())
	if err != nil {
		t.Fatalf("read committed identity: %v", err)
	}
	if inside.RepresentationNamespace != transactionalNamespace || inside.RepresentationNamespace == outside.RepresentationNamespace {
		t.Fatalf("transaction-local identity=%#v committed identity=%#v", inside, outside)
	}

	if err := tx.Rollback(t.Context()); err != nil && err != pgx.ErrTxClosed {
		t.Fatalf("rollback transaction: %v", err)
	}
}
