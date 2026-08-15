//go:build dbintegration

package dbintegration

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/migrate"
	"webtag/internal/repository"
	"webtag/internal/service"
)

const historicalMigrationRepairVersion = "historical2026081401"

func TestHistoricalMigrationPhaseTwoFailureRollsBackAssessmentAndRetries(t *testing.T) {
	pool := StartPostgres(t)
	linkID := seedHistoricalMigrationCandidate(t, pool, "phase-two-failure")
	runner := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(pool))

	installHistoricalMigrationPhaseTwoFailure(t, pool)
	failed, err := runner.Run(t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 1})
	if err == nil {
		t.Fatal("first Run() error = nil, want injected phase-two failure")
	}
	if failed.AutoMigrated != 0 || failed.Suggested != 0 || failed.Retained != 0 {
		t.Fatalf("failed Run() reported committed outcome: %#v", failed)
	}
	var classifierVersion *string
	if err := pool.QueryRow(t.Context(), `SELECT classifier_version FROM links WHERE id=$1`, linkID).Scan(&classifierVersion); err != nil {
		t.Fatalf("read failed candidate: %v", err)
	}
	if classifierVersion != nil {
		t.Fatalf("classifier_version after rolled-back phase two = %q, want NULL", *classifierVersion)
	}

	removeHistoricalMigrationPhaseTwoFailure(t, pool)
	retried, err := runner.Run(t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 1})
	if err != nil {
		t.Fatalf("retry Run() error = %v", err)
	}
	if retried.AutoMigrated != 1 || retried.Suggested != 0 || retried.Retained != 0 {
		t.Fatalf("retry Run() = %#v, want one committed automatic migration", retried)
	}
	var kind string
	if err := pool.QueryRow(t.Context(), `SELECT library_kind FROM links WHERE id=$1`, linkID).Scan(&kind); err != nil {
		t.Fatalf("read retried candidate: %v", err)
	}
	if kind != "site" {
		t.Fatalf("library_kind after retry = %q, want site", kind)
	}
}

func TestHistoricalMigrationTwoRunnersHaveOneCommittedOwner(t *testing.T) {
	pool := StartPostgres(t)
	seedHistoricalMigrationCandidate(t, pool, "dual-runner")
	replicaPool, err := pgxpool.New(t.Context(), DSN(t))
	if err != nil {
		t.Fatalf("open replica pool: %v", err)
	}
	t.Cleanup(replicaPool.Close)
	installHistoricalMigrationHold(t, pool, "dual-runner", 181, 181, 1)

	firstRunner := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(pool))
	secondRunner := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(replicaPool))
	type runResult struct {
		report service.HistoricalMigrationReport
		err    error
	}
	firstDone := make(chan runResult, 1)
	go func() {
		report, runErr := firstRunner.Run(context.Background(), service.HistoricalMigrationRunOptions{BatchSize: 1})
		firstDone <- runResult{report: report, err: runErr}
	}()
	waitForHistoricalMigrationAdvisoryLock(t, pool, 181, 181)

	secondCtx, cancelSecond := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancelSecond()
	secondReport, err := secondRunner.Run(secondCtx, service.HistoricalMigrationRunOptions{BatchSize: 1})
	if err != nil {
		t.Fatalf("second Run() error = %v; SKIP LOCKED must not wait for the owner", err)
	}
	first := <-firstDone
	if first.err != nil {
		t.Fatalf("first Run() error = %v", first.err)
	}
	if first.report.AutoMigrated != 1 || secondReport.Skipped != 1 || secondReport.AutoMigrated != 0 {
		t.Fatalf("first=%#v second=%#v, want one committed owner and one no-op", first.report, secondReport)
	}
	assertHistoricalMigrationAggregateCount(t, pool, 1)
}

func TestHistoricalMigrationCrashedOwnerReleasesClaimForTakeover(t *testing.T) {
	pool := StartPostgres(t)
	seedHistoricalMigrationCandidate(t, pool, "crashed-owner")
	replicaPool, err := pgxpool.New(t.Context(), DSN(t))
	if err != nil {
		t.Fatalf("open replica pool: %v", err)
	}
	t.Cleanup(replicaPool.Close)
	installHistoricalMigrationHold(t, pool, "crashed-owner", 181, 182, 30)

	firstRunner := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(pool))
	firstDone := make(chan error, 1)
	go func() {
		_, runErr := firstRunner.Run(context.Background(), service.HistoricalMigrationRunOptions{BatchSize: 1})
		firstDone <- runErr
	}()
	backendPID := waitForHistoricalMigrationAdvisoryLock(t, replicaPool, 181, 182)
	var terminated bool
	if err := replicaPool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, backendPID).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate owner backend %d = %t, %v", backendPID, terminated, err)
	}
	if err := <-firstDone; err == nil {
		t.Fatal("crashed owner Run() error = nil")
	}
	removeHistoricalMigrationHold(t, pool)

	takeover := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(replicaPool))
	report, err := takeover.Run(t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 1})
	if err != nil {
		t.Fatalf("takeover Run() error = %v", err)
	}
	if report.AutoMigrated != 1 || report.Skipped != 0 {
		t.Fatalf("takeover Run() = %#v, want one committed migration", report)
	}
	assertHistoricalMigrationAggregateCount(t, replicaPool, 1)
}

