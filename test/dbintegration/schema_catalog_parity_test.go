package dbintegration

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/migrate"
)

type catalogEntry struct {
	Section    string `json:"section"`
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

func TestFreshInstallAndProductionUpgradeCatalogParity(t *testing.T) {
	freshDSN := isolatedMigrationDatabase(t)
	freshPool := migrationTargetPool(t, freshDSN)
	if err := migrate.UpFreshInstall(t.Context(), freshPool); err != nil {
		t.Fatalf("apply fresh install: %v", err)
	}
	assertCurrentSchemaLedger(t, freshPool)
	assertCurrentRiverLedger(t, freshPool)

	upgradeDSN := isolatedMigrationDatabase(t)
	upgradePool := migrationTargetPool(t, upgradeDSN)
	prepareProductionUpgradeFixture(t, upgradePool)
	if err := migrate.Up(t.Context(), upgradePool); err != nil {
		t.Fatalf("apply production upgrade: %v", err)
	}
	assertCurrentSchemaLedger(t, upgradePool)
	assertCurrentRiverLedger(t, upgradePool)

	freshCatalog := readCatalogManifest(t, freshPool)
	upgradeCatalog := readCatalogManifest(t, upgradePool)
	if !reflect.DeepEqual(freshCatalog, upgradeCatalog) {
		t.Fatalf("fresh install and production upgrade catalogs differ\n%s",
			formatCatalogDiff(t, freshCatalog, upgradeCatalog))
	}
}

func assertCurrentRiverLedger(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	rows, err := pool.Query(t.Context(),
		`SELECT version::int FROM public.river_migration WHERE line=$1 ORDER BY version`,
		migrate.RiverLedgerLine)
	if err != nil {
		t.Fatalf("read river_migration: %v", err)
	}
	defer rows.Close()

	var got []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatalf("scan river_migration: %v", err)
		}
		got = append(got, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate river_migration: %v", err)
	}
	if want := migrate.RiverBundleVersions(); !slices.Equal(got, want) {
		t.Fatalf("river_migration = %v, want %v", got, want)
	}
}

func readCatalogManifest(t *testing.T, pool *pgxpool.Pool) []catalogEntry {
	t.Helper()

	queries := []string{
		`SELECT 'extension' AS section,
		        extname AS name,
		        extversion AS definition
		   FROM pg_extension
		  WHERE extname <> 'plpgsql'`,
		`SELECT 'type' AS section,
		        t.typname AS name,
		        concat_ws('|',
		            'typtype=' || t.typtype::text,
		            'category=' || t.typcategory::text,
		            'base=' || COALESCE(format_type(NULLIF(t.typbasetype, 0), NULL), ''),
		            'notnull=' || t.typnotnull::text,
		            'default=' || COALESCE(t.typdefault, '')
		        ) AS definition
		   FROM pg_type AS t
		   JOIN pg_namespace AS n ON n.oid = t.typnamespace
		  WHERE n.nspname = 'public'
		    AND t.typtype IN ('b','c','d','e')`,
		`SELECT 'relation' AS section,
		        c.relname AS name,
		        concat_ws('|',
		            'kind=' || c.relkind::text,
		            'persistence=' || c.relpersistence::text,
		            'rls=' || c.relrowsecurity::text,
		            'force_rls=' || c.relforcerowsecurity::text
		        ) AS definition
		   FROM pg_class AS c
		   JOIN pg_namespace AS n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public'
		    AND c.relkind IN ('r','p','S','v','m')`,
		`SELECT 'column' AS section,
		        c.relname || '.' || a.attname AS name,
		        concat_ws('|',
		            'type=' || format_type(a.atttypid, a.atttypmod),
		            'notnull=' || a.attnotnull::text,
		            'identity=' || a.attidentity::text,
		            'generated=' || a.attgenerated::text,
		            'default=' || COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		        ) AS definition
		   FROM pg_attribute AS a
		   JOIN pg_class AS c ON c.oid = a.attrelid
		   JOIN pg_namespace AS n ON n.oid = c.relnamespace
		   LEFT JOIN pg_attrdef AS d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		  WHERE n.nspname = 'public'
		    AND c.relkind IN ('r','p','v','m')
		    AND a.attnum > 0
		    AND NOT a.attisdropped`,
		`SELECT 'constraint' AS section,
		        COALESCE(c.relname || '.', '') || con.conname AS name,
		        con.contype::text || '|' || pg_get_constraintdef(con.oid, true) AS definition
		   FROM pg_constraint AS con
		   JOIN pg_namespace AS n ON n.oid = con.connamespace
		   LEFT JOIN pg_class AS c ON c.oid = con.conrelid
		  WHERE n.nspname = 'public'`,
		`SELECT 'index' AS section,
		        table_rel.relname || '.' || index_rel.relname AS name,
		        pg_get_indexdef(index_rel.oid) AS definition
		   FROM pg_index AS idx
		   JOIN pg_class AS index_rel ON index_rel.oid = idx.indexrelid
		   JOIN pg_class AS table_rel ON table_rel.oid = idx.indrelid
		   JOIN pg_namespace AS n ON n.oid = index_rel.relnamespace
		  WHERE n.nspname = 'public'`,
		`SELECT 'function' AS section,
		        p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')' AS name,
		        concat_ws('|',
		            'result=' || pg_get_function_result(p.oid),
		            'language=' || l.lanname,
		            'volatility=' || p.provolatile::text,
		            'parallel=' || p.proparallel::text,
		            'security_definer=' || p.prosecdef::text,
		            'definition=' || pg_get_functiondef(p.oid)
		        ) AS definition
		   FROM pg_proc AS p
		   JOIN pg_namespace AS n ON n.oid = p.pronamespace
		   JOIN pg_language AS l ON l.oid = p.prolang
		  WHERE n.nspname = 'public'`,
		`SELECT 'trigger' AS section,
		        c.relname || '.' || trg.tgname AS name,
		        pg_get_triggerdef(trg.oid, true) AS definition
		   FROM pg_trigger AS trg
		   JOIN pg_class AS c ON c.oid = trg.tgrelid
		   JOIN pg_namespace AS n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public'
		    AND NOT trg.tgisinternal`,
		`SELECT 'policy' AS section,
		        schemaname || '.' || tablename || '.' || policyname AS name,
		        concat_ws('|',
		            permissive,
		            roles::text,
		            cmd,
		            COALESCE(qual, ''),
		            COALESCE(with_check, '')
		        ) AS definition
		   FROM pg_policies
		  WHERE schemaname = 'public'`,
	}

	entries := make([]catalogEntry, 0, 256)
	for _, query := range queries {
		entries = append(entries, readCatalogEntries(t, pool, query)...)
	}
	slices.SortFunc(entries, func(a, b catalogEntry) int {
		left := a.Section + "\x00" + a.Name + "\x00" + a.Definition
		right := b.Section + "\x00" + b.Name + "\x00" + b.Definition
		return strings.Compare(left, right)
	})
	return entries
}

