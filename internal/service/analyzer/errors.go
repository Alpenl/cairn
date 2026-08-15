package analyzer

import "errors"

// ErrAnalyzerInvalidJSON marks responses where no valid JSON object
// could be extracted from the model output. Wrappers should use
// fmt.Errorf("...: %w", ErrAnalyzerInvalidJSON) so the retry loop's
// errors.Is check still triggers.
var ErrAnalyzerInvalidJSON = errors.New("analyzer response missing valid JSON")

// ErrAnalyzerEmptyResponse marks calls where the upstream returned an
// empty body or no choices. Both conditions are transient and the retry
// loop should re-issue the request.
var ErrAnalyzerEmptyResponse = errors.New("analyzer returned empty response")
