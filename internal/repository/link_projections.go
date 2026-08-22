package repository

import (
	"context"
	sqldb "database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// These column lists are intentionally explicit. Their 1:1 scanners and the
// projection contract tests make a column addition visible at the same review
// point as the consumer field that justifies it.
const (
	linkDetailColumns       = "id, url, title, summary, tags, fetcher_type, is_low_confidence, low_confidence_reason, status, error_msg, description, domain, content_type, library_kind, content_revision, metadata_revision, content_source, has_content, content_cjk_chars, content_words, path_depth, parent_path, parent_id, created_at, updated_at"
	linkParseInputColumns   = "id, url, source_kind, source_key, input_title, input_text, input_html, input_images, source_metadata, description, status, library_kind, library_kind_locked, content_revision, metadata_revision, parse_generation, updated_at"
	linkLifecycleColumns    = "id, url, status, library_kind, library_kind_locked, content_revision, has_content, deleted_at"
	linkSubmitLookupColumns = "id, url, source_key, status, library_kind, library_kind_locked, COALESCE(last_recollected_at, first_collected_at) AS parse_requested_at"
)

// canonicalLinkMatch renders the "this row is the canonical record for the
// normalized URL identity" predicate.
//
// IS NOT DISTINCT FROM rather than "=": the subquery yields NULL when the
// identity has no canonical mapping, and a NULL comparison inside ORDER BY
// would sort ahead of the real matches under DESC's NULLS FIRST default.
func canonicalLinkMatch(urlParam string) string {
	return "(links.id IS NOT DISTINCT FROM (SELECT ident.link_id FROM link_url_identities ident" +
		" WHERE ident.normalized_url = " + urlParam + "))"
}

const (
	getLinkDetailByIDSQL = "SELECT " + linkDetailColumns + " FROM links WHERE id = $1 AND deleted_at IS NULL"

	getLinkParseInputByIDSQL        = "SELECT " + linkParseInputColumns + " FROM links WHERE id = $1 AND deleted_at IS NULL"
	getLinkParseInputBySourceKeySQL = "SELECT " + linkParseInputColumns + " FROM links WHERE source_key = $1 AND deleted_at IS NULL LIMIT 1"

	getLinkLifecycleByIDSQL = "SELECT " + linkLifecycleColumns + " FROM links WHERE id = $1"

	getLinkSubmitLookupByIDSQL = "SELECT " + linkSubmitLookupColumns + " FROM links WHERE id = $1 AND deleted_at IS NULL"
)

// URL-keyed lookups match the canonical mapping, the normalized source key,
// and the stored display URL. The mapping wins when it exists; otherwise the
// historical earliest-created / lowest-ID rule remains the fallback.
//
// The raw url arm is what keeps a database whose backfill has not run yet
// behaving exactly as it did before.
var (
	getLinkDetailByURLSQL = "SELECT " + linkDetailColumns + " FROM links WHERE deleted_at IS NULL AND (" +
		canonicalLinkMatch("$1") + " OR source_key = $1 OR url = $1) ORDER BY " +
		canonicalLinkMatch("$1") + " DESC, created_at ASC, id ASC LIMIT 1"

	// Ingest keeps its pre-existing preference for an exact source_key match,
	// which is what disambiguates a content-hash identity from a url-only
	// legacy duplicate. The canonical has to outrank it explicitly here: unlike
	// the url arms above, a source_key match can reach a row that is younger
	// than the canonical and still be preferred by that rule.
	getLinkParseInputBySourceKeyOrURLSQL = "SELECT " + linkParseInputColumns + " FROM links WHERE deleted_at IS NULL AND (" +
		canonicalLinkMatch("$2") + " OR source_key = $1 OR url = $2) ORDER BY " +
		canonicalLinkMatch("$2") + " DESC, (source_key = $1) DESC, created_at ASC, id ASC LIMIT 1"

	getLinkSubmitLookupByURLSQL = "SELECT " + linkSubmitLookupColumns + " FROM links WHERE deleted_at IS NULL AND (" +
		canonicalLinkMatch("$1") + " OR source_key = $1 OR url = $1) ORDER BY " +
		canonicalLinkMatch("$1") + " DESC, created_at ASC, id ASC LIMIT 1"
)

type linkDetailScanBuffers struct {
	title               pgtype.Text
	summary             pgtype.Text
	fetcherType         pgtype.Text
	lowConfidenceReason pgtype.Text
	errorMsg            pgtype.Text
	description         pgtype.Text
	domain              pgtype.Text
	contentType         pgtype.Text
	libraryKind         pgtype.Text
	contentSource       pgtype.Text
	pathDepth           sqldb.NullInt64
	parentPath          pgtype.Text
	parentID            pgtype.UUID
}

func scanLinkDetail(row rowScanner) (LinkDetailProjection, error) {
	var projection LinkDetailProjection
	var buf linkDetailScanBuffers
	if err := row.Scan(
		&projection.ID,
		&projection.URL,
		&buf.title,
		&buf.summary,
		&projection.Tags,
		&buf.fetcherType,
		&projection.IsLowConfidence,
		&buf.lowConfidenceReason,
		&projection.Status,
		&buf.errorMsg,
		&buf.description,
		&buf.domain,
		&buf.contentType,
		&buf.libraryKind,
		&projection.ContentRevision,
		&projection.MetadataRevision,
		&buf.contentSource,
		&projection.HasContent,
		&projection.ContentCJKChars,
		&projection.ContentWords,
		&buf.pathDepth,
		&buf.parentPath,
		&buf.parentID,
		&projection.CreatedAt,
		&projection.UpdatedAt,
	); err != nil {
		return projection, err
	}

	projection.Title = textPointer(buf.title)
	projection.Summary = textPointer(buf.summary)
	projection.FetcherType = textPointer(buf.fetcherType)
	projection.LowConfidenceReason = textPointer(buf.lowConfidenceReason)
	projection.ErrorMsg = textPointer(buf.errorMsg)
	projection.Description = textPointer(buf.description)
	projection.Domain = textPointer(buf.domain)
	projection.ContentType = textPointer(buf.contentType)
	projection.LibraryKind = libraryKindPointer(buf.libraryKind)
	projection.ContentSource = contentSourceFromString(buf.contentSource.String)
	projection.PathDepth = intPointer(buf.pathDepth)
	projection.ParentPath = textPointer(buf.parentPath)
	projection.ParentID = uuidPointer(buf.parentID)
	return projection, nil
}

type linkParseInputScanBuffers struct {
	sourceKind     pgtype.Text
	sourceKey      pgtype.Text
	inputTitle     pgtype.Text
	inputText      pgtype.Text
	inputHTML      pgtype.Text
	inputImages    []byte
	sourceMetadata []byte
	description    pgtype.Text
	libraryKind    pgtype.Text
}

func scanLinkParseInput(row rowScanner) (LinkParseInput, error) {
	var projection LinkParseInput
	var buf linkParseInputScanBuffers
	if err := row.Scan(
		&projection.ID,
		&projection.URL,
		&buf.sourceKind,
		&buf.sourceKey,
		&buf.inputTitle,
		&buf.inputText,
		&buf.inputHTML,
		&buf.inputImages,
		&buf.sourceMetadata,
		&buf.description,
		&projection.Status,
		&buf.libraryKind,
		&projection.LibraryKindLocked,
		&projection.ContentRevision,
		&projection.MetadataRevision,
		&projection.ParseGeneration,
		&projection.UpdatedAt,
	); err != nil {
		return projection, err
	}

	projection.SourceKind = defaultSourceKind(buf.sourceKind.String)
	projection.SourceKey = defaultSourceKey(buf.sourceKey.String, projection.URL)
	projection.InputTitle = textPointer(buf.inputTitle)
	projection.InputText = textPointer(buf.inputText)
	projection.InputHTML = textPointer(buf.inputHTML)
	projection.Description = textPointer(buf.description)
	projection.LibraryKind = libraryKindPointer(buf.libraryKind)

	images, err := unmarshalStringSlice(buf.inputImages)
	if err != nil {
		return projection, fmt.Errorf("decode link parse input images: %w", err)
	}
	projection.InputImages = images
	metadata, err := unmarshalMetadata(buf.sourceMetadata)
	if err != nil {
		return projection, fmt.Errorf("decode link parse input metadata: %w", err)
	}
	projection.SourceMetadata = metadata
	return projection, nil
}

type linkLifecycleScanBuffers struct {
	libraryKind pgtype.Text
	deletedAt   pgtype.Timestamptz
}

func scanLinkLifecycle(row rowScanner) (LinkLifecycleProjection, error) {
	var projection LinkLifecycleProjection
	var buf linkLifecycleScanBuffers
	if err := row.Scan(
		&projection.ID,
		&projection.URL,
		&projection.Status,
		&buf.libraryKind,
		&projection.LibraryKindLocked,
		&projection.ContentRevision,
		&projection.HasContent,
		&buf.deletedAt,
	); err != nil {
		return projection, err
	}
	projection.LibraryKind = libraryKindPointer(buf.libraryKind)
	if buf.deletedAt.Valid {
		value := buf.deletedAt.Time
		projection.DeletedAt = &value
	}
	return projection, nil
}

func scanLinkSubmitLookup(row rowScanner) (LinkSubmitLookup, error) {
	var projection LinkSubmitLookup
	var sourceKey, libraryKind pgtype.Text
	if err := row.Scan(
		&projection.ID,
		&projection.URL,
		&sourceKey,
		&projection.Status,
		&libraryKind,
		&projection.LibraryKindLocked,
		&projection.ParseRequestedAt,
	); err != nil {
		return projection, err
	}
	projection.SourceKey = defaultSourceKey(sourceKey.String, projection.URL)
	projection.LibraryKind = libraryKindPointer(libraryKind)
	return projection, nil
}

func projectionOrNil[T any](value T, err error, operation string) (*T, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	return &value, nil
}

func (r *PGXLinkRepository) GetDetailByID(ctx context.Context, id uuid.UUID) (*LinkDetailProjection, error) {
	value, err := scanLinkDetail(r.db.QueryRow(ctx, getLinkDetailByIDSQL, id))
	return projectionOrNil(value, err, "get link detail by id")
}

func (r *PGXLinkRepository) GetDetailByURL(ctx context.Context, rawURL string) (*LinkDetailProjection, error) {
	value, err := scanLinkDetail(r.db.QueryRow(ctx, getLinkDetailByURLSQL, rawURL))
	return projectionOrNil(value, err, "get link detail by url")
}

func (r *PGXLinkRepository) GetParseInputByID(ctx context.Context, id uuid.UUID) (*LinkParseInput, error) {
	value, err := scanLinkParseInput(r.db.QueryRow(ctx, getLinkParseInputByIDSQL, id))
	return projectionOrNil(value, err, "get link parse input by id")
}

func (r *PGXLinkRepository) GetParseInputBySourceKeyOrURL(ctx context.Context, sourceKey, rawURL string) (*LinkParseInput, error) {
	var row pgx.Row
	if rawURL == "" {
		row = r.db.QueryRow(ctx, getLinkParseInputBySourceKeySQL, sourceKey)
	} else {
		row = r.db.QueryRow(ctx, getLinkParseInputBySourceKeyOrURLSQL, sourceKey, rawURL)
	}
	value, err := scanLinkParseInput(row)
	return projectionOrNil(value, err, "get link parse input by source key or url")
}

func (r *PGXLinkRepository) GetLifecycleByID(ctx context.Context, id uuid.UUID) (*LinkLifecycleProjection, error) {
	value, err := scanLinkLifecycle(r.db.QueryRow(ctx, getLinkLifecycleByIDSQL, id))
	return projectionOrNil(value, err, "get link lifecycle by id")
}

func (r *PGXLinkRepository) GetSubmitLookupByID(ctx context.Context, id uuid.UUID) (*LinkSubmitLookup, error) {
	value, err := scanLinkSubmitLookup(r.db.QueryRow(ctx, getLinkSubmitLookupByIDSQL, id))
	return projectionOrNil(value, err, "get link submit lookup by id")
}

func (r *PGXLinkRepository) GetSubmitLookupByURL(ctx context.Context, rawURL string) (*LinkSubmitLookup, error) {
	value, err := scanLinkSubmitLookup(r.db.QueryRow(ctx, getLinkSubmitLookupByURLSQL, rawURL))
	return projectionOrNil(value, err, "get link submit lookup by url")
}
