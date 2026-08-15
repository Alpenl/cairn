package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
)

const linkSelectColumns = "id, url, source_kind, source_key, input_title, input_text, input_html, input_images, source_metadata, title, summary, tags, fetcher_type, is_low_confidence, low_confidence_reason, status, error_msg, description, domain, content_type, requested_library_kind, requested_library_kind_source, library_kind, library_kind_source, library_kind_locked, predicted_library_kind, classification_confidence, classification_reason, classification_explanation, classifier_version, content_revision, metadata_revision, content_source, has_content, content_cjk_chars, content_words, first_collected_at, last_recollected_at, payload_purge_due_at, payload_purged_at, path_depth, parent_path, parent_id, created_at, updated_at"

// linkListColumns is the projection used by list/tree read endpoints. It
// drops input_title/input_text/input_html/input_images/source_metadata
// and the ingest-only source_kind/source_key — none of those fields are
// exposed by LinkResponse or TreeNodeResponse, but their JSONB and TEXT
// payloads can run into hundreds of KB per row on browser-capture
// ingests. A 100-row page can balloon by two orders of magnitude when
// the heavy columns ride along, so the list path uses this narrower
// projection while point lookups (Get*) keep linkSelectColumns to feed
// the parse pipeline that does need the inputs.
const linkListColumns = "id, url, title, summary, tags, fetcher_type, is_low_confidence, low_confidence_reason, status, error_msg, description, domain, content_type, library_kind, library_kind_source, library_kind_locked, predicted_library_kind, classification_confidence, classification_reason, classification_explanation, classifier_version, content_revision, metadata_revision, has_content, content_cjk_chars, content_words, first_collected_at, last_recollected_at, payload_purge_due_at, payload_purged_at, path_depth, parent_path, parent_id, created_at, updated_at"

// linkListColumnsWithTotal is the windowed-count variant of linkListColumns
// for the paginated list endpoint.
const linkListColumnsWithTotal = linkListColumns + ", COUNT(*) OVER() AS total_count"

const (
	insertLinkSQL         = "INSERT INTO links (url, source_kind, source_key, input_title, input_text, input_html, input_images, source_metadata, description, status, domain, content_type, requested_library_kind, requested_library_kind_source, library_kind, library_kind_source, library_kind_locked, predicted_library_kind, path_depth, parent_path, parent_id, content_source, first_collected_at, payload_purge_due_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, 'fetched', NOW(), CASE WHEN $15 = 'site' OR $18 = 'site' THEN NOW() + INTERVAL '24 hours' ELSE NULL END, NOW(), NOW()) RETURNING " + linkSelectColumns
	updateLinkStateSQL    = "UPDATE links SET status = $2, error_msg = $3, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL"
	updateLinkAnalysisSQL = "UPDATE links SET source_kind = COALESCE(NULLIF($2, ''), source_kind), source_key = COALESCE(NULLIF($3, ''), source_key), input_title = COALESCE($4, input_title), input_text = COALESCE($5, input_text), input_html = COALESCE($6, input_html), input_images = COALESCE($7::jsonb, input_images), source_metadata = COALESCE($8::jsonb, source_metadata), title = $9, summary = $10, tags = COALESCE($11, '{}'::text[]), fetcher_type = $12, is_low_confidence = $13, low_confidence_reason = $14, status = $15, error_msg = $16, domain = $17, content_type = $18, path_depth = $19, parent_path = $20, parent_id = $21, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL"
	// updateLinkAnalysisForParseSQL owns the parser-only metadata fence. The
	// prior CTE locks the live Link row and captures its revision before SET
	// expressions run. A stale (or pre-rollout zero) attempt still updates its
	// parse lifecycle artifacts, but leaves title/summary/tags byte-for-byte
	// untouched so the revision trigger cannot advance or invalidate the user
	// replacement. The RETURNING flag reflects the pre-write ownership check;
	// the returned revision is the trigger-adjusted authoritative value for
	// post-parse enrichments.
	updateLinkAnalysisForParseSQL = `WITH prior AS (
			SELECT id, metadata_revision
			FROM links
			WHERE id = $1 AND deleted_at IS NULL
			FOR UPDATE
		)
	UPDATE links AS link
	SET source_kind = COALESCE(NULLIF($2, ''), link.source_kind),
		source_key = COALESCE(NULLIF($3, ''), link.source_key),
		input_title = COALESCE($4, link.input_title),
		input_text = COALESCE($5, link.input_text),
		input_html = COALESCE($6, link.input_html),
		input_images = COALESCE($7::jsonb, link.input_images),
		source_metadata = COALESCE($8::jsonb, link.source_metadata),
			title = CASE WHEN $22 > 0 AND prior.metadata_revision = $22 THEN $9 ELSE link.title END,
			summary = CASE WHEN $22 > 0 AND prior.metadata_revision = $22 THEN $10 ELSE link.summary END,
			tags = CASE WHEN $22 > 0 AND prior.metadata_revision = $22 THEN COALESCE($11, '{}'::text[]) ELSE link.tags END,
		fetcher_type = $12,
		is_low_confidence = $13,
		low_confidence_reason = $14,
		status = $15,
		error_msg = $16,
		domain = $17,
		content_type = $18,
		path_depth = $19,
		parent_path = $20,
		parent_id = $21,
		updated_at = NOW()
	FROM prior
	WHERE link.id = prior.id
		RETURNING link.metadata_revision, ($22 > 0 AND prior.metadata_revision = $22) AS metadata_applied`
	deleteLinkSQL = "UPDATE links SET deleted_at = COALESCE(deleted_at, NOW()), updated_at = CASE WHEN deleted_at IS NULL THEN NOW() ELSE updated_at END WHERE id = $1"
	// The COUNT(*) fallback for the empty-page case is now built inline by
	// buildListDoneSQL alongside the status predicate (status = 'done' by
	// default, status = ANY(...) when a status set is requested), so the
	// count and page queries share one filter assembly path and never drift.
	// TODO(M12): on large tables consider a pg_class.reltuples estimate (or a
	// trigger-maintained counter) for empty-filter, non-first-page requests.
	// Concrete trigger thresholds: revisit when GET /api/links P95 latency
	// exceeds 200ms for page>5 (empty-page tail) over a 5-minute window, or
	// when the done-links row count crosses ~100k. Below those signals the
	// fallback fires only on cold paginate-past-end scans where a sequential
	// scan over a few thousand rows still completes in single-digit ms.
	getLinkByIDSQL             = "SELECT " + linkSelectColumns + " FROM links WHERE id = $1 AND deleted_at IS NULL"
	getLinkByURLSQL            = "SELECT " + linkSelectColumns + " FROM links WHERE url = $1 AND deleted_at IS NULL LIMIT 1"
	getLinkBySourceKeySQL      = "SELECT " + linkSelectColumns + " FROM links WHERE source_key = $1 AND deleted_at IS NULL LIMIT 1"
	getLinkBySourceKeyOrURLSQL = "SELECT " + linkSelectColumns + " FROM links WHERE (source_key = $1 OR url = $2) AND deleted_at IS NULL ORDER BY (source_key = $1) DESC, created_at ASC, id ASC LIMIT 1"
)

