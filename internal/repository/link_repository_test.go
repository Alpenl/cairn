package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestLinkRepositoryListDoneAppliesANDTagFilteringAndPagination(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)

	tagFilter := []string{"go", "ai"}
	domain := "example.com"
	contentType := "article"

	createdAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(5 * time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT "+linkListColumnsWithTotal+" FROM links WHERE deleted_at IS NULL AND status = 'done' AND tags @> $1::text[] AND domain = $2 AND content_type = $3 ORDER BY created_at DESC, id DESC LIMIT $4 OFFSET $5",
	)).
		WithArgs(tagFilter, domain, contentType, 10, 0).
		WillReturnRows(
			mock.NewRows(append(linkListColumnsForTest(), "total_count")).
				AddRow(
					uuid.MustParse("11111111-1111-1111-1111-111111111111"),
					"https://example.com/go/ai/",
					"Go AI",
					"summary",
					[]string{"go", "ai"},
					"basic",
					false,
					nil,
					model.LinkStatusDone,
					nil,
					"user description",
					"example.com",
					"article",
					"reading", int64(1), int64(1),
					false, 0, 0, // PF6: has_content / content_cjk_chars / content_words
					createdAt, nil, nil, nil,
					2,
					"/go/",
					nil,
					createdAt,
					updatedAt,
					int64(2),
				).
				AddRow(
					uuid.MustParse("22222222-2222-2222-2222-222222222222"),
					"https://example.com/go/ai/2/",
					"Go AI 2",
					nil,
					[]string{"go", "ai"},
					nil,
					true,
					"title_quality",
					model.LinkStatusDone,
					nil,
					nil,
					"example.com",
					"article",
					"reading", int64(1), int64(1),
					false, 0, 0, // PF6: has_content / content_cjk_chars / content_words
					createdAt.Add(-time.Hour), nil, nil, nil,
					3,
					"/go/ai/",
					nil,
					createdAt.Add(-time.Hour),
					updatedAt.Add(-time.Hour),
					int64(2),
				),
		)

	items, total, err := repo.ListDone(context.Background(), ListLinksFilter{
		Tags:        tagFilter,
		Domain:      &domain,
		ContentType: &contentType,
		Limit:       10,
		Offset:      0,
	})
	if err != nil {
		t.Fatalf("ListDone() error = %v", err)
	}

	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}

	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}

	if items[0].URL != "https://example.com/go/ai/" {
		t.Fatalf("items[0].URL = %q, want first filtered row", items[0].URL)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

// TestLinkRepositoryListDoneStatusSetUsesANY exercises the non-done
// status filter end-to-end through the pgxmock driver: a non-empty
// Statuses slice must produce `status = ANY($1::text[])` with the slice
// as the leading positional arg, and the remaining filters renumbered
// after it. This pins the contract the extension's "processing / failed"
// partitions depend on.
func TestLinkRepositoryListDoneStatusSetUsesANY(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)

	statuses := []string{"pending", "processing", "failed"}
	domain := "example.com"
	createdAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	failErr := "analyzer call failed: status=502"

	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT "+linkListColumnsWithTotal+" FROM links WHERE deleted_at IS NULL AND status = ANY($1::text[]) AND domain = $2 ORDER BY created_at DESC, id DESC LIMIT $3 OFFSET $4",
	)).
		WithArgs(statuses, domain, 20, 0).
		WillReturnRows(
			mock.NewRows(append(linkListColumnsForTest(), "total_count")).
				AddRow(
					uuid.MustParse("11111111-1111-1111-1111-111111111111"),
					"https://example.com/in-flight",
					nil,
					nil,
					[]string{},
					nil,
					false,
					nil,
					model.LinkStatusProcessing,
					nil,
					nil,
					"example.com",
					nil,
					nil, int64(1), int64(1),
					false, 0, 0, // PF6: has_content / content_cjk_chars / content_words
					createdAt, nil, nil, nil,
					nil,
					nil,
					nil,
					createdAt,
					updatedAt,
					int64(2),
				).
				AddRow(
					uuid.MustParse("22222222-2222-2222-2222-222222222222"),
					"https://example.com/broken",
					nil,
					nil,
					[]string{},
					nil,
					false,
					nil,
					model.LinkStatusFailed,
					failErr,
					nil,
					"example.com",
					nil,
					nil, int64(1), int64(1),
					false, 0, 0, // PF6: has_content / content_cjk_chars / content_words
					createdAt.Add(-time.Hour), nil, nil, nil,
					nil,
					nil,
					nil,
					createdAt.Add(-time.Hour),
					updatedAt.Add(-time.Hour),
					int64(2),
				),
		)

	items, total, err := repo.ListDone(context.Background(), ListLinksFilter{
		Statuses: statuses,
		Domain:   &domain,
		Limit:    20,
		Offset:   0,
	})
	if err != nil {
		t.Fatalf("ListDone() error = %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if items[0].Status != model.LinkStatusProcessing || items[1].Status != model.LinkStatusFailed {
		t.Fatalf("statuses = [%s %s], want [processing failed]", items[0].Status, items[1].Status)
	}
	if items[1].ErrorMsg == nil || *items[1].ErrorMsg != failErr {
		t.Fatalf("items[1].ErrorMsg = %v, want surfaced failure", items[1].ErrorMsg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestLinkRepositoryCreatePersistsMultimodalFields(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)

	linkID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	createdAt := time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC)
	updatedAt := createdAt.Add(2 * time.Minute)
	inputTitle := "Quarterly Report"
	inputText := "A multimodal ingest payload"
	inputHTML := "<article><p>A multimodal ingest payload</p></article>"
	description := "uploaded report"
	domain := "uploads.example.com"
	contentType := "article"
	pathDepth := 1
	parentPath := "/reports/"
	images := []string{
		"https://cdn.example.com/report/page-1.png",
		"https://cdn.example.com/report/page-2.png",
	}
	metadata := map[string]any{
		"origin":   "upload",
		"mime":     "application/pdf",
		"language": "en",
	}
	var nilKind *model.LibraryKind

	mock.ExpectQuery(regexp.QuoteMeta(insertLinkSQL)).
		WithArgs(
			"https://uploads.example.com/report",
			"document",
			"upload:report-42",
			&inputTitle,
			&inputText,
			&inputHTML,
			mustJSONString(t, images),
			mustJSONString(t, metadata),
			&description,
			model.LinkStatusPending,
			&domain,
			&contentType,
			nilKind,
			false,
			&pathDepth,
			&parentPath,
			nil,
		).
		WillReturnRows(
			mock.NewRows(linkColumns()).AddRow(
				linkID,
				"https://uploads.example.com/report",
				"document",
				"upload:report-42",
				inputTitle,
				inputText,
				inputHTML,
				mustJSONBytes(t, images),
				mustJSONBytes(t, metadata),
				nil,
				nil,
				nil,
				nil,
				false,
				nil,
				model.LinkStatusPending,
				nil,
				description,
				domain,
				contentType,
				nil, false, int64(1), int64(1), int64(1),
				string(model.ContentSourceFetched), false, 0, 0, // PF6: content_source / has_content / content_cjk_chars / content_words
				createdAt, nil, nil, nil,
				pathDepth,
				parentPath,
				nil,
				createdAt,
				updatedAt,
			),
		)

	link, err := repo.Create(context.Background(), CreateLinkParams{
		URL:            "https://uploads.example.com/report",
		SourceKind:     "document",
		SourceKey:      "upload:report-42",
		InputTitle:     &inputTitle,
		InputText:      &inputText,
		InputHTML:      &inputHTML,
		InputImages:    images,
		SourceMetadata: metadata,
		Description:    &description,
		Status:         model.LinkStatusPending,
		Domain:         &domain,
		ContentType:    &contentType,
		PathDepth:      &pathDepth,
		ParentPath:     &parentPath,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if link == nil {
		t.Fatal("Create() returned nil link")
	}

	if link.SourceKind != "document" {
		t.Fatalf("link.SourceKind = %q, want document", link.SourceKind)
	}

	if link.SourceKey != "upload:report-42" {
		t.Fatalf("link.SourceKey = %q, want upload:report-42", link.SourceKey)
	}

	if link.InputTitle == nil || *link.InputTitle != inputTitle {
		t.Fatalf("link.InputTitle = %v, want %q", link.InputTitle, inputTitle)
	}

	if got := link.InputImages; len(got) != len(images) || got[0] != images[0] || got[1] != images[1] {
		t.Fatalf("link.InputImages = %v, want %v", got, images)
	}

	if link.SourceMetadata["origin"] != "upload" {
		t.Fatalf("link.SourceMetadata[origin] = %v, want upload", link.SourceMetadata["origin"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestLinkRepositoryGetBySourceKeyReturnsMatchingLink(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)
	linkID := uuid.MustParse("abababab-abab-abab-abab-abababababab")
	createdAt := time.Date(2026, 5, 1, 7, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	inputTitle := "Gallery"
	inputText := "Image set ingest"
	inputHTML := "<div>Image set ingest</div>"
	images := []string{"https://cdn.example.com/1.png", "https://cdn.example.com/2.png"}
	metadata := map[string]any{"origin": "sync", "batch": "gallery-7"}

	mock.ExpectQuery(regexp.QuoteMeta(getLinkBySourceKeySQL)).
		WithArgs("gallery:7").
		WillReturnRows(
			mock.NewRows(linkColumns()).AddRow(
				linkID,
				"https://example.com/gallery/7",
				"image_set",
				"gallery:7",
				inputTitle,
				inputText,
				inputHTML,
				mustJSONBytes(t, images),
				mustJSONBytes(t, metadata),
				nil,
				nil,
				nil,
				nil,
				false,
				nil,
				model.LinkStatusDone,
				nil,
				nil,
				"example.com",
				"listing",
				"reading", false, int64(1), int64(1), int64(1),
				string(model.ContentSourceFetched), false, 0, 0, // PF6: content_source / has_content / content_cjk_chars / content_words
				createdAt, nil, nil, nil,
				1,
				"/gallery/",
				nil,
				createdAt,
				updatedAt,
			),
		)

	link, err := repo.GetBySourceKey(context.Background(), "gallery:7")
	if err != nil {
		t.Fatalf("GetBySourceKey() error = %v", err)
	}

	if link == nil {
		t.Fatal("GetBySourceKey() returned nil link")
	}

	if link.SourceKind != "image_set" {
		t.Fatalf("link.SourceKind = %q, want image_set", link.SourceKind)
	}

	if link.SourceKey != "gallery:7" {
		t.Fatalf("link.SourceKey = %q, want gallery:7", link.SourceKey)
	}

	if got := link.InputImages; len(got) != 2 || got[1] != "https://cdn.example.com/2.png" {
		t.Fatalf("link.InputImages = %v, want both image URLs", got)
	}

	if link.SourceMetadata["batch"] != "gallery-7" {
		t.Fatalf("link.SourceMetadata[batch] = %v, want gallery-7", link.SourceMetadata["batch"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}
