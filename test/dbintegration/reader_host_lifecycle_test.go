package dbintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

type readerLifecycleHostFixture struct {
	kind     model.ReaderHostKind
	id       uuid.UUID
	body     string
	revision int64
}

type readerLifecycleThoughtFixture struct {
	id      string
	target  json.RawMessage
	payload json.RawMessage
}

func TestReaderHostLifecyclePostgresStateMatrix(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	type expectation struct {
		err     error
		changed *bool
	}
	boolPointer := func(value bool) *bool { return &value }
	states := []string{"live", "trashed", "missing", "purged"}
	commands := []string{"delete", "restore", "purge"}
	for _, kind := range []model.ReaderHostKind{model.ReaderHostLink, model.ReaderHostInbox, model.ReaderHostNote} {
		for _, command := range commands {
			commandStates := append([]string(nil), states...)
			if command == "purge" {
				commandStates = append(commandStates, "purged-replay")
			}
			for _, state := range commandStates {
				name := strings.Join([]string{string(kind), command, state}, "/")
				t.Run(name, func(t *testing.T) {
					host := readerLifecycleHostFixture{kind: kind, id: uuid.New()}
					if state != "missing" {
						host = seedReaderLifecycleHost(t, pool, kind, name)
					}
					firstOperation := uuid.New()
					if state == "trashed" || state == "purged" || state == "purged-replay" {
						if _, err := repo.SoftDeleteHost(ctx, kind, host.id); err != nil {
							t.Fatalf("prepare trashed host: %v", err)
						}
					}
					if state == "purged" || state == "purged-replay" {
						if err := repo.PurgeHost(ctx, kind, host.id, firstOperation); err != nil {
							t.Fatalf("prepare purged host: %v", err)
						}
					}

					want := expectation{}
					switch command + "/" + state {
					case "delete/live":
						want.changed = boolPointer(true)
					case "delete/trashed":
						want.changed = boolPointer(false)
					case "restore/live":
						want.changed = boolPointer(false)
					case "restore/trashed":
						want.changed = boolPointer(true)
					case "purge/live":
						want.err = repository.ErrReaderHostNotTrashed
					case "purge/trashed", "purge/purged-replay":
					default:
						want.err = repository.ErrNotFound
					}

					var (
						changed bool
						err     error
					)
					switch command {
					case "delete":
						var result model.ReaderHostLifecycleResult
						result, err = repo.SoftDeleteHost(ctx, kind, host.id)
						changed = result.Changed
					case "restore":
						var result model.ReaderHostLifecycleResult
						result, err = repo.RestoreHost(ctx, kind, host.id)
						changed = result.Changed
					case "purge":
						operationID := uuid.New()
						if state == "purged-replay" {
							operationID = firstOperation
						}
						err = repo.PurgeHost(ctx, kind, host.id, operationID)
					default:
						t.Fatalf("unknown command %q", command)
					}
					if want.err == nil && err != nil {
						t.Fatalf("command error = %v, want success", err)
					}
					if want.err != nil && !errors.Is(err, want.err) {
						t.Fatalf("command error = %v, want %v", err, want.err)
					}
					if want.changed != nil && changed != *want.changed {
						t.Fatalf("changed = %v, want %v", changed, *want.changed)
					}

					exists, trashed := readerLifecycleHostState(t, pool, kind, host.id)
					switch {
					case state == "missing" || state == "purged" || state == "purged-replay":
						if exists {
							t.Fatal("missing/purged host was unexpectedly recreated")
						}
					case command == "purge" && state == "trashed":
						if exists {
							t.Fatal("successful purge left the host row behind")
						}
					case command == "delete":
						if !exists || !trashed {
							t.Fatalf("delete state = exists:%v trashed:%v, want existing trash row", exists, trashed)
						}
					case command == "restore":
						if !exists || trashed {
							t.Fatalf("restore state = exists:%v trashed:%v, want live row", exists, trashed)
						}
					case command == "purge" && state == "live":
						if !exists || trashed {
							t.Fatalf("rejected live purge mutated host: exists:%v trashed:%v", exists, trashed)
						}
					}
				})
			}
		}
	}
}

