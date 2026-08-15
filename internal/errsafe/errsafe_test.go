package errsafe_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"webtag/internal/errsafe"
)

// TestClassifyError_Sentinels confirms each exported sentinel resolves to
// the expected category label when wrapped via fmt.Errorf("...: %w", ...).
func TestClassifyError_Sentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sentinel error
		want     string
	}{
		{"unsafe target", errsafe.ErrUnsafeTarget, "unsafe_target"},
		{"content type", errsafe.ErrContentType, "content_type"},
		{"response too large", errsafe.ErrResponseTooLarge, "response_too_large"},
		{"timeout", errsafe.ErrTimeout, "timeout"},
		{"network", errsafe.ErrNetwork, "network"},
		{"upstream http", errsafe.ErrUpstreamHTTP, "upstream_http"},
		{"parse", errsafe.ErrParse, "parse"},
		{"parse job missing", errsafe.ErrParseJobMissing, "parse_job_missing"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wrapped := fmt.Errorf("call site context: %w", tt.sentinel)
			if got := errsafe.ClassifyError(wrapped); got != tt.want {
				t.Fatalf("ClassifyError(%q) = %q, want %q", wrapped.Error(), got, tt.want)
			}

			// Sanity check: errors.Is on the wrapped form must still work
			// against the sentinel. This is what callers will rely on.
			if !errors.Is(wrapped, tt.sentinel) {
				t.Fatalf("errors.Is(wrapped, sentinel) = false, want true")
			}
		})
	}
}

// TestClassifyError_StringFallback confirms that errors carrying no
// sentinel still fall through to the substring matcher so legacy /
// external errors keep producing the right category.
func TestClassifyError_StringFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "unsafe target via substring",
			err:  errors.New("Get \"http://10.0.0.1\": unsafe target host: 10.0.0.1"),
			want: "unsafe_target",
		},
		{
			name: "content type via substring",
			err:  errors.New("analyzer response content-type is not JSON: text/html"),
			want: "content_type",
		},
		{
			name: "response too large via substring",
			err:  errors.New("fetch https://example.com/file.pdf: PDF exceeds size limit"),
			want: "response_too_large",
		},
		{
			name: "timeout via substring",
			err:  errors.New("context deadline exceeded"),
			want: "timeout",
		},
		{
			name: "network via substring",
			err:  errors.New("dial tcp 127.0.0.1:80: connection refused"),
			want: "network",
		},
		{
			name: "upstream http via substring",
			err:  errors.New("analyzer call failed: status=502 body=stale"),
			want: "upstream_http",
		},
		{
			name: "parse via substring",
			err:  errors.New("invalid character 'x' looking for beginning of value"),
			want: "parse",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := errsafe.ClassifyError(tt.err); got != tt.want {
				t.Fatalf("ClassifyError(%q) = %q, want %q", tt.err.Error(), got, tt.want)
			}
		})
	}
}

// TestClassifyError_Unknown confirms an error that matches neither a
// sentinel nor any substring returns "unknown" rather than misclassifying.
func TestClassifyError_Unknown(t *testing.T) {
	t.Parallel()

	if got := errsafe.ClassifyError(errors.New("unexpected processing failure")); got != "unknown" {
		t.Fatalf("ClassifyError(unknown) = %q, want unknown", got)
	}
}

// TestClassifyError_Nil confirms a nil error collapses to "none" so
// callers can pass an unconditional reference without a guard.
func TestClassifyError_Nil(t *testing.T) {
	t.Parallel()

	if got := errsafe.ClassifyError(nil); got != "none" {
		t.Fatalf("ClassifyError(nil) = %q, want none", got)
	}
}

// TestSafeMessage_PrefersSentinel verifies that SafeMessage routes the
// category through ClassifyError so a sentinel-wrapped error gets the
// correct prefix even when the surface message would not otherwise
// trigger the matching substring rule.
func TestSafeMessage_PrefersSentinel(t *testing.T) {
	t.Parallel()

	// The surface message intentionally does NOT contain "too large" or
	// "exceeds size limit". Without sentinel routing, the legacy
	// substring path would label this "unknown".
	wrapped := fmt.Errorf("upstream signalled cap breach: %w", errsafe.ErrResponseTooLarge)

	got := errsafe.SafeMessage(wrapped)
	wantPrefix := "response_too_large: "
	if len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("SafeMessage = %q, want prefix %q", got, wantPrefix)
	}
}

