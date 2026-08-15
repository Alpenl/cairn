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
	if len(db.execs) == 0 || !strings.Contains(db.execs[0], "CREATE TABLE IF NOT EXISTS schema_migrations") {
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

func TestUpToStopsAtTarget(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{}}
	if err := UpTo(context.Background(), db, steps[0].ID); err != nil {
		t.Fatalf("UpTo(first) error = %v", err)
	}
	if got, want := db.inserts, []string{steps[0].ID}; !slices.Equal(got, want) {
		t.Fatalf("recorded migrations = %v, want %v", got, want)
	}
}

func TestUpToRejectsUnknownTargetBeforeMutation(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{}}
	err := UpTo(context.Background(), db, "missing-target")
	if err == nil || !strings.Contains(err.Error(), "unknown migration target") {
		t.Fatalf("UpTo(unknown) error = %v", err)
	}
	if len(db.execs) != 0 {
		t.Fatalf("UpTo(unknown) executed %d statements", len(db.execs))
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
	if err == nil || !strings.Contains(err.Error(), "apply migration "+TranslationSourceContractMigrationID) {
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
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO schema_migrations(version) VALUES ($1)")).
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

func TestApplyStepRecoversInvalidConcurrentIndex(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{indexInvalid: true}
	step := Step{
		ID:                    "concurrent-index-recovery",
		NonTransactional:      true,
		RecoverInvalidIndexes: []string{"public.idx_example"},
		SQL: []string{
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_example ON public.example(id)",
		},
	}
	if err := applyStep(context.Background(), db, nil, step); err != nil {
		t.Fatalf("applyStep() error = %v", err)
	}
	want := []string{
		`DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_example"`,
		step.SQL[0],
		`INSERT INTO schema_migrations(version) VALUES ($1)`,
	}
	if !slices.Equal(db.execs, want) {
		t.Fatalf("execs = %v, want %v", db.execs, want)
	}
	if len(db.queryRowArgs) != 1 || !slices.Equal(db.queryRowArgs[0], []any{`"public"."idx_example"`}) {
		t.Fatalf("catalog probe arguments = %v", db.queryRowArgs)
	}
}

func TestApplyStepPreservesValidConcurrentIndex(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{}
	step := Step{
		ID:                    "valid-concurrent-index",
		NonTransactional:      true,
		RecoverInvalidIndexes: []string{"public.idx_example"},
		SQL: []string{
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_example ON public.example(id)",
		},
	}
	if err := applyStep(context.Background(), db, nil, step); err != nil {
		t.Fatalf("applyStep() error = %v", err)
	}
	for _, statement := range db.execs {
		if strings.Contains(statement, "DROP INDEX") {
			t.Fatalf("valid index was dropped: %v", db.execs)
		}
	}
}

func TestApplyStepRejectsTransactionalIndexRecovery(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{}
	err := applyStep(context.Background(), db, nil, Step{
		ID:                    "invalid-recovery-config",
		RecoverInvalidIndexes: []string{"public.idx_example"},
		SQL:                   []string{"SELECT 1"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid-index recovery but is transactional") {
		t.Fatalf("applyStep() error = %v", err)
	}
}

func TestStepIDsAndExecutionModesAreValid(t *testing.T) {
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
		joined := strings.ToUpper(strings.Join(step.SQL, " "))
		needsNonTransactional := strings.Contains(joined, "CONCURRENTLY")
		if needsNonTransactional != step.NonTransactional {
			t.Errorf("migration %q NonTransactional=%v, SQL requires=%v", step.ID, step.NonTransactional, needsNonTransactional)
		}
		if step.Manual {
			t.Errorf("fresh-install public migration %q must not be manual", step.ID)
		}
		for index, statement := range step.SQL {
			if strings.TrimSpace(statement) == "" {
				t.Errorf("migration %q statement %d is empty", step.ID, index)
			}
		}
	}
}

func TestConcurrentIndexesRegisterRecovery(t *testing.T) {
	t.Parallel()

	pattern := regexp.MustCompile(`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_$]*)`)
	for _, step := range Steps() {
		created := pattern.FindAllStringSubmatch(strings.Join(step.SQL, "\n"), -1)
		registered := make(map[string]struct{}, len(step.RecoverInvalidIndexes))
		for _, name := range step.RecoverInvalidIndexes {
			registered[name] = struct{}{}
		}
		for _, match := range created {
			qualified := "public." + match[1]
			if _, ok := registered[qualified]; !ok {
				t.Errorf("migration %q creates %s without invalid-index recovery", step.ID, qualified)
			}
			delete(registered, qualified)
		}
		for stale := range registered {
			t.Errorf("migration %q registers recovery for uncreated index %s", step.ID, stale)
		}
	}
}

func TestPgvectorFailureIncludesActionableHint(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{
		rows:          &fakeRows{},
		failOnContain: "CREATE EXTENSION IF NOT EXISTS vector",
		execErr:       errors.New(`extension "vector" is not available`),
	}
	err := Up(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "pgvector") || !strings.Contains(err.Error(), db.execErr.Error()) {
		t.Fatalf("Up() error = %v, want underlying extension failure and pgvector hint", err)
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
	queryRowArgs  [][]any
	rows          *fakeRows
	queryErr      error
	queryRowErr   error
	execErr       error
	failOnContain string
	indexInvalid  bool
}

func (f *fakeQuerier) Exec(_ context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	if len(arguments) == 1 && strings.Contains(sql, "INSERT INTO schema_migrations") {
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

func (f *fakeQuerier) QueryRow(_ context.Context, _ string, arguments ...any) pgx.Row {
	f.queryRowArgs = append(f.queryRowArgs, slices.Clone(arguments))
	return fakeBoolRow{value: f.indexInvalid, err: f.queryRowErr}
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
