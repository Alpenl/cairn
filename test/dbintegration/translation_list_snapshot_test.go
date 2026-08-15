package dbintegration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/contentdoc"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service/linktranslation"
)

func TestTranslationListUsesOneRepeatableReadSnapshot(t *testing.T) {
	pool := StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	linkID := uuid.New()
	oldFullID := uuid.New()
	oldHistoricalSummaryID := uuid.New()
	oldBlockSummaryID := uuid.New()
	newFullID := uuid.New()
	newBlockSummaryID := uuid.New()
	const oldRevision int64 = 7
	const oldDocument = "# Old body"
	const newDocument = "# New body"
	const oldSummary = "Keep **legacy** and modern"
	const newSummary = "Keep **legacy** and modern!"
	const oldRenderedSummary = "Keep legacy and modern"
	const newRenderedSummary = "Keep legacy and modern!"

	if _, err := pool.Exec(ctx, `INSERT INTO links (
		id, url, source_key, status, summary, content_document, content_format,
		library_kind, library_kind_source, content_revision, first_collected_at
	) VALUES ($1, $2, $2, 'done', $3, $4, 'markdown', 'reading', 'user', $5, NOW())`,
		linkID,
		"https://example.com/rf5a-translation-list-snapshot/"+linkID.String(),
		oldSummary,
		oldDocument,
		oldRevision,
	); err != nil {
		t.Fatalf("seed snapshot link: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO link_translations (
		id, link_id, scope, block_key, start_offset, end_offset,
		source_text, translated_text, source_format, target_language, source_hash,
		source_content_revision, status, attempt_generation
	) VALUES
		($1, $4, 'full', 'content-document', 0, 10, $5, 'old full translated', 'markdown', 'zh-CN', $6, $7, 'done', 1),
		($2, $4, 'selection', 'summary', 5, 11, 'legacy', 'legacy translated', 'plain', 'zh-CN', $8, NULL, 'done', 0),
		($3, $4, 'selection', 'summary', 0, 22, $9, 'old block translated', 'plain', 'zh-CN', $10, NULL, 'done', 1)`,
		oldFullID,
		oldHistoricalSummaryID,
		oldBlockSummaryID,
		linkID,
		oldDocument,
		hashTranslationListSource(oldDocument),
		oldRevision,
		hashTranslationListSource("legacy"),
		oldRenderedSummary,
		contentdoc.RenderedSourceBlockPersistenceHash("summary", oldRenderedSummary),
	); err != nil {
		t.Fatalf("seed old snapshot translations: %v", err)
	}

	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin snapshot writer: %v", err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if _, err := writer.Exec(ctx, `LOCK TABLE link_translations IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock translation table: %v", err)
	}
	if _, err := writer.Exec(ctx, `UPDATE links SET
		content=NULL, content_document=$1, content_format='markdown', summary=$2, content_revision=$3
		WHERE id=$4`, newDocument, newSummary, oldRevision+1, linkID); err != nil {
		t.Fatalf("stage new canonical source: %v", err)
	}
	if _, err := writer.Exec(ctx, `INSERT INTO link_translations (
		id, link_id, scope, block_key, start_offset, end_offset,
		source_text, translated_text, source_format, target_language, source_hash,
		source_content_revision, status, attempt_generation
	) VALUES
		($1, $3, 'full', 'content-document', 0, 10, $4, 'new full translated', 'markdown', 'zh-CN', $5, $6, 'done', 1),
		($2, $3, 'selection', 'summary', 16, 22, 'modern', 'new block translated', 'plain', 'zh-CN', $7, NULL, 'done', 1)`,
		newFullID,
		newBlockSummaryID,
		linkID,
		newDocument,
		hashTranslationListSource(newDocument),
		oldRevision+1,
		contentdoc.RenderedSourceBlockPersistenceHash("summary", newRenderedSummary),
	); err != nil {
		t.Fatalf("stage new snapshot translations: %v", err)
	}

	const readerApplication = "webtag_translation_list_snapshot"
	readerPool := openNamedPool(t, readerApplication)
	translations := repository.NewPGXTranslationRepository(readerPool)
	service := linktranslation.NewService(linktranslation.ServiceOptions{Translations: translations})
	type listResult struct {
		list model.TranslationList
		err  error
	}
	firstResult := make(chan listResult, 1)
	go func() {
		list, listErr := service.List(ctx, linkID)
		firstResult <- listResult{list: list, err: listErr}
	}()

	waitForRF3BLockWait(t, ctx, pool, readerApplication, "FROM link_translations")
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit new source while list waits: %v", err)
	}

	var first listResult
	select {
	case first = <-firstResult:
	case <-ctx.Done():
		t.Fatalf("first list did not finish after writer commit: %v", ctx.Err())
	}
	if first.err != nil {
		t.Fatalf("first List() error = %v", first.err)
	}
	assertTranslationListSnapshot(t, first.list, oldRevision, hashTranslationListSource(oldRenderedSummary), map[uuid.UUID]bool{
		oldFullID:              false,
		oldHistoricalSummaryID: false,
		oldBlockSummaryID:      false,
	})

	second, err := service.List(ctx, linkID)
	if err != nil {
		t.Fatalf("second List() error = %v", err)
	}
	assertTranslationListSnapshot(t, second, oldRevision+1, hashTranslationListSource(newRenderedSummary), map[uuid.UUID]bool{
		oldFullID:              true,
		oldHistoricalSummaryID: false,
		oldBlockSummaryID:      true,
		newFullID:              false,
		newBlockSummaryID:      false,
	})
}

func assertTranslationListSnapshot(
	t *testing.T,
	list model.TranslationList,
	wantRevision int64,
	wantSummarySourceHash string,
	wantStale map[uuid.UUID]bool,
) {
	t.Helper()
	if list.CurrentContentRevision != wantRevision {
		t.Fatalf("current content revision = %d, want %d", list.CurrentContentRevision, wantRevision)
	}
	if list.CurrentSummarySourceHash == nil || *list.CurrentSummarySourceHash != wantSummarySourceHash {
		t.Fatalf("current summary source hash = %v, want %q", list.CurrentSummarySourceHash, wantSummarySourceHash)
	}
	if len(list.Items) != len(wantStale) {
		t.Fatalf("translation items = %+v, want IDs/stale %+v", list.Items, wantStale)
	}
	seen := make(map[uuid.UUID]struct{}, len(list.Items))
	for _, item := range list.Items {
		want, ok := wantStale[item.ID]
		if !ok {
			t.Fatalf("unexpected translation %s in snapshot: %+v", item.ID, item)
		}
		if item.Stale != want {
			t.Fatalf("translation %s stale = %v, want %v", item.ID, item.Stale, want)
		}
		seen[item.ID] = struct{}{}
	}
	if len(seen) != len(wantStale) {
		t.Fatalf("snapshot IDs = %+v, want %+v", seen, wantStale)
	}
}

func hashTranslationListSource(source string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
}
