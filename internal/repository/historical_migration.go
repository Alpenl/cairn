package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/alloc"
	"webtag/internal/model"
	"webtag/internal/siteidentity"
)

type HistoricalMigrationCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type HistoricalMigrationCandidate struct {
	ID                        uuid.UUID
	URL                       string
	Title                     string
	ContentType               model.ContentType
	ContentRevision           int64
	MetadataRevision          int64
	CreatedAt                 time.Time
	RequestedKind             model.RequestedLibraryKind
	RequestedSource           model.RequestedLibraryKindSource
	LibraryKind               model.LibraryKind
	Source                    model.LibraryKindSource
	Locked                    bool
	PredictedKind             model.LibraryKind
	ClassificationConfidence  string
	ClassificationReason      string
	ClassificationExplanation string
	ClassifierVersion         string
	HasContent                bool
	HasTranslations           bool
}

func (c HistoricalMigrationCandidate) HasReadingAssets() bool {
	return c.HasContent || c.HasTranslations
}

type HistoricalMigrationAssessment struct {
	Candidate     HistoricalMigrationCandidate
	PredictedKind model.LibraryKind
	Confidence    float32
	Reason        string
	AutoMigrate   bool
	Suggest       bool
}

type HistoricalMigrationOutcome string

const (
	HistoricalMigrationOutcomeNoop         HistoricalMigrationOutcome = "noop"
	HistoricalMigrationOutcomeRetained     HistoricalMigrationOutcome = "retained"
	HistoricalMigrationOutcomeAutoMigrated HistoricalMigrationOutcome = "auto_migrated"
	HistoricalMigrationOutcomeSuggested    HistoricalMigrationOutcome = "suggested"
)

type HistoricalMigrationStore interface {
	ListHistoricalMigrationCandidates(context.Context, HistoricalMigrationCursor, int) ([]HistoricalMigrationCandidate, error)
	CommitHistoricalMigrationAssessment(context.Context, HistoricalMigrationAssessment) (HistoricalMigrationOutcome, error)
}

