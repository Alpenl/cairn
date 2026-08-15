package dbintegration

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

// TestSavedContentGenerationMatrix exercises every repository command that
// can create, replace, clear, or preserve the saved body. The generation is a
// durable source identity used by Reader caches, translations, and content
// annotations, so these assertions deliberately require an exact delta rather
// than merely checking that the value changed.
func TestSavedContentGenerationMatrix(t *testing.T) { //nolint:gocyclo // each table row pins a distinct public transaction seam
	tests := []struct {
		name   string
		mutate func(*testing.T, *savedGenerationHarness) savedGenerationOutcome
	}{
		{
			name: "save creates the first body generation",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "save", false)
				before := readSavedGeneration(t, repo, fixture.linkID)
				body := generationBody("first saved body", false)
				revision, stored, err := repo.UpdateContentIfCurrent(t.Context(), fixture.linkID, fixture.parsedAt, body)
				if err != nil || !stored {
					t.Fatalf("UpdateContentIfCurrent() = revision %d, stored %v, error %v; want stored", revision, stored, err)
				}
				return generationOutcome(fixture.linkID, before, 1, bodyIdentity(body)).withReturnedRevision(revision)
			},
		},
		{
			name: "replace creates exactly one new body generation",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "replace", true)
				before := readSavedGeneration(t, repo, fixture.linkID)
				body := generationBody("replacement body", true)
				revision, replaced, err := repo.ReplaceContentIfCurrent(t.Context(), fixture.linkID, fixture.parsedAt, body)
				if err != nil || !replaced {
					t.Fatalf("ReplaceContentIfCurrent() = revision %d, replaced %v, error %v; want replaced", revision, replaced, err)
				}
				return generationOutcome(fixture.linkID, before, 1, bodyIdentity(body)).withReturnedRevision(revision)
			},
		},
		{
			name: "duplicate save is a no-op",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "duplicate-save", true)
				before := readSavedGeneration(t, repo, fixture.linkID)
				revision, stored, err := repo.UpdateContentIfCurrent(t.Context(), fixture.linkID, fixture.parsedAt, generationBody("ignored duplicate", true))
				if err != nil || stored || revision != 0 {
					t.Fatalf("duplicate UpdateContentIfCurrent() = revision %d, stored %v, error %v; want 0, false, nil", revision, stored, err)
				}
				return generationOutcome(fixture.linkID, before, 0, before.body)
			},
		},
		{
			name: "save CAS loss does not create a body generation",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "save-cas-loss", false)
				before := readSavedGeneration(t, repo, fixture.linkID)
				revision, stored, err := repo.UpdateContentIfCurrent(t.Context(), fixture.linkID, fixture.parsedAt.Add(time.Hour), generationBody("stale first save", true))
				if err != nil || stored || revision != 0 {
					t.Fatalf("stale UpdateContentIfCurrent() = revision %d, stored %v, error %v; want 0, false, nil", revision, stored, err)
				}
				return generationOutcome(fixture.linkID, before, 0, absentGenerationBody())
			},
		},
		{
			name: "replace CAS loss is a no-op",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "replace-cas-loss", true)
				before := readSavedGeneration(t, repo, fixture.linkID)
				revision, replaced, err := repo.ReplaceContentIfCurrent(t.Context(), fixture.linkID, fixture.parsedAt.Add(time.Hour), generationBody("stale replacement", true))
				if err != nil || replaced || revision != 0 {
					t.Fatalf("stale ReplaceContentIfCurrent() = revision %d, replaced %v, error %v; want 0, false, nil", revision, replaced, err)
				}
				return generationOutcome(fixture.linkID, before, 0, before.body)
			},
		},
		{
			name: "capture requeue clears the saved body",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "capture-requeue", true)
				before := readSavedGeneration(t, repo, fixture.linkID)
				captureText := "new browser capture"
				job, err := repo.RequeueExisting(t.Context(), fixture.linkID, &repository.CreateLinkParams{
					URL:                  fixture.url,
					SourceKind:           "browser_capture",
					SourceKey:            fixture.url,
					InputText:            &captureText,
					Status:               model.LinkStatusPending,
					RequestedLibraryKind: model.RequestedLibraryKindReading,
				})
				if err != nil || job == nil {
					t.Fatalf("RequeueExistingTx(capture) = %#v, %v", job, err)
				}
				return generationOutcome(fixture.linkID, before, 1, absentGenerationBody()).withStatus(model.LinkStatusPending)
			},
		},
		{
			name: "plain refresh preserves body and generation",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "refresh-requeue", true)
				before := readSavedGeneration(t, repo, fixture.linkID)
				job, err := repo.RequeueExisting(t.Context(), fixture.linkID, nil)
				if err != nil || job == nil {
					t.Fatalf("RequeueExistingTx(refresh) = %#v, %v", job, err)
				}
				return generationOutcome(fixture.linkID, before, 0, before.body).withStatus(model.LinkStatusPending)
			},
		},
		{
			name: "capture requeue rollback preserves body and generation",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "capture-rollback", true)
				before := readSavedGeneration(t, repo, fixture.linkID)
				captureText := "capture that must roll back"
				rollbackErr := errors.New("enqueue failed")
				commands := dbLinkCommands(repo.pool, repo.PGXLinkRepository, &countingSubmitQueue{enqueueErr: rollbackErr})
				_, err := commands.RequeueLink(t.Context(), service.RequeueLinkCommand{LinkID: fixture.linkID, Capture: &service.LinkCapture{
					URL:        fixture.url,
					SourceKind: "browser_capture",
					SourceKey:  fixture.url,
					InputText:  &captureText,
					Status:     model.LinkStatusPending,
				}})
				if !errors.Is(err, rollbackErr) {
					t.Fatalf("RequeueExistingTx(capture rollback) error = %v, want %v", err, rollbackErr)
				}
				return generationOutcome(fixture.linkID, before, 0, before.body).withStatus(model.LinkStatusDone)
			},
		},
		{
			name: "site completion clears the saved body",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newAutomaticSavedGenerationFixture(t, repo, "site-complete", true)
				mustMarkGenerationParseProcessing(t, repo, fixture)
				before := readSavedGeneration(t, repo, fixture.linkID)
				result, err := repo.CompleteSiteParse(t.Context(), generationSiteParseParams(fixture, true), fixture.jobID)
				if err != nil || result.SiteID == uuid.Nil || result.EntryID == uuid.Nil {
					t.Fatalf("CompleteSiteParse() = %#v, %v", result, err)
				}
				return generationOutcome(fixture.linkID, before, 1, absentGenerationBody()).
					withStatus(model.LinkStatusDone).
					withKind(model.LibraryKindSite)
			},
		},
		{
			name: "site completion rollback preserves body and generation",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newAutomaticSavedGenerationFixture(t, repo, "site-complete-rollback", true)
				mustMarkGenerationParseProcessing(t, repo, fixture)
				before := readSavedGeneration(t, repo, fixture.linkID)
				_, err := repo.CompleteSiteParse(t.Context(), generationSiteParseParams(fixture, false), fixture.jobID)
				if err == nil {
					t.Fatal("CompleteSiteParse(invalid aggregate) succeeded; want transaction rollback")
				}
				return generationOutcome(fixture.linkID, before, 0, before.body).
					withStatus(model.LinkStatusProcessing).
					withKind(model.LibraryKindReading)
			},
		},
		{
			name: "reading to site conversion clears the saved body",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "reading-to-site", true)
				before := readSavedGeneration(t, repo, fixture.linkID)
				result, err := repo.ConvertLink(t.Context(), repository.ConvertLinkParams{
					LinkID: fixture.linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: before.revision,
				})
				if err != nil || result.SiteID == nil || result.SiteRevision == nil || result.EntryID == nil {
					t.Fatalf("ConvertLink(reading to site) = %#v, %v", result, err)
				}
				return generationOutcome(fixture.linkID, before, 1, absentGenerationBody()).
					withReturnedRevision(result.ContentRevision).
					withStatus(model.LinkStatusDone).
					withKind(model.LibraryKindSite)
			},
		},
		{
			name: "conversion CAS loss preserves body and generation",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "conversion-cas-loss", true)
				before := readSavedGeneration(t, repo, fixture.linkID)
				_, err := repo.ConvertLink(t.Context(), repository.ConvertLinkParams{
					LinkID: fixture.linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: before.revision + 1,
				})
				if !errors.Is(err, repository.ErrRevisionConflict) {
					t.Fatalf("ConvertLink(stale revision) error = %v, want ErrRevisionConflict", err)
				}
				return generationOutcome(fixture.linkID, before, 0, before.body).
					withStatus(model.LinkStatusDone).
					withKind(model.LibraryKindReading)
			},
		},
		{
			name: "site to reading conversion creates a new empty generation",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture, site := newGenerationSiteFixture(t, repo, "site-to-reading")
				before := readSavedGeneration(t, repo, fixture.linkID)
				result, err := repo.ConvertLink(t.Context(), repository.ConvertLinkParams{
					LinkID:                  fixture.linkID,
					TargetKind:              model.LibraryKindReading,
					ExpectedContentRevision: before.revision,
					ExpectedSiteRevision:    site.SiteRevision,
				})
				if err != nil || result.ParseJobID == nil {
					t.Fatalf("ConvertLink(site to reading) = %#v, %v", result, err)
				}
				return generationOutcome(fixture.linkID, before, 1, absentGenerationBody()).
					withReturnedRevision(result.ContentRevision).
					withStatus(model.LinkStatusPending).
					withKind(model.LibraryKindReading)
			},
		},
		{
			name: "site to reading enqueue rollback preserves generation",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture, site := newGenerationSiteFixture(t, repo, "site-to-reading-rollback")
				before := readSavedGeneration(t, repo, fixture.linkID)
				rollbackErr := errors.New("conversion enqueue failed")
				commands := dbLinkCommands(repo.pool, repo.PGXLinkRepository, &countingSubmitQueue{enqueueErr: rollbackErr})
				_, err := commands.ConvertLink(t.Context(), service.ConvertLinkCommand{
					LinkID:                  fixture.linkID,
					TargetKind:              model.LibraryKindReading,
					ExpectedContentRevision: before.revision,
					ExpectedSiteRevision:    site.SiteRevision,
				})
				if !errors.Is(err, rollbackErr) {
					t.Fatalf("ConvertLink(site to reading rollback) error = %v, want %v", err, rollbackErr)
				}
				return generationOutcome(fixture.linkID, before, 0, before.body).
					withStatus(model.LinkStatusDone).
					withKind(model.LibraryKindSite)
			},
		},
		{
			name: "historical migration creates a new empty generation",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "historical-migration", false)
				assessment := prepareGenerationHistoricalMigration(t, repo, fixture)
				before := readSavedGeneration(t, repo, fixture.linkID)
				outcome, err := repo.CommitHistoricalMigrationAssessment(t.Context(), assessment)
				if err != nil || outcome != repository.HistoricalMigrationOutcomeAutoMigrated {
					t.Fatalf("CommitHistoricalMigrationAssessment() = %q, %v", outcome, err)
				}
				return generationOutcome(fixture.linkID, before, 1, absentGenerationBody()).
					withStatus(model.LinkStatusDone).
					withKind(model.LibraryKindSite)
			},
		},
		{
			name: "historical migration retry is a no-op",
			mutate: func(t *testing.T, repo *savedGenerationHarness) savedGenerationOutcome {
				fixture := newSavedGenerationFixture(t, repo, "historical-migration-retry", false)
				assessment := prepareGenerationHistoricalMigration(t, repo, fixture)
				if outcome, err := repo.CommitHistoricalMigrationAssessment(t.Context(), assessment); err != nil || outcome != repository.HistoricalMigrationOutcomeAutoMigrated {
					t.Fatalf("first CommitHistoricalMigrationAssessment() = %q, %v", outcome, err)
				}
				before := readSavedGeneration(t, repo, fixture.linkID)
				if outcome, err := repo.CommitHistoricalMigrationAssessment(t.Context(), assessment); err != nil || outcome != repository.HistoricalMigrationOutcomeNoop {
					t.Fatalf("second CommitHistoricalMigrationAssessment() = %q, %v, want no-op", outcome, err)
				}
				return generationOutcome(fixture.linkID, before, 0, before.body).
					withStatus(model.LinkStatusDone).
					withKind(model.LibraryKindSite)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := StartPostgres(t)
			repo := &savedGenerationHarness{PGXLinkRepository: repository.NewPGXLinkRepository(pool), pool: pool}
			outcome := test.mutate(t, repo)
			after := readSavedGeneration(t, repo, outcome.linkID)

			wantRevision := outcome.before.revision + outcome.wantDelta
			if after.revision != wantRevision {
				t.Fatalf("durable content revision = %d, want %d (before %d, delta %+d)", after.revision, wantRevision, outcome.before.revision, outcome.wantDelta)
			}
			if after.body != outcome.wantBody {
				t.Fatalf("durable saved body = %#v, want %#v", after.body, outcome.wantBody)
			}
			if outcome.returnedRevision != nil && *outcome.returnedRevision != after.revision {
				t.Fatalf("command returned content revision %d, durable row is %d", *outcome.returnedRevision, after.revision)
			}
			if outcome.wantStatus != nil && after.status != *outcome.wantStatus {
				t.Fatalf("durable link status = %q, want %q", after.status, *outcome.wantStatus)
			}
			if outcome.wantKind != nil && (!after.hasKind || after.kind != *outcome.wantKind) {
				t.Fatalf("durable library kind = %q (present %v), want %q", after.kind, after.hasKind, *outcome.wantKind)
			}
		})
	}
}

