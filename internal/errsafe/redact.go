package errsafe

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	postgresDSNRe           = regexp.MustCompile(`(?i)(postgres(?:ql)?://)([^:/?#@\s]+):([^@\s]*)@`)
	postgresKeywordSecretRe = regexp.MustCompile(`(?i)\b(password|passfile|sslpassword)\s*=\s*(?:'[^']*'|"[^"]*"|[^\s]+)`)
	urlInTextRe             = regexp.MustCompile(`(?i)(?:https?|postgres(?:ql)?)://[^\s"'` + "`" + `]+`)
	authorizationHeaderRe   = regexp.MustCompile(`(?i)(Authorization|Proxy-Authorization)\s*:\s*[^\r\n"']+`)
	cookieHeaderRe          = regexp.MustCompile(`(?i)(Cookie|Set-Cookie)\s*:\s*[^\r\n"']+`)
	cookiePairRe            = regexp.MustCompile(`([!#$%&*+.^_` + "`" + `|~0-9A-Za-z-]+)\s*=\s*(?:"[^"]*"|[^;\s,]+)`)
	bearerRe                = regexp.MustCompile(`(?i)Bearer\s+[^\s]+`)
	basicAuthRe             = regexp.MustCompile(`(?i)Basic\s+[A-Za-z0-9+/=]{4,}`)
	skKeyRe                 = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`)
	jwtRe                   = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)
)

// RedactSecrets removes credential-shaped values while retaining the host,
// path, and surrounding error context needed for diagnosis. It is shared by
// persisted client-safe messages and structured logs so the two projections
// cannot drift into different safety rules.
func RedactSecrets(raw string) string {
	redacted := urlInTextRe.ReplaceAllStringFunc(raw, func(match string) string {
		u, err := url.Parse(match)
		if err != nil {
			return match
		}
		changed := false
		hadUser := u.User != nil
		if u.User != nil {
			u.User = nil
			changed = true
		}
		if u.RawQuery != "" || u.ForceQuery {
			u.RawQuery = "<redacted>"
			u.ForceQuery = false
			changed = true
		}
		if u.Fragment != "" || u.RawFragment != "" {
			u.Fragment = ""
			u.RawFragment = ""
			changed = true
		}
		if !changed {
			return match
		}
		result := u.String()
		if hadUser {
			prefix := u.Scheme + "://"
			result = strings.Replace(result, prefix, prefix+"<redacted>@", 1)
		}
		return result
	})
	// Keep a regex fallback for malformed PostgreSQL URLs that net/url cannot
	// parse. Valid URLs were already handled above, including query/fragment
	// removal.
	redacted = postgresDSNRe.ReplaceAllString(redacted, `${1}<redacted>@`)
	redacted = postgresKeywordSecretRe.ReplaceAllString(redacted, "$1=<redacted>")
	redacted = authorizationHeaderRe.ReplaceAllStringFunc(redacted, redactAuthorizationHeader)
	redacted = cookieHeaderRe.ReplaceAllStringFunc(redacted, func(header string) string {
		return cookiePairRe.ReplaceAllString(header, "<redacted>")
	})
	redacted = bearerRe.ReplaceAllString(redacted, "Bearer <redacted>")
	redacted = basicAuthRe.ReplaceAllString(redacted, "Basic <redacted>")
	redacted = skKeyRe.ReplaceAllString(redacted, "sk-<redacted>")
	redacted = jwtRe.ReplaceAllString(redacted, "<redacted-jwt>")
	return redacted
}

func redactAuthorizationHeader(header string) string {
	colon := strings.IndexByte(header, ':')
	if colon < 0 {
		return "Authorization: <redacted>"
	}
	name := strings.TrimSpace(header[:colon])
	fields := strings.Fields(header[colon+1:])
	if len(fields) == 0 {
		return name + ": <redacted>"
	}
	return name + ": " + fields[0] + " <redacted>"
}
