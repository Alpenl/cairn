package dbintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// TestReaderThoughtReattachOpAndAppendShareHostFirstLockOrder holds the target
// host in SHARE mode, which makes the reattach operation wait at its first lock. A
// concurrent append for the same Thought and host must still finish while that
// blocker is held. With Thought -> host ordering, reattach would hold
// the Thought row while waiting on the host and the append would form a real
// PostgreSQL deadlock cycle after taking its host SHARE lock.
func TestReaderThoughtReattachOpAndAppendShareHostFirstLockOrder(t *testing.T) {
	pool := StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	reader := repository.NewPGXReaderVNextRepository(pool)
	sourceID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/reattach-source", "Source", "shared quote", "summary")
	targetID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/reattach-target", "Target", "shared quote", "summary")
	thoughtID := "thought-reattach-lock-" + uuid.NewString()
	sourceTarget := readerVNextJSON(t, map[string]any{
		"kind": "saved-content", "host_id": sourceID.String(),
		"version": map[string]any{"content_revision": 1},
	})
	sourcePayload := readerVNextJSON(t, map[string]any{
		"body": "source thought", "link_id": sourceID.String(), "source": "self",
		"quote": map[string]any{"exact": "shared quote", "start": 0, "end": 12},
	})
	if _, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID: "reattach-seed-" + uuid.NewString(), DeviceID: "device-seed", LogicalClock: 1,
		OperationKind: "add", AnnotationID: thoughtID, HostKind: "link", HostID: sourceID.String(),
		Target: sourceTarget, Payload: sourcePayload,
	}}); err != nil {
		t.Fatalf("seed thought: %v", err)
	}
	if err := reader.MarkThoughtHostTombstones(ctx, "link", sourceID.String(), "lock-order"); err != nil {
		t.Fatalf("tombstone source thought: %v", err)
	}
	seeded, err := reader.GetThought(ctx, thoughtID)
	if err != nil {
		t.Fatalf("read tombstoned thought: %v", err)
	}

	reattachPool := openNamedPool(t, "webtag_reader_reattach_host_order")
	appendPool := openNamedPool(t, "webtag_reader_append_host_order")
	reattachRepo := repository.NewPGXReaderVNextRepository(reattachPool)
	appendRepo := repository.NewPGXReaderVNextRepository(appendPool)
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin target host blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	var lockedID uuid.UUID
	if err := blocker.QueryRow(ctx, `
		SELECT id FROM links
		WHERE id=$1 AND deleted_at IS NULL
		FOR SHARE`, targetID).Scan(&lockedID); err != nil {
		t.Fatalf("hold target host share lock: %v", err)
	}

	target := readerVNextJSON(t, map[string]any{
		"kind": "saved-content", "host_id": targetID.String(),
		"version": map[string]any{"content_revision": 1},
	})
	reattachPayload := readerVNextJSON(t, map[string]any{
		"reattach": map[string]any{
			"expected_last_sequence": seeded.LastSequence,
			"expected_host_revision": 1,
		},
	})
	type reattachOutcome struct {
		acks []model.ReaderThoughtAck
		err  error
	}
	reattachDone := make(chan reattachOutcome, 1)
	go func() {
		acks, reattachErr := reattachRepo.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
			OpID: "reattach-command-" + uuid.NewString(), DeviceID: "device-reattach",
			LogicalClock: seeded.WinnerKey.LogicalClock + 1, OperationKind: "update",
			AnnotationID: thoughtID, HostKind: "link", HostID: targetID.String(),
			Target: target, Payload: reattachPayload,
			Reattach: &model.ReaderThoughtReattachOperation{
				ExpectedLastSequence: seeded.LastSequence, ExpectedHostRevision: 1,
			},
		}})
		reattachDone <- reattachOutcome{acks: acks, err: reattachErr}
	}()
	waitForPostgresLock(t, ctx, pool, "webtag_reader_reattach_host_order")

	appendPayload := readerVNextJSON(t, map[string]any{
		"body": "append wins", "link_id": targetID.String(), "source": "self",
		"quote": map[string]any{"exact": "shared quote", "start": 0, "end": 12},
	})
	appendDone := make(chan struct {
		acks []model.ReaderThoughtAck
		err  error
	}, 1)
	go func() {
		acks, appendErr := appendRepo.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
			OpID: "reattach-race-" + uuid.NewString(), DeviceID: "device-append", LogicalClock: 10,
			OperationKind: "update", AnnotationID: thoughtID, HostKind: "link", HostID: targetID.String(),
			Target: target, Payload: appendPayload,
		}})
		appendDone <- struct {
			acks []model.ReaderThoughtAck
			err  error
		}{acks: acks, err: appendErr}
	}()

	var appended struct {
		acks []model.ReaderThoughtAck
		err  error
	}
	select {
	case appended = <-appendDone:
	case <-time.After(2 * time.Second):
		t.Fatal("AppendThoughtOps remained blocked behind a reattach operation waiting on the same host")
	}
	assertNotDeadlock(t, "append", appended.err)
	if appended.err != nil || len(appended.acks) != 1 || appended.acks[0].Disposition != "applied" {
		t.Fatalf("AppendThoughtOps = acks=%#v err=%v, want one applied operation", appended.acks, appended.err)
	}

	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release target host blocker: %v", err)
	}
	reattached := <-reattachDone
	assertNotDeadlock(t, "reattach", reattached.err)
	if !errors.Is(reattached.err, repository.ErrRevisionConflict) || len(reattached.acks) != 0 {
		t.Fatalf("reattach op = acks=%#v error=%v, want ErrRevisionConflict after append changed the Thought", reattached.acks, reattached.err)
	}
	current, err := reader.GetThought(ctx, thoughtID)
	if err != nil {
		t.Fatalf("read final thought: %v", err)
	}
	if current.HostID != sourceID.String() || current.Body != "source thought" || current.LifecycleStatus != "tombstone" {
		t.Fatalf("historical read = %+v, want immutable source snapshot", current)
	}
	var projectedHostID, projectedBody, projectedWinnerOpID string
	var projectedSequence int64
	if err := pool.QueryRow(ctx, `
		SELECT host_id,body,last_sequence,winner_op_id
		FROM reader_thoughts
		WHERE id=$1`, thoughtID).Scan(
		&projectedHostID, &projectedBody, &projectedSequence, &projectedWinnerOpID,
	); err != nil {
		t.Fatalf("read final thought projection: %v", err)
	}
	if projectedHostID != targetID.String() || projectedBody != "append wins" ||
		projectedSequence != appended.acks[0].Sequence || projectedWinnerOpID != appended.acks[0].OpID {
		t.Fatalf("final projection = host=%q body=%q sequence=%d winner=%q, want append winner on target host", projectedHostID, projectedBody, projectedSequence, projectedWinnerOpID)
	}
}

