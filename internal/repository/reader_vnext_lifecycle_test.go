package repository

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

// completeThoughtReattachSnapshot keeps reattach tests on the same complete
// immutable snapshot contract enforced for production replay. Individual
// tests only need to describe the fields relevant to their reanchor branch.
func completeThoughtReattachSnapshot(thoughtID string, snapshot []byte) []byte {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(snapshot, &fields); err != nil {
		panic(err)
	}
	setString := func(name, value string) {
		if len(fields[name]) != 0 {
			return
		}
		raw, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		fields[name] = raw
	}
	fields["id"], _ = json.Marshal(thoughtID)
	if len(fields["snapshot_version"]) == 0 {
		fields["snapshot_version"] = json.RawMessage(`1`)
	}
	setString("host_kind", "link")
	setString("host_id", "old-link")
	setString("type", "thought")
	setString("body", "frozen body")
	setString("source", "user")
	if len(fields["link_id"]) == 0 {
		fields["link_id"] = json.RawMessage(`null`)
	}
	if len(fields["target"]) == 0 {
		fields["target"] = json.RawMessage(`{}`)
	}
	if len(fields["quote"]) == 0 {
		fields["quote"] = json.RawMessage(`null`)
	}
	if len(fields["original_host_snapshot"]) == 0 {
		fields["original_host_snapshot"] = json.RawMessage(`{}`)
	}
	if len(fields["original_host_identity"]) == 0 {
		fields["original_host_identity"] = json.RawMessage(`{"kind":"link","id":"old-link"}`)
	}
	at := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	setString("created_at", at)
	setString("updated_at", at)
	setString("frozen_at", at)
	completed, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return completed
}

func clientReattachOpForTest(
	thoughtID string,
	targetHostKind string,
	targetHostID string,
	expectedLastSequence int64,
	expectedHostRevision int64,
	logicalClock int64,
) model.ReaderThoughtOp {
	var target json.RawMessage
	switch targetHostKind {
	case "inbox":
		target = json.RawMessage(`{"kind":"inbox","host_id":"` + targetHostID + `","version":{"metadata_revision":` + strconv.FormatInt(expectedHostRevision, 10) + `}}`)
	case "note":
		target = json.RawMessage(`{"kind":"note","host_id":"` + targetHostID + `","version":{"note_revision":` + strconv.FormatInt(expectedHostRevision, 10) + `}}`)
	default:
		target = json.RawMessage(`{"kind":"saved-content","host_id":"` + targetHostID + `","version":{"content_revision":` + strconv.FormatInt(expectedHostRevision, 10) + `}}`)
	}
	return model.ReaderThoughtOp{
		OpID:          "client-reattach-op",
		DeviceID:      "client-device",
		LogicalClock:  logicalClock,
		OperationKind: "update",
		AnnotationID:  thoughtID,
		HostKind:      targetHostKind,
		HostID:        targetHostID,
		Target:        target,
		Payload: json.RawMessage(`{"reattach":{"expected_last_sequence":` +
			strconv.FormatInt(expectedLastSequence, 10) + `,"expected_host_revision":` +
			strconv.FormatInt(expectedHostRevision, 10) + `}}`),
		Reattach: &model.ReaderThoughtReattachOperation{
			ExpectedLastSequence: expectedLastSequence,
			ExpectedHostRevision: expectedHostRevision,
		},
	}
}

func TestNormalizeReaderTrashLimit(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		limit int
		want  int
	}{
		{name: "negative", limit: -1, want: 50},
		{name: "zero", limit: 0, want: 50},
		{name: "one", limit: 1, want: 1},
		{name: "maximum", limit: 200, want: 200},
		{name: "over maximum", limit: 201, want: 50},
		{name: "hostile maximum integer", limit: int(^uint(0) >> 1), want: 50},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeReaderTrashLimit(test.limit); got != test.want {
				t.Fatalf("normalizeReaderTrashLimit(%d) = %d, want %d", test.limit, got, test.want)
			}
		})
	}
}

