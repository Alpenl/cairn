package dbintegration

import (
	"testing"

	"webtag/internal/model"
)

func TestStalledTranslationAdvancesGeneration(t *testing.T) {
	harness := newRF5AScheduleHarness(t)
	fixture := harness.createSource(t, "stalled-translation", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Alpha beta")
	service := harness.service(harness.queue)
	request := model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
		ExpectedContentRevision: &fixture.revision,
	}

	first, err := service.Create(fixture.ctx, fixture.linkID, request)
	if err != nil || first == nil {
		t.Fatalf("first Create() = %+v, %v", first, err)
	}
	if _, err := harness.pool.Exec(fixture.ctx, `UPDATE link_translations
		SET status='processing', updated_at=NOW()-INTERVAL '24 hours' WHERE id=$1`, first.ID); err != nil {
		t.Fatalf("age translation attempt: %v", err)
	}

	again, err := service.Create(fixture.ctx, fixture.linkID, request)
	if err != nil || again == nil || again.ID != first.ID || again.AttemptGeneration != first.AttemptGeneration+1 ||
		again.Status != model.TranslationStatusPending {
		t.Fatalf("stalled Create() = %+v, %v; first=%+v", again, err, first)
	}
	oldAttempt := model.TranslationAttempt{
		TranslationID: first.ID, AttemptGeneration: first.AttemptGeneration,
		SourceHash: first.SourceHash, SourceContentRevision: first.SourceContentRevision,
	}
	if item, err := harness.translations.MarkProcessing(fixture.ctx, oldAttempt); err != nil || item != nil {
		t.Fatalf("old MarkProcessing() = %+v, %v; want stale rejection", item, err)
	}
}

func TestFreshTranslationReusesCurrentGeneration(t *testing.T) {
	harness := newRF5AScheduleHarness(t)
	fixture := harness.createSource(t, "fresh-translation", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Alpha beta")
	service := harness.service(harness.queue)
	request := model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
		ExpectedContentRevision: &fixture.revision,
	}

	first, err := service.Create(fixture.ctx, fixture.linkID, request)
	if err != nil || first == nil {
		t.Fatalf("first Create() = %+v, %v", first, err)
	}
	if _, err := harness.pool.Exec(fixture.ctx, `UPDATE link_translations
		SET status='processing', updated_at=NOW() WHERE id=$1`, first.ID); err != nil {
		t.Fatalf("mark translation fresh: %v", err)
	}
	again, err := service.Create(fixture.ctx, fixture.linkID, request)
	if err != nil || again == nil || again.ID != first.ID || again.AttemptGeneration != first.AttemptGeneration {
		t.Fatalf("fresh Create() = %+v, %v; first=%+v", again, err, first)
	}

	var jobs int
	if err := harness.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM river_job
		WHERE kind=$1 AND args->>'translation_id'=$2`, model.TranslationJobKind, first.ID.String()).Scan(&jobs); err != nil {
		t.Fatalf("count River jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("River jobs = %d, want one reused attempt", jobs)
	}
}