func TestReaderHostLifecyclePostgresLinkTrashTriggerFreezesCompleteReplaySnapshot(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	host := seedReaderLifecycleHost(t, pool, model.ReaderHostLink, "trigger-snapshot")
	var url string
	if err := pool.QueryRow(t.Context(), `SELECT url FROM links WHERE id=$1`, host.id).Scan(&url); err != nil {
		t.Fatalf("read trigger link URL: %v", err)
	}
	fixture := readerConversionThoughtFixture{
		linkID:    host.id,
		thoughtID: "trigger-snapshot-thought-" + uuid.NewString(),
		target: readerVNextJSON(t, map[string]any{
			"kind":    "saved-content",
			"host_id": host.id.String(),
			"version": map[string]any{"content_revision": host.revision},
		}),
		quote: readerVNextJSON(t, map[string]any{
			"exact": "anchor", "prefix": "prefix ", "suffix": " suffix", "start": 7, "end": 13,
		}),
		body:     "trigger frozen thought body",
		source:   "trigger-source",
		hostBody: host.body,
		url:      url,
	}
	acks, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID:          "trigger-snapshot-add-" + uuid.NewString(),
		DeviceID:      "trigger-snapshot-test",
		LogicalClock:  3,
		OperationKind: "add",
		AnnotationID:  fixture.thoughtID,
		HostKind:      "link",
		HostID:        host.id.String(),
		Target:        fixture.target,
		Payload: readerVNextJSON(t, map[string]any{
			"body": fixture.body, "source": fixture.source, "link_id": host.id.String(), "quote": fixture.quote,
		}),
	}})
	if err != nil || len(acks) != 1 {
		t.Fatalf("seed link trigger thought = %#v, %v", acks, err)
	}

	result, err := reader.SoftDeleteHost(ctx, model.ReaderHostLink, host.id)
	if err != nil || !result.Changed {
		t.Fatalf("SoftDeleteHost(link) = %+v, %v", result, err)
	}
	var snapshotBefore []byte
	var reason string
	if err := pool.QueryRow(t.Context(), `SELECT snapshot,reason FROM reader_thought_tombstones WHERE thought_id=$1`, fixture.thoughtID).Scan(&snapshotBefore, &reason); err != nil {
		t.Fatalf("read link trash trigger snapshot: %v", err)
	}
	if reason != "link_deleted" {
		t.Fatalf("trigger tombstone reason = %q, want link_deleted", reason)
	}
	assertReaderSnapshotReplayContract(t, snapshotBefore, fixture, true)

	if _, err := pool.Exec(t.Context(), `
		UPDATE links
		SET content_document='mutable host body must not win',content='mutable host body must not win',url='https://mutated.example/after-trash'
		WHERE id=$1`, host.id); err != nil {
		t.Fatalf("mutate trashed link after trigger: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE reader_thoughts
		SET host_kind='note',host_id='mutated-host',link_id=NULL,body='mutable thought body must not win',source='mutable-source',
			target=$2::jsonb,quote=$3::jsonb
		WHERE id=$1`, fixture.thoughtID,
		readerVNextJSON(t, map[string]any{"kind": "note", "host_id": "mutated-host"}),
		readerVNextJSON(t, map[string]any{"exact": "mutable quote"})); err != nil {
		t.Fatalf("mutate trigger thought projection: %v", err)
	}
	var snapshotAfter []byte
	if err := pool.QueryRow(t.Context(), `SELECT snapshot FROM reader_thought_tombstones WHERE thought_id=$1`, fixture.thoughtID).Scan(&snapshotAfter); err != nil {
		t.Fatalf("read trigger snapshot after mutation: %v", err)
	}
	if !bytes.Equal(snapshotBefore, snapshotAfter) {
		t.Fatalf("trigger snapshot changed after mutable mutation:\n before=%s\n after=%s", snapshotBefore, snapshotAfter)
	}

	replay, _, err := reader.ListThoughtsSince(ctx, "", 20)
	if err != nil || len(replay) != 1 {
		t.Fatalf("trigger tombstone replay = %#v, %v", replay, err)
	}
	item := replay[0]
	if item.ID != fixture.thoughtID || item.HostKind != "link" || item.HostID != host.id.String() || item.LinkID == nil || *item.LinkID != host.id ||
		item.Body != fixture.body || item.Source != fixture.source || item.LifecycleReason == nil || *item.LifecycleReason != "link_deleted" || item.TombstonedAt == nil {
		t.Fatalf("trigger replay did not use frozen authority: %+v", item)
	}
	assertReaderJSONEqual(t, "trigger replay target", item.Target, fixture.target)
	assertReaderJSONEqual(t, "trigger replay quote", item.Quote, fixture.quote)
	wantHostSnapshot, err := json.Marshal(fixture.hostBody)
	if err != nil {
		t.Fatalf("encode expected trigger host snapshot: %v", err)
	}
	assertReaderJSONEqual(t, "trigger replay host snapshot", item.OriginalHostSnapshot, wantHostSnapshot)
}

func TestReaderHostLifecyclePostgresConcurrentCommandsAndThoughtWrites(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	for _, kind := range []model.ReaderHostKind{model.ReaderHostLink, model.ReaderHostInbox, model.ReaderHostNote} {
		t.Run(string(kind), func(t *testing.T) {
			host := seedReaderLifecycleHost(t, pool, kind, "concurrent-"+string(kind))
			seedReaderLifecycleThought(t, repo, ctx, host, "initial", "anchor")

			const writers = 6
			start := make(chan struct{})
			errorsByWorker := make(chan error, writers+1)
			var wait sync.WaitGroup
			wait.Add(writers + 1)
			go func() {
				defer wait.Done()
				<-start
				_, err := repo.SoftDeleteHost(ctx, kind, host.id)
				errorsByWorker <- err
			}()
			for index := range writers {
				go func() {
					defer wait.Done()
					<-start
					fixture := newReaderLifecycleThoughtFixture(t, host, fmt.Sprintf("racer-%d", index), "anchor")
					_, err := repo.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
						OpID:          "add-" + uuid.NewString(),
						DeviceID:      "concurrency-test",
						LogicalClock:  1,
						OperationKind: "add",
						AnnotationID:  fixture.id,
						HostKind:      string(host.kind),
						HostID:        host.id.String(),
						Target:        fixture.target,
						Payload:       fixture.payload,
					}})
					errorsByWorker <- err
				}()
			}
			close(start)
			wait.Wait()
			close(errorsByWorker)
			for err := range errorsByWorker {
				if err != nil && !errors.Is(err, repository.ErrInvalidReaderThought) && !errors.Is(err, repository.ErrReaderThoughtLinkMismatch) {
					t.Fatalf("concurrent delete/thought error = %v", err)
				}
			}

			var leaked int
			if err := pool.QueryRow(t.Context(), `
				SELECT count(*)
				FROM reader_thoughts thought
				WHERE thought.host_kind=$1 AND thought.host_id=$2
				  AND thought.deleted=false
				  AND NOT EXISTS (
					SELECT 1 FROM reader_thought_tombstones tombstone
					WHERE tombstone.thought_id=thought.id
				  )`, kind, host.id.String()).Scan(&leaked); err != nil {
				t.Fatalf("count live thoughts after concurrent delete: %v", err)
			}
			if leaked != 0 {
				t.Fatalf("live thoughts after concurrent delete = %d, want zero", leaked)
			}

			restoreErrors := runReaderLifecycleConcurrent(8, func() error {
				_, err := repo.RestoreHost(ctx, kind, host.id)
				return err
			})
			assertReaderLifecycleErrors(t, restoreErrors)
			var restoreOps int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE device_id='reader-lifecycle' AND host_kind=$1 AND host_id=$2`, kind, host.id.String()).Scan(&restoreOps); err != nil {
				t.Fatalf("count restore operations: %v", err)
			}
			if restoreOps == 0 {
				t.Fatal("concurrent restore did not re-anchor the initial unique thought")
			}
			if _, err := repo.RestoreHost(ctx, kind, host.id); err != nil {
				t.Fatalf("restore no-op: %v", err)
			}
			var restoreOpsAfterNoop int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE device_id='reader-lifecycle' AND host_kind=$1 AND host_id=$2`, kind, host.id.String()).Scan(&restoreOpsAfterNoop); err != nil {
				t.Fatalf("count restore operations after no-op: %v", err)
			}
			if restoreOpsAfterNoop != restoreOps {
				t.Fatalf("restore no-op operations = %d, want %d", restoreOpsAfterNoop, restoreOps)
			}

			deleteErrors := runReaderLifecycleConcurrent(8, func() error {
				_, err := repo.SoftDeleteHost(ctx, kind, host.id)
				return err
			})
			assertReaderLifecycleErrors(t, deleteErrors)
			var thoughts, tombstones int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thoughts WHERE host_kind=$1 AND host_id=$2 AND deleted=false`, kind, host.id.String()).Scan(&thoughts); err != nil {
				t.Fatalf("count thoughts: %v", err)
			}
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_tombstones WHERE host_kind=$1 AND host_id=$2`, kind, host.id.String()).Scan(&tombstones); err != nil {
				t.Fatalf("count tombstones: %v", err)
			}
			if tombstones != thoughts {
				t.Fatalf("tombstones = %d, live thought rows = %d; want exactly one per thought", tombstones, thoughts)
			}

			operationID := uuid.New()
			purgeErrors := runReaderLifecycleConcurrent(8, func() error {
				return repo.PurgeHost(ctx, kind, host.id, operationID)
			})
			assertReaderLifecycleErrors(t, purgeErrors)
			var receipts, remainingThoughts int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_host_purge_receipts WHERE host_kind=$1 AND host_id=$2`, kind, host.id).Scan(&receipts); err != nil {
				t.Fatalf("count purge receipts: %v", err)
			}
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thoughts WHERE host_kind=$1 AND host_id=$2`, kind, host.id.String()).Scan(&remainingThoughts); err != nil {
				t.Fatalf("count thoughts after purge: %v", err)
			}
			if receipts != 1 || remainingThoughts != thoughts {
				t.Fatalf("after concurrent purge receipts/thoughts = %d/%d, want 1/%d", receipts, remainingThoughts, thoughts)
			}
		})
	}
}

func TestReaderHostLifecyclePostgresReanchorHistoryAndExplicitDelete(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()
	host := seedReaderLifecycleHost(t, pool, model.ReaderHostLink, "reanchor-matrix")

	unique := seedReaderLifecycleThought(t, repo, ctx, host, "unique", "unique phrase")
	ambiguous := seedReaderLifecycleThought(t, repo, ctx, host, "ambiguous", "repeated")
	missing := seedReaderLifecycleThought(t, repo, ctx, host, "missing", "not present")
	explicit := seedReaderLifecycleThought(t, repo, ctx, host, "explicit-delete", "anchor")
	if result, err := repo.SoftDeleteHost(ctx, host.kind, host.id); err != nil || !result.Changed {
		t.Fatalf("initial soft delete = %+v, %v", result, err)
	}
	if result, err := repo.SoftDeleteHost(ctx, host.kind, host.id); err != nil || result.Changed {
		t.Fatalf("soft delete no-op = %+v, %v", result, err)
	}
	var tombstones int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_tombstones WHERE host_kind=$1 AND host_id=$2`, host.kind, host.id.String()).Scan(&tombstones); err != nil {
		t.Fatalf("count initial tombstones: %v", err)
	}
	if tombstones != 4 {
		t.Fatalf("duplicate soft delete tombstones = %d, want 4", tombstones)
	}

	if _, err := repo.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID:          "delete-" + uuid.NewString(),
		DeviceID:      "explicit-delete-test",
		LogicalClock:  100,
		OperationKind: "delete",
		AnnotationID:  explicit.id,
		HostKind:      string(host.kind),
		HostID:        host.id.String(),
		Target:        explicit.target,
		Payload:       explicit.payload,
	}}); err != nil {
		t.Fatalf("explicitly delete thought while link is trashed: %v", err)
	}
	if result, err := repo.RestoreHost(ctx, host.kind, host.id); err != nil || !result.Changed {
		t.Fatalf("restore host = %+v, %v", result, err)
	}

	assertReaderThoughtLifecycle(t, repo, ctx, unique.id, false, "active")
	assertReaderThoughtLifecycle(t, repo, ctx, ambiguous.id, false, "tombstone")
	assertReaderThoughtLifecycle(t, repo, ctx, missing.id, false, "tombstone")
	if thought, err := repo.GetThought(ctx, explicit.id); !errors.Is(err, repository.ErrNotFound) || thought != nil {
		t.Fatalf("GetThought(%s) = %#v, %v; want explicit user deletion hidden", explicit.id, thought, err)
	}
	var explicitDeleted bool
	var explicitReason string
	if err := pool.QueryRow(t.Context(), `
		SELECT thought.deleted,tombstone.reason
		FROM reader_thoughts thought
		JOIN reader_thought_tombstones tombstone ON tombstone.thought_id=thought.id
		WHERE thought.id=$1`, explicit.id).Scan(&explicitDeleted, &explicitReason); err != nil {
		t.Fatalf("read explicit deletion projection: %v", err)
	}
	if !explicitDeleted || explicitReason != "user_deleted" {
		t.Fatalf("explicit deletion projection = deleted:%v reason:%q, want true/user_deleted", explicitDeleted, explicitReason)
	}
	var restoreOps int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE device_id='reader-lifecycle' AND host_kind=$1 AND host_id=$2`, host.kind, host.id.String()).Scan(&restoreOps); err != nil {
		t.Fatalf("count restore ops: %v", err)
	}
	if restoreOps != 1 {
		t.Fatalf("restore ops = %d, want one uniquely re-anchored thought", restoreOps)
	}
	if _, err := repo.RestoreHost(ctx, host.kind, host.id); err != nil {
		t.Fatalf("restore no-op: %v", err)
	}
	var restoreOpsAfterNoop int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE device_id='reader-lifecycle' AND host_kind=$1 AND host_id=$2`, host.kind, host.id.String()).Scan(&restoreOpsAfterNoop); err != nil {
		t.Fatalf("count restore ops after no-op: %v", err)
	}
	if restoreOpsAfterNoop != restoreOps {
		t.Fatalf("restore no-op appended %d extra operations", restoreOpsAfterNoop-restoreOps)
	}

	live, _, err := repo.ListThoughts(ctx, "", "", 100)
	if err != nil || len(live) != 1 || live[0].ID != unique.id {
		t.Fatalf("live thought list = %#v, %v; want only %s", live, err, unique.id)
	}
	search, total, _, err := repo.SearchThoughts(ctx, "history-search", "", 100)
	if err != nil || total != 3 || len(search) != 3 {
		t.Fatalf("history search = %#v total=%d err=%v; want live plus two tombstones", search, total, err)
	}

	if _, err := repo.SoftDeleteHost(ctx, host.kind, host.id); err != nil {
		t.Fatalf("soft delete before purge: %v", err)
	}
	operationID := uuid.New()
	if err := repo.PurgeHost(ctx, host.kind, host.id, operationID); err != nil {
		t.Fatalf("purge host: %v", err)
	}
	if err := repo.PurgeHost(ctx, host.kind, host.id, operationID); err != nil {
		t.Fatalf("same-operation purge retry: %v", err)
	}
	if err := repo.PurgeHost(ctx, host.kind, host.id, uuid.New()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("different-operation purge retry error = %v, want ErrNotFound", err)
	}

	var thoughtRows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thoughts WHERE host_kind=$1 AND host_id=$2`, host.kind, host.id.String()).Scan(&thoughtRows); err != nil {
		t.Fatalf("count thoughts after purge: %v", err)
	}
	if thoughtRows != 4 {
		t.Fatalf("thought rows after purge = %d, want 4", thoughtRows)
	}
	var snapshot []byte
	if err := pool.QueryRow(t.Context(), `SELECT snapshot FROM reader_thought_tombstones WHERE thought_id=$1`, unique.id).Scan(&snapshot); err != nil {
		t.Fatalf("read historical snapshot after purge: %v", err)
	}
	for _, field := range []string{"body", "target", "quote", "source", "link_id"} {
		if !strings.Contains(string(snapshot), `"`+field+`"`) {
			t.Fatalf("historical snapshot %s missing %q", snapshot, field)
		}
	}

	exported := make(map[string]bool)
	if err := repo.StreamReaderArchiveSection(ctx, "thought_tombstones", func(value []byte) error {
		var item struct {
			ThoughtID string `json:"thought_id"`
		}
		if err := json.Unmarshal(value, &item); err != nil {
			return err
		}
		exported[item.ThoughtID] = true
		return nil
	}); err != nil {
		t.Fatalf("export thought tombstones: %v", err)
	}
	for _, thoughtID := range []string{unique.id, ambiguous.id, missing.id} {
		if !exported[thoughtID] {
			t.Fatalf("archive omitted historical thought %s: %#v", thoughtID, exported)
		}
	}
	search, total, _, err = repo.SearchThoughts(ctx, "history-search", "", 100)
	if err != nil || total != 3 || len(search) != 3 {
		t.Fatalf("history search after purge = %#v total=%d err=%v", search, total, err)
	}
}

func TestReaderHostLifecyclePostgresPurgeRollbackAndMinimalReceipt(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	note := seedReaderLifecycleHost(t, pool, model.ReaderHostNote, "rollback")
	if _, err := pool.Exec(t.Context(), `INSERT INTO reader_note_history (note_id,revision,title,content,reanchor_ops) VALUES ($1,1,'rollback','rollback content','[]'::jsonb)`, note.id); err != nil {
		t.Fatalf("seed note history: %v", err)
	}
	if _, err := repo.SoftDeleteHost(ctx, note.kind, note.id); err != nil {
		t.Fatalf("soft delete rollback note: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION reader_test_reject_note_delete() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced purge failure'; END $$;
		CREATE TRIGGER reader_test_reject_note_delete
		BEFORE DELETE ON reader_notes
		FOR EACH ROW EXECUTE FUNCTION reader_test_reject_note_delete()`); err != nil {
		t.Fatalf("install rollback trigger: %v", err)
	}
	dropRollbackTrigger := func() {
		_, _ = pool.Exec(t.Context(), `DROP TRIGGER IF EXISTS reader_test_reject_note_delete ON reader_notes; DROP FUNCTION IF EXISTS reader_test_reject_note_delete()`)
	}
	t.Cleanup(dropRollbackTrigger)
	rollbackOperation := uuid.New()
	if err := repo.PurgeHost(ctx, note.kind, note.id, rollbackOperation); err == nil {
		t.Fatal("forced purge failure unexpectedly succeeded")
	}
	exists, trashed := readerLifecycleHostState(t, pool, note.kind, note.id)
	if !exists || !trashed {
		t.Fatalf("failed purge host state = exists:%v trashed:%v, want unchanged trash row", exists, trashed)
	}
	var receipts, history int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_host_purge_receipts WHERE host_kind=$1 AND host_id=$2`, note.kind, note.id).Scan(&receipts); err != nil {
		t.Fatalf("count rolled-back receipts: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_note_history WHERE note_id=$1`, note.id).Scan(&history); err != nil {
		t.Fatalf("count rolled-back note history: %v", err)
	}
	if receipts != 0 || history != 1 {
		t.Fatalf("failed purge receipt/history = %d/%d, want 0/1", receipts, history)
	}
	dropRollbackTrigger()

	secret := "private-title-private-body-private-url"
	link := seedReaderLifecycleHost(t, pool, model.ReaderHostLink, secret)
	if _, err := repo.SoftDeleteHost(ctx, link.kind, link.id); err != nil {
		t.Fatalf("soft delete receipt link: %v", err)
	}
	operationID := uuid.New()
	if err := repo.PurgeHost(ctx, link.kind, link.id, operationID); err != nil {
		t.Fatalf("purge receipt link: %v", err)
	}

	rows, err := pool.Query(t.Context(), `SELECT column_name FROM information_schema.columns WHERE table_schema='public' AND table_name='reader_host_purge_receipts' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("list receipt columns: %v", err)
	}
	columns := make([]string, 0, 5)
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			rows.Close()
			t.Fatalf("scan receipt column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate receipt columns: %v", err)
	}
	rows.Close()
	wantColumns := []string{"host_kind", "host_id", "operation_id", "outcome"}
	if !slices.Equal(columns, wantColumns) {
		t.Fatalf("receipt columns = %#v, want %#v", columns, wantColumns)
	}
	var rawReceipt []byte
	if err := pool.QueryRow(t.Context(), `SELECT to_jsonb(receipt) FROM reader_host_purge_receipts receipt WHERE host_kind=$1 AND host_id=$2`, link.kind, link.id).Scan(&rawReceipt); err != nil {
		t.Fatalf("read receipt JSON: %v", err)
	}
	if strings.Contains(string(rawReceipt), secret) || strings.Contains(string(rawReceipt), "title") || strings.Contains(string(rawReceipt), "body") || strings.Contains(string(rawReceipt), "url") || strings.Contains(string(rawReceipt), "quote") {
		t.Fatalf("purge receipt retained user content: %s", rawReceipt)
	}
}

func seedReaderLifecycleHost(t *testing.T, pool *pgxpool.Pool, kind model.ReaderHostKind, label string) readerLifecycleHostFixture {
	t.Helper()
	body := "prefix anchor suffix; unique phrase; repeated and repeated; history-search " + label
	fixture := readerLifecycleHostFixture{kind: kind, body: body, revision: 1}
	switch kind {
	case model.ReaderHostLink:
		fixture.id = seedReaderVNextSavedLink(t, pool, "https://trash.example/"+uuid.NewString(), label, body, "summary")
	case model.ReaderHostInbox:
		fixture.id = seedReaderVNextInbox(t, pool, "https://trash.example/"+uuid.NewString(), label, body, "summary")
	case model.ReaderHostNote:
		fixture.id = seedReaderVNextNote(t, pool, label, body).ID
	default:
		t.Fatalf("unsupported lifecycle host kind %q", kind)
	}
	return fixture
}

func readerLifecycleHostState(t *testing.T, pool *pgxpool.Pool, kind model.ReaderHostKind, id uuid.UUID) (bool, bool) {
	t.Helper()
	table := map[model.ReaderHostKind]string{
		model.ReaderHostLink:  "links",
		model.ReaderHostInbox: "reader_inbox",
		model.ReaderHostNote:  "reader_notes",
	}[kind]
	var deletedAt pgtype.Timestamptz
	err := pool.QueryRow(t.Context(), `SELECT deleted_at FROM `+table+` WHERE id=$1`, id).Scan(&deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	if err != nil {
		t.Fatalf("read %s lifecycle state: %v", kind, err)
	}
	return true, deletedAt.Valid
}

func newReaderLifecycleThoughtFixture(t *testing.T, host readerLifecycleHostFixture, label, exact string) readerLifecycleThoughtFixture {
	t.Helper()
	versionKey := map[model.ReaderHostKind]string{
		model.ReaderHostLink:  "content_revision",
		model.ReaderHostInbox: "metadata_revision",
		model.ReaderHostNote:  "note_revision",
	}[host.kind]
	targetKind := map[model.ReaderHostKind]string{
		model.ReaderHostLink:  "saved-content",
		model.ReaderHostInbox: "inbox",
		model.ReaderHostNote:  "note",
	}[host.kind]
	target := readerVNextJSON(t, map[string]any{
		"kind":    targetKind,
		"host_id": host.id.String(),
		"version": map[string]any{versionKey: host.revision},
	})
	payload := map[string]any{
		"body":   "history-search thought " + label,
		"quote":  map[string]any{"exact": exact},
		"source": "user",
	}
	if host.kind == model.ReaderHostLink {
		payload["link_id"] = host.id.String()
	}
	return readerLifecycleThoughtFixture{
		id:      "thought-" + label + "-" + uuid.NewString(),
		target:  target,
		payload: readerVNextJSON(t, payload),
	}
}

func seedReaderLifecycleThought(t *testing.T, repo *repository.PGXReaderVNextRepository, ctx context.Context, host readerLifecycleHostFixture, label, exact string) readerLifecycleThoughtFixture {
	t.Helper()
	fixture := newReaderLifecycleThoughtFixture(t, host, label, exact)
	acks, err := repo.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID:          "add-" + uuid.NewString(),
		DeviceID:      "lifecycle-test",
		LogicalClock:  1,
		OperationKind: "add",
		AnnotationID:  fixture.id,
		HostKind:      string(host.kind),
		HostID:        host.id.String(),
		Target:        fixture.target,
		Payload:       fixture.payload,
	}})
	if err != nil {
		t.Fatalf("seed %s thought %s: %v", host.kind, label, err)
	}
	if len(acks) != 1 || acks[0].Sequence <= 0 {
		t.Fatalf("seed %s thought acks = %#v", host.kind, acks)
	}
	return fixture
}

func runReaderLifecycleConcurrent(count int, command func() error) []error {
	start := make(chan struct{})
	errorsByWorker := make(chan error, count)
	var wait sync.WaitGroup
	wait.Add(count)
	for range count {
		go func() {
			defer wait.Done()
			<-start
			errorsByWorker <- command()
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByWorker)
	out := make([]error, 0, count)
	for err := range errorsByWorker {
		out = append(out, err)
	}
	return out
}

func assertReaderLifecycleErrors(t *testing.T, errs []error) {
	t.Helper()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent lifecycle command error = %v", err)
		}
	}
}

func assertReaderThoughtLifecycle(t *testing.T, repo *repository.PGXReaderVNextRepository, ctx context.Context, thoughtID string, deleted bool, lifecycle string) {
	t.Helper()
	thought, err := repo.GetThought(ctx, thoughtID)
	if err != nil {
		t.Fatalf("GetThought(%s): %v", thoughtID, err)
	}
	if thought.Deleted != deleted || thought.LifecycleStatus != lifecycle {
		t.Fatalf("thought %s lifecycle = deleted:%v status:%q, want deleted:%v status:%q", thoughtID, thought.Deleted, thought.LifecycleStatus, deleted, lifecycle)
	}
}
