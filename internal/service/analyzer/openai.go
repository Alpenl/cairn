package analyzer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"webtag/internal/errsafe"
	"webtag/internal/fetcher"
	"webtag/internal/jsonx"
)

// analyzerMaxRetryDelay caps exponential backoff so an aggressive
// EmptyResponseRetries config can't push a single attempt past ~30s,
// which is roughly the upper bound of OpenAI's published Retry-After
// guidance.
const analyzerMaxRetryDelay = 30 * time.Second

// analyzerCallError carries the retryability classification produced by the
// HTTP layer so the orchestration loop in Analyze can decide whether to
// back off + retry, fail immediately, or honour an upstream Retry-After.
// statusCode is 0 for errors raised before a response was read (transport
// failure, oversized body); it carries the upstream HTTP status otherwise so
// callers can distinguish a gateway rejecting our request shape (4xx) from a
// rate limit (429) or an upstream outage (5xx) without parsing message text.
type analyzerCallError struct {
	message    string
	retryable  bool
	retryAfter string
	statusCode int
	// schemaRejected is set when a 4xx body names response_format or
	// json_schema. It is the one bit extracted from the upstream body, which
	// is otherwise deliberately never carried into the message (it can reach
	// DB error_msg fields and API clients). Without it a 400 for an unrelated
	// cause — context_length_exceeded is the common one — would be
	// indistinguishable from "this gateway has no structured outputs", and
	// the demotion latch is process-wide and one-way.
	schemaRejected bool
	cause          error
}

func (e *analyzerCallError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.message
}

func (e *analyzerCallError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type analyzerChatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content jsonx.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// call performs a single OpenAI-compatible chat-completion request.
// All transport-level errors are wrapped in *analyzerCallError so the
// caller can inspect the retryable bit without string matching.
//
//nolint:gocyclo // reason: 单次 HTTP 请求 + 状态码分类 + JSON 解码的线性流程；每个分支对应一种错误形态（4xx / 429 / 5xx / body 解析失败），拆函数会把 resp/body 在多 helper 间反复传递。
func (a *OpenAIAnalyzer) call(ctx context.Context, payload map[string]any) (string, error) {
	body, err := jsonx.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal analyzer request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, a.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build analyzer request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		err = fetcher.SanitizeSecurityError(err)
		retryable := true
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			retryable = false
		case fetcher.IsUnsafeTargetError(err):
			retryable = false
		}
		return "", &analyzerCallError{
			message:   fmt.Sprintf("analyzer call failed: %v", err),
			retryable: retryable,
			// Preserve cancellation and timeout through the analyzer wrapper. The
			// caller uses errors.Is to distinguish a request that the user/server
			// cancelled from a provider failure; the surface message intentionally
			// remains free of the upstream response body.
			cause: err,
		}
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, a.maxResponseBytes+1))
	if err != nil {
		return "", &analyzerCallError{
			message:   fmt.Sprintf("read analyzer response: %v", err),
			retryable: true,
		}
	}
	if a.maxResponseBytes > 0 && int64(len(rawBody)) > a.maxResponseBytes {
		// cause: errsafe.ErrResponseTooLarge so errsafe.ClassifyError
		// resolves the category via errors.Is rather than substring-
		// matching "too large" in the surface message.
		return "", &analyzerCallError{
			message:   "analyzer response too large",
			retryable: false,
			cause:     errsafe.ErrResponseTooLarge,
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Intentionally avoid embedding the upstream response body in the
		// error message; the body may surface in DB error_msg fields and we
		// don't want vendor responses leaking to API clients.
		// cause: errsafe.ErrUpstreamHTTP lets errsafe.ClassifyError resolve
		// the "upstream_http" category through errors.Is.
		return "", &analyzerCallError{
			message:    fmt.Sprintf("analyzer call failed: status=%d len=%d", resp.StatusCode, len(rawBody)),
			retryable:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError,
			retryAfter: resp.Header.Get("Retry-After"),
			statusCode: resp.StatusCode,
			// Only 400/422 can demote, so only they pay the parse. Skipping it
			// keeps a 429/5xx storm from decoding one body per failed call.
			schemaRejected: isSchemaRejectionStatus(resp.StatusCode) && mentionsResponseSchema(rawBody),
			cause:          errsafe.ErrUpstreamHTTP,
		}
	}
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if contentType != "" && !strings.Contains(contentType, "application/json") {
		// cause: errsafe.ErrContentType — ditto, errors.Is wins over
		// substring "content-type" matching on the surface message.
		return "", &analyzerCallError{
			message:   fmt.Sprintf("analyzer response content-type is not JSON: %s", contentType),
			retryable: false,
			cause:     errsafe.ErrContentType,
		}
	}

	var decoded analyzerChatCompletionResponse
	if err := jsonx.Unmarshal(rawBody, &decoded); err != nil {
		return "", &analyzerCallError{
			message:   fmt.Sprintf("decode analyzer response: %v", err),
			retryable: true,
		}
	}
	if len(decoded.Choices) == 0 {
		return "", &analyzerCallError{
			message:   "analyzer call failed: empty choices",
			retryable: true,
			cause:     ErrAnalyzerEmptyResponse,
		}
	}

	content, err := decodeAnalyzerMessageContent(decoded.Choices[0].Message.Content)
	if err != nil {
		return "", &analyzerCallError{
			message:   fmt.Sprintf("decode analyzer message content: %v", err),
			retryable: true,
		}
	}

	return content, nil
}

// mentionsResponseSchema reports whether an upstream error blames the
// structured-output request fields. OpenAI-compatible gateways word the
// message freely but do name the offending field ("Unrecognized request
// argument supplied: response_format", "Invalid schema for response_format").
//
// It reads only error.param and error.message, NOT the raw body. Matching the
// whole body looks equivalent and is not: some gateways (one-api, new-api,
// LiteLLM in debug mode) echo the original request inside the error envelope,
// and OUR request contains both field names — so a plain body scan reports
// "schema rejected" for every 4xx those gateways ever produce, including
// context_length_exceeded. The demotion latch is process-wide and one-way, so
// that false positive costs the whole process its structured output.
//
// This is still best-effort: a gateway that echoes the request INTO
// error.message would fool it. demoteStructuredOutput therefore does not rely
// on this signal alone.
//
// The body is inspected here and immediately reduced to a bool; nothing from
// it is retained, logged, or attached to the error (it can reach DB
// error_msg fields and API clients).
func mentionsResponseSchema(body []byte) bool {
	var envelope struct {
		Error struct {
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := jsonx.Unmarshal(body, &envelope); err != nil {
		return false
	}
	return namesResponseSchemaField(envelope.Error.Param) ||
		namesResponseSchemaField(envelope.Error.Message)
}

func namesResponseSchemaField(s string) bool {
	lowered := strings.ToLower(s)
	return strings.Contains(lowered, "response_format") || strings.Contains(lowered, "json_schema")
}

// isRetryableAnalyzerError decides whether a single analyzer attempt
// should trigger a retry. Transport-layer errors carry their own
// retryable bit on *analyzerCallError; non-call errors are matched
// against the sentinel set so the loop never depends on error message
// text.
func isRetryableAnalyzerError(err error) bool {
	var callErr *analyzerCallError
	if errors.As(err, &callErr) {
		return callErr.retryable
	}

	return errors.Is(err, ErrAnalyzerInvalidJSON) || errors.Is(err, ErrAnalyzerEmptyResponse)
}

func analyzerRetryAfter(err error) string {
	var callErr *analyzerCallError
	if errors.As(err, &callErr) {
		return callErr.retryAfter
	}
	return ""
}