func TestHistoricalMigrationTimedOutOwnerReleasesClaimForTakeover(t *testing.T) {
	pool := StartPostgres(t)
	seedHistoricalMigrationCandidate(t, pool, "timed-out-owner")
	replicaPool, err := pgxpool.New(t.Context(), DSN(t))
	if err != nil {
		t.Fatalf("open replica pool: %v", err)
	}
	t.Cleanup(replicaPool.Close)
	installHistoricalMigrationHold(t, pool, "timed-out-owner", 181, 183, 30)

	ownerCtx, cancelOwner := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelOwner()
	owner := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(pool))
	type timedRunResult struct {
		report service.HistoricalMigrationReport
		err    error
	}
	ownerDone := make(chan timedRunResult, 1)
	go func() {
		report, runErr := owner.Run(ownerCtx, service.HistoricalMigrationRunOptions{BatchSize: 1})
		ownerDone <- timedRunResult{report: report, err: runErr}
	}()
	waitForHistoricalMigrationAdvisoryLock(t, replicaPool, 181, 183)
	timedOut := <-ownerDone
	if !errors.Is(timedOut.err, context.DeadlineExceeded) {
		t.Fatalf("timed-out owner error = %v, want context deadline", timedOut.err)
	}
	if timedOut.report.AutoMigrated != 0 || timedOut.report.Suggested != 0 || timedOut.report.Retained != 0 {
		t.Fatalf("timed-out owner reported committed outcome: %#v", timedOut.report)
	}
	removeHistoricalMigrationHold(t, pool)

	takeover := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(replicaPool))
	report, err := takeover.Run(t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 1})
	if err != nil {
		t.Fatalf("timeout takeover Run() error = %v", err)
	}
	if report.AutoMigrated != 1 || report.Skipped != 0 {
		t.Fatalf("timeout takeover Run() = %#v, want one committed migration", report)
	}
}

func TestHistoricalMigrationReviewActionAndBackgroundUseOneLockOrder(t *testing.T) {
	pool := StartPostgres(t)
	linkID := seedHistoricalMigrationCandidate(t, pool, "review-race")
	execHistoricalFixture(t, pool, `UPDATE links SET content='saved body',content_document='saved body',content_source='user' WHERE id=$1`, linkID)
	repo := repository.NewPGXLinkRepository(pool)
	candidates, err := repo.ListHistoricalMigrationCandidates(t.Context(), repository.HistoricalMigrationCursor{}, 1)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("list review race candidate = %#v, %v", candidates, err)
	}
	staleAssessment := repository.HistoricalMigrationAssessment{
		Candidate: candidates[0], PredictedKind: "site", Confidence: .99,
		Reason: "migration_assets_require_review", Suggest: true,
	}
	if outcome, commitErr := repo.CommitHistoricalMigrationAssessment(t.Context(), staleAssessment); commitErr != nil || outcome != repository.HistoricalMigrationOutcomeSuggested {
		t.Fatalf("create review race suggestion = %q, %v", outcome, commitErr)
	}
	var reviewID uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM library_review_items
		WHERE link_id=$1 AND kind='migration_suggestion' AND status='pending'`, linkID).Scan(&reviewID); err != nil {
		t.Fatalf("read review race suggestion: %v", err)
	}
	installHistoricalMigrationReviewMoveHold(t, pool)

	type moveResult struct {
		err error
	}
	moveDone := make(chan moveResult, 1)
	go func() {
		_, moveErr := repo.MoveHistoricalMigrationToSite(context.Background(), reviewID, 1)
		moveDone <- moveResult{err: moveErr}
	}()
	waitForHistoricalMigrationAdvisoryLock(t, pool, 181, 184)

	runnerCtx, cancelRunner := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelRunner()
	type backgroundResult struct {
		outcome repository.HistoricalMigrationOutcome
		err     error
	}
	runnerDone := make(chan backgroundResult, 1)
	go func() {
		outcome, commitErr := repository.NewPGXLinkRepository(pool).CommitHistoricalMigrationAssessment(runnerCtx, staleAssessment)
		runnerDone <- backgroundResult{outcome: outcome, err: commitErr}
	}()
	move := <-moveDone
	if move.err != nil {
		t.Fatalf("MoveHistoricalMigrationToSite() error = %v", move.err)
	}
	background := <-runnerDone
	if background.err != nil {
		t.Fatalf("background Run() error = %v", background.err)
	}
	if background.outcome != repository.HistoricalMigrationOutcomeNoop {
		t.Fatalf("background outcome = %q, want post-review no-op", background.outcome)
	}
	var kind, reviewStatus string
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT library_kind FROM links WHERE id=$1),
		(SELECT status FROM library_review_items WHERE id=$2)`, linkID, reviewID).Scan(&kind, &reviewStatus); err != nil {
		t.Fatalf("read review race result: %v", err)
	}
	if kind != "site" || reviewStatus != "applied" {
		t.Fatalf("review race result = kind:%q review:%q, want site/applied", kind, reviewStatus)
	}
}

