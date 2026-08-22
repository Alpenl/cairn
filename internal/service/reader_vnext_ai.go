package service

import (
	"context"
	"errors"

	"webtag/internal/problem"
)

// Keep the server-owned context bounded as one unit. The repository already
// limits each field, but content + summary + tags + thoughts can otherwise
// exceed the Reader prompt budget before the user's question is appended.
const readerAIMaxContextRunes = 12000

const readerAIContextTruncatedMarker = "\n[Reader AI context truncated]"

var errReaderAIEmptyResponse = errors.New("reader AI provider returned an empty response")

// mapReaderAIError converts provider and request-lifecycle failures into a
// stable client classification without copying the upstream error text. An
// existing public problem, such as cooldown_active or reader_not_found, remains
// authoritative and is not reclassified.
func mapReaderAIError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := problem.As(err); ok {
		return err
	}

	kind := problem.Upstream
	code := "ai_provider_error"
	message := "AI provider request failed"
	switch {
	case errors.Is(err, context.Canceled):
		kind = problem.Canceled
		code = "ai_request_canceled"
		message = "AI request was canceled"
	case errors.Is(err, context.DeadlineExceeded):
		kind = problem.Timeout
		code = "ai_timeout"
		message = "AI provider request timed out"
	case errors.Is(err, errReaderAIEmptyResponse):
		code = "ai_empty_response"
		message = "AI provider returned no answer"
	}

	return problem.Wrap(kind, code, message, err)
}

func boundReaderAIContext(value string) string {
	runes := []rune(value)
	if len(runes) <= readerAIMaxContextRunes {
		return value
	}
	marker := []rune(readerAIContextTruncatedMarker)
	keep := readerAIMaxContextRunes - len(marker)
	if keep <= 0 {
		return string(marker[:readerAIMaxContextRunes])
	}
	return string(runes[:keep]) + readerAIContextTruncatedMarker
}
