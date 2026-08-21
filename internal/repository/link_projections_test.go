package repository

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestLinkProjectionColumnsMatchDBTags(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		columns string
		typeOf  reflect.Type
	}{
		{name: "detail", columns: linkDetailColumns, typeOf: reflect.TypeOf(LinkDetailProjection{})},
		{name: "parse input", columns: linkParseInputColumns, typeOf: reflect.TypeOf(LinkParseInput{})},
		{name: "lifecycle", columns: linkLifecycleColumns, typeOf: reflect.TypeOf(LinkLifecycleProjection{})},
		{name: "submit lookup", columns: linkSubmitLookupColumns, typeOf: reflect.TypeOf(LinkSubmitLookup{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			columns := projectionColumns(tc.columns)
			if len(columns) != tc.typeOf.NumField() {
				t.Fatalf("SQL columns=%d projection fields=%d\nSQL: %s", len(columns), tc.typeOf.NumField(), tc.columns)
			}
			for i, column := range columns {
				field := tc.typeOf.Field(i)
				if got := field.Tag.Get("db"); got != projectionColumnName(column) {
					t.Fatalf("column[%d]=%q but %s.%s has db tag %q", i, column, tc.typeOf.Name(), field.Name, got)
				}
			}
		})
	}
}

func TestLinkProjectionScanners(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	parentID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	createdAt := time.Date(2026, 8, 8, 8, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)

	t.Run("detail", func(t *testing.T) {
		t.Parallel()
		mock, repo := newProjectionRepository(t)
		mock.ExpectQuery(regexp.QuoteMeta(getLinkDetailByIDSQL)).
			WithArgs(id).
			WillReturnRows(mock.NewRows(projectionColumns(linkDetailColumns)).AddRow(
				id, "https://example.com/detail", "Title", "Summary", []string{"go"}, "basic",
				true, "thin_content", model.LinkStatusDone, nil, "note", "example.com", "article",
				"reading", int64(7), int64(4), "user", true, 12, 34, 2, "/parent", parentID.String(), createdAt, updatedAt,
			))

		got, err := repo.GetDetailByID(context.Background(), id)
		if err != nil {
			t.Fatalf("GetDetailByID() error = %v", err)
		}
		if got == nil || got.ID != id || got.ParentID == nil || *got.ParentID != parentID || got.ContentSource != model.ContentSourceUser || got.MetadataRevision != 4 {
			t.Fatalf("detail = %#v", got)
		}
		if got.Title == nil || *got.Title != "Title" {
			t.Fatalf("detail nullable fields = %#v", got)
		}
	})

	t.Run("parse input", func(t *testing.T) {
		t.Parallel()
		mock, repo := newProjectionRepository(t)
		mock.ExpectQuery(regexp.QuoteMeta(getLinkParseInputByIDSQL)).
			WithArgs(id).
			WillReturnRows(mock.NewRows(projectionColumns(linkParseInputColumns)).AddRow(
				id, "https://example.com/capture", "browser_capture", "capture:1", "Input title",
				"Input text", "<p>Input</p>", []byte(`["https://img.example/a.png"]`),
				[]byte(`{"capture_source_fingerprint":"abc"}`), "note",
				model.LinkStatusPending, "reading", true, int64(4), int64(5), int64(6), updatedAt,
			))

		got, err := repo.GetParseInputByID(context.Background(), id)
		if err != nil {
			t.Fatalf("GetParseInputByID() error = %v", err)
		}
		if got == nil || got.SourceKind != "browser_capture" || got.SourceKey != "capture:1" {
			t.Fatalf("parse input = %#v", got)
		}
		if len(got.InputImages) != 1 || got.SourceMetadata["capture_source_fingerprint"] != "abc" {
			t.Fatalf("decoded parse payload = %#v", got)
		}
		if got.LibraryKind == nil || *got.LibraryKind != model.LibraryKindReading || !got.LibraryKindLocked {
			t.Fatalf("library selection = %v locked=%v", got.LibraryKind, got.LibraryKindLocked)
		}
		if got.MetadataRevision != 5 || got.ParseGeneration != 6 {
			t.Fatalf("parse fences = metadata revision %d, generation %d; want 5/6", got.MetadataRevision, got.ParseGeneration)
		}
	})

	t.Run("lifecycle", func(t *testing.T) {
		t.Parallel()
		mock, repo := newProjectionRepository(t)
		mock.ExpectQuery(regexp.QuoteMeta(getLinkLifecycleByIDSQL)).
			WithArgs(id).
			WillReturnRows(mock.NewRows(projectionColumns(linkLifecycleColumns)).AddRow(
				id, "https://example.com/lifecycle", model.LinkStatusDone, "reading", true,
				int64(9), true, nil,
			))

		got, err := repo.GetLifecycleByID(context.Background(), id)
		if err != nil {
			t.Fatalf("GetLifecycleByID() error = %v", err)
		}
		if got == nil || got.ContentRevision != 9 || !got.HasContent || got.LibraryKind == nil || *got.LibraryKind != model.LibraryKindReading || !got.LibraryKindLocked {
			t.Fatalf("lifecycle = %#v", got)
		}
	})

	t.Run("submit lookup", func(t *testing.T) {
		t.Parallel()
		mock, repo := newProjectionRepository(t)
		rawURL := "https://example.com/submit"
		mock.ExpectQuery(regexp.QuoteMeta(getLinkSubmitLookupByURLSQL)).
			WithArgs(rawURL).
			WillReturnRows(mock.NewRows(projectionColumns(linkSubmitLookupColumns)).AddRow(
				id, rawURL, nil, model.LinkStatusProcessing, "reading", true, createdAt,
			))

		got, err := repo.GetSubmitLookupByURL(context.Background(), rawURL)
		if err != nil {
			t.Fatalf("GetSubmitLookupByURL() error = %v", err)
		}
		if got == nil || got.SourceKey != rawURL || got.Status != model.LinkStatusProcessing ||
			got.LibraryKind == nil || *got.LibraryKind != model.LibraryKindReading || !got.LibraryKindLocked {
			t.Fatalf("submit lookup = %#v", got)
		}
	})
}

func newProjectionRepository(t *testing.T) (pgxmock.PgxPoolIface, *PGXLinkRepository) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet mock expectations: %v", err)
		}
		mock.Close()
	})
	return mock, NewPGXLinkRepository(mock)
}

func projectionColumns(columns string) []string {
	return splitProjectionColumns(columns)
}

func splitProjectionColumns(columns string) []string {
	var result []string
	start, depth := 0, 0
	for index, char := range columns {
		switch char {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				result = append(result, strings.TrimSpace(columns[start:index]))
				start = index + 1
			}
		}
	}
	return append(result, strings.TrimSpace(columns[start:]))
}

func projectionColumnName(expression string) string {
	upper := strings.ToUpper(expression)
	if index := strings.LastIndex(upper, " AS "); index >= 0 {
		return strings.TrimSpace(expression[index+4:])
	}
	return strings.TrimSpace(expression)
}
