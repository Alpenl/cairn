package fetcher

import "testing"

// TestWeChatFetcherCanHandle locks in the host whitelist so a future
// refactor that accidentally widens NewWeChatFetcher to also match
// other tencent.com domains gets caught.
func TestWeChatFetcherCanHandle(t *testing.T) {
	f := NewWeChatFetcher(nil)

	cases := []struct {
		url  string
		want bool
	}{
		{"https://mp.weixin.qq.com/s/abc123", true},
		{"https://MP.weixin.qq.com/s/abc123", true}, // case-insensitive host match
		{"https://weixin.qq.com/", false},           // wrong subdomain
		{"https://channels.weixin.qq.com/", false},  // unrelated wechat surface
		{"https://example.com/", false},
		{"https://mp.weixin.qq.com.evil.com/", false}, // suffix-injection guard
	}
	for _, tc := range cases {
		if got := f.CanHandle(tc.url); got != tc.want {
			t.Errorf("CanHandle(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
