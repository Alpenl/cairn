package dbintegration

import (
	"testing"

	"github.com/google/uuid"

	"webtag/internal/migrate"
)

func TestSiteProvenanceCleanupLeavesOnlySiteFacts(t *testing.T) {
	pool := StartPostgres(t)
	assertSiteProvenanceRemoved(t, pool)
}

func TestSiteProvenanceCleanupMigratesLegacyRows(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	pool := migrationTargetPool(t, dsn)
	prepareProductionUpgradeFixture(t, pool)

	siteID, linkID, entryID := uuid.New(), uuid.New(), uuid.New()
	seeds := []struct {
		name string
		sql  string
		args []any
	}{
		{"link", `INSERT INTO public.links (
			id,url,source_key,status,library_kind,library_kind_source,library_kind_locked,first_collected_at
		) VALUES ($1,'https://example.com/docs','https://example.com/docs','done','site','user',true,now())`, []any{linkID}},
		{"site", `INSERT INTO public.sites (
			id,site_key,name,name_source,intro,intro_source,homepage_url,homepage_source,
			icon_url,icon_source,user_note,primary_source,grouping_locked
		) VALUES (
			$1,'v1:host:example.com','Example','user','Useful','auto',
			'https://example.com','migration','https://example.com/icon.png','user',
			'keep this note','user',true
		)`, []any{siteID}},
		{"entry", `INSERT INTO public.site_entries (
			id,site_id,link_id,entry_name,entry_name_source,purpose,purpose_source,normalized_url
		) VALUES ($1,$2,$3,'Docs','user','Reference','auto','https://example.com/docs')`, []any{entryID, siteID, linkID}},
		{"primary entry", `UPDATE public.sites SET primary_entry_id=$1 WHERE id=$2`, []any{entryID, siteID}},
		{"tag", `INSERT INTO public.site_tags (site_id,tag,normalized_tag,source)
		VALUES ($1,'Go','go','auto')`, []any{siteID}},
		{"identity", `INSERT INTO public.site_identities (identity_key,site_id,source,locked)
		VALUES ('v1:host:example.com',$1,'manual_merge',true)`, []any{siteID}},
	}
	for _, seed := range seeds {
		if _, err := pool.Exec(t.Context(), seed.sql, seed.args...); err != nil {
			t.Fatalf("seed legacy Site %s: %v", seed.name, err)
		}
	}

	if err := migrate.Up(t.Context(), pool); err != nil {
		t.Fatalf("apply production schema upgrade: %v", err)
	}
	assertCurrentSchemaLedger(t, pool)
	assertSiteProvenanceRemoved(t, pool)

	var name, intro, homepage, icon, note, entryName, purpose, tag, identity string
	var primaryID uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT name,intro,homepage_url,icon_url,user_note,primary_entry_id
		FROM public.sites WHERE id=$1`, siteID).Scan(&name, &intro, &homepage, &icon, &note, &primaryID); err != nil {
		t.Fatalf("read migrated Site facts: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT entry_name,purpose FROM public.site_entries WHERE id=$1`, entryID).
		Scan(&entryName, &purpose); err != nil {
		t.Fatalf("read migrated Site entry: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT tag FROM public.site_tags WHERE site_id=$1 AND normalized_tag='go'`, siteID).
		Scan(&tag); err != nil {
		t.Fatalf("read migrated Site tag: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT identity_key FROM public.site_identities WHERE site_id=$1`, siteID).
		Scan(&identity); err != nil {
		t.Fatalf("read migrated Site identity: %v", err)
	}
	if name != "Example" || intro != "Useful" || homepage != "https://example.com" ||
		icon != "https://example.com/icon.png" || note != "keep this note" || primaryID != entryID ||
		entryName != "Docs" || purpose != "Reference" || tag != "Go" || identity != "v1:host:example.com" {
		t.Fatalf("migrated Site facts changed: name=%q intro=%q homepage=%q icon=%q note=%q primary=%s entry=%q purpose=%q tag=%q identity=%q",
			name, intro, homepage, icon, note, primaryID, entryName, purpose, tag, identity)
	}
}

func assertSiteProvenanceRemoved(t *testing.T, pool migrationRowQuerier) {
	t.Helper()
	var columns, constraints int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM information_schema.columns
		 WHERE table_schema='public' AND (
			(table_name='sites' AND column_name IN (
				'name_source','intro_source','homepage_source','icon_source','primary_source','grouping_locked')) OR
			(table_name='site_entries' AND column_name IN ('entry_name_source','purpose_source')) OR
			(table_name='site_tags' AND column_name='source') OR
			(table_name='site_identities' AND column_name IN ('source','locked')))),
		(SELECT count(*) FROM information_schema.table_constraints
		 WHERE constraint_schema='public' AND constraint_name IN (
			'chk_sites_optional_sources','chk_sites_sources','chk_site_entries_sources',
			'chk_site_tags_source','chk_site_identities_source'))`).Scan(&columns, &constraints); err != nil {
		t.Fatalf("inspect Site provenance schema: %v", err)
	}
	if columns != 0 || constraints != 0 {
		t.Fatalf("obsolete Site provenance remains: columns=%d constraints=%d", columns, constraints)
	}
}