func TestSafeMessage_RedactsSecretsBeforePersistence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		secret string
		err    error
	}{
		{
			name:   "url query",
			secret: "token=abc123",
			err:    errors.New("fetch https://example.com/article?token=abc123 failed"),
		},
		{
			name:   "bearer token",
			secret: "eyJhbGciOiJIUzI1NiJ9.payload.signature",
			err:    errors.New("upstream echoed Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature"),
		},
		{
			name:   "postgres credentials",
			secret: "webtag:secret-password",
			err:    errors.New("dial postgres://webtag:secret-password@db:5432/webtag"),
		},
		{
			name:   "openai key",
			secret: "sk-abcdef0123456789ABCDEF",
			err:    errors.New("upstream rejected sk-abcdef0123456789ABCDEF"),
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := errsafe.SafeMessage(tt.err)
			if strings.Contains(got, tt.secret) {
				t.Fatalf("SafeMessage leaked %q: %q", tt.secret, got)
			}
			if !strings.Contains(got, "<redacted>") {
				t.Fatalf("SafeMessage = %q, want redaction marker", got)
			}
		})
	}
}

func TestRedactSecrets_CoversCredentialFormsOutsideQueryStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		secrets []string
	}{
		{
			name:    "http userinfo and fragment",
			raw:     "fetch https://alice:swordfish@example.com/private#access_token=fragment-secret failed",
			secrets: []string{"alice", "swordfish", "fragment-secret"},
		},
		{
			name:    "all cookie pairs",
			raw:     "upstream echoed Cookie: session=first-secret; refresh=second-secret",
			secrets: []string{"first-secret", "second-secret"},
		},
		{
			name:    "authorization header",
			raw:     "request failed Authorization: ApiKey opaque-secret-value",
			secrets: []string{"opaque-secret-value"},
		},
		{
			name:    "postgres key value dsn",
			raw:     "dial host=db.internal user=webtag password='db-secret-value' dbname=webtag",
			secrets: []string{"db-secret-value"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := errsafe.RedactSecrets(tt.raw)
			for _, secret := range tt.secrets {
				if strings.Contains(got, secret) {
					t.Fatalf("RedactSecrets leaked %q: %q", secret, got)
				}
			}
		})
	}
}

// TestClassifyError_EOFTokenBoundary pins the L3 fix that replaced the
// loose strings.Contains(normalized, "eof") match with a word-boundary
// helper. The earlier match would mis-label any message containing the
// literal substring (e.g. "preformatted", "geofencing") as a network
// error and pollute the error_category metric labels.
func TestClassifyError_EOFTokenBoundary(t *testing.T) {
	t.Parallel()

	networkCases := []string{
		"EOF",
		"unexpected EOF",
		"read tcp 10.0.0.1:443->1.1.1.1:443: EOF",
		"http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug=\"\": EOF",
	}
	for _, msg := range networkCases {
		msg := msg
		t.Run("network/"+msg, func(t *testing.T) {
			t.Parallel()
			if got := errsafe.ClassifyError(errors.New(msg)); got != "network" {
				t.Fatalf("ClassifyError(%q) = %q, want network", msg, got)
			}
		})
	}

	notNetworkCases := []string{
		"preformatted markdown rejected by parser",
		"geofencing rule blocked request",
		"leofield: schema mismatch",
	}
	for _, msg := range notNetworkCases {
		msg := msg
		t.Run("not-network/"+msg, func(t *testing.T) {
			t.Parallel()
			if got := errsafe.ClassifyError(errors.New(msg)); got == "network" {
				t.Fatalf("ClassifyError(%q) = network, want anything but network", msg)
			}
		})
	}
}

// TestNormalizedPointer covers the exported helper now shared with
// service.diagnostics so any future regression there is caught here.
func TestNormalizedPointer(t *testing.T) {
	t.Parallel()

	if got := errsafe.NormalizedPointer(nil); got != "" {
		t.Fatalf("NormalizedPointer(nil) = %q, want \"\"", got)
	}

	value := "  HELLO World  "
	if got := errsafe.NormalizedPointer(&value); got != "hello world" {
		t.Fatalf("NormalizedPointer = %q, want %q", got, "hello world")
	}
}

// typedClassifyErr is a fixture type that implements error so we can
// exercise the errors.As path in ClassifyError. It wraps a sentinel via
// the Unwrap method, mirroring the *FetchError / *analyzerCallError
// pattern in fetcher/ and service/analyzer where the typed error carries
// the upstream sentinel as its cause field.
type typedClassifyErr struct {
	msg   string
	cause error
}

