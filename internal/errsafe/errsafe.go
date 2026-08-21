// Package errsafe centralises the "make this error safe to persist or
// surface" helpers shared by the service and worker layers. Keeping the
// classification + sanitisation here lets the worker queue avoid pulling
// in the much larger service package solely for SafeMessage, and
// gives both packages a common sentinel for "the inner workload already
// recorded its failure, do not re-persist".
//
// # Two classification paths
//
// There are two routes into the category labels, with different inputs
// and very different precision:
//
//  1. Live errors → use ClassifyError(err error). It first walks the
//     wrapped chain with errors.Is against the package sentinels
//     (ErrTimeout, ErrNetwork, …) so callers that wrap their failures
//     with %w get a deterministic category regardless of message text.
//     If no sentinel matches it falls back to the substring matcher on
//     err.Error() so stdlib / third-party errors (context.DeadlineExceeded,
//     net.OpError, pgx wrappers) still resolve to a useful label.
//
//  2. Persisted strings → use ClassifyPersisted(msg string). The input
//     is the raw error_msg text read back out of the DB or an upstream
//     status payload — the original error type information is gone, so
//     substring matching is the only signal available. Callers should
//     route through this name to make the "no error chain present"
//     intent obvious at the call site.
//
// Both routes converge on the same set of category labels, so dashboards
// and metric labels stay stable. The substring fallback is intentionally
// kept as a safety net rather than the primary classifier — wrap one of
// the Err* sentinels at the error site (with fmt.Errorf("...: %w", ...))
// to opt the call site into the deterministic path.
package errsafe

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrAlreadyPersisted signals that a Processor implementation already
// wrote its failure state to durable storage. Wrappers should expose it
// via errors.Is so the queue (or any other supervisor) can suppress its
// own redundant persist step without needing to import the underlying
// pipeline package.
var ErrAlreadyPersisted = errors.New("processor: failure already persisted")

// Classification sentinels. Wrap these via fmt.Errorf("...: %w", ErrXxx)
// at the error site so ClassifyError can recover the category through
// errors.Is without falling back to substring matching. Each sentinel
// pairs 1:1 with one of the Classify* category labels:
//
//	ErrUnsafeTarget     → "unsafe_target"
//	ErrContentType      → "content_type"
//	ErrResponseTooLarge → "response_too_large"
//	ErrTimeout          → "timeout"
//	ErrNetwork          → "network"
//	ErrUpstreamHTTP     → "upstream_http"
//	ErrParse            → "parse"
//	ErrBlockedByOrigin  → "blocked_by_origin"
//
// New sentinels are safe to add; renaming an existing one is a breaking
// change for callers that match against it via errors.Is.
var (
	ErrUnsafeTarget     = errors.New("unsafe target")
	ErrContentType      = errors.New("unexpected content type")
	ErrResponseTooLarge = errors.New("response too large")
	ErrTimeout          = errors.New("timeout")
	ErrNetwork          = errors.New("network failure")
	ErrUpstreamHTTP     = errors.New("upstream http error")
	ErrParse            = errors.New("parse failure")
	// ErrBlockedByOrigin marks a fetch that reached the origin and got a
	// well-formed HTTP 200 that is an anti-bot / verification interstitial
	// instead of the requested document. It is deliberately distinct from
	// ErrUpstreamHTTP: nothing failed at the transport level, so the only
	// way to tell them apart is by inspecting the body. Callers that can
	// re-fetch through a real browser key off this category.
	ErrBlockedByOrigin = errors.New("blocked by origin")
)

// safeErrorMessageMaxRunes caps the user-visible portion of a persisted
// error_msg so we never write multi-kilobyte upstream errors into the DB.
const safeErrorMessageMaxRunes = 200

// SafeMessage returns "<category>: <truncated summary>" suitable for
// persisting in DB columns and surfacing to API clients. The category is
// resolved via ClassifyError so wrapped sentinels take precedence over
// substring matching; the summary has credential-shaped values redacted,
// control characters stripped, and is capped at safeErrorMessageMaxRunes.
//
// Callers should still log the original error via the structured logger.
func SafeMessage(err error) string {
	if err == nil {
		return "none: "
	}
	raw := RedactSecrets(err.Error())
	return ClassifyError(err) + ": " + sanitizeSummary(raw)
}

