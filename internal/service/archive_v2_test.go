package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"webtag/internal/httperr"
)

type archiveV2LinksFake struct {
	payload string
	count   int
	err     error
}

func (f archiveV2LinksFake) ExportArchiveLinks(_ context.Context, w io.Writer) (int, error) {
	if _, err := io.WriteString(w, f.payload); err != nil {
		return 0, err
	}
	return f.count, f.err
}

type archiveV2SectionsFake struct {
	sections map[string][][]byte
	errAt    string
}

func (f archiveV2SectionsFake) StreamArchiveV2Section(_ context.Context, section string, yield func([]byte) error) error {
	for _, raw := range f.sections[section] {
		if err := yield(raw); err != nil {
			return err
		}
	}
	if section == f.errAt {
		return errors.New("section unavailable")
	}
	return nil
}

func TestParseArchiveV2SectionsAcceptsOnlyFrozenCanonicalValues(t *testing.T) {
	t.Parallel()
	full := FullArchiveV2Selection()
	for _, test := range []struct {
		name    string
		values  []string
		present bool
		want    ArchiveV2Selection
	}{
		{name: "omitted remains full for compatibility", want: full},
		{name: "base", values: []string{"base"}, present: true},
		{name: "base thoughts", values: []string{"base,thoughts"}, present: true, want: ArchiveV2Selection{IncludeThoughts: true}},
		{name: "base notes", values: []string{"base,notes"}, present: true, want: ArchiveV2Selection{IncludeNotes: true}},
		{name: "full explicit", values: []string{"base,thoughts,notes"}, present: true, want: full},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseArchiveV2Sections(test.values, test.present)
			if err != nil || got != test.want {
				t.Fatalf("ParseArchiveV2Sections(%#v, %v) = %#v, %v; want %#v, nil", test.values, test.present, got, err, test.want)
			}
		})
	}

	for _, raw := range [][]string{
		nil,
		{""},
		{"thoughts"},
		{"base,notes,thoughts"},
		{"base,thoughts,thoughts"},
		{"base, thoughts"},
		{"BASE"},
		{"base", "base,thoughts"},
	} {
		_, err := ParseArchiveV2Sections(raw, true)
		carrier, ok := httperr.As(err)
		if !ok || carrier.HTTPStatus() != http.StatusUnprocessableEntity {
			t.Fatalf("invalid values %#v error = %v, want stable 422", raw, err)
		}
		coder, ok := carrier.(httperr.ErrorCoder)
		if !ok || coder.HTTPErrorCode() != httperr.CodeInvalidArchiveSections {
			t.Fatalf("invalid values %#v code = %v, want %s", raw, err, httperr.CodeInvalidArchiveSections)
		}
	}
}

