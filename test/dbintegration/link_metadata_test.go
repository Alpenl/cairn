package dbintegration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgvector/pgvector-go"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

const (
	readerMetadataActivityBlockClass = 70_070
	readerMetadataActivityBlockKey   = 1
)

func TestReaderLinkMetadataCASReplacesImmediateProjections(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	links := repository.NewPGXLinkRepository(pool)
	read := service.NewLinkReadService(service.LinkReadServiceOptions{Links: links})
	tags := repository.NewPGXTagRepository(pool)

	linkID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/metadata-cas", "Before metadata", "metadata body", "Before summary")
	neighbourID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/metadata-neighbour", "Neighbour", "neighbour body", "Neighbour summary")
	siblingID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/metadata-sibling", "Sibling", "sibling body", "Sibling summary")
	setTags(t, pool, linkID, []string{"legacy", "obsolete"})
	setTags(t, pool, neighbourID, []string{"fresh", "related"})
	setTags(t, pool, siblingID, []string{"sibling"})

	conceptID := uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO concept
		(id,primary_name,display_name)
		VALUES ($1,'metadata-concept','Legacy Surface')`, conceptID); err != nil {
		t.Fatalf("insert metadata concept: %v", err)
	}
	for _, edge := range []struct {
		linkID     uuid.UUID
		surfaceTag string
	}{
		{linkID: linkID, surfaceTag: "Legacy Surface"},
		{linkID: siblingID, surfaceTag: "Sibling Surface"},
	} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO link_concept (link_id,concept_id,surface_tag) VALUES ($1,$2,$3)`, edge.linkID, conceptID, edge.surfaceTag); err != nil {
			t.Fatalf("attach metadata concept to %s: %v", edge.linkID, err)
		}
	}

	before, err := links.GetByID(ctx, linkID)
	if err != nil || before == nil {
		t.Fatalf("read fixture link: %#v, %v", before, err)
	}
	vector := linkMetadataVector(1)
	if _, err := pool.Exec(t.Context(), `UPDATE links SET embedding=$2,embedding_model='pre-metadata' WHERE id=$1`, linkID, pgvector.NewVector(vector)); err != nil {
		t.Fatalf("seed pre-metadata embedding: %v", err)
	}

	newTitle := "Revised metadata title"
	newSummary := "Revised metadata summary phrase"
	newTags := []string{"fresh", "replacement"}
	updated, err := reader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
		LinkID:           linkID,
		Title:            &newTitle,
		Summary:          &newSummary,
		Tags:             newTags,
		ExpectedRevision: before.MetadataRevision,
	})
	if err != nil {
		t.Fatalf("UpdateLinkMetadata changed tuple: %v", err)
	}
	if updated.MetadataRevision != before.MetadataRevision+1 || !updated.TagsChanged {
		t.Fatalf("metadata update result = %#v, want revision %d with tags changed", updated, before.MetadataRevision+1)
	}

	// The trigger invalidates semantic vectors immediately, and a background
	// backfill computed from the old revision must not restore it.
	if applied, err := links.UpdateLinkEmbedding(ctx, linkID, before.MetadataRevision, vector, "stale-backfill"); err != nil {
		t.Fatalf("stale embedding write returned error: %v", err)
	} else if applied {
		t.Fatal("stale embedding write applied = true, want false")
	}
	assertLinkMetadataEmbeddingNil(t, pool, linkID)

	after, err := links.GetByID(ctx, linkID)
	if err != nil || after == nil {
		t.Fatalf("read updated link: %#v, %v", after, err)
	}
	if after.Title == nil || *after.Title != newTitle || after.Summary == nil || *after.Summary != newSummary || !slices.Equal(after.Tags, newTags) || after.MetadataRevision != updated.MetadataRevision {
		t.Fatalf("updated row = %#v, want title/summary/tags/revision replacement", after)
	}

	detail, err := read.GetWithContent(ctx, linkID.String(), false)
	if err != nil {
		t.Fatalf("read detail projection: %v", err)
	}
	if detail.Title == nil || *detail.Title != newTitle || detail.Summary == nil || *detail.Summary != newSummary || !slices.Equal(detail.Tags, newTags) || detail.MetadataRevision != updated.MetadataRevision {
		t.Fatalf("detail projection = %#v", detail)
	}
	list, err := read.List(ctx, dto.ListLinksRequest{Limit: 20})
	if err != nil {
		t.Fatalf("read list projection: %v", err)
	}
	assertLinkMetadataResponse(t, list.Items, linkID, newTitle, newSummary, newTags, updated.MetadataRevision, "list")
	search, err := read.List(ctx, dto.ListLinksRequest{Query: "revised metadata summary"})
	if err != nil {
		t.Fatalf("search updated metadata: %v", err)
	}
	assertLinkMetadataResponse(t, search.Items, linkID, newTitle, newSummary, newTags, updated.MetadataRevision, "search")

	counts, err := tags.ListCounts(ctx)
	if err != nil {
		t.Fatalf("tag counts after replacement: %v", err)
	}
	if linkMetadataCount(counts, "legacy") != 0 || linkMetadataCount(counts, "obsolete") != 0 || linkMetadataCount(counts, "replacement") != 1 || linkMetadataCount(counts, "fresh") != 2 {
		t.Fatalf("tag counts = %#v, want old tags removed and replacement counted", counts)
	}
	related, _, _, err := reader.RelatedTags(ctx, &linkID, 12)
	if err != nil {
		t.Fatalf("related tags after replacement: %v", err)
	}
	if !slices.Contains(related, "related") || slices.Contains(related, "legacy") || slices.Contains(related, "fresh") {
		t.Fatalf("related tags = %#v, want only non-seed neighbours from the replacement tuple", related)
	}
	activityPage, err := reader.ListActivity(ctx, model.ReaderActivityQuery{
		Kind:  model.ReaderActivityKindTag,
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("tag activity after replacement: %v", err)
	}
	if !linkMetadataActivityContains(activityPage.Items, "replacement") || linkMetadataActivityContains(activityPage.Items, "legacy") || linkMetadataActivityContains(activityPage.Items, "obsolete") {
		t.Fatalf("tag activity = %#v, want replacement tags and no stale entries", activityPage.Items)
	}

	var displayName string
	if err := pool.QueryRow(t.Context(), `SELECT display_name FROM concept WHERE id=$1`, conceptID).Scan(&displayName); err != nil {
		t.Fatalf("read repaired concept display name: %v", err)
	}
	if displayName != "Sibling Surface" {
		t.Fatalf("concept display_name = %q, want remaining sibling surface", displayName)
	}
	var remainingEdges int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM link_concept WHERE link_id=$1`, linkID).Scan(&remainingEdges); err != nil {
		t.Fatalf("count cleared metadata concept edges: %v", err)
	}
	if remainingEdges != 0 {
		t.Fatalf("link_concept rows after tag replacement = %d, want 0", remainingEdges)
	}

	if _, err := reader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
		LinkID:           linkID,
		Title:            &newTitle,
		Summary:          &newSummary,
		Tags:             newTags,
		ExpectedRevision: before.MetadataRevision,
	}); !errors.Is(err, repository.ErrRevisionConflict) {
		t.Fatalf("stale UpdateLinkMetadata error = %v, want ErrRevisionConflict", err)
	}
	staleCheck, err := links.GetByID(ctx, linkID)
	if err != nil || staleCheck == nil || staleCheck.MetadataRevision != updated.MetadataRevision || !slices.Equal(staleCheck.Tags, newTags) {
		t.Fatalf("stale CAS changed row = %#v, %v", staleCheck, err)
	}

	noOp, err := reader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
		LinkID:           linkID,
		Title:            &newTitle,
		Summary:          &newSummary,
		Tags:             newTags,
		ExpectedRevision: updated.MetadataRevision,
	})
	if err != nil {
		t.Fatalf("identical metadata retry: %v", err)
	}
	if noOp.MetadataRevision != updated.MetadataRevision || noOp.TagsChanged {
		t.Fatalf("no-op result = %#v, want unchanged revision and tags flag", noOp)
	}
}

func TestLinkMetadataRevisionStopsAtJavaScriptSafeMaximum(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	links := repository.NewPGXLinkRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/metadata-safe-maximum", "Original metadata", "body", "Original summary")
	setTags(t, pool, linkID, []string{"original"})

	if _, err := pool.Exec(t.Context(), `UPDATE links SET metadata_revision=$2 WHERE id=$1`, linkID, model.LinkMetadataMaxRevision); err != nil {
		t.Fatalf("set metadata revision to JavaScript-safe maximum: %v", err)
	}
	atCeiling := linkMetadataReadLink(t, links, ctx, linkID)
	if atCeiling.MetadataRevision != model.LinkMetadataMaxRevision {
		t.Fatalf("metadata revision = %d, want %d", atCeiling.MetadataRevision, model.LinkMetadataMaxRevision)
	}

	assertSafeRevisionFailure := func(action string, err error, wantMessage string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s succeeded, want the metadata revision safety constraint", action)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("%s error = %T %v, want PostgreSQL constraint error", action, err, err)
		}
		if pgErr.Code != "23514" || pgErr.ConstraintName != "chk_links_metadata_revision_safe" {
			t.Fatalf("%s pg error = code:%q constraint:%q, want 23514/chk_links_metadata_revision_safe", action, pgErr.Code, pgErr.ConstraintName)
		}
		if wantMessage != "" && !strings.Contains(pgErr.Message, wantMessage) {
			t.Fatalf("%s pg error message = %q, want substring %q", action, pgErr.Message, wantMessage)
		}
	}

	_, err := pool.Exec(t.Context(), `UPDATE links SET title=$2 WHERE id=$1`, linkID, "Direct writer must stop")
	assertSafeRevisionFailure("direct metadata update at the maximum", err, "reached the JavaScript-safe maximum")
	afterDirect := linkMetadataReadLink(t, links, ctx, linkID)
	if afterDirect.MetadataRevision != model.LinkMetadataMaxRevision || !sameLinkMetadataTuple(afterDirect, atCeiling) {
		t.Fatalf("direct metadata writer changed a safe-ceiling row: before=%#v after=%#v", atCeiling, afterDirect)
	}

	replacementTitle := "Repository writer must stop"
	_, err = reader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
		LinkID:           linkID,
		Title:            &replacementTitle,
		Summary:          atCeiling.Summary,
		Tags:             append([]string(nil), atCeiling.Tags...),
		ExpectedRevision: model.LinkMetadataMaxRevision,
	})
	if !errors.Is(err, repository.ErrRevisionConflict) {
		t.Fatalf("changed safe-ceiling UpdateLinkMetadata() error = %v, want ErrRevisionConflict", err)
	}
	afterCAS := linkMetadataReadLink(t, links, ctx, linkID)
	if afterCAS.MetadataRevision != model.LinkMetadataMaxRevision || !sameLinkMetadataTuple(afterCAS, atCeiling) {
		t.Fatalf("repository metadata writer changed a safe-ceiling row: before=%#v after=%#v", atCeiling, afterCAS)
	}

	noOp, err := reader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
		LinkID:           linkID,
		Title:            atCeiling.Title,
		Summary:          atCeiling.Summary,
		Tags:             append([]string(nil), atCeiling.Tags...),
		ExpectedRevision: model.LinkMetadataMaxRevision,
	})
	if err != nil {
		t.Fatalf("identical safe-ceiling UpdateLinkMetadata(): %v", err)
	}
	if noOp.MetadataRevision != model.LinkMetadataMaxRevision || noOp.TagsChanged {
		t.Fatalf("safe-ceiling no-op = %#v, want retained maximum revision", noOp)
	}

	_, err = pool.Exec(t.Context(), `UPDATE links SET metadata_revision=$2 WHERE id=$1`, linkID, model.LinkMetadataMaxRevision+1)
	assertSafeRevisionFailure("direct oversized metadata revision", err, "")
	afterOversized := linkMetadataReadLink(t, links, ctx, linkID)
	if afterOversized.MetadataRevision != model.LinkMetadataMaxRevision || !sameLinkMetadataTuple(afterOversized, atCeiling) {
		t.Fatalf("oversized metadata revision changed row: before=%#v after=%#v", atCeiling, afterOversized)
	}
}

func TestUpdateLinkMetadataRejectsNonEligibleLinksWithoutWrite(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	links := repository.NewPGXLinkRepository(pool)
	cases := []struct {
		name  string
		apply func(uuid.UUID) error
	}{
		{
			name: "pending reading link",
			apply: func(linkID uuid.UUID) error {
				_, err := pool.Exec(t.Context(), `UPDATE links SET status='pending' WHERE id=$1`, linkID)
				return err
			},
		},
		{
			name: "confirmed site link",
			apply: func(linkID uuid.UUID) error {
				_, err := pool.Exec(t.Context(), `UPDATE links
					SET library_kind='site',library_kind_source='user',summary=NULL,content=NULL,content_document=NULL
					WHERE id=$1`, linkID)
				return err
			},
		},
		{
			name: "processing reading link",
			apply: func(linkID uuid.UUID) error {
				_, err := pool.Exec(t.Context(), `UPDATE links SET status='processing' WHERE id=$1`, linkID)
				return err
			},
		},
		{
			name: "unclassified done link",
			apply: func(linkID uuid.UUID) error {
				_, err := pool.Exec(t.Context(), `UPDATE links
					SET library_kind=NULL,library_kind_source=NULL
					WHERE id=$1`, linkID)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			linkID := seedReaderVNextSavedLink(t, pool,
				"https://reader-vnext.example/metadata-ineligible-"+uuid.NewString(),
				"Original metadata", "body", "Original summary")
			setTags(t, pool, linkID, []string{"original"})
			if err := tc.apply(linkID); err != nil {
				t.Fatalf("prepare non-eligible fixture: %v", err)
			}
			before := linkMetadataReadLink(t, links, ctx, linkID)

			title := "Rejected replacement"
			summary := "Rejected replacement summary"
			_, err := reader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
				LinkID:           linkID,
				Title:            &title,
				Summary:          &summary,
				Tags:             []string{"rejected"},
				ExpectedRevision: before.MetadataRevision,
			})
			if !errors.Is(err, repository.ErrRevisionConflict) {
				t.Fatalf("UpdateLinkMetadata() error = %v, want ErrRevisionConflict", err)
			}

			after := linkMetadataReadLink(t, links, ctx, linkID)
			if after.MetadataRevision != before.MetadataRevision || !sameLinkMetadataTuple(after, before) {
				t.Fatalf("non-eligible metadata write changed row: before=%#v after=%#v", before, after)
			}
		})
	}
}

func TestUpdateLinkMetadataEligibilityRaceRejectsWithoutWrite(t *testing.T) {
	pool := StartPostgres(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	links := repository.NewPGXLinkRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/metadata-eligibility-race", "Original metadata", "body", "Original summary")
	setTags(t, pool, linkID, []string{"original"})
	before := linkMetadataReadLink(t, links, ctx, linkID)

	transition, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin eligibility transition: %v", err)
	}
	defer func() { _ = transition.Rollback(context.Background()) }()
	var lockedID uuid.UUID
	if err := transition.QueryRow(ctx, `SELECT id FROM links WHERE id=$1 FOR UPDATE`, linkID).Scan(&lockedID); err != nil {
		t.Fatalf("lock eligibility fixture: %v", err)
	}
	if _, err := transition.Exec(ctx, `UPDATE links SET status='processing' WHERE id=$1`, linkID); err != nil {
		t.Fatalf("transition link out of confirmed reading: %v", err)
	}

	applicationName := "webtag_metadata_eligibility_" + uuid.NewString()
	casPool := openNamedPool(t, applicationName)
	casReader := repository.NewPGXReaderVNextRepository(casPool)
	title := "Rejected race replacement"
	summary := "Rejected race summary"
	result := make(chan error, 1)
	go func() {
		_, updateErr := casReader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
			LinkID:           linkID,
			Title:            &title,
			Summary:          &summary,
			Tags:             []string{"rejected-race"},
			ExpectedRevision: before.MetadataRevision,
		})
		result <- updateErr
	}()
	waitForPostgresLock(t, ctx, pool, applicationName)

	if err := transition.Commit(ctx); err != nil {
		t.Fatalf("commit eligibility transition: %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, repository.ErrRevisionConflict) {
			t.Fatalf("racing UpdateLinkMetadata() error = %v, want ErrRevisionConflict", err)
		}
	case <-ctx.Done():
		t.Fatalf("racing UpdateLinkMetadata did not finish: %v", ctx.Err())
	}

	after := linkMetadataReadLink(t, links, ctx, linkID)
	if after.Status != model.LinkStatusProcessing || after.MetadataRevision != before.MetadataRevision || !sameLinkMetadataTuple(after, before) {
		t.Fatalf("eligibility race changed metadata: before=%#v after=%#v", before, after)
	}
}

func TestRefreshActivityAndMetadataCASDoNotRestoreStaleTagActivity(t *testing.T) {
	pool := StartPostgres(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	legacyTag := "legacy-activity-race"
	replacementTag := "replacement-activity-race"
	linkID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/metadata-activity-race", "Original metadata", "body", "Original summary")
	setTags(t, pool, linkID, []string{legacyTag})
	initialReader := repository.NewPGXReaderVNextRepository(pool)
	if err := initialReader.RefreshActivity(ctx); err != nil {
		t.Fatalf("seed legacy activity: %v", err)
	}
	links := repository.NewPGXLinkRepository(pool)
	before := linkMetadataReadLink(t, links, ctx, linkID)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		DROP TRIGGER IF EXISTS webtag_test_block_legacy_reader_activity ON reader_tag_activity;
		DROP FUNCTION IF EXISTS webtag_test_block_legacy_reader_activity();
		CREATE FUNCTION webtag_test_block_legacy_reader_activity() RETURNS trigger
		LANGUAGE plpgsql AS $fn$
		BEGIN
			IF NEW.tag = $tag$%s$tag$ THEN
				PERFORM pg_advisory_xact_lock(%d, %d);
			END IF;
			RETURN NEW;
		END
		$fn$;
		CREATE TRIGGER webtag_test_block_legacy_reader_activity
		BEFORE INSERT OR UPDATE ON reader_tag_activity
		FOR EACH ROW EXECUTE FUNCTION webtag_test_block_legacy_reader_activity()`, legacyTag, readerMetadataActivityBlockClass, readerMetadataActivityBlockKey)); err != nil {
		t.Fatalf("install activity race blocker: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS webtag_test_block_legacy_reader_activity ON reader_tag_activity;
			DROP FUNCTION IF EXISTS webtag_test_block_legacy_reader_activity()`)
	})

	gateConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire activity race gate: %v", err)
	}
	if _, err := gateConn.Exec(ctx, `SELECT pg_advisory_lock($1,$2)`, readerMetadataActivityBlockClass, readerMetadataActivityBlockKey); err != nil {
		gateConn.Release()
		t.Fatalf("hold activity race gate: %v", err)
	}
	gateReleased := false
	releaseGate := func() {
		if gateReleased {
			return
		}
		gateReleased = true
		if _, err := gateConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1,$2)`, readerMetadataActivityBlockClass, readerMetadataActivityBlockKey); err != nil {
			t.Errorf("release activity race gate: %v", err)
		}
		gateConn.Release()
	}
	t.Cleanup(releaseGate)

	refreshApplication := "webtag_activity_refresh_" + uuid.NewString()
	metadataApplication := "webtag_metadata_activity_" + uuid.NewString()
	refreshPool := openNamedPool(t, refreshApplication)
	metadataPool := openNamedPool(t, metadataApplication)
	refreshReader := repository.NewPGXReaderVNextRepository(refreshPool)
	metadataReader := repository.NewPGXReaderVNextRepository(metadataPool)
	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- refreshReader.RefreshActivity(ctx)
	}()
	waitForPostgresLock(t, ctx, pool, refreshApplication)

	title := "Replaced while activity refresh is stale"
	summary := "Replaced activity summary"
	metadataDone := make(chan error, 1)
	go func() {
		_, updateErr := metadataReader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
			LinkID:           linkID,
			Title:            &title,
			Summary:          &summary,
			Tags:             []string{replacementTag},
			ExpectedRevision: before.MetadataRevision,
		})
		metadataDone <- updateErr
	}()
	waitForPostgresLock(t, ctx, pool, metadataApplication)

	releaseGate()
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatalf("stale RefreshActivity(): %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("stale RefreshActivity did not finish: %v", ctx.Err())
	}
	select {
	case err := <-metadataDone:
		if err != nil {
			t.Fatalf("racing UpdateLinkMetadata(): %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("racing UpdateLinkMetadata did not finish: %v", ctx.Err())
	}

	activityPage, err := initialReader.ListActivity(ctx, model.ReaderActivityQuery{
		Kind:  model.ReaderActivityKindTag,
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("list activity after race: %v", err)
	}
	if !linkMetadataActivityContains(activityPage.Items, replacementTag) || linkMetadataActivityContains(activityPage.Items, legacyTag) {
		t.Fatalf("activity after metadata race = %#v, want replacement without legacy", activityPage.Items)
	}
}

func TestLinkMetadataRevisionTriggerCoversDirectWritersAndStaleBackfill(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/direct-metadata-writer", "Initial title", "body", "Initial summary")
	before, err := links.GetByID(ctx, linkID)
	if err != nil || before == nil {
		t.Fatalf("read direct-writer fixture: %#v, %v", before, err)
	}
	vector := linkMetadataVector(2)
	if _, err := pool.Exec(t.Context(), `UPDATE links SET embedding=$2,embedding_model='old-model' WHERE id=$1`, linkID, pgvector.NewVector(vector)); err != nil {
		t.Fatalf("seed direct-writer embedding: %v", err)
	}

	if _, err := pool.Exec(t.Context(), `UPDATE links SET title=$2 WHERE id=$1`, linkID, "Direct title"); err != nil {
		t.Fatalf("direct title writer: %v", err)
	}
	afterTitle := linkMetadataReadLink(t, links, ctx, linkID)
	if afterTitle.MetadataRevision != before.MetadataRevision+1 {
		t.Fatalf("revision after direct title = %d, want %d", afterTitle.MetadataRevision, before.MetadataRevision+1)
	}
	assertLinkMetadataEmbeddingNil(t, pool, linkID)
	if applied, err := links.UpdateLinkEmbedding(ctx, linkID, before.MetadataRevision, vector, "stale-model"); err != nil {
		t.Fatalf("stale direct-writer backfill returned error: %v", err)
	} else if applied {
		t.Fatal("stale direct-writer backfill applied = true, want false")
	}
	assertLinkMetadataEmbeddingNil(t, pool, linkID)

	if _, err := pool.Exec(t.Context(), `UPDATE links SET summary=$2 WHERE id=$1`, linkID, "Direct summary"); err != nil {
		t.Fatalf("direct summary writer: %v", err)
	}
	afterSummary := linkMetadataReadLink(t, links, ctx, linkID)
	if afterSummary.MetadataRevision != afterTitle.MetadataRevision+1 {
		t.Fatalf("revision after direct summary = %d, want %d", afterSummary.MetadataRevision, afterTitle.MetadataRevision+1)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE links SET tags=$2 WHERE id=$1`, linkID, []string{"direct-tag"}); err != nil {
		t.Fatalf("direct tags writer: %v", err)
	}
	afterTags := linkMetadataReadLink(t, links, ctx, linkID)
	if afterTags.MetadataRevision != afterSummary.MetadataRevision+1 || !slices.Equal(afterTags.Tags, []string{"direct-tag"}) {
		t.Fatalf("row after direct tags = %#v", afterTags)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE links SET description=$2 WHERE id=$1`, linkID, "not metadata"); err != nil {
		t.Fatalf("direct non-metadata writer: %v", err)
	}
	afterNonMetadata := linkMetadataReadLink(t, links, ctx, linkID)
	if afterNonMetadata.MetadataRevision != afterTags.MetadataRevision {
		t.Fatalf("revision after non-metadata write = %d, want %d", afterNonMetadata.MetadataRevision, afterTags.MetadataRevision)
	}
}

func TestParseEmbeddingCannotRestoreVectorAfterMetadataTitleReplacement(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)
	reader := repository.NewPGXReaderVNextRepository(pool)

	link, attempt, err := links.SubmitNew(ctx, repository.CreateLinkParams{
		URL:                        "https://reader-vnext.example/metadata-parse-race",
		Status:                     model.LinkStatusPending,
		RequestedLibraryKind:       model.RequestedLibraryKindReading,
		RequestedLibraryKindSource: model.RequestedLibraryKindSourceAuto,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}
	oldTitle := "Parser title"
	completeEmbeddingAttempt(t, links, link.ID, attempt.ID, attempt.ExpectedMetadataRevision, oldTitle)
	before := linkMetadataReadLink(t, links, ctx, link.ID)
	newTitle := "User replacement title"
	if _, err := reader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
		LinkID:           link.ID,
		Title:            &newTitle,
		Summary:          nil,
		Tags:             []string{},
		ExpectedRevision: before.MetadataRevision,
	}); err != nil {
		t.Fatalf("UpdateLinkMetadata title replacement: %v", err)
	}

	if err := links.UpdateLinkEmbeddingForParse(ctx, link.ID, attempt.ID, &oldTitle, nil, linkMetadataVector(3), "late-parser"); err != nil {
		t.Fatalf("late parser embedding write returned error: %v", err)
	}
	assertLinkMetadataEmbeddingNil(t, pool, link.ID)
}

