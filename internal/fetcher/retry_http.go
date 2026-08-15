package fetcher

import (
	"net/http"
	"time"
)

// fetcherMaxRetryDelay caps any single retry sleep so a misbehaving
// upstream that returns Retry-After: 3600 cannot park a worker for
// the full hour. Mirrors the analyzer's analyzerMaxRetryDelay so
// both retry loops have the same worst case.
const fetcherMaxRetryDelay = 30 * time.Second

const defaultRetryAttempts = 2
const defaultRetryDelay = 25 * time.Millisecond

func isRetryableRequest(req *http.Request) bool {
	return req != nil && req.Method == http.MethodGet
}

func isRetryableResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
}