func TestListThoughtHistoryReturnsTombstoneRowsWithHistoricalLifecycle(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	tombstonedAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT " + readerThoughtColumns + ",tt.snapshot,tt.reason,tt.created_at FROM reader_thought_tombstones tt JOIN reader_thoughts ON reader_thoughts.id=tt.thought_id WHERE reader_thoughts.deleted=false AND tt.reason <> 'user_deleted' ORDER BY tt.created_at DESC,tt.thought_id DESC LIMIT $1",
	)).
		WithArgs(30).
		WillReturnRows(mock.NewRows(append(readerThoughtSyncColumnsForTest(), "snapshot", "reason", "tombstoned_at")).
			AddRow(append(readerThoughtSyncRow("historical-thought", 12, false, tombstonedAt), []any{[]byte(`{"snapshot_version":1,"id":"historical-thought","host_kind":"link","host_id":"link-historical-thought","link_id":"00000000-0000-0000-0000-000000000001","type":"thought","target":{},"quote":{},"body":"frozen body","source":"user","created_at":"2026-08-10T09:00:00Z","updated_at":"2026-08-10T09:00:00Z","original_host_snapshot":"frozen original","original_host_identity":{"kind":"link","id":"link-historical-thought"},"frozen_at":"2026-08-10T09:00:00Z"}`), "link_deleted", tombstonedAt}...)...))

	repo := NewPGXReaderVNextRepository(mock)
	items, next, err := repo.ListThoughtHistory(context.Background(), "", 30)
	if err != nil {
		t.Fatalf("ListThoughtHistory() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != "historical-thought" || items[0].LifecycleStatus != "tombstone" {
		t.Fatalf("items = %+v, want one historical tombstone", items)
	}
	if items[0].LifecycleReason == nil || *items[0].LifecycleReason != "link_deleted" {
		t.Fatalf("lifecycle reason = %v, want link_deleted", items[0].LifecycleReason)
	}
	if items[0].Body != "frozen body" || string(items[0].OriginalHostSnapshot) != `"frozen original"` {
		t.Fatalf("history did not use immutable snapshot: %+v", items[0])
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty cursor for a short page", next)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalThoughtReadersRejectMalformedSnapshotWithoutMutableFallback(t *testing.T) {
	t.Parallel()

	const thoughtID = "malformed-historical-thought"
	const mutableSentinel = "mutable-sentinel-must-not-escape"
	malformedSnapshot := []byte(`{"id":"malformed-historical-thought","body":"incomplete snapshot"}`)
	at := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	row := readerThoughtSyncRow(thoughtID, 12, false, at)
	row[6] = mutableSentinel
	row[7] = mutableSentinel

	t.Run("history", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatal(err)
		}
		defer mock.Close()
		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT " + readerThoughtColumns + ",tt.snapshot,tt.reason,tt.created_at FROM reader_thought_tombstones tt JOIN reader_thoughts ON reader_thoughts.id=tt.thought_id WHERE reader_thoughts.deleted=false AND tt.reason <> 'user_deleted' ORDER BY tt.created_at DESC,tt.thought_id DESC LIMIT $1",
		)).WithArgs(30).
			WillReturnRows(mock.NewRows(append(readerThoughtSyncColumnsForTest(), "snapshot", "reason", "tombstoned_at")).
				AddRow(append(row, malformedSnapshot, "link_deleted", at)...))

		items, next, err := NewPGXReaderVNextRepository(mock).ListThoughtHistory(context.Background(), "", 30)
		if !errors.Is(err, ErrInvalidReaderThought) || strings.Contains(err.Error(), mutableSentinel) {
			t.Fatalf("ListThoughtHistory() error = %v, want redacted ErrInvalidReaderThought", err)
		}
		if items != nil || next != "" {
			t.Fatalf("ListThoughtHistory() = items=%#v next=%q, want no fallback", items, next)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("get", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatal(err)
		}
		defer mock.Close()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT " + readerThoughtColumns + " FROM reader_thoughts WHERE id=$1 AND deleted=false")).
			WithArgs(thoughtID).
			WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()).AddRow(row...))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT snapshot,reason,created_at FROM reader_thought_tombstones WHERE thought_id=$1")).
			WithArgs(thoughtID).
			WillReturnRows(mock.NewRows([]string{"snapshot", "reason", "created_at"}).AddRow(malformedSnapshot, "link_deleted", at))

		item, err := NewPGXReaderVNextRepository(mock).GetThought(context.Background(), thoughtID)
		if !errors.Is(err, ErrInvalidReaderThought) || strings.Contains(err.Error(), mutableSentinel) {
			t.Fatalf("GetThought() error = %v, want redacted ErrInvalidReaderThought", err)
		}
		if item != nil {
			t.Fatalf("GetThought() = %#v, want no fallback", item)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("durable-client-reattach", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatal(err)
		}
		defer mock.Close()
		op := clientReattachOpForTest(thoughtID, "link", "target-link", 12, 1, 13)
		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT sequence,device_id,logical_clock,operation_kind,annotation_id,host_kind,host_id,target,payload,recovery_of,expected_winner_key FROM reader_thought_ops WHERE op_id=$1",
		)).WithArgs(op.OpID).WillReturnError(pgx.ErrNoRows)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT " + readerThoughtColumns + " FROM reader_thoughts WHERE id=$1 AND deleted=false FOR UPDATE")).
			WithArgs(thoughtID).
			WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()).AddRow(row...))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT snapshot FROM reader_thought_tombstones WHERE thought_id=$1 FOR UPDATE")).
			WithArgs(thoughtID).
			WillReturnRows(mock.NewRows([]string{"snapshot"}).AddRow(malformedSnapshot))

		_, _, err = NewPGXReaderVNextRepository(mock).appendClientReattachThoughtOp(context.Background(), mock, op)
		if !errors.Is(err, ErrInvalidReaderThought) || strings.Contains(err.Error(), mutableSentinel) {
			t.Fatalf("appendClientReattachThoughtOp() error = %v, want redacted ErrInvalidReaderThought", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestValidateReaderThoughtReplaySnapshotRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()

	const thoughtID = "versioned-historical-thought"
	valid := completeThoughtReattachSnapshot(thoughtID, []byte(`{"body":"frozen body"}`))
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(valid, &fields); err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name    string
		version json.RawMessage
		missing bool
	}{
		{name: "missing", missing: true},
		{name: "string", version: json.RawMessage(`"1"`)},
		{name: "decimal", version: json.RawMessage(`1.0`)},
		{name: "null", version: json.RawMessage(`null`)},
		{name: "future", version: json.RawMessage(`2`)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := make(map[string]json.RawMessage, len(fields))
			for name, value := range fields {
				candidate[name] = append(json.RawMessage(nil), value...)
			}
			if testCase.missing {
				delete(candidate, "snapshot_version")
			} else {
				candidate["snapshot_version"] = testCase.version
			}
			raw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateReaderThoughtReplaySnapshot(thoughtID, raw); !errors.Is(err, ErrInvalidReaderThought) {
				t.Fatalf("validateReaderThoughtReplaySnapshot() error = %v, want ErrInvalidReaderThought", err)
			}
		})
	}
}

