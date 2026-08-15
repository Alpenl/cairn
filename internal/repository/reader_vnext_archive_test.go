package repository

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

const readerArchiveValidThoughtSnapshot = `{
  "snapshot_version":1,
  "id":"thought-1",
  "host_kind":"link",
  "host_id":"link-1",
  "link_id":null,
  "type":"thought",
  "body":"frozen thought",
  "target":{},
  "quote":null,
  "source":"user",
  "created_at":"2026-08-11T00:00:00Z",
  "updated_at":"2026-08-11T00:00:00Z",
  "original_host_snapshot":{},
  "original_host_identity":{},
  "frozen_at":"2026-08-11T00:00:00Z"
}`

func TestReaderArchiveThoughtSectionsCarryLamportContract(t *testing.T) {
	t.Parallel()

	for section, wants := range map[string][]string{
		"thoughts": {
			"'contract_version',1",
			"'winner_key'",
			"'logical_clock',thought.winner_logical_clock",
			"'device_id',thought.winner_device_id",
			"'op_id',thought.winner_op_id",
			"'original_host_snapshot'",
			"'lifecycle_status'",
			"), tombstone.snapshot, thought.id::text",
		},
		"thought_ops": {
			"'contract_version',1",
			"'op_id',op_id",
			"'device_id',device_id",
			"'logical_clock',logical_clock",
			"'recovery_of',recovery_of",
			"'expected_current_winner_key',expected_winner_key",
		},
		"thought_supersession_events": {
			"'sequence',sequence",
			"'annotation_id',annotation_id",
			"'loser',loser",
			"'winner_at_detection',winner_at_detection",
		},
	} {
		sql := strings.ToLower(strings.Join(strings.Fields(readerArchiveSectionSQL[section]), " "))
		for _, want := range wants {
			if !strings.Contains(sql, want) {
				t.Fatalf("archive section %s missing %q: %s", section, want, sql)
			}
		}
	}
}

func TestReaderArchiveFeedProjectionIsCompleteAndExcludesRefreshLeases(t *testing.T) {
	t.Parallel()

	for section, fields := range map[string][]string{
		"feed_folders":       {"'id',id", "'name',name", "'created_at',created_at", "'updated_at',updated_at"},
		"feed_subscriptions": {"'folder_id',folder_id", "'canonical_url',canonical_url", "'active',active"},
		"feed_items":         {"'subscription_id',subscription_id", "'content_text',content_text", "'content_html',content_html", "'read_at',read_at", "'starred',starred", "'read_later',read_later", "'link_id',link_id"},
		"feed_saves":         {"'feed_item_id',feed_item_id", "'link_id',link_id", "'created_link',created_link"},
	} {
		sql := strings.ToLower(strings.Join(strings.Fields(readerArchiveSectionSQL[section]), " "))
		for _, field := range fields {
			if !strings.Contains(sql, field) {
				t.Fatalf("archive section %s missing %q: %s", section, field, sql)
			}
		}
	}

	subscriptions := strings.ToLower(readerArchiveSectionSQL["feed_subscriptions"])
	for _, operational := range []string{"refresh_claim_token", "refresh_claimed_until", "etag", "last_modified", "last_error", "failure_count"} {
		if strings.Contains(subscriptions, operational) {
			t.Fatalf("feed subscription archive leaks operational field %s", operational)
		}
	}
}

func TestReaderArchiveThoughtTombstonesNeverFallBackToMutableProjection(t *testing.T) {
	t.Parallel()

	sql := strings.ToLower(strings.Join(strings.Fields(readerArchiveSectionSQL["thoughts"]), " "))
	if strings.Contains(sql, "coalesce(tombstone.snapshot") {
		t.Fatalf("thought archive may not fall back from a tombstone snapshot to mutable projection: %s", sql)
	}
	for _, field := range []string{"'id',case when tombstone.thought_id is null", "'body',case when tombstone.thought_id is null", "'source',case when tombstone.thought_id is null", "'target',case when tombstone.thought_id is null", "'quote',case when tombstone.thought_id is null"} {
		if !strings.Contains(sql, field) {
			t.Fatalf("thought archive must select %s from one authority branch: %s", field, sql)
		}
	}
}

