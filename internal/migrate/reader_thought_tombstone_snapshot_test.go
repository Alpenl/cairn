package migrate

import (
	"os"
	"strings"
	"testing"
	"unicode"
)

func TestReaderThoughtTombstoneSnapshotMigrationPinsFullReplayContract(t *testing.T) {
	t.Parallel()

	var step Step
	for _, candidate := range Steps() {
		if candidate.ID == readerThoughtTombstoneSnapshotMigrationID {
			step = candidate
			break
		}
	}
	if step.ID == "" {
		t.Fatalf("migration %q not found", readerThoughtTombstoneSnapshotMigrationID)
	}
	if step.Manual || step.NonTransactional {
		t.Fatalf("tombstone snapshot migration flags = Manual:%v NonTransactional:%v, want automatic transactional", step.Manual, step.NonTransactional)
	}
	sql := compactReaderTombstoneSQL(strings.Join(step.SQL, "\n"))
	assertReaderTombstoneSnapshotContract(t, "migration trash trigger", readerTombstoneFunctionSQL(t, sql, "reader_tombstone_trashed_link_thoughts", false), "new")
	assertReaderTombstoneSnapshotContract(t, "migration delete trigger", readerTombstoneFunctionSQL(t, sql, "reader_tombstone_deleted_link_thoughts", false), "old")

	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	schemaSQL := compactReaderTombstoneSQL(string(schema))
	assertReaderTombstoneSnapshotContract(t, "schema.sql trash trigger", readerTombstoneFunctionSQL(t, schemaSQL, "reader_tombstone_trashed_link_thoughts", true), "new")
	assertReaderTombstoneSnapshotContract(t, "schema.sql delete trigger", readerTombstoneFunctionSQL(t, schemaSQL, "reader_tombstone_deleted_link_thoughts", true), "old")
}

func compactReaderTombstoneSQL(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
}

func readerTombstoneFunctionSQL(t *testing.T, sql, name string, schema bool) string {
	t.Helper()
	header := "createorreplacefunction" + name + "()returnstrigger"
	if schema {
		header = "createfunctionpublic." + name + "()returnstrigger"
	}
	start := strings.Index(sql, header)
	if start < 0 {
		t.Fatalf("tombstone function %q not found", name)
	}
	end := strings.Index(sql[start:], "end;$$")
	if end < 0 {
		t.Fatalf("tombstone function %q has no body terminator", name)
	}
	end += start + len("end;$$")
	if !schema {
		const fixedSearchPath = "setsearch_path=pg_catalog,public"
		suffix := strings.Index(sql[end:], fixedSearchPath)
		if suffix < 0 {
			t.Fatalf("tombstone function %q has no fixed search_path", name)
		}
		end += suffix + len(fixedSearchPath)
	}
	return sql[start:end]
}

func assertReaderTombstoneSnapshotContract(t *testing.T, source, sql, rowName string) {
	t.Helper()
	if strings.Contains(sql, "doupdate") {
		t.Fatalf("%s must not overwrite an immutable snapshot", source)
	}
	for _, want := range []string{
		"'snapshot_version',1",
		"'id',thought.id",
		"'host_kind',thought.host_kind",
		"'host_id',thought.host_id",
		"'link_id',thought.link_id",
		"'type','thought'",
		"'body',thought.body",
		"'target',thought.target",
		"'quote',thought.quote",
		"'source',thought.source",
		"'created_at',thought.created_at",
		"'updated_at',thought.updated_at",
		"'original_host_snapshot',to_jsonb(coalesce(" + rowName + ".content_document," + rowName + ".content,''))",
		"'original_host_identity',jsonb_build_object('kind','link','id'," + rowName + ".id,'url'," + rowName + ".url,'content_revision'," + rowName + ".content_revision)",
		"'frozen_at',current_timestamp",
		"thought.host_id=" + rowName + ".id::text",
		"onconflict(thought_id)donothing",
		"return" + rowName,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("%s tombstone snapshot contract missing %q", source, want)
		}
	}
	if strings.Contains(source, "schema.sql") {
		if !strings.Contains(sql, "setsearch_pathto'pg_catalog','public'") {
			t.Errorf("%s tombstone snapshot contract missing fixed search_path", source)
		}
	} else if !strings.Contains(sql, "setsearch_path=pg_catalog,public") {
		t.Errorf("%s tombstone snapshot contract missing fixed search_path", source)
	}
}
