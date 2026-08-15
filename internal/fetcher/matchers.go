package fetcher

import (
	nurl "net/url"
	"strings"
)

// hostFromURL extracts the lowercased host (without port) from a URL.
// Returns "" if the URL fails to parse — callers should treat that as
// "does not match" rather than panic.
func hostFromURL(rawURL string) string {
	parsed, err := nurl.Parse(rawURL)
	if err != nil || parsed == nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// hostMatches reports whether url's host equals one of the given hosts
// exactly (case-insensitive). For site-specific fetchers that should
// only fire on a known set of domains — wechat for "mp.weixin.qq.com",
// arxiv for "arxiv.org" / "www.arxiv.org" / etc.
//
// Subdomain wildcarding is intentionally omitted: each subdomain a site
// uses for content (mp. vs research. vs news.) usually warrants its own
// fetcher because the page shape differs. If a future fetcher genuinely
// needs suffix matching, add a separate hostSuffixMatches helper rather
// than overloading this one.
func hostMatches(url string, hosts []string) bool {
	host := hostFromURL(url)
	if host == "" {
		return false
	}
	for _, candidate := range hosts {
		if strings.EqualFold(host, candidate) {
			return true
		}
	}
	return false
}
