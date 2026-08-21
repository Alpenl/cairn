// Package durablework owns transactions that span business rows and River
// jobs. Its service-facing commands never expose pgx transactions; only this
// concrete adapter and its infrastructure ports know about them.
package durablework

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/contentdoc"
	"webtag/internal/database"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

const maxSelectionTranslationUTF16Units = 16_384

// TranslationQueue is the infrastructure-only River port. pgx stays here and
// never appears on the linktranslation service-facing scheduler interface.
type TranslationQueue interface {
	EnqueueTranslationTx(context.Context, pgx.Tx, model.TranslationScheduleCommand) (int64, error)
}

type TranslationSchedulerOptions struct {
	Transactions database.TxBeginner
	Products     *repository.PGXTranslationRepository
	Queue        TranslationQueue
}

type TranslationScheduler struct {
	transactions database.TxBeginner
	products     *repository.PGXTranslationRepository
	queue        TranslationQueue
	now          func() time.Time
}

func NewTranslationScheduler(opts TranslationSchedulerOptions) *TranslationScheduler {
	return &TranslationScheduler{
		transactions: opts.Transactions,
		products:     opts.Products,
		queue:        opts.Queue,
		now:          func() time.Time { return time.Now().UTC() },
	}
}

// Schedule validates source identity and creates a translation attempt and
// River job in one database transaction.
//
//nolint:gocyclo // The transaction keeps source CAS, product reuse, attempt supersession, and River insertion in one visible atomic sequence.
func (s *TranslationScheduler) Schedule(
	ctx context.Context,
	linkID uuid.UUID,
	req model.TranslationRequest,
	stallAfter time.Duration,
) (*model.LinkTranslation, error) {
	if s == nil || s.transactions == nil || s.products == nil || s.queue == nil {
		return nil, errors.New("schedule translation: durable scheduler is not configured")
	}

	var (
		result    *model.LinkTranslation
		scheduled bool
	)
	err := database.WithTx(ctx, s.transactions, func(tx pgx.Tx) error {
		source, err := s.products.LockTranslationSourceTx(ctx, tx, linkID)
		if err != nil {
			return err
		}
		if source == nil {
			return httperr.NewWithCode(http.StatusNotFound, httperr.CodeLinkNotFound, "link not found")
		}
		params, err := authoritativeTranslationParams(source, linkID, req)
		if err != nil {
			return err
		}
		params.StallAfter = stallAfter

		existing, err := s.products.FindTranslationIdentityTx(ctx, tx, params)
		if err != nil {
			return err
		}
		if reusableTranslation(existing, params.Force, s.now(), stallAfter) {
			result = existing
			return nil
		}

		var previous *model.TranslationAttempt
		if existing == nil {
			result, scheduled, err = s.products.InsertPendingTranslationTx(ctx, tx, params)
			if err != nil {
				return err
			}
			if !scheduled {
				result, err = s.products.FindTranslationIdentityTx(ctx, tx, params)
				if err != nil {
					return err
				}
				if result == nil {
					return errors.New("schedule translation: conflict row disappeared")
				}
				if reusableTranslation(result, params.Force, s.now(), stallAfter) {
					return nil
				}
				existing = result
			}
		}

		if !scheduled {
			previous = supersededTranslationAttempt(existing)
			result, err = s.products.AdvancePendingTranslationTx(ctx, tx, existing.ID)
			if err != nil {
				return err
			}
			scheduled = true
		}

		seed := model.TranslationAttemptSeed{
			TranslationID:         result.ID,
			AttemptGeneration:     result.AttemptGeneration,
			SourceHash:            result.SourceHash,
			SourceContentRevision: result.SourceContentRevision,
		}
		riverJobID, err := s.queue.EnqueueTranslationTx(ctx, tx, model.TranslationScheduleCommand{
			Seed:     seed,
			Previous: previous,
		})
		if err != nil {
			return err
		}
		if riverJobID <= 0 {
			return fmt.Errorf("schedule translation: invalid River job ID %d", riverJobID)
		}
		result, err = s.products.BindCurrentTranslationAttemptTx(ctx, tx, seed, riverJobID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("schedule translation: %w", err)
	}
	if result == nil {
		return nil, errors.New("schedule translation: transaction returned nil product")
	}
	return result, nil
}

//nolint:gocyclo // Scope and block branches are the closed source-identity protocol matrix; splitting them would scatter validation and stable conflict mapping.
func authoritativeTranslationParams(
	source *repository.TranslationSourceSnapshot,
	linkID uuid.UUID,
	req model.TranslationRequest,
) (repository.UpsertTranslationParams, error) {
	if err := rejectStaleSavedRevision(source, req); err != nil {
		return repository.UpsertTranslationParams{}, err
	}
	if source.LibraryKind != nil && *source.LibraryKind == model.LibraryKindSite {
		return repository.UpsertTranslationParams{}, httperr.NewWithCode(
			http.StatusConflict,
			httperr.CodeSiteOriginalContentForbidden,
			"website entries cannot be translated",
		)
	}
	if source.Status != model.LinkStatusDone {
		return repository.UpsertTranslationParams{}, httperr.NewWithCode(
			http.StatusConflict,
			httperr.CodeLinkNotReady,
			"link not parsed yet",
		)
	}

	params := repository.UpsertTranslationParams{
		LinkID:         linkID,
		Scope:          req.Scope,
		TargetLanguage: model.TranslationTargetChinese,
		Force:          req.Force,
	}
	switch req.Scope {
	case model.TranslationScopeFull:
		if req.ExpectedSourceHash != nil {
			return repository.UpsertTranslationParams{}, invalidTranslationRequest("saved content uses expected_content_revision")
		}
		text, format, blockKey, ok := fullSavedTranslationSource(source)
		if !ok {
			return repository.UpsertTranslationParams{}, httperr.NewWithCode(
				http.StatusConflict,
				httperr.CodeTranslationContentUnavailable,
				"save original before translating the full article",
			)
		}
		revision, err := verifiedSavedRevision(source, req.ExpectedContentRevision)
		if err != nil {
			return repository.UpsertTranslationParams{}, err
		}
		params.BlockKey = blockKey
		params.SourceText = text
		params.SourceFormat = format
		params.EndOffset = utf16Length(text)
		params.SourceHash = hashTranslationSource(text)
		params.SourceContentRevision = revision
		return params, nil

	case model.TranslationScopeSelection:
		if !validSelectionShape(req) {
			return repository.UpsertTranslationParams{}, invalidTranslationRequest("invalid selected text translation")
		}
		params.BlockKey = req.BlockKey
		params.StartOffset = req.StartOffset
		params.EndOffset = req.EndOffset
		params.SourceText = req.SourceText
		params.SourceFormat = model.TranslationFormatPlain

		switch req.BlockKey {
		case "content", "content-document":
			if req.ExpectedSourceHash != nil {
				return repository.UpsertTranslationParams{}, invalidTranslationRequest("saved content uses expected_content_revision")
			}
			revision, err := verifiedSavedRevision(source, req.ExpectedContentRevision)
			if err != nil {
				return repository.UpsertTranslationParams{}, err
			}
			format, canonical, err := savedSelectionCanonicalSource(source, req.BlockKey)
			if err != nil {
				return repository.UpsertTranslationParams{}, sourceBlockConflictError(source, req.BlockKey, nil)
			}
			if err := contentdoc.ValidateRenderedSelection(format, canonical, req.StartOffset, req.EndOffset, req.SourceText); err != nil {
				return repository.UpsertTranslationParams{}, sourceBlockConflictError(source, req.BlockKey, nil)
			}
			params.SourceHash = hashTranslationSource(req.SourceText)
			params.SourceContentRevision = revision
			return params, nil

		case "summary":
			if req.ExpectedContentRevision != nil {
				return repository.UpsertTranslationParams{}, invalidTranslationRequest("summary uses expected_source_hash")
			}
			if source.Summary == nil || strings.TrimSpace(*source.Summary) == "" {
				return repository.UpsertTranslationParams{}, sourceBlockConflictError(source, req.BlockKey, nil)
			}
			projection, err := contentdoc.RenderedBlockProjection(model.ContentFormatMarkdown, *source.Summary)
			if err != nil {
				return repository.UpsertTranslationParams{}, fmt.Errorf("project summary translation source: %w", err)
			}
			currentHash := hashTranslationSource(projection)
			switch {
			case req.ExpectedSourceHash == nil:
				return repository.UpsertTranslationParams{}, invalidTranslationRequest("expected_source_hash is required for summary")
			case !strings.EqualFold(strings.TrimSpace(*req.ExpectedSourceHash), currentHash):
				return repository.UpsertTranslationParams{}, sourceBlockConflictError(source, req.BlockKey, &currentHash)
			default:
				params.SourceHash = contentdoc.RenderedSourceBlockPersistenceHash(req.BlockKey, projection)
			}
			if err := contentdoc.ValidateRenderedSelection(model.ContentFormatMarkdown, *source.Summary, req.StartOffset, req.EndOffset, req.SourceText); err != nil {
				return repository.UpsertTranslationParams{}, sourceBlockConflictError(source, req.BlockKey, &currentHash)
			}
			return params, nil

		default:
			return repository.UpsertTranslationParams{}, invalidTranslationRequest("translation block is not available")
		}
	default:
		return repository.UpsertTranslationParams{}, invalidTranslationRequest("translation scope must be selection or full")
	}
}

func verifiedSavedRevision(
	source *repository.TranslationSourceSnapshot,
	expected *int64,
) (*int64, error) {
	if expected == nil {
		return nil, invalidTranslationRequest("expected_content_revision is required for saved content")
	}
	if *expected != source.ContentRevision {
		revision := source.ContentRevision
		return nil, httperr.NewWithCodeAndCurrentIdentity(
			http.StatusConflict,
			httperr.CodeContentRevisionConflict,
			"saved content changed; refresh and confirm the translation again",
			httperr.ConflictIdentity{ContentRevision: &revision, BlockKey: canonicalSavedBlockKey(source)},
		)
	}
	revision := source.ContentRevision
	return &revision, nil
}

func rejectStaleSavedRevision(
	source *repository.TranslationSourceSnapshot,
	req model.TranslationRequest,
) error {
	if req.ExpectedContentRevision == nil {
		return nil
	}
	if req.Scope != model.TranslationScopeFull &&
		(req.Scope != model.TranslationScopeSelection ||
			(req.BlockKey != "content" && req.BlockKey != "content-document")) {
		return nil
	}
	_, err := verifiedSavedRevision(source, req.ExpectedContentRevision)
	return err
}

func canonicalSavedBlockKey(source *repository.TranslationSourceSnapshot) string {
	if source.ContentFormat == model.ContentFormatMarkdown && source.ContentDocument != nil &&
		strings.TrimSpace(*source.ContentDocument) != "" {
		return "content-document"
	}
	return "content"
}

func savedSelectionCanonicalSource(source *repository.TranslationSourceSnapshot, blockKey string) (model.ContentFormat, string, error) {
	switch blockKey {
	case "content":
		if source.Content == nil || strings.TrimSpace(*source.Content) == "" ||
			(source.ContentDocument != nil && strings.TrimSpace(*source.ContentDocument) != "") {
			return "", "", contentdoc.ErrSelectionMismatch
		}
		return model.ContentFormatPlain, *source.Content, nil
	case "content-document":
		if source.ContentFormat != model.ContentFormatMarkdown || source.ContentDocument == nil ||
			strings.TrimSpace(*source.ContentDocument) == "" {
			return "", "", contentdoc.ErrSelectionMismatch
		}
		return model.ContentFormatMarkdown, *source.ContentDocument, nil
	default:
		return "", "", contentdoc.ErrSelectionMismatch
	}
}

func fullSavedTranslationSource(source *repository.TranslationSourceSnapshot) (string, model.TranslationFormat, string, bool) {
	if source.ContentFormat == model.ContentFormatMarkdown && source.ContentDocument != nil &&
		strings.TrimSpace(*source.ContentDocument) != "" {
		return *source.ContentDocument, model.TranslationFormatMarkdown, "content-document", true
	}
	if source.Content == nil || strings.TrimSpace(*source.Content) == "" {
		return "", "", "", false
	}
	return *source.Content, model.TranslationFormatPlain, "content", true
}

func validSelectionShape(req model.TranslationRequest) bool {
	if req.StartOffset < 0 || req.EndOffset <= req.StartOffset || !utf8.ValidString(req.SourceText) ||
		utf16Length(req.SourceText) > maxSelectionTranslationUTF16Units {
		return false
	}
	return utf16Length(trimECMAScriptWhitespace(req.SourceText)) >= 2
}

// trimECMAScriptWhitespace mirrors String.prototype.trim. Unicode White_Space
// is close but not identical: ECMAScript includes U+FEFF and excludes U+0085.
func trimECMAScriptWhitespace(text string) string {
	return strings.TrimFunc(text, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ',
			'\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
			return true
		default:
			return r >= '\u2000' && r <= '\u200a'
		}
	})
}

