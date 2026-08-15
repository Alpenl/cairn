package fetcher

import "testing"

func TestHostFromURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://mp.weixin.qq.com/s/abc", "mp.weixin.qq.com"},
		{"https://MP.WEIXIN.QQ.COM/s/abc", "mp.weixin.qq.com"},
		{"https://mp.weixin.qq.com:443/s/abc", "mp.weixin.qq.com"},
		{"http://example.com/path?q=1", "example.com"},
		{"not a url", ""}, // url.Parse accepts "not a url" with empty host
		{"", ""},
	}
	for _, tc := range cases {
		if got := hostFromURL(tc.in); got != tc.want {
			t.Errorf("hostFromURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHostMatches(t *testing.T) {
	hosts := []string{"mp.weixin.qq.com", "arxiv.org"}
	cases := []struct {
		url  string
		want bool
	}{
		{"https://mp.weixin.qq.com/s/abc", true},
		{"https://MP.weixin.qq.com/s/abc", true}, // case-insensitive
		{"https://arxiv.org/abs/1234", true},
		{"https://www.arxiv.org/abs/1234", false}, // exact match only, no subdomain wildcarding
		{"https://example.com/", false},
		{"https://mp.weixin.qq.com.evil.com/", false}, // suffix-injection guard
		{"", false},
	}
	for _, tc := range cases {
		if got := hostMatches(tc.url, hosts); got != tc.want {
			t.Errorf("hostMatches(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