type savedGenerationHarness struct {
	*repository.PGXLinkRepository
	pool *pgxpool.Pool
}

type savedGenerationFixture struct {
	linkID                   uuid.UUID
	jobID                    uuid.UUID
	expectedMetadataRevision int64
	url                      string
	parsedAt                 time.Time
}

type savedGenerationBody struct {
	present     bool
	text        string
	document    string
	hasDocument bool
	format      model.ContentFormat
	cjkChars    int
	words       int
}

type savedGenerationState struct {
	revision int64
	body     savedGenerationBody
	status   model.LinkStatus
	kind     model.LibraryKind
	hasKind  bool
}

type savedGenerationOutcome struct {
	linkID           uuid.UUID
	before           savedGenerationState
	wantDelta        int64
	wantBody         savedGenerationBody
	returnedRevision *int64
	wantStatus       *model.LinkStatus
	wantKind         *model.LibraryKind
}

func generationOutcome(linkID uuid.UUID, before savedGenerationState, wantDelta int64, wantBody savedGenerationBody) savedGenerationOutcome {
	return savedGenerationOutcome{linkID: linkID, before: before, wantDelta: wantDelta, wantBody: wantBody}
}

func (o savedGenerationOutcome) withReturnedRevision(revision int64) savedGenerationOutcome {
	o.returnedRevision = &revision
	return o
}