func TestHistoricalMigrationDryRunReportsWouldAndWritesNothing(t *testing.T) {
	pool := StartPostgres(t)
	autoID := seedHistoricalMigrationCandidate(t, pool, "dry-auto")
	suggestID := seedHistoricalMigrationCandidate(t, pool, "dry-suggest")
	retainID := seedHistoricalMigrationCandidate(t, pool, "dry-retain")
	execHistoricalFixture(t, pool, `UPDATE links SET content='saved body',content_document='saved body',content_source='user' WHERE id=$1`, suggestID)
	execHistoricalFixture(t, pool, `UPDATE links SET url='https://historical-dry-retain.example.com/article',source_key='https://historical-dry-retain.example.com/article',content_type='article' WHERE id=$1`, retainID)
	ids := []uuid.UUID{autoID, suggestID, retainID}
	before := historicalMigrationFingerprint(t, pool, ids)

	runner := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(pool))
	report, err := runner.Run(t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 2, DryRun: true})
	if err != nil {
		t.Fatalf("dry Run() error = %v", err)
	}
	if report.AutoMigrated != 0 || report.Suggested != 0 || report.Retained != 0 || report.PredictedSite != 0 {
		t.Fatalf("dry Run() reported committed outcomes: %#v", report)
	}
	if report.WouldAutoMigrate != 1 || report.WouldSuggest != 1 || report.WouldRetain != 1 {
		t.Fatalf("dry Run() = %#v, want one candidate in each Would bucket", report)
	}
	after := historicalMigrationFingerprint(t, pool, ids)
	if !bytes.Equal(before, after) {
		t.Fatalf("dry Run() changed database\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestHistoricalMigrationRuntimeRepairsOrphanAndStaleSuggestion(t *testing.T) {
	pool := StartPostgres(t)
	orphanID := seedHistoricalMigrationCandidate(t, pool, "runtime-orphan")
	staleID := seedHistoricalMigrationCandidate(t, pool, "runtime-stale")
	execHistoricalFixture(t, pool, `UPDATE links SET predicted_library_kind='site',classifier_version='historical-migration-v1' WHERE id=$1`, orphanID)
	execHistoricalFixture(t, pool, `UPDATE links SET content='saved body',content_document='saved body',content_source='user',predicted_library_kind='site',classifier_version='historical-migration-v1' WHERE id=$1`, staleID)
	insertHistoricalMigrationSuggestion(t, pool, staleID, 999)

	runner := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(pool))
	report, err := runner.Run(t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 2})
	if err != nil {
		t.Fatalf("repair Run() error = %v", err)
	}
	if report.AutoMigrated != 1 || report.Suggested != 1 {
		t.Fatalf("repair Run() = %#v, want repaired auto and suggestion outcomes", report)
	}
	var pendingCurrent, dismissedStale int
	if err := pool.QueryRow(t.Context(), `SELECT
		count(*) FILTER (WHERE status='pending' AND payload @> jsonb_build_object('content_revision',1::bigint)),
		count(*) FILTER (WHERE status='dismissed' AND payload @> jsonb_build_object('content_revision',999::bigint))
		FROM library_review_items WHERE link_id=$1 AND kind='migration_suggestion'`, staleID).Scan(&pendingCurrent, &dismissedStale); err != nil {
		t.Fatalf("read repaired suggestions: %v", err)
	}
	if pendingCurrent != 1 || dismissedStale != 1 {
		t.Fatalf("repaired suggestions = current:%d stale-dismissed:%d, want 1/1", pendingCurrent, dismissedStale)
	}
}

func TestHistoricalMigrationLegacyCurrentSuggestionConvergesToStrongIdentity(t *testing.T) {
	pool := StartPostgres(t)
	linkID := seedHistoricalMigrationCandidate(t, pool, "current-suggestion")
	execHistoricalFixture(t, pool, `UPDATE links SET content='saved body',content_document='saved body',content_source='user' WHERE id=$1`, linkID)
	reviewID := insertHistoricalMigrationSuggestion(t, pool, linkID, 1)

	report, err := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(pool)).Run(
		t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.Suggested != 1 || report.Skipped != 0 {
		t.Fatalf("Run() = %#v, want one committed suggestion", report)
	}
	var pending int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM library_review_items
		WHERE link_id=$1 AND kind='migration_suggestion' AND status='pending'`, linkID).Scan(&pending); err != nil {
		t.Fatalf("count current suggestion result: %v", err)
	}
	var revision int
	var gotReviewID uuid.UUID
	var classifier string
	if err := pool.QueryRow(t.Context(), `SELECT id,revision,
		(SELECT classifier_version FROM links WHERE id=$1)
		FROM library_review_items WHERE link_id=$1 AND kind='migration_suggestion' AND status='pending'`, linkID).Scan(
		&gotReviewID, &revision, &classifier); err != nil {
		t.Fatalf("read current suggestion result: %v", err)
	}
	var legacyStatus string
	var decisionVersion int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT status FROM library_review_items WHERE id=$1),
		(payload->>'decision_version')::int
		FROM library_review_items WHERE id=$2`, reviewID, gotReviewID).Scan(&legacyStatus, &decisionVersion); err != nil {
		t.Fatalf("read converged suggestion identity: %v", err)
	}
	if pending != 1 || gotReviewID == reviewID || revision != 1 || classifier != "historical-migration-v1" ||
		legacyStatus != "dismissed" || decisionVersion != 2 {
		t.Fatalf("converged suggestion = pending:%d id:%s legacy:%s revision:%d classifier:%q decision:%d",
			pending, gotReviewID, legacyStatus, revision, classifier, decisionVersion)
	}
	converged, err := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(pool)).Run(
		t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 1})
	if err != nil || converged.Scanned != 0 {
		t.Fatalf("converged Run() = %#v, %v, want no remaining candidate", converged, err)
	}
}

