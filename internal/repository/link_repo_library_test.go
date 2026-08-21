package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestUpdateLibraryClassificationForCompletionUsesSelectionCAS(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	id := uuid.New()
	expected := model.LibraryKindReading
	mock.ExpectExec(regexp.QuoteMeta(updateLibraryClassificationForCompletionSQL)).
		WithArgs(id, model.LibraryKindSite, true, &expected, false).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	err = updateLibraryClassificationForCompletionOn(
		context.Background(),
		mock,
		UpdateLibraryClassificationParams{ID: id, Kind: model.LibraryKindSite, Locked: true},
		&expected,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateLibraryClassificationForCompletionRejectsInvalidKind(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	err = updateLibraryClassificationForCompletionOn(
		context.Background(),
		mock,
		UpdateLibraryClassificationParams{ID: uuid.New()},
		nil,
		false,
	)
	if err == nil {
		t.Fatal("empty library kind must be rejected")
	}
}

func TestUpdateLibraryClassificationForCompletionClassifiesMiss(t *testing.T) {
	tests := []struct {
		name       string
		inspect    func(pgxmock.PgxPoolIface, uuid.UUID, *model.LibraryKind)
		want       error
		wantOpaque bool
	}{
		{
			name: "selection changed",
			inspect: func(mock pgxmock.PgxPoolIface, id uuid.UUID, expected *model.LibraryKind) {
				mock.ExpectQuery(regexp.QuoteMeta(selectLibrarySelectionChangedSQL)).
					WithArgs(id, expected, true).
					WillReturnRows(pgxmock.NewRows([]string{"changed"}).AddRow(true))
			},
			want: ErrLibrarySelectionChanged,
		},
		{
			name: "row missing",
			inspect: func(mock pgxmock.PgxPoolIface, id uuid.UUID, expected *model.LibraryKind) {
				mock.ExpectQuery(regexp.QuoteMeta(selectLibrarySelectionChangedSQL)).
					WithArgs(id, expected, true).
					WillReturnError(pgx.ErrNoRows)
			},
			want: ErrNotFound,
		},
		{
			name: "inspection failed",
			inspect: func(mock pgxmock.PgxPoolIface, id uuid.UUID, expected *model.LibraryKind) {
				mock.ExpectQuery(regexp.QuoteMeta(selectLibrarySelectionChangedSQL)).
					WithArgs(id, expected, true).
					WillReturnError(errors.New("connection reset"))
			},
			wantOpaque: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()

			id := uuid.New()
			expected := model.LibraryKindReading
			mock.ExpectExec(regexp.QuoteMeta(updateLibraryClassificationForCompletionSQL)).
				WithArgs(id, model.LibraryKindSite, false, &expected, true).
				WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			tt.inspect(mock, id, &expected)

			err = updateLibraryClassificationForCompletionOn(
				context.Background(),
				mock,
				UpdateLibraryClassificationParams{ID: id, Kind: model.LibraryKindSite},
				&expected,
				true,
			)
			if tt.wantOpaque {
				if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrLibrarySelectionChanged) {
					t.Fatalf("database failure was misclassified: %v", err)
				}
			} else if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
