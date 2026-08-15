package service

import (
	"context"
	"fmt"
	"io"

	"webtag/internal/dto"
	"webtag/internal/jsonx"
	"webtag/internal/repository"
)

// ConceptExporter is the narrow read interface for streaming the canonical
// installation vocabulary. A nil exporter produces an empty JSON array.
type ConceptExporter interface {
	StreamConcepts(ctx context.Context, yield func(repository.ConceptExportRow) error) error
}

// ExportConcepts streams the installation's canonical concept vocabulary as
// one JSON array, separate from the bare links array at GET /api/export.
//
// Rows are encoded incrementally; the repository keyset-pages them, so the
// full vocabulary is never held in memory. Like link export, an error after
// the opening bracket truncates the array and is returned for the handler to
// log. A nil exporter emits a valid empty array.
func (s *LinkReadService) ExportConcepts(ctx context.Context, w io.Writer) error {
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}
	if s.conceptExport != nil {
		first := true
		streamErr := s.conceptExport.StreamConcepts(ctx, func(row repository.ConceptExportRow) error {
			item := dto.ConceptExportItem{
				ID:          row.ID.String(),
				PrimaryName: row.PrimaryName,
				DisplayName: row.DisplayName,
				Aliases:     row.Aliases,
				UseCount:    row.UseCount,
				CreatedAt:   row.CreatedAt,
			}
			if item.Aliases == nil {
				item.Aliases = []string{}
			}
			if err := writeConceptExportItem(w, item, first); err != nil {
				return err
			}
			first = false
			return nil
		})
		if streamErr != nil {
			return fmt.Errorf("export concepts: %w", streamErr)
		}
	}
	_, err := io.WriteString(w, "]")
	return err
}

// writeConceptExportItem 编码一条概念并写进数组，第一条后每条前缀逗号，
// 保证产出是合法 JSON 数组（与 writeExportItem 同模式，逐条增量编码）。
func writeConceptExportItem(w io.Writer, item dto.ConceptExportItem, first bool) error {
	if !first {
		if _, err := io.WriteString(w, ","); err != nil {
			return err
		}
	}
	data, err := jsonx.Marshal(item)
	if err != nil {
		return fmt.Errorf("export marshal concept: %w", err)
	}
	_, err = w.Write(data)
	return err
}
