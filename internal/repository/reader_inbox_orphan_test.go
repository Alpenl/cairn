package repository

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
)

func TestClaimInboxDispatchOrphansTxUsesExactIdentityBoundAndReplicaLocks(t *testing.T) {
	t.Parallel()
	for _, fragment := range []string{
		"active_job.args->>'job_id'=j.id::text",
		"active_job.args->>'inbox_id'=i.id::text",
		"active_job.args->>'expected_metadata_revision'=j.expected_metadata_revision::text",
		"active_job.state IN ('available','pending','retryable','running','scheduled')",
		"LIMIT $2",
		"FOR UPDATE OF i,j SKIP LOCKED",
	} {
		if !strings.Contains(claimInboxDispatchOrphansSQL, fragment) {
			t.Fatalf("claim SQL missing %q:\n%s", fragment, claimInboxDispatchOrphansSQL)
		}
	}

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	mock.ExpectBegin()
	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	jobID, inboxID := uuid.New(), uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta(claimInboxDispatchOrphansSQL)).
		WithArgs("reader_inbox_resummarize", 17).
		WillReturnRows(pgxmock.NewRows([]string{"id", "id", "expected_metadata_revision", "status"}).
			AddRow(jobID, inboxID, int64(9), "running"))

	repo := NewPGXReaderVNextRepository(mock)
	got, err := repo.ClaimInboxDispatchOrphansTx(context.Background(), tx, "reader_inbox_resummarize", 17)
	if err != nil {
		t.Fatalf("ClaimInboxDispatchOrphansTx() error = %v", err)
	}
	if len(got) != 1 || got[0].JobID != jobID || got[0].InboxID != inboxID ||
		got[0].ExpectedMetadataRevision != 9 || got[0].Status != "running" {
		t.Fatalf("ClaimInboxDispatchOrphansTx() = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestResetInboxDispatchOrphanTxMakesRunningAttemptRunnable(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	mock.ExpectBegin()
	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	jobID := uuid.New()
	mock.ExpectExec(`UPDATE reader_inbox_jobs\s+SET status='queued',started_at=NULL,updated_at=NOW\(\)\s+WHERE id=\$1 AND status='running'`).
		WithArgs(jobID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := NewPGXReaderVNextRepository(mock).ResetInboxDispatchOrphanTx(context.Background(), tx, jobID); err != nil {
		t.Fatalf("ResetInboxDispatchOrphanTx() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestCountInboxDispatchOrphansUsesSameExactActiveIdentity(t *testing.T) {
	t.Parallel()
	for _, fragment := range []string{
		"active_job.args->>'job_id'=j.id::text",
		"active_job.args->>'inbox_id'=i.id::text",
		"active_job.args->>'expected_metadata_revision'=j.expected_metadata_revision::text",
	} {
		if !strings.Contains(countInboxDispatchOrphansSQL, fragment) {
			t.Fatalf("count SQL missing %q", fragment)
		}
	}

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery(regexp.QuoteMeta(countInboxDispatchOrphansSQL)).
		WithArgs("reader_inbox_resummarize").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(4)))

	got, err := NewPGXReaderVNextRepository(mock).CountInboxDispatchOrphans(context.Background(), "reader_inbox_resummarize")
	if err != nil || got != 4 {
		t.Fatalf("CountInboxDispatchOrphans() = (%d, %v), want (4, nil)", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}
