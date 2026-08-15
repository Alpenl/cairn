package repository

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pgxmock "github.com/pashagolub/pgxmock/v4"
)

func TestPGXIdempotencyAcquireReturnsOwnedGeneration(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	now := time.Unix(100, 0)
	expiresAt := now.Add(time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta(acquireIdempotencyKeySQL)).
		WithArgs("key", "owner-a", now, expiresAt).
		WillReturnRows(mock.NewRows([]string{"owner_token", "generation", "status", "body", "content_type", "in_flight", "expires_at"}).
			AddRow("owner-a", int64(1), 0, nil, "", true, expiresAt))

	repo := NewPGXIdempotencyRepository(mock)
	acquired, record, err := repo.Acquire(context.Background(), "key", "owner-a", now, expiresAt)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !acquired || record == nil {
		t.Fatalf("Acquire() = %v, %#v; want acquired record", acquired, record)
	}
	if record.OwnerToken != "owner-a" || record.Generation != 1 || !record.InFlight {
		t.Fatalf("claim = %#v, want owner-a generation 1 in-flight", record)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPGXIdempotencyAcquireConflictLoadsCurrentOwner(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	now := time.Unix(100, 0)
	expiresAt := now.Add(time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta(acquireIdempotencyKeySQL)).
		WithArgs("key", "owner-b", now, expiresAt).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(getIdempotencyRecordSQL)).
		WithArgs("key").
		WillReturnRows(mock.NewRows([]string{"owner_token", "generation", "status", "body", "content_type", "in_flight", "expires_at"}).
			AddRow("owner-a", int64(7), http.StatusCreated, []byte(`{"id":"one"}`), "application/json", false, expiresAt))

	repo := NewPGXIdempotencyRepository(mock)
	acquired, record, err := repo.Acquire(context.Background(), "key", "owner-b", now, expiresAt)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if acquired || record == nil || record.OwnerToken != "owner-a" || record.Generation != 7 {
		t.Fatalf("Acquire() = %v, %#v; want current owner-a generation 7", acquired, record)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPGXIdempotencyFinalizeUsesOwnerGenerationCAS(t *testing.T) {
	expiresAt := time.Unix(200, 0)
	for _, test := range []struct {
		name string
		run  func(*PGXIdempotencyRepository) error
		sql  string
		args []any
	}{
		{
			name: "store",
			run: func(repo *PGXIdempotencyRepository) error {
				return repo.Store(context.Background(), "key", "owner-a", 3, 201, []byte("body"), "application/json", expiresAt)
			},
			sql:  storeIdempotencyResponseSQL,
			args: []any{"key", "owner-a", int64(3), 201, []byte("body"), "application/json", expiresAt},
		},
		{
			name: "delete",
			run: func(repo *PGXIdempotencyRepository) error {
				return repo.Delete(context.Background(), "key", "owner-a", 3)
			},
			sql:  deleteIdempotencyKeySQL,
			args: []any{"key", "owner-a", int64(3)},
		},
	} {
		t.Run(test.name+" rejects stale claim", func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock.NewPool() error = %v", err)
			}
			defer mock.Close()
			mock.ExpectExec(regexp.QuoteMeta(test.sql)).WithArgs(test.args...).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			err = test.run(NewPGXIdempotencyRepository(mock))
			if !errors.Is(err, ErrIdempotencyClaimLost) {
				t.Fatalf("error = %v, want ErrIdempotencyClaimLost", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})

		t.Run(test.name+" accepts current claim", func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock.NewPool() error = %v", err)
			}
			defer mock.Close()
			mock.ExpectExec(regexp.QuoteMeta(test.sql)).WithArgs(test.args...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
			if err := test.run(NewPGXIdempotencyRepository(mock)); err != nil {
				t.Fatalf("finalize error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