// txBeginner is the subset of *pgxpool.Pool / pgxmock used to open a
// transaction for SubmitNew. Both production (pgxpool.Pool) and pgxmock
// satisfy it; declaring a local interface keeps the repository from
// importing pgxpool directly.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PGXLinkRepository 是 LinkStore 的 PG 实现，包装 *pgxpool.Pool 完成 links
// 表读写，并为 durablework 提供具体的 transaction-bound 写方法。
type PGXLinkRepository struct {
	db database.Querier
	tx txBeginner
}

// NewPGXLinkRepository requires the supplied Querier to also implement
// txBeginner so SubmitNew and revision-ordered deletion can open real
// transactions. Both production
// (*pgxpool.Pool) and the pgxmock test driver satisfy this — the previous
// non-tx fallback existed only for legacy stubs that no test exercises any
// more, so the constructor now panics if the contract is broken instead of
// silently sliding into split-write semantics.
func NewPGXLinkRepository(db database.Querier) *PGXLinkRepository {
	begin, ok := db.(txBeginner)
	if !ok {
		panic("repository: Querier must implement txBeginner for submit/delete transactions")
	}
	return &PGXLinkRepository{db: db, tx: begin}
}

// Compile-time assertions: PGXLinkRepository must implement every read/write
// role interface plus the LinkStore composite. Durable command methods are
// concrete adapter internals rather than service-facing repository ports. A future
// interface change here surfaces as a build error rather than a confusing
// runtime "method missing" panic in the wiring layer.
var (
	_ LinkReader = (*PGXLinkRepository)(nil)
	_ LinkWriter = (*PGXLinkRepository)(nil)
	_ LinkStore  = (*PGXLinkRepository)(nil)
)

// Create 向 links 表插入一行并返回完整模型。非事务场景下的快速入口，
// 想配合 parse_jobs 一起原子写入请走 SubmitNew。
func (r *PGXLinkRepository) Create(ctx context.Context, params CreateLinkParams) (*model.Link, error) {
	return insertLinkOn(ctx, r.db, params)
}