func (e *typedClassifyErr) Error() string { return e.msg }
func (e *typedClassifyErr) Unwrap() error { return e.cause }

// TestClassifyError_TypedErrorAs confirms errors.As / errors.Is via the
// wrapped chain on a custom typed error resolves to the sentinel
// category. The surface message intentionally avoids any substring the
// fallback matcher would catch, so the only way this returns the right
// label is the Is/As walk in ClassifyError.
func TestClassifyError_TypedErrorAs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cause   error
		surface string
		want    string
	}{
		{"timeout typed", errsafe.ErrTimeout, "operation aborted", "timeout"},
		{"network typed", errsafe.ErrNetwork, "transport blew up", "network"},
		{"upstream typed", errsafe.ErrUpstreamHTTP, "vendor said no", "upstream_http"},
		{"content type typed", errsafe.ErrContentType, "wrong mime", "content_type"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := &typedClassifyErr{msg: tt.surface, cause: tt.cause}
			if got := errsafe.ClassifyError(err); got != tt.want {
				t.Fatalf("ClassifyError(%q) = %q, want %q (sentinel route should win over substring fallback)", err.Error(), got, tt.want)
			}
			if !errors.Is(err, tt.cause) {
				t.Fatalf("errors.Is(typedErr, sentinel) = false, want true")
			}
		})
	}
}

// TestClassifyPersisted covers callers that hold a stored error_msg string
// with no error chain attached.
func TestClassifyPersisted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  string
		want string
	}{
		{"empty collapses to none", "", "none"},
		{"whitespace collapses to none", "   \t\n  ", "none"},
		{"persisted timeout", "context deadline exceeded", "timeout"},
		{"persisted unsafe target", "fetch http://10.0.0.1: unsafe target host: 10.0.0.1", "unsafe_target"},
		{"persisted response too large", "PDF exceeds size limit", "response_too_large"},
		{"persisted upstream http", "analyzer call failed: status=502 body=stale", "upstream_http"},
		{"persisted network", "dial tcp 127.0.0.1:80: connection refused", "network"},
		{"persisted parse", "invalid character 'x' looking for beginning of value", "parse"},
		{"persisted content type", "analyzer response content-type is not JSON: text/html", "content_type"},
		{"persisted unknown", "some bookkeeping went wrong", "unknown"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := errsafe.ClassifyPersisted(tt.msg); got != tt.want {
				t.Fatalf("ClassifyPersisted(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

// TestClassifyPersisted_HistoricalMessages exercises the substring rules
// against the literal strings the production DB likely contains today.
// If the classification engine ever drifts (e.g. a rule gets rewritten),
// this test fails loudly so we don't silently re-categorise historical
// error_msg values into a new bucket and break dashboards.
func TestClassifyPersisted_HistoricalMessages(t *testing.T) {
	t.Parallel()

	// Pairs of (legacy error_msg, expected category) — derived from the
	// fetcher / analyzer error templates that have shipped to production.
	cases := map[string]string{
		"fetch https://example.com/file.pdf: PDF exceeds size limit":        "response_too_large",
		"fetch https://example.com/file.pdf: response too large":            "response_too_large",
		"fetch https://example.com: response content-type is not supported": "content_type",
		"fetch https://api.github.com/repos/x/y: GitHub API HTTP 503":       "upstream_http",
		"fetch https://r.jina.ai/...: Jina HTTP 502":                        "upstream_http",
		"search HTTP 500":                          "upstream_http",
		"analyzer call failed: status=429 len=120": "upstream_http",
		"analyzer response content-type is not JSON: text/html; charset=utf-8":      "content_type",
		"analyzer response too large":                                               "response_too_large",
		"context deadline exceeded":                                                 "timeout",
		"Post \"https://example.com\": net/http: request canceled (Client.Timeout)": "timeout",
		"dial tcp 127.0.0.1:443: connect: connection refused":                       "network",
		"read tcp 10.0.0.1->1.1.1.1:443: EOF":                                       "network",
		"Get \"http://10.0.0.1\": unsafe target host: 10.0.0.1":                     "unsafe_target",
	}

	for msg, want := range cases {
		msg, want := msg, want
		t.Run(want+"/"+msg, func(t *testing.T) {
			t.Parallel()
			if got := errsafe.ClassifyPersisted(msg); got != want {
				t.Fatalf("ClassifyPersisted(%q) = %q, want %q (historical DB message must keep classifying as before)", msg, got, want)
			}
		})
	}
}
