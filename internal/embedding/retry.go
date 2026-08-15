package embedding

import (
	"errors"
	"time"
)

// maxRetryDelay caps exponential backoff between attempts so a hostile or
// misconfigured upstream sending Retry-After: 3600 cannot park a worker for
// an hour. 30s matches the analyzer's cap and the upper bound of OpenAI's
// published Retry-After guidance.
const maxRetryDelay = 30 * time.Second

// callError carries the retryability classification produced by the HTTP
// layer so the orchestration loop in Embed can decide whether to back off +
// retry, fail immediately, or honour an upstream Retry-After.
type callError struct {
	message    string
	retryable  bool
	retryAfter string
	cause      error
}

func (e *callError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.message
}

func (e *callError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// isRetryableError decides whether a single embedding attempt should
// trigger a retry. Transport-layer errors carry their own retryable bit on
// *callError; everything else (e.g. validation failures) is non-retryable.
func isRetryableError(err error) bool {
	var ce *callError
	if errors.As(err, &ce) {
		return ce.retryable
	}
	return false
}
