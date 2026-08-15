package repository

import (
	sqldb "database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/model"
)

type rowScanner interface {
	Scan(dest ...any) error
}

// linkScanBuffers holds the nullable-typed scratch destinations linkSelectColumns
// expects. Keeping them in one struct lets scanLink and scanLinkWithTotal share
// the same scan-target slice and post-scan unpacking, eliminating ~80 lines of
// near-duplicate field plumbing across the two helpers.
type linkScanBuffers struct {
	sourceKind                pgtype.Text
	sourceKey                 pgtype.Text
	inputTitle                pgtype.Text
	inputText                 pgtype.Text
	inputHTML                 pgtype.Text
	inputImages               []byte
	sourceMetadata            []byte
	title                     pgtype.Text
	summary                   pgtype.Text
	fetcherType               pgtype.Text
	lowConfidenceReason       pgtype.Text
	errorMsg                  pgtype.Text
	description               pgtype.Text
	domain                    pgtype.Text
	contentType               pgtype.Text
	requestedKind             pgtype.Text
	requestedKindSource       pgtype.Text
	libraryKind               pgtype.Text
	libraryKindSource         pgtype.Text
	predictedKind             pgtype.Text
	confidence                pgtype.Float4
	classificationReason      pgtype.Text
	classificationExplanation pgtype.Text
	classifierVersion         pgtype.Text
	contentSource             pgtype.Text
	firstCollectedAt          pgtype.Timestamptz
	lastRecollectedAt         pgtype.Timestamptz
	payloadPurgeDueAt         pgtype.Timestamptz
	payloadPurgedAt           pgtype.Timestamptz
	pathDepth                 sqldb.NullInt64
	parentPath                pgtype.Text
	parentID                  pgtype.UUID
}

// scanLinkFields returns the destination slice that maps 1:1 to
// linkSelectColumns. Callers append additional destinations (e.g. the windowed
// total) before passing the slice to row.Scan.
func scanLinkFields(link *model.Link, buf *linkScanBuffers) []any {
	return []any{
		&link.ID,
		&link.URL,
		&buf.sourceKind,
		&buf.sourceKey,
		&buf.inputTitle,
		&buf.inputText,
		&buf.inputHTML,
		&buf.inputImages,
		&buf.sourceMetadata,
		&buf.title,
		&buf.summary,
		&link.Tags,
		&buf.fetcherType,
		&link.IsLowConfidence,
		&buf.lowConfidenceReason,
		&link.Status,
		&buf.errorMsg,
		&buf.description,
		&buf.domain,
		&buf.contentType,
		&buf.requestedKind,
		&buf.requestedKindSource,
		&buf.libraryKind,
		&buf.libraryKindSource,
		&link.LibraryKindLocked,
		&buf.predictedKind,
		&buf.confidence,
		&buf.classificationReason,
		&buf.classificationExplanation,
		&buf.classifierVersion,
		&link.ContentRevision,
		&link.MetadataRevision,
		&buf.contentSource,
		&link.HasContent,
		&link.ContentCJKChars,
		&link.ContentWords,
		&buf.firstCollectedAt,
		&buf.lastRecollectedAt,
		&buf.payloadPurgeDueAt,
		&buf.payloadPurgedAt,
		&buf.pathDepth,
		&buf.parentPath,
		&buf.parentID,
		&link.CreatedAt,
		&link.UpdatedAt,
	}
}