func TestReaderThoughtPostgresUsesLogicalClockAndListsConflicts(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool, "https://reader-vnext.example/thought-sync", "Thought sync", "Thought sync body", "Thought sync summary")
	thoughtID := "thought-" + uuid.NewString()
	target := readerVNextJSON(t, map[string]any{
		"kind":    "saved-content",
		"host_id": linkID.String(),
		"version": map[string]any{"content_revision": 1},
	})
	winningPayload := readerVNextJSON(t, map[string]any{
		"body":    "logical clock winner",
		"link_id": linkID.String(),
		"quote":   map[string]any{"exact": "Thought sync body"},
	})
	losingPayload := readerVNextJSON(t, map[string]any{
		"body":    "late but older operation",
		"link_id": linkID.String(),
		"quote":   map[string]any{"exact": "Thought sync body"},
	})
	acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{
		{
			OpID:          "op-winning-clock",
			DeviceID:      "device-a",
			LogicalClock:  20,
			OperationKind: "add",
			AnnotationID:  thoughtID,
			HostKind:      "link",
			HostID:        linkID.String(),
			Target:        target,
			Payload:       winningPayload,
		},
		{
			OpID:          "op-losing-clock",
			DeviceID:      "device-z",
			LogicalClock:  10,
			OperationKind: "update",
			AnnotationID:  thoughtID,
			HostKind:      "link",
			HostID:        linkID.String(),
			Target:        target,
			Payload:       losingPayload,
		},
	})
	if err != nil {
		t.Fatalf("AppendThoughtOps: %v", err)
	}
	if len(acks) != 2 || acks[0].Sequence >= acks[1].Sequence {
		t.Fatalf("AppendThoughtOps acks = %#v, want ordered append sequences", acks)
	}
	winnerKey := model.ReaderThoughtVersionKey{
		LogicalClock: 20,
		DeviceID:     "device-a",
		OpID:         "op-winning-clock",
	}
	loserKey := model.ReaderThoughtVersionKey{
		LogicalClock: 10,
		DeviceID:     "device-z",
		OpID:         "op-losing-clock",
	}
	if acks[0].Disposition != "applied" || acks[0].SubmittedKey != winnerKey || acks[0].WinnerKey != winnerKey {
		t.Fatalf("winner ack = %#v, want applied with complete winner key", acks[0])
	}
	if acks[1].Disposition != "superseded" || acks[1].SubmittedKey != loserKey || acks[1].WinnerKey != winnerKey {
		t.Fatalf("loser ack = %#v, want superseded with final winner key", acks[1])
	}

	thought, err := reader.GetThought(ctx, thoughtID)
	if err != nil {
		t.Fatalf("GetThought: %v", err)
	}
	if thought.Body != "logical clock winner" || thought.LastSequence != acks[0].Sequence {
		t.Fatalf("materialized thought = %+v, want first high-clock operation", thought)
	}

	conflicts, next, err := reader.ListThoughtConflicts(ctx, "", 20)
	if err != nil {
		t.Fatalf("ListThoughtConflicts: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].Loser.OpID != "op-losing-clock" || conflicts[0].Winner.OpID != "op-winning-clock" {
		t.Fatalf("conflicts = %#v, want durable loser/winner pair", conflicts)
	}
	var conflictPayload map[string]any
	if err := json.Unmarshal(conflicts[0].Loser.Payload, &conflictPayload); err != nil {
		t.Fatalf("decode conflict payload: %v", err)
	}
	if next == "" || conflictPayload["body"] != "late but older operation" {
		t.Fatalf("conflict payload/cursor = %s/%q, want original payload and cursor", conflicts[0].Loser.Payload, next)
	}
	frozenFirstEvent, err := json.Marshal(conflicts[0])
	if err != nil {
		t.Fatalf("marshal first immutable event: %v", err)
	}
	for _, snapshot := range []struct {
		name string
		op   model.ReaderThoughtConflictOperation
	}{
		{name: "loser", op: conflicts[0].Loser},
		{name: "winner", op: conflicts[0].Winner},
	} {
		var persistedCreatedAt time.Time
		if err := pool.QueryRow(ctx, `
			SELECT created_at
			FROM reader_thought_ops
			WHERE op_id=$1`, snapshot.op.OpID).Scan(&persistedCreatedAt); err != nil {
			t.Fatalf("read %s operation created_at: %v", snapshot.name, err)
		}
		if snapshot.op.CreatedAt.IsZero() || !snapshot.op.CreatedAt.Equal(persistedCreatedAt) {
			t.Fatalf("%s event created_at = %s, want persisted %s", snapshot.name, snapshot.op.CreatedAt, persistedCreatedAt)
		}
	}

	retry, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID:          "op-losing-clock",
		DeviceID:      "device-z",
		LogicalClock:  10,
		OperationKind: "update",
		AnnotationID:  thoughtID,
		HostKind:      "link",
		HostID:        linkID.String(),
		Target:        target,
		Payload:       losingPayload,
	}})
	if err != nil {
		t.Fatalf("retry AppendThoughtOps: %v", err)
	}
	if len(retry) != 1 || retry[0].Disposition != "duplicate" ||
		retry[0].Sequence != acks[1].Sequence || retry[0].SubmittedKey != loserKey ||
		retry[0].WinnerKey != winnerKey {
		t.Fatalf("duplicate ack = %#v, want stable sequence and winner", retry)
	}
	conflictsAfterRetry, _, err := reader.ListThoughtConflicts(ctx, "", 20)
	if err != nil {
		t.Fatalf("ListThoughtConflicts after retry: %v", err)
	}
	if len(conflictsAfterRetry) != 1 {
		t.Fatalf("conflicts after duplicate retry = %#v, want one unchanged history row", conflictsAfterRetry)
	}

	// A later winner must append a second event rather than rewriting the
	// earlier loser's winner_at_detection snapshot.
	third, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID: "op-third-winner", DeviceID: "device-c", LogicalClock: 30,
		OperationKind: "update", AnnotationID: thoughtID, HostKind: "link", HostID: linkID.String(),
		Target: target, Payload: readerVNextJSON(t, map[string]any{"body": "third winner", "link_id": linkID.String(), "quote": map[string]any{"exact": "Thought sync body"}}),
	}})
	if err != nil || len(third) != 1 || third[0].Disposition != "applied" {
		t.Fatalf("third winner ack = %#v, %v", third, err)
	}
	events, _, err := reader.ListThoughtConflicts(ctx, "", 20)
	if err != nil {
		t.Fatalf("ListThoughtConflicts after third winner: %v", err)
	}
	if len(events) != 2 || events[0].Loser.OpID != "op-losing-clock" || events[0].Winner.OpID != "op-winning-clock" || events[1].Loser.OpID != "op-winning-clock" || events[1].Winner.OpID != "op-third-winner" {
		t.Fatalf("immutable supersession events = %#v, want B->A then A->C", events)
	}
	if afterThird, marshalErr := json.Marshal(events[0]); marshalErr != nil || string(afterThird) != string(frozenFirstEvent) {
		t.Fatalf("first immutable event changed after A->B->C: before=%s after=%s error=%v", frozenFirstEvent, afterThird, marshalErr)
	}

	type candidate struct {
		clock         int64
		deviceID      string
		opIDSuffix    string
		operationKind string
		body          string
	}
	matrix := []struct {
		name   string
		first  candidate
		second candidate
		winner int
	}{
		{
			name: "higher clock update beats delete",
			first: candidate{
				clock: 2, deviceID: "device-a", opIDSuffix: "-update", operationKind: "update", body: "updated-high-clock",
			},
			second: candidate{
				clock: 1, deviceID: "device-z", opIDSuffix: "-delete", operationKind: "delete", body: "before-delete",
			},
			winner: 0,
		},
		{
			name: "higher clock delete beats update",
			first: candidate{
				clock: 2, deviceID: "device-a", opIDSuffix: "-delete", operationKind: "delete", body: "before-delete",
			},
			second: candidate{
				clock: 1, deviceID: "device-z", opIDSuffix: "-update", operationKind: "update", body: "updated-low-clock",
			},
			winner: 0,
		},
		{
			name: "UTF-8 device bytes break equal clock",
			first: candidate{
				clock: 3, deviceID: "device-\U00010000", opIDSuffix: "-update", operationKind: "update", body: "updated-utf8-winner",
			},
			second: candidate{
				clock: 3, deviceID: "device-\ue000", opIDSuffix: "-delete", operationKind: "delete", body: "before-delete",
			},
			winner: 0,
		},
		{
			name: "operation id breaks equal device",
			first: candidate{
				clock: 4, deviceID: "device-same", opIDSuffix: "-z-update", operationKind: "update", body: "updated-op-winner",
			},
			second: candidate{
				clock: 4, deviceID: "device-same", opIDSuffix: "-a-delete", operationKind: "delete", body: "before-delete",
			},
			winner: 0,
		},
	}
	for caseIndex, testCase := range matrix {
		for _, reverse := range []bool{false, true} {
			thoughtID := fmt.Sprintf("matrix-%d-%t-%s", caseIndex, reverse, uuid.NewString())
			prefix := fmt.Sprintf("matrix-%d-%t-%s", caseIndex, reverse, uuid.NewString())
			candidates := []candidate{testCase.first, testCase.second}
			winner := candidates[testCase.winner]
			ordered := candidates
			if reverse {
				ordered = []candidate{candidates[1], candidates[0]}
			}
			ops := make([]model.ReaderThoughtOp, 0, len(ordered))
			for _, item := range ordered {
				ops = append(ops, model.ReaderThoughtOp{
					OpID:          prefix + item.opIDSuffix,
					DeviceID:      item.deviceID,
					LogicalClock:  item.clock,
					OperationKind: item.operationKind,
					AnnotationID:  thoughtID,
					HostKind:      "link",
					HostID:        linkID.String(),
					Target:        target,
					Payload: readerVNextJSON(t, map[string]any{
						"body": item.body, "link_id": linkID.String(),
						"quote": map[string]any{"exact": "Thought sync body"},
					}),
				})
			}
			matrixAcks, err := reader.AppendThoughtOps(ctx, ops)
			if err != nil {
				t.Fatalf("%s reverse=%v AppendThoughtOps: %v", testCase.name, reverse, err)
			}
			winnerKey := model.ReaderThoughtVersionKey{
				LogicalClock: winner.clock,
				DeviceID:     winner.deviceID,
				OpID:         prefix + winner.opIDSuffix,
			}
			for _, ack := range matrixAcks {
				wantDisposition := "superseded"
				if ack.SubmittedKey == winnerKey {
					wantDisposition = "applied"
				}
				if ack.Disposition != wantDisposition || ack.WinnerKey != winnerKey {
					t.Fatalf("%s reverse=%v ack = %#v, want disposition %s winner %+v", testCase.name, reverse, ack, wantDisposition, winnerKey)
				}
			}
			var materialized model.ReaderThought
			var userDeleted bool
			if err := pool.QueryRow(ctx, `
				SELECT winner_logical_clock,winner_device_id,winner_op_id,deleted,user_deleted,body
				FROM reader_thoughts
				WHERE id=$1`, thoughtID).Scan(
				&materialized.WinnerKey.LogicalClock, &materialized.WinnerKey.DeviceID,
				&materialized.WinnerKey.OpID, &materialized.Deleted, &userDeleted, &materialized.Body,
			); err != nil {
				t.Fatalf("%s reverse=%v read materialized projection: %v", testCase.name, reverse, err)
			}
			terminallyDeleted := winner.operationKind == "delete" || ordered[0].operationKind == "delete"
			if materialized.WinnerKey != winnerKey || materialized.Deleted != terminallyDeleted || userDeleted != terminallyDeleted {
				t.Fatalf("%s reverse=%v materialized = %#v user_deleted=%v, want winner %+v deleted=%v", testCase.name, reverse, materialized, userDeleted, winnerKey, terminallyDeleted)
			}
			if terminallyDeleted {
				if materialized.Body != "" {
					t.Fatalf("%s reverse=%v body = %q, want terminal deletion scrubbed", testCase.name, reverse, materialized.Body)
				}
				if item, err := reader.GetThought(ctx, thoughtID); !errors.Is(err, repository.ErrNotFound) || item != nil {
					t.Fatalf("%s reverse=%v GetThought() = %#v, %v, want terminal deletion hidden", testCase.name, reverse, item, err)
				}
			} else if materialized.Body != winner.body {
				t.Fatalf("%s reverse=%v body = %q, want %q", testCase.name, reverse, materialized.Body, winner.body)
			}
		}
	}

}

