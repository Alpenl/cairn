package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxHealthBytes bounds a /health or /ready body. They are four-field objects.
const maxHealthBytes int64 = 64 << 10

// HealthReport is the subset of webtag's /health document the helper reads.
//
// The endpoint merges extra fields from middleware, so it is treated as an
// open-ended object and only these four keys are consulted. commit is the one
// that matters: it is the only evidence the helper has that the process now
// answering is the one it just installed, rather than the old one that never
// actually stopped.
type HealthReport struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// HealthClient probes the running application.
type HealthClient struct {
	base   string
	client *http.Client
}

// NewHealthClient builds a probe against a loopback base URL.
func NewHealthClient(base string) *HealthClient {
	return &HealthClient{
		base:   base,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Health reads the identity of whatever is currently listening.
func (probe *HealthClient) Health(ctx context.Context) (HealthReport, error) {
	body, status, err := probe.get(ctx, "/health")
	if err != nil {
		return HealthReport{}, err
	}
	if status != http.StatusOK {
		return HealthReport{}, fmt.Errorf("/health answered %d", status)
	}
	var report HealthReport
	if err := json.Unmarshal(body, &report); err != nil {
		return HealthReport{}, fmt.Errorf("parse /health response: %w", err)
	}
	return report, nil
}

// Ready reports whether the application declares itself ready to serve.
func (probe *HealthClient) Ready(ctx context.Context) error {
	body, status, err := probe.get(ctx, "/ready")
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	var payload struct {
		Failed []string `json:"failed"`
	}
	_ = json.Unmarshal(body, &payload)
	if len(payload.Failed) > 0 {
		return fmt.Errorf("/ready answered %d with failing checks %v", status, payload.Failed)
	}
	return fmt.Errorf("/ready answered %d", status)
}

func (probe *HealthClient) get(ctx context.Context, path string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.base+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request for %s: %w", path, err)
	}
	response, err := probe.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("probe %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHealthBytes))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read %s response: %w", path, err)
	}
	return body, response.StatusCode, nil
}

// ErrIdentityMismatch is the specific failure that means the process answering
// is not the release that was just installed.
var ErrIdentityMismatch = errors.New("the running commit is not the target commit")

// AwaitTarget polls /health until the target commit answers, then requires
// /ready to succeed.
//
// The commit comparison is exact. A release build stamps the full 40-character
// revision into both the binary and the signed manifest, so a prefix match
// would only ever paper over a build that was not made by the release pipeline
// — which is the case where being lenient is most expensive.
func (probe *HealthClient) AwaitTarget(ctx context.Context, targetCommit string, interval, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		report, err := probe.Health(ctx)
		switch {
		case err != nil:
			lastErr = err
		case report.Commit == targetCommit:
			if err := probe.Ready(ctx); err != nil {
				lastErr = err
				break
			}
			return nil
		default:
			lastErr = fmt.Errorf("%w: /health reports commit %q, the target is %q",
				ErrIdentityMismatch, report.Commit, targetCommit)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the target release did not become ready within %s: %w", budget, lastErr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the target release was cancelled: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}