// applyLinkScanBuffers copies the nullable scratch values back onto the link
// after a successful Scan. Shared by scanLink and scanLinkWithTotal so a future
// column addition lands in exactly one place.
func applyLinkScanBuffers(link *model.Link, buf *linkScanBuffers) error {
	link.SourceKind = defaultSourceKind(buf.sourceKind.String)
	link.SourceKey = defaultSourceKey(buf.sourceKey.String, link.URL)
	link.InputTitle = textPointer(buf.inputTitle)
	link.InputText = textPointer(buf.inputText)
	link.InputHTML = textPointer(buf.inputHTML)
	link.Title = textPointer(buf.title)
	link.Summary = textPointer(buf.summary)
	link.FetcherType = textPointer(buf.fetcherType)
	link.LowConfidenceReason = textPointer(buf.lowConfidenceReason)
	link.ErrorMsg = textPointer(buf.errorMsg)
	link.Description = textPointer(buf.description)
	link.Domain = textPointer(buf.domain)
	link.ContentType = textPointer(buf.contentType)
	link.RequestedLibraryKind, link.RequestedLibraryKindSource = requestedLibraryIntentFromText(buf.requestedKind, buf.requestedKindSource)
	link.ContentSource = contentSourceFromString(buf.contentSource.String)
	applyLibraryScanFields(link, buf.libraryKind, buf.libraryKindSource, buf.predictedKind, buf.confidence, buf.classificationReason, buf.classificationExplanation, buf.classifierVersion, buf.firstCollectedAt, buf.lastRecollectedAt, buf.payloadPurgeDueAt, buf.payloadPurgedAt)
	link.PathDepth = intPointer(buf.pathDepth)
	link.ParentPath = textPointer(buf.parentPath)
	link.ParentID = uuidPointer(buf.parentID)

	images, err := unmarshalStringSlice(buf.inputImages)
	if err != nil {
		return fmt.Errorf("decode input images: %w", err)
	}
	link.InputImages = images

	metadata, err := unmarshalMetadata(buf.sourceMetadata)
	if err != nil {
		return fmt.Errorf("decode source metadata: %w", err)
	}
	link.SourceMetadata = metadata
	return nil
}

func scanLink(row rowScanner) (model.Link, error) {
	var (
		link model.Link
		buf  linkScanBuffers
	)
	if err := row.Scan(scanLinkFields(&link, &buf)...); err != nil {
		return link, err
	}
	if err := applyLinkScanBuffers(&link, &buf); err != nil {
		return link, err
	}
	return link, nil
}

// linkListScanBuffers is the trimmed counterpart of linkScanBuffers used by
// list/tree paths that select linkListColumns. The dropped scratch fields
// (sourceKind, sourceKey, inputTitle, inputText, inputHTML, inputImages,
// sourceMetadata) stay zero-valued on the returned model.Link — none of the
// list-path response mappers reference them.
type linkListScanBuffers struct {
	title                     pgtype.Text
	summary                   pgtype.Text
	fetcherType               pgtype.Text
	lowConfidenceReason       pgtype.Text
	errorMsg                  pgtype.Text
	description               pgtype.Text
	domain                    pgtype.Text
	contentType               pgtype.Text
	libraryKind               pgtype.Text
	libraryKindSource         pgtype.Text
	predictedKind             pgtype.Text
	confidence                pgtype.Float4
	classificationReason      pgtype.Text
	classificationExplanation pgtype.Text
	classifierVersion         pgtype.Text
	firstCollectedAt          pgtype.Timestamptz
	lastRecollectedAt         pgtype.Timestamptz
	payloadPurgeDueAt         pgtype.Timestamptz
	payloadPurgedAt           pgtype.Timestamptz
	pathDepth                 sqldb.NullInt64
	parentPath                pgtype.Text
	parentID                  pgtype.UUID
}

func scanLinkListFields(link *model.Link, buf *linkListScanBuffers) []any {
	return []any{
		&link.ID,
		&link.URL,
		&buf.title,
		&buf.summary,
		&link.Tags,
		&buf.fetcherType,
		&link.IsLowConfidence,
		&buf.lowConfidenceReason,
		&link.Status,
		&buf.errorMsg,
		&buf.description,
		&buf.domain,
		&buf.contentType,
		&buf.libraryKind,
		&buf.libraryKindSource,
		&link.LibraryKindLocked,
		&buf.predictedKind,
		&buf.confidence,
		&buf.classificationReason,
		&buf.classificationExplanation,
		&buf.classifierVersion,
		&link.ContentRevision,
		&link.MetadataRevision,
		&link.HasContent,
		&link.ContentCJKChars,
		&link.ContentWords,
		&buf.firstCollectedAt,
		&buf.lastRecollectedAt,
		&buf.payloadPurgeDueAt,
		&buf.payloadPurgedAt,
		&buf.pathDepth,
		&buf.parentPath,
		&buf.parentID,
		&link.CreatedAt,
		&link.UpdatedAt,
	}
}