func (o savedGenerationOutcome) withStatus(status model.LinkStatus) savedGenerationOutcome {
	o.wantStatus = &status
	return o
}

func (o savedGenerationOutcome) withKind(kind model.LibraryKind) savedGenerationOutcome {
	o.wantKind = &kind
	return o
}

func newSavedGenerationFixture(t *testing.T, repo *savedGenerationHarness, slug string, saveBody bool) savedGenerationFixture {
	t.Helper()
	return newSavedGenerationFixtureWithIntent(
		t,
		repo,
		slug,
		saveBody,
		model.RequestedLibraryKindReading,
		model.RequestedLibraryKindSourceUser,
	)
}

func newAutomaticSavedGenerationFixture(t *testing.T, repo *savedGenerationHarness, slug string, saveBody bool) savedGenerationFixture {
	t.Helper()
	fixture := newSavedGenerationFixtureWithIntent(
		t,
		repo,
		slug,
		saveBody,
		model.RequestedLibraryKindAuto,
		model.RequestedLibraryKindSourceAuto,
	)
	if err := repo.UpdateLibraryClassification(t.Context(), repository.UpdateLibraryClassificationParams{
		ID: fixture.linkID, Kind: model.LibraryKindReading, Source: model.LibraryKindSourceAuto,
	}); err != nil {
		t.Fatalf("seed automatic reading classification for %s: %v", slug, err)
	}
	link, err := repo.GetByID(t.Context(), fixture.linkID)
	if err != nil || link == nil {
		t.Fatalf("GetByID(automatic reading, %s) = %#v, %v", slug, link, err)
	}
	fixture.parsedAt = link.UpdatedAt
	return fixture
}