func TestHistoricalMigrationAssessmentRechecksMetadataOwnershipAndClassifierState(t *testing.T) {
	pool := StartPostgres(t)
	linkID := seedHistoricalMigrationCandidate(t, pool, "assessment-identity-change")
	repo := repository.NewPGXLinkRepository(pool)
	candidates, err := repo.ListHistoricalMigrationCandidates(t.Context(), repository.HistoricalMigrationCursor{}, 1)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("list assessment identity candidate = %#v, %v", candidates, err)
	}
	stale := repository.HistoricalMigrationAssessment{
		Candidate: candidates[0], PredictedKind: "site", Confidence: .99,
		Reason: "migration_obvious_homepage", AutoMigrate: true,
	}
	execHistoricalFixture(t, pool, `UPDATE links SET title='Changed title',content_type='article',
		requested_library_kind='reading',requested_library_kind_source='user',
		predicted_library_kind='reading',classification_confidence=.75,
		classification_reason='new_classifier_decision',classifier_version='classifier-v2' WHERE id=$1`, linkID)

	outcome, err := repo.CommitHistoricalMigrationAssessment(t.Context(), stale)
	if err != nil || outcome != repository.HistoricalMigrationOutcomeNoop {
		t.Fatalf("stale CommitHistoricalMigrationAssessment() = %q, %v, want no-op", outcome, err)
	}
	var kind, title, contentType, requestedSource, predicted, classifier string
	var contentRevision, metadataRevision int64
	if err := pool.QueryRow(t.Context(), `SELECT library_kind,title,content_type,requested_library_kind_source,
		predicted_library_kind,classifier_version,content_revision,metadata_revision FROM links WHERE id=$1`, linkID).Scan(
		&kind, &title, &contentType, &requestedSource, &predicted, &classifier, &contentRevision, &metadataRevision); err != nil {
		t.Fatalf("read stale assessment result: %v", err)
	}
	if kind != "reading" || title != "Changed title" || contentType != "article" || requestedSource != "user" ||
		predicted != "reading" || classifier != "classifier-v2" || contentRevision != 1 || metadataRevision != 2 {
		t.Fatalf("stale assessment overwrote current state: kind=%q title=%q type=%q requested=%q predicted=%q classifier=%q content=%d metadata=%d",
			kind, title, contentType, requestedSource, predicted, classifier, contentRevision, metadataRevision)
	}
}

