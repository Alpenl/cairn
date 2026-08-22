package repository

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestTagRepositoryListCountsAggregatesDoneLinks(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXTagRepository(mock)

	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT unnest(tags) AS tag, count(*) AS count FROM links WHERE status = 'done' AND deleted_at IS NULL AND tags IS NOT NULL GROUP BY tag ORDER BY count DESC",
	)).
		WillReturnRows(
			mock.NewRows([]string{"tag", "count"}).
				AddRow("go", int64(3)).
				AddRow("ai", int64(2)),
		)

	counts, err := repo.ListCounts(context.Background())
	if err != nil {
		t.Fatalf("ListCounts() error = %v", err)
	}

	if len(counts) != 2 {
		t.Fatalf("len(counts) = %d, want 2", len(counts))
	}

	if counts[0].Tag != "go" || counts[0].Count != 3 {
		t.Fatalf("counts[0] = %+v, want tag go with count 3", counts[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestTreeRepositoryListDomainsAggregatesCounts(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXTreeRepository(mock)

	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT domain, count(*) AS count FROM links WHERE status = 'done' AND deleted_at IS NULL GROUP BY domain ORDER BY count DESC, domain ASC",
	)).
		WillReturnRows(
			mock.NewRows([]string{"domain", "count"}).
				AddRow("example.com", int64(3)).
				AddRow("news.ycombinator.com", int64(2)).
				AddRow(nil, int64(1)).
				AddRow("", int64(1)),
		)

	got, err := repo.ListDomains(context.Background())
	if err != nil {
		t.Fatalf("ListDomains() error = %v", err)
	}
	if got.Total != 7 {
		t.Fatalf("total = %d, want 7 including domainless rows", got.Total)
	}
	if len(got.Domains) != 2 || got.Domains[0].Domain != "example.com" || got.Domains[0].Count != 3 {
		t.Fatalf("got = %#v, want example.com/3 first", got)
	}
}

func TestTreeRepositoryListDomainsScopedFiltersLibraryKindAndKeepsDomainlessTotal(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXTreeRepository(mock)
	kind := model.LibraryKindReading
	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT domain, count(*) AS count FROM links WHERE status = 'done' AND deleted_at IS NULL AND library_kind = $1 GROUP BY domain ORDER BY count DESC, domain ASC",
	)).
		WithArgs(kind).
		WillReturnRows(
			mock.NewRows([]string{"domain", "count"}).
				AddRow("reading.example", int64(2)).
				AddRow(nil, int64(1)).
				AddRow("", int64(1)),
		)

	got, err := repo.ListDomainsScoped(context.Background(), kind)
	if err != nil {
		t.Fatalf("ListDomainsScoped() error = %v", err)
	}
	if got.Total != 4 {
		t.Fatalf("total = %d, want 4 including empty-domain reading rows", got.Total)
	}
	if len(got.Domains) != 1 || got.Domains[0] != (DomainTreeSummary{Domain: "reading.example", Count: 2}) {
		t.Fatalf("domains = %#v, want only reading.example/2", got.Domains)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

// linkColumns / mustJSONBytes / mustJSONString are the shared
// row-builder + JSON-marshal helpers used by every repo test file in the
// package. They live in repository_test.go (not a dedicated _helpers file)
// because Go's package-level test build picks all *_test.go files up the
// same way and there is no need for an extra file just to host four
// helpers.
func linkColumns() []string {
	return []string{
		"id",
		"url",
		"source_kind",
		"source_key",
		"input_title",
		"input_text",
		"input_html",
		"input_images",
		"source_metadata",
		"title",
		"summary",
		"tags",
		"fetcher_type",
		"is_low_confidence",
		"low_confidence_reason",
		"status",
		"error_msg",
		"description",
		"domain",
		"content_type",
		"library_kind",
		"library_kind_locked",
		"content_revision",
		"metadata_revision",
		"parse_generation",
		"content_source",
		"has_content",
		"content_cjk_chars",
		"content_words",
		"first_collected_at",
		"last_recollected_at",
		"payload_purge_due_at",
		"payload_purged_at",
		"path_depth",
		"parent_path",
		"parent_id",
		"created_at",
		"updated_at",
	}
}

// linkListColumnsForTest mirrors the projection used by the list/tree paths
// (linkListColumns in link_repo.go): no source_kind/source_key, no input_*
// columns, no source_metadata. Tests for ListDone / ListVisible use this
// to build mock rows whose layout matches the trimmed scan path.
func linkListColumnsForTest() []string {
	return []string{
		"id",
		"url",
		"title",
		"summary",
		"tags",
		"fetcher_type",
		"is_low_confidence",
		"low_confidence_reason",
		"status",
		"error_msg",
		"description",
		"domain",
		"content_type",
		"library_kind",
		"content_revision",
		"metadata_revision",
		"has_content",
		"content_cjk_chars",
		"content_words",
		"first_collected_at",
		"last_recollected_at",
		"payload_purge_due_at",
		"payload_purged_at",
		"path_depth",
		"parent_path",
		"parent_id",
		"created_at",
		"updated_at",
	}
}

func mustJSONBytes(t *testing.T, value any) []byte {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(%T) error = %v", value, err)
	}
	return data
}

func mustJSONString(t *testing.T, value any) string {
	t.Helper()
	return string(mustJSONBytes(t, value))
}