const (
	historicalMigrationClassifierVersion = "historical-migration-v1"
	historicalMigrationDecisionVersion   = 2

	currentHistoricalMigrationReviewIdentitySQL = `r.payload @> jsonb_build_object(
 'decision_version',2,'target_kind','site','url',l.url,'title',COALESCE(l.title,''),
 'content_type',COALESCE(l.content_type,'unknown'),'content_revision',l.content_revision,'metadata_revision',l.metadata_revision,
 'requested_library_kind',l.requested_library_kind,'requested_library_kind_source',l.requested_library_kind_source,
 'library_kind',l.library_kind,'library_kind_source',l.library_kind_source,'library_kind_locked',l.library_kind_locked,
 'predicted_library_kind',l.predicted_library_kind,'confidence',l.classification_confidence,
 'reason',COALESCE(l.classification_reason,''),'classification_explanation',COALESCE(l.classification_explanation,''),
 'classifier_version',COALESCE(l.classifier_version,''),
 'has_content',(l.content IS NOT NULL OR l.content_document IS NOT NULL),
 'has_translations',EXISTS (SELECT 1 FROM link_translations identity_translation WHERE identity_translation.link_id=l.id))`

	listHistoricalMigrationCandidatesSQL = `SELECT l.id, l.url, COALESCE(l.title, ''), COALESCE(l.content_type, 'unknown'), l.content_revision, l.created_at,
 l.metadata_revision, l.requested_library_kind, l.requested_library_kind_source,
 l.library_kind, l.library_kind_source, l.library_kind_locked,
 COALESCE(l.predicted_library_kind,''), COALESCE(l.classification_confidence::text,''), COALESCE(l.classification_reason,''),
 COALESCE(l.classification_explanation,''), COALESCE(l.classifier_version,''),
 (l.content IS NOT NULL OR l.content_document IS NOT NULL),
 EXISTS (SELECT 1 FROM link_translations t WHERE t.link_id=l.id)
FROM links l WHERE l.status='done' AND l.deleted_at IS NULL AND l.library_kind='reading' AND l.library_kind_source='migration' AND l.library_kind_locked=false
 AND l.requested_library_kind_source='auto'
 AND (l.classifier_version IS DISTINCT FROM 'historical-migration-v1' OR
      (l.classifier_version='historical-migration-v1' AND l.predicted_library_kind='site' AND NOT EXISTS (
       SELECT 1 FROM library_review_items r WHERE r.link_id=l.id AND r.kind='migration_suggestion' AND r.status='pending'
        AND ` + currentHistoricalMigrationReviewIdentitySQL + `
        AND ((l.content IS NOT NULL OR l.content_document IS NOT NULL) OR EXISTS (SELECT 1 FROM link_translations t WHERE t.link_id=l.id)))))
 AND (l.created_at > $1 OR (l.created_at=$1 AND l.id>$2)) ORDER BY l.created_at, l.id LIMIT $3`
	claimHistoricalMigrationCandidateSQL = `SELECT l.id, l.url, COALESCE(l.title, ''), COALESCE(l.content_type, 'unknown'), l.content_revision, l.created_at,
 l.metadata_revision, l.requested_library_kind, l.requested_library_kind_source,
 l.library_kind, l.library_kind_source, l.library_kind_locked,
 COALESCE(l.predicted_library_kind,''), COALESCE(l.classification_confidence::text,''), COALESCE(l.classification_reason,''),
 COALESCE(l.classification_explanation,''), COALESCE(l.classifier_version,''),
 (l.content IS NOT NULL OR l.content_document IS NOT NULL),
 EXISTS (SELECT 1 FROM link_translations t WHERE t.link_id=l.id)
FROM links l WHERE l.id=$1 AND l.status='done' AND l.deleted_at IS NULL AND l.library_kind='reading' AND l.library_kind_source='migration' AND l.library_kind_locked=false
 AND l.requested_library_kind_source='auto'
 AND (l.classifier_version IS DISTINCT FROM 'historical-migration-v1' OR
      (l.classifier_version='historical-migration-v1' AND l.predicted_library_kind='site' AND NOT EXISTS (
       SELECT 1 FROM library_review_items r WHERE r.link_id=l.id AND r.kind='migration_suggestion' AND r.status='pending'
        AND ` + currentHistoricalMigrationReviewIdentitySQL + `
        AND ((l.content IS NOT NULL OR l.content_document IS NOT NULL) OR EXISTS (SELECT 1 FROM link_translations t WHERE t.link_id=l.id)))))
FOR UPDATE OF l SKIP LOCKED`
	lockHistoricalMigrationReviewsSQL = `SELECT id FROM library_review_items
WHERE link_id=$1 AND kind='migration_suggestion' AND status='pending' ORDER BY id FOR UPDATE`
	recordHistoricalMigrationAssessmentSQL = `UPDATE links SET predicted_library_kind=$1, classification_confidence=$2, classification_reason=$3, classification_explanation=NULL, classifier_version='historical-migration-v1', updated_at=NOW() WHERE id=$4 AND library_kind='reading' AND library_kind_source='migration' AND library_kind_locked=false AND content_revision=$5`
	applyHistoricalSiteMigrationSQL        = `UPDATE links SET library_kind='site', library_kind_source='migration', library_kind_locked=false, summary=NULL,
 content=NULL, content_cjk_chars=0, content_words=0, content_document=NULL, content_format='plain', content_source='fetched', input_text=NULL, input_html=NULL, input_images=NULL, source_metadata=NULL,
 embedding=NULL, embedding_model=NULL, payload_purge_due_at=NULL, payload_purged_at=NOW(), content_revision=content_revision+1, updated_at=NOW()
WHERE id=$1`
	insertHistoricalMigrationReviewSQL = `INSERT INTO library_review_items (kind, link_id, payload, status, revision, created_at)
VALUES ('migration_suggestion', $1, $2::jsonb, 'pending', 1, NOW())
ON CONFLICT DO NOTHING`
	dismissStaleHistoricalMigrationReviewSQL = `UPDATE library_review_items SET status='dismissed',revision=revision+1,resolved_at=NOW()
WHERE link_id=$1 AND kind='migration_suggestion' AND status='pending'
 AND payload IS DISTINCT FROM $2::jsonb`
	dismissAllHistoricalMigrationReviewsSQL = `UPDATE library_review_items SET status='dismissed',revision=revision+1,resolved_at=NOW()
WHERE link_id=$1 AND kind='migration_suggestion' AND status='pending'`
	hasCurrentHistoricalMigrationReviewSQL = `SELECT EXISTS (SELECT 1 FROM library_review_items
WHERE link_id=$1 AND kind='migration_suggestion' AND status='pending'
 AND payload=$2::jsonb)`
	insertMigrationSiteSQL               = "INSERT INTO sites (site_key, name, name_source, intro, intro_source, homepage_url, homepage_source, first_collected_at, last_collected_at, created_at, updated_at) VALUES ($1,$2,'migration',$3,'migration',$4::text,CASE WHEN $4::text IS NULL THEN NULL ELSE 'migration' END,NOW(),NOW(),NOW(),NOW()) ON CONFLICT (site_key) DO UPDATE SET last_collected_at=NOW(),updated_at=NOW() RETURNING id,(xmax=0)"
	insertMigrationIdentitySQL           = "INSERT INTO site_identities (identity_key,site_id,source,locked,created_at,updated_at) VALUES ($1,$2,'migration',false,NOW(),NOW()) ON CONFLICT (identity_key) DO UPDATE SET updated_at=NOW() RETURNING site_id"
	insertMigrationEntrySQL              = "INSERT INTO site_entries (site_id,link_id,entry_name,entry_name_source,purpose,purpose_source,normalized_url,first_collected_at,created_at,updated_at) VALUES ($1,$2,$3,'migration','', 'migration',$4,NOW(),NOW(),NOW()) ON CONFLICT (link_id) DO UPDATE SET last_recollected_at=NOW(),updated_at=NOW() RETURNING id,site_id"
	lockMigrationReviewSQL               = "SELECT link_id, payload FROM library_review_items WHERE id=$1 AND revision=$2 AND status='pending' AND kind='migration_suggestion' FOR UPDATE"
	lockHistoricalMigrationReviewLinkSQL = `SELECT url, COALESCE(title,''), status, deleted_at IS NULL, COALESCE(content_type,'unknown'), content_revision, metadata_revision,
 requested_library_kind, requested_library_kind_source, library_kind, library_kind_source, library_kind_locked,
 COALESCE(predicted_library_kind,''), classification_confidence, COALESCE(classification_reason,''),
 COALESCE(classification_explanation,''), COALESCE(classifier_version,''),
 (content IS NOT NULL OR content_document IS NOT NULL),
 EXISTS (SELECT 1 FROM link_translations t WHERE t.link_id=links.id)
FROM links WHERE id=$1 FOR UPDATE`
	dismissLockedMigrationReviewSQL = `UPDATE library_review_items SET status='dismissed',revision=revision+1,resolved_at=NOW()
WHERE id=$1 AND revision=$2 AND status='pending' AND kind='migration_suggestion'`
	keepMigrationReadingSQL   = "UPDATE links SET requested_library_kind='reading', requested_library_kind_source='user', library_kind_source='user', library_kind_locked=true, updated_at=NOW() WHERE id=$1 AND library_kind='reading'"
	resolveMigrationReviewSQL = "UPDATE library_review_items SET status='applied', revision=revision+1, resolved_at=NOW() WHERE id=$1 AND revision=$2 AND status='pending' RETURNING " + libraryReviewColumns
)