func TestHistoricalMigrationReviewFailsClosedWhenDecisionIdentityChanges(t *testing.T) {
	pool := StartPostgres(t)
	linkID := seedHistoricalMigrationCandidate(t, pool, "review-identity-change")
	execHistoricalFixture(t, pool, `UPDATE links SET content='saved body',content_document='saved body',content_source='user' WHERE id=$1`, linkID)
	repo := repository.NewPGXLinkRepository(pool)
	report, err := service.NewHistoricalMigrationRunner(repo).Run(t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 1})
	if err != nil || report.Suggested != 1 {
		t.Fatalf("create strong review Run() = %#v, %v", report, err)
	}
	var reviewID uuid.UUID
	var reviewRevision int64
	if err := pool.QueryRow(t.Context(), `SELECT id,revision FROM library_review_items
		WHERE link_id=$1 AND kind='migration_suggestion' AND status='pending'`, linkID).Scan(&reviewID, &reviewRevision); err != nil {
		t.Fatalf("read strong review: %v", err)
	}

	// Content type and classifier state can change without advancing the saved
	// content revision. The review must still become non-executable.
	execHistoricalFixture(t, pool, `UPDATE links SET content_type='article',predicted_library_kind='reading',
		classification_reason='new_classifier_decision',classifier_version='classifier-v2' WHERE id=$1`, linkID)
	item, err := repo.MoveHistoricalMigrationToSite(t.Context(), reviewID, reviewRevision)
	if item != nil || !errors.Is(err, repository.ErrRevisionConflict) {
		t.Fatalf("stale MoveHistoricalMigrationToSite() = %#v, %v, want nil/revision conflict", item, err)
	}
	var kind, reviewStatus string
	var contentRevision int64
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT library_kind FROM links WHERE id=$1),
		(SELECT content_revision FROM links WHERE id=$1),
		(SELECT status FROM library_review_items WHERE id=$2)`, linkID, reviewID).Scan(&kind, &contentRevision, &reviewStatus); err != nil {
		t.Fatalf("read stale review result: %v", err)
	}
	if kind != "reading" || contentRevision != 1 || reviewStatus != "dismissed" {
		t.Fatalf("stale review result = kind:%q content-revision:%d review:%q, want reading/1/dismissed", kind, contentRevision, reviewStatus)
	}
}

func TestHistoricalMigrationKeepActionAppliesStrongIdentityReview(t *testing.T) {
	pool := StartPostgres(t)
	linkID := seedHistoricalMigrationCandidate(t, pool, "keep-strong-review")
	execHistoricalFixture(t, pool, `UPDATE links SET content='saved body',content_document='saved body',content_source='user' WHERE id=$1`, linkID)
	repo := repository.NewPGXLinkRepository(pool)
	report, err := service.NewHistoricalMigrationRunner(repo).Run(t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 1})
	if err != nil || report.Suggested != 1 {
		t.Fatalf("create keep review Run() = %#v, %v", report, err)
	}
	var reviewID uuid.UUID
	var revision int64
	if err := pool.QueryRow(t.Context(), `SELECT id,revision FROM library_review_items
		WHERE link_id=$1 AND kind='migration_suggestion' AND status='pending'`, linkID).Scan(&reviewID, &revision); err != nil {
		t.Fatalf("read keep review: %v", err)
	}
	item, err := repo.KeepHistoricalMigrationReading(t.Context(), reviewID, revision)
	if err != nil || item == nil || item.Status != "applied" {
		t.Fatalf("KeepHistoricalMigrationReading() = %#v, %v", item, err)
	}
	var kind, source, requestedSource string
	var locked bool
	if err := pool.QueryRow(t.Context(), `SELECT library_kind,library_kind_source,library_kind_locked,
		requested_library_kind_source FROM links WHERE id=$1`, linkID).Scan(&kind, &source, &locked, &requestedSource); err != nil {
		t.Fatalf("read keep action result: %v", err)
	}
	if kind != "reading" || source != "user" || !locked || requestedSource != "user" {
		t.Fatalf("keep action result = kind:%q source:%q locked:%t requested:%q", kind, source, locked, requestedSource)
	}
}

func TestHistoricalMigrationCommittedNonSuggestionOutcomesDismissPendingReviews(t *testing.T) {
	pool := StartPostgres(t)
	autoID := seedHistoricalMigrationCandidate(t, pool, "dismiss-auto-review")
	retainID := seedHistoricalMigrationCandidate(t, pool, "dismiss-retain-review")
	execHistoricalFixture(t, pool, `UPDATE links SET url='https://historical-dismiss-retain-review.example.com/article',
		source_key='https://historical-dismiss-retain-review.example.com/article',content_type='article' WHERE id=$1`, retainID)
	autoReviewID := insertHistoricalMigrationSuggestion(t, pool, autoID, 1)
	retainReviewID := insertHistoricalMigrationSuggestion(t, pool, retainID, 1)

	report, err := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(pool)).Run(
		t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 2})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.AutoMigrated != 1 || report.Retained != 1 || report.Suggested != 0 {
		t.Fatalf("Run() = %#v, want one auto migration and one retain", report)
	}
	var autoStatus, retainStatus, autoKind, retainKind string
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT status FROM library_review_items WHERE id=$1),
		(SELECT status FROM library_review_items WHERE id=$2),
		(SELECT library_kind FROM links WHERE id=$3),
		(SELECT library_kind FROM links WHERE id=$4)`, autoReviewID, retainReviewID, autoID, retainID).Scan(
		&autoStatus, &retainStatus, &autoKind, &retainKind); err != nil {
		t.Fatalf("read non-suggestion cleanup: %v", err)
	}
	if autoStatus != "dismissed" || retainStatus != "dismissed" || autoKind != "site" || retainKind != "reading" {
		t.Fatalf("non-suggestion cleanup = auto:%s/%s retain:%s/%s", autoStatus, autoKind, retainStatus, retainKind)
	}
}

