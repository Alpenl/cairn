package app

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service/translator"
)

type rolloutWiringTranslator struct{}

func (rolloutWiringTranslator) Translate(context.Context, translator.Request) (translator.Result, error) {
	return translator.Result{}, nil
}

func TestBuildTranslationProcessorWiresProductionLogger(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	var logs bytes.Buffer
	layer := &persistenceLayer{
		translations: repository.NewPGXTranslationRepository(mock),
		logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	}
	processor := buildTranslationProcessor(layer, rolloutWiringTranslator{})
	translationID := uuid.New()
	const sourceHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	attempt := model.TranslationAttempt{
		TranslationID:     translationID,
		AttemptGeneration: 3, RiverJobID: 91,
		SourceHash: sourceHash,
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE link_translations")).
		WithArgs("翻译任务已取消，请重试", translationID, int64(91), int64(3), sourceHash, (*int64)(nil)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	if err := processor.RecordCancellation(t.Context(), attempt, context.Canceled); err != nil {
		t.Fatalf("RecordCancellation() error = %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "translation attempt projection rejected") ||
		!strings.Contains(got, "reason="+model.TranslationAttemptRejectionNotCurrent.String()) {
		t.Fatalf("production processor log = %q, want observable rejection", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