func (r *PGXLinkRepository) ListHistoricalMigrationCandidates(ctx context.Context, cursor HistoricalMigrationCursor, limit int) ([]HistoricalMigrationCandidate, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := r.db.Query(ctx, listHistoricalMigrationCandidatesSQL, cursor.CreatedAt, cursor.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("list historical migration candidates: %w", err)
	}
	defer rows.Close()
	out := make([]HistoricalMigrationCandidate, 0, alloc.Hint(limit))
	for rows.Next() {
		var c HistoricalMigrationCandidate
		if err := rows.Scan(
			&c.ID, &c.URL, &c.Title, &c.ContentType, &c.ContentRevision, &c.CreatedAt,
			&c.MetadataRevision, &c.RequestedKind, &c.RequestedSource,
			&c.LibraryKind, &c.Source, &c.Locked,
			&c.PredictedKind, &c.ClassificationConfidence, &c.ClassificationReason,
			&c.ClassificationExplanation, &c.ClassifierVersion,
			&c.HasContent, &c.HasTranslations,
		); err != nil {
			return nil, fmt.Errorf("scan historical migration candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate historical migration candidates: %w", err)
	}
	return out, nil
}

func (r *PGXLinkRepository) CommitHistoricalMigrationAssessment(ctx context.Context, assessment HistoricalMigrationAssessment) (HistoricalMigrationOutcome, error) { //nolint:gocyclo // claim, assessment persistence, and the selected final action are one transaction.
	if assessment.Candidate.ID == uuid.Nil || assessment.AutoMigrate && assessment.Suggest {
		return HistoricalMigrationOutcomeNoop, fmt.Errorf("commit historical migration assessment: invalid assessment")
	}
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return HistoricalMigrationOutcomeNoop, fmt.Errorf("begin historical migration assessment: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return HistoricalMigrationOutcomeNoop, err
	}
	if err := lockHistoricalMigrationReviews(ctx, tx, assessment.Candidate.ID); err != nil {
		return HistoricalMigrationOutcomeNoop, err
	}

	var current HistoricalMigrationCandidate
	err = tx.QueryRow(ctx, claimHistoricalMigrationCandidateSQL, assessment.Candidate.ID).Scan(
		&current.ID, &current.URL, &current.Title, &current.ContentType, &current.ContentRevision, &current.CreatedAt,
		&current.MetadataRevision, &current.RequestedKind, &current.RequestedSource,
		&current.LibraryKind, &current.Source, &current.Locked,
		&current.PredictedKind, &current.ClassificationConfidence, &current.ClassificationReason,
		&current.ClassificationExplanation, &current.ClassifierVersion,
		&current.HasContent, &current.HasTranslations,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return HistoricalMigrationOutcomeNoop, nil
	}
	if err != nil {
		return HistoricalMigrationOutcomeNoop, fmt.Errorf("claim historical migration candidate: %w", err)
	}
	if !sameHistoricalMigrationCandidate(current, assessment.Candidate) {
		return HistoricalMigrationOutcomeNoop, nil
	}

	outcome := HistoricalMigrationOutcomeRetained
	switch {
	case assessment.AutoMigrate:
		if assessment.PredictedKind != model.LibraryKindSite || current.HasReadingAssets() {
			return HistoricalMigrationOutcomeNoop, nil
		}
		outcome = HistoricalMigrationOutcomeAutoMigrated
	case assessment.Suggest:
		if assessment.PredictedKind != model.LibraryKindSite || !current.HasReadingAssets() {
			return HistoricalMigrationOutcomeNoop, nil
		}
		outcome = HistoricalMigrationOutcomeSuggested
	case assessment.PredictedKind != model.LibraryKindReading:
		return HistoricalMigrationOutcomeNoop, fmt.Errorf("commit historical migration assessment: invalid retained assessment")
	}

	tag, err := tx.Exec(ctx, recordHistoricalMigrationAssessmentSQL,
		assessment.PredictedKind, nullableMigrationConfidence(assessment.Confidence), nullableMigrationReason(assessment.Reason),
		current.ID, current.ContentRevision)
	if err != nil {
		return HistoricalMigrationOutcomeNoop, fmt.Errorf("record historical migration assessment: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return HistoricalMigrationOutcomeNoop, nil
	}

	switch outcome {
	case HistoricalMigrationOutcomeAutoMigrated:
		if err := dismissAllHistoricalMigrationReviews(ctx, tx, current.ID); err != nil {
			return HistoricalMigrationOutcomeNoop, err
		}
		if err := applyHistoricalSiteMigrationOn(ctx, tx, current.ID, current.URL, current.Title); err != nil {
			return HistoricalMigrationOutcomeNoop, err
		}
	case HistoricalMigrationOutcomeSuggested:
		if err := ensureCurrentHistoricalMigrationSuggestion(ctx, tx, current, assessment); err != nil {
			return HistoricalMigrationOutcomeNoop, err
		}
	case HistoricalMigrationOutcomeRetained:
		if err := dismissAllHistoricalMigrationReviews(ctx, tx, current.ID); err != nil {
			return HistoricalMigrationOutcomeNoop, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return HistoricalMigrationOutcomeNoop, fmt.Errorf("commit historical migration assessment: %w", err)
	}
	return outcome, nil
}

func lockHistoricalMigrationReviews(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) error {
	rows, err := tx.Query(ctx, lockHistoricalMigrationReviewsSQL, linkID)
	if err != nil {
		return fmt.Errorf("lock historical migration reviews: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var reviewID uuid.UUID
		if err := rows.Scan(&reviewID); err != nil {
			return fmt.Errorf("scan historical migration review lock: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate historical migration review locks: %w", err)
	}
	return nil
}

func sameHistoricalMigrationCandidate(current, assessed HistoricalMigrationCandidate) bool {
	if !current.CreatedAt.Equal(assessed.CreatedAt) {
		return false
	}
	current.CreatedAt = time.Time{}
	assessed.CreatedAt = time.Time{}
	return current == assessed
}

func applyHistoricalSiteMigrationOn(ctx context.Context, tx pgx.Tx, linkID uuid.UUID, rawURL, title string) error {
	identity, err := siteidentity.FromURL(rawURL)
	if err != nil {
		return fmt.Errorf("derive migration site identity: %w", err)
	}
	name := strings.TrimSpace(title)
	if name == "" {
		name = identity.Host
	}
	if _, err := tx.Exec(ctx, applyHistoricalSiteMigrationSQL, linkID); err != nil {
		return fmt.Errorf("apply historical site migration: %w", err)
	}
	siteID, err := aggregateMigrationSiteOn(ctx, tx, linkID, identity.Key, identity.NormalizedURL, name)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO site_tags (site_id,tag,normalized_tag,source,created_at,updated_at) SELECT $1,tag,lower(tag),'migration',NOW(),NOW() FROM unnest((SELECT tags FROM links WHERE id=$2)) tag WHERE btrim(tag)<>'' ON CONFLICT (site_id,normalized_tag) DO NOTHING", siteID, linkID); err != nil {
		return fmt.Errorf("copy migration tags: %w", err)
	}
	return nil
}

type historicalMigrationReviewPayload struct {
	DecisionVersion            int                              `json:"decision_version"`
	TargetKind                 model.LibraryKind                `json:"target_kind"`
	Reason                     string                           `json:"reason"`
	Confidence                 float32                          `json:"confidence"`
	Title                      string                           `json:"title"`
	URL                        string                           `json:"url"`
	ContentType                model.ContentType                `json:"content_type"`
	ContentRevision            int64                            `json:"content_revision"`
	MetadataRevision           int64                            `json:"metadata_revision"`
	RequestedLibraryKind       model.RequestedLibraryKind       `json:"requested_library_kind"`
	RequestedLibraryKindSource model.RequestedLibraryKindSource `json:"requested_library_kind_source"`
	LibraryKind                model.LibraryKind                `json:"library_kind"`
	LibraryKindSource          model.LibraryKindSource          `json:"library_kind_source"`
	LibraryKindLocked          bool                             `json:"library_kind_locked"`
	PredictedLibraryKind       model.LibraryKind                `json:"predicted_library_kind"`
	ClassificationExplanation  string                           `json:"classification_explanation"`
	ClassifierVersion          string                           `json:"classifier_version"`
	HasContent                 bool                             `json:"has_content"`
	HasTranslations            bool                             `json:"has_translations"`
}

func newHistoricalMigrationReviewPayload(current HistoricalMigrationCandidate, assessment HistoricalMigrationAssessment) historicalMigrationReviewPayload {
	return historicalMigrationReviewPayload{
		DecisionVersion: historicalMigrationDecisionVersion, TargetKind: model.LibraryKindSite,
		Reason: assessment.Reason, Confidence: assessment.Confidence,
		Title: current.Title, URL: current.URL, ContentType: current.ContentType,
		ContentRevision: current.ContentRevision, MetadataRevision: current.MetadataRevision,
		RequestedLibraryKind: current.RequestedKind, RequestedLibraryKindSource: current.RequestedSource,
		LibraryKind: current.LibraryKind, LibraryKindSource: current.Source, LibraryKindLocked: current.Locked,
		PredictedLibraryKind: assessment.PredictedKind, ClassificationExplanation: "",
		ClassifierVersion: historicalMigrationClassifierVersion,
		HasContent:        current.HasContent, HasTranslations: current.HasTranslations,
	}
}

func ensureCurrentHistoricalMigrationSuggestion(ctx context.Context, tx pgx.Tx, current HistoricalMigrationCandidate, assessment HistoricalMigrationAssessment) error {
	payload, err := json.Marshal(newHistoricalMigrationReviewPayload(current, assessment))
	if err != nil {
		return fmt.Errorf("encode historical migration suggestion: %w", err)
	}
	if _, err := tx.Exec(ctx, dismissStaleHistoricalMigrationReviewSQL, current.ID, payload); err != nil {
		return fmt.Errorf("dismiss stale historical migration suggestion: %w", err)
	}
	if _, err := tx.Exec(ctx, insertHistoricalMigrationReviewSQL, current.ID, payload); err != nil {
		return fmt.Errorf("create historical migration suggestion: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, hasCurrentHistoricalMigrationReviewSQL, assessment.Candidate.ID, payload).Scan(&exists); err != nil {
		return fmt.Errorf("verify historical migration suggestion: %w", err)
	}
	if !exists {
		return fmt.Errorf("verify historical migration suggestion: current revision is missing")
	}
	return nil
}

func dismissAllHistoricalMigrationReviews(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) error {
	if _, err := tx.Exec(ctx, dismissAllHistoricalMigrationReviewsSQL, linkID); err != nil {
		return fmt.Errorf("dismiss historical migration suggestions: %w", err)
	}
	return nil
}

func aggregateMigrationSiteOn(ctx context.Context, tx pgx.Tx, linkID uuid.UUID, identityKey, normalizedURL, name string) (uuid.UUID, error) {
	var siteID, candidateID uuid.UUID
	var created bool
	err := tx.QueryRow(ctx, findSiteIdentityForUpdateSQL, identityKey).Scan(&siteID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = tx.QueryRow(ctx, insertMigrationSiteSQL, identityKey, name, "", nil).Scan(&candidateID, &created); err != nil {
			return uuid.Nil, fmt.Errorf("insert migration site: %w", err)
		}
		if err = tx.QueryRow(ctx, insertMigrationIdentitySQL, identityKey, candidateID).Scan(&siteID); err != nil {
			return uuid.Nil, fmt.Errorf("bind migration site identity: %w", err)
		}
		if siteID != candidateID && created {
			if _, err = tx.Exec(ctx, deleteUnboundSiteSQL, candidateID); err != nil {
				return uuid.Nil, fmt.Errorf("delete migration candidate site: %w", err)
			}
		}
		created = created && siteID == candidateID
	} else if err != nil {
		return uuid.Nil, fmt.Errorf("find migration site identity: %w", err)
	}
	entryID, entrySiteID, err := upsertMigrationSiteEntry(ctx, tx, siteID, linkID, name, normalizedURL)
	if err != nil {
		return uuid.Nil, err
	}
	if _, err = tx.Exec(ctx, setPrimarySiteEntrySQL, entryID, entrySiteID); err != nil {
		return uuid.Nil, fmt.Errorf("set migration primary entry: %w", err)
	}
	if !created {
		if _, err = tx.Exec(ctx, invalidateSiteEmbeddingSQL, entrySiteID); err != nil {
			return uuid.Nil, fmt.Errorf("invalidate migration site embedding: %w", err)
		}
	}
	return entrySiteID, nil
}

func nullableMigrationConfidence(value float32) any {
	if value <= 0 {
		return nil
	}
	return value
}
func nullableMigrationReason(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

var _ HistoricalMigrationStore = (*PGXLinkRepository)(nil)
var _ MigrationReviewActionExecutor = (*PGXLinkRepository)(nil)

type historicalMigrationReviewLinkState struct {
	URL                       string
	Title                     string
	Status                    string
	Active                    bool
	ContentType               model.ContentType
	ContentRevision           int64
	MetadataRevision          int64
	RequestedKind             model.RequestedLibraryKind
	RequestedSource           model.RequestedLibraryKindSource
	LibraryKind               model.LibraryKind
	LibraryKindSource         model.LibraryKindSource
	LibraryKindLocked         bool
	PredictedKind             model.LibraryKind
	ClassificationConfidence  pgtype.Float4
	ClassificationReason      string
	ClassificationExplanation string
	ClassifierVersion         string
	HasContent                bool
	HasTranslations           bool
}

func lockHistoricalMigrationReviewLink(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) (historicalMigrationReviewLinkState, bool, error) {
	var current historicalMigrationReviewLinkState
	err := tx.QueryRow(ctx, lockHistoricalMigrationReviewLinkSQL, linkID).Scan(
		&current.URL, &current.Title, &current.Status, &current.Active, &current.ContentType,
		&current.ContentRevision, &current.MetadataRevision, &current.RequestedKind, &current.RequestedSource,
		&current.LibraryKind, &current.LibraryKindSource, &current.LibraryKindLocked,
		&current.PredictedKind, &current.ClassificationConfidence, &current.ClassificationReason,
		&current.ClassificationExplanation, &current.ClassifierVersion,
		&current.HasContent, &current.HasTranslations,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return historicalMigrationReviewLinkState{}, false, nil
	}
	if err != nil {
		return historicalMigrationReviewLinkState{}, false, fmt.Errorf("lock historical migration review link: %w", err)
	}
	return current, true, nil
}

func isCurrentHistoricalMigrationReview(raw []byte, current historicalMigrationReviewLinkState) bool {
	var payload historicalMigrationReviewPayload
	if json.Unmarshal(raw, &payload) != nil || !current.ClassificationConfidence.Valid {
		return false
	}
	eligibility := struct {
		Active            bool
		Status            string
		RequestedSource   model.RequestedLibraryKindSource
		LibraryKind       model.LibraryKind
		LibraryKindSource model.LibraryKindSource
		LibraryKindLocked bool
		PredictedKind     model.LibraryKind
		Reason            string
		Explanation       string
		ClassifierVersion string
	}{
		current.Active, current.Status, current.RequestedSource, current.LibraryKind, current.LibraryKindSource,
		current.LibraryKindLocked, current.PredictedKind, current.ClassificationReason,
		current.ClassificationExplanation, current.ClassifierVersion,
	}
	wantEligibility := struct {
		Active            bool
		Status            string
		RequestedSource   model.RequestedLibraryKindSource
		LibraryKind       model.LibraryKind
		LibraryKindSource model.LibraryKindSource
		LibraryKindLocked bool
		PredictedKind     model.LibraryKind
		Reason            string
		Explanation       string
		ClassifierVersion string
	}{
		true, string(model.LinkStatusDone), model.RequestedLibraryKindSourceAuto, model.LibraryKindReading,
		model.LibraryKindSourceMigration, false, model.LibraryKindSite, "migration_assets_require_review", "",
		historicalMigrationClassifierVersion,
	}
	if eligibility != wantEligibility || !current.HasContent && !current.HasTranslations {
		return false
	}
	expected := historicalMigrationReviewPayload{
		DecisionVersion: historicalMigrationDecisionVersion, TargetKind: model.LibraryKindSite,
		Reason: current.ClassificationReason, Confidence: current.ClassificationConfidence.Float32,
		Title: current.Title, URL: current.URL, ContentType: current.ContentType,
		ContentRevision: current.ContentRevision, MetadataRevision: current.MetadataRevision,
		RequestedLibraryKind: current.RequestedKind, RequestedLibraryKindSource: current.RequestedSource,
		LibraryKind: current.LibraryKind, LibraryKindSource: current.LibraryKindSource,
		LibraryKindLocked: current.LibraryKindLocked, PredictedLibraryKind: current.PredictedKind,
		ClassificationExplanation: current.ClassificationExplanation, ClassifierVersion: current.ClassifierVersion,
		HasContent: current.HasContent, HasTranslations: current.HasTranslations,
	}
	return payload == expected
}

func dismissInvalidHistoricalMigrationReview(ctx context.Context, tx pgx.Tx, reviewID uuid.UUID, revision int64) (*model.LibraryReviewItem, error) {
	tag, err := tx.Exec(ctx, dismissLockedMigrationReviewSQL, reviewID, revision)
	if err != nil {
		return nil, fmt.Errorf("dismiss invalid historical migration review: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrRevisionConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit invalid historical migration review dismissal: %w", err)
	}
	return nil, ErrRevisionConflict
}

func (r *PGXLinkRepository) KeepHistoricalMigrationReading(ctx context.Context, reviewID uuid.UUID, revision int64) (*model.LibraryReviewItem, error) {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin keep migration reading: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return nil, err
	}
	var linkID uuid.UUID
	var payload []byte
	if err := tx.QueryRow(ctx, lockMigrationReviewSQL, reviewID, revision).Scan(&linkID, &payload); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRevisionConflict
		}
		return nil, err
	}
	current, found, err := lockHistoricalMigrationReviewLink(ctx, tx, linkID)
	if err != nil {
		return nil, err
	}
	if !found || !isCurrentHistoricalMigrationReview(payload, current) {
		return dismissInvalidHistoricalMigrationReview(ctx, tx, reviewID, revision)
	}
	if tag, err := tx.Exec(ctx, keepMigrationReadingSQL, linkID); err != nil {
		return nil, err
	} else if tag.RowsAffected() != 1 {
		return nil, ErrRevisionConflict
	}
	item, err := scanLibraryReview(tx.QueryRow(ctx, resolveMigrationReviewSQL, reviewID, revision))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRevisionConflict
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &item, nil
}

//nolint:gocyclo // one transaction must keep review lock, conversion, aggregation, tag copy, and resolution atomic.
func (r *PGXLinkRepository) MoveHistoricalMigrationToSite(ctx context.Context, reviewID uuid.UUID, revision int64) (*model.LibraryReviewItem, error) {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return nil, err
	}
	var linkID uuid.UUID
	var payload []byte
	if err := tx.QueryRow(ctx, lockMigrationReviewSQL, reviewID, revision).Scan(&linkID, &payload); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRevisionConflict
		}
		return nil, err
	}
	current, found, err := lockHistoricalMigrationReviewLink(ctx, tx, linkID)
	if err != nil {
		return nil, err
	}
	if !found || !isCurrentHistoricalMigrationReview(payload, current) {
		return dismissInvalidHistoricalMigrationReview(ctx, tx, reviewID, revision)
	}
	if _, err := convertReadingToSiteOn(ctx, tx, ConvertLinkParams{LinkID: linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: current.ContentRevision}, current.URL, current.Title, ""); err != nil {
		return nil, err
	}
	item, err := scanLibraryReview(tx.QueryRow(ctx, resolveMigrationReviewSQL, reviewID, revision))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRevisionConflict
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &item, nil
}

