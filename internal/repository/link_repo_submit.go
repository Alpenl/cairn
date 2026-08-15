package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/model"
)

// insertLinkBatchColumnCount is the number of value placeholders emitted
// per row by buildBatchInsertLinksSQL. It must stay in lock-step with
// insertLinkSQL's column list (url, source_kind, source_key,
// input_title, input_text, input_html, input_images, source_metadata,
// description, status, domain, content_type, requested_library_kind,
// requested_library_kind_source, library_kind, library_kind_source,
// library_kind_locked, predicted_library_kind, path_depth, parent_path,
// parent_id — 21 bound columns; content_source / first_collected_at /
// created_at / updated_at and the site deadline are SQL literals/expressions,
// not bound parameters. The column NAME list in buildBatchInsertLinksSQL must
// contain all 21 of these plus content_source / first_collected_at /
// payload_purge_due_at / created_at / updated_at (26 names), matching the 26
// value expressions per row (21 placeholders + 5 SQL literals/expressions);
// TestBuildBatchInsertLinksSQLPlaceholderStride locks that names ==
// expressions invariant.
// The constant exists so the test suite can lock the stride against
// accidental drift; a future column addition requires updating this
// constant, the column NAME list, and the VALUES tuple builder below.
const insertLinkBatchColumnCount = 21

const (
	// A replacement capture derives its purge deadline from the intent that
	// actually wins. New user intent outranks the committed intent; committed
	// user intent outranks an automatic rule; concrete automatic rules outrank
	// older automatic intent and the current final partition.
	requeueLinkCaptureSQL = `UPDATE links SET source_kind = $2, source_key = $3, input_title = $4, input_text = $5, input_html = $6, input_images = $7::jsonb, source_metadata = $8::jsonb, description = COALESCE($9, description),
		requested_library_kind = CASE WHEN $11 = 'user' OR (requested_library_kind_source <> 'user' AND $10 IN ('reading', 'site')) THEN $10 ELSE requested_library_kind END,
		requested_library_kind_source = CASE WHEN $11 = 'user' OR (requested_library_kind_source <> 'user' AND $10 IN ('reading', 'site')) THEN $11 ELSE requested_library_kind_source END,
		library_kind = CASE WHEN $11 = 'user' THEN $10 ELSE library_kind END,
		library_kind_source = CASE WHEN $11 = 'user' THEN 'user' ELSE library_kind_source END,
		library_kind_locked = CASE WHEN $11 = 'user' THEN true ELSE library_kind_locked END,
		content = NULL, content_cjk_chars = 0, content_words = 0, content_document = NULL, content_format = 'plain', content_source = 'fetched', content_revision = content_revision + 1, embedding = NULL, embedding_model = NULL,
		payload_purge_due_at = CASE
			WHEN $11 = 'user' THEN CASE WHEN $10 = 'site' THEN NOW() + INTERVAL '24 hours' ELSE NULL END
			WHEN requested_library_kind_source = 'user' THEN CASE WHEN requested_library_kind = 'site' THEN NOW() + INTERVAL '24 hours' ELSE NULL END
			WHEN $10 = 'site' THEN NOW() + INTERVAL '24 hours'
			WHEN $10 = 'reading' THEN NULL
			WHEN requested_library_kind = 'site' THEN NOW() + INTERVAL '24 hours'
			WHEN requested_library_kind = 'reading' THEN NULL
			WHEN library_kind = 'site' THEN NOW() + INTERVAL '24 hours'
			ELSE NULL
		END,
		payload_purged_at = CASE WHEN $10 = 'site' OR library_kind = 'site' THEN NULL ELSE payload_purged_at END,
		status = 'pending', error_msg = NULL, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`
	requeueLinkRefreshSQL           = "UPDATE links SET embedding = NULL, embedding_model = NULL, status = 'pending', error_msg = NULL, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL"
	supersedeParseJobsSQL           = "UPDATE parse_jobs SET status = 'failed', error_msg = $2, updated_at = NOW() WHERE link_id = $1 AND status IN ('pending', 'processing')"
	selectSubmitLinksSQL            = "SELECT " + linkSelectColumns + ", deleted_at IS NOT NULL FROM links WHERE (source_key = ANY($1::text[]) OR url = ANY($2::text[]) OR id = ANY($3::uuid[])) ORDER BY id FOR UPDATE"
	selectCanonicalURLIdentitiesSQL = "SELECT normalized_url, link_id FROM link_url_identities WHERE normalized_url = ANY($1::text[])"
	adoptSubmittedLinkSQL           = "UPDATE links SET feed_managed=false,updated_at=NOW() WHERE id=$1 AND feed_managed=true"
	restartRestoredLinkSQL          = "UPDATE links SET status='pending',error_msg=NULL,updated_at=NOW() WHERE id=$1"
	lockRequestedLibraryIntentSQL   = "SELECT status, requested_library_kind_source FROM links WHERE id = $1 AND deleted_at IS NULL FOR UPDATE"
	updateRequestedLibraryIntentSQL = `UPDATE links SET
		requested_library_kind = $2, requested_library_kind_source = $3,
		payload_purge_due_at = CASE WHEN $2 = 'site' THEN COALESCE(payload_purge_due_at, NOW() + INTERVAL '24 hours') WHEN $2 = 'reading' THEN NULL ELSE payload_purge_due_at END,
		payload_purged_at = CASE WHEN $2 = 'site' THEN NULL ELSE payload_purged_at END,
		updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`
	parseJobSupersededMessage  = "superseded by a newer parse request"
	linkRestoredAttemptMessage = "superseded by link restore"
)

