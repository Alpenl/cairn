package security

import (
	"errors"
	"net/url"
	"strings"
)

// ErrSensitiveURLDisclosure marks a URL that is valid for Cairn's local
// fetcher but must not be delegated to a third party. The error deliberately
// contains no URL details so it is safe to persist or log.
var ErrSensitiveURLDisclosure = errors.New("third-party URL delegation blocked by privacy policy")

// ThirdPartyURLProjection returns the URL representation that may appear in a
// third-party request body. Userinfo, query, and fragment are always removed.
// delegationSafe is true only for absolute HTTP(S) URLs that had neither
// userinfo nor a query; a fragment alone is harmless once projected away.
//
// The original string remains the source-fetch and identity value. This
// projection must never be used for local fetches, display, or deduplication.
func ThirdPartyURLProjection(raw string) (projected string, delegationSafe bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}

	hadUserinfo := parsed.User != nil
	hadQuery := parsed.RawQuery != "" || parsed.ForceQuery
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""

	scheme := strings.ToLower(parsed.Scheme)
	return parsed.String(), (scheme == "http" || scheme == "https") && !hadUserinfo && !hadQuery
}
