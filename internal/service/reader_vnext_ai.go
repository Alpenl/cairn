package service

import (
	"context"
	"errors"
	"net/http"

	"webtag/internal/httperr"
)

// net/http does not expose the de-facto client-closed status used by many
// gateways. Keep it local to the Reader AI surface instead of widening the
// shared HTTP error contract for one endpoint.
const readerAIClientClosedRequest = 499

// Keep the server-owned context bounded as one unit. The repository already
// limits each field, but content + summary + tags + thoughts can otherwise
// exceed the Reader prompt budget before the user's question is appended.
const readerAIMaxContextRunes = 12000

const readerAIContextTruncatedMarker = "\n[Reader AI context truncated]"

var errReaderAIEmptyResponse = errors.New("reader AI provider returned an empty response")

type readerAICapability interface {
	Available() bool
}

// readerAIStatusError keeps the public error classification and the original
// cause together. The cause is available to errors.Is in service tests and
// observability code, while Error() and the HTTP carrier never expose provider
// payloads, prompts, or completions to the client.
type readerAIStatusError struct {
	carrier *httperr.Error
	cause   error
}

func (e *readerAIStatusError) Error() string {
	if e == nil || e.carrier == nil {
		return ""
	}
	return e.carrier.Error()
}

func (e *readerAIStatusError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *readerAIStatusError) HTTPStatus() int {
	if e == nil || e.carrier == nil {
		return http.StatusInternalServerError
	}
	return e.carrier.HTTPStatus()
}

func (e *readerAIStatusError) HTTPMessage() string {
	if e == nil || e.carrier == nil {
		return "AI provider request failed"
	}
	return e.carrier.HTTPMessage()
}

func (e *readerAIStatusError) HTTPErrorCode() string {
	if e == nil || e.carrier == nil {
		return "ai_provider_error"
	}
	return e.carrier.HTTPErrorCode()
}

// mapReaderAIError converts provider and request-lifecycle failures into a
// stable client classification without copying the upstream error text. An
// existing HTTP carrier, such as rate_limit_exceeded or reader_not_found, remains
// authoritative and is not reclassified.
func mapReaderAIError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := httperr.As(err); ok {
		return err
	}

	status := http.StatusBadGateway
	code := "ai_provider_error"
	message := "AI provider request failed"
	switch {
	case errors.Is(err, context.Canceled):
		status = readerAIClientClosedRequest
		code = "ai_request_canceled"
		message = "AI request was canceled"
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
		code = "ai_timeout"
		message = "AI provider request timed out"
	case errors.Is(err, errReaderAIEmptyResponse):
		code = "ai_empty_response"
		message = "AI provider returned no answer"
	}

	return &readerAIStatusError{
		carrier: httperr.NewWithCode(status, code, message),
		cause:   err,
	}
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