// SubmitNew inserts the link and its initial parse_jobs row in a single
// transaction so a partial failure cannot leave an orphan link with no job
// (the parse pipeline only schedules links that have a pending job, so a
// silent failure to create the job would strand the link in 'pending'
// forever). The transaction wraps both INSERTs.
//
// Product-facing callers use durablework.LinkCommands so the River row is
// inserted in the same transaction. This repository-owned wrapper remains for
// persistence tests and maintenance callers that intentionally create only the
// business rows.
//
// SubmitNew remains the single-URL path (Submit / Refresh / Ingest). The
// /api/links/batch fan-out switched to SubmitBatch (below) which performs
// one multi-row INSERT under ON CONFLICT, avoiding the per-item advisory
// lock round-trip.
func (r *PGXLinkRepository) SubmitNew(ctx context.Context, params CreateLinkParams) (*model.Link, *model.ParseJob, error) {
	results, err := r.SubmitBatch(ctx, []CreateLinkParams{params})
	if err != nil {
		return nil, nil, err
	}
	if len(results) != 1 || results[0].Link == nil {
		return nil, nil, fmt.Errorf("submit_new: expected one link result, got %d", len(results))
	}
	return results[0].Link, results[0].Job, nil
}

// RequeueExisting atomically moves an existing link back to pending and creates
// the immutable parse attempt. A nil
// capture is the Refresh form and preserves all source input. A non-nil
// capture is the ingest re-submit form and replaces the captured source fields
// exactly, including clearing fields that are nil in the latest snapshot.
func (r *PGXLinkRepository) RequeueExisting(
	ctx context.Context,
	linkID uuid.UUID,
	capture *CreateLinkParams,
) (*model.ParseJob, error) {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin requeue existing tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := r.RequeueExistingTx(ctx, tx, linkID, capture)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit requeue existing tx: %w", err)
	}
	return job, nil
}

// RequeueExistingTx is the transaction-bound implementation used by the
// durablework adapter. It never commits and never calls infrastructure hooks;
// the adapter owns queue cancellation/insertion and the final commit decision.
func (r *PGXLinkRepository) RequeueExistingTx(
	ctx context.Context,
	tx pgx.Tx,
	linkID uuid.UUID,
	capture *CreateLinkParams,
) (*model.ParseJob, error) {
	inputImages, sourceMetadata, err := marshalRequeueCapture(capture)
	if err != nil {
		return nil, err
	}
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return nil, err
	}
	if err := updateLinkForRequeue(ctx, tx, linkID, capture, inputImages, sourceMetadata); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, supersedeParseJobsSQL, linkID, parseJobSupersededMessage); err != nil {
		return nil, fmt.Errorf("supersede existing parse jobs: %w", err)
	}

	row := tx.QueryRow(ctx, insertJobSQL, linkID, parseJobsPerLinkRetention)
	job, err := scanJob(row)
	if err != nil {
		return nil, fmt.Errorf("insert job in requeue existing tx: %w", err)
	}
	return &job, nil
}