// GetByID is the legacy full-row lookup retained for repository persistence
// tests, maintenance probes, and the frozen RF9 benchmark baseline. Product
// feature reads must use GetDetailByID, GetParseInputByID,
// GetLifecycleByID, or GetSubmitLookupByID so capture TOAST is paid only by a
// consumer that needs it. A miss returns (nil, nil).
func (r *PGXLinkRepository) GetByID(ctx context.Context, id uuid.UUID) (*model.Link, error) {
	row := r.db.QueryRow(ctx, getLinkByIDSQL, id)
	link, err := scanLink(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get link by id: %w", err)
	}

	return &link, nil
}

// GetByURL is the legacy full-row URL lookup retained for persistence tests
// and maintenance probes. Product detail and submit paths use their explicit
// URL projections. A miss returns (nil, nil).
func (r *PGXLinkRepository) GetByURL(ctx context.Context, url string) (*model.Link, error) {
	row := r.db.QueryRow(ctx, getLinkByURLSQL, url)
	link, err := scanLink(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get link by url: %w", err)
	}

	return &link, nil
}

// GetBySourceKey is the legacy full-row source-key lookup retained for
// persistence tests and the compatibility implementation below. Product
// ingest uses GetParseInputBySourceKeyOrURL. A miss returns (nil, nil).
func (r *PGXLinkRepository) GetBySourceKey(ctx context.Context, sourceKey string) (*model.Link, error) {
	row := r.db.QueryRow(ctx, getLinkBySourceKeySQL, sourceKey)
	link, err := scanLink(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get link by source key: %w", err)
	}

	return &link, nil
}

// GetBySourceKeyOrURL is the legacy full-row compatibility lookup retained for
// persistence tests. IngestService now uses the equivalent LinkParseInput
// projection. The single SQL works
// because source_key has a UNIQUE index and url has a non-unique index
// — the OR plan still uses a bitmap-or of the two indexes. The explicit
// ordering makes an exact source_key row canonical when legacy releases left
// both a URL-keyed Submit row and a hash-keyed capture row for the same URL;
// created_at/id make the URL-only fallback deterministic as well. When url is
// empty the call falls back to the source_key-only path, avoiding a scan
// trigger on rows with empty url.
func (r *PGXLinkRepository) GetBySourceKeyOrURL(ctx context.Context, sourceKey, url string) (*model.Link, error) {
	if url == "" {
		return r.GetBySourceKey(ctx, sourceKey)
	}
	row := r.db.QueryRow(ctx, getLinkBySourceKeyOrURLSQL, sourceKey, url)
	link, err := scanLink(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get link by source key or url: %w", err)
	}
	return &link, nil
}