func TestArchiveV2SelectedStreamFiltersPrivateGroupsAndAuthenticatesManifest(t *testing.T) {
	reader := &readerArchiveStreamFake{rows: map[string][][]byte{
		"thoughts":           {[]byte(`{"contract_version":1,"id":"thought-sentinel","winner_key":{"logical_clock":7,"device_id":"d","op_id":"o"}}`)},
		"thought_ops":        {[]byte(`{"contract_version":1,"logical_clock":7,"device_id":"device-sentinel","op_id":"op-sentinel"}`)},
		"thought_tombstones": {[]byte(`{"thought_id":"tombstone-sentinel"}`)},
		"notes":              {[]byte(`{"id":"note-sentinel","draft_content":"private-note-sentinel"}`)},
		"note_history":       {[]byte(`{"id":"history-sentinel","content":"private-history-sentinel"}`)},
		"inbox":              {[]byte(`{"id":"base-sentinel"}`)},
	}}
	svc := NewArchiveV2Service(
		archiveV2LinksFake{payload: `[{"id":"link-1"}]`, count: 1},
		archiveV2SectionsFake{},
		reader,
	)

	for _, test := range []struct {
		name          string
		selection     ArchiveV2Selection
		included      []string
		excluded      []string
		readerKeys    []string
		manifestCount []string
	}{
		{
			name:       "base only",
			selection:  ArchiveV2Selection{},
			included:   []string{"base-sentinel"},
			excluded:   []string{"thought-sentinel", "op-sentinel", "tombstone-sentinel", "note-sentinel", "history-sentinel"},
			readerKeys: readerArchiveBaseSections,
		},
		{
			name:       "thoughts only",
			selection:  ArchiveV2Selection{IncludeThoughts: true},
			included:   []string{"base-sentinel", "thought-sentinel", "op-sentinel", "tombstone-sentinel"},
			excluded:   []string{"note-sentinel", "history-sentinel"},
			readerKeys: append(append([]string{}, readerArchiveBaseSections...), readerArchiveThoughtSections...),
		},
		{
			name:       "notes only",
			selection:  ArchiveV2Selection{IncludeNotes: true},
			included:   []string{"base-sentinel", "note-sentinel", "history-sentinel"},
			excluded:   []string{"thought-sentinel", "op-sentinel", "tombstone-sentinel"},
			readerKeys: append(append([]string{}, readerArchiveBaseSections...), readerArchiveNoteSections...),
		},
		{
			name:       "full",
			selection:  FullArchiveV2Selection(),
			included:   []string{"base-sentinel", "thought-sentinel", "op-sentinel", "tombstone-sentinel", "note-sentinel", "history-sentinel"},
			readerKeys: readerArchiveSections,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader.calls = nil
			var body bytes.Buffer
			const namespace = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			if err := svc.Export(context.Background(), &body, ArchiveV2ExportOptions{
				Selection:           test.selection,
				ClientDataNamespace: namespace,
			}); err != nil {
				t.Fatalf("Export() error = %v", err)
			}
			raw := body.String()
			for _, sentinel := range test.included {
				if !strings.Contains(raw, sentinel) {
					t.Errorf("archive omitted selected sentinel %q", sentinel)
				}
			}
			for _, sentinel := range test.excluded {
				if strings.Contains(raw, sentinel) {
					t.Errorf("archive leaked unselected sentinel %q", sentinel)
				}
			}
			if len(reader.calls) != len(test.readerKeys) {
				t.Fatalf("reader queries = %#v, want exactly %#v", reader.calls, test.readerKeys)
			}
			for index, section := range test.readerKeys {
				if reader.calls[index] != section {
					t.Fatalf("reader query %d = %q, want %q", index, reader.calls[index], section)
				}
			}

			var decoded struct {
				Manifest archiveV2Manifest          `json:"manifest"`
				Reader   map[string]json.RawMessage `json:"reader"`
			}
			if err := json.Unmarshal(body.Bytes(), &decoded); err != nil {
				t.Fatalf("archive is not valid JSON: %v", err)
			}
			if decoded.Manifest.ClientDataNamespace != namespace {
				t.Fatalf("manifest namespace = %q, want %q", decoded.Manifest.ClientDataNamespace, namespace)
			}
			if got, want := strings.Join(decoded.Manifest.Sections, ","), test.selection.Canonical(); got != want {
				t.Fatalf("manifest sections = %q, want %q", got, want)
			}
			for _, section := range test.readerKeys {
				if _, ok := decoded.Reader[section]; !ok {
					t.Errorf("reader body omitted selected section %q", section)
				}
				if _, ok := decoded.Manifest.Counts["reader."+section]; !ok {
					t.Errorf("manifest omitted count for selected reader section %q", section)
				}
			}
			for _, section := range readerArchiveSections {
				selected := false
				for _, expected := range test.readerKeys {
					selected = selected || expected == section
				}
				if !selected {
					if _, ok := decoded.Reader[section]; ok {
						t.Errorf("reader body includes unselected section %q", section)
					}
					if _, ok := decoded.Manifest.Counts["reader."+section]; ok {
						t.Errorf("manifest includes unselected count %q", section)
					}
				}
			}
			marker := strings.LastIndex(raw, `,"manifest":`)
			if marker < 1 {
				t.Fatal("archive did not end payload with manifest field")
			}
			digest := sha256.Sum256([]byte(raw[:marker]))
			if got, want := decoded.Manifest.ChecksumSHA256, hex.EncodeToString(digest[:]); got != want {
				t.Fatalf("manifest checksum = %q, want %q", got, want)
			}
		})
	}
}

func TestArchiveV2PropagatesStreamErrorAndLeavesIncompleteJSON(t *testing.T) {
	t.Parallel()
	svc := NewArchiveV2Service(
		archiveV2LinksFake{payload: "[]"},
		archiveV2SectionsFake{errAt: "site_tags"},
		&readerArchiveStreamFake{},
	)
	var body bytes.Buffer
	err := svc.Export(context.Background(), &body, ArchiveV2ExportOptions{
		ClientDataNamespace: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	})
	if err == nil {
		t.Fatal("Export() must return a section stream error")
	}
	var decoded any
	if json.Unmarshal(body.Bytes(), &decoded) == nil {
		t.Fatalf("failed streaming archive must be incomplete JSON, got %s", body.String())
	}
}

func TestArchiveV2PropagatesLinkExportError(t *testing.T) {
	t.Parallel()
	svc := NewArchiveV2Service(
		archiveV2LinksFake{payload: "[", err: errors.New("link cursor failed")},
		archiveV2SectionsFake{},
		&readerArchiveStreamFake{},
	)
	var body bytes.Buffer
	if err := svc.Export(context.Background(), &body, ArchiveV2ExportOptions{
		ClientDataNamespace: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}); err == nil {
		t.Fatal("Export() must return a link export error")
	}
	if body.String() == "" {
		t.Fatal("archive framing should have begun before link export")
	}
}
