package dbintegration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

type readerReattachPersistedState struct {
	lastSequence int64
	hostID       string
	operations   int
	tombstones   int
}

func TestReaderThoughtReattachPostgresPrecedenceStateMatrix(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	reader := service.NewReaderVNextService(repo, nil)
	ctx := t.Context()
	target := seedReaderLifecycleHost(t, pool, model.ReaderHostLink, "reattach-target")

	t.Run("missing source is not reclassified by expected sequence", func(t *testing.T) {
		for _, expectedSequence := range []int64{0, 1} {
			_, err := reader.ReattachThought(ctx, "missing-"+uuid.NewString(), readerReattachRequest(target, expectedSequence, target.revision))
			assertReaderReattachHTTPError(t, err, http.StatusNotFound, "reader_not_found")
		}
	})

	t.Run("active source is not reclassified by expected sequence", func(t *testing.T) {
		host := seedReaderLifecycleHost(t, pool, model.ReaderHostLink, "reattach-active-source")
		thought := seedReaderLifecycleThought(t, repo, ctx, host, "reattach-active", "unique phrase")
		before := snapshotReaderReattachState(t, pool, repo, ctx, thought.id)
		for _, expectedSequence := range []int64{before.lastSequence, before.lastSequence + 1} {
			_, err := reader.ReattachThought(ctx, thought.id, readerReattachRequest(target, expectedSequence, target.revision))
			assertReaderReattachHTTPError(t, err, http.StatusConflict, "thought_reattach_invalid_state")
			assertReaderReattachStateUnchanged(t, pool, repo, ctx, thought.id, before)
		}
	})

	t.Run("historical source resolves missing target before either CAS", func(t *testing.T) {
		thought, sequence := seedTombstonedReaderReattachThought(t, pool, repo, ctx, "reattach-missing-target", "unique phrase")
		before := snapshotReaderReattachState(t, pool, repo, ctx, thought.id)
		missingTarget := readerLifecycleHostFixture{kind: model.ReaderHostLink, id: uuid.New(), revision: 1}
		for _, expectedSequence := range []int64{sequence, sequence + 1} {
			_, err := reader.ReattachThought(ctx, thought.id, readerReattachRequest(missingTarget, expectedSequence, missingTarget.revision))
			assertReaderReattachHTTPError(t, err, http.StatusNotFound, "reader_not_found")
			assertReaderReattachStateUnchanged(t, pool, repo, ctx, thought.id, before)
		}
	})

	t.Run("stale source sequence follows valid lifecycle and target", func(t *testing.T) {
		thought, sequence := seedTombstonedReaderReattachThought(t, pool, repo, ctx, "reattach-stale-source", "unique phrase")
		before := snapshotReaderReattachState(t, pool, repo, ctx, thought.id)
		_, err := reader.ReattachThought(ctx, thought.id, readerReattachRequest(target, sequence+1, target.revision))
		assertReaderReattachHTTPError(t, err, http.StatusConflict, httperr.CodeRevisionConflict)
		assertReaderReattachStateUnchanged(t, pool, repo, ctx, thought.id, before)
	})

	t.Run("stale target revision follows valid lifecycle and source CAS", func(t *testing.T) {
		thought, sequence := seedTombstonedReaderReattachThought(t, pool, repo, ctx, "reattach-stale-target", "unique phrase")
		before := snapshotReaderReattachState(t, pool, repo, ctx, thought.id)
		_, err := reader.ReattachThought(ctx, thought.id, readerReattachRequest(target, sequence, target.revision+1))
		assertReaderReattachHTTPError(t, err, http.StatusConflict, httperr.CodeRevisionConflict)
		assertReaderReattachStateUnchanged(t, pool, repo, ctx, thought.id, before)
	})

	t.Run("non-unique reanchor is zero-write validation failure", func(t *testing.T) {
		thought, sequence := seedTombstonedReaderReattachThought(t, pool, repo, ctx, "reattach-ambiguous", "repeated")
		before := snapshotReaderReattachState(t, pool, repo, ctx, thought.id)
		_, err := reader.ReattachThought(ctx, thought.id, readerReattachRequest(target, sequence, target.revision))
		assertReaderReattachHTTPError(t, err, http.StatusUnprocessableEntity, "invalid_reanchor_ops")
		assertReaderReattachStateUnchanged(t, pool, repo, ctx, thought.id, before)
	})

	t.Run("historical source reattaches with one operation and clears one tombstone", func(t *testing.T) {
		thought, sequence := seedTombstonedReaderReattachThought(t, pool, repo, ctx, "reattach-success", "unique phrase")
		before := snapshotReaderReattachState(t, pool, repo, ctx, thought.id)
		reattached, err := reader.ReattachThought(ctx, thought.id, readerReattachRequest(target, sequence, target.revision))
		if err != nil {
			t.Fatalf("ReattachThought() error = %v", err)
		}
		if reattached.ID != thought.id || reattached.HostID != target.id.String() || reattached.LifecycleStatus != "active" {
			t.Fatalf("ReattachThought() = %#v, want active thought on target %s", reattached, target.id)
		}
		after := snapshotReaderReattachState(t, pool, repo, ctx, thought.id)
		if after.operations != before.operations+1 || after.tombstones != 0 || after.hostID != target.id.String() || after.lastSequence <= before.lastSequence {
			t.Fatalf("reattach persisted state = %#v after %#v, want exactly one operation, no tombstone, and target host", after, before)
		}
	})
}