func newSavedGenerationFixtureWithIntent(
	t *testing.T,
	repo *savedGenerationHarness,
	slug string,
	saveBody bool,
	requestedKind model.RequestedLibraryKind,
	requestedSource model.RequestedLibraryKindSource,
) savedGenerationFixture {
	t.Helper()
	ctx := t.Context()
	rawURL := fmt.Sprintf("https://saved-generation.example.com/%s", slug)
	link, job, err := repo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:                        rawURL,
		SourceKind:                 "url",
		SourceKey:                  rawURL,
		Status:                     model.LinkStatusPending,
		RequestedLibraryKind:       requestedKind,
		RequestedLibraryKindSource: requestedSource,
	})
	if err != nil || link == nil || job == nil {
		t.Fatalf("SubmitNew(%s) = link %#v, job %#v, error %v", slug, link, job, err)
	}
	if err := repo.UpdateState(ctx, repository.UpdateLinkStateParams{ID: link.ID, Status: model.LinkStatusDone}); err != nil {
		t.Fatalf("UpdateState(done, %s): %v", slug, err)
	}
	parsed, err := repo.GetByID(ctx, link.ID)
	if err != nil || parsed == nil {
		t.Fatalf("GetByID(parsed, %s) = %#v, %v", slug, parsed, err)
	}
	fixture := savedGenerationFixture{
		linkID:                   link.ID,
		jobID:                    job.ID,
		expectedMetadataRevision: job.ExpectedMetadataRevision,
		url:                      rawURL,
		parsedAt:                 parsed.UpdatedAt,
	}
	if !saveBody {
		return fixture
	}

	body := generationBody("fixture body for "+slug, true)
	revision, stored, err := repo.UpdateContentIfCurrent(ctx, link.ID, fixture.parsedAt, body)
	if err != nil || !stored {
		t.Fatalf("seed UpdateContentIfCurrent(%s) = revision %d, stored %v, error %v", slug, revision, stored, err)
	}
	if durable := readSavedGeneration(t, repo, link.ID); durable.revision != revision || durable.body != bodyIdentity(body) {
		t.Fatalf("seed body for %s = revision %d/body %#v, want revision %d/body %#v", slug, durable.revision, durable.body, revision, bodyIdentity(body))
	}
	return fixture
}

