package siteidentity

import "testing"

func TestFromURLGolden(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		key           string
		normalizedURL string
		adapter       string
	}{
		{
			name:          "generic site removes tracking and www",
			raw:           "HTTPS://WWW.Example.COM/docs/../tools/?utm_source=newsletter&b=2&a=1#overview",
			key:           "v1:host:example.com",
			normalizedURL: "https://example.com/tools?a=1&b=2",
		},
		{
			name:          "github repository groups subpaths",
			raw:           "https://github.com/OpenAI/GPT-5/issues/1?utm_campaign=test",
			key:           "v1:github:openai/gpt-5",
			normalizedURL: "https://github.com/OpenAI/GPT-5/issues/1",
			adapter:       "github",
		},
		{
			name:          "github owners do not merge",
			raw:           "https://github.com/other/project",
			key:           "v1:github:other/project",
			normalizedURL: "https://github.com/other/project",
			adapter:       "github",
		},
		{
			name:          "gitlab project groups subresources",
			raw:           "https://gitlab.com/group/project/-/merge_requests/3?utm_source=feed",
			key:           "v1:gitlab:group/project",
			normalizedURL: "https://gitlab.com/group/project/-/merge_requests/3",
			adapter:       "gitlab",
		},
		{
			name:          "gitlab nested namespace preserves project identity",
			raw:           "https://gitlab.com/parent/team/project/-/issues",
			key:           "v1:gitlab:parent/team/project",
			normalizedURL: "https://gitlab.com/parent/team/project/-/issues",
			adapter:       "gitlab",
		},
		{
			name:          "notion public pages are narrower than platform",
			raw:           "https://www.notion.so/Acme-Workspace-abc123/Project-xyz987",
			key:           "v1:notion:acme-workspace-abc123",
			normalizedURL: "https://notion.so/Acme-Workspace-abc123/Project-xyz987",
			adapter:       "notion",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FromURL(tt.raw)
			if err != nil {
				t.Fatalf("FromURL() error = %v", err)
			}
			if got.Key != tt.key || got.NormalizedURL != tt.normalizedURL || got.Adapter != tt.adapter {
				t.Fatalf("FromURL() = %#v, want key=%q normalized=%q adapter=%q", got, tt.key, tt.normalizedURL, tt.adapter)
			}
		})
	}
}

func TestFromURLRejectsInvalidTargets(t *testing.T) {
	for _, raw := range []string{"", "ftp://example.com/file", "https://user@example.com/path", "https://127.0.0.1/"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := FromURL(raw); err == nil {
				t.Fatalf("FromURL(%q) error = nil", raw)
			}
		})
	}
}