// upsertMigrationSiteEntry 复用或创建历史迁移的 site entry。
//
// 同 aggregateSiteOn：site_entries 有 (link_id) 与 (site_id, normalized_url) 两个
// 唯一索引，ON CONFLICT 只能推断前者。历史数据里同一站点的两个 URL 变体会带着新的
// link_id 插入，绕过 (link_id) 冲突而直接撞上 idx_site_entries_site_url 抛 23505。
func upsertMigrationSiteEntry(ctx context.Context, tx pgx.Tx, siteID, linkID uuid.UUID, name, normalizedURL string) (uuid.UUID, uuid.UUID, error) {
	var entryID, entrySiteID uuid.UUID
	switch err := tx.QueryRow(ctx, findSiteEntryByNormalizedURLSQL, siteID, normalizedURL, linkID).
		Scan(&entryID, &entrySiteID); {
	case err == nil:
		if _, err := tx.Exec(ctx, touchSiteEntrySQL, entryID); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("touch migration site entry: %w", err)
		}
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx, insertMigrationEntrySQL, siteID, linkID, name, normalizedURL).
			Scan(&entryID, &entrySiteID); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("insert migration site entry: %w", err)
		}
	default:
		return uuid.Nil, uuid.Nil, fmt.Errorf("find migration site entry by normalized url: %w", err)
	}
	return entryID, entrySiteID, nil
}