func readCatalogEntries(t *testing.T, pool *pgxpool.Pool, query string) []catalogEntry {
	t.Helper()
	rows, err := pool.Query(t.Context(), query)
	if err != nil {
		t.Fatalf("query catalog manifest: %v\n%s", err, query)
	}
	defer rows.Close()

	var entries []catalogEntry
	for rows.Next() {
		var entry catalogEntry
		if err := rows.Scan(&entry.Section, &entry.Name, &entry.Definition); err != nil {
			t.Fatalf("scan catalog manifest: %v", err)
		}
		entry.Definition = normalizeCatalogDefinition(entry.Definition)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate catalog manifest: %v", err)
	}
	return entries
}

func normalizeCatalogDefinition(definition string) string {
	definition = strings.ReplaceAll(definition, "pg_catalog.", "")
	return strings.Join(strings.Fields(definition), " ")
}

func formatCatalogDiff(t *testing.T, freshCatalog, upgradeCatalog []catalogEntry) string {
	t.Helper()

	type catalogKey struct {
		section string
		name    string
	}

	freshByKey := make(map[catalogKey]catalogEntry, len(freshCatalog))
	upgradeByKey := make(map[catalogKey]catalogEntry, len(upgradeCatalog))
	for _, entry := range freshCatalog {
		freshByKey[catalogKey{section: entry.Section, name: entry.Name}] = entry
	}
	for _, entry := range upgradeCatalog {
		upgradeByKey[catalogKey{section: entry.Section, name: entry.Name}] = entry
	}

	var freshOnly, upgradeOnly, changed []catalogEntry
	for key, freshEntry := range freshByKey {
		upgradeEntry, ok := upgradeByKey[key]
		if !ok {
			freshOnly = append(freshOnly, freshEntry)
			continue
		}
		if freshEntry.Definition != upgradeEntry.Definition {
			changed = append(changed, catalogEntry{
				Section:    freshEntry.Section,
				Name:       freshEntry.Name,
				Definition: "fresh=" + freshEntry.Definition + "\nupgrade=" + upgradeEntry.Definition,
			})
		}
	}
	for key, upgradeEntry := range upgradeByKey {
		if _, ok := freshByKey[key]; !ok {
			upgradeOnly = append(upgradeOnly, upgradeEntry)
		}
	}

	sortCatalogEntries(freshOnly)
	sortCatalogEntries(upgradeOnly)
	sortCatalogEntries(changed)

	const limit = 40
	return strings.Join([]string{
		formatCatalogDiffSection(t, "fresh-only", freshOnly, limit),
		formatCatalogDiffSection(t, "upgrade-only", upgradeOnly, limit),
		formatCatalogDiffSection(t, "changed", changed, limit),
	}, "\n")
}

func sortCatalogEntries(entries []catalogEntry) {
	slices.SortFunc(entries, func(a, b catalogEntry) int {
		left := a.Section + "\x00" + a.Name + "\x00" + a.Definition
		right := b.Section + "\x00" + b.Name + "\x00" + b.Definition
		return strings.Compare(left, right)
	})
}

func formatCatalogDiffSection(t *testing.T, name string, entries []catalogEntry, limit int) string {
	t.Helper()
	total := len(entries)
	if total == 0 {
		return name + " (0):\n  (none)"
	}
	shown := entries
	if total > limit {
		shown = entries[:limit]
	}
	var builder strings.Builder
	_, _ = fmt.Fprintf(&builder, "%s (%d of %d shown):", name, len(shown), total)
	for _, entry := range shown {
		_, _ = fmt.Fprintf(&builder, "\n  - %s %s: %s",
			entry.Section, entry.Name, truncateCatalogDefinition(entry.Definition))
	}
	if total > limit {
		_, _ = fmt.Fprintf(&builder, "\n  ... %d more", total-limit)
	}
	return builder.String()
}

func truncateCatalogDefinition(definition string) string {
	const limit = 240
	if len(definition) <= limit {
		return definition
	}
	return definition[:limit] + "..."
}
