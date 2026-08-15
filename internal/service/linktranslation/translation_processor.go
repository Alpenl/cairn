package linktranslation

import (
	"context"
	"fmt"
	"log/slog"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service/translator"
)

const translationFailureMessage = "翻译服务暂时不可用，请重试"
const translationCancelledMessage = "翻译任务已取消，请重试"

type JobProcessor interface {
	Run(context.Context, model.TranslationAttempt) error
	RecordDiscard(context.Context, model.TranslationAttempt, error) error
	RecordCancellation(context.Context, model.TranslationAttempt, error) error
}

type ProcessorOptions struct {
	Translations repository.TranslationStore
	Translator   translator.Translator
	Logger       *slog.Logger
}

type Processor struct {
	translations repository.TranslationStore
	translator   translator.Translator
	logger       *slog.Logger
}

func NewProcessor(opts ProcessorOptions) *Processor {
	return &Processor{
		translations: opts.Translations,
		translator:   opts.Translator,
		logger:       opts.Logger,
	}
}

func (p *Processor) Run(ctx context.Context, attempt model.TranslationAttempt) error {
	item, err := p.translations.MarkProcessing(ctx, attempt)
	if err != nil {
		return err
	}
	if item == nil {
		p.logRejected(ctx, "processing", attempt)
		return nil
	}
	format := translator.FormatPlain
	if item.SourceFormat == model.TranslationFormatMarkdown {
		format = translator.FormatMarkdown
	}
	result, err := p.translator.Translate(ctx, translator.Request{
		Text:   item.SourceText,
		Format: format,
	})
	if err != nil {
		return fmt.Errorf("translate link content: %w", err)
	}
	applied, err := p.translations.Complete(ctx, attempt, result.Text, result.Model)
	if err != nil {
		return err
	}
	if !applied {
		p.logRejected(ctx, "success", attempt)
	}
	return nil
}

func (p *Processor) RecordDiscard(ctx context.Context, attempt model.TranslationAttempt, _ error) error {
	return p.projectFailure(ctx, attempt, translationFailureMessage, "failure")
}

func (p *Processor) RecordCancellation(ctx context.Context, attempt model.TranslationAttempt, _ error) error {
	return p.projectFailure(ctx, attempt, translationCancelledMessage, "cancellation")
}

func (p *Processor) projectFailure(ctx context.Context, attempt model.TranslationAttempt, message, projection string) error {
	applied, err := p.translations.Fail(ctx, attempt, message)
	if err != nil {
		return err
	}
	if !applied {
		p.logRejected(ctx, projection, attempt)
	}
	return nil
}

func (p *Processor) logRejected(ctx context.Context, projection string, attempt model.TranslationAttempt) {
	if p.logger == nil {
		return
	}
	p.logger.WarnContext(ctx, "translation attempt projection rejected",
		"reason", model.TranslationAttemptRejectionNotCurrent.String(),
		"projection", projection,
		"translation_id", attempt.TranslationID.String(),
		"river_job_id", attempt.RiverJobID,
		"attempt_generation", attempt.AttemptGeneration,
	)
}

var _ JobProcessor = (*Processor)(nil)