func readSavedGeneration(t *testing.T, repo *savedGenerationHarness, linkID uuid.UUID) savedGenerationState {
	t.Helper()
	link, err := repo.GetByID(t.Context(), linkID)
	if err != nil || link == nil {
		t.Fatalf("GetByID(%s) = %#v, %v", linkID, link, err)
	}
	body, err := repo.GetContent(t.Context(), linkID)
	if err != nil {
		t.Fatalf("GetContent(%s): %v", linkID, err)
	}
	var (
		content, document *string
		format            string
		cjkChars, words   int
	)
	if err := repo.pool.QueryRow(t.Context(), `SELECT content, content_document, content_format, content_cjk_chars, content_words
		FROM links WHERE id = $1`, linkID).Scan(&content, &document, &format, &cjkChars, &words); err != nil {
		t.Fatalf("read durable saved body columns for %s: %v", linkID, err)
	}
	rawBody := savedGenerationBody{format: model.ContentFormat(format), cjkChars: cjkChars, words: words}
	if content != nil {
		rawBody.present, rawBody.text = true, *content
	}
	if document != nil {
		rawBody.hasDocument, rawBody.document = true, *document
	}
	state := savedGenerationState{revision: link.ContentRevision, body: rawBody, status: link.Status}
	if link.LibraryKind != nil {
		state.kind, state.hasKind = *link.LibraryKind, true
	}
	if body != nil {
		if body.Revision != link.ContentRevision {
			t.Fatalf("GetContent(%s) revision = %d, GetByID revision = %d", linkID, body.Revision, link.ContentRevision)
		}
		publicIdentity := bodyIdentity(*body)
		publicIdentity.cjkChars, publicIdentity.words = rawBody.cjkChars, rawBody.words
		if publicIdentity != rawBody {
			t.Fatalf("GetContent(%s) body = %#v, durable columns = %#v", linkID, publicIdentity, rawBody)
		}
	}
	if link.HasContent != rawBody.present || (body != nil) != rawBody.present {
		t.Fatalf("saved body presence for %s = projection %v/public read %v/durable row %v", linkID, link.HasContent, body != nil, rawBody.present)
	}
	return state
}

func generationBody(text string, structured bool) model.SavedContent {
	if !structured {
		return model.SavedContent{Text: text, Format: model.ContentFormatPlain, Words: 3}
	}
	text += " 中文"
	document := "# " + text + "\n\nStructured snapshot."
	return model.SavedContent{Text: text, Document: &document, Format: model.ContentFormatMarkdown, CJKChars: 2, Words: 4}
}

func bodyIdentity(body model.SavedContent) savedGenerationBody {
	identity := savedGenerationBody{
		present: true, text: body.Text, format: body.Format,
		cjkChars: body.CJKChars, words: body.Words,
	}
	if body.Document != nil {
		identity.document, identity.hasDocument = *body.Document, true
	}
	return identity
}

