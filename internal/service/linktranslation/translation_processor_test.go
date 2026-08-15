package linktranslation

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service/translator"
)

type processorTranslationStore struct {
	row             *model.LinkTranslation
	markedAttempt   model.TranslationAttempt
	completedID     uuid.UUID
	completedText   string
	completedModel  string
	failedID        uuid.UUID
	failedMessage   string
	completeApplied *bool
	failApplied     *bool
}

func (*processorTranslationStore) FindByIdentity(context.Context, repository.UpsertTranslationParams) (*model.LinkTranslation, error) {
	return nil, nil
}

func (*processorTranslationStore) ListByLink(context.Context, uuid.UUID) ([]model.LinkTranslation, error) {
	return nil, nil
}

func (s *processorTranslationStore) GetByID(context.Context, uuid.UUID) (*model.LinkTranslation, error) {
	return s.row, nil
}

func (s *processorTranslationStore) MarkProcessing(_ context.Context, attempt model.TranslationAttempt) (*model.LinkTranslation, error) {
	s.markedAttempt = attempt
	if s.row == nil {
		return nil, nil
	}
	copy := *s.row
	copy.Status = model.TranslationStatusProcessing
	return &copy, nil
}

func (s *processorTranslationStore) Complete(_ context.Context, attempt model.TranslationAttempt, text, modelName string) (bool, error) {
	s.completedID = attempt.TranslationID
	s.completedText = text
	s.completedModel = modelName
	if s.completeApplied != nil {
		return *s.completeApplied, nil
	}
	return true, nil
}

func (s *processorTranslationStore) Fail(_ context.Context, attempt model.TranslationAttempt, message string) (bool, error) {
	s.failedID = attempt.TranslationID
	s.failedMessage = message
	if s.failApplied != nil {
		return *s.failApplied, nil
	}
	return true, nil
}

type fakeTranslator struct {
	request translator.Request
	result  translator.Result
	err     error
	calls   int
}

func (f *fakeTranslator) Translate(_ context.Context, req translator.Request) (translator.Result, error) {
	f.calls++
	f.request = req
	return f.result, f.err
}

func TestTranslationProcessorRejectsStaleAttemptBeforeTranslator(t *testing.T) {
	t.Parallel()

	attempt := model.TranslationAttempt{
		TranslationID:     uuid.New(),
		AttemptGeneration: 4, RiverJobID: 901,
	}
	store := &processorTranslationStore{}
	engine := &fakeTranslator{}
	var logs bytes.Buffer
	processor := NewProcessor(ProcessorOptions{
		Translations: store,
		Translator:   engine,
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	})

	if err := processor.Run(context.Background(), attempt); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if engine.calls != 0 {
		t.Fatalf("translator calls = %d, want 0", engine.calls)
	}
	if got := logs.String(); !strings.Contains(got, "translation attempt projection rejected") ||
		!strings.Contains(got, "reason="+model.TranslationAttemptRejectionNotCurrent.String()) ||
		!strings.Contains(got, "projection=processing") {
		t.Fatalf("log = %q", got)
	}
}

func TestTranslationProcessorLogsRejectedTerminalAttempts(t *testing.T) {
	t.Parallel()

	attempt := model.TranslationAttempt{
		TranslationID:     uuid.New(),
		AttemptGeneration: 4, RiverJobID: 902,
	}
	for _, projection := range []string{"success", "failure", "cancellation"} {
		t.Run(projection, func(t *testing.T) {
			t.Parallel()
			applied := false
			store := &processorTranslationStore{completeApplied: &applied, failApplied: &applied}
			if projection == "success" {
				store.row = &model.LinkTranslation{
					ID:         attempt.TranslationID,
					SourceText: "hello", SourceFormat: model.TranslationFormatPlain,
				}
			}
			var logs bytes.Buffer
			processor := NewProcessor(ProcessorOptions{
				Translations: store,
				Translator:   &fakeTranslator{result: translator.Result{Text: "你好", Model: "test"}},
				Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
			})
			var err error
			switch projection {
			case "success":
				err = processor.Run(context.Background(), attempt)
			case "failure":
				err = processor.RecordDiscard(context.Background(), attempt, errors.New("discarded"))
			case "cancellation":
				err = processor.RecordCancellation(context.Background(), attempt, context.Canceled)
			}
			if err != nil {
				t.Fatalf("projection error = %v", err)
			}
			if got := logs.String(); !strings.Contains(got, "translation attempt projection rejected") ||
				!strings.Contains(got, "reason="+model.TranslationAttemptRejectionNotCurrent.String()) ||
				!strings.Contains(got, "projection="+projection) {
				t.Fatalf("log = %q", got)
			}
		})
	}
}

