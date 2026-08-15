// Package embedding encapsulates the OpenAI-compatible /embeddings client
// powering v3 retrieval-style tagging. It mirrors internal/service/analyzer
// in shape — same SSRF-hardened transport (fetcher.HardenedHTTPClient, gated
// by AI_ALLOW_UNSAFE_TARGETS), same exponential-backoff-with-jitter retry
// policy, same 1 MiB response-body cap — but targets POST /embeddings and
// returns dense float32 vectors instead of parsed chat completions.
//
// The single public method, Embed, takes a batch of input strings and
// returns one vector per input, in input order. The upstream contract is:
//
//	request : {"model": "...", "input": ["a", "b", ...]}
//	response: {"data": [{"index": 0, "embedding": [...]}, ...]}
//
// Responses are re-ordered by their `index` field before returning so the
// caller can rely on positional alignment regardless of upstream ordering.
package embedding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"webtag/internal/errsafe"
	"webtag/internal/fetcher"
	"webtag/internal/jsonx"
	"webtag/internal/retry"
)

const (
	defaultRequestTimeout   = 30 * time.Second
	defaultRetryAttempts    = 3
	defaultRetryDelay       = 25 * time.Millisecond
	defaultMaxResponseBytes = 1 << 20
)

// Sentinel errors so callers (and tests) can branch on the failure shape
// via errors.Is rather than substring-matching the surface message.
var (
	// ErrDisabled is returned by Embed when no model is configured —
	// EMBEDDING_MODEL empty means the retrieval path is turned off and the
	// caller should fall back to free-generation tagging.
	ErrDisabled = errors.New("embedding: client disabled (no model configured)")
	// ErrEmptyInput is returned when Embed is called with no inputs.
	ErrEmptyInput = errors.New("embedding: empty input batch")
	// ErrEmptyVector is returned when the upstream returns a zero-length
	// embedding for an input.
	ErrEmptyVector = errors.New("embedding: upstream returned empty vector")
	// ErrDimensionMismatch is returned when an upstream vector's length does
	// not match the configured EMBEDDING_DIMENSIONS.
	ErrDimensionMismatch = errors.New("embedding: vector dimension mismatch")
	// ErrCountMismatch is returned when the upstream returns a different
	// number of vectors than inputs sent.
	ErrCountMismatch = errors.New("embedding: response vector count mismatch")
)

// Options configures the Client. The wiring group (Model / BaseURL / APIKey
// / HTTPClient / RequestTimeout / RetryAttempts / RetryDelay / Dimensions)
// is what app.deps injects from config. MaxResponseBytes stays overridable
// so unit tests can pin a small cap to exercise the "response too large"
// branch without monkey-patching package vars.
type Options struct {
	Model            string
	BaseURL          string
	APIKey           string
	Dimensions       int
	HTTPClient       *http.Client
	RequestTimeout   time.Duration
	RetryAttempts    int
	RetryDelay       time.Duration
	MaxResponseBytes int64
}

// Client is the OpenAI-compatible embeddings client.
type Client struct {
	model            string
	baseURL          string
	apiKey           string
	dimensions       int
	client           *http.Client
	requestTimeout   time.Duration
	retryAttempts    int
	retryDelay       time.Duration
	maxResponseBytes int64
}

// NewClient builds a Client from opts; unset fields fall back to package
// defaults and the HTTPClient defaults to fetcher's SSRF-hardened client.
func NewClient(opts Options) *Client {
	client := opts.HTTPClient
	if client == nil {
		client = fetcher.NewHardenedHTTPClient()
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultRequestTimeout
	}
	if opts.RetryAttempts <= 0 {
		opts.RetryAttempts = defaultRetryAttempts
	}
	if opts.RetryDelay <= 0 {
		opts.RetryDelay = defaultRetryDelay
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = defaultMaxResponseBytes
	}
	return &Client{
		model:            strings.TrimSpace(opts.Model),
		baseURL:          strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		apiKey:           strings.TrimSpace(opts.APIKey),
		dimensions:       opts.Dimensions,
		client:           client,
		requestTimeout:   opts.RequestTimeout,
		retryAttempts:    opts.RetryAttempts,
		retryDelay:       opts.RetryDelay,
		maxResponseBytes: opts.MaxResponseBytes,
	}
}

// Enabled reports whether a model is configured. When false, Embed returns
// ErrDisabled without making any network call.
func (c *Client) Enabled() bool {
	return c != nil && c.model != ""
}

// Dimensions returns the configured embedding width (0 when unset).
func (c *Client) Dimensions() int {
	if c == nil {
		return 0
	}
	return c.dimensions
}

// Model returns the configured embedding model name (empty when disabled).
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}