func absentGenerationBody() savedGenerationBody {
	return savedGenerationBody{format: model.ContentFormatPlain}
}

func mustMarkGenerationParseProcessing(t *testing.T, repo *savedGenerationHarness, fixture savedGenerationFixture) {
	t.Helper()
	if err := repo.MarkParseProcessing(t.Context(), fixture.linkID, fixture.jobID); err != nil {
		t.Fatalf("MarkParseProcessing(%s): %v", fixture.linkID, err)
	}
}

func generationSiteParseParams(fixture savedGenerationFixture, validAggregate bool) repository.CompleteSiteParseParams {
	title := "Saved generation site"
	fetcherType := "test"
	domain := "saved-generation.example.com"
	contentType := string(model.ContentTypeHomepage)
	identityKey := "v1:host:saved-generation.example.com"
	if !validAggregate {
		identityKey = ""
	}
	return repository.CompleteSiteParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID:                       fixture.linkID,
			ExpectedMetadataRevision: fixture.expectedMetadataRevision,
			Title:                    &title,
			Tags:                     []string{"generation"},
			FetcherType:              &fetcherType,
			Domain:                   &domain,
			ContentType:              &contentType,
			Status:                   model.LinkStatusDone,
		},
		Classification: repository.UpdateLibraryClassificationParams{
			ID:     fixture.linkID,
			Kind:   model.LibraryKindSite,
			Source: model.LibraryKindSourceAuto,
		},
		Site: repository.AggregateSiteParams{
			LinkID:        fixture.linkID,
			IdentityKey:   identityKey,
			NormalizedURL: fixture.url,
			Name:          title,
			EntryName:     title,
		},
	}
}

func newGenerationSiteFixture(t *testing.T, repo *savedGenerationHarness, slug string) (savedGenerationFixture, repository.ConvertLinkResult) {
	t.Helper()
	fixture := newSavedGenerationFixture(t, repo, slug, true)
	before := readSavedGeneration(t, repo, fixture.linkID)
	result, err := repo.ConvertLink(t.Context(), repository.ConvertLinkParams{
		LinkID: fixture.linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: before.revision,
	})
	if err != nil || result.SiteID == nil || result.SiteRevision == nil || result.EntryID == nil {
		t.Fatalf("prepare site ConvertLink(%s) = %#v, %v", slug, result, err)
	}
	if durable := readSavedGeneration(t, repo, fixture.linkID); durable.revision != result.ContentRevision || durable.body.present || durable.kind != model.LibraryKindSite {
		t.Fatalf("prepared site %s = revision %d/body %#v/kind %q, result %#v", slug, durable.revision, durable.body, durable.kind, result)
	}
	return fixture, result
}

func prepareGenerationHistoricalMigration(t *testing.T, repo *savedGenerationHarness, fixture savedGenerationFixture) repository.HistoricalMigrationAssessment {
	t.Helper()
	if _, err := repo.pool.Exec(t.Context(), `UPDATE links SET requested_library_kind='auto',requested_library_kind_source='auto' WHERE id=$1`, fixture.linkID); err != nil {
		t.Fatalf("prepare historical migration auto ownership: %v", err)
	}
	if err := repo.UpdateLibraryClassification(t.Context(), repository.UpdateLibraryClassificationParams{
		ID: fixture.linkID, Kind: model.LibraryKindReading, Source: model.LibraryKindSourceMigration,
	}); err != nil {
		t.Fatalf("prepare historical migration classification: %v", err)
	}
	candidates, err := repo.ListHistoricalMigrationCandidates(t.Context(), repository.HistoricalMigrationCursor{}, 100)
	if err != nil {
		t.Fatalf("ListHistoricalMigrationCandidates() error = %v", err)
	}
	var candidate *repository.HistoricalMigrationCandidate
	for i := range candidates {
		if candidates[i].ID == fixture.linkID {
			candidate = &candidates[i]
			break
		}
	}
	if candidate == nil {
		t.Fatalf("historical migration fixture %s was not listed", fixture.linkID)
	}
	return repository.HistoricalMigrationAssessment{
		Candidate:     *candidate,
		PredictedKind: model.LibraryKindSite,
		Confidence:    0.99,
		Reason:        "generation_matrix",
		AutoMigrate:   true,
	}
}
