package dbintegration

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/migrate"
)

func TestReaderCategoryCleanupLeavesOnlyTags(t *testing.T) {
	pool := StartPostgres(t)
	assertReaderCategorySchemaRemoved(t, pool)
}

func TestReaderCategoryCleanupFoldsMembershipNamesIntoTags(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	pool := migrationTargetPool(t, dsn)
	prepareProductionUpgradeFixture(t, pool)
	installLegacyReaderCategorySchema(t, pool)

	inboxID, linkID := uuid.New(), uuid.New()
	sharedCategoryID, inboxCategoryID, linkCategoryID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.reader_inbox (id,url,identity_key,tags,metadata_revision)
		VALUES ($1,'https://category.example/inbox','https://category.example/inbox',ARRAY['shared','manual'],4)`, inboxID); err != nil {
		t.Fatalf("seed legacy categorized Inbox: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.links (
			id,url,source_key,status,tags,metadata_revision,first_collected_at
		) VALUES (
			$1,'https://category.example/link','https://category.example/link',
			'done',ARRAY['shared','saved'],7,CURRENT_TIMESTAMP
		)`, linkID); err != nil {
		t.Fatalf("seed legacy categorized Link: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.reader_categories (id,name) VALUES
		($1,'shared'),($2,'inbox-only'),($3,'link-only')`,
		sharedCategoryID, inboxCategoryID, linkCategoryID); err != nil {
		t.Fatalf("seed legacy Reader Categories: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.reader_categorizables (
		category_id,host_kind,host_id
	) VALUES ($1,'inbox',$4),($2,'inbox',$4),($1,'link',$5),($3,'link',$5)`,
		sharedCategoryID, inboxCategoryID, linkCategoryID,
		inboxID.String(), linkID.String()); err != nil {
		t.Fatalf("seed legacy Reader Category memberships: %v", err)
	}

	if err := migrate.Up(t.Context(), pool); err != nil {
		t.Fatalf("apply production schema upgrade: %v", err)
	}
	assertCurrentSchemaLedger(t, pool)
	assertReaderCategorySchemaRemoved(t, pool)

	var inboxTags, linkTags []string
	var inboxRevision, linkRevision int64
	if err := pool.QueryRow(t.Context(), `SELECT tags,metadata_revision
		FROM public.reader_inbox WHERE id=$1`, inboxID).Scan(&inboxTags, &inboxRevision); err != nil {
		t.Fatalf("read migrated Inbox tags: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT tags,metadata_revision
		FROM public.links WHERE id=$1`, linkID).Scan(&linkTags, &linkRevision); err != nil {
		t.Fatalf("read migrated Link tags: %v", err)
	}
	if want := []string{"inbox-only", "manual", "shared"}; !slices.Equal(inboxTags, want) {
		t.Fatalf("migrated Inbox tags = %v, want %v", inboxTags, want)
	}
	if want := []string{"link-only", "saved", "shared"}; !slices.Equal(linkTags, want) {
		t.Fatalf("migrated Link tags = %v, want %v", linkTags, want)
	}
	if inboxRevision != 5 || linkRevision != 8 {
		t.Fatalf("migrated metadata revisions = Inbox:%d Link:%d, want 5/8",
			inboxRevision, linkRevision)
	}
}

func TestReaderCategoryCleanupRejectsUnsupportedHostKinds(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	pool := migrationTargetPool(t, dsn)
	prepareProductionUpgradeFixture(t, pool)
	installLegacyReaderCategorySchema(t, pool)

	categoryID, noteID := uuid.New(), uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.reader_notes (id,title)
		VALUES ($1,'legacy categorized note')`, noteID); err != nil {
		t.Fatalf("seed legacy categorized Note: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.reader_categories (id,name)
		VALUES ($1,'note-only')`, categoryID); err != nil {
		t.Fatalf("seed legacy Note Category: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.reader_categorizables (
		category_id,host_kind,host_id
	) VALUES ($1,'note',$2)`, categoryID, noteID.String()); err != nil {
		t.Fatalf("seed unsupported Reader Category membership: %v", err)
	}

	err := migrate.Up(t.Context(), pool)
	if err == nil {
		t.Fatal("Reader Category simplification unexpectedly accepted a Note membership")
	}
	if !strings.Contains(err.Error(), "unsupported host kind") {
		t.Fatalf("Reader Category migration error = %q, want unsupported host kind", err)
	}
	assertProductionBaselineLedger(t, pool)

	var categories, memberships int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM public.reader_categories),
		(SELECT count(*) FROM public.reader_categorizables)`).Scan(&categories, &memberships); err != nil {
		t.Fatalf("read rolled-back Reader Category state: %v", err)
	}
	if categories != 1 || memberships != 1 {
		t.Fatalf("Reader Category rollback retained categories/memberships = %d/%d, want 1/1",
			categories, memberships)
	}
}

func installLegacyReaderCategorySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		CREATE TABLE public.reader_categories (
			id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
			name text NOT NULL UNIQUE,
			created_at timestamptz DEFAULT now() NOT NULL
		);
		CREATE TABLE public.reader_categorizables (
			category_id uuid NOT NULL REFERENCES public.reader_categories(id) ON DELETE CASCADE,
			host_kind text NOT NULL,
			host_id text NOT NULL,
			PRIMARY KEY (category_id,host_kind,host_id)
		)`); err != nil {
		t.Fatalf("install legacy Reader Category schema: %v", err)
	}
}

func assertReaderCategorySchemaRemoved(t *testing.T, pool migrationRowQuerier) {
	t.Helper()
	var tables int
	if err := pool.QueryRow(t.Context(), `SELECT count(*)
		FROM information_schema.tables
		WHERE table_schema='public'
		  AND table_name IN ('reader_categories','reader_categorizables')`).Scan(&tables); err != nil {
		t.Fatalf("inspect Reader Category schema: %v", err)
	}
	if tables != 0 {
		t.Fatalf("obsolete Reader Category tables = %d, want zero", tables)
	}
}
