package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

type historicalMigrationStoreFake struct {
	rows          []HistoricalMigrationCandidate
	committed     []HistoricalMigrationAssessment
	outcomes      []repository.HistoricalMigrationOutcome
	commitOutcome repository.HistoricalMigrationOutcome
	commitErr     error
	listCalls     int
}

// 一次正常的 Run 最多列 ceil(len(rows)/limit)+1 次。远高于此就说明游标没在推进，
// 外层 for 正拿同一个游标无限列下去。
const maxFakeListCalls = 50

func (f *historicalMigrationStoreFake) ListHistoricalMigrationCandidates(_ context.Context, cursor HistoricalMigrationCursor, limit int) ([]HistoricalMigrationCandidate, error) {
	// 死循环护栏。没有它，「游标不推进」这类回归的症状是**测试挂死**，要等整个
	// package 的 timeout 才 panic dump——诊断成本高，还拖垮整个包的反馈。有了它，
	// 同一个回归在一秒内变成一条说明白了的断言失败。
	f.listCalls++
	if f.listCalls > maxFakeListCalls {
		return nil, errors.New("cursor is not advancing: the run listed the same batch over and over")
	}
	var out []HistoricalMigrationCandidate
	for _, row := range f.rows {
		if !cursor.CreatedAt.IsZero() && (row.CreatedAt.Before(cursor.CreatedAt) || (row.CreatedAt.Equal(cursor.CreatedAt) && row.ID.String() <= cursor.ID.String())) {
			continue
		}
		out = append(out, row)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
func (f *historicalMigrationStoreFake) CommitHistoricalMigrationAssessment(_ context.Context, a HistoricalMigrationAssessment) (repository.HistoricalMigrationOutcome, error) {
	if f.commitErr != nil {
		return repository.HistoricalMigrationOutcomeNoop, f.commitErr
	}
	outcome := f.commitOutcome
	if outcome == "" {
		switch {
		case a.AutoMigrate:
			outcome = repository.HistoricalMigrationOutcomeAutoMigrated
		case a.Suggest:
			outcome = repository.HistoricalMigrationOutcomeSuggested
		default:
			outcome = repository.HistoricalMigrationOutcomeRetained
		}
	}
	f.committed = append(f.committed, a)
	f.outcomes = append(f.outcomes, outcome)
	return outcome, nil
}

func migrationCandidate(now time.Time, rawURL string) HistoricalMigrationCandidate {
	return HistoricalMigrationCandidate{ID: uuid.New(), URL: rawURL, ContentType: model.ContentTypeHomepage, ContentRevision: 1, CreatedAt: now, LibraryKind: model.LibraryKindReading, Source: model.LibraryKindSourceMigration}
}

func TestHistoricalMigrationAutomaticallyMovesOnlyAssetFreeObviousHomepage(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	auto := migrationCandidate(now, "https://example.com/")
	assets := migrationCandidate(now.Add(time.Second), "https://docs.example.com/")
	assets.HasContent = true
	article := migrationCandidate(now.Add(2*time.Second), "https://example.com/posts/one")
	article.ContentType = model.ContentTypeArticle
	store := &historicalMigrationStoreFake{rows: []HistoricalMigrationCandidate{auto, assets, article}}
	report, err := NewHistoricalMigrationRunner(store).Run(context.Background(), HistoricalMigrationRunOptions{BatchSize: 2})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.Scanned != 3 || report.AutoMigrated != 1 || report.Suggested != 1 || report.Retained != 1 || len(store.committed) != 3 || store.committed[0].Candidate.ID != auto.ID || store.committed[1].Candidate.ID != assets.ID {
		t.Fatalf("report=%#v committed=%#v outcomes=%#v", report, store.committed, store.outcomes)
	}
}

func TestHistoricalMigrationDryRunProducesNoWrites(t *testing.T) {
	t.Parallel()
	store := &historicalMigrationStoreFake{rows: []HistoricalMigrationCandidate{migrationCandidate(time.Now().UTC(), "https://example.com/")}}
	report, err := NewHistoricalMigrationRunner(store).Run(context.Background(), HistoricalMigrationRunOptions{DryRun: true})
	if err != nil || report.AutoMigrated != 0 || report.WouldAutoMigrate != 1 {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
	if len(store.committed) != 0 {
		t.Fatalf("dry run wrote store: %#v", store)
	}
}

func TestHistoricalMigrationNeverOverridesUserLockedOrNonMigrationRows(t *testing.T) {
	t.Parallel()
	locked := migrationCandidate(time.Now().UTC(), "https://example.com/")
	locked.Locked = true
	assessment := assessHistoricalCandidate(locked)
	if assessment.AutoMigrate || assessment.Suggest || assessment.Reason != "migration_ineligible" {
		t.Fatalf("locked assessment = %#v", assessment)
	}
}

func TestHistoricalMigrationConcurrentConflictIsNoopNotSuggestion(t *testing.T) {
	t.Parallel()
	store := &historicalMigrationStoreFake{
		rows:          []HistoricalMigrationCandidate{migrationCandidate(time.Now().UTC(), "https://example.com/")},
		commitOutcome: repository.HistoricalMigrationOutcomeNoop,
	}
	report, err := NewHistoricalMigrationRunner(store).Run(context.Background(), HistoricalMigrationRunOptions{})
	if err != nil || report.AutoMigrated != 0 || report.Suggested != 0 || report.Skipped != 1 {
		t.Fatalf("Run() = %#v, %v, want committed no-op", report, err)
	}
}

// A transactional re-read no-op is an expected ownership/staleness outcome,
// not a batch error. Every no-op must still advance the in-run cursor.
func TestHistoricalMigrationSkipsRevisionConflictInsteadOfAbortingRun(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	store := &historicalMigrationStoreFake{
		rows: []HistoricalMigrationCandidate{
			migrationCandidate(now, "https://example.com/"),
			migrationCandidate(now.Add(time.Second), "https://other.example.com/"),
		},
		commitOutcome: repository.HistoricalMigrationOutcomeNoop,
	}

	report, err := NewHistoricalMigrationRunner(store).Run(context.Background(), HistoricalMigrationRunOptions{BatchSize: 2})
	if err != nil {
		t.Fatalf("Run() error = %v; a revision conflict must skip the candidate, not abort the batch", err)
	}
	if report.Scanned != 2 {
		t.Fatalf("Scanned = %d, want 2 — both candidates were still examined", report.Scanned)
	}
	if report.Skipped != 2 {
		t.Fatalf("Skipped = %d, want 2", report.Skipped)
	}
	// No committed bucket may be incremented for a no-op.
	if report.AutoMigrated != 0 || report.Suggested != 0 || report.Retained != 0 {
		t.Fatalf("outcome buckets not rolled back: %#v", report)
	}
	if len(store.outcomes) != 2 {
		t.Fatalf("committed outcomes = %d, want two no-ops", len(store.outcomes))
	}
}

// TestHistoricalMigrationStillAbortsOnRealRecordFailure keeps the no-op path
// narrow: a genuine commit failure must stop rather than become Skipped.
func TestHistoricalMigrationStillAbortsOnRealRecordFailure(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	store := &historicalMigrationStoreFake{
		rows:      []HistoricalMigrationCandidate{migrationCandidate(now, "https://example.com/")},
		commitErr: errors.New("connection reset"),
	}

	report, err := NewHistoricalMigrationRunner(store).Run(context.Background(), HistoricalMigrationRunOptions{BatchSize: 2})
	if err == nil {
		t.Fatal("Run() error = nil; a non-conflict store failure must abort the batch")
	}
	// Skipped 必须为 0，证明真实故障没有被当成可容忍的 ownership no-op。
	if report.Skipped != 0 {
		t.Fatalf("Skipped = %d, want 0; a genuine store failure is an abort, not a tolerated skip", report.Skipped)
	}
}

func TestHistoricalMigrationFailedPhaseTwoReportsNoCommittedOutcome(t *testing.T) {
	t.Parallel()
	phaseTwoErr := errors.New("site aggregation failed")
	store := &historicalMigrationStoreFake{
		rows:      []HistoricalMigrationCandidate{migrationCandidate(time.Now().UTC(), "https://example.com/")},
		commitErr: phaseTwoErr,
	}

	report, err := NewHistoricalMigrationRunner(store).Run(context.Background(), HistoricalMigrationRunOptions{})
	if !errors.Is(err, phaseTwoErr) {
		t.Fatalf("Run() error = %v, want phase-two failure", err)
	}
	if report.Scanned != 1 {
		t.Fatalf("Scanned = %d, want 1", report.Scanned)
	}
	if report.PredictedSite != 0 || report.AutoMigrated != 0 || report.Suggested != 0 || report.Retained != 0 {
		t.Fatalf("failed phase two reported uncommitted outcome: %#v", report)
	}
}

func TestHistoricalMigrationInvalidCommittedOutcomeChangesNoCounters(t *testing.T) {
	t.Parallel()
	store := &historicalMigrationStoreFake{
		rows:          []HistoricalMigrationCandidate{migrationCandidate(time.Now().UTC(), "https://example.com/")},
		commitOutcome: repository.HistoricalMigrationOutcome("invalid"),
	}

	report, err := NewHistoricalMigrationRunner(store).Run(context.Background(), HistoricalMigrationRunOptions{})
	if err == nil {
		t.Fatal("Run() error = nil, want invalid committed outcome")
	}
	if report.PredictedSite != 0 || report.AutoMigrated != 0 || report.Suggested != 0 || report.Retained != 0 {
		t.Fatalf("invalid committed outcome changed counters: %#v", report)
	}
}
