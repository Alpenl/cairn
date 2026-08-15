// Package fetcher – WeChat public-account article fetcher.
//
// mp.weixin.qq.com serves articles as static HTML once you pass the
// initial request-time anti-bot gate. The gate is sensitive to two
// inputs above all else:
//
//  1. User-Agent. The default WebTag-Go UA is auto-rejected; any
//     plausible desktop Chrome string passes. We pin a specific recent
//     Chrome on macOS — bump in lockstep with what the wider WeChat
//     reader fleet sends. Pretending to be mobile WeChat is overkill
//     and brittle.
//  2. Request rate per source IP. Bursts of even a few requests / minute
//     can flip the host into a 30-minute soft block, served as a JS
//     verification page. The OutboundLimiter below caps us at 1 request
//     every 30 seconds per host — slow on purpose, since this fetcher
//     is on the manual-paste path, not a crawler.
//
// Caveats:
//   - Articles whose URL signature has expired return HTTP 200 with a
//     redirect page; readability extracts no body and the standard
//     fallback chain (jina → search) takes over.
//   - Datacenter / cloud-provider IPs are blanket-blocked regardless of
//     UA. This fetcher is built for self-hosted deployments on
//     residential or office networks. Running webtag on AWS/GCP/Aliyun
//     and expecting this to work is wishful thinking; jina fallback
//     will pick up the slack at ~80% success rate (Jina runs headless
//     Chrome server-side and absorbs the bot detection).
package fetcher

import "time"

// NewWeChatFetcher returns a Fetcher targeting mp.weixin.qq.com only.
// Hands back a Fetcher (not *HostBoundHTMLFetcher) because callers
// only ever route it through Router.Register — the concrete type
// would just leak the template.
func NewWeChatFetcher(client *HTTPClient) Fetcher {
	return NewHostBoundHTMLFetcher(client, HostBoundHTMLFetcherOptions{
		Hosts: []string{"mp.weixin.qq.com"},
		// Recent Chrome on macOS — bump when wider WeChat reader fleet
		// fingerprints shift. Verified working against a live article
		// at the time of writing.
		UserAgent:  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
		Timeout:    20 * time.Second, // mp.weixin.qq.com pages average ~3MB; default 12s is tight
		MaxBytes:   4 << 20,          // 4MB cap — observed 2.99MB on a single article in May 2026
		Limiter:    NewOutboundLimiter(1, 30*time.Second),
		FetcherTag: "wechat",
	})
}