func TestStreamReaderArchiveSectionsCoverInstallation(t *testing.T) {
	sections := make([]string, 0, len(readerArchiveSectionSQL))
	for section := range readerArchiveSectionSQL {
		sections = append(sections, section)
	}
	sort.Strings(sections)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)

	for _, section := range sections {
		query := readerArchiveSectionSQL[section]
		if strings.Contains(strings.ToLower(query), "tenant") {
			t.Fatalf("archive section %s retains tenant ownership: %s", section, query)
		}
		mockRows := mock.NewRows([]string{"jsonb_build_object"}).AddRow([]byte(`{"section":"` + section + `"}`))
		switch section {
		case "thoughts":
			mockRows = mock.NewRows([]string{"jsonb_build_object", "snapshot", "thought_id"}).
				AddRow([]byte(`{"section":"thoughts"}`), nil, "thought-1")
		case "thought_tombstones":
			mockRows = mock.NewRows([]string{"jsonb_build_object"}).AddRow([]byte(`{"thought_id":"thought-1","snapshot":` + readerArchiveValidThoughtSnapshot + `}`))
		}
		mock.ExpectQuery(regexp.QuoteMeta(strings.TrimSpace(query))).
			WillReturnRows(mockRows)
		var rows int
		if err := repo.StreamReaderArchiveSection(context.Background(), section, func(raw []byte) error {
			rows++
			if len(raw) == 0 {
				t.Fatal("archive callback received empty JSON")
			}
			return nil
		}); err != nil {
			t.Fatalf("StreamReaderArchiveSection(%q) error = %v", section, err)
		}
		if rows != 1 {
			t.Fatalf("StreamReaderArchiveSection(%q) callbacks = %d, want 1", section, rows)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamReaderArchiveThoughtSnapshotsFailClosed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		section string
		columns []string
		values  []any
	}{
		{
			name:    "flattened thoughts",
			section: "thoughts",
			columns: []string{"jsonb_build_object", "snapshot", "thought_id"},
			values:  []any{[]byte(`{"id":"thought-1","lifecycle_status":"tombstone"}`), []byte(`{}`), "thought-1"},
		},
		{
			name:    "thought tombstones",
			section: "thought_tombstones",
			columns: []string{"jsonb_build_object"},
			values:  []any{[]byte(`{"thought_id":"thought-1","snapshot":{}}`)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()

			rows := mock.NewRows(tc.columns).AddRow(tc.values...)
			mock.ExpectQuery(regexp.QuoteMeta(strings.TrimSpace(readerArchiveSectionSQL[tc.section]))).
				WillReturnRows(rows)
			repo := NewPGXReaderVNextRepository(mock)
			called := false
			err = repo.StreamReaderArchiveSection(context.Background(), tc.section, func([]byte) error {
				called = true
				return nil
			})
			if !errors.Is(err, ErrInvalidReaderThought) {
				t.Fatalf("StreamReaderArchiveSection(%q) error = %v, want ErrInvalidReaderThought", tc.section, err)
			}
			if called {
				t.Fatalf("StreamReaderArchiveSection(%q) yielded malformed tombstone", tc.section)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStreamReaderArchiveThoughtSnapshotsRejectUnsupportedVersions(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		version string
	}{
		{name: "missing", version: ""},
		{name: "string", version: `"1"`},
		{name: "decimal", version: `1.0`},
		{name: "null", version: `null`},
		{name: "future", version: `2`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()

			snapshot := strings.Replace(readerArchiveValidThoughtSnapshot, `"snapshot_version":1,`, "", 1)
			if testCase.version != "" {
				snapshot = strings.Replace(snapshot, "{", `{"snapshot_version":`+testCase.version+`,`, 1)
			}
			mock.ExpectQuery("(?s)SELECT .*FROM reader_thought_tombstones.*WHERE reason <> 'user_deleted'.*").
				WillReturnRows(mock.NewRows([]string{"jsonb_build_object"}).
					AddRow([]byte(`{"thought_id":"thought-1","snapshot":` + snapshot + `}`)))

			called := false
			err = NewPGXReaderVNextRepository(mock).StreamReaderArchiveSection(context.Background(), "thought_tombstones", func([]byte) error {
				called = true
				return nil
			})
			if !errors.Is(err, ErrInvalidReaderThought) || called {
				t.Fatalf("StreamReaderArchiveSection() error/called = %v/%v, want fail-closed", err, called)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStreamReaderArchiveSectionRejectsUnknownSectionWithoutQuery(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	err = repo.StreamReaderArchiveSection(context.Background(), "not-a-reader-section", func([]byte) error { return nil })
	if err == nil {
		t.Fatal("unknown section should fail")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamReaderArchiveSectionPropagatesCallbackError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery(regexp.QuoteMeta(strings.TrimSpace(readerArchiveSectionSQL["thoughts"]))).
		WillReturnRows(mock.NewRows([]string{"jsonb_build_object", "snapshot", "thought_id"}).AddRow([]byte(`{"id":"thought-1"}`), nil, "thought-1"))
	repo := NewPGXReaderVNextRepository(mock)
	want := errors.New("stop archive")
	err = repo.StreamReaderArchiveSection(context.Background(), "thoughts", func([]byte) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamReaderArchiveSectionRejectsInvalidJSON(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT jsonb_build_object(")).
		WillReturnRows(mock.NewRows([]string{"jsonb_build_object"}).AddRow([]byte("not-json")))
	repo := NewPGXReaderVNextRepository(mock)
	err = repo.StreamReaderArchiveSection(context.Background(), "notes", func([]byte) error { return nil })
	if err == nil {
		t.Fatal("invalid JSON should fail")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
