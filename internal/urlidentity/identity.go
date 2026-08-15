// Package urlidentity owns the single URL normalisation contract that every
// collection entry point must use before it looks anything up or writes
// anything down.
//
// The reason this is a package rather than a helper next to one handler: the
// service has six independent ways to submit a URL (public links, batch,
// Inbox ingest, Inbox confirm, Extension/Reader capture, RSS subscription
// save). When each of them decided for itself what "the same page" meant, the
// same logical page produced a different reading record per entry point, and
// the differences were invisible until a user saw the duplicate. A shared
// package with one exported function makes "an entry point invented its own
// rules" a compile-time-visible act rather than a silent drift.
package urlidentity

import (
	"errors"
	"net/url"
	"sort"
	"strings"
)

// The three failure modes callers need to distinguish. They map 1:1 onto the
// public API error codes, so the HTTP layer can translate without re-parsing
// the URL or matching on message text.
var (
	// ErrEmpty reports a blank input.
	ErrEmpty = errors.New("urlidentity: url is empty")
	// ErrNotAbsolute reports an input that is not an absolute URL carrying
	// both a scheme and a host — relative references, scheme-only strings,
	// and opaque URLs such as "javascript:alert(1)" or "data:text/html,x"
	// all land here because they have no host.
	ErrNotAbsolute = errors.New("urlidentity: url must be absolute with scheme and host")
	// ErrUnsupportedScheme reports an absolute URL whose scheme is neither
	// http nor https.
	ErrUnsupportedScheme = errors.New("urlidentity: url scheme must be http or https")
)

// Normalize returns the canonical identity form of an absolute HTTP(S) URL.
//
// Two inputs that normalise to the same string are the same page for the
// purposes of deduplication, in-flight job reuse, and record creation. The
// rules are deliberately exhaustive and frozen; each one exists because a
// real pair of URLs differing only in that respect produced two records:
//
//   - http and https are never merged — they are genuinely different origins.
//   - scheme and host lowercase; one trailing host dot removed; one leading
//     "www." label folded away.
//   - the scheme's default port (80 / 443) removed; every other explicit port
//     kept and part of the identity.
//   - fragment removed entirely — it never reaches the server.
//   - path cleaned: "." and ".." resolved, repeated slashes collapsed, the
//     trailing slash dropped on non-root paths, the root path kept as "/".
//   - utm_*, fbclid and gclid dropped, matched case-insensitively.
//   - every other query pair preserved verbatim — including empty values and
//     repeated keys — then ordered by encoded key then encoded value so that
//     parameter order stops being part of the identity.
//
// Normalize never resolves relative references against a base and never
// contacts the network: the returned string addresses exactly the resource
// the caller passed in.
func Normalize(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrEmpty
	}

	parsed, err := url.Parse(trimmed)
	// A missing host is checked before the scheme allow-list so that opaque
	// dangerous URLs ("javascript:", "data:", "file:///etc/passwd") report the
	// same "not an absolute web URL" failure they always have, rather than
	// silently changing the public error code for existing clients.
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", ErrNotAbsolute
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", ErrUnsupportedScheme
	}

	host := normalizeHost(parsed.Hostname())
	if host == "" {
		return "", ErrNotAbsolute
	}

	var out strings.Builder
	out.WriteString(scheme)
	out.WriteString("://")
	// Userinfo is not part of the frozen rule set, so it is carried through
	// untouched rather than silently discarded: dropping it would merge two
	// URLs that address the resource with different credentials.
	if parsed.User != nil {
		out.WriteString(parsed.User.String())
		out.WriteByte('@')
	}
	out.WriteString(hostPort(scheme, host, parsed.Port()))
	out.WriteString(normalizePath(parsed.EscapedPath()))
	if query := normalizeQuery(parsed.RawQuery); query != "" {
		out.WriteByte('?')
		out.WriteString(query)
	}
	return out.String(), nil
}

// normalizeHost lowercases the host, removes the FQDN trailing dot, then folds
// a single leading "www." label. Only one "www." is folded: "www.www.a.com" is
// a genuinely different hostname from "www.a.com" and collapsing every leading
// label would merge unrelated sites.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	return strings.TrimPrefix(host, "www.")
}

// hostPort re-attaches the port when it carries identity. IPv6 literals are
// re-bracketed because url.URL.Hostname strips the brackets off.
func hostPort(scheme, host, port string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		return host
	}
	return host + ":" + port
}

// normalizePath cleans the *escaped* path rather than url.URL.Path.
//
// Working on the decoded path would treat "%2E%2E" as a ".." segment and
// "%2F" as a separator, letting an encoded path traverse out of its own
// directory and letting two distinct resources collapse into one. Splitting
// the escaped form keeps percent-encoded bytes opaque, so only literal "."
// and ".." segments are resolved.
func normalizePath(escaped string) string {
	if escaped == "" || escaped == "/" {
		return "/"
	}
	segments := strings.Split(escaped, "/")
	kept := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch segment {
		case "", ".":
			// Empty segments are the repeated slashes and the trailing
			// slash; "." addresses the current directory.
			continue
		case "..":
			// A ".." with nothing above it is dropped rather than escaping
			// the root, matching how servers resolve the request target.
			if len(kept) > 0 {
				kept = kept[:len(kept)-1]
			}
		default:
			kept = append(kept, segment)
		}
	}
	if len(kept) == 0 {
		return "/"
	}
	return "/" + strings.Join(kept, "/")
}

// normalizeQuery drops the tracking parameters and gives everything else a
// stable order. It works on the raw pair text instead of url.Values so that
// encoding, empty values, and repeated keys survive byte-for-byte: those are
// business parameters as far as this package is concerned, and rewriting them
// would change which resource the identity addresses.
func normalizeQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	type pair struct{ key, value, raw string }
	pairs := make([]pair, 0, strings.Count(rawQuery, "&")+1)
	for _, raw := range strings.Split(rawQuery, "&") {
		if raw == "" {
			// "a=1&&b=2" carries no third parameter.
			continue
		}
		key, value, _ := strings.Cut(raw, "=")
		if isTrackingKey(key) {
			continue
		}
		pairs = append(pairs, pair{key: key, value: value, raw: raw})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].key != pairs[j].key {
			return pairs[i].key < pairs[j].key
		}
		if pairs[i].value != pairs[j].value {
			return pairs[i].value < pairs[j].value
		}
		// "k" and "k=" both decode to the key "k" with an empty value, so
		// key+value alone is not a total order and the result would depend on
		// the order the client happened to send them in. The raw text breaks
		// the tie deterministically; genuinely identical pairs compare equal
		// here and keep their input order, which is unobservable.
		return pairs[i].raw < pairs[j].raw
	})
	ordered := make([]string, len(pairs))
	for i, item := range pairs {
		ordered[i] = item.raw
	}
	return strings.Join(ordered, "&")
}

// isTrackingKey matches the decoded key case-insensitively so that
// "UTM_Source" and "utm%5Fsource" are recognised the same way a server would
// recognise them.
func isTrackingKey(encodedKey string) bool {
	key := encodedKey
	if decoded, err := url.QueryUnescape(encodedKey); err == nil {
		key = decoded
	}
	key = strings.ToLower(key)
	return strings.HasPrefix(key, "utm_") || key == "fbclid" || key == "gclid"
}
