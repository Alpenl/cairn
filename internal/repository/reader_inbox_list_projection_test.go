package repository

import (
	"strings"
	"testing"
)

// TestReaderInboxListColumnsExcludeDetailOnlyPayload is the offline half of the
// "a 4 MiB body must not ride along on the list response" regression. The
// end-to-end half runs against real PostgreSQL in
// test/dbintegration/reader_inbox_list_projection_test.go; this one fails in
// `make gate` the moment someone reintroduces a detail column into the queue
// projection, which is how the oversized payload got there in the first place.
func TestReaderInboxListColumnsExcludeDetailOnlyPayload(t *testing.T) {
	t.Parallel()

	for _, column := range []string{
		"identity_key",
		"suggested_tags",
		"proposal_signals",
		"proposal_status",
		"category_ids",
		"reader_categorizables",
		"job_id",
		"deleted_at",
	} {
		if strings.Contains(readerInboxListColumns, column) {
			t.Fatalf("readerInboxListColumns selects detail-only column %q:\n%s", column, readerInboxListColumns)
		}
	}
	// body and note appear only inside the bounded preview expression: the
	// projection must never select either column on its own.
	for _, bare := range []string{", body,", ", note,", ", body ", ", note ", ", summary,"} {
		if strings.Contains(readerInboxListColumns, bare) {
			t.Fatalf("readerInboxListColumns selects %q outside the bounded preview:\n%s", bare, readerInboxListColumns)
		}
	}
	if !strings.Contains(readerInboxListColumns, "AS preview") {
		t.Fatalf("readerInboxListColumns lost the bounded preview expression:\n%s", readerInboxListColumns)
	}
	// The cut has to happen inside PostgreSQL. Truncating in Go would still
	// pull the whole column across the wire, which is the cost this projection
	// exists to remove.
	if !strings.Contains(readerInboxListColumns, "left(COALESCE(") {
		t.Fatalf("readerInboxListColumns must cut the preview source in SQL:\n%s", readerInboxListColumns)
	}
}

func TestReaderInboxPreviewStaysBoundedForAnOversizedCapture(t *testing.T) {
	t.Parallel()

	// The SQL cut hands Go at most readerInboxPreviewSourceLimit characters,
	// so that is the worst case this function has to bound.
	oversized := strings.Repeat("正文段落 body paragraph ", readerInboxPreviewSourceLimit)
	preview := readerInboxPreview(string([]rune(oversized)[:readerInboxPreviewSourceLimit]))
	if runes := []rune(preview); len(runes) != readerInboxPreviewLimit+1 || runes[len(runes)-1] != '…' {
		t.Fatalf("preview length = %d runes, want %d plus the truncation mark", len([]rune(preview)), readerInboxPreviewLimit)
	}

	if got := readerInboxPreview("  card   text\n\nwith  gaps  "); got != "card text with gaps" {
		t.Fatalf("readerInboxPreview() = %q, want the collapsed single-line card text", got)
	}
	if got := readerInboxPreview(""); got != "" {
		t.Fatalf("readerInboxPreview(\"\") = %q, want an empty preview", got)
	}
}