// UpdateRequestedLibraryIntentTx serializes a new concrete capture intent with
// terminal parse completion. It deliberately updates only the requested fact;
// the terminal transaction remains responsible for the final partition and
// its user lock after analysis artifacts have been reconciled.
func (r *PGXLinkRepository) UpdateRequestedLibraryIntentTx(
	ctx context.Context,
	tx pgx.Tx,
	params UpdateRequestedLibraryIntentParams,
) (UpdateRequestedLibraryIntentResult, error) {
	kind, source := normalizeRequestedLibraryIntent(params.Kind, params.Source)
	if kind == model.RequestedLibraryKindAuto {
		return UpdateRequestedLibraryIntentResult{}, fmt.Errorf("update requested library intent: concrete kind is required")
	}
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return UpdateRequestedLibraryIntentResult{}, err
	}
	var (
		status        model.LinkStatus
		currentSource model.RequestedLibraryKindSource
	)
	if err := tx.QueryRow(ctx, lockRequestedLibraryIntentSQL, params.ID).Scan(&status, &currentSource); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UpdateRequestedLibraryIntentResult{}, ErrNotFound
		}
		return UpdateRequestedLibraryIntentResult{}, fmt.Errorf("lock requested library intent: %w", err)
	}
	// Automatic source rules may make an unlocked intent concrete (RSS uses
	// reading/auto), but they cannot erase an explicit choice that already
	// committed. A later user command still overwrites either provenance.
	if source == model.RequestedLibraryKindSourceAuto && currentSource == model.RequestedLibraryKindSourceUser {
		return UpdateRequestedLibraryIntentResult{Status: status}, nil
	}
	tag, err := tx.Exec(ctx, updateRequestedLibraryIntentSQL, params.ID, kind, source)
	if err != nil {
		return UpdateRequestedLibraryIntentResult{}, fmt.Errorf("update requested library intent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return UpdateRequestedLibraryIntentResult{}, ErrNotFound
	}
	return UpdateRequestedLibraryIntentResult{Status: status}, nil
}

func marshalRequeueCapture(capture *CreateLinkParams) (inputImages, sourceMetadata any, err error) {
	if capture == nil {
		return nil, nil, nil
	}
	inputImages, err = marshalJSONB(capture.InputImages)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal requeue input images: %w", err)
	}
	sourceMetadata, err = marshalJSONB(capture.SourceMetadata)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal requeue source metadata: %w", err)
	}
	return inputImages, sourceMetadata, nil
}