func TestReaderThoughtPostgresSerializesConcurrentSupersessionTransitions(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool, "https://reader-vnext.example/concurrent-supersession", "Concurrent supersession", "Concurrent supersession body", "Concurrent supersession summary")
	thoughtID := "concurrent-supersession-" + uuid.NewString()
	target := readerVNextJSON(t, map[string]any{
		"kind": "saved-content", "host_id": linkID.String(), "version": map[string]any{"content_revision": 1},
	})
	operation := func(opID string, clock int64, body string) model.ReaderThoughtOp {
		return model.ReaderThoughtOp{
			OpID: opID, DeviceID: "concurrent-device-" + opID, LogicalClock: clock,
			OperationKind: "update", AnnotationID: thoughtID, HostKind: "link", HostID: linkID.String(),
			Target: target, Payload: readerVNextJSON(t, map[string]any{
				"body": body, "source": "user", "link_id": linkID.String(),
				"quote": map[string]any{"exact": "Concurrent supersession body"},
			}),
		}
	}
	base := operation("base", 10, "base winner")
	if acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{base}); err != nil || len(acks) != 1 || acks[0].Disposition != "applied" {
		t.Fatalf("append base winner = %#v, %v", acks, err)
	}
	if _, ordinaryCursor, err := reader.ListThoughtsSince(ctx, "", 10); err != nil || ordinaryCursor == "" {
		t.Fatalf("advance ordinary Thought cursor = %q, %v", ordinaryCursor, err)
	}

	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire advisory-lock connection: %v", err)
	}
	defer lockConn.Release()
	lockKey := thoughtID
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, lockKey); err != nil {
		t.Fatalf("hold supersession advisory lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = lockConn.Exec(t.Context(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey)
		}
	}()

	first := operation("first", 20, "first candidate")
	second := operation("second", 30, "second candidate")
	type appendResult struct {
		opID string
		err  error
	}
	results := make(chan appendResult, 2)
	started := make(chan struct{}, 2)
	appendConcurrent := func(op model.ReaderThoughtOp) {
		started <- struct{}{}
		_, appendErr := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{op})
		results <- appendResult{opID: op.OpID, err: appendErr}
	}
	go appendConcurrent(first)
	go appendConcurrent(second)
	<-started
	<-started
	select {
	case result := <-results:
		t.Fatalf("concurrent append %s bypassed the Thought advisory lock: %v", result.opID, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey); err != nil {
		t.Fatalf("release supersession advisory lock: %v", err)
	}
	locked = false
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("concurrent append %s: %v", result.opID, result.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for serialized Thought appends")
		}
	}

	events, _, err := reader.ListThoughtConflicts(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListThoughtConflicts after concurrent appends: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("concurrent supersession events = %#v, want one event for each durable loser", events)
	}
	losers := map[string]bool{}
	for _, event := range events {
		if losers[event.Loser.OpID] {
			t.Fatalf("duplicate durable loser event for %q: %#v", event.Loser.OpID, events)
		}
		losers[event.Loser.OpID] = true
	}
	if !losers[base.OpID] || !losers[first.OpID] || losers[second.OpID] {
		t.Fatalf("concurrent durable loser set = %#v, want base and first only", losers)
	}

	if acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{first}); err != nil || len(acks) != 1 || acks[0].Disposition != "duplicate" {
		t.Fatalf("retry first concurrent append = %#v, %v", acks, err)
	}
	if afterRetry, _, err := reader.ListThoughtConflicts(ctx, "", 10); err != nil || len(afterRetry) != len(events) {
		t.Fatalf("events after duplicate retry = %#v, %v; want unchanged", afterRetry, err)
	}
}

