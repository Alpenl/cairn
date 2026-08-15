package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func ruleRows(mock pgxmock.PgxPoolIface, id uuid.UUID, host string, revision int64) *pgxmock.Rows {
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	return mock.NewRows([]string{"id", "host", "identity_adapter", "path_prefix", "target_kind", "enabled", "revision", "created_at", "updated_at"}).
		AddRow(id, host, nil, "/docs", string(model.LibraryKindReading), true, revision, now, now)
}

func TestListClassificationRulesIsInstallScoped(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	id := uuid.New()
	mock.ExpectQuery("SELECT " + classificationRuleColumns + " FROM library_classification_rules ORDER BY host, path_prefix NULLS FIRST, id").
		WillReturnRows(ruleRows(mock, id, "example.com", 3))
	rules, err := NewPGXClassificationRuleRepository(mock).ListClassificationRules(context.Background())
	if err != nil || len(rules) != 1 || rules[0].ID != id || rules[0].PathPrefix == nil || *rules[0].PathPrefix != "/docs" {
		t.Fatalf("ListClassificationRules() = %#v, %v", rules, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUpdateClassificationRuleUsesRevisionCAS(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	id := uuid.New()
	host := "docs.example.com"
	path := "/guide"
	pathUpdate := &path
	p := UpdateClassificationRuleParams{ID: id, Revision: 4, Host: &host, PathPrefix: &pathUpdate}
	var nilKind *model.LibraryKind
	var nilEnabled *bool
	mock.ExpectQuery("UPDATE library_classification_rules SET host=COALESCE").
		WithArgs(&host, false, nil, true, "/guide", nilKind, nilEnabled, id, int64(4)).
		WillReturnRows(ruleRows(mock, id, host, 5))
	rule, err := NewPGXClassificationRuleRepository(mock).UpdateClassificationRule(context.Background(), p)
	if err != nil || rule.Revision != 5 || rule.Host != host {
		t.Fatalf("UpdateClassificationRule() = %#v, %v", rule, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
