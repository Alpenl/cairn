package migrate

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func TestUpCreatesLedgerAndAppliesFreshInstallPlan(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{}}
	if err := Up(context.Background(), db); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	if len(db.execs) == 0 || !strings.Contains(db.execs[0], "CREATE TABLE IF NOT EXISTS public.schema_migrations") {
		t.Fatalf("first exec = %q, want schema_migrations creation", firstString(db.execs))
	}
	want := make([]string, 0, len(steps))
	for _, step := range steps {
		want = append(want, step.ID)
	}
	if !slices.Equal(db.inserts, want) {
		t.Fatalf("recorded migrations = %v, want %v", db.inserts, want)
	}
}

func TestUpSkipsAppliedSteps(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{versions: []string{steps[0].ID}}}
	if err := Up(context.Background(), db); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	if slices.Contains(db.inserts, steps[0].ID) {
		t.Fatalf("Up() reapplied %q: %v", steps[0].ID, db.inserts)
	}
	want := make([]string, 0, len(steps)-1)
	for _, step := range steps[1:] {
		want = append(want, step.ID)
	}
	if got := db.inserts; !slices.Equal(got, want) {
		t.Fatalf("recorded migrations = %v, want %v", got, want)
	}
}

func TestUpFreshInstallUsesOrdinaryPlan(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{}}
	if err := UpFreshInstall(context.Background(), db); err != nil {
		t.Fatalf("UpFreshInstall() error = %v", err)
	}
	want := make([]string, 0, len(steps))
	for _, step := range steps {
		want = append(want, step.ID)
	}
	if got := db.inserts; !slices.Equal(got, want) {
		t.Fatalf("recorded migrations = %v, want %v", got, want)
	}
}

func TestUpReturnsLedgerQueryError(t *testing.T) {
	t.Parallel()

	err := Up(context.Background(), &fakeQuerier{queryErr: errors.New("query failed")})
	if err == nil || !strings.Contains(err.Error(), "query schema_migrations") {
		t.Fatalf("Up() error = %v, want ledger query failure", err)
	}
}

func TestUpReturnsStatementErrorWithMigrationID(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{
		rows:          &fakeRows{},
		failOnContain: "CREATE TABLE public.links",
		execErr:       errors.New("boom"),
	}
	err := Up(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "apply migration "+CurrentSchemaMigrationID) {
		t.Fatalf("Up() error = %v, want migration ID", err)
	}
}

func TestTransactionalStepCommitsDDLAndLedgerTogether(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE example(id bigint)")).
		WillReturnResult(pgxmock.NewResult("CREATE", 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO public.schema_migrations(version) VALUES ($1)")).
		WithArgs("transactional-test").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	if err := applyStep(context.Background(), mock, mock, Step{
		ID:  "transactional-test",
		SQL: []string{"CREATE TABLE example(id bigint)"},
	}); err != nil {
		t.Fatalf("applyStep() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestTransactionalStepRollsBackBeforeLedger(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	mock.ExpectBeginTx(pgx.TxOptions{})
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE example(id bigint)")).
		WillReturnError(errors.New("forced DDL failure"))
	mock.ExpectRollback()

	err = applyStep(context.Background(), mock, mock, Step{
		ID:  "rollback-test",
		SQL: []string{"CREATE TABLE example(id bigint)"},
	})
	if err == nil || !strings.Contains(err.Error(), "forced DDL failure") {
		t.Fatalf("applyStep() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestStepIDAndSQLAreValid(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, len(steps))
	for _, step := range Steps() {
		if strings.TrimSpace(step.ID) == "" {
			t.Fatal("migration has an empty ID")
		}
		if _, duplicate := seen[step.ID]; duplicate {
			t.Fatalf("duplicate migration ID %q", step.ID)
		}
		seen[step.ID] = struct{}{}
		if len(step.SQL) == 0 {
			t.Fatalf("migration %q has no SQL", step.ID)
		}
		for index, statement := range step.SQL {
			if strings.TrimSpace(statement) == "" {
				t.Errorf("migration %q statement %d is empty", step.ID, index)
			}
		}
	}
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

type fakeQuerier struct {
	execs         []string
	inserts       []string
	rows          *fakeRows
	queryErr      error
	queryRowErr   error
	execErr       error
	failOnContain string
}

func (f *fakeQuerier) Exec(_ context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	if len(arguments) == 1 && strings.Contains(sql, "schema_migrations(version)") {
		if version, ok := arguments[0].(string); ok {
			f.inserts = append(f.inserts, version)
		}
	}
	if f.failOnContain != "" && strings.Contains(sql, f.failOnContain) {
		return pgconn.CommandTag{}, f.execErr
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeQuerier) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.rows, nil
}

func (f *fakeQuerier) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return fakeBoolRow{err: f.queryRowErr}
}

type fakeBoolRow struct {
	value bool
	err   error
}

func (r fakeBoolRow) Scan(destinations ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(destinations) != 1 {
		return errors.New("unexpected bool scan arity")
	}
	destination, ok := destinations[0].(*bool)
	if !ok {
		return errors.New("unexpected bool scan destination")
	}
	*destination = r.value
	return nil
}

type fakeRows struct {
	versions []string
	index    int
	err      error
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeRows) NextResultSet() bool                          { return false }

func (r *fakeRows) Next() bool {
	if r.index >= len(r.versions) {
		return false
	}
	r.index++
	return true
}

func (r *fakeRows) Scan(destinations ...any) error {
	if len(destinations) != 1 {
		return errors.New("unexpected scan arity")
	}
	destination, ok := destinations[0].(*string)
	if !ok {
		return errors.New("unexpected scan destination")
	}
	*destination = r.versions[r.index-1]
	return nil
}