func TestReaderThoughtPostgresSerializesConcurrentInitialThoughtCreators(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool, "https://reader-vnext.example/concurrent-initial-supersession", "Concurrent initial supersession", "Concurrent initial supersession body", "Concurrent initial supersession summary")
	thoughtID := "concurrent-initial-supersession-" + uuid.NewString()
	target := readerVNextJSON(t, map[string]any{
		"kind": "saved-content", "host_id": linkID.String(), "version": map[string]any{"content_revision": 1},
	})
	operation := func(opID string, clock int64, body string) model.ReaderThoughtOp {
		return model.ReaderThoughtOp{
			OpID: opID, DeviceID: "concurrent-initial-device-" + opID, LogicalClock: clock,
			OperationKind: "add", AnnotationID: thoughtID, HostKind: "link", HostID: linkID.String(),
			Target: target, Payload: readerVNextJSON(t, map[string]any{
				"body": body, "source": "user", "link_id": linkID.String(),
				"quote": map[string]any{"exact": "Concurrent initial supersession body"},
			}),
		}
	}

	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire initial-creator advisory-lock connection: %v", err)
	}
	defer lockConn.Release()
	lockKey := thoughtID
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, lockKey); err != nil {
		t.Fatalf("hold initial-creator advisory lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = lockConn.Exec(t.Context(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey)
		}
	}()

	low := operation("initial-low", 20, "initial lower candidate")
	high := operation("initial-high", 30, "initial higher candidate")
	type appendResult struct {
		opID string
		err  error
	}
	results := make(chan appendResult, 2)
	started := make(chan struct{}, 2)
	appendConcurrent := func(op model.ReaderThoughtOp) {
		started <- struct{}{}
		_, appendErr := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{op})
		results <- appendResult{opID: op.OpID, err: appendErr}
	}
	go appendConcurrent(low)
	go appendConcurrent(high)
	<-started
	<-started
	select {
	case result := <-results:
		t.Fatalf("initial Thought append %s bypassed the advisory lock: %v", result.opID, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey); err != nil {
		t.Fatalf("release initial-creator advisory lock: %v", err)
	}
	locked = false
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("initial Thought append %s: %v", result.opID, result.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for serialized initial Thought creators")
		}
	}

	thought, err := reader.GetThought(ctx, thoughtID)
	if err != nil {
		t.Fatalf("GetThought after initial creators: %v", err)
	}
	winnerKey := model.ReaderThoughtVersionKey{LogicalClock: high.LogicalClock, DeviceID: high.DeviceID, OpID: high.OpID}
	if thought.WinnerKey != winnerKey || thought.Body != "initial higher candidate" {
		t.Fatalf("initial materialized thought = %#v, want high-clock winner %+v", thought, winnerKey)
	}
	events, _, err := reader.ListThoughtConflicts(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListThoughtConflicts after initial creators: %v", err)
	}
	if len(events) != 1 || events[0].Loser.OpID != low.OpID || events[0].Winner.OpID != high.OpID {
		t.Fatalf("initial supersession events = %#v, want low -> high once", events)
	}
}

func TestReaderThoughtPostgresRecoveryCASRollsBackRejectedCandidate(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool, "https://reader-vnext.example/recovery-cas", "Recovery CAS", "Recovery CAS body", "Recovery CAS summary")
	thoughtID := "recovery-cas-" + uuid.NewString()
	target := readerVNextJSON(t, map[string]any{
		"kind": "saved-content", "host_id": linkID.String(), "version": map[string]any{"content_revision": 1},
	})
	winning := model.ReaderThoughtOp{
		OpID: "winner-op", DeviceID: "winner-device", LogicalClock: 10,
		OperationKind: "add", AnnotationID: thoughtID, HostKind: "link", HostID: linkID.String(),
		Target: target, Payload: readerVNextJSON(t, map[string]any{
			"body": "current winner", "link_id": linkID.String(), "quote": map[string]any{"exact": "Recovery CAS body"},
		}),
	}
	if acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{winning}); err != nil || len(acks) != 1 || acks[0].Disposition != "applied" {
		t.Fatalf("append winner = %#v, %v", acks, err)
	}
	loser := model.ReaderThoughtOp{
		OpID: "loser-op", DeviceID: "loser-device", LogicalClock: 4,
		OperationKind: "update", AnnotationID: thoughtID, HostKind: "link", HostID: linkID.String(),
		Target: target, Payload: readerVNextJSON(t, map[string]any{
			"body": "durable loser", "link_id": linkID.String(), "quote": map[string]any{"exact": "Recovery CAS body"},
		}),
	}
	if acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{loser}); err != nil || len(acks) != 1 || acks[0].Disposition != "superseded" {
		t.Fatalf("append durable loser = %#v, %v", acks, err)
	}

	recovery := model.ReaderThoughtOp{
		OpID: "recovery-op", DeviceID: "recovery-device", LogicalClock: 11,
		OperationKind: "update", AnnotationID: thoughtID, HostKind: "link", HostID: linkID.String(),
		Target: target, Payload: readerVNextJSON(t, map[string]any{
			"body": "recovered loser", "link_id": linkID.String(), "quote": map[string]any{"exact": "Recovery CAS body"},
		}),
		RecoveryOf:        &model.ReaderThoughtVersionKey{LogicalClock: loser.LogicalClock, DeviceID: loser.DeviceID, OpID: loser.OpID},
		ExpectedWinnerKey: &model.ReaderThoughtVersionKey{LogicalClock: 9, DeviceID: "stale-device", OpID: "stale-op"},
	}
	if _, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{recovery}); !errors.Is(err, repository.ErrReaderThoughtRecoveryConflict) {
		t.Fatalf("stale recovery error = %v, want ErrReaderThoughtRecoveryConflict", err)
	}

	// The rejected insert must have rolled back. Reusing its id with the live
	// key therefore appends a candidate instead of returning a duplicate.
	recovery.ExpectedWinnerKey = &model.ReaderThoughtVersionKey{
		LogicalClock: 10, DeviceID: "winner-device", OpID: "winner-op",
	}
	acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{recovery})
	if err != nil || len(acks) != 1 || acks[0].Disposition != "applied" {
		t.Fatalf("recovery after CAS refresh = %#v, %v", acks, err)
	}
	thought, err := reader.GetThought(ctx, thoughtID)
	if err != nil || thought.Body != "recovered loser" || thought.WinnerKey.OpID != recovery.OpID {
		t.Fatalf("recovered thought = %#v, %v", thought, err)
	}
}