func applyLinkListScanBuffers(link *model.Link, buf *linkListScanBuffers) {
	link.Title = textPointer(buf.title)
	link.Summary = textPointer(buf.summary)
	link.FetcherType = textPointer(buf.fetcherType)
	link.LowConfidenceReason = textPointer(buf.lowConfidenceReason)
	link.ErrorMsg = textPointer(buf.errorMsg)
	link.Description = textPointer(buf.description)
	link.Domain = textPointer(buf.domain)
	link.ContentType = textPointer(buf.contentType)
	applyLibraryScanFields(link, buf.libraryKind, buf.libraryKindSource, buf.predictedKind, buf.confidence, buf.classificationReason, buf.classificationExplanation, buf.classifierVersion, buf.firstCollectedAt, buf.lastRecollectedAt, buf.payloadPurgeDueAt, buf.payloadPurgedAt)
	link.PathDepth = intPointer(buf.pathDepth)
	link.ParentPath = textPointer(buf.parentPath)
	link.ParentID = uuidPointer(buf.parentID)
}

func applyLibraryScanFields(link *model.Link, kind, source, predicted pgtype.Text, confidence pgtype.Float4, reason, explanation, version pgtype.Text, firstCollectedAt, lastRecollectedAt, payloadPurgeDueAt, payloadPurgedAt pgtype.Timestamptz) {
	link.LibraryKind = libraryKindPointer(kind)
	link.LibraryKindSource = libraryKindSourcePointer(source)
	link.PredictedLibraryKind = libraryKindPointer(predicted)
	link.ClassificationConfidence = float32Pointer(confidence)
	link.ClassificationReason = textPointer(reason)
	link.ClassificationExplanation = textPointer(explanation)
	link.ClassifierVersion = textPointer(version)
	link.FirstCollectedAt = firstCollectedAt.Time
	link.LastRecollectedAt = timePointer(lastRecollectedAt)
	link.PayloadPurgeDueAt = timePointer(payloadPurgeDueAt)
	link.PayloadPurgedAt = timePointer(payloadPurgedAt)
}

func libraryKindPointer(value pgtype.Text) *model.LibraryKind {
	if !value.Valid {
		return nil
	}
	kind := model.LibraryKind(value.String)
	return &kind
}

func requestedLibraryIntentFromText(kind, source pgtype.Text) (model.RequestedLibraryKind, model.RequestedLibraryKindSource) {
	return normalizeRequestedLibraryIntent(model.RequestedLibraryKind(kind.String), model.RequestedLibraryKindSource(source.String))
}

func contentSourceFromString(value string) model.ContentSource {
	switch model.ContentSource(value) {
	case model.ContentSourceUser:
		return model.ContentSourceUser
	case model.ContentSourceFetched:
		return model.ContentSourceFetched
	default:
		// Historical rows and old mocks predate content_source. The migration
		// defaults them to fetched; keep the same compatibility rule at the
		// scan boundary for zero-valued fixtures.
		return model.ContentSourceFetched
	}
}

func libraryKindSourcePointer(value pgtype.Text) *model.LibraryKindSource {
	if !value.Valid {
		return nil
	}
	source := model.LibraryKindSource(value.String)
	return &source
}

func float32Pointer(value pgtype.Float4) *float32 {
	if !value.Valid {
		return nil
	}
	confidence := value.Float32
	return &confidence
}

func timePointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	timestamp := value.Time
	return &timestamp
}

func scanLinkList(row rowScanner) (model.Link, error) {
	var (
		link model.Link
		buf  linkListScanBuffers
	)
	if err := row.Scan(scanLinkListFields(&link, &buf)...); err != nil {
		return link, err
	}
	applyLinkListScanBuffers(&link, &buf)
	return link, nil
}

func scanLinkListWithTotal(row rowScanner) (model.Link, int, error) {
	var (
		link  model.Link
		buf   linkListScanBuffers
		total int64
	)
	dest := append(scanLinkListFields(&link, &buf), &total)
	if err := row.Scan(dest...); err != nil {
		return link, 0, err
	}
	applyLinkListScanBuffers(&link, &buf)
	return link, int(total), nil
}