func linkMetadataReadLink(t *testing.T, links *repository.PGXLinkRepository, ctx context.Context, linkID uuid.UUID) *model.Link {
	t.Helper()
	link, err := links.GetByID(ctx, linkID)
	if err != nil || link == nil {
		t.Fatalf("GetByID(%s) = %#v, %v", linkID, link, err)
	}
	return link
}

func linkMetadataVector(first float32) []float32 {
	vector := make([]float32, 1536)
	vector[0] = first
	return vector
}

func assertLinkMetadataEmbeddingNil(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, linkID uuid.UUID) {
	t.Helper()
	var embedding, modelName *string
	if err := pool.QueryRow(t.Context(), `SELECT embedding::text,embedding_model FROM links WHERE id=$1`, linkID).Scan(&embedding, &modelName); err != nil {
		t.Fatalf("read embedding state: %v", err)
	}
	if embedding != nil || modelName != nil {
		t.Fatalf("embedding state = vector=%v model=%v, want both NULL", embedding, modelName)
	}
}

func assertLinkMetadataResponse(t *testing.T, items []dto.LinkResponse, linkID uuid.UUID, title, summary string, tags []string, revision int64, surface string) {
	t.Helper()
	for _, item := range items {
		if item.ID != linkID.String() {
			continue
		}
		if item.Title == nil || *item.Title != title || item.Summary == nil || *item.Summary != summary || !slices.Equal(item.Tags, tags) || item.MetadataRevision != revision {
			t.Fatalf("%s item = %#v, want metadata replacement", surface, item)
		}
		return
	}
	t.Fatalf("%s items = %#v, want link %s", surface, items, linkID)
}

func linkMetadataCount(counts []repository.TagCount, tag string) int {
	for _, count := range counts {
		if count.Tag == tag {
			return count.Count
		}
	}
	return 0
}

func linkMetadataActivityContains(items []model.ReaderActivity, tag string) bool {
	for _, item := range items {
		if item.Kind == model.ReaderActivityKindTag && item.Key == tag {
			return true
		}
	}
	return false
}

func sameLinkMetadataTuple(got, want *model.Link) bool {
	return sameOptionalLinkMetadataString(got.Title, want.Title) &&
		sameOptionalLinkMetadataString(got.Summary, want.Summary) &&
		slices.Equal(got.Tags, want.Tags)
}

func sameOptionalLinkMetadataString(got, want *string) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}