func TestHistoricalMigrationDeletedCandidateIsNotScanned(t *testing.T) {
	pool := StartPostgres(t)
	linkID := seedHistoricalMigrationCandidate(t, pool, "deleted")
	execHistoricalFixture(t, pool, `UPDATE links SET deleted_at=NOW() WHERE id=$1`, linkID)

	report, err := service.NewHistoricalMigrationRunner(repository.NewPGXLinkRepository(pool)).Run(
		t.Context(), service.HistoricalMigrationRunOptions{BatchSize: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.Scanned != 0 || report.AutoMigrated != 0 || report.Suggested != 0 || report.Retained != 0 {
		t.Fatalf("deleted candidate Run() = %#v, want no scan or writes", report)
	}
}

func TestHistoricalMigrationRepairUpgradeFixtureIsSelectiveAndIdempotent(t *testing.T) {
	pool := StartPostgres(t)
	hadLedger := migrationLedgerContains(t, pool, historicalMigrationRepairVersion)
	t.Cleanup(func() { restoreMigrationLedger(t, pool, historicalMigrationRepairVersion, hadLedger) })
	if _, err := pool.Exec(t.Context(), `DELETE FROM schema_migrations WHERE version=$1`, historicalMigrationRepairVersion); err != nil {
		t.Fatalf("remove repair ledger for upgrade fixture: %v", err)
	}

	orphanID := seedHistoricalMigrationCandidate(t, pool, "upgrade-orphan")
	mismatchID := seedHistoricalMigrationCandidate(t, pool, "upgrade-mismatch")
	validID := seedHistoricalMigrationCandidate(t, pool, "upgrade-valid")
	retainedID := seedHistoricalMigrationCandidate(t, pool, "upgrade-retained")
	userOwnedID := seedHistoricalMigrationCandidate(t, pool, "upgrade-user-owned")
	nonReadingID := seedHistoricalMigrationCandidate(t, pool, "upgrade-non-reading")
	execHistoricalFixture(t, pool, `UPDATE links SET predicted_library_kind='site',classifier_version='historical-migration-v1' WHERE id=$1`, orphanID)
	execHistoricalFixture(t, pool, `UPDATE links SET content='saved',content_document='saved',content_source='user',predicted_library_kind='site',classifier_version='historical-migration-v1' WHERE id=ANY($1::uuid[])`, []uuid.UUID{mismatchID, validID})
	execHistoricalFixture(t, pool, `UPDATE links SET predicted_library_kind='reading',classifier_version='historical-migration-v1' WHERE id=$1`, retainedID)
	execHistoricalFixture(t, pool, `UPDATE links SET content='user saved',content_document='user saved',content_source='user',library_kind_source='user',library_kind_locked=true,predicted_library_kind='site',classifier_version='historical-migration-v1' WHERE id=$1`, userOwnedID)
	execHistoricalFixture(t, pool, `UPDATE links SET library_kind='site',predicted_library_kind='site',classifier_version='historical-migration-v1' WHERE id=$1`, nonReadingID)
	insertHistoricalMigrationSuggestion(t, pool, mismatchID, 999)
	insertHistoricalMigrationSuggestion(t, pool, validID, 1)
	insertHistoricalMigrationSuggestion(t, pool, userOwnedID, 1)
	insertHistoricalMigrationSuggestion(t, pool, nonReadingID, 1)

	if err := migrate.Up(t.Context(), pool); err != nil {
		t.Fatalf("apply historical repair migration: %v", err)
	}
	assertHistoricalMigrationUpgradeRepair(t, pool, orphanID, mismatchID, validID, retainedID, userOwnedID, nonReadingID)
	first := historicalMigrationFingerprint(t, pool, []uuid.UUID{orphanID, mismatchID, validID, retainedID, userOwnedID, nonReadingID})

	if _, err := pool.Exec(t.Context(), `DELETE FROM schema_migrations WHERE version=$1`, historicalMigrationRepairVersion); err != nil {
		t.Fatalf("remove repair ledger for replay: %v", err)
	}
	if err := migrate.Up(t.Context(), pool); err != nil {
		t.Fatalf("replay historical repair migration: %v", err)
	}
	second := historicalMigrationFingerprint(t, pool, []uuid.UUID{orphanID, mismatchID, validID, retainedID, userOwnedID, nonReadingID})
	if !bytes.Equal(first, second) {
		t.Fatalf("repair replay changed converged state\nfirst:  %s\nsecond: %s", first, second)
	}
}

func historicalMigrationFingerprint(t *testing.T, pool *pgxpool.Pool, ids []uuid.UUID) []byte {
	t.Helper()
	var fingerprint []byte
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
		'links',COALESCE((SELECT jsonb_agg(to_jsonb(link_row) ORDER BY link_row.id) FROM links AS link_row WHERE id=ANY($1::uuid[])),'[]'::jsonb),
		'reviews',COALESCE((SELECT jsonb_agg(to_jsonb(review_row) ORDER BY review_row.id) FROM library_review_items AS review_row WHERE link_id=ANY($1::uuid[])),'[]'::jsonb),
		'entries',COALESCE((SELECT jsonb_agg(to_jsonb(entry_row) ORDER BY entry_row.id) FROM site_entries AS entry_row WHERE link_id=ANY($1::uuid[])),'[]'::jsonb),
		'sites',COALESCE((SELECT jsonb_agg(to_jsonb(site_row) ORDER BY site_row.id) FROM sites AS site_row WHERE id IN (SELECT site_id FROM site_entries WHERE link_id=ANY($1::uuid[]))),'[]'::jsonb),
		'identities',COALESCE((SELECT jsonb_agg(to_jsonb(identity_row) ORDER BY identity_row.identity_key) FROM site_identities AS identity_row WHERE site_id IN (SELECT site_id FROM site_entries WHERE link_id=ANY($1::uuid[]))),'[]'::jsonb),
		'tags',COALESCE((SELECT jsonb_agg(to_jsonb(tag_row) ORDER BY tag_row.site_id,tag_row.normalized_tag) FROM site_tags AS tag_row WHERE site_id IN (SELECT site_id FROM site_entries WHERE link_id=ANY($1::uuid[]))),'[]'::jsonb))`, ids).Scan(&fingerprint); err != nil {
		t.Fatalf("read historical migration fingerprint: %v", err)
	}
	return fingerprint
}

func execHistoricalFixture(t *testing.T, pool *pgxpool.Pool, statement string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), statement, args...); err != nil {
		t.Fatalf("prepare historical migration fixture: %v", err)
	}
}

func insertHistoricalMigrationSuggestion(t *testing.T, pool *pgxpool.Pool, linkID uuid.UUID, contentRevision int64) uuid.UUID {
	t.Helper()
	var reviewID uuid.UUID
	if err := pool.QueryRow(t.Context(), `INSERT INTO library_review_items (kind,link_id,payload,status)
		VALUES ('migration_suggestion',$1,jsonb_build_object('target_kind','site','content_revision',$2::bigint),'pending') RETURNING id`, linkID, contentRevision).Scan(&reviewID); err != nil {
		t.Fatalf("insert historical migration suggestion: %v", err)
	}
	return reviewID
}

func migrationLedgerContains(t *testing.T, pool *pgxpool.Pool, version string) bool {
	t.Helper()
	var found bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&found); err != nil {
		t.Fatalf("read migration ledger %s: %v", version, err)
	}
	return found
}

func restoreMigrationLedger(t *testing.T, pool *pgxpool.Pool, version string, present bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if present {
		_, err = pool.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1) ON CONFLICT DO NOTHING`, version)
	} else {
		_, err = pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version=$1`, version)
	}
	if err != nil {
		t.Errorf("restore migration ledger %s: %v", version, err)
	}
}

func assertHistoricalMigrationUpgradeRepair(t *testing.T, pool *pgxpool.Pool, orphanID, mismatchID, validID, retainedID, userOwnedID, nonReadingID uuid.UUID) {
	t.Helper()
	var orphanClassifier, mismatchClassifier *string
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT classifier_version FROM links WHERE id=$1),
		(SELECT classifier_version FROM links WHERE id=$2)`, orphanID, mismatchID).Scan(&orphanClassifier, &mismatchClassifier); err != nil {
		t.Fatalf("read repaired orphan assessments: %v", err)
	}
	if orphanClassifier != nil || mismatchClassifier != nil {
		t.Fatalf("orphan classifiers = orphan:%v mismatch:%v, want NULL/NULL", orphanClassifier, mismatchClassifier)
	}

	var validClassifier, retainedClassifier, retainedPrediction string
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT classifier_version FROM links WHERE id=$1),
		(SELECT classifier_version FROM links WHERE id=$2),
		(SELECT predicted_library_kind FROM links WHERE id=$2)`, validID, retainedID).Scan(&validClassifier, &retainedClassifier, &retainedPrediction); err != nil {
		t.Fatalf("read preserved historical assessments: %v", err)
	}
	if validClassifier != "historical-migration-v1" || retainedClassifier != "historical-migration-v1" || retainedPrediction != "reading" {
		t.Fatalf("preserved assessments = valid:%q retained:%q/%q", validClassifier, retainedClassifier, retainedPrediction)
	}

	var userSource string
	var userLocked bool
	var userClassifier, nonReadingKind, nonReadingClassifier string
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT library_kind_source FROM links WHERE id=$1),
		(SELECT library_kind_locked FROM links WHERE id=$1),
		(SELECT classifier_version FROM links WHERE id=$1),
		(SELECT library_kind FROM links WHERE id=$2),
		(SELECT classifier_version FROM links WHERE id=$2)`, userOwnedID, nonReadingID).Scan(
		&userSource, &userLocked, &userClassifier, &nonReadingKind, &nonReadingClassifier); err != nil {
		t.Fatalf("read preserved user/non-reading links: %v", err)
	}
	if userSource != "user" || !userLocked || userClassifier != "historical-migration-v1" || nonReadingKind != "site" || nonReadingClassifier != "historical-migration-v1" {
		t.Fatalf("preserved links = user:%q/%t/%q non-reading:%q/%q", userSource, userLocked, userClassifier, nonReadingKind, nonReadingClassifier)
	}

	var validPending, stalePending, staleDismissed int
	if err := pool.QueryRow(t.Context(), `SELECT
		count(*) FILTER (WHERE link_id=$1 AND status='pending'),
		count(*) FILTER (WHERE link_id=ANY($2::uuid[]) AND status='pending'),
		count(*) FILTER (WHERE link_id=ANY($2::uuid[]) AND status='dismissed')
		FROM library_review_items WHERE kind='migration_suggestion'`, validID, []uuid.UUID{mismatchID, userOwnedID, nonReadingID}).Scan(
		&validPending, &stalePending, &staleDismissed); err != nil {
		t.Fatalf("read repaired migration suggestions: %v", err)
	}
	if validPending != 1 || stalePending != 0 || staleDismissed != 3 {
		t.Fatalf("repair suggestion states = valid:%d stale-pending:%d stale-dismissed:%d, want 1/0/3", validPending, stalePending, staleDismissed)
	}
}

