package service

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// HistoricalMigrationCursor is stable across restarts and deliberately uses
// the immutable creation order rather than a mutable updated_at value.
type HistoricalMigrationCursor = repository.HistoricalMigrationCursor
type HistoricalMigrationCandidate = repository.HistoricalMigrationCandidate
type HistoricalMigrationAssessment = repository.HistoricalMigrationAssessment
type HistoricalMigrationStore = repository.HistoricalMigrationStore

type HistoricalMigrationRunOptions struct {
	BatchSize int
	DryRun    bool
}

type HistoricalMigrationReport struct {
	Scanned          int
	PredictedSite    int
	AutoMigrated     int
	Suggested        int
	Retained         int
	WouldAutoMigrate int
	WouldSuggest     int
	WouldRetain      int
	// Skipped counts candidates that were locked by another replica or changed
	// between listing and the transactional re-read. They remain Scanned, but
	// never enter a committed outcome bucket.
	Skipped int
	// LastCursor 只在**单次 Run 内**有效：Run 每次都从零值游标起步，worker 拿到
	// report 之后也只用于日志，从不持久化、从不喂回下一次 Run。
	//
	// 所以别把它当成断点续跑的令牌。跨 Run 不重复处理由 repository 的
	// classifier predicate 保证；只有 rolling upgrade 留下的 orphan assessment
	// 会被显式重新纳入候选。
	// 游标在这里的唯一职责是让**本轮**的批次向前推进，不要原地打转。
	LastCursor HistoricalMigrationCursor
}

func (r *HistoricalMigrationReport) tallyCommitted(outcome repository.HistoricalMigrationOutcome, assessment repository.HistoricalMigrationAssessment) error {
	var counter *int
	switch outcome {
	case repository.HistoricalMigrationOutcomeAutoMigrated:
		counter = &r.AutoMigrated
	case repository.HistoricalMigrationOutcomeSuggested:
		counter = &r.Suggested
	case repository.HistoricalMigrationOutcomeRetained:
		counter = &r.Retained
	default:
		return fmt.Errorf("historical migration: invalid committed outcome %q", outcome)
	}
	if assessment.PredictedKind == model.LibraryKindSite {
		r.PredictedSite++
	}
	(*counter)++
	return nil
}

func (r *HistoricalMigrationReport) tallyWould(assessment repository.HistoricalMigrationAssessment) {
	switch {
	case assessment.AutoMigrate:
		r.WouldAutoMigrate++
	case assessment.Suggest:
		r.WouldSuggest++
	default:
		r.WouldRetain++
	}
}

// HistoricalMigrationRunner performs only local, conservative assessment.
// It deliberately neither fetches pages nor calls an AI model; unclear rows
// remain reading content. Stores must claim and re-read the candidate, persist
// the assessment, apply the final action, and return its outcome atomically.
type HistoricalMigrationRunner struct{ store HistoricalMigrationStore }

func NewHistoricalMigrationRunner(store HistoricalMigrationStore) *HistoricalMigrationRunner {
	return &HistoricalMigrationRunner{store: store}
}

func (r *HistoricalMigrationRunner) Run(ctx context.Context, options HistoricalMigrationRunOptions) (HistoricalMigrationReport, error) { //nolint:gocyclo // 批处理主循环，同 SiteEmbeddingBackfiller.Run
	if options.BatchSize < 1 {
		options.BatchSize = 100
	}
	if options.BatchSize > 1000 {
		options.BatchSize = 1000
	}
	var report HistoricalMigrationReport
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		batch, err := r.store.ListHistoricalMigrationCandidates(ctx, report.LastCursor, options.BatchSize)
		if err != nil {
			return report, err
		}
		if len(batch) == 0 {
			return report, nil
		}
		for _, candidate := range batch {
			assessment := assessHistoricalCandidate(candidate)
			report.Scanned++
			if options.DryRun {
				report.tallyWould(assessment)
			} else {
				outcome, err := r.store.CommitHistoricalMigrationAssessment(ctx, assessment)
				if err != nil {
					return report, err
				}
				if outcome == repository.HistoricalMigrationOutcomeNoop {
					report.Skipped++
					report.LastCursor = HistoricalMigrationCursor{CreatedAt: candidate.CreatedAt, ID: candidate.ID}
					continue
				}
				if err := report.tallyCommitted(outcome, assessment); err != nil {
					return report, err
				}
			}
			report.LastCursor = HistoricalMigrationCursor{CreatedAt: candidate.CreatedAt, ID: candidate.ID}
		}
		if len(batch) < options.BatchSize {
			return report, nil
		}
	}
}

func assessHistoricalCandidate(candidate HistoricalMigrationCandidate) HistoricalMigrationAssessment {
	assessment := HistoricalMigrationAssessment{Candidate: candidate, PredictedKind: model.LibraryKindReading, Reason: "migration_retain_reading"}
	if candidate.LibraryKind != model.LibraryKindReading || candidate.Source != model.LibraryKindSourceMigration || candidate.Locked {
		assessment.Reason = "migration_ineligible"
		return assessment
	}
	parsed, err := url.Parse(candidate.URL)
	if err != nil || parsed.Hostname() == "" {
		assessment.Reason = "migration_invalid_url"
		return assessment
	}
	path := strings.Trim(parsed.Path, "/")
	if path != "" && candidate.ContentType != model.ContentTypeHomepage {
		return assessment
	}
	// Only root/homepage candidates qualify as automatic migration. This is
	// intentionally stricter than live classification because local notes are
	// not visible to the server and historical content must stay non-destructive.
	assessment.PredictedKind = model.LibraryKindSite
	assessment.Confidence = .99
	assessment.Reason = "migration_obvious_homepage"
	if candidate.HasReadingAssets() {
		assessment.Suggest = true
		assessment.Reason = "migration_assets_require_review"
		return assessment
	}
	assessment.AutoMigrate = true
	return assessment
}