func TestReaderThoughtPostgresRecoveryRetryAcknowledgesAfterHostTrash(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/recovery-retry-after-trash", "Recovery retry", "Recovery retry body", "Recovery retry summary")
	thoughtID := "recovery-retry-after-trash-" + uuid.NewString()
	target := readerVNextJSON(t, map[string]any{
		"kind": "saved-content", "host_id": linkID.String(), "version": map[string]any{"content_revision": 1},
	})
	winner := model.ReaderThoughtOp{
		OpID: "recovery-retry-winner-" + uuid.NewString(), DeviceID: "winner-device", LogicalClock: 10,
		OperationKind: "add", AnnotationID: thoughtID, HostKind: "link", HostID: linkID.String(),
		Target: target, Payload: readerVNextJSON(t, map[string]any{
			"body": "current winner", "link_id": linkID.String(), "quote": map[string]any{"exact": "Recovery retry body"},
		}),
	}
	if acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{winner}); err != nil || len(acks) != 1 || acks[0].Disposition != "applied" {
		t.Fatalf("append winner = %#v, %v", acks, err)
	}
	loser := model.ReaderThoughtOp{
		OpID: "recovery-retry-loser-" + uuid.NewString(), DeviceID: "loser-device", LogicalClock: 4,
		OperationKind: "update", AnnotationID: thoughtID, HostKind: "link", HostID: linkID.String(),
		Target: target, Payload: readerVNextJSON(t, map[string]any{
			"body": "durable loser", "link_id": linkID.String(), "quote": map[string]any{"exact": "Recovery retry body"},
		}),
	}
	if acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{loser}); err != nil || len(acks) != 1 || acks[0].Disposition != "superseded" {
		t.Fatalf("append durable loser = %#v, %v", acks, err)
	}

	recovery := model.ReaderThoughtOp{
		OpID: "recovery-retry-op-" + uuid.NewString(), DeviceID: "recovery-device", LogicalClock: 11,
		OperationKind: "update", AnnotationID: thoughtID, HostKind: "link", HostID: linkID.String(),
		Target: target, Payload: readerVNextJSON(t, map[string]any{
			"body": "recovered loser", "link_id": linkID.String(), "quote": map[string]any{"exact": "Recovery retry body"},
		}),
		RecoveryOf:        &model.ReaderThoughtVersionKey{LogicalClock: loser.LogicalClock, DeviceID: loser.DeviceID, OpID: loser.OpID},
		ExpectedWinnerKey: &model.ReaderThoughtVersionKey{LogicalClock: winner.LogicalClock, DeviceID: winner.DeviceID, OpID: winner.OpID},
	}
	firstAcks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{recovery})
	if err != nil || len(firstAcks) != 1 || firstAcks[0].Disposition != "applied" {
		t.Fatalf("accept recovery before trash = %#v, %v", firstAcks, err)
	}
	firstAck := firstAcks[0]

	trashResult, err := reader.SoftDeleteHost(ctx, model.ReaderHostLink, linkID)
	if err != nil || !trashResult.Changed || trashResult.State != model.ReaderHostTrashed {
		t.Fatalf("trash link after accepted recovery = %+v, %v", trashResult, err)
	}

	retryAcks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{recovery})
	if err != nil {
		t.Fatalf("retry accepted recovery after trash: %v", err)
	}
	if len(retryAcks) != 1 {
		t.Fatalf("retry accepted recovery acknowledgements = %#v, want one", retryAcks)
	}
	retryAck := retryAcks[0]
	if retryAck.Disposition != "duplicate" || retryAck.Sequence != firstAck.Sequence || retryAck.SubmittedKey != firstAck.SubmittedKey {
		t.Fatalf("retry accepted recovery acknowledgement = %#v, want duplicate replay of %#v", retryAck, firstAck)
	}

	newRecovery := recovery
	newRecovery.OpID = "recovery-retry-new-op-" + uuid.NewString()
	if _, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{newRecovery}); !errors.Is(err, repository.ErrReaderThoughtLinkMismatch) {
		t.Fatalf("new recovery on trashed link error = %v, want ErrReaderThoughtLinkMismatch", err)
	}
}

type readerConversionThoughtFixture struct {
	linkID          uuid.UUID
	thoughtID       string
	target          json.RawMessage
	quote           json.RawMessage
	body            string
	source          string
	hostBody        string
	url             string
	initialSequence int64
}

func seedReaderConversionThought(t *testing.T, pool *pgxpool.Pool, reader *repository.PGXReaderVNextRepository, ctx context.Context, label string) readerConversionThoughtFixture {
	t.Helper()
	fixture := readerConversionThoughtFixture{
		thoughtID: "conversion-thought-" + uuid.NewString(),
		body:      "frozen conversion thought " + label,
		source:    "conversion-source",
		hostBody:  "frozen original host document " + label,
		url:       "https://reader-vnext.example/conversion-" + uuid.NewString(),
	}
	fixture.linkID = seedReaderVNextSavedLink(t, pool, fixture.url, "Conversion "+label, fixture.hostBody, "conversion summary")
	fixture.target = readerVNextJSON(t, map[string]any{
		"kind":    "saved-content",
		"host_id": fixture.linkID.String(),
		"version": map[string]any{"content_revision": 1},
	})
	fixture.quote = readerVNextJSON(t, map[string]any{
		"exact":  "conversion anchor",
		"prefix": "before ",
		"suffix": " after",
		"start":  7,
		"end":    24,
	})
	acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID:          "conversion-add-" + uuid.NewString(),
		DeviceID:      "conversion-test",
		LogicalClock:  7,
		OperationKind: "add",
		AnnotationID:  fixture.thoughtID,
		HostKind:      "link",
		HostID:        fixture.linkID.String(),
		Target:        fixture.target,
		Payload: readerVNextJSON(t, map[string]any{
			"body":    fixture.body,
			"source":  fixture.source,
			"link_id": fixture.linkID.String(),
			"quote":   fixture.quote,
		}),
	}})
	if err != nil {
		t.Fatalf("seed conversion thought: %v", err)
	}
	if len(acks) != 1 || acks[0].Sequence <= 0 {
		t.Fatalf("seed conversion thought acks = %#v", acks)
	}
	fixture.initialSequence = acks[0].Sequence
	return fixture
}

func assertReaderJSONEqual(t *testing.T, label string, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode %s actual JSON: %v; raw=%s", label, err, got)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode %s expected JSON: %v; raw=%s", label, err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s = %s, want %s", label, got, want)
	}
}

func assertReaderConversionReplay(t *testing.T, thought model.ReaderThought, fixture readerConversionThoughtFixture) {
	t.Helper()
	if thought.ID != fixture.thoughtID || thought.HostKind != "link" || thought.HostID != fixture.linkID.String() ||
		thought.LinkID == nil || *thought.LinkID != fixture.linkID || thought.Body != fixture.body || thought.Source != fixture.source ||
		thought.LifecycleStatus != "tombstone" || thought.LifecycleReason == nil || *thought.LifecycleReason != "link_converted_to_site" || thought.TombstonedAt == nil {
		t.Fatalf("conversion replay = %+v, want frozen link tombstone", thought)
	}
	assertReaderJSONEqual(t, "conversion replay target", thought.Target, fixture.target)
	assertReaderJSONEqual(t, "conversion replay quote", thought.Quote, fixture.quote)
	wantHostSnapshot, err := json.Marshal(fixture.hostBody)
	if err != nil {
		t.Fatalf("encode expected host snapshot: %v", err)
	}
	assertReaderJSONEqual(t, "conversion replay original host snapshot", thought.OriginalHostSnapshot, wantHostSnapshot)
}

