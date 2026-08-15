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
			columns := strings.Split(tc.columns, ", ")
			if len(columns) != tc.typeOf.NumField() {
				t.Fatalf("SQL columns=%d projection fields=%d\nSQL: %s", len(columns), tc.typeOf.NumField(), tc.columns)
			}
			for i, column := range columns {
				field := tc.typeOf.Field(i)
				if got := field.Tag.Get("db"); got != column {
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
				"reading", "auto", true, "site", float64(0.75), "personal_rule_host", "why", "v2",
				int64(7), int64(4), "user", true, 12, 34, 2, "/parent", parentID.String(), createdAt, updatedAt,
			))

		got, err := repo.GetDetailByID(context.Background(), id)
		if err != nil {
			t.Fatalf("GetDetailByID() error = %v", err)
		}
		if got == nil || got.ID != id || got.ParentID == nil || *got.ParentID != parentID || got.ContentSource != model.ContentSourceUser || got.MetadataRevision != 4 {
			t.Fatalf("detail = %#v", got)
		}
		if got.ClassificationConfidence == nil || *got.ClassificationConfidence != float32(0.75) || got.Title == nil || *got.Title != "Title" {
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
				[]byte(`{"parse_depth":"deep","capture_source_fingerprint":"abc"}`), "note",
				model.LinkStatusPending, "reading", "user", "reading", true, int64(4), updatedAt,
			))

		got, err := repo.GetParseInputByID(context.Background(), id)
		if err != nil {
			t.Fatalf("GetParseInputByID() error = %v", err)
		}
		if got == nil || got.SourceKind != "browser_capture" || got.SourceKey != "capture:1" {
			t.Fatalf("parse input = %#v", got)
		}
		if len(got.InputImages) != 1 || got.SourceMetadata["parse_depth"] != "deep" {
			t.Fatalf("decoded parse payload = %#v", got)
		}
		if got.RequestedLibraryKind != model.RequestedLibraryKindReading || got.RequestedLibraryKindSource != model.RequestedLibraryKindSourceUser {
			t.Fatalf("requested intent = %s/%s", got.RequestedLibraryKind, got.RequestedLibraryKindSource)
		}
	})

	t.Run("lifecycle", func(t *testing.T) {
		t.Parallel()
		mock, repo := newProjectionRepository(t)
		mock.ExpectQuery(regexp.QuoteMeta(getLinkLifecycleByIDSQL)).
			WithArgs(id).
			WillReturnRows(mock.NewRows(projectionColumns(linkLifecycleColumns)).AddRow(
				id, "https://example.com/lifecycle", model.LinkStatusDone, "reading", "auto", true,
				"personal_rule_host", int64(9), true, nil,
			))

		got, err := repo.GetLifecycleByID(context.Background(), id)
		if err != nil {
			t.Fatalf("GetLifecycleByID() error = %v", err)
		}
		if got == nil || got.ContentRevision != 9 || !got.HasContent || got.LibraryKindSource == nil || *got.LibraryKindSource != model.LibraryKindSourceAuto {
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
				id, rawURL, nil, model.LinkStatusProcessing, "reading", "user", "reading",
			))

		got, err := repo.GetSubmitLookupByURL(context.Background(), rawURL)
		if err != nil {
			t.Fatalf("GetSubmitLookupByURL() error = %v", err)
		}
		if got == nil || got.SourceKey != rawURL || got.Status != model.LinkStatusProcessing ||
			got.RequestedLibraryKind != model.RequestedLibraryKindReading ||
			got.RequestedLibraryKindSource != model.RequestedLibraryKindSourceUser {
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
	return strings.Split(columns, ", ")
}
