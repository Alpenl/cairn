package app

import (
	"regexp"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/config"
	"webtag/internal/migrate"
)

func TestTranslationSourceRolloutPreflightRejectsCompatAfterContract(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT EXISTS (
		SELECT 1 FROM schema_migrations WHERE version = $1
	)`)).
		WithArgs(migrate.TranslationSourceContractMigrationID).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))

	err = validateTranslationSourceRolloutPreflight(
		t.Context(),
		config.TranslationSourceRolloutCompat,
		mock,
	)
	if err == nil {
		t.Fatal("preflight error = nil, want compat-after-contract rejection")
	}
	for _, want := range []string{
		"TRANSLATION_SOURCE_ROLLOUT=compat",
		migrate.TranslationSourceContractMigrationID,
		"strict",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("preflight error = %q, want %q", err, want)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationSourceRolloutPreflightAllowsCompatBeforeContract(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT EXISTS (
		SELECT 1 FROM schema_migrations WHERE version = $1
	)`)).
		WithArgs(migrate.TranslationSourceContractMigrationID).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))

	if err := validateTranslationSourceRolloutPreflight(
		t.Context(),
		config.TranslationSourceRolloutCompat,
		mock,
	); err != nil {
		t.Fatalf("preflight error = %v, want compat before contract allowed", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationSourceRolloutPreflightStrictDoesNotProbeMigrationState(t *testing.T) {
	if err := validateTranslationSourceRolloutPreflight(
		t.Context(),
		config.TranslationSourceRolloutStrict,
		nil,
	); err != nil {
		t.Fatalf("strict preflight error = %v, want nil without a marker probe", err)
	}
}