func seedHistoricalMigrationCandidate(t *testing.T, pool *pgxpool.Pool, suffix string) uuid.UUID {
	t.Helper()
	rawURL := "https://historical-" + suffix + ".example.com/"
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `INSERT INTO links
		(url,source_key,title,status,content_type,library_kind,library_kind_source,library_kind_locked,content_revision,first_collected_at)
		VALUES ($1,$1,$2,'done','homepage','reading','migration',false,1,NOW()) RETURNING id`,
		rawURL, "Historical "+suffix).Scan(&id); err != nil {
		t.Fatalf("seed historical migration candidate: %v", err)
	}
	return id
}

func installHistoricalMigrationPhaseTwoFailure(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION test_fail_historical_migration_phase_two() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.library_kind='site' AND OLD.library_kind='reading' AND NEW.url LIKE 'https://historical-phase-two-failure.%' THEN
				RAISE EXCEPTION 'injected historical migration phase-two failure';
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER test_fail_historical_migration_phase_two
		BEFORE UPDATE OF library_kind ON links
		FOR EACH ROW EXECUTE FUNCTION test_fail_historical_migration_phase_two()`); err != nil {
		t.Fatalf("install phase-two failure: %v", err)
	}
	t.Cleanup(func() { removeHistoricalMigrationPhaseTwoFailure(t, pool) })
}

func removeHistoricalMigrationPhaseTwoFailure(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		DROP TRIGGER IF EXISTS test_fail_historical_migration_phase_two ON links;
		DROP FUNCTION IF EXISTS test_fail_historical_migration_phase_two()`); err != nil {
		t.Fatalf("remove phase-two failure: %v", err)
	}
}