func assertReaderSnapshotReplayContract(t *testing.T, raw []byte, fixture readerConversionThoughtFixture, requireLinkURL bool) {
	t.Helper()
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode frozen snapshot: %v; raw=%s", err, raw)
	}
	for _, field := range []string{
		"snapshot_version", "id", "host_kind", "host_id", "link_id", "type", "body", "target", "quote", "source",
		"created_at", "updated_at", "original_host_snapshot", "original_host_identity", "frozen_at",
	} {
		if len(snapshot[field]) == 0 {
			t.Fatalf("frozen snapshot missing %q: %s", field, raw)
		}
	}
	var snapshotVersion int
	if err := json.Unmarshal(snapshot["snapshot_version"], &snapshotVersion); err != nil || snapshotVersion != 1 {
		t.Fatalf("snapshot version = %d (%v), want 1", snapshotVersion, err)
	}
	var id, hostKind, hostID, typeName, body, source string
	if err := json.Unmarshal(snapshot["id"], &id); err != nil || id != fixture.thoughtID {
		t.Fatalf("snapshot id = %q (%v), want %q", id, err, fixture.thoughtID)
	}
	if err := json.Unmarshal(snapshot["host_kind"], &hostKind); err != nil || hostKind != "link" {
		t.Fatalf("snapshot host kind = %q (%v), want link", hostKind, err)
	}
	if err := json.Unmarshal(snapshot["host_id"], &hostID); err != nil || hostID != fixture.linkID.String() {
		t.Fatalf("snapshot host id = %q (%v), want %s", hostID, err, fixture.linkID)
	}
	if err := json.Unmarshal(snapshot["type"], &typeName); err != nil || typeName != "thought" {
		t.Fatalf("snapshot type = %q (%v), want thought", typeName, err)
	}
	if err := json.Unmarshal(snapshot["body"], &body); err != nil || body != fixture.body {
		t.Fatalf("snapshot body = %q (%v), want %q", body, err, fixture.body)
	}
	if err := json.Unmarshal(snapshot["source"], &source); err != nil || source != fixture.source {
		t.Fatalf("snapshot source = %q (%v), want %q", source, err, fixture.source)
	}
	assertReaderJSONEqual(t, "snapshot target", snapshot["target"], fixture.target)
	assertReaderJSONEqual(t, "snapshot quote", snapshot["quote"], fixture.quote)
	var quote map[string]json.RawMessage
	if err := json.Unmarshal(snapshot["quote"], &quote); err != nil || len(quote["prefix"]) == 0 || len(quote["suffix"]) == 0 {
		t.Fatalf("snapshot quote must retain prefix/suffix: %s (%v)", snapshot["quote"], err)
	}
	wantHostSnapshot, err := json.Marshal(fixture.hostBody)
	if err != nil {
		t.Fatalf("encode expected host snapshot: %v", err)
	}
	assertReaderJSONEqual(t, "snapshot original host content", snapshot["original_host_snapshot"], wantHostSnapshot)
	var identity map[string]json.RawMessage
	if err := json.Unmarshal(snapshot["original_host_identity"], &identity); err != nil {
		t.Fatalf("decode snapshot original host identity: %v", err)
	}
	var identityKind, identityID string
	if err := json.Unmarshal(identity["kind"], &identityKind); err != nil || identityKind != "link" {
		t.Fatalf("snapshot identity kind = %q (%v), want link", identityKind, err)
	}
	if err := json.Unmarshal(identity["id"], &identityID); err != nil || identityID != fixture.linkID.String() {
		t.Fatalf("snapshot identity id = %q (%v), want %s", identityID, err, fixture.linkID)
	}
	if !requireLinkURL {
		var identityLinkID string
		if err := json.Unmarshal(identity["link_id"], &identityLinkID); err != nil || identityLinkID != fixture.linkID.String() {
			t.Fatalf("snapshot identity link_id = %q (%v), want %s", identityLinkID, err, fixture.linkID)
		}
	}
	if requireLinkURL {
		var url string
		var revision int64
		if err := json.Unmarshal(identity["url"], &url); err != nil || url != fixture.url {
			t.Fatalf("snapshot identity url = %q (%v), want %q", url, err, fixture.url)
		}
		if err := json.Unmarshal(identity["content_revision"], &revision); err != nil || revision != 1 {
			t.Fatalf("snapshot identity content revision = %d (%v), want 1", revision, err)
		}
	}
	for _, field := range []string{"created_at", "updated_at", "frozen_at"} {
		var at time.Time
		if err := json.Unmarshal(snapshot[field], &at); err != nil || at.IsZero() {
			t.Fatalf("snapshot %s = %s (%v), want timestamp", field, snapshot[field], err)
		}
	}
}

