package repository

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestStreamArchiveV2SectionInstallScopesAndYieldsRowsInQueryOrder(t *testing.T) {
	t.Parallel()
	for section, query := range archiveV2SiteSectionSQL {
		section, query := section, query
		t.Run(section, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()
			mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(mock.NewRows([]string{"json"}).AddRow(`{"n":1}`).AddRow(`{"n":2}`))
			var rows []string
			err = NewPGXSiteRepository(mock).StreamArchiveV2Section(context.Background(), section, func(raw []byte) error { rows = append(rows, string(raw)); return nil })
			if err != nil || len(rows) != 2 || rows[0] != `{"n":1}` || rows[1] != `{"n":2}` {
				t.Fatalf("stream = %#v, %v", rows, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStreamArchiveV2SectionRejectsUnknownAndStopsOnConsumerError(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	if err := repo.StreamArchiveV2Section(context.Background(), "unknown", func([]byte) error { return nil }); err == nil {
		t.Fatal("unknown section must fail")
	}
	mock.ExpectQuery(regexp.QuoteMeta(archiveV2SiteSectionSQL["sites"])).WillReturnRows(mock.NewRows([]string{"json"}).AddRow(`{"n":1}`))
	want := errors.New("writer closed")
	if err := repo.StreamArchiveV2Section(context.Background(), "sites", func([]byte) error { return want }); !errors.Is(err, want) {
		t.Fatalf("yield error = %v, want %v", err, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveV2RepositoryProjectionsExcludeStorageOnlyFields(t *testing.T) {
	t.Parallel()
	for section, query := range archiveV2SiteSectionSQL {
		if !strings.Contains(query, "jsonb_build_object") {
			t.Fatalf("%s does not use an explicit JSON projection", section)
		}
		for _, forbidden := range []string{"'tenant_id'"} {
			if strings.Contains(query, forbidden) {
				t.Fatalf("%s archive projection includes storage-only field %s", section, forbidden)
			}
		}
	}
}
