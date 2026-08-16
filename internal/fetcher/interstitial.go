// Package fetcher – anti-bot interstitial detection.
//
// Some origins answer a blocked request with HTTP 200 and a small,
// well-formed HTML page asking the visitor to prove they are human. To
// every layer below this one that looks like a successful fetch: the
// transport succeeded, the content type is right, readability finds a
// title and a paragraph. It then flows into analysis, and the model
// dutifully summarises the interstitial — producing a link whose status
// is `done`, whose title is "微信公众号环境异常验证页", and whose summary
// explains that there is no article here.
//
// That failure mode is worse than a plain error. A failed fetch is
// visible and retryable; a confidently summarised block page is silent
// data loss. So an interstitial must be rejected as content, not merely
// scored as weak.
//
// Detection deliberately requires two independent signals — a marker
// phrase AND a body short enough to be an interstitial rather than an
// article. An article discussing WeChat's verification flow would match
// the phrases; it would not be 300 characters long. The one exception is
// the WeChat captcha endpoint, whose URL alone is conclusive.
package fetcher

import (
	"strings"
	"unicode/utf8"
)

// interstitialMaxRunes bounds how long a page can be and still be
// considered a block interstitial. Real interstitials are tiny — the
// WeChat 环境异常 page is a heading, a link and a button. The limit is
// set well above that so a slightly chattier variant still matches,
// and well below any real article so a piece *about* anti-bot pages
// does not.
const interstitialMaxRunes = 1200

// interstitialMarkers are phrase sets that identify a block page. Every
// phrase within a set must be present, so a single incidental mention
// cannot trigger a match. Sets are matched case-insensitively against
// the title and body together.
var interstitialMarkers = [][]string{
	// mp.weixin.qq.com serves this when the source IP trips its rate or
	// reputation checks. Observed live: heading 环境异常, a 去验证 link,
	// and an iframe with a 确定 button.
	{"环境异常", "去验证"},
	// The same gate phrased for the mobile web view.
	{"访问过于频繁"},
	// Cloudflare's managed challenge and block pages.
	{"just a moment", "cloudflare"},
	{"attention required", "cloudflare"},
	{"checking your browser before accessing"},
	// Generic Chinese verification interstitials.
	{"完成验证后即可继续访问"},
}

// interstitialURLMarkers are URL substrings that identify a block page
// on their own. Landing on one of these endpoints means the origin
// redirected us away from the document; no body inspection is needed.
var interstitialURLMarkers = []string{
	// WeChat's captcha endpoint. A fetch that ends here never saw the
	// article, regardless of what the captcha page itself contains.
	"/mp/wappoc_appmsgcaptcha",
	"/mp/verifycode",
}

// blockedInterstitial reports whether content is an anti-bot or
// verification interstitial served in place of the requested document.
//
// It is intentionally conservative: a false positive discards a real
// article, which is worse than the occasional block page slipping
// through to the weak-quality path that already exists.
func blockedInterstitial(content Content) bool {
	if urlLooksLikeInterstitial(content.URL) {
		return true
	}
	if utf8.RuneCountInString(content.Body) > interstitialMaxRunes {
		return false
	}
	haystack := strings.ToLower(content.Title + "\n" + content.Body)
	for _, markers := range interstitialMarkers {
		if allPresent(haystack, markers) {
			return true
		}
	}
	return false
}

// urlLooksLikeInterstitial reports whether a URL is itself a known
// verification endpoint.
func urlLooksLikeInterstitial(rawURL string) bool {
	lowered := strings.ToLower(rawURL)
	for _, marker := range interstitialURLMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// allPresent reports whether every marker appears in haystack. Markers
// are expected to already be lowercase.
func allPresent(haystack string, markers []string) bool {
	for _, marker := range markers {
		if !strings.Contains(haystack, marker) {
			return false
		}
	}
	return len(markers) > 0
}
