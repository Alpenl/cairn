package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

type readerArchiveReader interface {
	StreamReaderArchiveSection(context.Context, string, func([]byte) error) error
}

var readerArchiveBaseSections = []string{
	"feed_folders",
	"feed_subscriptions",
	"feed_items",
	"feed_saves",
	"inbox",
	"todos",
	"engagement",
	"feed_hides",
}

// Thought rows, their operation clock, and host tombstones are one privacy
// group. Exporting a clock without its tombstone (or the reverse) could revive
// a user-deleted thought during archive restoration.
var readerArchiveThoughtSections = []string{
	"thoughts",
	"thought_ops",
	"thought_supersession_events",
	"thought_tombstones",
}

// Note history contains prior private bodies and therefore follows notes as a
// single selector group rather than being treated as generic Reader metadata.
var readerArchiveNoteSections = []string{
	"notes",
	"note_history",
}

func readerArchiveSectionsFor(selection ArchiveV2Selection) []string {
	sections := make([]string, 0, len(readerArchiveBaseSections)+len(readerArchiveThoughtSections)+len(readerArchiveNoteSections))
	sections = append(sections, readerArchiveBaseSections...)
	if selection.IncludeThoughts {
		sections = append(sections, readerArchiveThoughtSections...)
	}
	if selection.IncludeNotes {
		sections = append(sections, readerArchiveNoteSections...)
	}
	return sections
}

var readerArchiveSections = readerArchiveSectionsFor(FullArchiveV2Selection())

type readerArchiveExporter struct {
	reader readerArchiveReader
}

func (s readerArchiveExporter) ExportReaderArchive(ctx context.Context, w io.Writer, selection ArchiveV2Selection) (map[string]int, error) {
	if _, err := io.WriteString(w, `{"schema_version":2,"thought_contract_version":1`); err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(readerArchiveSections))
	for _, section := range readerArchiveSectionsFor(selection) {
		if _, err := io.WriteString(w, `,"`+section+`":[`); err != nil {
			return nil, err
		}
		count := 0
		if err := s.reader.StreamReaderArchiveSection(ctx, section, func(raw []byte) error {
			if err := validateReaderArchiveRow(raw); err != nil {
				return err
			}
			if count > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if _, err := w.Write(raw); err != nil {
				return err
			}
			count++
			return nil
		}); err != nil {
			return nil, fmt.Errorf("export reader archive section %s: %w", section, err)
		}
		if _, err := io.WriteString(w, "]"); err != nil {
			return nil, err
		}
		counts[section] = count
	}
	if _, err := io.WriteString(w, "}"); err != nil {
		return nil, err
	}
	return counts, nil
}

func validateReaderArchiveRow(raw []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return fmt.Errorf("reader archive row is invalid JSON: %w", err)
	}
	if object == nil {
		return fmt.Errorf("reader archive row must be a JSON object")
	}
	if _, ok := object["tenant_id"]; ok {
		return fmt.Errorf("reader archive row contains tenant_id")
	}
	return nil
}
