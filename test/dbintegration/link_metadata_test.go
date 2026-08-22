package dbintegration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
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
	setTags(t, pool, linkID, []string{"legacy", "obsolete"})
	setTags(t, pool, neighbourID, []string{"fresh", "related"})

	before, err := links.GetByID(ctx, linkID)
	if err != nil || before == nil {
		t.Fatalf("read fixture link: %#v, %v", before, err)
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
	related, err := reader.RelatedTags(ctx, &linkID, 12)
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
					SET library_kind='site',library_kind_locked=true,summary=NULL,content=NULL,content_document=NULL
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
					SET library_kind=NULL,library_kind_locked=false
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

func TestLinkMetadataRevisionTriggerCoversDirectWriters(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/direct-metadata-writer", "Initial title", "body", "Initial summary")
	before, err := links.GetByID(ctx, linkID)
	if err != nil || before == nil {
		t.Fatalf("read direct-writer fixture: %#v, %v", before, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE links SET title=$2 WHERE id=$1`, linkID, "Direct title"); err != nil {
		t.Fatalf("direct title writer: %v", err)
	}
	afterTitle := linkMetadataReadLink(t, links, ctx, linkID)
	if afterTitle.MetadataRevision != before.MetadataRevision+1 {
		t.Fatalf("revision after direct title = %d, want %d", afterTitle.MetadataRevision, before.MetadataRevision+1)
	}
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

func linkMetadataReadLink(t *testing.T, links *repository.PGXLinkRepository, ctx context.Context, linkID uuid.UUID) *model.Link {
	t.Helper()
	link, err := links.GetByID(ctx, linkID)
	if err != nil || link == nil {
		t.Fatalf("GetByID(%s) = %#v, %v", linkID, link, err)
	}
	return link
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