func updateLinkForRequeue(
	ctx context.Context,
	tx pgx.Tx,
	linkID uuid.UUID,
	capture *CreateLinkParams,
	inputImages, sourceMetadata any,
) error {
	if capture == nil {
		tag, updateErr := tx.Exec(ctx, requeueLinkRefreshSQL, linkID)
		if updateErr != nil {
			return fmt.Errorf("mark existing link pending: %w", updateErr)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	}

	requestedKind, requestedSource := normalizeRequestedLibraryIntent(capture.RequestedLibraryKind, capture.RequestedLibraryKindSource)
	tag, updateErr := tx.Exec(
		ctx,
		requeueLinkCaptureSQL,
		linkID,
		defaultSourceKind(capture.SourceKind),
		defaultSourceKey(capture.SourceKey, capture.URL),
		capture.InputTitle,
		capture.InputText,
		capture.InputHTML,
		inputImages,
		sourceMetadata,
		capture.Description,
		requestedKind,
		requestedSource,
	)
	if updateErr != nil {
		return fmt.Errorf("update existing capture for requeue: %w", updateErr)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SubmitBatch issues a single multi-row INSERT for every CreateLinkParams
// in `items` and, for the rows that were genuinely new, a second multi-
// row INSERT into parse_jobs — all inside one transaction. This replaces
// the old SubmitService.Batch fan-out (100 goroutines each running their
// own SubmitNew + per-URL advisory lock) with a single round-trip.
//
// Two implementation notes the SQL leans on:
//
//  1. ON CONFLICT (source_key) DO NOTHING uses the installation-wide identity.
//     DO NOTHING deliberately avoids
//     rewriting existing rows on repeated saves. Since PostgreSQL omits
//     conflicts from RETURNING, a follow-up SELECT in the same transaction
//     fills only the missing source_keys back into the result map.
//
//  2. Every row emitted by RETURNING is necessarily new, so Inserted does not
//     depend on PostgreSQL system-column heuristics. Missing rows selected by
//     source_key are necessarily pre-existing and never receive a parse job.
//
// RETURNING order is not guaranteed to match the VALUES order (PG
// re-orders by physical insert order, which for ON CONFLICT depends on
// which row branch was taken). We index returning rows by source_key
// in a map and rebuild the result slice in input order so callers
// observe a stable 1:1 with their items.
//
// The caller MUST deduplicate `items` by source_key first — PostgreSQL
// rejects a single INSERT ... ON CONFLICT command that targets the
// same conflict row twice. SubmitService.Batch dedupes by canonical
// URL before calling, which matches because defaultSourceKey("", url)
// == url for the URL-keyed path.
func (r *PGXLinkRepository) SubmitBatch(ctx context.Context, items []CreateLinkParams) ([]LinkSubmitResult, error) {
	if len(items) == 0 {
		return nil, nil
	}
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin submit_batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	results, err := r.SubmitBatchTx(ctx, tx, items)
	if err != nil {
		return results, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit submit_batch tx: %w", err)
	}
	return results, nil
}

// SubmitBatchTx writes business rows into an existing transaction.
//
//nolint:gocyclo // single-tx INSERT/conflict resolution/job creation must share tx-local maps and rollback as one unit.
func (r *PGXLinkRepository) SubmitBatchTx(
	ctx context.Context,
	tx pgx.Tx,
	items []CreateLinkParams,
) (results []LinkSubmitResult, err error) {
	if len(items) == 0 {
		// Early return before opening a tx: a no-op batch call
		// should not even talk to the DB (matches the contract
		// callers in SubmitService.Batch rely on for empty/all-
		// rejected requests).
		return nil, nil
	}
	// lock_library_feed_revisions takes the representation gate exclusively.
	// Taking the shared gate first would require an in-place advisory-lock
	// upgrade; two replicas can each hold shared and deadlock waiting for the
	// other to release before either obtains exclusive.
	if err := prelockLibraryFeedRevisions(ctx, tx); err != nil {
		return nil, err
	}

	results = make([]LinkSubmitResult, len(items))
	resolved := make([]bool, len(items))
	prepared := make(map[uuid.UUID]preparedSubmittedLink)
	existing, err := querySubmitExistingLinks(ctx, tx, items)
	if err != nil {
		return nil, err
	}

	newItems := make([]CreateLinkParams, 0, len(items))
	newIndexes := make([]int, 0, len(items))
	for i, params := range items {
		key := defaultSourceKey(params.SourceKey, params.URL)
		if link := existing.lookup(key, params.URL); link != nil {
			job, restored, prepareErr := prepareExistingSubmittedLink(ctx, tx, link, existing.trashed[link.ID], prepared)
			if prepareErr != nil {
				return nil, prepareErr
			}
			linkCopy := *link
			if job != nil {
				linkCopy.Status = model.LinkStatusPending
			}
			results[i] = LinkSubmitResult{Link: &linkCopy, Job: job, Restored: restored}
			resolved[i] = true
			continue
		}
		newItems = append(newItems, params)
		newIndexes = append(newIndexes, i)
	}

	insertedBySourceKey := make(map[string]*model.Link, len(newItems))
	insertedLinkIDs := make([]uuid.UUID, 0, len(newItems))
	if len(newItems) > 0 {
		linksSQL, _ := buildBatchInsertLinksSQL(len(newItems))
		args := make([]any, 0, len(newItems)*insertLinkBatchColumnCount)
		for _, params := range newItems {
			inputImages, marshalErr := marshalJSONB(params.InputImages)
			if marshalErr != nil {
				return nil, fmt.Errorf("marshal input images: %w", marshalErr)
			}
			sourceMetadata, marshalErr := marshalJSONB(params.SourceMetadata)
			if marshalErr != nil {
				return nil, fmt.Errorf("marshal source metadata: %w", marshalErr)
			}
			requestedKind, requestedSource := normalizeRequestedLibraryIntent(params.RequestedLibraryKind, params.RequestedLibraryKindSource)
			libraryKind, libraryKindSource, libraryKindLocked := requestedLibraryClassification(requestedKind, requestedSource)
			args = append(args,
				params.URL,
				defaultSourceKind(params.SourceKind),
				defaultSourceKey(params.SourceKey, params.URL),
				params.InputTitle,
				params.InputText,
				params.InputHTML,
				inputImages,
				sourceMetadata,
				params.Description,
				params.Status,
				params.Domain,
				params.ContentType,
				requestedKind,
				requestedSource,
				libraryKind,
				libraryKindSource,
				libraryKindLocked,
				params.PredictedLibraryKind,
				params.PathDepth,
				params.ParentPath,
				nullableUUIDValue(params.ParentID),
			)
		}

		rows, queryErr := tx.Query(ctx, linksSQL, args...)
		if queryErr != nil {
			return nil, fmt.Errorf("submit_batch insert links: %w", queryErr)
		}
		insertedLinks, collectErr := collectSubmitLinks(rows, "inserted")
		if collectErr != nil {
			return nil, collectErr
		}
		for i := range insertedLinks {
			link := &insertedLinks[i]
			insertedBySourceKey[link.SourceKey] = link
			insertedLinkIDs = append(insertedLinkIDs, link.ID)
		}
	}

	// A same-source_key writer that does not participate in URL advisory
	// locking can still win between the pre-read and INSERT. DO NOTHING omits
	// that row from RETURNING, so re-read only the unresolved new candidates.
	missingItems := make([]CreateLinkParams, 0, len(newItems)-len(insertedBySourceKey))
	for _, params := range newItems {
		if insertedBySourceKey[defaultSourceKey(params.SourceKey, params.URL)] == nil {
			missingItems = append(missingItems, params)
		}
	}
	var conflicts submitLinkIndex
	if len(missingItems) > 0 {
		found, queryErr := querySubmitExistingLinks(ctx, tx, missingItems)
		if queryErr != nil {
			return nil, queryErr
		}
		conflicts = found
	}

	// Multi-row parse_jobs insert covering only the freshly inserted
	// links. Existing rows do not need a new pending job — the
	// caller will route those through submitExisting, which honours
	// the link's current status (done/processing/etc).
	jobsByLinkID := make(map[uuid.UUID]model.ParseJob, len(insertedLinkIDs))
	if len(insertedLinkIDs) > 0 {
		jobsSQL := buildBatchInsertJobsSQL(len(insertedLinkIDs))
		jobArgs := make([]any, 0, len(insertedLinkIDs))
		for _, id := range insertedLinkIDs {
			jobArgs = append(jobArgs, id)
		}
		jobRows, queryErr := tx.Query(ctx, jobsSQL, jobArgs...)
		if queryErr != nil {
			return nil, fmt.Errorf("submit_batch insert jobs: %w", queryErr)
		}
		for jobRows.Next() {
			job, scanErr := scanJob(jobRows)
			if scanErr != nil {
				jobRows.Close()
				return nil, fmt.Errorf("scan submit_batch job: %w", scanErr)
			}
			jobsByLinkID[job.LinkID] = job
		}
		if iterErr := jobRows.Err(); iterErr != nil {
			jobRows.Close()
			return nil, fmt.Errorf("iterate submit_batch jobs: %w", iterErr)
		}
		jobRows.Close()
	}

	for newPos, params := range newItems {
		i := newIndexes[newPos]
		key := defaultSourceKey(params.SourceKey, params.URL)
		link := insertedBySourceKey[key]
		inserted := link != nil
		if link == nil {
			link = conflicts.lookup(key, params.URL)
		}
		if link == nil {
			// Defensive: every input item must show up either in INSERT
			// RETURNING or in the conflict follow-up SELECT. A missing entry
			// indicates a caller violated the dedupe contract or
			// the DB returned an unexpected projection — either
			// way fail loud rather than silently dropping the
			// input.
			return nil, fmt.Errorf("submit_batch: missing row for source_key %q url %q", key, params.URL)
		}
		linkCopy := *link
		entry := LinkSubmitResult{Link: &linkCopy, Inserted: inserted}
		if inserted {
			job, jobOK := jobsByLinkID[link.ID]
			if !jobOK {
				return nil, fmt.Errorf("submit_batch: missing parse job for inserted link %s", link.ID)
			}
			jobCopy := job
			entry.Job = &jobCopy
		} else {
			job, restored, prepareErr := prepareExistingSubmittedLink(ctx, tx, link, conflicts.trashed[link.ID], prepared)
			if prepareErr != nil {
				return nil, prepareErr
			}
			entry.Job = job
			entry.Restored = restored
			if job != nil {
				linkCopy.Status = model.LinkStatusPending
				entry.Link = &linkCopy
			}
		}
		results[i] = entry
		resolved[i] = true
	}
	for i, ok := range resolved {
		if !ok {
			return nil, fmt.Errorf("submit_batch: unresolved input at index %d", i)
		}
	}
	return results, nil
}

// submitLinkIndex is the batch path's answer to "does this item already have a
// record?". The three dimensions are not interchangeable and their order is the
// contract: the canonical URL identity outranks both raw matches, because when a
// the installation holds several rows that normalise to the same URL, exactly one
// of them is the future target and the others must stay untouched. Matching on
// source_key or url first would hand the reuse to whichever colliding row the
// caller happened to spell.
type submitLinkIndex struct {
	byIdentity  map[string]*model.Link
	bySourceKey map[string]*model.Link
	byURL       map[string]*model.Link
	trashed     map[uuid.UUID]bool
}

func (index submitLinkIndex) lookup(sourceKey, rawURL string) *model.Link {
	if link := index.byIdentity[sourceKey]; link != nil {
		return link
	}
	if link := index.byIdentity[rawURL]; link != nil {
		return link
	}
	if link := index.bySourceKey[sourceKey]; link != nil {
		return link
	}
	return index.byURL[rawURL]
}

func querySubmitExistingLinks(
	ctx context.Context,
	tx pgx.Tx,
	items []CreateLinkParams,
) (submitLinkIndex, error) {
	keys := make([]string, 0, len(items))
	urls := make([]string, 0, len(items))
	identityCandidates := make([]string, 0, len(items)*2)
	seen := make(map[string]struct{}, len(items)*2)
	for _, item := range items {
		key := defaultSourceKey(item.SourceKey, item.URL)
		keys = append(keys, key)
		urls = append(urls, item.URL)
		for _, candidate := range [2]string{key, item.URL} {
			if _, duplicate := seen[candidate]; duplicate {
				continue
			}
			seen[candidate] = struct{}{}
			identityCandidates = append(identityCandidates, candidate)
		}
	}

	canonicalByIdentity, err := queryCanonicalLinkIDs(ctx, tx, identityCandidates)
	if err != nil {
		return submitLinkIndex{}, err
	}
	canonicalIDs := make([]uuid.UUID, 0, len(canonicalByIdentity))
	for _, linkID := range canonicalByIdentity {
		canonicalIDs = append(canonicalIDs, linkID)
	}

	rows, err := tx.Query(ctx, selectSubmitLinksSQL, keys, urls, canonicalIDs)
	if err != nil {
		return submitLinkIndex{}, fmt.Errorf("submit_batch select existing links: %w", err)
	}
	links, trashed, err := collectSubmitExistingLinks(rows)
	if err != nil {
		return submitLinkIndex{}, err
	}

	index := submitLinkIndex{
		byIdentity:  make(map[string]*model.Link, len(canonicalByIdentity)),
		bySourceKey: make(map[string]*model.Link, len(links)),
		byURL:       make(map[string]*model.Link, len(links)),
		trashed:     trashed,
	}
	byID := make(map[uuid.UUID]*model.Link, len(links))
	for i := range links {
		link := &links[i]
		byID[link.ID] = link
		// selectSubmitLinksSQL locks in ID order. Preserve the first fallback so
		// legacy duplicate rows resolve to the same lowest-ID Link on every run.
		if _, exists := index.bySourceKey[link.SourceKey]; !exists {
			index.bySourceKey[link.SourceKey] = link
		}
		if _, exists := index.byURL[link.URL]; !exists {
			index.byURL[link.URL] = link
		}
	}
	for normalizedURL, linkID := range canonicalByIdentity {
		if link := byID[linkID]; link != nil {
			index.byIdentity[normalizedURL] = link
		}
	}
	return index, nil
}

func collectSubmitExistingLinks(rows pgx.Rows) ([]model.Link, map[uuid.UUID]bool, error) {
	links := make([]model.Link, 0)
	trashed := make(map[uuid.UUID]bool)
	for rows.Next() {
		var (
			link      model.Link
			buf       linkScanBuffers
			isTrashed bool
		)
		fields := append(scanLinkFields(&link, &buf), &isTrashed)
		if err := rows.Scan(fields...); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan submit_batch existing link: %w", err)
		}
		if err := applyLinkScanBuffers(&link, &buf); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("apply submit_batch existing link buffers: %w", err)
		}
		links = append(links, link)
		trashed[link.ID] = isTrashed
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("iterate submit_batch existing links: %w", err)
	}
	rows.Close()
	return links, trashed, nil
}

type preparedSubmittedLink struct {
	job      *model.ParseJob
	restored bool
}

func prepareExistingSubmittedLink(
	ctx context.Context,
	tx pgx.Tx,
	link *model.Link,
	trashed bool,
	prepared map[uuid.UUID]preparedSubmittedLink,
) (*model.ParseJob, bool, error) {
	if result, ok := prepared[link.ID]; ok {
		return result.job, result.restored, nil
	}
	if !trashed {
		if _, err := tx.Exec(ctx, adoptSubmittedLinkSQL, link.ID); err != nil {
			return nil, false, fmt.Errorf("adopt submitted link: %w", err)
		}
		prepared[link.ID] = preparedSubmittedLink{}
		return nil, false, nil
	}
	restored, err := restoreLinkOn(ctx, tx, link.ID)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, adoptSubmittedLinkSQL, link.ID); err != nil {
		return nil, false, fmt.Errorf("adopt restored submitted link: %w", err)
	}
	if restored.status != model.LinkStatusPending && restored.status != model.LinkStatusProcessing {
		prepared[link.ID] = preparedSubmittedLink{restored: restored.changed}
		return nil, restored.changed, nil
	}
	if _, err := tx.Exec(ctx, supersedeParseJobsSQL, link.ID, linkRestoredAttemptMessage); err != nil {
		return nil, false, fmt.Errorf("terminalize restored link parse attempts: %w", err)
	}
	if _, err := tx.Exec(ctx, restartRestoredLinkSQL, link.ID); err != nil {
		return nil, false, fmt.Errorf("restart restored link: %w", err)
	}
	job, err := scanJob(tx.QueryRow(ctx, insertJobSQL, link.ID, parseJobsPerLinkRetention))
	if err != nil {
		return nil, false, fmt.Errorf("create restored link parse job: %w", err)
	}
	prepared[link.ID] = preparedSubmittedLink{job: &job, restored: restored.changed}
	return &job, restored.changed, nil
}

