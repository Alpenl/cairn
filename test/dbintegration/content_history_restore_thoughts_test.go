package dbintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestContentHistoryRestoreReanchorsOnlyCurrentSavedContentThoughts(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool, "https://reader-vnext.example/content-history-restore", "Restore", "before restore", "summary")
	if _, err := pool.Exec(t.Context(), `
		UPDATE links
		SET content='before restore',content_document='before restore',content_revision=4
		WHERE id=$1`, linkID); err != nil {
		t.Fatalf("set current content revision: %v", err)
	}
	var historyID int64
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO reader_content_history (link_id,revision,content,content_document,content_format,content_source)
		VALUES ($1,2,'unique phrase same phrase same phrase','unique phrase same phrase same phrase','plain','user')
		RETURNING id`, linkID).Scan(&historyID); err != nil {
		t.Fatalf("seed restore history: %v", err)
	}

	target := func(kind string, revision any) json.RawMessage {
		version := map[string]any{}
		if revision != nil {
			version["content_revision"] = revision
		}
		return readerVNextJSON(t, map[string]any{
			"kind": kind, "host_id": linkID.String(), "version": version,
		})
	}
	payload := func(body, exact string) json.RawMessage {
		return readerVNextJSON(t, map[string]any{
			"body": body, "source": "user", "link_id": linkID.String(),
			"quote": map[string]any{"exact": exact, "prefix": "", "suffix": ""},
		})
	}
	type thoughtFixture struct {
		id     string
		target json.RawMessage
		body   string
		exact  string
		clock  int64
	}
	fixtures := []thoughtFixture{
		{id: "restore-eligible-" + uuid.NewString(), target: target("saved-content", 4), body: "eligible", exact: "unique phrase", clock: 17},
		{id: "restore-ambiguous-" + uuid.NewString(), target: target("saved-content", 4), body: "ambiguous", exact: "same phrase", clock: 19},
		{id: "restore-summary-" + uuid.NewString(), target: target("summary", 4), body: "summary", exact: "unique phrase", clock: 23},
		{id: "restore-legacy-" + uuid.NewString(), target: target("saved-content", nil), body: "legacy", exact: "unique phrase", clock: 29},
		{id: "restore-old-" + uuid.NewString(), target: target("saved-content", 3), body: "old", exact: "unique phrase", clock: 31},
		{id: "restore-history-" + uuid.NewString(), target: target("saved-content", 4), body: "already historical", exact: "unique phrase", clock: 37},
	}
	ops := make([]model.ReaderThoughtOp, 0, len(fixtures))
	for _, fixture := range fixtures {
		ops = append(ops, model.ReaderThoughtOp{
			OpID:          "seed-" + fixture.id,
			DeviceID:      "restore-seed",
			LogicalClock:  fixture.clock,
			OperationKind: "add",
			AnnotationID:  fixture.id,
			HostKind:      "link",
			HostID:        linkID.String(),
			Target:        fixture.target,
			Payload:       payload(fixture.body, fixture.exact),
		})
	}
	if _, err := reader.AppendThoughtOps(ctx, ops); err != nil {
		t.Fatalf("seed thought operations: %v", err)
	}

	alreadyHistorical := fixtures[5]
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
		SELECT id,host_kind,host_id,'preexisting_history',
			jsonb_build_object(
				'snapshot_version',1,
				'id',id,
				'host_kind',host_kind,
				'host_id',host_id,
				'link_id',link_id,
				'type','thought',
				'body',body,
				'target',target,
				'quote',quote,
				'source',source,
				'created_at',created_at,
				'updated_at',updated_at,
				'original_host_snapshot','{}'::jsonb,
				'original_host_identity',jsonb_build_object('kind',host_kind,'id',host_id,'link_id',link_id),
				'frozen_at',CURRENT_TIMESTAMP)
		FROM reader_thoughts
		WHERE id=$1`, alreadyHistorical.id); err != nil {
		t.Fatalf("seed existing tombstone: %v", err)
	}

	before := make(map[string]restoreThoughtState, len(fixtures))
	for _, fixture := range fixtures {
		before[fixture.id] = readRestoreThoughtState(t, pool, fixture.id)
	}

	restoredRevision, err := reader.RestoreContentHistory(ctx, linkID, historyID, 4)
	if err != nil || restoredRevision != 5 {
		t.Fatalf("RestoreContentHistory = revision %d err %v, want 5/nil", restoredRevision, err)
	}

	eligible := readRestoreThoughtState(t, pool, fixtures[0].id)
	if eligible.winnerClock != fixtures[0].clock+1 || eligible.tombstoneReason != "" {
		t.Fatalf("eligible thought lifecycle = %#v, want strictly newer live winner", eligible)
	}
	var eligibleTarget struct {
		Version struct {
			ContentRevision int64 `json:"content_revision"`
		} `json:"version"`
	}
	if err := json.Unmarshal(eligible.target, &eligibleTarget); err != nil || eligibleTarget.Version.ContentRevision != 5 {
		t.Fatalf("eligible target = %s err %v, want revision 5", eligible.target, err)
	}

	ambiguous := readRestoreThoughtState(t, pool, fixtures[1].id)
	if ambiguous.tombstoneReason != "content_restored" || ambiguous.winnerClock != fixtures[1].clock+1 {
		t.Fatalf("ambiguous thought lifecycle = %#v, want content_restored with newer lifecycle winner", ambiguous)
	}
	if !bytes.Equal(ambiguous.target, before[fixtures[1].id].target) ||
		!bytes.Equal(ambiguous.quote, before[fixtures[1].id].quote) ||
		ambiguous.body != before[fixtures[1].id].body {
		t.Fatalf("ambiguous thought changed durable snapshot = %#v", ambiguous)
	}

	for _, fixture := range fixtures[2:] {
		after := readRestoreThoughtState(t, pool, fixture.id)
		if !restoreThoughtStateEqual(before[fixture.id], after) {
			t.Fatalf("ineligible %q changed: before=%#v after=%#v", fixture.id, before[fixture.id], after)
		}
	}

	live, _, err := reader.ListThoughts(ctx, "", "", 20)
	if err != nil {
		t.Fatalf("ListThoughts after restore: %v", err)
	}
	if containsRestoreThought(live, fixtures[1].id) || containsRestoreThought(live, alreadyHistorical.id) || !containsRestoreThought(live, fixtures[0].id) {
		t.Fatalf("live thoughts after restore = %#v, want only the eligible thought among changed candidates", live)
	}
	history, _, err := reader.ListThoughtHistory(ctx, "", 20)
	if err != nil {
		t.Fatalf("ListThoughtHistory after restore: %v", err)
	}
	if !containsRestoreThought(history, fixtures[1].id) || !containsRestoreThought(history, alreadyHistorical.id) {
		t.Fatalf("history thoughts after restore = %#v, want ambiguous and preexisting history", history)
	}

	_, err = reader.RestoreContentHistory(ctx, linkID, historyID, 4)
	if !errors.Is(err, repository.ErrRevisionConflict) {
		t.Fatalf("duplicate RestoreContentHistory error = %v, want ErrRevisionConflict", err)
	}
	var reanchorOps int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM reader_thought_ops
		WHERE op_id=$1`, "content-restore-"+linkID.String()+"-5-"+fixtures[0].id).Scan(&reanchorOps); err != nil {
		t.Fatalf("count derived restore operation: %v", err)
	}
	if reanchorOps != 1 {
		t.Fatalf("derived restore operation count = %d, want one idempotent operation", reanchorOps)
	}
}

type restoreThoughtState struct {
	target          []byte
	quote           []byte
	body            string
	winnerClock     int64
	tombstoneReason string
}

func readRestoreThoughtState(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, thoughtID string) restoreThoughtState {
	t.Helper()
	var state restoreThoughtState
	if err := pool.QueryRow(t.Context(), `
		SELECT thought.target,thought.quote,thought.body,thought.winner_logical_clock,COALESCE(tombstone.reason,'')
		FROM reader_thoughts thought
		LEFT JOIN reader_thought_tombstones tombstone
		  ON tombstone.thought_id=thought.id
		WHERE thought.id=$1`, thoughtID).Scan(
		&state.target, &state.quote, &state.body, &state.winnerClock, &state.tombstoneReason,
	); err != nil {
		t.Fatalf("read thought %s: %v", thoughtID, err)
	}
	return state
}

func restoreThoughtStateEqual(left, right restoreThoughtState) bool {
	return left.body == right.body &&
		left.winnerClock == right.winnerClock &&
		left.tombstoneReason == right.tombstoneReason &&
		bytes.Equal(left.target, right.target) &&
		bytes.Equal(left.quote, right.quote)
}

func containsRestoreThought(items []model.ReaderThought, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}