func TestMaterializeThoughtOnlyMarksUserDeleteWhenDeleteWinsLWW(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		rows       int64
		wantMarker bool
	}{
		{name: "losing delayed delete keeps host snapshot", rows: 0},
		{name: "winning user delete replaces host snapshot with marker", rows: 1, wantMarker: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()

			linkID := uuid.New()
			mock.ExpectQuery("(?s)SELECT operation.sequence.*FROM reader_thoughts thought.*reader_thought_ops operation").
				WithArgs("thought-delete").
				WillReturnError(pgx.ErrNoRows)
			mock.ExpectExec("(?s)INSERT INTO reader_thoughts.*ON CONFLICT").
				WithArgs(
					pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
					pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
					pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
					pgxmock.AnyArg(),
				).
				WillReturnResult(pgxmock.NewResult("UPDATE", testCase.rows))
			if testCase.wantMarker {
				mock.ExpectExec("(?s)INSERT INTO reader_thought_tombstones.*user_deleted.*ON CONFLICT").
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), int64(42)).
					WillReturnResult(pgxmock.NewResult("INSERT", 1))
			}
			expectThoughtTodoProjectionRefresh(mock, "thought-delete")

			repo := NewPGXReaderVNextRepository(mock)
			err = repo.materializeThought(context.Background(), mock, model.ReaderThoughtOp{
				OpID:          "delete-" + uuid.NewString(),
				DeviceID:      "device-test",
				LogicalClock:  9,
				OperationKind: "delete",
				AnnotationID:  "thought-delete",
				HostKind:      "link",
				HostID:        linkID.String(),
				Target:        []byte(`{"kind":"saved-content","host_id":"` + linkID.String() + `","version":{"content_revision":1}}`),
				Payload:       []byte(`{}`),
			}, 42)
			if err != nil {
				t.Fatalf("materializeThought() error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReaderArchiveExportsTombstoneSnapshotWithoutTenantIdentity(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	raw := []byte(`{"thought_id":"thought-1","host_kind":"link","host_id":"link-1","reason":"link_deleted","snapshot":{"snapshot_version":1,"id":"thought-1","host_kind":"link","host_id":"link-1","link_id":null,"type":"thought","body":"keep this","target":{},"quote":null,"source":"user","created_at":"2026-08-11T00:00:00Z","updated_at":"2026-08-11T00:00:00Z","original_host_snapshot":{},"original_host_identity":{},"frozen_at":"2026-08-11T00:00:00Z"}}`)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT jsonb_build_object(")).
		WillReturnRows(mock.NewRows([]string{"jsonb_build_object"}).AddRow(raw))

	repo := NewPGXReaderVNextRepository(mock)
	var exported []byte
	if err := repo.StreamReaderArchiveSection(context.Background(), "thought_tombstones", func(value []byte) error {
		exported = append(exported, value...)
		return nil
	}); err != nil {
		t.Fatalf("StreamReaderArchiveSection() error = %v", err)
	}
	var value map[string]any
	if err := json.Unmarshal(exported, &value); err != nil {
		t.Fatalf("archive row is invalid JSON: %v", err)
	}
	if _, ok := value["tenant_id"]; ok {
		t.Fatal("archive row leaked tenant_id")
	}
	if value["reason"] != "link_deleted" {
		t.Fatalf("archive reason = %v, want link_deleted", value["reason"])
	}
	if _, ok := value["snapshot"].(map[string]any); !ok {
		t.Fatalf("archive snapshot = %#v, want object", value["snapshot"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReaderSearchUsesInstallationScopeAndNarrowProjections(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	linkID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	updatedAt := time.Date(2026, 8, 10, 11, 0, 0, 0, time.UTC)
	snapshotAt := updatedAt.Add(time.Minute)
	mock.ExpectQuery("(?s)WITH search_authority.*matching_thoughts.*LEFT JOIN reader_thought_tombstones.*thought.deleted=false.*snapshot.*quote.*ORDER BY updated_at DESC, thought_id DESC LIMIT \\$4").
		WithArgs("%private%", nil, nil, 21).
		WillReturnRows(mock.NewRows([]string{"id", "host_kind", "host_id", "link_id", "snippet", "count", "updated_at", "lifecycle_status", "lifecycle_reason", "history_deep_link", "snapshot_sequence", "snapshot_at"}).
			AddRow("thought-tenant", "link", "link-tenant", linkID.String(), "private thought", int64(1), updatedAt, "active", nil, "", int64(12), snapshotAt))
	mock.ExpectQuery("(?s)SELECT id, title.*FROM reader_notes.*WHERE deleted_at IS NULL.*title ILIKE \\$1 OR published_content ILIKE \\$1.*LIMIT \\$2").
		WithArgs("%private%", 20).
		WillReturnRows(mock.NewRows([]string{"id", "title", "snippet", "published_revision", "count", "updated_at"}).
			AddRow(uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd"), "Private note", "private note", int64(2), int64(1), updatedAt))

	repo := NewPGXReaderVNextRepository(mock)
	ctx := context.Background()
	thoughts, thoughtTotal, _, err := repo.SearchThoughts(ctx, "private", "", 20)
	if err != nil || thoughtTotal != 1 || len(thoughts) != 1 {
		t.Fatalf("SearchThoughts() = items=%+v total=%d err=%v", thoughts, thoughtTotal, err)
	}
	notes, noteTotal, err := repo.SearchPublishedNotes(ctx, "private", 20)
	if err != nil || noteTotal != 1 || len(notes) != 1 {
		t.Fatalf("SearchPublishedNotes() = items=%+v total=%d err=%v", notes, noteTotal, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
