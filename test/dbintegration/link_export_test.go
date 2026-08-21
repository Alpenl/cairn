// link_export_test.go exercises the Archive v2 links section against a real
// Postgres. The pure-Go unit tests in internal/service cover the cursor-batch
// loop and array framing with in-memory fakes; this file proves the service
// streams the actual done-link rows from the DB, excludes non-done links,
// carries the full business fields, and never leaks raw input fields.
package dbintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/repository"
	"webtag/internal/service"
)

// seedExportLink inserts a link in the given status, optionally with raw input_*
// payload. These raw capture fields must not be serialized.
func seedExportLink(t *testing.T, pool *pgxpool.Pool, url, status, title string, tags []string, withSensitive bool) {
	t.Helper()
	if withSensitive {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO links (url, source_key, status, title, summary, tags, domain, content_type, fetcher_type,
			                    input_title, input_text, input_html, first_collected_at)
			 VALUES ($1, $1, $2, $3, 'a summary', $4, 'example.com', 'article', 'basic',
			         'raw input title', 'raw input text', '<p>raw html</p>', NOW())`,
			url, status, title, tags,
		); err != nil {
			t.Fatalf("seed export link %q: %v", url, err)
		}
		return
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO links (url, source_key, status, title, summary, tags, domain, content_type, fetcher_type, first_collected_at)
		 VALUES ($1, $1, $2, $3, 'a summary', $4, 'example.com', 'article', 'basic', NOW())`,
		url, status, title, tags,
	); err != nil {
		t.Fatalf("seed export link %q: %v", url, err)
	}
}

// TestExportArchiveLinksStreamsDoneLinksOnlyWithFullFields verifies that the
// v2 links section returns every done link (and only done links) with full
// business fields, and never serializes the raw input_* fields.
func TestExportArchiveLinksStreamsDoneLinksOnlyWithFullFields(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()

	// Two done links (one carrying sensitive input_* columns) and two
	// non-done links that must be excluded from the export.
	seedExportLink(t, pool, "https://example.com/done-1", "done", "Done One", []string{"go", "db"}, true)
	seedExportLink(t, pool, "https://example.com/done-2", "done", "Done Two", []string{"ai"}, false)
	seedExportLink(t, pool, "https://example.com/pending", "pending", "Pending", []string{"x"}, false)
	seedExportLink(t, pool, "https://example.com/failed", "failed", "Failed", []string{"y"}, false)

	links := repository.NewPGXLinkRepository(pool)
	svc := service.NewLinkReadService(service.LinkReadServiceOptions{Links: links})

	var buf bytes.Buffer
	count, err := svc.ExportArchiveLinks(ctx, &buf)
	if err != nil {
		t.Fatalf("ExportArchiveLinks: %v", err)
	}
	if count != 2 {
		t.Fatalf("ExportArchiveLinks count = %d, want 2", count)
	}

	// Decode into generic maps so we can assert which JSON keys are present /
	// absent — the typed DTO would silently drop unknown keys.
	var items []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &items); err != nil {
		t.Fatalf("export body is not a valid JSON array: %v\nbody=%s", err, buf.String())
	}

	if len(items) != 2 {
		t.Fatalf("exported %d items, want 2 (done only); body=%s", len(items), buf.String())
	}

	urls := map[string]map[string]any{}
	for _, it := range items {
		url, _ := it["url"].(string)
		urls[url] = it
	}
	if _, ok := urls["https://example.com/done-1"]; !ok {
		t.Errorf("done-1 missing from export")
	}
	if _, ok := urls["https://example.com/done-2"]; !ok {
		t.Errorf("done-2 missing from export")
	}
	if _, ok := urls["https://example.com/pending"]; ok {
		t.Errorf("pending link must NOT be exported")
	}
	if _, ok := urls["https://example.com/failed"]; ok {
		t.Errorf("failed link must NOT be exported")
	}

	// Field completeness on the sensitive row.
	done1 := urls["https://example.com/done-1"]
	for _, key := range []string{"id", "url", "title", "summary", "tags", "domain", "content_type", "fetcher_type", "status", "created_at", "updated_at", "is_low_confidence"} {
		if _, ok := done1[key]; !ok {
			t.Errorf("export item missing business field %q; keys=%v", key, keysOf(done1))
		}
	}

	// Sensitive fields must NEVER appear.
	for _, forbidden := range []string{"input_title", "input_text", "input_html", "input_images"} {
		if _, ok := done1[forbidden]; ok {
			t.Errorf("export leaked forbidden field %q", forbidden)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
