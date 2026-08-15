package service

import (
	"context"
	"fmt"
	"io"

	"webtag/internal/dto"
	"webtag/internal/jsonx"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// exportBatchSize is the cursor page size the export stream walks the table in.
// 500 keeps each round trip and the per-batch concept-display lookup bounded
// while still amortizing the cursor overhead across a meaningful number of rows
// — the whole point of streaming is that the response never holds more than one
// batch of LinkResponse objects in memory regardless of how large the knowledge
// base grows.
const exportBatchSize = 500

// Export streams every done link as a single JSON array to w, batched by a
// (created_at, id) cursor so memory stays O(exportBatchSize) no matter how many
// links exist. Each element is a LinkResponse — the same DTO the read API
// returns — so input_* raw multimodal fields and the embedding vector are never
// serialized (LinkResponse simply has no such fields). Concept display names
// override link.tags per batch exactly like the List path, so synonymous
// surface forms collapse to one canonical string.
//
// The array framing ("[", item commas, "]") is written incrementally: callers
// that wire this to an http.ResponseWriter get a true stream. An error after
// the opening "[" has already flushed leaves a truncated (invalid-JSON)
// response — that is acceptable for a best-effort bulk export and far better
// than buffering the entire table to guarantee atomicity; the handler logs the
// failure and the client re-requests. Note the opening "[" is written before
// the first batch query, so even a first-batch DB error reaches the client as
// a truncated body ("[") under an already-committed 200 — same contract as a
// mid-stream failure; the returned error still lands in the handler's log.
func (s *LinkReadService) Export(ctx context.Context, w io.Writer) error {
	_, err := s.ExportWithCount(ctx, w)
	return err
}

// ExportWithCount is the archive-v2 variant of Export. It keeps the same
// cursor-batched, constant-memory stream while returning the exact number of
// rows emitted into the JSON array for the archive manifest.
func (s *LinkReadService) ExportWithCount(ctx context.Context, w io.Writer) (int, error) {
	if _, err := io.WriteString(w, "["); err != nil {
		return 0, err
	}

	var (
		after   *repository.ListLinksCursor
		written int
	)
	for {
		links, _, err := s.links.ListDone(ctx, repository.ListLinksFilter{
			Statuses: []string{string(model.LinkStatusDone)},
			Cursor:   true,
			After:    after,
			Limit:    exportBatchSize,
		})
		if err != nil {
			return 0, fmt.Errorf("export list done links: %w", err)
		}
		if len(links) == 0 {
			break
		}

		displayByLink := s.fetchDisplayNames(ctx, links)
		for i := range links {
			resp := linkToResponse(links[i])
			if names, ok := displayByLink[links[i].ID]; ok && len(names) > 0 {
				resp.Tags = names
			}
			if err := writeExportItem(w, resp, written == 0); err != nil {
				return 0, err
			}
			written++
		}

		if len(links) < exportBatchSize {
			// A short batch means the cursor is exhausted — no further page can
			// exist, so stop without issuing the extra empty round trip.
			break
		}
		last := links[len(links)-1]
		after = &repository.ListLinksCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	if _, err := io.WriteString(w, "]"); err != nil {
		return 0, err
	}
	return written, nil
}

// writeExportItem encodes one LinkResponse and writes it into the array,
// prefixing a comma for every element after the first so the result is a valid
// JSON array. Marshaling per item (rather than one big slice) is what keeps the
// stream incremental.
func writeExportItem(w io.Writer, item dto.LinkResponse, first bool) error {
	if !first {
		if _, err := io.WriteString(w, ","); err != nil {
			return err
		}
	}
	data, err := jsonx.Marshal(item)
	if err != nil {
		return fmt.Errorf("export marshal link: %w", err)
	}
	_, err = w.Write(data)
	return err
}
