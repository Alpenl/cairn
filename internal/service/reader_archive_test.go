package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type readerArchiveStreamFake struct {
	rows  map[string][][]byte
	errAt string
	calls []string
}

func (f *readerArchiveStreamFake) StreamReaderArchiveSection(_ context.Context, section string, yield func([]byte) error) error {
	f.calls = append(f.calls, section)
	for _, row := range f.rows[section] {
		if err := yield(row); err != nil {
			return err
		}
	}
	if section == f.errAt {
		return errors.New("archive section failed")
	}
	return nil
}

func TestExportReaderArchiveWritesAllInstallationSections(t *testing.T) {
	fake := &readerArchiveStreamFake{rows: map[string][][]byte{
		"thoughts":                    {[]byte(`{"id":"thought-1"}`)},
		"notes":                       {[]byte(`{"id":"note-1","draft_content":"private draft"}`)},
		"thought_tombstones":          {[]byte(`{"thought_id":"thought-2"}`)},
		"thought_supersession_events": {[]byte(`{"sequence":7,"annotation_id":"thought-1","loser":{"body":"durable loser"},"winner_at_detection":{"body":"winner"}}`)},
	}}
	exporter := readerArchiveExporter{reader: fake}

	var body bytes.Buffer
	counts, err := exporter.ExportReaderArchive(context.Background(), &body, FullArchiveV2Selection())
	if err != nil {
		t.Fatalf("ExportReaderArchive() error = %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body.Bytes(), &decoded); err != nil {
		t.Fatalf("archive is not valid JSON: %v\n%s", err, body.String())
	}
	if string(decoded["schema_version"]) != "2" {
		t.Fatalf("schema_version = %s, want 2", decoded["schema_version"])
	}
	if string(decoded["thought_contract_version"]) != "1" {
		t.Fatalf("thought_contract_version = %s, want 1", decoded["thought_contract_version"])
	}
	for _, section := range readerArchiveSections {
		var rows []json.RawMessage
		if err := json.Unmarshal(decoded[section], &rows); err != nil {
			t.Fatalf("section %s is not an array: %v", section, err)
		}
	}
	if got := string(decoded["notes"]); got != `[{"id":"note-1","draft_content":"private draft"}]` {
		t.Fatalf("notes = %s", got)
	}
	if got := counts["notes"]; got != 1 {
		t.Fatalf("notes count = %d, want 1", got)
	}
	if got := string(decoded["thought_supersession_events"]); got != `[{"sequence":7,"annotation_id":"thought-1","loser":{"body":"durable loser"},"winner_at_detection":{"body":"winner"}}]` {
		t.Fatalf("thought_supersession_events = %s", got)
	}
	if len(fake.calls) != len(readerArchiveSections) {
		t.Fatalf("section calls = %d, want %d", len(fake.calls), len(readerArchiveSections))
	}
}

func TestExportReaderArchivePreservesServerAcceptedThoughtBoundaryRows(t *testing.T) {
	annotationID := strings.Repeat("a", 129)
	const deletedHostID = "purged-inbox:legacy-42"
	fake := &readerArchiveStreamFake{rows: map[string][][]byte{
		"thoughts":           {[]byte(`{"id":"` + annotationID + `","source":"reader-v0-import"}`)},
		"thought_ops":        {[]byte(`{"annotation_id":"` + annotationID + `","operation_kind":"delete","host_kind":"inbox","host_id":"` + deletedHostID + `"}`)},
		"thought_tombstones": {[]byte(`{"thought_id":"deleted-thought","host_kind":"inbox","host_id":"` + deletedHostID + `"}`)},
	}}
	exporter := readerArchiveExporter{reader: fake}
	var body bytes.Buffer
	counts, err := exporter.ExportReaderArchive(context.Background(), &body, ArchiveV2Selection{IncludeThoughts: true})
	if err != nil {
		t.Fatalf("ExportReaderArchive() error = %v", err)
	}

	var decoded struct {
		Thoughts []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"thoughts"`
		ThoughtOps []struct {
			AnnotationID string `json:"annotation_id"`
			HostID       string `json:"host_id"`
		} `json:"thought_ops"`
		Tombstones []struct {
			HostID string `json:"host_id"`
		} `json:"thought_tombstones"`
	}
	if err := json.Unmarshal(body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode exported Reader archive: %v", err)
	}
	if len(decoded.Thoughts) != 1 || decoded.Thoughts[0].ID != annotationID || decoded.Thoughts[0].Source != "reader-v0-import" {
		t.Fatalf("thought export = %#v, want 129-byte ID and persisted source", decoded.Thoughts)
	}
	if len(decoded.ThoughtOps) != 1 || decoded.ThoughtOps[0].AnnotationID != annotationID || decoded.ThoughtOps[0].HostID != deletedHostID {
		t.Fatalf("thought operation export = %#v, want preserved delete host", decoded.ThoughtOps)
	}
	if len(decoded.Tombstones) != 1 || decoded.Tombstones[0].HostID != deletedHostID {
		t.Fatalf("thought tombstone export = %#v, want preserved non-UUID host", decoded.Tombstones)
	}
	for _, section := range []string{"thoughts", "thought_ops", "thought_tombstones"} {
		if got := counts[section]; got != 1 {
			t.Fatalf("%s count = %d, want 1", section, got)
		}
	}
	if got := counts["thought_supersession_events"]; got != 0 {
		t.Fatalf("thought_supersession_events count = %d, want 0", got)
	}
}

func TestArchiveV2IncludesReaderArchive(t *testing.T) {
	reader := &readerArchiveStreamFake{rows: map[string][][]byte{
		"notes": {[]byte(`{"id":"note-1"}`)},
	}}
	svc := NewArchiveV2Service(
		archiveV2LinksFake{payload: "[]"},
		archiveV2SectionsFake{},
		reader,
	)

	var body bytes.Buffer
	options := ArchiveV2ExportOptions{
		Selection:           FullArchiveV2Selection(),
		ClientDataNamespace: "reader-archive-test",
	}
	if err := svc.Export(context.Background(), &body, options); err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body.Bytes(), &decoded); err != nil {
		t.Fatalf("archive is not valid JSON: %v", err)
	}
	var readerArchive map[string]json.RawMessage
	if err := json.Unmarshal(decoded["reader"], &readerArchive); err != nil {
		t.Fatalf("reader archive is not an object: %v", err)
	}
	var notes []json.RawMessage
	if err := json.Unmarshal(readerArchive["notes"], &notes); err != nil || len(notes) != 1 {
		t.Fatalf("reader notes = %s, err=%v", readerArchive["notes"], err)
	}
}

func TestExportReaderArchivePropagatesSectionError(t *testing.T) {
	fake := &readerArchiveStreamFake{errAt: "notes"}
	exporter := readerArchiveExporter{reader: fake}
	var body bytes.Buffer
	_, err := exporter.ExportReaderArchive(context.Background(), &body, FullArchiveV2Selection())
	if err == nil {
		t.Fatal("section error must be returned")
	}
}
func TestExportReaderArchiveRejectsTopLevelTenantIdentity(t *testing.T) {
	fake := &readerArchiveStreamFake{rows: map[string][][]byte{
		"notes": {[]byte(`{"id":"note-1","tenant_id":"tenant-secret"}`)},
	}}
	exporter := readerArchiveExporter{reader: fake}
	var body bytes.Buffer
	if _, err := exporter.ExportReaderArchive(context.Background(), &body, FullArchiveV2Selection()); err == nil {
		t.Fatal("archive export must reject a top-level tenant_id")
	}
}

func TestExportReaderArchiveSkipsPrivateGroupsAtTheRepositoryBoundary(t *testing.T) {
	fake := &readerArchiveStreamFake{rows: map[string][][]byte{
		"thoughts":           {[]byte(`{"id":"thought-private"}`)},
		"thought_tombstones": {[]byte(`{"thought_id":"tombstone-private"}`)},
		"notes":              {[]byte(`{"id":"note-private"}`)},
		"note_history":       {[]byte(`{"id":"history-private"}`)},
		"inbox":              {[]byte(`{"id":"base-row"}`)},
	}}
	exporter := readerArchiveExporter{reader: fake}
	var body bytes.Buffer
	counts, err := exporter.ExportReaderArchive(context.Background(), &body, ArchiveV2Selection{})
	if err != nil {
		t.Fatalf("ExportReaderArchive() error = %v", err)
	}
	for _, privateSection := range append(append([]string{}, readerArchiveThoughtSections...), readerArchiveNoteSections...) {
		for _, queried := range fake.calls {
			if queried == privateSection {
				t.Fatalf("base export queried private section %q", privateSection)
			}
		}
		if _, ok := counts[privateSection]; ok {
			t.Fatalf("base export counted private section %q", privateSection)
		}
	}
	if body.String() == "" || !bytes.Contains(body.Bytes(), []byte("base-row")) {
		t.Fatalf("base export did not stream its non-sensitive rows: %s", body.String())
	}
}
