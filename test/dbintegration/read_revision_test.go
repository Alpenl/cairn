package dbintegration

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/repository"
	"webtag/internal/representation"
)

func mustRevisionComponents(t *testing.T, names ...representation.ComponentName) representation.ComponentSet {
	t.Helper()
	components, err := representation.NewComponentSet(names...)
	if err != nil {
		t.Fatalf("NewComponentSet(%v): %v", names, err)
	}
	return components
}

func revisionByName(t *testing.T, base representation.VersionBase) map[representation.ComponentName]int64 {
	t.Helper()
	revisions := make(map[representation.ComponentName]int64, len(base.Components))
	for _, component := range base.Components {
		if _, duplicate := revisions[component.Name]; duplicate {
			t.Fatalf("duplicate revision component %q in %#v", component.Name, base.Components)
		}
		revisions[component.Name] = component.Revision
	}
	return revisions
}

func TestInstallationRepresentationStateIsCompleteAndSingleton(t *testing.T) {
	pool := StartPostgres(t)

	var (
		stateRows   int
		libraryRows int
		globalRows  int
		feedRows    int
		namespace   uuid.UUID
	)
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM installation_state),
		(SELECT count(*) FROM library_read_revision),
		(SELECT count(*) FROM global_read_revision),
		(SELECT count(*) FROM feed_read_revision),
		(SELECT representation_namespace FROM installation_state WHERE singleton)`).
		Scan(&stateRows, &libraryRows, &globalRows, &feedRows, &namespace); err != nil {
		t.Fatalf("read installation representation state: %v", err)
	}
	if stateRows != 1 || libraryRows != 1 || globalRows != 1 || feedRows != 1 {
		t.Fatalf("singleton row counts state/library/global/feed=%d/%d/%d/%d, want 1/1/1/1",
			stateRows, libraryRows, globalRows, feedRows)
	}
	if namespace == uuid.Nil {
		t.Fatal("installation representation namespace is nil")
	}

	if _, err := pool.Exec(t.Context(), `INSERT INTO installation_state (singleton) VALUES (false)`); err == nil {
		t.Fatal("installation_state accepted a non-singleton row")
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO installation_state (singleton) VALUES (true)`); err == nil {
		t.Fatal("installation_state accepted a second singleton row")
	}
}

func TestReadRevisionReturnsEveryInstallationComponentCombination(t *testing.T) {
	pool := StartPostgres(t)
	revisions := repository.NewPGXReadRevisionRepository(pool)

	if _, err := pool.Exec(t.Context(), `UPDATE library_read_revision SET revision=11 WHERE singleton`); err != nil {
		t.Fatalf("seed library revision: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE global_read_revision SET revision=22 WHERE singleton`); err != nil {
		t.Fatalf("seed global revision: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE feed_read_revision SET revision=33 WHERE singleton`); err != nil {
		t.Fatalf("seed feed revision: %v", err)
	}

	want := map[representation.ComponentName]int64{
		representation.LibraryComponent: 11,
		representation.GlobalComponent:  22,
		representation.FeedComponent:    33,
	}
	sets := [][]representation.ComponentName{
		{},
		{representation.LibraryComponent},
		{representation.GlobalComponent},
		{representation.FeedComponent},
		{representation.LibraryComponent, representation.GlobalComponent},
		{representation.LibraryComponent, representation.FeedComponent},
		{representation.GlobalComponent, representation.FeedComponent},
		{representation.LibraryComponent, representation.GlobalComponent, representation.FeedComponent},
	}
	var namespace uuid.UUID
	for _, names := range sets {
		components := mustRevisionComponents(t, names...)
		base, err := revisions.Current(t.Context(), components)
		if err != nil {
			t.Errorf("Current(%q): %v", components.Key(), err)
			continue
		}
		if !base.ValidFor(components) {
			t.Errorf("Current(%q) returned invalid base %#v", components.Key(), base)
			continue
		}
		if namespace == uuid.Nil {
			namespace = base.RepresentationNamespace
		} else if base.RepresentationNamespace != namespace {
			t.Errorf("Current(%q) namespace=%s, want stable %s", components.Key(), base.RepresentationNamespace, namespace)
		}
		got := revisionByName(t, base)
		if len(got) != len(names) {
			t.Errorf("Current(%q) components=%v, want %v", components.Key(), got, names)
			continue
		}
		for _, name := range names {
			if got[name] != want[name] {
				t.Errorf("Current(%q) %s revision=%d, want %d", components.Key(), name, got[name], want[name])
			}
		}
	}
}

func TestReadRevisionFailsClosedWithoutInstallationState(t *testing.T) {
	pool := StartPostgres(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin missing-state probe: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()

	if _, err := tx.Exec(t.Context(), `DELETE FROM installation_state`); err != nil {
		t.Fatalf("delete installation state in probe transaction: %v", err)
	}
	_, err = repository.NewPGXReadRevisionRepository(tx).Current(
		t.Context(),
		mustRevisionComponents(t, representation.LibraryComponent),
	)
	if err == nil || !strings.Contains(err.Error(), "installation state is missing") {
		t.Fatalf("Current() missing-state error=%v, want actionable fail-closed error", err)
	}
}

func TestReadRevisionCoalescesMissingRevisionRowsDuringRepair(t *testing.T) {
	pool := StartPostgres(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin missing-revision probe: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()

	if _, err := tx.Exec(t.Context(), `DELETE FROM library_read_revision; DELETE FROM feed_read_revision`); err != nil {
		t.Fatalf("delete revision rows in probe transaction: %v", err)
	}
	components := mustRevisionComponents(t, representation.LibraryComponent, representation.GlobalComponent, representation.FeedComponent)
	base, err := repository.NewPGXReadRevisionRepository(tx).Current(t.Context(), components)
	if err != nil {
		t.Fatalf("Current() with repairable missing revision rows: %v", err)
	}
	got := revisionByName(t, base)
	if got[representation.LibraryComponent] != 0 || got[representation.FeedComponent] != 0 {
		t.Fatalf("coalesced library/feed revisions=%d/%d, want 0/0", got[representation.LibraryComponent], got[representation.FeedComponent])
	}
	if got[representation.GlobalComponent] < 0 {
		t.Fatalf("global revision=%d, want non-negative", got[representation.GlobalComponent])
	}
}

func TestReadRevisionRepositoryAcceptsTransactionalQuerier(t *testing.T) {
	pool := StartPostgres(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()

	if _, err := tx.Exec(t.Context(), `UPDATE library_read_revision SET revision=revision+1 WHERE singleton`); err != nil {
		t.Fatalf("advance transaction-local revision: %v", err)
	}
	components := mustRevisionComponents(t, representation.LibraryComponent)
	inside, err := repository.NewPGXReadRevisionRepository(tx).Current(t.Context(), components)
	if err != nil {
		t.Fatalf("read transaction-local revision: %v", err)
	}
	outside, err := repository.NewPGXReadRevisionRepository(pool).Current(t.Context(), components)
	if err != nil {
		t.Fatalf("read committed revision: %v", err)
	}
	if revisionByName(t, inside)[representation.LibraryComponent] != revisionByName(t, outside)[representation.LibraryComponent]+1 {
		t.Fatalf("transaction-local base=%#v committed base=%#v", inside, outside)
	}

	if err := tx.Rollback(t.Context()); err != nil && err != pgx.ErrTxClosed {
		t.Fatalf("rollback transaction: %v", err)
	}
}