func TestReaderThoughtReattachPostgresConcurrentTargetRevisionWinsOverWrite(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	reader := service.NewReaderVNextService(repo, nil)
	ctx := t.Context()
	target := seedReaderLifecycleHost(t, pool, model.ReaderHostLink, "reattach-concurrent-target")
	thought, sequence := seedTombstonedReaderReattachThought(t, pool, repo, ctx, "reattach-concurrent-source", "unique phrase")
	before := snapshotReaderReattachState(t, pool, repo, ctx, thought.id)

	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin target lock transaction: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = locker.Rollback(context.Background())
		}
	}()
	var lockedTarget uuid.UUID
	if err := locker.QueryRow(ctx, `SELECT id FROM links WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, target.id).Scan(&lockedTarget); err != nil {
		t.Fatalf("lock target host: %v", err)
	}
	var lockerPID int32
	if err := locker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&lockerPID); err != nil {
		t.Fatalf("read target lock backend pid: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := reader.ReattachThought(ctx, thought.id, readerReattachRequest(target, sequence, target.revision))
		result <- err
	}()
	waitForReaderReattachTargetLock(t, pool, lockerPID)
	if _, err := locker.Exec(ctx, `UPDATE links SET content_revision=content_revision+1 WHERE id=$1`, target.id); err != nil {
		t.Fatalf("concurrently advance target revision: %v", err)
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatalf("release target lock: %v", err)
	}
	committed = true

	select {
	case err := <-result:
		assertReaderReattachHTTPError(t, err, http.StatusConflict, httperr.CodeRevisionConflict)
	case <-time.After(5 * time.Second):
		t.Fatal("ReattachThought did not finish after target revision update")
	}
	assertReaderReattachStateUnchanged(t, pool, repo, ctx, thought.id, before)
}

func seedTombstonedReaderReattachThought(t *testing.T, pool *pgxpool.Pool, repo *repository.PGXReaderVNextRepository, ctx context.Context, label, exact string) (readerLifecycleThoughtFixture, int64) {
	t.Helper()
	host := seedReaderLifecycleHost(t, pool, model.ReaderHostLink, label+"-source")
	thought := seedReaderLifecycleThought(t, repo, ctx, host, label, exact)
	if result, err := repo.SoftDeleteHost(ctx, host.kind, host.id); err != nil || !result.Changed {
		t.Fatalf("tombstone source %s: result=%#v error=%v", thought.id, result, err)
	}
	stored, err := repo.GetThought(ctx, thought.id)
	if err != nil {
		t.Fatalf("read tombstoned thought %s: %v", thought.id, err)
	}
	if stored.LifecycleStatus != "tombstone" {
		t.Fatalf("thought lifecycle = %q, want tombstone", stored.LifecycleStatus)
	}
	return thought, stored.LastSequence
}

func readerReattachRequest(target readerLifecycleHostFixture, sequence, revision int64) dto.ReaderThoughtReattachRequest {
	return dto.ReaderThoughtReattachRequest{
		TargetHostKind:       string(target.kind),
		TargetHostID:         target.id.String(),
		ExpectedLastSequence: sequence,
		ExpectedHostRevision: revision,
	}
}

func snapshotReaderReattachState(t *testing.T, pool *pgxpool.Pool, repo *repository.PGXReaderVNextRepository, ctx context.Context, thoughtID string) readerReattachPersistedState {
	t.Helper()
	thought, err := repo.GetThought(ctx, thoughtID)
	if err != nil {
		t.Fatalf("get thought %s: %v", thoughtID, err)
	}
	state := readerReattachPersistedState{lastSequence: thought.LastSequence, hostID: thought.HostID}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE annotation_id=$1`, thoughtID).Scan(&state.operations); err != nil {
		t.Fatalf("count thought operations for %s: %v", thoughtID, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_tombstones WHERE thought_id=$1`, thoughtID).Scan(&state.tombstones); err != nil {
		t.Fatalf("count thought tombstones for %s: %v", thoughtID, err)
	}
	return state
}

func assertReaderReattachStateUnchanged(t *testing.T, pool *pgxpool.Pool, repo *repository.PGXReaderVNextRepository, ctx context.Context, thoughtID string, want readerReattachPersistedState) {
	t.Helper()
	if got := snapshotReaderReattachState(t, pool, repo, ctx, thoughtID); got != want {
		t.Fatalf("failed reattach state = %#v, want %#v", got, want)
	}
}

func assertReaderReattachHTTPError(t *testing.T, err error, wantStatus int, wantCode string) {
	t.Helper()
	carrier, ok := httperr.As(err)
	if !ok {
		t.Fatalf("error = %v, want HTTP error", err)
	}
	if carrier.HTTPStatus() != wantStatus {
		t.Fatalf("HTTP status = %d, want %d; error=%v", carrier.HTTPStatus(), wantStatus, err)
	}
	coder, ok := carrier.(httperr.ErrorCoder)
	if !ok || coder.HTTPErrorCode() != wantCode {
		got := ""
		if ok {
			got = coder.HTTPErrorCode()
		}
		t.Fatalf("error code = %q, want %q; error=%v", got, wantCode, err)
	}
}

func waitForReaderReattachTargetLock(t *testing.T, pool *pgxpool.Pool, lockerPID int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1
				FROM pg_locks waiter
				JOIN pg_locks holder
				  ON waiter.locktype='transactionid'
				 AND holder.locktype='transactionid'
				 AND waiter.transactionid=holder.transactionid
				WHERE NOT waiter.granted AND holder.granted AND holder.pid=$1
			)`, lockerPID).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe target lock wait: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ReattachThought never waited on the target lock")
}