func TestTranslationProcessorPersistsResult(t *testing.T) {
	t.Parallel()

	linkID, translationID := uuid.New(), uuid.New()
	riverJobID := int64(301)
	attempt := model.TranslationAttempt{
		TranslationID:     translationID,
		AttemptGeneration: 1, RiverJobID: riverJobID,
	}
	store := &processorTranslationStore{row: &model.LinkTranslation{
		ID:                translationID,
		LinkID:            linkID,
		SourceText:        "# Heading\n\nBody",
		SourceFormat:      model.TranslationFormatMarkdown,
		Status:            model.TranslationStatusPending,
		AttemptGeneration: 1,
		CurrentRiverJobID: &riverJobID,
	}}
	engine := &fakeTranslator{result: translator.Result{
		Text: "# 标题\n\n正文", Model: "grok-4.3-fast",
	}}
	processor := NewProcessor(ProcessorOptions{
		Translations: store,
		Translator:   engine,
	})

	if err := processor.Run(context.Background(), attempt); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if engine.request.Text != store.row.SourceText || engine.request.Format != translator.FormatMarkdown {
		t.Fatalf("translator request = %+v", engine.request)
	}
	if store.completedID != translationID || store.completedText != "# 标题\n\n正文" || store.completedModel != "grok-4.3-fast" {
		t.Fatalf("completed = %s %q %q", store.completedID, store.completedText, store.completedModel)
	}
}

func TestTranslationProcessorStoresSafeFinalFailure(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	attempt := model.TranslationAttempt{
		TranslationID:     translationID,
		AttemptGeneration: 4, RiverJobID: 909,
	}
	store := &processorTranslationStore{}
	processor := NewProcessor(ProcessorOptions{
		Translations: store,
		Translator:   &fakeTranslator{err: errors.New("upstream body contained secret details")},
	})

	if err := processor.RecordDiscard(context.Background(), attempt, errors.New("upstream body contained secret details")); err != nil {
		t.Fatalf("RecordDiscard() error = %v", err)
	}
	if store.failedID != translationID || store.failedMessage != "翻译服务暂时不可用，请重试" {
		t.Fatalf("stored failure = %s %q", store.failedID, store.failedMessage)
	}
}

func TestTranslationProcessorDoesNotCompleteFailedAttempt(t *testing.T) {
	t.Parallel()

	linkID, translationID := uuid.New(), uuid.New()
	attempt := model.TranslationAttempt{
		TranslationID:     translationID,
		AttemptGeneration: 2, RiverJobID: 302,
	}
	store := &processorTranslationStore{row: &model.LinkTranslation{
		ID: translationID, LinkID: linkID,
		SourceText: "A long translation can fail after earlier chunks.",
	}}
	processor := NewProcessor(ProcessorOptions{
		Translations: store,
		Translator: &fakeTranslator{
			result: translator.Result{Model: "grok-4.3-fast"},
			err:    errors.New("later chunk timed out"),
		},
	})

	if err := processor.Run(context.Background(), attempt); err == nil {
		t.Fatal("Run() error = nil, want translation failure")
	}
	if store.completedID != uuid.Nil {
		t.Fatalf("failed translation was completed as %s", store.completedID)
	}
}