func installHistoricalMigrationHold(t *testing.T, pool *pgxpool.Pool, suffix string, classID, objectID, seconds int) {
	t.Helper()
	if suffix != "dual-runner" && suffix != "crashed-owner" && suffix != "timed-out-owner" {
		t.Fatalf("unsupported historical migration hold fixture %q", suffix)
	}
	t.Cleanup(func() { removeHistoricalMigrationHold(t, pool) })
	if _, err := pool.Exec(t.Context(), `CREATE TABLE test_historical_migration_hold (
			suffix text PRIMARY KEY,
			class_id integer NOT NULL,
			object_id integer NOT NULL,
			hold_seconds integer NOT NULL
		)`); err != nil {
		t.Fatalf("create historical migration hold control: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO test_historical_migration_hold VALUES ($1,$2,$3,$4)`, suffix, classID, objectID, seconds); err != nil {
		t.Fatalf("configure historical migration hold: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `CREATE FUNCTION test_hold_historical_migration() RETURNS trigger LANGUAGE plpgsql AS $$
		DECLARE control test_historical_migration_hold%ROWTYPE;
		BEGIN
			SELECT * INTO control FROM test_historical_migration_hold
			WHERE NEW.url LIKE 'https://historical-' || suffix || '.%';
			IF FOUND AND NEW.classifier_version='historical-migration-v1'
				AND OLD.classifier_version IS DISTINCT FROM NEW.classifier_version THEN
				PERFORM pg_advisory_xact_lock(control.class_id,control.object_id);
				PERFORM pg_sleep(control.hold_seconds);
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER test_hold_historical_migration
		BEFORE UPDATE OF classifier_version ON links
		FOR EACH ROW EXECUTE FUNCTION test_hold_historical_migration()`); err != nil {
		t.Fatalf("install historical migration hold: %v", err)
	}
}

func removeHistoricalMigrationHold(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		DROP TRIGGER IF EXISTS test_hold_historical_migration ON links;
		DROP FUNCTION IF EXISTS test_hold_historical_migration();
		DROP TABLE IF EXISTS test_historical_migration_hold`); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("remove historical migration hold: %v", err)
	}
}

func installHistoricalMigrationReviewMoveHold(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() { removeHistoricalMigrationReviewMoveHold(t, pool) })
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION test_hold_historical_review_move() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.library_kind='site' AND OLD.library_kind='reading' AND NEW.url LIKE 'https://historical-review-race.%' THEN
				PERFORM pg_advisory_xact_lock(181,184);
				PERFORM pg_sleep(1);
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER test_hold_historical_review_move
		BEFORE UPDATE OF library_kind ON links
		FOR EACH ROW EXECUTE FUNCTION test_hold_historical_review_move()`); err != nil {
		t.Fatalf("install historical review move hold: %v", err)
	}
}

func removeHistoricalMigrationReviewMoveHold(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		DROP TRIGGER IF EXISTS test_hold_historical_review_move ON links;
		DROP FUNCTION IF EXISTS test_hold_historical_review_move()`); err != nil {
		t.Errorf("remove historical review move hold: %v", err)
	}
}

func waitForHistoricalMigrationAdvisoryLock(t *testing.T, pool *pgxpool.Pool, classID, objectID int) uint32 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var backendPID *uint32
		if err := pool.QueryRow(t.Context(), `SELECT pid::bigint FROM pg_locks
			WHERE locktype='advisory' AND classid=$1::oid AND objid=$2::oid AND objsubid=2 AND granted
			LIMIT 1`, classID, objectID).Scan(&backendPID); err == nil && backendPID != nil {
			return *backendPID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for advisory lock (%d,%d)", classID, objectID)
	return 0
}

func assertHistoricalMigrationAggregateCount(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	var links, entries int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM links WHERE library_kind='site'),
		(SELECT count(*) FROM site_entries)`).Scan(&links, &entries); err != nil {
		t.Fatalf("count historical migration aggregate: %v", err)
	}
	if links != want || entries != want {
		t.Fatalf("historical migration aggregate = links:%d entries:%d, want %d/%d", links, entries, want, want)
	}
}