func queryCanonicalLinkIDs(
	ctx context.Context,
	tx pgx.Tx,
	normalizedURLs []string,
) (map[string]uuid.UUID, error) {
	canonical := make(map[string]uuid.UUID, len(normalizedURLs))
	if len(normalizedURLs) == 0 {
		return canonical, nil
	}
	rows, err := tx.Query(ctx, selectCanonicalURLIdentitiesSQL, normalizedURLs)
	if err != nil {
		return nil, fmt.Errorf("submit_batch select canonical url identities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			normalizedURL string
			linkID        uuid.UUID
		)
		if scanErr := rows.Scan(&normalizedURL, &linkID); scanErr != nil {
			return nil, fmt.Errorf("scan submit_batch canonical url identity: %w", scanErr)
		}
		canonical[normalizedURL] = linkID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate submit_batch canonical url identities: %w", err)
	}
	return canonical, nil
}

func collectSubmitLinks(rows pgx.Rows, kind string) ([]model.Link, error) {
	links := make([]model.Link, 0)
	for rows.Next() {
		var link model.Link
		var buf linkScanBuffers
		if err := rows.Scan(scanLinkFields(&link, &buf)...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan submit_batch %s link: %w", kind, err)
		}
		if err := applyLinkScanBuffers(&link, &buf); err != nil {
			rows.Close()
			return nil, fmt.Errorf("apply submit_batch %s link buffers: %w", kind, err)
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate submit_batch %s links: %w", kind, err)
	}
	rows.Close()
	return links, nil
}

// buildBatchInsertLinksSQL emits the multi-row INSERT template used by
// SubmitBatch. It returns the prepared SQL plus the per-row column
// count so callers can size their arg slice without a second source of
// truth. The placeholder stride must stay aligned with insertLinkSQL —
// the test suite locks that invariant.
func buildBatchInsertLinksSQL(n int) (string, int) {
	var sb strings.Builder
	sb.WriteString(
		"INSERT INTO links (url, source_kind, source_key, input_title, input_text, " +
			"input_html, input_images, source_metadata, description, status, domain, " +
			"content_type, requested_library_kind, requested_library_kind_source, library_kind, library_kind_source, library_kind_locked, predicted_library_kind, path_depth, " +
			"parent_path, parent_id, content_source, first_collected_at, payload_purge_due_at, created_at, updated_at) VALUES ",
	)
	placeholder := 1
	for row := 0; row < n; row++ {
		if row > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('(')
		for col := 0; col < insertLinkBatchColumnCount; col++ {
			if col > 0 {
				sb.WriteString(", ")
			}
			sb.WriteByte('$')
			sb.WriteString(itoa(placeholder))
			placeholder++
		}
		// The site-intent deadline is derived from this row's final or predicted
		// library kind placeholder. The remaining NOW() literals keep the
		// per-row arg count fixed at insertLinkBatchColumnCount.
		rowFirstPlaceholder := placeholder - insertLinkBatchColumnCount
		libraryKindPlaceholder := rowFirstPlaceholder + 14
		predictedKindPlaceholder := rowFirstPlaceholder + 17
		sb.WriteString(", 'fetched', NOW(), CASE WHEN $")
		sb.WriteString(itoa(libraryKindPlaceholder))
		sb.WriteString(" = 'site' OR $")
		sb.WriteString(itoa(predictedKindPlaceholder))
		sb.WriteString(" = 'site' THEN NOW() + INTERVAL '24 hours' ELSE NULL END, NOW(), NOW()")
		sb.WriteByte(')')
	}
	sb.WriteString(
		" ON CONFLICT (source_key) DO NOTHING RETURNING " + linkSelectColumns,
	)
	return sb.String(), insertLinkBatchColumnCount
}

// buildBatchInsertJobsSQL emits the multi-row parse_jobs INSERT used by
// SubmitBatch for freshly inserted links. Each row binds a single
// link_id placeholder; status / timestamps are SQL literals to keep the
// argument count predictable. The link metadata revision is selected in the
// same statement as the job insert, so the immutable attempt fence cannot
// race a Reader metadata replacement. RETURNING uses jobSelectColumns so the
// scan path reuses scanJob without an alternate projection.
func buildBatchInsertJobsSQL(n int) string {
	var sb strings.Builder
	sb.WriteString("INSERT INTO parse_jobs (link_id, status, created_at, updated_at, expected_metadata_revision) VALUES ")
	for row := 0; row < n; row++ {
		if row > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("($")
		sb.WriteString(itoa(row + 1))
		sb.WriteString(", 'pending', NOW(), NOW(), (SELECT metadata_revision FROM links WHERE id = $")
		sb.WriteString(itoa(row + 1))
		sb.WriteString("))")
	}
	sb.WriteString(" RETURNING " + jobSelectColumns)
	return sb.String()
}

// itoa formats a non-negative int into decimal without strconv.Itoa's
// heap allocation. The 6-byte stack buffer is sized for placeholders
// up to 999999, which is six orders of magnitude beyond the actual
// ceiling (defaultBatchSubmitLimit=100 items * 21 cols = 2100). The
// extra headroom costs nothing because the buffer lives on the stack
// for the lifetime of one buildBatchInsertLinksSQL call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [6]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