// embeddingResponse is the subset of the OpenAI /embeddings response we
// consume. Index is used to re-sort vectors into input order.
type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one vector per input, in input order. Retries on transient
// upstream failures (429 / 5xx / network) with exponential backoff + jitter;
// fails fast on context cancellation, SSRF rejection, and 4xx. An empty
// vector, a dimension mismatch, or a vector-count mismatch is a hard error.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if len(inputs) == 0 {
		return nil, ErrEmptyInput
	}

	payload := map[string]any{
		"model": c.model,
		"input": inputs,
	}

	policy := retry.NewPolicy(c.retryDelay, maxRetryDelay)
	var lastErr error
	for attempt := 0; attempt < c.retryAttempts; attempt++ {
		vectors, err := c.call(ctx, payload, len(inputs))
		if err != nil {
			lastErr = err
			if !isRetryableError(err) {
				return nil, err
			}
			if attempt < c.retryAttempts-1 {
				var retryAfter string
				var callErr *callError
				if errors.As(err, &callErr) {
					retryAfter = callErr.retryAfter
				}
				if waitErr := policy.Wait(ctx, policy.Delay(attempt, retryAfter)); waitErr != nil {
					return nil, fmt.Errorf("embedding retry interrupted: %w", waitErr)
				}
			}
			continue
		}
		return vectors, nil
	}

	return nil, fmt.Errorf("embedding failed after %d retries: %w", c.retryAttempts, lastErr)
}

// call performs a single POST /embeddings request and decodes the response
// into input-ordered vectors. Transport-level failures are wrapped in
// *callError carrying the retryable classification.
//
//nolint:gocyclo // reason: 单次 HTTP 请求 + 状态码分类 + JSON 解码 + 向量校验的线性流程；每个分支对应一种错误形态，拆函数会把 resp/body 在多 helper 间反复传递。
func (c *Client) call(ctx context.Context, payload map[string]any, wantCount int) ([][]float32, error) {
	body, err := jsonx.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		err = fetcher.SanitizeSecurityError(err)
		retryable := true
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			retryable = false
		case fetcher.IsUnsafeTargetError(err):
			retryable = false
		}
		// Preserve the transport error as the cause so callers can resolve
		// SSRF / timeout / network categories via errors.Is through the
		// *callError chain (e.g. fetcher.IsUnsafeTargetError).
		return nil, &callError{
			message:   fmt.Sprintf("embedding call failed: %v", err),
			retryable: retryable,
			cause:     err,
		}
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes+1))
	if err != nil {
		return nil, &callError{message: fmt.Sprintf("read embedding response: %v", err), retryable: true}
	}
	if c.maxResponseBytes > 0 && int64(len(rawBody)) > c.maxResponseBytes {
		return nil, &callError{
			message:   "embedding response too large",
			retryable: false,
			cause:     errsafe.ErrResponseTooLarge,
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Intentionally avoid embedding the upstream body in the message;
		// it may surface in DB error_msg / logs and we don't want vendor
		// responses leaking. errsafe.ErrUpstreamHTTP lets ClassifyError
		// resolve the category through errors.Is.
		return nil, &callError{
			message:    fmt.Sprintf("embedding call failed: status=%d len=%d", resp.StatusCode, len(rawBody)),
			retryable:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError,
			retryAfter: resp.Header.Get("Retry-After"),
			cause:      errsafe.ErrUpstreamHTTP,
		}
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if contentType != "" && !strings.Contains(contentType, "application/json") {
		return nil, &callError{
			message:   fmt.Sprintf("embedding response content-type is not JSON: %s", contentType),
			retryable: false,
			cause:     errsafe.ErrContentType,
		}
	}

	var decoded embeddingResponse
	if err := jsonx.Unmarshal(rawBody, &decoded); err != nil {
		return nil, &callError{message: fmt.Sprintf("decode embedding response: %v", err), retryable: true}
	}

	vectors, err := c.assembleVectors(decoded, wantCount)
	if err != nil {
		return nil, err
	}
	return vectors, nil
}

// assembleVectors re-orders the decoded data entries into input order and
// validates count, emptiness, and dimensions. These validation failures are
// non-retryable: a malformed-but-well-formed response will not improve on a
// retry, and retrying wastes upstream quota.
func (c *Client) assembleVectors(decoded embeddingResponse, wantCount int) ([][]float32, error) {
	if len(decoded.Data) != wantCount {
		return nil, fmt.Errorf("%w: got %d vectors for %d inputs", ErrCountMismatch, len(decoded.Data), wantCount)
	}

	// Sort by index so positional alignment with inputs holds regardless of
	// the order the upstream emits data entries.
	sorted := make([]struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}, len(decoded.Data))
	copy(sorted, decoded.Data)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })

	out := make([][]float32, wantCount)
	for i := range sorted {
		// After sorting, the i-th entry must carry index i; a gap or
		// duplicate index means the response cannot be aligned to inputs.
		if sorted[i].Index != i {
			return nil, fmt.Errorf("%w: non-contiguous index %d at position %d", ErrCountMismatch, sorted[i].Index, i)
		}
		vec := sorted[i].Embedding
		if len(vec) == 0 {
			return nil, fmt.Errorf("%w: input index %d", ErrEmptyVector, i)
		}
		if c.dimensions > 0 && len(vec) != c.dimensions {
			return nil, fmt.Errorf("%w: input index %d got %d, want %d", ErrDimensionMismatch, i, len(vec), c.dimensions)
		}
		out[i] = vec
	}
	return out, nil
}