func reusableTranslation(item *model.LinkTranslation, force bool, now time.Time, stallAfter time.Duration) bool {
	if item == nil {
		return false
	}
	if item.Status == model.TranslationStatusPending || item.Status == model.TranslationStatusProcessing {
		return item.UpdatedAt.IsZero() || stallAfter <= 0 || now.Sub(item.UpdatedAt) <= stallAfter
	}
	return item.Status == model.TranslationStatusDone && !force
}

func supersededTranslationAttempt(item *model.LinkTranslation) *model.TranslationAttempt {
	if item == nil || item.CurrentRiverJobID == nil || *item.CurrentRiverJobID <= 0 ||
		(item.Status != model.TranslationStatusPending && item.Status != model.TranslationStatusProcessing) {
		return nil
	}
	return &model.TranslationAttempt{
		TranslationID:         item.ID,
		AttemptGeneration:     item.AttemptGeneration,
		RiverJobID:            *item.CurrentRiverJobID,
		SourceHash:            item.SourceHash,
		SourceContentRevision: item.SourceContentRevision,
	}
}

func invalidTranslationRequest(message string) error {
	return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeTranslationInvalidRequest, message)
}

func sourceBlockConflictError(
	source *repository.TranslationSourceSnapshot,
	blockKey string,
	currentHash *string,
) error {
	identity := httperr.ConflictIdentity{BlockKey: blockKey}
	if blockKey == "content" || blockKey == "content-document" {
		revision := source.ContentRevision
		identity.ContentRevision = &revision
		identity.BlockKey = canonicalSavedBlockKey(source)
	} else {
		identity.SourceHash = currentHash
	}
	return httperr.NewWithCodeAndCurrentIdentity(
		http.StatusConflict,
		httperr.CodeSourceBlockConflict,
		"translation source block changed; refresh and confirm the selection again",
		identity,
	)
}

func hashTranslationSource(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}

func utf16Length(text string) int {
	return len(utf16.Encode([]rune(text)))
}