func TestReaderThoughtConversionReplaysImmutableSnapshotAfterPriorCursor(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	fixture := seedReaderConversionThought(t, pool, reader, ctx, "replay")

	before, beforeCursor, err := reader.ListThoughtsSince(ctx, "", 20)
	if err != nil || len(before) != 1 || before[0].ID != fixture.thoughtID || beforeCursor == "" {
		t.Fatalf("pre-conversion sync = %#v cursor=%q err=%v", before, beforeCursor, err)
	}
	result, err := repository.NewPGXLinkRepository(pool).ConvertLink(ctx, repository.ConvertLinkParams{
		LinkID: fixture.linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: 1,
	})
	if err != nil || result.Kind != model.LibraryKindSite || result.ContentRevision != 2 {
		t.Fatalf("ConvertLink() = %#v, %v", result, err)
	}

	firstReplay, nextCursor, err := reader.ListThoughtsSince(ctx, beforeCursor, 20)
	if err != nil || len(firstReplay) != 1 || nextCursor == "" || firstReplay[0].LastSequence <= fixture.initialSequence {
		t.Fatalf("post-conversion sync = %#v cursor=%q err=%v", firstReplay, nextCursor, err)
	}
	assertReaderConversionReplay(t, firstReplay[0], fixture)

	var snapshotBefore []byte
	if err := pool.QueryRow(t.Context(), `SELECT snapshot FROM reader_thought_tombstones WHERE thought_id=$1`, fixture.thoughtID).Scan(&snapshotBefore); err != nil {
		t.Fatalf("read conversion snapshot: %v", err)
	}
	assertReaderSnapshotReplayContract(t, snapshotBefore, fixture, false)

	live, _, err := reader.ListThoughts(ctx, "", "", 20)
	if err != nil || len(live) != 0 {
		t.Fatalf("live thoughts after conversion = %#v, %v; want none", live, err)
	}
	history, _, err := reader.ListThoughtHistory(ctx, "", 20)
	if err != nil || len(history) != 1 {
		t.Fatalf("history after conversion = %#v, %v; want one", history, err)
	}
	assertReaderConversionReplay(t, history[0], fixture)
	search, total, _, err := reader.SearchThoughts(ctx, fixture.body, "", 20)
	if err != nil || total != 1 || len(search) != 1 || search[0].ID != fixture.thoughtID || search[0].LifecycleStatus != "tombstone" {
		t.Fatalf("search after conversion = %#v total=%d err=%v", search, total, err)
	}
	var archived bool
	if err := reader.StreamReaderArchiveSection(ctx, "thought_tombstones", func(value []byte) error {
		var item struct {
			ThoughtID string          `json:"thought_id"`
			Snapshot  json.RawMessage `json:"snapshot"`
		}
		if err := json.Unmarshal(value, &item); err != nil {
			return err
		}
		if item.ThoughtID == fixture.thoughtID {
			archived = true
			assertReaderSnapshotReplayContract(t, item.Snapshot, fixture, false)
		}
		return nil
	}); err != nil {
		t.Fatalf("export conversion tombstone: %v", err)
	}
	if !archived {
		t.Fatal("archive omitted conversion tombstone")
	}

	if _, err := pool.Exec(t.Context(), `
		UPDATE reader_thoughts
		SET host_kind='note',host_id='mutated-host',link_id=NULL,
			body='mutable body must not win',source='mutable-source-must-not-win',
			target=$2::jsonb,quote=$3::jsonb
		WHERE id=$1`, fixture.thoughtID,
		readerVNextJSON(t, map[string]any{"kind": "note", "host_id": "mutated-host"}),
		readerVNextJSON(t, map[string]any{"exact": "mutable quote"})); err != nil {
		t.Fatalf("mutate projection after conversion: %v", err)
	}
	var snapshotAfter []byte
	if err := pool.QueryRow(t.Context(), `SELECT snapshot FROM reader_thought_tombstones WHERE thought_id=$1`, fixture.thoughtID).Scan(&snapshotAfter); err != nil {
		t.Fatalf("read conversion snapshot after projection mutation: %v", err)
	}
	if !bytes.Equal(snapshotBefore, snapshotAfter) {
		t.Fatalf("conversion snapshot changed after mutable projection mutation:\n before=%s\n after=%s", snapshotBefore, snapshotAfter)
	}
	secondReplay, _, err := reader.ListThoughtsSince(ctx, beforeCursor, 20)
	if err != nil || len(secondReplay) != 1 {
		t.Fatalf("replay after projection mutation = %#v, %v", secondReplay, err)
	}
	assertReaderConversionReplay(t, secondReplay[0], fixture)
	var lifecycleOps int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE annotation_id=$1 AND device_id='reader-lifecycle'`, fixture.thoughtID).Scan(&lifecycleOps); err != nil {
		t.Fatalf("count conversion lifecycle operations: %v", err)
	}
	if lifecycleOps != 1 {
		t.Fatalf("conversion lifecycle operations = %d, want 1", lifecycleOps)
	}
}

func TestReaderThoughtConversionRollsBackLifecycleSnapshotAndOperation(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	fixture := seedReaderConversionThought(t, pool, reader, ctx, "rollback")

	if _, err := pool.Exec(t.Context(), `
		CREATE OR REPLACE FUNCTION reader_test_reject_conversion_update() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced conversion rollback'; END $$`); err != nil {
		t.Fatalf("create conversion rollback function: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `DROP TRIGGER IF EXISTS reader_test_reject_conversion_update ON links`); err != nil {
		t.Fatalf("drop prior conversion rollback trigger: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `CREATE TRIGGER reader_test_reject_conversion_update BEFORE UPDATE ON links FOR EACH ROW EXECUTE FUNCTION reader_test_reject_conversion_update()`); err != nil {
		t.Fatalf("install conversion rollback trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reader_test_reject_conversion_update ON links`); err != nil {
			t.Errorf("drop conversion rollback trigger: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS reader_test_reject_conversion_update()`); err != nil {
			t.Errorf("drop conversion rollback function: %v", err)
		}
	})

	_, err := repository.NewPGXLinkRepository(pool).ConvertLink(ctx, repository.ConvertLinkParams{
		LinkID: fixture.linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "forced conversion rollback") {
		t.Fatalf("ConvertLink() error = %v, want forced conversion rollback", err)
	}
	var tombstones, lifecycleOps int
	var sequence int64
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_tombstones WHERE thought_id=$1`, fixture.thoughtID).Scan(&tombstones); err != nil {
		t.Fatalf("count rolled-back conversion tombstones: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE annotation_id=$1 AND device_id='reader-lifecycle'`, fixture.thoughtID).Scan(&lifecycleOps); err != nil {
		t.Fatalf("count rolled-back conversion lifecycle operations: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT last_sequence FROM reader_thoughts WHERE id=$1`, fixture.thoughtID).Scan(&sequence); err != nil {
		t.Fatalf("read rolled-back thought sequence: %v", err)
	}
	if tombstones != 0 || lifecycleOps != 0 || sequence != fixture.initialSequence {
		t.Fatalf("rolled-back conversion tombstones/ops/sequence = %d/%d/%d, want 0/0/%d", tombstones, lifecycleOps, sequence, fixture.initialSequence)
	}
}

func TestReaderThoughtConcurrentConversionWritesExactlyOneLifecycleSnapshot(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	fixture := seedReaderConversionThought(t, pool, reader, ctx, "concurrent")

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			<-start
			_, err := repository.NewPGXLinkRepository(pool).ConvertLink(ctx, repository.ConvertLinkParams{
				LinkID: fixture.linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: 1,
			})
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errs)

	succeeded := 0
	for err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, repository.ErrRevisionConflict) {
			t.Fatalf("concurrent ConvertLink() error = %v, want ErrRevisionConflict", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent conversions succeeded = %d, want 1", succeeded)
	}
	var tombstones, lifecycleOps int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_tombstones WHERE thought_id=$1 AND reason='link_converted_to_site'`, fixture.thoughtID).Scan(&tombstones); err != nil {
		t.Fatalf("count concurrent conversion tombstones: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE annotation_id=$1 AND device_id='reader-lifecycle'`, fixture.thoughtID).Scan(&lifecycleOps); err != nil {
		t.Fatalf("count concurrent conversion lifecycle operations: %v", err)
	}
	if tombstones != 1 || lifecycleOps != 1 {
		t.Fatalf("concurrent conversion tombstones/ops = %d/%d, want 1/1", tombstones, lifecycleOps)
	}
}