// ClassifyError prefers errors.Is against the package sentinels and only
// falls back to substring matching against err.Error() when no sentinel
// matches. New error sites in fetcher/service should wrap one of the
// sentinels (Err...) so this path becomes the primary route; the
// substring fallback exists for legacy / external errors. Callers
// holding a persisted error_msg string (no live error chain) should
// route through ClassifyPersisted instead so the intent is explicit.
func ClassifyError(err error) string {
	if err == nil {
		return "none"
	}
	switch {
	case errors.Is(err, ErrUnsafeTarget):
		return "unsafe_target"
	case errors.Is(err, ErrContentType):
		return "content_type"
	case errors.Is(err, ErrResponseTooLarge):
		return "response_too_large"
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrNetwork):
		return "network"
	case errors.Is(err, ErrUpstreamHTTP):
		return "upstream_http"
	case errors.Is(err, ErrBlockedByOrigin):
		return "blocked_by_origin"
	case errors.Is(err, ErrParse):
		return "parse"
	}
	return classifyNormalized(normalize(err.Error()))
}

// ClassifyPersisted classifies a persisted error_msg string (the column
// stored on link / parse_job rows). Use it when the original error chain
// is unavailable and substring matching is the only remaining signal.
func ClassifyPersisted(msg string) string {
	return classifyNormalized(normalize(msg))
}

// classifyNormalized is the shared substring-matching engine. It expects
// already-normalised input (lowercased, trimmed) and returns the canonical
// category label.
func classifyNormalized(normalized string) string {
	if normalized == "" {
		return "none"
	}

	switch {
	case strings.Contains(normalized, "unsafe target host"):
		return "unsafe_target"
	case strings.Contains(normalized, "content-type"):
		return "content_type"
	case strings.Contains(normalized, "too large"),
		strings.Contains(normalized, "exceeds size limit"):
		return "response_too_large"
	case strings.Contains(normalized, "deadline exceeded"),
		strings.Contains(normalized, "timeout"),
		strings.Contains(normalized, "timed out"):
		return "timeout"
	case strings.Contains(normalized, "connection refused"),
		strings.Contains(normalized, "no such host"),
		strings.Contains(normalized, "dial tcp"),
		containsEOFToken(normalized),
		strings.Contains(normalized, "tls handshake timeout"):
		return "network"
	case strings.Contains(normalized, "status="),
		strings.Contains(normalized, " http "),
		strings.HasPrefix(normalized, "http "),
		strings.Contains(normalized, "github api http"),
		strings.Contains(normalized, "search http"),
		strings.Contains(normalized, "jina http"):
		return "upstream_http"
	case strings.Contains(normalized, "decode"),
		strings.Contains(normalized, "invalid character"),
		strings.Contains(normalized, "unexpected end of json"),
		strings.Contains(normalized, "parse"):
		return "parse"
	default:
		return "unknown"
	}
}

func sanitizeSummary(raw string) string {
	cleaned := make([]rune, 0, len(raw))
	for _, r := range raw {
		switch {
		case r == '\n', r == '\r', r == '\t':
			cleaned = append(cleaned, ' ')
		case r < 0x20:
			continue
		default:
			cleaned = append(cleaned, r)
		}
	}
	collapsed := strings.Join(strings.Fields(string(cleaned)), " ")
	if utf8.RuneCountInString(collapsed) <= safeErrorMessageMaxRunes {
		return collapsed
	}
	count := 0
	for idx := range collapsed {
		if count == safeErrorMessageMaxRunes {
			return collapsed[:idx]
		}
		count++
	}
	return collapsed
}

// containsEOFToken reports whether s contains "eof" as a standalone token —
// i.e. with non-letter neighbours on both sides. The previous loose
// strings.Contains(normalized, "eof") match would classify any string
// containing the literal substring (e.g. "preformatted", "leofield",
// "geofencing") as a network error, which produced misleading metric
// labels and DB error_msg prefixes. Wrapping io.EOF with errsafe.ErrNetwork
// at the call site is still preferable; this helper is the substring
// fallback for legacy / external errors.
func containsEOFToken(s string) bool {
	for {
		idx := strings.Index(s, "eof")
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isASCIILetter(s[idx-1])
		rightOK := idx+3 == len(s) || !isASCIILetter(s[idx+3])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+3:]
	}
}

// isASCIILetter is intentionally ASCII-only: classification normalizes the
// input first, so we never see uppercase letters here, and EOF strings
// produced by stdlib / our own wrappers never contain non-ASCII letters
// adjacent to the token.
func isASCIILetter(b byte) bool {
	return b >= 'a' && b <= 'z'
}

// NormalizedPointer returns the pointer's underlying string lowercased and
// whitespace-trimmed, or "" for nil pointers. It is exported so other
// packages (notably service.diagnostics) can share the same normalisation
// rules as classification instead of duplicating the helper.
func NormalizedPointer(value *string) string {
	if value == nil {
		return ""
	}
	return normalize(*value)
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