// UpdateState 仅切换 links.status / error_msg，命中 0 行时返回 ErrNotFound。
func (r *PGXLinkRepository) UpdateState(ctx context.Context, params UpdateLinkStateParams) error {
	tag, err := r.db.Exec(ctx, updateLinkStateSQL, params.ID, params.Status, params.ErrorMsg)
	if err != nil {
		return fmt.Errorf("update link state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateAnalysis 在解析管线完成后写回标题/摘要/标签等分析产物。
// SQL 中对 input_* / source_metadata 使用 COALESCE(NULLIF(...))，
// 让本次未提供的字段保留旧值，避免在重试场景中把原始素材覆盖成 NULL。
func (r *PGXLinkRepository) UpdateAnalysis(ctx context.Context, params UpdateLinkAnalysisParams) error {
	return updateLinkAnalysisOn(ctx, r.db, params)
}

func updateLinkAnalysisOn(ctx context.Context, db database.Querier, params UpdateLinkAnalysisParams) error {
	inputImages, err := marshalJSONB(params.InputImages)
	if err != nil {
		return fmt.Errorf("marshal input images: %w", err)
	}
	sourceMetadata, err := marshalJSONB(params.SourceMetadata)
	if err != nil {
		return fmt.Errorf("marshal source metadata: %w", err)
	}

	tag, err := db.Exec(
		ctx,
		updateLinkAnalysisSQL,
		params.ID,
		params.SourceKind,
		params.SourceKey,
		params.InputTitle,
		params.InputText,
		params.InputHTML,
		inputImages,
		sourceMetadata,
		params.Title,
		params.Summary,
		params.Tags,
		params.FetcherType,
		params.IsLowConfidence,
		params.LowConfidenceReason,
		params.Status,
		params.ErrorMsg,
		params.Domain,
		params.ContentType,
		params.PathDepth,
		params.ParentPath,
		nullableUUIDValue(params.ParentID),
	)
	if err != nil {
		return fmt.Errorf("update link analysis: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// updateLinkAnalysisForParseOn is the terminal parser counterpart of
// updateLinkAnalysisOn. It deliberately does not share the generic SQL: a
// parser has a revision-scoped lease for the user-owned tuple, while ordinary
// maintenance callers retain their existing explicit write contract.
func updateLinkAnalysisForParseOn(ctx context.Context, db database.Querier, params UpdateLinkAnalysisParams) (CompleteReadingParseResult, error) {
	inputImages, err := marshalJSONB(params.InputImages)
	if err != nil {
		return CompleteReadingParseResult{}, fmt.Errorf("marshal input images: %w", err)
	}
	sourceMetadata, err := marshalJSONB(params.SourceMetadata)
	if err != nil {
		return CompleteReadingParseResult{}, fmt.Errorf("marshal source metadata: %w", err)
	}

	var result CompleteReadingParseResult
	err = db.QueryRow(
		ctx,
		updateLinkAnalysisForParseSQL,
		params.ID,
		params.SourceKind,
		params.SourceKey,
		params.InputTitle,
		params.InputText,
		params.InputHTML,
		inputImages,
		sourceMetadata,
		params.Title,
		params.Summary,
		params.Tags,
		params.FetcherType,
		params.IsLowConfidence,
		params.LowConfidenceReason,
		params.Status,
		params.ErrorMsg,
		params.Domain,
		params.ContentType,
		params.PathDepth,
		params.ParentPath,
		nullableUUIDValue(params.ParentID),
		params.ExpectedMetadataRevision,
	).Scan(&result.MetadataRevision, &result.MetadataApplied)
	if errors.Is(err, pgx.ErrNoRows) {
		return CompleteReadingParseResult{}, ErrNotFound
	}
	if err != nil {
		return CompleteReadingParseResult{}, fmt.Errorf("update link analysis for parse: %w", err)
	}
	return result, nil
}

// Delete 按主键删除一条 link，命中 0 行时返回 ErrNotFound。它复用 lifecycle
// transaction，确保 FK 对 feed_items 的 SET NULL 与 links trigger 始终遵循
// library -> feed revision 的预锁顺序。
func (r *PGXLinkRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.DeleteLifecycle(ctx, id)
}

// insertLinkOn shares the link INSERT body between Create (uses r.db) and
// SubmitNew (uses a tx). Keeping the body in one place avoids a future schema
// change ending up half-applied in only one of the two paths.
func insertLinkOn(ctx context.Context, exec database.Querier, params CreateLinkParams) (*model.Link, error) {
	inputImages, err := marshalJSONB(params.InputImages)
	if err != nil {
		return nil, fmt.Errorf("marshal input images: %w", err)
	}
	sourceMetadata, err := marshalJSONB(params.SourceMetadata)
	if err != nil {
		return nil, fmt.Errorf("marshal source metadata: %w", err)
	}
	requestedKind, requestedSource := normalizeRequestedLibraryIntent(params.RequestedLibraryKind, params.RequestedLibraryKindSource)
	libraryKind, libraryKindSource, libraryKindLocked := requestedLibraryClassification(requestedKind, requestedSource)

	row := exec.QueryRow(
		ctx,
		insertLinkSQL,
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

	link, err := scanLink(row)
	if err != nil {
		return nil, fmt.Errorf("insert link: %w", err)
	}
	return &link, nil
}

// requestedLibraryClassification translates request-only intent into the
// persisted final fields. Auto intentionally leaves the partition unresolved:
// the parse pipeline supplies the eventual classifier decision, while an
// explicit request is immediately final and cannot be auto-overridden.
func requestedLibraryClassification(requested model.RequestedLibraryKind, requestedSource model.RequestedLibraryKindSource) (*model.LibraryKind, *model.LibraryKindSource, bool) {
	source := model.LibraryKindSourceAuto
	locked := false
	if requestedSource == model.RequestedLibraryKindSourceUser {
		source = model.LibraryKindSourceUser
		locked = true
	}
	switch requested {
	case model.RequestedLibraryKindReading:
		kind := model.LibraryKindReading
		return &kind, &source, locked
	case model.RequestedLibraryKindSite:
		kind := model.LibraryKindSite
		return &kind, &source, locked
	default:
		return nil, nil, false
	}
}

func normalizeRequestedLibraryIntent(kind model.RequestedLibraryKind, source model.RequestedLibraryKindSource) (model.RequestedLibraryKind, model.RequestedLibraryKindSource) {
	switch kind {
	case model.RequestedLibraryKindReading, model.RequestedLibraryKindSite:
		if source == model.RequestedLibraryKindSourceAuto {
			return kind, source
		}
		return kind, model.RequestedLibraryKindSourceUser
	default:
		return model.RequestedLibraryKindAuto, model.RequestedLibraryKindSourceAuto
	}
}