func TestReaderThoughtConversionLateOperationsKeepFrozenSnapshotByteEquivalent(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	fixture := seedReaderConversionThought(t, pool, reader, ctx, "late-ops")
	_, beforeCursor, err := reader.ListThoughtsSince(ctx, "", 20)
	if err != nil || beforeCursor == "" {
		t.Fatalf("pre-conversion cursor = %q, %v", beforeCursor, err)
	}
	if _, err := repository.NewPGXLinkRepository(pool).ConvertLink(ctx, repository.ConvertLinkParams{
		LinkID: fixture.linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: 1,
	}); err != nil {
		t.Fatalf("convert before late operations: %v", err)
	}
	converted, _, err := reader.ListThoughtsSince(ctx, beforeCursor, 20)
	if err != nil || len(converted) != 1 {
		t.Fatalf("conversion replay = %#v, %v", converted, err)
	}
	var snapshotBefore []byte
	if err := pool.QueryRow(t.Context(), `SELECT snapshot FROM reader_thought_tombstones WHERE thought_id=$1`, fixture.thoughtID).Scan(&snapshotBefore); err != nil {
		t.Fatalf("read conversion snapshot before late operations: %v", err)
	}

	lateUpdate := model.ReaderThoughtOp{
		OpID:          "conversion-late-update-" + uuid.NewString(),
		DeviceID:      "late-operation-test",
		LogicalClock:  converted[0].WinnerKey.LogicalClock + 1,
		OperationKind: "update",
		AnnotationID:  fixture.thoughtID,
		HostKind:      "link",
		HostID:        fixture.linkID.String(),
		Target:        fixture.target,
		Payload: readerVNextJSON(t, map[string]any{
			"body": "late mutable update must not replace snapshot", "source": "late-operation-source",
			"link_id": fixture.linkID.String(), "quote": fixture.quote,
		}),
	}
	updateAcks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{lateUpdate})
	if err != nil || len(updateAcks) != 1 || updateAcks[0].Disposition != "applied" || updateAcks[0].Sequence <= converted[0].LastSequence {
		t.Fatalf("late update acks = %#v, %v", updateAcks, err)
	}
	duplicateAcks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{lateUpdate})
	if err != nil || len(duplicateAcks) != 1 || duplicateAcks[0].Disposition != "duplicate" || duplicateAcks[0].Sequence != updateAcks[0].Sequence {
		t.Fatalf("duplicate late update acks = %#v, %v", duplicateAcks, err)
	}
	lateDelete := model.ReaderThoughtOp{
		OpID:          "conversion-late-delete-" + uuid.NewString(),
		DeviceID:      "late-operation-test",
		LogicalClock:  converted[0].WinnerKey.LogicalClock - 1,
		OperationKind: "delete",
		AnnotationID:  fixture.thoughtID,
		HostKind:      "link",
		HostID:        fixture.linkID.String(),
		Target:        fixture.target,
		Payload: readerVNextJSON(t, map[string]any{
			"body": "late delete payload", "source": "late-delete-source", "link_id": fixture.linkID.String(), "quote": fixture.quote,
		}),
	}
	deleteAcks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{lateDelete})
	if err != nil || len(deleteAcks) != 1 || deleteAcks[0].Disposition != "superseded" || deleteAcks[0].Sequence <= updateAcks[0].Sequence {
		t.Fatalf("late delete acks = %#v, %v", deleteAcks, err)
	}

	var snapshotAfter []byte
	if err := pool.QueryRow(t.Context(), `SELECT snapshot FROM reader_thought_tombstones WHERE thought_id=$1`, fixture.thoughtID).Scan(&snapshotAfter); err != nil {
		t.Fatalf("read conversion snapshot after late operations: %v", err)
	}
	if !bytes.Equal(snapshotBefore, snapshotAfter) {
		t.Fatalf("late operations rewrote immutable snapshot:\n before=%s\n after=%s", snapshotBefore, snapshotAfter)
	}
	replayed, _, err := reader.ListThoughtsSince(ctx, beforeCursor, 20)
	if err != nil || len(replayed) != 1 || replayed[0].LastSequence != updateAcks[0].Sequence {
		t.Fatalf("replay after late operations = %#v, %v", replayed, err)
	}
	assertReaderConversionReplay(t, replayed[0], fixture)
	var operations int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE annotation_id=$1`, fixture.thoughtID).Scan(&operations); err != nil {
		t.Fatalf("count late operation log rows: %v", err)
	}
	if operations != 4 {
		t.Fatalf("operation rows after update/duplicate/delete = %d, want 4", operations)
	}
}

func TestReaderThoughtConversionUserDeleteScrubsReplayAndVisibility(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	fixture := seedReaderConversionThought(t, pool, reader, ctx, "user-delete")
	_, beforeCursor, err := reader.ListThoughtsSince(ctx, "", 20)
	if err != nil || beforeCursor == "" {
		t.Fatalf("pre-delete sync cursor = %q, err=%v", beforeCursor, err)
	}
	if _, err := repository.NewPGXLinkRepository(pool).ConvertLink(ctx, repository.ConvertLinkParams{
		LinkID: fixture.linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: 1,
	}); err != nil {
		t.Fatalf("convert link before user delete: %v", err)
	}
	converted, _, err := reader.ListThoughtsSince(ctx, beforeCursor, 20)
	if err != nil || len(converted) != 1 {
		t.Fatalf("conversion replay before user delete = %#v, %v", converted, err)
	}
	deleteSecret := "private user delete payload must never replay"
	acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID:          "conversion-user-delete-" + uuid.NewString(),
		DeviceID:      "conversion-user-delete",
		LogicalClock:  converted[0].WinnerKey.LogicalClock + 1,
		OperationKind: "delete",
		AnnotationID:  fixture.thoughtID,
		HostKind:      "link",
		HostID:        fixture.linkID.String(),
		Target:        fixture.target,
		Payload: readerVNextJSON(t, map[string]any{
			"body":    deleteSecret,
			"source":  deleteSecret,
			"link_id": fixture.linkID.String(),
			"quote":   fixture.quote,
		}),
	}})
	if err != nil || len(acks) != 1 || acks[0].Disposition != "applied" {
		t.Fatalf("user delete after conversion = %#v, %v", acks, err)
	}

	synced, _, err := reader.ListThoughtsSince(ctx, beforeCursor, 20)
	if err != nil || len(synced) != 1 {
		t.Fatalf("user-deleted conversion sync = %#v, %v", synced, err)
	}
	item := synced[0]
	if !item.Deleted || item.LifecycleReason == nil || *item.LifecycleReason != "user_deleted" ||
		item.Body != "" || item.Source != "" || item.LinkID != nil || len(item.Quote) != 0 ||
		len(item.OriginalHostSnapshot) != 0 || !bytes.Equal(item.Target, []byte(`{}`)) ||
		strings.Contains(string(readerVNextJSON(t, item)), deleteSecret) || strings.Contains(string(readerVNextJSON(t, item)), fixture.body) {
		t.Fatalf("user-deleted conversion replay leaked content: %+v", item)
	}
	if got, _, err := reader.ListThoughtHistory(ctx, "", 20); err != nil || len(got) != 0 {
		t.Fatalf("history after user delete = %#v, %v; want empty", got, err)
	}
	if got, total, _, err := reader.SearchThoughts(ctx, fixture.body, "", 20); err != nil || total != 0 || len(got) != 0 {
		t.Fatalf("search after user delete = %#v total=%d err=%v; want empty", got, total, err)
	}
	for _, section := range []string{"thought_tombstones", "thought_ops"} {
		count := 0
		if err := reader.StreamReaderArchiveSection(ctx, section, func([]byte) error {
			count++
			return nil
		}); err != nil {
			t.Fatalf("archive %s after user delete: %v", section, err)
		}
		if count != 0 {
			t.Fatalf("archive %s emitted %d rows after user delete, want none", section, count)
		}
	}
	var snapshot []byte
	if err := pool.QueryRow(t.Context(), `SELECT snapshot FROM reader_thought_tombstones WHERE thought_id=$1`, fixture.thoughtID).Scan(&snapshot); err != nil {
		t.Fatalf("read user-deleted conversion marker: %v", err)
	}
	if strings.Contains(string(snapshot), fixture.body) || strings.Contains(string(snapshot), deleteSecret) || strings.Contains(string(snapshot), fixture.hostBody) {
		t.Fatalf("user-delete marker leaked frozen or mutable content: %s", snapshot)
	}
}
