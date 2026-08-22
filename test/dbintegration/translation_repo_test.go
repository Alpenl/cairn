package dbintegration

import (
	"testing"

	"webtag/internal/model"
)

func TestTranslationGenerationRejectsStaleWorkerProjection(t *testing.T) {
	harness := newRF5AScheduleHarness(t)
	fixture := harness.createSource(t, "generation-fence", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Alpha beta")
	service := harness.service(harness.queue)
	request := model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
		ExpectedContentRevision: &fixture.revision,
	}

	first, err := service.Create(fixture.ctx, fixture.linkID, request)
	if err != nil || first == nil || first.AttemptGeneration != 1 {
		t.Fatalf("first Create() = %+v, %v", first, err)
	}
	firstAttempt := model.TranslationAttempt{
		TranslationID: first.ID, AttemptGeneration: first.AttemptGeneration,
		SourceHash: first.SourceHash, SourceContentRevision: first.SourceContentRevision,
	}
	if processing, err := harness.translations.MarkProcessing(fixture.ctx, firstAttempt); err != nil || processing == nil {
		t.Fatalf("first MarkProcessing() = %+v, %v", processing, err)
	}
	if completed, err := harness.translations.Complete(fixture.ctx, firstAttempt, "first", "test"); err != nil || !completed {
		t.Fatalf("first Complete() = %v, %v", completed, err)
	}

	request.Force = true
	second, err := service.Create(fixture.ctx, fixture.linkID, request)
	if err != nil || second == nil || second.ID != first.ID || second.AttemptGeneration != 2 {
		t.Fatalf("forced Create() = %+v, %v; first=%+v", second, err, first)
	}

	if completed, err := harness.translations.Complete(fixture.ctx, firstAttempt, "stale", "test"); err != nil || completed {
		t.Fatalf("stale Complete() = %v, %v; want rejected", completed, err)
	}
	secondAttempt := model.TranslationAttempt{
		TranslationID: second.ID, AttemptGeneration: second.AttemptGeneration,
		SourceHash: second.SourceHash, SourceContentRevision: second.SourceContentRevision,
	}
	if processing, err := harness.translations.MarkProcessing(fixture.ctx, secondAttempt); err != nil || processing == nil {
		t.Fatalf("second MarkProcessing() = %+v, %v", processing, err)
	}
	if completed, err := harness.translations.Complete(fixture.ctx, secondAttempt, "current", "test"); err != nil || !completed {
		t.Fatalf("current Complete() = %v, %v", completed, err)
	}

	stored, err := harness.translations.GetByID(fixture.ctx, first.ID)
	if err != nil || stored == nil || stored.Status != model.TranslationStatusDone ||
		stored.AttemptGeneration != 2 || stored.TranslatedText == nil || *stored.TranslatedText != "current" {
		t.Fatalf("stored product = %+v, %v", stored, err)
	}

	var jobs int
	if err := harness.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM river_job
		WHERE kind=$1 AND args->>'translation_id'=$2`, model.TranslationJobKind, first.ID.String()).Scan(&jobs); err != nil {
		t.Fatalf("count River attempts: %v", err)
	}
	if jobs != 2 {
		t.Fatalf("River attempts = %d, want one attempt per accepted command", jobs)
	}
}
